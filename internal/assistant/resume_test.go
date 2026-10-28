package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type fakeResumeConnection struct {
	view       desktopConversation
	calls      int
	snapshots  int
	beforeRead func(int)
	beforeSend func()
	sendError  error
}

func (f *fakeResumeConnection) snapshot(_ string) (desktopConversation, string, error) {
	f.snapshots++
	if f.beforeRead != nil {
		f.beforeRead(f.snapshots)
	}
	return f.view, "original-owner", nil
}
func (f *fakeResumeConnection) continueTurn(_, _ string, _ resumeTurn) error {
	if f.beforeSend != nil {
		f.beforeSend()
	}
	f.calls++
	return f.sendError
}
func limitedConversation() desktopConversation {
	v := desktopConversation{ID: "thread-a", ResumeState: "resumed", ModelProvider: "openai", Turns: []resumeTurn{{ID: "limited-turn", Status: "failed", Error: json.RawMessage(`{"message":"not used for matching","codexErrorInfo":"usageLimitExceeded"}`)}}}
	v.Runtime.Type = "idle"
	return v
}
func resumeFixture(t *testing.T) (Reader, *fakeResumeConnection, resumeDependencies) {
	t.Helper()
	r := warmupFixtureReader(t)
	c, err := r.scopeCredential("desktop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := ResumeReport{Enabled: true, Scope: "desktop", ID: "thread-a", Key: "bound-thread", Identity: credentialIdentity(c), Handled: map[string]bool{}}
	if err = writeState(r.resumePath(), p); err != nil {
		t.Fatal(err)
	}
	f := &fakeResumeConnection{view: limitedConversation()}
	d := resumeDependencies{connect: func(context.Context) (resumeConnection, func(), error) { return f, func() {}, nil }, credential: func() (quotaCredential, error) { return c, nil }, verifyRollout: func(desktopConversation, resumeTurn) error { return nil }, quota: func(context.Context, quotaCredential) AccountQuota {
		q := testQuota(time.Now(), false)
		limit := false
		q.LimitReached = &limit
		return q
	}}
	return r, f, d
}
func TestResumeOnlyExplicitLimitAndIdle(t *testing.T) {
	for _, kind := range []string{"normal", "interrupted", "network", "429", "active", "approval", "unconfirmed_submission", "active_goal", "not_resumed", "non_openai", "cloud", "missing_tail"} {
		t.Run(kind, func(t *testing.T) {
			r, f, d := resumeFixture(t)
			switch kind {
			case "normal":
				f.view.Turns[0].Status = "completed"
			case "interrupted":
				f.view.Turns[0].Status = "interrupted"
			case "network":
				f.view.Turns[0].Error = json.RawMessage(`{"codexErrorInfo":"responseStreamDisconnected"}`)
			case "429":
				f.view.Turns[0].Error = json.RawMessage(`{"message":"usage limit reached","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":429}}}`)
			case "active":
				f.view.Runtime.Type = "active"
			case "approval":
				f.view.Requests = []json.RawMessage{json.RawMessage(`{}`)}
			case "unconfirmed_submission":
				f.view.UnconfirmedSubmissions = append(f.view.UnconfirmedSubmissions, struct {
					Terminal bool `json:"terminal"`
				}{})
			case "active_goal":
				f.view.Goal = &struct {
					Status string `json:"status"`
				}{Status: "active"}
			case "not_resumed":
				f.view.ResumeState = "needs_resume"
			case "non_openai":
				f.view.ModelProvider = "custom"
			case "cloud":
				f.view.Mode = "durable"
			case "missing_tail":
				f.view.Turns = nil
			}
			if _, e := r.checkResume(context.Background(), d); e != nil {
				t.Fatal(e)
			}
			if f.calls != 0 {
				t.Fatal("non-limit or busy thread was resumed")
			}
		})
	}
}
func TestResumeDurableDedupAndAmbiguousSend(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "ambiguous"}[ambiguous], func(t *testing.T) {
			r, f, d := resumeFixture(t)
			f.beforeSend = func() {
				if len(r.ResumeStatus().Handled) != 1 {
					t.Fatal("dedup not durable before send")
				}
			}
			if ambiguous {
				f.sendError = errors.New("socket disconnected")
			}
			for i := 0; i < 3; i++ {
				if _, e := r.checkResume(context.Background(), d); e != nil {
					t.Fatal(e)
				}
			}
			if f.calls != 1 {
				t.Fatalf("sent %d times", f.calls)
			}
			// Re-enabling cannot erase the registration for an ambiguous delivery.
			p := r.ResumeStatus()
			p.Enabled = false
			_ = writeState(r.resumePath(), p)
			p.Enabled = true
			_ = writeState(r.resumePath(), p)
			_, _ = r.checkResume(context.Background(), d)
			if f.calls != 1 {
				t.Fatal("reenabling repeated prior request")
			}
		})
	}
}
func TestResumeRechecksTargetAndPolicyAfterQuota(t *testing.T) {
	for _, kind := range []string{"disabled", "different_target", "manual_continue", "credentials_expired", "different_identity", "quota_unknown", "quota_exhausted", "credential_rejected"} {
		t.Run(kind, func(t *testing.T) {
			r, f, d := resumeFixture(t)
			original := d.quota
			d.quota = func(ctx context.Context, c quotaCredential) AccountQuota {
				q := original(ctx, c)
				switch kind {
				case "disabled":
					_, _ = r.ConfigureResume(ctx, "", "", false)
				case "different_target":
					p := r.ResumeStatus()
					p.ID = "another"
					_ = writeState(r.resumePath(), p)
				case "manual_continue":
					f.view.Turns[0].ID = "manual-new-turn"
					f.view.Turns[0].Status = "inProgress"
				case "credentials_expired":
					d.credential = func() (quotaCredential, error) { return quotaCredential{}, errors.New("expired") }
				case "different_identity":
					d.credential = func() (quotaCredential, error) { return quotaCredential{access: "invalid"}, nil }
				case "quota_unknown":
					q.LimitReached = nil
				case "quota_exhausted":
					q.Windows[1].UsedPercent = 100
				case "credential_rejected":
					q.Status = "credential_rejected"
					q.Message = "请在 CC Switch 更新认证"
				}
				return q
			}
			// Capture closure edits to d in the quota callback on final lookup.
			origCredential := d.credential
			lookup := origCredential
			wrapped := d
			wrapped.credential = func() (quotaCredential, error) { return lookup() }
			wrapped.quota = func(ctx context.Context, c quotaCredential) AccountQuota {
				q := d.quota(ctx, c)
				lookup = d.credential
				return q
			}
			if _, e := r.checkResume(context.Background(), wrapped); e != nil {
				t.Fatal(e)
			}
			if f.calls != 0 {
				t.Fatal("changed state still submitted")
			}
		})
	}
}
func TestResumeCredentialExpiryIsAnExplicitCCSwitchError(t *testing.T) {
	r, f, d := resumeFixture(t)
	d.credential = func() (quotaCredential, error) { return quotaCredential{}, errors.New("expired") }
	p, e := r.checkResume(context.Background(), d)
	if e != nil || p.Status != "credential_error" || f.calls != 0 {
		t.Fatalf("bad expiry handling: %+v %v", p, e)
	}
}
func TestCanonicalResumeRequiresNewestBoundary(t *testing.T) {
	var v desktopConversation
	data := `{"resumeState":"resumed","modelProvider":"openai","threadRuntimeStatus":{"type":"idle"},"turnHistory":{"kind":"canonical","history":{"entitiesByKey":{"old":{"turnId":"old","status":"failed","error":{"codexErrorInfo":"usageLimitExceeded"}},"new":{"turnId":"new","status":"completed"}},"islands":[{"entries":[{"value":"old"},{"value":"new"}],"newerBoundary":{"status":"exhausted"}}]}}}`
	if json.Unmarshal([]byte(data), &v) != nil {
		t.Fatal("invalid fixture")
	}
	if _, ok := v.limitedTurn(); ok {
		t.Fatal("old limit resumed after newer completion")
	}
	v.History.History.Islands[0].Entries = v.History.History.Islands[0].Entries[:1]
	if _, ok := v.limitedTurn(); !ok {
		t.Fatal("explicit latest limit not recognized")
	}
	v.History.History.Islands[0].Newer.Status = "available"
	if _, ok := v.limitedTurn(); ok {
		t.Fatal("incomplete newer history accepted")
	}
}
func TestResumeUnknownStateStopsAndDoesNotExportSecrets(t *testing.T) {
	r, _, _ := resumeFixture(t)
	writeFixture(t, r.resumePath(), `{"enabled":true,`)
	p := r.ResumeStatus()
	if p.Enabled || p.Status != "error" {
		t.Fatal("corrupt policy must fail closed")
	}
	var decoded map[string]any
	b, _ := json.Marshal(p)
	_ = json.Unmarshal(b, &decoded)
	for _, key := range []string{"access_token", "refresh_token", "conversation", "input"} {
		if _, ok := decoded[key]; ok {
			t.Fatal("private content in resume state")
		}
	}
}

func TestResumeRecognizesCCSwitchOfficialProviderName(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	for _, tc := range []struct {
		config string
		name   string
		ok     bool
	}{
		{"model_provider='custom'\n[model_providers.custom]\nrequires_openai_auth=true\nwire_api='responses'\n", "custom", true},
		{"model_provider='custom'\n[model_providers.custom]\nrequires_openai_auth=true\nbase_url='https://third-party.invalid/v1'\n", "custom", false},
		{"model_provider='custom'\n[model_providers.custom]\nrequires_openai_auth=true\nenv_key='OTHER_KEY'\n", "custom", false},
		{"model_provider='custom'\n[model_providers.custom]\nrequires_openai_auth=false\n", "custom", false},
	} {
		writeFixture(t, filepath.Join(r.Home, ".codex/config.toml"), tc.config)
		name, ok := r.managedResumeProvider("desktop")
		if name != tc.name || ok != tc.ok {
			t.Fatalf("provider recognition: %q %v", name, ok)
		}
	}
}

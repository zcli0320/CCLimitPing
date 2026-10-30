package assistant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativePreviewUsesItsOwnAccountsAndAutomationState(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	quotaFixtures(t, home)
	// The CC Switch fixture has two accounts; preview must not read them.
	s := r.Snapshot(context.Background())
	if len(s.Accounts) != 0 {
		t.Fatal("preview read CC Switch accounts")
	}
	if !s.Capabilities["cli_handoff"].Available || !s.Capabilities["account_switch"].Available {
		t.Fatal("native CLI handoff and manual account manager capabilities missing")
	}
	if !s.Capabilities["desktop_handoff"].Available || !s.Capabilities["account_warmup"].Available {
		t.Fatal("preview automation capabilities are incorrect")
	}
	root := r.helperStateRoot()
	if !strings.HasSuffix(root, "CodexHelper-AccountPreview") {
		t.Fatal("preview did not isolate its state")
	}
	writeFixture(t, filepath.Join(root, "accounts", "codex_oauth_auth.json"),
		`{"version":2,"accounts":{"native":{"account_id":"native","email":"person@example.test","refresh_token":"SECRET_REFRESH","id_token":"SECRET_ID"}}}`)
	s = r.Snapshot(context.Background())
	if len(s.Accounts) != 1 || s.Accounts[0].ID != "native" || strings.Contains(s.Accounts[0].Name, "person@") {
		t.Fatal("native account metadata was not shown safely")
	}
	output, _ := json.Marshal(s)
	if strings.Contains(string(output), "SECRET_") || strings.Contains(string(output), "person@example.test") {
		t.Fatal("snapshot exposed a credential or full email")
	}
	stable := filepath.Join(home, "Library", "Application Support", "CodexHelper", "warmup", "policy.json")
	writeFixture(t, stable, `{"version":1,"enabled":true,"allowed":{}}`)
	if r.WarmupStatus().Enabled {
		t.Fatal("preview inherited stable warmup authorization")
	}
}

func TestNativePreviewQuotaUsesNativeStoreAndNoCCStash(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	// Until the native store exists, CC Switch and its stashes have no effect.
	if got := r.quotaCredentials(time.Now()); len(got) != 0 {
		t.Fatal("preview read CC Switch credentials")
	}
	ccStore, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json"), string(ccStore))
	if got := r.quotaCredentials(time.Now()); len(got) != 2 {
		t.Fatal("preview failed to match native account identities")
	}
}

func TestNativeWarmupAuthorizesOnlyNativeAccounts(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	quotaFixtures(t, home)
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json"),
		`{"version":2,"accounts":{"native":{"account_id":"native","email":"person@example.test"}}}`)
	if _, err := r.ConfigureWarmup(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.readWarmupState()
	if err != nil || len(p.Allowed) != 1 || !p.Allowed[privateKey("native")] {
		t.Fatal("warmup must only authorize the native account store")
	}
}

func TestNativeResumeRequiresTheSelectedScopesOwnCredential(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	ccStore, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json"), string(ccStore))
	for _, scope := range []string{"desktop", "cli"} {
		credential, err := r.scopeCredential(scope, time.Now())
		if err != nil || len(credential.sources) != 1 {
			t.Fatalf("%s scope credential unavailable: %v", scope, err)
		}
	}
}

func TestNativeReauthBlocksStaleLiveCredentialsUntilReapplied(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	bindings := filepath.Join(r.helperStateRoot(), "accounts", "bindings.json")
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-b","needs_reapply":false}}`)
	credentials := r.warmupCredentials(context.Background(), time.Now())
	if _, ok := credentials["managed-a"]; ok {
		t.Fatal("reauthenticated account used a stale live credential")
	}
	if _, ok := credentials["managed-b"]; !ok {
		t.Fatal("unrelated account lost its valid credential")
	}
	if _, err := r.scopeCredential("desktop", time.Now()); err == nil || !strings.Contains(err.Error(), "重新应用") {
		t.Fatal("reauthenticated Desktop binding did not give an actionable reapply error")
	}
	if _, err := r.scopeCredential("cli", time.Now()); err != nil {
		t.Fatal("unrelated CLI binding was rejected")
	}
	cliPath := filepath.Join(home, ".codex-cli", "auth.json")
	cliBefore, err := os.ReadFile(cliPath)
	if err != nil {
		t.Fatal(err)
	}
	desktopAuth, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedCLI nativeQuotaAuth
	if err := json.Unmarshal(desktopAuth, &updatedCLI); err != nil {
		t.Fatal(err)
	}
	updatedCLI.Tokens.Access = quotaToken("user-a", "workspace-a", time.Now().Add(2*time.Hour).Unix())
	updatedAuth, err := json.Marshal(updatedCLI)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, cliPath, string(updatedAuth))
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-a","needs_reapply":false}}`)
	selected, ok := r.warmupCredentials(context.Background(), time.Now())["managed-a"]
	if !ok || selected.access != updatedCLI.Tokens.Access || len(selected.sources) != 1 || selected.sources[0] != "CLI" {
		t.Fatal("a reapplied CLI was not selected independently of the stale Desktop binding")
	}
	activitySources, err := r.warmupActivitySources("managed-a", selected.sources)
	if err != nil || len(activitySources) != 2 || !strings.Contains(strings.Join(activitySources, ","), "Desktop") || !strings.Contains(strings.Join(activitySources, ","), "CLI") {
		t.Fatal("warmup did not inspect both bound clients")
	}
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-a","needs_reapply":true}}`)
	if _, err := r.warmupActivitySources("managed-a", selected.sources); err == nil {
		t.Fatal("warmup accepted a credential invalidated after selection")
	}
	if _, err := r.warmupActivitySources("managed-a", []string{"小助手缓存"}); err == nil {
		t.Fatal("warmup accepted cached access while reapplication was pending")
	}
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-a","needs_reapply":false}}`)
	if _, err := r.scopeCredential("cli", time.Now()); err != nil {
		t.Fatal("the reapplied CLI should remain usable")
	}
	if _, err := r.scopeCredential("desktop", time.Now()); err == nil || !strings.Contains(err.Error(), "重新应用") {
		t.Fatal("the stale Desktop binding should still require reapplication")
	}
	authorizeWarmupA(t, r)
	now := time.Now()
	checkedActivity := false
	d := warmupDependencies{
		now: func() time.Time { return now },
		quota: func(_ context.Context, credential quotaCredential) AccountQuota {
			if credential.access != updatedCLI.Tokens.Access {
				t.Fatal("warmup chose the stale Desktop credential")
			}
			return testQuota(now, false)
		},
		activity: func(sources []string) (bool, error) {
			checkedActivity = true
			if len(sources) != 2 || !strings.Contains(strings.Join(sources, ","), "Desktop") || !strings.Contains(strings.Join(sources, ","), "CLI") {
				t.Fatal("warmup omitted one of the bound client histories")
			}
			return true, nil
		},
		run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
			t.Fatal("warmup ran despite activity in a bound client")
			return WarmupExecution{}, nil
		},
	}
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil || !checkedActivity {
		t.Fatal("warmup did not check activity across both bindings")
	}
	writeFixture(t, cliPath, string(cliBefore))
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":false},"cli":{"account_id":"managed-b","needs_reapply":false}}`)
	if _, ok := r.warmupCredentials(context.Background(), time.Now())["managed-a"]; !ok {
		t.Fatal("reapplied account did not recover")
	}
	writeFixture(t, bindings, `{"version":1,"desktop":`)
	if len(r.warmupCredentials(context.Background(), time.Now())) != 0 {
		t.Fatal("unreadable binding state allowed credential use")
	}
}

func TestNativePendingTransactionBlocksScopeCredential(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "pending.json"), `{}`)
	if _, err := r.scopeCredential("desktop", time.Now()); err == nil {
		t.Fatal("an unresolved account transaction was accepted for resume")
	}
}

func TestNativeReauthDoesNotStartWarmupWithStaleCredential(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "bindings.json"), `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":null}`)
	authorizeWarmupA(t, r)
	now := time.Now()
	d := warmupDependencies{
		now: func() time.Time { return now },
		quota: func(context.Context, quotaCredential) AccountQuota {
			t.Fatal("stale account reached quota checking")
			return AccountQuota{}
		},
		activity: func([]string) (bool, error) { return false, nil },
		run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
			t.Fatal("stale account submitted a warmup")
			return WarmupExecution{}, nil
		},
	}
	for i := 0; i < 2; i++ {
		report, err := r.checkWarmup(context.Background(), "", d)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Accounts) != 1 || report.Accounts[0].Message != errNativeReapply.Error() {
			t.Fatal("warmup did not explain the required reapplication")
		}
		now = now.Add(time.Minute)
	}
}

func TestNativeReauthDuringWarmupPreflightAborts(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	desktopAuth, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(home, ".codex-cli", "auth.json"), string(desktopAuth))
	bindings := filepath.Join(r.helperStateRoot(), "accounts", "bindings.json")
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-a","needs_reapply":false}}`)
	authorizeWarmupA(t, r)
	now := time.Now()
	attempts := 0
	d := warmupDependencies{
		now:      func() time.Time { return now },
		quota:    func(context.Context, quotaCredential) AccountQuota { return testQuota(now, false) },
		activity: func([]string) (bool, error) { return false, nil },
		run: func(_ context.Context, _ string, _ quotaCredential, preSubmit func() error) (WarmupExecution, error) {
			attempts++
			writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-a","needs_reapply":true}}`)
			if err := preSubmit(); err != nil {
				return WarmupExecution{}, err
			}
			t.Fatal("warmup preflight allowed credentials invalidated by a new login")
			return WarmupExecution{}, nil
		},
	}
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Second)
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil || attempts != 1 {
		t.Fatal("warmup did not abort at the submission boundary")
	}
}

func TestNativeHandoffMapsAccountsFromItsOwnStore(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	// Remove the old store: matching must use the new account library.
	if err := os.Remove(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json")); err != nil {
		t.Fatal(err)
	}
	source, target, err := r.desktopHandoffAccounts()
	if err != nil || source != "managed-a" || target != "managed-b" {
		t.Fatalf("native account mapping: %q %q %v", source, target, err)
	}
	s := &cliHandoffSupervisor{r: r}
	if err := s.identifyAccounts(context.Background()); err != nil || s.sourceID != "managed-b" || s.targetID != "managed-a" {
		t.Fatalf("native CLI account mapping: %v", err)
	}
}

func TestNativeHandoffInstalledRequiresBindingAndNoPendingTransaction(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	root := filepath.Join(r.helperStateRoot(), "accounts")
	bindings := filepath.Join(root, "bindings.json")
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":false},"cli":null}`)
	if !r.nativeHandoffInstalled("desktop", "managed-a") {
		t.Fatal("matching binding rejected")
	}
	if r.nativeHandoffInstalled("cli", "managed-a") || r.nativeHandoffInstalled("desktop", "managed-b") {
		t.Fatal("wrong scope or identity accepted")
	}
	writeFixture(t, filepath.Join(root, "pending.json"), `{}`)
	if r.nativeHandoffInstalled("desktop", "managed-a") {
		t.Fatal("pending transaction treated as installed")
	}
	if err := os.Remove(filepath.Join(root, "pending.json")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, bindings, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":null}`)
	if r.nativeHandoffInstalled("desktop", "managed-a") {
		t.Fatal("stale binding accepted")
	}
}

func TestNativePendingTransactionCannotBeAcknowledgedOrRearmed(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r := Reader{Home: t.TempDir()}
	pending := filepath.Join(r.helperStateRoot(), "accounts", "pending.json")
	writeFixture(t, pending, `{}`)
	state := HandoffState{Version: 1, Scope: "desktop", SessionID: "session", Threshold: 10, Stage: "uncertain", Message: "check"}
	if err := writeState(r.handoffPath(), state); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcknowledgeHandoff(); err == nil {
		t.Fatal("pending native transaction was acknowledged")
	}
	if _, err := r.ConfigureHandoff("desktop", "session", 10, true); err == nil {
		t.Fatal("pending native transaction was rearmed")
	}
	if !r.Snapshot(context.Background()).Handoff.RecoveryPending {
		t.Fatal("pending native transaction hidden from UI")
	}
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("recovery evidence was changed")
	}
}

func TestNativeWarmupSchedulesBothAccountsAndPausesPendingSwitch(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	data, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json"), string(data))
	if _, err := r.ConfigureWarmup(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sent := map[string]bool{}
	calls := 0
	d := warmupDependencies{
		now:  func() time.Time { return now },
		busy: func([]string) bool { return false },
		quota: func(_ context.Context, c quotaCredential) AccountQuota {
			return testQuota(now, sent[credentialIdentity(c)])
		},
		run: func(_ context.Context, _ string, c quotaCredential, claim func() error) (WarmupExecution, error) {
			if err := claim(); err != nil {
				return WarmupExecution{}, err
			}
			calls++
			sent[credentialIdentity(c)] = true
			return WarmupExecution{Submitted: true, Completed: true, IdentityVerified: true}, nil
		},
	}
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("first observation must not submit")
	}
	now = now.Add(time.Minute)
	pending := filepath.Join(r.helperStateRoot(), "accounts", "pending.json")
	writeFixture(t, pending, `{}`)
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil || calls != 0 {
		t.Fatal("pending switch must pause warmup")
	}
	if err := os.Remove(pending); err != nil {
		t.Fatal(err)
	}
	report, err := r.checkWarmup(context.Background(), "", d)
	if err != nil || calls != 2 || len(report.Accounts) != 2 {
		t.Fatalf("native warmup failed: calls=%d err=%v", calls, err)
	}
	for _, item := range report.Accounts {
		if item.Status != "confirmed" || item.Attempts != 1 {
			t.Fatal("native warmup confirmation missing")
		}
	}
}

func TestNativeResumeSendsOnceOnlyAfterExplicitLimit(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	data, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json"), string(data))
	credential, err := r.scopeCredential("desktop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := ResumeReport{Enabled: true, Scope: "desktop", ID: "thread-a", Key: "bound-thread", Identity: credentialIdentity(credential), Handled: map[string]bool{}}
	if err = writeState(r.resumePath(), p); err != nil {
		t.Fatal(err)
	}
	fake := &fakeResumeConnection{view: limitedConversation()}
	d := resumeDependencies{
		connect:       func(context.Context) (resumeConnection, func(), error) { return fake, func() {}, nil },
		credential:    func() (quotaCredential, error) { return r.scopeCredential("desktop", time.Now()) },
		verifyRollout: func(desktopConversation, resumeTurn) error { return nil },
		quota: func(context.Context, quotaCredential) AccountQuota {
			q := testQuota(time.Now(), false)
			v := false
			q.LimitReached = &v
			return q
		},
	}
	for i := 0; i < 3; i++ {
		if _, err := r.checkResume(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if fake.calls != 1 || len(r.ResumeStatus().Handled) != 1 {
		t.Fatalf("expected exactly one continuation, got %d", fake.calls)
	}
}

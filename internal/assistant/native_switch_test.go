package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeNativeEnvironment struct {
	cc       bool
	busy     bool
	stopped  bool
	restored int
}

func (e *fakeNativeEnvironment) inspect(context.Context, string) (bool, error) {
	if e.busy {
		return e.cc, errors.New("standby busy")
	}
	return e.cc, nil
}
func (e *fakeNativeEnvironment) quiesce(context.Context, string) error {
	if e.busy {
		return errors.New("busy")
	}
	e.cc = false
	e.stopped = true
	return nil
}
func (e *fakeNativeEnvironment) verify(context.Context, string) error {
	if !e.stopped || e.cc || e.busy {
		return errors.New("not quiescent")
	}
	return nil
}
func (e *fakeNativeEnvironment) restore(_ context.Context, was bool) error {
	e.cc = was
	e.restored++
	e.stopped = false
	return nil
}

func nativeTestToken(letter string, expires time.Time) string {
	raw, _ := json.Marshal(map[string]any{"iss": "https://auth.openai.com", "sub": "subject-" + letter, "exp": expires.Unix(), "email": "user-" + letter + "@example.test", "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": "user-" + letter, "chatgpt_account_id": "workspace-" + letter}})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".synthetic"
}

func nativeFixture(t *testing.T) (Reader, *nativeDualSwitcher, *fakeNativeEnvironment) {
	t.Helper()
	r := Reader{Home: t.TempDir(), SQLite: "/usr/bin/sqlite3"}
	stamp := time.Now().Add(-time.Minute).UTC()
	accounts := map[string]any{}
	for i, letter := range []string{"a", "b"} {
		token := nativeTestToken(letter, time.Now().Add(time.Hour))
		accounts["managed-"+letter] = map[string]any{"account_id": "managed-" + letter, "chatgpt_account_id": "workspace-" + letter, "id_token": token, "refresh_token": "synthetic-refresh-" + letter, "token_updated_at_ms": stamp.UnixMilli(), "email": "user-" + letter + "@example.test", "unknown_account_field": "preserved"}
		a := handoffNativeAuth{Mode: "chatgpt", LastRefresh: stamp.Format(time.RFC3339Nano)}
		a.Tokens.Access, a.Tokens.Identity, a.Tokens.Refresh, a.Tokens.Workspace = token, token, "synthetic-refresh-"+letter, "workspace-"+letter
		root := []string{".codex", ".codex-cli"}[i]
		writeFixture(t, filepath.Join(r.Home, root, "auth.json"), string(nativeAuthBytes(a)))
		writeFixture(t, filepath.Join(r.Home, root, "config.toml"), "# retain comments and client settings\nmodel='fixture-model'\n")
	}
	raw, _ := json.Marshal(map[string]any{"version": 2, "accounts": accounts, "default_account_id": "managed-a", "unknown_store_field": "preserved"})
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), string(raw))
	settings, _ := json.Marshal(map[string]any{"codexConfigDir": filepath.Join(r.Home, ".codex-cli"), "currentProviderCodex": "provider-b", "unknown_setting": "preserved"})
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/settings.json"), string(settings))
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/live-state.json"), `{"version":1,"apps":{"codex":{"mode":"direct"}}}`)
	fixtureDB(t, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER,settings_config TEXT); INSERT INTO providers VALUES('provider-a','Fixture A','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-a"}}',0,'{"auth":{},"config":"model_provider=\"openai\""}'); INSERT INTO providers VALUES('provider-b','Fixture B','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-b"}}',1,'{"auth":{},"config":"model_provider=\"openai\""}');`)
	e := &fakeNativeEnvironment{cc: true}
	n := newNativeDualSwitcher(r)
	n.env = e
	n.endpoint = "http://127.0.0.1/native-test-oauth"
	n.client = &http.Client{Transport: quotaRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "127.0.0.1" {
			return nil, errors.New("external network forbidden in native fixture")
		}
		_ = req.ParseForm()
		letter := "a"
		if strings.HasSuffix(req.Form.Get("refresh_token"), "b") {
			letter = "b"
		}
		body, _ := json.Marshal(map[string]any{"access_token": nativeTestToken(letter, time.Now().Add(time.Hour)), "id_token": nativeTestToken(letter, time.Now().Add(time.Hour)), "refresh_token": "synthetic-refresh-" + letter})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	n.query = func(context.Context, quotaCredential) AccountQuota {
		q := testQuota(time.Now(), false)
		v := false
		q.LimitReached = &v
		return q
	}
	t.Cleanup(n.close)
	return r, n, e
}

func nativeState(t *testing.T, r Reader, scope string) HandoffState {
	t.Helper()
	s, err := r.configureNativeHandoff(scope, "session", 10)
	if err != nil {
		t.Fatal(err)
	}
	s.SourceAccount, s.TargetAccount, err = r.nativeAccountPair(scope)
	if err != nil {
		t.Fatal(err)
	}
	s.TurnID = "turn"
	s.Directory = "/fixture"
	s.Model = "fixture-model"
	s.Effort = "low"
	s.Permissions = "default"
	s.Stage = "pending"
	s.PendingAction = "prepare_target"
	if err = writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNativeSwapsBothProjectionsAndRestoresOnlyManager(t *testing.T) {
	for _, scope := range []string{"desktop", "cli"} {
		t.Run(scope, func(t *testing.T) {
			r, n, e := nativeFixture(t)
			s := nativeState(t, r, scope)
			ctx := context.Background()
			before := map[string]string{}
			for _, root := range []string{".codex", ".codex-cli"} {
				b, _ := os.ReadFile(filepath.Join(r.Home, root, "config.toml"))
				before[root] = digestString(b)
			}
			if err := n.prepare(ctx, s); err != nil {
				t.Fatal(err)
			}
			s.PendingAction = "switch"
			_ = writeState(r.handoffPath(), s)
			if err := n.switchAccount(ctx, s, func(context.Context) error { return errors.New("active") }); err == nil {
				t.Fatal("switched before active stopped")
			}
			s.PendingAction = "switch"
			_ = writeState(r.handoffPath(), s)
			if err := n.switchAccount(ctx, s, stoppedFixture); err != nil {
				t.Fatal(err)
			}
			if !n.installed(s) || !r.AccountAllocation().Distinct {
				t.Fatal("two-client assignment not verified")
			}
			active, _ := r.clientAccountID(scope)
			passive, _ := r.clientAccountID(nativeScopeOther(scope))
			if active != s.TargetAccount || passive != s.SourceAccount {
				t.Fatal("incorrect assignments")
			}
			for root, hash := range before {
				b, _ := os.ReadFile(filepath.Join(r.Home, root, "config.toml"))
				if digestString(b) != hash {
					t.Fatal("client config changed")
				}
			}
			if err := n.finish(ctx, s, func(context.Context, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if !e.cc || e.restored != 1 {
				t.Fatal("CC Switch original running state not restored")
			}
			if _, err := os.Stat(r.nativeTransactionPath()); !os.IsNotExist(err) {
				t.Fatal("verified transaction remains")
			}
			if err := n.switchAccount(ctx, s, stoppedFixture); err == nil {
				t.Fatal("completed action replayed")
			}
		})
	}
}

func TestNativeRepairsDuplicateBeforeQuota(t *testing.T) {
	for _, scope := range []string{"desktop", "cli"} {
		t.Run(scope, func(t *testing.T) {
			r, n, _ := nativeFixture(t)
			activePath, _ := r.handoffAuthPath(scope)
			passivePath, _ := r.handoffAuthPath(nativeScopeOther(scope))
			raw, _ := os.ReadFile(activePath)
			writeFixture(t, passivePath, string(raw))
			if r.AccountAllocation().Distinct {
				t.Fatal("fixture not duplicated")
			}
			s := nativeState(t, r, scope)
			calls := 0
			n.query = func(context.Context, quotaCredential) AccountQuota { calls++; return AccountQuota{Status: "error"} }
			if err := n.prepare(context.Background(), s); !errors.Is(err, errNativeAllocationRepaired) {
				t.Fatal(err)
			}
			if !r.AccountAllocation().Distinct || calls != 0 {
				t.Fatal("allocation repair waited for quota")
			}
			after, _ := os.ReadFile(activePath)
			if string(raw) != string(after) {
				t.Fatal("active client's auth was changed during repair")
			}
		})
	}
}

func TestNativeRefreshPreservesUnknownFieldsAndGeneration(t *testing.T) {
	r, n, _ := nativeFixture(t)
	path := filepath.Join(r.Home, ".codex-cli/auth.json")
	raw, _ := os.ReadFile(path)
	var a handoffNativeAuth
	_ = json.Unmarshal(raw, &a)
	a.Tokens.Access = nativeTestToken("b", time.Now().Add(-time.Hour))
	writeFixture(t, path, string(nativeAuthBytes(a)))
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		_ = req.ParseForm()
		if req.Form.Get("refresh_token") != "synthetic-refresh-b" {
			t.Error("wrong refresh generation")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": nativeTestToken("b", time.Now().Add(time.Hour)), "id_token": nativeTestToken("b", time.Now().Add(2*time.Hour)), "refresh_token": "new-generation-b"})
	}))
	defer server.Close()
	n.client = server.Client()
	n.endpoint = server.URL
	s := nativeState(t, r, "desktop")
	if err := n.prepare(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || n.prepared.Tokens.Refresh != "new-generation-b" {
		t.Fatal("refresh not resolved once")
	}
	store, err := r.readNativeStore()
	if err != nil {
		t.Fatal(err)
	}
	if jsonField[string](store.raw, "unknown_store_field") != "preserved" || jsonField[string](store.accounts["managed-b"], "unknown_account_field") != "preserved" {
		t.Fatal("unknown account data lost")
	}
	if jsonField[string](store.accounts["managed-b"], "refresh_token") != "new-generation-b" {
		t.Fatal("new generation not persisted")
	}
	journal, _ := os.ReadFile(r.nativeTransactionPath())
	if strings.Contains(string(journal), "new-generation-b") {
		t.Fatal("journal leaked token")
	}
}

func TestNativeAmbiguousRefreshIsNotRetried(t *testing.T) {
	r, n, _ := nativeFixture(t)
	path := filepath.Join(r.Home, ".codex-cli/auth.json")
	raw, _ := os.ReadFile(path)
	var a handoffNativeAuth
	_ = json.Unmarshal(raw, &a)
	a.Tokens.Access = nativeTestToken("b", time.Now().Add(-time.Hour))
	writeFixture(t, path, string(nativeAuthBytes(a)))
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(503) }))
	defer server.Close()
	n.client = server.Client()
	n.endpoint = server.URL
	s := nativeState(t, r, "desktop")
	if n.prepare(context.Background(), s) == nil {
		t.Fatal("ambiguous refresh accepted")
	}
	n.close()
	if n.prepare(context.Background(), s) == nil || requests != 1 {
		t.Fatal("unconfirmed refresh repeated")
	}
	if _, err := os.Stat(r.nativeTransactionPath()); err != nil {
		t.Fatal("recovery record lost")
	}
}

func TestNativePreservesRecoveryAtEveryPublicationBoundary(t *testing.T) {
	for stop := 0; stop < 7; stop++ {
		t.Run(string(rune('a'+stop)), func(t *testing.T) {
			r, n, _ := nativeFixture(t)
			s := nativeState(t, r, "desktop")
			if err := n.prepare(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			s.PendingAction = "switch"
			_ = writeState(r.handoffPath(), s)
			hit := -1
			n.afterWrite = func(i int) error {
				hit = i
				if i == stop {
					return errors.New("injected crash")
				}
				return nil
			}
			if n.switchAccount(context.Background(), s, stoppedFixture) == nil {
				t.Fatal("crash ignored")
			}
			if hit != stop {
				t.Fatalf("publication boundary not exercised: got %d, want %d", hit, stop)
			}
			if n.installed(s) {
				t.Fatal("partial operation accepted")
			}
			n.close()
			if _, err := os.Stat(r.nativeTransactionPath()); err != nil {
				t.Fatal("partial transaction lost")
			}
			if newNativeDualSwitcher(r).inspect(context.Background(), "desktop") == nil {
				t.Fatal("unresolved action can be replayed")
			}
		})
	}
}

func TestNativePreparedAuthorizationCannotBeReused(t *testing.T) {
	for _, field := range []string{"authorization", "scope", "source", "target", "disabled", "unregistered"} {
		t.Run(field, func(t *testing.T) {
			r, n, _ := nativeFixture(t)
			s := nativeState(t, r, "desktop")
			if err := n.prepare(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(r.nativeTransactionPath())
			if err != nil {
				t.Fatal(err)
			}
			changed := s
			switch field {
			case "authorization":
				changed.AuthorizationID = "different-authorization"
			case "scope":
				changed.Scope = "cli"
			case "source":
				changed.SourceAccount = s.TargetAccount
			case "target":
				changed.TargetAccount = s.SourceAccount
			case "disabled":
				changed.Enabled = false
			case "unregistered":
				changed.Threshold++
			}
			if field != "unregistered" {
				if err = writeState(r.handoffPath(), changed); err != nil {
					t.Fatal(err)
				}
			}
			if n.prepare(context.Background(), changed) == nil {
				t.Fatal("prepared authorization reused after binding changed")
			}
			after, err := os.ReadFile(r.nativeTransactionPath())
			if err != nil || string(before) != string(after) {
				t.Fatal("rejected authorization changed recovery record")
			}
		})
	}
}

func TestNativeCoordinatorUsesExistingDesktopLifecycle(t *testing.T) {
	r, n, e := nativeFixture(t)
	_, d, f := desktopCoordinatorFixture(t)
	d.r = r
	d.native = n
	f.home = r.Home
	f.e.View.RolloutPath = filepath.Join(r.Home, ".codex/sessions/native.jsonl")
	if _, err := r.configureNativeHandoff("desktop", "session", 10); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && r.HandoffStatus().Stage != "completed"; i++ {
		s, err := r.AdvanceHandoff(context.Background(), d)
		if err != nil || s.Stage == "uncertain" {
			t.Fatalf("stage %s: %v", s.Stage, err)
		}
	}
	s := r.HandoffStatus()
	if s.Stage != "completed" || len(f.actions) != 4 || !r.AccountAllocation().Distinct {
		t.Fatalf("stage=%s actions=%v", s.Stage, f.actions)
	}
	next, rearmed, err := d.finishCompleted(context.Background(), s)
	if err != nil || !rearmed || next.Backend != nativeBackend || e.restored != 1 {
		t.Fatalf("finish: %v", err)
	}
}

func TestNativeRefusesBusyStandbyAndExternalRestart(t *testing.T) {
	r, n, e := nativeFixture(t)
	s := nativeState(t, r, "desktop")
	e.busy = true
	if n.prepare(context.Background(), s) == nil {
		t.Fatal("busy standby accepted")
	}
	e.busy = false
	if err := n.prepare(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.PendingAction = "switch"
	_ = writeState(r.handoffPath(), s)
	e.cc = true
	before, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if n.switchAccount(context.Background(), s, stoppedFixture) == nil {
		t.Fatal("restarted manager accepted")
	}
	after, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if string(before) != string(after) {
		t.Fatal("live manager race modified credentials")
	}
}

func TestNativeCLICoordinatorRestartsOnlyWorkingClient(t *testing.T) {
	runNativeCLICoordinator(t, false)
}

func TestNativeCLIIdleDuplicateRepair(t *testing.T) { runNativeCLICoordinator(t, true) }

func runNativeCLICoordinator(t *testing.T, idleRepair bool) {
	r, n, e := nativeFixture(t)
	a, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	b, _ := os.ReadFile(filepath.Join(r.Home, ".codex-cli/auth.json"))
	writeFixture(t, filepath.Join(r.Home, ".codex-cli/auth.json"), string(a))
	writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(b))
	if idleRepair {
		writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(a))
	}
	work := filepath.Join(r.Home, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"cwd": work, "model": "fixture-model", "reasoningEffort": "low", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "readOnly"}, "activePermissionProfile": nil, "collaborationMode": nil, "serviceTier": nil, "disabledPluginIds": []any{}}
	state := map[string]any{"settings": settings, "thread": map[string]any{"id": "fixture-thread", "cwd": work, "modelProvider": "openai", "status": map[string]any{"type": "active", "activeFlags": []any{}}, "turns": []any{map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}, "itemsView": "full"}}}}
	if idleRepair {
		thread := state["thread"].(map[string]any)
		thread["status"] = map[string]any{"type": "idle"}
		thread["turns"].([]any)[0].(map[string]any)["status"] = "completed"
	}
	raw, _ := json.Marshal(state)
	writeFixture(t, filepath.Join(r.Home, "fake-state.json"), string(raw))
	script := filepath.Join(r.Home, "fake-codex")
	if err := os.WriteFile(script, []byte(fakeHandoffCLI), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "codex-helper-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{Transport: quotaRoundTrip(func(req *http.Request) (*http.Response, error) {
		claims, ok := decodeTokenClaims(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if !ok {
			return nil, errors.New("fixture token invalid")
		}
		used := 20
		if claims.Auth.User == "user-a" {
			used = 91
		}
		body := `{"rate_limit":{"primary_window":{"used_percent":` + strconv.Itoa(used) + `,"limit_window_seconds":18000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800},"limit_reached":false}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	n.query = func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) }
	s := &cliHandoffSupervisor{r: r, ctx: ctx, id: "fixture-thread", cwd: work, exe: script, socket: filepath.Join(root, "control.sock"), env: append(cliEnvironment(r.Home), "FAKE_HANDOFF_HOME="+r.Home), provider: "openai", input: &cliHandoffInput{}, sourceID: "managed-a", targetID: "managed-b", client: client, native: n}
	if err = s.start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.stop(stop)
	}()
	if _, err = r.configureNativeHandoff("cli", s.id, 10); err != nil {
		t.Fatal(err)
	}
	if idleRepair {
		for i := 0; i < 3; i++ {
			if _, err = r.AdvanceHandoff(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		if !r.AccountAllocation().Distinct || r.HandoffStatus().Stage != "watching" {
			t.Fatal("idle CLI allocation not repaired")
		}
		after, _ := os.ReadFile(filepath.Join(r.Home, ".codex-cli/auth.json"))
		if string(after) != string(a) {
			t.Fatal("working CLI auth changed during allocation repair")
		}
		var result struct {
			Continued int `json:"continued"`
		}
		_ = readJSON(filepath.Join(r.Home, "fake-state.json"), &result)
		if result.Continued != 0 {
			t.Fatal("idle CLI unexpectedly continued")
		}
		return
	}
	for i := 0; i < 20 && r.HandoffStatus().Stage != "completed"; i++ {
		u, err := r.AdvanceHandoff(ctx, s)
		if err != nil || u.Stage == "uncertain" {
			t.Fatalf("step %d stage=%s message=%s err=%v", i, u.Stage, u.Message, err)
		}
	}
	u := r.HandoffStatus()
	if u.Stage != "completed" || !r.AccountAllocation().Distinct {
		t.Fatalf("stalled at %s", u.Stage)
	}
	if err = n.finish(ctx, u, s.verifyAccountID); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Continued int `json:"continued"`
	}
	_ = readJSON(filepath.Join(r.Home, "fake-state.json"), &result)
	if result.Continued != 1 || e.restored != 1 {
		t.Fatal("working client continuation or manager restoration repeated")
	}
	active, _ := r.clientAccountID("cli")
	passive, _ := r.clientAccountID("desktop")
	if active != "managed-b" || passive != "managed-a" {
		t.Fatal("CLI/Desktop assignments incorrect")
	}
}

func TestNativeUnpublishedPreparationCanBeReviewedWithoutRollback(t *testing.T) {
	r, n, e := nativeFixture(t)
	s := nativeState(t, r, "desktop")
	before, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if err := n.prepare(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	txn, err := n.txn()
	if err != nil {
		t.Fatal(err)
	}
	n.close()
	s.Enabled = false
	s.Stage = "uncertain"
	s.PendingAction = ""
	s.LastStage = "waiting_exit"
	_ = writeState(r.handoffPath(), s)
	state, err := r.reviewNativePreparation(context.Background(), n)
	if err != nil || state.Stage != "off" || state.Enabled {
		t.Fatalf("review: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if string(before) != string(after) || !r.AccountAllocation().Distinct || e.restored != 1 {
		t.Fatal("review changed auth or failed to restore manager")
	}
	if _, err = os.Stat(filepath.Join(n.dir(txn), "reviewed-native-transaction.json")); err != nil {
		t.Fatal("original journal not archived")
	}
	if _, err = os.Stat(filepath.Join(n.dir(txn), "target-auth.json")); err != nil {
		t.Fatal("prepared credential recovery payload lost")
	}
}

func TestNativePublishedPreparationCannotBeDiscardedByReview(t *testing.T) {
	r, n, _ := nativeFixture(t)
	s := nativeState(t, r, "desktop")
	if err := n.prepare(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.PendingAction = "switch"
	_ = writeState(r.handoffPath(), s)
	hit := false
	n.afterWrite = func(int) error { hit = true; return errors.New("crash") }
	if n.switchAccount(context.Background(), s, stoppedFixture) == nil {
		t.Fatal("crash ignored")
	}
	if !hit {
		t.Fatal("publication fault was not exercised")
	}
	n.close()
	s.Enabled = false
	s.Stage = "uncertain"
	_ = writeState(r.handoffPath(), s)
	if _, err := r.reviewNativePreparation(context.Background(), n); err == nil {
		t.Fatal("published operation discarded")
	}
	if _, err := os.Stat(r.nativeTransactionPath()); err != nil {
		t.Fatal("pending transaction removed")
	}
}

func TestNativeIdleDuplicateRepairDoesNotStartTask(t *testing.T) {
	r, n, _ := nativeFixture(t)
	raw, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	writeFixture(t, filepath.Join(r.Home, ".codex-cli/auth.json"), string(raw))
	_, d, f := desktopCoordinatorFixture(t)
	d.r = r
	d.native = n
	f.home = r.Home
	f.e.View.Runtime.Type = "idle"
	f.e.View.Turns[0].Status = "completed"
	if _, err := r.configureNativeHandoff("desktop", "session", 10); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if !r.AccountAllocation().Distinct || len(f.actions) != 0 || r.HandoffStatus().Stage != "watching" {
		t.Fatal("idle repair started or interrupted a task")
	}
}

func TestNativeConfigurationAndCardRoutingAreRestricted(t *testing.T) {
	for _, mode := range []string{"keyring", "auto", "ephemeral"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := nativeFixture(t)
			writeFixture(t, filepath.Join(r.Home, ".codex-cli/config.toml"), "cli_auth_credentials_store='"+mode+"'\n")
			if r.validateNativeConfiguration() == nil {
				t.Fatal("unsupported auth storage accepted")
			}
		})
	}
	r, _, _ := nativeFixture(t)
	cmd := exec.Command(r.SQLite, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `UPDATE providers SET settings_config='{"config":"model_provider=\"openai\"\n[model_providers.other]\nbase_url=\"https://example.invalid\""}' WHERE app_type='codex'`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.nativeBinding(context.Background(), "managed-b"); err == nil {
		t.Fatal("card can rewrite unrelated client route")
	}
}

func TestNativeModelOnlyCardPreservesIsolatedClientConfigs(t *testing.T) {
	r, n, _ := nativeFixture(t)
	cmd := exec.Command(r.SQLite, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `UPDATE providers SET settings_config='{"config":"model=\"fixture-model\""}' WHERE app_type='codex'`)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, root := range []string{".codex", ".codex-cli"} {
		raw, err := os.ReadFile(filepath.Join(r.Home, root, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		before[root] = string(raw)
	}
	s := nativeState(t, r, "desktop")
	if err := n.prepare(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.PendingAction = "switch"
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	if err := n.switchAccount(context.Background(), s, stoppedFixture); err != nil {
		t.Fatal(err)
	}
	for root, want := range before {
		raw, err := os.ReadFile(filepath.Join(r.Home, root, "config.toml"))
		if err != nil || string(raw) != want {
			t.Fatal("model metadata overwrote isolated client config")
		}
	}
	writeFixture(t, filepath.Join(r.Home, ".codex-cli/config.toml"), "model='different-model'\n")
	if _, err := r.nativeBinding(context.Background(), "managed-b"); err == nil {
		t.Fatal("mismatched model card accepted")
	}
}

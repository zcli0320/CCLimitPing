package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testQuota(now time.Time, running bool) AccountQuota {
	reset := now.Add(5 * time.Hour).Unix()
	after := int64(18000)
	used := float64(0)
	if running {
		reset = now.Add(4 * time.Hour).Unix()
		after = 14400
		used = 2
	}
	return AccountQuota{Status: "ready", Windows: []QuotaWindow{{Seconds: 18000, UsedPercent: used, ResetAt: &reset, ResetAfter: &after}, {Seconds: 604800, UsedPercent: 10}}}
}
func TestWarmupDecisionOnlyAfterIdleConfirmed(t *testing.T) {
	now := time.Now()
	record := &WarmupAccount{}
	if decideWarmup(testQuota(now, true), record, now, false) || record.Status != "window_running" {
		t.Fatal("running window must skip")
	}
	record = &WarmupAccount{}
	if decideWarmup(testQuota(now, false), record, now, false) {
		t.Fatal("first idle reading must not send")
	}
	if !decideWarmup(testQuota(now.Add(15*time.Second), false), record, now.Add(15*time.Second), false) {
		t.Fatal("confirmed idle window should be eligible")
	}
	record.NoRetryBefore = now.Add(5 * time.Hour)
	if decideWarmup(testQuota(now, false), record, now, false) || record.Status != "verification_pending" {
		t.Fatal("ambiguous previous request must not retry")
	}
	q := testQuota(now, false)
	q.Windows[1].UsedPercent = 100
	if decideWarmup(q, &WarmupAccount{}, now, true) {
		t.Fatal("even a test cannot use an exhausted weekly account")
	}
	if decideWarmup(AccountQuota{Status: "network_error"}, &WarmupAccount{}, now, false) {
		t.Fatal("network error must not trigger")
	}
	q = testQuota(now, false)
	q.Windows = q.Windows[:1]
	if decideWarmup(q, &WarmupAccount{}, now, true) {
		t.Fatal("unknown weekly state must not trigger")
	}
	q = testQuota(now, false)
	limited := true
	q.LimitReached = &limited
	if decideWarmup(q, &WarmupAccount{}, now, true) {
		t.Fatal("server-reported limit must prevent a request")
	}
}

func warmupFixtureReader(t *testing.T) Reader {
	home := t.TempDir()
	quotaFixtures(t, home)
	writeFixture(t, filepath.Join(home, ".cc-switch/settings.json"), `{}`)
	fixtureDB(t, filepath.Join(home, ".cc-switch/cc-switch.db"), `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER,sort_index INTEGER);
INSERT INTO providers VALUES('one','One','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-a"}}',1,1);
INSERT INTO providers VALUES('two','Two','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-b"}}',0,2);`)
	return Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
}

func TestWarmupBothAccountsPersistClaimsAndAvoidDuplicateRequests(t *testing.T) {
	r := warmupFixtureReader(t)
	ctx := context.Background()
	now := time.Now()
	calls := 0
	sent := map[string]bool{}
	if _, err := r.ConfigureWarmup(ctx, true); err != nil {
		t.Fatal(err)
	}
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(_ context.Context, c quotaCredential) AccountQuota {
		return testQuota(now, sent[credentialIdentity(c)])
	}, run: func(_ context.Context, _ string, c quotaCredential, claim func() error) (WarmupExecution, error) {
		if err := claim(); err != nil {
			return WarmupExecution{}, err
		}
		calls++
		sent[credentialIdentity(c)] = true
		_, journal, err := r.readWarmupState()
		if err != nil {
			t.Fatal(err)
		}
		if journal.Accounts[credentialIdentity(c)].Attempts != 1 {
			t.Fatal("claim not durably saved before run")
		}
		return WarmupExecution{Submitted: true, Completed: true, IdentityVerified: true, Usage: &WarmupUsage{Input: 12, Output: 1, Total: 13}}, nil
	}}
	if _, err := r.checkWarmup(ctx, "", d); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("first poll must only confirm idle")
	}
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(report.Accounts) != 2 {
		t.Fatalf("both accounts not warmed independently: %d", calls)
	}
	for _, account := range report.Accounts {
		if account.Status != "confirmed" || account.Attempts != 1 || account.ConfirmedAttempt != 1 {
			t.Fatalf("unconfirmed warmup: %+v", account)
		}
	}
	now = now.Add(time.Minute)
	if _, err := r.checkWarmup(ctx, "", d); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("active windows received duplicate requests")
	}
	if _, err := r.ConfigureWarmup(ctx, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Hour)
	if _, err := r.checkWarmup(ctx, "", d); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("disabled policy sent a request")
	}
	data, _ := os.ReadFile(filepath.Join(r.warmupRoot(), "journal.json"))
	for _, secret := range []string{"fake-test-signature", "REFRESH_MUST_NOT_BE_USED", "workspace-a", "user-a", "managed-a"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("credentials or raw identity persisted")
		}
	}
}

func TestRecoveredCredentialBypassesOnlyMissingCredentialBackoff(t *testing.T) {
	r := warmupFixtureReader(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := r.ConfigureWarmup(ctx, true); err != nil {
		t.Fatal(err)
	}
	key := credentialIdentity(r.quotaCredentials(now)["managed-b"])
	j := warmupJournal{Version: 1, Accounts: map[string]*WarmupAccount{key: {Key: key, Status: "credential_unavailable", NextCheck: now.Add(5 * time.Minute)}}, Bindings: map[string]string{privateKey("managed-b"): key}}
	if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
		t.Fatal(err)
	}
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, true) }}
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range report.Accounts {
		if a.Key == key && a.Status == "window_running" {
			return
		}
	}
	t.Fatal("recovered credential remained hidden behind backoff")
}

func TestWarmupCrashAfterSubmitIsNotRetriedAfterRestart(t *testing.T) {
	r := warmupFixtureReader(t)
	now := time.Now()
	calls := 0
	ctx := context.Background()
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, false) }}
	probeCtx, cancel := context.WithCancel(ctx)
	d.run = func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		if err := claim(); err != nil {
			return WarmupExecution{}, err
		}
		calls++
		cancel()
		return WarmupExecution{Submitted: true}, errors.New("lost response")
	}
	if _, err := r.checkWarmup(probeCtx, "managed-a", d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ConfigureWarmup(ctx, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	d.run = func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
		calls++
		return WarmupExecution{}, errors.New("must not run")
	}
	// Only authorize the previously submitted identity in this test.
	p, j, err := r.readWarmupState()
	if err != nil {
		t.Fatal(err)
	}
	p.Allowed = map[string]bool{privateKey("managed-a"): true}
	if writeState(filepath.Join(r.warmupRoot(), "policy.json"), p) != nil {
		t.Fatal("save")
	}
	if len(j.Accounts) != 1 {
		t.Fatal("test did not persist original claim")
	}
	if _, err := r.checkWarmup(ctx, "", d); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("ambiguous request retried after restart")
	}
}

func TestWarmupBusyAndUserDisableBeforeSubmit(t *testing.T) {
	r := warmupFixtureReader(t)
	now := time.Now()
	ctx := context.Background()
	calls := 0
	if _, err := r.ConfigureWarmup(ctx, true); err != nil {
		t.Fatal(err)
	}
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return true }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, false) }, run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
		calls++
		return WarmupExecution{}, nil
	}}
	_, _ = r.checkWarmup(ctx, "", d)
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || report.Accounts[0].Status != "account_busy" {
		t.Fatal("busy account not postponed")
	}
	now = now.Add(time.Minute)
	d.busy = func([]string) bool { return false }
	d.run = func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		_, e := r.ConfigureWarmup(ctx, false)
		if e != nil {
			t.Fatal(e)
		}
		return WarmupExecution{}, claim()
	}
	report, err = r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range report.Accounts {
		if record.Attempts != 0 {
			t.Fatal("disable race sent a request")
		}
	}
}

func TestWarmupExecutionLockAndCorruptJournal(t *testing.T) {
	r := warmupFixtureReader(t)
	unlock, err := lockWarmup(filepath.Join(r.warmupRoot(), "execution.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckWarmup(context.Background(), ""); err == nil {
		t.Fatal("concurrent execution not blocked")
	}
	unlock()
	writeFixture(t, filepath.Join(r.warmupRoot(), "journal.json"), "broken")
	if _, err := r.CheckWarmup(context.Background(), "managed-a"); err == nil {
		t.Fatal("corrupt journal must fail closed")
	}
}

func TestWarmupResetThenIdleStartsEachAccountExactlyOnce(t *testing.T) {
	r := warmupFixtureReader(t)
	now := time.Now()
	ctx := context.Background()
	// Simulate a credential still valid across the future reset. Managed
	// refresh itself remains outside this scheduler.
	for i, dir := range []string{".codex", ".codex-cli"} {
		letter := []string{"a", "b"}[i]
		token := quotaToken("user-"+letter, "workspace-"+letter, now.Add(12*time.Hour).Unix())
		data, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": token, "id_token": token, "account_id": "workspace-" + letter}})
		writeFixture(t, filepath.Join(r.Home, dir, "auth.json"), string(data))
	}
	if _, err := r.ConfigureWarmup(ctx, true); err != nil {
		t.Fatal(err)
	}
	running := true
	sent := map[string]bool{}
	count := 0
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(_ context.Context, c quotaCredential) AccountQuota {
		return testQuota(now, running || sent[credentialIdentity(c)])
	}, run: func(_ context.Context, _ string, c quotaCredential, claim func() error) (WarmupExecution, error) {
		if err := claim(); err != nil {
			return WarmupExecution{}, err
		}
		count++
		sent[credentialIdentity(c)] = true
		return WarmupExecution{Submitted: true, Completed: true, IdentityVerified: true}, nil
	}}
	_, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("normal work window should skip")
	}
	now = now.Add(4*time.Hour + 11*time.Second)
	running = false
	_, err = r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("reset must be confirmed before sending")
	}
	now = now.Add(15 * time.Second)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("after reset expected two independent sends, got %d", count)
	}
	for _, record := range report.Accounts {
		if record.Status != "confirmed" {
			t.Fatal("post-reset request did not verify")
		}
	}
	_, _ = r.checkWarmup(ctx, "", d)
	if count != 2 {
		t.Fatal("duplicate warmup after reset")
	}
}

func TestWarmupUserRequestDuringSetupCancelsSyntheticRequest(t *testing.T) {
	r := warmupFixtureReader(t)
	ctx := context.Background()
	now := time.Now()
	running := false
	if _, err := r.ConfigureWarmup(ctx, true); err != nil {
		t.Fatal(err)
	}
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, running) }, run: func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		running = true
		return WarmupExecution{}, claim()
	}}
	_, _ = r.checkWarmup(ctx, "", d)
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range report.Accounts {
		if record.Attempts != 0 {
			t.Fatal("normal request started window during setup but warmup still submitted")
		}
	}
}

func TestWarmupExternalAuthAndEphemeralTurnProtocol(t *testing.T) {
	env := warmupEnvironment("/temporary")
	for _, value := range env {
		if strings.HasPrefix(value, "OPENAI_API_KEY=") || strings.HasPrefix(value, "CODEX_AUTH_JSON=") {
			t.Fatal("ambient auth leaked into runner")
		}
	}
	data, _ := json.Marshal(WarmupExecution{Submitted: true, Model: "small"})
	if strings.Contains(string(data), "access") {
		t.Fatal("runner exports credentials")
	}
	var input bytes.Buffer
	scan := bufio.NewScanner(strings.NewReader(`{"id":"approval","method":"item/commandExecution/requestApproval","params":{"secret":"NO_ECHO"}}` + "\n"))
	rpc := warmupRPC{in: &input, lines: scan, result: &WarmupExecution{}}
	if _, err := rpc.receive(); err == nil {
		t.Fatal("runner should refuse server initiated tool or refresh requests")
	}
	if !strings.Contains(input.String(), `"error"`) || strings.Contains(input.String(), "approved") || strings.Contains(input.String(), "NO_ECHO") {
		t.Fatal("tool denial or output sanitization failed")
	}
	input.Reset()
	rpc = warmupRPC{in: &input, lines: bufio.NewScanner(strings.NewReader(`{"id":1,"error":{"code":-1,"message":"SECRET_BACKEND_ERROR"}}` + "\n")), result: &WarmupExecution{}}
	if _, err := rpc.call("model/list", map[string]any{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("backend errors must be sanitized")
	}
}

func TestWarmupServerCountdownWinsOverClockSkew(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour).Unix()
	after := int64(12000)
	w := QuotaWindow{Seconds: 18000, ResetAt: &past, ResetAfter: &after}
	if !windowRunning(w, now) {
		t.Fatal("valid server countdown should prevent a duplicate despite local skew")
	}
	if !windowDeadline(w, now).Equal(now.Add(12000 * time.Second)) {
		t.Fatal("deadline must follow server countdown")
	}
	w.ResetAt = nil
	if !windowRunning(w, now) {
		t.Fatal("countdown alone still proves an active window")
	}
}

// Explicit opt-in: validates the installed protocol without submitting a turn.
func TestWarmupLiveIsolationPreflight(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_PREFLIGHT") != "1" {
		t.Skip("explicit live preflight only")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}
	credentials := r.quotaCredentials(time.Now())
	if len(credentials) == 0 {
		t.Fatal("no matching current access token")
	}
	for _, credential := range credentials {
		stop := errors.New("preflight stop before any model request")
		result, err := runIsolatedWarmup(context.Background(), codexExecutable(r.Home), credential, func() error { return stop })
		if !errors.Is(err, stop) || result.Submitted || !result.IdentityVerified || result.Model == "" {
			t.Fatalf("preflight failed: result %+v error %v", result, err)
		}
		return
	}
}

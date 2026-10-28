package assistant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit real-account allocation repair. This does not stop/restart Desktop
// or send model input. Unknown outcomes retain the native journal and payloads.
func TestLiveNativeAllocationRepair(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_NATIVE_ALLOCATION") != "repair-standby-once" {
		t.Skip("explicit real-account allocation repair not requested")
	}
	evidence := os.Getenv("CODEX_HELPER_LIVE_NATIVE_EVIDENCE")
	if !filepath.IsAbs(evidence) {
		t.Fatal("absolute private evidence directory required")
	}
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal("evidence directory unavailable")
	}
	f, err := os.OpenFile(filepath.Join(evidence, "started.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("experiment already registered")
	}
	_ = json.NewEncoder(f).Encode(map[string]any{"kind": "real-native-allocation-repair", "started_at": time.Now().UTC()})
	_ = f.Close()
	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}
	beforeState := r.HandoffStatus()
	if beforeState.Enabled || r.ResumeStatus().Enabled {
		t.Fatal("disable session automation before allocation experiment")
	}
	beforeAllocation := r.AccountAllocation()
	if beforeAllocation.Distinct {
		t.Fatal("allocation already distinct; no repair was exercised")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	life, err := newDesktopHandoffLifecycle(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	source, target, err := r.nativeAccountPair("desktop")
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	s := HandoffState{Version: 2, Backend: nativeBackend, Enabled: true, AuthorizationID: hex.EncodeToString(nonce[:]), Scope: "desktop", SourceAccount: source, TargetAccount: target, Threshold: 8, Stage: "pending", PendingAction: string(HandoffPrepareTarget)}
	paths := []string{".codex/auth.json", ".codex/config.toml", ".codex-cli/auth.json", ".codex-cli/config.toml", ".cc-switch/codex_oauth_auth.json", ".cc-switch/settings.json"}
	hashes := func() map[string]string {
		m := map[string]string{}
		for _, name := range paths {
			if b, e := os.ReadFile(filepath.Join(r.Home, name)); e == nil {
				m[name] = digestString(b)
			}
		}
		return m
	}
	before := hashes()
	n := newNativeDualSwitcher(r)
	defer n.close()
	err = n.prepare(ctx, s)
	result := map[string]any{"success": false, "kind": "real-native-allocation-repair", "active_scope": "desktop", "before_hashes": before, "after_hashes": hashes(), "finished_at": time.Now().UTC()}
	defer func() { _ = writeState(filepath.Join(evidence, "result.json"), result) }()
	if !errors.Is(err, errNativeAllocationRepaired) {
		result["error"] = errString(err)
		t.Fatalf("native allocation repair not verified: %v", err)
	}
	after := hashes()
	if before[".codex/auth.json"] != after[".codex/auth.json"] || before[".codex/config.toml"] != after[".codex/config.toml"] || before[".codex-cli/config.toml"] != after[".codex-cli/config.toml"] {
		t.Fatal("repair changed active login or client configuration")
	}
	if !r.AccountAllocation().Distinct {
		t.Fatal("real allocation still conflicts")
	}
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := findDesktopProcessPair(procs, os.Getuid(), life.appPath)
	if err != nil || pair != life.old {
		t.Fatal("allocation repair restarted the working Desktop")
	}
	if r.HandoffStatus() != beforeState {
		t.Fatal("allocation repair modified conversation authorization")
	}
	if _, err = os.Stat(r.nativeTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("verified allocation recovery not cleaned")
	}
	result["success"] = true
	result["distinct"] = true
	result["working_process_unchanged"] = true
	t.Log("real native allocation repair verified: both disk identities distinct; working Desktop and task untouched")
}

func errString(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// Run from the detached launcher: quitting Desktop must not kill this verifier.
func TestLiveNativeDualDesktopHandoff(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_NATIVE_FULL") != "run-native-dual-once" {
		t.Skip("explicit full native handoff not requested")
	}
	id := os.Getenv("CODEX_HELPER_LIVE_FULL_DESKTOP_THREAD")
	evidence := os.Getenv("CODEX_HELPER_LIVE_NATIVE_EVIDENCE")
	if !desktopHandoffUUID.MatchString(id) || !filepath.IsAbs(evidence) {
		t.Fatal("exact session and private evidence required")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}
	if r.HandoffStatus().Enabled || r.ResumeStatus().Enabled {
		t.Fatal("another automatic operation is enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 210*time.Second)
	defer cancel()
	n := newNativeDualSwitcher(r)
	defer n.close()
	if err = n.inspect(ctx, "desktop"); err != nil {
		t.Fatal(err)
	}
	life, err := newDesktopHandoffLifecycle(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range life.oldTree {
		if p.PID == os.Getpid() {
			t.Fatal("verifier must be outside Desktop process tree")
		}
	}
	source, target, err := r.nativeAccountPair("desktop")
	if err != nil {
		t.Fatal(err)
	}
	credentials := r.quotaCredentials(time.Now())
	a, okA := credentials[source]
	b, okB := credentials[target]
	if !okA || !okB {
		t.Fatal("real quota credentials unavailable")
	}
	qa, qb := n.query(ctx, a), n.query(ctx, b)
	ar, ak := handoffWindow(&qa, 18000)
	br, bk := handoffWindow(&qb, 18000)
	threshold := (ar + br) / 2
	if !ak || !bk || !validHandoffThreshold(threshold) || ar >= threshold || !handoffTargetQuotaOK(qb, threshold) {
		t.Fatal("real source/standby quota ordering unsuitable")
	}
	t.Logf("real native trigger: source=%.1f target=%.1f adjusted-threshold=%.1f", ar, br, threshold)
	state, err := r.configureNativeHandoff("desktop", id, threshold)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r.HandoffStatus().Enabled {
			_, _ = r.ConfigureHandoff("", "", 0, false)
		}
	}()
	runtime := &desktopSingleSessionRuntime{r: r, id: id, provider: "custom", lifecycle: life, wholeAppExitAccepted: true}
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		t.Fatal("provider unsupported")
	}
	runtime.provider = provider
	deps := desktopRunnerDependencies{native: n, makeRuntime: func(context.Context, string) (desktopHandoffRuntime, error) { return runtime, nil }, resetRuntime: func(context.Context, desktopHandoffRuntime) error { cancel(); return nil }, quota: n.query, tick: time.Second, watchInterval: time.Second}
	err = r.runDesktopHandoff(ctx, id, threshold, state.AuthorizationID, deps)
	allocation := r.AccountAllocation()
	result := map[string]any{"success": errors.Is(err, context.Canceled) && allocation.Distinct, "working_scope": "desktop", "distinct": allocation.Distinct, "trigger": "adjusted-real-threshold", "source_remaining": ar, "target_remaining": br, "threshold": threshold, "finished_at": time.Now().UTC()}
	if err != nil {
		result["runner_result"] = errString(err)
	}
	_ = writeState(filepath.Join(evidence, "native-result.json"), result)
	if !errors.Is(err, context.Canceled) || !allocation.Distinct {
		t.Fatalf("native handoff not verified: %v stage=%s", err, r.HandoffStatus().Stage)
	}
	if _, err = os.Stat(r.nativeTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("native transaction remains")
	}
	active, _ := r.clientAccountID("desktop")
	passive, _ := r.clientAccountID("cli")
	if active != target || passive != source {
		t.Fatal("final account assignments incorrect")
	}
	t.Log("real native dual-client handoff verified; working Desktop continued once; CLI receives source account and remains stopped")
}

func TestLiveDesktopReadOnlyDiagnostic(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_READONLY_THREAD")
	if !desktopHandoffUUID.MatchString(id) {
		t.Skip("read-only session diagnostic not requested")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	life, err := newDesktopHandoffLifecycle(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		t.Fatal("provider unavailable")
	}
	runtime := &desktopSingleSessionRuntime{r: r, id: id, provider: provider, lifecycle: life, wholeAppExitAccepted: true}
	e, err := runtime.observe(ctx, id)
	turn, has := e.View.latest()
	var effort any
	_ = json.Unmarshal(turn.Params["effort"], &effort)
	t.Logf("read-only observation: error=%v has-turn=%t runtime=%s turn-status=%s owner-known=%t params-count=%d snapshot-effort=%v latest-effort=%s", err, has, e.View.Runtime.Type, turn.Status, e.Owner != "", len(turn.Params), effort, e.View.LatestEffort)
}

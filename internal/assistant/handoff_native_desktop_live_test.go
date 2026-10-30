package assistant

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeDesktopLiveHarnessRejectsInvalidSession(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate isolated test executable")
	}
	child := exec.Command(exe, "-test.run=^TestLiveDesktopViaNativeManager$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_HELPER_LIVE_NATIVE_DESKTOP_") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "CODEX_HELPER_LIVE_NATIVE_DESKTOP_CONFIRM=run-native-handoff-now", "CODEX_HELPER_LIVE_NATIVE_DESKTOP_THREAD=invalid")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Run(); err == nil || !strings.Contains(output.String(), "explicitly selected Desktop test session is required") {
		t.Fatal("live experiment did not reject an invalid session before inspecting the real environment")
	}
}

// This opt-in real-device experiment is not part of the installed app. It
// must be run from a process outside Desktop's process tree against a human-
// selected disposable session. Both account quotas and all client actions are
// real; only the source-low trigger is simulated. No credential is refreshed.
func TestLiveDesktopViaNativeManager(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_NATIVE_DESKTOP_THREAD")
	if id == "" || os.Getenv("CODEX_HELPER_LIVE_NATIVE_DESKTOP_CONFIRM") != "run-native-handoff-now" {
		t.Skip("explicit native Desktop experiment not requested")
	}
	if !desktopHandoffUUID.MatchString(id) || os.Getenv("CODEX_HELPER_TEST_HOME") != "" {
		t.Fatal("a real, explicitly selected Desktop test session is required")
	}
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r, err := NewReader()
	if err != nil {
		t.Fatal("local home unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal("process state unavailable")
	}
	stableApp := filepath.Join(r.Home, "Applications/Codex 小助手.app/Contents/MacOS/CodexHelper")
	for _, p := range procs {
		if p.UID == os.Getuid() && (p.Command == stableApp || strings.HasPrefix(p.Command, stableApp+" ")) {
			t.Fatal("quit the stable helper before the experiment")
		}
	}
	if r.HandoffStatus().Enabled || r.ResumeStatus().Enabled || r.nativeAccountPending() {
		t.Fatal("an automation or unresolved account transaction is active")
	}
	bridge := filepath.Join(r.Home, "Applications/Codex 小助手账号测试版.app/Contents/MacOS/codex-runtime-bridge")
	bridgeInfo, err := os.Lstat(bridge)
	if err != nil || !bridgeInfo.Mode().IsRegular() || bridgeInfo.Mode().Perm()&0111 == 0 || bridgeInfo.Mode().Perm()&0022 != 0 {
		t.Fatal("installed identity bridge unavailable")
	}
	pair, err := findDesktopProcessPairWithBridge(procs, os.Getuid(), desktopAppPath, bridge)
	if err != nil || pair.Bridge.PID == 0 || !desktopServerUsesHome(ctx, pair.Server, r.Home) {
		t.Fatal("Desktop must already be running through the installed identity bridge")
	}
	tree := desktopProcessTree(procs, pair.App)
	for _, p := range tree {
		if p.PID == os.Getpid() {
			t.Fatal("run the experiment outside the Desktop process tree")
		}
	}
	lifecycle := &desktopHandoffLifecycle{
		home: r.Home, appPath: desktopAppPath, bridgePath: bridge, uid: os.Getuid(),
		old: pair, oldTree: tree, read: readDesktopProcesses, usesHome: desktopServerUsesHome,
		command: func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Env = desktopLaunchEnvironment(os.Environ())
			return cmd.Run()
		},
	}
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		t.Fatal("Desktop provider unavailable")
	}
	_, target, err := r.desktopHandoffAccounts()
	if err != nil {
		t.Fatal("two distinct account mappings are unavailable")
	}
	runtime := &desktopSingleSessionRuntime{r: r, id: id, provider: provider, lifecycle: lifecycle, wholeAppExitAccepted: true}
	evidence, err := runtime.observe(ctx, id)
	turn, hasTurn := evidence.View.latest()
	activeGoal := evidence.View.Goal != nil && evidence.View.Goal.Status == "active"
	if err != nil || !evidence.RuntimeIdentityVerified || !evidence.InterventionKnown || evidence.Owner == "" ||
		evidence.View.ID != id || evidence.View.Runtime.Type != "active" || evidence.View.ResumeState != "resumed" ||
		evidence.View.ModelProvider != provider || evidence.View.Mode == "durable" || activeGoal || !hasTurn || turn.Status != "inProgress" {
		t.Fatal("selected test session or running account is not independently verified")
	}
	// Hold the existing scheduler lock so an already-enabled warmup policy
	// cannot submit a concurrent request while the real handoff is in progress.
	// The user's policy itself remains unchanged.
	warmupUnlock, err := lockWarmup(filepath.Join(r.warmupRoot(), "execution.lock"))
	if err != nil {
		t.Fatal("warmup is already running; wait until it finishes")
	}
	defer warmupUnlock()
	manager, err := r.startNativeHandoffManager(ctx)
	if err != nil {
		t.Fatal("native account manager is unavailable")
	}
	defer manager.Close()
	if err := manager.ready(ctx, target, "desktop"); err != nil {
		t.Fatal("target account or same-directory writer is not ready")
	}
	targetQuota, err := manager.quota(ctx, target)
	if err != nil || !resumeQuotaReady(targetQuota) {
		t.Fatal("target quota is unavailable")
	}
	current, err := r.scopeCredential("desktop", time.Now())
	if err != nil {
		t.Fatal("current account access is unavailable")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	sourceQuota := queryAccountQuota(ctx, client, current)
	sourceFive, sourceOK := handoffWindow(&sourceQuota, 18000)
	sourceWeek, sourceWeekOK := handoffWindow(&sourceQuota, 604800)
	targetFive, targetOK := handoffWindow(&targetQuota, 18000)
	targetWeek, targetWeekOK := handoffWindow(&targetQuota, 604800)
	if !resumeQuotaReady(sourceQuota) || !sourceOK || !sourceWeekOK || !targetOK || !targetWeekOK || sourceFive <= 15 || targetFive <= 15 || sourceWeek <= 10 || targetWeek <= 10 {
		t.Fatal("both real accounts need sufficient quota before this bounded test")
	}
	// No live action has occurred before all the checks above. Simulate only the
	// source threshold; the two real accounts still provide real quota data.
	const threshold = 10.0
	state, err := r.ConfigureHandoff("desktop", id, threshold, true)
	if err != nil {
		t.Fatal("cannot authorize the selected test session")
	}
	defer func() {
		current := r.HandoffStatus()
		if current.Enabled && current.AuthorizationID == state.AuthorizationID {
			_, _ = r.ConfigureHandoff("", "", 0, false)
		}
	}()
	var completed atomic.Bool
	deps := desktopRunnerDependencies{
		manager:     manager,
		makeRuntime: func(context.Context, string) (desktopHandoffRuntime, error) { return runtime, nil },
		resetRuntime: func(context.Context, desktopHandoffRuntime) error {
			completed.Store(true)
			cancel()
			return nil
		},
		quota: func(ctx context.Context, c quotaCredential) AccountQuota {
			q := queryAccountQuota(ctx, client, c)
			if q.Status == "ready" && credentialIdentity(c) == credentialIdentity(current) {
				for i := range q.Windows {
					if q.Windows[i].Seconds == 18000 {
						q.Windows[i].UsedPercent = 99
					}
				}
			}
			return q
		},
		tick: time.Second, watchInterval: time.Second,
	}
	err = r.runDesktopHandoff(ctx, id, threshold, state.AuthorizationID, deps)
	if !errors.Is(err, context.Canceled) || !completed.Load() {
		stage := r.HandoffStatus().Stage
		switch stage {
		case "off", "watching", "ready_stop", "waiting_stop", "ready_exit", "waiting_exit", "ready_switch", "waiting_switch", "ready_restart", "waiting_restart", "ready_continue", "waiting_continue", "completed", "uncertain", "cancelled", "error":
			t.Fatalf("native Desktop handoff did not reach verified completion; stage=%s", stage)
		default:
			t.Fatal("native Desktop handoff did not reach verified completion; state needs inspection")
		}
	}
	if !r.nativeHandoffInstalled("desktop", target) || r.nativeAccountPending() {
		t.Fatal("target authentication or transaction state was not confirmed")
	}
	if _, err := os.Lstat(r.desktopRunnerPath()); !os.IsNotExist(err) {
		t.Fatal("Desktop runner status was not removed")
	}
	t.Log("real native-manager Desktop handoff verified; source-low trigger simulated")
}

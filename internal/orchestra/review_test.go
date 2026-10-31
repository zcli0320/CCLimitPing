package orchestra

// Regression tests for the build 27 review by Codex (2026-10-31): cancel vs
// launch, task folder replaced after dispatch, executor lookup PATH.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func idleRunner(t *testing.T, e env) *Runner {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(e.cfg.Dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Runner{Cfg: e.cfg, Store: e.store, Logf: func(string, ...any) {}}
}

func finishRun(r *Runner, a *active) {
	if a != nil {
		<-a.done
		r.finish(a, a.why)
	}
}

func TestCancelledSnapshotIsNotStarted(t *testing.T) {
	e := setup(t)
	r := idleRunner(t, e)
	snap, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "R1", "TASK R1")
	if _, err := e.store.Cancel("R1"); err != nil {
		t.Fatal(err)
	}
	a := r.start(snap) // stale snapshot still says queued
	finishRun(r, a)
	got, _ := e.store.Get("R1")
	if a != nil || got.Status != Cancelled || len(got.Runs) != 0 {
		t.Fatalf("started=%v status=%s runs=%d", a != nil, got.Status, len(got.Runs))
	}
	if _, err := os.Stat(e.args); err == nil {
		t.Fatal("executor was launched for a cancelled task")
	}
}

// A cancel issued while the runner is between claiming and launching must
// wait for the launch and then stop it: it can't be lost, and the task can't
// end up running without the cancel being seen.
func TestCancelDuringLaunchIsSerializedAndStops(t *testing.T) {
	e := setup(t)
	t.Setenv("SLOW", "60")
	r := idleRunner(t, e)
	snap, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "R2", "TASK R2")
	cancelled := make(chan error, 1)
	testHookBeforeLaunch = func(string) {
		go func() { _, err := e.store.Cancel("R2"); cancelled <- err }()
		select {
		case err := <-cancelled:
			t.Errorf("cancel completed inside the claim (err=%v): launch and cancel are not serialized", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Cleanup(func() { testHookBeforeLaunch = nil })
	a := r.start(snap)
	testHookBeforeLaunch = nil
	if a == nil {
		t.Fatal("task was not launched")
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("cancel after launch: %v", err)
	}
	running := map[string]*active{"codex": a}
	for i := 0; i < 300 && len(running) > 0; i++ {
		r.tick(running)
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := e.store.Get("R2")
	if got.Status != Cancelled {
		t.Fatalf("status=%s", got.Status)
	}
	if err := syscall.Kill(-a.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("executor still alive: %v", err)
	}
}

func TestFolderReplacedBySymlinkIsRefused(t *testing.T) {
	e := setup(t)
	r := idleRunner(t, e)
	other := filepath.Join(filepath.Dir(e.folder), "other-project")
	os.Mkdir(other, 0o700)
	snap, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "R3", "TASK R3")
	os.Rename(e.folder, e.folder+"-old")
	if err := os.Symlink(other, e.folder); err != nil {
		t.Fatal(err)
	}
	a := r.start(snap)
	finishRun(r, a)
	got, _ := e.store.Get("R3")
	if a != nil || got.Status != Failed || !strings.Contains(got.LastReceipt(), "替换或重定向") {
		t.Fatalf("started=%v status=%s msg=%q", a != nil, got.Status, got.LastReceipt())
	}
}

func TestFolderRecreatedAtSamePathIsRefused(t *testing.T) {
	e := setup(t)
	r := idleRunner(t, e)
	snap, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "R4", "TASK R4")
	os.Rename(e.folder, e.folder+"-old")
	os.Mkdir(e.folder, 0o700)
	a := r.start(snap)
	finishRun(r, a)
	got, _ := e.store.Get("R4")
	if a != nil || got.Status != Failed || !strings.Contains(got.LastReceipt(), "不是派发时") {
		t.Fatalf("started=%v status=%s msg=%q", a != nil, got.Status, got.LastReceipt())
	}
}

func TestTaskWithoutFolderIdentityMustBeRedispatched(t *testing.T) {
	e := setup(t)
	r := idleRunner(t, e)
	snap, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "R5", "TASK R5")
	e.store.Update("R5", func(t *Task) error { t.FolderDev, t.FolderIno = 0, 0; return nil })
	a := r.start(snap)
	finishRun(r, a)
	got, _ := e.store.Get("R5")
	if a != nil || got.Status != Failed || !strings.Contains(got.LastReceipt(), "重新派发") {
		t.Fatalf("started=%v status=%s msg=%q", a != nil, got.Status, got.LastReceipt())
	}
}

func TestExecutorIsResolvedWithLoginPath(t *testing.T) {
	e := setup(t)
	r := idleRunner(t, e)
	claude := e.cfg.Agents["claude"]
	binDir := filepath.Dir(claude.Bin)
	claude.Bin = "claude" // bare name: must be found via PATH
	r.Cfg.Agents["claude"] = claude
	r.path = binDir + ":/usr/bin:/bin"
	t.Setenv("PATH", "/usr/bin:/bin") // the GUI's bare PATH lacks binDir
	snap, _ := e.store.Dispatch(e.cfg, "lead", "claude", e.folder, "R6", "TASK R6")
	a := r.start(snap)
	finishRun(r, a)
	got, _ := e.store.Get("R6")
	if a == nil || got.Status != Receipt {
		t.Fatalf("started=%v status=%s msg=%q", a != nil, got.Status, got.LastReceipt())
	}
	if _, err := claude.Resolve("/usr/bin:/bin"); err == nil {
		t.Fatal("Resolve must use the PATH it is given, not the process PATH")
	}
}

func TestLoginPathIsCachedPrivatelyAndRefreshed(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(t.TempDir(), "fakeshell")
	os.WriteFile(shell, []byte("#!/bin/sh\nPATH=/first:/usr/bin:/bin\nexport PATH\nexec /bin/sh -c \"$2\"\n"), 0o700)
	t.Setenv("SHELL", shell)
	if got := LoginPath(dir, false); got != "/first:/usr/bin:/bin" {
		t.Fatalf("first: %q", got)
	}
	info, err := os.Stat(filepath.Join(dir, "login-path"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", info, err)
	}
	os.WriteFile(shell, []byte("#!/bin/sh\nPATH=/second:/usr/bin:/bin\nexport PATH\nexec /bin/sh -c \"$2\"\n"), 0o700)
	if got := LoginPath(dir, false); got != "/first:/usr/bin:/bin" {
		t.Fatalf("fresh cache should be reused, got %q", got)
	}
	if got := LoginPath(dir, true); got != "/second:/usr/bin:/bin" {
		t.Fatalf("refresh: %q", got)
	}
	old := time.Now().Add(-2 * LoginPathMaxAge)
	os.Chtimes(filepath.Join(dir, "login-path"), old, old)
	os.WriteFile(shell, []byte("#!/bin/sh\nPATH=/third:/usr/bin:/bin\nexport PATH\nexec /bin/sh -c \"$2\"\n"), 0o700)
	if got := LoginPath(dir, false); got != "/third:/usr/bin:/bin" {
		t.Fatalf("stale cache must be recomputed, got %q", got)
	}
}

// Codex REV1: a cancel between the persisted claim and the launch must
// prevent the spawn.
func TestCancelAfterClaimBeforeLaunchPreventsSpawn(t *testing.T) {
	e := setup(t)
	r := idleRunner(t, e)
	snap, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "C1", "TASK C1")
	testHookAfterClaim = func(id string) {
		got, _ := e.store.Get(id)
		if got.Status != Running || len(got.Runs) != 1 || got.Runs[0].PID != 0 || got.Runs[0].Marker == "" {
			t.Errorf("claim not persisted before launch: %+v", got)
		}
		if _, err := e.store.Cancel(id); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookAfterClaim = nil })
	a := r.start(snap)
	testHookAfterClaim = nil
	got, _ := e.store.Get("C1")
	if a != nil || got.Status != Cancelled {
		t.Fatalf("started=%v status=%s", a != nil, got.Status)
	}
	if _, err := os.Stat(e.args); err == nil {
		t.Fatal("executor spawned after cancel")
	}
}

// Codex REV1: a runner that dies between the persisted claim and recording
// the PID leaves "running, pid 0". Recovery must fail it and never replay it.
func TestRecoveryOfStartingTaskDoesNotReplay(t *testing.T) {
	e := setup(t)
	task, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "C2", "TASK C2")
	e.store.Update(task.ID, func(t *Task) error {
		t.Status = Running
		t.Runs = []Run{{Kind: "task", Started: time.Now(), Marker: "C2-1-x"}}
		return nil
	})
	runFor(t, e)
	got := waitDone(t, e, "C2")
	if got.Status != Failed || !got.Unconfirmed || !strings.Contains(got.LastReceipt(), "未重新执行") || !strings.Contains(got.LastReceipt(), "可能仍在运行") {
		t.Fatalf("status=%s unconfirmed=%v msg=%q", got.Status, got.Unconfirmed, got.LastReceipt())
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(e.args); err == nil {
		t.Fatal("the task was replayed")
	}
	if _, err := e.store.RequestRework("C2", "again"); err == nil || !strings.Contains(err.Error(), "confirm-stopped") {
		t.Fatalf("rework must wait for confirmation: %v", err)
	}
	if _, err := e.store.ConfirmStopped("C2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RequestRework("C2", "again"); err != nil {
		t.Fatalf("rework after confirmation: %v", err)
	}
}

// Codex REV1 follow-ups: recovery must never kill (PID reuse, orphaned
// children in other groups make identity unprovable) and must block rework
// until the user confirms, whether or not a PID was recorded.
func TestRecoveryNeverKillsAndRequiresConfirmation(t *testing.T) {
	e := setup(t)
	task, _ := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "C3", "TASK C3")
	live := exec.Command("/bin/sleep", "60")
	started := time.Now()
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Process.Kill(); live.Wait() })
	e.store.Update(task.ID, func(t *Task) error {
		t.Status = Running
		t.Runs = []Run{{Kind: "task", Started: started, PID: live.Process.Pid}}
		return nil
	})
	runFor(t, e)
	got := waitDone(t, e, "C3")
	if got.Status != Failed || !got.Unconfirmed || !strings.Contains(got.LastReceipt(), "可能仍在运行") {
		t.Fatalf("status=%s unconfirmed=%v msg=%q", got.Status, got.Unconfirmed, got.LastReceipt())
	}
	if err := live.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("recovery killed a process: %v", err)
	}
	if _, err := e.store.RequestRework("C3", "again"); err == nil {
		t.Fatal("rework must wait for confirm-stopped")
	}
}

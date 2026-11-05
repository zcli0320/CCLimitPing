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

const fakeReviewer = `#!/bin/bash
echo "$@" >> "$ARGS_LOG"
cat > "$ARGS_LOG.reviewer_prompt"
[ -n "$REVIEW_SLEEP" ] && sleep "$REVIEW_SLEEP"
[ -n "$REVIEW_FAIL" ] && { echo "reviewer boom" >&2; exit 2; }
verdict="${REVIEW_VERDICT:-VERDICT: PASS}"
printf '{"type":"result","session_id":"rev-s1","is_error":false,"result":"Review complete.\\n%s"}\n' "$verdict"
`

func setupReviewEnv(t *testing.T) env {
	t.Helper()
	e := setup(t)
	bin := filepath.Join(t.TempDir(), "reviewer-claude")
	if err := os.WriteFile(bin, []byte(fakeReviewer), 0o700); err != nil {
		t.Fatal(err)
	}
	claude := e.cfg.Agents["claude"]
	claude.Bin = bin
	e.cfg.Agents["claude"] = claude
	return e
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"pass", "All good.\nVERDICT: PASS", "pass"},
		{"changes", "Fix line 10.\nVERDICT: CHANGES", "changes"},
		{"case and spaces", "  verdict:   pass  \n", "pass"},
		{"last line wins", "VERDICT: PASS\nWait, found a bug\nVERDICT: CHANGES", "changes"},
		{"last line wins pass", "VERDICT: CHANGES\nActually fine\nVERDICT: PASS", "pass"},
		{"bold", "Summary\n**VERDICT: PASS**", "pass"},
		{"bold label", "**VERDICT:** CHANGES", "changes"},
		{"bullet", "- VERDICT: PASS", "pass"},
		{"bullet bold", "- **VERDICT: CHANGES**", "changes"},
		{"code span", "`VERDICT: PASS`", "pass"},
		{"none", "RECEIPT R1: done\nLooks fine", "unavailable"},
		{"empty", "", "unavailable"},
		{"inline prose ignored", "The VERDICT: PASS line must be standalone", "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVerdict(tc.in); got != tc.want {
				t.Fatalf("parseVerdict(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReviewIDTruncation(t *testing.T) {
	if got := reviewID("T1", 1); got != "T1-r1" || !taskID.MatchString(got) {
		t.Fatalf("short: %q", got)
	}
	long := strings.Repeat("a", 64)
	for _, n := range []int{1, 9, 12, 999} {
		got := reviewID(long, n)
		if len(got) > 64 || !taskID.MatchString(got) || !strings.HasSuffix(got, "-r"+strings.TrimSpace(strings.Split(got, "-r")[1])) {
			t.Fatalf("n=%d: len=%d id=%q", n, len(got), got)
		}
	}
}

// Parent completion and reviewer archival are separate writes by the runner.
func waitReviewArchived(e env, id string) (*Task, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		child, err := e.store.Get(id)
		if err != nil || child.Status == Accepted || time.Now().After(deadline) {
			return child, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewPassAndWait(t *testing.T) {
	e := setupReviewEnv(t)
	t.Setenv("REVIEW_SLEEP", "0.3")
	t.Setenv("REVIEW_VERDICT", "VERDICT: PASS")
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1: build feature", nil, nil); err != nil {
		t.Fatal(err)
	}
	var sawReviewing bool
	for i := 0; i < 200; i++ {
		if tk, _ := e.store.Get("T1"); tk != nil && tk.Status == Reviewing {
			sawReviewing = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !sawReviewing {
		t.Fatal("parent never entered reviewing status")
	}
	// Wait with a short timeout must not return early thinking Reviewing is Done.
	mid, err := e.store.Wait("T1", 40*time.Millisecond, 10*time.Millisecond)
	if err != nil || mid.Status != Reviewing || mid.Done() {
		t.Fatalf("Wait during reviewing: task=%+v err=%v", mid, err)
	}
	done := waitDone(t, e, "T1")
	if done.Status != Receipt || done.ReviewVerdict != "pass" || done.ReviewTask != "T1-r1" || done.ReviewedRuns != 1 || !strings.Contains(done.ReviewText, "VERDICT: PASS") {
		t.Fatalf("parent after pass: %+v", done)
	}
	child, err := waitReviewArchived(e, "T1-r1")
	if err != nil || child.Status != Accepted || child.ReviewOf != "T1" || child.Project != "P1" {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	prompt, _ := os.ReadFile(e.args + ".reviewer_prompt")
	if !strings.Contains(string(prompt), "You are the reviewer for project P1. Review, do not modify files.") ||
		!strings.Contains(string(prompt), "TASK T1: build feature") ||
		!strings.Contains(string(prompt), "RECEIPT T1: done") {
		t.Fatalf("review card prompt: %s", prompt)
	}
}

func TestReviewChangesAutoRework(t *testing.T) {
	e := setupReviewEnv(t)
	t.Setenv("REVIEW_VERDICT", "foo.go:10: missing check\\nVERDICT: CHANGES")
	if err := WriteSettings(e.cfg.Dir, Settings{Account: "desktop", MinRemaining: 10, AutoReworkMax: 1, CheckTimeoutSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1: work", nil, nil); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, e, "T1")
	if done.Status != Receipt || done.ReviewVerdict != "changes" || done.AutoReworkCount != 1 || len(done.Runs) != 2 || done.ReviewedRuns != 2 || done.ReviewTask != "T1-r2" {
		t.Fatalf("final task=%+v", done)
	}
	if done.Runs[1].Kind != "auto_rework" {
		t.Fatalf("second run kind=%q, want auto_rework", done.Runs[1].Kind)
	}
	// The rework prompt must carry the card's limits, so a review-only card
	// never turns into code edits (seen with a real read-only review task).
	if log, err := os.ReadFile(e.args + ".prompts"); err != nil || !strings.Contains(string(log), AutoReworkLimits) {
		t.Fatalf("auto rework prompt lacks the card limits: %v", err)
	}
	r1, err := waitReviewArchived(e, "T1-r1")
	if err != nil || r1.Status != Accepted {
		t.Fatalf("r1=%+v err=%v", r1, err)
	}
	r2, err := waitReviewArchived(e, "T1-r2")
	if err != nil || r2.Status != Accepted {
		t.Fatalf("r2=%+v err=%v", r2, err)
	}
}

func TestReviewSkippedWhenReviewerIsLeadOrNoProject(t *testing.T) {
	e := setupReviewEnv(t)
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "claude", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	t1 := waitDone(t, e, "T1")
	if t1.Status != Receipt || t1.ReviewTask != "" || t1.ReviewedRuns != 0 {
		t.Fatalf("reviewer==lead task=%+v", t1)
	}
	if _, err := e.store.Get("T1-r1"); err == nil {
		t.Fatal("review child should not exist when reviewer == lead")
	}

	// The executor never reviews its own receipt (P1 lists reviewer claude,
	// so dispatch to claude here).
	if _, err := e.store.DispatchInProject(e.cfg, "lead", "P1", "claude", "", "T3", "TASK T3", nil, nil); err != nil {
		t.Fatal(err)
	}
	t3 := waitDone(t, e, "T3")
	if t3.Status != Receipt || t3.ReviewTask != "" || t3.ReviewedRuns != 0 {
		t.Fatalf("reviewer==executor task=%+v", t3)
	}

	if _, err := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "T2", "TASK T2"); err != nil {
		t.Fatal(err)
	}
	t2 := waitDone(t, e, "T2")
	if t2.Status != Receipt || t2.ReviewTask != "" || t2.ReviewedRuns != 0 {
		t.Fatalf("no-project task=%+v", t2)
	}
}

func TestReviewerFails(t *testing.T) {
	e := setupReviewEnv(t)
	t.Setenv("REVIEW_FAIL", "1")
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, e, "T1")
	if done.Status != Receipt || done.ReviewVerdict != "unavailable" || !strings.Contains(done.ReviewText, "reviewer boom") {
		t.Fatalf("failed reviewer parent=%+v", done)
	}
	// Parent receipt and child archival are separate store writes.
	child, err := e.store.Get("T1-r1")
	deadline := time.Now().Add(5 * time.Second)
	for err == nil && child.Status != Accepted && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		child, err = e.store.Get("T1-r1")
	}
	if err != nil || child.Status != Accepted {
		t.Fatalf("failed reviewer child=%+v err=%v", child, err)
	}
}

func TestAcceptAndCancelWhileReviewing(t *testing.T) {
	e := setupReviewEnv(t)
	t.Setenv("REVIEW_SLEEP", "30")
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)

	// Accept while reviewing cancels child.
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		tk, _ := e.store.Get("T1")
		ch, _ := e.store.Get("T1-r1")
		if tk != nil && tk.Status == Reviewing && ch != nil && ch.Status == Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := e.store.RequestRework("T1", "no rework while reviewing"); err == nil {
		t.Fatal("RequestRework must fail while Reviewing")
	}
	acc, err := e.store.Accept("T1")
	if err != nil || acc.Status != Accepted {
		t.Fatalf("Accept while reviewing: %+v %v", acc, err)
	}
	ch1 := waitDone(t, e, "T1-r1")
	if ch1.Status != Cancelled {
		t.Fatalf("child after parent Accept: %+v", ch1)
	}

	// Cancel while reviewing cancels both.
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T2", "TASK T2", nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		tk, _ := e.store.Get("T2")
		ch, _ := e.store.Get("T2-r1")
		if tk != nil && tk.Status == Reviewing && ch != nil && ch.Status == Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	can, err := e.store.Cancel("T2")
	if err != nil || can.Status != Cancelled {
		t.Fatalf("Cancel while reviewing: %+v %v", can, err)
	}
	ch2 := waitDone(t, e, "T2-r1")
	if ch2.Status != Cancelled {
		t.Fatalf("child after parent Cancel: %+v", ch2)
	}
}

func TestReviewCrashRecovery(t *testing.T) {
	e := setupReviewEnv(t)
	r := idleRunner(t, e)
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Update("T1", func(tk *Task) error {
		tk.Status = Receipt
		tk.Runs = []Run{{Kind: "task", Started: time.Now(), Ended: time.Now(), Receipt: "RECEIPT T1: done"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Pre-existing review child (as if runner crashed after dispatch before parent Update).
	if _, err := e.store.dispatch(e.cfg, "orchestra", "P1", "claude", e.folder, "T1-r1", "existing review card", nil, nil, "T1"); err != nil {
		t.Fatal(err)
	}
	tasks, _ := e.store.List()
	r.reviewTick(tasks)

	parent, err := e.store.Get("T1")
	if err != nil || parent.Status != Reviewing || parent.ReviewTask != "T1-r1" || parent.ReviewedRuns != 1 {
		t.Fatalf("recovered parent=%+v err=%v", parent, err)
	}
	all, _ := e.store.List()
	if len(all) != 2 {
		t.Fatalf("expected 2 tasks (parent + child), got %d", len(all))
	}
}

func receiptInProject(t *testing.T, e env, reviewer string) {
	t.Helper()
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: reviewer,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Update("T1", func(tk *Task) error {
		tk.Status = Receipt
		tk.Runs = []Run{{Kind: "task", Started: time.Now(), Ended: time.Now(), Receipt: "RECEIPT T1: done"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewUndispatchableIsRecorded(t *testing.T) {
	e := setupReviewEnv(t)
	receiptInProject(t, e, "claude")
	delete(e.cfg.Agents, "claude") // reviewer removed after the project was created
	r := idleRunner(t, e)
	tasks, _ := e.store.List()
	r.reviewTick(tasks)
	p, _ := e.store.Get("T1")
	if p.Status != Receipt || p.ReviewVerdict != "unavailable" || !strings.Contains(p.ReviewText, "claude") || p.ReviewedRuns != 1 {
		t.Fatalf("parent=%+v", p)
	}
	if !e.store.needsLead(p) {
		t.Fatal("lead still waits on a review that can never run")
	}
}

func TestWaitDoesNotReturnBeforeReviewStarts(t *testing.T) {
	e := setupReviewEnv(t)
	receiptInProject(t, e, "claude")
	got, err := e.store.Wait("T1", 300*time.Millisecond, 50*time.Millisecond)
	if err != nil || got.Status != Receipt || e.store.needsLead(got) {
		t.Fatalf("wait returned a receipt awaiting review as needing the lead: %+v %v", got, err)
	}
}

func TestReviewStaleReceiptCannotStartAfterAccept(t *testing.T) {
	e := setupReviewEnv(t)
	receiptInProject(t, e, "claude")
	r := idleRunner(t, e)
	stale, _ := e.store.List()
	if _, err := e.store.Accept("T1"); err != nil {
		t.Fatal(err)
	}
	r.reviewTick(stale)
	child, err := e.store.Get("T1-r1")
	if err != nil || child.Status != Cancelled {
		t.Fatalf("stale review child=%+v err=%v", child, err)
	}
	if a := r.start(child); a != nil {
		finishRun(r, a)
		t.Fatal("stale review was launched")
	}
}

func TestOrphanReviewCannotStartAfterCrashAndAccept(t *testing.T) {
	e := setupReviewEnv(t)
	receiptInProject(t, e, "claude")
	child, err := e.store.dispatch(e.cfg, "orchestra", "P1", "claude", e.folder, "T1-r1", "review", nil, nil, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Accept("T1"); err != nil {
		t.Fatal(err)
	}
	r := idleRunner(t, e)
	if a := r.start(child); a != nil {
		finishRun(r, a)
		t.Fatal("orphan review was launched")
	}
	child, _ = e.store.Get(child.ID)
	if child.Status != Cancelled {
		t.Fatalf("orphan status=%s", child.Status)
	}
}

func TestReviewParentChangesAfterClaimPreventsLaunch(t *testing.T) {
	e := setupReviewEnv(t)
	receiptInProject(t, e, "claude")
	r := idleRunner(t, e)
	tasks, _ := e.store.List()
	r.reviewTick(tasks)
	child, err := e.store.Get("T1-r1")
	if err != nil {
		t.Fatal(err)
	}
	testHookAfterClaim = func(string) {
		_, err := e.store.Update("T1", func(parent *Task) error {
			parent.Status = Accepted // simulate interruption before child cancellation
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookAfterClaim = nil })
	if a := r.start(child); a != nil {
		finishRun(r, a)
		t.Fatal("review launched after parent changed")
	}
	child, _ = e.store.Get(child.ID)
	if child.Status != Cancelled {
		t.Fatalf("child status=%s", child.Status)
	}
}

func TestReviewRejectsReplacedParentFolder(t *testing.T) {
	e := setupReviewEnv(t)
	receiptInProject(t, e, "claude")
	if err := os.Rename(e.folder, e.folder+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.folder, 0o700); err != nil {
		t.Fatal(err)
	}
	r := idleRunner(t, e)
	tasks, _ := e.store.List()
	r.reviewTick(tasks)
	parent, _ := e.store.Get("T1")
	if parent.Status != Receipt || parent.ReviewVerdict != "unavailable" || !strings.Contains(parent.ReviewText, "工作目录") {
		t.Fatalf("parent=%+v", parent)
	}
	if _, err := e.store.Get("T1-r1"); err == nil {
		t.Fatal("review was created for replacement directory")
	}
}

// The process that accepts a reviewing task can die between accepting the
// parent and cancelling its review; the runner must still stop the review.
func TestRunningReviewStopsWhenParentCancelWasLost(t *testing.T) {
	e := setupReviewEnv(t)
	t.Setenv("REVIEW_SLEEP", "30")
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		ch, _ := e.store.Get("T1-r1")
		if ch != nil && ch.Status == Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Accept the parent only, as if the accepting process died before Cancel(child).
	if _, err := e.store.Update("T1", func(p *Task) error { p.Status = Accepted; return nil }); err != nil {
		t.Fatal(err)
	}
	if ch := waitDone(t, e, "T1-r1"); ch.Status != Cancelled {
		t.Fatalf("orphaned running review: %+v", ch)
	}
}

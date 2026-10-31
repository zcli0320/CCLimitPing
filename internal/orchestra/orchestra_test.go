package orchestra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCodex mimics `codex exec --json -o <file>`: prints thread.started, writes
// the final message, records its argv. SLOW makes it sleep, SILENT skips the answer.
const fakeCodex = `#!/bin/bash
[ -n "$IGNORE_TERM" ] && { trap '' TERM; touch "$ARGS_LOG.trap"; }
# Like Codex's shell tool: a child in its own process group.
[ -n "$SPAWN_GROUP" ] && { /usr/bin/perl -e 'setpgrp(0,0); exec "sleep", "60"' & echo $! > "$ARGS_LOG.child"; }
echo "$@" >> "$ARGS_LOG"
echo "$PATH" > "$ARGS_LOG.path"
prompt=$(cat)
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
echo '{"type":"thread.started","thread_id":"thread-1"}'
echo "pid $$" >&2
[ -n "$SLOW" ] && sleep "$SLOW"
[ -n "$SILENT" ] && { echo "boom" >&2; exit 3; }
id=$(printf '%s' "$prompt" | grep -oE 'TASK [A-Za-z0-9.-]+' | head -1 | cut -d' ' -f2)
printf 'RECEIPT %s: done\nChanged: none\nhome=%s\n' "${id:-rework}" "$CODEX_HOME" > "$out"
`

const fakeClaude = `#!/bin/bash
echo "$@" >> "$ARGS_LOG"
cat > /dev/null
echo '{"type":"result","session_id":"claude-s1","is_error":false,"result":"RECEIPT C1: done"}'
`

type env struct {
	cfg    Config
	store  Store
	folder string
	args   string
}

func setup(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	folder := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o700)
	write := func(name, body string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	args := filepath.Join(root, "args.log")
	t.Setenv("ARGS_LOG", args)
	agents := DefaultAgents(root)
	codex := agents["codex"]
	codex.Bin, codex.Env = write("codex", fakeCodex), []string{"CODEX_HOME=/runner-home"}
	agents["codex"] = codex
	claude := agents["claude"]
	claude.Bin = write("claude", fakeClaude)
	agents["claude"] = claude
	cfg := Config{Dir: filepath.Join(root, "state"), Agents: agents,
		AllowedRoots: []string{filepath.Join(root, "work")}, Timeout: time.Minute}
	return env{cfg: cfg, store: Store{Dir: cfg.Dir}, folder: folder, args: args}
}

func runFor(t *testing.T, e env) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	r := &Runner{Cfg: e.cfg, Store: e.store, Poll: 20 * time.Millisecond}
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel, done
}

func waitDone(t *testing.T, e env, id string) *Task {
	t.Helper()
	task, err := e.store.Wait(id, 10*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !task.Done() {
		t.Fatalf("task %s still %s", id, task.Status)
	}
	return task
}

func TestDispatchRejectsBadInput(t *testing.T) {
	e := setup(t)
	cases := []struct{ executor, folder, id, card, want string }{
		{"codex", e.folder, "bad id", "x", "任务编号无效"},
		{"nobody", e.folder, "T1", "x", "未知的执行代理"},
		{"codex", "relative", "T1", "x", "绝对路径"},
		{"codex", t.TempDir(), "T1", "x", "不在允许范围内"},
		{"codex", e.folder, "T1", "  ", "内容为空"},
	}
	for _, c := range cases {
		if _, err := e.store.Dispatch(e.cfg, "claude", c.executor, c.folder, c.id, c.card); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Dispatch(%q,%q,%q): err=%v, want %q", c.executor, c.folder, c.id, err, c.want)
		}
	}
	if _, err := e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1"); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestDispatchFollowsSymlinkedRootOnlyInside(t *testing.T) {
	e := setup(t)
	link := filepath.Join(filepath.Dir(e.folder), "link-out")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Dispatch(e.cfg, "claude", "codex", link, "T1", "x"); err == nil {
		t.Fatal("a symlink pointing outside the allowed root must be refused")
	}
}

func TestCodexTaskThenReworkResumesSession(t *testing.T) {
	e := setup(t)
	runFor(t, e)
	if _, err := e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1: probe"); err != nil {
		t.Fatal(err)
	}
	task := waitDone(t, e, "T1")
	if task.Status != Receipt || !strings.Contains(task.LastReceipt(), "RECEIPT T1: done") {
		t.Fatalf("status=%s receipt=%q", task.Status, task.LastReceipt())
	}
	if !strings.Contains(task.LastReceipt(), "home=/runner-home") {
		t.Fatalf("executor env not passed: %q", task.LastReceipt())
	}
	if task.Session != "thread-1" {
		t.Fatalf("session=%q", task.Session)
	}
	if _, err := e.store.Accept("T1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RequestRework("T1", "fix x"); err == nil {
		t.Fatal("an accepted task must not be reworked")
	}

	if _, err := e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T2", "TASK T2"); err != nil {
		t.Fatal(err)
	}
	waitDone(t, e, "T2")
	if _, err := e.store.RequestRework("T2", "REWORK T2: fix y"); err != nil {
		t.Fatal(err)
	}
	task = waitDone(t, e, "T2")
	if task.Status != Receipt || len(task.Runs) != 2 || task.Runs[1].Kind != "rework" {
		t.Fatalf("rework: status=%s runs=%+v", task.Status, task.Runs)
	}
	log, _ := os.ReadFile(e.args)
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "exec resume thread-1 ") || strings.Contains(last, " -C ") {
		t.Fatalf("rework argv = %q", last)
	}
	if !strings.Contains(lines[0], `sandbox_mode="workspace-write"`) {
		t.Fatalf("sandbox flag missing: %q", lines[0])
	}
}

func TestClaudeExecutor(t *testing.T) {
	e := setup(t)
	runFor(t, e)
	e.store.Dispatch(e.cfg, "codex", "claude", e.folder, "C1", "TASK C1")
	task := waitDone(t, e, "C1")
	if task.Status != Receipt || task.LastReceipt() != "RECEIPT C1: done" || task.Session != "claude-s1" {
		t.Fatalf("status=%s receipt=%q session=%q", task.Status, task.LastReceipt(), task.Session)
	}
	e.store.RequestRework("C1", "again")
	waitDone(t, e, "C1")
	log, _ := os.ReadFile(e.args)
	if !strings.Contains(string(log), "--resume claude-s1") {
		t.Fatalf("claude rework argv: %s", log)
	}
}

func TestNoFinalMessageFailsWithStderr(t *testing.T) {
	e := setup(t)
	t.Setenv("SILENT", "1")
	runFor(t, e)
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1")
	task := waitDone(t, e, "T1")
	if task.Status != Failed || !strings.Contains(task.LastReceipt(), "boom") || task.Runs[0].Exit != 3 {
		t.Fatalf("status=%s exit=%d msg=%q", task.Status, task.Runs[0].Exit, task.LastReceipt())
	}
}

func TestCancelStopsProcessGroup(t *testing.T) {
	e := setup(t)
	t.Setenv("SLOW", "30")
	runFor(t, e)
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1")
	var pid int
	for i := 0; i < 200 && pid == 0; i++ {
		if task, _ := e.store.Get("T1"); task.Status == Running {
			pid = task.Runs[0].PID
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("task never started")
	}
	if _, err := e.store.Cancel("T1"); err != nil {
		t.Fatal(err)
	}
	task := waitDone(t, e, "T1")
	if task.Status != Cancelled {
		t.Fatalf("status=%s", task.Status)
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group %d still alive: %v", pid, err)
	}
}

func TestQueuedCancelAndOneRunPerExecutor(t *testing.T) {
	e := setup(t)
	t.Setenv("SLOW", "1")
	runFor(t, e)
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "A", "TASK A")
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "B", "TASK B")
	time.Sleep(200 * time.Millisecond)
	if b, _ := e.store.Get("B"); b.Status != Queued {
		t.Fatalf("second codex task must wait, got %s", b.Status)
	}
	if _, err := e.store.Cancel("B"); err != nil {
		t.Fatal(err)
	}
	if b, _ := e.store.Get("B"); b.Status != Cancelled {
		t.Fatalf("queued cancel: %s", b.Status)
	}
	if a := waitDone(t, e, "A"); a.Status != Receipt {
		t.Fatalf("A: %s", a.Status)
	}
}

func TestRunnerRestartDoesNotRerun(t *testing.T) {
	e := setup(t)
	task, _ := e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1")
	e.store.Update(task.ID, func(t *Task) error {
		t.Status = Running
		t.Runs = []Run{{Kind: "task", Started: time.Now()}}
		return nil
	})
	runFor(t, e)
	task = waitDone(t, e, "T1")
	if task.Status != Failed || !strings.Contains(task.LastReceipt(), "未重新执行") {
		t.Fatalf("status=%s msg=%q", task.Status, task.LastReceipt())
	}
	if _, err := os.Stat(e.args); err == nil {
		t.Fatal("the stale task was rerun")
	}
}

func TestSecondRunnerIsRefused(t *testing.T) {
	e := setup(t)
	runFor(t, e)
	time.Sleep(100 * time.Millisecond)
	r := &Runner{Cfg: e.cfg, Store: e.store}
	if err := r.Run(context.Background()); !errors.Is(err, ErrRunnerBusy) {
		t.Fatalf("second runner: %v", err)
	}
}

func TestAntigravityIsReportedUnavailable(t *testing.T) {
	e := setup(t)
	runFor(t, e)
	e.store.Dispatch(e.cfg, "claude", "antigravity", e.folder, "G1", "TASK G1")
	task := waitDone(t, e, "G1")
	if task.Status != Failed || !strings.Contains(task.LastReceipt(), "Antigravity") {
		t.Fatalf("status=%s msg=%q", task.Status, task.LastReceipt())
	}
}

func TestShutdownKillsExecutorThatIgnoresTerm(t *testing.T) {
	e := setup(t)
	t.Setenv("SLOW", "60")
	t.Setenv("IGNORE_TERM", "1")
	old := ShutdownGrace
	ShutdownGrace = 300 * time.Millisecond
	t.Cleanup(func() { ShutdownGrace = old })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&Runner{Cfg: e.cfg, Store: e.store, Poll: 20 * time.Millisecond}).Run(ctx) }()
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1")
	var pid int
	for i := 0; i < 200 && pid == 0; i++ {
		if task, _ := e.store.Get("T1"); task.Status == Running {
			pid = task.Runs[0].PID
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("task never started")
	}
	for i := 0; i < 200; i++ { // wait until the script ignores SIGTERM
		if _, err := os.Stat(e.args + ".trap"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := time.Now()
	cancel()
	select {
	case <-done:
		if waited := time.Since(stopped); waited < ShutdownGrace {
			t.Fatalf("runner stopped after %v, before the grace period: the executor did not ignore SIGTERM", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop within the grace period")
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("executor group %d survived shutdown: %v", pid, err)
	}
	if task, _ := e.store.Get("T1"); task.Status != Failed || !strings.Contains(task.LastReceipt(), "执行器已关闭") {
		t.Fatalf("status=%s msg=%q", task.Status, task.LastReceipt())
	}
}

func TestCancelKillsChildrenInOtherProcessGroups(t *testing.T) {
	e := setup(t)
	t.Setenv("SLOW", "60")
	t.Setenv("SPAWN_GROUP", "1")
	runFor(t, e)
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1")
	var child int
	for i := 0; i < 250 && child == 0; i++ {
		if b, err := os.ReadFile(e.args + ".child"); err == nil {
			fmt.Sscan(string(b), &child)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("child never started")
	}
	time.Sleep(100 * time.Millisecond)
	if pgid, _ := syscall.Getpgid(child); pgid != child {
		t.Fatalf("test setup: child should lead its own group, pgid=%d", pgid)
	}
	if _, err := e.store.Cancel("T1"); err != nil {
		t.Fatal(err)
	}
	if task := waitDone(t, e, "T1"); task.Status != Cancelled {
		t.Fatalf("status=%s", task.Status)
	}
	for i := 0; i < 50; i++ {
		if errors.Is(syscall.Kill(child, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d in its own process group survived the cancel", child)
}

func TestExecutorGetsLoginShellPath(t *testing.T) {
	e := setup(t)
	shell := filepath.Join(t.TempDir(), "fakeshell")
	os.WriteFile(shell, []byte("#!/bin/sh\necho 'motd noise'\nPATH=/login/bin:/usr/bin:/bin\nexport PATH\nexec /bin/sh -c \"$2\"\n"), 0o700)
	t.Setenv("SHELL", shell)
	runFor(t, e)
	e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "T1", "TASK T1")
	waitDone(t, e, "T1")
	got, _ := os.ReadFile(e.args + ".path")
	if strings.TrimSpace(string(got)) != "/login/bin:/usr/bin:/bin" {
		t.Fatalf("executor PATH = %q", got)
	}
}

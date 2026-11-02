package orchestra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func checksFixture(t *testing.T, sandbox bool) env {
	t.Helper()
	e := setup(t)
	root, err := os.MkdirTemp(filepath.Dir(filepath.Dir(filepath.Dir(mustGetwd(t)))), "orc9-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	if err = os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	os.MkdirAll(filepath.Join(home, ".codex"), 0700)
	a := e.cfg.Agents["codex"]
	a.Env = []string{"CODEX_HOME=" + filepath.Join(home, ".codex")}
	prompt := filepath.Join(home, "prompts")
	t.Setenv("ORC9_PROMPTS", prompt)
	t.Setenv("ORC9_REAL_CODEX", bundledCodex)
	script := `#!/bin/sh
if [ "$1" = sandbox ]; then
  if [ "$ORC9_SANDBOX" = 1 ]; then exec "$ORC9_REAL_CODEX" "$@"; fi
  exit 1
fi
printf '%s\n' "$*" >> "$ARGS_LOG"
cat >> "$ORC9_PROMPTS"
if [ -n "$ORC9_MUTATE" ]; then printf changed >> "$ORC9_MUTATE"; fi
while [ "$1" != "-o" ] && [ "$#" -gt 0 ]; do shift; done
shift; printf 'RECEIPT T: done\n' > "$1"
printf '{"type":"thread.started","thread_id":"fixture-session"}\n'
`
	t.Setenv("ORC9_SANDBOX", map[bool]string{true: "1", false: "0"}[sandbox])
	if err := os.WriteFile(a.Bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e.cfg.Agents["codex"] = a
	return e
}

func TestOutOfScopeAloneDoesNotTriggerRework(t *testing.T) {
	e := checksFixture(t, false)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", e.folder}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	run("init", "-q")
	path := filepath.Join(e.folder, "outside.txt")
	os.WriteFile(path, []byte("base"), 0600)
	run("add", "outside.txt")
	run("-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "-qm", "base")
	os.WriteFile(path, []byte("already-dirty"), 0600)
	t.Setenv("ORC9_MUTATE", path)
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", nil, []string{"inside/"}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "T1")
	if task.Status != Receipt || len(task.Runs) != 1 || task.AutoReworkCount != 0 || task.Runs[0].Changes == nil || len(task.Runs[0].Changes.OutOfScope) != 1 || task.Runs[0].Changes.OutOfScope[0] != "outside.txt" {
		t.Fatalf("%+v", task)
	}
}
func TestChecksPassAndBoundedAutoRework(t *testing.T) {
	if _, err := os.Stat(bundledCodex); err != nil {
		t.Skip("bundled sandbox unavailable")
	}
	for _, tc := range []struct {
		name, command string
		max, wantRuns int
		status        string
	}{{"pass", "printf success", 1, 1, "passed"}, {"fail-and-rework", "printf deliberate-failure; exit 7", 1, 2, "failed"}, {"disabled", "exit 8", 0, 1, "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := checksFixture(t, true)
			s := DefaultSettings()
			s.AutoReworkMax = tc.max
			if err := WriteSettings(e.cfg.Dir, s); err != nil {
				t.Fatal(err)
			}
			if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{tc.command}, nil); err != nil {
				t.Fatal(err)
			}
			runFor(t, e)
			task := waitDone(t, e, "T1")
			if task.Status != Receipt || len(task.Runs) != tc.wantRuns || len(task.Runs[len(task.Runs)-1].Checks) != 1 || task.Runs[len(task.Runs)-1].Checks[0].Status != tc.status {
				t.Fatalf("%+v", task)
			}
			logInfo, err := os.Stat(task.Runs[len(task.Runs)-1].Checks[0].Log)
			if err != nil || logInfo.Mode().Perm() != 0600 {
				t.Fatalf("check log permissions: %v %v", logInfo, err)
			}
			if tc.wantRuns == 2 {
				args, _ := os.ReadFile(e.args)
				prompts, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), "prompts"))
				if !strings.Contains(string(args), "resume fixture-session") || !strings.Contains(string(prompts), "deliberate-failure") || task.Runs[1].Kind != "auto_rework" || task.AutoReworkCount != 1 {
					t.Fatalf("args=%s prompts=%s task=%+v", args, prompts, task)
				}
			}
		})
	}
}
func TestNoSandboxNeverRunsCommand(t *testing.T) {
	e := checksFixture(t, false)
	marker := filepath.Join(e.folder, "marker")
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{"touch marker"}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "T1")
	if task.Status != Receipt || len(task.Runs[0].Checks) != 1 || task.Runs[0].Checks[0].Status != "unavailable" {
		t.Fatalf("%+v", task)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unsandboxed check executed")
	}
}

func TestPretendSandboxFailsClosed(t *testing.T) {
	e := checksFixture(t, false)
	agent := e.cfg.Agents["codex"]
	script := `#!/bin/sh
if [ "$1" = sandbox ]; then
  if [ "$2" = --help ]; then printf 'sandbox --permission-profile'; exit 0; fi
  while [ "$1" != -- ] && [ "$#" -gt 0 ]; do shift; done
  shift; exec "$@"
fi
cat >/dev/null
while [ "$1" != "-o" ] && [ "$#" -gt 0 ]; do shift; done
shift; echo 'RECEIPT T: done' > "$1"
echo '{"type":"thread.started","thread_id":"fixture-session"}'
`
	if err := os.WriteFile(agent.Bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{"touch marker"}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "T1")
	if task.Status != Receipt || len(task.Runs[0].Checks) != 1 || task.Runs[0].Checks[0].Status != "unavailable" {
		t.Fatalf("%+v", task)
	}
	if _, err := os.Stat(filepath.Join(e.folder, "marker")); !os.IsNotExist(err) {
		t.Fatal("unconfined check ran")
	}
}

func TestSandboxCheckCannotWriteOutsideTaskFolder(t *testing.T) {
	if _, err := os.Stat(bundledCodex); err != nil {
		t.Skip("bundled sandbox unavailable")
	}
	parent, err := os.MkdirTemp(filepath.Dir(filepath.Dir(filepath.Dir(mustGetwd(t)))), "orc9-boundary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	folder := filepath.Join(parent, "task")
	if err = os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	e := checksFixture(t, true)
	e.cfg.AllowedRoots = []string{parent}
	e.folder = folder
	s := DefaultSettings()
	s.AutoReworkMax = 0
	if err = WriteSettings(e.cfg.Dir, s); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside-marker")
	command := "printf x > '" + outside + "'"
	if _, err = e.store.DispatchWithEvidence(e.cfg, "lead", "codex", folder, "T1", "card", []string{command}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "T1")
	if task.Status != Receipt || len(task.Runs[0].Checks) != 1 || task.Runs[0].Checks[0].Status != "failed" {
		t.Fatalf("%+v", task)
	}
	if _, err = os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("sandbox wrote outside task folder")
	}
}
func mustGetwd(t *testing.T) string {
	t.Helper()
	d, e := os.Getwd()
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func TestDirtyGitFileAndScopeEvidence(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "file"), []byte("original"), 0600)
	run("add", "file")
	run("-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "-qm", "fixture")
	os.WriteFile(filepath.Join(dir, "file"), []byte("dirty-before"), 0600)
	before := snapshotGit(dir)
	if before.status != "available" {
		t.Fatal(before.status)
	}
	os.WriteFile(filepath.Join(dir, "file"), []byte("dirty-after"), 0600)
	after := changesSince(dir, before, []string{"allowed/"})
	if len(after.Changed) != 1 || after.Changed[0] != "file" || len(after.OutOfScope) != 1 {
		t.Fatalf("%+v", after)
	}
	if got := snapshotGit(t.TempDir()); got.status != "unavailable" {
		t.Fatal(got)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "inner"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "sub/inner")
	run("-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "-qm", "sub")
	inside := filepath.Join(dir, "sub")
	if err := os.WriteFile(filepath.Join(inside, "inner"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	old := snapshotGit(inside)
	if old.status != "available" {
		t.Fatal(old)
	}
	os.WriteFile(filepath.Join(inside, "inner"), []byte("changed"), 0600)
	got := changesSince(inside, old, []string{"other/"})
	if len(got.Changed) != 1 || got.Changed[0] != "inner" || len(got.OutOfScope) != 1 {
		t.Fatalf("nested: %+v", got)
	}
	baseline := snapshotGit(inside)
	os.WriteFile(filepath.Join(inside, "new"), []byte("committed"), 0600)
	run("add", "sub/new")
	run("-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "-qm", "new")
	headChanged := changesSince(inside, baseline, nil)
	if !headChanged.HeadChanged || len(headChanged.Changed) != 1 || headChanged.Changed[0] != "new" {
		t.Fatalf("head change: %+v", headChanged)
	}
	large, err := os.Create(filepath.Join(inside, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate((50 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	_ = large.Close()
	if snapshot := snapshotGit(inside); snapshot.status != "unavailable" || snapshot.reason != "too large" {
		t.Fatalf("large: %+v", snapshot)
	}
}
func TestCancelDuringSandboxedCheckingKillsTree(t *testing.T) {
	if _, err := os.Stat(bundledCodex); err != nil {
		t.Skip("bundled sandbox unavailable")
	}
	e := checksFixture(t, true)
	pidfile := filepath.Join(e.folder, "child.pid")
	command := `python3 -c 'import os,time; os.setsid(); open("child.pid","w").write(str(os.getpid())); time.sleep(300)' & wait`
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{command}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	deadline := time.Now().Add(10 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidfile)
		if err == nil {
			fmtS := strings.TrimSpace(string(b))
			_, _ = fmt.Sscan(fmtS, &pid)
			if pid > 0 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("check child did not start")
	}
	if _, err := e.store.Cancel("T1"); err != nil {
		t.Fatal(err)
	}
	task := waitDone(t, e, "T1")
	if task.Status != Cancelled {
		t.Fatalf("%+v", task)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("check child survived cancel")
}

func TestShutdownDuringSandboxedCheckingKillsTree(t *testing.T) {
	if _, err := os.Stat(bundledCodex); err != nil {
		t.Skip("bundled sandbox unavailable")
	}
	e := checksFixture(t, true)
	pidfile := filepath.Join(e.folder, "child.pid")
	command := `python3 -c 'import os,time; os.setsid(); open("child.pid","w").write(str(os.getpid())); time.sleep(300)' & wait`
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{command}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	r := &Runner{Cfg: e.cfg, Store: e.store, Poll: 20 * time.Millisecond}
	go func() { done <- r.Run(ctx) }()
	defer cancel()
	deadline := time.Now().Add(10 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidfile)
		if err == nil {
			_, _ = fmt.Sscan(strings.TrimSpace(string(b)), &pid)
			if pid > 0 {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("check child did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("check child survived shutdown")
}

func TestAutoReworkStillPassesQuotaGate(t *testing.T) {
	e := checksFixture(t, false)
	e.cfg.Home = t.TempDir()
	e.cfg.Quota = func(context.Context) (float64, bool) { return 0, true }
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.Update("T1", func(t *Task) error {
		t.Rework = "fix"
		t.Session = "fixture-session"
		t.AutoReworkPending = true
		t.AutoReworkCount = 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r := Runner{Cfg: e.cfg, Store: e.store, Logf: func(string, ...any) {}}
	r.tick(map[string]*active{})
	task, _ := e.store.Get("T1")
	if task.Status != Queued || len(task.Runs) != 0 {
		t.Fatalf("quota gate bypassed: %+v", task)
	}
}

func TestCheckTimeoutIsRecorded(t *testing.T) {
	if _, err := os.Stat(bundledCodex); err != nil {
		t.Skip("bundled sandbox unavailable")
	}
	e := checksFixture(t, true)
	s := DefaultSettings()
	s.CheckTimeoutSeconds = 1
	s.AutoReworkMax = 0
	if err := WriteSettings(e.cfg.Dir, s); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "codex", e.folder, "T1", "card", []string{"sleep 30"}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "T1")
	if task.Status != Receipt || len(task.Runs[0].Checks) != 1 || !task.Runs[0].Checks[0].TimedOut || task.Runs[0].Checks[0].Status != "failed" {
		t.Fatalf("%+v", task)
	}
}

func TestReworkEvidenceListsAllFailuresWithinLimit(t *testing.T) {
	var checks []CheckResult
	for i := 0; i < 10; i++ {
		checks = append(checks, CheckResult{Command: fmt.Sprintf("check-%d-%s", i, strings.Repeat("x", 400)), Exit: i + 1, Tail: strings.Repeat("failure ", 2000)})
	}
	result := reworkEvidence(checks)
	if len([]rune(result)) > 3000 {
		t.Fatalf("too long: %d", len([]rune(result)))
	}
	for i := 0; i < 10; i++ {
		if !strings.Contains(result, fmt.Sprintf("check-%d-", i)) || !strings.Contains(result, fmt.Sprintf("exit %d", i+1)) {
			t.Fatalf("failure %d missing", i)
		}
	}
}

func TestEvidenceFieldsOmittedWithoutChecksOrScope(t *testing.T) {
	task := Task{ID: "T", Status: Receipt, Runs: []Run{{Kind: "task", Receipt: "done"}}}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"checks"`, `"scope"`, `"changed"`, `"auto_rework_count"`, `"changes"`, `"phase"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("unexpected %s in %s", key, b)
		}
	}
	if Evidence(&task) != nil {
		t.Fatal("unexpected evidence for legacy task")
	}
}

package orchestra

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSettingsAndQuotaGate(t *testing.T) {
	e := setup(t)
	e.cfg.Home = t.TempDir()
	s, err := ReadSettings(e.cfg.Dir)
	if err != nil || s != DefaultSettings() {
		t.Fatalf("default: %+v %v", s, err)
	}
	s.MinRemaining = 15
	if err = WriteSettings(e.cfg.Dir, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadSettings(e.cfg.Dir); got.Account != s.Account || got.MinRemaining != s.MinRemaining || got.Revision == "" {
		t.Fatal(got)
	}
	value, known, calls := float64(5), true, 0
	e.cfg.Quota = func(context.Context) (float64, bool) { calls++; return value, known }
	r := Runner{Cfg: e.cfg, Store: e.store, Logf: func(string, ...any) {}}
	if r.quotaAllowed() {
		t.Fatal("low quota allowed")
	}
	if r.quotaAllowed() || calls != 1 {
		t.Fatal("too many checks")
	}
	status, err := ReadRunnerStatus(e.cfg.Dir)
	if err != nil || !status.Paused || status.Remaining == nil || *status.Remaining != 5 {
		t.Fatalf("%+v %v", status, err)
	}
	r.quotaStatus.CheckedAt = time.Now().Add(-time.Minute)
	known = false
	if r.quotaAllowed() {
		t.Fatal("unknown allowed")
	}
	r.quotaStatus.CheckedAt = time.Now().Add(-time.Minute)
	value, known = 40, true
	if !r.quotaAllowed() {
		t.Fatal("healthy quota blocked")
	}
	s.Account = "separate"
	if err = WriteSettings(e.cfg.Dir, s); err != nil {
		t.Fatal(err)
	}
	before := calls
	if !r.quotaAllowed() || calls != before {
		t.Fatal("separate account queried Desktop quota")
	}
}

func TestUnknownQuotaLeavesCodexQueued(t *testing.T) {
	e := setup(t)
	e.cfg.Home = t.TempDir()
	e.cfg.Quota = func(context.Context) (float64, bool) { return 0, false }
	if _, err := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "Q2", "card"); err != nil {
		t.Fatal(err)
	}
	r := Runner{Cfg: e.cfg, Store: e.store, Logf: func(string, ...any) {}}
	r.tick(map[string]*active{})
	task, err := e.store.Get("Q2")
	if err != nil || task.Status != Queued || len(task.Runs) != 0 {
		t.Fatalf("%+v %v", task, err)
	}
	status, err := ReadRunnerStatus(e.cfg.Dir)
	if err != nil || !status.Paused || status.Remaining != nil {
		t.Fatalf("%+v %v", status, err)
	}
}

func TestQuotaExhaustedAndResume(t *testing.T) {
	e := setup(t)
	e.cfg.Home = t.TempDir()
	e.cfg.Quota = func(context.Context) (float64, bool) { return 80, true }
	binary := e.cfg.Agents["codex"].Bin
	script := `#!/bin/sh
echo "$@" >> "$ARGS_LOG"
case "$*" in *resume*) echo '{"type":"thread.started","thread_id":"session-x"}'; while [ "$1" != "-o" ]; do shift; done; shift; echo done > "$1" ;; *) echo '{"type":"thread.started","thread_id":"session-x"}'; echo '{"type":"turn.failed","error":{"message":"usage_limit reached"}}'; exit 1 ;; esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "Q1", "card")
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "Q1")
	if task.Status != QuotaExhausted || task.Session != "session-x" {
		t.Fatalf("%+v", task)
	}
	status, err := ReadRunnerStatus(e.cfg.Dir)
	if err != nil || !status.Paused {
		t.Fatalf("status %+v %v", status, err)
	}
	if _, err = e.store.Resume("Q1"); err != nil {
		t.Fatal(err)
	}
	// Expire the persisted gate via an independent runner's test clock.
	r := Runner{Cfg: e.cfg, Store: e.store, Logf: func(string, ...any) {}, path: os.Getenv("PATH")}
	r.quotaSettings = DefaultSettings()
	r.quotaStatus = RunnerStatus{CheckedAt: time.Now().Add(-time.Minute)}
	if !r.quotaAllowed() {
		t.Fatal("resume gate blocked")
	}
	a := r.start(taskWithID(t, e.store, "Q1"))
	if a == nil {
		t.Fatal("resume did not launch")
	}
	<-a.done
	r.finish(a, "")
	task, _ = e.store.Get("Q1")
	if task.Status != Receipt || task.Session != "session-x" {
		t.Fatalf("%+v", task)
	}
	b, _ := os.ReadFile(e.args)
	if !strings.Contains(string(b), "resume session-x") {
		t.Fatalf("args %s", b)
	}
}
func taskWithID(t *testing.T, s Store, id string) *Task {
	t.Helper()
	v, e := s.Get(id)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func TestUsageLimitIgnoresOrdinaryMessage(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "out")
	errPath := filepath.Join(d, "err")
	os.WriteFile(out, []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"usage limit\"}}\n"), 0600)
	if usageLimit(out, errPath) {
		t.Fatal("ordinary message is not an error")
	}
	os.WriteFile(out, []byte("{\"type\":\"error\",\"message\":\"usage limit reached\"}\n"), 0600)
	if !usageLimit(out, errPath) {
		t.Fatal("missing structured error")
	}
}

func TestSeparateModeUsesSeparateHome(t *testing.T) {
	e := setup(t)
	home := t.TempDir()
	e.cfg.Home = home
	if err := WriteSettings(e.cfg.Dir, Settings{Account: "separate", MinRemaining: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "S1", "card"); err != nil {
		t.Fatal(err)
	}
	path := e.cfg.Agents["codex"].Bin
	script := "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > \"$ARGS_LOG\"\nwhile [ \"$1\" != \"-o\" ]; do shift; done\nshift\necho done > \"$1\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "S1")
	if task.Status != Receipt {
		t.Fatalf("%+v", task)
	}
	b, err := os.ReadFile(e.args)
	if err != nil || string(b) != filepath.Join(home, ".codex-runner") {
		t.Fatalf("home %q: %v", b, err)
	}
}

// A run that finished with a receipt stays a receipt even if a log line
// mentions a usage limit (e.g. a warning near the end of the window).
func TestFinishedRunWithLimitWarningIsReceipt(t *testing.T) {
	e := setup(t)
	e.cfg.Home = t.TempDir()
	e.cfg.Quota = func(context.Context) (float64, bool) { return 80, true }
	script := `#!/bin/sh
echo '{"type":"thread.started","thread_id":"s1"}'
echo 'warning: approaching usage limit' >&2
while [ "$1" != "-o" ]; do shift; done; shift; echo 'RECEIPT W1: done' > "$1"
`
	if err := os.WriteFile(e.cfg.Agents["codex"].Bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e.store.Dispatch(e.cfg, "lead", "codex", e.folder, "W1", "card")
	runFor(t, e)
	task := waitDone(t, e, "W1")
	if task.Status != Receipt || task.LastReceipt() != "RECEIPT W1: done" {
		t.Fatalf("status=%s msg=%q", task.Status, task.LastReceipt())
	}
	if status, err := ReadRunnerStatus(e.cfg.Dir); err == nil && status.Paused {
		t.Fatalf("queue paused after a finished run: %+v", status)
	}
}

package orchestra

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The executor is a fake; checksFixture's Codex wrapper only invokes the
// bundled local sandbox and never calls a model or an account.
func setupFakeAGY(t *testing.T, sandbox bool) env {
	t.Helper()
	e := checksFixture(t, sandbox)
	agy := e.cfg.Agents["antigravity"]
	agy.Bin = filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$ARGS_LOG"
case "$*" in *--conversation*) touch fixed;; *) printf change >> outside.txt;; esac
printf '%s\n' '{"conversation_id":"agy-fixture","status":"SUCCESS","response":"RECEIPT AGY: done"}'
`
	if err := os.WriteFile(agy.Bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e.cfg.Agents["antigravity"] = agy
	return e
}

func TestAGYChecksReworkAndScope(t *testing.T) {
	e := setupFakeAGY(t, true)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", e.folder}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(e.folder, "outside.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "outside.txt")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "-qm", "fixture")
	checks := []string{"printf passed", "test -f fixed || { echo expected-failure; exit 4; }"}
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "antigravity", e.folder, "A1", "card", checks, []string{"allowed/"}); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "A1")
	if task.Status != Receipt || task.Session != "agy-fixture" || task.AutoReworkCount != 1 || len(task.Runs) != 2 {
		t.Fatalf("task=%+v", task)
	}
	first, last := task.Runs[0], task.Runs[1]
	if len(first.Checks) != 2 || first.Checks[0].Status != "passed" || first.Checks[1].Status != "failed" || !strings.Contains(first.Checks[1].Tail, "expected-failure") {
		t.Fatalf("first=%+v", first)
	}
	if len(last.Checks) != 2 || last.Checks[0].Status != "passed" || last.Checks[1].Status != "passed" || last.Kind != "auto_rework" {
		t.Fatalf("last=%+v", last)
	}
	if first.Changes == nil || first.Changes.Status != "available" || len(first.OutOfScope) != 1 || first.OutOfScope[0] != "outside.txt" {
		t.Fatalf("changes=%+v", first)
	}
	argv, err := os.ReadFile(e.args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--conversation\nagy-fixture\n") {
		t.Fatalf("resume args missing: %s", argv)
	}
}

func TestAGYChecksUnavailableWithoutCodexSandbox(t *testing.T) {
	e := setupFakeAGY(t, false)
	codex := e.cfg.Agents["codex"]
	codex.Bin = filepath.Join(t.TempDir(), "missing-codex")
	e.cfg.Agents["codex"] = codex
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "antigravity", e.folder, "A1", "card", []string{"touch marker"}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "A1")
	if task.Status != Receipt || len(task.Runs) != 1 || len(task.Runs[0].Checks) != 1 || task.Runs[0].Checks[0].Status != "unavailable" {
		t.Fatalf("%+v", task)
	}
	if _, err := os.Stat(filepath.Join(e.folder, "marker")); !os.IsNotExist(err) {
		t.Fatal("check ran without sandbox")
	}
}

func TestClaudeChecksIgnoreCodexUsageLimitText(t *testing.T) {
	e := checksFixture(t, true)
	a := e.cfg.Agents["claude"]
	a.Bin = filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"session_id":"claude-fixture","result":"RECEIPT C: done","is_error":false}'
echo 'usage limit warning' >&2
exit 1
`
	if err := os.WriteFile(a.Bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e.cfg.Agents["claude"] = a
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "claude", e.folder, "C1", "card", []string{"printf ok"}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "C1")
	if task.Status != Receipt || len(task.Runs) != 1 || len(task.Runs[0].Checks) != 1 || task.Runs[0].Checks[0].Status != "passed" {
		t.Fatal(fmt.Sprintf("%+v", task))
	}
}

func TestClaudeAutoReworkResumesSession(t *testing.T) {
	e := checksFixture(t, true)
	a := e.cfg.Agents["claude"]
	a.Bin = filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$ARGS_LOG"
cat >/dev/null
case "$*" in *--resume*) touch fixed;; esac
printf '%s\n' '{"session_id":"claude-fixture","result":"RECEIPT C: done","is_error":false}'
`
	if err := os.WriteFile(a.Bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e.cfg.Agents["claude"] = a
	if _, err := e.store.DispatchWithEvidence(e.cfg, "lead", "claude", e.folder, "C1", "card", []string{"test -f fixed || { echo failed-once; exit 1; }"}, nil); err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	task := waitDone(t, e, "C1")
	if task.Status != Receipt || task.AutoReworkCount != 1 || len(task.Runs) != 2 || task.Runs[1].Kind != "auto_rework" || task.Runs[1].Checks[0].Status != "passed" {
		t.Fatalf("%+v", task)
	}
	argv, err := os.ReadFile(e.args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--resume\nclaude-fixture\n") {
		t.Fatalf("resume args missing: %s", argv)
	}
}

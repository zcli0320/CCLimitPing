package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wavever/CCLimitPing/internal/orchestra"
)

// Exercise the actual command entry in a marked temporary home. Invalid
// session/authorization values must reach their normal guards without ever
// enabling a handoff or touching the user's account files.
func TestDesktopHandoffCommandRoutesThroughValidation(t *testing.T) {
	if child := os.Getenv("CODEX_HELPER_TEST_DESKTOP_ENTRY"); child != "" {
		switch child {
		case "begin":
			os.Args = []string{"codex-helper", "handoff", "desktop-begin", "invalid-session", "10"}
		case "run":
			os.Args = []string{"codex-helper", "handoff", "desktop-run", "invalid-session", "10", "invalid-authorization"}
		default:
			os.Exit(3)
		}
		main()
		os.Exit(0)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".codex-helper-test-root"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(op string) ([]byte, error) {
		child := exec.Command(exe, "-test.run=^TestDesktopHandoffCommandRoutesThroughValidation$")
		child.Env = append(os.Environ(), "CODEX_HELPER_NATIVE_PREVIEW=1", "CODEX_HELPER_TEST_HOME="+home, "CODEX_HELPER_TEST_DESKTOP_ENTRY="+op)
		return child.CombinedOutput()
	}
	out, err := run("begin")
	if err != nil {
		t.Fatalf("Desktop begin did not reach validation: %v: %s", err, out)
	}
	var state struct {
		Enabled bool   `json:"enabled"`
		Message string `json:"message"`
	}
	if json.Unmarshal(out, &state) != nil || state.Enabled || !strings.Contains(state.Message, "会话或阈值无效") {
		t.Fatalf("Desktop begin did not return the expected guarded result: %s", out)
	}
	out, err = run("run")
	if err == nil || !strings.Contains(string(out), "切号授权已变化") {
		t.Fatalf("Desktop run must reject missing authorization, output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "Application Support", "CodexHelper-AccountPreview", "handoff", "state.json")); !os.IsNotExist(err) {
		t.Fatal("invalid Desktop command created a handoff authorization")
	}
}

func TestProjectUpdateCLIFlagParsingAndStoreCalls(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	dir1 := filepath.Join(work, "p1")
	dir2 := filepath.Join(work, "p2")
	for _, d := range []string{dir1, dir2} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := orchestra.DefaultConfig(root, filepath.Join(root, "state"))
	cfg.AllowedRoots = []string{work}
	store := orchestra.Store{Dir: cfg.Dir}
	if _, err := store.CreateProject(cfg, orchestra.Project{
		ID: "P1", Name: "Orig", Folder: dir1, Lead: "claude", Executor: "codex",
		Reviewer: "claude", Checks: []string{"true"}, Scope: []string{"src/"},
	}); err != nil {
		t.Fatal(err)
	}

	// Only flags provided are changed.
	id, patch, err := parseProjectUpdateFlags([]string{"P1", "--name", "NewName", "--reviewer", "", "--check", "go vet ./...", "--check", "go test ./...", "--sync-agents-md=true"})
	if err != nil || id != "P1" || patch.Folder != nil || patch.Lead != nil || patch.Executor != nil || patch.Scope != nil {
		t.Fatalf("id=%q patch=%+v err=%v", id, patch, err)
	}
	p, err := store.UpdateProject(cfg, id, patch)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "NewName" || p.Reviewer != "" || p.Lead != "claude" || len(p.Checks) != 2 || len(p.Scope) != 1 || !p.SyncAgentsMD {
		t.Fatalf("after first CLI update: %+v", p)
	}

	// Clear checks and scope, update folder, lead, executor, sync-agents-md=false.
	id, patch, err = parseProjectUpdateFlags([]string{"P1", "--folder", dir2, "--to", "claude", "--lead", "codex", "--clear-checks", "--clear-scope", "--sync-agents-md=false"})
	if err != nil {
		t.Fatal(err)
	}
	p, err = store.UpdateProject(cfg, id, patch)
	if err != nil {
		t.Fatal(err)
	}
	if p.Executor != "claude" || p.Lead != "codex" || len(p.Checks) != 0 || len(p.Scope) != 0 || p.SyncAgentsMD || !strings.HasSuffix(p.Folder, "/p2") {
		t.Fatalf("after second CLI update: %+v", p)
	}
	if gotPath := orchestra.RulesPath(p); !strings.HasSuffix(gotPath, "/p2/.codex-helper/projects/P1.md") {
		t.Fatalf("RulesPath=%q", gotPath)
	}
}

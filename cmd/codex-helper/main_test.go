package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

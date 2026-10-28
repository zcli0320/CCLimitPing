package assistant

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDesktopProcessIdentityAndExit(t *testing.T) {
	const app = "/Applications/ChatGPT.app"
	list := "100 1 501 Wed Oct 28 00:49:14 2026 /Applications/ChatGPT.app/Contents/MacOS/ChatGPT\n" +
		"101 100 501 Wed Oct 28 00:49:16 2026 /Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server\n" +
		"102 1 501 Wed Oct 28 00:49:17 2026 /other/codex app-server\n"
	procs, err := parseDesktopProcessList(list)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := findDesktopProcessPair(procs, 501, app)
	if err != nil || pair.App.PID != 100 || pair.Server.PID != 101 {
		t.Fatal("could not identify direct app backend")
	}
	if desktopOldProcessesExited(pair, procs, 501, app) {
		t.Fatal("live app accepted as exited")
	}
	if desktopOldProcessesExited(pair, procs[1:], 501, app) {
		t.Fatal("orphaned backend accepted as exited")
	}
	if !desktopOldProcessesExited(pair, procs[2:], 501, app) {
		t.Fatal("unrelated CLI should not be treated as Desktop")
	}
	newApp := procs[0]
	newApp.PID = 500
	if desktopOldProcessesExited(pair, append(procs[2:], newApp), 501, app) {
		t.Fatal("new app already started while switching")
	}
	if _, err := findDesktopProcessPair(append(procs, procs[0]), 501, app); err == nil {
		t.Fatal("duplicate Desktop app accepted")
	}
}

func TestDesktopEnvironmentRequiresExpectedRoot(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"command HOME=/Users/test PATH=/bin", true},
		{"command HOME=/Users/test CODEX_HOME=/Users/test/.codex", true},
		{"command HOME=/Users/test CODEX_HOME=/Users/test/.codex-cli", false},
		{"command HOME=/Users/other", false},
		{"command HOME=/Users/test HOME=/Users/other", false},
	} {
		if desktopEnvironmentMatches(tc.env, "/Users/test") != tc.want {
			t.Fatalf("unexpected environment check for %s", tc.env)
		}
	}
}

func TestDesktopProcessTreeRejectsOrphanedChild(t *testing.T) {
	root := desktopProcessIdentity{PID: 1, UID: 501, Start: "root"}
	child := desktopProcessIdentity{PID: 2, PPID: 1, UID: 501, Start: "child"}
	grandchild := desktopProcessIdentity{PID: 3, PPID: 2, UID: 501, Start: "grandchild"}
	other := desktopProcessIdentity{PID: 4, PPID: 1, UID: 502, Start: "other-user"}
	tree := desktopProcessTree([]desktopProcessIdentity{root, grandchild, child, other}, root)
	if len(tree) != 3 {
		t.Fatal("descendant inventory incomplete")
	}
	orphan := grandchild
	orphan.PPID = 1
	if desktopCapturedProcessesExited(tree, []desktopProcessIdentity{orphan}) {
		t.Fatal("orphaned child ignored")
	}
	orphan.Start = "replacement"
	if !desktopCapturedProcessesExited(tree, []desktopProcessIdentity{orphan}) {
		t.Fatal("reused pid confused with old process")
	}
}

func TestLiveDesktopProcessReadOnly(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_DESKTOP_PROCESS_TEST") != "1" {
		t.Skip("read-only live probe is opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := findDesktopProcessPair(procs, os.Getuid(), "/Applications/ChatGPT.app")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil || !desktopServerUsesHome(ctx, pair.Server, home) {
		t.Fatal("backend home not confirmed")
	}
	t.Log("current Desktop app, direct backend and config home confirmed")
}

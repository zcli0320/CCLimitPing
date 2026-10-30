package assistant

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDesktopLifecycleRequiresActualOldExitAndNewHome(t *testing.T) {
	const app = "/Applications/ChatGPT.app"
	old := desktopProcessPair{App: desktopProcessIdentity{PID: 100, UID: 501, Start: "old-app", Command: app + "/Contents/MacOS/ChatGPT"}, Server: desktopProcessIdentity{PID: 101, PPID: 100, UID: 501, Start: "old-server", Command: app + "/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server"}}
	procs := []desktopProcessIdentity{old.App, old.Server}
	var calls [][]string
	homeOK := true
	d := &desktopHandoffLifecycle{home: "/Users/test", appPath: app, uid: 501, old: old,
		read:     func(context.Context) ([]desktopProcessIdentity, error) { return procs, nil },
		usesHome: func(context.Context, desktopProcessIdentity, string) bool { return homeOK },
		command: func(_ context.Context, name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			return nil
		},
	}
	ctx := context.Background()
	if err := d.requestExit(ctx); err != nil || len(calls) != 1 || calls[0][0] != "/usr/bin/osascript" {
		t.Fatal("graceful quit not requested")
	}
	if d.observeExit(ctx) || d.requestRestart(ctx, "00000000-0000-7000-8000-000000000001") == nil {
		t.Fatal("restart accepted while old processes existed")
	}
	procs = []desktopProcessIdentity{old.Server}
	if d.observeExit(ctx) {
		t.Fatal("orphaned server was ignored")
	}
	procs = nil
	if !d.observeExit(ctx) {
		t.Fatal("exit not detected")
	}
	if d.requestRestart(ctx, "invalid/identifier") == nil || len(calls) != 1 {
		t.Fatal("invalid thread identifier accepted")
	}
	id := "00000000-0000-7000-8000-000000000001"
	if err := d.requestRestart(ctx, id); err != nil || !reflect.DeepEqual(calls[1], []string{"/usr/bin/open", "-a", app, "codex://threads/" + id}) {
		t.Fatal("wrong restart request")
	}
	newApp := old.App
	newApp.PID = 200
	newApp.Start = "new-app"
	newServer := old.Server
	newServer.PID = 201
	newServer.PPID = 200
	newServer.Start = "new-server"
	procs = []desktopProcessIdentity{newApp, newServer}
	homeOK = false
	if d.observeColdStart(ctx) {
		t.Fatal("wrong home accepted")
	}
	homeOK = true
	if !d.observeColdStart(ctx) {
		t.Fatal("cold start not verified")
	}
}

func TestDesktopLifecycleRejectsReplacementAndQuitFailure(t *testing.T) {
	const app = "/Applications/ChatGPT.app"
	old := desktopProcessPair{App: desktopProcessIdentity{PID: 100, UID: 501, Start: "old", Command: app + "/Contents/MacOS/ChatGPT"}, Server: desktopProcessIdentity{PID: 101, PPID: 100, UID: 501, Start: "old-server", Command: app + "/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server"}}
	procs := []desktopProcessIdentity{old.App, old.Server}
	calls := 0
	d := desktopHandoffLifecycle{home: "/Users/test", appPath: app, uid: 501, old: old, read: func(context.Context) ([]desktopProcessIdentity, error) { return procs, nil }, usesHome: func(context.Context, desktopProcessIdentity, string) bool { return true }, command: func(context.Context, string, ...string) error { calls++; return errors.New("denied") }}
	if d.requestExit(context.Background()) == nil || calls != 1 {
		t.Fatal("quit failure accepted")
	}
	procs[0].Start = "replacement"
	if d.requestExit(context.Background()) == nil || calls != 1 {
		t.Fatal("changed instance was signalled")
	}
}

func TestDesktopLaunchDoesNotInheritExperimentFlags(t *testing.T) {
	actual := desktopLaunchEnvironment([]string{"HOME=/Users/test", "CODEX_HOME=/Users/test/.codex", "PATH=/bin", "CODEX_HELPER_LIVE_FULL_DESKTOP_CONFIRM=run-full-handoff-now", "CODEX_HELPER_LIVE_FULL_DESKTOP_THREAD=test", "CODEX_HELPER_TEST_HOME=/tmp/test"})
	if !reflect.DeepEqual(actual, []string{"HOME=/Users/test", "CODEX_HOME=/Users/test/.codex", "PATH=/bin"}) {
		t.Fatal("test authority escaped into Desktop launch")
	}
}

func TestDesktopLifecycleRestartsWithExactRuntimeBridge(t *testing.T) {
	const app = "/Applications/ChatGPT.app"
	const bridge = "/Users/test/Applications/Helper Preview.app/Contents/MacOS/codex-runtime-bridge"
	var called []string
	d := desktopHandoffLifecycle{home: "/Users/test", appPath: app, uid: 501, bridgePath: bridge, oldExitConfirmed: true,
		old:  desktopProcessPair{App: desktopProcessIdentity{PID: 100, Start: "old-app"}, Server: desktopProcessIdentity{PID: 101, Start: "old-server"}},
		read: func(context.Context) ([]desktopProcessIdentity, error) { return nil, nil },
		command: func(_ context.Context, name string, args ...string) error {
			called = append([]string{name}, args...)
			return nil
		},
	}
	d.oldTree = []desktopProcessIdentity{d.old.App, d.old.Server}
	id := "00000000-0000-7000-8000-000000000001"
	if err := d.requestRestart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	expected := []string{"/usr/bin/open", "-a", app, "--env", "CODEX_CLI_PATH=" + bridge, "--env", "HOME=/Users/test", "--env", "CODEX_HOME=/Users/test/.codex", "--env", "CODEX_HELPER_NATIVE_PREVIEW=1", "codex://threads/" + id}
	if !reflect.DeepEqual(called, expected) {
		t.Fatal("restart lost bridge or scoped home")
	}
}

func TestDesktopLifecyclePinsFirstColdStartInstance(t *testing.T) {
	const app = "/Applications/ChatGPT.app"
	old := desktopProcessPair{App: desktopProcessIdentity{PID: 100, UID: 501, Start: "old-app"}, Server: desktopProcessIdentity{PID: 101, UID: 501, Start: "old-server"}}
	main := desktopProcessIdentity{PID: 200, UID: 501, Start: "new-app", Command: app + "/Contents/MacOS/ChatGPT"}
	backend := desktopProcessIdentity{PID: 201, PPID: 200, UID: 501, Start: "new-server", Command: app + "/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server"}
	procs := []desktopProcessIdentity{main, backend}
	d := desktopHandoffLifecycle{home: "/Users/test", appPath: app, uid: 501, old: old, oldTree: []desktopProcessIdentity{old.App, old.Server}, oldExitConfirmed: true,
		read: func(context.Context) ([]desktopProcessIdentity, error) { return procs, nil }, usesHome: func(context.Context, desktopProcessIdentity, string) bool { return true }}
	if !d.observeColdStart(context.Background()) {
		t.Fatal("first valid cold start rejected")
	}
	procs[1].Start = "replaced-server"
	if d.observeColdStart(context.Background()) {
		t.Fatal("replacement after cold start was accepted")
	}
}

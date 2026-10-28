package assistant

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var desktopHandoffUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// The caller must first establish the single selected session's authorization
// and stop state. The UI remains disabled until full real-device validation.
// The command interface permits tests to exercise all decisions without
// terminating or launching the user's application.
type desktopHandoffLifecycle struct {
	home, appPath    string
	uid              int
	old              desktopProcessPair
	oldTree          []desktopProcessIdentity
	oldExitConfirmed bool
	read             func(context.Context) ([]desktopProcessIdentity, error)
	usesHome         func(context.Context, desktopProcessIdentity, string) bool
	command          func(context.Context, string, ...string) error
}

func newDesktopHandoffLifecycle(ctx context.Context, home string) (*desktopHandoffLifecycle, error) {
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return nil, err
	}
	const app = "/Applications/ChatGPT.app"
	pair, err := findDesktopProcessPair(procs, os.Getuid(), app)
	if err != nil || !desktopServerUsesHome(ctx, pair.Server, home) {
		return nil, errors.New("Desktop 实例或配置目录无法确认")
	}
	return &desktopHandoffLifecycle{home: home, appPath: app, uid: os.Getuid(), old: pair, oldTree: desktopProcessTree(procs, pair.App), read: readDesktopProcesses, usesHome: desktopServerUsesHome, command: func(ctx context.Context, name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = desktopLaunchEnvironment(os.Environ())
		return cmd.Run()
	}}, nil
}

func desktopLaunchEnvironment(env []string) []string {
	var clean []string
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(key, "CODEX_HELPER_LIVE_") && key != "CODEX_HELPER_TEST_HOME" {
			clean = append(clean, value)
		}
	}
	return clean
}

func (d *desktopHandoffLifecycle) requestExit(ctx context.Context) error {
	if d.read == nil || d.usesHome == nil || d.command == nil || d.oldExitConfirmed {
		return errors.New("Desktop 退出条件不可确认")
	}
	procs, err := d.read(ctx)
	if err != nil {
		return err
	}
	current, err := findDesktopProcessPair(procs, d.uid, d.appPath)
	if err != nil || current != d.old || !d.usesHome(ctx, current.Server, d.home) {
		return errors.New("Desktop 实例已变化")
	}
	d.oldTree = desktopProcessTree(procs, d.old.App)
	// One graceful quit request; a refusal, timeout or prompt is not a success.
	// A native confirmation can hold the Apple event itself open, before the
	// coordinator reaches waiting_exit. Bound that request independently.
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if d.command(requestCtx, "/usr/bin/osascript", "-e", `tell application id "com.openai.codex" to quit`) != nil {
		return errors.New("Desktop 正常退出请求未确认")
	}
	return nil
}

func (d *desktopHandoffLifecycle) observeExit(ctx context.Context) bool {
	if d.read == nil {
		return false
	}
	procs, err := d.read(ctx)
	if err != nil || !desktopOldProcessesExited(d.old, procs, d.uid, d.appPath) || !desktopCapturedProcessesExited(d.oldTree, procs) {
		return false
	}
	d.oldExitConfirmed = true
	return true
}

func (d *desktopHandoffLifecycle) requestRestart(ctx context.Context, id string) error {
	if !desktopHandoffUUID.MatchString(id) || !d.oldExitConfirmed || d.command == nil || !d.observeExit(ctx) {
		return errors.New("Desktop 重启条件不可确认")
	}
	if d.command(ctx, "/usr/bin/open", "-a", d.appPath, "codex://threads/"+id) != nil {
		return errors.New("Desktop 重启请求未确认")
	}
	return nil
}

func (d *desktopHandoffLifecycle) observeColdStart(ctx context.Context) bool {
	if !d.oldExitConfirmed || d.read == nil || d.usesHome == nil {
		return false
	}
	procs, err := d.read(ctx)
	if err != nil {
		return false
	}
	if !desktopCapturedProcessesExited(d.oldTree, procs) {
		return false
	}
	for _, p := range procs {
		if (p.PID == d.old.App.PID && p.Start == d.old.App.Start) || (p.PID == d.old.Server.PID && p.Start == d.old.Server.Start) {
			return false
		}
	}
	pair, err := findDesktopProcessPair(procs, d.uid, d.appPath)
	return err == nil && pair.App != d.old.App && pair.Server != d.old.Server && d.usesHome(ctx, pair.Server, d.home)
}

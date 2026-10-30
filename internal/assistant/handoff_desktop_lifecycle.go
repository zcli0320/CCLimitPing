package assistant

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var desktopHandoffUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// The caller must first establish the single selected session's authorization
// and stop state. In the account preview, the running identity must also be
// verified before any stop, exit, or continuation request. Tests can supply an
// isolated lifecycle without terminating or launching the user's application.
type desktopHandoffLifecycle struct {
	home, appPath, bridgePath string
	uid                       int
	old                       desktopProcessPair
	restarted                 desktopProcessPair
	oldTree                   []desktopProcessIdentity
	oldExitConfirmed          bool
	read                      func(context.Context) ([]desktopProcessIdentity, error)
	usesHome                  func(context.Context, desktopProcessIdentity, string) bool
	command                   func(context.Context, string, ...string) error
}

func newDesktopHandoffLifecycle(ctx context.Context, home string) (*desktopHandoffLifecycle, error) {
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return nil, err
	}
	const app = desktopAppPath
	bridgePath := ""
	if os.Getenv("CODEX_HELPER_NATIVE_PREVIEW") == "1" {
		bridgePath, err = desktopRuntimeBridgeExecutable()
		if err != nil {
			return nil, err
		}
	}
	pair, err := findDesktopProcessPairWithBridge(procs, os.Getuid(), app, bridgePath)
	if err != nil || !desktopServerUsesHome(ctx, pair.Server, home) {
		return nil, errors.New("Desktop 实例或配置目录无法确认")
	}
	return &desktopHandoffLifecycle{home: home, appPath: app, bridgePath: bridgePath, uid: os.Getuid(), old: pair, oldTree: desktopProcessTree(procs, pair.App), read: readDesktopProcesses, usesHome: desktopServerUsesHome, command: func(ctx context.Context, name string, args ...string) error {
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
	current, err := findDesktopProcessPairWithBridge(procs, d.uid, d.appPath, d.bridgePath)
	if err != nil || current != d.old || !d.usesHome(ctx, current.Server, d.home) {
		return errors.New("Desktop 实例已变化")
	}
	d.oldTree = desktopProcessTree(procs, d.old.App)
	// One graceful quit request; a refusal, timeout or prompt is not a success.
	if d.command(ctx, "/usr/bin/osascript", "-e", `tell application id "com.openai.codex" to quit`) != nil {
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
	args := []string{"-a", d.appPath, "codex://threads/" + id}
	if d.bridgePath != "" {
		args = desktopRuntimeLaunchArgs(d.home, d.appPath, d.bridgePath, id)
	}
	if d.command(ctx, "/usr/bin/open", args...) != nil {
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
	pair, err := findDesktopProcessPairWithBridge(procs, d.uid, d.appPath, d.bridgePath)
	if err != nil || pair.App == d.old.App || pair.Server == d.old.Server || !d.usesHome(ctx, pair.Server, d.home) {
		return false
	}
	if d.restarted.App.PID == 0 {
		d.restarted = pair
	}
	return d.restarted == pair
}

func desktopRuntimeBridgeExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", errors.New("小助手运行核验程序不可定位")
	}
	path := filepath.Join(filepath.Dir(executable), "codex-runtime-bridge")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return "", errors.New("小助手运行核验程序不可用，请使用完整安装包")
	}
	return path, nil
}
func desktopRuntimeLaunchArgs(home, app, bridge, id string) []string {
	return []string{"-a", app, "--env", "CODEX_CLI_PATH=" + bridge, "--env", "HOME=" + home, "--env", "CODEX_HOME=" + filepath.Join(home, ".codex"), "--env", "CODEX_HELPER_NATIVE_PREVIEW=1", "codex://threads/" + id}
}

// This first launch never quits an existing app. The caller has to close
// Desktop first; later handoffs use the stop/exit state machine above.
func (r Reader) LaunchDesktopWithRuntimeBridge(ctx context.Context, id string) error {
	if !r.nativeAccountPreview() || os.Getenv("CODEX_HELPER_TEST_HOME") != "" || !desktopHandoffUUID.MatchString(id) {
		return errors.New("当前环境不能启动真实 Desktop")
	}
	bridge, err := desktopRuntimeBridgeExecutable()
	if err != nil {
		return err
	}
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return err
	}
	for _, p := range procs {
		if p.UID == os.Getuid() && p.Command == filepath.Join(desktopAppPath, "Contents/MacOS/ChatGPT") {
			return errors.New("请先正常退出 Desktop，再为所选会话建立运行账号核验连接")
		}
	}
	valid := false
	snapshot := r.Snapshot(ctx)
	for _, s := range snapshot.Sessions {
		if s.Scope == "desktop" && s.ID == id && !s.Archived {
			valid = true
			break
		}
	}
	if !valid {
		return errors.New("所选 Desktop 会话不可用")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/open", desktopRuntimeLaunchArgs(r.Home, desktopAppPath, bridge, id)...)
	cmd.Env = desktopLaunchEnvironment(os.Environ())
	if cmd.Run() != nil {
		return errors.New("Desktop 核验启动请求未完成")
	}
	return nil
}

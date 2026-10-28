package assistant

import (
	"context"
	"errors"
	"github.com/BurntSushi/toml"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type nativeSystemEnvironment struct {
	r              Reader
	passiveDesktop *desktopHandoffLifecycle
	passiveCLI     *cliRunnerState
}

func (r Reader) validateNativeConfiguration() error {
	for _, scope := range []string{"desktop", "cli"} {
		if _, ok := r.managedResumeProvider(scope); !ok {
			return errors.New("两端均须使用支持的官方直连认证")
		}
		path, err := r.handoffAuthPath(scope)
		if err != nil {
			return err
		}
		var config struct {
			Store string `toml:"cli_auth_credentials_store"`
		}
		if _, err = toml.DecodeFile(filepath.Join(filepath.Dir(path), "config.toml"), &config); err != nil || (config.Store != "" && config.Store != "file") {
			return errors.New("原生双端交换仅支持文件凭证存储，其他模式暂时搁置")
		}
	}
	return nil
}

func ccSwitchProcesses(procs []desktopProcessIdentity) []desktopProcessIdentity {
	var found []desktopProcessIdentity
	for _, p := range procs {
		if p.UID == os.Getuid() && strings.Contains(p.Command, ".app/Contents/MacOS/cc-switch") {
			found = append(found, p)
		}
	}
	return found
}

// Only inspect CODEX_HOME, never return or log process environments.
func processCodexHome(ctx context.Context, pid int, home string) (string, bool) {
	out, err := exec.CommandContext(ctx, "/bin/ps", "eww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", false
	}
	for _, field := range strings.Fields(string(out)) {
		if strings.HasPrefix(field, "CODEX_HOME=") {
			return strings.TrimPrefix(field, "CODEX_HOME="), true
		}
	}
	if strings.Contains(string(out)+" ", "HOME="+home+" ") {
		return filepath.Join(home, ".codex"), true
	}
	return "", false
}

func (e *nativeSystemEnvironment) cliProcesses(ctx context.Context) ([]desktopProcessIdentity, error) {
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return nil, err
	}
	var found []desktopProcessIdentity
	for _, p := range procs {
		if p.UID != os.Getuid() {
			continue
		}
		words := strings.Fields(p.Command)
		if len(words) == 0 {
			continue
		}
		base := filepath.Base(words[0])
		isCodex := base == "codex" || base == "codex-cli" || base == "codex.js" || strings.Contains(p.Command, "/@openai/codex/")
		if !isCodex || strings.Contains(p.Command, "/ChatGPT.app/") {
			continue
		}
		root, known := processCodexHome(ctx, p.PID, e.r.Home)
		if !known {
			return nil, errors.New("存在无法确认环境目录的 Codex 进程")
		}
		if sameExistingPath(root, filepath.Join(e.r.Home, ".codex-cli")) {
			found = append(found, p)
		}
	}
	return found, nil
}

func (e *nativeSystemEnvironment) inspect(ctx context.Context, working string) (bool, error) {
	if err := e.r.validateNativeConfiguration(); err != nil {
		return false, err
	}
	e.passiveDesktop = nil
	e.passiveCLI = nil
	version, versionErr := exec.CommandContext(ctx, "/usr/libexec/PlistBuddy", "-c", "Print :CFBundleShortVersionString", "/Applications/CC Switch.app/Contents/Info.plist").Output()
	if versionErr != nil || strings.TrimSpace(string(version)) != "4.0.21" {
		return false, errors.New("当前原生账号适配器仅验证 CC Switch 4.0.21；此步骤暂时搁置")
	}
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return false, err
	}
	cc := ccSwitchProcesses(procs)
	if len(cc) > 1 {
		return false, errors.New("发现多个 CC Switch 实例")
	}
	if len(cc) == 1 && !strings.HasPrefix(cc[0].Command, "/Applications/CC Switch.app/Contents/MacOS/cc-switch") {
		return false, errors.New("CC Switch 运行副本与受支持路径不一致")
	}
	if working == "desktop" {
		cli, err := e.cliProcesses(ctx)
		if err != nil {
			return false, err
		}
		if len(cli) > 0 {
			var runner cliRunnerState
			if readJSON(e.r.cliRunnerPath(), &runner) != nil || !e.r.cliManaged(runner.ID) {
				return false, errors.New("待用 CLI 仍有未托管实例；请先正常退出，该次操作暂时搁置")
			}
			provider, ok := e.r.managedResumeProvider("cli")
			if !ok {
				return false, errors.New("待用 CLI 配置不可确认")
			}
			thread, err := readHandoffCLIThread(ctx, runner.Socket, runner.ID, provider)
			if err != nil || thread.Status.Type != "idle" {
				return false, errors.New("待用 CLI 仍在工作，暂不交换账号")
			}
			turn, ok := thread.latest()
			if !ok || turn.Status == "inProgress" || !turn.toolsStopped() {
				return false, errors.New("待用 CLI 工具尚未结束")
			}
			for _, p := range cli {
				if p.PID != runner.ServerPID && p.PID != runner.ClientPID && p.PPID != runner.PID {
					return false, errors.New("发现其他 CLI 实例，暂不交换账号")
				}
			}
			e.passiveCLI = &runner
		}
	} else if working == "cli" {
		pair, err := findDesktopProcessPair(procs, os.Getuid(), "/Applications/ChatGPT.app")
		if err == nil {
			if !desktopServerUsesHome(ctx, pair.Server, e.r.Home) {
				return false, errors.New("待用 Desktop 配置目录不可确认")
			}
			snapshot := e.r.Snapshot(ctx)
			id := ""
			for _, s := range snapshot.Sessions {
				if s.Scope == "desktop" && !s.Archived {
					id = s.ID
					break
				}
			}
			if id == "" {
				return false, errors.New("待用 Desktop 会话无法确认")
			}
			client, err := connectDesktop(ctx, e.r.Home)
			if err != nil {
				return false, err
			}
			v, owner, err := client.snapshot(id)
			client.conn.Close()
			if err != nil || owner == "" || v.Runtime.Type != "idle" || len(v.Requests) != 0 || (v.Goal != nil && v.Goal.Status == "active") {
				return false, errors.New("待用 Desktop 仍在工作或状态未确认")
			}
			turn, hasTurn := v.latest()
			if !hasTurn || turn.Status == "inProgress" || !desktopTurnToolsStopped(turn) {
				return false, errors.New("待用 Desktop 工具尚未结束或列表不完整")
			}
			for _, p := range v.UnconfirmedSubmissions {
				if !p.Terminal {
					return false, errors.New("待用 Desktop 有待确认输入")
				}
			}
			e.passiveDesktop, err = newDesktopHandoffLifecycle(ctx, e.r.Home)
			if err != nil {
				return false, err
			}
		} else {
			for _, p := range procs {
				if strings.HasPrefix(p.Command, "/Applications/ChatGPT.app/Contents/") {
					return false, errors.New("待用 Desktop 存在未退出的进程")
				}
			}
		}
	} else {
		return false, errors.New("工作端无效")
	}
	return len(cc) == 1, nil
}

func (e *nativeSystemEnvironment) quiesce(ctx context.Context, working string) error {
	if e.passiveDesktop != nil {
		if err := e.passiveDesktop.requestExit(ctx); err != nil {
			return err
		}
	}
	if e.passiveCLI != nil {
		procs, err := readDesktopProcesses(ctx)
		if err != nil {
			return err
		}
		owned := false
		for _, p := range procs {
			if p.PID == e.passiveCLI.PID && strings.Contains(p.Command, "/codex-helper ") && strings.Contains(p.Command, e.passiveCLI.ID) {
				owned = true
			}
		}
		if !owned {
			return errors.New("待用 CLI 控制者不可确认")
		}
		if syscall.Kill(e.passiveCLI.PID, syscall.SIGTERM) != nil {
			return errors.New("待用 CLI 退出请求未确认")
		}
	}
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return err
	}
	if len(ccSwitchProcesses(procs)) > 0 {
		if exec.CommandContext(ctx, "/usr/bin/osascript", "-e", `tell application id "com.ccswitch.desktop" to quit`).Run() != nil {
			return errors.New("CC Switch 正常退出未确认；系统权限步骤暂时搁置")
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err = e.verify(ctx, working); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("待用端或 CC Switch 尚未退出，已停止交换")
}

func (e *nativeSystemEnvironment) verify(ctx context.Context, working string) error {
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		return err
	}
	if len(ccSwitchProcesses(procs)) != 0 {
		return errors.New("CC Switch 重新运行，已停止原生写入")
	}
	if working == "desktop" {
		p, err := e.cliProcesses(ctx)
		if err != nil {
			return err
		}
		if len(p) > 0 {
			return errors.New("待用 CLI 尚未退出")
		}
	} else {
		if e.passiveDesktop != nil && !e.passiveDesktop.observeExit(ctx) {
			return errors.New("待用 Desktop 的原进程树尚未退出")
		}
		for _, p := range procs {
			if strings.HasPrefix(p.Command, "/Applications/ChatGPT.app/Contents/") {
				return errors.New("待用 Desktop 尚未退出")
			}
		}
	}
	return nil
}

func (e *nativeSystemEnvironment) restore(ctx context.Context, running bool) error {
	if !running {
		return nil
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/open", "-a", "/Applications/CC Switch.app")
	cmd.Env = desktopLaunchEnvironment(os.Environ())
	if cmd.Run() != nil {
		return errors.New("账号已交换，但 CC Switch 重开需要处理")
	}
	return nil
}

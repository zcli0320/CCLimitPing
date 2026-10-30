package assistant

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const desktopAppPath = "/Applications/ChatGPT.app"

func desktopBundledBackend() string {
	return filepath.Join(desktopAppPath, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex")
}

// Desktop may put global configuration flags before the subcommand. Wrap only
// an unambiguous stdio app-server invocation; daemon, help, non-stdio and
// unknown forms remain transparent passthrough commands.
func desktopStdioServerArgs(args []string) bool {
	server := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "app-server" {
			server = i
			break
		}
		switch {
		case arg == "-c", arg == "--config", arg == "--enable", arg == "--disable":
			i++
			if i >= len(args) || args[i] == "" {
				return false
			}
		case arg == "--strict-config":
		case strings.HasPrefix(arg, "--config="), strings.HasPrefix(arg, "--enable="), strings.HasPrefix(arg, "--disable="), strings.HasPrefix(arg, "-c="):
		default:
			return false
		}
	}
	if server < 0 {
		return false
	}
	seenListen := false
	for i := server + 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-c", arg == "--config", arg == "--enable", arg == "--disable", arg == "--startup-client-info", arg == "--cloud-skills-snapshot", arg == "--code-mode-host":
			i++
			if i >= len(args) || args[i] == "" {
				return false
			}
		case arg == "--listen":
			i++
			if seenListen || i >= len(args) || args[i] != "stdio://" {
				return false
			}
			seenListen = true
		case strings.HasPrefix(arg, "--listen="):
			if seenListen || strings.TrimPrefix(arg, "--listen=") != "stdio://" {
				return false
			}
			seenListen = true
		case arg == "--stdio", arg == "--strict-config", arg == "--analytics-default-enabled":
		case strings.HasPrefix(arg, "--config="), strings.HasPrefix(arg, "--enable="), strings.HasPrefix(arg, "--disable="), strings.HasPrefix(arg, "--startup-client-info="), strings.HasPrefix(arg, "--cloud-skills-snapshot="), strings.HasPrefix(arg, "--code-mode-host="), strings.HasPrefix(arg, "-c="):
		default:
			return false
		}
	}
	return true
}

// RunDesktopRuntimeBridge is the transparent CODEX_CLI_PATH executable. It
// never changes auth/config. If private identity IPC cannot start, Desktop's
// original stdio traffic still works and automatic handoff remains unavailable.
func RunDesktopRuntimeBridge(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	r, err := NewReader()
	if err != nil {
		return err
	}
	return runDesktopRuntimeBridge(ctx, r, desktopBundledBackend(), args, stdin, stdout, stderr)
}
func runDesktopRuntimeBridge(ctx context.Context, r Reader, backend string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	child := exec.CommandContext(ctx, backend, args...)
	// Prevent recursive executable overrides in any child launched by Codex.
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "CODEX_CLI_PATH" {
			child.Env = append(child.Env, value)
		}
	}
	child.Stderr = stderr
	if !desktopStdioServerArgs(args) {
		child.Stdin = stdin
		child.Stdout = stdout
		if child.Run() != nil {
			return errors.New("Codex 后端命令未完成")
		}
		return nil
	}
	input, err := child.StdinPipe()
	if err != nil {
		return errors.New("Codex 后端输入不可用")
	}
	output, err := child.StdoutPipe()
	if err != nil {
		input.Close()
		return errors.New("Codex 后端输出不可用")
	}
	if child.Start() != nil {
		input.Close()
		return errors.New("Codex 后端未启动")
	}
	defer input.Close()
	bridge := newRuntimeIdentityBridge(input, stdout)
	defer bridge.close()
	var endpoint *runtimeIdentityEndpoint
	if configured := os.Getenv("CODEX_HOME"); configured == "" || filepath.Clean(configured) == filepath.Join(r.Home, ".codex") {
		if root, err := r.runtimeIdentityDirectory(true); err == nil {
			endpoint, _ = startRuntimeIdentityEndpoint(root, bridge, child.Process.Pid)
		}
	}
	if endpoint != nil {
		defer endpoint.Close()
	} else {
		fmt.Fprintln(stderr, "Codex 小助手运行账号核验连接不可用，自动交接保持关闭。")
	}
	pump := func(reader io.Reader, forward func([]byte) error) error {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 32<<20)
		for scanner.Scan() {
			if forward(scanner.Bytes()) != nil {
				return errors.New("Codex 后端桥连接中断")
			}
		}
		if scanner.Err() != nil {
			return errors.New("Codex 后端桥消息不完整或超出限制")
		}
		return nil
	}
	go func() {
		err := pump(stdin, bridge.forwardClient)
		input.Close()
		if err != nil {
			cancel()
		}
	}()
	finished := make(chan error, 1)
	go func() { finished <- pump(output, bridge.forwardBackend) }()
	var streamError error
	select {
	case streamError = <-finished:
		if streamError != nil {
			cancel()
		}
	case <-ctx.Done():
		streamError = ctx.Err()
	}
	bridge.close()
	if streamError != nil {
		cancel()
	}
	if child.Wait() != nil || streamError != nil {
		return errors.New("Codex 后端桥连接已结束")
	}
	return nil
}

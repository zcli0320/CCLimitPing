package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// CLIHandoffPreflight is a read-only observation of the selected thread in the
// helper's own CLI backend. It is not an authorization to stop or switch it.
type CLIHandoffPreflight struct {
	SessionID string   `json:"session_id"`
	TurnID    string   `json:"turn_id,omitempty"`
	TurnState string   `json:"turn_state,omitempty"`
	Runtime   string   `json:"runtime,omitempty"`
	Ready     bool     `json:"ready"`
	Blockers  []string `json:"blockers"`
}

type cliHandoffCaller interface {
	call(string, any) (json.RawMessage, error)
}

// inspectCLIHandoff uses only thread/read. In particular it must not create or
// resume another writer for the existing thread, or send model input.
func inspectCLIHandoff(rpc cliHandoffCaller, id, provider string) (CLIHandoffPreflight, error) {
	out := CLIHandoffPreflight{SessionID: id, Ready: false, Blockers: []string{}}
	if !validHandoffText(id) || strings.TrimSpace(id) != id || strings.TrimSpace(id) == "" || !validHandoffText(provider) {
		return out, errors.New("无法确认准确 CLI 会话和供应商")
	}
	raw, err := rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": true})
	if err != nil {
		return out, errors.New("托管 CLI 会话读取失败")
	}
	var response struct {
		Thread struct {
			ID       string `json:"id"`
			Provider string `json:"modelProvider"`
			Status   struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != id || response.Thread.Provider != provider {
		return out, errors.New("托管 CLI 返回的会话或供应商不匹配")
	}
	out.Runtime = response.Thread.Status.Type
	if len(response.Thread.Turns) > 0 {
		latest := response.Thread.Turns[len(response.Thread.Turns)-1]
		out.TurnID, out.TurnState = latest.ID, latest.Status
	}
	if out.Runtime == "" || out.TurnID == "" || out.TurnState == "" {
		out.Blockers = append(out.Blockers, "无法确认当前 CLI 轮次完整状态")
	}
	out.Blockers = append(out.Blockers,
		"交接时将重新检查托管任务、工具及其他会话的状态",
		"认证与重启后的账号仍需在实际交接中核验",
	)
	return out, nil
}

// ownedCLIHandoffSocket accepts only the private temp directory created by the
// helper's own RunCLIResume. This is still only suitable for read-only checks:
// PID liveness alone does not establish a safe lifetime-bound process identity.
func ownedCLIHandoffSocket(r Reader, id string) (string, error) {
	if !r.cliManaged(id) {
		return "", errors.New("请先由小助手托管所选 CLI 会话")
	}
	var runner cliRunnerState
	if readJSON(r.cliRunnerPath(), &runner) != nil || runner.ID != id {
		return "", errors.New("托管 CLI 状态不可用")
	}
	path := filepath.Clean(runner.Socket)
	if err := validateOwnedCLISocket(path); err != nil {
		return "", err
	}
	return path, nil
}

// Modern Codex versions may replace the requested socket path with a symlink
// to a socket in their per-user private daemon directory. Validate both the
// caller-owned temporary directory and the actual socket's owner and parent.
func validateOwnedCLISocket(path string) error {
	tempRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return errors.New("托管 CLI 临时目录不可访问")
	}
	return validateCLISocketInTemp(path, tempRoot)
}

func validateCLISocketInTemp(path, tempRoot string) error {
	path = filepath.Clean(path)
	root := filepath.Dir(path)
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if !filepath.IsAbs(path) || filepath.Base(path) != "control.sock" || !strings.HasPrefix(filepath.Base(root), "codex-helper-cli-") || err != nil || parent != tempRoot {
		return errors.New("托管 CLI 控制连接路径不受支持")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("托管 CLI 控制目录权限不正确")
	}
	rootStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || rootStat.Uid != uint32(os.Getuid()) {
		return errors.New("托管 CLI 控制目录所有者不正确")
	}
	info, err = os.Lstat(path)
	if err != nil {
		return errors.New("托管 CLI 控制连接不存在")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, e := os.Readlink(path)
		if e != nil || !filepath.IsAbs(link) {
			return errors.New("托管 CLI 控制连接目标不受支持")
		}
		daemonDir := filepath.Join(tempRoot, fmt.Sprintf("codex-daemon-%d", os.Getuid()))
		if filepath.Dir(filepath.Clean(link)) != daemonDir {
			return errors.New("托管 CLI 控制连接目标不受支持")
		}
		dir, e := os.Lstat(daemonDir)
		if e != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
			return errors.New("Codex 控制目录权限不正确")
		}
		owner, ok := dir.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) {
			return errors.New("Codex 控制目录所有者不正确")
		}
		info, err = os.Lstat(link)
		if err != nil {
			return errors.New("Codex 控制连接不存在")
		}
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("托管 CLI 控制连接不是 socket")
	}
	socketStat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || socketStat.Uid != uint32(os.Getuid()) {
		return errors.New("托管 CLI 控制连接所有者不正确")
	}
	return nil
}

func (r Reader) CheckCLIHandoffPreflight(ctx context.Context, id string) (CLIHandoffPreflight, error) {
	out := CLIHandoffPreflight{SessionID: id, Ready: false}
	provider, ok := r.managedResumeProvider("cli")
	if !ok {
		return out, errors.New("CLI 账号配置不可确认")
	}
	path, err := ownedCLIHandoffSocket(r, id)
	if err != nil {
		return out, err
	}
	c, err := connectUnixJSON(ctx, path)
	if err != nil {
		return out, err
	}
	defer c.Close()
	scanner := bufio.NewScanner(c)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	rpc := warmupRPC{in: c, lines: scanner, result: &WarmupExecution{}}
	if _, err = rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_helper_handoff_preflight", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return out, errors.New("托管 CLI 协议初始化失败")
	}
	if err = rpc.send(map[string]any{"method": "initialized"}); err != nil {
		return out, errors.New("托管 CLI 协议初始化失败")
	}
	return inspectCLIHandoff(&rpc, id, provider)
}

// These protocol parameter builders are deliberately not wired to an executor.
// They make the future required preconditions explicit for an isolated test.
// A future caller must re-read and match the live thread/turn, process owner,
// user authorization and all stopping evidence immediately before dispatch.
func cliHandoffSteerParams(threadID, expectedTurnID, text string) (map[string]any, error) {
	if !validHandoffText(threadID) || strings.TrimSpace(threadID) != threadID || strings.TrimSpace(threadID) == "" || !validHandoffText(expectedTurnID) || strings.TrimSpace(expectedTurnID) != expectedTurnID || strings.TrimSpace(expectedTurnID) == "" || !validHandoffText(text) || strings.TrimSpace(text) == "" {
		return nil, errors.New("缺少准确的 CLI 会话、轮次或收尾指令")
	}
	return map[string]any{"threadId": threadID, "expectedTurnId": expectedTurnID, "input": []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}}, nil
}

func cliHandoffInterruptParams(threadID, turnID string) (map[string]any, error) {
	if !validHandoffText(threadID) || strings.TrimSpace(threadID) != threadID || strings.TrimSpace(threadID) == "" || !validHandoffText(turnID) || strings.TrimSpace(turnID) != turnID || strings.TrimSpace(turnID) == "" {
		return nil, errors.New("缺少准确的 CLI 会话或轮次")
	}
	return map[string]any{"threadId": threadID, "turnId": turnID}, nil
}

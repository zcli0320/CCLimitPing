package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type WarmupUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Cached int64 `json:"cached_input_tokens"`
	Total  int64 `json:"total_tokens"`
}
type WarmupExecution struct {
	Submitted        bool         `json:"submitted"`
	Completed        bool         `json:"completed"`
	IdentityVerified bool         `json:"identity_verified"`
	Model            string       `json:"model,omitempty"`
	Effort           string       `json:"effort,omitempty"`
	Usage            *WarmupUsage `json:"usage,omitempty"`
}

func codexExecutable(home string) string {
	candidates := []string{
		"/opt/homebrew/lib/node_modules/@openai/codex/node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex",
		"/usr/local/lib/node_modules/@openai/codex/node_modules/@openai/codex-darwin-x64/vendor/x86_64-apple-darwin/bin/codex",
		filepath.Join(home, ".codex/packages/standalone/current/bin/codex"),
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return path
		}
	}
	return ""
}

// Only allow ambient OS/network settings. In particular CODEX_HOME, OpenAI
// credentials, MCP setup and project instruction paths are never inherited.
func warmupEnvironment(root string) []string {
	allow := map[string]bool{"PATH": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "https_proxy": true, "http_proxy": true, "all_proxy": true, "no_proxy": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	env := []string{"HOME=" + root, "CODEX_HOME=" + filepath.Join(root, "codex"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache")}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if allow[name] {
			env = append(env, value)
		}
	}
	return env
}

type warmupRPC struct {
	in           io.Writer
	lines        *bufio.Scanner
	next         int
	result       *WarmupExecution
	thread, turn string
	observe      func(rpcEnvelope)
}
type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (rpc *warmupRPC) send(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("预热协议编码失败")
	}
	if _, err = rpc.in.Write(append(data, '\n')); err != nil {
		return errors.New("预热连接不可写")
	}
	return nil
}
func (rpc *warmupRPC) receive() (rpcEnvelope, error) {
	for rpc.lines.Scan() {
		var msg rpcEnvelope
		if json.Unmarshal(rpc.lines.Bytes(), &msg) != nil {
			return msg, errors.New("预热协议响应无效")
		}
		if msg.Method != "" && len(msg.ID) > 0 {
			_ = rpc.send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32000, "message": "Warmup does not grant tools or rotate credentials"}})
			return msg, errors.New("预热遇到认证续期或工具请求，已停止")
		}
		if msg.Method == "thread/tokenUsage/updated" {
			var p struct {
				ThreadID   string `json:"threadId"`
				TurnID     string `json:"turnId"`
				TokenUsage struct {
					Total struct {
						Input  int64 `json:"inputTokens"`
						Output int64 `json:"outputTokens"`
						Cached int64 `json:"cachedInputTokens"`
						Total  int64 `json:"totalTokens"`
					} `json:"total"`
				} `json:"tokenUsage"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && p.ThreadID == rpc.thread && (rpc.turn == "" || rpc.turn == p.TurnID) {
				u := p.TokenUsage.Total
				rpc.result.Usage = &WarmupUsage{Input: u.Input, Output: u.Output, Cached: u.Cached, Total: u.Total}
			}
		}
		return msg, nil
	}
	return rpcEnvelope{}, errors.New("预热连接结束或响应超出上限")
}
func (rpc *warmupRPC) call(method string, params any) (json.RawMessage, error) {
	rpc.next++
	id := rpc.next
	if err := rpc.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for n := 0; n < 10000; n++ {
		msg, err := rpc.receive()
		if err != nil {
			return nil, err
		}
		if rpc.observe != nil {
			rpc.observe(msg)
		}
		var responseID int
		if json.Unmarshal(msg.ID, &responseID) != nil || responseID != id {
			continue
		}
		if len(msg.Error) > 0 && string(msg.Error) != "null" {
			return nil, fmt.Errorf("Codex 拒绝 %s 操作，未回显服务响应", method)
		}
		return msg.Result, nil
	}
	return nil, errors.New("预热协议消息过多")
}

type warmupModel struct {
	Model       string `json:"model"`
	Hidden      bool   `json:"hidden"`
	Default     bool   `json:"isDefault"`
	Description string `json:"description"`
	Effort      string `json:"defaultReasoningEffort"`
	Efforts     []struct {
		Effort string `json:"reasoningEffort"`
	} `json:"supportedReasoningEfforts"`
}

func chooseWarmupModel(models []warmupModel) (string, string) {
	var chosen *warmupModel
	for i := range models {
		m := &models[i]
		if m.Hidden || m.Model == "" {
			continue
		}
		if chosen == nil || m.Default {
			chosen = m
		}
		text := strings.ToLower(m.Model + " " + m.Description)
		if strings.Contains(text, "mini") || strings.Contains(text, "luna") || strings.Contains(text, "affordable") {
			chosen = m
			break
		}
	}
	if chosen == nil {
		return "", ""
	}
	for _, effort := range []string{"minimal", "low", "none"} {
		for _, e := range chosen.Efforts {
			if e.Effort == effort {
				return chosen.Model, effort
			}
		}
	}
	return chosen.Model, chosen.Effort
}

func runIsolatedWarmup(ctx context.Context, executable string, credential quotaCredential, beforeSubmit func() error) (result WarmupExecution, err error) {
	if executable == "" {
		return result, errors.New("未找到可用的 Codex 可执行文件")
	}
	if credential.expires <= time.Now().Add(60*time.Second).Unix() {
		return result, errors.New("有效访问凭证即将过期，等待 CC Switch 刷新")
	}
	root, err := os.MkdirTemp("", "codex-helper-warmup-")
	if err != nil {
		return result, errors.New("不能创建独立预热目录")
	}
	defer os.RemoveAll(root)
	for _, dir := range []string{"codex", "work", "config", "data", "cache"} {
		if os.MkdirAll(filepath.Join(root, dir), 0700) != nil {
			return result, errors.New("不能创建独立预热环境")
		}
	}
	// No refresh token, copied config, history, hooks, skills or MCP servers.
	config := "cli_auth_credentials_store = \"ephemeral\"\nweb_search = \"disabled\"\n[analytics]\nenabled = false\n[features]\nhooks = false\nshell_tool = false\nexec_tool = false\napply_patch_freeform = false\napps = false\nmulti_agent = false\n"
	if os.WriteFile(filepath.Join(root, "codex", "config.toml"), []byte(config), 0600) != nil {
		return result, errors.New("不能写入临时预热配置")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "app-server", "--listen", "stdio://")
	cmd.Dir = filepath.Join(root, "work")
	cmd.Env = warmupEnvironment(root)
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return result, errors.New("不能创建预热输入管道")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result, errors.New("不能创建预热输出管道")
	}
	if cmd.Start() != nil {
		return result, errors.New("独立 Codex 进程启动失败")
	}
	defer func() { _ = in.Close(); _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); _ = cmd.Wait() }()
	scan := bufio.NewScanner(io.LimitReader(out, 32<<20))
	scan.Buffer(make([]byte, 4096), 4<<20)
	rpc := warmupRPC{in: in, lines: scan, result: &result}
	if _, err = rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_helper_warmup", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return result, err
	}
	if err = rpc.send(map[string]any{"method": "initialized"}); err != nil {
		return result, err
	}
	login, err := rpc.call("account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": credential.access, "chatgptAccountId": credential.workspace})
	if err != nil {
		return result, err
	}
	var authResult struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(login, &authResult) != nil || authResult.Type != "chatgptAuthTokens" {
		return result, errors.New("未确认独立外部令牌认证模式")
	}
	result.IdentityVerified = true
	if _, statErr := os.Stat(filepath.Join(root, "codex", "auth.json")); !os.IsNotExist(statErr) {
		return result, errors.New("临时执行环境未满足无认证落盘要求")
	}
	catalog, err := rpc.call("model/list", map[string]any{"limit": 100})
	if err != nil {
		return result, err
	}
	var models struct {
		Data []warmupModel `json:"data"`
	}
	if json.Unmarshal(catalog, &models) != nil {
		return result, errors.New("模型目录不可读取")
	}
	result.Model, result.Effort = chooseWarmupModel(models.Data)
	if result.Model == "" {
		return result, errors.New("该账号未提供可用预热模型")
	}
	thread, err := rpc.call("thread/start", map[string]any{"ephemeral": true, "model": result.Model, "cwd": cmd.Dir, "approvalPolicy": "never", "sandbox": "read-only", "baseInstructions": "Reply with exactly OK. Never use tools.", "developerInstructions": "This is a single minimal connectivity request. Reply OK and stop."})
	if err != nil {
		return result, err
	}
	var started struct {
		Thread struct {
			ID        string `json:"id"`
			Ephemeral bool   `json:"ephemeral"`
		} `json:"thread"`
	}
	if json.Unmarshal(thread, &started) != nil || started.Thread.ID == "" || !started.Thread.Ephemeral {
		return result, errors.New("未确认临时预热会话")
	}
	rpc.thread = started.Thread.ID
	if err = beforeSubmit(); err != nil {
		return result, err
	}
	// The durable callback runs before this possibly billable write. Any loss
	// after this point is ambiguous and must be verified, never blindly retried.
	result.Submitted = true
	turnParams := map[string]any{"threadId": rpc.thread, "input": []any{map[string]any{"type": "text", "text": "Reply OK.", "text_elements": []any{}}}}
	if result.Effort != "" {
		turnParams["effort"] = result.Effort
	}
	turn, err := rpc.call("turn/start", turnParams)
	if err != nil {
		return result, err
	}
	var begun struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(turn, &begun) != nil || begun.Turn.ID == "" {
		return result, errors.New("未确认预热轮次")
	}
	rpc.turn = begun.Turn.ID
	for n := 0; n < 10000; n++ {
		msg, e := rpc.receive()
		if e != nil {
			return result, e
		}
		if msg.Method != "turn/completed" {
			continue
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.ThreadID != rpc.thread || p.Turn.ID != rpc.turn {
			continue
		}
		if p.Turn.Status != "completed" {
			return result, errors.New("预热轮次未正常完成")
		}
		result.Completed = true
		return result, nil
	}
	return result, errors.New("预热轮次消息超出上限")
}

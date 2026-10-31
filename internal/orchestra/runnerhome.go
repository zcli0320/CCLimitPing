package orchestra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type RunnerLoginStatus struct {
	LoggedIn bool   `json:"logged_in"`
	State    string `json:"state"`
	Mode     string `json:"mode"`
	Detail   string `json:"detail"`
}

// EnsureRunnerHome creates only missing files, and refuses redirected paths.
func EnsureRunnerHome(home string) (dir string, created []string, err error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		return "", nil, errors.New("执行账号目录无效")
	}
	dir = filepath.Join(home, ".codex-runner")
	if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return dir, nil, err
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return dir, nil, errors.New("执行账号目录不是普通文件夹")
	}
	config, err := runnerConfig(home)
	if err != nil {
		return dir, nil, err
	}
	files := []struct{ name, data string }{{"config.toml", config}, {"AGENTS.md", runnerInstructions(home)}}
	created = []string{}
	for _, item := range files {
		path := filepath.Join(dir, item.name)
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(e) {
			if info, e := os.Lstat(path); e != nil || !info.Mode().IsRegular() {
				return dir, created, fmt.Errorf("%s 不是普通文件", item.name)
			}
			continue
		}
		if e != nil {
			return dir, created, e
		}
		_, e = f.WriteString(item.data)
		closeErr := f.Close()
		if e != nil {
			return dir, created, e
		}
		if closeErr != nil {
			return dir, created, closeErr
		}
		created = append(created, item.name)
	}
	return dir, created, nil
}

// runnerConfig derives the runner's config from the user's own Codex config at
// setup time, so nothing machine- or person-specific is compiled in. Only the
// model choice, the selected provider's connection fields and proxy settings
// are carried over; credentials, headers and MCP servers never are.
func runnerConfig(home string) (string, error) {
	var user map[string]any
	if data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml")); err == nil {
		if _, err := toml.Decode(string(data), &user); err != nil {
			user = nil // unreadable: fall back to Codex defaults
		}
	}
	out := map[string]any{}
	for _, key := range []string{"model", "model_reasoning_effort", "model_verbosity", "model_provider", "web_search"} {
		if v, ok := user[key]; ok {
			out[key] = v
		}
	}
	if name, ok := user["model_provider"].(string); ok {
		providers, _ := user["model_providers"].(map[string]any)
		if p, ok := providers[name].(map[string]any); ok {
			safe := map[string]any{}
			for _, key := range []string{"name", "base_url", "wire_api", "requires_openai_auth", "supports_websockets"} {
				if v, ok := p[key]; ok {
					safe[key] = v
				}
			}
			out["model_providers"] = map[string]any{name: safe}
		}
	}
	if features, ok := user["features"].(map[string]any); ok {
		if v, ok := features["respect_system_proxy"]; ok {
			out["features"] = map[string]any{"respect_system_proxy": v}
		}
	}
	if policy, ok := user["shell_environment_policy"].(map[string]any); ok {
		set, _ := policy["set"].(map[string]any)
		proxies := map[string]any{}
		for k, v := range set {
			if strings.HasSuffix(strings.ToLower(k), "_proxy") {
				proxies[k] = v
			}
		}
		if len(proxies) > 0 {
			out["shell_environment_policy"] = map[string]any{"set": proxies}
		}
	}
	out["cli_auth_credentials_store"] = "file"
	out["sandbox_mode"] = "workspace-write"
	out["approval_policy"] = "never"
	out["suppress_unstable_features_warning"] = true
	roots := []string{}
	for _, rel := range []string{"Library/Caches/go-build", "go/pkg/mod", ".cargo"} {
		if info, err := os.Stat(filepath.Join(home, rel)); err == nil && info.IsDir() {
			roots = append(roots, filepath.Join(home, rel))
		}
	}
	out["sandbox_workspace_write"] = map[string]any{"network_access": true, "writable_roots": roots}
	var b strings.Builder
	b.WriteString("# Codex Helper task runner. Created once from ~/.codex/config.toml; edit freely.\n")
	if err := toml.NewEncoder(&b).Encode(out); err != nil {
		return "", err
	}
	return b.String(), nil
}

const runnerSection = `
# Codex runner
You run headless for another agent's task cards. No human watches the run.
Never quit or restart Codex Desktop or CC Switch, log in, or change real accounts
unless the card says the user approved it. Your final message is your receipt.
`

// runnerInstructions keeps the user's own global Codex instructions, minus the
// agent-bridge block (the runner relays receipts itself).
func runnerInstructions(home string) string {
	data, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	text := string(data)
	if i := strings.Index(text, "<!-- agent-bridge:start -->"); i >= 0 {
		if j := strings.Index(text[i:], "<!-- agent-bridge:end -->"); j >= 0 {
			text = text[:i] + text[i+j+len("<!-- agent-bridge:end -->"):]
		}
	}
	return strings.TrimSpace(text+"\n"+runnerSection) + "\n"
}

// RunnerLogin never returns raw CLI output, which can contain account details.
func RunnerLogin(home, codexBin string) RunnerLoginStatus {
	unknown := RunnerLoginStatus{State: "unknown", Detail: "无法读取登录状态"}
	if !filepath.IsAbs(home) || codexBin == "" {
		return unknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, "login", "status")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_HOME=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+filepath.Join(home, ".codex-runner"))
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return unknown
	}
	text := strings.ToLower(string(out))
	if strings.Contains(text, "not logged in") {
		return RunnerLoginStatus{State: "not_logged_in", Detail: "未登录"}
	}
	if err != nil {
		return unknown
	}
	if strings.Contains(text, "logged in") {
		mode := ""
		if strings.Contains(text, "chatgpt") {
			mode = "ChatGPT"
		} else if strings.Contains(text, "api key") {
			mode = "API key"
		}
		return RunnerLoginStatus{LoggedIn: true, State: "logged_in", Mode: mode, Detail: "已登录"}
	}
	return unknown
}

// Package orchestra lets one coding agent (the lead) hand task cards to another
// agent (the executor) and get a receipt back. Executors run headless as
// child processes of whoever runs the Runner (the helper app or a terminal),
// so they keep that process's network and never inherit the lead's sandbox.
package orchestra

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Agent describes how to start one kind of executor headless.
type Agent struct {
	Name  string   `json:"name"`
	Label string   `json:"label"`
	Bin   string   `json:"bin"`
	Env   []string `json:"env,omitempty"`  // extra KEY=VALUE pairs
	Args  []string `json:"args,omitempty"` // extra flags (sandbox, permissions)
}

// Invocation is one prepared executor process.
type Invocation struct {
	Argv  []string
	Dir   string
	Env   []string
	Stdin string
}

var errNoCLI = errors.New("not supported")

const bundledCodex = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"

// DefaultAgents are the executors the helper knows. Bins are resolved at use.
func DefaultAgents(home string) map[string]Agent {
	codexBin := bundledCodex
	if _, err := os.Stat(codexBin); err != nil {
		codexBin = "codex"
	}
	// The override is restricted to an explicitly marked isolated test root.
	if testHome := os.Getenv("CODEX_HELPER_TEST_HOME"); filepath.IsAbs(testHome) && testHome != home && isFile(filepath.Join(testHome, ".codex-helper-test-root")) {
		if override := os.Getenv("CODEX_HELPER_ORCHESTRA_CODEX_BIN"); filepath.IsAbs(override) {
			codexBin = override
		}
	}
	codex := Agent{Name: "codex", Label: "Codex", Bin: codexBin,
		// -c, not -s: `codex exec resume` takes only -c overrides.
		Args: []string{"-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true"}}
	// A separate CODEX_HOME keeps the executor's account out of account-switch tests.
	codex.Env = []string{"CODEX_HOME=" + filepath.Join(home, ".codex-runner")}
	return map[string]Agent{
		"codex": codex,
		"claude": {Name: "claude", Label: "Claude Code", Bin: "claude",
			Args: []string{"--permission-mode", "acceptEdits", "--allowedTools", "Bash,Read,Edit,Write,Glob,Grep"}},
		"antigravity": {Name: "antigravity", Label: "Antigravity", Bin: "agy"},
	}
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// Resolve finds the agent's executable in path (a PATH list; the login-shell
// PATH, so it matches what the executor process gets); an error says why it
// can't be used.
func (a Agent) Resolve(path string) (string, error) {
	if a.Name == "antigravity" {
		return "", fmt.Errorf("Antigravity 的无界面命令行尚未接入（未找到 agy 或参数未核实）")
	}
	if filepath.IsAbs(a.Bin) {
		if !isFile(a.Bin) {
			return "", fmt.Errorf("%s 程序不存在：%s", a.Label, a.Bin)
		}
		return a.Bin, nil
	}
	if !strings.ContainsRune(a.Bin, os.PathSeparator) {
		for _, dir := range filepath.SplitList(path) {
			if !filepath.IsAbs(dir) {
				continue
			}
			p := filepath.Join(dir, a.Bin)
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("未找到 %s 命令行（%s）", a.Label, a.Bin)
}

// prepare builds the process for a new task (session == "") or a follow-up in
// an existing executor session. lastFile receives Codex's final message.
func (a Agent) prepare(path, session, folder, prompt, lastFile string) (Invocation, error) {
	bin, err := a.Resolve(path)
	if err != nil {
		return Invocation{}, err
	}
	inv := Invocation{Dir: folder, Env: append(os.Environ(), a.Env...), Stdin: prompt}
	switch a.Name {
	case "codex":
		argv := []string{bin, "exec"}
		if session != "" {
			argv = append(argv, "resume", session)
		}
		argv = append(argv, a.Args...)
		argv = append(argv, "--json", "--skip-git-repo-check", "-o", lastFile)
		if session == "" {
			argv = append(argv, "-C", folder)
		}
		inv.Argv = append(argv, "-")
	case "claude":
		argv := []string{bin, "-p", "--output-format", "json"}
		if session != "" {
			argv = append(argv, "--resume", session)
		}
		inv.Argv = append(argv, a.Args...)
	default:
		return Invocation{}, fmt.Errorf("%s: %w", a.Label, errNoCLI)
	}
	return inv, nil
}

// result reads the executor's session id and final message from its output.
func (a Agent) result(stdoutPath, lastFile string) (session, final string, err error) {
	data, _ := os.ReadFile(stdoutPath)
	switch a.Name {
	case "codex":
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, `"thread.started"`) {
				continue
			}
			var ev struct {
				ThreadID string `json:"thread_id"`
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.ThreadID != "" {
				session = ev.ThreadID
				break
			}
		}
		last, _ := os.ReadFile(lastFile)
		return session, strings.TrimSpace(string(last)), nil
	case "claude":
		var out struct {
			SessionID string `json:"session_id"`
			Result    string `json:"result"`
			IsError   bool   `json:"is_error"`
		}
		if e := json.Unmarshal(bytesTrim(data), &out); e != nil {
			return "", "", errors.New("Claude 输出不是预期的 JSON")
		}
		if out.IsError {
			return out.SessionID, "", fmt.Errorf("Claude 报告错误：%s", tail(out.Result, 400))
		}
		return out.SessionID, strings.TrimSpace(out.Result), nil
	}
	return "", "", errNoCLI
}

func bytesTrim(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

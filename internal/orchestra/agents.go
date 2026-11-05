// Package orchestra lets one coding agent (the lead) hand task cards to another
// agent (the executor) and get a receipt back. Executors run headless as
// child processes of whoever runs the Runner (the helper app or a terminal),
// so they keep that process's network and never inherit the lead's sandbox.
package orchestra

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
		Args: []string{"-c", `model="gpt-6.1-terra"`, "-c", `model_reasoning_effort="xhigh"`,
			"-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.network_access=true"}}
	return map[string]Agent{
		"codex": codex,
		"claude": {Name: "claude", Label: "Claude Code", Bin: "claude",
			Args: []string{"--permission-mode", "acceptEdits", "--allowedTools", "Bash,Read,Edit,Write,Glob,Grep"}},
		// HTTP/2 off: through the user's flaky proxy, agy's pooled HTTP/2 streams die
		// mid-run (instant EOF on reuse, or ~250s stalls); HTTP/1.1 recovers better.
		"antigravity": {Name: "antigravity", Label: "Antigravity", Bin: "agy", Env: []string{"GODEBUG=http2client=0"},
			Args: []string{"--model", "gemini-4-argon-medium", "--mode", "accept-edits", "--settings", `{"permissions":{"allow":["command(*)"]}}`, "--output-format", "json"}},
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
	env := os.Environ()
	if a.Name == "codex" {
		filtered := env[:0]
		for _, v := range env {
			if !strings.HasPrefix(v, "CODEX_HOME=") {
				filtered = append(filtered, v)
			}
		}
		env = filtered
	}
	inv := Invocation{Dir: folder, Env: append(env, a.Env...), Stdin: prompt}
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
		argv := []string{bin, "-p", "--output-format", "stream-json", "--verbose"}
		if session != "" {
			argv = append(argv, "--resume", session)
		}
		inv.Argv = append(argv, a.Args...)
	case "antigravity":
		argv := append([]string{bin}, a.Args...)
		if session != "" {
			argv = append(argv, "--conversation", session)
		}
		inv.Argv = append(argv, "-p", prompt)
		inv.Stdin = ""
	default:
		return Invocation{}, fmt.Errorf("%s: %w", a.Label, errNoCLI)
	}
	return inv, nil
}

// result reads the executor's session id and final message from its output.
// run only matters for Antigravity; see agyTranscriptFinal.
func (a Agent) result(stdoutPath, lastFile string, run AGYRunIdentity) (session, final string, err error) {
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
	case "antigravity":
		var out struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			Response       string `json:"response"`
			DeniedActions  []struct {
				Action      string `json:"action"`
				DisplayName string `json:"display_name"`
			} `json:"denied_actions"`
		}
		payload := bytesTrim(data)
		if json.Unmarshal(payload, &out) != nil {
			lines := strings.Split(string(payload), "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				line := strings.TrimSpace(lines[i])
				if strings.HasPrefix(line, "{") {
					payload = []byte(line)
					break
				}
			}
		}
		if e := json.Unmarshal(payload, &out); e != nil {
			if conv := agyFindConversationByMarker(run.Marker); conv != "" {
				if final := agyTranscriptFinal(conv, run); final != "" {
					return conv, final, nil
				}
				return conv, "", errors.New("Antigravity 输出不是预期的 JSON（已绑定对话会话）")
			}
			return out.ConversationID, "", errors.New("Antigravity 输出不是预期的 JSON")
		}
		if out.ConversationID == "" && run.Marker != "" {
			out.ConversationID = agyFindConversationByMarker(run.Marker)
		}
		if len(out.DeniedActions) > 0 {
			names := make([]string, 0, len(out.DeniedActions))
			for _, action := range out.DeniedActions {
				name := action.DisplayName
				if name == "" {
					name = action.Action
				}
				names = append(names, name)
			}
			return out.ConversationID, "", fmt.Errorf("Antigravity 拒绝了操作：%s", strings.Join(names, "、"))
		}
		if out.Status != "SUCCESS" {
			// A stream that dies after the turn already finished (agy's
			// post-reply EOF) still left the answer in the transcript.
			if final := agyTranscriptFinal(out.ConversationID, run); final != "" {
				return out.ConversationID, final, nil
			}
			return out.ConversationID, "", fmt.Errorf("Antigravity 报告错误：%s", out.Status)
		}
		if strings.TrimSpace(out.Response) == "" {
			return out.ConversationID, "", errors.New("Antigravity 返回的回复为空")
		}
		return out.ConversationID, strings.TrimSpace(out.Response), nil
	case "claude":
		var out struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Result    string `json:"result"`
			IsError   bool   `json:"is_error"`
		}
		payload := bytesTrim(data)
		if json.Unmarshal(payload, &out) != nil || (out.Result == "" && out.SessionID == "") {
			lines := strings.Split(string(payload), "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				line := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(line, "{") {
					continue
				}
				var candidate struct {
					Type      string `json:"type"`
					SessionID string `json:"session_id"`
					Result    string `json:"result"`
					IsError   bool   `json:"is_error"`
				}
				if json.Unmarshal([]byte(line), &candidate) == nil {
					if out.SessionID == "" && candidate.SessionID != "" {
						out.SessionID = candidate.SessionID
					}
					if candidate.Type == "result" || candidate.Result != "" || candidate.IsError {
						out = candidate
						break
					}
				}
			}
		}
		if out.SessionID == "" && out.Result == "" && !out.IsError {
			return "", "", errors.New("Claude 输出不是预期的 JSON")
		}
		if out.IsError {
			return out.SessionID, "", fmt.Errorf("Claude 报告错误：%s", tail(out.Result, 400))
		}
		return out.SessionID, strings.TrimSpace(out.Result), nil
	}
	return "", "", errNoCLI
}

// AGYBrainRoot is where agy keeps per-conversation transcripts.
var AGYBrainRoot = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain")
}

// AGYNoFallback disables the transcript fallback for a run whose conversation
// could not be measured before launch.
const AGYNoFallback = math.MaxInt

type agyStep struct {
	StepIndex *int              `json:"step_index"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Content   string            `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

func agyTranscript(conversation string) ([]agyStep, bool) {
	if conversation == "" || strings.ContainsAny(conversation, "/\\") || strings.HasPrefix(conversation, ".") {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(AGYBrainRoot(), conversation, ".system_generated", "logs", "transcript.jsonl"))
	if err != nil {
		return nil, false
	}
	var steps []agyStep
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var step agyStep
		// A torn record or missing index cannot prove which turn completed.
		if json.Unmarshal([]byte(line), &step) != nil || step.StepIndex == nil || *step.StepIndex < 0 {
			return nil, false
		}
		steps = append(steps, step)
	}
	return steps, true
}

// AGYTranscriptLastStep is the highest step index already in a conversation,
// recorded before a resumed run so only its own turn can count; -1 when the
// conversation has no transcript yet, AGYNoFallback when it is unreadable.
func AGYTranscriptLastStep(conversation string) int {
	if conversation == "" {
		return -1
	}
	if _, err := os.Stat(filepath.Join(AGYBrainRoot(), conversation)); os.IsNotExist(err) {
		return -1
	}
	steps, ok := agyTranscript(conversation)
	if !ok {
		return AGYNoFallback
	}
	last := -1
	for _, s := range steps {
		if *s.StepIndex > last {
			last = *s.StepIndex
		}
	}
	return last
}

// AGYRunIdentity ties a transcript turn to one run: the turn must start after
// AfterStep (the conversation's last step before launch) and its input must
// carry Marker, which the runner appends to the prompt.
type AGYRunIdentity struct {
	AfterStep int
	Marker    string
}

// agyTranscriptFinal returns the conversation's final answer only when the
// newest turn is this run's (see AGYRunIdentity), its steps are contiguous and
// all finished, every tool call got a result, and it ended with a finished
// model reply without tool calls. Anything it cannot vouch for fails closed.
func agyTranscriptFinal(conversation string, run AGYRunIdentity) string {
	if run.AfterStep == AGYNoFallback || strings.TrimSpace(run.Marker) == "" {
		return ""
	}
	steps, ok := agyTranscript(conversation)
	if !ok {
		return ""
	}
	turn := -1
	for i, s := range steps {
		if s.Type == "USER_INPUT" {
			turn = i
		}
	}
	if turn < 0 || *steps[turn].StepIndex <= run.AfterStep || !strings.Contains(steps[turn].Content, run.Marker) {
		return ""
	}
	pending := 0
	for i, s := range steps[turn:] {
		if *s.StepIndex != *steps[turn].StepIndex+i || s.Status != "DONE" {
			return ""
		}
		switch s.Type {
		case "USER_INPUT", "ERROR_MESSAGE":
		case "PLANNER_RESPONSE":
			if pending > 0 {
				return ""
			}
			pending = len(s.ToolCalls)
		case "GENERIC":
			if pending == 0 {
				return ""
			}
			pending--
		default:
			return ""
		}
	}
	last := steps[len(steps)-1]
	if pending > 0 || last.Type != "PLANNER_RESPONSE" || len(last.ToolCalls) > 0 {
		return ""
	}
	return strings.TrimSpace(last.Content)
}

func bytesTrim(b []byte) []byte { return []byte(strings.TrimSpace(string(b))) }

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

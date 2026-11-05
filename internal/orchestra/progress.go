package orchestra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AGYStreamRecoveryPrompt instructs a resumed AGY session to verify already
// landed file changes before finishing the remaining work after a disconnect.
const AGYStreamRecoveryPrompt = "刚才执行连接中断（网络或模型断流）。请先使用 read_file 检查已落盘的文件修改与当前进展，不要盲目重复已完成的操作，只继续完成剩余未完成的部分，最后按格式输出 RECEIPT。"

// TaskProgress summarizes live or recent step-by-step execution activity for
// a task run so the UI and MCP lead do not see a black box while an agent runs.
type TaskProgress struct {
	State          string    `json:"state"`                     // starting | working | retrying | checking | interrupted | finished
	Summary        string    `json:"summary"`                   // one-line status for tables and headers
	LatestStage    string    `json:"latest_stage,omitempty"`    // newest stage milestone or assistant note
	Stages         []string  `json:"stages,omitempty"`          // recent stage milestones (up to 6)
	RecentActions  []string  `json:"recent_actions,omitempty"`  // recent tool calls or commands (up to 6)
	Steps          int       `json:"steps,omitempty"`           // step count observed in the current run
	Warning        string    `json:"warning,omitempty"`         // stream drop, model retry, or stall warning
	ConversationID string    `json:"conversation_id,omitempty"` // bound executor session / conversation id
	LastActive     time.Time `json:"last_active,omitempty"`     // last log/transcript modification timestamp
	IdleSeconds    int       `json:"idle_seconds,omitempty"`    // seconds since LastActive while running
}

// agyFindConversationByMarker locates the Antigravity conversation whose
// USER_INPUT contains the unique run marker injected by Runner.start.
func agyFindConversationByMarker(marker string) string {
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return ""
	}
	root := AGYBrainRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	type candidate struct {
		id  string
		mod time.Time
	}
	var list []candidate
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		logPath := filepath.Join(root, e.Name(), ".system_generated", "logs", "transcript.jsonl")
		if info, err := os.Stat(logPath); err == nil && info.Mode().IsRegular() {
			list = append(list, candidate{id: e.Name(), mod: info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	if len(list) > 30 {
		list = list[:30]
	}
	needle := "Run marker: " + marker
	for _, c := range list {
		data, err := os.ReadFile(filepath.Join(root, c.id, ".system_generated", "logs", "transcript.jsonl"))
		if err != nil {
			continue
		}
		if strings.Contains(string(data), needle) || strings.Contains(string(data), marker) {
			return c.id
		}
	}
	return ""
}

// agyTranscriptInterrupted reports whether a run's turn started in the
// conversation transcript (matched by Marker after AfterStep) but ended
// without a completed final reply (e.g. stream EOF / Model unavailable).
func agyTranscriptInterrupted(conversation string, run AGYRunIdentity) bool {
	if conversation == "" || run.AfterStep == AGYNoFallback || strings.TrimSpace(run.Marker) == "" {
		return false
	}
	if agyTranscriptFinal(conversation, run) != "" {
		return false
	}
	steps := agyTranscriptForgiving(conversation)
	if len(steps) == 0 {
		return false
	}
	turn := -1
	for i, s := range steps {
		if s.Type == "USER_INPUT" && strings.Contains(s.Content, run.Marker) {
			turn = i
		}
	}
	if turn < 0 {
		return false
	}
	if steps[turn].StepIndex != nil && *steps[turn].StepIndex <= run.AfterStep {
		return false
	}
	return len(steps[turn:]) >= 1
}

type agyLiveStep struct {
	StepIndex *int              `json:"step_index"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Content   string            `json:"content"`
	Error     string            `json:"error"`
	Thinking  string            `json:"thinking"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

func agyTranscriptForgiving(conversation string) []agyLiveStep {
	if conversation == "" || strings.ContainsAny(conversation, "/\\") || strings.HasPrefix(conversation, ".") {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(AGYBrainRoot(), conversation, ".system_generated", "logs", "transcript.jsonl"))
	if err != nil {
		return nil
	}
	var steps []agyLiveStep
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var step agyLiveStep
		if json.Unmarshal([]byte(line), &step) == nil && step.Type != "" {
			steps = append(steps, step)
		}
	}
	return steps
}

// Progress inspects live or most-recent execution logs/transcripts for t.
func Progress(t *Task) *TaskProgress {
	if t == nil || len(t.Runs) == 0 {
		return nil
	}
	run := t.Runs[len(t.Runs)-1]
	var p *TaskProgress
	switch t.Executor {
	case "antigravity":
		p = progressAntigravity(t, &run)
	case "codex":
		p = progressCodex(t, &run)
	case "claude":
		p = progressClaude(t, &run)
	default:
		p = &TaskProgress{State: "working", Summary: "执行中"}
	}
	if p == nil {
		return nil
	}
	if run.Phase == "checking" && t.Status == Running {
		p.State = "checking"
		p.Summary = "正在执行项目检查命令…"
	}
	return p
}

func progressAntigravity(t *Task, run *Run) *TaskProgress {
	conv := t.Session
	if run.Marker != "" {
		if found := agyFindConversationByMarker(run.Marker); found != "" {
			conv = found
		}
	}
	p := &TaskProgress{ConversationID: conv}
	if conv != "" {
		logPath := filepath.Join(AGYBrainRoot(), conv, ".system_generated", "logs", "transcript.jsonl")
		if info, err := os.Stat(logPath); err == nil {
			p.LastActive = info.ModTime()
		}
	}
	if p.LastActive.IsZero() && run.Log != "" {
		if info, err := os.Stat(run.Log); err == nil {
			p.LastActive = info.ModTime()
		}
	}
	if p.LastActive.IsZero() {
		p.LastActive = run.Started
	}
	if t.Status == Running && !p.LastActive.IsZero() {
		if sec := int(time.Since(p.LastActive).Seconds()); sec > 0 {
			p.IdleSeconds = sec
		}
	}
	steps := agyTranscriptForgiving(conv)
	if len(steps) == 0 {
		if t.Status == Running {
			p.State = "starting"
			p.Summary = "正在启动 Antigravity 并建立会话…"
		} else if t.Status == Failed {
			p.State = "interrupted"
			p.Summary = "未读取到 Antigravity 阶段轨迹"
		} else {
			p.State = "finished"
			p.Summary = "执行已完成"
		}
		return p
	}
	turn := 0
	for i, s := range steps {
		if s.Type == "USER_INPUT" && (run.Marker == "" || strings.Contains(s.Content, run.Marker)) {
			turn = i
		}
	}
	turnSteps := steps[turn:]
	p.Steps = len(turnSteps)
	errCount := 0
	lastErr := ""
	for _, s := range turnSteps {
		switch s.Type {
		case "PLANNER_RESPONSE":
			if stage := cleanStageLine(s.Content); stage != "" {
				p.Stages = appendCapped(p.Stages, stage, 6)
				p.LatestStage = stage
			} else if think := cleanStageLine(s.Thinking); think != "" && p.LatestStage == "" {
				p.LatestStage = "思考：" + think
			}
			for _, raw := range s.ToolCalls {
				if act := summarizeAGYToolCall(raw); act != "" {
					p.RecentActions = appendCapped(p.RecentActions, act, 6)
				}
			}
		case "ERROR_MESSAGE":
			errCount++
			msg := strings.TrimSpace(s.Error)
			if msg == "" {
				msg = strings.TrimSpace(s.Content)
			}
			lastErr = firstLine(msg, 100)
		}
	}
	finalizeAGYProgress(t, p, turnSteps, errCount, lastErr)
	return p
}
func finalizeAGYProgress(t *Task, p *TaskProgress, turnSteps []agyLiveStep, errCount int, lastErr string) {
	lastStep := turnSteps[len(turnSteps)-1]
	lastIsError := lastStep.Type == "ERROR_MESSAGE"
	if errCount > 0 {
		if lastIsError {
			p.Warning = fmt.Sprintf("检测到 AGY 断流/异常（共 %d 次）：%s", errCount, lastErr)
		} else {
			p.Warning = fmt.Sprintf("本轮已扛过 %d 次模型重试/断流（最近：%s）", errCount, lastErr)
		}
	} else if t.Status == Running && p.IdleSeconds >= 45 {
		p.Warning = fmt.Sprintf("已 %ds 无新步骤落盘，可能正在执行长命令或等待网络响应", p.IdleSeconds)
	}
	switch {
	case t.Status == Running && lastIsError:
		p.State = "retrying"
		p.Summary = fmt.Sprintf("第 %d 步 · 模型断流重试中（%s）", p.Steps, lastErr)
	case t.Status == Running:
		p.State = "working"
		if len(p.RecentActions) > 0 {
			p.Summary = fmt.Sprintf("第 %d 步 · %s", p.Steps, p.RecentActions[len(p.RecentActions)-1])
		} else if p.LatestStage != "" {
			p.Summary = fmt.Sprintf("第 %d 步 · %s", p.Steps, p.LatestStage)
		} else {
			p.Summary = fmt.Sprintf("第 %d 步 · 正在思考与规划…", p.Steps)
		}
	case t.Status == Failed:
		p.State = "interrupted"
		p.Summary = fmt.Sprintf("在第 %d 步中断", p.Steps)
		if p.LatestStage != "" {
			p.Summary += " · " + p.LatestStage
		}
	default:
		p.State = "finished"
		p.Summary = fmt.Sprintf("已完成（共 %d 步）", p.Steps)
	}
}

func progressCodex(t *Task, run *Run) *TaskProgress {
	p := &TaskProgress{ConversationID: t.Session, LastActive: run.Started}
	if run.Log != "" {
		if info, err := os.Stat(run.Log); err == nil {
			p.LastActive = info.ModTime()
		}
	}
	if t.Status == Running && !p.LastActive.IsZero() {
		if sec := int(time.Since(p.LastActive).Seconds()); sec > 0 {
			p.IdleSeconds = sec
		}
	}
	data, _ := os.ReadFile(run.Log)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Command string `json:"command"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.ThreadID != "" && p.ConversationID == "" {
			p.ConversationID = ev.ThreadID
		}
		switch ev.Type {
		case "item.started", "item.completed":
			p.Steps++
			switch ev.Item.Type {
			case "agent_message", "reasoning":
				if stage := cleanStageLine(ev.Item.Text); stage != "" {
					p.Stages = appendCapped(p.Stages, stage, 6)
					p.LatestStage = stage
				}
			case "command_execution":
				if cmd := firstLine(ev.Item.Command, 80); cmd != "" {
					p.RecentActions = appendCapped(p.RecentActions, "命令: "+cmd, 6)
				}
			case "file_change":
				p.RecentActions = appendCapped(p.RecentActions, "修改文件", 6)
			}
		case "error", "turn.failed":
			if msg := firstLine(ev.Message, 100); msg != "" {
				p.Warning = "Codex 异常：" + msg
			}
		}
	}
	finalizeGenericProgress(t, p, "Codex")
	return p
}
func progressClaude(t *Task, run *Run) *TaskProgress {
	p := &TaskProgress{ConversationID: t.Session, LastActive: run.Started}
	if run.Log != "" {
		if info, err := os.Stat(run.Log); err == nil {
			p.LastActive = info.ModTime()
		}
	}
	if t.Status == Running && !p.LastActive.IsZero() {
		if sec := int(time.Since(p.LastActive).Seconds()); sec > 0 {
			p.IdleSeconds = sec
		}
	}
	data, _ := os.ReadFile(run.Log)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct {
					Type  string          `json:"type"`
					Text  string          `json:"text"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.SessionID != "" && p.ConversationID == "" {
			p.ConversationID = ev.SessionID
		}
		if ev.Type == "assistant" {
			p.Steps++
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if stage := cleanStageLine(c.Text); stage != "" {
						p.Stages = appendCapped(p.Stages, stage, 6)
						p.LatestStage = stage
					}
				case "tool_use":
					if c.Name != "" {
						p.RecentActions = appendCapped(p.RecentActions, c.Name, 6)
					}
				}
			}
		}
	}
	finalizeGenericProgress(t, p, "Claude")
	return p
}

func finalizeGenericProgress(t *Task, p *TaskProgress, label string) {
	if t.Status == Running && p.IdleSeconds >= 45 && p.Warning == "" {
		p.Warning = fmt.Sprintf("已 %ds 无新日志输出，可能正在推理或执行命令", p.IdleSeconds)
	}
	switch {
	case t.Status == Running && p.Steps == 0:
		p.State = "starting"
		p.Summary = fmt.Sprintf("正在启动 %s 会话…", label)
	case t.Status == Running:
		p.State = "working"
		if len(p.RecentActions) > 0 {
			p.Summary = fmt.Sprintf("第 %d 步 · %s", p.Steps, p.RecentActions[len(p.RecentActions)-1])
		} else if p.LatestStage != "" {
			p.Summary = fmt.Sprintf("第 %d 步 · %s", p.Steps, p.LatestStage)
		} else {
			p.Summary = fmt.Sprintf("第 %d 步 · 正在执行…", p.Steps)
		}
	case t.Status == Failed:
		p.State = "interrupted"
		p.Summary = fmt.Sprintf("在第 %d 步中断", p.Steps)
	default:
		p.State = "finished"
		p.Summary = fmt.Sprintf("已完成（共 %d 步）", p.Steps)
	}
}

func summarizeAGYToolCall(raw json.RawMessage) string {
	var tc struct {
		Name string `json:"name"`
		Args struct {
			Path        string `json:"Path"`
			CommandLine string `json:"CommandLine"`
			ToolSummary string `json:"toolSummary"`
		} `json:"args"`
	}
	if json.Unmarshal(raw, &tc) != nil || tc.Name == "" {
		return ""
	}
	cleanArg := func(v string) string {
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	if p := cleanArg(tc.Args.Path); p != "" {
		return fmt.Sprintf("%s (%s)", tc.Name, filepath.Base(p))
	}
	if ts := cleanArg(tc.Args.ToolSummary); ts != "" {
		return fmt.Sprintf("%s (%s)", tc.Name, firstLine(ts, 48))
	}
	if cmd := cleanArg(tc.Args.CommandLine); cmd != "" {
		return fmt.Sprintf("%s (%s)", tc.Name, firstLine(cmd, 48))
	}
	return tc.Name
}

func cleanStageLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "STAGE:") {
			return firstLine(strings.TrimSpace(line[6:]), 120)
		}
	}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "RECEIPT ") || strings.HasPrefix(line, "Changed:") || strings.HasPrefix(line, "Verified:") || strings.HasPrefix(line, "Doubts:") {
			continue
		}
		return firstLine(line, 120)
	}
	return ""
}

func firstLine(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.Split(s, "\n")[0])
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}

func appendCapped(list []string, item string, capN int) []string {
	if item == "" || (len(list) > 0 && list[len(list)-1] == item) {
		return list
	}
	list = append(list, item)
	if len(list) > capN {
		list = list[len(list)-capN:]
	}
	return list
}

package orchestra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProgressAntigravityCodexAndClaude(t *testing.T) {
	root := t.TempDir()
	old := AGYBrainRoot
	AGYBrainRoot = func() string { return root }
	t.Cleanup(func() { AGYBrainRoot = old })

	conv := "conv-live-1"
	logDir := filepath.Join(root, conv, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	steps := []string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"TASK A1\nRun marker: m-live-1"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"STAGE: 已定位问题并准备修改 Orchestra.swift","tool_calls":[{"name":"edit_file","args":{"Path":"/work/Orchestra.swift"}}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE","content":"Made 1 replacement"}`,
		`{"step_index":3,"type":"ERROR_MESSAGE","status":"DONE","error":"Model unavailable: stream reset"}`,
	}
	if err := os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(strings.Join(steps, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	agyTask := &Task{
		ID:       "A1",
		Executor: "antigravity",
		Status:   Running,
		Runs:     []Run{{Kind: "task", Started: time.Now(), Marker: "m-live-1"}},
	}
	p := Progress(agyTask)
	if p == nil || p.ConversationID != conv || p.State != "retrying" || p.Steps != 4 {
		t.Fatalf("agy progress=%+v", p)
	}
	if p.LatestStage != "已定位问题并准备修改 Orchestra.swift" || len(p.RecentActions) != 1 || p.RecentActions[0] != "edit_file (Orchestra.swift)" {
		t.Fatalf("agy stage/actions=%+v", p)
	}
	if !strings.Contains(p.Warning, "Model unavailable") {
		t.Fatalf("agy warning=%q", p.Warning)
	}

	codexLog := filepath.Join(t.TempDir(), "codex.out.jsonl")
	codexLines := []string{
		`{"type":"thread.started","thread_id":"th-99"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"STAGE: 已完成接口定义"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}`,
	}
	_ = os.WriteFile(codexLog, []byte(strings.Join(codexLines, "\n")+"\n"), 0o600)
	codexTask := &Task{
		ID: "X1", Executor: "codex", Status: Running,
		Runs: []Run{{Kind: "task", Started: time.Now(), Log: codexLog}},
	}
	cp := Progress(codexTask)
	if cp == nil || cp.ConversationID != "th-99" || cp.LatestStage != "已完成接口定义" || len(cp.RecentActions) != 1 || !strings.Contains(cp.Summary, "go test") {
		t.Fatalf("codex progress=%+v", cp)
	}

	claudeLog := filepath.Join(t.TempDir(), "claude.out.jsonl")
	claudeLines := []string{
		`{"type":"assistant","session_id":"cl-77","message":{"content":[{"type":"text","text":"STAGE: 正在核对单元测试"},{"type":"tool_use","name":"Bash"}]}}`,
	}
	_ = os.WriteFile(claudeLog, []byte(strings.Join(claudeLines, "\n")+"\n"), 0o600)
	claudeTask := &Task{
		ID: "C1", Executor: "claude", Status: Running,
		Runs: []Run{{Kind: "task", Started: time.Now(), Log: claudeLog}},
	}
	clp := Progress(claudeTask)
	if clp == nil || clp.ConversationID != "cl-77" || clp.LatestStage != "正在核对单元测试" || len(clp.RecentActions) != 1 || clp.RecentActions[0] != "Bash" {
		t.Fatalf("claude progress=%+v", clp)
	}
}

func TestAGYStreamDisconnectBindsSessionAndAutoRecoversOnce(t *testing.T) {
	e := setup(t)
	brain := t.TempDir()
	old := AGYBrainRoot
	AGYBrainRoot = func() string { return brain }
	t.Cleanup(func() { AGYBrainRoot = old })

	bin := filepath.Join(t.TempDir(), "agy")
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s\n' "$@" >> "$ARGS_LOG"
conv="agy-conv-recover"
dir="%s/$conv/.system_generated/logs"
mkdir -p "$dir"
prompt=""
for a in "$@"; do prompt="$a"; done
marker=$(printf '%%s' "$prompt" | grep -oE 'Run marker: [^ ]+' | tail -1 | cut -d' ' -f3)
if printf '%%s\n' "$@" | grep -q -- '--conversation'; then
  printf '{"step_index":2,"type":"USER_INPUT","status":"DONE","content":"resume Run marker: %%s"}\n' "$marker" >> "$dir/transcript.jsonl"
  printf '{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"RECEIPT G-REC: done"}\n' >> "$dir/transcript.jsonl"
  printf '{"conversation_id":"agy-conv-recover","status":"SUCCESS","response":"RECEIPT G-REC: done"}\n'
  exit 0
fi
printf '{"step_index":0,"type":"USER_INPUT","status":"DONE","content":"start Run marker: %%s"}\n' "$marker" > "$dir/transcript.jsonl"
printf '{"step_index":1,"type":"ERROR_MESSAGE","status":"DONE","error":"EOF"}\n' >> "$dir/transcript.jsonl"
exit 1
`, brain)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	agent := e.cfg.Agents["antigravity"]
	agent.Bin = bin
	e.cfg.Agents["antigravity"] = agent

	runFor(t, e)
	if _, err := e.store.Dispatch(e.cfg, "claude", "antigravity", e.folder, "G-REC", "TASK G-REC"); err != nil {
		t.Fatal(err)
	}
	task := waitDone(t, e, "G-REC")
	if task.Status != Receipt || task.Session != "agy-conv-recover" || len(task.Runs) != 2 || task.Runs[1].Kind != "stream_recovery" {
		t.Fatalf("recovered task=%+v", task)
	}
}

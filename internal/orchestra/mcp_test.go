package orchestra

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type mcpFixture struct {
	in    *io.PipeWriter
	lines *bufio.Reader
}

func startMCP(t *testing.T, e env) mcpFixture {
	t.Helper()
	in, writer := io.Pipe()
	reader, out := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeMCP(ctx, in, out, e.cfg, e.store, "claude"); out.Close() }()
	t.Cleanup(func() {
		writer.Close()
		cancel()
		if err := <-done; err != nil && err != context.Canceled {
			t.Errorf("ServeMCP: %v", err)
		}
		reader.Close()
	})
	return mcpFixture{writer, bufio.NewReader(reader)}
}
func (f mcpFixture) send(t *testing.T, s string) {
	t.Helper()
	if _, err := fmt.Fprintln(f.in, s); err != nil {
		t.Fatal(err)
	}
}
func (f mcpFixture) receive(t *testing.T) map[string]any {
	t.Helper()
	line, err := f.lines.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(line, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func mcpCall(t *testing.T, f mcpFixture, id int, name string, args any) map[string]any {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	f.send(t, string(b))
	return f.receive(t)
}
func resultText(t *testing.T, r map[string]any) (map[string]any, bool) {
	t.Helper()
	result, ok := r["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %v", r)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	var value map[string]any
	_ = json.Unmarshal([]byte(text), &value)
	return value, result["isError"] == true
}
func TestMCPProtocolAndTools(t *testing.T) {
	e := setup(t)
	f := startMCP(t, e)
	f.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	r := f.receive(t)
	init := r["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatal(init)
	}
	f.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	f.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	r = f.receive(t)
	if r["id"] != float64(2) {
		t.Fatalf("notification got a reply: %v", r)
	}
	tools := r["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 9 {
		t.Fatalf("tools=%v", tools)
	}
	seen := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		seen[tool["name"].(string)] = true
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Fatal(tool)
		}
	}
	for _, name := range []string{"list_agents", "runner_status", "dispatch_task", "get_task", "list_tasks", "wait_task", "rework_task", "cancel_task", "accept_task"} {
		if !seen[name] {
			t.Fatal(name)
		}
	}
	f.send(t, `{"jsonrpc":"2.0","id":3,"method":"missing"}`)
	r = f.receive(t)
	if r["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatal(r)
	}
	f.send(t, `{`)
	r = f.receive(t)
	if r["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatal(r)
	}
}
func TestMCPTaskLifecycleAndErrors(t *testing.T) {
	e := setup(t)
	f := startMCP(t, e)
	r := mcpCall(t, f, 1, "dispatch_task", map[string]any{"id": "T1", "executor": "codex", "folder": e.folder, "card": "TASK T1"})
	v, bad := resultText(t, r)
	if bad || v["status"] != "queued" || !strings.Contains(v["note"].(string), "orchestra run") {
		t.Fatal(r)
	}
	stored, err := e.store.Get("T1")
	if err != nil || stored.Lead != "claude" {
		t.Fatalf("lead: %#v %v", stored, err)
	}
	r = mcpCall(t, f, 2, "get_task", map[string]any{"id": "T1"})
	v, bad = resultText(t, r)
	if bad || v["status"] != "queued" {
		t.Fatal(r)
	}
	r = mcpCall(t, f, 3, "cancel_task", map[string]any{"id": "T1"})
	v, bad = resultText(t, r)
	if bad || v["status"] != "cancelled" {
		t.Fatal(r)
	}
	r = mcpCall(t, f, 4, "get_task", map[string]any{"id": "../bad"})
	_, bad = resultText(t, r)
	if !bad {
		t.Fatal(r)
	}
	r = mcpCall(t, f, 5, "dispatch_task", map[string]any{"executor": "bad"})
	_, bad = resultText(t, r)
	if !bad {
		t.Fatal(r)
	}
}
func TestMCPWaitBoundsAndRunnerStatus(t *testing.T) {
	for _, tc := range []struct {
		in   *int
		want time.Duration
	}{{nil, 300 * time.Second}, {intPtr(999), 540 * time.Second}, {intPtr(5), 5 * time.Second}} {
		d, e := waitTimeout(tc.in)
		if e != nil || d != tc.want {
			t.Fatalf("timeout=%v %v", d, e)
		}
	}
	if _, e := waitTimeout(intPtr(-1)); e == nil {
		t.Fatal("negative timeout")
	}
	e := setup(t)
	f := startMCP(t, e)
	r := mcpCall(t, f, 1, "runner_status", map[string]any{})
	v, _ := resultText(t, r)
	if v["active"] != false {
		t.Fatal(r)
	}
	runFor(t, e)
	deadline := time.Now().Add(2 * time.Second)
	for !RunnerActive(e.cfg.Dir) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	r = mcpCall(t, f, 2, "runner_status", map[string]any{})
	v, _ = resultText(t, r)
	if v["active"] != true {
		t.Fatal(r)
	}
}
func intPtr(i int) *int { return &i }

func TestMCPWaitDoesNotBlockOtherCallsAndCanBeCancelled(t *testing.T) {
	e := setup(t)
	f := startMCP(t, e)
	if _, err := e.store.Dispatch(e.cfg, "claude", "codex", e.folder, "W1", "TASK W1"); err != nil {
		t.Fatal(err)
	}
	// No runner: W1 stays queued, so this wait would last 540s.
	f.send(t, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"wait_task","arguments":{"id":"W1","timeout_seconds":540}}}`)
	f.send(t, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"get_task","arguments":{"id":"W1"}}}`)
	got := make(chan map[string]any, 1)
	go func() { got <- f.receive(t) }()
	select {
	case m := <-got:
		if m["id"] != float64(11) {
			t.Fatalf("first reply should be get_task (11), got %v", m["id"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("get_task was blocked behind wait_task")
	}
	f.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10}}`)
	f.send(t, `{"jsonrpc":"2.0","id":12,"method":"ping"}`)
	if m := f.receive(t); m["id"] != float64(12) {
		t.Fatalf("cancelled wait_task must not answer; got reply for %v", m["id"])
	}
}

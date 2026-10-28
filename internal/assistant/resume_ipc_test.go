package assistant

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func fakeIPC(t *testing.T, handler func(map[string]any) map[string]any) (*desktopIPC, func()) {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		for {
			var head [4]byte
			if _, e := io.ReadFull(b, head[:]); e != nil {
				return
			}
			n := binary.LittleEndian.Uint32(head[:])
			raw := make([]byte, n)
			if _, e := io.ReadFull(b, raw); e != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			reply := handler(req)
			if reply == nil {
				continue
			}
			data, _ := json.Marshal(reply)
			frame := make([]byte, 4+len(data))
			binary.LittleEndian.PutUint32(frame, uint32(len(data)))
			copy(frame[4:], data)
			if _, e := b.Write(frame); e != nil {
				return
			}
		}
	}()
	return &desktopIPC{conn: a, client: "helper"}, func() { a.Close() }
}
func TestDesktopOwnerContinuationPreservesSettingsAndResponse(t *testing.T) {
	var sent map[string]any
	ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
		sent = m
		return map[string]any{"type": "response", "requestId": m["requestId"], "resultType": "success", "result": map[string]any{"result": map[string]any{"turn": map[string]any{"id": "new-turn", "status": "inProgress"}}}}
	})
	defer close()
	turn := resumeTurn{ID: "failed", Params: map[string]json.RawMessage{"model": json.RawMessage(`"original-model"`), "effort": json.RawMessage(`"high"`), "sandboxPolicy": json.RawMessage(`{"type":"readOnly"}`), "input": json.RawMessage(`["OLD SECRET PROMPT"]`), "additionalContext": json.RawMessage(`{"old":"context"}`)}}
	if e := ipc.continueTurn("exact-thread", "exact-owner", turn); e != nil {
		t.Fatal(e)
	}
	if sent["targetClientId"] != "exact-owner" || sent["version"] != float64(2) {
		t.Fatal("wrong owner/protocol version")
	}
	p := sent["params"].(map[string]any)
	if p["conversationId"] != "exact-thread" {
		t.Fatal("wrong thread")
	}
	req := p["turnStart"].(map[string]any)["request"].(map[string]any)
	if req["model"] != "original-model" || req["effort"] != "high" || req["sandboxPolicy"].(map[string]any)["type"] != "readOnly" {
		t.Fatal("execution settings changed")
	}
	if _, ok := req["additionalContext"]; ok {
		t.Fatal("stale context copied")
	}
	input := req["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["text"] == "OLD SECRET PROMPT" {
		t.Fatal("old prompt resubmitted")
	}
}
func TestDesktopSnapshotFiltersOtherThreadAndHost(t *testing.T) {
	ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
		return map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": "original-owner", "params": map[string]any{"hostId": "local", "conversationId": "exact-thread", "change": map[string]any{"type": "snapshot", "conversationState": map[string]any{"id": "exact-thread", "resumeState": "resumed", "modelProvider": "openai", "threadRuntimeStatus": map[string]any{"type": "idle"}}}}}
	})
	defer close()
	v, owner, e := ipc.snapshot("exact-thread")
	if e != nil || owner != "original-owner" || v.ID != "exact-thread" {
		t.Fatalf("bad snapshot: %v", e)
	}
}
func TestDesktopConnectionRejectsArbitraryFiles(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex/ipc/ipc.sock")
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	_ = os.WriteFile(path, []byte("not a socket"), 0600)
	if c, e := connectDesktop(context.Background(), home); e == nil {
		c.conn.Close()
		t.Fatal("connected to non-socket")
	}
}

package assistant

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopStopRequiresFinishedExactTurnAndRollout(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "sessions", "fixture.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"thread"}}` + "\n"
	start := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}` + "\n"
	finish := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}` + "\n"
	writeFixture(t, path, meta+start+finish)
	v := desktopConversation{ID: "thread", ResumeState: "resumed", ModelProvider: "openai", RolloutPath: path, Turns: []resumeTurn{{ID: "turn", Status: "completed"}}}
	v.Runtime.Type = "idle"
	check := func(view desktopConversation, owner string) bool {
		return desktopHandoffStopObserved(home, "thread", "turn", "owner", "openai", view, owner)
	}
	if !check(v, "owner") {
		t.Fatal("completed target turn rejected")
	}
	for _, mutation := range []func(*desktopConversation){
		func(v *desktopConversation) { v.Turns[0].ID = "new-turn" },
		func(v *desktopConversation) { v.Turns[0].Status = "inProgress" },
		func(v *desktopConversation) { v.Runtime.Type = "active" },
		func(v *desktopConversation) { v.Requests = []json.RawMessage{json.RawMessage(`{}`)} },
		func(v *desktopConversation) {
			v.Goal = &struct {
				Status string `json:"status"`
			}{Status: "active"}
		},
	} {
		copyView := v
		copyView.Turns = append([]resumeTurn(nil), v.Turns...)
		mutation(&copyView)
		if check(copyView, "owner") {
			t.Fatal("changed or active conversation was accepted")
		}
	}
	if check(v, "different-owner") {
		t.Fatal("different owner accepted")
	}
	writeFixture(t, path, meta+start+finish+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"new"}}`+"\n")
	if check(v, "owner") {
		t.Fatal("stale snapshot accepted after a newer turn started")
	}
}

func TestDesktopContinueRequiresExactStoppedSession(t *testing.T) {
	for _, tc := range []struct {
		name, owner, turn, returned string
		wantSent, wantOK            bool
	}{
		{"matching", "owner", "turn", "new-turn", true, true},
		{"wrong owner", "other", "turn", "new-turn", false, false},
		{"newer turn", "owner", "different-turn", "new-turn", false, false},
		{"response repeats previous turn", "owner", "turn", "turn", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".codex/sessions/fixture.jsonl")
			writeFixture(t, path, desktopSettingsFixture("thread", "turn")+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`+"\n")
			var sent map[string]any
			ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
				if m["type"] == "broadcast" {
					return map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": tc.owner, "params": map[string]any{"hostId": "local", "conversationId": "thread", "change": map[string]any{"type": "snapshot", "conversationState": map[string]any{"id": "thread", "resumeState": "resumed", "modelProvider": "openai", "rolloutPath": path, "threadRuntimeStatus": map[string]any{"type": "idle"}, "turns": []any{map[string]any{"turnId": tc.turn, "status": "completed"}}}}}}
				}
				sent = m
				return map[string]any{"type": "response", "requestId": m["requestId"], "resultType": "success", "result": map[string]any{"result": map[string]any{"turn": map[string]any{"id": tc.returned, "status": "inProgress"}}}}
			})
			defer close()
			_, err := ipc.continueDesktopHandoff(home, "thread", "turn", "owner", "openai")
			if (sent != nil) != tc.wantSent || (err == nil) != tc.wantOK {
				t.Fatalf("sent=%t success=%t", sent != nil, err == nil)
			}
			if sent != nil {
				params := sent["params"].(map[string]any)
				req := params["turnStart"].(map[string]any)["request"].(map[string]any)
				input := req["input"].([]any)[0].(map[string]any)
				if sent["targetClientId"] != "owner" || req["threadId"] != "thread" || !strings.Contains(input["text"].(string), "切换账号") {
					t.Fatal("incorrect continuation target or message")
				}
				if req["effort"] != "high" || req["sandboxPolicy"].(map[string]any)["type"] != "readOnly" {
					t.Fatal("saved execution settings not preserved")
				}
			}
		})
	}
}

func TestDesktopStopRequestsOnlySelectedActiveTurn(t *testing.T) {
	for _, tc := range []struct {
		name, observedTurn, replyTurn string
		wantSent, wantOK              bool
	}{
		{"same turn", "turn", "turn", true, true},
		{"turn changed before sending", "new", "turn", false, false},
		{"owner selected a different turn", "turn", "new", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent map[string]any
			ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
				if m["type"] == "broadcast" {
					return map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": "owner", "params": map[string]any{"hostId": "local", "conversationId": "thread", "change": map[string]any{"type": "snapshot", "conversationState": map[string]any{"id": "thread", "resumeState": "resumed", "threadRuntimeStatus": map[string]any{"type": "active"}, "turns": []any{map[string]any{"turnId": tc.observedTurn, "status": "inProgress"}}}}}}
				}
				sent = m
				return map[string]any{"type": "response", "requestId": m["requestId"], "resultType": "success", "result": map[string]any{"result": map[string]any{"turnId": tc.replyTurn}}}
			})
			defer close()
			err := ipc.requestHandoffStop("thread", "owner", "turn")
			if (sent != nil) != tc.wantSent || (err == nil) != tc.wantOK {
				t.Fatalf("sent=%t success=%t", sent != nil, err == nil)
			}
			if sent != nil {
				params := sent["params"].(map[string]any)
				if sent["targetClientId"] != "owner" || params["conversationId"] != "thread" || sent["method"] != "thread-follower-steer-turn" {
					t.Fatal("stop request was not addressed to the selected owner and thread")
				}
			}
		})
	}
}

func TestDesktopContinueQuotaFailedTurnAfterRestart(t *testing.T) {
	for _, kind := range []string{"quota", "ordinary-error", "running-tool"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".codex/sessions/fixture.jsonl")
			writeFixture(t, path, desktopSettingsFixture("thread", "turn")+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`+"\n")
			sent := 0
			ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
				if m["type"] == "broadcast" {
					info := "usageLimitExceeded"
					if kind == "ordinary-error" {
						info = "internalServerError"
					}
					items := []any{}
					if kind == "running-tool" {
						items = append(items, map[string]any{"type": "commandExecution", "status": "inProgress"})
					}
					return map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": "new-owner", "params": map[string]any{"hostId": "local", "conversationId": "thread", "change": map[string]any{"type": "snapshot", "conversationState": map[string]any{"id": "thread", "resumeState": "resumed", "modelProvider": "openai", "rolloutPath": path, "threadRuntimeStatus": map[string]any{"type": "systemError"}, "turns": []any{map[string]any{"turnId": "turn", "status": "failed", "error": map[string]any{"codexErrorInfo": info}, "items": items}}}}}}
				}
				sent++
				return map[string]any{"type": "response", "requestId": m["requestId"], "resultType": "success", "result": map[string]any{"result": map[string]any{"turn": map[string]any{"id": "new-turn", "status": "inProgress"}}}}
			})
			defer close()
			_, err := ipc.continueDesktopHandoff(home, "thread", "turn", "new-owner", "openai")
			if kind == "quota" {
				if err != nil || sent != 1 {
					t.Fatal("verified quota-failed turn was not continued once")
				}
			} else if err == nil || sent != 0 {
				t.Fatal("unproven failed turn was continued")
			}
		})
	}
}

func TestDesktopInterruptUsesExactOwnerAndExpectedTurn(t *testing.T) {
	var sent map[string]any
	ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
		sent = m
		return map[string]any{"type": "response", "requestId": m["requestId"], "resultType": "success", "result": map[string]any{"method": "thread-follower-interrupt-turn", "result": map[string]any{"ok": true, "interruptedTurnId": "turn"}}}
	})
	defer close()
	if err := ipc.interruptHandoffTurn("thread", "owner", "turn"); err != nil {
		t.Fatal(err)
	}
	params, ok := sent["params"].(map[string]any)
	if !ok || sent["targetClientId"] != "owner" || sent["version"] != float64(4) || params["conversationId"] != "thread" || params["expectedTurnId"] != "turn" {
		t.Fatal("interrupt was not bound to the exact owner and turn")
	}
	if err := ipc.interruptHandoffTurn("thread", "owner", ""); err == nil {
		t.Fatal("missing expected turn accepted")
	}
}

func TestDesktopInterruptRejectsDifferentConfirmedTurn(t *testing.T) {
	ipc, close := fakeIPC(t, func(m map[string]any) map[string]any {
		return map[string]any{"type": "response", "requestId": m["requestId"], "resultType": "success", "result": map[string]any{"method": "thread-follower-interrupt-turn", "result": map[string]any{"ok": true, "interruptedTurnId": "other"}}}
	})
	defer close()
	if err := ipc.interruptHandoffTurn("thread", "owner", "turn"); err == nil {
		t.Fatal("wrong interrupted turn accepted")
	}
}

type handoffSnapshotFixture struct {
	conversation desktopConversation
	owner        string
	err          error
	requested    string
}

func (f *handoffSnapshotFixture) snapshot(id string) (desktopConversation, string, error) {
	f.requested = id
	return f.conversation, f.owner, f.err
}

func TestDesktopHandoffPreflightFailsClosedEvenForRunningThread(t *testing.T) {
	f := &handoffSnapshotFixture{owner: "owner", conversation: desktopConversation{ID: "thread", ResumeState: "resumed", ModelProvider: "openai", Turns: []resumeTurn{{ID: "turn", Status: "inProgress"}}}}
	f.conversation.Runtime.Type = "running"
	p, err := inspectDesktopHandoff(f, "thread", "openai")
	if err != nil || p.Ready || f.requested != "thread" || p.TurnID != "turn" || len(p.Blockers) < 3 {
		t.Fatalf("unexpected preflight: %+v %v", p, err)
	}
	if !strings.Contains(strings.Join(p.Blockers, " "), "子代理") {
		t.Fatal("interrupt effects must remain visible")
	}
}

func TestDesktopHandoffPreflightRejectsWrongOwnerThreadAndErrors(t *testing.T) {
	for _, f := range []*handoffSnapshotFixture{
		{owner: "owner", conversation: desktopConversation{ID: "different"}},
		{conversation: desktopConversation{ID: "thread"}},
		{err: errors.New("unavailable")},
	} {
		if p, err := inspectDesktopHandoff(f, "thread", "openai"); err == nil || p.Ready {
			t.Fatalf("unverified identity accepted: %+v", p)
		}
	}
}

func TestDesktopHandoffPreflightRejectsUnsupportedSettingsAndMissingTurn(t *testing.T) {
	f := &handoffSnapshotFixture{owner: "owner", conversation: desktopConversation{ID: "thread", ResumeState: "resumed", ModelProvider: "other"}}
	p, err := inspectDesktopHandoff(f, "thread", "openai")
	if err != nil || p.Ready || len(p.Blockers) < 5 {
		t.Fatalf("unsupported settings accepted: %+v %v", p, err)
	}
}

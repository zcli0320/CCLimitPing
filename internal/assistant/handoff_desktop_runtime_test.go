package assistant

import (
	"encoding/json"
	"testing"
)

func TestDesktopSettingsFreezeAndRestore(t *testing.T) {
	v := desktopConversation{Cwd: "/project", LatestModel: "model", LatestEffort: "high"}
	turn := resumeTurn{Params: map[string]json.RawMessage{
		"cwd": json.RawMessage(`"/project"`), "model": json.RawMessage(`"model"`), "effort": json.RawMessage(`"high"`),
		"approvalPolicy": json.RawMessage(`"never"`), "sandboxPolicy": json.RawMessage(`{"type":"readOnly"}`),
	}}
	dir, model, effort, proof, ok := desktopSettings(v, turn)
	if !ok || dir != "/project" || model != "model" || effort != "high" || len(proof) != 64 {
		t.Fatal("settings not frozen")
	}
	turn.Params["sandboxPolicy"] = json.RawMessage(`{"type":"workspaceWrite"}`)
	_, _, _, changed, ok := desktopSettings(v, turn)
	if !ok || changed == proof {
		t.Fatal("permission change not detected")
	}
	turn.Params = nil
	if _, _, _, _, ok := desktopSettings(v, turn); ok {
		t.Fatal("missing settings accepted")
	}
}

func TestDesktopManualMessageEvidenceExcludesOurPause(t *testing.T) {
	id := "thread"
	turn := resumeTurn{ID: "turn", Items: []json.RawMessage{
		json.RawMessage(`{"type":"userMessage","id":"existing"}`),
		json.RawMessage(`{"type":"agentMessage","id":"agent"}`),
	}}
	baseline := desktopUserItems(id, turn)
	own, _ := json.Marshal(map[string]any{"type": "steeringUserMessage", "id": "own", "clientUserMessageId": handoffSteerClientID(id, turn.ID)})
	turn.Items = append(turn.Items, own)
	for item := range desktopUserItems(id, turn) {
		if !baseline[item] {
			t.Fatal("our own pause counted as manual input")
		}
	}
	turn.Items = append(turn.Items, json.RawMessage(`{"type":"userMessage","id":"new-user"}`))
	if !desktopUserItems(id, turn)["new-user"] {
		t.Fatal("manual message not detected")
	}
}

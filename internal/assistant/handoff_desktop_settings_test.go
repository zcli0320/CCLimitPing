package assistant

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func desktopSettingsFixture(session, turn string) string {
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": session}})
	context, _ := json.Marshal(map[string]any{"type": "turn_context", "payload": map[string]any{"turn_id": turn, "cwd": "/fixture", "model": "fixture-model", "effort": "high", "approval_policy": "never", "approvals_reviewer": "user", "sandbox_policy": map[string]any{"type": "read-only"}, "collaboration_mode": nil, "active_permission_profile": nil}})
	return string(meta) + "\n" + string(context) + "\n"
}

func TestDesktopPersistedSettingsSurviveSnapshotReload(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex/sessions/test.jsonl")
	writeFixture(t, path, desktopSettingsFixture("thread", "turn"))
	v := desktopConversation{ID: "thread", RolloutPath: path}
	before := resumeTurn{ID: "turn", Params: map[string]json.RawMessage{"model": json.RawMessage(`"fixture-model"`), "effort": json.RawMessage(`null`), "approvalPolicy": json.RawMessage(`"never"`)}}
	after := resumeTurn{ID: "turn", Params: map[string]json.RawMessage{"effort": json.RawMessage(`"high"`), "sandboxPolicy": json.RawMessage(`{"type":"readOnly"}`)}}
	x, err := desktopPersistedTurn(home, v, before)
	if err != nil {
		t.Fatal(err)
	}
	y, err := desktopPersistedTurn(home, v, after)
	if err != nil {
		t.Fatal(err)
	}
	dir, model, effort, proof, ok := desktopSettings(v, x)
	dir2, model2, effort2, proof2, ok2 := desktopSettings(v, y)
	if !ok || !ok2 || dir != dir2 || model != model2 || effort != "high" || effort2 != "high" || proof != proof2 {
		t.Fatal("reload altered effective settings")
	}
	writeFixture(t, path, strings.ReplaceAll(desktopSettingsFixture("thread", "turn"), "read-only", "danger-full-access"))
	z, err := desktopPersistedTurn(home, v, after)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, changed, _ := desktopSettings(v, z)
	if changed == proof {
		t.Fatal("actual permission change accepted")
	}
}

func TestDesktopPersistedSettingsRejectUnknownOrPartialEvidence(t *testing.T) {
	for _, kind := range []string{"wrong-session", "wrong-turn", "partial", "duplicate", "missing-effort"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".codex/sessions/test.jsonl")
			data := desktopSettingsFixture("thread", "turn")
			switch kind {
			case "wrong-session":
				data = desktopSettingsFixture("other", "turn")
			case "wrong-turn":
				data = desktopSettingsFixture("thread", "other")
			case "partial":
				data += `{"type":`
			case "duplicate":
				data += strings.Split(data, "\n")[1] + "\n"
			case "missing-effort":
				data = strings.ReplaceAll(data, `"effort":"high",`, "")
			}
			writeFixture(t, path, data)
			if _, err := desktopPersistedTurn(home, desktopConversation{ID: "thread", RolloutPath: path}, resumeTurn{ID: "turn"}); err == nil {
				t.Fatal("unproven context accepted")
			}
		})
	}
}

func TestDesktopPersistedExplicitDefaultEffort(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex/sessions/test.jsonl")
	data := strings.ReplaceAll(desktopSettingsFixture("thread", "turn"), `"effort":"high",`, "")
	data = strings.ReplaceAll(data, `"collaboration_mode":null`, `"collaboration_mode":{"mode":"default","settings":{"model":"fixture-model","reasoning_effort":null,"developer_instructions":null}}`)
	writeFixture(t, path, data)
	view := desktopConversation{ID: "thread", RolloutPath: path}
	x, err := desktopPersistedTurn(home, view, resumeTurn{ID: "turn", Params: map[string]json.RawMessage{"effort": json.RawMessage(`null`)}})
	if err != nil || string(x.Params["effort"]) != `"default"` || string(x.Params["saved_default_effort"]) != "true" {
		t.Fatalf("explicit default not preserved: %v", err)
	}
	if _, err = desktopPersistedTurn(home, view, resumeTurn{ID: "turn", Params: map[string]json.RawMessage{"effort": json.RawMessage(`"high"`)}}); err == nil {
		t.Fatal("known high effort downgraded")
	}
}

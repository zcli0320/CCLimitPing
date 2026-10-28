package assistant

import (
	"context"
	"encoding/json"
	"testing"
	"time"
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

func TestDesktopExitObservationDoesNotRequireLiveIPC(t *testing.T) {
	r, d, _ := desktopCoordinatorFixture(t)
	const app = "/Applications/ChatGPT.app"
	pair := desktopProcessPair{App: desktopProcessIdentity{PID: 100, UID: 501, Start: "app", Command: app + "/Contents/MacOS/ChatGPT"}, Server: desktopProcessIdentity{PID: 101, PPID: 100, UID: 501, Start: "server", Command: app + "/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex app-server"}}
	procs := []desktopProcessIdentity{pair.App, pair.Server}
	life := &desktopHandoffLifecycle{home: r.Home, appPath: app, uid: 501, old: pair, oldTree: procs, read: func(context.Context) ([]desktopProcessIdentity, error) { return procs, nil }}
	runtime := &desktopSingleSessionRuntime{r: r, id: "session", lifecycle: life, exitRequested: true, wholeAppExitAccepted: true}
	e, err := runtime.observe(context.Background(), "session")
	if err != nil || !e.InterventionKnown || e.OldExited {
		t.Fatal("shutdown read tried unavailable IPC")
	}
	d.runtime = runtime
	s := r.HandoffStatus()
	s.Stage = "waiting_exit"
	s.SourceAccount = "managed-a"
	s.TargetAccount = "managed-b"
	s.ActionAt = time.Now().Add(-46 * time.Second).Unix()
	if err = writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	s, err = r.AdvanceHandoff(context.Background(), d)
	if err != nil || s.Enabled || s.Stage != "uncertain" {
		t.Fatalf("exit timeout suppressed: %+v %v", s, err)
	}
	procs = nil
	e, err = runtime.observe(context.Background(), "session")
	if err != nil || !e.OldExited {
		t.Fatal("captured process exit was not detected")
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

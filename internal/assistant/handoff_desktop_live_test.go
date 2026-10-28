package assistant

// This opt-in probe sends a pause request to the exact test conversation
// explicitly supplied by a human. It does not exit the app or change auth.
import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestLiveDesktopStopProbe(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_DESKTOP_TEST_THREAD")
	if id == "" {
		t.Skip("explicit temporary Desktop test conversation required")
	}
	if len(id) != 36 || !validHandoffText(id) {
		t.Fatal("invalid temporary test conversation")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("local home unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := connectDesktop(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	v, owner, err := c.snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	turn, ok := v.latest()
	if !ok || turn.ID == "" || turn.Status != "inProgress" || v.Runtime.Type != "active" {
		t.Fatal("temporary test conversation has no observable active turn")
	}
	if err := c.requestHandoffStop(id, owner, turn.ID); err != nil {
		t.Fatal(err)
	}
	t.Log("pause request acknowledged for the exact temporary test turn")
}

func TestLiveDesktopStopObserved(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_DESKTOP_TEST_THREAD")
	turnID := os.Getenv("CODEX_HELPER_LIVE_DESKTOP_TEST_TURN")
	if id == "" || turnID == "" {
		t.Skip("explicit completed test turn required")
	}
	if len(id) != 36 || len(turnID) != 36 {
		t.Fatal("invalid temporary test identifiers")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("local home unavailable")
	}
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		t.Fatal("Desktop provider unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := connectDesktop(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	v, owner, err := c.snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if !desktopHandoffStopObserved(r.Home, id, turnID, owner, provider, v, owner) {
		t.Fatalf("completed temporary turn not confirmed (state=%s runtime=%s provider-match=%t path-present=%t)", v.ResumeState, v.Runtime.Type, v.ModelProvider == provider, v.RolloutPath != "")
	}
	t.Log("completed test turn and local rollout agree")
}

func TestLiveDesktopSnapshotMetadata(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_DESKTOP_TEST_THREAD")
	if id == "" {
		t.Skip("explicit test conversation required")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("home unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := connectDesktop(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	v, owner, err := c.snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	turn, ok := v.latest()
	keys := []string{"cwd", "model", "effort", "approvalPolicy", "sandboxPolicy", "permissions", "collaborationMode", "serviceTier"}
	for _, key := range keys {
		_, found := turn.Params[key]
		t.Logf("setting %s present=%t", key, found)
	}
	counts := map[string]int{}
	for _, raw := range turn.Items {
		var item struct {
			Type                string `json:"type"`
			ClientID            string `json:"clientId"`
			ClientUserMessageID string `json:"clientUserMessageId"`
		}
		if json.Unmarshal(raw, &item) == nil {
			counts[item.Type]++
			if item.Type == "steeringUserMessage" {
				t.Logf("steer client-id-match=%t client-user-id-match=%t", item.ClientID == handoffSteerClientID(id, turn.ID), item.ClientUserMessageID == handoffSteerClientID(id, turn.ID))
			}
		}
	}
	t.Logf("item types=%v visible user-items=%d", counts, len(desktopUserItems(id, turn)))
	t.Logf("owner=%t latest=%t runtime=%s resume=%s goal=%t", owner != "", ok, v.Runtime.Type, v.ResumeState, v.Goal != nil)
}

func TestLiveDesktopRuntimeReadOnly(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_DESKTOP_TEST_THREAD")
	if id == "" {
		t.Skip("explicit test conversation required")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("home unavailable")
	}
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		t.Fatal("provider unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lifecycle, err := newDesktopHandoffLifecycle(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &desktopSingleSessionRuntime{r: r, id: id, provider: provider, lifecycle: lifecycle, wholeAppExitAccepted: true}
	e, err := runtime.observe(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.View.ID != id || e.Owner == "" || !e.InterventionKnown || e.Directory == "" || e.Model == "" || e.Permissions == "" {
		t.Fatal("live runtime observation incomplete")
	}
	t.Log("selected session, running app, config root and execution settings were observed")
}

func TestLiveDesktopRecoveryDiagnostics(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_RECOVERY_DIAGNOSTICS") != "1" {
		t.Skip("read-only diagnostics opt-in")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}
	s := r.HandoffStatus()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := connectDesktop(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	v, owner, err := c.snapshot(s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var old resumeTurn
	for _, turn := range v.Turns {
		if turn.ID == s.TurnID {
			old = turn
		}
	}
	if v.History != nil {
		for _, turn := range v.History.History.Entities {
			if turn.ID == s.TurnID {
				old = turn
			}
		}
	}
	dir, model, effort, perm, ok := desktopSettings(v, old)
	t.Logf("old-turn-present=%t status=%s settings-readable=%t dir-match=%t model-match=%t effort-match=%t permissions-match=%t", old.ID != "", old.Status, ok, dir == s.Directory, model == s.Model, effort == s.Effort, perm == s.Permissions)
	resolved, resolveErr := desktopPersistedTurn(r.Home, v, old)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	_, _, effectiveEffort, _, settingsOK := desktopSettings(v, resolved)
	t.Logf("saved-context-readable=%t saved-effort=%s", settingsOK, effectiveEffort)
	for _, key := range []string{"effort", "approvalPolicy", "approvalsReviewer", "sandboxPolicy", "permissions", "collaborationMode", "serviceTier", "multiAgentMode"} {
		raw := old.Params[key]
		if key == "collaborationMode" {
			var mode struct {
				Mode     string `json:"mode"`
				Settings struct {
					Model        string  `json:"model"`
					Effort       string  `json:"reasoning_effort"`
					Instructions *string `json:"developer_instructions"`
				} `json:"settings"`
			}
			_ = json.Unmarshal(raw, &mode)
			t.Logf("collaboration mode=%s effort=%s instructions-present=%t", mode.Mode, mode.Settings.Effort, mode.Settings.Instructions != nil)
		} else {
			t.Logf("%s=%s", key, raw)
		}
	}
	b := &desktopCCSwitch{r: r}
	t.Logf("target-installed=%t owner-present=%t current-resume=%s rollout=%t", b.installed(s), owner != "", v.ResumeState, v.RolloutPath != "")
	var txn ccSwitchTransaction
	if readJSON(r.ccSwitchTransactionPath(), &txn) == nil {
		t.Logf("transaction-phase=%s", txn.Phase)
	}
}

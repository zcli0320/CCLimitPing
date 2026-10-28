package assistant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDesktopNaturalLimitContinuesOnlyObservedTurn(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	if _, err := d.Observe(context.Background(), r.HandoffStatus()); err != nil {
		t.Fatal(err)
	}
	base := d.quota
	d.quota = func(ctx context.Context, c quotaCredential) AccountQuota {
		q := base(ctx, c)
		claims, _ := decodeTokenClaims(c.access)
		if claims.Auth.User == "user-a" {
			q.Windows[0].UsedPercent = 100
		}
		return q
	}
	f.e.View.Turns[0].Status = "failed"
	f.e.View.Turns[0].Error = json.RawMessage(`{"codexErrorInfo":"usageLimitExceeded"}`)
	f.e.View.Turns[0].Items = []json.RawMessage{}
	f.e.View.Runtime.Type = "systemError"
	writeFixture(t, f.e.View.RolloutPath, `{"type":"session_meta","payload":{"id":"session"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`+"\n")
	observed, err := d.Observe(context.Background(), r.HandoffStatus())
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.AdvanceHandoff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if !observed.AlreadyLimited || !observed.StopConfirmed || s.Stage != "ready_exit" || len(f.actions) != 0 {
		t.Fatalf("exact limited turn did not reach exit: %+v stage=%s", observed, s.Stage)
	}
	for i := 0; i < 9; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if r.HandoffStatus().Stage != "completed" || len(f.actions) != 3 || f.actions[0] != "exit" || f.actions[2] != "continue" {
		t.Fatalf("limited handoff did not continue once: stage=%s actions=%v", r.HandoffStatus().Stage, f.actions)
	}
}
func TestDesktopRefreshWhileWatchingReprepares(t *testing.T) {
	r, _ := ccSwitchFixture(t)
	_, d, f := desktopCoordinatorFixture(t)
	d.r = r
	d.ccswitch = &desktopCCSwitch{r: r}
	f.home = r.Home
	f.e.View.RolloutPath = filepath.Join(r.Home, ".codex/sessions/probe.jsonl")
	if _, err := r.ConfigureHandoff("desktop", "session", 10, true); err != nil {
		t.Fatal(err)
	}
	first, err := d.Observe(context.Background(), r.HandoffStatus())
	if err != nil || !first.TargetCredentialReady {
		t.Fatal("fixture not ready")
	}
	p := filepath.Join(r.Home, ".codex/auth.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]any
	_ = json.Unmarshal(raw, &native)
	native["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ = json.Marshal(native)
	writeFixture(t, p, string(raw))
	next, err := d.Observe(context.Background(), r.HandoffStatus())
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.Observe(context.Background(), r.HandoffStatus())
	if err != nil {
		t.Fatal(err)
	}
	if !next.TargetCredentialReady || !again.TargetCredentialReady || next.UserIntervened || !d.preparedStillValid() {
		t.Fatal("same-account refresh permanently invalidated readiness")
	}
}
func TestDesktopMissingRunnerIsReportedPaused(t *testing.T) {
	r, _, _ := desktopCoordinatorFixture(t)
	snapshot := r.Snapshot(context.Background())
	if snapshot.Handoff.Enabled || snapshot.Handoff.RunnerReady || snapshot.Handoff.Stage != "error" || !snapshot.Handoff.AuthorizationPending {
		t.Fatalf("missing runner reported as active: %+v", snapshot.Handoff)
	}
	if !r.HandoffStatus().Enabled {
		t.Fatal("read-only snapshot changed authorization")
	}
	s := r.HandoffStatus()
	s.Stage = "waiting_switch"
	s.PendingAction = "switch"
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	snapshot = r.Snapshot(context.Background())
	if snapshot.Handoff.Enabled || snapshot.Handoff.Stage != "uncertain" || r.HandoffStatus().PendingAction != "switch" {
		t.Fatal("missing in-flight runner lost review requirement or journal")
	}
}

func TestDesktopNaturalLimitRefusesUnprovenStops(t *testing.T) {
	for _, kind := range []string{"not-observed-active", "different-turn", "different-owner", "manual-message", "changed-settings", "ordinary-error", "tool-running", "tool-background", "missing-items", "partial-items", "approval", "active-goal", "missing-completion", "newer-rollout-turn", "other-work"} {
		t.Run(kind, func(t *testing.T) {
			r, d, f := desktopCoordinatorFixture(t)
			if kind != "not-observed-active" {
				if _, err := d.Observe(context.Background(), r.HandoffStatus()); err != nil {
					t.Fatal(err)
				}
			}
			f.e.View.Turns[0].Status = "failed"
			f.e.View.Turns[0].Error = json.RawMessage(`{"codexErrorInfo":"usageLimitExceeded"}`)
			f.e.View.Turns[0].Items = []json.RawMessage{}
			f.e.View.Runtime.Type = "systemError"
			log := `{"type":"session_meta","payload":{"id":"session"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}` + "\n"
			if kind != "missing-completion" {
				log += `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}` + "\n"
			}
			if kind == "newer-rollout-turn" {
				log += `{"type":"event_msg","payload":{"type":"task_started","turn_id":"new"}}` + "\n"
			}
			writeFixture(t, f.e.View.RolloutPath, log)
			switch kind {
			case "different-turn":
				f.e.View.Turns[0].ID = "new"
			case "different-owner":
				f.e.Owner = "other"
			case "manual-message":
				f.e.View.Turns[0].Items = []json.RawMessage{json.RawMessage(`{"type":"userMessage","id":"manual"}`)}
			case "changed-settings":
				f.e.Permissions = "changed"
			case "ordinary-error":
				f.e.View.Turns[0].Error = json.RawMessage(`{"codexErrorInfo":"internalServerError"}`)
			case "tool-running":
				f.e.View.Turns[0].Items = []json.RawMessage{json.RawMessage(`{"type":"mcpToolCall","status":"inProgress"}`)}
			case "tool-background":
				f.e.View.Turns[0].Items = []json.RawMessage{json.RawMessage(`{"type":"commandExecution","status":"completed","processId":"42"}`)}
			case "missing-items":
				f.e.View.Turns[0].Items = nil
			case "partial-items":
				f.e.View.Turns[0].ItemsView = "partial"
			case "approval":
				f.e.View.Requests = []json.RawMessage{json.RawMessage(`{}`)}
			case "active-goal":
				f.e.View.Goal = &struct {
					Status string `json:"status"`
				}{Status: "active"}
			case "other-work":
				f.e.OtherWorkIdle = false
			}
			for i := 0; i < 4; i++ {
				if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
					t.Fatal(err)
				}
			}
			if r.HandoffStatus().Stage != "watching" || len(f.actions) != 0 {
				t.Fatalf("unproven stop triggered %s: %v", r.HandoffStatus().Stage, f.actions)
			}
		})
	}
}

func TestDesktopLimitAfterTriggerOrAcceptedPause(t *testing.T) {
	for _, stage := range []string{"ready_stop", "waiting_stop"} {
		t.Run(stage, func(t *testing.T) {
			r, d, f := desktopCoordinatorFixture(t)
			for r.HandoffStatus().Stage != stage {
				if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
					t.Fatal(err)
				}
			}
			f.e.View.Turns[0].Status = "failed"
			f.e.View.Turns[0].Error = json.RawMessage(`{"codexErrorInfo":{"type":"UsageLimitExceeded"}}`)
			f.e.View.Turns[0].Items = []json.RawMessage{}
			f.e.View.Runtime.Type = "systemError"
			writeFixture(t, f.e.View.RolloutPath, `{"type":"session_meta","payload":{"id":"session"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`+"\n")
			for i := 0; i < 12; i++ {
				if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
					t.Fatal(err)
				}
			}
			if r.HandoffStatus().Stage != "completed" {
				t.Fatalf("limited handoff stalled at %s", r.HandoffStatus().Stage)
			}
			continues := 0
			for _, action := range f.actions {
				if action == "continue" {
					continues++
				}
			}
			if continues != 1 {
				t.Fatalf("continued %d times", continues)
			}
		})
	}
}

func TestDesktopCredentialGenerationFrozenAfterTrigger(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if r.HandoffStatus().Stage != "ready_stop" {
		t.Fatal("fixture did not trigger")
	}
	p := filepath.Join(r.Home, ".codex/auth.json")
	raw, _ := os.ReadFile(p)
	var auth map[string]any
	_ = json.Unmarshal(raw, &auth)
	auth["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ = json.Marshal(auth)
	writeFixture(t, p, string(raw))
	if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if r.HandoffStatus().Stage != "watching" || len(f.actions) != 0 {
		t.Fatal("changed armed credential generation was accepted")
	}
	if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if r.HandoffStatus().Stage != "ready_stop" {
		t.Fatal("same account did not reprepare after returning to monitoring")
	}
}

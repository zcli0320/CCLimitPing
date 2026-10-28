package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type fakeDesktopRuntime struct {
	t       *testing.T
	home    string
	e       desktopRuntimeEvidence
	actions []string
	fail    string
}

func (f *fakeDesktopRuntime) observe(context.Context, string) (desktopRuntimeEvidence, error) {
	return f.e, nil
}
func (f *fakeDesktopRuntime) requestStop(_ context.Context, id, owner, turn string) error {
	f.actions = append(f.actions, "stop")
	if f.fail == "stop" {
		return errors.New("ambiguous")
	}
	if id != "session" || owner != "old-owner" || turn != "turn" {
		return errors.New("wrong target")
	}
	f.e.View.Runtime.Type = "idle"
	f.e.View.Turns[0].Status = "completed"
	writeFixture(f.t, f.e.View.RolloutPath, `{"type":"session_meta","payload":{"id":"session"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}`+"\n"+`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`+"\n")
	return nil
}
func (f *fakeDesktopRuntime) requestExit(context.Context) error {
	f.actions = append(f.actions, "exit")
	if f.fail == "exit" {
		return errors.New("ambiguous")
	}
	f.e.OldExited = true
	return nil
}
func (f *fakeDesktopRuntime) bindLimitedStop(context.Context, string, string, string) error {
	return nil
}
func (f *fakeDesktopRuntime) requestRestart(_ context.Context, id string) error {
	f.actions = append(f.actions, "restart")
	if f.fail == "restart" || id != "session" {
		return errors.New("failed")
	}
	f.e.ColdStartVerified, f.e.ConfigHomeVerified, f.e.SettingsVerified = true, true, true
	f.e.Owner = "new-owner"
	return nil
}
func (f *fakeDesktopRuntime) requestContinue(_ context.Context, id, owner string, turn resumeTurn) (string, error) {
	f.actions = append(f.actions, "continue")
	if f.fail == "continue" || id != "session" || owner != "new-owner" || turn.ID != "turn" {
		return "", errors.New("failed")
	}
	f.e.View.Turns = []resumeTurn{{ID: "new-turn", Status: "inProgress"}}
	f.e.View.Runtime.Type = "active"
	f.e.ContinueTurnID = "new-turn"
	return "new-turn", nil
}

func desktopCoordinatorFixture(t *testing.T) (Reader, *desktopHandoffCoordinator, *fakeDesktopRuntime) {
	t.Helper()
	r, _ := handoffAuthFixture(t)
	if _, err := r.ConfigureHandoff("desktop", "session", 10, true); err != nil {
		t.Fatal(err)
	}
	view := desktopConversation{ID: "session", ResumeState: "resumed", ModelProvider: "openai", RolloutPath: filepath.Join(r.Home, ".codex/sessions/fixture.jsonl"), Turns: []resumeTurn{{ID: "turn", Status: "inProgress"}}}
	view.Runtime.Type = "active"
	f := &fakeDesktopRuntime{t: t, home: r.Home, e: desktopRuntimeEvidence{View: view, Owner: "old-owner", Directory: "/fixture", Model: "fixture-model", Effort: "low", Permissions: "default", OtherWorkIdle: true, InterventionKnown: true}}
	query := func(_ context.Context, c quotaCredential) AccountQuota {
		claims, ok := decodeTokenClaims(c.access)
		if !ok {
			return AccountQuota{Status: "error"}
		}
		q := testQuota(time.Now(), false)
		limit := false
		q.LimitReached = &limit
		if claims.Auth.User == "user-a" {
			q.Windows[0].UsedPercent = 91
		}
		return q
	}
	d := &desktopHandoffCoordinator{r: r, id: "session", provider: "openai", source: "managed-a", target: "managed-b", runtime: f, quota: query}
	return r, d, f
}

func TestDesktopCoordinatorIsolatedHandoff(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	for i := 0; i < 11; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	state := r.HandoffStatus()
	if state.Stage != "completed" || state.Enabled {
		t.Fatalf("unexpected stage %s", state.Stage)
	}
	if !reflect.DeepEqual(f.actions, []string{"stop", "exit", "restart", "continue"}) {
		t.Fatalf("unexpected actions %v", f.actions)
	}
	path := filepath.Join(r.Home, ".codex/auth.json")
	data, err := os.ReadFile(path)
	if err != nil || !r.handoffCurrentMatchesAccount(data, "managed-b") {
		t.Fatal("target fixture credential not installed")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); err != nil {
		t.Fatal("recovery record must remain until verified cleanup")
	}
	if _, err := r.AdvanceHandoff(context.Background(), d); err != nil || len(f.actions) != 4 {
		t.Fatal("completed handoff repeated")
	}
	next, rearmed, err := d.finishCompleted(context.Background(), state)
	if err != nil || !rearmed || !next.Enabled || next.Stage != "watching" || d.source != "managed-b" || d.target != "managed-a" {
		t.Fatal("verified handoff did not safely rearm")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("verified credential recovery record was not cleaned up")
	}
}

func TestDesktopCoordinatorWaitsForOtherWork(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	f.e.OtherWorkIdle = false
	for i := 0; i < 5; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if r.HandoffStatus().Stage != "waiting_stop" || !reflect.DeepEqual(f.actions, []string{"stop"}) {
		t.Fatal("exit did not wait for other work")
	}
	data, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if !r.handoffCurrentMatchesAccount(data, "managed-a") {
		t.Fatal("source credential changed while waiting")
	}
}

func TestDesktopCoordinatorRefusesUnverifiedRestartAndAmbiguity(t *testing.T) {
	for _, tc := range []string{"unknown-intervention", "stop-error", "wrong-home"} {
		t.Run(tc, func(t *testing.T) {
			r, d, f := desktopCoordinatorFixture(t)
			if tc == "unknown-intervention" {
				f.e.InterventionKnown = false
			}
			if tc == "stop-error" {
				f.fail = "stop"
			}
			for i := 0; i < 10; i++ {
				if tc == "wrong-home" && r.HandoffStatus().Stage == "waiting_restart" {
					f.e.ConfigHomeVerified = false
				}
				if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
					t.Fatal(err)
				}
			}
			for _, action := range f.actions {
				if action == "continue" {
					t.Fatal("continued without required evidence")
				}
			}
			if tc == "stop-error" && (r.HandoffStatus().Stage != "uncertain" || len(f.actions) != 1) {
				t.Fatal("ambiguous stop was repeated")
			}
			if tc == "unknown-intervention" && len(f.actions) != 0 {
				t.Fatal("unknown state triggered an action")
			}
		})
	}
}

func TestDesktopCoordinatorRetainsRecoveryWhenVerificationFails(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	for i := 0; i < 11; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	completed := r.HandoffStatus()
	f.e.ConfigHomeVerified = false
	if _, rearmed, err := d.finishCompleted(context.Background(), completed); err == nil || rearmed {
		t.Fatal("unverified new instance was accepted")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); err != nil {
		t.Fatal("unverified recovery copy was removed")
	}
}

func TestDesktopCoordinatorChecksBackupAndSourceBeforePause(t *testing.T) {
	for _, change := range []string{"backup-unavailable", "source-changed"} {
		t.Run(change, func(t *testing.T) {
			r, d, f := desktopCoordinatorFixture(t)
			if change == "backup-unavailable" {
				d.quota = func(_ context.Context, c quotaCredential) AccountQuota {
					claims, _ := decodeTokenClaims(c.access)
					if claims.Auth.User == "user-b" {
						return AccountQuota{Status: "credential_rejected"}
					}
					q := testQuota(time.Now(), false)
					q.Windows[0].UsedPercent = 91
					return q
				}
			} else {
				writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), `{"unexpected":"change"}`)
			}
			for i := 0; i < 3; i++ {
				if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.actions) != 0 {
				t.Fatal("paused before confirming both accounts")
			}
		})
	}
}

func TestDesktopCoordinatorNeverSwitchesBeforeExitConfirmed(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	for i := 0; i < 4; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if r.HandoffStatus().Stage != "waiting_exit" {
		t.Fatal("fixture did not reach exit")
	}
	f.e.OldExited = false
	for i := 0; i < 3; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if r.HandoffStatus().Stage != "waiting_exit" || !r.handoffCurrentMatchesAccount(raw, "managed-a") {
		t.Fatal("switched while original instance remained")
	}
}

func TestDesktopCoordinatorRejectsChangedTargetBeforeRestart(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	for i := 0; i < 6; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if r.HandoffStatus().Stage != "waiting_switch" {
		t.Fatal("fixture did not switch")
	}
	writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), `{"independent":"change"}`)
	for i := 0; i < 3; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if r.HandoffStatus().Stage != "waiting_switch" || !reflect.DeepEqual(f.actions, []string{"stop", "exit"}) {
		t.Fatal("restarted after credential changed")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); err != nil {
		t.Fatal("recovery record must be retained")
	}
}

func TestDesktopCoordinatorCancellationBeforeSwitchKeepsSource(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	for i := 0; i < 5; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if r.HandoffStatus().Stage != "ready_switch" {
		t.Fatal("fixture did not reach switch")
	}
	if _, err := r.ConfigureHandoff("", "", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if !r.handoffCurrentMatchesAccount(raw, "managed-a") || !reflect.DeepEqual(f.actions, []string{"stop", "exit"}) {
		t.Fatal("cancelled handoff changed credentials")
	}
}

func TestDesktopCoordinatorRechecksCredentialAfterQuotaBeforePause(t *testing.T) {
	r, d, f := desktopCoordinatorFixture(t)
	base := d.quota
	d.quota = func(ctx context.Context, c quotaCredential) AccountQuota {
		q := base(ctx, c)
		claims, _ := decodeTokenClaims(c.access)
		if claims.Auth.User == "user-b" && r.HandoffStatus().PendingAction == string(HandoffRequestStop) {
			writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), `{"independent":"change"}`)
		}
		return q
	}
	for i := 0; i < 3; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.actions) != 0 || r.HandoffStatus().Stage != "uncertain" {
		t.Fatal("pause sent after credential changed during quota check")
	}
}

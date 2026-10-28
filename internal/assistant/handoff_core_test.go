package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeHandoffOps struct {
	observation HandoffObservation
	actions     []HandoffAction
	err         error
	onObserve   func()
	onExecute   func(HandoffAction, HandoffState)
}

func (f *fakeHandoffOps) Observe(_ context.Context, _ HandoffState) (HandoffObservation, error) {
	if f.onObserve != nil {
		f.onObserve()
	}
	return f.observation, nil
}
func (f *fakeHandoffOps) Execute(_ context.Context, a HandoffAction, s HandoffState) error {
	f.actions = append(f.actions, a)
	if f.onExecute != nil {
		f.onExecute(a, s)
	}
	return f.err
}
func handoffEvidence() HandoffObservation {
	return HandoffObservation{TargetMatches: true, Running: true, TurnID: "turn-1", SourceAccount: "source", TargetAccount: "backup", Directory: "/project", Model: "model", Effort: "high", Permissions: "default", CurrentQuotaKnown: true, CurrentRemaining: 9, TargetQuotaKnown: true, TargetRemaining: 80, TargetWeeklyUsable: true, TargetCredentialReady: true, StopConfirmed: true, StoppedForHandoff: true, OtherSessionsIdle: true, OldExited: true, SwitchVerified: true, RestartReady: true, IdentityVerified: true, SettingsVerified: true, ContinueStarted: true}
}
func handoffFixture(t *testing.T) Reader {
	t.Helper()
	r := Reader{Home: t.TempDir()}
	if _, e := r.ConfigureHandoff("desktop", "thread-1", 10, true); e != nil {
		t.Fatal(e)
	}
	return r
}
func stepHandoff(t *testing.T, r Reader, f *fakeHandoffOps) HandoffState {
	t.Helper()
	s, e := r.AdvanceHandoff(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestHandoffThresholdAndBackupCheckedBeforeStopping(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	f.observation.CurrentRemaining = 10
	if s := stepHandoff(t, r, f); s.Stage != "watching" {
		t.Fatal("threshold should be strictly less")
	}
	f.observation.CurrentRemaining = 9
	for _, invalid := range []func(*HandoffObservation){
		func(o *HandoffObservation) { o.TargetCredentialReady = false },
		func(o *HandoffObservation) { o.TargetRemaining = 10 },
		func(o *HandoffObservation) { o.TargetWeeklyUsable = false },
		func(o *HandoffObservation) { o.TargetQuotaKnown = false },
	} {
		f.observation = handoffEvidence()
		invalid(&f.observation)
		if s := stepHandoff(t, r, f); s.Stage != "watching" || len(f.actions) != 0 {
			t.Fatal("must never stop before checking backup")
		}
	}
}

func TestHandoffCanRecoverAlreadyObservedUsageLimit(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	f.observation.Running = false
	f.observation.AlreadyLimited = true
	f.observation.TargetCredentialReady = false
	if s := stepHandoff(t, r, f); s.Stage != "watching" || len(f.actions) != 0 {
		t.Fatal("limited turn must still require an available backup")
	}
	f.observation.TargetCredentialReady = true
	if s := stepHandoff(t, r, f); s.Stage != "ready_exit" || len(f.actions) != 0 {
		t.Fatal("confirmed usage-limited turn should not send a stop request")
	}
	if s := stepHandoff(t, r, f); s.Stage != "waiting_exit" || len(f.actions) != 1 || f.actions[0] != HandoffExit {
		t.Fatal("limited turn did not enter normal exit")
	}
}
func TestHandoffCompleteSequenceAndNoRepeat(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	f.onExecute = func(a HandoffAction, s HandoffState) {
		persisted := r.HandoffStatus()
		if persisted.PendingAction != string(a) || persisted.Stage != "pending" || s.PendingAction != string(a) {
			t.Fatal("must claim action before execution")
		}
	}
	for i := 0; i < 11; i++ {
		stepHandoff(t, r, f)
	}
	s := r.HandoffStatus()
	if s.Stage != "completed" || s.Enabled {
		t.Fatalf("not completed: %+v", s)
	}
	want := []HandoffAction{HandoffRequestStop, HandoffExit, HandoffSwitch, HandoffRestart, HandoffContinue}
	if !reflect.DeepEqual(f.actions, want) {
		t.Fatalf("incorrect actions: %v", f.actions)
	}
	stepHandoff(t, r, f)
	if !reflect.DeepEqual(f.actions, want) {
		t.Fatal("completed handoff repeated")
	}
	data, e := os.ReadFile(r.handoffPath())
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), "token") || strings.Contains(string(data), "chat text") {
		t.Fatal("unexpected secret in state")
	}
	info, e := os.Stat(r.handoffPath())
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state must be private")
	}
}
func TestHandoffWaitsForStopAndOtherChatsAndInterruptsOnce(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	stepHandoff(t, r, f)
	f.observation.StopConfirmed = false
	f.observation.StopTimedOut = true
	stepHandoff(t, r, f)
	stepHandoff(t, r, f)
	if len(f.actions) != 2 || f.actions[1] != HandoffInterrupt {
		t.Fatalf("interrupt repeated: %v", f.actions)
	}
	f.observation.StopConfirmed = true
	f.observation.StoppedForHandoff = false
	if s := stepHandoff(t, r, f); s.Stage != "waiting_stop" {
		t.Fatal("stop without ownership evidence")
	}
	f.observation.StoppedForHandoff = true
	f.observation.OtherSessionsIdle = false
	if s := stepHandoff(t, r, f); s.Stage != "waiting_stop" {
		t.Fatal("other active sessions must delay exit")
	}
	f.observation.OtherSessionsIdle = true
	stepHandoff(t, r, f)
	stepHandoff(t, r, f)
	if f.actions[len(f.actions)-1] != HandoffExit {
		t.Fatal("stopped session did not proceed")
	}
}
func TestHandoffAmbiguousActionAndCrashNeverRepeat(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence(), err: errors.New("secret should never appear")}
	stepHandoff(t, r, f)
	s := stepHandoff(t, r, f)
	if s.Stage != "uncertain" || s.Enabled || s.PendingAction != "request_stop" || strings.Contains(s.Message, "secret") {
		t.Fatalf("wrong uncertain state: %+v", s)
	}
	stepHandoff(t, r, f)
	if len(f.actions) != 1 {
		t.Fatal("ambiguous action repeated")
	}
	if _, e := r.ConfigureHandoff("desktop", "thread-1", 10, true); e == nil {
		t.Fatal("unresolved action discarded")
	}
	r2 := handoffFixture(t)
	s = r2.HandoffStatus()
	s.Stage = "pending"
	s.PendingAction = "continue"
	if e := writeState(r2.handoffPath(), s); e != nil {
		t.Fatal(e)
	}
	f2 := &fakeHandoffOps{observation: handoffEvidence()}
	s = stepHandoff(t, r2, f2)
	if s.Stage != "uncertain" || s.Enabled || len(f2.actions) != 0 {
		t.Fatal("crash should not resend")
	}
}

func TestHandoffWaitsForBackendExitAndPausesOnTimeout(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	for i := 0; i < 4; i++ {
		stepHandoff(t, r, f)
	}
	if s := r.HandoffStatus(); s.Stage != "waiting_exit" || len(f.actions) != 2 || f.actions[1] != HandoffExit {
		t.Fatal("fixture did not reach backend shutdown")
	}
	f.observation.OldExited = false
	if s := stepHandoff(t, r, f); s.Stage != "waiting_exit" || len(f.actions) != 2 {
		t.Fatal("switch began before backend exit was confirmed")
	}
	f.observation.ExitTimedOut = true
	if s := stepHandoff(t, r, f); s.Stage != "uncertain" || s.Enabled || len(f.actions) != 2 {
		t.Fatal("unconfirmed exit did not pause without switching")
	}
}
func TestHandoffManualInterventionAndObservationRaceCancel(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	f.onObserve = func() {
		if _, e := r.ConfigureHandoff("", "", 0, false); e != nil {
			t.Fatal(e)
		}
	}
	if s := stepHandoff(t, r, f); s.Enabled || len(f.actions) != 0 {
		t.Fatal("disabled action raced")
	}
	r = handoffFixture(t)
	f = &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	f.observation.UserIntervened = true
	if s := stepHandoff(t, r, f); s.Stage != "cancelled" || s.Enabled || len(f.actions) != 0 {
		t.Fatal("manual intervention not respected")
	}
}

func TestHandoffCancellationDuringActionPreventsFurtherSteps(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	f.onExecute = func(_ HandoffAction, _ HandoffState) {
		if _, err := r.ConfigureHandoff("", "", 0, false); err != nil {
			t.Fatal(err)
		}
	}
	s := stepHandoff(t, r, f)
	if s.Enabled || s.Stage != "uncertain" || len(f.actions) != 1 {
		t.Fatal("cancellation during an action was not preserved")
	}
	stepHandoff(t, r, f)
	if len(f.actions) != 1 {
		t.Fatal("a further action ran after cancellation")
	}
}

func TestOldCancellationDoesNotDisableNewAuthorization(t *testing.T) {
	r := handoffFixture(t)
	if _, err := r.ConfigureHandoff("", "", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ConfigureHandoff("desktop", "new-thread", 10, true); err != nil {
		t.Fatal(err)
	}
	f := &fakeHandoffOps{observation: handoffEvidence()}
	s := stepHandoff(t, r, f)
	if !s.Enabled {
		t.Fatal("stale cancellation disabled a new authorization")
	}
}

func TestHandoffRearmsOnlyIfCompletedAuthorizationIsUnchanged(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	var completed HandoffState
	for i := 0; i < 11; i++ {
		completed = stepHandoff(t, r, f)
	}
	if completed.Stage != "completed" {
		t.Fatal("fixture did not complete")
	}
	if _, err := r.ConfigureHandoff("", "", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.rearmCompletedHandoff(completed); err != nil || ok {
		t.Fatal("cancelled authorization was re-enabled")
	}
	r = handoffFixture(t)
	f = &fakeHandoffOps{observation: handoffEvidence()}
	for i := 0; i < 11; i++ {
		completed = stepHandoff(t, r, f)
	}
	state, ok, err := r.rearmCompletedHandoff(completed)
	if err != nil || !ok || !state.Enabled || state.Stage != "watching" || state.SessionID != completed.SessionID || state.AuthorizationID != completed.AuthorizationID {
		t.Fatal("completed session did not resume monitoring")
	}
}

func TestHandoffDisableAfterRequestPreservesUncertainty(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	stepHandoff(t, r, f)
	s, err := r.ConfigureHandoff("", "", 0, false)
	if err != nil || s.Enabled || s.Stage != "uncertain" || s.TurnID != "turn-1" {
		t.Fatal("disabling after external action must retain the handoff evidence")
	}
	if _, err = r.ConfigureHandoff("desktop", "thread-1", 10, true); err == nil {
		t.Fatal("unresolved stopped task cannot be silently rebound")
	}
}

func TestAcknowledgementCannotDiscardCredentialRecovery(t *testing.T) {
	r := handoffFixture(t)
	s := r.HandoffStatus()
	s.Enabled = false
	s.Stage = "uncertain"
	s.PendingAction = "switch"
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	if err := writeState(r.handoffAuthTransactionPath(), map[string]any{"version": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcknowledgeHandoff(); err == nil {
		t.Fatal("unresolved credential recovery was discarded")
	}
	if err := os.Remove(r.handoffAuthTransactionPath()); err != nil {
		t.Fatal(err)
	}
	cleared, err := r.AcknowledgeHandoff()
	if err != nil || cleared.Stage != "off" || cleared.Enabled || cleared.PendingAction != "" {
		t.Fatal("manual acknowledgement did not clear the non-credential uncertainty")
	}
}

func TestHandoffRechecksQuotaBeforeRequestingStop(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	f.observation.CurrentRemaining = 50
	if s := stepHandoff(t, r, f); s.Stage != "watching" || len(f.actions) != 0 {
		t.Fatal("recovered source quota must cancel the stop request")
	}
}

func TestHandoffRechecksSettingsBeforeRequestingStop(t *testing.T) {
	r := handoffFixture(t)
	f := &fakeHandoffOps{observation: handoffEvidence()}
	stepHandoff(t, r, f)
	f.observation.Permissions = "changed permission"
	if state := stepHandoff(t, r, f); state.Stage != "watching" || len(f.actions) != 0 {
		t.Fatal("changed settings must cancel the stop request")
	}
}
func TestHandoffBadOrUnknownStateFailsClosed(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	for _, x := range []float64{-1, 0, 100, 101} {
		if _, e := r.ConfigureHandoff("desktop", "id", x, true); e == nil {
			t.Fatal("invalid threshold")
		}
	}
	writeFixture(t, filepath.Join(r.Home, "Library/Application Support/CodexHelper/handoff/state.json"), "broken")
	if s := r.HandoffStatus(); s.Stage != "error" {
		t.Fatal("corrupt state accepted")
	}
}

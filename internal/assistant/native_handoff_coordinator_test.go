package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeNativeHandoffManager struct {
	r        Reader
	readyErr error
	applyErr error
	applies  int
	readies  int
}

func (m *fakeNativeHandoffManager) ready(context.Context, string, string) error {
	m.readies++
	return m.readyErr
}
func (m *fakeNativeHandoffManager) quota(context.Context, string) (AccountQuota, error) {
	q := testQuota(time.Now(), false)
	v := false
	q.LimitReached = &v
	return q, nil
}
func (m *fakeNativeHandoffManager) apply(_ context.Context, id, scope string) error {
	m.applies++
	if m.applyErr != nil {
		return m.applyErr
	}
	if scope != "desktop" && scope != "cli" {
		return errors.New("wrong target")
	}
	source, target := ".codex-cli", ".codex"
	if scope == "cli" {
		source, target = ".codex", ".codex-cli"
	}
	raw, err := os.ReadFile(filepath.Join(m.r.Home, source, "auth.json"))
	if err != nil {
		return err
	}
	if !m.r.handoffCurrentMatchesAccount(raw, id) {
		return errors.New("wrong target")
	}
	if err := os.WriteFile(filepath.Join(m.r.Home, target, "auth.json"), raw, 0600); err != nil {
		return err
	}
	bindings := map[string]any{"version": 1, "desktop": nil, "cli": nil}
	bindings[scope] = map[string]any{"account_id": id}
	return writeState(filepath.Join(m.r.helperStateRoot(), "accounts", "bindings.json"), bindings)
}

func nativeDesktopFixture(t *testing.T) (Reader, *desktopHandoffCoordinator, *fakeDesktopRuntime, *fakeNativeHandoffManager) {
	t.Helper()
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r, d, f := desktopCoordinatorFixture(t)
	store, err := os.ReadFile(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	m := &fakeNativeHandoffManager{r: r}
	d.manager = m
	f.e.RuntimeIdentityVerified = true
	return r, d, f, m
}

func TestNativeDesktopWaitsForIndependentRuntimeIdentity(t *testing.T) {
	r, d, f, manager := nativeDesktopFixture(t)
	f.e.RuntimeIdentityVerified = false
	for i := 0; i < 6; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if manager.applies != 0 || len(f.actions) != 0 || r.HandoffStatus().Stage != "watching" {
		t.Fatal("unknown runtime identity initiated handoff")
	}
}

func TestNativeDesktopCoordinatorUsesManagerOnce(t *testing.T) {
	r, d, f, manager := nativeDesktopFixture(t)
	for i := 0; i < 11; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	state := r.HandoffStatus()
	if state.Stage != "completed" || manager.applies != 1 || len(f.actions) != 4 {
		t.Fatalf("stage=%s applications=%d actions=%v", state.Stage, manager.applies, f.actions)
	}
	if !r.nativeHandoffInstalled("desktop", "managed-b") {
		t.Fatal("target binding not confirmed")
	}
	if _, rearmed, err := d.finishCompleted(context.Background(), state); err != nil || !rearmed {
		t.Fatalf("finish: %v", err)
	}
}

func TestNativeDesktopCoordinatorDoesNotStopWhenManagerNotReady(t *testing.T) {
	r, d, f, manager := nativeDesktopFixture(t)
	manager.readyErr = errors.New("not ready")
	for i := 0; i < 5; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if manager.applies != 0 || len(f.actions) != 0 || r.HandoffStatus().Stage != "watching" {
		t.Fatal("unready target initiated handoff")
	}
}

func TestNativeDesktopCoordinatorDoesNotRepeatAmbiguousApply(t *testing.T) {
	r, d, _, manager := nativeDesktopFixture(t)
	manager.applyErr = errors.New("connection lost")
	for i := 0; i < 12; i++ {
		_, _ = r.AdvanceHandoff(context.Background(), d)
	}
	if manager.applies != 1 || r.HandoffStatus().Stage != "uncertain" {
		t.Fatalf("apply=%d stage=%s", manager.applies, r.HandoffStatus().Stage)
	}
}

func TestNativeCLISwitchUsesFrozenSourceAndManager(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	manager := &fakeNativeHandoffManager{r: r}
	s := &cliHandoffSupervisor{r: r, id: "session", sourceID: "managed-b", targetID: "managed-a", manager: manager, input: &cliHandoffInput{}, armed: true}
	if _, ok := s.backupEvidence(context.Background(), 10); !ok {
		t.Fatal("fixture backup not ready")
	}
	state := HandoffState{Scope: "cli", SessionID: "session", SourceAccount: "managed-b", TargetAccount: "managed-a", Threshold: 10}
	if err := s.Execute(context.Background(), HandoffSwitch, state); err != nil {
		t.Fatal(err)
	}
	if manager.applies != 1 || !r.nativeHandoffInstalled("cli", "managed-a") {
		t.Fatal("native switch not verified")
	}
}

func TestNativeCLISwitchRejectsChangedSource(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	quotaFixtures(t, home)
	r := Reader{Home: home}
	store, err := os.ReadFile(filepath.Join(home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	manager := &fakeNativeHandoffManager{r: r}
	s := &cliHandoffSupervisor{r: r, id: "session", sourceID: "managed-b", targetID: "managed-a", manager: manager, input: &cliHandoffInput{}, armed: true}
	if _, ok := s.backupEvidence(context.Background(), 10); !ok {
		t.Fatal("fixture backup not ready")
	}
	f, err := os.OpenFile(filepath.Join(home, ".codex-cli", "auth.json"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(" ")
	_ = f.Close()
	state := HandoffState{Scope: "cli", SessionID: "session", SourceAccount: "managed-b", TargetAccount: "managed-a", Threshold: 10}
	if err := s.Execute(context.Background(), HandoffSwitch, state); err == nil || manager.applies != 0 {
		t.Fatal("changed source accepted")
	}
}

func TestNativePreflightUsesManagerQuotaAndRespectsReadiness(t *testing.T) {
	manager := &fakeNativeHandoffManager{}
	accounts := []Account{{ID: "managed-a", Name: "A"}, {ID: "managed-b", Name: "B"}}
	ready, issues := nativePreflightQuotas(context.Background(), manager, "desktop", "managed-a", accounts)
	if !ready["managed-b"] || accounts[1].Quota == nil || accounts[0].Quota != nil {
		t.Fatal("manager target quota was not attached")
	}
	if len(issues) != 0 {
		t.Fatal("ready target received an unrelated warning")
	}
	out := HandoffPreflightReport{Threshold: 10}
	if handoffQuotaEvidence(&out, accounts, "managed-a") != "managed-b" || !out.QuotaReady {
		t.Fatal("native target quota was not usable")
	}
	manager.readyErr = errors.New("not ready")
	accounts[1].Quota = nil
	ready, issues = nativePreflightQuotas(context.Background(), manager, "desktop", "managed-a", accounts)
	if ready["managed-b"] || accounts[1].Quota != nil || len(issues) == 0 {
		t.Fatal("unready target received quota evidence")
	}
	manager.readyErr = errors.New("CC Switch 也管理该端，请先退出它以避免同时写入")
	_, issues = nativePreflightQuotas(context.Background(), manager, "desktop", "managed-a", accounts)
	if len(issues) != 1 || !strings.Contains(issues[0], "CC Switch") {
		t.Fatal("same-directory CC Switch conflict was not reported")
	}
}

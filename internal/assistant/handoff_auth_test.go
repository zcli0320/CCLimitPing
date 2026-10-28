package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func handoffAuthFixture(t *testing.T) (Reader, map[string]any) {
	t.Helper()
	home := t.TempDir()
	quotaFixtures(t, home)
	data, err := os.ReadFile(filepath.Join(home, ".codex-cli", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var native map[string]any
	if json.Unmarshal(data, &native) != nil {
		t.Fatal("invalid fixture")
	}
	native["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	native["tokens"].(map[string]any)["refresh_token"] = "REFRESH_MUST_NOT_BE_USED"
	writeHandoffStash(t, home, native)
	return Reader{Home: home}, native
}
func writeHandoffStash(t *testing.T, home string, native map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "logins": map[string]any{"local-key": native}})
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(home, ".cc-switch", "codex-login-stash.json"), string(b))
}
func authorizeFixtureHandoff(t *testing.T, r Reader) {
	t.Helper()
	s := HandoffState{Version: 1, Enabled: true, Scope: "desktop", SessionID: "session", TurnID: "turn", SourceAccount: "managed-a", TargetAccount: "managed-b", Threshold: 10, Stage: "pending", PendingAction: "switch"}
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
}
func quotaForSwitch(_ context.Context, c quotaCredential) AccountQuota {
	claims, ok := decodeTokenClaims(c.access)
	if !ok || claims.Auth.User != "user-b" || c.workspace != "workspace-b" {
		return AccountQuota{Status: "credential_rejected"}
	}
	q := testQuota(time.Now(), false)
	limited := false
	q.LimitReached = &limited
	return q
}
func stoppedFixture(context.Context) error { return nil }

func TestHandoffAuthCandidateRequiresConsistentWholeRecord(t *testing.T) {
	r, native := handoffAuthFixture(t)
	now := time.Now()
	result := r.inspectHandoffAuthCandidate("managed-b", now)
	if !result.LocallyConsistent || !result.CandidateReady {
		t.Fatal("matching complete candidate should be available")
	}
	output, _ := json.Marshal(result)
	for _, sensitive := range []string{"REFRESH_MUST_NOT_BE_USED", "fake-test-signature", "user-b", "workspace-b", "managed-b"} {
		if strings.Contains(string(output), sensitive) {
			t.Fatal("inspection exposed private data")
		}
	}
	tokens := native["tokens"].(map[string]any)
	tokens["refresh_token"] = "older-generation"
	writeHandoffStash(t, r.Home, native)
	if r.inspectHandoffAuthCandidate("managed-b", now).CandidateReady {
		t.Fatal("different refresh generations accepted")
	}
	tokens["refresh_token"] = "REFRESH_MUST_NOT_BE_USED"
	tokens["access_token"] = quotaToken("user-b", "workspace-b", now.Add(-time.Minute).Unix())
	writeHandoffStash(t, r.Home, native)
	if r.inspectHandoffAuthCandidate("managed-b", now).CandidateReady {
		t.Fatal("expired candidate accepted")
	}
	tokens["access_token"] = quotaToken("other", "workspace-b", now.Add(time.Hour).Unix())
	writeHandoffStash(t, r.Home, native)
	if r.inspectHandoffAuthCandidate("managed-b", now).CandidateReady {
		t.Fatal("different user accepted")
	}
}

func TestHandoffSwitchChecksAndAtomicallyRestores(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	authorizeFixtureHandoff(t, r)
	p, candidate, err := r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	commit, err := r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, quotaForSwitch)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if sha256.Sum256(after) != candidate.digest {
		t.Fatal("did not install the complete selected record")
	}
	info, err := os.Stat(commit.backupPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup must be private")
	}
	// Reconstruct recovery after losing all in-memory preparation data.
	recovered, recoveredCommit, err := r.handoffAuthRecovery()
	if err != nil || recoveredCommit.backupPath != commit.backupPath {
		t.Fatal("durable recovery record is missing")
	}
	if err := r.restoreHandoffAuth(recovered, recoveredCommit, stoppedFixture, context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if string(before) != string(restored) {
		t.Fatal("recovery did not restore exact original")
	}
	if err := r.finishHandoffAuth(context.Background(), nil); err == nil {
		t.Fatal("backup must not be removed before runtime verification")
	}
	if err := r.finishHandoffAuth(context.Background(), func(_ context.Context, account string) error {
		if account != "managed-a" {
			return errors.New("wrong account")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(commit.backupPath); !os.IsNotExist(err) {
		t.Fatal("verified handoff should remove its backup")
	}
}

func TestDesktopHandoffReviewKeepsVerifiedCredential(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	authorizeFixtureHandoff(t, r)
	p, _, err := r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, quotaForSwitch); err != nil {
		t.Fatal(err)
	}
	s := r.HandoffStatus()
	s.Enabled = false
	s.Stage = "uncertain"
	s.PendingAction = ""
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	state, err := r.acknowledgeHandoff(context.Background(), quotaForSwitch)
	if err != nil || state.Stage != "off" {
		t.Fatal("verified Desktop recovery could not be acknowledged")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("verified recovery record not cleaned")
	}
	raw, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if !r.handoffCurrentMatchesAccount(raw, "managed-b") {
		t.Fatal("review changed target credential")
	}
}

func TestHandoffSwitchNeverOverwritesChangedAuth(t *testing.T) {
	r, native := handoffAuthFixture(t)
	authorizeFixtureHandoff(t, r)
	p, _, err := r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// An independent actor changes the destination during the quota request.
	changed := []byte(`{"independent_change":true}`)
	verify := func(ctx context.Context, c quotaCredential) AccountQuota {
		writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(changed))
		return quotaForSwitch(ctx, c)
	}
	if _, err := r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, verify); err == nil {
		t.Fatal("racing destination accepted")
	}
	after, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if string(after) != string(changed) {
		t.Fatal("independent change was overwritten")
	}
	// A changed candidate is rejected before any write.
	r, native = handoffAuthFixture(t)
	authorizeFixtureHandoff(t, r)
	p, _, err = r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	native["last_refresh"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	writeHandoffStash(t, r.Home, native)
	if _, err = r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, quotaForSwitch); err == nil {
		t.Fatal("changed candidate accepted")
	}
}

func TestHandoffSwitchStopsWhenUserIntervenesDuringQuotaCheck(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	authorizeFixtureHandoff(t, r)
	p, _, err := r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Home, ".codex/auth.json")
	before, _ := os.ReadFile(path)
	s := &cliHandoffSupervisor{input: &cliHandoffInput{}, armed: true}
	_, err = r.commitHandoffAuth(context.Background(), p, 10, s.stoppedForHandoff, func(ctx context.Context, c quotaCredential) AccountQuota {
		s.input.forward([]byte("manual\n"))
		return quotaForSwitch(ctx, c)
	})
	if err == nil {
		t.Fatal("credential switch ignored intervention during its quota check")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("source authentication changed despite intervention")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("a cancelled pre-commit left a credential transaction")
	}
}

func TestHandoffSwitchRequiresConsentStoppedClientAndUsableBackup(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	p, _, err := r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, quotaForSwitch); err == nil {
		t.Fatal("unauthorized switch accepted")
	}
	authorizeFixtureHandoff(t, r)
	if _, err = r.commitHandoffAuth(context.Background(), p, 10, func(context.Context) error { return errors.New("running") }, quotaForSwitch); err == nil {
		t.Fatal("running client accepted")
	}
	if _, err = r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, func(context.Context, quotaCredential) AccountQuota {
		return AccountQuota{Status: "credential_rejected"}
	}); err == nil {
		t.Fatal("rejected credential accepted")
	}
}

func TestHandoffRestoreRefusesToOverwriteAnotherChange(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	authorizeFixtureHandoff(t, r)
	p, _, _ := r.prepareHandoffAuth("desktop", "managed-b", time.Now())
	commit, err := r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, quotaForSwitch)
	if err != nil {
		t.Fatal(err)
	}
	changed := `{"another_login":true}`
	writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), changed)
	if err := r.restoreHandoffAuth(p, commit, stoppedFixture, context.Background()); err == nil {
		t.Fatal("changed auth overwritten")
	}
	live, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if string(live) != changed {
		t.Fatal("changed auth was overwritten")
	}
}

func cliRecoveryFixture(t *testing.T) (Reader, handoffAuthCommit) {
	t.Helper()
	r, _ := handoffAuthFixture(t)
	source, err := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.Home, ".codex-cli/auth.json"), string(source))
	authorizeFixtureHandoff(t, r)
	s := r.HandoffStatus()
	s.Scope = "cli"
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.prepareHandoffAuth("cli", "managed-b", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.commitHandoffAuth(context.Background(), p, 10, stoppedFixture, quotaForSwitch)
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled, s.Stage, s.PendingAction = false, "uncertain", "switch"
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	return r, commit
}

func TestHandoffRecoveryRequiresReviewAndKeepsCurrentAuth(t *testing.T) {
	r, commit := cliRecoveryFixture(t)
	state := r.HandoffStatus()
	if _, err := r.ConfigureHandoff("cli", "different-session", 10, true); err == nil || r.HandoffStatus() != state {
		t.Fatal("an unresolved transaction must not lose its recovery coordinates")
	}
	if _, err := r.acknowledgeHandoff(context.Background(), func(context.Context, quotaCredential) AccountQuota {
		return AccountQuota{Status: "credential_rejected"}
	}); err == nil {
		t.Fatal("a rejected current credential must keep recovery available")
	}
	if _, err := os.Stat(commit.backupPath); err != nil {
		t.Fatal("backup disappeared before successful verification")
	}
	path := filepath.Join(r.Home, ".codex-cli/auth.json")
	before, _ := os.ReadFile(path)
	lock, err := lockWarmup(filepath.Join(filepath.Dir(r.resumePath()), "cli-runner.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.acknowledgeHandoff(context.Background(), quotaForSwitch); err == nil {
		t.Fatal("an active owned runner must prevent acknowledgement")
	}
	lock()
	cleared, err := r.acknowledgeHandoff(context.Background(), quotaForSwitch)
	if err != nil || cleared.Stage != "off" || cleared.Enabled {
		t.Fatalf("verified current account could not be retained: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("acknowledgement modified authentication")
	}
	if _, err := os.Stat(commit.backupPath); !os.IsNotExist(err) {
		t.Fatal("verified recovery copy was not removed")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("verified transaction was not removed")
	}
}

func TestHandoffRecoveryRefusesChangedCurrentCredential(t *testing.T) {
	r, commit := cliRecoveryFixture(t)
	path := filepath.Join(r.Home, ".codex-cli/auth.json")
	_, err := r.acknowledgeHandoff(context.Background(), func(ctx context.Context, c quotaCredential) AccountQuota {
		writeFixture(t, path, `{"independent_change":true}`)
		return quotaForSwitch(ctx, c)
	})
	if err == nil {
		t.Fatal("a concurrent credential change was accepted")
	}
	if _, err := os.Stat(commit.backupPath); err != nil {
		t.Fatal("backup disappeared after a concurrent change")
	}
	if r.HandoffStatus().Stage != "uncertain" {
		t.Fatal("a concurrent change cleared the uncertain state")
	}
}

func TestCompletedHandoffWithRecoveryRecordCannotBeReconfigured(t *testing.T) {
	r, _ := cliRecoveryFixture(t)
	s := r.HandoffStatus()
	s.Stage, s.PendingAction = "completed", ""
	if err := writeState(r.handoffPath(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ConfigureHandoff("cli", "new-session", 10, true); err == nil || r.HandoffStatus() != s {
		t.Fatal("completed state with a recovery copy was replaced")
	}
	cleared, err := r.acknowledgeHandoff(context.Background(), quotaForSwitch)
	if err != nil || cleared.Stage != "off" {
		t.Fatal("completed recovery could not be acknowledged")
	}
}

type simulatedDirectHandoff struct {
	r         Reader
	closed    bool
	switched  bool
	restarted bool
	continued bool
}

func (s *simulatedDirectHandoff) Observe(_ context.Context, _ HandoffState) (HandoffObservation, error) {
	return HandoffObservation{TargetMatches: true, Running: !s.closed, TurnID: "turn", SourceAccount: "managed-a", TargetAccount: "managed-b", Directory: "/project", Model: "model", Effort: "low", Permissions: "default", CurrentQuotaKnown: true, CurrentRemaining: 9, TargetQuotaKnown: true, TargetRemaining: 80, TargetWeeklyUsable: true, TargetCredentialReady: true, StopConfirmed: true, StoppedForHandoff: true, OtherSessionsIdle: true, OldExited: s.closed, SwitchVerified: s.switched, RestartReady: s.restarted, IdentityVerified: s.restarted, SettingsVerified: s.restarted, ContinueStarted: s.continued}, nil
}
func (s *simulatedDirectHandoff) Execute(ctx context.Context, action HandoffAction, state HandoffState) error {
	switch action {
	case HandoffRequestStop:
	case HandoffExit:
		s.closed = true
	case HandoffSwitch:
		p, _, err := s.r.prepareHandoffAuth(state.Scope, state.TargetAccount, time.Now())
		if err != nil {
			return err
		}
		_, err = s.r.commitHandoffAuth(ctx, p, state.Threshold, func(context.Context) error {
			if !s.closed {
				return errors.New("still running")
			}
			return nil
		}, quotaForSwitch)
		if err != nil {
			return err
		}
		s.switched = true
	case HandoffRestart:
		s.restarted = true
	case HandoffContinue:
		s.continued = true
	default:
		return errors.New("unexpected action")
	}
	return nil
}

func TestDirectCredentialSwitchIntegratesWithHandoffJournal(t *testing.T) {
	r, _ := handoffAuthFixture(t)
	if _, err := r.ConfigureHandoff("desktop", "session", 10, true); err != nil {
		t.Fatal(err)
	}
	fake := &simulatedDirectHandoff{r: r}
	for i := 0; i < 12; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), fake); err != nil {
			t.Fatal(err)
		}
	}
	if state := r.HandoffStatus(); state.Stage != "completed" || !fake.switched || !fake.continued {
		t.Fatal("isolated handoff did not complete")
	}
	live, _ := os.ReadFile(filepath.Join(r.Home, ".codex", "auth.json"))
	if !r.handoffCurrentMatchesAccount(live, "managed-b") {
		t.Fatal("selected target auth was not installed")
	}
	if err := r.finishHandoffAuth(context.Background(), func(_ context.Context, account string) error {
		if !fake.restarted || account != "managed-b" {
			return errors.New("runtime identity not confirmed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

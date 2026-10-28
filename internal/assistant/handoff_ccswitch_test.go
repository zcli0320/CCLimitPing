package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func ccSwitchFixture(t *testing.T) (Reader, map[string]any) {
	r, native := handoffAuthFixture(t)
	r.SQLite = "/usr/bin/sqlite3"
	settings, _ := json.Marshal(map[string]string{"codexConfigDir": filepath.Join(r.Home, ".codex")})
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/settings.json"), string(settings))
	writeFixture(t, filepath.Join(r.Home, ".codex/config.toml"), "model='fixture-model'\n")
	db := filepath.Join(r.Home, ".cc-switch/cc-switch.db")
	sql := `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER); INSERT INTO providers VALUES('target-provider','Fixture target','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-b"}}',0);`
	if out, err := exec.Command(r.SQLite, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("fixture db: %s", out)
	}
	return r, native
}

func TestCCSwitchBridgeUsesManagerResultNotStash(t *testing.T) {
	r, native := ccSwitchFixture(t)
	authorizeFixtureHandoff(t, r)
	b := &desktopCCSwitch{r: r}
	request := 0
	b.run = func(_ context.Context, action, name string) error {
		if action != "switch" || name != "Fixture target" {
			return errors.New("wrong action")
		}
		request++
		data, _ := json.Marshal(native)
		writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(data))
		return exec.Command(r.SQLite, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `UPDATE providers SET is_current=1 WHERE id='target-provider'`).Run()
	}
	if err := b.switchAccount(context.Background(), r.HandoffStatus(), func(context.Context) error { return errors.New("still running") }); err == nil || request != 0 {
		t.Fatal("switched before exit")
	}
	if err := b.switchAccount(context.Background(), r.HandoffStatus(), stoppedFixture); err != nil {
		t.Fatal(err)
	}
	if request != 1 || !b.installed(r.HandoffStatus()) {
		t.Fatal("manager-produced auth not confirmed")
	}
	if _, err := r.ConfigureHandoff("desktop", "other", 10, true); err == nil {
		t.Fatal("unresolved switch record discarded")
	}
	if err := b.finish(context.Background(), r.HandoffStatus(), func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.ccSwitchTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("verified transaction not cleaned")
	}
}

func TestCCSwitchBridgeKeepsRecoveryAfterAmbiguousAction(t *testing.T) {
	r, _ := ccSwitchFixture(t)
	authorizeFixtureHandoff(t, r)
	b := &desktopCCSwitch{r: r, run: func(context.Context, string, string) error { return errors.New("connection lost") }}
	if err := b.switchAccount(context.Background(), r.HandoffStatus(), stoppedFixture); err == nil {
		t.Fatal("ambiguous action accepted")
	}
	if _, err := os.Stat(r.ccSwitchTransactionPath()); err != nil {
		t.Fatal("recovery record missing")
	}
	if b.installed(r.HandoffStatus()) {
		t.Fatal("source auth mistaken for target")
	}
}

func TestCCSwitchExplicitReviewArchivesAndKeepsCurrent(t *testing.T) {
	r, native := ccSwitchFixture(t)
	authorizeFixtureHandoff(t, r)
	b := &desktopCCSwitch{r: r, run: func(context.Context, string, string) error {
		raw, _ := json.Marshal(native)
		writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(raw))
		return exec.Command(r.SQLite, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `UPDATE providers SET is_current=1 WHERE id='target-provider'`).Run()
	}}
	if err := b.switchAccount(context.Background(), r.HandoffStatus(), stoppedFixture); err != nil {
		t.Fatal(err)
	}
	var txn ccSwitchTransaction
	_ = readJSON(r.ccSwitchTransactionPath(), &txn)
	s := r.HandoffStatus()
	s.Enabled = false
	s.Stage = "uncertain"
	s.PendingAction = ""
	_ = writeState(r.handoffPath(), s)
	state, err := r.acknowledgeHandoff(context.Background(), quotaForSwitch)
	if err != nil || state.Stage != "off" {
		t.Fatalf("review: %v", err)
	}
	dir := filepath.Join(filepath.Dir(r.handoffPath()), "backups", txn.BackupDir)
	for _, name := range []string{"auth.json", "config.toml", "reviewed-state.json", "reviewed-transaction.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("review discarded original evidence")
		}
	}
	if _, err := os.Stat(r.ccSwitchTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("review did not close active transaction")
	}
	raw, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if !r.handoffCurrentMatchesAccount(raw, "managed-b") {
		t.Fatal("review changed current account")
	}
}

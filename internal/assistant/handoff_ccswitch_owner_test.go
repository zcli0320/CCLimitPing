package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopWriterOwnerAndReadiness(t *testing.T) {
	r, d, _, manager := nativeDesktopFixture(t)
	r.SQLite = "/usr/bin/sqlite3"
	d.r = r
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/settings.json"), `{"codexConfigDir":"~/.codex"}`)
	db := filepath.Join(r.Home, ".cc-switch/cc-switch.db")
	sql := `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER); INSERT INTO providers VALUES('p','fixture','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-b"}}',0);`
	if out, err := exec.Command(r.SQLite, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("db: %s: %v", out, err)
	}
	if !r.desktopWriterIsCCSwitch() {
		t.Fatal("Desktop owner not detected")
	}
	manager.readyErr = errors.New("must not call native ready")
	d.ccswitch = &desktopCCSwitch{r: r}
	if _, ok := d.targetEvidence(context.Background(), 10); !ok || manager.readies != 0 {
		t.Fatal("CC Switch route did not use read-only quota")
	}
	accounts := []Account{{ID: "managed-a"}, {ID: "managed-b"}}
	ready, issues := nativePreflightQuotas(context.Background(), manager, "desktop", "managed-a", accounts, true)
	if !ready["managed-b"] || len(issues) != 0 || manager.readies != 0 {
		t.Fatal("preflight called native ready")
	}
	for _, settings := range []string{`{"codexConfigDir":"~/.codex-cli"}`} {
		writeFixture(t, filepath.Join(r.Home, ".cc-switch/settings.json"), settings)
		if r.desktopWriterIsCCSwitch() {
			t.Fatal("wrong owner")
		}
		d.ccswitch = nil
		if _, ok := d.targetEvidence(context.Background(), 10); ok {
			t.Fatal("native ready error ignored")
		}
	}
	if manager.readies != 1 {
		t.Fatalf("native ready calls: %d", manager.readies)
	}
	if err := os.Remove(filepath.Join(r.Home, ".cc-switch/settings.json")); err != nil {
		t.Fatal(err)
	}
	if r.desktopWriterIsCCSwitch() {
		t.Fatal("missing settings selected CC Switch")
	}
	if _, ok := d.targetEvidence(context.Background(), 10); ok || manager.readies != 2 {
		t.Fatal("missing settings did not use native readiness")
	}
}

func mappedCCSwitchFixture(t *testing.T) (Reader, map[string]any) {
	t.Helper()
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r, live := ccSwitchFixture(t)
	data, err := os.ReadFile(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(data))
	var store map[string]any
	if json.Unmarshal(data, &store) != nil {
		t.Fatal("bad store")
	}
	accounts := store["accounts"].(map[string]any)
	b := accounts["managed-b"].(map[string]any)
	b["account_id"] = "cc-b"
	accounts["cc-b"] = b
	delete(accounts, "managed-b")
	data, _ = json.Marshal(store)
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), string(data))
	if err := exec.Command(r.SQLite, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `UPDATE providers SET meta='{"authBinding":{"authProvider":"codex_oauth","accountId":"cc-b"}}'`).Run(); err != nil {
		t.Fatal(err)
	}
	return r, live
}

func TestCCSwitchAccountMappingUniqueMissingDuplicate(t *testing.T) {
	r, _ := mappedCCSwitchFixture(t)
	key, err := r.ccSwitchAccountKey("managed-b")
	if err != nil || key != "cc-b" {
		t.Fatalf("mapping: %q %v", key, err)
	}
	if row, err := r.ccSwitchBinding(context.Background(), "managed-b"); err != nil || row.ID != "target-provider" {
		t.Fatalf("binding: %+v %v", row, err)
	}
	if _, err := r.ccSwitchAccountKey("missing"); err == nil {
		t.Fatal("missing helper account accepted")
	}
	var store map[string]any
	path := filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json")
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &store)
	accounts := store["accounts"].(map[string]any)
	b := accounts["cc-b"].(map[string]any)
	copyB := make(map[string]any)
	for k, v := range b {
		copyB[k] = v
	}
	copyB["account_id"] = "cc-copy"
	accounts["cc-copy"] = copyB
	data, _ = json.Marshal(store)
	writeFixture(t, path, string(data))
	if _, err := r.ccSwitchAccountKey("managed-b"); err == nil || !strings.Contains(err.Error(), "多个") {
		t.Fatalf("duplicate: %v", err)
	}
	delete(accounts, "cc-b")
	delete(accounts, "cc-copy")
	data, _ = json.Marshal(store)
	writeFixture(t, path, string(data))
	if _, err := r.ccSwitchAccountKey("managed-b"); err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("no match: %v", err)
	}
}

func TestCCSwitchLiveMatchesMappedKey(t *testing.T) {
	r, live := mappedCCSwitchFixture(t)
	data, _ := json.Marshal(live)
	writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(data))
	if _, err := r.ccSwitchLiveMatches("managed-b"); err != nil {
		t.Fatal(err)
	}
}

func TestCCSwitchOwnerCoordinatorUsesBridgeAndRetainsIdentityGate(t *testing.T) {
	r, d, runtime, manager := nativeDesktopFixture(t)
	r.SQLite = "/usr/bin/sqlite3"
	d.r = r
	writeFixture(t, filepath.Join(r.Home, ".cc-switch/settings.json"), `{"codexConfigDir":"~/.codex"}`)
	writeFixture(t, filepath.Join(r.Home, ".codex/config.toml"), "model_provider = 'openai'\n")
	db := filepath.Join(r.Home, ".cc-switch/cc-switch.db")
	sql := `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER); INSERT INTO providers VALUES('p','fixture','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-b"}}',0);`
	if out, err := exec.Command(r.SQLite, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("db: %s: %v", out, err)
	}
	manager.readyErr = errors.New("native write readiness must not be called")
	calls := 0
	d.ccswitch = &desktopCCSwitch{r: r, run: func(_ context.Context, action, name string) error {
		calls++
		if action != "switch" || name != "fixture" {
			return errors.New("wrong bridge action")
		}
		raw, err := os.ReadFile(filepath.Join(r.Home, ".codex-cli/auth.json"))
		if err != nil {
			return err
		}
		var auth map[string]any
		if err := json.Unmarshal(raw, &auth); err != nil {
			return err
		}
		auth["tokens"].(map[string]any)["refresh_token"] = "REFRESH_MUST_NOT_BE_USED"
		raw, _ = json.Marshal(auth)
		writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(raw))
		return exec.Command(r.SQLite, db, `UPDATE providers SET is_current=1 WHERE id='p'`).Run()
	}}
	runtime.e.RuntimeIdentityVerified = false
	for i := 0; i < 3; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 || len(runtime.actions) != 0 {
		t.Fatal("unverified runtime initiated handoff")
	}
	runtime.e.RuntimeIdentityVerified = true
	for i := 0; i < 11; i++ {
		if _, err := r.AdvanceHandoff(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	state := r.HandoffStatus()
	if state.Stage != "completed" || calls != 1 || manager.applies != 0 || manager.readies != 0 {
		t.Fatalf("stage=%s bridge=%d applies=%d ready=%d", state.Stage, calls, manager.applies, manager.readies)
	}
	if _, rearmed, err := d.finishCompleted(context.Background(), state); err != nil || !rearmed {
		t.Fatalf("finish: %v", err)
	}
}

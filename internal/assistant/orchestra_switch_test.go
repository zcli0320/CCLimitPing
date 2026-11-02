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

type fakeOrchestraManager struct {
	*fakeNativeHandoffManager
	checks    int
	stopError error
	failAt    int
}

func (m *fakeOrchestraManager) Close() {}
func (m *fakeOrchestraManager) orchestraDesktopStopped(context.Context, string) error {
	m.checks++
	if m.failAt > 0 && m.checks >= m.failAt {
		return errors.New("Desktop started")
	}
	return m.stopError
}

func TestOrchestraSwitchNativeAndGuards(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopError error
		failAt    int
		failApply bool
		wantApply int
		uncertain bool
	}{
		{"success", nil, 0, false, 1, false}, {"running", errors.New("Desktop running"), 0, false, 0, false}, {"unknown", errors.New("cannot inspect"), 0, false, 0, false}, {"appears-before-write", nil, 3, false, 0, true}, {"write-fails", nil, 0, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, base := nativeDesktopFixture(t)
			if err := writeState(r.handoffPath(), HandoffState{Version: 1, Threshold: 10, Stage: "off"}); err != nil {
				t.Fatal(err)
			}
			m := &fakeOrchestraManager{fakeNativeHandoffManager: base, stopError: tc.stopError, failAt: tc.failAt}
			if tc.failApply {
				m.applyErr = errors.New("failed")
			}
			deps := orchestraSwitchDeps{manager: func(context.Context) (orchestraSwitchManager, error) { return m, nil }, bridge: newDesktopCCSwitch}
			reserved := 0
			out := r.orchestraSwitchDesktop(context.Background(), "attempt-1", 10, func(string) error { reserved++; return nil }, func() bool { return true }, deps)
			if m.applies != tc.wantApply || out.Uncertain != tc.uncertain {
				t.Fatalf("result=%+v applies=%d checks=%d reserved=%d", out, m.applies, m.checks, reserved)
			}
			if tc.name == "success" && (!out.Known || r.HandoffStatus().Stage != "completed") {
				t.Fatalf("result=%+v state=%+v", out, r.HandoffStatus())
			}
		})
	}
}

func TestOrchestraSwitchNoEligibleTarget(t *testing.T) {
	r, _, _, base := nativeDesktopFixture(t)
	if err := writeState(r.handoffPath(), HandoffState{Version: 1, Threshold: 10, Stage: "off"}); err != nil {
		t.Fatal(err)
	}
	base.readyErr = errors.New("unavailable")
	m := &fakeOrchestraManager{fakeNativeHandoffManager: base}
	deps := orchestraSwitchDeps{manager: func(context.Context) (orchestraSwitchManager, error) { return m, nil }, bridge: newDesktopCCSwitch}
	result := r.orchestraSwitchDesktop(context.Background(), "attempt", 10, func(string) error { t.Fatal("write reserved without candidate"); return nil }, func() bool { return true }, deps)
	if result.Reason != "没有额度充足的备用账号" || m.applies != 0 {
		t.Fatalf("%+v", result)
	}
}

func TestOrchestraSwitchCCSwitchRoute(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r, native := ccSwitchFixture(t)
	store, err := os.ReadFile(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	if err = writeState(r.handoffPath(), HandoffState{Version: 1, Threshold: 10, Stage: "off"}); err != nil {
		t.Fatal(err)
	}
	m := &fakeOrchestraManager{fakeNativeHandoffManager: &fakeNativeHandoffManager{r: r}}
	b := &desktopCCSwitch{r: r, run: func(_ context.Context, action, name string) error {
		if action != "switch" {
			return errors.New("bad operation")
		}
		raw, _ := json.Marshal(native)
		writeFixture(t, filepath.Join(r.Home, ".codex/auth.json"), string(raw))
		return exec.Command(r.SQLite, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `UPDATE providers SET is_current=1 WHERE id='target-provider'`).Run()
	}}
	deps := orchestraSwitchDeps{manager: func(context.Context) (orchestraSwitchManager, error) { return m, nil }, bridge: func(Reader) (*desktopCCSwitch, error) { return b, nil }}
	out := r.orchestraSwitchDesktop(context.Background(), "attempt-cc", 10, func(route string) error {
		if route != "ccswitch" {
			t.Fatal(route)
		}
		return nil
	}, func() bool { return true }, deps)
	if !out.Known || out.Uncertain || out.Route != "ccswitch" {
		t.Fatalf("result=%+v", out)
	}
}

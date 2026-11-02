package orchestra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wavever/CCLimitPing/internal/assistant"
)

func switchFixture(t *testing.T) (env, *Runner, []*Task) {
	t.Helper()
	e := setup(t)
	e.cfg.Home = t.TempDir()
	if err := WriteSettings(e.cfg.Dir, Settings{Account: "desktop", MinRemaining: 10, AutoSwitch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Dispatch(e.cfg, "test", "codex", e.folder, "A", "card"); err != nil {
		t.Fatal(err)
	}
	_, err := e.store.Update("A", func(t *Task) error { t.Status = QuotaExhausted; t.Session = "fixture-session"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Cfg: e.cfg, Store: e.store, Logf: func(string, ...any) {}, quotaStatus: RunnerStatus{Paused: true, Account: "Desktop", CheckedAt: time.Now()}}
	tasks, _ := e.store.List()
	return e, r, tasks
}

func TestAutoSwitchRequeuesExactlyOnce(t *testing.T) {
	e, r, tasks := switchFixture(t)
	calls := 0
	r.Cfg.Switch = func(_ context.Context, _ string, _ float64, before func(string) error, permit func() bool) assistant.OrchestraSwitchResult {
		calls++
		if !permit() {
			t.Fatal("not permitted")
		}
		if e := before("native"); e != nil {
			t.Fatal(e)
		}
		return assistant.OrchestraSwitchResult{Known: true, Remaining: 70}
	}
	r.maybeAutoSwitch(tasks)
	task, _ := e.store.Get("A")
	status, _ := ReadSwitchStatus(e.cfg.Dir)
	if calls != 1 || task.Status != Queued || task.Session != "fixture-session" || status.Stage != "completed" || status.ResumeClaims["A"] != "queued" || r.quotaStatus.Paused {
		t.Fatalf("calls=%d task=%+v status=%+v", calls, task, status)
	}
	r.quotaStatus.Paused = true
	r.lastSwitchCheck = time.Time{}
	r.maybeAutoSwitch(tasks)
	if calls != 1 {
		t.Fatal("cooldown allowed another attempt")
	}
}

func TestAutoSwitchDisabledAndSeparate(t *testing.T) {
	for _, s := range []Settings{{Account: "desktop", MinRemaining: 10}, {Account: "separate", MinRemaining: 10, AutoSwitch: true}} {
		e, r, tasks := switchFixture(t)
		if err := WriteSettings(e.cfg.Dir, s); err != nil {
			t.Fatal(err)
		}
		r.Cfg.Switch = func(context.Context, string, float64, func(string) error, func() bool) assistant.OrchestraSwitchResult {
			t.Fatal("switch attempted")
			return assistant.OrchestraSwitchResult{}
		}
		r.maybeAutoSwitch(tasks)
	}
}

func TestAutoSwitchFailuresAndRecovery(t *testing.T) {
	for _, reason := range []string{"关闭 Codex Desktop 后可自动切号", "无法确认 Desktop 已关闭", "没有额度充足的备用账号"} {
		e, r, tasks := switchFixture(t)
		r.Cfg.Switch = func(context.Context, string, float64, func(string) error, func() bool) assistant.OrchestraSwitchResult {
			return assistant.OrchestraSwitchResult{Reason: reason}
		}
		r.maybeAutoSwitch(tasks)
		task, _ := e.store.Get("A")
		if task.Status != QuotaExhausted || !r.quotaStatus.Paused || r.quotaStatus.Reason != reason {
			t.Fatal(task, r.quotaStatus)
		}
	}
	e, r, tasks := switchFixture(t)
	r.Cfg.Switch = func(_ context.Context, _ string, _ float64, before func(string) error, _ func() bool) assistant.OrchestraSwitchResult {
		if err := before("ccswitch"); err != nil {
			t.Fatal(err)
		}
		return assistant.OrchestraSwitchResult{Uncertain: true, Reason: "ambiguous"}
	}
	r.maybeAutoSwitch(tasks)
	s, _ := ReadSwitchStatus(e.cfg.Dir)
	if !s.ReviewRequired || s.Stage != "uncertain" || !r.quotaStatus.Paused {
		t.Fatal(s)
	}
	// A new runner must not repeat an attempt persisted before a crash.
	s.Stage = "pending"
	s.ReviewRequired = false
	if err := writeJSON(e.cfg.Dir, "auto-switch.json", s); err != nil {
		t.Fatal(err)
	}
	r2 := &Runner{Cfg: r.Cfg, Store: e.store, quotaStatus: RunnerStatus{Paused: true}, Logf: func(string, ...any) {}}
	r2.Cfg.Switch = func(context.Context, string, float64, func(string) error, func() bool) assistant.OrchestraSwitchResult {
		t.Fatal("replayed write")
		return assistant.OrchestraSwitchResult{}
	}
	r2.maybeAutoSwitch(tasks)
	s, _ = ReadSwitchStatus(e.cfg.Dir)
	if !s.ReviewRequired || s.Stage != "uncertain" {
		t.Fatal(s)
	}
}

func TestAutoSwitchCooldownClockRollback(t *testing.T) {
	e, r, tasks := switchFixture(t)
	if err := writeJSON(e.cfg.Dir, "auto-switch.json", SwitchStatus{Stage: "completed", StartedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r.Cfg.Switch = func(context.Context, string, float64, func(string) error, func() bool) assistant.OrchestraSwitchResult {
		t.Fatal("backward clock bypassed cooldown")
		return assistant.OrchestraSwitchResult{}
	}
	r.maybeAutoSwitch(tasks)
	if !r.quotaStatus.Paused {
		t.Fatal(errors.New("queue unpaused"))
	}
}

func TestAutoSwitchCooldownWritesStatusOnce(t *testing.T) {
	e, r, tasks := switchFixture(t)
	if err := writeJSON(e.cfg.Dir, "auto-switch.json", SwitchStatus{Stage: "completed", StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	r.maybeAutoSwitch(tasks)
	path := filepath.Join(e.cfg.Dir, "runner-status.json")
	if s, err := ReadRunnerStatus(e.cfg.Dir); err != nil || !s.Paused || s.Reason != "自动切号冷却中" {
		t.Fatalf("status=%+v err=%v", s, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r.maybeAutoSwitch(tasks)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unchanged cooldown rewrote runner-status.json: %v", err)
	}
}

func TestAutoSwitchAckRequiresResolvedJournalAndFreshTrigger(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	e, r, tasks := switchFixture(t)
	s := SwitchStatus{Stage: "uncertain", ReviewRequired: true, StartedAt: time.Now().UTC(), Trigger: switchTrigger(tasks), HelperPreview: true}
	if err := writeJSON(e.cfg.Dir, "auto-switch.json", s); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.cfg.Home, "Library/Application Support/CodexHelper-AccountPreview/handoff")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"threshold":10,"stage":"uncertain"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcknowledgeAutoSwitch(e.cfg.Dir, e.cfg.Home); err == nil {
		t.Fatal("unresolved handoff accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"threshold":10,"stage":"off"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pending := range []string{filepath.Join(dir, "ccswitch-transaction.json"), filepath.Join(dir, "credential-transaction.json"), filepath.Join(e.cfg.Home, "Library/Application Support/CodexHelper-AccountPreview/accounts/pending.json")} {
		if err := os.MkdirAll(filepath.Dir(pending), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pending, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := AcknowledgeAutoSwitch(e.cfg.Dir, e.cfg.Home); err == nil {
			t.Fatalf("accepted pending journal %s", filepath.Base(pending))
		}
		if err := os.Remove(pending); err != nil {
			t.Fatal(err)
		}
	}
	ack, err := AcknowledgeAutoSwitch(e.cfg.Dir, e.cfg.Home)
	if err != nil || ack.Stage != "acknowledged" || ack.ReviewRequired || !ack.StartedAt.Equal(s.StartedAt) {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	r.Cfg.Switch = func(context.Context, string, float64, func(string) error, func() bool) assistant.OrchestraSwitchResult {
		t.Fatal("repeated attempt")
		return assistant.OrchestraSwitchResult{}
	}
	r.maybeAutoSwitch(tasks)
	ack.StartedAt = time.Now().Add(-time.Hour)
	if err := writeJSON(e.cfg.Dir, "auto-switch.json", ack); err != nil {
		t.Fatal(err)
	}
	r.lastSwitchCheck = time.Time{}
	r.maybeAutoSwitch(tasks)
	if r.quotaStatus.Reason != "已解除核查暂停，等待新的额度触发" {
		t.Fatal(r.quotaStatus)
	}
}

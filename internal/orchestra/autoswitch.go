package orchestra

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wavever/CCLimitPing/internal/assistant"
)

const switchCooldown = 30 * time.Minute

func (r *Runner) maybeAutoSwitch(tasks []*Task) {
	if r.Cfg.Home == "" || !r.quotaStatus.Paused {
		return
	}
	unlock, err := lockSettings(r.Cfg.Dir)
	if err != nil {
		return
	}
	defer unlock()
	settings, err := ReadSettings(r.Cfg.Dir)
	if err != nil || !settings.AutoSwitch || settings.Account != "desktop" {
		return
	}
	old, err := ReadSwitchStatus(r.Cfg.Dir)
	if err != nil {
		return
	}
	if old.ReviewRequired || old.Stage == "uncertain" {
		r.switchPause("自动切号结果待核查")
		return
	}
	if old.Stage == "pending" || old.Stage == "resuming" {
		old.Stage = "uncertain"
		old.ReviewRequired = true
		_ = writeJSON(r.Cfg.Dir, "auto-switch.json", old)
		r.switchPause("自动切号结果待核查")
		return
	}
	if !old.StartedAt.IsZero() && time.Since(old.StartedAt) < switchCooldown {
		r.switchPause("自动切号冷却中")
		return
	}
	if !r.lastSwitchCheck.IsZero() && time.Since(r.lastSwitchCheck) < time.Minute {
		return
	}
	r.lastSwitchCheck = time.Now()
	var exhausted []string
	need := false
	for _, t := range tasks {
		if t.Executor != "codex" {
			continue
		}
		if t.Status == Running || t.Unconfirmed {
			r.switchPause("执行进程状态待确认")
			return
		}
		if t.Status == Queued || t.Status == QuotaExhausted {
			need = true
		}
		if t.Status == QuotaExhausted && t.Session != "" {
			exhausted = append(exhausted, t.ID)
		}
	}
	if !need {
		return
	}
	trigger := switchTrigger(tasks)
	if old.Stage == "acknowledged" && (old.Trigger == "" || old.Trigger == trigger) {
		r.switchPause("已解除核查暂停，等待新的额度触发")
		return
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		r.switchPause("无法登记自动切号")
		return
	}
	id := hex.EncodeToString(nonce[:])
	state := SwitchStatus{Stage: "pending", AttemptID: id, StartedAt: time.Now().UTC(), Source: "当前账号", Target: "备用账号", Tasks: exhausted, ResumeClaims: map[string]string{}}
	state.Trigger = trigger
	state.HelperPreview = os.Getenv("CODEX_HELPER_NATIVE_PREVIEW") == "1"
	reserved := false
	permit := func() bool {
		now, e := ReadSettings(r.Cfg.Dir)
		return e == nil && now == settings && now.AutoSwitch && now.Account == "desktop"
	}
	before := func(route string) error {
		state.Route = route
		if !permit() {
			return context.Canceled
		}
		if e := writeJSON(r.Cfg.Dir, "auto-switch.json", state); e != nil {
			return e
		}
		reserved = true
		return nil
	}
	switcher := r.Cfg.Switch
	if switcher == nil {
		switcher = (assistant.Reader{Home: r.Cfg.Home}).OrchestraSwitchDesktop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	result := switcher(ctx, id, settings.MinRemaining, before, permit)
	cancel()
	if !reserved {
		if result.Reason != "" {
			r.switchPause(result.Reason)
		}
		return
	}
	if result.Uncertain || !result.Known || result.Remaining < settings.MinRemaining || !permit() {
		state.Stage = "uncertain"
		state.ReviewRequired = true
		_ = writeJSON(r.Cfg.Dir, "auto-switch.json", state)
		r.switchPause("自动切号结果待核查")
		return
	}
	state.Stage = "resuming"
	if writeJSON(r.Cfg.Dir, "auto-switch.json", state) != nil {
		r.switchPause("自动切号结果待核查")
		return
	}
	for _, taskID := range exhausted {
		state.ResumeClaims[taskID] = "reserved"
		if writeJSON(r.Cfg.Dir, "auto-switch.json", state) != nil {
			r.switchPause("继续任务的登记结果待核查")
			return
		}
		_, e := r.Store.Resume(taskID)
		if e != nil {
			state.Stage = "uncertain"
			state.ReviewRequired = true
			_ = writeJSON(r.Cfg.Dir, "auto-switch.json", state)
			r.switchPause("继续任务的结果待核查")
			return
		}
		state.ResumeClaims[taskID] = "queued"
		if writeJSON(r.Cfg.Dir, "auto-switch.json", state) != nil {
			r.switchPause("继续任务的结果待核查")
			return
		}
	}
	state.Stage = "completed"
	if writeJSON(r.Cfg.Dir, "auto-switch.json", state) != nil {
		r.switchPause("自动切号结果待核查")
		return
	}
	remaining := result.Remaining
	r.quotaSettings = settings
	r.quotaStatus = RunnerStatus{Account: "Desktop", Remaining: &remaining, CheckedAt: time.Now().UTC()}
	if writeJSON(r.Cfg.Dir, "runner-status.json", r.quotaStatus) != nil {
		r.quotaStatus.Paused = true
	}
}

func switchTrigger(tasks []*Task) string {
	var entries []string
	for _, t := range tasks {
		if t.Executor != "codex" || (t.Status != Queued && t.Status != QuotaExhausted) {
			continue
		}
		last := ""
		if len(t.Runs) > 0 {
			last = t.Runs[len(t.Runs)-1].Marker
		}
		entries = append(entries, fmt.Sprintf("%s\x00%s\x00%s", t.ID, t.Status, last))
	}
	sort.Strings(entries)
	digest := sha256.Sum256([]byte(strings.Join(entries, "\x01")))
	return hex.EncodeToString(digest[:])
}

// AcknowledgeAutoSwitch is deliberately exposed only to an explicit user CLI
// action. It keeps the cooldown and requires a new trigger before another try.
func AcknowledgeAutoSwitch(dir, home string) (SwitchStatus, error) {
	unlock, err := lockSettings(dir)
	if err != nil {
		return SwitchStatus{}, err
	}
	defer unlock()
	var result SwitchStatus
	err = (assistant.Reader{Home: home}).WithOrchestraRecoveryResolved(func() error {
		s, e := ReadSwitchStatus(dir)
		if e != nil {
			return e
		}
		if s.Stage != "uncertain" || !s.ReviewRequired {
			return fmt.Errorf("没有待确认的自动切号记录")
		}
		if s.HelperPreview != (os.Getenv("CODEX_HELPER_NATIVE_PREVIEW") == "1") {
			return fmt.Errorf("账号管理状态目录不匹配，无法确认")
		}
		tasks, e := (Store{Dir: dir}).List()
		if e != nil {
			return e
		}
		s.Trigger = switchTrigger(tasks)
		s.Stage = "acknowledged"
		s.ReviewRequired = false
		if e = writeJSON(dir, "auto-switch.json", s); e != nil {
			return e
		}
		result = s
		return nil
	})
	return result, err
}

// switchPause runs on every runner tick while a cooldown or review is pending,
// so it only rewrites runner-status.json when the pause state actually changes.
func (r *Runner) switchPause(reason string) {
	if r.quotaStatus.Paused && r.quotaStatus.Reason == reason {
		return
	}
	r.quotaStatus.Paused = true
	r.quotaStatus.Reason = reason
	_ = writeJSON(r.Cfg.Dir, "runner-status.json", r.quotaStatus)
}

package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type WarmupAccount struct {
	Key              string           `json:"key"`
	Name             string           `json:"name"`
	Status           string           `json:"status"`
	Message          string           `json:"message"`
	LastCheck        time.Time        `json:"last_check,omitempty"`
	LastAttempt      time.Time        `json:"last_attempt,omitempty"`
	NextCheck        time.Time        `json:"next_check,omitempty"`
	WindowReset      int64            `json:"window_reset,omitempty"`
	Attempts         int              `json:"attempts"`
	ConfirmedAttempt int              `json:"confirmed_attempt,omitempty"`
	Execution        *WarmupExecution `json:"execution,omitempty"`
	NoRetryBefore    time.Time        `json:"no_retry_before,omitempty"`
	IdleObserved     time.Time        `json:"idle_observed,omitempty"`
	ZeroSampleAt     time.Time        `json:"zero_sample_at,omitempty"`
	ZeroDeadline     int64            `json:"zero_deadline,omitempty"`
	LastProbe        time.Time        `json:"last_probe,omitempty"`
}

const warmupWindowRecheck = 2 * time.Minute

func isCachedWindowStatus(status string) bool {
	switch status {
	case "window_running", "confirmed", "window_running_uncertain_request", "probe_verified":
		return true
	default:
		return false
	}
}

func warmupWindowStatusStale(record *WarmupAccount, now time.Time) bool {
	return record.LastCheck.IsZero() || record.LastCheck.After(now) || now.Sub(record.LastCheck) >= warmupWindowRecheck
}

func warmupHasConfirmedRequest(record *WarmupAccount, now time.Time) bool {
	return record.Attempts > 0 && record.ConfirmedAttempt == record.Attempts && record.NoRetryBefore.After(now) &&
		record.Execution != nil && record.Execution.Submitted && record.Execution.Completed && record.Execution.IdentityVerified
}

type WarmupReport struct {
	Enabled         bool            `json:"enabled"`
	Message         string          `json:"message"`
	Accounts        []WarmupAccount `json:"accounts"`
	Available       bool            `json:"available"`
	Error           string          `json:"error,omitempty"`
	Schedule        WarmupSchedule  `json:"schedule"`
	ScheduleAllowed bool            `json:"schedule_allowed"`
	NextOpening     *time.Time      `json:"next_opening,omitempty"`
}
type warmupPolicy struct {
	Version  int             `json:"version"`
	Enabled  bool            `json:"enabled"`
	Allowed  map[string]bool `json:"allowed"`
	Schedule *WarmupSchedule `json:"schedule,omitempty"`
}
type warmupJournal struct {
	Version  int                       `json:"version"`
	Accounts map[string]*WarmupAccount `json:"accounts"`
	Bindings map[string]string         `json:"bindings,omitempty"`
}

func privateKey(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}
func credentialIdentity(c quotaCredential) string {
	claims, _ := decodeTokenClaims(c.access)
	return privateKey(claims.Auth.User + "\x00" + claims.Auth.Workspace)
}
func (r Reader) warmupRoot() string {
	return filepath.Join(r.helperStateRoot(), "warmup")
}

func readState[T any](path string, out *T) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return readJSON(path, out)
}
func writeState(path string, value any) error {
	root := filepath.Dir(path)
	if os.MkdirAll(root, 0700) != nil {
		return errors.New("不能创建预热状态目录")
	}
	if os.Chmod(root, 0700) != nil {
		return errors.New("不能保护预热状态目录")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("不能编码预热状态")
	}
	f, err := os.CreateTemp(root, ".state-")
	if err != nil {
		return errors.New("不能保存预热状态")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("不能同步预热状态")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("不能提交预热状态")
	}
	if dir, err := os.Open(root); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
func lockWarmup(path string) (func(), error) {
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return nil, errors.New("预热状态不可访问")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("不能锁定预热状态")
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = f.Close()
		return nil, errors.New("已有一次预热检查正在执行")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func (r Reader) readWarmupState() (warmupPolicy, warmupJournal, error) {
	p := warmupPolicy{Version: 1, Allowed: map[string]bool{}}
	j := warmupJournal{Version: 1, Accounts: map[string]*WarmupAccount{}}
	if readState(filepath.Join(r.warmupRoot(), "policy.json"), &p) != nil || readState(filepath.Join(r.warmupRoot(), "journal.json"), &j) != nil || p.Version != 1 || j.Version != 1 {
		return p, j, errors.New("预热状态损坏或版本不支持；已停止自动请求")
	}
	if p.Allowed == nil {
		p.Allowed = map[string]bool{}
	}
	if j.Accounts == nil {
		j.Accounts = map[string]*WarmupAccount{}
	}
	if j.Bindings == nil {
		j.Bindings = map[string]string{}
	}
	if err := scheduleOf(p).validate(); err != nil {
		return p, j, errors.New("预热时段设置无效，已停止自动请求")
	}
	return p, j, nil
}
func (r Reader) ConfigureWarmup(ctx context.Context, enabled bool) (WarmupReport, error) {
	unlock, err := lockWarmup(filepath.Join(r.warmupRoot(), "policy.lock"))
	if err != nil {
		return WarmupReport{}, err
	}
	defer unlock()
	p, j, err := r.readWarmupState()
	if err != nil {
		return WarmupReport{}, err
	}
	p.Enabled = enabled
	if enabled {
		accounts := r.automationAccounts(ctx)
		if len(accounts) == 0 {
			return WarmupReport{}, errors.New("没有可启用预热的账号")
		}
		p.Allowed = map[string]bool{}
		for _, a := range accounts {
			p.Allowed[privateKey(a.ID)] = true
		}
	}
	if err := writeState(filepath.Join(r.warmupRoot(), "policy.json"), p); err != nil {
		return WarmupReport{}, err
	}
	return r.warmupReport(p, j), nil
}
func (r Reader) WarmupStatus() WarmupReport {
	p, j, err := r.readWarmupState()
	if err != nil {
		return WarmupReport{Message: err.Error(), Accounts: []WarmupAccount{}}
	}
	return r.warmupReport(p, j)
}
func (r Reader) warmupReport(p warmupPolicy, j warmupJournal) WarmupReport {
	return r.warmupReportAt(p, j, time.Now())
}
func (r Reader) warmupReportAt(p warmupPolicy, j warmupJournal, now time.Time) WarmupReport {
	report := WarmupReport{Enabled: p.Enabled, Available: codexExecutable(r.Home) != "", Message: "自动预热已关闭", Accounts: []WarmupAccount{}}
	report.Schedule = scheduleOf(p)
	report.ScheduleAllowed = report.Schedule.permits(now)
	if !report.ScheduleAllowed {
		next := report.Schedule.nextOpening(now)
		report.NextOpening = &next
	}
	if p.Enabled {
		report.Message = "自动预热已开启；窗口运行时等待，账号空闲且窗口可用时触发"
		if !report.ScheduleAllowed {
			report.Message = "当前不在允许预热时段，等待下次开始时间"
		}
	}
	for _, a := range j.Accounts {
		account := *a
		if isCachedWindowStatus(account.Status) && warmupWindowStatusStale(&account, now) {
			account.Status = "window_check_pending"
			account.Message = "上次窗口状态已过期，等待重新查询"
			if warmupHasConfirmedRequest(&account, now) {
				account.Message = "本次预热请求已完成，当前窗口状态待重新查询"
			}
		}
		report.Accounts = append(report.Accounts, account)
	}
	sort.Slice(report.Accounts, func(i, j int) bool { return report.Accounts[i].Name < report.Accounts[j].Name })
	return report
}

func windowRunning(w QuotaWindow, now time.Time) bool {
	if w.ResetAfter != nil && *w.ResetAfter > 0 {
		// Prefer the server countdown to a potentially skewed local clock.
		return *w.ResetAfter < w.Seconds-2 || w.UsedPercent > 0
	}
	if w.ResetAt == nil || *w.ResetAt <= now.Unix() {
		return false
	}
	if w.UsedPercent > 0 {
		return true
	}
	return time.Unix(*w.ResetAt, 0).Sub(now) < time.Duration(w.Seconds)*time.Second-10*time.Second
}

func windowDeadline(w QuotaWindow, now time.Time) time.Time {
	if w.ResetAfter != nil && *w.ResetAfter > 0 {
		return now.Add(time.Duration(*w.ResetAfter) * time.Second)
	}
	if w.ResetAt != nil && *w.ResetAt > now.Unix() {
		return time.Unix(*w.ResetAt, 0)
	}
	return now.Add(time.Minute)
}

func validRunningWindow(w QuotaWindow, now time.Time) bool {
	if w.ResetAfter != nil {
		return *w.ResetAfter > 0 && *w.ResetAfter <= w.Seconds && windowRunning(w, now)
	}
	return w.ResetAt != nil && *w.ResetAt > now.Unix() && windowRunning(w, now)
}

// A zero-percent response may be rounded, and an unused window may report a
// reset that moves with the clock. Two samples can establish a fixed deadline;
// a single short countdown cannot prove the request opened a window.
func zeroUsageWindowStable(first QuotaWindow, firstAt time.Time, next QuotaWindow, nextAt time.Time) bool {
	elapsed := nextAt.Sub(firstAt)
	if first.Seconds != 18000 || next.Seconds != 18000 || first.UsedPercent != 0 || next.UsedPercent != 0 || elapsed < 2*time.Second || elapsed > time.Minute || !validRunningWindow(next, nextAt) {
		return false
	}
	firstKnown := (first.ResetAfter != nil && *first.ResetAfter > 0 && *first.ResetAfter <= first.Seconds) || (first.ResetAfter == nil && first.ResetAt != nil && *first.ResetAt > firstAt.Unix())
	nextKnown := (next.ResetAfter != nil && *next.ResetAfter > 0 && *next.ResetAfter <= next.Seconds) || (next.ResetAfter == nil && next.ResetAt != nil && *next.ResetAt > nextAt.Unix())
	if !firstKnown || !nextKnown {
		return false
	}
	delta := windowDeadline(next, nextAt).Sub(windowDeadline(first, firstAt))
	return delta >= -2*time.Second && delta <= 2*time.Second
}

func decideWarmup(q AccountQuota, record *WarmupAccount, now time.Time, probe bool) bool {
	previousCheck, previousStatus := record.LastCheck, record.Status
	record.LastCheck = now
	record.NextCheck = now.Add(time.Minute)
	if q.Status != "ready" {
		record.Status = "quota_unavailable"
		record.Message = q.Message
		record.NextCheck = now.Add(5 * time.Minute)
		return false
	}
	var five, week *QuotaWindow
	for i := range q.Windows {
		w := &q.Windows[i]
		if w.Seconds == 18000 {
			five = w
		}
		if w.Seconds == 604800 {
			week = w
		}
	}
	if five == nil || week == nil {
		record.Status = "window_unknown"
		record.Message = "未取得完整五小时／周窗口，不发送预热请求"
		record.NextCheck = now.Add(5 * time.Minute)
		return false
	}
	if week.UsedPercent >= 100 {
		record.Status = "weekly_exhausted"
		record.Message = "周额度已耗尽，等待恢复"
		record.NextCheck = now.Add(5 * time.Minute)
		return false
	}
	if q.LimitReached != nil && *q.LimitReached {
		record.Status = "account_limited"
		record.Message = "服务报告账号受限，等待恢复"
		record.NextCheck = now.Add(5 * time.Minute)
		return false
	}
	if probe {
		if !record.LastProbe.IsZero() && now.Sub(record.LastProbe) < 24*time.Hour {
			record.Status = "probe_already_run"
			record.Message = "该账号今天已登记一次真实测试，不重复发送"
			return false
		}
		return true
	}
	resetKnown := five.ResetAt != nil && *five.ResetAt > 0
	if five.ResetAfter != nil {
		resetKnown = *five.ResetAfter >= 0 && *five.ResetAfter <= five.Seconds
	}
	if !resetKnown {
		record.Status = "window_unknown"
		record.Message = "五小时窗口重置时间未知，等待完整数据；不发送预热请求"
		record.IdleObserved = time.Time{}
		record.NextCheck = now.Add(5 * time.Minute)
		return false
	}
	if windowRunning(*five, now) {
		deadline := windowDeadline(*five, now)
		if five.UsedPercent == 0 {
			// Zero may be rounded usage, or an idle window with a synthetic
			// sliding reset. A single short countdown cannot distinguish them.
			if record.ZeroSampleAt.IsZero() && previousStatus == "confirmed" && warmupHasConfirmedRequest(record, now) &&
				record.WindowReset > 0 && !previousCheck.IsZero() && now.Sub(previousCheck) >= 10*time.Second && now.Sub(previousCheck) <= 10*time.Minute {
				record.ZeroSampleAt, record.ZeroDeadline = previousCheck, record.WindowReset
			}
			elapsed := now.Sub(record.ZeroSampleAt)
			if record.ZeroSampleAt.IsZero() || elapsed < 0 || elapsed > 10*time.Minute {
				record.ZeroSampleAt, record.ZeroDeadline = now, deadline.Unix()
				record.IdleObserved = time.Time{}
				record.Status = "window_uncertain"
				record.Message = "额度为零，正在复核窗口倒计时"
				record.NextCheck = now.Add(10 * time.Second)
				return false
			}
			if elapsed < 10*time.Second {
				record.Status = "window_uncertain"
				record.Message = "额度为零，等待再次核对窗口倒计时"
				record.NextCheck = record.ZeroSampleAt.Add(10 * time.Second)
				return false
			}
			shift := deadline.Unix() - record.ZeroDeadline
			if shift >= -2 && shift <= 2 {
				record.ZeroSampleAt, record.ZeroDeadline = now, deadline.Unix()
			} else if shift >= int64(elapsed.Seconds())-2 && shift <= int64(elapsed.Seconds())+2 {
				// The endpoint is advancing with the clock, so the earlier
				// observation also serves as the first idle confirmation.
				if record.IdleObserved.IsZero() {
					record.IdleObserved = record.ZeroSampleAt
				}
				goto idle
			} else {
				record.ZeroSampleAt, record.ZeroDeadline = now, deadline.Unix()
				record.IdleObserved = time.Time{}
				record.Status = "window_uncertain"
				record.Message = "窗口重置时间不稳定，稍后重新核查"
				record.NextCheck = now.Add(time.Minute)
				return false
			}
		} else {
			record.ZeroSampleAt, record.ZeroDeadline = time.Time{}, 0
		}
		record.Status = "window_running"
		record.Message = "额度显示五小时窗口正在运行；这不代表本次预热已完成"
		if delta := deadline.Unix() - record.WindowReset; warmupHasConfirmedRequest(record, now) && record.WindowReset > 0 && delta >= -2 && delta <= 2 {
			record.Status = "confirmed"
			record.Message = "本次独立预热已完成，额度窗口仍在运行"
		}
		record.IdleObserved = time.Time{}
		record.WindowReset = deadline.Unix()
		record.NextCheck = now.Add(warmupWindowRecheck)
		if end := deadline.Add(10 * time.Second); end.Before(record.NextCheck) {
			record.NextCheck = end
		}
		return false
	}
	record.ZeroSampleAt, record.ZeroDeadline = time.Time{}, 0
	if five.UsedPercent > 0 {
		record.Status = "awaiting_reset"
		record.Message = "窗口数据尚未归零，等待服务更新"
		return false
	}
idle:
	if record.NoRetryBefore.After(now) {
		record.Status = "verification_pending"
		record.Message = "已登记可能发送的请求；等待窗口回查，不重复发送"
		return false
	}
	if record.IdleObserved.IsZero() || now.Sub(record.IdleObserved) > 2*time.Minute {
		record.IdleObserved = now
		record.Status = "confirming_idle"
		record.Message = "窗口似乎未启动，将再次查询确认"
		record.NextCheck = now.Add(10 * time.Second)
		return false
	}
	if now.Sub(record.IdleObserved) < 10*time.Second {
		record.Status = "confirming_idle"
		record.Message = "等待第二次窗口检查"
		return false
	}
	return true
}

type warmupDependencies struct {
	now      func() time.Time
	quota    func(context.Context, quotaCredential) AccountQuota
	run      func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error)
	busy     func([]string) bool
	activity func([]string) (bool, error)
}

func (d warmupDependencies) accountActivity(sources []string) (bool, error) {
	if d.activity != nil {
		return d.activity(sources)
	}
	if d.busy != nil {
		return d.busy(sources), nil
	}
	return false, errors.New("账号活动状态无法确认，暂缓预热")
}

func (r Reader) CheckWarmup(ctx context.Context, probeID string) (WarmupReport, error) {
	return r.checkWarmupMode(ctx, probeID, false, r.warmupDependencies())
}

// RefreshWarmup bypasses cached check times but never starts a model request.
// Existing request claims, cooldowns, and automatic policy remain unchanged.
func (r Reader) RefreshWarmup(ctx context.Context) (WarmupReport, error) {
	return r.checkWarmupMode(ctx, "", true, r.warmupDependencies())
}

func (r Reader) warmupDependencies() warmupDependencies {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return warmupDependencies{now: time.Now, quota: func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) }, run: runIsolatedWarmup, activity: func(s []string) (bool, error) { return r.activityStatus(s, time.Now()) }}
}
func (r Reader) checkWarmup(ctx context.Context, probeID string, d warmupDependencies) (WarmupReport, error) {
	return r.checkWarmupMode(ctx, probeID, false, d)
}
func (r Reader) checkWarmupMode(ctx context.Context, probeID string, refreshOnly bool, d warmupDependencies) (WarmupReport, error) {
	if r.nativeAccountPreview() {
		if _, err := os.Lstat(filepath.Join(r.helperStateRoot(), "accounts", "pending.json")); !os.IsNotExist(err) {
			report := r.WarmupStatus()
			report.Message = "账号切换或恢复核查期间暂停预热"
			return report, nil
		}
	}
	unlock, err := lockWarmup(filepath.Join(r.warmupRoot(), "execution.lock"))
	if err != nil {
		return r.WarmupStatus(), err
	}
	defer unlock()
	p, j, err := r.readWarmupState()
	if err != nil {
		return WarmupReport{}, err
	}
	if !p.Enabled && probeID == "" && !refreshOnly {
		return r.warmupReportAt(p, j, d.now()), nil
	}
	schedule := scheduleOf(p)
	if probeID == "" && !refreshOnly && !schedule.permits(d.now()) {
		report := r.warmupReportAt(p, j, d.now())
		report.ScheduleAllowed = false
		next := schedule.nextOpening(d.now())
		report.NextOpening = &next
		report.Message = "当前不在允许预热时段，等待下次开始时间"
		return report, nil
	}
	accounts := r.automationAccounts(ctx)
	credentials := r.warmupCredentials(ctx, d.now())
	seen := map[string]bool{}
	probeFound := false
	for _, account := range accounts {
		if ctx.Err() != nil {
			break
		}
		if probeID != "" && account.ID != probeID {
			continue
		}
		if probeID != "" {
			probeFound = true
		}
		if probeID == "" && !p.Allowed[privateKey(account.ID)] {
			continue
		}
		if probeID == "" && !refreshOnly {
			fresh, _, e := r.readWarmupState()
			if e != nil {
				return r.warmupReportAt(p, j, d.now()), e
			}
			if !fresh.Enabled || !scheduleOf(fresh).permits(d.now()) {
				break
			}
		}
		localKey := privateKey(account.ID)
		key := localKey
		credential, ok := credentials[account.ID]
		if ok {
			key = credentialIdentity(credential)
			j.Bindings[localKey] = key
			if prior := j.Accounts[localKey]; prior != nil && key != localKey {
				if j.Accounts[key] == nil {
					prior.Key = key
					j.Accounts[key] = prior
				}
				delete(j.Accounts, localKey)
			}
		} else if prior := j.Bindings[localKey]; prior != "" {
			key = prior
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		record := j.Accounts[key]
		if record == nil {
			record = &WarmupAccount{Key: key, Name: account.Name}
			j.Accounts[key] = record
		}
		record.Name = account.Name
		// A newly available credential should clear an old missing-credential
		// status on the next scheduler tick, without waiting out its backoff.
		staleWindow := isCachedWindowStatus(record.Status) && warmupWindowStatusStale(record, d.now())
		if probeID == "" && !refreshOnly && record.NextCheck.After(d.now()) && !(ok && record.Status == "credential_unavailable") && !staleWindow {
			continue
		}
		if !ok {
			record.LastCheck = d.now()
			record.Status = "credential_unavailable"
			record.Message = "等待该账号的有效访问凭证"
			if r.nativeAccountPreview() {
				bindings, err := r.readNativeBindings()
				if err != nil {
					record.Message = errNativeBindings.Error()
				} else if bindings.reapplyBlocked()[account.ID] {
					record.Message = errNativeReapply.Error()
				}
			}
			record.NextCheck = d.now().Add(5 * time.Minute)
			continue
		}
		q := d.quota(ctx, credential)
		if !decideWarmup(q, record, d.now(), probeID != "") {
			continue
		}
		if probeID == "" {
			sources, activityErr := r.warmupActivitySources(account.ID, credential.sources)
			busy := false
			if activityErr == nil {
				busy, activityErr = d.accountActivity(sources)
			}
			if activityErr != nil || busy {
				record.Status, record.Message = "account_busy", "对应账号目录近期有任务活动，稍后再检查"
				if activityErr != nil {
					record.Status, record.Message = "activity_unknown", activityErr.Error()
				}
				record.NextCheck = d.now().Add(time.Minute)
				continue
			}
		}
		if refreshOnly {
			record.Status = "ready_to_warmup"
			record.Message = "已重新查询：窗口未启动；本次仅检查，后续按自动策略调度"
			record.NextCheck = d.now().Add(time.Minute)
			continue
		}
		if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
			return r.warmupReportAt(p, j, d.now()), err
		}
		execution, runErr := d.run(ctx, codexExecutable(r.Home), credential, func() error {
			if r.nativeAccountPreview() {
				if _, err := os.Lstat(filepath.Join(r.helperStateRoot(), "accounts", "pending.json")); !os.IsNotExist(err) {
					return errors.New("账号操作待核查，本次预热取消")
				}
			}
			freshPolicy, _, e := r.readWarmupState()
			if e != nil {
				return e
			}
			if probeID == "" && (!freshPolicy.Enabled || !freshPolicy.Allowed[privateKey(account.ID)]) {
				return errors.New("用户已关闭该账号预热")
			}
			if probeID == "" && !scheduleOf(freshPolicy).permits(d.now()) {
				return errors.New("已超出允许预热时段，本次请求取消")
			}
			fresh, exists := r.warmupCredentials(ctx, d.now())[account.ID]
			if !exists || fresh.access != credential.access {
				return errors.New("凭证已变化，本次预热取消")
			}
			if probeID == "" {
				if !decideWarmup(d.quota(ctx, fresh), record, d.now(), false) {
					return errors.New("窗口状态已变化，取消本次请求")
				}
				sources, activityErr := r.warmupActivitySources(account.ID, fresh.sources)
				busy := false
				if activityErr == nil {
					busy, activityErr = d.accountActivity(sources)
				}
				if activityErr != nil {
					record.Status = "activity_unknown"
					return activityErr
				}
				if busy {
					record.Status = "account_busy"
					return errors.New("账号任务活动发生变化，取消本次请求")
				}
			}
			// The final quota/activity checks can cross the end boundary or a
			// user may edit the schedule while they run. Read policy again at
			// the last possible point before recording a billable request.
			if probeID == "" {
				latest, _, e := r.readWarmupState()
				if e != nil {
					return e
				}
				if !latest.Enabled || !latest.Allowed[privateKey(account.ID)] || !scheduleOf(latest).permits(d.now()) {
					return errors.New("预热开关或允许时段已改变，本次请求取消")
				}
			}
			record.LastAttempt = d.now()
			record.NoRetryBefore = d.now().Add(5*time.Hour + 30*time.Second)
			record.Attempts++
			record.Status = "submitted"
			record.Message = "请求已登记，正在执行独立预热"
			record.NextCheck = d.now().Add(time.Minute)
			if probeID != "" {
				record.LastProbe = d.now()
			}
			return writeState(filepath.Join(r.warmupRoot(), "journal.json"), j)
		})
		record.Execution = &execution
		if !execution.Submitted && runErr != nil {
			if record.Status != "window_running" && record.Status != "account_busy" && record.Status != "activity_unknown" && record.Status != "window_unknown" {
				record.Status = "setup_failed"
			}
			record.Message = runErr.Error()
			record.NextCheck = d.now().Add(5 * time.Minute)
			continue
		}
		record.Status = "verification_pending"
		record.Message = "请求是否成功不能仅凭进程判断，正在回查窗口"
		record.NextCheck = d.now().Add(time.Minute)
		if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
			return r.warmupReportAt(p, j, d.now()), err
		}
		verified := false
		var zeroSample *QuotaWindow
		var zeroSampleAt time.Time
		for tries := 0; tries < 3; tries++ {
			if tries > 0 {
				select {
				case <-ctx.Done():
					break
				case <-time.After(3 * time.Second):
				}
			}
			after := d.quota(ctx, credential)
			observedAt := d.now()
			for _, w := range after.Windows {
				if after.Status != "ready" || w.Seconds != 18000 {
					continue
				}
				if w.UsedPercent > 0 && validRunningWindow(w, observedAt) {
					verified = true
				} else if w.UsedPercent == 0 {
					if zeroSample != nil && zeroUsageWindowStable(*zeroSample, zeroSampleAt, w, observedAt) {
						verified = true
					} else if zeroSample == nil {
						sample := w
						zeroSample, zeroSampleAt = &sample, observedAt
					}
				}
				if verified {
					deadline := windowDeadline(w, observedAt)
					record.WindowReset = deadline.Unix()
					record.LastCheck = observedAt
					record.NextCheck = observedAt.Add(warmupWindowRecheck)
					if end := deadline.Add(10 * time.Second); end.Before(record.NextCheck) {
						record.NextCheck = end
					}
				}
			}
			if verified || ctx.Err() != nil {
				break
			}
		}
		completed := execution.Submitted && execution.Completed && execution.IdentityVerified && runErr == nil
		if verified && completed {
			record.Status = "confirmed"
			record.ConfirmedAttempt = record.Attempts
			record.Message = "独立请求正常完成，额度回查显示五小时窗口运行"
		}
		if verified && !completed {
			record.Status = "window_running_uncertain_request"
			record.Message = "额度窗口已运行，但本次预热请求完成状态未确认；不会重复发送"
		}
		if !verified {
			record.Message = "已登记一次请求，窗口尚未确认；后续仅回查，不重复请求"
		}
		if probeID != "" && verified && completed {
			record.Status = "probe_verified"
			record.Message = "独立真实请求测试完成并回查；不代表经历了窗口重置"
			record.NextCheck = d.now().Add(time.Minute)
		}
		if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
			return r.warmupReportAt(p, j, d.now()), err
		}
	}
	if probeID != "" && !probeFound {
		return r.warmupReportAt(p, j, d.now()), errors.New("指定账号不在当前账号列表中")
	}
	if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
		return r.warmupReportAt(p, j, d.now()), err
	}
	latest, _, e := r.readWarmupState()
	if e == nil {
		p = latest
	}
	report := r.warmupReportAt(p, j, d.now())
	if refreshOnly {
		report.Message = "已重新查询额度；本次仅检查，不发送预热请求"
	}
	return report, nil
}

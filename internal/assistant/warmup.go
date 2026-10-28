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
	Key           string           `json:"key"`
	Name          string           `json:"name"`
	Status        string           `json:"status"`
	Message       string           `json:"message"`
	LastCheck     time.Time        `json:"last_check,omitempty"`
	LastAttempt   time.Time        `json:"last_attempt,omitempty"`
	NextCheck     time.Time        `json:"next_check,omitempty"`
	WindowReset   int64            `json:"window_reset,omitempty"`
	Attempts      int              `json:"attempts"`
	Execution     *WarmupExecution `json:"execution,omitempty"`
	NoRetryBefore time.Time        `json:"no_retry_before,omitempty"`
	IdleObserved  time.Time        `json:"idle_observed,omitempty"`
	LastProbe     time.Time        `json:"last_probe,omitempty"`
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
	return filepath.Join(r.Home, "Library/Application Support/CodexHelper/warmup")
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
		accounts, _ := r.accounts(ctx, filepath.Join(r.Home, ".cc-switch"), "", nil)
		if len(accounts) == 0 {
			return WarmupReport{}, errors.New("没有可启用预热的 CC Switch 账号")
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
	report := WarmupReport{Enabled: p.Enabled, Available: codexExecutable(r.Home) != "", Message: "自动预热已关闭", Accounts: []WarmupAccount{}}
	report.Schedule = scheduleOf(p)
	report.ScheduleAllowed = report.Schedule.permits(time.Now())
	if !report.ScheduleAllowed {
		next := report.Schedule.nextOpening(time.Now())
		report.NextOpening = &next
	}
	if p.Enabled {
		report.Message = "自动预热已开启；窗口运行时等待，账号空闲且窗口可用时触发"
		if !report.ScheduleAllowed {
			report.Message = "当前不在允许预热时段，等待下次开始时间"
		}
	}
	for _, a := range j.Accounts {
		report.Accounts = append(report.Accounts, *a)
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

func decideWarmup(q AccountQuota, record *WarmupAccount, now time.Time, probe bool) bool {
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
	if windowRunning(*five, now) {
		record.Status = "window_running"
		record.Message = "五小时窗口已运行，无需额外预热"
		record.IdleObserved = time.Time{}
		deadline := windowDeadline(*five, now)
		record.WindowReset = deadline.Unix()
		record.NextCheck = deadline.Add(10 * time.Second)
		return false
	}
	if five.UsedPercent > 0 {
		record.Status = "awaiting_reset"
		record.Message = "窗口数据尚未归零，等待服务更新"
		return false
	}
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
	now   func() time.Time
	quota func(context.Context, quotaCredential) AccountQuota
	run   func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error)
	busy  func([]string) bool
}

func (r Reader) CheckWarmup(ctx context.Context, probeID string) (WarmupReport, error) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return r.checkWarmup(ctx, probeID, warmupDependencies{now: time.Now, quota: func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) }, run: runIsolatedWarmup, busy: func(s []string) bool { return r.recentActivity(s, time.Now()) }})
}
func (r Reader) checkWarmup(ctx context.Context, probeID string, d warmupDependencies) (WarmupReport, error) {
	if _, err := os.Lstat(r.nativeTransactionPath()); !os.IsNotExist(err) {
		report := r.WarmupStatus()
		report.Message = "双端账号交换或恢复核查期间暂停预热"
		return report, nil
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
	if !p.Enabled && probeID == "" {
		return r.warmupReport(p, j), nil
	}
	schedule := scheduleOf(p)
	if probeID == "" && !schedule.permits(d.now()) {
		report := r.warmupReport(p, j)
		report.ScheduleAllowed = false
		next := schedule.nextOpening(d.now())
		report.NextOpening = &next
		report.Message = "当前不在允许预热时段，等待下次开始时间"
		return report, nil
	}
	accounts, _ := r.accounts(ctx, filepath.Join(r.Home, ".cc-switch"), "", nil)
	credentials := r.quotaCredentials(d.now())
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
		if probeID == "" {
			fresh, _, e := r.readWarmupState()
			if e != nil {
				return r.warmupReport(p, j), e
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
		if probeID == "" && record.NextCheck.After(d.now()) && !(ok && record.Status == "credential_unavailable") {
			continue
		}
		if !ok {
			record.Status = "credential_unavailable"
			record.Message = "等待 CC Switch 提供该账号的有效访问凭证"
			record.NextCheck = d.now().Add(5 * time.Minute)
			continue
		}
		q := d.quota(ctx, credential)
		if !decideWarmup(q, record, d.now(), probeID != "") {
			continue
		}
		if probeID == "" && d.busy(credential.sources) {
			record.Status = "account_busy"
			record.Message = "对应账号目录近期有任务活动，稍后再检查"
			record.NextCheck = d.now().Add(time.Minute)
			continue
		}
		if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
			return r.warmupReport(p, j), err
		}
		execution, runErr := d.run(ctx, codexExecutable(r.Home), credential, func() error {
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
			fresh, exists := r.quotaCredentials(d.now())[account.ID]
			if !exists || fresh.access != credential.access {
				return errors.New("凭证已变化，本次预热取消")
			}
			if probeID == "" {
				if !decideWarmup(d.quota(ctx, fresh), record, d.now(), false) {
					return errors.New("窗口状态已变化，取消本次请求")
				}
				if d.busy(fresh.sources) {
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
			if record.Status != "window_running" && record.Status != "account_busy" {
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
			return r.warmupReport(p, j), err
		}
		verified := false
		for tries := 0; tries < 3; tries++ {
			if tries > 0 {
				select {
				case <-ctx.Done():
					break
				case <-time.After(3 * time.Second):
				}
			}
			after := d.quota(ctx, credential)
			for _, w := range after.Windows {
				if after.Status == "ready" && w.Seconds == 18000 && windowRunning(w, d.now()) {
					verified = true
					deadline := windowDeadline(w, d.now())
					record.WindowReset = deadline.Unix()
					record.NextCheck = deadline.Add(10 * time.Second)
				}
			}
			if verified || ctx.Err() != nil {
				break
			}
		}
		if verified && execution.Completed {
			record.Status = "confirmed"
			record.Message = "独立请求完成，已回查确认五小时窗口运行"
		}
		if verified && !execution.Completed {
			record.Status = "window_running_uncertain_request"
			record.Message = "窗口已运行，但本次请求完成状态不明确；不会重复发送"
		}
		if !verified {
			record.Message = "已登记一次请求，窗口尚未确认；后续仅回查，不重复请求"
		}
		if probeID != "" && verified {
			record.Status = "probe_verified"
			record.Message = "独立真实请求测试完成并回查；不代表经历了窗口重置"
			record.NextCheck = d.now().Add(time.Minute)
		}
		if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
			return r.warmupReport(p, j), err
		}
	}
	if probeID != "" && !probeFound {
		return r.warmupReport(p, j), errors.New("指定账号不在当前 CC Switch 账号列表中")
	}
	if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
		return r.warmupReport(p, j), err
	}
	latest, _, e := r.readWarmupState()
	if e == nil {
		p = latest
	}
	return r.warmupReport(p, j), nil
}

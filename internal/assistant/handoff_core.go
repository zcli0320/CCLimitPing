package assistant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HandoffState is a journal of one explicitly authorized handoff. Account IDs
// must be opaque identifiers, not credentials. Never put tokens or chat text
// into this structure. A pending action means its external result is unknown.
type HandoffState struct {
	Version         int     `json:"version"`
	Enabled         bool    `json:"enabled"`
	AuthorizationID string  `json:"authorization_id,omitempty"`
	Scope           string  `json:"scope"`
	SessionID       string  `json:"session_id"`
	TurnID          string  `json:"turn_id,omitempty"`
	SourceAccount   string  `json:"source_account,omitempty"`
	TargetAccount   string  `json:"target_account,omitempty"`
	Directory       string  `json:"directory,omitempty"`
	Model           string  `json:"model,omitempty"`
	Effort          string  `json:"effort,omitempty"`
	Permissions     string  `json:"permissions,omitempty"`
	Threshold       float64 `json:"threshold"`
	Stage           string  `json:"stage"`
	Message         string  `json:"message"`
	PendingAction   string  `json:"pending_action,omitempty"`
	InterruptSent   bool    `json:"interrupt_sent,omitempty"`
	ActionAt        int64   `json:"action_at,omitempty"`
	LastStage       string  `json:"last_stage,omitempty"`
}

type HandoffAction string

// Safe operation errors contain fixed, local diagnostics only. Backend and
// network error text is never copied into persistent UI state.
type handoffSafeError string

func (e handoffSafeError) Error() string { return string(e) }

const (
	HandoffRequestStop HandoffAction = "request_stop"
	HandoffInterrupt   HandoffAction = "interrupt"
	HandoffExit        HandoffAction = "exit"
	HandoffSwitch      HandoffAction = "switch"
	HandoffRestart     HandoffAction = "restart"
	HandoffContinue    HandoffAction = "continue"
)

func handoffActionLabel(action HandoffAction) string {
	switch action {
	case HandoffRequestStop:
		return "收尾请求"
	case HandoffInterrupt:
		return "轮次中断"
	case HandoffExit:
		return "原实例退出"
	case HandoffSwitch:
		return "认证切换"
	case HandoffRestart:
		return "新实例启动"
	case HandoffContinue:
		return "继续请求"
	default:
		return "交接操作"
	}
}

// HandoffObservation contains affirmative evidence, not guesses from a file
// timestamp or from the display title. False includes unknown. Observation must
// be read-only; the adapter owns the meaning and provenance of every flag.
type HandoffObservation struct {
	Warning               string
	TargetMatches         bool
	Running               bool
	AlreadyLimited        bool
	UserIntervened        bool
	FinishedNormally      bool
	TurnID                string
	SourceAccount         string
	TargetAccount         string
	Directory             string
	Model                 string
	Effort                string
	Permissions           string
	CurrentQuotaKnown     bool
	CurrentRemaining      float64
	TargetQuotaKnown      bool
	TargetRemaining       float64
	TargetWeeklyUsable    bool
	TargetCredentialReady bool
	StopConfirmed         bool
	StoppedForHandoff     bool
	StopTimedOut          bool
	OtherSessionsIdle     bool
	OldExited             bool
	ExitTimedOut          bool
	SwitchVerified        bool
	RestartReady          bool
	IdentityVerified      bool
	SettingsVerified      bool
	ContinueStarted       bool
}

// Implementations must refuse an action unless its exact target and evidence
// can be established. Execute errors are deliberately treated as ambiguous.
// This interface alone does not provide a live Desktop or CLI adapter.
type HandoffOperations interface {
	Observe(context.Context, HandoffState) (HandoffObservation, error)
	Execute(context.Context, HandoffAction, HandoffState) error
}

func (r Reader) handoffPath() string {
	return filepath.Join(r.helperStateRoot(), "handoff", "state.json")
}

func (r Reader) handoffCancelPath() string {
	return filepath.Join(filepath.Dir(r.handoffPath()), "cancel.json")
}

func (r Reader) handoffCancelled(s HandoffState) bool {
	if s.AuthorizationID == "" {
		return false
	}
	var request struct {
		AuthorizationID string `json:"authorization_id"`
	}
	return readJSON(r.handoffCancelPath(), &request) == nil && request.AuthorizationID == s.AuthorizationID
}

func (r Reader) requestHandoffCancellation() (HandoffState, error) {
	s := r.HandoffStatus()
	if !s.Enabled || s.AuthorizationID == "" {
		return s, nil
	}
	return s, writeState(r.handoffCancelPath(), map[string]string{"authorization_id": s.AuthorizationID})
}

func validHandoffText(s string) bool {
	return s != "" && len(s) <= 4096 && !strings.ContainsRune(s, '\x00')
}
func validHandoffPercent(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100
}
func validHandoffThreshold(v float64) bool {
	return validHandoffPercent(v) && v > 0 && v < 100
}

func (r Reader) HandoffStatus() HandoffState {
	s := HandoffState{Version: 1, Threshold: 10, Stage: "off", Message: "自动切号未开启"}
	if readState(r.handoffPath(), &s) != nil || s.Version != 1 || !validHandoffThreshold(s.Threshold) {
		return HandoffState{Version: 1, Stage: "error", Message: "交接状态不可读取，已停止"}
	}
	return s
}

// ConfigureHandoff changes authorization only. This does not start a task or
// contact either account. An unresolved external action cannot be discarded by
// merely enabling the feature again.
func (r Reader) ConfigureHandoff(scope, sessionID string, threshold float64, enabled bool) (HandoffState, error) {
	// A cancellation marker is written before trying the coordinator lock, so
	// a user changing targets does not wait for a network request to finish.
	if !enabled {
		if _, err := r.requestHandoffCancellation(); err != nil {
			return r.HandoffStatus(), err
		}
	}
	unlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		if !enabled {
			s := r.HandoffStatus()
			s.Message = "停止请求已登记，正在等待当前步骤结束"
			return s, nil
		}
		return r.HandoffStatus(), err
	}
	defer unlock()
	s := r.HandoffStatus()
	if s.Stage == "error" {
		return s, errors.New("交接状态损坏，请检查后处理")
	}
	if !enabled {
		s.Enabled = false
		// Once any external action has been requested, disabling automation
		// cannot establish that the task or client is back in a normal state.
		// Preserve the journal and require inspection before a fresh handoff.
		_, txnErr := os.Lstat(r.handoffAuthTransactionPath())
		_, ccErr := os.Lstat(r.ccSwitchTransactionPath())
		if os.IsNotExist(txnErr) && os.IsNotExist(ccErr) && !r.nativeAccountPending() && s.PendingAction == "" && (s.Stage == "off" || s.Stage == "watching" || s.Stage == "ready_stop" || s.Stage == "completed" || s.Stage == "cancelled") {
			s.Stage, s.Message = "off", "自动切号已关闭"
		} else {
			s.LastStage = s.Stage
			s.Stage, s.Message = "uncertain", "动作结果未确认，请先核查"
		}
		return s, writeState(r.handoffPath(), s)
	}
	if s.PendingAction != "" || s.Stage == "uncertain" {
		return s, errors.New("上次交接结果未确认，请先核查")
	}
	if r.nativeAccountPending() {
		return s, errors.New("账号切换有待核查记录，请先在账号管理中核查")
	}
	// Never discard the coordinates of an unfinished credential transaction.
	// Even a completed handoff can have a recovery copy awaiting cleanup.
	if _, err := os.Lstat(r.handoffAuthTransactionPath()); err == nil || !os.IsNotExist(err) {
		return s, errors.New("仍有认证恢复记录，请先核查账号并解除暂停")
	}
	if _, err := os.Lstat(r.ccSwitchTransactionPath()); !os.IsNotExist(err) {
		return s, errors.New("仍有 CC Switch 切换恢复记录，请先核查")
	}
	if (scope != "desktop" && scope != "cli") || !validHandoffText(sessionID) || !validHandoffThreshold(threshold) {
		return s, errors.New("会话或阈值无效")
	}
	// Reconfiguration always revokes previous in-flight authority.
	if s.Enabled && s.Stage != "watching" && s.Stage != "completed" && s.Stage != "off" {
		return s, errors.New("交接进行中，请先关闭并核查任务")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return s, errors.New("无法创建交接授权")
	}
	s = HandoffState{Version: 1, Enabled: true, AuthorizationID: hex.EncodeToString(nonce[:]), Scope: scope, SessionID: sessionID, Threshold: threshold, Stage: "watching", Message: "等待所选会话触发交接"}
	return s, writeState(r.handoffPath(), s)
}

func handoffBackupReady(o HandoffObservation, threshold float64) bool {
	return o.TargetQuotaKnown && validHandoffPercent(o.TargetRemaining) && o.TargetRemaining > threshold && o.TargetWeeklyUsable && o.TargetCredentialReady
}

func handoffFrozenEvidence(o HandoffObservation, s HandoffState) bool {
	return o.TargetMatches && o.TurnID == s.TurnID && o.SourceAccount == s.SourceAccount && o.TargetAccount == s.TargetAccount
}

// AdvanceHandoff performs at most one externally visible action. The caller
// must explicitly supply the adapter; no implicit production adapter exists.
// A state lock spans an action, so disabling/rebinding cannot race its commit.
// Observation occurs outside that lock, and the journal is re-read before use.
func (r Reader) AdvanceHandoff(ctx context.Context, ops HandoffOperations) (HandoffState, error) {
	if ops == nil {
		return r.HandoffStatus(), errors.New("交接适配器不可用")
	}
	unlockExec, err := lockWarmup(r.handoffPath() + ".execution.lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer unlockExec()
	before := r.HandoffStatus()
	if !before.Enabled || before.Stage == "error" || before.Stage == "completed" || before.Stage == "uncertain" {
		return before, nil
	}
	if before.PendingAction != "" {
		// After a crash, do not repeat an action whose outcome was never saved.
		return r.handoffMarkUncertain(before)
	}
	o, observeErr := ops.Observe(ctx, before)
	unlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer unlock()
	s := r.HandoffStatus()
	if s != before {
		return s, nil
	}
	cancel := func() (HandoffState, error) {
		s.Enabled = false
		if s.Stage == "watching" || s.Stage == "ready_stop" {
			s.Stage, s.Message = "off", "自动切号已关闭"
		} else {
			s.Stage, s.Message = "uncertain", "交接已停止，请检查当前会话和账号状态"
		}
		return s, writeState(r.handoffPath(), s)
	}
	if r.handoffCancelled(s) {
		return cancel()
	}
	if observeErr != nil {
		message := "暂时无法读取原会话状态，等待下一次检查"
		if s.Message != message {
			s.Message = message
			return s, writeState(r.handoffPath(), s)
		}
		return s, nil
	}
	if o.UserIntervened || o.FinishedNormally {
		s.Enabled = false
		if s.Stage == "watching" || s.Stage == "ready_stop" {
			s.Stage, s.Message = "cancelled", "任务已结束或用户已介入，交接取消"
		} else {
			s.Stage, s.Message = "uncertain", "交接期间任务结束或用户已介入，请核查当前状态"
		}
		return s, writeState(r.handoffPath(), s)
	}
	persist := func(stage, message string) (HandoffState, error) {
		s.Stage, s.Message = stage, message
		return s, writeState(r.handoffPath(), s)
	}
	execute := func(action HandoffAction, next, message string) (HandoffState, error) {
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
		if r.handoffCancelled(s) {
			return cancel()
		}
		previousStage := s.Stage
		s.PendingAction, s.Stage, s.Message = string(action), "pending", handoffActionLabel(action)+"已登记，等待确认"
		if e := writeState(r.handoffPath(), s); e != nil {
			return s, e
		}
		if r.handoffCancelled(s) {
			s.PendingAction = ""
			s.Stage = previousStage
			return cancel()
		}
		if e := ops.Execute(ctx, action, s); e != nil {
			s.Enabled, s.Stage, s.Message = false, "uncertain", handoffActionLabel(action)+"结果未确认，请核查原会话和运行状态"
			var safe handoffSafeError
			if errors.As(e, &safe) {
				s.Message += "（" + safe.Error() + "）"
			}
			if e2 := writeState(r.handoffPath(), s); e2 != nil {
				return s, e2
			}
			return s, nil
		}
		s.PendingAction = ""
		if r.handoffCancelled(s) {
			s.Enabled = false
			s.Stage, s.Message = "uncertain", "交接期间收到停止请求，请检查当前会话和账号状态"
			return s, writeState(r.handoffPath(), s)
		}
		if action == HandoffInterrupt {
			s.InterruptSent = true
		}
		if action == HandoffRequestStop || action == HandoffExit {
			s.ActionAt = time.Now().Unix()
		}
		return persist(next, message)
	}
	switch s.Stage {
	case "watching":
		if o.Warning != "" {
			if s.Message != o.Warning {
				return persist("watching", o.Warning)
			}
			return s, nil
		}
		if !o.TargetMatches || (!o.Running && !(o.AlreadyLimited && o.StopConfirmed && o.StoppedForHandoff && o.OtherSessionsIdle)) || !o.CurrentQuotaKnown || !validHandoffPercent(o.CurrentRemaining) || o.CurrentRemaining >= s.Threshold {
			return s, nil
		}
		if !handoffBackupReady(o, s.Threshold) {
			return persist("watching", "备用账号认证或额度不可用；当前任务继续")
		}
		if !validHandoffText(o.TurnID) || !validHandoffText(o.SourceAccount) || !validHandoffText(o.TargetAccount) || o.SourceAccount == o.TargetAccount || !validHandoffText(o.Directory) || !validHandoffText(o.Model) || !validHandoffText(o.Effort) || !validHandoffText(o.Permissions) {
			return persist("watching", "无法确认会话、账号或执行设置；当前任务继续")
		}
		s.TurnID, s.SourceAccount, s.TargetAccount = o.TurnID, o.SourceAccount, o.TargetAccount
		s.Directory, s.Model, s.Effort, s.Permissions = o.Directory, o.Model, o.Effort, o.Permissions
		if o.AlreadyLimited {
			return persist("ready_exit", "原任务已因额度停止，准备退出")
		}
		return persist("ready_stop", "已确认备用账号，准备安全收尾")
	case "ready_stop":
		if handoffFrozenEvidence(o, s) && o.AlreadyLimited && o.StopConfirmed && o.StoppedForHandoff && o.OtherSessionsIdle && o.Directory == s.Directory && o.Model == s.Model && o.Effort == s.Effort && o.Permissions == s.Permissions && o.CurrentQuotaKnown && validHandoffPercent(o.CurrentRemaining) && o.CurrentRemaining < s.Threshold && handoffBackupReady(o, s.Threshold) {
			return persist("ready_exit", "原任务已因额度停止，准备退出")
		}
		if !handoffFrozenEvidence(o, s) || !o.Running || o.Directory != s.Directory || o.Model != s.Model || o.Effort != s.Effort || o.Permissions != s.Permissions || !o.CurrentQuotaKnown || !validHandoffPercent(o.CurrentRemaining) || o.CurrentRemaining >= s.Threshold || !handoffBackupReady(o, s.Threshold) {
			return persist("watching", "交接条件变化，当前任务继续")
		}
		return execute(HandoffRequestStop, "waiting_stop", "已请求收尾，等待原任务停稳")
	case "waiting_stop":
		if !handoffFrozenEvidence(o, s) {
			return s, nil
		}
		if o.StopConfirmed && o.StoppedForHandoff && o.OtherSessionsIdle {
			return persist("ready_exit", "原任务已停稳，准备退出")
		}
		if o.StopConfirmed && o.StoppedForHandoff && !o.OtherSessionsIdle {
			return persist("waiting_stop", "等待后台终端及其他会话结束；不会自动终止它们")
		}
		if !o.StopConfirmed && o.StopTimedOut && !s.InterruptSent && handoffBackupReady(o, s.Threshold) {
			return execute(HandoffInterrupt, "waiting_stop", "已请求中断准确轮次，等待停稳")
		}
	case "ready_exit":
		if handoffFrozenEvidence(o, s) && o.StopConfirmed && o.StoppedForHandoff && !o.OtherSessionsIdle {
			return persist("waiting_stop", "等待后台终端及其他会话结束；不会自动终止它们")
		}
		if !handoffFrozenEvidence(o, s) || !o.StopConfirmed || !o.StoppedForHandoff || !o.OtherSessionsIdle || !handoffBackupReady(o, s.Threshold) {
			return s, nil
		}
		return execute(HandoffExit, "waiting_exit", "已请求正常退出，等待确认")
	case "waiting_exit":
		if o.OldExited {
			return persist("ready_switch", "旧实例已退出，准备切换认证")
		}
		if o.ExitTimedOut {
			s.Enabled = false
			return persist("uncertain", "托管后端尚未退出，已暂停切号；请等待并核查原会话")
		}
	case "ready_switch":
		if !o.OldExited || !handoffBackupReady(o, s.Threshold) || o.TargetAccount != s.TargetAccount {
			return s, nil
		}
		return execute(HandoffSwitch, "waiting_switch", "已请求切换认证，等待确认")
	case "waiting_switch":
		if o.SwitchVerified && o.TargetAccount == s.TargetAccount {
			return persist("ready_restart", "认证切换已确认，准备启动")
		}
	case "ready_restart":
		if !o.OldExited || !o.SwitchVerified || o.TargetAccount != s.TargetAccount {
			return s, nil
		}
		return execute(HandoffRestart, "waiting_restart", "已请求启动，等待核验")
	case "waiting_restart":
		if o.RestartReady && o.TargetMatches && o.IdentityVerified && o.SettingsVerified && o.TargetAccount == s.TargetAccount {
			return persist("ready_continue", "原会话和新账号已核验")
		}
	case "ready_continue":
		if !o.RestartReady || !o.TargetMatches || !o.IdentityVerified || !o.SettingsVerified || o.TargetAccount != s.TargetAccount {
			return s, nil
		}
		return execute(HandoffContinue, "waiting_continue", "已登记继续，等待新轮次开始")
	case "waiting_continue":
		if o.TargetMatches && o.IdentityVerified && o.ContinueStarted {
			s.Enabled = false
			return persist("completed", "原会话已开始新轮次")
		}
	default:
		return s, errors.New("未知交接状态，已停止")
	}
	return s, nil
}

func (r Reader) handoffMarkUncertain(before HandoffState) (HandoffState, error) {
	unlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer unlock()
	s := r.HandoffStatus()
	if s == before && s.PendingAction != "" {
		s.Enabled, s.Stage, s.Message = false, "uncertain", "上次动作结果未确认，请核查后处理"
		return s, writeState(r.handoffPath(), s)
	}
	return s, nil
}

// Keep the original user authorization for this one session after a completed
// handoff, unless the user cancelled or changed it while completion was being
// verified. The caller swaps the source/target accounts only after this succeeds.
func (r Reader) rearmCompletedHandoff(expected HandoffState) (HandoffState, bool, error) {
	unlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), false, err
	}
	defer unlock()
	current := r.HandoffStatus()
	if current != expected || current.Stage != "completed" || current.Enabled || current.AuthorizationID == "" || r.handoffCancelled(current) || r.nativeAccountPending() {
		return current, false, nil
	}
	current = HandoffState{Version: 1, Enabled: true, AuthorizationID: current.AuthorizationID, Scope: current.Scope, SessionID: current.SessionID, Threshold: current.Threshold, Stage: "watching", Message: "交接完成，继续监控此会话"}
	if err := writeState(r.handoffPath(), current); err != nil {
		return current, false, err
	}
	return current, true, nil
}

// A completed handoff is not ready to be rearmed while the credential recovery
// record still needs attention. Preserve the frozen coordinates across restarts.
func (r Reader) handoffCleanupUncertain(expected HandoffState) (HandoffState, error) {
	unlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer unlock()
	current := r.HandoffStatus()
	if current != expected || current.Stage != "completed" || current.Enabled {
		return current, nil
	}
	current.Stage = "uncertain"
	current.Message = "交接已开始新轮次，但认证恢复记录未清理；请关闭托管终端并核查账号"
	if current.Scope == "desktop" {
		current.Message = "交接已开始新轮次，但认证恢复记录未清理；请检查 Desktop 的会话和账号"
	}
	return current, writeState(r.handoffPath(), current)
}

// Acknowledge does not modify authentication or replay an action. If a
// credential transaction remains, an explicit user acknowledgement keeps the
// current record only after it passes local and service verification.
func (r Reader) AcknowledgeHandoff() (HandoffState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return r.acknowledgeHandoff(ctx, func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) })
}

func (r Reader) acknowledgeHandoff(ctx context.Context, verify func(context.Context, quotaCredential) AccountQuota) (HandoffState, error) {
	unlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer unlock()
	s := r.HandoffStatus()
	if r.nativeAccountPending() {
		return s, errors.New("账号切换有待核查记录，请先在账号管理中核查")
	}
	if _, err := os.Lstat(r.ccSwitchTransactionPath()); err == nil {
		return r.reviewCCSwitchHandoff(ctx, s, verify)
	} else if !os.IsNotExist(err) {
		return s, errors.New("CC Switch 恢复记录不可读取")
	}
	_, txnErr := os.Lstat(r.handoffAuthTransactionPath())
	if txnErr != nil && !os.IsNotExist(txnErr) {
		return s, errors.New("认证恢复记录不可检查")
	}
	hasTransaction := txnErr == nil
	if s.Enabled || (s.Stage != "uncertain" && !(s.Stage == "completed" && hasTransaction)) {
		return s, errors.New("当前没有待确认的交接")
	}
	if s.Scope == "cli" {
		unlockRunner, e := lockWarmup(filepath.Join(filepath.Dir(r.resumePath()), "cli-runner.lock"))
		if e != nil {
			return s, errors.New("请先关闭该托管终端，再确认已处理")
		}
		defer unlockRunner()
		if r.cliManaged(s.SessionID) {
			return s, errors.New("请先关闭该托管终端，再确认已处理")
		}
		if err := r.ensureNoLivePreviousCLIRunner(); err != nil {
			return s, err
		}
	}
	if s.Scope == "desktop" {
		unlockRunner, e := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.lock"))
		if e != nil {
			return s, errors.New("请先停止 Desktop 交接并核查账号")
		}
		defer unlockRunner()
	}
	if hasTransaction {
		if s.Scope != "cli" && s.Scope != "desktop" {
			return s, errors.New("该认证恢复记录需要手动检查")
		}
		if err := r.resolveHandoffAuthAfterReview(ctx, s, verify); err != nil {
			return s, err
		}
	}
	s.Enabled = false
	s.Stage = "off"
	s.PendingAction = ""
	s.Message = "已确认交接状态；重新使用需要手动开启"
	if hasTransaction {
		s.Message = "当前认证已通过检查并保留；重新打开会话时请确认实际账号"
	}
	return s, writeState(r.handoffPath(), s)
}

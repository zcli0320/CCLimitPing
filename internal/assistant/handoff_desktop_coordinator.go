package assistant

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
)

// desktopRuntimeEvidence must come from an adapter for the actual Desktop
// instance. A false flag includes unknown; an on-disk credential alone cannot
// establish a process, its configuration root, or another conversation's state.
type desktopRuntimeEvidence struct {
	View                                  desktopConversation
	Owner                                 string
	Directory, Model, Effort, Permissions string
	OtherWorkIdle                         bool
	WholeAppExitAccepted                  bool
	InterventionKnown                     bool
	UserIntervened                        bool
	OldExited                             bool
	ColdStartVerified                     bool
	ConfigHomeVerified                    bool
	SettingsVerified                      bool
	ContinueTurnID                        string
}

// The native implementation remains gated in the UI until a full real-device
// exit, credential switch and restart is verified. Tests use an isolated fake.
type desktopHandoffRuntime interface {
	observe(context.Context, string) (desktopRuntimeEvidence, error)
	requestStop(context.Context, string, string, string) error
	bindLimitedStop(context.Context, string, string, string) error
	requestExit(context.Context) error
	requestRestart(context.Context, string) error
	requestContinue(context.Context, string, string, resumeTurn) (string, error)
}

type desktopHandoffCoordinator struct {
	r                            Reader
	id, provider, source, target string
	runtime                      desktopHandoffRuntime
	quota                        func(context.Context, quotaCredential) AccountQuota
	prepared                     handoffAuthPreparation
	preparedOK                   bool
	stopAccepted                 bool
	limitedTurn                  string
	active                       HandoffObservation
	activeOwner                  string
	activeUsers                  map[string]bool
	owner                        string
	continuedTurn                string
	ccswitch                     *desktopCCSwitch
	native                       *nativeDualSwitcher
	lastObservation              HandoffObservation
	observationFailed            bool
}

func (d *desktopHandoffCoordinator) targetEvidence(ctx context.Context, threshold float64) (AccountQuota, bool) {
	if d.native != nil {
		return d.native.evidence(ctx, d.r.HandoffStatus())
	}
	if d.ccswitch != nil {
		credential, found := d.r.quotaCredentials(time.Now())[d.target]
		if !found || d.quota == nil {
			return AccountQuota{}, false
		}
		q := d.quota(ctx, credential)
		if !resumeQuotaReady(q) || !handoffTargetQuotaOK(q, threshold) {
			return q, false
		}
		path, err := d.r.handoffAuthPath("desktop")
		if err != nil {
			return q, false
		}
		raw, err := readProtectedHandoffFile(path)
		if err != nil || !d.r.handoffCurrentMatchesAccount(raw, d.source) {
			return q, false
		}
		if d.preparedOK && sha256.Sum256(raw) != d.prepared.currentHash {
			return q, false
		}
		if _, err := d.r.ccSwitchBinding(ctx, d.target); err != nil {
			return q, false
		}
		d.prepared = handoffAuthPreparation{scope: "desktop", accountID: d.target, currentHash: sha256.Sum256(raw)}
		d.preparedOK = true
		return q, true
	}
	p, c, err := d.r.prepareHandoffAuth("desktop", d.target, time.Now())
	if err != nil || d.quota == nil {
		return AccountQuota{}, false
	}
	q := d.quota(ctx, c.quotaCredential())
	if !resumeQuotaReady(q) || !handoffTargetQuotaOK(q, threshold) {
		return q, false
	}
	if d.preparedOK && (d.prepared != p) {
		return q, false
	}
	d.prepared, d.preparedOK = p, true
	return q, true
}

func (d *desktopHandoffCoordinator) targetInstalled() bool {
	if d.native != nil {
		return d.native.installed(d.r.HandoffStatus())
	}
	if d.ccswitch != nil {
		return d.ccswitch.installed(d.r.HandoffStatus())
	}
	if !d.preparedOK {
		return false
	}
	path, err := d.r.handoffAuthPath("desktop")
	if err != nil {
		return false
	}
	data, err := readProtectedHandoffFile(path)
	return err == nil && sha256.Sum256(data) == d.prepared.candidateHash && d.r.handoffCurrentMatchesAccount(data, d.target)
}

func (d *desktopHandoffCoordinator) preparedStillValid() bool {
	if d.native != nil {
		_, ok := d.native.evidence(context.Background(), d.r.HandoffStatus())
		return ok
	}
	if !d.preparedOK {
		return false
	}
	path, err := d.r.handoffAuthPath("desktop")
	if err != nil {
		return false
	}
	current, err := readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(current) != d.prepared.currentHash || !d.r.handoffCurrentMatchesAccount(current, d.source) {
		return false
	}
	if d.ccswitch != nil {
		_, err := d.r.ccSwitchBinding(context.Background(), d.target)
		return err == nil
	}
	candidate, err := d.r.findHandoffAuthCandidate(d.target, time.Now())
	return err == nil && candidate.digest == d.prepared.candidateHash
}

func (d *desktopHandoffCoordinator) sourceCurrent() (quotaCredential, bool) {
	path, err := d.r.handoffAuthPath("desktop")
	if err != nil {
		return quotaCredential{}, false
	}
	raw, err := readProtectedHandoffFile(path)
	if err != nil || !d.r.handoffCurrentMatchesAccount(raw, d.source) {
		return quotaCredential{}, false
	}
	c, err := d.r.scopeCredential("desktop", time.Now())
	return c, err == nil
}

func (d *desktopHandoffCoordinator) Observe(ctx context.Context, s HandoffState) (observed HandoffObservation, observeError error) {
	defer func() { d.lastObservation = observed; d.observationFailed = observeError != nil }()
	o := HandoffObservation{TargetMatches: s.Scope == "desktop" && s.SessionID == d.id, SourceAccount: d.source, TargetAccount: d.target}
	if !o.TargetMatches || d.runtime == nil || d.quota == nil || d.source == d.target {
		return o, nil
	}
	e, err := d.runtime.observe(ctx, d.id)
	if err != nil {
		return o, err
	}
	o.UserIntervened = e.UserIntervened
	if !e.InterventionKnown {
		o.Warning = "无法确认是否存在手动操作，等待检查"
		return o, nil
	}
	o.OldExited = e.OldExited
	if e.OldExited && (s.Stage == "watching" || s.Stage == "ready_stop" || s.Stage == "waiting_stop" || s.Stage == "ready_exit") {
		o.UserIntervened = true
		return o, nil
	}
	if s.Stage == "waiting_exit" {
		o.ExitTimedOut = !e.OldExited && s.ActionAt > 0 && time.Now().Unix()-s.ActionAt >= 45
	}
	if s.Stage == "waiting_switch" || s.Stage == "ready_restart" {
		o.SwitchVerified = d.targetInstalled()
		return o, nil
	}
	if s.Stage == "ready_switch" {
		q, ok := d.targetEvidence(ctx, s.Threshold)
		o.TargetCredentialReady, o.TargetQuotaKnown, o.TargetWeeklyUsable = ok, ok, ok
		o.TargetRemaining, _ = handoffWindow(&q, 18000)
		return o, nil
	}
	if s.Stage == "waiting_exit" {
		return o, nil
	}
	turn, hasTurn := e.View.latest()
	if s.Stage == "waiting_restart" || s.Stage == "ready_continue" || s.Stage == "waiting_continue" {
		o.RestartReady = e.ColdStartVerified && e.ConfigHomeVerified && d.targetInstalled()
		// Inferred from confirmed full exit, cold start and effective config
		// home plus a whole-record match. This is not direct account/read.
		o.IdentityVerified = o.RestartReady
		o.SettingsVerified = e.SettingsVerified
		o.TargetMatches = o.TargetMatches && e.View.ID == d.id && e.Owner != "" && hasTurn
		if s.Stage != "waiting_continue" {
			o.TargetMatches = o.TargetMatches && desktopHandoffStopObserved(d.r.Home, d.id, s.TurnID, e.Owner, d.provider, e.View, e.Owner)
		} else {
			o.ContinueStarted = hasTurn && d.continuedTurn != "" && e.ContinueTurnID == d.continuedTurn && turn.ID == d.continuedTurn && (turn.Status == "inProgress" || turn.Status == "completed")
		}
		return o, nil
	}
	current, ok := d.sourceCurrent()
	if !ok {
		o.UserIntervened = true
		return o, nil
	}
	q := d.quota(ctx, current)
	o.CurrentRemaining, o.CurrentQuotaKnown = handoffWindow(&q, 18000)
	// Monitoring is not an armed credential transaction. Normal token refresh
	// may update the same account while watching; freeze only when it triggers.
	if s.Stage == "watching" {
		d.preparedOK = false
	}
	backup, ok := d.targetEvidence(ctx, s.Threshold)
	o.TargetCredentialReady, o.TargetQuotaKnown, o.TargetWeeklyUsable = ok, ok, ok
	o.TargetRemaining, _ = handoffWindow(&backup, 18000)
	o.TargetMatches = o.TargetMatches && e.View.ID == d.id && e.Owner != "" && e.View.ModelProvider == d.provider && e.View.ResumeState == "resumed" && e.View.Mode != "durable" && hasTurn
	if !o.TargetMatches {
		return o, nil
	}
	o.TurnID = turn.ID
	o.Directory, o.Model, o.Effort, o.Permissions = e.Directory, e.Model, e.Effort, e.Permissions
	o.Running = e.View.Runtime.Type == "active" && turn.Status == "inProgress" && len(e.View.Requests) == 0
	if d.native != nil {
		o.NeedsPreparation = !d.native.ready
		o.AllocationConflict = !d.r.AccountAllocation().Distinct
	}
	if s.Stage == "watching" && o.Running {
		d.active, d.activeOwner, d.activeUsers = o, e.Owner, desktopUserItems(d.id, turn)
	}
	if (s.Stage == "watching" || s.Stage == "ready_prepare" || s.Stage == "ready_stop") && d.limitedMatches(e, turn.ID) {
		d.owner, d.limitedTurn = e.Owner, turn.ID
		o.AlreadyLimited, o.StopConfirmed, o.StoppedForHandoff = true, true, true
		o.OtherSessionsIdle = e.OtherWorkIdle || e.WholeAppExitAccepted
	}
	if s.Stage == "waiting_stop" || s.Stage == "ready_exit" {
		o.StopConfirmed = d.stopObserved(e, s.TurnID)
		o.StoppedForHandoff = o.StopConfirmed
		o.OtherSessionsIdle = e.OtherWorkIdle || e.WholeAppExitAccepted
		// This adapter only requests a graceful stop. No forced interruption.
	}
	return o, nil
}

// Natural exhaustion must belong to a turn observed running by this runner.
// A stale failed turn, a new user message or changed settings cannot authorize
// quitting Desktop. The runtime binds the same guard before the exit action.
func (d *desktopHandoffCoordinator) limitedMatches(e desktopRuntimeEvidence, turnID string) bool {
	if d.active.TurnID != turnID || d.activeOwner == "" || e.Owner != d.activeOwner || e.Directory != d.active.Directory || e.Model != d.active.Model || e.Effort != d.active.Effort || e.Permissions != d.active.Permissions || !desktopHandoffLimitedTurn(e.View) || !desktopHandoffStopObserved(d.r.Home, d.id, turnID, d.activeOwner, d.provider, e.View, e.Owner) {
		return false
	}
	turn, _ := e.View.latest()
	for item := range desktopUserItems(d.id, turn) {
		if !d.activeUsers[item] {
			return false
		}
	}
	return true
}

func (d *desktopHandoffCoordinator) stopObserved(e desktopRuntimeEvidence, turnID string) bool {
	if d.limitedTurn == turnID {
		return d.limitedMatches(e, turnID)
	}
	return d.stopAccepted && desktopHandoffStopObserved(d.r.Home, d.id, turnID, d.owner, d.provider, e.View, e.Owner)
}

func (d *desktopHandoffCoordinator) Execute(ctx context.Context, action HandoffAction, s HandoffState) error {
	if d.runtime == nil || d.quota == nil || s.Scope != "desktop" || s.SessionID != d.id || s.SourceAccount != d.source || s.TargetAccount != d.target {
		return errors.New("交接目标不可确认")
	}
	if action == HandoffSwitch {
		if !d.preparedOK && d.native == nil {
			return errors.New("备用认证未准备")
		}
		stopped := func(ctx context.Context) error {
			e, err := d.runtime.observe(ctx, d.id)
			if err != nil || !e.OldExited || !e.InterventionKnown || e.UserIntervened {
				return errors.New("旧实例尚未确认退出")
			}
			return nil
		}
		if d.native != nil {
			return d.native.switchAccount(ctx, s, stopped)
		}
		if d.ccswitch != nil {
			return d.ccswitch.switchAccount(ctx, s, stopped)
		}
		_, err := d.r.commitHandoffAuth(ctx, d.prepared, s.Threshold, stopped, d.quota)
		return err
	}
	e, err := d.runtime.observe(ctx, d.id)
	if err != nil || !e.InterventionKnown || e.UserIntervened {
		return errors.New("无法确认运行状态")
	}
	switch action {
	case HandoffPrepareTarget:
		if d.native == nil {
			return errors.New("原生切号后端不存在")
		}
		return d.native.prepare(ctx, s)
	case HandoffRequestStop:
		turn, ok := e.View.latest()
		if !ok || e.View.ID != d.id || e.Owner == "" || e.View.ModelProvider != d.provider || e.View.ResumeState != "resumed" || e.View.Mode == "durable" || len(e.View.Requests) != 0 || e.View.Runtime.Type != "active" || turn.ID != s.TurnID || turn.Status != "inProgress" || e.Directory != s.Directory || e.Model != s.Model || e.Effort != s.Effort || e.Permissions != s.Permissions {
			return errors.New("原轮次已变化")
		}
		for _, submission := range e.View.UnconfirmedSubmissions {
			if !submission.Terminal {
				return errors.New("存在未确认提交")
			}
		}
		current, ok := d.sourceCurrent()
		if !ok {
			return errors.New("原账号已变化")
		}
		remaining, known := handoffWindowPtr(d.quota(ctx, current), 18000)
		if !known || remaining >= s.Threshold {
			return errors.New("当前额度条件已变化")
		}
		if _, ok := d.targetEvidence(ctx, s.Threshold); !ok || !d.preparedStillValid() || d.r.handoffCancelled(s) {
			return errors.New("备用账号或授权已变化")
		}
		if err := d.runtime.requestStop(ctx, d.id, e.Owner, s.TurnID); err != nil {
			return err
		}
		d.owner, d.stopAccepted = e.Owner, true
		return nil
	case HandoffExit:
		if (!e.OtherWorkIdle && !e.WholeAppExitAccepted) || !d.stopObserved(e, s.TurnID) {
			return errors.New("原会话尚未确认停稳")
		}
		if d.limitedTurn == s.TurnID {
			if err := d.runtime.bindLimitedStop(ctx, d.id, d.owner, s.TurnID); err != nil {
				return err
			}
		}
		if _, ok := d.sourceCurrent(); !ok {
			return errors.New("原账号已变化")
		}
		if _, ok := d.targetEvidence(ctx, s.Threshold); !ok || !d.preparedStillValid() || d.r.handoffCancelled(s) {
			return errors.New("备用账号或授权已变化")
		}
		e, err = d.runtime.observe(ctx, d.id)
		if err != nil || !e.InterventionKnown || e.UserIntervened || (!e.OtherWorkIdle && !e.WholeAppExitAccepted) || !d.stopObserved(e, s.TurnID) {
			return errors.New("原会话状态已变化")
		}
		return d.runtime.requestExit(ctx)
	case HandoffRestart:
		if !e.OldExited || !d.targetInstalled() || d.r.handoffCancelled(s) {
			return errors.New("旧实例或目标认证不可确认")
		}
		return d.runtime.requestRestart(ctx, d.id)
	case HandoffContinue:
		turn, ok := e.View.latest()
		if !e.ColdStartVerified || !e.ConfigHomeVerified || !e.SettingsVerified || !d.targetInstalled() || !ok || !desktopHandoffStopObserved(d.r.Home, d.id, s.TurnID, e.Owner, d.provider, e.View, e.Owner) || d.r.handoffCancelled(s) {
			return errors.New("新实例或原会话不可确认")
		}
		newTurn, err := d.runtime.requestContinue(ctx, d.id, e.Owner, turn)
		if err != nil || !validHandoffText(newTurn) || newTurn == s.TurnID {
			return errors.New("继续轮次未确认")
		}
		d.continuedTurn = newTurn
		return nil
	default:
		return errors.New("此适配器不支持强制中断")
	}
}

func handoffWindowPtr(q AccountQuota, seconds int64) (float64, bool) {
	return handoffWindow(&q, seconds)
}

// finishCompleted clears the private recovery copy only after a verified new
// turn, then preserves the same explicit session authorization for monitoring.
// If verification fails, the recovery transaction remains for user review.
func (d *desktopHandoffCoordinator) finishCompleted(ctx context.Context, completed HandoffState) (HandoffState, bool, error) {
	if completed.Stage != "completed" || completed.Enabled || completed.Scope != "desktop" || completed.SessionID != d.id || completed.TargetAccount != d.target || d.continuedTurn == "" || d.runtime == nil {
		return completed, false, errors.New("交接结果不可确认")
	}
	verify := func(ctx context.Context, account string) error {
		if account != d.target || !d.targetInstalled() {
			return errors.New("目标凭证不一致")
		}
		e, err := d.runtime.observe(ctx, d.id)
		if err != nil || !e.InterventionKnown || e.UserIntervened || !e.ColdStartVerified || !e.ConfigHomeVerified || !e.SettingsVerified || e.View.ID != d.id || e.Owner == "" || e.ContinueTurnID != d.continuedTurn {
			return errors.New("新实例状态不可确认")
		}
		turn, ok := e.View.latest()
		if !ok || turn.ID != d.continuedTurn || (turn.Status != "inProgress" && turn.Status != "completed") {
			return errors.New("继续轮次不可确认")
		}
		return nil
	}
	var finishErr error
	if d.native != nil {
		finishErr = d.native.finish(ctx, completed, verify)
	} else if d.ccswitch != nil {
		finishErr = d.ccswitch.finish(ctx, completed, verify)
	} else {
		finishErr = d.r.finishHandoffAuth(ctx, verify)
	}
	if finishErr != nil {
		return completed, false, finishErr
	}
	next, rearmed, err := d.r.rearmCompletedHandoff(completed)
	if err != nil || !rearmed {
		return next, rearmed, err
	}
	d.source, d.target = d.target, d.source
	d.prepared, d.preparedOK = handoffAuthPreparation{}, false
	d.stopAccepted, d.owner, d.continuedTurn = false, "", ""
	d.limitedTurn, d.activeOwner, d.activeUsers, d.active = "", "", nil, HandoffObservation{}
	return next, true, nil
}

package assistant

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OrchestraSwitchResult contains only display-safe information.
type OrchestraSwitchResult struct {
	Route, Reason    string
	Remaining        float64
	Known, Uncertain bool
}

type orchestraSwitchManager interface {
	handoffAccountManager
	orchestraDesktopStopped(context.Context, string) error
	Close()
}
type orchestraSwitchDeps struct {
	manager func(context.Context) (orchestraSwitchManager, error)
	bridge  func(Reader) (*desktopCCSwitch, error)
}

// WithOrchestraRecoveryResolved holds the same handoff locks while a caller
// records an explicit user acknowledgement. It never changes credentials.
func (r Reader) WithOrchestraRecoveryResolved(ack func() error) error {
	if ack == nil {
		return errors.New("缺少用户确认")
	}
	execUnlock, err := lockWarmup(r.handoffPath() + ".execution.lock")
	if err != nil {
		return err
	}
	defer execUnlock()
	runnerUnlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.lock"))
	if err != nil {
		return err
	}
	defer runnerUnlock()
	stateUnlock, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return err
	}
	defer stateUnlock()
	s := r.HandoffStatus()
	if (s.Stage != "off" && s.Stage != "completed" && s.Stage != "cancelled") || s.Enabled || s.PendingAction != "" || r.nativeAccountPending() {
		return errors.New("请先核查并处理现有交接或账号恢复记录")
	}
	for _, path := range []string{r.handoffAuthTransactionPath(), r.ccSwitchTransactionPath()} {
		if _, e := os.Lstat(path); !os.IsNotExist(e) {
			return errors.New("请先核查并处理现有交接或账号恢复记录")
		}
	}
	return ack()
}

// OrchestraSwitchDesktop uses the shared handoff journal and transaction
// writers. beforeWrite must durably register this attempt and revalidate the
// executor's authorization; permit is rechecked at each existing write guard.
func (r Reader) OrchestraSwitchDesktop(ctx context.Context, attempt string, threshold float64, beforeWrite func(string) error, permit func() bool) (out OrchestraSwitchResult) {
	return r.orchestraSwitchDesktop(ctx, attempt, threshold, beforeWrite, permit, orchestraSwitchDeps{
		manager: func(c context.Context) (orchestraSwitchManager, error) { return r.startNativeHandoffManager(c) },
		bridge:  newDesktopCCSwitch,
	})
}

func (r Reader) orchestraSwitchDesktop(ctx context.Context, attempt string, threshold float64, beforeWrite func(string) error, permit func() bool, deps orchestraSwitchDeps) (out OrchestraSwitchResult) {
	out.Reason = "无法确认自动切号条件"
	if !r.nativeAccountPreview() || attempt == "" || !validHandoffThreshold(threshold) || beforeWrite == nil || permit == nil {
		return
	}
	unlockExec, err := lockWarmup(r.handoffPath() + ".execution.lock")
	if err != nil {
		return
	}
	defer unlockExec()
	unlockRunner, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.lock"))
	if err != nil {
		return
	}
	defer unlockRunner()
	unlockState, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return
	}
	defer unlockState()
	s := r.HandoffStatus()
	resume := r.ResumeStatus()
	if (s.Stage != "off" && s.Stage != "completed" && s.Stage != "cancelled") || s.Enabled || s.PendingAction != "" || resume.Enabled || resume.Status == "error" || r.nativeAccountPending() {
		out.Reason = "已有交接或恢复记录，请先核查"
		return
	}
	for _, path := range []string{r.handoffAuthTransactionPath(), r.ccSwitchTransactionPath()} {
		if _, e := os.Lstat(path); !os.IsNotExist(e) {
			out.Reason = "已有交接或恢复记录，请先核查"
			return
		}
	}
	owner, err := r.desktopCCSwitchOwner()
	if err != nil {
		return
	}
	out.Route = "native"
	if owner {
		out.Route = "ccswitch"
	}
	if deps.manager == nil || deps.bridge == nil {
		return
	}
	manager, err := deps.manager(ctx)
	if err != nil {
		return
	}
	defer manager.Close()
	stopped := func(c context.Context) error {
		if !permit() {
			return errors.New("自动切号授权已变化")
		}
		return manager.orchestraDesktopStopped(c, out.Route)
	}
	if err = stopped(ctx); err != nil {
		if strings.Contains(err.Error(), "Desktop") || strings.Contains(err.Error(), "Codex") {
			out.Reason = "关闭 Codex Desktop 后可自动切号"
		} else {
			out.Reason = "无法确认 Desktop 已关闭"
		}
		return
	}
	source, target, err := r.desktopHandoffAccounts()
	if err != nil {
		out.Reason = "没有额度充足的备用账号"
		return
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	coordinator := desktopHandoffCoordinator{r: r, source: source, target: target, manager: manager, quota: func(c context.Context, token quotaCredential) AccountQuota {
		return queryAccountQuota(c, client, token)
	}}
	if owner {
		coordinator.ccswitch, err = deps.bridge(r)
		if err != nil {
			return
		}
	}
	if _, ok := coordinator.targetEvidence(ctx, threshold); !ok {
		out.Reason = "没有额度充足的备用账号"
		return
	}
	if err = stopped(ctx); err != nil || !coordinator.preparedStillValid() {
		out.Reason = "切号条件已变化"
		return
	}
	if err = beforeWrite(out.Route); err != nil {
		out.Reason = "无法登记自动切号"
		return
	}
	s = HandoffState{Version: 1, Kind: "orchestra_auto_switch", Enabled: true, AuthorizationID: attempt, Scope: "desktop", SourceAccount: source, TargetAccount: target, Threshold: threshold, Stage: "pending", PendingAction: string(HandoffSwitch), Message: "执行器自动切号已登记"}
	if err = writeState(r.handoffPath(), s); err != nil {
		out.Reason = "无法保存交接记录"
		out.Uncertain = true
		return
	}
	fail := func() OrchestraSwitchResult {
		s.Enabled = false
		s.Stage = "uncertain"
		s.Message = "自动切号结果待核查"
		_ = writeState(r.handoffPath(), s)
		out.Uncertain = true
		out.Reason = "自动切号结果待核查"
		return out
	}
	if stopped(ctx) != nil || !coordinator.preparedStillValid() {
		return fail()
	}
	if _, ok := coordinator.targetEvidence(ctx, threshold); !ok {
		return fail()
	}
	if owner {
		err = coordinator.ccswitch.switchAccount(ctx, s, stopped)
		if err == nil && !coordinator.ccswitch.installed(s) {
			err = errors.New("CC Switch 结果不可确认")
		}
	} else {
		if err = r.requireNativeWriteOwner("desktop"); err == nil {
			err = stopped(ctx)
		}
		if err == nil && !coordinator.preparedStillValid() {
			err = errors.New("认证已变化")
		}
		if err == nil {
			err = manager.apply(ctx, target, "desktop")
		}
		if err == nil && !coordinator.targetInstalled() {
			err = errors.New("目标认证不可确认")
		}
	}
	if err != nil {
		return fail()
	}
	if stopped(ctx) != nil {
		return fail()
	}
	q, err := manager.quota(ctx, target)
	if err != nil {
		return fail()
	}
	remaining, known := handoffWindow(&q, 18000)
	if !known || remaining < threshold {
		return fail()
	}
	if owner {
		if err = coordinator.ccswitch.finish(ctx, s, func(c context.Context, account string) error {
			if stopped(c) != nil || account != target || !coordinator.targetInstalled() {
				return errors.New("切号结果不可确认")
			}
			return nil
		}); err != nil {
			return fail()
		}
	}
	s.Enabled = false
	s.PendingAction = ""
	s.Stage = "completed"
	s.Message = "执行器账号切换已核验"
	if writeState(r.handoffPath(), s) != nil {
		return fail()
	}
	out.Remaining, out.Known, out.Reason = remaining, true, ""
	return
}

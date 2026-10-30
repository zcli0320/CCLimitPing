package assistant

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type desktopRunnerState struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"session_id"`
	AuthorizationID string `json:"authorization_id"`
	Ready           bool   `json:"ready"`
}

// Fixed status evidence only: no credentials, prompts, permission payloads or
// account names. Persist changes so a timeout cannot erase the last failed gate.
type desktopStepCheckpoint struct {
	Stage             string `json:"stage"`
	ObservationFailed bool   `json:"observation_failed"`
	TargetMatches     bool   `json:"target_matches"`
	OldExited         bool   `json:"old_exited"`
	StopConfirmed     bool   `json:"stop_confirmed"`
	RestartReady      bool   `json:"restart_ready"`
	IdentityVerified  bool   `json:"identity_verified"`
	SettingsVerified  bool   `json:"settings_verified"`
	ContinueStarted   bool   `json:"continue_started"`
}

func (r Reader) desktopRunnerPath() string {
	return filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.json")
}

func (r Reader) desktopRunnerReady(s HandoffState) bool {
	var record desktopRunnerState
	return s.Enabled && s.Scope == "desktop" && readJSON(r.desktopRunnerPath(), &record) == nil && record.Ready && record.PID > 0 && record.SessionID == s.SessionID && record.AuthorizationID == s.AuthorizationID && syscall.Kill(record.PID, 0) == nil
}

func (r Reader) desktopHandoffAccounts() (string, string, error) {
	var store handoffManagedStore
	if readJSON(r.managedAccountsStorePath(), &store) != nil || store.Version != 2 || len(store.Accounts) != 2 {
		return "", "", errors.New("需要账号库中恰好两个账号")
	}
	path, err := r.handoffAuthPath("desktop")
	if err != nil {
		return "", "", err
	}
	raw, err := readProtectedHandoffFile(path)
	if err != nil {
		return "", "", err
	}
	if _, err := r.scopeCredential("desktop", time.Now()); err != nil {
		return "", "", err
	}
	source, target := "", ""
	for id := range store.Accounts {
		if r.handoffCurrentMatchesAccount(raw, id) {
			if source != "" {
				return "", "", errors.New("当前账号映射不唯一")
			}
			source = id
		}
	}
	for id := range store.Accounts {
		if id != source {
			if target != "" {
				return "", "", errors.New("备用账号映射不唯一")
			}
			target = id
		}
	}
	if source == "" || target == "" {
		return "", "", errors.New("无法匹配来源和备用账号")
	}
	return source, target, nil
}

func requireReadyDesktopPreflight(selected DesktopHandoffPreflight) error {
	if !selected.RuntimeIdentityVerified {
		return errors.New("Desktop 运行账号未独立确认；请正常退出后以核验方式打开，再重新检查")
	}
	if !selected.Ready {
		return errors.New("所选 Desktop 会话尚未就绪；请点击“检查所选会话”查看原因")
	}
	return nil
}

// PrepareDesktopHandoff authorizes only the selected session and records no
// credentials. The UI must explain that the entire Desktop app will quit.
func (r Reader) PrepareDesktopHandoff(ctx context.Context, id string, threshold float64) (HandoffState, error) {
	if !capabilities()["desktop_handoff"].Available {
		return r.HandoffStatus(), errors.New("Desktop 自动切号尚未完成真实账号验收")
	}
	if !desktopHandoffUUID.MatchString(id) || !validHandoffThreshold(threshold) {
		return r.HandoffStatus(), errors.New("会话或阈值无效")
	}
	owner, err := r.desktopCCSwitchOwner()
	if err != nil {
		return r.HandoffStatus(), err
	}
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.lock"))
	if err != nil {
		return r.HandoffStatus(), errors.New("已有 Desktop 交接任务")
	}
	defer unlock()
	if r.ResumeStatus().Enabled {
		return r.HandoffStatus(), errors.New("请先关闭不切号续跑")
	}
	s := r.Snapshot(ctx)
	valid := false
	for _, source := range s.Sources {
		if source.ID == "desktop" && source.Complete {
			valid = true
		}
	}
	selected := false
	for _, item := range s.Sessions {
		if item.Scope == "desktop" && item.ID == id && !item.Archived && item.Project != "" {
			selected = true
			break
		}
	}
	if !valid || !selected {
		return r.HandoffStatus(), errors.New("所选 Desktop 会话或项目不可用")
	}
	if _, ok := r.managedResumeProvider("desktop"); !ok {
		return r.HandoffStatus(), errors.New("Desktop 官方账号配置不可确认")
	}
	_, target, err := r.desktopHandoffAccounts()
	if err != nil {
		return r.HandoffStatus(), err
	}
	if r.nativeAccountPreview() {
		manager, err := r.startNativeHandoffManager(ctx)
		if err != nil {
			return r.HandoffStatus(), err
		}
		defer manager.Close()
		if !owner {
			if err := manager.ready(ctx, target, "desktop"); err != nil {
				return r.HandoffStatus(), err
			}
		}
		q, err := manager.quota(ctx, target)
		if err != nil || !handoffTargetQuotaOK(q, threshold) {
			return r.HandoffStatus(), errors.New("备用账号额度或登录不可用")
		}
	}
	if !r.nativeAccountPreview() || owner {
		bridge, err := newDesktopCCSwitch(r)
		if err != nil {
			return r.HandoffStatus(), err
		}
		if err := bridge.inspect(ctx, target); err != nil {
			return r.HandoffStatus(), err
		}
	}
	if r.nativeAccountPreview() {
		// An existing history row is not proof that Desktop has the selected
		// conversation loaded and ready to control. Do not report an armed
		// monitor until the same read-only preflight succeeds.
		selected, err := r.CheckDesktopHandoffPreflight(ctx, id)
		if err != nil {
			return r.HandoffStatus(), errors.New("所选 Desktop 会话无法核查；请在 Desktop 打开会话后重新检查")
		}
		if err := requireReadyDesktopPreflight(selected); err != nil {
			return r.HandoffStatus(), err
		}
	} else if _, err := newDesktopHandoffLifecycle(ctx, r.Home); err != nil {
		return r.HandoffStatus(), err
	}
	currentOwner, err := r.desktopCCSwitchOwner()
	if err != nil {
		return r.HandoffStatus(), err
	}
	if currentOwner != owner {
		return r.HandoffStatus(), errors.New("Desktop 管理方已变化，请重新检查")
	}
	return r.ConfigureHandoff("desktop", id, threshold, true)
}

func (r Reader) RunDesktopHandoff(ctx context.Context, id string, threshold float64, authorizationID string) error {
	if !capabilities()["desktop_handoff"].Available {
		return errors.New("Desktop 自动切号尚未完成真实账号验收")
	}
	state := r.HandoffStatus()
	if !state.Enabled || state.Scope != "desktop" || state.SessionID != id || state.Stage != "watching" || state.AuthorizationID == "" || state.AuthorizationID != authorizationID || state.Threshold != threshold || r.handoffCancelled(state) {
		return errors.New("该会话的切号授权已变化")
	}
	owner, err := r.desktopCCSwitchOwner()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	var bridge *desktopCCSwitch
	var manager *nativeManagerClient
	var handoffManager handoffAccountManager
	if r.nativeAccountPreview() {
		var err error
		manager, err = r.startNativeHandoffManager(ctx)
		if err != nil {
			return err
		}
		defer manager.Close()
		handoffManager = manager
	}
	if !r.nativeAccountPreview() || owner {
		var err error
		bridge, err = newDesktopCCSwitch(r)
		if err != nil {
			return err
		}
	}
	deps := desktopRunnerDependencies{
		ccswitch: bridge,
		manager:  handoffManager,
		makeRuntime: func(ctx context.Context, provider string) (desktopHandoffRuntime, error) {
			lifecycle, err := newDesktopHandoffLifecycle(ctx, r.Home)
			if err != nil {
				return nil, err
			}
			return &desktopSingleSessionRuntime{r: r, id: id, provider: provider, lifecycle: lifecycle, wholeAppExitAccepted: true}, nil
		},
		resetRuntime: func(ctx context.Context, value desktopHandoffRuntime) error {
			runtime, ok := value.(*desktopSingleSessionRuntime)
			if !ok {
				return errors.New("Desktop 适配器不可用")
			}
			lifecycle, err := newDesktopHandoffLifecycle(ctx, r.Home)
			if err != nil {
				return err
			}
			runtime.lifecycle = lifecycle
			runtime.stopTurn = ""
			runtime.oldOwner = ""
			runtime.baselineUsers = nil
			runtime.baselineDirectory = ""
			runtime.baselineModel = ""
			runtime.baselineEffort = ""
			runtime.baselinePermissions = ""
			runtime.launchRequested = false
			runtime.continuedTurn = ""
			return nil
		},
		quota: func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) },
		tick:  2 * time.Second, watchInterval: 30 * time.Second,
	}
	return r.runDesktopHandoff(ctx, id, threshold, authorizationID, deps)
}

type desktopRunnerDependencies struct {
	ccswitch            *desktopCCSwitch
	manager             handoffAccountManager
	makeRuntime         func(context.Context, string) (desktopHandoffRuntime, error)
	resetRuntime        func(context.Context, desktopHandoffRuntime) error
	quota               func(context.Context, quotaCredential) AccountQuota
	tick, watchInterval time.Duration
}

func (r Reader) runDesktopHandoff(ctx context.Context, id string, threshold float64, authorizationID string, deps desktopRunnerDependencies) error {
	if deps.makeRuntime == nil || deps.resetRuntime == nil || deps.quota == nil || deps.tick <= 0 || deps.watchInterval <= 0 {
		return errors.New("Desktop 监控配置不可用")
	}
	state := r.HandoffStatus()
	if !state.Enabled || state.Stage != "watching" || state.Scope != "desktop" || state.SessionID != id || state.Threshold != threshold || state.AuthorizationID == "" || state.AuthorizationID != authorizationID || r.handoffCancelled(state) {
		return errors.New("该会话的切号授权已变化")
	}
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.lock"))
	if err != nil {
		return errors.New("已有 Desktop 交接任务")
	}
	defer unlock()
	defer func() {
		current := r.HandoffStatus()
		if current.Enabled && current.AuthorizationID == authorizationID {
			_, _ = r.ConfigureHandoff("", "", 0, false)
		}
	}()
	defer func() {
		var record desktopRunnerState
		if readJSON(r.desktopRunnerPath(), &record) == nil && record.PID == os.Getpid() && record.AuthorizationID == authorizationID {
			_ = os.Remove(r.desktopRunnerPath())
		}
	}()
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		return errors.New("Desktop 官方账号配置不可确认")
	}
	source, target, err := r.desktopHandoffAccounts()
	if err != nil {
		return err
	}
	runtime, err := deps.makeRuntime(ctx, provider)
	if err != nil {
		return err
	}
	d := &desktopHandoffCoordinator{r: r, id: id, provider: provider, source: source, target: target, runtime: runtime, quota: deps.quota, ccswitch: deps.ccswitch, manager: deps.manager}
	if r.HandoffStatus() != state || r.handoffCancelled(state) {
		return errors.New("切号授权已取消")
	}
	if err := writeState(r.desktopRunnerPath(), desktopRunnerState{PID: os.Getpid(), SessionID: id, AuthorizationID: authorizationID, Ready: true}); err != nil {
		return errors.New("无法登记 Desktop 监控")
	}
	ticker := time.NewTicker(deps.tick)
	defer ticker.Stop()
	lastWatching := time.Time{}
	var lastCheckpoint desktopStepCheckpoint
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		// A ready tick and cancellation can win select simultaneously. Do not
		// observe or advance another handoff after cancellation was requested.
		if err := ctx.Err(); err != nil {
			return err
		}
		state = r.HandoffStatus()
		if !state.Enabled || state.AuthorizationID != authorizationID || state.Scope != "desktop" || state.SessionID != id {
			return nil
		}
		if state.Stage == "watching" && time.Since(lastWatching) < deps.watchInterval {
			continue
		}
		if state.Stage == "watching" {
			lastWatching = time.Now()
		}
		step, cancel := context.WithTimeout(ctx, 45*time.Second)
		updated, err := r.AdvanceHandoff(step, d)
		cancel()
		o := d.lastObservation
		checkpoint := desktopStepCheckpoint{Stage: updated.Stage, ObservationFailed: d.observationFailed, TargetMatches: o.TargetMatches, OldExited: o.OldExited, StopConfirmed: o.StopConfirmed, RestartReady: o.RestartReady, IdentityVerified: o.IdentityVerified, SettingsVerified: o.SettingsVerified, ContinueStarted: o.ContinueStarted}
		if checkpoint != lastCheckpoint {
			if e := writeState(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-checkpoint.json"), checkpoint); e != nil {
				return errors.New("Desktop 检查点无法保存，交接已停止")
			}
			lastCheckpoint = checkpoint
		}
		if err != nil {
			continue
		}
		if updated.Stage == "uncertain" {
			return errors.New(updated.Message)
		}
		if updated.Stage != "completed" {
			continue
		}
		_, rearmed, err := d.finishCompleted(ctx, updated)
		if err != nil {
			_, _ = r.handoffCleanupUncertain(updated)
			return errors.New("继续已开始，但认证恢复记录尚未核验")
		}
		if !rearmed {
			return nil
		}
		if err = deps.resetRuntime(ctx, runtime); err != nil {
			return errors.New("交接完成，但新实例监控未能启动")
		}
		lastWatching = time.Time{}
	}
}

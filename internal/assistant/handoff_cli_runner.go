package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type ownedCLIProcess struct {
	cmd      *exec.Cmd
	done     chan struct{}
	mu       sync.Mutex
	err      error
	termSent bool
}

func startOwnedCLIProcess(cmd *exec.Cmd, terminal bool) (*ownedCLIProcess, *os.File, error) {
	var tty *os.File
	var err error
	if terminal {
		tty, err = pty.Start(cmd)
	} else {
		err = cmd.Start()
	}
	if err != nil {
		return nil, nil, err
	}
	p := &ownedCLIProcess{cmd: cmd, done: make(chan struct{})}
	go func() { e := cmd.Wait(); p.mu.Lock(); p.err = e; p.mu.Unlock(); close(p.done) }()
	return p, tty, nil
}
func (p *ownedCLIProcess) exited() bool {
	if p == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
func (p *ownedCLIProcess) shutdownRequested() bool {
	if p == nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.termSent
}
func (p *ownedCLIProcess) stop(ctx context.Context) error {
	return p.stopWithTimeout(ctx, 8*time.Second)
}
func (p *ownedCLIProcess) requestStop() error {
	if p == nil || p.exited() {
		return nil
	}
	p.mu.Lock()
	if !p.termSent {
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !p.exited() {
			p.mu.Unlock()
			return err
		}
		p.termSent = true
	}
	p.mu.Unlock()
	return nil
}
func (p *ownedCLIProcess) stopWithTimeout(ctx context.Context, timeout time.Duration) error {
	if p == nil || p.exited() {
		return nil
	}
	if err := p.requestStop(); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return errors.New("进程退出超时")
	}
}

type cliHandoffInput struct {
	sync.Mutex
	terminal *os.File
	epoch    uint64
	typing   bool
}

func (i *cliHandoffInput) forward(b []byte) {
	i.Lock()
	defer i.Unlock()
	i.epoch++
	i.typing = !strings.ContainsAny(string(b), "\r\n")
	if i.terminal != nil {
		_, _ = i.terminal.Write(b)
	}
}

type cliHandoffSupervisor struct {
	r               Reader
	ctx             context.Context
	id              string
	cwd             string
	exe             string
	socket          string
	env             []string
	provider        string
	input           *cliHandoffInput
	server          *ownedCLIProcess
	tui             *ownedCLIProcess
	terminal        *os.File
	sourceID        string
	targetID        string
	prepared        handoffAuthPreparation
	preparedOK      bool
	settings        cliTurnSettings
	observedEpoch   uint64
	armedEpoch      uint64
	armed           bool
	continuedTurn   string
	seenActiveTurn  string
	seenActiveEpoch uint64
	client          *http.Client
	manager         handoffAccountManager
	output          io.Writer
}

func (s *cliHandoffSupervisor) start() error {
	if s.server != nil && !s.server.exited() {
		return errors.New("原后端仍在运行")
	}
	_ = os.Remove(s.socket)
	server := exec.Command(s.exe, "app-server", "--listen", "unix://"+s.socket)
	server.Env = s.env
	server.Stderr = io.Discard
	server.Stdout = io.Discard
	p, _, err := startOwnedCLIProcess(server, false)
	if err != nil {
		return errors.New("托管 CLI 后端启动失败")
	}
	s.server = p
	// Record ownership before waiting for the socket or TUI: if startup fails
	// and the server does not exit, the recovery UI must still see that PID.
	if err = writeState(s.r.cliRunnerPath(), cliRunnerState{ID: s.id, PID: os.Getpid(), ServerPID: p.cmd.Process.Pid, Socket: s.socket}); err != nil {
		return err
	}
	ready := false
	for n := 0; n < 100; n++ {
		if info, e := os.Stat(s.socket); e == nil && info.Mode()&os.ModeSocket != 0 {
			ready = true
			break
		}
		if p.exited() {
			return errors.New("托管 CLI 后端提前退出")
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		return errors.New("托管 CLI 后端连接超时")
	}
	cmd := exec.Command(s.exe, "resume", "--remote", "unix://"+s.socket, s.id)
	cmd.Env = s.env
	cmd.Dir = s.cwd
	tui, tty, err := startOwnedCLIProcess(cmd, true)
	if err != nil {
		return errors.New("托管 CLI 终端启动失败")
	}
	s.tui, s.terminal = tui, tty
	s.input.Lock()
	s.input.terminal = tty
	s.input.Unlock()
	if pty.InheritSize(os.Stdin, tty) != nil {
		_ = pty.Setsize(tty, &pty.Winsize{Rows: 40, Cols: 120})
	}
	output := s.output
	if output == nil {
		output = os.Stdout
	}
	go func() { _, _ = io.Copy(output, tty) }()
	readyCtx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	loaded := false
	for !loaded {
		if s.tui.exited() || s.server.exited() {
			return errors.New("托管终端未能加载原会话")
		}
		thread, readErr := readHandoffCLIThread(readyCtx, s.socket, s.id, s.provider)
		loaded = readErr == nil && (thread.Status.Type == "idle" || thread.Status.Type == "active" || thread.Status.Type == "systemError")
		if loaded {
			break
		}
		select {
		case <-readyCtx.Done():
			return errors.New("托管终端加载原会话超时")
		case <-time.After(150 * time.Millisecond):
		}
	}
	if err = writeState(s.r.cliRunnerPath(), cliRunnerState{ID: s.id, PID: os.Getpid(), ServerPID: s.server.cmd.Process.Pid, ClientPID: s.tui.cmd.Process.Pid, Socket: s.socket, Ready: true}); err != nil {
		return err
	}
	return nil
}
func (s *cliHandoffSupervisor) stop(ctx context.Context) error {
	s.input.Lock()
	s.input.terminal = nil
	s.input.Unlock()
	return s.stopDetached(ctx)
}
func (s *cliHandoffSupervisor) stopForHandoff(ctx context.Context) error {
	// Serialize the final input check with the beginning of shutdown. Otherwise
	// a user can type after Observe and still have their terminal terminated.
	s.input.Lock()
	defer s.input.Unlock()
	if !s.armed || s.input.typing || s.input.epoch != s.armedEpoch {
		return handoffSafeError("检测到手动输入")
	}
	s.input.terminal = nil
	if err := s.tui.stop(ctx); err != nil {
		return handoffSafeError("托管终端未能退出")
	}
	if s.terminal != nil {
		_ = s.terminal.Close()
		s.terminal = nil
	}
	// Acknowledging the signal is not an exit confirmation. The coordinator
	// observes the owned process in waiting_exit before switching credentials.
	if err := s.server.requestStop(); err != nil {
		return handoffSafeError("托管后端未能收到退出请求")
	}
	return nil
}
func (s *cliHandoffSupervisor) stopDetached(ctx context.Context) error {
	if err := s.tui.stop(ctx); err != nil {
		return handoffSafeError("托管终端未能退出")
	}
	if s.terminal != nil {
		_ = s.terminal.Close()
		s.terminal = nil
	}
	// A real backend can take longer than the terminal to close network and
	// session resources. The result is still checked before touching auth.
	if err := s.server.stopWithTimeout(ctx, 10*time.Second); err != nil {
		return handoffSafeError("托管后端未能退出")
	}
	return nil
}
func (s *cliHandoffSupervisor) stopped(context.Context) error {
	if !s.tui.exited() || !s.server.exited() {
		return errors.New("托管实例仍在运行")
	}
	return nil
}
func (s *cliHandoffSupervisor) inputs() (uint64, bool) {
	s.input.Lock()
	defer s.input.Unlock()
	return s.input.epoch, s.input.typing
}
func (s *cliHandoffSupervisor) armedInputUnchanged() bool {
	epoch, typing := s.inputs()
	return s.armed && !typing && epoch == s.armedEpoch
}
func (s *cliHandoffSupervisor) stoppedForHandoff(ctx context.Context) error {
	if !s.armedInputUnchanged() {
		return errors.New("检测到手动输入")
	}
	return s.stopped(ctx)
}

func (s *cliHandoffSupervisor) identifyAccounts(ctx context.Context) error {
	current, err := s.r.scopeCredential("cli", time.Now())
	if err != nil {
		return err
	}
	accounts := s.r.automationAccounts(ctx)
	credentials := s.r.quotaCredentials(time.Now())
	source := ""
	for _, account := range accounts {
		credential, ok := credentials[account.ID]
		if ok && credentialIdentity(credential) == credentialIdentity(current) {
			if source != "" {
				return errors.New("当前账号映射不唯一")
			}
			source = account.ID
		}
	}
	if source == "" {
		return errors.New("当前 CLI 账号未匹配账号库")
	}
	target := ""
	for _, account := range accounts {
		if account.ID != source {
			if target != "" {
				return errors.New("当前仅支持两个账号")
			}
			target = account.ID
		}
	}
	if target == "" {
		return errors.New("缺少备用账号")
	}
	s.sourceID, s.targetID = source, target
	return nil
}
func (s *cliHandoffSupervisor) backupEvidence(ctx context.Context, threshold float64) (AccountQuota, bool) {
	if s.manager != nil {
		path, err := s.r.handoffAuthPath("cli")
		if err != nil {
			return AccountQuota{}, false
		}
		raw, err := readProtectedHandoffFile(path)
		if err != nil || !s.r.handoffCurrentMatchesAccount(raw, s.sourceID) {
			return AccountQuota{}, false
		}
		if s.preparedOK && sha256.Sum256(raw) != s.prepared.currentHash {
			return AccountQuota{}, false
		}
		if s.manager.ready(ctx, s.targetID, "cli") != nil {
			return AccountQuota{}, false
		}
		q, err := s.manager.quota(ctx, s.targetID)
		if err != nil || !resumeQuotaReady(q) || !handoffTargetQuotaOK(q, threshold) {
			return q, false
		}
		s.prepared = handoffAuthPreparation{scope: "cli", accountID: s.targetID, currentHash: sha256.Sum256(raw)}
		s.preparedOK = true
		return q, true
	}
	prep, candidate, err := s.r.prepareHandoffAuth("cli", s.targetID, time.Now())
	if err != nil {
		return AccountQuota{}, false
	}
	q := queryAccountQuota(ctx, s.client, candidate.quotaCredential())
	if !resumeQuotaReady(q) || !handoffTargetQuotaOK(q, threshold) {
		return q, false
	}
	if !s.preparedOK {
		s.prepared = prep
		s.preparedOK = true
	} else if prep.currentHash != s.prepared.currentHash || prep.candidateHash != s.prepared.candidateHash || prep.accountID != s.prepared.accountID {
		return q, false
	}
	return q, true
}
func (s *cliHandoffSupervisor) currentIdentityOK(now time.Time) bool {
	c, err := s.r.scopeCredential("cli", now)
	if err != nil {
		return false
	}
	creds := s.r.quotaCredentials(now)
	source, ok := creds[s.sourceID]
	return ok && credentialIdentity(c) == credentialIdentity(source)
}
func (s *cliHandoffSupervisor) Observe(ctx context.Context, state HandoffState) (HandoffObservation, error) {
	o := HandoffObservation{TargetMatches: state.Scope == "cli" && state.SessionID == s.id, SourceAccount: s.sourceID, TargetAccount: s.targetID}
	if !o.TargetMatches {
		return o, nil
	}
	if s.armed {
		epoch, typing := s.inputs()
		o.UserIntervened = typing || epoch != s.armedEpoch
	}
	if state.Stage == "waiting_exit" || state.Stage == "ready_switch" || state.Stage == "waiting_switch" || state.Stage == "ready_restart" {
		o.OldExited = s.stopped(ctx) == nil
		o.ExitTimedOut = state.Stage == "waiting_exit" && state.ActionAt > 0 && time.Now().Unix()-state.ActionAt >= 45
		if state.Stage == "ready_switch" {
			q, ok := s.backupEvidence(ctx, state.Threshold)
			o.TargetCredentialReady = ok
			o.TargetQuotaKnown = ok
			o.TargetWeeklyUsable = ok
			o.TargetRemaining, _ = handoffWindow(&q, 18000)
		}
		if state.Stage == "waiting_switch" || state.Stage == "ready_restart" {
			o.SwitchVerified = s.authTargetInstalled()
		}
		return o, nil
	}
	if state.Stage == "waiting_restart" || state.Stage == "ready_continue" || state.Stage == "waiting_continue" {
		o.RestartReady = s.server != nil && !s.server.exited() && s.tui != nil && !s.tui.exited()
		if !o.RestartReady {
			return o, nil
		}
		o.IdentityVerified = s.verifyTarget(ctx) == nil
		thread, err := readHandoffCLIThread(ctx, s.socket, s.id, s.provider)
		if err != nil {
			return o, nil
		}
		turn, ok := thread.latest()
		o.TargetMatches = ok
		if state.Stage == "waiting_restart" || state.Stage == "ready_continue" {
			o.TargetMatches = ok && turn.ID == state.TurnID && thread.Status.Type == "idle"
			o.SettingsVerified = s.actualSettingsMatch(ctx, thread, state)
		}
		if state.Stage == "waiting_continue" {
			if ok && s.continuedTurn != "" && turn.ID == s.continuedTurn {
				o.ContinueStarted = turn.Status == "completed" || (turn.Status == "inProgress" && turn.StartedAt != nil && *turn.StartedAt > 0)
				if turn.Status == "failed" || turn.Status == "interrupted" {
					o.UserIntervened = true
				}
			}
		}
		return o, nil
	}
	if s.server == nil || s.server.exited() || s.tui == nil || s.tui.exited() {
		return o, nil
	}
	if !s.currentIdentityOK(time.Now()) {
		o.UserIntervened = true
		return o, nil
	}
	thread, err := readHandoffCLIThread(ctx, s.socket, s.id, s.provider)
	if err != nil {
		return o, err
	}
	turn, ok := thread.latest()
	if !ok {
		return o, nil
	}
	o.TurnID = turn.ID
	if state.Stage == "watching" || state.Stage == "ready_stop" {
		active := thread.Status.Type == "active" && turn.Status == "inProgress" && len(thread.Status.ActiveFlags) == 0
		limited := state.Stage == "watching" && s.seenActiveTurn == turn.ID && (thread.Status.Type == "idle" || thread.Status.Type == "systemError") && turn.Status == "failed" && usageLimitError(turn.Error) && turn.toolsStopped()
		o.Running = active
		if !active && !limited {
			if state.Stage == "ready_stop" {
				o.FinishedNormally = true
			}
			return o, nil
		}
		epoch, typing := s.inputs()
		if active {
			s.seenActiveTurn = turn.ID
			s.seenActiveEpoch = epoch
		}
		if limited && (typing || epoch != s.seenActiveEpoch) {
			o.UserIntervened = true
			return o, nil
		}
		settings, e := readHandoffCLISettings(ctx, s.socket, s.id, turn.ID, false)
		if e != nil {
			o.Warning = "当前轮次的执行设置暂不可确认，等待重试"
			return o, nil
		}
		if thread.Cwd != "" && settings.Cwd != thread.Cwd && !sameExistingPath(settings.Cwd, thread.Cwd) {
			o.Warning = "会话工作目录与当前执行目录不一致，等待检查"
			return o, nil
		}
		encoded, e := json.Marshal(settings)
		if e != nil {
			return o, nil
		}
		s.settings = settings
		o.Directory = settings.Cwd
		o.Model = settings.Model
		o.Effort = settings.Effort
		o.Permissions = string(encoded)
		if state.Stage == "watching" {
			s.preparedOK = false
		}
		current, e := s.r.scopeCredential("cli", time.Now())
		if e == nil {
			cq := queryAccountQuota(ctx, s.client, current)
			o.CurrentRemaining, o.CurrentQuotaKnown = handoffWindow(&cq, 18000)
		}
		if !o.CurrentQuotaKnown {
			o.Warning = "当前账号额度暂不可读取，等待重试"
		}
		// The real-integration harness can simulate reaching the threshold only
		// in an explicitly marked isolated home. The account and quota queries
		// still use the actual isolated backend and real service.
		if testHome := os.Getenv("CODEX_HELPER_TEST_HOME"); testHome != "" && testHome == s.r.Home && os.Getenv("CODEX_HELPER_TEST_TRIGGER") == "1" {
			if marker, e := os.Stat(filepath.Join(testHome, ".codex-helper-test-root")); e == nil && marker.Mode().IsRegular() && o.CurrentQuotaKnown {
				o.CurrentRemaining = state.Threshold / 2
			}
		}
		epoch, typing = s.inputs()
		if limited && (typing || epoch != s.seenActiveEpoch) {
			o.UserIntervened = true
			return o, nil
		}
		s.observedEpoch = epoch
		if typing {
			o.Running = false
		}
		if (!o.Running && !limited) || !o.CurrentQuotaKnown || o.CurrentRemaining >= state.Threshold {
			return o, nil
		}
		q, ready := s.backupEvidence(ctx, state.Threshold)
		o.TargetCredentialReady = ready
		o.TargetQuotaKnown = ready
		o.TargetWeeklyUsable = ready
		o.TargetRemaining, _ = handoffWindow(&q, 18000)
		if limited && ready {
			o.OtherSessionsIdle, err = otherHandoffCLIThreadsIdle(ctx, s.socket, s.id, s.provider)
			if err != nil {
				return o, err
			}
			if o.OtherSessionsIdle {
				s.armed = true
				s.armedEpoch = epoch
				o.AlreadyLimited = true
				o.StopConfirmed = true
				o.StoppedForHandoff = true
			}
		}
		return o, nil
	}
	o.TurnID = turn.ID
	o.StopConfirmed = turn.ID == state.TurnID && (thread.Status.Type == "idle" || thread.Status.Type == "systemError") && (turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted") && turn.toolsStopped()
	if s.armed && o.StopConfirmed {
		switch turn.Status {
		case "completed":
			o.StoppedForHandoff = turn.confirmsHandoffResponse(handoffSteerClientID(s.id, state.TurnID))
			if !o.StoppedForHandoff {
				o.FinishedNormally = true
			}
		case "interrupted":
			o.StoppedForHandoff = state.InterruptSent
			if !o.StoppedForHandoff {
				o.UserIntervened = true
			}
		case "failed":
			o.StoppedForHandoff = usageLimitError(turn.Error)
			if !o.StoppedForHandoff {
				o.UserIntervened = true
			}
		}
	}
	o.StopTimedOut = thread.Status.Type == "active" && turn.Status == "inProgress" && state.ActionAt > 0 && time.Now().Unix()-state.ActionAt >= 60
	if o.StopConfirmed {
		if !s.actualSettingsMatch(ctx, thread, state) {
			o.UserIntervened = true
			return o, nil
		}
		o.OtherSessionsIdle, err = otherHandoffCLIThreadsIdle(ctx, s.socket, s.id, s.provider)
		if err != nil {
			return o, err
		}
	}
	q, ready := s.backupEvidence(ctx, state.Threshold)
	o.TargetCredentialReady = ready
	o.TargetQuotaKnown = ready
	o.TargetWeeklyUsable = ready
	o.TargetRemaining, _ = handoffWindow(&q, 18000)
	return o, nil
}
func (s *cliHandoffSupervisor) authTargetInstalled() bool {
	if s.manager != nil {
		return s.r.nativeHandoffInstalled("cli", s.targetID)
	}
	path, err := s.r.handoffAuthPath("cli")
	if err != nil {
		return false
	}
	data, err := readProtectedHandoffFile(path)
	return err == nil && s.r.handoffCurrentMatchesAccount(data, s.targetID)
}
func (s *cliHandoffSupervisor) settingsMatch(state HandoffState) bool {
	var settings cliTurnSettings
	if json.Unmarshal([]byte(state.Permissions), &settings) != nil || settings.TurnID != state.TurnID || settings.Cwd != state.Directory || settings.Model != state.Model || settings.Effort != state.Effort || len(settings.Approval) == 0 || len(settings.Sandbox) == 0 {
		return false
	}
	current, err := json.Marshal(s.settings)
	return err == nil && string(current) == state.Permissions
}
func (s *cliHandoffSupervisor) actualSettingsMatch(ctx context.Context, thread cliHandoffThread, state HandoffState) bool {
	if !s.settingsMatch(state) || (thread.Cwd != state.Directory && !sameExistingPath(thread.Cwd, state.Directory)) {
		return false
	}
	current, err := readHandoffCLISettings(ctx, s.socket, s.id, state.TurnID, true)
	if err != nil {
		return false
	}
	encoded, err := json.Marshal(current)
	return err == nil && string(encoded) == state.Permissions
}
func (s *cliHandoffSupervisor) verifyTarget(ctx context.Context) error {
	if !s.authTargetInstalled() {
		return errors.New("目标认证与账号不一致")
	}
	var store handoffManagedStore
	if readJSON(s.r.managedAccountsStorePath(), &store) != nil {
		return errors.New("账号信息不可读取")
	}
	account, ok := store.Accounts[s.targetID]
	if !ok || strings.TrimSpace(account.Email) == "" {
		return errors.New("账号邮箱不可核验")
	}
	// account/read does not return workspace ID. An email shared by the two
	// accounts is insufficient to distinguish the running identity.
	source, exists := store.Accounts[s.sourceID]
	if !exists || strings.TrimSpace(source.Email) == "" || strings.EqualFold(strings.TrimSpace(source.Email), strings.TrimSpace(account.Email)) {
		return errors.New("两个账号的运行身份无法仅通过邮箱区分")
	}
	return withHandoffCLIRPC(ctx, s.socket, func(rpc *warmupRPC) error {
		raw, e := rpc.call("account/read", map[string]any{"refreshToken": false})
		if e != nil {
			return e
		}
		var result struct {
			Account struct {
				Type  string  `json:"type"`
				Email *string `json:"email"`
			} `json:"account"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Account.Type != "chatgpt" || result.Account.Email == nil || !strings.EqualFold(strings.TrimSpace(*result.Account.Email), strings.TrimSpace(account.Email)) {
			return errors.New("新实例账号不匹配")
		}
		_, e = rpc.call("account/rateLimits/read", map[string]any{})
		return e
	})
}
func (s *cliHandoffSupervisor) Execute(ctx context.Context, action HandoffAction, state HandoffState) error {
	if action != HandoffRequestStop && !s.armedInputUnchanged() {
		return handoffSafeError("检测到手动输入")
	}
	switch action {
	case HandoffRequestStop:
		s.input.Lock()
		defer s.input.Unlock()
		if s.input.typing || s.input.epoch != s.observedEpoch {
			return errors.New("检测到手动输入")
		}
		if err := requestHandoffCLISteer(ctx, s.socket, s.id, state.TurnID); err != nil {
			return err
		}
		s.armed = true
		s.armedEpoch = s.input.epoch
		return nil
	case HandoffInterrupt:
		s.input.Lock()
		defer s.input.Unlock()
		if !s.armed || s.input.typing || s.input.epoch != s.armedEpoch {
			return errors.New("检测到手动输入")
		}
		return requestHandoffCLIInterrupt(ctx, s.socket, s.id, state.TurnID)
	case HandoffExit:
		return s.stopForHandoff(ctx)
	case HandoffSwitch:
		if !s.preparedOK {
			return errors.New("备用认证未准备")
		}
		if s.manager != nil {
			if err := s.stoppedForHandoff(ctx); err != nil {
				return err
			}
			path, err := s.r.handoffAuthPath("cli")
			if err != nil {
				return err
			}
			raw, err := readProtectedHandoffFile(path)
			if err != nil || sha256.Sum256(raw) != s.prepared.currentHash || !s.r.handoffCurrentMatchesAccount(raw, s.sourceID) || s.r.handoffCancelled(state) {
				return errors.New("原认证或授权已变化")
			}
			if err := s.manager.apply(ctx, s.targetID, "cli"); err != nil {
				return err
			}
			if !s.authTargetInstalled() {
				return errors.New("目标认证结果待核查")
			}
			return nil
		}
		_, err := s.r.commitHandoffAuth(ctx, s.prepared, state.Threshold, s.stoppedForHandoff, func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, s.client, c) })
		return err
	case HandoffRestart:
		if err := s.start(); err != nil {
			return err
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			thread, err := readHandoffCLIThread(ctx, s.socket, s.id, s.provider)
			if err == nil {
				turn, ok := thread.latest()
				if ok && turn.ID == state.TurnID && thread.Status.Type == "idle" {
					if err := s.verifyTarget(ctx); err != nil {
						return err
					}
					if !s.settingsMatch(state) {
						return errors.New("原执行设置不可读取")
					}
					if err := applyHandoffCLISettings(ctx, s.socket, s.id, s.settings); err != nil {
						return err
					}
					// settings/update acknowledges enqueueing the override. Its
					// effect is asynchronous, so verify a fresh snapshot before
					// allowing continuation instead of trusting the acknowledgement.
					for time.Now().Before(deadline) {
						current, e := readHandoffCLIThread(ctx, s.socket, s.id, s.provider)
						if e == nil && current.Status.Type == "idle" && s.actualSettingsMatch(ctx, current, state) {
							return nil
						}
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(100 * time.Millisecond):
						}
					}
					return errors.New("新实例的执行设置不一致")
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		return errors.New("新实例未能加载原会话")
	case HandoffContinue:
		if !s.settingsMatch(state) {
			return errors.New("原执行设置不可确认")
		}
		settings := s.settings
		params := map[string]any{"threadId": s.id, "cwd": settings.Cwd, "model": settings.Model, "approvalPolicy": settings.Approval, "input": []any{map[string]any{"type": "text", "text": "继续之前为切换账号而中断的任务。先核对当前文件和运行状态，再接着工作。"}}}
		var profile struct {
			ID string `json:"id"`
		}
		if len(settings.ActivePermissionProfile) > 0 && json.Unmarshal(settings.ActivePermissionProfile, &profile) == nil && profile.ID != "" {
			params["permissions"] = profile.ID
		} else {
			params["sandboxPolicy"] = settings.Sandbox
		}
		if settings.Effort != "default" {
			params["effort"] = settings.Effort
		}
		if len(settings.Reviewer) > 0 && string(settings.Reviewer) != "null" {
			params["approvalsReviewer"] = settings.Reviewer
		}
		if len(settings.Collaboration) > 0 && string(settings.Collaboration) != "null" {
			mode, err := collaborationForHandoff(settings)
			if err != nil {
				return err
			}
			params["collaborationMode"] = mode
		}
		if len(settings.ServiceTier) > 0 && string(settings.ServiceTier) != "null" {
			params["serviceTier"] = settings.ServiceTier
		}
		if len(settings.DisabledPlugins) > 0 && string(settings.DisabledPlugins) != "null" {
			params["disabledPluginIds"] = settings.DisabledPlugins
		}
		s.input.Lock()
		defer s.input.Unlock()
		if !s.armed || s.input.typing || s.input.epoch != s.armedEpoch {
			return errors.New("检测到手动输入")
		}
		return withHandoffCLIRPC(ctx, s.socket, func(rpc *warmupRPC) error {
			raw, e := rpc.call("turn/start", params)
			if e != nil {
				return e
			}
			var response struct {
				Turn struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(raw, &response) != nil || response.Turn.ID == "" || response.Turn.ID == state.TurnID || response.Turn.Status != "inProgress" {
				return errors.New("继续轮次未确认")
			}
			s.continuedTurn = response.Turn.ID
			return nil
		})
	default:
		return errors.New("未知交接动作")
	}
}

// PrepareCLIHandoff binds authorization before a terminal is launched. This
// lets changing the GUI selection cancel even while Terminal is starting.
func (r Reader) PrepareCLIHandoff(ctx context.Context, id string, threshold float64) (HandoffState, error) {
	if !validHandoffThreshold(threshold) {
		return r.HandoffStatus(), errors.New("阈值无效")
	}
	lock, err := lockWarmup(filepath.Join(filepath.Dir(r.resumePath()), "cli-runner.lock"))
	if err != nil {
		return r.HandoffStatus(), errors.New("已有托管终端，请先关闭")
	}
	defer lock()
	if err := r.ensureNoLivePreviousCLIRunner(); err != nil {
		return r.HandoffStatus(), err
	}
	snapshot := r.Snapshot(ctx)
	found := false
	for _, item := range snapshot.Sessions {
		if item.Scope == "cli" && item.ID == id && !item.Archived && item.Project != "" {
			found = true
			break
		}
	}
	if !found {
		return r.HandoffStatus(), errors.New("所选 CLI 会话或工作目录不可用")
	}
	if _, ok := r.managedResumeProvider("cli"); !ok {
		return r.HandoffStatus(), errors.New("CLI 官方账号配置不可确认")
	}
	if _, err := r.scopeCredential("cli", time.Now()); err != nil {
		return r.HandoffStatus(), err
	}
	if r.nativeAccountPreview() {
		probe := &cliHandoffSupervisor{r: r}
		if err := probe.identifyAccounts(ctx); err != nil {
			return r.HandoffStatus(), err
		}
		manager, err := r.startNativeHandoffManager(ctx)
		if err != nil {
			return r.HandoffStatus(), err
		}
		defer manager.Close()
		if err := manager.ready(ctx, probe.targetID, "cli"); err != nil {
			return r.HandoffStatus(), err
		}
		q, err := manager.quota(ctx, probe.targetID)
		if err != nil || !handoffTargetQuotaOK(q, threshold) {
			return r.HandoffStatus(), errors.New("备用账号额度或登录不可用")
		}
	}
	return r.ConfigureHandoff("cli", id, threshold, true)
}

func (r Reader) RunCLIHandoff(ctx context.Context, id string, threshold float64, authorizationID string) error {
	if !validHandoffThreshold(threshold) {
		return errors.New("阈值无效")
	}
	authorized := r.HandoffStatus()
	if !authorized.Enabled || authorized.Stage != "watching" || authorized.Scope != "cli" || authorized.SessionID != id || authorized.Threshold != threshold || authorized.AuthorizationID == "" || authorized.AuthorizationID != authorizationID || r.handoffCancelled(authorized) {
		return errors.New("该会话的自动切号授权已变化，请重新开启")
	}
	defer func() {
		state := r.HandoffStatus()
		if state.Enabled && state.AuthorizationID == authorizationID {
			_, _ = r.ConfigureHandoff("", "", 0, false)
		}
	}()
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.resumePath()), "cli-runner.lock"))
	if err != nil {
		return errors.New("已有托管终端，请先关闭")
	}
	defer unlock()
	if err := r.ensureNoLivePreviousCLIRunner(); err != nil {
		return err
	}
	snapshot := r.Snapshot(ctx)
	var selected *Session
	for _, item := range snapshot.Sessions {
		if item.Scope == "cli" && item.ID == id && !item.Archived {
			v := item
			selected = &v
			break
		}
	}
	if selected == nil || selected.Project == "" {
		return errors.New("所选 CLI 会话或工作目录不可用")
	}
	provider, ok := r.managedResumeProvider("cli")
	if !ok {
		return errors.New("CLI 官方账号配置不可确认")
	}
	exe := codexExecutable(r.Home)
	if exe == "" {
		return errors.New("未找到 Codex CLI")
	}
	root, err := os.MkdirTemp("/tmp", "codex-helper-cli-")
	if err != nil {
		return errors.New("无法创建托管目录")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &cliHandoffSupervisor{r: r, ctx: ctx, id: id, cwd: selected.Project, exe: exe, socket: filepath.Join(root, "control.sock"), env: cliEnvironment(r.Home), provider: provider, input: &cliHandoffInput{}, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	if r.nativeAccountPreview() {
		manager, e := r.startNativeHandoffManager(ctx)
		if e != nil {
			_ = os.RemoveAll(root)
			return e
		}
		defer manager.Close()
		s.manager = manager
	}
	if err = s.identifyAccounts(ctx); err != nil {
		return err
	}
	defer func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = s.stop(stopCtx)
		if s.server.exited() && s.tui.exited() {
			_ = os.Remove(r.cliRunnerPath())
			_ = os.RemoveAll(root)
		} else {
			fmt.Fprintln(os.Stderr, "托管进程仍未退出；保留控制信息，交接保持暂停。")
			if s.server != nil && s.server.cmd.Process != nil {
				fmt.Fprintln(os.Stderr, "托管后端进程号：", s.server.cmd.Process.Pid)
			}
		}
	}()
	if err = s.start(); err != nil {
		return err
	}
	current := r.HandoffStatus()
	if current != authorized || r.handoffCancelled(current) {
		return errors.New("启动时自动切号授权已取消")
	}
	fmt.Println("Codex 小助手：自动切号已绑定此托管 CLI 会话。关闭本终端停止托管。")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, e := term.MakeRaw(int(os.Stdin.Fd())); e == nil {
			defer term.Restore(int(os.Stdin.Fd()), old)
		}
	}
	go func() {
		b := make([]byte, 4096)
		for {
			n, e := os.Stdin.Read(b)
			if n > 0 {
				s.input.forward(b[:n])
			}
			if e != nil {
				return
			}
		}
	}()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-resize:
				s.input.Lock()
				if s.input.terminal != nil {
					_ = pty.InheritSize(os.Stdin, s.input.terminal)
				}
				s.input.Unlock()
			}
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastWatching := time.Time{}
	watchInterval := 30 * time.Second
	if testHome := os.Getenv("CODEX_HELPER_TEST_HOME"); testHome != "" && testHome == r.Home {
		if marker, e := os.Stat(filepath.Join(testHome, ".codex-helper-test-root")); e == nil && marker.Mode().IsRegular() {
			watchInterval = 2 * time.Second
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		state := r.HandoffStatus()
		if state.Enabled && state.Scope == "cli" && state.SessionID == id {
			if state.Stage != "watching" || time.Since(lastWatching) >= watchInterval {
				if state.Stage == "watching" {
					lastWatching = time.Now()
				}
				stepCtx, done := context.WithTimeout(ctx, 45*time.Second)
				updated, e := r.AdvanceHandoff(stepCtx, s)
				done()
				if e != nil {
					fmt.Fprintln(os.Stderr, "自动切号检查未完成：", e)
				}
				if updated.Stage == "uncertain" {
					fmt.Fprintln(os.Stderr, updated.Message)
				}
				if updated.Stage == "completed" {
					var finishErr error
					if s.manager != nil {
						finishErr = s.verifyAccountID(ctx, s.targetID)
					} else {
						finishErr = r.finishHandoffAuth(ctx, s.verifyAccountID)
					}
					if err := finishErr; err != nil {
						fmt.Fprintln(os.Stderr, "认证恢复副本等待核验清理：", err)
						_, _ = r.handoffCleanupUncertain(updated)
					} else if _, rearmed, err := r.rearmCompletedHandoff(updated); err != nil {
						fmt.Fprintln(os.Stderr, "切号已完成，重新监控未开启：", err)
					} else if rearmed {
						s.sourceID, s.targetID = s.targetID, s.sourceID
						s.preparedOK = false
						s.armed = false
						s.continuedTurn = ""
						s.seenActiveTurn = ""
						lastWatching = time.Time{}
					}
				}
			}
		}
		if s.tui != nil && s.tui.exited() {
			current := r.HandoffStatus()
			if !current.Enabled || (current.Stage != "waiting_exit" && current.Stage != "ready_switch" && current.Stage != "waiting_switch" && current.Stage != "ready_restart" && current.Stage != "waiting_restart") {
				if current.Enabled && current.AuthorizationID == authorizationID {
					_, _ = r.ConfigureHandoff("", "", 0, false)
				}
				if !s.server.exited() && !s.server.shutdownRequested() {
					stopCtx, done := context.WithTimeout(ctx, 10*time.Second)
					err := s.stop(stopCtx)
					done()
					if err != nil {
						fmt.Fprintln(os.Stderr, "托管终端已退出，但后端尚未确认退出；请等待。")
					}
				}
				if s.server.exited() {
					return nil
				}
			}
		}
	}
}
func (s *cliHandoffSupervisor) verifyAccountID(ctx context.Context, id string) error {
	if id != s.targetID {
		return errors.New("账号不匹配")
	}
	return s.verifyTarget(ctx)
}

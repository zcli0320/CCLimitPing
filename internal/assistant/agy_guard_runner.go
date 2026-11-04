package assistant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type agyTerminalInputTracker struct {
	sync.Mutex
	terminal    io.Writer
	epoch       uint64
	typing      bool
	userStopped bool
	lastInputAt time.Time
}

func (t *agyTerminalInputTracker) forwardUserBytes(b []byte) {
	t.Lock()
	defer t.Unlock()
	t.epoch++
	t.lastInputAt = time.Now()
	// Check if user pressed Ctrl+C (0x03) or Esc (0x1b without bracketed paste / arrow sequence)
	if isUserStopSequence(b) {
		t.userStopped = true
		t.typing = false
	} else if strings.ContainsAny(string(b), "\r\n") {
		// User submitted a prompt
		t.typing = false
		t.userStopped = false
	} else {
		t.typing = true
	}
	if t.terminal != nil {
		_, _ = t.terminal.Write(b)
	}
}

func isUserStopSequence(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, ch := range b {
		if ch == 0x03 { // Ctrl+C
			return true
		}
	}
	// Bare Esc key (1 byte 0x1b, not an ANSI escape sequence like \x1b[A or \x1b[200~)
	if len(b) == 1 && b[0] == 0x1b {
		return true
	}
	return false
}

func (t *agyTerminalInputTracker) snapshot() (epoch uint64, typing bool, userStopped bool, lastInput time.Time) {
	t.Lock()
	defer t.Unlock()
	return t.epoch, t.typing, t.userStopped, t.lastInputAt
}

// ResolveAGYExecutable locates the user's agy executable in PATH without modifying
// any binary or proxy wrappers.
func ResolveAGYExecutable() (string, error) {
	if override := os.Getenv("CODEX_HELPER_AGY_BIN"); filepath.IsAbs(override) {
		if info, err := os.Stat(override); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return override, nil
		}
	}
	pathEnv := os.Getenv("PATH")
	home, _ := os.UserHomeDir()
	searchDirs := []string{
		filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		filepath.Join(home, ".antigravity", "bin"),
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir != "" {
			searchDirs = append(searchDirs, dir)
		}
	}
	for _, dir := range searchDirs {
		candidate := filepath.Join(dir, "agy")
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("未找到 AGY CLI 可执行文件（已检查 ~/.local/bin/agy, /opt/homebrew/bin/agy 及 PATH）")
}

func generateAGYLaunchID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("agy-%s-%s", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

func fileInode(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino), true
	}
	return 0, false
}

// RunManagedAGYSession launches an interactive AGY CLI session inside an owned
// PTY, attaches a dedicated `--log-file`, and runs the bounded monitor/recovery
// loop for the lifetime of this process.
func (r Reader) RunManagedAGYSession(ctx context.Context, folder string, extraArgs []string) (int, error) {
	if folder == "" {
		return 1, errors.New("工作目录不能为空")
	}
	info, err := os.Stat(folder)
	if err != nil || !info.IsDir() {
		return 1, fmt.Errorf("工作目录不存在或不是文件夹：%s", folder)
	}

	agyBin, err := ResolveAGYExecutable()
	if err != nil {
		return 127, err
	}

	launchID := generateAGYLaunchID()
	if err := os.MkdirAll(r.agyGuardLogsDir(), 0o700); err != nil {
		return 1, err
	}
	if err := os.MkdirAll(r.agyGuardSessionsDir(), 0o700); err != nil {
		return 1, err
	}
	logFile := filepath.Join(r.agyGuardLogsDir(), launchID+".log")
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return 1, err
	}
	_ = f.Close()
	initialInode, _ := fileInode(logFile)

	// Preserve existing launch parameters and permission semantics:
	// agy --dangerously-skip-permissions --add-dir <folder> --log-file <logFile> [extraArgs...]
	argv := []string{"--dangerously-skip-permissions", "--add-dir", folder, "--log-file", logFile}
	argv = append(argv, extraArgs...)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.Command(agyBin, argv...)
	cmd.Dir = folder
	cmd.Env = os.Environ()

	tty, err := pty.Start(cmd)
	if err != nil {
		return 1, fmt.Errorf("无法启动受管 AGY 终端：%w", err)
	}
	defer func() { _ = tty.Close() }()

	childPID := 0
	if cmd.Process != nil {
		childPID = cmd.Process.Pid
	}

	proxyInfo := InspectProxyEnv(cmd.Env)
	proxyHealthyAtStart := false
	if proxyInfo.Conflict == "" && proxyInfo.HostPort != "" {
		proxyHealthyAtStart = CheckProxyReachable(proxyInfo.HostPort, 800*time.Millisecond)
	}
	now := time.Now().UTC()
	session := AGYGuardSession{
		LaunchID:         launchID,
		RunnerPID:        os.Getpid(),
		ChildPID:         childPID,
		Folder:           folder,
		LogFile:          logFile,
		LogInode:         initialInode,
		State:            AGYGuardStateObserving,
		StateLabel:       AGYGuardStateObserving.Label(),
		Message:          "受管终端已启动，等待绑定当前会话 ID…",
		ProxyURL:         proxyInfo.ProxyURL,
		ProxyConflict:    proxyInfo.Conflict,
		ProxyHealthy:     proxyHealthyAtStart,
		NetworkDigest:    CurrentNetworkDigest(),
		HandledIncidents: map[string]bool{},
		StartedAt:        now,
		UpdatedAt:        now,
	}
	_ = r.writeAGYSession(session)

	// Handle terminal window size propagation (SIGWINCH)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-resize:
				_ = pty.InheritSize(os.Stdin, tty)
			}
		}
	}()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		_ = pty.InheritSize(os.Stdin, tty)
		if oldState, e := term.MakeRaw(int(os.Stdin.Fd())); e == nil {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
		}
	} else {
		_ = pty.Setsize(tty, &pty.Winsize{Rows: 40, Cols: 120})
	}

	tracker := &agyTerminalInputTracker{terminal: tty}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, readErr := os.Stdin.Read(buf)
				if n > 0 {
					tracker.forwardUserBytes(buf[:n])
				}
				if readErr != nil {
					return
				}
			}
		}()
	}

	outputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(os.Stdout, tty)
		close(outputDone)
	}()

	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		r.monitorManagedAGYLoop(ctx, &session, tracker)
	}()

	waitErr := cmd.Wait()
	cancel()
	select {
	case <-outputDone:
	case <-time.After(500 * time.Millisecond):
	}
	<-monitorDone

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	r.finalizeExitedAGYSession(&session, logFile, exitCode)
	_ = r.writeAGYSession(session)
	return exitCode, nil
}

// finalizeExitedAGYSession sets the final state after the AGY child exited.
// Never claim task completion merely from exitCode == 0.
func (r Reader) finalizeExitedAGYSession(session *AGYGuardSession, logFile string, exitCode int) {
	finalLog, logErr := ParseAGYDedicatedLog(logFile)
	currentInode, inodeOK := fileInode(logFile)
	logUsable := logErr == nil && inodeOK && session.LogInode != 0 && currentInode == session.LogInode
	session.Exited = true
	session.ExitCode = exitCode
	if logUsable && finalLog.ActiveConversationID != "" {
		session.ConversationID = finalLog.ActiveConversationID
	}
	switch {
	case !logUsable && !r.agyTurnFinishedAtExit(session.ConversationID):
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = fmt.Sprintf("进程已退出（退出码 %d），但专用日志不可读取或已替换，无法确认最后一轮是否完成", exitCode)
		session.Message = session.LastReason
	case logUsable && finalLog.LastTurnStartedLine > 0 && finalLog.LastTurnCompletedLine < finalLog.LastTurnStartedLine &&
		!r.agyTurnFinishedAtExit(session.ConversationID):
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = fmt.Sprintf("进程已退出（退出码 %d），但最后一轮生成未见完成信号，不凭退出码宣告任务完成", exitCode)
		session.Message = session.LastReason
	default:
		session.State = AGYGuardStateIdle
		session.Message = fmt.Sprintf("AGY CLI 已退出（退出码 %d）", exitCode)
	}
}

// agyTurnFinishedAtExit reads AGY's DB row and transcript for the bound
// conversation; see agyTurnFinishedInStore.
func (r Reader) agyTurnFinishedAtExit(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	db, dbOK := r.readAGYConversationSummary(ctx, conversationID)
	return agyTurnFinishedInStore(dbOK, db, r.readAGYTranscriptSummary(conversationID))
}

func (r Reader) monitorManagedAGYLoop(ctx context.Context, session *AGYGuardSession, tracker *agyTerminalInputTracker) {
	pollInterval := 2 * time.Second
	if v := os.Getenv("CODEX_HELPER_AGY_POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 50 {
			pollInterval = time.Duration(ms) * time.Millisecond
		}
	}

	var proxyHealthySince time.Time
	if session.ProxyHealthy && !session.StartedAt.IsZero() {
		proxyHealthySince = session.StartedAt
	}
	lastNetDigest := session.NetworkDigest
	var lastEvaluatedEpoch uint64

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now().UTC()
		policy := r.ReadAGYGuardPolicy()
		childAlive := isProcessAlive(session.ChildPID)

		// Verify dedicated log file inode hasn't been rotated or replaced
		currentInode, inodeOK := fileInode(session.LogFile)
		logReplaced := !inodeOK || (session.LogInode != 0 && currentInode != session.LogInode)

		logAnalysis, logErr := ParseAGYDedicatedLog(session.LogFile)
		logReadable := logErr == nil && !logReplaced

		// Check network interface digest & proxy health
		netDigest := CurrentNetworkDigest()
		if netDigest != lastNetDigest {
			// Wi-Fi <-> Ethernet or network change detected: reset stability timer!
			proxyHealthySince = time.Time{}
			lastNetDigest = netDigest
			session.NetworkDigest = netDigest
		}

		// Use the session's captured proxy or current environment (consistent with launch)
		proxyInfo := InspectProxyEnv(os.Environ())
		if session.ProxyURL != "" && proxyInfo.ProxyURL != session.ProxyURL {
			proxyInfo = InspectProxyEnv([]string{"HTTPS_PROXY=" + session.ProxyURL, "HTTP_PROXY=" + session.ProxyURL})
		}
		proxyReachable := false
		if proxyInfo.Conflict == "" && proxyInfo.HostPort != "" {
			proxyReachable = CheckProxyReachable(proxyInfo.HostPort, 1200*time.Millisecond)
		}
		if proxyReachable {
			if proxyHealthySince.IsZero() {
				proxyHealthySince = now
			}
		} else {
			proxyHealthySince = time.Time{}
		}
		var proxyStableFor time.Duration
		if !proxyHealthySince.IsZero() {
			proxyStableFor = now.Sub(proxyHealthySince)
		}

		activeConv := logAnalysis.ActiveConversationID
		if activeConv == "" {
			activeConv = session.ConversationID
		}
		var dbSummary AGYDBSummary
		var dbReadable bool
		var transcriptSummary AGYTranscriptSummary
		if activeConv != "" {
			dbSummary, dbReadable = r.readAGYConversationSummary(ctx, activeConv)
			transcriptSummary = r.readAGYTranscriptSummary(activeConv)
		}

		activeTools, subErr := CheckChildSubprocesses(session.ChildPID, logAnalysis.ServerPID)
		epoch, typing, userStopped, _ := tracker.snapshot()

		evalIn := AGYEvaluationInput{
			Now:                     now,
			Policy:                  policy,
			Log:                     logAnalysis,
			LogReadable:             logReadable,
			LogRotatedOrReplaced:    logReplaced,
			DB:                      dbSummary,
			DBReadable:              dbReadable,
			Transcript:              transcriptSummary,
			ChildAlive:              childAlive,
			ChildPIDChanged:         false,
			ActiveSubprocesses:      activeTools,
			SubprocessCheckFailed:   subErr != nil,
			UserTyping:              typing,
			UserStopped:             userStopped,
			UserInputEpoch:          epoch,
			LastEvaluatedInputEpoch: lastEvaluatedEpoch,
			Proxy:                   proxyInfo,
			ProxyReachable:          proxyReachable,
			ProxyStableFor:          proxyStableFor,
			BoundConversationID:     session.ConversationID,
			RecoveryTimestamps:      session.RecoveryTimestamps,
			HandledIncidents:        session.HandledIncidents,
		}

		decision := EvaluateAGYGuardState(evalIn)
		lastEvaluatedEpoch = epoch

		session.ConversationID = decision.BoundConversationID
		session.BindingConfirmed = decision.BindingConfirmed
		session.State = decision.State
		session.StateLabel = decision.State.Label()
		session.Message = decision.Message
		if decision.Reason != "" {
			session.LastReason = decision.Reason
		}
		session.ProxyURL = proxyInfo.ProxyURL
		session.ProxyConflict = proxyInfo.Conflict
		session.ProxyHealthy = proxyReachable
		session.ProxyStableSeconds = int(proxyStableFor / time.Second)
		session.ActiveTools = activeTools
		session.BackgroundTaskDoubt = transcriptSummary.HasBackgroundCommand || logAnalysis.UnfinishedBgTaskInLog
		session.UserTyping = typing
		session.AwaitingApproval = logAnalysis.ToolConfirmPendingInTurn || transcriptSummary.PendingAskQuestion
		session.Signals = decision.Signals

		// Prune recovery window count
		windowDur := time.Duration(policy.CooldownMinutes) * time.Minute
		var recent []time.Time
		for _, ts := range session.RecoveryTimestamps {
			if now.Sub(ts) <= windowDur {
				recent = append(recent, ts)
			}
		}
		session.RecoveryTimestamps = recent
		session.RecoveryCountWindow = len(recent)

		// If evaluation hit the recovery cap, persist pause to policy and notify user once
		if !decision.ShouldTrigger && strings.Contains(decision.Message, "已达到") && strings.Contains(decision.Message, "上限") && !policy.Paused {
			_, _ = r.SaveAGYGuardPolicy(func(p *AGYGuardPolicy) {
				p.Paused = true
				p.PauseReason = decision.Reason
			})
		}

		_ = r.writeAGYSession(*session)

		if decision.ShouldTrigger && decision.IncidentKey != "" {
			r.executeBoundedAGYRecovery(ctx, session, tracker, decision, evalIn)
		}
	}
}

// executeBoundedAGYRecovery performs the strict 6-step recovery protocol:
//  1. Re-verify recovery eligibility and no user input
//  2. Send cancel request for current turn (Esc to TUI, never killing AGY process)
//  3. Wait for explicit cancellation confirmation in dedicated log & DB/transcript
//  4. Wait/verify proxy & network stability (preserving existing proxy e.g. 17893/1082)
//  5. Send a single recovery prompt to the same confirmed conversation
//  6. Observe actual progress
func (r Reader) executeBoundedAGYRecovery(
	ctx context.Context,
	session *AGYGuardSession,
	tracker *agyTerminalInputTracker,
	decision AGYEvaluationDecision,
	initial AGYEvaluationInput,
) {
	armedEpoch, typing, userStopped, _ := tracker.snapshot()
	if typing || userStopped || armedEpoch != initial.UserInputEpoch {
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "恢复执行前检测到用户输入或停止操作，已立即撤销自动恢复"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	// Record incident key BEFORE writing anything to the terminal so we never
	// double-send for the same failure even if interrupted mid-flight.
	if session.HandledIncidents == nil {
		session.HandledIncidents = map[string]bool{}
	}
	session.HandledIncidents[decision.IncidentKey] = true
	session.State = AGYGuardStateRecovering
	session.Message = "正在请求取消当前卡死轮次并等待取消确认…"
	_ = r.writeAGYSession(*session)

	// Step 2: Request cancellation of the current in-progress response via Esc (0x1b).
	tracker.Lock()
	if tracker.typing || tracker.userStopped || tracker.epoch != armedEpoch || tracker.terminal == nil {
		tracker.Unlock()
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "取消发送前检测到用户介入，已中止自动恢复"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}
	_, writeErr := io.WriteString(tracker.terminal, "\x1b")
	tracker.Unlock()
	if writeErr != nil {
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "向受管终端发送取消请求失败，已停止自动恢复"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	// Step 3: Wait up to 8 seconds for explicit cancellation confirmation in the dedicated log
	cancelConfirmed := false
	cancelDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(cancelDeadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		curEpoch, curTyping, curStopped, _ := tracker.snapshot()
		if curTyping || curStopped || curEpoch != armedEpoch {
			session.State = AGYGuardStateWaitingUserAction
			session.LastReason = "等待取消确认期间检测到用户操作，已立即撤销待执行的恢复指令"
			session.Message = session.LastReason
			_ = r.writeAGYSession(*session)
			return
		}
		logNow, err := ParseAGYDedicatedLog(session.LogFile)
		if err == nil && logNow.ActiveConversationID == session.ConversationID && logNow.CancelConfirmedAfterRequest {
			cancelConfirmed = true
			break
		}
	}

	if !cancelConfirmed {
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "未能确认当前轮次已安全取消（为防止新旧请求叠加或后台任务未结束，已暂停自动恢复）"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	// Verify no background tasks or active subprocesses remain after cancellation
	logAfterCancel, _ := ParseAGYDedicatedLog(session.LogFile)
	activeToolsAfter, subErr := CheckChildSubprocesses(session.ChildPID, logAfterCancel.ServerPID)
	transcriptAfter := r.readAGYTranscriptSummary(session.ConversationID)
	if subErr != nil || activeToolsAfter > 0 || transcriptAfter.PendingToolCall || transcriptAfter.HasBackgroundCommand {
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "当前轮已取消，但检测到后台任务或工具状态未完全结束（killBackgroundTasks=false），已停止自动发送继续"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	// Step 4: Re-verify proxy & network stability before injecting recovery prompt
	proxyNow := InspectProxyEnv(os.Environ())
	if proxyNow.Conflict != "" || !CheckProxyReachable(proxyNow.HostPort, 1500*time.Millisecond) || CurrentNetworkDigest() != session.NetworkDigest {
		session.State = AGYGuardStateWaitingNetwork
		session.LastReason = "取消后复查发现代理或网络状态再次波动，已中止本次恢复发送"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	// Re-check policy & user input right at the submission boundary
	freshPolicy := r.ReadAGYGuardPolicy()
	if !freshPolicy.AutoRecoveryEnabled || freshPolicy.Paused {
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "恢复指令发送前自动恢复已被关闭或暂停"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	tracker.Lock()
	if tracker.typing || tracker.userStopped || tracker.epoch != armedEpoch || tracker.terminal == nil {
		tracker.Unlock()
		session.State = AGYGuardStateWaitingUserAction
		session.LastReason = "发送恢复指令前检测到用户输入，已优先交由用户处理"
		session.Message = session.LastReason
		_ = r.writeAGYSession(*session)
		return
	}

	// Step 5: Send single bounded recovery prompt using bracketed paste + Enter
	now := time.Now().UTC()
	session.RecoveryTimestamps = append(session.RecoveryTimestamps, now)
	session.RecoveryCountWindow = len(session.RecoveryTimestamps)
	session.State = AGYGuardStateRecovering
	session.LastReason = fmt.Sprintf("已取消卡死轮次并发送一次恢复指令（原因：%s）", decision.Reason)
	session.Message = session.LastReason
	_ = r.writeAGYSession(*session)

	_, writeErr = io.WriteString(tracker.terminal, "\x1b[200~"+AGYRecoveryPrompt+"\x1b[201~")
	tracker.Unlock()
	if writeErr != nil {
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(150 * time.Millisecond):
	}

	tracker.Lock()
	if !tracker.typing && !tracker.userStopped && tracker.epoch == armedEpoch && tracker.terminal != nil {
		_, _ = io.WriteString(tracker.terminal, "\r")
	}
	tracker.Unlock()
}

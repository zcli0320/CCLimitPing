package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// AGYGuardState represents the canonical state of a managed AGY CLI session.
type AGYGuardState string

const (
	AGYGuardStateObserving         AGYGuardState = "observing"           // 观察中
	AGYGuardStateRunning           AGYGuardState = "running"             // 正常运行
	AGYGuardStateModelRetrying     AGYGuardState = "model_retrying"      // 模型重试
	AGYGuardStateWaitingNetwork    AGYGuardState = "waiting_network"     // 等待网络恢复
	AGYGuardStateRecovering        AGYGuardState = "recovering"          // 恢复处理中
	AGYGuardStateWaitingUserAction AGYGuardState = "waiting_user_action" // 等待用户处理
	AGYGuardStateIdle              AGYGuardState = "idle"                // 空闲／已结束
)

func (s AGYGuardState) Label() string {
	switch s {
	case AGYGuardStateObserving:
		return "观察中"
	case AGYGuardStateRunning:
		return "正常运行"
	case AGYGuardStateModelRetrying:
		return "模型重试"
	case AGYGuardStateWaitingNetwork:
		return "等待网络恢复"
	case AGYGuardStateRecovering:
		return "恢复处理中"
	case AGYGuardStateWaitingUserAction:
		return "等待用户处理"
	case AGYGuardStateIdle:
		return "空闲／已结束"
	default:
		return "观察中"
	}
}

// AGYRecoveryPrompt is the single bounded recovery prompt sent to the same
// conversation after a verified network stall cancellation. It explicitly
// instructs the agent to inspect already-completed file edits and command
// results before continuing, and never replays raw side-effecting commands.
const AGYRecoveryPrompt = "请先核对已完成的文件修改和命令结果，确认无误后继续完成尚未完成的部分；不要盲目重复执行已有副作用的命令。"

// AGYGuardPolicy holds the centralized recovery policy and user controls.
// AutoRecoveryEnabled defaults to false on delivery.
type AGYGuardPolicy struct {
	GuardLaunchEnabled    bool   `json:"guard_launch_enabled"`
	AutoRecoveryEnabled   bool   `json:"auto_recovery_enabled"`
	Paused                bool   `json:"paused"`
	PauseReason           string `json:"pause_reason,omitempty"`
	CooldownMinutes       int    `json:"cooldown_minutes"`
	MaxRecoveriesInWindow int    `json:"max_recoveries_in_window"`
	NetworkStableSeconds  int    `json:"network_stable_seconds"`
	ConsecutiveFailures   int    `json:"consecutive_failures"`
}

func DefaultAGYGuardPolicy() AGYGuardPolicy {
	return AGYGuardPolicy{
		GuardLaunchEnabled:    false,
		AutoRecoveryEnabled:   false,
		Paused:                false,
		CooldownMinutes:       30,
		MaxRecoveriesInWindow: 2,
		NetworkStableSeconds:  20,
		ConsecutiveFailures:   2,
	}
}

func (p *AGYGuardPolicy) normalize() {
	if p.CooldownMinutes <= 0 {
		p.CooldownMinutes = 30
	}
	if p.MaxRecoveriesInWindow <= 0 {
		p.MaxRecoveriesInWindow = 2
	}
	if p.NetworkStableSeconds <= 0 {
		p.NetworkStableSeconds = 20
	}
	if p.ConsecutiveFailures <= 0 {
		p.ConsecutiveFailures = 2
	}
}

// AGYSignalEvidence records one status signal source and its reliability level.
type AGYSignalEvidence struct {
	Source     string `json:"source"`
	Confidence string `json:"confidence"` // "high", "medium", "low"
	Detail     string `json:"detail"`
}

// AGYGuardSession represents one managed AGY CLI runner instance.
type AGYGuardSession struct {
	LaunchID            string              `json:"launch_id"`
	RunnerPID           int                 `json:"runner_pid"`
	ChildPID            int                 `json:"child_pid"`
	Folder              string              `json:"folder"`
	LogFile             string              `json:"log_file"`
	LogInode            uint64              `json:"log_inode,omitempty"`
	ConversationID      string              `json:"conversation_id,omitempty"`
	BindingConfirmed    bool                `json:"binding_confirmed"`
	BindingReason       string              `json:"binding_reason,omitempty"`
	State               AGYGuardState       `json:"state"`
	StateLabel          string              `json:"state_label"`
	Message             string              `json:"message"`
	LastReason          string              `json:"last_reason,omitempty"`
	ProxyURL            string              `json:"proxy_url,omitempty"`
	ProxyConflict       string              `json:"proxy_conflict,omitempty"`
	ProxyHealthy        bool                `json:"proxy_healthy"`
	ProxyStableSeconds  int                 `json:"proxy_stable_seconds,omitempty"`
	NetworkDigest       string              `json:"network_digest,omitempty"`
	ActiveTools         int                 `json:"active_tools,omitempty"`
	BackgroundTaskDoubt bool                `json:"background_task_doubt,omitempty"`
	UserTyping          bool                `json:"user_typing,omitempty"`
	AwaitingApproval    bool                `json:"awaiting_approval,omitempty"`
	RecoveryCountWindow int                 `json:"recovery_count_window"`
	RecoveryTimestamps  []time.Time         `json:"recovery_timestamps,omitempty"`
	HandledIncidents    map[string]bool     `json:"handled_incidents,omitempty"`
	Signals             []AGYSignalEvidence `json:"signals,omitempty"`
	StartedAt           time.Time           `json:"started_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
	Exited              bool                `json:"exited,omitempty"`
	ExitCode            int                 `json:"exit_code,omitempty"`
}

// AGYGuardReport is the JSON summary returned to the macOS native app.
type AGYGuardReport struct {
	Policy      AGYGuardPolicy    `json:"policy"`
	ActiveCount int               `json:"active_count"`
	Headline    string            `json:"headline"`
	Summary     string            `json:"summary"`
	LastReason  string            `json:"last_reason,omitempty"`
	Sessions    []AGYGuardSession `json:"sessions"`
}

func (r Reader) agyGuardDir() string {
	return filepath.Join(r.helperStateRoot(), "agy-guard")
}

func (r Reader) agyGuardPolicyPath() string {
	return filepath.Join(r.agyGuardDir(), "policy.json")
}

func (r Reader) agyGuardSessionsDir() string {
	return filepath.Join(r.agyGuardDir(), "sessions")
}

func (r Reader) agyGuardLogsDir() string {
	return filepath.Join(r.agyGuardDir(), "logs")
}

func (r Reader) agyAntigravityRoot() string {
	if override := os.Getenv("CODEX_HELPER_AGY_HOME"); filepath.IsAbs(override) {
		return override
	}
	return filepath.Join(r.Home, ".gemini", "antigravity-cli")
}

func (r Reader) ReadAGYGuardPolicy() AGYGuardPolicy {
	policy := DefaultAGYGuardPolicy()
	_ = readState(r.agyGuardPolicyPath(), &policy)
	policy.normalize()
	return policy
}

func (r Reader) SaveAGYGuardPolicy(mutate func(*AGYGuardPolicy)) (AGYGuardPolicy, error) {
	if err := os.MkdirAll(r.agyGuardDir(), 0o700); err != nil {
		return DefaultAGYGuardPolicy(), err
	}
	unlock, err := lockWarmup(r.agyGuardPolicyPath() + ".lock")
	if err != nil {
		return r.ReadAGYGuardPolicy(), err
	}
	defer unlock()
	policy := r.ReadAGYGuardPolicy()
	mutate(&policy)
	policy.normalize()
	if err := writeState(r.agyGuardPolicyPath(), policy); err != nil {
		return policy, err
	}
	return policy, nil
}

// ConfigureAGYGuardPolicy validates CLI configure flags strictly (rejecting
// unknown flags, missing values, or invalid booleans/numbers) and persists the
// updated policy atomically. Both --paused and --pause are supported.
func (r Reader) ConfigureAGYGuardPolicy(args []string) (AGYGuardPolicy, error) {
	// Pre-validate against a copy so invalid flags never partially mutate state.
	probe := DefaultAGYGuardPolicy()
	if err := ParseAndApplyAGYGuardConfigureArgs(&probe, args); err != nil {
		return r.ReadAGYGuardPolicy(), err
	}
	var applyErr error
	updated, saveErr := r.SaveAGYGuardPolicy(func(p *AGYGuardPolicy) {
		applyErr = ParseAndApplyAGYGuardConfigureArgs(p, args)
	})
	if applyErr != nil {
		return updated, applyErr
	}
	return updated, saveErr
}

func parseAGYGuardBoolFlag(flag, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("无效布尔参数值 %s=%q（仅支持 on/off/true/false/1/0）", flag, raw)
	}
}

func parseAGYGuardPositiveIntFlag(flag, raw string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("无效正整数参数值 %s=%q", flag, raw)
	}
	return v, nil
}

// ParseAndApplyAGYGuardConfigureArgs parses `codex-helper agy-guard configure`
// flags and applies them to `p`. Unknown flags or missing/invalid values return
// an error.
func ParseAndApplyAGYGuardConfigureArgs(p *AGYGuardPolicy, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("缺少 configure 参数")
	}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if i+1 >= len(args) {
			return fmt.Errorf("参数 %s 缺少取值", flag)
		}
		val := args[i+1]
		i++
		switch flag {
		case "--guard-launch":
			b, err := parseAGYGuardBoolFlag(flag, val)
			if err != nil {
				return err
			}
			p.GuardLaunchEnabled = b
		case "--auto-recovery":
			b, err := parseAGYGuardBoolFlag(flag, val)
			if err != nil {
				return err
			}
			p.AutoRecoveryEnabled = b
			if !p.AutoRecoveryEnabled {
				p.Paused = false
				p.PauseReason = ""
			}
		case "--paused", "--pause":
			b, err := parseAGYGuardBoolFlag(flag, val)
			if err != nil {
				return err
			}
			p.Paused = b
			if !p.Paused {
				p.PauseReason = ""
			} else if p.PauseReason == "" {
				p.PauseReason = "用户手动暂停"
			}
		case "--cooldown-minutes":
			v, err := parseAGYGuardPositiveIntFlag(flag, val)
			if err != nil {
				return err
			}
			p.CooldownMinutes = v
		case "--max-recoveries":
			v, err := parseAGYGuardPositiveIntFlag(flag, val)
			if err != nil {
				return err
			}
			p.MaxRecoveriesInWindow = v
		case "--stable-seconds":
			v, err := parseAGYGuardPositiveIntFlag(flag, val)
			if err != nil {
				return err
			}
			p.NetworkStableSeconds = v
		case "--consecutive-failures":
			v, err := parseAGYGuardPositiveIntFlag(flag, val)
			if err != nil {
				return err
			}
			p.ConsecutiveFailures = v
		default:
			return fmt.Errorf("未知 agy-guard configure 参数：%s", flag)
		}
	}
	p.normalize()
	return nil
}

func (r Reader) agySessionStatePath(launchID string) string {
	safe := regexp.MustCompile(`[^a-zA-Z0-9._-]`).ReplaceAllString(launchID, "")
	if safe == "" {
		safe = "invalid"
	}
	return filepath.Join(r.agyGuardSessionsDir(), safe+".json")
}

func (r Reader) writeAGYSession(s AGYGuardSession) error {
	if err := os.MkdirAll(r.agyGuardSessionsDir(), 0o700); err != nil {
		return err
	}
	s.StateLabel = s.State.Label()
	s.UpdatedAt = time.Now().UTC()
	return writeState(r.agySessionStatePath(s.LaunchID), s)
}

// AGYGuardStatus aggregates policy and all managed sessions, marking crashed
// runners cleanly without ever signaling unknown processes.
func (r Reader) AGYGuardStatus() AGYGuardReport {
	policy := r.ReadAGYGuardPolicy()
	entries, _ := os.ReadDir(r.agyGuardSessionsDir())
	var sessions []AGYGuardSession
	now := time.Now().UTC()
	windowDur := time.Duration(policy.CooldownMinutes) * time.Minute

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.agyGuardSessionsDir(), entry.Name())
		var s AGYGuardSession
		if readState(path, &s) != nil || s.LaunchID == "" {
			continue
		}
		// Prune recovery timestamps to current window
		var recent []time.Time
		for _, ts := range s.RecoveryTimestamps {
			if now.Sub(ts) <= windowDur {
				recent = append(recent, ts)
			}
		}
		s.RecoveryTimestamps = recent
		s.RecoveryCountWindow = len(recent)

		if !s.Exited {
			runnerAlive := s.RunnerPID > 0 && isProcessAlive(s.RunnerPID)
			childAlive := s.ChildPID > 0 && isProcessAlive(s.ChildPID)
			if !runnerAlive {
				s.Exited = true
				if childAlive {
					s.State = AGYGuardStateWaitingUserAction
					s.LastReason = "受管运行器已异常退出，但原 AGY 进程可能仍在运行；已停止自动控制，请手动检查终端"
					s.Message = s.LastReason
				} else {
					s.State = AGYGuardStateIdle
					if s.LastReason == "" {
						s.LastReason = "受管运行器已退出"
					}
					s.Message = "会话已结束"
				}
				s.StateLabel = s.State.Label()
				_ = writeState(path, s)
			}
		}
		s.StateLabel = s.State.Label()
		// Keep active sessions or recently updated sessions (within 6 hours)
		if !s.Exited || now.Sub(s.UpdatedAt) <= 6*time.Hour {
			sessions = append(sessions, s)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Exited != sessions[j].Exited {
			return !sessions[i].Exited
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	activeCount := 0
	var lastReason string
	var primaryState AGYGuardState = AGYGuardStateIdle
	for _, s := range sessions {
		if !s.Exited {
			activeCount++
			if primaryState == AGYGuardStateIdle {
				primaryState = s.State
			}
		}
		if lastReason == "" && s.LastReason != "" {
			lastReason = s.LastReason
		}
	}
	if lastReason == "" && policy.PauseReason != "" {
		lastReason = policy.PauseReason
	}

	report := AGYGuardReport{
		Policy:      policy,
		ActiveCount: activeCount,
		LastReason:  lastReason,
		Sessions:    sessions,
	}
	if sessions == nil {
		report.Sessions = []AGYGuardSession{}
	}

	modeDesc := "仅观察（自动恢复默认关闭）"
	if policy.AutoRecoveryEnabled {
		if policy.Paused {
			modeDesc = "自动恢复已暂停"
		} else {
			modeDesc = fmt.Sprintf("自动恢复已开启（%d 分钟最多 %d 次）", policy.CooldownMinutes, policy.MaxRecoveriesInWindow)
		}
	}

	if activeCount == 0 {
		if len(sessions) > 0 {
			report.Headline = fmt.Sprintf("%s · 当前无活动受管会话", AGYGuardStateIdle.Label())
		} else {
			report.Headline = "未运行受管 AGY 会话 · " + modeDesc
		}
	} else {
		report.Headline = fmt.Sprintf("%s · %d 个受管会话运行中 · %s", primaryState.Label(), activeCount, modeDesc)
	}

	var lines []string
	if lastReason != "" {
		lines = append(lines, "最近原因："+lastReason)
	}
	for _, s := range sessions {
		if s.Exited && activeCount > 0 {
			continue
		}
		shortConv := "未绑定会话"
		if s.ConversationID != "" {
			if len(s.ConversationID) >= 8 {
				shortConv = "会话 " + s.ConversationID[:8]
			} else {
				shortConv = "会话 " + s.ConversationID
			}
		}
		folderBase := filepath.Base(s.Folder)
		line := fmt.Sprintf("[%s] %s · %s · PID %d · %s", s.StateLabel, folderBase, shortConv, s.ChildPID, s.Message)
		lines = append(lines, line)
		if len(lines) >= 4 {
			break
		}
	}
	if len(lines) == 0 {
		if policy.GuardLaunchEnabled {
			lines = append(lines, "已开启受管启动：点击「启动 AGY CLI」将通过受管运行器接入专用日志与状态监测。")
		} else {
			lines = append(lines, "当前为普通启动模式；勾选「受管启动 AGY（状态监测）」可接入专用日志与会话监测。")
		}
	}
	report.Summary = strings.Join(lines, "\n")
	return report
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// InspectProxyEnv inspects the current environment without modifying any system
// or shell settings. It preserves inherited proxies (such as Claude's 17893 or
// AGY's 1082) and reports casing/protocol conflicts.
type AGYProxyInspection struct {
	ProxyURL string
	HostPort string
	Conflict string
}

func InspectProxyEnv(env []string) AGYProxyInspection {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	checkPair := func(upper, lower string) string {
		u, l := m[upper], m[lower]
		if u != "" && l != "" && u != l {
			return fmt.Sprintf("代理环境变量冲突：%s=%q 与 %s=%q 不一致", upper, u, lower, l)
		}
		return ""
	}
	for _, pair := range [][2]string{
		{"HTTP_PROXY", "http_proxy"},
		{"HTTPS_PROXY", "https_proxy"},
		{"ALL_PROXY", "all_proxy"},
	} {
		if conflict := checkPair(pair[0], pair[1]); conflict != "" {
			return AGYProxyInspection{Conflict: conflict}
		}
	}

	raw := firstNonEmpty(
		m["HTTPS_PROXY"], m["https_proxy"],
		m["HTTP_PROXY"], m["http_proxy"],
		m["ALL_PROXY"], m["all_proxy"],
	)
	if raw == "" {
		// Note: ~/.local/bin/agy defaults to http://127.0.0.1:1082 when no proxy
		// env var is inherited, while preserving any inherited proxy like 17893.
		raw = "http://127.0.0.1:1082"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		if !strings.Contains(raw, "://") {
			if p2, e2 := url.Parse("http://" + raw); e2 == nil && p2.Host != "" {
				parsed = p2
			}
		}
	}
	if parsed == nil || parsed.Host == "" {
		return AGYProxyInspection{ProxyURL: raw, Conflict: fmt.Sprintf("代理地址格式无法解析：%q", raw)}
	}
	hostPort := parsed.Host
	if !strings.Contains(hostPort, ":") {
		if parsed.Scheme == "https" {
			hostPort += ":443"
		} else {
			hostPort += ":80"
		}
	}
	return AGYProxyInspection{ProxyURL: raw, HostPort: hostPort}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// CheckProxyReachable tests TCP reachability of the configured proxy without
// falling back to direct connections or switching ports.
func CheckProxyReachable(hostPort string, timeout time.Duration) bool {
	if hostPort == "" {
		return false
	}
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// CurrentNetworkDigest returns a sorted summary of active non-loopback IPv4/IPv6
// interface addresses so Wi-Fi <-> Ethernet transitions are detected and reset
// the network stability window without pinning a specific interface name.
func CurrentNetworkDigest() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "unknown"
	}
	var parts []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			parts = append(parts, iface.Name+":"+ipNet.IP.String())
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// AGYLogAnalysis summarizes the parsed facts from a dedicated `--log-file`.
type AGYLogAnalysis struct {
	ServerPID                     int
	ActiveConversationID          string
	SwitchedConversationCount     int
	ForwardingCount               int
	LastTurnStartedLine           int
	LastTurnCompletedLine         int
	LastTurnCancelledLine         int
	LastProgressLine              int
	LastCancelKillBgFalse         bool
	CancelConfirmedAfterRequest   bool
	ForegroundNetFailuresInTurn   int
	TotalForegroundFailuresInTurn int
	Has5m0sTimeoutInTurn          bool
	LastForegroundFailureReason   string
	LastForegroundFailureLine     int
	HasNonTransientErrorInTurn    bool
	NonTransientErrorDetail       string
	BackgroundNoiseCount          int
	ToolConfirmPendingInTurn      bool
	UnfinishedBgTaskInLog         bool
	TerminalGone                  bool
}

var (
	reStartingLS       = regexp.MustCompile(`Starting language server process with pid (\d+)`)
	reStreamingConv    = regexp.MustCompile(`Streaming conversation ([0-9a-fA-F-]{36})`)
	reForwardingMsg    = regexp.MustCompile(`Forwarding user message to conversation ([0-9a-fA-F-]{36})`)
	reSendingMsg       = regexp.MustCompile(`Sending user message to conversation ([0-9a-fA-F-]{36})`)
	reSendLatency      = regexp.MustCompile(`SEND_USER_CASCADE_MESSAGE_LATENCY.*cascade_id:([0-9a-fA-F-]{36})`)
	reCancellingResp   = regexp.MustCompile(`Cancelling in-progress response for conversation ([0-9a-fA-F-]{36})`)
	reCancellingConv   = regexp.MustCompile(`Cancelling conversation ([0-9a-fA-F-]{36}) \(killBackgroundTasks=(true|false)\)`)
	reStreamExited     = regexp.MustCompile(`Stream goroutine exited for ([0-9a-fA-F-]{36}), sending completion signal`)
	reStreamCompleted  = regexp.MustCompile(`Stream completed for ([0-9a-fA-F-]{36}), clearing ResponsePending`)
	reRunAttemptFailed = regexp.MustCompile(`run\.go:\d+\] Run: attempt (\d+) failed \((.+)\)(?:, retrying in ([0-9a-zA-Z.]+))?`)
	reToolConfirmation = regexp.MustCompile(`Tool confirmation for conversation ([0-9a-fA-F-]{36}) step (\d+)`)
	reAgentExecErr     = regexp.MustCompile(`(?:agent executor error: )?generating and executing: (.+)`)
	reLogStepProgress  = regexp.MustCompile(`(?i)(Monitor agy_tool_safety FINISHED|Monitor sir_flash_lite FINISHED|Monitor sir_flash_lite SKIPPED|Monitor severe_internal_risks_agy SKIPPED|LLM Monitor .* Evaluation complete|HandleUserInput called|Executing tool|Tool execution|Step \d+ completed|Completed step|PLANNER_RESPONSE|GENERIC.*DONE|Run: attempt \d+ succeeded|Response completed|Model response|Tool completed)`)
)

// ParseAGYDedicatedLog parses a dedicated AGY CLI log file from the beginning.
// Because each managed launch uses its own `--log-file`, events in this file
// belong exclusively to this AGY CLI process.
func ParseAGYDedicatedLog(path string) (AGYLogAnalysis, error) {
	f, err := os.Open(path)
	if err != nil {
		return AGYLogAnalysis{}, err
	}
	defer f.Close()

	var out AGYLogAnalysis
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lineNum := 0
	seenConvs := map[string]bool{}
	var streamExitedAtLine int
	var cancelConvAtLine int
	lastFailedAttempt := 0

	resetActiveFailureInterval := func(progressLine int) {
		out.ForegroundNetFailuresInTurn = 0
		out.Has5m0sTimeoutInTurn = false
		out.LastForegroundFailureReason = ""
		out.LastForegroundFailureLine = 0
		lastFailedAttempt = 0
		if progressLine > out.LastProgressLine {
			out.LastProgressLine = progressLine
		}
	}

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// 1. Ignore known background/telemetry/config/monitor noise
		if isBackgroundNoiseLogLine(line) {
			out.BackgroundNoiseCount++
			continue
		}

		if m := reStartingLS.FindStringSubmatch(line); m != nil {
			if pid, e := strconv.Atoi(m[1]); e == nil {
				out.ServerPID = pid
			}
			continue
		}

		if m := reStreamingConv.FindStringSubmatch(line); m != nil {
			cid := m[1]
			if out.ActiveConversationID != "" && out.ActiveConversationID != cid {
				out.SwitchedConversationCount++
				// Reset turn-scoped counters on conversation switch
				out.LastTurnStartedLine = 0
				out.LastTurnCompletedLine = 0
				out.LastTurnCancelledLine = 0
				out.TotalForegroundFailuresInTurn = 0
				out.HasNonTransientErrorInTurn = false
				out.NonTransientErrorDetail = ""
				out.ToolConfirmPendingInTurn = false
				resetActiveFailureInterval(0)
				out.LastProgressLine = 0
			}
			out.ActiveConversationID = cid
			seenConvs[cid] = true
			continue
		}

		matchedTurnStart := false
		for _, re := range []*regexp.Regexp{reForwardingMsg, reSendingMsg, reSendLatency} {
			if m := re.FindStringSubmatch(line); m != nil {
				cid := m[1]
				if out.ActiveConversationID != "" && out.ActiveConversationID != cid {
					out.SwitchedConversationCount++
				}
				out.ActiveConversationID = cid
				seenConvs[cid] = true
				out.ForwardingCount++
				out.LastTurnStartedLine = lineNum
				out.TotalForegroundFailuresInTurn = 0
				out.HasNonTransientErrorInTurn = false
				out.NonTransientErrorDetail = ""
				out.ToolConfirmPendingInTurn = false
				out.CancelConfirmedAfterRequest = false
				resetActiveFailureInterval(0)
				out.LastProgressLine = 0
				matchedTurnStart = true
				break
			}
		}
		if matchedTurnStart {
			continue
		}

		if m := reCancellingResp.FindStringSubmatch(line); m != nil {
			if out.ActiveConversationID == "" || out.ActiveConversationID == m[1] {
				out.ActiveConversationID = m[1]
				out.LastTurnCancelledLine = lineNum
				out.ToolConfirmPendingInTurn = false
			}
			continue
		}

		if m := reCancellingConv.FindStringSubmatch(line); m != nil {
			if out.ActiveConversationID == "" || out.ActiveConversationID == m[1] {
				out.ActiveConversationID = m[1]
				out.LastTurnCancelledLine = lineNum
				cancelConvAtLine = lineNum
				out.LastCancelKillBgFalse = (m[2] == "false")
				if streamExitedAtLine >= out.LastTurnStartedLine && streamExitedAtLine > 0 {
					out.CancelConfirmedAfterRequest = true
				}
			}
			continue
		}

		if m := reStreamExited.FindStringSubmatch(line); m != nil {
			if out.ActiveConversationID == "" || out.ActiveConversationID == m[1] {
				streamExitedAtLine = lineNum
				if cancelConvAtLine >= out.LastTurnStartedLine && cancelConvAtLine > 0 {
					out.CancelConfirmedAfterRequest = true
				}
			}
			continue
		}

		if m := reStreamCompleted.FindStringSubmatch(line); m != nil {
			if out.ActiveConversationID == "" || out.ActiveConversationID == m[1] {
				out.LastTurnCompletedLine = lineNum
				out.CancelConfirmedAfterRequest = true
				out.ToolConfirmPendingInTurn = false
				resetActiveFailureInterval(lineNum)
			}
			continue
		}

		if strings.Contains(line, "Waiting for tool confirmation") || strings.Contains(line, "Awaiting tool confirmation") || strings.Contains(line, "Tool confirmation required") {
			out.ToolConfirmPendingInTurn = true
			resetActiveFailureInterval(lineNum)
			continue
		}

		if m := reToolConfirmation.FindStringSubmatch(line); m != nil {
			if out.ActiveConversationID == "" || out.ActiveConversationID == m[1] {
				out.ToolConfirmPendingInTurn = false
				resetActiveFailureInterval(lineNum)
			}
			continue
		}

		if strings.Contains(line, "bgTask ") && (strings.Contains(line, "failed") || strings.Contains(line, "kill failed")) {
			out.UnfinishedBgTaskInLog = true
			continue
		}

		if strings.Contains(line, "Terminal gone, shutting down") || strings.Contains(line, "CLI program exited, shutting down") {
			out.TerminalGone = true
			continue
		}

		if m := reRunAttemptFailed.FindStringSubmatch(line); m != nil {
			attemptNum, _ := strconv.Atoi(m[1])
			reason := m[2]
			if isTransientForegroundNetworkFailure(reason) {
				// If a previous step had reached attempt >= 2 and now a new step's
				// retry loop starts at attempt 1, reset the previous step's count.
				if attemptNum == 1 && lastFailedAttempt > 1 {
					resetActiveFailureInterval(lineNum - 1)
				}
				lastFailedAttempt = attemptNum
				out.ForegroundNetFailuresInTurn++
				out.TotalForegroundFailuresInTurn++
				out.LastForegroundFailureLine = lineNum
				out.LastForegroundFailureReason = summarizeNetFailure(reason)
				if strings.Contains(reason, "stream idle timeout (5m0s)") {
					out.Has5m0sTimeoutInTurn = true
				}
			} else if isNonTransientModelError(reason) {
				out.HasNonTransientErrorInTurn = true
				out.NonTransientErrorDetail = summarizeNonTransientError(reason)
			}
			continue
		}

		if m := reAgentExecErr.FindStringSubmatch(line); m != nil {
			reason := m[1]
			if isNonTransientModelError(reason) {
				out.HasNonTransientErrorInTurn = true
				out.NonTransientErrorDetail = summarizeNonTransientError(reason)
			} else if isTransientForegroundNetworkFailure(reason) {
				out.ForegroundNetFailuresInTurn++
				out.TotalForegroundFailuresInTurn++
				out.LastForegroundFailureLine = lineNum
				out.LastForegroundFailureReason = summarizeNetFailure(reason)
				if strings.Contains(reason, "stream idle timeout (5m0s)") {
					out.Has5m0sTimeoutInTurn = true
				}
			}
			continue
		}

		// Any explicit model response / tool safety completion / step progress
		// clears earlier transient failures in the turn.
		if reLogStepProgress.MatchString(line) {
			resetActiveFailureInterval(lineNum)
			continue
		}
	}

	return out, scanner.Err()
}

func isBackgroundNoiseLogLine(line string) bool {
	noisePatterns := []string{
		"aicode.googleapis.com/v1alpha:fetchConfig",
		"AICode FetchConfig failed",
		"Failed to poll ListExperiments",
		"v1internal:listExperiments",
		"play.googleapis.com/log",
		"Cache(loadCodeAssistResponse)",
		"Cache(availableModels)",
		"Cache(userInfo)",
		"Cache(peopleInfo)",
		"Cache(retrieveUserQuotaSummary)",
		"Failed to refresh cache in background",
		"v1internal:fetchAvailableModels",
		"v1internal:loadCodeAssist",
		"v1internal:retrieveUserQuotaSummary",
	}
	for _, pat := range noisePatterns {
		if strings.Contains(line, pat) {
			return true
		}
	}
	// Safety monitor errors (e.g. sir_flash_lite network failure) are background
	// noise and must not be counted as foreground failures, whereas monitor
	// FINISHED/SKIPPED lines prove a foreground tool call was generated.
	if strings.Contains(line, "Monitor sir_flash_lite") ||
		strings.Contains(line, "LLM Monitor sir_flash_lite") ||
		strings.Contains(line, "Monitor agy_tool_safety") ||
		strings.Contains(line, "Monitor severe_internal_risks_agy") {
		if !reLogStepProgress.MatchString(line) {
			return true
		}
	}
	return false
}

func isTransientForegroundNetworkFailure(reason string) bool {
	if isNonTransientModelError(reason) {
		return false
	}
	transientMarkers := []string{
		"EOF",
		"unexpected EOF",
		"connection reset by peer",
		"stream idle timeout (5m0s)",
		"use of closed network connection",
		"context deadline exceeded",
		"connection refused",
		"i/o timeout",
		"TLS handshake timeout",
	}
	for _, m := range transientMarkers {
		if strings.Contains(reason, m) {
			return true
		}
	}
	return false
}

func isNonTransientModelError(reason string) bool {
	lower := strings.ToLower(reason)
	nonTransient := []string{
		"location is not supported",
		"location is not eligible",
		"permission_denied",
		"unauthenticated",
		"not logged into antigravity",
		"resource_exhausted",
		"quota",
		"rate limit",
		"failed_precondition",
		"account_disabled",
		"invalid_argument",
	}
	for _, m := range nonTransient {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func summarizeNetFailure(reason string) string {
	switch {
	case strings.Contains(reason, "stream idle timeout (5m0s)"):
		return "前台模型流超时（stream idle timeout 5m0s）"
	case strings.Contains(reason, "connection reset by peer"):
		return "前台模型请求连接被重置（connection reset by peer）"
	case strings.Contains(reason, "unexpected EOF") || strings.Contains(reason, ": EOF"):
		return "前台模型请求连接异常中断（EOF）"
	case strings.Contains(reason, "use of closed network connection"):
		return "前台模型请求连接已关闭（closed network connection）"
	case strings.Contains(reason, "connection refused"):
		return "本地代理连接被拒绝（connection refused）"
	default:
		return "前台模型请求网络错误：" + truncateAGYText(reason, 80)
	}
}

func summarizeNonTransientError(reason string) string {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "location is not supported") || strings.Contains(lower, "location is not eligible"):
		return "地区受限（User location is not supported），不执行自动恢复"
	case strings.Contains(lower, "not logged into antigravity") || strings.Contains(lower, "unauthenticated"):
		return "账号未登录或凭证失效，不执行自动恢复"
	case strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "quota"):
		return "账号配额或速率受限，不执行自动恢复"
	default:
		return "非瞬时模型错误（" + truncateAGYText(reason, 80) + "），不执行自动恢复"
	}
}

func truncateAGYText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// AGYDBSummary holds read-only metadata from conversation_summaries.db.
type AGYDBSummary struct {
	ConversationID       string
	Status               string
	StepCount            int
	NotFullyIdle         bool
	Killed               bool
	LastUserInputStepIdx int
	WorkspaceURIs        string
}

func (r Reader) readAGYConversationSummary(ctx context.Context, conversationID string) (AGYDBSummary, bool) {
	if !regexp.MustCompile(`^[0-9a-fA-F-]{36}$`).MatchString(conversationID) {
		return AGYDBSummary{}, false
	}
	dbPath := filepath.Join(r.agyAntigravityRoot(), "conversation_summaries.db")
	var rows []struct {
		ConversationID       string `json:"conversation_id"`
		Status               string `json:"status"`
		StepCount            int    `json:"step_count"`
		NotFullyIdle         any    `json:"not_fully_idle"`
		Killed               any    `json:"killed"`
		LastUserInputStepIdx int    `json:"last_user_input_step_index"`
		WorkspaceURIs        string `json:"workspace_uris"`
	}
	q := fmt.Sprintf(
		"SELECT conversation_id, status, step_count, not_fully_idle, killed, last_user_input_step_index, workspace_uris FROM conversation_summaries WHERE conversation_id='%s' LIMIT 1;",
		conversationID,
	)
	if err := r.query(ctx, dbPath, q, &rows); err != nil || len(rows) == 0 {
		return AGYDBSummary{}, false
	}
	row := rows[0]
	return AGYDBSummary{
		ConversationID:       row.ConversationID,
		Status:               row.Status,
		StepCount:            row.StepCount,
		NotFullyIdle:         asTruthySQLiteBool(row.NotFullyIdle),
		Killed:               asTruthySQLiteBool(row.Killed),
		LastUserInputStepIdx: row.LastUserInputStepIdx,
		WorkspaceURIs:        row.WorkspaceURIs,
	}, true
}

func asTruthySQLiteBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case int64:
		return val != 0
	case string:
		return val == "1" || strings.EqualFold(val, "true")
	default:
		return false
	}
}

// AGYTranscriptSummary holds read-only facts from `<brain>/<id>/.system_generated/logs/transcript.jsonl`.
type AGYTranscriptSummary struct {
	Readable                 bool
	ParseError               bool
	TranscriptIncomplete     bool
	MaxStepIndex             int
	LastNonErrorStepIndex    int
	LastErrorStepIndex       int
	RunningStepCount         int
	PendingToolCall          bool
	PendingToolName          string
	PendingAskQuestion       bool
	HasBackgroundCommand     bool
	LastTurnErrorCount       int
	TotalTurnErrorCount      int
	OvercomeTurnErrorCount   int
	CompletedToolStepsInTurn int
	LastTurnHas5m0s          bool
	LastTurnErrorStepIndices []int
	AllTurnErrorStepIndices  []int
	LastStepType             string
	LastStepStatus           string
}

func (r Reader) readAGYTranscriptSummary(conversationID string) AGYTranscriptSummary {
	if !regexp.MustCompile(`^[0-9a-fA-F-]{36}$`).MatchString(conversationID) {
		return AGYTranscriptSummary{}
	}
	path := filepath.Join(r.agyAntigravityRoot(), "brain", conversationID, ".system_generated", "logs", "transcript.jsonl")
	return ParseAGYTranscriptFile(path)
}

func ParseAGYTranscriptFile(path string) AGYTranscriptSummary {
	f, err := os.Open(path)
	if err != nil {
		return AGYTranscriptSummary{}
	}
	defer f.Close()

	var summary AGYTranscriptSummary
	summary.MaxStepIndex = -1
	summary.LastNonErrorStepIndex = -1
	summary.LastErrorStepIndex = -1

	type stepRecord struct {
		StepIndex *int   `json:"step_index"`
		Source    string `json:"source"`
		Type      string `json:"type"`
		Status    string `json:"status"`
		Error     string `json:"error"`
		Content   string `json:"content"`
		ToolCalls []struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		} `json:"tool_calls"`
	}

	byIndex := map[int]stepRecord{}
	var order []int
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec stepRecord
		if json.Unmarshal([]byte(line), &rec) != nil || rec.StepIndex == nil || *rec.StepIndex < 0 || strings.TrimSpace(rec.Type) == "" {
			summary.ParseError = true
			summary.Readable = false
			return summary
		}
		idx := *rec.StepIndex
		if _, exists := byIndex[idx]; !exists {
			order = append(order, idx)
		}
		byIndex[idx] = rec
	}
	if scanner.Err() != nil {
		summary.ParseError = true
		summary.Readable = false
		return summary
	}
	if len(order) == 0 {
		summary.TranscriptIncomplete = true
		summary.Readable = false
		return summary
	}
	sort.Ints(order)

	summary.Readable = true
	lastUserIdx := -1
	var pendingBatchTools []string
	var batchHasAskQuestion bool

	for _, idx := range order {
		rec := byIndex[idx]
		if idx > summary.MaxStepIndex {
			summary.MaxStepIndex = idx
		}
		summary.LastStepType = rec.Type
		summary.LastStepStatus = rec.Status

		// Check if any tool call anywhere in the conversation spawned a daemon or background task
		for _, tc := range rec.ToolCalls {
			if tc.Name == "run_command" {
				if isDaemon, ok := tc.Args["IsDaemon"].(bool); ok && isDaemon {
					summary.HasBackgroundCommand = true
				}
				if waitMs, ok := tc.Args["WaitMsBeforeAsync"].(float64); ok && waitMs > 0 && waitMs <= 1000 {
					summary.HasBackgroundCommand = true
				}
			}
			if tc.Name == "invoke_subagent" || tc.Name == "schedule" {
				summary.HasBackgroundCommand = true
			}
		}

		if rec.Type == "USER_INPUT" {
			lastUserIdx = idx
			summary.LastNonErrorStepIndex = idx
			summary.LastErrorStepIndex = -1
			summary.LastTurnErrorCount = 0
			summary.TotalTurnErrorCount = 0
			summary.OvercomeTurnErrorCount = 0
			summary.CompletedToolStepsInTurn = 0
			summary.LastTurnHas5m0s = false
			summary.LastTurnErrorStepIndices = nil
			summary.AllTurnErrorStepIndices = nil
			summary.RunningStepCount = 0
			summary.PendingToolCall = false
			summary.PendingToolName = ""
			summary.PendingAskQuestion = false
			pendingBatchTools = nil
			batchHasAskQuestion = false
			if rec.Status != "" && rec.Status != "DONE" {
				summary.RunningStepCount++
			}
			continue
		}

		if idx <= lastUserIdx {
			continue
		}

		if rec.Type == "ERROR_MESSAGE" {
			errText := strings.TrimSpace(rec.Error + " " + rec.Content)
			if errText == "" || isTransientForegroundNetworkFailure(errText) {
				summary.TotalTurnErrorCount++
				summary.LastTurnErrorCount++
				summary.LastErrorStepIndex = idx
				summary.LastTurnErrorStepIndices = append(summary.LastTurnErrorStepIndices, idx)
				summary.AllTurnErrorStepIndices = append(summary.AllTurnErrorStepIndices, idx)
				if strings.Contains(errText, "stream idle timeout (5m0s)") {
					summary.LastTurnHas5m0s = true
				}
			}
			continue
		}

		// Non-error step after USER_INPUT: this represents step progress, so any
		// earlier transient errors in this turn have been overcome!
		if idx > summary.LastNonErrorStepIndex {
			summary.LastNonErrorStepIndex = idx
		}
		summary.OvercomeTurnErrorCount = summary.TotalTurnErrorCount
		summary.LastTurnErrorCount = 0
		summary.LastTurnHas5m0s = false
		summary.LastTurnErrorStepIndices = nil

		if rec.Status != "DONE" {
			summary.RunningStepCount++
			summary.PendingToolCall = true
		}

		switch rec.Type {
		case "PLANNER_RESPONSE":
			// If a previous PLANNER_RESPONSE in this turn still had unclosed tool calls,
			// the turn has unclosed/mismatched tool execution.
			if len(pendingBatchTools) > 0 {
				summary.PendingToolCall = true
				if summary.PendingToolName == "" {
					summary.PendingToolName = pendingBatchTools[0]
				}
				if batchHasAskQuestion {
					summary.PendingAskQuestion = true
				}
				summary.TranscriptIncomplete = true
			}
			pendingBatchTools = nil
			batchHasAskQuestion = false
			for _, tc := range rec.ToolCalls {
				name := strings.TrimSpace(tc.Name)
				if name == "" {
					summary.TranscriptIncomplete = true
					summary.Readable = false
					name = "unknown_tool"
				}
				pendingBatchTools = append(pendingBatchTools, name)
				if name == "ask_question" {
					batchHasAskQuestion = true
				}
			}
		case "GENERIC":
			if rec.Status != "DONE" {
				if summary.PendingToolName == "" {
					if len(pendingBatchTools) > 0 {
						summary.PendingToolName = pendingBatchTools[0]
					} else {
						summary.PendingToolName = "tool_running"
					}
				}
				if batchHasAskQuestion {
					summary.PendingAskQuestion = true
				}
			} else {
				summary.CompletedToolStepsInTurn++
				if len(pendingBatchTools) > 0 {
					pendingBatchTools = pendingBatchTools[1:]
					if len(pendingBatchTools) == 0 {
						batchHasAskQuestion = false
					}
				} else {
					// Orphan GENERIC result with no matching PLANNER_RESPONSE tool call
					summary.TranscriptIncomplete = true
					summary.Readable = false
				}
			}
		}
	}

	if lastUserIdx < 0 {
		summary.TranscriptIncomplete = true
		summary.Readable = false
	} else {
		// Verify there are no missing step_index gaps within the active turn
		for step := lastUserIdx; step <= summary.MaxStepIndex; step++ {
			if _, ok := byIndex[step]; !ok {
				summary.TranscriptIncomplete = true
				summary.Readable = false
				break
			}
		}
	}

	// If any tool calls from the latest PLANNER_RESPONSE have not yet produced
	// corresponding GENERIC DONE result steps, mark them as pending.
	if len(pendingBatchTools) > 0 {
		summary.PendingToolCall = true
		if batchHasAskQuestion {
			summary.PendingAskQuestion = true
			summary.PendingToolName = "ask_question"
		} else if summary.PendingToolName == "" {
			summary.PendingToolName = pendingBatchTools[0]
		}
	}

	return summary
}

// CheckChildSubprocesses inspects whether the AGY CLI process or its language
// server has any unexpected child processes (such as a running shell command or
// sub-process tool).
func CheckChildSubprocesses(childPID, serverPID int) (int, error) {
	if childPID <= 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,comm=")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	childrenOf := map[int][]int{}
	commOf := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		ppid, e2 := strconv.Atoi(fields[1])
		if e1 != nil || e2 != nil {
			continue
		}
		childrenOf[ppid] = append(childrenOf[ppid], pid)
		commOf[pid] = strings.Join(fields[2:], " ")
	}
	// Count descendants of childPID excluding serverPID itself (unless serverPID has children!)
	activeTools := 0
	var walk func(int)
	walk = func(p int) {
		for _, ch := range childrenOf[p] {
			if ch == serverPID {
				// Walk inside language_server: any child of language_server is a tool/shell/subagent!
				for _, lsChild := range childrenOf[ch] {
					activeTools++
					walk(lsChild)
				}
				continue
			}
			// If serverPID is not yet known from log, skip the single language_server process itself
			base := filepath.Base(commOf[ch])
			if serverPID == 0 && (strings.Contains(base, "language_server") || base == "agy") {
				walk(ch)
				continue
			}
			activeTools++
			walk(ch)
		}
	}
	walk(childPID)
	return activeTools, nil
}

// AGYEvaluationInput combines all observed signals for a single evaluation tick.
type AGYEvaluationInput struct {
	Now                     time.Time
	Policy                  AGYGuardPolicy
	Log                     AGYLogAnalysis
	LogReadable             bool
	LogRotatedOrReplaced    bool
	DB                      AGYDBSummary
	DBReadable              bool
	Transcript              AGYTranscriptSummary
	ChildAlive              bool
	ChildPIDChanged         bool
	ActiveSubprocesses      int
	SubprocessCheckFailed   bool
	UserTyping              bool
	UserStopped             bool
	UserInputEpoch          uint64
	LastEvaluatedInputEpoch uint64
	Proxy                   AGYProxyInspection
	ProxyReachable          bool
	ProxyStableFor          time.Duration
	BoundConversationID     string
	RecoveryTimestamps      []time.Time
	HandledIncidents        map[string]bool
}

// AGYEvaluationDecision is the deterministic result of evaluating all signals.
type AGYEvaluationDecision struct {
	State               AGYGuardState
	Message             string
	Reason              string
	BoundConversationID string
	BindingConfirmed    bool
	ShouldTrigger       bool
	IncidentKey         string
	Signals             []AGYSignalEvidence
}

// EvaluateAGYGuardState is the pure, deterministic decision engine for session
// state classification and bounded recovery eligibility.
func EvaluateAGYGuardState(in AGYEvaluationInput) AGYEvaluationDecision {
	in.Policy.normalize()
	var signals []AGYSignalEvidence

	if !in.ChildAlive {
		if in.Log.LastTurnStartedLine > 0 && in.Log.LastTurnCompletedLine < in.Log.LastTurnStartedLine {
			reason := "AGY 进程已退出，但最后一轮任务未见完成信号（不凭退出码宣告完成）"
			signals = append(signals, AGYSignalEvidence{Source: "process_exit", Confidence: "high", Detail: reason})
			return AGYEvaluationDecision{
				State:               AGYGuardStateWaitingUserAction,
				Message:             reason,
				Reason:              reason,
				BoundConversationID: firstNonEmpty(in.Log.ActiveConversationID, in.BoundConversationID),
				BindingConfirmed:    in.BoundConversationID != "",
				Signals:             signals,
			}
		}
		signals = append(signals, AGYSignalEvidence{Source: "process", Confidence: "high", Detail: "AGY 进程已退出"})
		return AGYEvaluationDecision{
			State:               AGYGuardStateIdle,
			Message:             "AGY CLI 已退出",
			Reason:              in.Log.LastForegroundFailureReason,
			BoundConversationID: in.BoundConversationID,
			BindingConfirmed:    in.BoundConversationID != "",
			Signals:             signals,
		}
	}

	if in.ChildPIDChanged || in.LogRotatedOrReplaced || !in.LogReadable {
		reason := "日志轮换或进程身份变化，无法确认会话归属，已暂停自动操作"
		if !in.LogReadable {
			reason = "专用日志暂不可读取，已暂停自动操作"
		}
		signals = append(signals, AGYSignalEvidence{Source: "log_identity", Confidence: "high", Detail: reason})
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: in.BoundConversationID,
			BindingConfirmed:    false,
			Signals:             signals,
		}
	}

	// 1. Verify session binding
	boundID := in.BoundConversationID
	activeID := in.Log.ActiveConversationID
	if activeID == "" {
		signals = append(signals, AGYSignalEvidence{Source: "dedicated_log", Confidence: "high", Detail: "尚未在专用日志中观测到具体会话 ID"})
		return AGYEvaluationDecision{
			State:               AGYGuardStateObserving,
			Message:             "正在观察终端启动，等待确认当前会话归属…",
			BoundConversationID: "",
			BindingConfirmed:    false,
			Signals:             signals,
		}
	}

	if boundID != "" && boundID != activeID {
		// Conversation switched inside TUI: re-verify binding in DB & transcript
		if !in.DBReadable || in.DB.ConversationID != activeID || !in.Transcript.Readable {
			reason := fmt.Sprintf("检测到会话切换（%s → %s），但新会话存储尚未核验，已暂停自动恢复", shortID(boundID), shortID(activeID))
			signals = append(signals, AGYSignalEvidence{Source: "session_switch", Confidence: "high", Detail: reason})
			return AGYEvaluationDecision{
				State:               AGYGuardStateWaitingUserAction,
				Message:             reason,
				Reason:              reason,
				BoundConversationID: activeID,
				BindingConfirmed:    false,
				Signals:             signals,
			}
		}
	}
	boundID = activeID
	bindingConfirmed := true
	signals = append(signals, AGYSignalEvidence{
		Source:     "dedicated_log",
		Confidence: "high",
		Detail:     fmt.Sprintf("已由专用日志绑定会话 %s", shortID(boundID)),
	})

	if in.DBReadable && in.DB.ConversationID == boundID {
		signals = append(signals, AGYSignalEvidence{
			Source:     "conversation_summaries.db",
			Confidence: "high",
			Detail:     fmt.Sprintf("status=%s, step_count=%d, not_fully_idle=%v", in.DB.Status, in.DB.StepCount, in.DB.NotFullyIdle),
		})
	} else {
		bindingConfirmed = false
		signals = append(signals, AGYSignalEvidence{
			Source:     "conversation_summaries.db",
			Confidence: "low",
			Detail:     "只读查询尚未匹配到该会话摘要",
		})
	}

	if in.Transcript.Readable && !in.Transcript.ParseError && !in.Transcript.TranscriptIncomplete {
		signals = append(signals, AGYSignalEvidence{
			Source:     "transcript.jsonl",
			Confidence: "high",
			Detail:     fmt.Sprintf("max_step=%d, last_non_error=%d, net_errors=%d, pending_tool=%v", in.Transcript.MaxStepIndex, in.Transcript.LastNonErrorStepIndex, in.Transcript.LastTurnErrorCount, in.Transcript.PendingToolCall),
		})
	} else {
		bindingConfirmed = false
		detail := "会话轨迹文件暂不可读"
		if in.Transcript.ParseError || in.Transcript.TranscriptIncomplete {
			detail = fmt.Sprintf("会话轨迹解析异常或不完整（parse_error=%v, incomplete=%v）", in.Transcript.ParseError, in.Transcript.TranscriptIncomplete)
		}
		signals = append(signals, AGYSignalEvidence{
			Source:     "transcript.jsonl",
			Confidence: "low",
			Detail:     detail,
		})
	}

	// 2. Check user explicit stop or typing priority
	if in.UserStopped {
		reason := "检测到用户主动停止（Ctrl+C / Esc），优先交由用户处理，不触发自动恢复"
		signals = append(signals, AGYSignalEvidence{Source: "pty_input", Confidence: "high", Detail: reason})
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	if in.UserTyping {
		reason := "用户正在终端编辑输入，暂停一切自动恢复注入"
		signals = append(signals, AGYSignalEvidence{Source: "pty_input", Confidence: "high", Detail: reason})
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// 3. Check non-transient errors (account, location, quota)
	if in.Log.HasNonTransientErrorInTurn {
		signals = append(signals, AGYSignalEvidence{Source: "dedicated_log", Confidence: "high", Detail: in.Log.NonTransientErrorDetail})
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             in.Log.NonTransientErrorDetail,
			Reason:              in.Log.NonTransientErrorDetail,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// 4. Determine whether the conversation is IDLE / completed
	turnCompletedInLog := in.Log.LastTurnCompletedLine > in.Log.LastTurnStartedLine && in.Log.LastTurnStartedLine > 0
	dbIdle := in.DBReadable && in.DB.Status == "CASCADE_RUN_STATUS_IDLE" && !in.DB.NotFullyIdle
	if (turnCompletedInLog || dbIdle) && in.Log.LastTurnCancelledLine <= in.Log.LastTurnCompletedLine {
		msg := "会话当前处于空闲状态"
		if in.Log.BackgroundNoiseCount > 0 {
			msg = fmt.Sprintf("会话已空闲（已忽略 %d 条后台配置/遥测日志报错，不会误发继续）", in.Log.BackgroundNoiseCount)
		}
		return AGYEvaluationDecision{
			State:               AGYGuardStateIdle,
			Message:             msg,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// 5. Check active tools, pending approvals, or unconfirmed background tasks
	if in.Log.ToolConfirmPendingInTurn || in.Transcript.PendingAskQuestion {
		reason := "正在等待用户授权或回答问题，不执行自动恢复"
		signals = append(signals, AGYSignalEvidence{Source: "tool_confirmation", Confidence: "high", Detail: reason})
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// Partition failure intervals by actual progress in both log and transcript so historical
	// failures that were already overcome by model/tool progress do not re-trigger recovery.
	effectiveLogFailures := in.Log.ForegroundNetFailuresInTurn
	effectiveLog5m0s := in.Log.Has5m0sTimeoutInTurn
	effectiveTranscriptErrors := in.Transcript.LastTurnErrorCount
	effectiveTranscript5m0s := in.Transcript.LastTurnHas5m0s

	if in.Log.LastForegroundFailureLine > 0 &&
		(in.Log.LastProgressLine > in.Log.LastForegroundFailureLine || in.Log.LastTurnCompletedLine > in.Log.LastForegroundFailureLine) {
		effectiveLogFailures = 0
		effectiveLog5m0s = false
	}

	maxTranscriptErrIdx := in.Transcript.LastErrorStepIndex
	for _, idx := range in.Transcript.LastTurnErrorStepIndices {
		if idx > maxTranscriptErrIdx {
			maxTranscriptErrIdx = idx
		}
	}
	for _, idx := range in.Transcript.AllTurnErrorStepIndices {
		if idx > maxTranscriptErrIdx {
			maxTranscriptErrIdx = idx
		}
	}

	overcome := in.Transcript.OvercomeTurnErrorCount
	if maxTranscriptErrIdx > 0 && in.Transcript.LastNonErrorStepIndex > maxTranscriptErrIdx {
		effectiveTranscriptErrors = 0
		effectiveTranscript5m0s = false
		if len(in.Transcript.AllTurnErrorStepIndices) > overcome {
			overcome = len(in.Transcript.AllTurnErrorStepIndices)
		}
		if len(in.Transcript.LastTurnErrorStepIndices) > overcome {
			overcome = len(in.Transcript.LastTurnErrorStepIndices)
		}
		if in.Transcript.LastTurnErrorCount > overcome {
			overcome = in.Transcript.LastTurnErrorCount
		}
		if overcome == 0 {
			overcome = effectiveLogFailures
		}
	}

	if overcome > 0 {
		if in.Log.TotalForegroundFailuresInTurn > 0 {
			effectiveLogFailures = in.Log.TotalForegroundFailuresInTurn - overcome
		} else {
			effectiveLogFailures = effectiveLogFailures - overcome
		}
		if effectiveLogFailures <= 0 {
			effectiveLogFailures = 0
			effectiveLog5m0s = false
		}
	} else if in.Transcript.TotalTurnErrorCount == 0 &&
		in.Log.LastProgressLine == 0 &&
		in.Transcript.CompletedToolStepsInTurn > 0 &&
		!in.Transcript.PendingToolCall &&
		in.Transcript.RunningStepCount == 0 {
		// When a transcript shows completed model response + completed tool step(s) in the
		// current turn and no unclosed tools, historical log failures from earlier in the
		// turn have been superseded by step progress.
		effectiveLogFailures = 0
		effectiveLog5m0s = false
	}

	hasForegroundFailure := effectiveLogFailures > 0 || effectiveTranscriptErrors > 0
	failureThresholdMet := effectiveLogFailures >= in.Policy.ConsecutiveFailures ||
		effectiveTranscriptErrors >= in.Policy.ConsecutiveFailures ||
		effectiveLog5m0s ||
		effectiveTranscript5m0s

	if !hasForegroundFailure {
		// Normal running / long thinking without foreground errors
		msg := "会话正常执行中"
		if in.ActiveSubprocesses > 0 || in.Transcript.PendingToolCall {
			msg = "正在执行工具或子进程任务"
		}
		return AGYEvaluationDecision{
			State:               AGYGuardStateRunning,
			Message:             msg,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// We have at least one foreground model network failure in the current turn.
	failureReason := in.Log.LastForegroundFailureReason
	if failureReason == "" {
		failureReason = "前台模型请求出现网络异常"
	}

	if !failureThresholdMet {
		msg := fmt.Sprintf("检测到单次模型网络重试（%s），继续观察内置重试进展", failureReason)
		return AGYEvaluationDecision{
			State:               AGYGuardStateModelRetrying,
			Message:             msg,
			Reason:              failureReason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// Failure threshold IS met (e.g., >=2 consecutive failures or 5m0s timeout without progress).
	// Verify all safety preconditions before allowing recovery!
	if !bindingConfirmed {
		reason := fmt.Sprintf("%s；但会话归属或轨迹状态信号不足，转入等待用户处理", failureReason)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    false,
			Signals:             signals,
		}
	}

	if in.SubprocessCheckFailed || in.ActiveSubprocesses > 0 || in.Transcript.PendingToolCall || in.Transcript.RunningStepCount > 0 || in.Transcript.HasBackgroundCommand || in.Log.UnfinishedBgTaskInLog {
		var blocker string
		switch {
		case in.ActiveSubprocesses > 0:
			blocker = fmt.Sprintf("检测到 %d 个活动工具/子进程仍在运行", in.ActiveSubprocesses)
		case in.Transcript.PendingToolCall:
			blocker = fmt.Sprintf("工具调用（%s）尚在执行或状态未确认", in.Transcript.PendingToolName)
		case in.Transcript.HasBackgroundCommand || in.Log.UnfinishedBgTaskInLog:
			blocker = "会话包含后台命令或子代理任务（killBackgroundTasks=false 无法保证后台任务已结束）"
		default:
			blocker = "进程树或步骤状态不可确认"
		}
		reason := fmt.Sprintf("%s；但%s，已暂停自动恢复", failureReason, blocker)
		signals = append(signals, AGYSignalEvidence{Source: "safety_gate", Confidence: "high", Detail: blocker})
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			Signals:             signals,
		}
	}

	// Check incident deduplication
	incidentKey := fmt.Sprintf("%s:step:%d:turn:%d", boundID, in.Transcript.LastNonErrorStepIndex, in.Log.LastTurnStartedLine)
	if in.HandledIncidents[incidentKey] {
		reason := fmt.Sprintf("当前故障节点已发送过一次恢复指令（%s），等待用户核查，不重复发送", failureReason)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	// Check policy switches (default off, pause, cooldown cap)
	if !in.Policy.AutoRecoveryEnabled {
		reason := fmt.Sprintf("满足恢复候选条件（%s），但自动恢复当前关闭（仅观察模式）", failureReason)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              failureReason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	if in.Policy.Paused {
		reason := fmt.Sprintf("自动恢复已暂停（%s）", firstNonEmpty(in.Policy.PauseReason, failureReason))
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	windowDur := time.Duration(in.Policy.CooldownMinutes) * time.Minute
	recentCount := 0
	for _, ts := range in.RecoveryTimestamps {
		if in.Now.Sub(ts) <= windowDur {
			recentCount++
		}
	}
	if recentCount >= in.Policy.MaxRecoveriesInWindow {
		reason := fmt.Sprintf("已达到 %d 分钟内最多自动恢复 %d 次的上限（最近原因：%s），已暂停自动尝试并请用户检查网络", in.Policy.CooldownMinutes, in.Policy.MaxRecoveriesInWindow, failureReason)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	// Check proxy configuration & network stability window
	if in.Proxy.Conflict != "" {
		reason := fmt.Sprintf("检测到代理环境变量冲突（%s），已暂停自动恢复", in.Proxy.Conflict)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingUserAction,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	requiredStable := time.Duration(in.Policy.NetworkStableSeconds) * time.Second
	if !in.ProxyReachable {
		reason := fmt.Sprintf("本地代理 %s 暂不可连，等待原代理恢复（不自动直连或切端口）", in.Proxy.HostPort)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingNetwork,
			Message:             reason,
			Reason:              reason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	if in.ProxyStableFor < requiredStable {
		rem := int((requiredStable - in.ProxyStableFor + 999*time.Millisecond) / time.Second)
		reason := fmt.Sprintf("代理 %s 已连通，等待网络稳定窗口（剩余约 %d 秒）后再执行恢复", in.Proxy.HostPort, rem)
		return AGYEvaluationDecision{
			State:               AGYGuardStateWaitingNetwork,
			Message:             reason,
			Reason:              failureReason,
			BoundConversationID: boundID,
			BindingConfirmed:    bindingConfirmed,
			IncidentKey:         incidentKey,
			Signals:             signals,
		}
	}

	// All recovery conditions and network stability requirements are met!
	return AGYEvaluationDecision{
		State:               AGYGuardStateRecovering,
		Message:             fmt.Sprintf("已确认前台网络故障（%s）且代理稳定，准备取消当前轮并发送恢复指令", failureReason),
		Reason:              failureReason,
		BoundConversationID: boundID,
		BindingConfirmed:    bindingConfirmed,
		ShouldTrigger:       true,
		IncidentKey:         incidentKey,
		Signals:             signals,
	}
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

var _ = io.EOF

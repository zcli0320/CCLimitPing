package assistant

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultAGYGuardPolicyAutoRecoveryDisabledByDefault(t *testing.T) {
	p := DefaultAGYGuardPolicy()
	if p.GuardLaunchEnabled {
		t.Fatalf("expected GuardLaunchEnabled default false, got true")
	}
	if p.AutoRecoveryEnabled {
		t.Fatalf("expected AutoRecoveryEnabled default false on delivery, got true")
	}
	if p.CooldownMinutes != 30 {
		t.Fatalf("expected CooldownMinutes 30, got %d", p.CooldownMinutes)
	}
	if p.MaxRecoveriesInWindow != 2 {
		t.Fatalf("expected MaxRecoveriesInWindow 2, got %d", p.MaxRecoveriesInWindow)
	}
	if p.NetworkStableSeconds != 20 {
		t.Fatalf("expected NetworkStableSeconds 20, got %d", p.NetworkStableSeconds)
	}
}

func TestParseAGYDedicatedLogIgnoresIdleBackgroundNoiseAndDetectsForegroundFailures(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dedicated.log")
	convID := "2bb57372-64fb-4dc7-b5b5-b66fd6da3c6d"

	// Scenario 2: Session completed into IDLE, followed by continuous fetchConfig,
	// listExperiments, telemetry, and safety monitor EOF errors.
	idleWithNoise := strings.Join([]string{
		`I0419 10:00:01.000000 12345 client.go:196] Starting language server process with pid 54321`,
		`I0419 10:00:02.000000 12345 app.go:686] Streaming conversation ` + convID,
		`I0419 10:00:03.000000 12345 app.go:793] Forwarding user message to conversation ` + convID,
		`I0419 10:00:25.000000 12345 app.go:764] Stream completed for ` + convID + `, clearing ResponsePending`,
		`E0419 10:01:00.000000 54321 client.go:530] AICode FetchConfig failed: Post "https://aicode.googleapis.com/v1alpha:fetchConfig": EOF`,
		`W0419 10:02:00.000000 54321 log.go:258] Failed to poll ListExperiments: Post "https://daily-cloudcode-pa.googleapis.com/v1internal:listExperiments": read tcp 127.0.0.1:50000->127.0.0.1:1082: read: connection reset by peer`,
		`E0419 10:03:00.000000 54321 log.go:268] Post "https://play.googleapis.com/log?format=json": EOF`,
		`E0419 10:04:00.000000 54321 cache.go:180] Cache(loadCodeAssistResponse): Failed to refresh cache in background: EOF`,
		`W0419 10:05:00.000000 54321 run.go:288] Monitor sir_flash_lite failed: Post "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse": EOF`,
	}, "\n") + "\n"

	if err := os.WriteFile(logPath, []byte(idleWithNoise), 0o600); err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseAGYDedicatedLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ServerPID != 54321 {
		t.Fatalf("expected ServerPID 54321, got %d", parsed.ServerPID)
	}
	if parsed.ActiveConversationID != convID {
		t.Fatalf("expected ActiveConversationID %s, got %s", convID, parsed.ActiveConversationID)
	}
	if parsed.ForegroundNetFailuresInTurn != 0 {
		t.Fatalf("expected 0 foreground failures after background noise, got %d", parsed.ForegroundNetFailuresInTurn)
	}
	if parsed.BackgroundNoiseCount != 5 {
		t.Fatalf("expected 5 background noise lines ignored, got %d", parsed.BackgroundNoiseCount)
	}
	if parsed.LastTurnCompletedLine <= parsed.LastTurnStartedLine {
		t.Fatalf("expected turn completed line (%d) > started line (%d)", parsed.LastTurnCompletedLine, parsed.LastTurnStartedLine)
	}

	// Now append a new user turn with 2 foreground network failures and cancellation confirmation.
	activeWithFails := idleWithNoise + strings.Join([]string{
		`I0419 10:10:00.000000 12345 app.go:793] Forwarding user message to conversation ` + convID,
		`W0419 10:10:15.000000 54321 run.go:301] Run: attempt 1 failed (request failed (200 OK): error reading from server: unexpected EOF), retrying in 480ms`,
		`W0419 10:10:30.000000 54321 run.go:301] Run: attempt 2 failed (Post "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse": read tcp 127.0.0.1:51111->127.0.0.1:1082: read: connection reset by peer), retrying in 1.2s`,
		`I0419 10:10:35.000000 12345 app.go:932] Cancelling in-progress response for conversation ` + convID,
		`I0419 10:10:35.100000 54321 server.go:2589] Cancelling conversation ` + convID + ` (killBackgroundTasks=false)`,
		`I0419 10:10:35.200000 12345 app.go:730] Stream goroutine exited for ` + convID + `, sending completion signal`,
	}, "\n") + "\n"

	if err := os.WriteFile(logPath, []byte(activeWithFails), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed2, err := ParseAGYDedicatedLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if parsed2.ForegroundNetFailuresInTurn != 2 {
		t.Fatalf("expected 2 foreground failures in current turn, got %d", parsed2.ForegroundNetFailuresInTurn)
	}
	if !parsed2.CancelConfirmedAfterRequest {
		t.Fatalf("expected CancelConfirmedAfterRequest=true after Cancelling + Stream goroutine exited")
	}
	if !parsed2.LastCancelKillBgFalse {
		t.Fatalf("expected LastCancelKillBgFalse=true")
	}
}

func TestAGYInspectProxyEnvPreserves17893And1082AndDetectsConflicts(t *testing.T) {
	// 1. Default 1082 when no env vars set
	pDefault := InspectProxyEnv([]string{"PATH=/usr/bin"})
	if pDefault.HostPort != "127.0.0.1:1082" || pDefault.Conflict != "" {
		t.Fatalf("unexpected default proxy inspection: %+v", pDefault)
	}

	// 2. Claude's 17893 preserved without overriding to 1082
	pClaude := InspectProxyEnv([]string{
		"HTTPS_PROXY=http://127.0.0.1:17893",
		"HTTP_PROXY=http://127.0.0.1:17893",
	})
	if pClaude.HostPort != "127.0.0.1:17893" || pClaude.Conflict != "" {
		t.Fatalf("expected Claude proxy 127.0.0.1:17893 preserved without conflict, got %+v", pClaude)
	}

	// 3. Conflicting uppercase/lowercase proxy vars reported
	pConflict := InspectProxyEnv([]string{
		"HTTPS_PROXY=http://127.0.0.1:17893",
		"https_proxy=http://127.0.0.1:1082",
	})
	if pConflict.Conflict == "" {
		t.Fatalf("expected conflict reported between 17893 and 1082, got none: %+v", pConflict)
	}
}

func TestEvaluateAGYGuardStateAllVerificationScenarios(t *testing.T) {
	now := time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC)
	convID := "2bb57372-64fb-4dc7-b5b5-b66fd6da3c6d"

	baseInput := func() AGYEvaluationInput {
		policy := DefaultAGYGuardPolicy()
		policy.GuardLaunchEnabled = true
		policy.AutoRecoveryEnabled = true
		return AGYEvaluationInput{
			Now:                 now,
			Policy:              policy,
			BoundConversationID: convID,
			LogReadable:         true,
			Log: AGYLogAnalysis{
				ServerPID:            54321,
				ActiveConversationID: convID,
				ForwardingCount:      1,
				LastTurnStartedLine:  10,
			},
			DBReadable: true,
			DB: AGYDBSummary{
				ConversationID: convID,
				Status:         "CASCADE_RUN_STATUS_RUNNING",
				StepCount:      12,
				NotFullyIdle:   true,
			},
			Transcript: AGYTranscriptSummary{
				Readable:              true,
				MaxStepIndex:          11,
				LastNonErrorStepIndex: 11,
				LastStepType:          "PLANNER_RESPONSE",
				LastStepStatus:        "DONE",
			},
			ChildAlive:       true,
			Proxy:            AGYProxyInspection{ProxyURL: "http://127.0.0.1:1082", HostPort: "127.0.0.1:1082"},
			ProxyReachable:   true,
			ProxyStableFor:   25 * time.Second,
			HandledIncidents: map[string]bool{},
		}
	}

	t.Run("1_NormalLongThinkingWithoutErrors_DoesNotInterrupt", func(t *testing.T) {
		in := baseInput()
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateRunning || dec.ShouldTrigger {
			t.Fatalf("expected Running with ShouldTrigger=false, got state=%s trigger=%v msg=%s", dec.State, dec.ShouldTrigger, dec.Message)
		}
	})

	t.Run("2_IdleSessionWithContinuousConfigAndTelemetryEOF_DoesNotRecover", func(t *testing.T) {
		in := baseInput()
		in.Log.LastTurnCompletedLine = 25
		in.Log.BackgroundNoiseCount = 18
		in.DB.Status = "CASCADE_RUN_STATUS_IDLE"
		in.DB.NotFullyIdle = false
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateIdle || dec.ShouldTrigger {
			t.Fatalf("expected Idle with ShouldTrigger=false, got state=%s trigger=%v msg=%s", dec.State, dec.ShouldTrigger, dec.Message)
		}
	})

	t.Run("3a_SingleForegroundNetworkFailure_EntersModelRetrying", func(t *testing.T) {
		in := baseInput()
		in.Log.ForegroundNetFailuresInTurn = 1
		in.Log.LastForegroundFailureReason = "前台模型请求连接异常中断（EOF）"
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateModelRetrying || dec.ShouldTrigger {
			t.Fatalf("expected ModelRetrying with ShouldTrigger=false, got state=%s trigger=%v", dec.State, dec.ShouldTrigger)
		}
	})

	t.Run("3b_ConsecutiveForegroundNetworkFailures_DefaultPolicyOff_StaysInObservation", func(t *testing.T) {
		in := baseInput()
		in.Policy.AutoRecoveryEnabled = false // Delivery default!
		in.Log.ForegroundNetFailuresInTurn = 2
		in.Log.LastForegroundFailureReason = "前台模型请求连接被重置（connection reset by peer）"
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateWaitingUserAction || dec.ShouldTrigger {
			t.Fatalf("expected WaitingUserAction & ShouldTrigger=false when AutoRecoveryEnabled=false, got state=%s trigger=%v", dec.State, dec.ShouldTrigger)
		}
		if !strings.Contains(dec.Message, "自动恢复当前关闭") {
			t.Fatalf("expected message to mention auto-recovery disabled, got: %s", dec.Message)
		}
	})

	t.Run("3c_ConsecutiveForegroundNetworkFailures_Or5m0sTimeout_EntersRecoveryWhenEnabled", func(t *testing.T) {
		in := baseInput()
		in.Log.ForegroundNetFailuresInTurn = 2
		in.Log.LastForegroundFailureReason = "前台模型请求连接被重置（connection reset by peer）"
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateRecovering || !dec.ShouldTrigger {
			t.Fatalf("expected Recovering & ShouldTrigger=true, got state=%s trigger=%v msg=%s", dec.State, dec.ShouldTrigger, dec.Message)
		}

		// Also verify single 5m0s stream idle timeout triggers candidate
		in5m := baseInput()
		in5m.Log.ForegroundNetFailuresInTurn = 1
		in5m.Log.Has5m0sTimeoutInTurn = true
		in5m.Log.LastForegroundFailureReason = "前台模型流超时（stream idle timeout 5m0s）"
		dec5m := EvaluateAGYGuardState(in5m)
		if dec5m.State != AGYGuardStateRecovering || !dec5m.ShouldTrigger {
			t.Fatalf("expected 5m0s timeout to trigger recovery, got state=%s trigger=%v", dec5m.State, dec5m.ShouldTrigger)
		}
	})

	t.Run("4_ActiveToolOrUnfinishedBackgroundTask_PausesAutoRecovery", func(t *testing.T) {
		// Case A: Active subprocess in process tree
		in := baseInput()
		in.Log.ForegroundNetFailuresInTurn = 2
		in.ActiveSubprocesses = 1
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateWaitingUserAction || dec.ShouldTrigger {
			t.Fatalf("expected WaitingUserAction when subprocess active, got state=%s trigger=%v", dec.State, dec.ShouldTrigger)
		}

		// Case B: Pending tool call in transcript
		inTool := baseInput()
		inTool.Log.ForegroundNetFailuresInTurn = 2
		inTool.Transcript.PendingToolCall = true
		inTool.Transcript.PendingToolName = "run_command"
		decTool := EvaluateAGYGuardState(inTool)
		if decTool.State != AGYGuardStateWaitingUserAction || decTool.ShouldTrigger {
			t.Fatalf("expected WaitingUserAction when tool pending, got state=%s trigger=%v", decTool.State, decTool.ShouldTrigger)
		}

		// Case C: Background task / subagent in transcript (killBackgroundTasks=false hazard)
		inBg := baseInput()
		inBg.Log.ForegroundNetFailuresInTurn = 2
		inBg.Transcript.HasBackgroundCommand = true
		decBg := EvaluateAGYGuardState(inBg)
		if decBg.State != AGYGuardStateWaitingUserAction || decBg.ShouldTrigger {
			t.Fatalf("expected WaitingUserAction when background task exists, got state=%s trigger=%v", decBg.State, decBg.ShouldTrigger)
		}
	})

	t.Run("5_AwaitingApprovalOrUserTyping_NeverInjectsInput", func(t *testing.T) {
		// Tool confirmation pending
		inConfirm := baseInput()
		inConfirm.Log.ForegroundNetFailuresInTurn = 2
		inConfirm.Log.ToolConfirmPendingInTurn = true
		decConfirm := EvaluateAGYGuardState(inConfirm)
		if decConfirm.State != AGYGuardStateWaitingUserAction || decConfirm.ShouldTrigger {
			t.Fatalf("expected WaitingUserAction on tool confirmation, got state=%s trigger=%v", decConfirm.State, decConfirm.ShouldTrigger)
		}

		// User typing in PTY
		inTyping := baseInput()
		inTyping.Log.ForegroundNetFailuresInTurn = 2
		inTyping.UserTyping = true
		decTyping := EvaluateAGYGuardState(inTyping)
		if decTyping.State != AGYGuardStateWaitingUserAction || decTyping.ShouldTrigger {
			t.Fatalf("expected WaitingUserAction when user typing, got state=%s trigger=%v", decTyping.State, decTyping.ShouldTrigger)
		}
	})

	t.Run("6_ProxyPortDisappearsThenRecovers_WaitsFor20sStabilityAndPreserves17893", func(t *testing.T) {
		in := baseInput()
		in.Log.ForegroundNetFailuresInTurn = 2
		in.Proxy = AGYProxyInspection{ProxyURL: "http://127.0.0.1:17893", HostPort: "127.0.0.1:17893"}

		// Proxy down
		in.ProxyReachable = false
		in.ProxyStableFor = 0
		decDown := EvaluateAGYGuardState(in)
		if decDown.State != AGYGuardStateWaitingNetwork || decDown.ShouldTrigger {
			t.Fatalf("expected WaitingNetwork when proxy 17893 unreachable, got state=%s trigger=%v", decDown.State, decDown.ShouldTrigger)
		}
		if !strings.Contains(decDown.Message, "127.0.0.1:17893") {
			t.Fatalf("expected message to reference preserved proxy 17893, got: %s", decDown.Message)
		}

		// Proxy back up for only 5 seconds (< 20s window)
		in.ProxyReachable = true
		in.ProxyStableFor = 5 * time.Second
		decWarming := EvaluateAGYGuardState(in)
		if decWarming.State != AGYGuardStateWaitingNetwork || decWarming.ShouldTrigger {
			t.Fatalf("expected WaitingNetwork while stability window < 20s, got state=%s trigger=%v", decWarming.State, decWarming.ShouldTrigger)
		}

		// Proxy stable for 21 seconds (>= 20s window)
		in.ProxyStableFor = 21 * time.Second
		decReady := EvaluateAGYGuardState(in)
		if decReady.State != AGYGuardStateRecovering || !decReady.ShouldTrigger {
			t.Fatalf("expected Recovering once proxy 17893 stable >= 20s, got state=%s trigger=%v", decReady.State, decReady.ShouldTrigger)
		}
	})

	t.Run("8_UserStopTakesPriorityOverAutoRecovery", func(t *testing.T) {
		in := baseInput()
		in.Log.ForegroundNetFailuresInTurn = 3
		in.UserStopped = true
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateWaitingUserAction || dec.ShouldTrigger {
			t.Fatalf("expected user stop to preempt auto-recovery, got state=%s trigger=%v", dec.State, dec.ShouldTrigger)
		}
	})

	t.Run("10_SessionSwitchLogRotationOrPIDChange_PausesOrRebindsSafely", func(t *testing.T) {
		// Unverified session switch pauses
		inSwitch := baseInput()
		inSwitch.Log.ActiveConversationID = "99999999-9999-9999-9999-999999999999"
		inSwitch.DB.ConversationID = convID // DB still on old conversation
		inSwitch.Log.ForegroundNetFailuresInTurn = 2
		decSwitch := EvaluateAGYGuardState(inSwitch)
		if decSwitch.State != AGYGuardStateWaitingUserAction || decSwitch.ShouldTrigger || decSwitch.BindingConfirmed {
			t.Fatalf("expected unverified session switch to pause recovery, got %+v", decSwitch)
		}

		// Log rotation pauses
		inRot := baseInput()
		inRot.LogRotatedOrReplaced = true
		inRot.Log.ForegroundNetFailuresInTurn = 2
		decRot := EvaluateAGYGuardState(inRot)
		if decRot.State != AGYGuardStateWaitingUserAction || decRot.ShouldTrigger {
			t.Fatalf("expected log rotation to pause recovery, got %+v", decRot)
		}

		// Child PID change pauses
		inPID := baseInput()
		inPID.ChildPIDChanged = true
		inPID.Log.ForegroundNetFailuresInTurn = 2
		decPID := EvaluateAGYGuardState(inPID)
		if decPID.State != AGYGuardStateWaitingUserAction || decPID.ShouldTrigger {
			t.Fatalf("expected PID change to pause recovery, got %+v", decPID)
		}
	})

	t.Run("11_ReachingRecoveryCap_StopsAttemptsAndPauses", func(t *testing.T) {
		in := baseInput()
		in.Log.ForegroundNetFailuresInTurn = 2
		in.RecoveryTimestamps = []time.Time{
			now.Add(-15 * time.Minute),
			now.Add(-5 * time.Minute),
		}
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateWaitingUserAction || dec.ShouldTrigger {
			t.Fatalf("expected recovery cap (2 in 30m) to block 3rd attempt, got state=%s trigger=%v", dec.State, dec.ShouldTrigger)
		}
		if !strings.Contains(dec.Message, "上限") {
			t.Fatalf("expected cap message, got: %s", dec.Message)
		}
	})

	t.Run("12_NonTransientError_LocationOrQuota_NeverRecovers", func(t *testing.T) {
		in := baseInput()
		in.Log.HasNonTransientErrorInTurn = true
		in.Log.NonTransientErrorDetail = "地区受限（User location is not supported），不执行自动恢复"
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateWaitingUserAction || dec.ShouldTrigger {
			t.Fatalf("expected non-transient error to block recovery, got state=%s trigger=%v", dec.State, dec.ShouldTrigger)
		}
	})

	t.Run("13_ExitCodeZeroWithIncompleteTurn_DoesNotClaimTaskCompletion", func(t *testing.T) {
		in := baseInput()
		in.ChildAlive = false
		in.Log.LastTurnStartedLine = 20
		in.Log.LastTurnCompletedLine = 10 // Turn 20 never completed before exit!
		dec := EvaluateAGYGuardState(in)
		if dec.State != AGYGuardStateWaitingUserAction || dec.ShouldTrigger {
			t.Fatalf("expected incomplete turn on exit to enter WaitingUserAction, got state=%s msg=%s", dec.State, dec.Message)
		}
	})
}

func TestAGYMultiSessionIsolationAndAbnormalRunnerExitCleanup(t *testing.T) {
	home := t.TempDir()
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}

	// Session A: dead runner PID (999999) while child is also gone -> transitions cleanly without signaling anything
	sessA := AGYGuardSession{
		LaunchID:       "agy-session-a",
		RunnerPID:      999999,
		ChildPID:       999998,
		Folder:         "/tmp/proj-a",
		ConversationID: "11111111-1111-1111-1111-111111111111",
		State:          AGYGuardStateRunning,
		StartedAt:      time.Now().UTC().Add(-10 * time.Minute),
		UpdatedAt:      time.Now().UTC().Add(-1 * time.Minute),
	}
	if err := r.writeAGYSession(sessA); err != nil {
		t.Fatal(err)
	}

	// Session B: active runner (current test process PID) on a different conversation
	sessB := AGYGuardSession{
		LaunchID:         "agy-session-b",
		RunnerPID:        os.Getpid(),
		ChildPID:         os.Getpid(),
		Folder:           "/tmp/proj-b",
		ConversationID:   "22222222-2222-2222-2222-222222222222",
		BindingConfirmed: true,
		State:            AGYGuardStateRunning,
		Message:          "会话 B 正常运行",
		StartedAt:        time.Now().UTC().Add(-5 * time.Minute),
		UpdatedAt:        time.Now().UTC(),
	}
	if err := r.writeAGYSession(sessB); err != nil {
		t.Fatal(err)
	}

	report := r.AGYGuardStatus()
	if report.ActiveCount != 1 {
		t.Fatalf("expected 1 active session and 1 cleaned exited session, got active=%d total=%d", report.ActiveCount, len(report.Sessions))
	}
	if report.Sessions[0].LaunchID != "agy-session-b" || report.Sessions[0].ConversationID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("expected active session B first, got %+v", report.Sessions[0])
	}
	if !report.Sessions[1].Exited || report.Sessions[1].State != AGYGuardStateIdle {
		t.Fatalf("expected crashed runner session A marked Exited/Idle, got %+v", report.Sessions[1])
	}
}

func TestManagedAGYPTYRunnerEndToEndWithMockAGYAndSpacesInPath(t *testing.T) {
	// Start a local TCP listener to simulate a healthy local proxy (e.g. 127.0.0.1:<port>)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	home := t.TempDir()
	agyRoot := filepath.Join(home, ".gemini", "antigravity-cli")
	convID := "33333333-4444-5555-6666-777777777777"
	transcriptDir := filepath.Join(agyRoot, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create SQLite conversation_summaries.db and transcript.jsonl for convID
	dbPath := filepath.Join(agyRoot, "conversation_summaries.db")
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	createSQL := fmt.Sprintf(
		"CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, status TEXT, step_count INTEGER, not_fully_idle INTEGER, killed INTEGER, last_user_input_step_index INTEGER, workspace_uris TEXT); "+
			"INSERT INTO conversation_summaries VALUES ('%s', 'CASCADE_RUN_STATUS_RUNNING', 4, 1, 0, 1, 'file:///tmp');",
		convID,
	)
	if out, err := exec.Command("/usr/bin/sqlite3", dbPath, createSQL).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 init failed: %v (%s)", err, string(out))
	}

	transcriptFile := filepath.Join(transcriptDir, "transcript.jsonl")
	transcriptLines := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptFile, []byte(transcriptLines), 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a workspace folder containing spaces and single quotes to test argument forwarding
	workspace := filepath.Join(home, "My Project's Folder 2026")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	// Write a mock `agy` script that:
	// 1. Verifies `--dangerously-skip-permissions --add-dir <workspace> --log-file <log>`
	// 2. Writes initial turn + 2 consecutive EOF failures to `<log>`
	// 3. Reads PTY stdin: when it receives Esc (\x1b), writes cancellation confirmation to `<log>`
	// 4. When it receives the bounded recovery prompt, writes `Stream completed` and exits 0.
	mockAGY := filepath.Join(home, "mock-agy.sh")
	markerFile := filepath.Join(home, "recovered.txt")
	script := fmt.Sprintf(`#!/bin/zsh
set -e
LOG_FILE=""
ADD_DIR=""
SKIP_PERM=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dangerously-skip-permissions) SKIP_PERM=1; shift ;;
    --add-dir) ADD_DIR="$2"; shift 2 ;;
    --log-file) LOG_FILE="$2"; shift 2 ;;
    *) shift ;;
  esac
done

if [[ "$SKIP_PERM" != "1" || "$ADD_DIR" != %q || -z "$LOG_FILE" ]]; then
  echo "bad args: skip=$SKIP_PERM dir=$ADD_DIR log=$LOG_FILE" >&2
  exit 2
fi

echo "I0419 12:00:00.000000 $$ client.go:196] Starting language server process with pid $$" >> "$LOG_FILE"
echo "I0419 12:00:00.100000 $$ app.go:686] Streaming conversation %s" >> "$LOG_FILE"
echo "I0419 12:00:00.200000 $$ app.go:793] Forwarding user message to conversation %s" >> "$LOG_FILE"
echo "W0419 12:00:00.300000 $$ run.go:301] Run: attempt 1 failed (request failed: unexpected EOF), retrying in 100ms" >> "$LOG_FILE"
echo "W0419 12:00:00.400000 $$ run.go:301] Run: attempt 2 failed (request failed: connection reset by peer), retrying in 200ms" >> "$LOG_FILE"

PY_CODE='
import os, select, sys, termios, time, tty

log_file, conv_id, marker_file = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    tty.setraw(0)
except Exception:
    pass

buf = b""
cancelled = False
deadline = time.time() + 10.0

while time.time() < deadline:
    rlist, _, _ = select.select([0], [], [], 0.05)
    if not rlist:
        continue
    ch = os.read(0, 1024)
    if not ch:
        continue
    buf += ch
    if not cancelled and b"\x1b" in buf:
        cancelled = True
        buf = b""
        with open(log_file, "a") as f:
            f.write(f"I0419 12:00:01.000000 {os.getpid()} app.go:932] Cancelling in-progress response for conversation {conv_id}\n")
            f.write(f"I0419 12:00:01.050000 {os.getpid()} server.go:2589] Cancelling conversation {conv_id} (killBackgroundTasks=false)\n")
            f.write(f"I0419 12:00:01.100000 {os.getpid()} app.go:730] Stream goroutine exited for {conv_id}, sending completion signal\n")
            f.flush()
    elif cancelled and "请先核对已完成的文件修改和命令结果".encode("utf-8") in buf and (b"\r" in buf or b"\n" in buf):
        with open(marker_file, "w") as mf:
            mf.write(buf.decode("utf-8", errors="replace"))
        with open(log_file, "a") as f:
            f.write(f"I0419 12:00:02.000000 {os.getpid()} app.go:793] Forwarding user message to conversation {conv_id}\n")
            f.write(f"I0419 12:00:02.500000 {os.getpid()} app.go:764] Stream completed for {conv_id}, clearing ResponsePending\n")
            f.flush()
        sys.exit(0)
sys.exit(3)
'
exec python3 -u -c "$PY_CODE" "$LOG_FILE" %q %q
`, workspace, convID, convID, convID, markerFile)

	if err := os.WriteFile(mockAGY, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	// Enable auto-recovery with a 1-second network stability window for fast offline test execution
	_, err = r.SaveAGYGuardPolicy(func(p *AGYGuardPolicy) {
		p.GuardLaunchEnabled = true
		p.AutoRecoveryEnabled = true
		p.NetworkStableSeconds = 1
		p.ConsecutiveFailures = 2
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEX_HELPER_AGY_BIN", mockAGY)
	t.Setenv("CODEX_HELPER_AGY_HOME", agyRoot)
	t.Setenv("CODEX_HELPER_AGY_POLL_MS", "100")
	t.Setenv("HTTPS_PROXY", "http://"+ln.Addr().String())
	t.Setenv("HTTP_PROXY", "http://"+ln.Addr().String())
	t.Setenv("https_proxy", "")
	t.Setenv("http_proxy", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("all_proxy", "")

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	code, err := r.RunManagedAGYSession(ctx, workspace, nil)
	if err != nil {
		t.Fatalf("RunManagedAGYSession returned error: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0 after bounded recovery, got %d; guard status: %+v", code, r.AGYGuardStatus().Sessions)
	}

	recoveredBytes, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("expected recovery prompt marker file to be written: %v", err)
	}
	if !strings.Contains(string(recoveredBytes), AGYRecoveryPrompt) {
		t.Fatalf("expected recovery prompt in PTY input, got: %q", string(recoveredBytes))
	}

	status := r.AGYGuardStatus()
	if len(status.Sessions) != 1 {
		t.Fatalf("expected 1 recorded session, got %d", len(status.Sessions))
	}
	s := status.Sessions[0]
	if s.ConversationID != convID || s.RecoveryCountWindow != 1 || s.State != AGYGuardStateIdle {
		t.Fatalf("unexpected final session state: %+v", s)
	}
}

func TestConfigureAGYGuardPolicyPausedFlagAndRejectsUnknownArgs(t *testing.T) {
	home := t.TempDir()
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}

	// 1. Default policy starts with Paused=false, AutoRecoveryEnabled=false
	p0 := r.ReadAGYGuardPolicy()
	if p0.Paused || p0.AutoRecoveryEnabled {
		t.Fatalf("expected default policy Paused=false and AutoRecoveryEnabled=false, got %+v", p0)
	}

	// 2. Swift UI sends --paused on / --paused off
	p1, err := r.ConfigureAGYGuardPolicy([]string{"--paused", "on"})
	if err != nil {
		t.Fatalf("expected --paused on to succeed, got error: %v", err)
	}
	if !p1.Paused || !r.ReadAGYGuardPolicy().Paused {
		t.Fatalf("expected Paused=true after --paused on, got %+v", p1)
	}

	p2, err := r.ConfigureAGYGuardPolicy([]string{"--paused", "off"})
	if err != nil {
		t.Fatalf("expected --paused off to succeed, got error: %v", err)
	}
	if p2.Paused || r.ReadAGYGuardPolicy().Paused {
		t.Fatalf("expected Paused=false after --paused off, got %+v", p2)
	}

	// 3. Also accepts --pause on for CLI compatibility
	p3, err := r.ConfigureAGYGuardPolicy([]string{"--pause", "on"})
	if err != nil {
		t.Fatalf("expected --pause on to succeed, got error: %v", err)
	}
	if !p3.Paused {
		t.Fatalf("expected Paused=true after --pause on, got %+v", p3)
	}

	// 4. Unknown flags, missing values, or invalid values must fail explicitly and NOT mutate state
	badCases := [][]string{
		{"--unknown-flag", "on"},
		{"--paused"},
		{"--paused", "maybe"},
		{"--auto-recovery", "invalid"},
		{"--max-recoveries", "0"},
		{"--cooldown-minutes", "-5"},
		{"--stable-seconds", "abc"},
		{"--paused", "off", "--bogus"},
	}
	for _, args := range badCases {
		if _, err := r.ConfigureAGYGuardPolicy(args); err == nil {
			t.Fatalf("expected ConfigureAGYGuardPolicy(%v) to return error, but got nil", args)
		}
		// Ensure previous state (Paused=true) was not corrupted by partial application
		if !r.ReadAGYGuardPolicy().Paused {
			t.Fatalf("failed ConfigureAGYGuardPolicy(%v) mutated persisted policy", args)
		}
	}
}

func TestAGYProgressAfterEOFClearsHistoricalFailures(t *testing.T) {
	convID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tmpDir := t.TempDir()

	writeAndParseLog := func(name string, lines []string) AGYLogAnalysis {
		t.Helper()
		p := filepath.Join(tmpDir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseAGYDedicatedLog(p)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}

	// Scenario A: 2 EOF errors in log & transcript, followed by successful model response and completed tool step
	logAnalysis := writeAndParseLog("progress.log", []string{
		"I0419 10:00:00.000000 50001 client.go:196] Starting language server process with pid 50002",
		"I0419 10:00:01.000000 50001 app.go:686] Streaming conversation " + convID,
		"I0419 10:00:02.000000 50001 app.go:793] Forwarding user message to conversation " + convID,
		"W0419 10:00:05.000000 50002 run.go:301] Run: attempt 1 failed (request failed: Post \"https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse\": unexpected EOF), retrying in 236ms",
		"W0419 10:00:10.000000 50002 run.go:301] Run: attempt 2 failed (request failed: Post \"https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse\": connection reset by peer), retrying in 412ms",
		"I0419 10:00:15.000000 50002 runner.go:607] Monitor agy_tool_safety FINISHED in 120ms",
	})
	if logAnalysis.ForegroundNetFailuresInTurn != 0 {
		t.Fatalf("expected ForegroundNetFailuresInTurn=0 after model/tool progress line, got %d", logAnalysis.ForegroundNetFailuresInTurn)
	}
	if logAnalysis.TotalForegroundFailuresInTurn != 2 {
		t.Fatalf("expected TotalForegroundFailuresInTurn=2, got %d", logAnalysis.TotalForegroundFailuresInTurn)
	}

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	transcriptContent := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`,
		`{"step_index":1,"type":"ERROR_MESSAGE","status":"DONE","content":"request failed: unexpected EOF"}`,
		`{"step_index":2,"type":"ERROR_MESSAGE","status":"DONE","content":"request failed: connection reset by peer"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"id":"c1","name":"read_file"}]}`,
		`{"step_index":4,"type":"GENERIC","status":"DONE"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}
	trSummary := ParseAGYTranscriptFile(transcriptPath)
	if !trSummary.Readable || trSummary.ParseError || trSummary.TranscriptIncomplete {
		t.Fatalf("expected valid readable transcript, got %+v", trSummary)
	}
	if trSummary.LastTurnErrorCount != 0 {
		t.Fatalf("expected LastTurnErrorCount=0 after step progress (steps 3,4), got %d", trSummary.LastTurnErrorCount)
	}
	if trSummary.OvercomeTurnErrorCount != 2 {
		t.Fatalf("expected OvercomeTurnErrorCount=2, got %d", trSummary.OvercomeTurnErrorCount)
	}

	policy := DefaultAGYGuardPolicy()
	policy.AutoRecoveryEnabled = true

	// Even if log did NOT emit a runner.go line, transcript step advancement (steps 3 & 4 after error steps 1 & 2)
	// must suppress historical log failures in EvaluateAGYGuardState!
	rawLogWithoutRunnerLine := writeAndParseLog("no_runner.log", []string{
		"I0419 10:00:00.000000 50001 client.go:196] Starting language server process with pid 50002",
		"I0419 10:00:01.000000 50001 app.go:686] Streaming conversation " + convID,
		"I0419 10:00:02.000000 50001 app.go:793] Forwarding user message to conversation " + convID,
		"W0419 10:00:05.000000 50002 run.go:301] Run: attempt 1 failed (request failed: unexpected EOF), retrying in 236ms",
		"W0419 10:00:10.000000 50002 run.go:301] Run: attempt 2 failed (request failed: connection reset by peer), retrying in 412ms",
	})

	dec := EvaluateAGYGuardState(AGYEvaluationInput{
		Policy:              policy,
		BoundConversationID: convID,
		LogReadable:         true,
		Log:                 rawLogWithoutRunnerLine,
		DB: AGYDBSummary{
			ConversationID: convID,
			Status:         "CASCADE_RUN_STATUS_RUNNING",
			StepCount:      5,
			NotFullyIdle:   true,
		},
		DBReadable:     true,
		Transcript:     trSummary,
		ChildAlive:     true,
		ProxyReachable: true,
		ProxyStableFor: 25 * time.Second,
		Now:            time.Now(),
	})
	if dec.ShouldTrigger || dec.State != AGYGuardStateRunning {
		t.Fatalf("expected ShouldTrigger=false and State=Running after progress overcame 2 EOFs, got State=%s ShouldTrigger=%v Reason=%s", dec.State, dec.ShouldTrigger, dec.Reason)
	}

	// Furthermore, if 1 new EOF happens on the next step after progress, it must be counted as 1 (ModelRetrying), NOT 3 (ShouldTrigger)!
	logWithOneNewEOF := writeAndParseLog("one_new_eof.log", []string{
		"I0419 10:00:00.000000 50001 client.go:196] Starting language server process with pid 50002",
		"I0419 10:00:01.000000 50001 app.go:686] Streaming conversation " + convID,
		"I0419 10:00:02.000000 50001 app.go:793] Forwarding user message to conversation " + convID,
		"W0419 10:00:05.000000 50002 run.go:301] Run: attempt 1 failed (request failed: unexpected EOF), retrying in 236ms",
		"W0419 10:00:10.000000 50002 run.go:301] Run: attempt 2 failed (request failed: connection reset by peer), retrying in 412ms",
		"W0419 10:00:25.000000 50002 run.go:301] Run: attempt 1 failed (request failed: unexpected EOF), retrying in 200ms",
	})
	if logWithOneNewEOF.ForegroundNetFailuresInTurn != 1 {
		t.Fatalf("expected new attempt 1 after attempt 2 to reset current step failure count to 1, got %d", logWithOneNewEOF.ForegroundNetFailuresInTurn)
	}
	decSingleNew := EvaluateAGYGuardState(AGYEvaluationInput{
		Policy:              policy,
		BoundConversationID: convID,
		LogReadable:         true,
		Log:                 logWithOneNewEOF,
		DB: AGYDBSummary{
			ConversationID: convID,
			Status:         "CASCADE_RUN_STATUS_RUNNING",
			StepCount:      6,
			NotFullyIdle:   true,
		},
		DBReadable:     true,
		Transcript:     trSummary,
		ChildAlive:     true,
		ProxyReachable: true,
		ProxyStableFor: 25 * time.Second,
		Now:            time.Now(),
	})
	if decSingleNew.ShouldTrigger || decSingleNew.State != AGYGuardStateModelRetrying {
		t.Fatalf("expected single new EOF after progress to be ModelRetrying (ShouldTrigger=false), got State=%s ShouldTrigger=%v", decSingleNew.State, decSingleNew.ShouldTrigger)
	}
}

func TestAGYTranscriptMultiToolCallsPartialCompletionAndInvalidJSON(t *testing.T) {
	convID := "99999999-8888-7777-6666-555555555555"
	tmpDir := t.TempDir()
	policy := DefaultAGYGuardPolicy()
	policy.AutoRecoveryEnabled = true

	stalledLogFile := filepath.Join(tmpDir, "stalled.log")
	stalledLogLines := strings.Join([]string{
		"I0419 10:00:00.000000 50001 client.go:196] Starting language server process with pid 50002",
		"I0419 10:00:01.000000 50001 app.go:686] Streaming conversation " + convID,
		"I0419 10:00:02.000000 50001 app.go:793] Forwarding user message to conversation " + convID,
		"W0419 10:00:05.000000 50002 run.go:301] Run: attempt 1 failed (request failed: unexpected EOF), retrying in 236ms",
		"W0419 10:00:10.000000 50002 run.go:301] Run: attempt 2 failed (request failed: connection reset by peer), retrying in 412ms",
	}, "\n") + "\n"
	if err := os.WriteFile(stalledLogFile, []byte(stalledLogLines), 0o600); err != nil {
		t.Fatal(err)
	}
	stalledLog, err := ParseAGYDedicatedLog(stalledLogFile)
	if err != nil {
		t.Fatal(err)
	}

	// Case 1: PLANNER_RESPONSE returns 2 tool calls (read_file + run_command), but only 1 GENERIC DONE has arrived
	partialToolPath := filepath.Join(tmpDir, "partial_tool.jsonl")
	partialToolContent := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"id":"t1","name":"read_file"},{"id":"t2","name":"run_command"}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(partialToolPath, []byte(partialToolContent), 0o600); err != nil {
		t.Fatal(err)
	}
	trPartial := ParseAGYTranscriptFile(partialToolPath)
	if !trPartial.PendingToolCall || trPartial.PendingToolName != "run_command" {
		t.Fatalf("expected PendingToolCall=true and PendingToolName=run_command when 2 tools requested and 1 finished, got %+v", trPartial)
	}
	decPartial := EvaluateAGYGuardState(AGYEvaluationInput{
		Policy:              policy,
		BoundConversationID: convID,
		LogReadable:         true,
		Log:                 stalledLog,
		DB: AGYDBSummary{
			ConversationID: convID,
			Status:         "CASCADE_RUN_STATUS_RUNNING",
			StepCount:      3,
			NotFullyIdle:   true,
		},
		DBReadable:     true,
		Transcript:     trPartial,
		ChildAlive:     true,
		ProxyReachable: true,
		ProxyStableFor: 25 * time.Second,
		Now:            time.Now(),
	})
	if decPartial.ShouldTrigger || decPartial.State != AGYGuardStateWaitingUserAction {
		t.Fatalf("expected partial multi-tool completion to block recovery, got %+v", decPartial)
	}

	// Case 2: PLANNER_RESPONSE returns 2 tool calls (read_file + ask_question), and only 1 GENERIC DONE has arrived
	partialAskPath := filepath.Join(tmpDir, "partial_ask.jsonl")
	partialAskContent := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"id":"t1","name":"read_file"},{"id":"t2","name":"ask_question"}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(partialAskPath, []byte(partialAskContent), 0o600); err != nil {
		t.Fatal(err)
	}
	trAsk := ParseAGYTranscriptFile(partialAskPath)
	if !trAsk.PendingAskQuestion || !trAsk.PendingToolCall {
		t.Fatalf("expected PendingAskQuestion=true and PendingToolCall=true when ask_question is still unclosed, got %+v", trAsk)
	}

	// Case 3: File containing all invalid JSON or truncated JSON line must return Readable=false and ParseError=true
	invalidFiles := map[string]string{
		"all_invalid":   "not json at all\n{broken json\n",
		"mixed_invalid": "{\"step_index\":0,\"type\":\"USER_INPUT\",\"status\":\"DONE\"}\n{\"step_index\":1,\"type\":\"PLANNER_RESPONSE\"",
		"empty_file":    "\n  \n",
		"no_user_input": `{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE"}` + "\n",
		"step_gap":      "{\"step_index\":0,\"type\":\"USER_INPUT\",\"status\":\"DONE\"}\n{\"step_index\":2,\"type\":\"PLANNER_RESPONSE\",\"status\":\"DONE\"}\n",
	}
	for name, content := range invalidFiles {
		p := filepath.Join(tmpDir, name+".jsonl")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		trBad := ParseAGYTranscriptFile(p)
		if trBad.Readable {
			t.Fatalf("expected %s transcript to have Readable=false, got %+v", name, trBad)
		}
		decBad := EvaluateAGYGuardState(AGYEvaluationInput{
			Policy:              policy,
			BoundConversationID: convID,
			LogReadable:         true,
			Log:                 stalledLog,
			DB: AGYDBSummary{
				ConversationID: convID,
				Status:         "CASCADE_RUN_STATUS_RUNNING",
				StepCount:      2,
				NotFullyIdle:   true,
			},
			DBReadable:     true,
			Transcript:     trBad,
			ChildAlive:     true,
			ProxyReachable: true,
			ProxyStableFor: 25 * time.Second,
			Now:            time.Now(),
		})
		if decBad.ShouldTrigger || decBad.BindingConfirmed || decBad.State != AGYGuardStateWaitingUserAction {
			t.Fatalf("expected %s transcript to block auto-recovery with BindingConfirmed=false, got %+v", name, decBad)
		}
	}
}

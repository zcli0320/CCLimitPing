package assistant

// This opt-in integration test sends a small real request through each of the
// user's two accounts. Both app-servers use one temporary history root and
// external access tokens; no refresh tokens or real session files are copied.
import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

type liveHandoffServer struct {
	cmd           *exec.Cmd
	in            io.WriteCloser
	socket        string
	socketRoot    string
	rpc           *warmupRPC
	completed     map[string]string
	started       map[string]bool
	notifications map[string]int
	errorCode     int
	errorFlags    []string
}

func startLiveHandoffServer(t *testing.T, ctx context.Context, exe, root string, credential quotaCredential) *liveHandoffServer {
	t.Helper()
	socketRoot, err := os.MkdirTemp("/tmp", "handoff-live-")
	if err != nil {
		t.Fatal("test socket directory failed")
	}
	socket := filepath.Join(socketRoot, "control.sock")
	cmd := exec.CommandContext(ctx, exe, "app-server", "--listen", "unix://"+socket)
	cmd.Dir = filepath.Join(root, "work")
	cmd.Env = warmupEnvironment(root)
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdout = io.Discard
	if cmd.Start() != nil {
		_ = os.RemoveAll(socketRoot)
		t.Fatal("test server launch failed")
	}
	var in *unixJSONSocket
	for i := 0; i < 100; i++ {
		in, err = connectUnixJSON(ctx, socket)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
		_ = os.RemoveAll(socketRoot)
		t.Fatal("test server socket failed")
	}
	_ = in.SetDeadline(time.Now().Add(100 * time.Second))
	scan := bufio.NewScanner(io.LimitReader(in, 32<<20))
	scan.Buffer(make([]byte, 4096), 4<<20)
	s := &liveHandoffServer{cmd: cmd, in: in, socket: socket, socketRoot: socketRoot, completed: map[string]string{}, started: map[string]bool{}, notifications: map[string]int{}, rpc: &warmupRPC{in: in, lines: scan, result: &WarmupExecution{}}}
	s.rpc.observe = func(m rpcEnvelope) {
		if m.Method != "" {
			s.notifications[m.Method]++
		}
		if len(m.Error) > 0 && string(m.Error) != "null" {
			var e struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(m.Error, &e) == nil {
				s.errorCode = e.Code
				for _, word := range []string{"invalid", "thread", "turn", "active", "found", "interrupt", "cancel", "busy", "unsupported", "method", "initialized", "already"} {
					if strings.Contains(strings.ToLower(e.Message), word) {
						s.errorFlags = append(s.errorFlags, word)
					}
				}
			}
		}
		if m.Method == "turn/completed" || m.Method == "turn/started" {
			var p struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(m.Params, &p) == nil && p.ThreadID != "" && p.Turn.ID != "" {
				if m.Method == "turn/started" {
					s.started[p.ThreadID+"\x00"+p.Turn.ID] = true
				} else {
					s.completed[p.ThreadID+"\x00"+p.Turn.ID] = p.Turn.Status
				}
			}
		}
	}
	if _, err = s.rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_helper_isolated_handoff_test", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		s.stop()
		t.Fatal("initialize failed")
	}
	if s.rpc.send(map[string]any{"method": "initialized"}) != nil {
		s.stop()
		t.Fatal("initialized failed")
	}
	raw, err := s.rpc.call("account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": credential.access, "chatgptAccountId": credential.workspace})
	var auth struct {
		Type string `json:"type"`
	}
	if err != nil || json.Unmarshal(raw, &auth) != nil || auth.Type != "chatgptAuthTokens" {
		s.stop()
		t.Fatal("external token login failed")
	}
	if _, err = os.Stat(filepath.Join(root, "codex", "auth.json")); !os.IsNotExist(err) {
		s.stop()
		t.Fatal("unexpected auth file in temporary home")
	}
	return s
}
func (s *liveHandoffServer) stop() {
	_ = s.in.Close()
	if s.cmd.Process != nil {
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	}
	_ = s.cmd.Wait()
	_ = os.RemoveAll(s.socketRoot)
}
func liveCall(t *testing.T, s *liveHandoffServer, method string, params any) json.RawMessage {
	t.Helper()
	b, err := s.rpc.call(method, params)
	if err != nil {
		t.Fatalf("%s failed", method)
	}
	return b
}
func liveWaitTurn(t *testing.T, s *liveHandoffServer, threadID, turnID string) string {
	t.Helper()
	lastRuntime, lastTurn := "", "missing"
	for i := 0; i < 40; i++ {
		var response struct {
			Thread struct {
				ID     string `json:"id"`
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
				Turns []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turns"`
			} `json:"thread"`
		}
		data := liveCall(t, s, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
		if json.Unmarshal(data, &response) != nil || response.Thread.ID != threadID {
			t.Fatal("thread/read did not return the test thread")
		}
		lastRuntime = response.Thread.Status.Type
		if status := s.completed[threadID+"\x00"+turnID]; status != "" && response.Thread.Status.Type == "idle" {
			return status
		}
		for _, turn := range response.Thread.Turns {
			if turn.ID == turnID {
				lastTurn = turn.Status
			}
			if turn.ID == turnID && turn.Status != "inProgress" && response.Thread.Status.Type == "idle" {
				return turn.Status
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("test turn did not become idle (runtime=%s, turn=%s, completion-event=%t, events=%v)", lastRuntime, lastTurn, s.completed[threadID+"\x00"+turnID] != "", s.notifications)
	return ""
}

func liveWaitStarted(t *testing.T, s *liveHandoffServer, threadID, turnID string) {
	t.Helper()
	for i := 0; i < 50 && !s.started[threadID+"\x00"+turnID]; i++ {
		_ = liveCall(t, s, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
		time.Sleep(100 * time.Millisecond)
	}
	if !s.started[threadID+"\x00"+turnID] {
		t.Fatal("did not observe the exact turn/started event")
	}
}

func TestLiveIsolatedTwoAccountHandoff(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_HANDOFF_TEST") != "1" {
		t.Skip("explicit live integration test not requested")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("cannot locate home")
	}
	creds := r.quotaCredentials(time.Now())
	keys := make([]string, 0, len(creds))
	for k := range creds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 {
		t.Fatal("two distinct usable account credentials are required")
	}
	if credentialIdentity(creds[keys[0]]) == credentialIdentity(creds[keys[1]]) {
		t.Fatal("accounts do not have distinct identities")
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	for _, k := range keys {
		if !resumeQuotaReady(queryAccountQuota(ctx, client, creds[k])) {
			t.Fatal("an account has unavailable quota or credentials")
		}
	}
	root := t.TempDir()
	for _, d := range []string{"codex", "work", "config", "data", "cache"} {
		if os.MkdirAll(filepath.Join(root, d), 0700) != nil {
			t.Fatal("test directory failed")
		}
	}
	config := "cli_auth_credentials_store = \"ephemeral\"\nweb_search = \"disabled\"\n[analytics]\nenabled = false\n[features]\nhooks = false\nshell_tool = false\nexec_tool = false\napply_patch_freeform = false\napps = false\nmulti_agent = false\n"
	if os.WriteFile(filepath.Join(root, "codex/config.toml"), []byte(config), 0600) != nil {
		t.Fatal("test configuration failed")
	}
	exe := codexExecutable(r.Home)
	if exe == "" {
		t.Fatal("Codex executable missing")
	}
	a := startLiveHandoffServer(t, ctx, exe, root, creds[keys[0]])
	aRunning := true
	defer func() {
		if aRunning {
			a.stop()
		}
	}()
	catalog := liveCall(t, a, "model/list", map[string]any{"limit": 100})
	var models struct {
		Data []warmupModel `json:"data"`
	}
	if json.Unmarshal(catalog, &models) != nil {
		a.stop()
		aRunning = false
		t.Fatal("model list failed")
	}
	model, effort := chooseWarmupModel(models.Data)
	if model == "" {
		a.stop()
		aRunning = false
		t.Fatal("no test model")
	}
	var started struct {
		Thread struct {
			ID        string `json:"id"`
			Ephemeral bool   `json:"ephemeral"`
		} `json:"thread"`
	}
	raw := liveCall(t, a, "thread/start", map[string]any{"ephemeral": false, "model": model, "cwd": filepath.Join(root, "work"), "approvalPolicy": "never", "sandbox": "read-only", "baseInstructions": "Do not call tools. Answer only in plain text."})
	if json.Unmarshal(raw, &started) != nil || started.Thread.ID == "" || started.Thread.Ephemeral {
		a.stop()
		aRunning = false
		t.Fatal("persistent test thread not created")
	}
	threadID := started.Thread.ID
	var turn struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	raw = liveCall(t, a, "turn/start", map[string]any{"threadId": threadID, "model": model, "effort": effort, "input": []any{map[string]any{"type": "text", "text": "请详细解释十个基础数学概念。不要使用工具。"}}})
	if json.Unmarshal(raw, &turn) != nil || turn.Turn.ID == "" {
		a.stop()
		aRunning = false
		t.Fatal("first test turn did not start")
	}
	turnID := turn.Turn.ID
	liveWaitStarted(t, a, threadID, turnID)
	observed, readErr := readHandoffCLIThread(ctx, a.socket, threadID, "openai")
	if readErr != nil || observed.Path == nil {
		t.Fatal("the running test thread did not expose its persisted path")
	}
	settings, readErr := readHandoffCLISettings(ctx, a.socket, threadID, turnID, false)
	if readErr != nil || settings.Model != model || settings.Cwd != filepath.Join(root, "work") {
		t.Fatal("the running test thread did not expose its effective execution settings")
	}
	if err := requestHandoffCLISteer(ctx, a.socket, threadID, turnID); err != nil {
		t.Fatal("exact-turn stop request was not accepted")
	}
	interruptErr := requestHandoffCLIInterrupt(ctx, a.socket, threadID, turnID)
	if interruptErr != nil {
		t.Logf("interrupt request failed: %v; server code=%d; error keywords=%v", interruptErr, a.errorCode, a.errorFlags)
	} else {
		t.Log("interrupt request accepted")
	}
	status := liveWaitTurn(t, a, threadID, turnID)
	if interruptErr != nil || status != "interrupted" {
		a.stop()
		aRunning = false
		t.Fatalf("exact interruption not confirmed (status %s)", status)
	}
	a.stop()
	aRunning = false
	t.Log("first account: exact turn interrupted; isolated server exited")
	b := startLiveHandoffServer(t, ctx, exe, root, creds[keys[1]])
	defer b.stop()
	var resumed struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	raw = liveCall(t, b, "thread/resume", map[string]any{"threadId": threadID, "cwd": filepath.Join(root, "work"), "model": model, "approvalPolicy": "never", "sandbox": "read-only"})
	if json.Unmarshal(raw, &resumed) != nil || resumed.Thread.ID != threadID {
		t.Fatal("original isolated thread did not resume")
	}
	if err := applyHandoffCLISettings(ctx, b.socket, threadID, settings); err != nil {
		t.Fatalf("restoring the execution settings failed: %v", err)
	}
	restoredSettings, err := readHandoffCLISettings(ctx, b.socket, threadID, turnID, true)
	if err != nil {
		t.Fatalf("resumed test thread settings could not be read: %v", err)
	}
	beforeSettings, _ := json.Marshal(settings)
	afterSettings, _ := json.Marshal(restoredSettings)
	if string(beforeSettings) != string(afterSettings) {
		var beforeMode, afterMode struct {
			Mode     string `json:"mode"`
			Settings struct {
				ReasoningEffort *string `json:"reasoning_effort"`
			} `json:"settings"`
		}
		_ = json.Unmarshal(settings.Collaboration, &beforeMode)
		_ = json.Unmarshal(restoredSettings.Collaboration, &afterMode)
		t.Logf("test effort before=%s after=%s; collaboration mode before=%s after=%s", settings.Effort, restoredSettings.Effort, beforeMode.Mode, afterMode.Mode)
		t.Logf("test service tier before=%s after=%s", string(settings.ServiceTier), string(restoredSettings.ServiceTier))
		t.Fatalf("resumed settings differ (cwd=%t model=%t effort=%t approval=%t sandbox=%t reviewer=%t profile=%t collaboration=%t tier=%t)", settings.Cwd == restoredSettings.Cwd, settings.Model == restoredSettings.Model, settings.Effort == restoredSettings.Effort, string(settings.Approval) == string(restoredSettings.Approval), string(settings.Sandbox) == string(restoredSettings.Sandbox), string(settings.Reviewer) == string(restoredSettings.Reviewer), string(settings.ActivePermissionProfile) == string(restoredSettings.ActivePermissionProfile), string(settings.Collaboration) == string(restoredSettings.Collaboration), string(settings.ServiceTier) == string(restoredSettings.ServiceTier))
	}
	raw = liveCall(t, b, "turn/start", map[string]any{"threadId": threadID, "model": model, "effort": effort, "input": []any{map[string]any{"type": "text", "text": "继续之前的任务，测试中请只回复 OK。"}}})
	if json.Unmarshal(raw, &turn) != nil || turn.Turn.ID == "" || turn.Turn.ID == turnID {
		t.Fatal("continued turn did not start")
	}
	status = liveWaitTurn(t, b, threadID, turn.Turn.ID)
	if status != "completed" {
		t.Fatalf("continued turn did not complete (%s)", status)
	}
	fmt.Fprintln(io.Discard, threadID) // never log account IDs, tokens or conversation IDs
	t.Log("second account: the same isolated thread resumed and a new turn completed")
}

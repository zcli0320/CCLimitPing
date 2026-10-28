package assistant

// Opt-in end-to-end test. It runs the production CLI handoff supervisor in an
// isolated home and test workspace, using real access tokens with deliberately
// invalid synthetic refresh tokens. It cannot rotate the user's credentials.
import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

type liveTerminalCapture struct {
	sync.Mutex
	data []byte
}

func (c *liveTerminalCapture) Write(p []byte) (int, error) {
	c.Lock()
	defer c.Unlock()
	c.data = append(c.data, p...)
	if len(c.data) > 1<<20 {
		c.data = c.data[len(c.data)-(1<<20):]
	}
	return len(p), nil
}
func (c *liveTerminalCapture) flags() []string {
	c.Lock()
	defer c.Unlock()
	data := string(c.data)
	var out []string
	for _, term := range []string{"已有托管终端", "会话或工作目录不可用", "账号配置不可确认", "未找到 Codex", "账号未匹配", "账号映射不唯一", "缺少备用账号", "凭证不可用", "隔离测试目录", "后端启动失败", "后端连接超时", "后端提前退出", "终端启动失败", "交接状态损坏", "上次交接结果未确认", "会话或阈值无效", "无法保护", "用户目录不可用", "test root", "需要信任", "trust", "error", "permission", "授权", "失败"} {
		if strings.Contains(strings.ToLower(data), strings.ToLower(term)) {
			out = append(out, term)
		}
	}
	return out
}
func (c *liveTerminalCapture) size() int { c.Lock(); defer c.Unlock(); return len(c.data) }
func (c *liveTerminalCapture) syntheticErrors(testHome string) []string {
	c.Lock()
	defer c.Unlock()
	text := regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`).ReplaceAllString(string(c.data), " ")
	text = strings.ReplaceAll(text, testHome, "[test-home]")
	text = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`).ReplaceAllString(text, "[fixture-email]")
	text = regexp.MustCompile(`[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{5,}`).ReplaceAllString(text, "[fixture-token]")
	return regexp.MustCompile(`(?i)(?:error|failed|panic).{0,180}`).FindAllString(text, 4)
}

func startNativeHandoffTestServer(t *testing.T, ctx context.Context, exe, home string) *liveHandoffServer {
	t.Helper()
	socketRoot, err := os.MkdirTemp("/tmp", "handoff-native-")
	if err != nil {
		t.Fatal("test socket failed")
	}
	socket := filepath.Join(socketRoot, "control.sock")
	cmd := exec.CommandContext(ctx, exe, "app-server", "--listen", "unix://"+socket)
	cmd.Dir = filepath.Join(home, "work")
	for _, v := range cliEnvironment(home) {
		if !strings.HasPrefix(v, "HOME=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home)
	cmd.Stderr = io.Discard
	cmd.Stdout = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if cmd.Start() != nil {
		_ = os.RemoveAll(socketRoot)
		t.Fatal("native test backend failed")
	}
	var conn *unixJSONSocket
	for i := 0; i < 100; i++ {
		conn, err = connectUnixJSON(ctx, socket)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
		_ = os.RemoveAll(socketRoot)
		t.Fatal("native test socket failed")
	}
	_ = conn.SetDeadline(time.Now().Add(100 * time.Second))
	scan := bufio.NewScanner(io.LimitReader(conn, 32<<20))
	scan.Buffer(make([]byte, 4096), 4<<20)
	s := &liveHandoffServer{cmd: cmd, in: conn, socket: socket, socketRoot: socketRoot, completed: map[string]string{}, started: map[string]bool{}, notifications: map[string]int{}, rpc: &warmupRPC{in: conn, lines: scan, result: &WarmupExecution{}}}
	s.rpc.observe = func(m rpcEnvelope) {
		if m.Method != "" {
			s.notifications[m.Method]++
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
				key := p.ThreadID + "\x00" + p.Turn.ID
				if m.Method == "turn/started" {
					s.started[key] = true
				} else {
					s.completed[key] = p.Turn.Status
				}
			}
		}
	}
	if _, err = s.rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_helper_tui_test", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		s.stop()
		t.Fatal("native backend initialization failed")
	}
	if s.rpc.send(map[string]any{"method": "initialized"}) != nil {
		s.stop()
		t.Fatal("native backend notification failed")
	}
	return s
}

func TestLiveOwnedCLIHandoff(t *testing.T) {
	if os.Getenv("CODEX_HELPER_LIVE_TUI_HANDOFF_TEST") != "1" {
		t.Skip("explicit live TUI integration test not requested")
	}
	bin := os.Getenv("CODEX_HELPER_LIVE_TEST_BIN")
	if bin == "" {
		t.Fatal("test helper binary path required")
	}
	real, err := NewReader()
	if err != nil {
		t.Fatal("real account source unavailable")
	}
	creds := real.quotaCredentials(time.Now())
	keys := make([]string, 0, len(creds))
	for key := range creds {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) != 2 {
		t.Fatal("two usable access tokens required")
	}
	var managed handoffManagedStore
	if readJSON(filepath.Join(real.Home, ".cc-switch/codex_oauth_auth.json"), &managed) != nil {
		t.Fatal("account metadata unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	for _, key := range keys {
		if creds[key].expires < time.Now().Add(10*time.Minute).Unix() || !resumeQuotaReady(queryAccountQuota(ctx, client, creds[key])) {
			t.Fatal("test account needs valid access and quota")
		}
	}
	// Use synthetic local account ids and invalid refresh tokens in a private
	// test home. Real refresh tokens never enter this test home or subprocess.
	home := t.TempDir()
	writeFixture(t, filepath.Join(home, ".codex-helper-test-root"), "isolated")
	for _, dir := range []string{".codex", ".codex-cli", ".cc-switch", "work"} {
		if os.MkdirAll(filepath.Join(home, dir), 0700) != nil {
			t.Fatal("test home failed")
		}
	}
	fakeAccounts := map[string]any{}
	for i, key := range keys {
		name := []string{"source", "target"}[i]
		a := managed.Accounts[key]
		c := creds[key]
		if a.ID == "" || a.Identity == "" || a.Email == "" {
			t.Fatal("test account metadata incomplete")
		}
		dummy := "invalid-test-refresh-" + name
		fakeAccounts[name] = map[string]any{"account_id": name, "chatgpt_account_id": c.workspace, "id_token": a.Identity, "email": a.Email, "refresh_token": dummy}
		auth := map[string]any{"auth_mode": "chatgpt", "last_refresh": time.Now().UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": c.access, "id_token": a.Identity, "account_id": c.workspace, "refresh_token": dummy}}
		b, _ := json.Marshal(auth)
		dir := ".codex-cli"
		if i == 1 {
			dir = ".codex"
		}
		writeFixture(t, filepath.Join(home, dir, "auth.json"), string(b))
	}
	b, _ := json.Marshal(map[string]any{"version": 2, "accounts": fakeAccounts})
	writeFixture(t, filepath.Join(home, ".cc-switch/codex_oauth_auth.json"), string(b))
	writeFixture(t, filepath.Join(home, ".cc-switch/settings.json"), `{}`)
	fixtureDB(t, filepath.Join(home, ".cc-switch/cc-switch.db"), `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER,sort_index INTEGER);
INSERT INTO providers VALUES('p1','source','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"source"}}',1,1);
INSERT INTO providers VALUES('p2','target','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"target"}}',0,2);`)
	config := "cli_auth_credentials_store = \"file\"\nmodel = \"gpt-6-luna\"\nmodel_reasoning_effort = \"low\"\nweb_search = \"disabled\"\n[analytics]\nenabled = false\n[features]\nhooks = false\nshell_tool = false\nexec_tool = false\napply_patch_freeform = false\napps = false\nmulti_agent = false\n"
	writeFixture(t, filepath.Join(home, ".codex-cli/config.toml"), config)
	exe := codexExecutable(home)
	if exe == "" {
		t.Fatal("Codex executable missing")
	}
	setup := startNativeHandoffTestServer(t, ctx, exe, home)
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	raw := liveCall(t, setup, "thread/start", map[string]any{"ephemeral": false, "model": "gpt-6-luna", "cwd": filepath.Join(home, "work"), "approvalPolicy": "never", "sandbox": "read-only", "baseInstructions": "Do not use tools. Reply concisely."})
	if json.Unmarshal(raw, &started) != nil || started.Thread.ID == "" {
		setup.stop()
		t.Fatal("test session creation failed")
	}
	id := started.Thread.ID
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	raw = liveCall(t, setup, "turn/start", map[string]any{"threadId": id, "effort": "low", "input": []any{map[string]any{"type": "text", "text": "Reply OK."}}})
	if json.Unmarshal(raw, &turn) != nil || turn.Turn.ID == "" {
		setup.stop()
		t.Fatal("test setup turn failed")
	}
	if liveWaitTurn(t, setup, id, turn.Turn.ID) != "completed" {
		setup.stop()
		t.Fatal("test setup did not finish")
	}
	setup.stop()
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	found := false
	for _, s := range r.Snapshot(ctx).Sessions {
		if s.Scope == "cli" && s.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("test session not found by helper")
	}
	qTarget := queryAccountQuota(ctx, client, creds[keys[1]])
	remaining, ok := handoffWindow(&qTarget, 18000)
	if !ok || remaining <= 1 {
		t.Fatal("target test account has no quota")
	}
	threshold := remaining / 2
	if threshold >= 99 {
		threshold = 50
	}
	if threshold <= 0 || threshold >= remaining {
		t.Fatal("test threshold unavailable")
	}
	auth, err := r.PrepareCLIHandoff(ctx, id, threshold)
	if err != nil {
		t.Fatalf("test handoff preparation failed: %v", err)
	}
	cmd := exec.CommandContext(ctx, bin, "handoff", "cli", id, strconv.FormatFloat(threshold, 'g', -1, 64), auth.AuthorizationID)
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal("test home resolution failed")
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TERM=") && !strings.HasPrefix(entry, "CODEX_HELPER_TEST_HOME=") && !strings.HasPrefix(entry, "CODEX_HELPER_TEST_TRIGGER=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color", "CODEX_HELPER_TEST_HOME="+resolvedHome, "CODEX_HELPER_TEST_TRIGGER=1")
	cmd.Dir = filepath.Join(home, "work")
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal("test helper terminal failed")
	}
	done := make(chan struct{})
	output := &liveTerminalCapture{}
	go func() { _, _ = io.Copy(output, terminal); close(done) }()
	testBackendPID := 0
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = terminal.Close()
		_ = cmd.Wait()
		// The test backend may still be draining after the test helper exits.
		// Terminate only the exact fixture process with this temporary HOME;
		// never act on a PID just because it appears in the runner record.
		if testBackendPID > 0 {
			process, e := exec.Command("/bin/ps", "eww", "-p", strconv.Itoa(testBackendPID), "-o", "command=").Output()
			line := string(process)
			if e == nil && strings.Contains(line, "CODEX_HELPER_TEST_HOME="+resolvedHome) && strings.Contains(line, "codex-helper-cli-") && strings.Contains(line, "app-server") {
				_ = syscall.Kill(testBackendPID, syscall.SIGKILL)
			}
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	ready := false
	loggedRunner := false
	for i := 0; i < 100; i++ {
		state := r.HandoffStatus()
		if state.Enabled && !loggedRunner {
			var current cliRunnerState
			err := readJSON(r.cliRunnerPath(), &current)
			info, statErr := os.Lstat(current.Socket)
			t.Logf("initial runner: readable=%t pid-alive=%t socket-not-found=%t socket-is-socket=%t path-base=%s", err == nil, current.PID > 0 && syscall.Kill(current.PID, 0) == nil, os.IsNotExist(statErr), statErr == nil && info.Mode()&os.ModeSocket != 0, filepath.Base(current.Socket))
			loggedRunner = true
		}
		if state.Enabled && r.cliManaged(id) {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("test helper timed out during startup")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !ready {
		state := r.HandoffStatus()
		var diagnostic cliRunnerState
		readErr := readJSON(r.cliRunnerPath(), &diagnostic)
		info, socketErr := os.Lstat(diagnostic.Socket)
		socketOK := socketErr == nil && info.Mode()&os.ModeSocket != 0
		pidOK := diagnostic.PID > 0 && syscall.Kill(diagnostic.PID, 0) == nil
		t.Fatalf("test helper did not enable the target session (stage=%s, enabled=%t, runner-readable=%t, runner-id-matches=%t, runner-alive=%t, socket-is-socket=%t, socket-not-found=%t, output-bytes=%d, terminal diagnostic flags=%v)", state.Stage, state.Enabled, readErr == nil, diagnostic.ID == id, pidOK, socketOK, os.IsNotExist(socketErr), output.size(), output.flags())
	}
	var runner cliRunnerState
	if readJSON(r.cliRunnerPath(), &runner) != nil {
		t.Fatal("test runner metadata missing")
	}
	testBackendPID = runner.ServerPID
	loaded := false
	for i := 0; i < 80; i++ {
		thread, e := readHandoffCLIThread(ctx, runner.Socket, id, "openai")
		if e == nil && thread.Status.Type == "idle" {
			loaded = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !loaded {
		t.Fatal("test TUI did not load the thread")
	}
	// The prompt is sent only to the test terminal's own PTY.
	_, _ = terminal.Write([]byte("\x1b[200~请简要说明二十个数学概念，每个一两句话，不使用工具。\x1b[201~"))
	time.Sleep(200 * time.Millisecond)
	_, _ = terminal.Write([]byte("\r"))
	last := ""
	sawSwitch := false
	for i := 0; i < 70; i++ {
		state := r.HandoffStatus()
		var owned cliRunnerState
		if readJSON(r.cliRunnerPath(), &owned) == nil && owned.PID == cmd.Process.Pid && owned.ServerPID > 0 {
			testBackendPID = owned.ServerPID
		}
		if state.Stage != last {
			t.Logf("test stage: %s", state.Stage)
			last = state.Stage
		}
		if state.Stage == "waiting_switch" || state.Stage == "ready_restart" || state.Stage == "waiting_restart" || state.Stage == "ready_continue" || state.Stage == "waiting_continue" {
			sawSwitch = true
		}
		if i == 8 {
			thread, e := readHandoffCLIThread(ctx, runner.Socket, id, "openai")
			latest, ok := thread.latest()
			settingsOK := false
			settingsErr := ""
			settingsSize := 0
			cwdMatches := false
			if e == nil && ok {
				settings, se := readHandoffCLISettings(ctx, runner.Socket, id, latest.ID, false)
				settingsOK = se == nil
				encoded, _ := json.Marshal(settings)
				settingsSize = len(encoded)
				cwdMatches = thread.Cwd == "" || settings.Cwd == thread.Cwd || sameExistingPath(settings.Cwd, thread.Cwd)
				if se != nil {
					settingsErr = se.Error()
				}
			}
			candidate, candidateErr := r.findHandoffAuthCandidate("target", time.Now())
			backupOK := false
			if candidateErr == nil {
				q := queryAccountQuota(ctx, client, candidate.quotaCredential())
				backupOK = resumeQuotaReady(q) && handoffTargetQuotaOK(q, state.Threshold)
			}
			current, currentErr := r.scopeCredential("cli", time.Now())
			currentOK := false
			if currentErr == nil {
				q := queryAccountQuota(ctx, client, current)
				_, currentOK = handoffWindow(&q, 18000)
			}
			t.Logf("test observation: thread-readable=%t runtime=%s flags=%v turn-present=%t turn-status=%s cwd-matches=%t settings-ok=%t settings-size=%d settings-error=%s backup-ok=%t current-quota-ok=%t state-message=%s terminal-flags=%v", e == nil, thread.Status.Type, thread.Status.ActiveFlags, ok, latest.Status, cwdMatches, settingsOK, settingsSize, settingsErr, backupOK, currentOK, state.Message, output.flags())
		}
		if state.Stage == "completed" || (sawSwitch && state.Stage == "watching" && state.Enabled) {
			live, err := readProtectedHandoffFile(filepath.Join(home, ".codex-cli/auth.json"))
			if err != nil || !r.handoffCurrentMatchesAccount(live, "target") {
				t.Fatal("target account was not installed")
			}
			t.Log("production CLI supervisor switched the isolated native auth and started a new turn")
			return
		}
		if state.Stage == "uncertain" || state.Stage == "error" || state.Stage == "cancelled" {
			t.Fatalf("test handoff stopped at %s: %s", state.Stage, state.Message)
		}
		if i == 20 && state.Stage == "watching" && !sawSwitch {
			t.Fatal("test did not leave the monitoring stage")
		}
		select {
		case <-ctx.Done():
			t.Fatal("test handoff timed out")
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("test handoff did not complete, last stage=%s", last)
}

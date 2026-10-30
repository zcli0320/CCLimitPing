package assistant

// A process-tree fixture for the Desktop identity bridge. The "Desktop" is
// this test executable, not the user's application; its backend is the real
// installed Codex executable, with synthetic authentication and blocked proxy.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	role := os.Getenv("CODEX_HELPER_DESKTOP_PROCESS_FIXTURE")
	if role == "" {
		os.Exit(m.Run())
	}
	root := os.Getenv("CODEX_HELPER_TEST_HOME")
	r, err := NewReader()
	if err != nil || root == "" || !sameExistingPath(root, r.Home) {
		os.Exit(90)
	}
	pathInsideRoot := func(path string) bool {
		rel, e := filepath.Rel(r.Home, path)
		return e == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	switch role {
	case "bridge":
		backend := os.Getenv("CODEX_HELPER_DESKTOP_FIXTURE_BACKEND")
		if !pathInsideRoot(backend) || len(os.Args) < 4 || os.Args[1] != "-c" || os.Args[3] != "app-server" {
			os.Exit(91)
		}
		if runDesktopRuntimeBridge(context.Background(), r, backend, os.Args[1:], os.Stdin, os.Stdout, io.Discard) != nil {
			os.Exit(92)
		}
		os.Exit(0)
	case "desktop":
		bridge := os.Getenv("CODEX_HELPER_DESKTOP_FIXTURE_BRIDGE")
		if !pathInsideRoot(bridge) {
			os.Exit(93)
		}
		os.Exit(runIsolatedDesktopFixture(bridge))
	default:
		os.Exit(98)
	}
}

func runIsolatedDesktopFixture(bridge string) int {
	// Current Desktop places a global configuration override before the
	// app-server command; this exercises that exact argument ordering.
	child := exec.Command(bridge, "-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled", "--listen", "stdio://")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_HELPER_DESKTOP_PROCESS_FIXTURE=") {
			child.Env = append(child.Env, entry)
		}
	}
	child.Env = append(child.Env, "CODEX_HELPER_DESKTOP_PROCESS_FIXTURE=bridge")
	input, err := child.StdinPipe()
	if err != nil {
		return 94
	}
	output, err := child.StdoutPipe()
	if err != nil || child.Start() != nil {
		return 95
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() {
		_ = input.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = child.Process.Kill()
			<-done
		}
	}()
	encoder := json.NewEncoder(input)
	if encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "isolated-desktop-process-fixture", "version": "1"}}}) != nil {
		return 96
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	initialized := false
	for scanner.Scan() {
		var response struct {
			ID    int             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &response) == nil && response.ID == 1 {
			initialized = len(response.Error) == 0
			break
		}
	}
	if !initialized || encoder.Encode(map[string]any{"method": "initialized"}) != nil {
		return 97
	}
	go func() {
		for scanner.Scan() {
		}
	}()
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

type isolatedDesktopProcess struct {
	cmd   *exec.Cmd
	input io.WriteCloser
	done  chan error
	once  sync.Once
}

func (p *isolatedDesktopProcess) stop() error {
	var result error
	p.once.Do(func() {
		_ = p.input.Close()
		select {
		case result = <-p.done:
		case <-time.After(4 * time.Second):
			_ = p.cmd.Process.Kill()
			result = <-p.done
		}
	})
	return result
}

func startIsolatedDesktopProcess(t *testing.T, mainPath, bridgePath, backendPath, home string) *isolatedDesktopProcess {
	t.Helper()
	cmd := exec.Command(mainPath)
	cmd.Dir = home
	allow := map[string]bool{"HOME": true, "PATH": true, "TMPDIR": true, "USER": true, "LOGNAME": true, "LANG": true, "LC_ALL": true, "TZ": true}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allow[key] {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		"CODEX_HELPER_TEST_HOME="+home,
		"CODEX_HELPER_NATIVE_PREVIEW=1",
		"CODEX_HELPER_DESKTOP_PROCESS_FIXTURE=desktop",
		"CODEX_HELPER_DESKTOP_FIXTURE_BRIDGE="+bridgePath,
		"CODEX_HELPER_DESKTOP_FIXTURE_BACKEND="+backendPath,
		"CODEX_HOME="+filepath.Join(home, ".codex"),
		"XDG_CONFIG_HOME="+filepath.Join(home, "xdg-config"),
		"XDG_CACHE_HOME="+filepath.Join(home, "xdg-cache"),
		"XDG_DATA_HOME="+filepath.Join(home, "xdg-data"),
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1",
		"http_proxy=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1", "all_proxy=http://127.0.0.1:1")
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("cannot create isolated Desktop control pipe")
	}
	output, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("cannot start isolated Desktop process")
	}
	p := &isolatedDesktopProcess{cmd: cmd, input: input, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() { _ = p.stop() })
	ready := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(output); ready <- scanner.Scan() && scanner.Text() == "ready" }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("isolated Desktop process failed to initialize")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("isolated Desktop process initialization timed out")
	}
	return p
}

func TestDesktopRealBridgeProcessTreeColdRestart(t *testing.T) {
	if os.Getenv("CODEX_HELPER_DESKTOP_PROCESS_TEST") != "1" {
		t.Skip("explicit isolated Desktop process-tree test required")
	}
	realBackend := os.Getenv("CODEX_HELPER_RUNTIME_PROXY_TEST_BIN")
	if realBackend == "" {
		t.Fatal("explicit local Codex backend required")
	}
	home, err := filepath.EvalSymlinks(shortBridgeDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	writeFixture(t, filepath.Join(home, ".codex-helper-test-root"), "isolated")
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	identityRoot, err := r.runtimeIdentityDirectory(true)
	if err != nil {
		t.Fatal("cannot create isolated identity endpoint directory")
	}
	t.Cleanup(func() { _ = os.Remove(identityRoot) })
	now := time.Now()
	accounts := map[string]any{}
	var loginA, loginB []byte
	for i, letter := range []string{"a", "b"} {
		user, workspace := "user-"+letter, "workspace-"+letter
		token := syntheticHandoffToken(user, workspace, now.Add(time.Hour).Unix())
		accounts[letter] = map[string]any{"account_id": letter, "chatgpt_account_id": workspace, "id_token": token}
		auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": now.UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": "synthetic-" + letter, "account_id": workspace}})
		if i == 0 {
			loginA = auth
		} else {
			loginB = auth
		}
	}
	store, _ := json.Marshal(map[string]any{"version": 2, "accounts": accounts})
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), "cli_auth_credentials_store = \"file\"\n[analytics]\nenabled = false\n[features]\napps = false\n")
	writeFixture(t, filepath.Join(home, ".codex", "auth.json"), string(loginA))
	app := filepath.Join(home, "Fixture Desktop.app")
	mainPath := filepath.Join(app, "Contents/MacOS/ChatGPT")
	bridgePath := filepath.Join(home, "fixture bridge", "codex-runtime-bridge")
	backendPath := filepath.Join(app, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for path, target := range map[string]string{mainPath: self, bridgePath: self, backendPath: realBackend} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	first := startIsolatedDesktopProcess(t, mainPath, bridgePath, backendPath, home)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := findDesktopProcessPairWithBridge(procs, os.Getuid(), app, bridgePath)
	if err != nil || pair.App.PID != first.cmd.Process.Pid || pair.Bridge.PID <= 0 {
		t.Fatalf("isolated app/bridge/backend process tree not confirmed: %v", err)
	}
	usesHome := func(ctx context.Context, server desktopProcessIdentity, expected string) bool {
		out, err := exec.CommandContext(ctx, "/bin/ps", "eww", "-p", fmt.Sprint(server.PID)).Output()
		if err != nil {
			return false
		}
		for _, entry := range strings.Fields(string(out)) {
			if entry == "CODEX_HOME="+filepath.Join(expected, ".codex") {
				return true
			}
		}
		return false
	}
	lifecycle := &desktopHandoffLifecycle{home: home, appPath: app, bridgePath: bridgePath, uid: os.Getuid(), old: pair,
		oldTree: desktopProcessTree(procs, pair.App), read: readDesktopProcesses, usesHome: usesHome}
	runtime := &desktopSingleSessionRuntime{r: r, lifecycle: lifecycle}
	verified := false
	for i := 0; i < 30; i++ {
		if runtime.verifyRuntimeIdentity(ctx) {
			verified = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !verified {
		t.Fatal("isolated real backend identity proof failed")
	}
	writeFixture(t, filepath.Join(home, ".codex", "auth.json"), string(loginB))
	if runtime.verifyRuntimeIdentity(ctx) {
		t.Fatal("different on-disk identity was accepted as the running instance")
	}
	writeFixture(t, filepath.Join(home, ".codex", "auth.json"), string(loginA))
	lifecycle.command = func(_ context.Context, name string, _ ...string) error {
		if name != "/usr/bin/osascript" {
			return errors.New("unexpected isolated quit command")
		}
		return first.stop()
	}
	if err := lifecycle.requestExit(ctx); err != nil || !lifecycle.observeExit(ctx) {
		t.Fatal("isolated old process tree did not exit")
	}
	writeFixture(t, filepath.Join(home, ".codex", "auth.json"), string(loginB))
	var second *isolatedDesktopProcess
	lifecycle.command = func(_ context.Context, name string, _ ...string) error {
		if name != "/usr/bin/open" {
			return errors.New("unexpected isolated restart command")
		}
		second = startIsolatedDesktopProcess(t, mainPath, bridgePath, backendPath, home)
		return nil
	}
	if err := lifecycle.requestRestart(ctx, "00000000-0000-7000-8000-000000000001"); err != nil {
		t.Fatal("isolated cold restart was rejected")
	}
	runtime.launchRequested = true
	if !lifecycle.observeColdStart(ctx) || second == nil || lifecycle.restarted.App.PID != second.cmd.Process.Pid || !runtime.verifyRuntimeIdentity(ctx) {
		t.Fatal("isolated cold restart or new backend identity not confirmed")
	}
	actual := lifecycle.restarted
	lifecycle.restarted.Server.Start = "different-generation"
	if runtime.verifyRuntimeIdentity(ctx) {
		t.Fatal("a replaced backend generation was accepted")
	}
	lifecycle.restarted = actual
	newProcs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal("cannot inspect isolated restarted process tree")
	}
	newTree := desktopProcessTree(newProcs, actual.App)
	if err := second.stop(); err != nil {
		t.Fatal("isolated restarted process did not exit cleanly")
	}
	remaining, err := readDesktopProcesses(ctx)
	if err != nil || !desktopCapturedProcessesExited(newTree, remaining) || !desktopOldProcessesExited(actual, remaining, os.Getuid(), app) {
		t.Fatal("isolated restarted process tree left a descendant running")
	}
	after, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil || !bytes.Equal(after, loginB) {
		t.Fatal("real backend changed the synthetic authentication file")
	}
	t.Log("real backend + bridge + simulated Desktop process tree: identity, exit, cold restart and changed-instance guard passed")
}

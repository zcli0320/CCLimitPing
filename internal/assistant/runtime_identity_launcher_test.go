package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeBridgeLauncherFixture(t *testing.T) {
	if os.Getenv("CODEX_HELPER_RUNTIME_FIXTURE") != "1" {
		return
	}
	r, err := NewReader()
	if err != nil {
		os.Exit(3)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := []string{"app-server", "--listen", "stdio://"}
	if os.Getenv("CODEX_HELPER_RUNTIME_DESKTOP_PREFIX") == "1" {
		args = append([]string{"-c", "features.code_mode_host=true"}, args...)
	}
	err = runDesktopRuntimeBridge(ctx, r, os.Getenv("CODEX_HELPER_RUNTIME_PROXY_TEST_BIN"), args, os.Stdin, os.Stdout, io.Discard)
	if err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestDesktopStdioServerArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{"desktop current ordering", []string{"-c", "features.code_mode_host=true", "app-server", "--analytics-default-enabled"}, true},
		{"server first", []string{"app-server", "--listen", "stdio://"}, true},
		{"config on both sides", []string{"--config=features.x=true", "app-server", "-c", "model=x", "--stdio"}, true},
		{"flag value is not command", []string{"-c", "app-server", "exec", "prompt"}, false},
		{"unrelated command", []string{"-c", "x=y", "exec", "app-server"}, false},
		{"daemon version", []string{"-c", "x=y", "app-server", "daemon", "version"}, false},
		{"server help", []string{"app-server", "--help"}, false},
		{"print configuration", []string{"app-server", "--print-startup-config"}, false},
		{"unix transport", []string{"app-server", "--listen", "unix:///tmp/test.sock"}, false},
		{"ws transport", []string{"app-server", "--listen=ws://127.0.0.1:0"}, false},
		{"incomplete option", []string{"-c"}, false},
		{"unknown option", []string{"--unknown", "app-server"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := desktopStdioServerArgs(test.args); got != test.want {
				t.Fatalf("server argument detection = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRuntimeBridgePassesNonServerCommandsThrough(t *testing.T) {
	dir := t.TempDir()
	backend := filepath.Join(dir, "fixture-backend")
	writeFixture(t, backend, "#!/bin/sh\nprintf 'plain non-JSON output\\n'\n")
	if err := os.Chmod(backend, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--version"}, {"app-server", "daemon", "version"}, {"app-server", "--help"}, {"app-server", "--listen", "unix:///tmp/example.sock"}} {
		var output, stderr bytes.Buffer
		if err := runDesktopRuntimeBridge(context.Background(), Reader{Home: dir}, backend, args, bytes.NewReader(nil), &output, &stderr); err != nil || output.String() != "plain non-JSON output\n" {
			t.Fatalf("non-server command was not transparently passed through: %v", err)
		}
	}
}

func TestRuntimeBridgeLauncherWithRealBackend(t *testing.T) {
	testRuntimeBridgeLauncher(t, false, false)
}
func TestRuntimeBridgeLauncherContinuesWithoutEndpoint(t *testing.T) {
	testRuntimeBridgeLauncher(t, true, false)
}
func TestRuntimeBridgeLauncherWithDesktopConfigPrefix(t *testing.T) {
	testRuntimeBridgeLauncher(t, false, true)
}
func testRuntimeBridgeLauncher(t *testing.T, unavailable, prefixed bool) {
	t.Helper()
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	backend := os.Getenv("CODEX_HELPER_RUNTIME_PROXY_TEST_BIN")
	if backend == "" {
		t.Skip("explicit installed Codex backend required")
	}
	home := shortBridgeDir(t)
	token := syntheticHandoffToken("launcher-user", "launcher-workspace", time.Now().Add(time.Hour).Unix())
	auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": time.Now().UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": "synthetic-launcher-refresh", "account_id": "launcher-workspace"}})
	writeFixture(t, filepath.Join(home, ".codex", "auth.json"), string(auth))
	writeFixture(t, filepath.Join(home, ".codex", "config.toml"), "cli_auth_credentials_store = \"file\"\n[analytics]\nenabled = false\n[features]\napps = false\n")
	r := Reader{Home: home}
	if unavailable {
		root, err := r.runtimeIdentityDirectory(true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0755); err != nil {
			t.Fatal(err)
		}
		defer func() { os.Chmod(root, 0700); os.Remove(root) }()
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRuntimeBridgeLauncherFixture$")
	if packaged := os.Getenv("CODEX_HELPER_RUNTIME_TEST_BRIDGE_BIN"); packaged != "" {
		args := []string{"app-server", "--listen", "stdio://"}
		if prefixed {
			args = append([]string{"-c", "features.code_mode_host=true"}, args...)
		}
		cmd = exec.CommandContext(ctx, packaged, args...)
	}
	cmd.Env = append(desktopLaunchEnvironment(os.Environ()), "HOME="+home, "CODEX_HOME="+filepath.Join(home, ".codex"), "CODEX_HELPER_NATIVE_PREVIEW=1", "CODEX_HELPER_RUNTIME_FIXTURE=1", "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "http_proxy=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1", "all_proxy=http://127.0.0.1:1")
	if prefixed {
		cmd.Env = append(cmd.Env, "CODEX_HELPER_RUNTIME_DESKTOP_PREFIX=1")
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		input.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
		cancel()
	}()
	enc := json.NewEncoder(input)
	_ = enc.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "runtime-launcher-test", "version": "0.4.3"}}})
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 32<<20)
	initialized := false
	for scanner.Scan() {
		var m struct {
			ID    int             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &m) == nil && m.ID == 1 {
			initialized = len(m.Error) == 0
			break
		}
	}
	if !initialized {
		t.Fatal("wrapper did not preserve initialization")
	}
	if enc.Encode(map[string]any{"method": "initialized"}) != nil {
		t.Fatal("initialize notification failed")
	}
	root, err := r.runtimeIdentityDirectory(false)
	if err != nil && !unavailable {
		t.Fatal(err)
	}
	var child desktopProcessIdentity
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PPID == cmd.Process.Pid && desktopProcessRuns(p, backend, "app-server") {
			if child.PID != 0 {
				t.Fatal("multiple backends")
			}
			child = p
		}
	}
	if child.PID == 0 {
		t.Fatal("wrapper backend child not found")
	}
	if !unavailable {
		identity, err := queryRuntimeIdentityEndpoint(ctx, runtimeIdentitySocket(root, cmd.Process.Pid), cmd.Process.Pid, child.PID)
		if err != nil || identity.Fingerprint != credentialIdentity(quotaCredential{access: token}) {
			t.Fatal("packaged protocol did not attest its own backend", err)
		}
	}
	_ = enc.Encode(map[string]any{"id": 2, "method": "getAuthStatus", "params": map[string]bool{"includeToken": false, "refreshToken": false}})
	gotAccount := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), token) {
			t.Fatal("injected token reached app stdout")
		}
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &m) == nil && m.ID == 2 {
			gotAccount = len(m.Result) > 0
			break
		}
	}
	if !gotAccount {
		t.Fatal("normal app traffic failed after identity query")
	}
	after, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil || !bytes.Equal(auth, after) {
		t.Fatal("read-only wrapper changed synthetic auth")
	}
	if unavailable {
		t.Log("normal real-backend traffic survived unavailable private identity IPC; no model request")
	} else {
		t.Log("real backend + wrapper process + private IPC + OS peer PID verified; no model request")
	}
}

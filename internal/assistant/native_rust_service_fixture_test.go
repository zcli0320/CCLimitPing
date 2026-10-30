package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Only the Rust unit-test executable exposes this method. It uses the real
// account transaction with a fake Desktop runtime and isolated account roots;
// its process guard is exercised separately against real processes.
type isolatedDesktopManager struct{ *nativeManagerClient }

func (m isolatedDesktopManager) apply(ctx context.Context, id, scope string) error {
	var result map[string]any
	return m.call(ctx, "handoff_fixture_apply_isolated", map[string]string{"account_id": id, "scope": scope}, &result)
}

func TestNativeDesktopCoordinatorWithRealRustManagerAndSimulatedRuntime(t *testing.T) {
	if os.Getenv("CODEX_HELPER_NATIVE_DESKTOP_MANAGER_TEST") != "1" || os.Getenv("CODEX_HELPER_RUST_SERVICE_TEST_BIN") == "" {
		t.Skip("explicit isolated Desktop manager integration required")
	}
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r, coordinator, runtime := desktopCoordinatorFixture(t)
	writeFixture(t, filepath.Join(r.Home, ".codex-helper-test-root"), "isolated")
	writeFixture(t, filepath.Join(r.Home, ".cc-switch", "settings.json"), `{"codexConfigDir":"~/.codex-cli"}`)
	now := time.Now().UTC()
	accounts := map[string]any{}
	var incoming []byte
	for i, dir := range []string{".codex", ".codex-cli"} {
		letter := []string{"a", "b"}[i]
		id, user, workspace := "managed-"+letter, "user-"+letter, "workspace-"+letter
		token := syntheticHandoffToken(user, workspace, now.Add(time.Hour).Unix())
		refresh := "synthetic-refresh-" + letter
		accounts[id] = map[string]any{"account_id": id, "chatgpt_account_id": workspace, "id_token": token, "refresh_token": refresh, "authenticated_at": now.Unix(), "token_updated_at_ms": now.UnixMilli()}
		auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": now.Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": refresh, "account_id": workspace}})
		writeFixture(t, filepath.Join(r.Home, dir, "auth.json"), string(auth))
		writeFixture(t, filepath.Join(r.Home, dir, "config.toml"), "cli_auth_credentials_store = \"file\"\n")
		if i == 1 {
			incoming = auth
		}
	}
	store, _ := json.Marshal(map[string]any{"version": 2, "accounts": accounts})
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	var quotaRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/usage" {
			t.Error("unexpected local request")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		quotaRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":10000,"reset_at":2000000000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":500000,"reset_at":2000500000},"limit_reached":false}}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manager, closeManager := startRealRustServiceFixture(t, ctx, r, server.URL+"/usage")
	defer closeManager()
	coordinator.manager = isolatedDesktopManager{manager}
	runtime.e.RuntimeIdentityVerified = true // Simulated, not a live Desktop identity.
	for i := 0; i < 11; i++ {
		if _, err := r.AdvanceHandoff(ctx, coordinator); err != nil {
			t.Fatal("isolated Desktop step:", err)
		}
	}
	state := r.HandoffStatus()
	if state.Stage != "completed" || !reflect.DeepEqual(runtime.actions, []string{"stop", "exit", "restart", "continue"}) {
		t.Fatal("simulated Desktop did not complete exactly one handoff")
	}
	for path, before := range map[string][]byte{
		filepath.Join(r.Home, ".codex", "auth.json"):     incoming,
		filepath.Join(r.Home, ".codex-cli", "auth.json"): incoming,
		r.managedAccountsStorePath():                     store,
	} {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatal("isolated Desktop authentication postcondition failed")
		}
	}
	if quotaRequests.Load() == 0 || !r.nativeHandoffInstalled("desktop", "managed-b") {
		t.Fatal("manager quota or target binding was not verified")
	}
	if _, err := os.Lstat(filepath.Join(r.helperStateRoot(), "accounts", "pending.json")); !os.IsNotExist(err) {
		t.Fatal("manager left a pending transaction")
	}
	if _, err := r.AdvanceHandoff(ctx, coordinator); err != nil || len(runtime.actions) != 4 {
		t.Fatal("completed Desktop handoff was repeated")
	}
	if _, rearmed, err := coordinator.finishCompleted(ctx, state); err != nil || !rearmed || coordinator.source != "managed-b" || coordinator.target != "managed-a" {
		t.Fatal("completed Desktop handoff was not safely rearmed")
	}
}

// The Rust test-only fixture runs the unchanged production shared service.
// Only its quota transport is loopback; no fixture is in the installed app.
func startRealRustServiceFixture(t *testing.T, ctx context.Context, r Reader, quotaURL string) (*nativeManagerClient, func()) {
	t.Helper()
	executable := os.Getenv("CODEX_HELPER_RUST_SERVICE_TEST_BIN")
	if executable == "" {
		t.Fatal("missing explicit Rust test executable")
	}
	if _, err := os.Stat(filepath.Join(r.Home, ".codex-helper-test-root")); err != nil {
		t.Fatal("unmarked test root")
	}
	cmd := exec.Command(executable, "--ignored", "--exact", "service::tests::isolated_integration_service_fixture", "--nocapture")
	cmd.Env = append(os.Environ(), "CODEX_HELPER_TEST_HOME="+r.Home, "CODEX_HELPER_ACCOUNT_SERVICE=1", "CODEX_HELPER_TEST_RUST_SERVICE_FIXTURE=run-isolated-manager", "CODEX_HELPER_TEST_QUOTA_ENDPOINT="+quotaURL)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	var manager *nativeManagerClient
	cleanup := func() {
		once.Do(func() {
			if manager != nil {
				manager.Close()
			}
			stdin.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		})
	}
	t.Cleanup(cleanup)
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := r.nativeManagerSocketPath(); err == nil {
			break
		}
		select {
		case <-done:
			done <- nil
			cleanup()
			t.Fatal("Rust fixture exited before socket ready")
		default:
		}
		if time.Now().After(deadline) {
			cleanup()
			t.Fatal("Rust fixture did not publish its private socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager, err = r.startNativeHandoffManager(ctx)
	if err != nil {
		cleanup()
		t.Fatal("real Rust manager lease failed:", err)
	}
	return manager, cleanup
}

// Exercise the real manager's process guard against the already-running
// Desktop without terminating it or writing to its real configuration root.
func TestNativeDesktopManagerRefusesApplyWhileDesktopIsRunning(t *testing.T) {
	if os.Getenv("CODEX_HELPER_NATIVE_DESKTOP_GUARD_TEST") != "1" || os.Getenv("CODEX_HELPER_RUST_SERVICE_TEST_BIN") == "" {
		t.Skip("explicit real-manager desktop guard test required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	procs, err := readDesktopProcesses(ctx)
	if err != nil {
		t.Fatal("cannot inspect Desktop processes:", err)
	}
	if _, err := findDesktopProcessPair(procs, os.Getuid(), desktopAppPath); err != nil {
		home, homeErr := os.UserHomeDir()
		confirmed := false
		if homeErr == nil {
			for _, bundle := range []string{"Codex 小助手.app", "Codex 小助手账号测试版.app"} {
				bridge := filepath.Join(home, "Applications", bundle, "Contents", "MacOS", "codex-runtime-bridge")
				pair, pairErr := findDesktopProcessPairWithBridge(procs, os.Getuid(), desktopAppPath, bridge)
				if pairErr == nil && pair.Bridge.PID > 0 {
					confirmed = true
					break
				}
			}
		}
		if !confirmed {
			t.Skip("no confirmed running Desktop process pair")
		}
	}

	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	home := t.TempDir()
	writeFixture(t, filepath.Join(home, ".codex-helper-test-root"), "isolated")
	writeFixture(t, filepath.Join(home, ".cc-switch", "settings.json"), `{"codexConfigDir":"~/.codex-cli"}`)
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	now := time.Now()
	accounts := map[string]any{}
	var beforeDesktop, beforeCLI []byte
	for i, scope := range []string{".codex", ".codex-cli"} {
		user, account, workspace := fmt.Sprintf("user-%d", i), fmt.Sprintf("managed-%d", i), fmt.Sprintf("workspace-%d", i)
		token := syntheticHandoffToken(user, workspace, now.Add(time.Hour).Unix())
		refresh := "synthetic-refresh-" + account
		accounts[account] = map[string]any{"account_id": account, "chatgpt_account_id": workspace, "id_token": token, "refresh_token": refresh, "authenticated_at": now.Unix(), "token_updated_at_ms": now.UnixMilli()}
		auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": now.UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": refresh, "account_id": workspace}})
		writeFixture(t, filepath.Join(home, scope, "config.toml"), "cli_auth_credentials_store = \"file\"\n")
		writeFixture(t, filepath.Join(home, scope, "auth.json"), string(auth))
		if i == 0 {
			beforeDesktop = auth
		} else {
			beforeCLI = auth
		}
	}
	store, _ := json.Marshal(map[string]any{"version": 2, "accounts": accounts})
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	manager, closeManager := startRealRustServiceFixture(t, ctx, r, server.URL+"/usage")
	defer closeManager()
	if err := manager.ready(ctx, "managed-1", "desktop"); err != nil {
		t.Fatal("isolated target readiness failed:", err)
	}
	if err := manager.apply(ctx, "managed-1", "desktop"); err == nil || !strings.Contains(err.Error(), "请先退出 Desktop") {
		t.Fatal("running Desktop did not block manager apply:", err)
	}
	for path, before := range map[string][]byte{
		filepath.Join(home, ".codex", "auth.json"):     beforeDesktop,
		filepath.Join(home, ".codex-cli", "auth.json"): beforeCLI,
		r.managedAccountsStorePath():                   store,
	} {
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("blocked apply changed isolated authentication state")
		}
	}
	if _, err := os.Lstat(filepath.Join(r.helperStateRoot(), "accounts", "pending.json")); !os.IsNotExist(err) {
		t.Fatal("blocked apply created a pending transaction")
	}
}

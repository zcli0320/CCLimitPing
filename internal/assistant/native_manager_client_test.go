package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nativeManagerTestScript(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manager")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+source), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeManagerSerializesRequestsAndDoesNotExposeProtocol(t *testing.T) {
	path := nativeManagerTestScript(t, `n=0
while IFS= read -r request; do
  n=$((n + 1))
  if [ "$n" = 1 ]; then printf '{"id":1,"result":{"accounts":[]}}\n';
  else printf '{"id":2,"error":"账号不可用"}\n'; fi
done
`)
	c, err := startNativeManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var result struct {
		Accounts []any `json:"accounts"`
	}
	if err := c.call(context.Background(), "list", nil, &result); err != nil || result.Accounts == nil {
		t.Fatalf("list: %v", err)
	}
	if err := c.call(context.Background(), "apply", map[string]string{"account_id": "fake", "scope": "cli"}, nil); err == nil {
		t.Fatal("manager error ignored")
	}
}

func TestNativeManagerTimeoutKillsOwnedProcessAndDoesNotRetry(t *testing.T) {
	path := nativeManagerTestScript(t, "read request\nexec /bin/sleep 20\n")
	c, err := startNativeManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := c.call(ctx, "apply", nil, nil); err == nil {
		t.Fatal("missing timeout")
	}
	if err := c.call(context.Background(), "apply", nil, nil); err == nil {
		t.Fatal("timed-out operation retried")
	}
}

func TestNativeManagerRejectsMismatchedResponse(t *testing.T) {
	path := nativeManagerTestScript(t, "read request\nprintf '{\"id\":99,\"result\":{}}\\n'\n")
	c, err := startNativeManager(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.call(context.Background(), "list", nil, nil); err == nil {
		t.Fatal("unexpected response accepted")
	}
}

func TestNativeManagerQuotaRequiresCompletePositiveEvidence(t *testing.T) {
	for _, test := range []struct {
		name, response string
		ok             bool
	}{
		{"ready", `{"id":1,"result":{"limit_reached":false,"windows":[{"seconds":18000,"remaining":20},{"seconds":604800,"remaining":60}]}}`, true},
		{"unknown_limit", `{"id":1,"result":{"windows":[{"seconds":18000,"remaining":20},{"seconds":604800,"remaining":60}]}}`, false},
		{"exhausted", `{"id":1,"result":{"limit_reached":true,"windows":[{"seconds":18000,"remaining":20},{"seconds":604800,"remaining":60}]}}`, false},
		{"missing_week", `{"id":1,"result":{"limit_reached":false,"windows":[{"seconds":18000,"remaining":20}]}}`, false},
		{"invalid_percent", `{"id":1,"result":{"limit_reached":false,"windows":[{"seconds":18000,"remaining":101},{"seconds":604800,"remaining":60}]}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := nativeManagerTestScript(t, "read request\nprintf '%s\\n' '"+test.response+"'\n")
			c, err := startNativeManager(path)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			q, err := c.quota(context.Background(), "fake")
			if (err == nil) != test.ok {
				t.Fatalf("quota evidence: %+v, %v", q, err)
			}
		})
	}
}

// Opt-in because it uses the separately built Rust executable. All material
// here is synthetic, and no quota or model request is made.
func TestNativeManagerRealIPCWithSyntheticCredentials(t *testing.T) {
	exe := os.Getenv("CODEX_HELPER_NATIVE_MANAGER_TEST_BIN")
	if exe == "" {
		t.Skip("set CODEX_HELPER_NATIVE_MANAGER_TEST_BIN to the built account manager")
	}
	home := t.TempDir()
	writeFixture(t, filepath.Join(home, ".codex-helper-test-root"), "")
	t.Setenv("CODEX_HELPER_TEST_HOME", home)
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r := Reader{Home: home}
	now := time.Now().UTC()
	jwt := func(subject, workspace string) string {
		v, _ := json.Marshal(map[string]any{"sub": subject, "iss": "https://auth.openai.com", "exp": now.Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_user_id": subject, "chatgpt_account_id": workspace}})
		return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(v) + "."
	}
	accounts := map[string]any{}
	for _, item := range []struct{ id, dir string }{{"a", ".codex"}, {"b", ".codex-cli"}} {
		ws, token, refresh := "workspace-"+item.id, jwt("user-"+item.id, "workspace-"+item.id), "fake-"+item.id
		accounts[item.id] = map[string]any{"account_id": item.id, "chatgpt_account_id": ws, "email": item.id + "@example.test", "id_token": token, "refresh_token": refresh, "authenticated_at": now.Unix(), "token_updated_at_ms": now.UnixMilli()}
		auth, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"id_token": token, "access_token": token, "refresh_token": refresh, "account_id": ws}, "last_refresh": now.Format(time.RFC3339Nano)})
		writeFixture(t, filepath.Join(home, item.dir, "auth.json"), string(auth))
		writeFixture(t, filepath.Join(home, item.dir, "config.toml"), "cli_auth_credentials_store = \"file\"\n")
	}
	writeFixture(t, filepath.Join(home, ".cc-switch", "settings.json"), `{}`)
	store, _ := json.Marshal(map[string]any{"version": 2, "accounts": accounts, "default_account_id": "a"})
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	writeFixture(t, filepath.Join(r.helperStateRoot(), "accounts", "bindings.json"), `{"version":1,"desktop":{"account_id":"a","refresh_digest":"fake"},"cli":{"account_id":"b","refresh_digest":"fake"}}`)
	beforeSource, _ := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	beforeTarget, _ := os.ReadFile(filepath.Join(home, ".codex-cli", "auth.json"))
	beforeStore, _ := os.ReadFile(r.managedAccountsStorePath())
	c, err := startNativeManager(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var list struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := c.call(ctx, "list", nil, &list); err != nil || len(list.Accounts) != 2 {
		t.Fatalf("list: %v", err)
	}
	other, err := startNativeManager(exe)
	if err != nil {
		t.Fatal(err)
	}
	var otherList struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := other.call(ctx, "list", nil, &otherList); err == nil {
		other.Close()
		t.Fatal("second manager acquired the same account store")
	}
	other.Close()
	var fixtureOnly map[string]any
	if err := c.call(ctx, "handoff_fixture_apply_isolated", map[string]string{"account_id": "a", "scope": "cli"}, &fixtureOnly); err == nil {
		t.Fatal("release manager exposed the isolated test-only transaction")
	}
	if err := c.ready(ctx, "a", "cli"); err != nil {
		t.Fatalf("ready: %v", err)
	}
	// The installed manager must reject an active target even when both the
	// executable and a symlinked CODEX_HOME contain spaces. This is a real,
	// harmless local process in the isolated test root, not the user's Codex.
	bin := filepath.Join(home, "executable path with spaces")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	source, executable := filepath.Join(bin, "fixture.c"), filepath.Join(bin, "codex")
	if err := os.WriteFile(source, []byte("#include <unistd.h>\nint main(void) { sleep(15); return 0; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/usr/bin/cc", "-o", executable, source).Run(); err != nil {
		t.Fatal("compile isolated process:", err)
	}
	aliasDir := filepath.Join(home, "home alias with spaces")
	if err := os.MkdirAll(aliasDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliasDir, "cli")
	if err := os.Symlink(filepath.Join(home, ".codex-cli"), alias); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(executable)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "CODEX_HOME=") {
			process.Env = append(process.Env, value)
		}
	}
	process.Env = append(process.Env, "CODEX_HOME="+alias)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if err := process.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("isolated process stopped before guard check")
	}
	if err := c.apply(ctx, "a", "cli"); err == nil || !strings.Contains(err.Error(), "请先退出使用 ~/.codex-cli 的终端") {
		t.Fatal("live isolated CLI was not rejected by the account manager")
	}
	if after, err := os.ReadFile(filepath.Join(home, ".codex-cli", "auth.json")); err != nil || !bytes.Equal(after, beforeTarget) {
		t.Fatal("blocked apply changed the target login")
	}
	_ = process.Process.Kill()
	_ = process.Wait()
	stopped = true
	if err := c.apply(ctx, "a", "cli"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	afterTarget, _ := os.ReadFile(filepath.Join(home, ".codex-cli", "auth.json"))
	afterSource, _ := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	afterStore, _ := os.ReadFile(r.managedAccountsStorePath())
	if !bytes.Equal(afterTarget, beforeSource) || !bytes.Equal(afterSource, beforeSource) || !bytes.Equal(afterStore, beforeStore) || !r.nativeHandoffInstalled("cli", "a") {
		t.Fatal("synthetic handoff did not preserve source/store or verify target")
	}
	// Keep the manager running as the app would, then acquire the same
	// manager from a separate handoff connection. A second owner must fail.
	c.Close()
	service := exec.Command(exe)
	service.Env = append(os.Environ(), "CODEX_HELPER_ACCOUNT_SERVICE=1")
	service.Stderr = io.Discard
	stdout, err := service.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := service.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	var socketPath string
	defer func() {
		_ = stdin.Close()
		_ = service.Process.Kill()
		_ = service.Wait()
		if socketPath != "" {
			_ = os.Remove(socketPath)
			_ = os.Remove(filepath.Dir(socketPath))
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if path, err := r.nativeManagerSocketPath(); err == nil {
			socketPath = path
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shared manager did not publish its private socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	shared, err := r.startNativeHandoffManager(ctx)
	if err != nil {
		t.Fatalf("shared lease: %v", err)
	}
	if second, err := r.startNativeHandoffManager(ctx); err == nil {
		second.Close()
		shared.Close()
		t.Fatal("second handoff acquired shared manager")
	}
	reader, err := r.connectNativeManager(ctx, false)
	if err != nil {
		shared.Close()
		t.Fatalf("warmup connection: %v", err)
	}
	var managed managedQuotaStore
	if readJSON(r.managedAccountsStorePath(), &managed) != nil {
		t.Fatal("synthetic manager store unavailable")
	}
	warmup, err := reader.warmupCredential(ctx, "a", time.Now(), managed)
	reader.Close()
	if err != nil || warmup.workspace != "workspace-a" || len(warmup.sources) != 1 || warmup.sources[0] != "小助手缓存" {
		shared.Close()
		t.Fatalf("warmup access: %v", err)
	}
	if _, err := stdin.Write([]byte("{\"id\":7,\"method\":\"remove\",\"account_id\":\"a\"}\n")); err != nil {
		t.Fatal(err)
	}
	if file, ok := stdout.(*os.File); ok {
		_ = file.SetReadDeadline(time.Now().Add(3 * time.Second))
	}
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var guiReply nativeManagerReply
	if json.Unmarshal(line, &guiReply) != nil || guiReply.ID != 7 || guiReply.Error == "" {
		t.Fatal("GUI mutation was not rejected while handoff owned the manager")
	}
	stillStore, _ := os.ReadFile(r.managedAccountsStorePath())
	if !bytes.Equal(stillStore, beforeStore) {
		t.Fatal("blocked GUI mutation changed account store")
	}
	if err := shared.ready(ctx, "a", "cli"); err != nil {
		shared.Close()
		t.Fatalf("shared ready: %v", err)
	}
	shared.Close()
	for {
		second, err := r.startNativeHandoffManager(ctx)
		if err == nil {
			second.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shared lease did not release on disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

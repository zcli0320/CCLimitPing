package assistant

// An opt-in test using the real local Codex executable and terminal against a
// local fake Responses API. All credentials and histories in this test are
// synthetic; it never sends model requests or credentials to OpenAI.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type diagnosticNativeOps struct {
	supervisor *cliHandoffSupervisor
	lastError  error
}

func (d *diagnosticNativeOps) Observe(ctx context.Context, s HandoffState) (HandoffObservation, error) {
	return d.supervisor.Observe(ctx, s)
}
func (d *diagnosticNativeOps) Execute(ctx context.Context, a HandoffAction, s HandoffState) error {
	d.lastError = d.supervisor.Execute(ctx, a, s)
	return d.lastError
}

func syntheticHandoffToken(user, workspace string, expiry int64) string {
	data, _ := json.Marshal(map[string]any{"sub": user, "iss": "https://auth.openai.com", "exp": expiry, "email": user + "@example.test", "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": user, "chatgpt_account_id": workspace, "chatgpt_plan_type": "plus"}})
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(data) + ".synthetic-signature"
}

func TestNativeCLISupervisorWithLocalModel(t *testing.T) { runNativeCLIWithLocalModel(t, false) }
func TestNativeCLISupervisorWithRealRustManagerAndLocalModel(t *testing.T) {
	if os.Getenv("CODEX_HELPER_RUST_SERVICE_TEST_BIN") == "" {
		t.Skip("explicit Rust test service executable required")
	}
	runNativeCLIWithLocalModel(t, true)
}
func runNativeCLIWithLocalModel(t *testing.T, realManager bool) {
	t.Helper()
	if os.Getenv("CODEX_HELPER_NATIVE_HANDOFF_TEST") != "1" {
		t.Skip("native local integration test is opt-in")
	}
	nativeManager := realManager || os.Getenv("CODEX_HELPER_NATIVE_MANAGER_HANDOFF_TEST") == "1"
	if nativeManager {
		t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	}
	var requests, refreshRequests, quotaRequests atomic.Int32
	var routesMu sync.Mutex
	routes := map[string]int{}
	defer func() { routesMu.Lock(); defer routesMu.Unlock(); t.Logf("local test request routes: %v", routes) }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routesMu.Lock()
		routes[r.Method+" "+r.URL.Path]++
		routesMu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/responses"):
			n := requests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			f, _ := w.(http.Flusher)
			fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"fixture\"}}\n\n")
			if f != nil {
				f.Flush()
			}
			if n == 2 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(8 * time.Second):
				}
			}
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"fixture-msg\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\",\"usage\":{\"input_tokens\":0,\"input_tokens_details\":null,\"output_tokens\":0,\"output_tokens_details\":null,\"total_tokens\":0}}}\n\n")
			if f != nil {
				f.Flush()
			}
		case strings.HasSuffix(r.URL.Path, "/usage"):
			quotaRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":10000,"reset_at":2000000000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":500000,"reset_at":2000500000},"limit_reached":false}}`)
		case strings.HasSuffix(r.URL.Path, "/accounts/check"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []any{map[string]any{"id": r.Header.Get("ChatGPT-Account-Id"), "workspace_backend_origin": "https://chatgpt.com", "account_routing_override": "NO_CONSTRAINT"}}})
		case strings.Contains(r.URL.Path, "oauth"):
			refreshRequests.Add(1)
			w.WriteHeader(500)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	for _, dir := range []string{".codex", ".codex-cli", ".cc-switch", "work"} {
		if os.MkdirAll(filepath.Join(home, dir), 0700) != nil {
			t.Fatal("test directory failed")
		}
	}
	// A running CC Switch must have an explicit target in the isolated home.
	// Keep it on the separate Desktop root so the CLI handoff can prove that
	// the production guard allows a different target without bypassing it.
	writeFixture(t, filepath.Join(home, ".cc-switch/settings.json"), `{"codexConfigDir":"~/.codex"}`)
	writeFixture(t, filepath.Join(home, ".codex-helper-test-root"), "isolated")
	t.Setenv("CODEX_HELPER_TEST_HOME", home)
	// Any unexpected non-local connection is routed to a closed loopback port.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	now := time.Now()
	managed := map[string]any{}
	for i, user := range []string{"user-a", "user-b"} {
		id := []string{"managed-a", "managed-b"}[i]
		workspace := []string{"workspace-a", "workspace-b"}[i]
		tok := syntheticHandoffToken(user, workspace, now.Add(time.Hour).Unix())
		refresh := "synthetic-refresh-" + id
		managed[id] = map[string]any{"account_id": id, "chatgpt_account_id": workspace, "id_token": tok, "email": user + "@example.test", "refresh_token": refresh, "authenticated_at": now.Unix(), "token_updated_at_ms": now.UnixMilli()}
		auth := map[string]any{"auth_mode": "chatgpt", "last_refresh": now.UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": tok, "id_token": tok, "refresh_token": refresh, "account_id": workspace}}
		b, _ := json.Marshal(auth)
		dir := ".codex-cli"
		if i == 1 {
			dir = ".codex"
		}
		writeFixture(t, filepath.Join(home, dir, "auth.json"), string(b))
	}
	b, _ := json.Marshal(map[string]any{"version": 2, "accounts": managed})
	writeFixture(t, filepath.Join(home, ".cc-switch/codex_oauth_auth.json"), string(b))
	config := fmt.Sprintf("model_provider = \"routing\"\nmodel = \"gpt-6-luna\"\nmodel_reasoning_effort = \"low\"\ncli_auth_credentials_store = \"file\"\nchatgpt_base_url = %q\nweb_search = \"disabled\"\n[analytics]\nenabled = false\n[features]\nhooks = false\nshell_tool = false\nexec_tool = false\napply_patch_freeform = false\napps = false\nmulti_agent = false\n[model_providers.routing]\nname = \"Local test\"\nbase_url = %q\nrequires_openai_auth = true\nrequest_max_retries = 0\nstream_max_retries = 0\n", server.URL+"/backend-api", server.URL+"/v1")
	provider := "routing"
	if realManager {
		// Keep the saved provider on the official auth path. The test wrapper
		// supplies the explicit loopback transport to each synthetic Codex process.
		config = strings.Replace(config, fmt.Sprintf("base_url = %q\n", server.URL+"/v1"), "", 1)
	}
	writeFixture(t, filepath.Join(home, ".codex-cli/config.toml"), config)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	exe := codexExecutable(home)
	if exe == "" {
		t.Fatal("native executable missing")
	}
	if realManager {
		quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
		wrapper := filepath.Join(home, "local-codex")
		override := fmt.Sprintf("model_providers.routing.base_url=%q", server.URL+"/v1")
		script := "#!/bin/sh\nexec " + quote(exe) + " -c " + quote(override) + " \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		exe = wrapper
	}
	setup := startNativeHandoffTestServer(t, ctx, exe, home)
	defer setup.stop()
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	raw := liveCall(t, setup, "thread/start", map[string]any{"ephemeral": false, "model": "gpt-6-luna", "cwd": filepath.Join(home, "work"), "approvalPolicy": "never", "sandbox": "read-only"})
	if json.Unmarshal(raw, &started) != nil || started.Thread.ID == "" {
		setup.stop()
		t.Fatal("test session creation failed")
	}
	id := started.Thread.ID
	var initial struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	raw = liveCall(t, setup, "turn/start", map[string]any{"threadId": id, "effort": "low", "input": []any{map[string]any{"type": "text", "text": "Reply OK."}}})
	if json.Unmarshal(raw, &initial) != nil || initial.Turn.ID == "" {
		setup.stop()
		t.Fatal("setup turn failed")
	}
	if status := liveWaitTurn(t, setup, id, initial.Turn.ID); status != "completed" {
		setup.stop()
		t.Fatalf("setup turn not completed (status=%s, local-model-requests=%d)", status, requests.Load())
	}
	setup.stop()
	r := Reader{Home: home, SQLite: "/usr/bin/sqlite3"}
	var accountStoreBefore []byte
	if nativeManager {
		writeFixture(t, r.managedAccountsStorePath(), string(b))
		accountStoreBefore = append([]byte(nil), b...)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "codex-helper-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	fakeQuota := &http.Client{Transport: quotaRoundTrip(func(req *http.Request) (*http.Response, error) {
		claims, ok := decodeTokenClaims(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if !ok {
			return nil, fmt.Errorf("invalid synthetic token")
		}
		used := 91
		if claims.Auth.Workspace == "workspace-b" {
			used = 20
		}
		body := fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800},"limit_reached":false}}`, used)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	env := []string{}
	for _, e := range cliEnvironment(home) {
		if !strings.HasPrefix(e, "TERM=") {
			env = append(env, e)
		}
	}
	env = append(env, "TERM=xterm-256color")
	output := &liveTerminalCapture{}
	s := &cliHandoffSupervisor{r: r, ctx: ctx, id: id, cwd: filepath.Join(home, "work"), exe: exe, socket: filepath.Join(socketRoot, "control.sock"), env: env, provider: provider, input: &cliHandoffInput{}, sourceID: "managed-a", targetID: "managed-b", client: fakeQuota, output: output}
	if realManager {
		manager, closeManager := startRealRustServiceFixture(t, ctx, r, server.URL+"/usage")
		defer closeManager()
		s.manager = manager
		if err := manager.ready(ctx, "managed-b", "cli"); err != nil {
			t.Fatal("real Rust target readiness failed:", err)
		}
		if _, err := manager.quota(ctx, "managed-b"); err != nil {
			t.Fatal("real Rust local quota failed:", err)
		}
	} else if nativeManager {
		s.manager = &fakeNativeHandoffManager{r: r}
	}
	if err = s.start(); err != nil {
		t.Fatalf("native supervisor start: %v", err)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 8*time.Second)
		defer done()
		_ = s.stop(stop)
		_ = os.Remove(r.cliRunnerPath())
	}()
	loaded := false
	var lastErr error
	var lastThread cliHandoffThread
	for i := 0; i < 80; i++ {
		thread, e := readHandoffCLIThread(ctx, s.socket, id, provider)
		lastErr = e
		lastThread = thread
		if e == nil && thread.Status.Type == "idle" {
			loaded = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !loaded {
		t.Fatalf("native TUI did not resume test thread (server-exited=%t tui-exited=%t read-error=%v runtime=%s output-bytes=%d flags=%v errors=%v)", s.server.exited(), s.tui.exited(), lastErr, lastThread.Status.Type, output.size(), output.flags(), output.syntheticErrors(home))
	}
	if _, err = r.ConfigureHandoff("cli", id, 10, true); err != nil {
		t.Fatal(err)
	}
	var turnID string
	err = withHandoffCLIRPC(ctx, s.socket, func(rpc *warmupRPC) error {
		raw, e := rpc.call("turn/start", map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": "Do a short test."}}})
		if e != nil {
			return e
		}
		var v struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Turn.ID == "" {
			return fmt.Errorf("missing test turn")
		}
		turnID = v.Turn.ID
		return nil
	})
	if err != nil {
		t.Fatalf("native test turn start failed: %v", err)
	}
	active := false
	for i := 0; i < 50; i++ {
		thread, e := readHandoffCLIThread(ctx, s.socket, id, provider)
		turn, ok := thread.latest()
		if e == nil && ok && turn.ID == turnID && turn.Status == "inProgress" && turn.StartedAt != nil {
			active = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !active {
		t.Fatal("native test turn did not start")
	}
	last := ""
	ops := &diagnosticNativeOps{supervisor: s}
	for i := 0; i < 50; i++ {
		state, e := r.AdvanceHandoff(ctx, ops)
		if e != nil {
			t.Fatalf("native step error: %v", e)
		}
		if state.Stage != last {
			t.Logf("native stage: %s", state.Stage)
			last = state.Stage
		}
		if state.Stage == "completed" {
			live, e := readProtectedHandoffFile(filepath.Join(home, ".codex-cli/auth.json"))
			if e != nil || !r.handoffCurrentMatchesAccount(live, "managed-b") {
				t.Fatal("synthetic target auth not installed")
			}
			if nativeManager {
				e = s.verifyAccountID(ctx, "managed-b")
			} else {
				e = r.finishHandoffAuth(ctx, s.verifyAccountID)
			}
			if e != nil {
				t.Fatalf("native verification cleanup failed: %v", e)
			}
			if realManager {
				if refreshRequests.Load() != 0 || quotaRequests.Load() == 0 {
					t.Fatal("real manager did not use local quota without refresh")
				}
				store, err := os.ReadFile(r.managedAccountsStorePath())
				if err != nil || !bytes.Equal(store, accountStoreBefore) {
					t.Fatal("handoff changed the managed account store")
				}
				if !r.nativeHandoffInstalled("cli", "managed-b") {
					t.Fatal("real manager binding not published")
				}
				if _, err := os.Lstat(filepath.Join(r.helperStateRoot(), "accounts", "pending.json")); !os.IsNotExist(err) {
					t.Fatal("completed real manager left a pending write")
				}
				t.Logf("real Rust manager + real Codex handoff completed; local quota requests=%d; no refresh request", quotaRequests.Load())
			}
			t.Logf("native process handoff completed; fake model requests: %d", requests.Load())
			return
		}
		if state.Stage == "uncertain" || state.Stage == "error" || state.Stage == "cancelled" {
			thread, _ := readHandoffCLIThread(ctx, s.socket, id, provider)
			t.Logf("settings journal matches=%t, thread cwd matches=%t, status=%s, actual matches=%t", s.settingsMatch(state), thread.Cwd == state.Directory || sameExistingPath(thread.Cwd, state.Directory), thread.Status.Type, s.actualSettingsMatch(ctx, thread, state))
			current, settingsErr := readHandoffCLISettings(ctx, s.socket, id, state.TurnID, true)
			t.Logf("settings read error=%v", settingsErr)
			if settingsErr == nil {
				beforeRaw, _ := json.Marshal(s.settings)
				afterRaw, _ := json.Marshal(current)
				var before, after map[string]json.RawMessage
				_ = json.Unmarshal(beforeRaw, &before)
				_ = json.Unmarshal(afterRaw, &after)
				for key, value := range before {
					if string(value) != string(after[key]) {
						t.Logf("changed setting: %s (before bytes=%d, after bytes=%d)", key, len(value), len(after[key]))
					}
				}
				for key := range after {
					if _, exists := before[key]; !exists {
						t.Logf("added setting: %s", key)
					}
				}
			}
			t.Fatalf("native handoff stopped at %s: %s (detail: %v)", state.Stage, state.Message, ops.lastError)
		}
		select {
		case <-ctx.Done():
			t.Fatal("native handoff timed out")
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Fatalf("native handoff stalled at %s", last)
}

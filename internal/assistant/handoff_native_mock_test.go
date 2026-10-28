package assistant

// An opt-in test using the real local Codex executable and terminal against a
// local fake Responses API. All credentials and histories in this test are
// synthetic; it never sends model requests or credentials to OpenAI.
import (
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
	data, _ := json.Marshal(map[string]any{"iss": "https://auth.openai.com", "sub": "synthetic-" + user, "exp": expiry, "email": user + "@example.test", "https://api.openai.com/auth": map[string]any{"chatgpt_user_id": user, "chatgpt_account_id": workspace, "chatgpt_plan_type": "plus"}})
	return "e30." + base64.RawURLEncoding.EncodeToString(data) + ".synthetic-signature"
}

func TestNativeCLISupervisorWithLocalModel(t *testing.T) {
	if os.Getenv("CODEX_HELPER_NATIVE_HANDOFF_TEST") != "1" {
		t.Skip("native local integration test is opt-in")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":10000,"reset_at":2000000000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":500000,"reset_at":2000500000},"limit_reached":false}}`)
		case strings.HasSuffix(r.URL.Path, "/accounts/check"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []any{map[string]any{"id": r.Header.Get("ChatGPT-Account-Id"), "workspace_backend_origin": "https://chatgpt.com", "account_routing_override": "NO_CONSTRAINT"}}})
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
		managed[id] = map[string]any{"account_id": id, "chatgpt_account_id": workspace, "id_token": tok, "email": user + "@example.test", "refresh_token": refresh}
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
	writeFixture(t, filepath.Join(home, ".codex-cli/config.toml"), config)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	exe := codexExecutable(home)
	if exe == "" {
		t.Fatal("native executable missing")
	}
	setup := startNativeHandoffTestServer(t, ctx, exe, home)
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
	dual := os.Getenv("CODEX_HELPER_NATIVE_DUAL_TEST") == "1"
	if dual {
		writeFixture(t, filepath.Join(home, ".codex/config.toml"), "model='gpt-6-luna'\n")
		settings, _ := json.Marshal(map[string]any{"codexConfigDir": filepath.Join(home, ".codex-cli"), "currentProviderCodex": "provider-a"})
		writeFixture(t, filepath.Join(home, ".cc-switch/settings.json"), string(settings))
		fixtureDB(t, filepath.Join(home, ".cc-switch/cc-switch.db"), `CREATE TABLE providers(id TEXT,name TEXT,app_type TEXT,meta TEXT,is_current INTEGER,settings_config TEXT); INSERT INTO providers VALUES('provider-a','A','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-a"}}',1,'{"config":""}'); INSERT INTO providers VALUES('provider-b','B','codex','{"authBinding":{"authProvider":"codex_oauth","accountId":"managed-b"}}',0,'{"config":""}');`)
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
	s := &cliHandoffSupervisor{r: r, ctx: ctx, id: id, cwd: filepath.Join(home, "work"), exe: exe, socket: filepath.Join(socketRoot, "control.sock"), env: env, provider: "routing", input: &cliHandoffInput{}, sourceID: "managed-a", targetID: "managed-b", client: fakeQuota, output: output}
	if dual {
		s.native = newNativeDualSwitcher(r)
		s.native.env = &fakeNativeEnvironment{cc: true}
		s.native.client = fakeQuota
		s.native.query = func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, fakeQuota, c) }
		defer s.native.close()
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
		thread, e := readHandoffCLIThread(ctx, s.socket, id, "routing")
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
	if dual {
		_, err = r.configureNativeHandoff("cli", id, 10)
	} else {
		_, err = r.ConfigureHandoff("cli", id, 10, true)
	}
	if err != nil {
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
		thread, e := readHandoffCLIThread(ctx, s.socket, id, "routing")
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
			if dual {
				e = s.native.finish(ctx, state, s.verifyAccountID)
			} else {
				e = r.finishHandoffAuth(ctx, s.verifyAccountID)
			}
			if e != nil {
				t.Fatalf("native verification cleanup failed: %v", e)
			}
			if dual && !r.AccountAllocation().Distinct {
				t.Fatal("native CLI handoff did not preserve different client accounts")
			}
			t.Logf("native process handoff completed; fake model requests: %d", requests.Load())
			return
		}
		if state.Stage == "uncertain" || state.Stage == "error" || state.Stage == "cancelled" {
			thread, _ := readHandoffCLIThread(ctx, s.socket, id, "routing")
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

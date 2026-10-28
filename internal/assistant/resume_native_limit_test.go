package assistant

// This opt-in probe uses only synthetic credentials and a loopback model
// service. It checks the native CLI's persisted events after a quota error.
import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeLimitRolloutLifecycle(t *testing.T) {
	if os.Getenv("CODEX_HELPER_NATIVE_LIMIT_TEST") != "1" {
		t.Skip("native quota lifecycle probe is opt-in")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/responses") {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"Synthetic usage limit reached","resets_at":2000000000}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	home := t.TempDir()
	for _, dir := range []string{".codex-cli", "work"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(home, ".codex-helper-test-root"), "isolated")
	t.Setenv("CODEX_HELPER_TEST_HOME", home)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	now := time.Now()
	token := syntheticHandoffToken("fixture-user", "fixture-workspace", now.Add(time.Hour).Unix())
	auth := map[string]any{"auth_mode": "chatgpt", "last_refresh": now.UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": "synthetic-only", "account_id": "fixture-workspace"}}
	b, _ := json.Marshal(auth)
	writeFixture(t, filepath.Join(home, ".codex-cli/auth.json"), string(b))
	config := fmt.Sprintf("model_provider = \"routing\"\nmodel = \"gpt-6-luna\"\ncli_auth_credentials_store = \"file\"\nchatgpt_base_url = %q\nweb_search = \"disabled\"\n[analytics]\nenabled = false\n[features]\nhooks = false\nshell_tool = false\nexec_tool = false\napply_patch_freeform = false\napps = false\nmulti_agent = false\n[model_providers.routing]\nname = \"Local test\"\nbase_url = %q\nrequires_openai_auth = true\nrequest_max_retries = 0\nstream_max_retries = 0\n", server.URL+"/backend-api", server.URL+"/v1")
	writeFixture(t, filepath.Join(home, ".codex-cli/config.toml"), config)
	exe := codexExecutable(home)
	if exe == "" {
		t.Fatal("native executable missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	s := startNativeHandoffTestServer(t, ctx, exe, home)
	defer s.stop()
	observer := s.rpc.observe
	var temporaryReadError bool
	s.rpc.observe = func(m rpcEnvelope) {
		observer(m)
		var failure struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		temporaryReadError = json.Unmarshal(m.Error, &failure) == nil && failure.Code == -32601 && failure.Message == "list_turns is not supported yet"
	}
	var started struct {
		Thread struct {
			ID   string  `json:"id"`
			Path *string `json:"path"`
		} `json:"thread"`
	}
	raw := liveCall(t, s, "thread/start", map[string]any{"ephemeral": false, "model": "gpt-6-luna", "cwd": filepath.Join(home, "work"), "approvalPolicy": "never", "sandbox": "read-only"})
	if json.Unmarshal(raw, &started) != nil || started.Thread.ID == "" {
		t.Fatal("test thread unavailable")
	}
	var begin struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	raw = liveCall(t, s, "turn/start", map[string]any{"threadId": started.Thread.ID, "input": []any{map[string]any{"type": "text", "text": "Synthetic test"}}})
	if json.Unmarshal(raw, &begin) != nil || begin.Turn.ID == "" {
		t.Fatal("test turn unavailable")
	}
	var read struct {
		Thread struct {
			Path   *string `json:"path"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns []struct {
				ID     string          `json:"id"`
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"turns"`
		} `json:"thread"`
	}
	status := ""
	var err error
	for n := 0; n < 40; n++ {
		raw, err = s.rpc.call("thread/read", map[string]any{"threadId": started.Thread.ID, "includeTurns": true})
		if err != nil {
			// Immediately after turn/start the native backend can momentarily
			// treat the thread as unloaded; its fallback cannot list turns yet.
			// Retry only this exact startup response, never a limit or auth error.
			if temporaryReadError {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			t.Fatalf("synthetic thread read failed: %v (requests=%d)", err, requests.Load())
		}
		if json.Unmarshal(raw, &read) == nil && (read.Thread.Status.Type == "idle" || read.Thread.Status.Type == "systemError") {
			for _, turn := range read.Thread.Turns {
				if turn.ID == begin.Turn.ID && turn.Status != "inProgress" && turn.Status != "" {
					status = turn.Status
				}
			}
			if status != "" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status == "" || read.Thread.Path == nil {
		t.Fatal("test rollout path unavailable")
	}
	var structuredLimit bool
	var errorKind string
	for _, turn := range read.Thread.Turns {
		if turn.ID == begin.Turn.ID {
			structuredLimit = usageLimitError(turn.Error)
			var detail struct {
				Info string `json:"codexErrorInfo"`
			}
			_ = json.Unmarshal(turn.Error, &detail)
			errorKind = detail.Info
		}
	}
	f, err := os.Open(*read.Thread.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []string
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 8<<20)
	for scan.Scan() {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &record) == nil && record.Type == "event_msg" && record.Payload.TurnID == begin.Turn.ID {
			if record.Payload.Type == "task_started" || record.Payload.Type == "task_complete" || record.Payload.Type == "turn_aborted" {
				events = append(events, record.Payload.Type)
			}
		}
	}
	if scan.Err() != nil {
		t.Fatal("synthetic rollout unreadable")
	}
	t.Logf("native limit probe: status=%s structured-limit=%t error-kind=%s local-model-requests=%d turn-events=%v", status, structuredLimit, errorKind, requests.Load(), events)
	if status != "failed" || !structuredLimit {
		t.Fatal("fixture did not reproduce a structured usage limit")
	}
	if len(events) < 2 || events[0] != "task_started" || events[len(events)-1] != "task_complete" {
		t.Fatal("usage-limited turn did not persist matching start and completion")
	}
	if err := rolloutProvesCompleted(filepath.Join(home, ".codex-cli"), *read.Thread.Path, started.Thread.ID, begin.Turn.ID); err != nil {
		t.Fatal("native quota-limited turn did not pass the rollout fence")
	}
	appendFile, err := os.OpenFile(*read.Thread.Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := appendFile.WriteString(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"new-turn"}}` + "\n")
	closeErr := appendFile.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("could not add the synthetic later turn")
	}
	if rolloutProvesCompleted(filepath.Join(home, ".codex-cli"), *read.Thread.Path, started.Thread.ID, begin.Turn.ID) == nil {
		t.Fatal("newer native-rollout turn was accepted")
	}
}

package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type bridgeCapture struct {
	mu    sync.Mutex
	data  bytes.Buffer
	lines chan []byte
}

func (w *bridgeCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data.Write(p)
	if w.lines != nil {
		w.lines <- append([]byte(nil), p...)
	}
	return len(p), nil
}
func (w *bridgeCapture) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.data.String() }

func TestRuntimeBridgeForwardsTrafficAndReadsOnlyItsBackendIdentity(t *testing.T) {
	backend := &bridgeCapture{lines: make(chan []byte, 10)}
	app := &bridgeCapture{}
	b := newRuntimeIdentityBridge(backend, app)
	defer b.close()
	if _, err := b.identity(context.Background()); err == nil {
		t.Fatal("identity accepted before initialized")
	}
	original := []byte(`{"id":7,"method":"account/read","params":{"refreshToken":false}}`)
	if err := b.forwardClient(original); err != nil {
		t.Fatal(err)
	}
	if got := <-backend.lines; string(got) != string(original)+"\n" {
		t.Fatal("client request changed")
	}
	response := []byte(`{"id":7,"result":{"account":null}}`)
	if err := b.forwardBackend(response); err != nil || app.text() != string(response)+"\n" {
		t.Fatal("original response changed")
	}
	if err := b.forwardClient([]byte(`{"method":"initialized"}`)); err != nil {
		t.Fatal(err)
	}
	<-backend.lines
	result := make(chan desktopBackendIdentity, 1)
	failure := make(chan error, 1)
	go func() { value, err := b.identity(context.Background()); result <- value; failure <- err }()
	var req struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params struct {
			Include bool `json:"includeToken"`
			Refresh bool `json:"refreshToken"`
		} `json:"params"`
	}
	select {
	case raw := <-backend.lines:
		if json.Unmarshal(raw, &req) != nil {
			t.Fatal("invalid identity request")
		}
	case <-time.After(time.Second):
		t.Fatal("identity not requested")
	}
	if req.Method != "getAuthStatus" || !req.Params.Include || req.Params.Refresh {
		t.Fatal("identity request may rotate credentials")
	}
	token := syntheticHandoffToken("bridge-user", "bridge-workspace", time.Now().Add(time.Hour).Unix())
	reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"authMethod": "chatgpt", "authToken": token}})
	if err := b.forwardBackend(reply); err != nil {
		t.Fatal(err)
	}
	value := <-result
	if err := <-failure; err != nil {
		t.Fatal(err)
	}
	if value.Fingerprint != credentialIdentity(quotaCredential{access: token}) || value.ValidUntil <= time.Now().Unix() {
		t.Fatal("wrong backend identity")
	}
	public, _ := json.Marshal(value)
	if strings.Contains(string(public), token) || strings.Contains(app.text(), token) || strings.Contains(app.text(), req.ID) {
		t.Fatal("injected credential response escaped")
	}
}

func TestRuntimeBridgeReadyAfterInitializeResponseWithoutNotification(t *testing.T) {
	backend := &bridgeCapture{}
	app := &bridgeCapture{}
	b := newRuntimeIdentityBridge(backend, app)
	defer b.close()
	if err := b.forwardClient([]byte(`{"id":1,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.forwardBackend([]byte(`{"id":2,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.forwardBackend([]byte(`{"id":1,"error":{"message":"no"}}`)); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	early := b.ready
	b.mu.Unlock()
	if early {
		t.Fatal("ready before a successful initialize response")
	}
	if err := b.forwardBackend([]byte(`{"id":1,"result":{"userAgent":"late"}}`)); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	early = b.ready
	b.mu.Unlock()
	if early {
		t.Fatal("accepted a late result after initialize failed")
	}
	if err := b.forwardClient([]byte(`{"id":1,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.forwardBackend([]byte(`{"id":1,"result":{"userAgent":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	ready := b.ready
	b.mu.Unlock()
	if !ready {
		t.Fatal("initialize response did not make the bridge ready")
	}
	if !strings.Contains(app.text(), `"userAgent":"x"`) {
		t.Fatal("initialize response not forwarded to Desktop")
	}
}

func TestRuntimeBridgeCancellationSuppressesLateCredentialResponse(t *testing.T) {
	backend := &bridgeCapture{lines: make(chan []byte, 10)}
	app := &bridgeCapture{}
	b := newRuntimeIdentityBridge(backend, app)
	defer b.close()
	_ = b.forwardClient([]byte(`{"method":"initialized"}`))
	<-backend.lines
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan error, 1)
	go func() { _, err := b.identity(ctx); out <- err }()
	raw := <-backend.lines
	var req struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &req)
	cancel()
	if err := <-out; !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not preserved")
	}
	late, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"authToken": "PRIVATE_LATE_TOKEN"}})
	if err := b.forwardBackend(late); err != nil || app.text() != "" {
		t.Fatal("late credential reply reached Desktop")
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := b.identity(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancelled read accepted")
	}
	if len(backend.lines) != 0 {
		t.Fatal("pre-cancelled read sent a request")
	}
}

func TestRuntimeBridgeRejectsInvalidIdentityAndReportsClosedConnection(t *testing.T) {
	for _, token := range []string{"invalid", syntheticHandoffToken("u", "w", time.Now().Add(-time.Hour).Unix())} {
		backend := &bridgeCapture{lines: make(chan []byte, 10)}
		app := &bridgeCapture{}
		b := newRuntimeIdentityBridge(backend, app)
		_ = b.forwardClient([]byte(`{"method":"initialized"}`))
		<-backend.lines
		out := make(chan error, 1)
		go func() { _, err := b.identity(context.Background()); out <- err }()
		raw := <-backend.lines
		var req struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &req)
		reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"authMethod": "chatgpt", "authToken": token}})
		_ = b.forwardBackend(reply)
		if <-out == nil {
			t.Fatal("invalid identity accepted")
		}
		b.close()
		if _, err := b.identity(context.Background()); err == nil {
			t.Fatal("closed backend accepted")
		}
	}
}

func TestRuntimeBridgeWithRealCodexBackendAndSyntheticAuth(t *testing.T) {
	exe := os.Getenv("CODEX_HELPER_RUNTIME_PROXY_TEST_BIN")
	if exe == "" {
		t.Skip("explicit installed Codex executable required")
	}
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	token := syntheticHandoffToken("runtime-bridge-user", "runtime-bridge-workspace", time.Now().Add(time.Hour).Unix())
	raw, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "last_refresh": time.Now().UTC().Format(time.RFC3339Nano), "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": "synthetic-runtime-refresh", "account_id": "runtime-bridge-workspace"}})
	writeFixture(t, filepath.Join(codexHome, "auth.json"), string(raw))
	writeFixture(t, filepath.Join(codexHome, "config.toml"), `cli_auth_credentials_store = "file"
[analytics]
enabled = false
[features]
apps = false
`)
	before, _ := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "app-server", "--listen", "stdio://")
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "CODEX_HOME="+codexHome, "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "http_proxy=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1", "all_proxy=http://127.0.0.1:1")
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); cancel(); _ = cmd.Wait() }()
	app := &bridgeCapture{lines: make(chan []byte, 100)}
	b := newRuntimeIdentityBridge(in, app)
	defer b.close()
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 32<<20)
		for scanner.Scan() {
			if b.forwardBackend(scanner.Bytes()) != nil {
				break
			}
		}
		b.close()
	}()
	init, _ := json.Marshal(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "codex-helper-bridge-test", "version": "0.4.3"}}})
	if err = b.forwardClient(init); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-app.lines:
		var message struct {
			ID    int             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(reply, &message) != nil || message.ID != 1 || len(message.Error) > 0 {
			t.Fatal("backend initialization failed")
		}
	case <-ctx.Done():
		t.Fatal("backend initialization timed out")
	}
	// Deliberately omit the initialized notification: current Desktop may not
	// send it, while the real backend accepts identity requests after its reply.
	identity, err := b.identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Fingerprint != credentialIdentity(quotaCredential{access: token}) {
		t.Fatal("runtime identity did not match its own synthetic auth")
	}
	after, _ := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("read-only identity changed credentials")
	}
	if strings.Contains(app.text(), token) {
		t.Fatal("injected token response was forwarded to the app")
	}
	t.Log("real backend returned matching runtime identity; synthetic auth unchanged; no model request sent")
}

func TestRuntimeBridgeRejectsTrafficAfterClose(t *testing.T) {
	backend, app := &bridgeCapture{}, &bridgeCapture{}
	b := newRuntimeIdentityBridge(backend, app)
	b.close()
	if b.forwardClient([]byte(`{"method":"initialized"}`)) == nil || backend.text() != "" {
		t.Fatal("closed bridge forwarded a client message")
	}
	if b.forwardBackend([]byte(`{"method":"thread/started"}`)) == nil || app.text() != "" {
		t.Fatal("closed bridge forwarded a backend message")
	}
}

type stalledBridgeWriter struct {
	release, entered chan struct{}
	once             sync.Once
}

func (w *stalledBridgeWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"method":"getAuthStatus"`)) {
		w.once.Do(func() { close(w.entered) })
		<-w.release
	}
	return len(p), nil
}
func TestRuntimeBridgeTimeoutWhileBackendWriteIsStalled(t *testing.T) {
	writer := &stalledBridgeWriter{release: make(chan struct{}), entered: make(chan struct{})}
	b := newRuntimeIdentityBridge(writer, &bridgeCapture{})
	defer b.close()
	defer close(writer.release)
	if err := b.forwardClient([]byte(`{"method":"initialized"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := b.identity(ctx); result <- err }()
	<-writer.entered
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("wrong timeout result")
		}
	case <-time.After(time.Second):
		t.Fatal("identity timeout blocked behind backend write")
	}
}

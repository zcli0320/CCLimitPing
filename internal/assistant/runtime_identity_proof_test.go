package assistant

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopRuntimeProofRejectsDifferentIdentityOrReplacedInstance(t *testing.T) {
	r, _, _, _ := nativeDesktopFixture(t)
	credential, err := r.scopeCredential("desktop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	backend, app := &bridgeCapture{lines: make(chan []byte, 20)}, &bridgeCapture{}
	b := newRuntimeIdentityBridge(backend, app)
	defer b.close()
	_ = b.forwardClient([]byte(`{"method":"initialized"}`))
	<-backend.lines
	root, err := r.runtimeIdentityDirectory(true)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := startRuntimeIdentityEndpoint(root, b, 123)
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	var token atomic.Value
	token.Store(credential.access)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case raw := <-backend.lines:
				var req struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(raw, &req)
				response, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"authMethod": "chatgpt", "authToken": token.Load().(string)}})
				_ = b.forwardBackend(response)
			}
		}
	}()
	const proxy = "/test/codex-runtime-bridge"
	main := desktopProcessIdentity{PID: 122, UID: os.Getuid(), Start: "app", Command: desktopAppPath + "/Contents/MacOS/ChatGPT"}
	bridge := desktopProcessIdentity{PID: os.Getpid(), PPID: 122, UID: os.Getuid(), Start: "bridge", Command: proxy + " app-server"}
	server := desktopProcessIdentity{PID: 123, PPID: os.Getpid(), UID: os.Getuid(), Start: "backend", Command: desktopBundledBackend() + " app-server"}
	pair := desktopProcessPair{App: main, Server: server, Bridge: bridge}
	procs := []desktopProcessIdentity{main, bridge, server}
	runtime := desktopSingleSessionRuntime{r: r, lifecycle: &desktopHandoffLifecycle{uid: os.Getuid(), appPath: desktopAppPath, bridgePath: proxy, old: pair,
		read: func(context.Context) ([]desktopProcessIdentity, error) { return procs, nil }, usesHome: func(context.Context, desktopProcessIdentity, string) bool { return true }}}
	if err := runtime.requireRuntimeIdentity(context.Background()); err != nil {
		t.Fatal("matching private backend proof rejected")
	}
	runtime.lifecycle.old.Server.Start = "other-generation"
	if runtime.requireRuntimeIdentity(context.Background()) == nil {
		t.Fatal("replaced Desktop instance accepted as original")
	}
	runtime.lifecycle.old = pair
	token.Store(syntheticHandoffToken("other-user", "other-workspace", time.Now().Add(time.Hour).Unix()))
	if runtime.requireRuntimeIdentity(context.Background()) == nil {
		t.Fatal("different runtime account accepted")
	}
}

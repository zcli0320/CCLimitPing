package assistant

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortBridgeDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "ch-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func TestRuntimeEndpointBoundsRequestsAndNeverExportsCredentials(t *testing.T) {
	root := shortBridgeDir(t)
	backend, app := &bridgeCapture{lines: make(chan []byte, 16)}, &bridgeCapture{}
	b := newRuntimeIdentityBridge(backend, app)
	defer b.close()
	_ = b.forwardClient([]byte(`{"method":"initialized"}`))
	<-backend.lines
	endpoint, err := startRuntimeIdentityEndpoint(root, b, 123)
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	if info, err := os.Stat(endpoint.path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket is not private")
	}
	token := syntheticHandoffToken("endpoint-user", "endpoint-workspace", time.Now().Add(time.Hour).Unix())
	go func() {
		raw := <-backend.lines
		var req struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &req)
		reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"authMethod": "chatgpt", "authToken": token}})
		_ = b.forwardBackend(reply)
	}()
	proof, err := queryRuntimeIdentityEndpoint(context.Background(), endpoint.path, os.Getpid(), 123)
	if err != nil || proof.Fingerprint != credentialIdentity(quotaCredential{access: token}) {
		t.Fatal("identity query failed:", err)
	}
	public, _ := json.Marshal(proof)
	if strings.Contains(string(public), token) || strings.Contains(app.text(), token) {
		t.Fatal("endpoint leaked a credential")
	}
	if _, err := queryRuntimeIdentityEndpoint(context.Background(), endpoint.path, os.Getpid()+1, 123); err == nil {
		t.Fatal("wrong peer PID accepted")
	}
	if _, err := queryRuntimeIdentityEndpoint(context.Background(), endpoint.path, os.Getpid(), 124); err == nil {
		t.Fatal("wrong backend PID accepted")
	}
	conn, err := net.Dial("unix", endpoint.path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte(strings.Repeat("x", 2048)))
	var result [128]byte
	n, _ := conn.Read(result[:])
	conn.Close()
	if n > 0 && strings.Contains(string(result[:n]), "fingerprint") {
		t.Fatal("oversized request accepted")
	}
	endpoint.Close()
	if _, err := os.Lstat(endpoint.path); !os.IsNotExist(err) {
		t.Fatal("stale socket survived close")
	}
}

func TestRuntimeEndpointRejectsUnprotectedOrSymlinkDirectory(t *testing.T) {
	root := shortBridgeDir(t)
	b := newRuntimeIdentityBridge(&bridgeCapture{}, &bridgeCapture{})
	defer b.close()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if endpoint, err := startRuntimeIdentityEndpoint(root, b, 123); err == nil {
		endpoint.Close()
		t.Fatal("unprotected directory accepted")
	}
	os.Chmod(root, 0700)
	link := filepath.Join(shortBridgeDir(t), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if endpoint, err := startRuntimeIdentityEndpoint(link, b, 123); err == nil {
		endpoint.Close()
		t.Fatal("symlink directory accepted")
	}
}

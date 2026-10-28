package assistant

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func fakeCLIWebSocket(t *testing.T, legacy ...bool) (string, func()) {
	t.Helper()
	root, e := os.MkdirTemp("/tmp", "helper-ws-test-")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "test.sock")
	l, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				reader := bufio.NewReader(c)
				request, e := http.ReadRequest(reader)
				if e != nil {
					return
				}
				hash := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(hash[:])+"\r\n\r\n")
				for {
					var head [2]byte
					if _, e = io.ReadFull(reader, head[:]); e != nil {
						return
					}
					n := int(head[1] & 127)
					if n == 126 {
						var size [2]byte
						if _, e = io.ReadFull(reader, size[:]); e != nil {
							return
						}
						n = int(binary.BigEndian.Uint16(size[:]))
					}
					if head[1]&128 == 0 || n > 1<<20 {
						return
					}
					var mask [4]byte
					if _, e = io.ReadFull(reader, mask[:]); e != nil {
						return
					}
					data := make([]byte, n)
					if _, e = io.ReadFull(reader, data); e != nil {
						return
					}
					for i := range data {
						data[i] ^= mask[i%4]
					}
					var m struct {
						ID     int    `json:"id"`
						Method string `json:"method"`
						Params struct {
							Include bool `json:"includeTurns"`
						} `json:"params"`
					}
					if json.Unmarshal(data, &m) != nil {
						return
					}
					if m.ID == 0 {
						continue
					}
					var result any = map[string]any{}
					switch m.Method {
					case "thread/read":
						result = map[string]any{"thread": map[string]any{"id": "cli-thread", "modelProvider": "openai", "status": map[string]any{"type": "idle"}}}
					case "thread/turns/list":
						result = map[string]any{"data": []any{map[string]any{"id": "failed-turn", "status": "failed", "error": map[string]any{"codexErrorInfo": "usageLimitExceeded"}}}}
					}
					reply := map[string]any{"id": m.ID, "result": result}
					if len(legacy) > 0 && legacy[0] {
						if m.Method == "thread/turns/list" {
							reply = map[string]any{"id": m.ID, "error": map[string]any{"code": -32600, "message": "fixture legacy history"}}
						}
						if m.Method == "thread/read" && m.Params.Include {
							result.(map[string]any)["thread"].(map[string]any)["turns"] = []any{map[string]any{"id": "old", "status": "completed"}, map[string]any{"id": "failed-turn", "status": "failed", "error": map[string]any{"codexErrorInfo": "usageLimitExceeded"}}}
						}
					}
					b, _ := json.Marshal(reply)
					frame := []byte{0x81}
					if len(b) < 126 {
						frame = append(frame, byte(len(b)))
					} else {
						frame = append(frame, 126, byte(len(b)>>8), byte(len(b)))
					}
					frame = append(frame, b...)
					if _, e = c.Write(frame); e != nil {
						return
					}
				}
			}()
		}
	}()
	return path, func() { l.Close(); os.RemoveAll(root) }
}

func TestNativeCLIUnixProtocolWithoutModel(t *testing.T) {
	exe := os.Getenv("CODEX_HELPER_NATIVE_TEST")
	if exe == "" {
		t.Skip("native Codex preflight is opt-in")
	}
	root, e := os.MkdirTemp("/tmp", "helper-native-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(root)
	home := filepath.Join(root, "codex")
	_ = os.MkdirAll(home, 0700)
	work := filepath.Join(root, "work")
	_ = os.MkdirAll(work, 0700)
	_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte("web_search=\"disabled\"\n[analytics]\nenabled=false\n[features]\napps=false\nhooks=false\n"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socket := filepath.Join(root, "control.sock")
	cmd := exec.CommandContext(ctx, exe, "app-server", "--listen", "unix://"+socket)
	cmd.Env = []string{"HOME=" + root, "CODEX_HOME=" + home, "PATH=/usr/bin:/bin:/opt/homebrew/bin", "TMPDIR=/tmp"}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	for n := 0; n < 100; n++ {
		if _, e = os.Stat(socket); e == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	c, e := connectUnixJSON(ctx, socket)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), 4<<20)
	rpc := warmupRPC{in: c, lines: scan, result: &WarmupExecution{}}
	if _, e = rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "helper_native_test", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); e != nil {
		t.Fatal(e)
	}
	_ = rpc.send(map[string]any{"method": "initialized"})
	raw, e := rpc.call("thread/start", map[string]any{"cwd": work, "approvalPolicy": "on-request", "sandbox": "read-only"})
	if e != nil {
		t.Fatal(e)
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if e = json.Unmarshal(raw, &response); e != nil || response.Thread.ID == "" {
		t.Fatal("no isolated thread")
	}
	if _, e = rpc.call("thread/inject_items", map[string]any{"threadId": response.Thread.ID, "items": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Protocol fixture only; no model generation."}}}}}); e != nil {
		t.Fatal(e)
	}
	// A second connection reads the same loaded backend. No turn/start call:
	// this verifies the real protocol without making any model request.
	turn, limited, e := cliThread(ctx, socket, response.Thread.ID, "openai")
	if e != nil || limited || turn.ID != "" {
		t.Fatalf("native read/turns protocol: %v", e)
	}
}

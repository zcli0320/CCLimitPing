package assistant

// This mediator is used by the controlled Desktop launcher. It preserves the
// application's JSON-line traffic and can issue a read against that same
// initialized backend. Only a hash of its user/workspace identity leaves it.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

type desktopBackendIdentity struct {
	Fingerprint string `json:"fingerprint"`
	ValidUntil  int64  `json:"valid_until"`
}

type runtimeIdentityBridge struct {
	backend, app     io.Writer
	backendMu, appMu sync.Mutex
	mu               sync.Mutex
	prefix           string
	serial           uint64
	ready, closed    bool
	initializeID     string
	pending          map[string]chan json.RawMessage
	done             chan struct{}
	identityWriter   chan struct{}
}

func newRuntimeIdentityBridge(backend, app io.Writer) *runtimeIdentityBridge {
	b := &runtimeIdentityBridge{backend: backend, app: app, pending: map[string]chan json.RawMessage{}, done: make(chan struct{}), identityWriter: make(chan struct{}, 1)}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		b.close()
		return b
	}
	b.prefix = "codex-helper-runtime-" + hex.EncodeToString(nonce[:]) + "-"
	return b
}

func bridgeWrite(w io.Writer, mu *sync.Mutex, line []byte) error {
	mu.Lock()
	defer mu.Unlock()
	return bridgeWriteFrame(w, line)
}
func bridgeWriteFrame(w io.Writer, line []byte) error {
	frame := append(append([]byte(nil), line...), '\n')
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return errors.New("后端桥连接已关闭")
		}
		if n <= 0 || n > len(frame) {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func (b *runtimeIdentityBridge) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		b.ready = false
		close(b.done)
	}
}

func (b *runtimeIdentityBridge) forwardClient(line []byte) error {
	if err := b.checkOpen(); err != nil {
		return err
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(line, &message) != nil {
		return errors.New("Desktop 后端协议格式无效")
	}
	var id string
	if json.Unmarshal(message.ID, &id) == nil && strings.HasPrefix(id, b.prefix) {
		return errors.New("Desktop 后端请求标识冲突")
	}
	if message.Method == "initialize" && len(message.ID) > 0 && string(message.ID) != "null" {
		b.mu.Lock()
		b.initializeID = string(message.ID)
		b.mu.Unlock()
	}
	if err := bridgeWrite(b.backend, &b.backendMu, line); err != nil {
		b.close()
		return err
	}
	if message.Method == "initialized" && len(message.ID) == 0 {
		b.mu.Lock()
		if !b.closed {
			b.ready = true
		}
		b.mu.Unlock()
	}
	return nil
}

func (b *runtimeIdentityBridge) forwardBackend(line []byte) error {
	if err := b.checkOpen(); err != nil {
		return err
	}
	var message struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &message) != nil {
		return errors.New("Desktop 后端协议格式无效")
	}
	// Newer Desktop builds may never send the "initialized" notification; the
	// backend already serves requests once it has answered "initialize".
	if len(message.ID) > 0 {
		b.mu.Lock()
		if !b.closed && b.initializeID != "" && b.initializeID == string(message.ID) {
			b.initializeID = ""
			if len(message.Result) > 0 && string(message.Result) != "null" && (len(message.Error) == 0 || string(message.Error) == "null") {
				b.ready = true
			}
		}
		b.mu.Unlock()
	}
	var id string
	if json.Unmarshal(message.ID, &id) == nil && b.prefix != "" && strings.HasPrefix(id, b.prefix) {
		b.mu.Lock()
		reply := b.pending[id]
		delete(b.pending, id)
		b.mu.Unlock()
		if reply != nil {
			reply <- append(json.RawMessage(nil), line...)
		}
		// Also suppress late replies after timeout; they can contain an access token.
		return nil
	}
	if err := bridgeWrite(b.app, &b.appMu, line); err != nil {
		b.close()
		return err
	}
	return nil
}

func (b *runtimeIdentityBridge) identity(ctx context.Context) (desktopBackendIdentity, error) {
	var identity desktopBackendIdentity
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b.mu.Lock()
	if !b.ready || b.closed || len(b.pending) >= 8 {
		b.mu.Unlock()
		return identity, errors.New("Desktop 运行账号连接尚未就绪")
	}
	b.serial++
	id := b.prefix + strconv.FormatUint(b.serial, 10)
	reply := make(chan json.RawMessage, 1)
	b.pending[id] = reply
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, id); b.mu.Unlock() }()
	request, _ := json.Marshal(map[string]any{"id": id, "method": "getAuthStatus", "params": map[string]bool{"includeToken": true, "refreshToken": false}})
	if err := ctx.Err(); err != nil {
		return identity, err
	}
	// A backend that stops draining stdin must not hang an identity query.
	// Keep at most one injected writer alive, even across repeated timeouts.
	select {
	case b.identityWriter <- struct{}{}:
	case <-ctx.Done():
		return identity, ctx.Err()
	case <-b.done:
		return identity, errors.New("Desktop 运行账号连接已关闭")
	}
	written := make(chan error, 1)
	go func() {
		defer func() { <-b.identityWriter }()
		b.backendMu.Lock()
		defer b.backendMu.Unlock()
		if ctx.Err() != nil {
			written <- ctx.Err()
			return
		}
		written <- bridgeWriteFrame(b.backend, request)
	}()
	select {
	case err := <-written:
		if err != nil {
			if ctx.Err() == nil {
				b.close()
			}
			return identity, err
		}
	case <-ctx.Done():
		return identity, ctx.Err()
	case <-b.done:
		return identity, errors.New("Desktop 运行账号连接已关闭")
	}
	select {
	case <-ctx.Done():
		return identity, ctx.Err()
	case <-b.done:
		return identity, errors.New("Desktop 运行账号连接已关闭")
	case raw := <-reply:
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				Method string `json:"authMethod"`
				Token  string `json:"authToken"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &response) != nil || (len(response.Error) > 0 && string(response.Error) != "null") || response.Result.Method != "chatgpt" {
			return identity, errors.New("Desktop 运行账号不可确认")
		}
		claims, ok := decodeTokenClaims(response.Result.Token)
		if !ok || claims.Expires <= time.Now().Add(60*time.Second).Unix() || claims.NotBefore > time.Now().Unix() {
			return identity, errors.New("Desktop 运行凭证不可确认")
		}
		identity.Fingerprint = credentialIdentity(quotaCredential{access: response.Result.Token})
		identity.ValidUntil = claims.Expires
		return identity, nil
	}
}

func (b *runtimeIdentityBridge) checkOpen() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("Desktop 运行账号连接已关闭")
	}
	return nil
}

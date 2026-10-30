package assistant

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The endpoint exports only an identity digest. A fresh challenge and OS peer
// PID bind each read to the exact bridge in the observed Desktop process tree.
type runtimeIdentityRequest struct {
	Challenge  string `json:"challenge"`
	BackendPID int    `json:"backend_pid"`
}
type runtimeIdentityReply struct {
	Challenge   string                 `json:"challenge"`
	BackendPID  int                    `json:"backend_pid"`
	Identity    desktopBackendIdentity `json:"identity"`
	Unavailable bool                   `json:"unavailable,omitempty"`
}
type runtimeIdentityEndpoint struct {
	path        string
	listener    *net.UnixListener
	cancel      context.CancelFunc
	once        sync.Once
	stopped     chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	connections map[*net.UnixConn]bool
}

func privateRuntimeDirectory(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return errors.New("运行核验目录无法创建")
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("运行核验目录权限不正确")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("运行核验目录所有者不正确")
	}
	return nil
}
func (r Reader) runtimeIdentityDirectory(create bool) (string, error) {
	parent := filepath.Join("/tmp", "codex-helper-runtime-"+strconv.Itoa(os.Getuid()))
	if err := privateRuntimeDirectory(parent, create); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(r.helperStateRoot()))
	root := filepath.Join(parent, hex.EncodeToString(digest[:8]))
	if err := privateRuntimeDirectory(root, create); err != nil {
		return "", err
	}
	return root, nil
}
func runtimeIdentitySocket(root string, pid int) string {
	return filepath.Join(root, "b-"+strconv.Itoa(pid)+".sock")
}
func startRuntimeIdentityEndpoint(root string, b *runtimeIdentityBridge, backendPID int) (*runtimeIdentityEndpoint, error) {
	if err := privateRuntimeDirectory(root, false); err != nil {
		return nil, err
	}
	path := runtimeIdentitySocket(root, os.Getpid())
	if backendPID <= 0 || len(path) >= 104 {
		return nil, errors.New("运行核验连接路径无效")
	}
	// Never unlink an existing endpoint: its ownership and result may be unknown.
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return nil, errors.New("运行核验连接已存在")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("运行核验连接无法启动")
	}
	if os.Chmod(path, 0600) != nil {
		listener.Close()
		return nil, errors.New("运行核验连接权限无法设置")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &runtimeIdentityEndpoint{path: path, listener: listener, cancel: cancel, stopped: make(chan struct{}), connections: map[*net.UnixConn]bool{}}
	go func() {
		defer close(e.stopped)
		slots := make(chan struct{}, 8)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			e.mu.Lock()
			e.connections[conn] = true
			e.mu.Unlock()
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				defer func() { <-slots; e.mu.Lock(); delete(e.connections, conn); e.mu.Unlock(); conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
				_, uid, err := runtimeSocketPeer(conn)
				if err != nil || uid != os.Getuid() {
					return
				}
				raw, err := readRuntimeFrame(conn)
				if err != nil {
					return
				}
				var req runtimeIdentityRequest
				if json.Unmarshal(raw, &req) != nil || len(req.Challenge) != 64 || req.BackendPID != backendPID {
					return
				}
				if value, err := hex.DecodeString(req.Challenge); err != nil || len(value) != 32 {
					return
				}
				result := runtimeIdentityReply{Challenge: req.Challenge, BackendPID: backendPID}
				result.Identity, err = b.identity(ctx)
				result.Unavailable = err != nil
				_ = json.NewEncoder(conn).Encode(result)
			}()
		}
	}()
	return e, nil
}
func (e *runtimeIdentityEndpoint) Close() {
	e.once.Do(func() {
		e.cancel()
		e.listener.Close()
		<-e.stopped
		e.mu.Lock()
		for conn := range e.connections {
			conn.Close()
		}
		e.mu.Unlock()
		e.wg.Wait()
	})
}
func readRuntimeFrame(reader io.Reader) ([]byte, error) {
	raw, err := bufio.NewReader(io.LimitReader(reader, 1025)).ReadBytes('\n')
	if err != nil || len(raw) > 1024 || len(raw) == 0 {
		return nil, errors.New("运行核验消息超出限制或不完整")
	}
	return raw, nil
}
func queryRuntimeIdentityEndpoint(ctx context.Context, path string, bridgePID, backendPID int) (desktopBackendIdentity, error) {
	var identity desktopBackendIdentity
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return identity, errors.New("Desktop 未通过运行核验方式启动")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return identity, errors.New("运行核验连接所有者不符")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return identity, errors.New("运行核验连接不可用")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return identity, errors.New("运行核验连接类型不符")
	}
	pid, uid, err := runtimeSocketPeer(unixConn)
	if err != nil || pid != bridgePID || uid != os.Getuid() {
		return identity, errors.New("Desktop 运行核验进程不匹配")
	}
	var challenge [32]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return identity, errors.New("运行核验随机数不可用")
	}
	req := runtimeIdentityRequest{Challenge: hex.EncodeToString(challenge[:]), BackendPID: backendPID}
	if json.NewEncoder(conn).Encode(req) != nil {
		return identity, errors.New("运行核验请求未完成")
	}
	raw, err := readRuntimeFrame(conn)
	if err != nil {
		return identity, err
	}
	var reply runtimeIdentityReply
	if json.Unmarshal(raw, &reply) != nil || reply.Challenge != req.Challenge || reply.BackendPID != backendPID || reply.Unavailable || len(reply.Identity.Fingerprint) != 64 || strings.ContainsAny(reply.Identity.Fingerprint, "\r\n") || reply.Identity.ValidUntil <= time.Now().Add(time.Minute).Unix() {
		return identity, errors.New("Desktop 运行身份不可确认")
	}
	if _, err := hex.DecodeString(reply.Identity.Fingerprint); err != nil {
		return identity, errors.New("Desktop 运行身份格式不符")
	}
	return reply.Identity, nil
}

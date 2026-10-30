package assistant

// A handoff owns one account-manager process for its entire lifetime. Keeping
// the process alive retains its access-token cache and exclusive store lock;
// the executor never needs to receive a refresh token or an auth record.
import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type handoffAccountManager interface {
	ready(context.Context, string, string) error
	quota(context.Context, string) (AccountQuota, error)
	apply(context.Context, string, string) error
}

type nativeManagerReply struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

type nativeManagerClient struct {
	cmd    *exec.Cmd
	conn   net.Conn
	home   string
	in     io.WriteCloser
	lines  chan []byte
	done   chan struct{}
	mu     sync.Mutex
	next   int
	closed bool
}

func nativeManagerExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", errors.New("无法定位账号管理器")
	}
	path := filepath.Join(filepath.Dir(executable), "codex-accounts")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", errors.New("账号管理器不可用")
	}
	return path, nil
}

func startNativeManager(executable string) (*nativeManagerClient, error) {
	cmd := exec.Command(executable)
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("无法连接账号管理器")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, errors.New("无法连接账号管理器")
	}
	if err = cmd.Start(); err != nil {
		_ = in.Close()
		return nil, errors.New("无法启动账号管理器")
	}
	c := &nativeManagerClient{cmd: cmd, in: in, lines: make(chan []byte, 1), done: make(chan struct{})}
	go c.readLines(out)
	return c, nil
}

func (c *nativeManagerClient) readLines(out io.Reader) {
	defer close(c.lines)
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case c.lines <- line:
		case <-c.done:
			return
		}
	}
}

func (r Reader) nativeManagerSocketPath() (string, error) {
	root, err := filepath.EvalSymlinks(filepath.Join(r.helperStateRoot(), "accounts"))
	if err != nil {
		return "", errors.New("账号目录不可确认")
	}
	sum := sha256.Sum256([]byte(root))
	dir := filepath.Join("/tmp", fmt.Sprintf("codex-helper-account-%d-%x", os.Getuid(), sum[:8]))
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("账号连接目录不可用")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", errors.New("账号连接目录归属不安全")
	}
	path := filepath.Join(dir, "manager.sock")
	info, err = os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("账号管理器未就绪")
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", errors.New("账号连接归属不安全")
	}
	return path, nil
}

func (r Reader) connectNativeManager(ctx context.Context, acquireHandoff bool) (*nativeManagerClient, error) {
	path, err := r.nativeManagerSocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("无法连接账号管理器，请重启小助手")
	}
	c := &nativeManagerClient{conn: conn, in: conn, home: r.Home, lines: make(chan []byte, 1), done: make(chan struct{})}
	go c.readLines(conn)
	var list struct {
		Accounts []json.RawMessage `json:"accounts"`
		Pending  bool              `json:"pending"`
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.call(probeCtx, "list", nil, &list); err != nil {
		c.Close()
		return nil, err
	}
	if list.Pending || list.Accounts == nil {
		c.Close()
		return nil, errors.New("账号管理器不可用或有待核查的切换")
	}
	if acquireHandoff {
		var lease struct {
			Acquired bool `json:"acquired"`
		}
		if err := c.call(probeCtx, "handoff_acquire", nil, &lease); err != nil {
			c.Close()
			return nil, err
		}
		if !lease.Acquired {
			c.Close()
			return nil, errors.New("账号管理权不可确认")
		}
	}
	return c, nil
}

func (r Reader) startNativeHandoffManager(ctx context.Context) (*nativeManagerClient, error) {
	return r.connectNativeManager(ctx, true)
}

func (c *nativeManagerClient) warmupCredential(ctx context.Context, account string, now time.Time, store managedQuotaStore) (quotaCredential, error) {
	var result struct {
		Access    string `json:"access"`
		Workspace string `json:"workspace"`
		Expires   int64  `json:"expires"`
	}
	if err := c.call(ctx, "warmup_access", map[string]string{"account_id": account}, &result); err != nil {
		return quotaCredential{}, err
	}
	claims, ok := decodeTokenClaims(result.Access)
	managed, found := store.Accounts[account]
	identity, valid := decodeTokenClaims(managed.IdentityToken)
	if !ok || !found || managed.ID != account || !valid || identity.Auth != claims.Auth || managed.Workspace != result.Workspace || claims.Auth.Workspace != result.Workspace || claims.Expires != result.Expires || claims.Expires <= now.Add(60*time.Second).Unix() || claims.NotBefore > now.Unix() {
		return quotaCredential{}, errors.New("预热账号凭证不可确认")
	}
	// A cached backup has no single running client. Check both histories.
	return quotaCredential{access: result.Access, workspace: result.Workspace, expires: result.Expires, sources: []string{"小助手缓存"}}, nil
}

func (c *nativeManagerClient) stopLocked() {
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
	_ = c.in.Close()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if c.cmd != nil {
		_ = c.cmd.Wait()
	}
}

func (c *nativeManagerClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *nativeManagerClient) call(ctx context.Context, method string, fields map[string]string, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("账号管理器已停止")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if (method == "apply" || method == "handoff_apply" || method == "handoff_fixture_apply_isolated") && fields["scope"] == "desktop" {
		if c.home == "" {
			return errors.New("无法确认 Desktop 目标目录，账号切换已停止")
		}
		if err := (Reader{Home: c.home}).requireNativeWriteOwner("desktop"); err != nil {
			return err
		}
	}
	c.next++
	request := map[string]any{"id": c.next, "method": method}
	for k, v := range fields {
		if k == "id" || k == "method" {
			return errors.New("账号请求参数无效")
		}
		request[k] = v
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > 64<<10 {
		return errors.New("账号请求无效")
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.stopLocked()
		return errors.New("账号管理器连接已关闭")
	}
	select {
	case <-ctx.Done():
		c.stopLocked()
		return errors.New("账号管理器操作超时，结果待核查")
	case line, ok := <-c.lines:
		if !ok {
			c.stopLocked()
			return errors.New("账号管理器已停止，请关闭账号窗口后重试")
		}
		var reply nativeManagerReply
		if json.Unmarshal(line, &reply) != nil || reply.ID != c.next {
			c.stopLocked()
			return errors.New("账号管理器返回无效状态")
		}
		if reply.Error != "" {
			return errors.New(reply.Error)
		}
		if result != nil && (len(reply.Result) == 0 || json.Unmarshal(reply.Result, result) != nil) {
			return errors.New("账号管理器返回无效状态")
		}
		return nil
	}
}

func (c *nativeManagerClient) ready(ctx context.Context, account, scope string) error {
	var result struct {
		Ready bool `json:"ready"`
	}
	if err := c.call(ctx, "handoff_ready", map[string]string{"account_id": account, "scope": scope}, &result); err != nil {
		return err
	}
	if !result.Ready {
		return errors.New("备用账号状态不可确认")
	}
	return nil
}

func (c *nativeManagerClient) quota(ctx context.Context, account string) (AccountQuota, error) {
	var result struct {
		Windows []struct {
			Seconds   int64   `json:"seconds"`
			Remaining float64 `json:"remaining"`
		} `json:"windows"`
		LimitReached *bool `json:"limit_reached"`
	}
	if err := c.call(ctx, "handoff_quota", map[string]string{"account_id": account}, &result); err != nil {
		return AccountQuota{}, err
	}
	q := AccountQuota{Status: "ready", LimitReached: result.LimitReached}
	for _, w := range result.Windows {
		if w.Seconds <= 0 || math.IsNaN(w.Remaining) || math.IsInf(w.Remaining, 0) || w.Remaining < 0 || w.Remaining > 100 {
			return AccountQuota{}, errors.New("备用账号额度数据无效")
		}
		q.Windows = append(q.Windows, QuotaWindow{Seconds: w.Seconds, UsedPercent: 100 - w.Remaining})
	}
	if !resumeQuotaReady(q) {
		return AccountQuota{}, errors.New("备用账号额度不可确认")
	}
	return q, nil
}

func (c *nativeManagerClient) apply(ctx context.Context, account, scope string) error {
	var result struct {
		State string `json:"state"`
	}
	if err := c.call(ctx, "handoff_apply", map[string]string{"account_id": account, "scope": scope}, &result); err != nil {
		return err
	}
	if result.State != "installed" && result.State != "already_bound" {
		return errors.New("账号切换结果待核查")
	}
	return nil
}

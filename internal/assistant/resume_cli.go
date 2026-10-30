package assistant

// CLI continuation is deliberately limited to a TUI we launched under our own
// PTY. Structured thread reads come from that same backend. We never type into
// an arbitrary Terminal window or infer a running process from a history file.
import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type cliRunnerState struct {
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	ServerPID int    `json:"server_pid,omitempty"`
	ClientPID int    `json:"client_pid,omitempty"`
	Ready     bool   `json:"ready,omitempty"`
	Socket    string `json:"socket"`
}

func (r Reader) cliRunnerPath() string {
	return filepath.Join(filepath.Dir(r.resumePath()), "cli-runner.json")
}
func (r Reader) cliManaged(id string) bool {
	var runner cliRunnerState
	if readJSON(r.cliRunnerPath(), &runner) != nil || runner.ID != id || runner.PID <= 0 || syscall.Kill(runner.PID, 0) != nil {
		return false
	}
	return validateOwnedCLISocket(runner.Socket) == nil
}

// A supervisor may have exited while its backend is still shutting down. A
// free lock alone is not evidence that the old backend is gone. PID reuse may
// conservatively require manual inspection; never signal a PID from this file.
func (r Reader) ensureNoLivePreviousCLIRunner() error {
	if _, err := os.Lstat(r.cliRunnerPath()); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.New("托管进程记录不可检查")
	}
	var runner cliRunnerState
	if readJSON(r.cliRunnerPath(), &runner) != nil {
		return errors.New("托管进程记录不可读取，请先检查")
	}
	for _, pid := range []int{runner.PID, runner.ServerPID, runner.ClientPID} {
		if pid <= 0 {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			return errors.New("先前的托管进程仍在运行，请等待退出后重试")
		} else if err != syscall.ESRCH {
			return errors.New("先前的托管进程状态不可确认")
		}
	}
	return nil
}
func cliEnvironment(home string) []string {
	var env []string
	testHome := os.Getenv("CODEX_HELPER_TEST_HOME") != ""
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if !(testHome && k == "HOME") && k != "CODEX_HOME" && k != "CODEX_THREAD_ID" && k != "CODEX_TURN_ID" && k != "OPENAI_API_KEY" && k != "CODEX_API_KEY" && k != "OPENAI_BASE_URL" && !strings.HasPrefix(k, "CODEX_INTERNAL_") {
			env = append(env, e)
		}
	}
	if testHome {
		env = append(env, "HOME="+home)
	}
	return append(env, "CODEX_HOME="+filepath.Join(home, ".codex-cli"))
}
func cliThread(ctx context.Context, socket, id, provider string) (resumeTurn, bool, error) {
	connection, err := connectUnixJSON(ctx, socket)
	if err != nil {
		return resumeTurn{}, false, err
	}
	defer connection.Close()
	scan := bufio.NewScanner(connection)
	scan.Buffer(make([]byte, 4096), 32<<20)
	rpc := warmupRPC{in: connection, lines: scan, result: &WarmupExecution{}}
	if _, err = rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_helper_resume", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return resumeTurn{}, false, err
	}
	if err = rpc.send(map[string]any{"method": "initialized"}); err != nil {
		return resumeTurn{}, false, err
	}
	raw, err := rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return resumeTurn{}, false, err
	}
	var response struct {
		Thread struct {
			ID       string `json:"id"`
			Provider string `json:"modelProvider"`
			Status   struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns []struct {
				ID     string          `json:"id"`
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != id || response.Thread.Provider != provider {
		return resumeTurn{}, false, errors.New("CLI 会话不匹配")
	}
	if response.Thread.Status.Type != "idle" && response.Thread.Status.Type != "systemError" {
		return resumeTurn{}, false, nil
	}
	raw, err = rpc.call("thread/turns/list", map[string]any{"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"})
	if err != nil {
		// Legacy local threads do not support list_turns in this CLI. Reading
		// their turns does not resume the thread or acquire another writer.
		raw, err = rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": true})
		if err != nil || json.Unmarshal(raw, &response) != nil {
			return resumeTurn{}, false, errors.New("CLI 原会话历史不可读取")
		}
		if response.Thread.ID != id || response.Thread.Provider != provider || (response.Thread.Status.Type != "idle" && response.Thread.Status.Type != "systemError") || len(response.Thread.Turns) == 0 {
			return resumeTurn{}, false, nil
		}
		t := response.Thread.Turns[len(response.Thread.Turns)-1]
		return resumeTurn{ID: t.ID, Status: t.Status, Error: t.Error}, t.ID != "" && t.Status == "failed" && usageLimitError(t.Error), nil
	}
	var turns struct {
		Data []struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &turns) != nil || len(turns.Data) != 1 {
		return resumeTurn{}, false, nil
	}
	t := turns.Data[0]
	turn := resumeTurn{ID: t.ID, Status: t.Status, Error: t.Error}
	return turn, t.ID != "" && t.Status == "failed" && usageLimitError(t.Error), nil
}

type cliInputState struct {
	sync.Mutex
	epoch  uint64
	typing bool
}

func (s *cliInputState) input(b []byte) {
	s.epoch++
	s.typing = !strings.ContainsAny(string(b), "\r\n")
}

func (r Reader) RunCLIResume(ctx context.Context, id string) error {
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.resumePath()), "cli-runner.lock"))
	if err != nil {
		return errors.New("已有一个托管终端；请在原终端继续或先退出它")
	}
	defer unlock()
	if err := r.ensureNoLivePreviousCLIRunner(); err != nil {
		return err
	}
	var selected *Session
	snapshot := r.Snapshot(ctx)
	for _, s := range snapshot.Sessions {
		if s.Scope == "cli" && s.ID == id && !s.Archived {
			copy := s
			selected = &copy
			break
		}
	}
	if selected == nil {
		return errors.New("所选 CLI 会话不存在或已归档")
	}
	if _, err = r.scopeCredential("cli", time.Now()); err != nil {
		return err
	}
	_, supported := r.managedResumeProvider("cli")
	if !supported {
		return errors.New("CLI 没有使用可确认的 OpenAI 登录配置")
	}
	exe := codexExecutable(r.Home)
	if exe == "" {
		return errors.New("未找到 Codex CLI")
	}
	root, err := os.MkdirTemp("/tmp", "codex-helper-cli-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "control.sock")
	env := cliEnvironment(r.Home)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := exec.CommandContext(ctx, exe, "app-server", "--listen", "unix://"+socket)
	server.Env = env
	server.Stderr = io.Discard
	server.Stdout = io.Discard
	if server.Start() != nil {
		return errors.New("CLI 托管后端未能启动")
	}
	defer func() { _ = server.Process.Signal(syscall.SIGTERM); _ = server.Wait() }()
	ready := false
	for n := 0; n < 100; n++ {
		if info, e := os.Stat(socket); e == nil && info.Mode()&os.ModeSocket != 0 {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		return errors.New("CLI 托管后端连接超时")
	}
	if err = writeState(r.cliRunnerPath(), cliRunnerState{ID: id, PID: os.Getpid(), Socket: socket}); err != nil {
		return err
	}
	defer os.Remove(r.cliRunnerPath())
	if _, err = r.ConfigureResume(ctx, "cli", id, true); err != nil {
		return err
	}
	defer func() {
		p := r.ResumeStatus()
		if p.Scope == "cli" && p.ID == id {
			_, _ = r.ConfigureResume(context.Background(), "", "", false)
		}
	}()
	fmt.Println("Codex 小助手：已托管所选 CLI 原会话。额度恢复后继续一次，不切号。关闭本终端即停止。")
	cmd := exec.CommandContext(ctx, exe, "resume", "--remote", "unix://"+socket, id)
	cmd.Env = env
	cmd.Dir = selected.Project
	if cmd.Dir == "" {
		cmd.Dir = r.Home
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	terminal, err := pty.Start(cmd)
	if err != nil {
		return errors.New("无法启动托管 CLI 终端")
	}
	defer terminal.Close()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-resize:
				_ = pty.InheritSize(os.Stdin, terminal)
			}
		}
	}()
	resize <- syscall.SIGWINCH
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, e := term.MakeRaw(int(os.Stdin.Fd())); e == nil {
			defer term.Restore(int(os.Stdin.Fd()), old)
		}
	}
	input := &cliInputState{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, e := os.Stdin.Read(b)
			if n > 0 {
				input.Lock()
				input.input(b[:n])
				_, _ = terminal.Write(b[:n])
				input.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(os.Stdout, terminal); close(done) }()
	go r.watchCLIResume(ctx, exe, socket, id, env, terminal, input)
	err = cmd.Wait()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	return err
}
func (r Reader) watchCLIResume(ctx context.Context, exe, socket, id string, env []string, terminal io.Writer, input *cliInputState) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	blocked := ""
	var blockedEpoch uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
		p := r.ResumeStatus()
		if !p.Enabled || p.Scope != "cli" || p.ID != id {
			continue
		}
		provider, supported := r.managedResumeProvider("cli")
		if !supported {
			r.cliMessage(id, "provider_changed", "CLI 认证配置已变化，请重新确认登录配置")
			continue
		}
		turn, limited, e := cliThread(ctx, socket, id, provider)
		if e != nil {
			r.cliMessage(id, "waiting_client", "托管 CLI 读取失败，请手动继续或重新打开托管终端")
			continue
		}
		if !limited {
			blocked = ""
			r.cliMessage(id, "watching", "正在托管原 CLI；等待明确限额错误")
			continue
		}
		input.Lock()
		epoch, typing := input.epoch, input.typing
		input.Unlock()
		if blocked != turn.ID {
			blocked = turn.ID
			blockedEpoch = epoch
		}
		if typing || epoch != blockedEpoch {
			r.cliMessage(id, "user_intervened", "检测到手动输入，本次限额交给你处理")
			continue
		}
		key := privateKey(p.Key + "\x00" + turn.ID)
		if p.Handled[key] {
			continue
		}
		c, e := r.scopeCredential("cli", time.Now())
		if e != nil {
			r.cliMessage(id, "credential_error", "凭证不可用或已过期，请更新该账号登录")
			continue
		}
		if credentialIdentity(c) != p.Identity {
			r.cliMessage(id, "account_changed", "账号已改变，请重新开启续跑")
			continue
		}
		q := queryAccountQuota(ctx, client, c)
		if !resumeQuotaReady(q) {
			msg := "已触限，等待当前账号额度恢复"
			if q.Status != "ready" {
				msg = q.Message
			}
			r.cliMessage(id, "waiting_quota", msg)
			continue
		}
		fresh, limited, e := cliThread(ctx, socket, id, provider)
		if e != nil || !limited || fresh.ID != turn.ID {
			continue
		}
		_, _ = r.submitCLIResume(ctx, id, key, p.Identity, epoch, terminal, input)
	}
}

func (r Reader) submitCLIResume(ctx context.Context, id, key, identity string, epoch uint64, terminal io.Writer, input *cliInputState) (bool, error) {
	c, e := r.scopeCredential("cli", time.Now())
	if e != nil || credentialIdentity(c) != identity {
		return false, errors.New("CLI 凭证已变化，请更新登录后重新开启")
	}
	lock, e := lockWarmup(r.resumePath() + ".lock")
	if e != nil {
		return false, e
	}
	defer lock()
	p := r.ResumeStatus()
	input.Lock()
	defer input.Unlock()
	if !p.Enabled || p.Scope != "cli" || p.ID != id || p.Identity != identity || p.Handled[key] || input.typing || input.epoch != epoch || ctx.Err() != nil {
		return false, nil
	}
	// Registration precedes even a potentially partial write. Bracketed paste
	// keeps multiline/paste heuristics separate from the submission key.
	p.Handled[key] = true
	p.Status = "submitted"
	p.Message = "CLI 继续请求已登记；请查看原终端，不会重复发送"
	if e = writeState(r.resumePath(), p); e != nil {
		return false, e
	}
	if _, e = io.WriteString(terminal, "\x1b[200~继续之前因额度限制中断的任务。\x1b[201~"); e != nil {
		return false, e
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(150 * time.Millisecond):
	}
	_, e = io.WriteString(terminal, "\r")
	return e == nil, e
}
func (r Reader) cliMessage(id, status, message string) {
	lock, e := lockWarmup(r.resumePath() + ".lock")
	if e != nil {
		return
	}
	defer lock()
	p := r.ResumeStatus()
	if p.Enabled && p.Scope == "cli" && p.ID == id {
		p.Status = status
		p.Message = message
		_ = writeState(r.resumePath(), p)
	}
}

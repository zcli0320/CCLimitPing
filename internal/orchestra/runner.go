package orchestra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wavever/CCLimitPing/internal/assistant"
)

// Config is where tasks live and what executors may touch.
type Config struct {
	Dir          string // state root (tasks, logs, runner lock)
	Home         string
	Quota        func(context.Context) (float64, bool)
	Switch       func(context.Context, string, float64, func(string) error, func() bool) assistant.OrchestraSwitchResult
	Agents       map[string]Agent // executors by name
	AllowedRoots []string         // task folders must be inside one of these
	Timeout      time.Duration    // one executor run at most this long
}

// DefaultConfig keeps state under root and allows folders in the home folder.
func DefaultConfig(home, root string) Config {
	return Config{Dir: root, Home: home, Agents: DefaultAgents(home), AllowedRoots: []string{home}, Timeout: 3 * time.Hour}
}

func (c Config) allowedFolder(folder string) (string, error) {
	if folder == "" || !filepath.IsAbs(folder) {
		return "", fmt.Errorf("工作目录必须是绝对路径：%q", folder)
	}
	dir, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return "", fmt.Errorf("工作目录不存在：%s", folder)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("工作目录不是文件夹：%s", folder)
	}
	for _, root := range c.AllowedRoots {
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if dir != r && strings.HasPrefix(dir, r+string(os.PathSeparator)) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("工作目录不在允许范围内：%s", folder)
}

// verifyFolder checks, right before launch, that the task's folder is still
// the directory that was dispatched: same resolved path (no symlink appeared
// on it) and same device+inode (not deleted and recreated). A same-user race
// between this check and the executor's chdir remains possible.
func (c Config) verifyFolder(t *Task) (string, error) {
	dir, err := c.allowedFolder(t.Folder)
	if err != nil {
		return "", err
	}
	if dir != t.Folder {
		return "", fmt.Errorf("工作目录已被替换或重定向：%s 现在指向 %s", t.Folder, dir)
	}
	if t.FolderIno == 0 {
		return "", errors.New("任务缺少派发时的目录身份记录，请重新派发")
	}
	dev, ino, err := folderID(dir)
	if err != nil || dev != t.FolderDev || ino != t.FolderIno {
		return "", fmt.Errorf("工作目录已不是派发时的那个文件夹：%s", t.Folder)
	}
	return dir, nil
}

func folderID(dir string) (dev, ino uint64, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("无法读取目录身份")
	}
	return uint64(st.Dev), uint64(st.Ino), nil
}

// Preamble tells an executor how to behave and how to answer.
const Preamble = `You are running headless as the executor for another coding agent (the lead),
started by Codex Helper. No human is watching this run and you cannot ask questions.
- Do only what the card below asks, inside its folder and its Don't limits. Don't widen the scope.
- Anything that needs the user's approval (credentials, logins, real accounts, payments, deleting
  data, anything irreversible, quitting or restarting other apps) you must NOT do unless the card
  says the user approved it; report it as blocked instead.
- Report what actually happened. Never mark a check passed that you didn't run.
Your final message is delivered to the lead as your receipt, in exactly this format:
RECEIPT <id>: <done|blocked>
Changed: <files>
Verified: <commands and results>
Doubts: <remaining issues>
`

// AutoReworkLimits keeps an automatic rework inside the original card: a
// review-only or read-only card must not turn into code edits because a
// reviewer or check asked for changes.
const AutoReworkLimits = "The original card's Don't limits still apply. If the card is a review, a test run, or says not to edit files, do not edit anything: report what would need to change in your receipt instead."

// Runner starts queued tasks, at most one per executor at a time.
type Runner struct {
	Cfg             Config
	Store           Store
	Poll            time.Duration
	Logf            func(format string, args ...any)
	path            string // the user's login-shell PATH, so executors find Homebrew tools
	quotaStatus     RunnerStatus
	quotaSettings   Settings
	lastSwitchCheck time.Time
}

// LoginPathMaxAge is how long a cached login-shell PATH is reused.
var LoginPathMaxAge = 10 * time.Minute

// LoginPath is the PATH of the user's login shell, cached in stateDir. A GUI
// app starts with a bare PATH; executors launched from it (and the GUI's
// availability check) would otherwise miss Homebrew and ~/.local/bin. The
// runner refreshes it at start; everyone else reuses a fresh cache.
func LoginPath(stateDir string, refresh bool) string {
	cache := filepath.Join(stateDir, "login-path")
	if !refresh {
		if info, err := os.Lstat(cache); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) < LoginPathMaxAge {
			if data, err := os.ReadFile(cache); err == nil && len(data) > 0 {
				return string(data)
			}
		}
	}
	path := readLoginPath()
	if os.MkdirAll(stateDir, 0o700) == nil {
		tmp := cache + fmt.Sprintf(".%d.tmp", os.Getpid())
		if os.WriteFile(tmp, []byte(path), 0o600) == nil {
			os.Rename(tmp, cache)
		}
	}
	return path
}

func readLoginPath() string {
	shell := os.Getenv("SHELL")
	if !filepath.IsAbs(shell) {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-ilc", `printf '\n__PATH__%s__END__\n' "$PATH"`)
	out, _ := cmd.Output()
	text := string(out)
	if i := strings.LastIndex(text, "__PATH__"); i >= 0 {
		if j := strings.Index(text[i:], "__END__"); j > len("__PATH__") {
			return text[i+len("__PATH__") : i+j]
		}
	}
	return os.Getenv("PATH") + ":/opt/homebrew/bin:/usr/local/bin"
}

type active struct {
	id           string
	cmd          *exec.Cmd
	done         chan error
	start        time.Time
	agyRun       AGYRunIdentity // binds an Antigravity transcript turn to this run
	files        runFiles
	agent        Agent
	term         time.Time // when SIGTERM was sent
	why          string    // why it was stopped
	tree         []int     // executor's descendants seen when stopping (they may sit in other groups)
	phase        string
	baseline     gitSnapshot
	folder       string
	env          []string
	checks       []string
	checkIndex   int
	checkTimeout time.Duration
	checkLog     string
	profile      string
	sandboxBin   string
	probeDir     string
	checkStarted time.Time
}

type runFiles struct{ out, err, last string }

// ShutdownGrace is how long a stopping runner lets executors exit after SIGTERM.
var ShutdownGrace = 5 * time.Second

// ErrRunnerBusy means another runner already owns this state root.
var ErrRunnerBusy = errors.New("另一个任务执行器已在运行")

// RunnerActive checks whether a runner currently holds the state-root lock.
func RunnerActive(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// Run executes tasks until ctx ends; running executors are stopped on exit.
func (r *Runner) Run(ctx context.Context) error {
	if r.Poll == 0 {
		r.Poll = time.Second
	}
	if r.Logf == nil {
		r.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(filepath.Join(r.Cfg.Dir, "logs"), 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(r.Cfg.Dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrRunnerBusy
	}
	if status, e := ReadSwitchStatus(r.Cfg.Dir); e == nil && (status.Stage == "pending" || status.Stage == "resuming") {
		status.Stage, status.ReviewRequired = "uncertain", true
		if err := writeJSON(r.Cfg.Dir, "auto-switch.json", status); err != nil {
			return err
		}
	}
	// A restart must not bypass a recent persisted pause or hammer quota reads.
	if status, e := ReadRunnerStatus(r.Cfg.Dir); e == nil && status.Paused && !status.CheckedAt.IsZero() {
		if settings, e := ReadSettings(r.Cfg.Dir); e == nil && ((settings.Account == "desktop" && status.Account == "Desktop") || (settings.Account == "separate" && status.Account == "独立账号")) {
			r.quotaStatus, r.quotaSettings = status, settings
		}
	}
	r.recoverStale()
	if r.path == "" {
		r.path = LoginPath(r.Cfg.Dir, true)
	}
	running := map[string]*active{} // by executor
	for {
		select {
		case <-ctx.Done():
			// Bounded: the helper app SIGKILLs the runner 10 s after SIGTERM, and
			// an executor that outlived its runner would keep spending usage.
			for _, a := range running {
				r.stop(a, "执行器已关闭")
			}
			for _, a := range running {
				select {
				case <-a.done:
				case <-time.After(ShutdownGrace):
					a.kill()
					<-a.done
				}
				a.reap()
				if a.phase == "probe" || a.phase == "check" {
					r.finishChecksInterrupted(a, "执行器已关闭，任务被中断")
				} else {
					r.finish(a, "执行器已关闭，任务被中断")
				}
			}
			return nil
		default:
		}
		r.tick(running)
		select {
		case <-ctx.Done():
		case <-time.After(r.Poll):
		}
	}
}

// recoverStale marks tasks left "running" by a runner that died: they are not rerun.
// recoverStale fails tasks a crashed runner left running. It never replays
// them and never kills anything: after a crash the executor's process tree
// can't be identified reliably (PIDs get reused; children in their own process
// groups are reparented to launchd). So every such task is marked
// unconfirmed and can't be reworked until the user confirms it has stopped.
func (r *Runner) recoverStale() {
	tasks, _ := r.Store.List()
	for _, t := range tasks {
		if t.Status != Running {
			continue
		}
		r.Store.Update(t.ID, func(t *Task) error {
			if n := len(t.Runs); n > 0 && t.Runs[n-1].Ended.IsZero() {
				run := &t.Runs[n-1]
				run.Ended, run.Exit = time.Now(), -1
				run.Error = "执行器在任务运行中重启，任务未重新执行。执行进程可能仍在运行"
				if run.PID > 0 {
					run.Error += fmt.Sprintf("（原 PID %d，其子进程可能在其他进程组）", run.PID)
				}
				run.Error += "；请检查并停止后用 confirm-stopped 确认，确认前不能返工"
			}
			t.Status, t.Unconfirmed = Failed, true
			return nil
		})
	}
}

func (r *Runner) tick(running map[string]*active) {
	for name, a := range running {
		select {
		case <-a.done:
			a.reap()
			if a.phase == "probe" || a.phase == "check" {
				if r.advanceChecks(a) {
					delete(running, name)
				}
			} else {
				started, handled := r.beginChecks(a)
				if !handled {
					r.finish(a, a.why)
				}
				if !started {
					delete(running, name)
				}
			}
			continue
		default:
		}
		t, err := r.Store.Get(a.id)
		switch {
		case err == nil && t.CancelRequested && a.term.IsZero():
			r.stop(a, "已按请求停止")
		case err == nil && a.term.IsZero() && !r.Store.reviewParentActive(t):
			// The parent moved on but its cancel of this review was lost (the
			// accepting process died between the two writes); stop it next tick.
			_, _ = r.Store.Cancel(a.id)
		case (a.phase == "probe" || a.phase == "check") && time.Since(a.checkStarted) > a.checkTimeout && a.term.IsZero():
			r.stop(a, "检查超时")
		case a.phase == "" && r.Cfg.Timeout > 0 && time.Since(a.start) > r.Cfg.Timeout && a.term.IsZero():
			r.stop(a, fmt.Sprintf("超过 %s 未结束，已停止", r.Cfg.Timeout))
		case !a.term.IsZero() && time.Since(a.term) > 10*time.Second:
			a.kill()
		}
	}
	tasks, _ := r.Store.List()
	r.reviewTick(tasks)
	tasks, _ = r.Store.List()
	if running["codex"] == nil && r.quotaStatus.Paused && !r.quotaAllowed() {
		r.maybeAutoSwitch(tasks)
	}
	for _, t := range tasks {
		if t.Status != Queued || running[t.Executor] != nil {
			continue
		}
		if t.Executor == "codex" && r.Cfg.Home != "" && !r.quotaAllowed() {
			r.maybeAutoSwitch(tasks)
			continue
		}
		if a := r.start(t); a != nil {
			running[t.Executor] = a
		}
	}
}

// start runs a queued task in two locked steps, each persisted before the
// next action:
//  1. claim: still queued and not cancelled -> write running with a Run that
//     has no PID yet (a crash from here on is recovered as failed, never
//     replayed);
//  2. launch: re-check cancel, start the process, write its PID.
//
// A cancel is therefore seen before the spawn (nothing starts) or lands on a
// running task with a PID (tick stops it). Residual risk: if the runner dies
// in the instant between the spawn and step 2's write, the task is recovered
// as failed (not replayed) but that executor keeps running unseen; macOS
// doesn't let us tag and find it later (ps shows no environment). snap only
// names the task; its state is re-read under the lock.
func (r *Runner) start(snap *Task) *active {
	var agent Agent
	var files runFiles
	var inv Invocation
	var marker string
	claimed, err := r.Store.Update(snap.ID, func(t *Task) error {
		if t.Status != Queued || t.CancelRequested {
			return errNotQueued
		}
		if !r.Store.reviewParentActive(t) {
			t.Status = Cancelled
			return nil
		}
		a, ok := r.Cfg.Agents[t.Executor]
		agent = a
		if ok && t.Executor == "codex" && r.Cfg.Home != "" {
			settings, e := ReadSettings(r.Cfg.Dir)
			if e != nil || settings != r.quotaSettings {
				return errNotQueued
			}
			if settings.Account == "separate" {
				agent.Env = append(agent.Env, "CODEX_HOME="+filepath.Join(r.Cfg.Home, ".codex-runner"))
			}
		}
		n := len(t.Runs) + 1
		base := filepath.Join(r.Cfg.Dir, "logs", fmt.Sprintf("%s-%d", t.ID, n))
		files = runFiles{out: base + ".out.jsonl", err: base + ".err.txt", last: base + ".last.txt"}
		marker = fmt.Sprintf("%s-%d-%d", t.ID, n, time.Now().UnixNano())
		brief := ""
		if t.Project != "" && t.ReviewOf == "" {
			if p, e := r.Store.GetProject(t.Project); e == nil {
				brief = ProjectBrief(p)
			}
		}
		kind, prompt, session := "task", Preamble+"\n"+brief+"--- card ---\n"+t.Card+"\n", ""
		if t.Rework != "" {
			kind = "rework"
			if t.AutoReworkPending {
				kind = "auto_rework"
			}
			if t.Session != "" {
				session = t.Session
				prompt = t.Rework + "\n\nFix only what this rework names, then end with a new receipt in the same RECEIPT format."
			} else {
				prompt += "\n--- rework of the card above ---\n" + t.Rework + "\n"
			}
		}
		prompt += "\nEnd with RECEIPT <id>: <done|blocked> / Changed: / Verified: (commands and results) / Doubts:.\n"
		if t.Executor == "antigravity" {
			// Ties this run's turn in the agy transcript to this run; see agyTranscriptFinal.
			prompt += "Run marker: " + marker + "\n"
		}
		var dir string
		err := error(nil)
		if !ok {
			err = fmt.Errorf("未知的执行代理：%s", t.Executor)
		} else if dir, err = r.Cfg.verifyFolder(t); err == nil {
			inv, err = agent.prepare(r.path, session, dir, prompt, files.last)
		}
		now := time.Now()
		run := Run{Kind: kind, Started: now, Log: files.out, Marker: marker}
		t.Rework = ""
		t.AutoReworkPending = false
		if err != nil {
			run.Ended, run.Exit, run.Error = now, -1, "无法启动："+err.Error()
			t.Runs, t.Status = append(t.Runs, run), Failed
			r.Logf("%s 启动失败：%v", t.ID, err)
			return nil
		}
		t.Runs, t.Status = append(t.Runs, run), Running // PID 0: starting
		return nil
	})
	if err != nil || claimed.Status != Running {
		return nil // cancelled or changed before the claim, or failed to prepare
	}
	if r.path != "" {
		inv.Env = append(inv.Env, "PATH="+r.path) // later entries win in exec.Cmd
	}
	if testHookAfterClaim != nil {
		testHookAfterClaim(snap.ID)
	}
	var a *active
	_, err = r.Store.Update(snap.ID, func(t *Task) error {
		n := len(t.Runs) - 1
		if n < 0 || t.Status != Running || t.Runs[n].Marker != marker {
			return errNotQueued
		}
		run := &t.Runs[n]
		if t.CancelRequested || !r.Store.reviewParentActive(t) {
			run.Ended, run.Exit, run.Error = time.Now(), -1, "启动前已按请求取消"
			t.Status, t.CancelRequested = Cancelled, false
			return nil
		}
		if testHookBeforeLaunch != nil {
			testHookBeforeLaunch(t.ID)
		}
		baseline := gitSnapshot{}
		if len(t.Checks) > 0 || len(t.Scope) > 0 {
			baseline = snapshotGit(inv.Dir)
		}
		agyRun := AGYRunIdentity{AfterStep: -1, Marker: marker}
		if agent.Name == "antigravity" {
			agyRun.AfterStep = AGYTranscriptLastStep(t.Session)
		}
		cmd, err := launch(inv, files)
		if err != nil {
			run.Ended, run.Exit, run.Error = time.Now(), -1, "无法启动："+err.Error()
			t.Status = Failed
			r.Logf("%s 启动失败：%v", t.ID, err)
			return nil
		}
		run.PID = cmd.Process.Pid
		a = &active{id: t.ID, cmd: cmd, done: make(chan error, 1), start: run.Started, files: files, agent: agent, agyRun: agyRun, baseline: baseline, folder: inv.Dir, env: inv.Env, checks: append([]string(nil), t.Checks...)}
		return nil
	})
	if a == nil {
		return nil
	}
	if err != nil {
		// Launched but the PID could not be written: don't leave it running unseen.
		a.kill()
		a.cmd.Wait()
		r.Logf("%s 记录写入失败，已停止：%v", snap.ID, err)
		return nil
	}
	r.Logf("%s 已交给 %s（pid %d）", a.id, a.agent.Label, a.cmd.Process.Pid)
	go func() { a.done <- a.cmd.Wait() }()
	return a
}

var errNotQueued = errors.New("任务已不在队列中")

// Test-only hooks: between the persisted claim and the launch step, and
// inside the launch step just before the process starts.
var testHookAfterClaim, testHookBeforeLaunch func(id string)

func launch(inv Invocation, f runFiles) (*exec.Cmd, error) {
	out, err := os.OpenFile(f.out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errf, err := os.OpenFile(f.err, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer errf.Close()
	cmd := exec.Command(inv.Argv[0], inv.Argv[1:]...)
	cmd.Dir, cmd.Env = inv.Dir, inv.Env
	cmd.Stdin = strings.NewReader(inv.Stdin)
	cmd.Stdout, cmd.Stderr = out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// stop asks the executor and everything it started to exit. Codex runs its
// shell commands in their own process groups, so the group signal alone
// would leave them running (and reparented to launchd) after Codex exits.
func (r *Runner) stop(a *active, why string) {
	a.why, a.term = why, time.Now()
	a.tree = descendants(a.cmd.Process.Pid)
	syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	for _, pid := range a.tree {
		syscall.Kill(pid, syscall.SIGTERM)
	}
}

// kill force-stops the executor's group and every descendant recorded or still attached.
func (a *active) kill() {
	pids := append(a.tree, descendants(a.cmd.Process.Pid)...)
	syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// reap kills descendants recorded at stop time that outlived the executor.
func (a *active) reap() {
	if a.term.IsZero() {
		return
	}
	for _, pid := range a.tree {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// descendants lists all processes below pid, from one `ps` snapshot.
func descendants(pid int) []int {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		var c, p int
		if n, _ := fmt.Sscan(line, &c, &p); n == 2 {
			children[p] = append(children[p], c)
		}
	}
	var all []int
	queue := []int{pid}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			all = append(all, c)
			queue = append(queue, c)
		}
	}
	return all
}

func (r *Runner) finish(a *active, why string) {
	exit := a.cmd.ProcessState.ExitCode()
	session, final, perr := a.agent.result(a.files.out, a.files.last, a.agyRun)
	limited := why == "" && a.agent.Name == "codex" && (final == "" || exit != 0) && usageLimit(a.files.out, a.files.err)
	var changes *ChangeEvidence
	if a.baseline.status != "" {
		if t, e := r.Store.Get(a.id); e == nil {
			changes = changesSince(a.folder, a.baseline, t.Scope)
		}
	}
	if limited {
		account := "Desktop"
		if r.quotaSettings.Account == "separate" {
			account = "独立账号"
		}
		r.quotaStatus = RunnerStatus{Paused: true, Reason: "Codex 使用额度已耗尽", Account: account, CheckedAt: time.Now().UTC()}
		if err := writeJSON(r.Cfg.Dir, "runner-status.json", r.quotaStatus); err != nil {
			r.Logf("无法保存暂停状态：%v", err)
		}
	}
	r.Store.Update(a.id, func(t *Task) error {
		n := len(t.Runs) - 1
		if n < 0 {
			return nil
		}
		run := &t.Runs[n]
		assignChanges(run, changes)
		run.Ended, run.Exit = time.Now(), exit
		if session != "" {
			t.Session = session
		}
		// A run that finished with an answer is a receipt even if a log line
		// mentions a usage limit; only an unfinished run counts as exhausted.
		switch {
		case why != "":
			run.Error = why
			if t.CancelRequested {
				t.Status = Cancelled
			} else {
				t.Status = Failed
			}
		case limited:
			run.Error, t.Status = "Codex 使用额度已耗尽，等待额度恢复后继续", QuotaExhausted
		case final != "":
			run.Receipt, t.Status = final, Receipt
		default:
			msg := fmt.Sprintf("执行代理退出（代码 %d），没有给出最终回复", exit)
			if perr != nil {
				msg += "：" + perr.Error()
			}
			if errTail := readTail(a.files.err, 1200); errTail != "" {
				msg += "\nstderr 末尾：\n" + errTail
			}
			run.Error, t.Status = msg, Failed
		}
		t.CancelRequested = false
		return nil
	})
	r.Logf("%s 已结束（退出代码 %d）", a.id, exit)
}

func (r *Runner) quotaAllowed() bool {
	if sw, e := ReadSwitchStatus(r.Cfg.Dir); e != nil || sw.ReviewRequired || sw.Stage == "pending" || sw.Stage == "resuming" || sw.Stage == "uncertain" {
		r.switchPause("自动切号结果待核查")
		return false
	}
	s, err := ReadSettings(r.Cfg.Dir)
	if err != nil {
		r.quotaStatus = RunnerStatus{Paused: true, Reason: "执行器设置不可读取", CheckedAt: time.Now().UTC()}
		_ = writeJSON(r.Cfg.Dir, "runner-status.json", r.quotaStatus)
		return false
	}
	if r.quotaSettings == s && !r.quotaStatus.CheckedAt.IsZero() && time.Since(r.quotaStatus.CheckedAt) < time.Minute {
		return !r.quotaStatus.Paused
	}
	r.quotaSettings = s
	if s.Account == "separate" {
		status, e := ReadRunnerStatus(r.Cfg.Dir)
		if e != nil || (status.Paused && status.Account == "独立账号") {
			return false
		}
		r.quotaStatus = RunnerStatus{Account: "独立账号", Reason: "独立账号：开始前不检查额度", CheckedAt: time.Now().UTC()}
		if writeJSON(r.Cfg.Dir, "runner-status.json", r.quotaStatus) != nil {
			r.quotaStatus.Paused = true
			return false
		}
		return true
	}
	query := r.Cfg.Quota
	if query == nil {
		query = (assistant.Reader{Home: r.Cfg.Home}).DesktopFiveHourRemaining
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	remaining, known := query(ctx)
	status := RunnerStatus{Account: "Desktop", CheckedAt: time.Now().UTC()}
	if known && remaining >= 0 && remaining <= 100 {
		status.Remaining = &remaining
		if remaining < s.MinRemaining {
			status.Paused = true
			status.Reason = "五小时剩余额度低于阈值"
		}
	} else {
		status.Paused = true
		status.Reason = "无法确定五小时剩余额度"
	}
	if err := writeJSON(r.Cfg.Dir, "runner-status.json", status); err != nil {
		status.Paused = true
		status.Reason = "无法保存执行器状态"
	}
	r.quotaStatus = status
	return !status.Paused
}

func usageLimit(outPath, errPath string) bool {
	contains := func(s string) bool {
		s = strings.ToLower(s)
		return strings.Contains(s, "usage_limit") || strings.Contains(s, "usage limit")
	}
	if contains(readTail(errPath, 65536)) {
		return true
	}
	b, _ := os.ReadFile(outPath)
	for _, line := range strings.Split(string(b), "\n") {
		var event struct {
			Type  string          `json:"type"`
			Error json.RawMessage `json:"error"`
			Item  struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"item"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && ((event.Type == "error" || event.Type == "turn.failed") && (contains(string(event.Error)) || contains(event.Message)) || event.Item.Type == "error" && contains(event.Item.Message)) {
			return true
		}
	}
	return false
}

func readTail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > int64(n) {
		_, _ = f.Seek(-int64(n), io.SeekEnd)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(n)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

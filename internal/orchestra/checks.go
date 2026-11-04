package orchestra

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const probeMarker = "__ORC9_SANDBOX_READY__"
const unsafeProbeMarker = "__ORC9_SANDBOX_FAILED__"

func prepareSandboxProbe(a *active) error {
	home := ""
	for _, v := range a.env {
		if strings.HasPrefix(v, "HOME=") {
			home = strings.TrimPrefix(v, "HOME=")
		}
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("HOME unavailable")
	}
	realHome, e := filepath.EvalSymlinks(home)
	if e != nil {
		return e
	}
	realFolder, e := filepath.EvalSymlinks(a.folder)
	if e != nil {
		return e
	}
	if realHome == realFolder || strings.HasPrefix(realHome, realFolder+string(os.PathSeparator)) {
		return fmt.Errorf("no external sandbox probe location")
	}
	dir, e := os.MkdirTemp(realHome, ".orc9-probe-")
	if e != nil {
		return e
	}
	a.probeDir = dir
	probe := filepath.Join(dir, "outside")
	f, e := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Remove(probe)
}
func cleanupSandboxProbe(a *active) {
	if a.probeDir != "" {
		_ = os.RemoveAll(a.probeDir)
		a.probeDir = ""
	}
}

func sandboxSupported(bin string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "sandbox", "--help")
	var output capBuffer
	output.max = 64 << 10
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	return cmd.Run() == nil && strings.Contains(output.String(), "--permission-profile") && strings.Contains(output.String(), "sandbox")
}
func sandboxInvocation(a *active, command string) Invocation {
	profile := a.profile
	return Invocation{Dir: a.folder, Env: a.env, Argv: []string{a.sandboxBin, "sandbox", "-P", profile, "-C", a.folder, "-c", fmt.Sprintf(`permissions.%s.extends=":workspace"`, profile), "-c", fmt.Sprintf("permissions.%s.network.enabled=true", profile), "--", "/bin/sh", "-c", command}}
}
func launchCheck(inv Invocation, log string) (*exec.Cmd, error) {
	f, e := os.OpenFile(log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	cmd := exec.Command(inv.Argv[0], inv.Argv[1:]...)
	cmd.Dir, cmd.Env = inv.Dir, inv.Env
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	return cmd, nil
}
func (r *Runner) beginChecks(a *active) (bool, bool) {
	if a.why != "" || len(a.checks) == 0 {
		return false, false
	}
	exit := a.cmd.ProcessState.ExitCode()
	session, final, _ := a.agent.result(a.files.out, a.files.last, a.agyRun)
	if final == "" || (a.agent.Name == "codex" && exit != 0 && usageLimit(a.files.out, a.files.err)) {
		return false, false
	}
	settings, e := ReadSettings(r.Cfg.Dir)
	if e != nil {
		r.finishUnavailable(a, session, final, "设置不可读取")
		return false, true
	}
	seconds := settings.CheckTimeoutSeconds
	if seconds == 0 {
		seconds = 600
	}
	a.checkTimeout = time.Duration(seconds) * time.Second
	a.sandboxBin = a.cmd.Path
	if a.agent.Name != "codex" {
		codex, ok := r.Cfg.Agents["codex"]
		if !ok {
			r.finishUnavailable(a, session, final, "无可用沙箱")
			return false, true
		}
		a.sandboxBin, e = codex.Resolve(r.path)
		if e != nil {
			r.finishUnavailable(a, session, final, "无可用沙箱")
			return false, true
		}
	}
	if !sandboxSupported(a.sandboxBin) {
		r.finishUnavailable(a, session, final, "无可用沙箱")
		return false, true
	}
	if e = prepareSandboxProbe(a); e != nil {
		cleanupSandboxProbe(a)
		r.finishUnavailable(a, session, final, "无法核验沙箱边界")
		return false, true
	}
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", a.id, time.Now().UnixNano())))
	a.profile = fmt.Sprintf("orc9_%x", hash[:8])
	a.phase = "probe"
	a.checkStarted = time.Now()
	a.checkTimeout = 5 * time.Second
	logDir := filepath.Join(r.Cfg.Dir, "logs", a.id)
	if e = os.MkdirAll(logDir, 0700); e != nil {
		cleanupSandboxProbe(a)
		r.finishUnavailable(a, session, final, "无法创建检查日志")
		return false, true
	}
	log := filepath.Join(logDir, fmt.Sprintf("%d-probe.txt", a.checkStarted.UnixNano()))
	a.checkLog = log
	started := false
	_, e = r.Store.Update(a.id, func(t *Task) error {
		if t.Status != Running {
			return errNotQueued
		}
		n := len(t.Runs) - 1
		if n < 0 {
			return errNotQueued
		}
		if t.CancelRequested {
			return nil
		}
		run := &t.Runs[n]
		run.Phase = "checking"
		run.Exit = exit
		run.Receipt = final
		if session != "" {
			t.Session = session
		}
		probe := sandboxInvocation(a, `if /usr/bin/touch "$1" 2>/dev/null; then printf `+unsafeProbeMarker+`; else printf `+probeMarker+`; fi`)
		probe.Argv = append(probe.Argv, "orc9-probe", filepath.Join(a.probeDir, "outside"))
		cmd, e := launchCheck(probe, log)
		if e != nil {
			return e
		}
		a.cmd = cmd
		a.done = make(chan error, 1)
		run.PID = cmd.Process.Pid
		started = true
		return nil
	})
	if e != nil || !started {
		if started {
			a.kill()
			_ = a.cmd.Wait()
		}
		cleanupSandboxProbe(a)
		r.finishUnavailable(a, session, final, "沙箱无法启动")
		return false, true
	}
	go func() { a.done <- a.cmd.Wait() }()
	return true, true
}

// advanceChecks returns true when no process remains for this task.
func (r *Runner) advanceChecks(a *active) bool {
	if a.why != "" && a.why != "检查超时" {
		cleanupSandboxProbe(a)
		r.finishChecksInterrupted(a, a.why)
		return true
	}
	if a.phase == "probe" {
		_, probeErr := os.Lstat(filepath.Join(a.probeDir, "outside"))
		if a.why != "" || a.cmd.ProcessState.ExitCode() != 0 || readTail(a.checkLog, 100) != probeMarker || !os.IsNotExist(probeErr) {
			cleanupSandboxProbe(a)
			r.finishUnavailable(a, "", "", "沙箱不可用")
			return true
		}
		cleanupSandboxProbe(a)
	} else {
		check := CheckResult{Command: a.checks[a.checkIndex], Exit: a.cmd.ProcessState.ExitCode(), Duration: time.Since(a.checkStarted).Seconds(), TimedOut: a.why == "检查超时", Log: a.checkLog, Tail: readTail(a.checkLog, 4096), Status: "passed"}
		if check.Exit != 0 || check.TimedOut {
			check.Status = "failed"
		}
		_, err := r.Store.Update(a.id, func(t *Task) error {
			if t.Status != Running || len(t.Runs) == 0 {
				return errNotQueued
			}
			run := &t.Runs[len(t.Runs)-1]
			run.Checks = append(run.Checks, check)
			return nil
		})
		if err != nil {
			r.finishChecksInterrupted(a, "检查结果无法保存")
			return true
		}
		a.checkIndex++
	}
	if a.checkIndex >= len(a.checks) {
		r.finishChecks(a)
		return true
	}
	settings, err := ReadSettings(r.Cfg.Dir)
	if err != nil {
		r.finishUnavailable(a, "", "", "设置不可读取")
		return true
	}
	seconds := settings.CheckTimeoutSeconds
	if seconds == 0 {
		seconds = 600
	}
	a.checkTimeout = time.Duration(seconds) * time.Second
	a.phase = "check"
	a.why = ""
	a.term = time.Time{}
	a.tree = nil
	a.checkStarted = time.Now()
	a.checkLog = filepath.Join(r.Cfg.Dir, "logs", a.id, fmt.Sprintf("%d-check-%d.txt", a.checkStarted.UnixNano(), a.checkIndex+1))
	started := false
	_, err = r.Store.Update(a.id, func(t *Task) error {
		if t.Status != Running || t.CancelRequested {
			return errNotQueued
		}
		cmd, e := launchCheck(sandboxInvocation(a, a.checks[a.checkIndex]), a.checkLog)
		if e != nil {
			return e
		}
		a.cmd = cmd
		a.done = make(chan error, 1)
		t.Runs[len(t.Runs)-1].PID = cmd.Process.Pid
		started = true
		return nil
	})
	if err != nil || !started {
		if started {
			a.kill()
			_ = a.cmd.Wait()
		}
		t, _ := r.Store.Get(a.id)
		if t != nil && t.CancelRequested {
			r.finishChecksInterrupted(a, "已按请求停止")
		} else {
			r.finishUnavailable(a, "", "", "沙箱无法启动")
		}
		return true
	}
	go func() { a.done <- a.cmd.Wait() }()
	return false
}
func (r *Runner) finishUnavailable(a *active, session, final, reason string) {
	_, err := r.Store.Update(a.id, func(t *Task) error {
		if len(t.Runs) == 0 {
			return nil
		}
		run := &t.Runs[len(t.Runs)-1]
		if a.phase == "" && a.cmd.ProcessState != nil {
			run.Exit = a.cmd.ProcessState.ExitCode()
		}
		if session != "" {
			t.Session = session
		}
		if final != "" {
			run.Receipt = final
		}
		for i := len(run.Checks); i < len(a.checks); i++ {
			run.Checks = append(run.Checks, CheckResult{Command: a.checks[i], Status: "unavailable", Exit: -1, Tail: reason})
		}
		return nil
	})
	if err == nil {
		r.finishChecks(a)
	}
}
func (r *Runner) finishChecksInterrupted(a *active, why string) {
	cleanupSandboxProbe(a)
	r.Store.Update(a.id, func(t *Task) error {
		if len(t.Runs) == 0 {
			return nil
		}
		run := &t.Runs[len(t.Runs)-1]
		run.Phase = ""
		run.Ended = time.Now()
		run.Error = why
		if t.CancelRequested {
			t.Status = Cancelled
		} else {
			t.Status = Failed
		}
		t.CancelRequested = false
		return nil
	})
}
func (r *Runner) finishChecks(a *active) {
	t, e := r.Store.Get(a.id)
	if e != nil {
		return
	}
	changes := changesSince(a.folder, a.baseline, t.Scope)
	settings, settingsErr := ReadSettings(r.Cfg.Dir)
	r.Store.Update(a.id, func(t *Task) error {
		if len(t.Runs) == 0 {
			return nil
		}
		run := &t.Runs[len(t.Runs)-1]
		run.Phase = ""
		run.Ended = time.Now()
		assignChanges(run, changes)
		if t.CancelRequested {
			t.Status = Cancelled
			t.CancelRequested = false
			return nil
		}
		var fails []CheckResult
		for _, c := range run.Checks {
			if c.Status == "failed" {
				fails = append(fails, c)
			}
		}
		if len(fails) > 0 && settingsErr == nil && t.AutoReworkCount < settings.AutoReworkMax && t.Session != "" {
			text := reworkEvidence(fails)
			t.Rework = "Fix the following failed checks, then give a new receipt:\n" + text + "\n" + AutoReworkLimits
			t.AutoReworkCount++
			t.AutoReworkPending = true
			t.Status = Queued
		} else {
			t.Status = Receipt
		}
		return nil
	})
}

func reworkEvidence(checks []CheckResult) string {
	if len(checks) == 0 {
		return ""
	}
	tailLimit := 1200 / len(checks)
	parts := make([]string, 0, len(checks))
	for _, c := range checks {
		command := []rune(c.Command)
		if len(command) > 160 {
			command = append(command[:157], '.', '.', '.')
		}
		parts = append(parts, fmt.Sprintf("%s (exit %d):\n%s", string(command), c.Exit, tail(c.Tail, tailLimit)))
	}
	text := strings.Join(parts, "\n\n")
	if len([]rune(text)) > 3000 {
		return string([]rune(text)[:3000])
	}
	return text
}

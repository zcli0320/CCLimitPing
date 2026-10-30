package assistant

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Process identities include launch time, so PID reuse is not mistaken for
// survival or a clean restart. Only the actual Codex app and its direct bundled
// app-server are eligible; other CLI clients are never terminated.
type desktopProcessIdentity struct {
	PID, PPID, UID int
	Start, Command string
}
type desktopProcessPair struct{ App, Server, Bridge desktopProcessIdentity }

func desktopProcessTree(procs []desktopProcessIdentity, root desktopProcessIdentity) []desktopProcessIdentity {
	known := map[int]bool{root.PID: true}
	result := []desktopProcessIdentity{root}
	for changed := true; changed; {
		changed = false
		for _, p := range procs {
			if p.UID == root.UID && known[p.PPID] && !known[p.PID] {
				known[p.PID] = true
				result = append(result, p)
				changed = true
			}
		}
	}
	return result
}

func desktopCapturedProcessesExited(old []desktopProcessIdentity, current []desktopProcessIdentity) bool {
	if len(old) == 0 {
		return false
	}
	for _, before := range old {
		if before.PID <= 0 || before.Start == "" {
			return false
		}
		for _, now := range current {
			if now.PID == before.PID && now.UID == before.UID && now.Start == before.Start {
				return false
			}
		}
	}
	return true
}

func parseDesktopProcessList(text string) ([]desktopProcessIdentity, error) {
	var result []desktopProcessIdentity
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 9 {
			return nil, errors.New("进程列表格式不受支持")
		}
		pid, e1 := strconv.Atoi(fields[0])
		ppid, e2 := strconv.Atoi(fields[1])
		uid, e3 := strconv.Atoi(fields[2])
		if e1 != nil || e2 != nil || e3 != nil || pid <= 0 {
			return nil, errors.New("进程列表格式不受支持")
		}
		result = append(result, desktopProcessIdentity{PID: pid, PPID: ppid, UID: uid, Start: strings.Join(fields[3:8], " "), Command: strings.Join(fields[8:], " ")})
	}
	return result, nil
}

func findDesktopProcessPair(procs []desktopProcessIdentity, uid int, appPath string) (desktopProcessPair, error) {
	return findDesktopProcessPairWithBridge(procs, uid, appPath, "")
}

func desktopProcessRuns(p desktopProcessIdentity, executable, arg string) bool {
	if executable == "" || !strings.HasPrefix(p.Command, executable+" ") {
		return false
	}
	for _, word := range strings.Fields(strings.TrimPrefix(p.Command, executable+" ")) {
		if word == arg {
			return true
		}
	}
	return false
}

func findDesktopProcessPairWithBridge(procs []desktopProcessIdentity, uid int, appPath, bridgePath string) (desktopProcessPair, error) {
	var pair desktopProcessPair
	main := filepath.Join(appPath, "Contents/MacOS/ChatGPT")
	server := filepath.Join(appPath, "Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex")
	for _, p := range procs {
		if p.UID == uid && p.Command == main {
			if pair.App.PID != 0 {
				return desktopProcessPair{}, errors.New("发现多个 Desktop 主进程")
			}
			pair.App = p
		}
	}
	if pair.App.PID == 0 {
		return pair, errors.New("Desktop 未运行")
	}
	for _, p := range procs {
		if p.UID == uid && p.PPID == pair.App.PID && desktopProcessRuns(p, bridgePath, "app-server") {
			if pair.Bridge.PID != 0 {
				return desktopProcessPair{}, errors.New("发现多个 Desktop 运行核验进程")
			}
			pair.Bridge = p
		}
	}
	for _, p := range procs {
		if p.UID != uid || !desktopProcessRuns(p, server, "app-server") {
			continue
		}
		if p.PPID != pair.App.PID && (pair.Bridge.PID == 0 || p.PPID != pair.Bridge.PID) {
			continue
		}
		if pair.Server.PID != 0 {
			return desktopProcessPair{}, errors.New("发现多个 Desktop 后端")
		}
		pair.Server = p
	}
	if pair.Server.PID == 0 || (pair.Bridge.PID != 0 && pair.Server.PPID != pair.Bridge.PID) {
		return desktopProcessPair{}, errors.New("Desktop 后端不可确认")
	}
	return pair, nil
}

func desktopOldProcessesExited(old desktopProcessPair, procs []desktopProcessIdentity, uid int, appPath string) bool {
	if old.App.PID <= 0 || old.Server.PID <= 0 || old.App.Start == "" || old.Server.Start == "" {
		return false
	}
	for _, p := range procs {
		if p.UID != uid {
			continue
		}
		if (p.PID == old.App.PID && p.Start == old.App.Start) || (p.PID == old.Server.PID && p.Start == old.Server.Start) || (old.Bridge.PID > 0 && p.PID == old.Bridge.PID && p.Start == old.Bridge.Start) {
			return false
		}
		if p.Command == filepath.Join(appPath, "Contents/MacOS/ChatGPT") {
			return false
		}
	}
	return true
}

func desktopEnvironmentMatches(text, home string) bool {
	// ps renders environment as whitespace-separated text. Ambiguous paths or
	// duplicate variables cannot prove the effective root and fail closed.
	if strings.ContainsAny(home, " \t\n\r") || !filepath.IsAbs(home) {
		return false
	}
	values := map[string]string{}
	for _, word := range strings.Fields(text) {
		key, value, ok := strings.Cut(word, "=")
		if !ok || (key != "HOME" && key != "CODEX_HOME") {
			continue
		}
		if _, exists := values[key]; exists {
			return false
		}
		values[key] = value
	}
	if values["HOME"] != home {
		return false
	}
	codexHome, present := values["CODEX_HOME"]
	return !present || filepath.Clean(codexHome) == filepath.Join(home, ".codex")
}

func readDesktopProcesses(ctx context.Context) ([]desktopProcessIdentity, error) {
	cmd := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,uid=,lstart=,command=")
	cmd.Env = desktopProcessListEnvironment(os.Environ())
	out, err := cmd.Output()
	if err != nil || len(out) > 8<<20 {
		return nil, errors.New("无法检查 Desktop 进程")
	}
	return parseDesktopProcessList(string(out))
}

// lstart follows the caller's locale (zh_CN prints four fields, not five), so
// time is pinned to C. LC_ALL=C would also make ps escape non-ASCII bytes in
// commands (M-e...), which hides a bridge installed under a Chinese app name.
func desktopProcessListEnvironment(env []string) []string {
	var out []string
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if key != "LC_ALL" && key != "LANG" && key != "LC_TIME" && key != "LC_CTYPE" {
			out = append(out, value)
		}
	}
	return append(out, "LC_TIME=C", "LC_CTYPE=UTF-8")
}

func desktopServerUsesHome(ctx context.Context, p desktopProcessIdentity, home string) bool {
	if p.PID <= 0 || p.UID != os.Getuid() {
		return false
	}
	out, err := exec.CommandContext(ctx, "/bin/ps", "eww", "-p", strconv.Itoa(p.PID)).Output()
	return err == nil && len(out) <= 1<<20 && desktopEnvironmentMatches(string(out), home)
}

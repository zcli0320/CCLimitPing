package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wavever/CCLimitPing/internal/orchestra"
)

const orchestraUsage = `Usage: codex-helper orchestra <command>
  agents                                   executors and whether each can run
  dispatch --to <agent> --folder <dir> --id <id> [--lead <agent>]   card on stdin
  list | get <id>
  wait <id> [--timeout 540s]               until the task needs the lead again
  rework <id>                              rework text on stdin
  cancel <id> | accept <id>
  confirm-stopped <id>                     after a runner crash: the old executor is gone
  mcp --lead <name>                        MCP stdio server
  setup | login-status                     isolated Codex executor account
  runner                                   runner status and state directory
  run                                      execute queued tasks (Ctrl-C stops)`

func orchestraConfig() (orchestra.Config, orchestra.Store) {
	home, err := os.UserHomeDir()
	if err != nil {
		orchestraFail(err)
	}
	root := os.Getenv("CODEX_HELPER_ORCHESTRA_DIR")
	if root == "" {
		name := "CodexHelper"
		if os.Getenv("CODEX_HELPER_NATIVE_PREVIEW") == "1" {
			name = "CodexHelper-AccountPreview"
		}
		root = filepath.Join(home, "Library", "Application Support", name, "orchestra")
	}
	cfg := orchestra.DefaultConfig(home, root)
	return cfg, orchestra.Store{Dir: root}
}

func orchestraFail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func orchestraPrint(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		os.Exit(1)
	}
}

func orchestraStdin() string {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		orchestraFail(err)
	}
	return string(data)
}

func orchestraCommand() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, orchestraUsage)
		os.Exit(2)
	}
	cfg, store := orchestraConfig()
	sub, args := os.Args[2], os.Args[3:]
	needID := func() string {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, orchestraUsage)
			os.Exit(2)
		}
		return args[0]
	}
	var task *orchestra.Task
	var err error
	switch sub {
	case "setup", "login-status":
		if len(args) != 0 {
			orchestraFail(fmt.Errorf("%s takes no arguments", sub))
		}
		home, e := os.UserHomeDir()
		if e != nil {
			orchestraFail(e)
		}
		dir := filepath.Join(home, ".codex-runner")
		created := []string{}
		if sub == "setup" {
			dir, created, e = orchestra.EnsureRunnerHome(home)
			if e != nil {
				orchestraFail(e)
			}
		}
		bin, e := cfg.Agents["codex"].Resolve(orchestra.LoginPath(cfg.Dir, false))
		login := orchestra.RunnerLogin(home, bin)
		orchestraPrint(map[string]any{"dir": dir, "created": created, "login": login, "codex_bin": bin})
		return
	case "runner":
		if len(args) != 0 {
			orchestraFail(fmt.Errorf("runner takes no arguments"))
		}
		orchestraPrint(map[string]any{"active": orchestra.RunnerActive(cfg.Dir), "dir": cfg.Dir})
		return
	case "mcp":
		fs := flag.NewFlagSet("mcp", flag.ExitOnError)
		lead := fs.String("lead", "", "lead agent")
		fs.Parse(args)
		if *lead == "" || fs.NArg() != 0 {
			orchestraFail(fmt.Errorf("mcp requires --lead <name>"))
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := orchestra.ServeMCP(ctx, os.Stdin, os.Stdout, cfg, store, *lead); err != nil {
			orchestraFail(err)
		}
		return
	case "agents":
		type row struct {
			Name      string `json:"name"`
			Label     string `json:"label"`
			Available bool   `json:"available"`
			Reason    string `json:"reason,omitempty"`
			Login     string `json:"login,omitempty"`
		}
		var rows []row
		for _, name := range []string{"codex", "claude", "antigravity"} {
			a := cfg.Agents[name]
			bin, e := a.Resolve(orchestra.LoginPath(cfg.Dir, false))
			r := row{Name: name, Label: a.Label, Available: e == nil}
			if name == "codex" {
				home, err := os.UserHomeDir()
				if err != nil || e != nil {
					r.Login = "unknown"
				} else {
					r.Login = orchestra.RunnerLogin(home, bin).State
				}
			}
			if e != nil {
				r.Reason = e.Error()
			}
			rows = append(rows, r)
		}
		orchestraPrint(rows)
		return
	case "dispatch":
		fs := flag.NewFlagSet("dispatch", flag.ExitOnError)
		to := fs.String("to", "", "executor agent")
		folder := fs.String("folder", "", "working folder")
		id := fs.String("id", "", "task id")
		lead := fs.String("lead", "", "lead agent")
		fs.Parse(args)
		task, err = store.Dispatch(cfg, *lead, *to, *folder, *id, orchestraStdin())
	case "list":
		tasks, e := store.List()
		if e != nil {
			orchestraFail(e)
		}
		orchestraPrint(tasks)
		return
	case "get":
		task, err = store.Get(needID())
	case "wait":
		id := needID()
		fs := flag.NewFlagSet("wait", flag.ExitOnError)
		timeout := fs.Duration("timeout", 540*time.Second, "max wait")
		fs.Parse(args[1:])
		task, err = store.Wait(id, *timeout, time.Second)
	case "rework":
		task, err = store.RequestRework(needID(), orchestraStdin())
	case "cancel":
		task, err = store.Cancel(needID())
	case "accept":
		task, err = store.Accept(needID())
	case "confirm-stopped":
		task, err = store.ConfirmStopped(needID())
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		r := &orchestra.Runner{Cfg: cfg, Store: store, Logf: func(f string, a ...any) {
			fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+f+"\n", a...)
		}}
		fmt.Fprintf(os.Stderr, "任务执行器已启动（%s），Ctrl-C 停止\n", cfg.Dir)
		if err := r.Run(ctx); err != nil {
			orchestraFail(err)
		}
		return
	default:
		fmt.Fprintln(os.Stderr, orchestraUsage)
		os.Exit(2)
	}
	if err != nil {
		orchestraFail(err)
	}
	orchestraPrint(task)
}

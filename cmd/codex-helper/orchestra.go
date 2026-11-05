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
	"strings"
	"syscall"
	"time"

	"github.com/wavever/CCLimitPing/internal/assistant"
	"github.com/wavever/CCLimitPing/internal/orchestra"
)

const orchestraUsage = `Usage: codex-helper orchestra <command>
  agents                                   executors and whether each can run
  project create --id <id> --name <n> --folder <dir> --to <agent> [--reviewer <r>] [--lead <l>] [--check <c>]... [--scope <s>]... [--sync-agents-md]
  project update <id> [--name <n>] [--folder <dir>] [--to <agent>] [--lead <l>] [--reviewer <r>] [--check <c>]... [--clear-checks] [--scope <s>]... [--clear-scope] [--sync-agents-md=true|false]
  project list [--all] | project get <id> | project path <id> | project archive <id>
  dispatch [--project <id>] [--to <agent>] [--folder <dir>] --id <id> [--lead <agent>]   card on stdin
  assign --id <task-id> [--project <project-id>]
  list [--project <id>] | get <id>
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
	reader, err := assistant.NewReader()
	if err != nil {
		orchestraFail(err)
	}
	cfg.Home = reader.Home
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
func orchestraTaskJSON(t *orchestra.Task) any {
	if t == nil {
		return nil
	}
	e := orchestra.Evidence(t)
	p := orchestra.Progress(t)
	if e == nil && p == nil {
		return t
	}
	b, _ := json.Marshal(t)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if e != nil {
		m["evidence"] = e
	}
	if p != nil {
		m["progress"] = p
		if (t.Session == "") && p.ConversationID != "" {
			m["session"] = p.ConversationID
		}
	}
	return m
}

func orchestraStdin() string {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		orchestraFail(err)
	}
	return string(data)
}

type repeatedString []string

func (r *repeatedString) String() string     { return fmt.Sprint([]string(*r)) }
func (r *repeatedString) Set(v string) error { *r = append(*r, v); return nil }

func parseProjectUpdateFlags(pargs []string) (string, orchestra.ProjectPatch, error) {
	var id string
	if len(pargs) > 0 && !strings.HasPrefix(pargs[0], "-") {
		id, pargs = pargs[0], pargs[1:]
	}
	fs := flag.NewFlagSet("project update", flag.ContinueOnError)
	idFlag := fs.String("id", "", "project id")
	name := fs.String("name", "", "project name")
	folder := fs.String("folder", "", "working folder")
	to := fs.String("to", "", "executor agent")
	lead := fs.String("lead", "", "lead agent")
	reviewer := fs.String("reviewer", "", "reviewer agent")
	clearChecks := fs.Bool("clear-checks", false, "clear all checks")
	clearScope := fs.Bool("clear-scope", false, "clear all scope prefixes")
	syncAgents := fs.Bool("sync-agents-md", false, "sync managed block in AGENTS.md")
	var checks, scope repeatedString
	fs.Var(&checks, "check", "check command (repeatable)")
	fs.Var(&scope, "scope", "allowed path prefix (repeatable)")
	if err := fs.Parse(pargs); err != nil {
		return "", orchestra.ProjectPatch{}, err
	}
	if id == "" && *idFlag != "" {
		id = *idFlag
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return "", orchestra.ProjectPatch{}, fmt.Errorf("unexpected project update arguments")
	}
	if id == "" {
		return "", orchestra.ProjectPatch{}, fmt.Errorf("project update requires <id>")
	}
	var patch orchestra.ProjectPatch
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		seen[f.Name] = true
		switch f.Name {
		case "name":
			v := *name
			patch.Name = &v
		case "folder":
			v := *folder
			patch.Folder = &v
		case "to":
			v := *to
			patch.Executor = &v
		case "lead":
			v := *lead
			patch.Lead = &v
		case "reviewer":
			v := *reviewer
			patch.Reviewer = &v
		case "sync-agents-md":
			v := *syncAgents
			patch.SyncAgentsMD = &v
		}
	})
	if seen["clear-checks"] && *clearChecks && !seen["check"] {
		empty := []string{}
		patch.Checks = &empty
	} else if seen["check"] {
		v := append([]string(nil), checks...)
		patch.Checks = &v
	}
	if seen["clear-scope"] && *clearScope && !seen["scope"] {
		empty := []string{}
		patch.Scope = &empty
	} else if seen["scope"] {
		v := append([]string(nil), scope...)
		patch.Scope = &v
	}
	return id, patch, nil
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
	case "auto-switch-ack":
		if len(args) != 0 {
			orchestraFail(fmt.Errorf("auto-switch-ack takes no arguments"))
		}
		status, e := orchestra.AcknowledgeAutoSwitch(cfg.Dir, cfg.Home)
		if e != nil {
			orchestraFail(e)
		}
		orchestraPrint(status)
		return
	case "settings":
		fs := flag.NewFlagSet("settings", flag.ExitOnError)
		account := fs.String("account", "", "desktop or separate")
		minimum := fs.Float64("min-remaining", -1, "minimum percent")
		autoSwitch := fs.Bool("auto-switch", false, "automatically switch while Desktop is closed")
		autoStartRunner := fs.Bool("auto-start-runner", false, "start the runner with Codex Helper")
		autoRework := fs.Int("auto-rework-max", -1, "maximum automatic reworks (0-2)")
		checkTimeout := fs.Int("check-timeout-seconds", -1, "per-check timeout")
		fs.Parse(args)
		if fs.NArg() != 0 {
			orchestraFail(fmt.Errorf("unexpected settings arguments"))
		}
		s, e := orchestra.ReadSettings(cfg.Dir)
		if e != nil {
			orchestraFail(e)
		}
		changed := false
		fs.Visit(func(f *flag.Flag) {
			changed = true
			if f.Name == "account" {
				s.Account = *account
			}
			if f.Name == "min-remaining" {
				s.MinRemaining = *minimum
			}
			if f.Name == "auto-switch" {
				s.AutoSwitch = *autoSwitch
			}
			if f.Name == "auto-start-runner" {
				s.AutoStartRunner = *autoStartRunner
			}
			if f.Name == "auto-rework-max" {
				s.AutoReworkMax = *autoRework
			}
			if f.Name == "check-timeout-seconds" {
				s.CheckTimeoutSeconds = *checkTimeout
			}
		})
		if changed {
			if e = orchestra.WriteSettings(cfg.Dir, s); e != nil {
				orchestraFail(e)
			}
		}
		orchestraPrint(s)
		return
	case "setup", "login-status":
		if len(args) != 0 {
			orchestraFail(fmt.Errorf("%s takes no arguments", sub))
		}
		home := cfg.Home
		var e error
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
		status, e := orchestra.ReadRunnerStatus(cfg.Dir)
		if e != nil {
			orchestraFail(e)
		}
		orchestraPrint(map[string]any{"active": orchestra.RunnerActive(cfg.Dir), "dir": cfg.Dir, "paused": status.Paused, "reason": status.Reason, "remaining": status.Remaining, "account": status.Account, "checked_at": status.CheckedAt, "auto_switch": status.AutoSwitch})
		return
	case "mcp":
		fs := flag.NewFlagSet("mcp", flag.ExitOnError)
		lead := fs.String("lead", "", "lead agent")
		fs.Parse(args)
		if *lead == "" || fs.NArg() != 0 {
			orchestraFail(fmt.Errorf("mcp requires --lead <name>"))
		}
		cwd, _ := os.Getwd()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := orchestra.ServeMCP(ctx, os.Stdin, os.Stdout, cfg, store, *lead, cwd); err != nil {
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
				home := cfg.Home
				if e != nil {
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
	case "project":
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, orchestraUsage)
			os.Exit(2)
		}
		psub, pargs := args[0], args[1:]
		switch psub {
		case "create":
			fs := flag.NewFlagSet("project create", flag.ExitOnError)
			id := fs.String("id", "", "project id")
			name := fs.String("name", "", "project name")
			folder := fs.String("folder", "", "working folder")
			to := fs.String("to", "", "executor agent")
			reviewer := fs.String("reviewer", "", "reviewer agent")
			lead := fs.String("lead", "", "lead agent")
			syncAgents := fs.Bool("sync-agents-md", false, "sync managed block in AGENTS.md")
			var checks, scope repeatedString
			fs.Var(&checks, "check", "check command (repeatable)")
			fs.Var(&scope, "scope", "allowed path prefix (repeatable)")
			fs.Parse(pargs)
			p, e := store.CreateProject(cfg, orchestra.Project{
				ID: *id, Name: *name, Folder: *folder, Executor: *to,
				Reviewer: *reviewer, Lead: *lead, Checks: checks, Scope: scope, SyncAgentsMD: *syncAgents,
			})
			if e != nil {
				orchestraFail(e)
			}
			orchestraPrint(p)
		case "update":
			id, patch, e := parseProjectUpdateFlags(pargs)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				fmt.Fprintln(os.Stderr, orchestraUsage)
				os.Exit(2)
			}
			p, e := store.UpdateProject(cfg, id, patch)
			if e != nil {
				orchestraFail(e)
			}
			orchestraPrint(p)
		case "path":
			if len(pargs) != 1 {
				fmt.Fprintln(os.Stderr, orchestraUsage)
				os.Exit(2)
			}
			p, e := store.GetProject(pargs[0])
			if e != nil {
				orchestraFail(e)
			}
			path := orchestra.RulesPath(p)
			if path != "" {
				if _, statErr := os.Stat(path); os.IsNotExist(statErr) && !p.Archived {
					_ = orchestra.WriteProjectFile(p)
				}
			}
			fmt.Println(path)
		case "list":
			fs := flag.NewFlagSet("project list", flag.ExitOnError)
			all := fs.Bool("all", false, "include archived projects")
			fs.Parse(pargs)
			ps, e := store.ListProjects()
			if e != nil {
				orchestraFail(e)
			}
			out := []*orchestra.Project{}
			for _, p := range ps {
				if *all || !p.Archived {
					out = append(out, p)
				}
			}
			orchestraPrint(out)
		case "get":
			if len(pargs) != 1 {
				fmt.Fprintln(os.Stderr, orchestraUsage)
				os.Exit(2)
			}
			p, e := store.GetProject(pargs[0])
			if e != nil {
				orchestraFail(e)
			}
			orchestraPrint(p)
		case "archive":
			if len(pargs) != 1 {
				fmt.Fprintln(os.Stderr, orchestraUsage)
				os.Exit(2)
			}
			p, e := store.ArchiveProject(pargs[0])
			if e != nil {
				orchestraFail(e)
			}
			orchestraPrint(p)
		default:
			fmt.Fprintln(os.Stderr, orchestraUsage)
			os.Exit(2)
		}
		return
	case "dispatch":
		fs := flag.NewFlagSet("dispatch", flag.ExitOnError)
		project := fs.String("project", "", "project id")
		to := fs.String("to", "", "executor agent")
		folder := fs.String("folder", "", "working folder")
		id := fs.String("id", "", "task id")
		lead := fs.String("lead", "", "lead agent")
		var checks, scope repeatedString
		fs.Var(&checks, "check", "check command (repeatable)")
		fs.Var(&scope, "scope", "allowed path prefix (repeatable)")
		fs.Parse(args)
		task, err = store.DispatchInProject(cfg, *lead, *project, *to, *folder, *id, orchestraStdin(), checks, scope)
	case "assign":
		fs := flag.NewFlagSet("assign", flag.ExitOnError)
		id := fs.String("id", "", "task id")
		project := fs.String("project", "", "project id (empty to unassign)")
		fs.Parse(args)
		if *id == "" && fs.NArg() == 1 {
			*id = fs.Arg(0)
		}
		task, err = store.AssignProject(*id, *project)
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		project := fs.String("project", "", "filter by project id")
		fs.Parse(args)
		tasks, e := store.List()
		if e != nil {
			orchestraFail(e)
		}
		if tasks == nil && *project == "" {
			orchestraPrint(tasks)
		} else {
			rows := make([]any, 0, len(tasks))
			for _, t := range tasks {
				if *project == "" || t.Project == *project {
					rows = append(rows, orchestraTaskJSON(t))
				}
			}
			orchestraPrint(rows)
		}
		return
	case "get":
		task, err = store.Get(needID())
		if err != nil {
			break
		}
		if err == nil && len(args) == 1 {
			fmt.Println(orchestra.Summary(task))
			for _, v := range orchestra.FailingChecks(task) {
				fmt.Printf("%s (exit %d)\n%s\n", v.Command, v.Exit, v.Tail)
			}
			return
		}
		if len(args) != 2 || args[1] != "--json" {
			orchestraFail(fmt.Errorf("get accepts <id> [--json]"))
		}
	case "wait":
		id := needID()
		fs := flag.NewFlagSet("wait", flag.ExitOnError)
		timeout := fs.Duration("timeout", 540*time.Second, "max wait")
		fs.Bool("json", false, "output JSON")
		fs.Parse(args[1:])
		task, err = store.Wait(id, *timeout, time.Second)
	case "rework":
		task, err = store.RequestRework(needID(), orchestraStdin())
	case "resume":
		task, err = store.Resume(needID())
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
	orchestraPrint(orchestraTaskJSON(task))
}

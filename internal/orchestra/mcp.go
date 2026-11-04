package orchestra

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func schema(required []string, fields map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": fields, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func mcpTools() []mcpTool {
	str := map[string]any{"type": "string", "minLength": 1}
	checks := map[string]any{"type": "array", "items": str, "maxItems": 10}
	scope := map[string]any{"type": "array", "items": str}
	empty := func() map[string]any { return schema(nil, map[string]any{}) }
	id := func() map[string]any { return schema([]string{"id"}, map[string]any{"id": str}) }
	return []mcpTool{
		{"list_agents", "列出执行代理 / List agents", empty()},
		{"runner_status", "查看执行器状态 / Check runner status", empty()},
		{"get_runner_status", "查看执行器额度暂停状态", empty()},
		{"resume_task", "继续额度耗尽的原会话", id()},
		{"dispatch_task", "提交任务卡 / Dispatch a task", schema([]string{"id", "card"}, map[string]any{"project": str, "executor": str, "folder": str, "id": str, "card": str, "checks": checks, "scope": scope})},
		{"get_task", "读取任务 / Get a task", id()},
		{"list_tasks", "列出任务 / List tasks", schema(nil, map[string]any{"status": map[string]any{"type": "string", "enum": []string{"queued", "running", "reviewing", "receipt", "failed", "quota_exhausted", "cancelled", "accepted"}}, "project": str})},
		{"wait_task", "等待任务 / Wait for a task", schema([]string{"id"}, map[string]any{"id": str, "timeout_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 540, "default": 300}})},
		{"rework_task", "请求返工 / Request rework", schema([]string{"id", "text"}, map[string]any{"id": str, "text": str})},
		{"cancel_task", "取消任务 / Cancel a task", id()},
		{"accept_task", "验收任务 / Accept a task", id()},
		{"list_projects", "列出项目 / List projects", schema(nil, map[string]any{"include_archived": map[string]any{"type": "boolean", "default": false}, "mine": map[string]any{"type": "boolean", "default": false}})},
		{"get_project", "读取项目 / Get a project", id()},
		{"create_project", "创建项目 / Create a project", schema([]string{"id", "name", "folder", "executor"}, map[string]any{"id": str, "name": str, "folder": str, "executor": str, "lead": str, "reviewer": str, "checks": checks, "scope": scope, "sync_agents_md": map[string]any{"type": "boolean"}})},
		{"update_project", "更新项目 / Update a project", schema([]string{"id"}, map[string]any{"id": str, "name": str, "folder": str, "executor": str, "lead": map[string]any{"type": "string"}, "reviewer": map[string]any{"type": "string"}, "checks": checks, "scope": scope, "sync_agents_md": map[string]any{"type": "boolean"}})},
	}
}

type projectView struct {
	Project
	ContainsCWD bool `json:"contains_cwd"`
}

func leadInstructions(lead string, projects []*Project, cwd string, runnerActive bool) string {
	lines := []string{
		"本服务向执行代理派发任务卡：通过 dispatch_task 派发，wait_task 等待回执，accept_task 验收或 rework_task 返工。",
	}
	var mine []*Project
	for _, p := range projects {
		if p != nil && !p.Archived && lead != "" && (p.Lead == lead || p.Reviewer == lead) {
			mine = append(mine, p)
		}
	}
	const maxProjects = 15
	for i, p := range mine {
		if i >= maxProjects {
			break
		}
		role := "lead"
		if p.Lead == lead && p.Reviewer == lead {
			role = "lead/reviewer"
		} else if p.Reviewer == lead {
			role = "reviewer"
		}
		rev := p.Reviewer
		if rev == "" {
			rev = "-"
		}
		lines = append(lines, fmt.Sprintf("- %s (%s) role=%s folder=%s executor=%s reviewer=%s", p.ID, p.Name, role, p.Folder, p.Executor, rev))
	}
	if len(mine) > maxProjects {
		lines = append(lines, fmt.Sprintf("…另有 %d 个项目未列出", len(mine)-maxProjects))
	}
	if cur, err := pickProject(projectsForFolder(projects, cwd), lead); cur != nil {
		lines = append(lines, fmt.Sprintf("当前目录属于项目 %s，dispatch_task 可省略 project 和 folder", cur.ID))
		if brief := strings.TrimRight(projectBriefCap(cur, maxLeadRulesBytes), "\n"); brief != "" {
			lines = append(lines, brief)
		}
	} else if err != nil {
		lines = append(lines, err.Error()+"；派发时须指定 project")
	}
	if !runnerActive {
		lines = append(lines, "执行器未运行：任务只会排队，需在小助手『协作』窗口启动执行器")
	}
	return strings.Join(lines, "\n")
}

type taskView struct {
	ID            string        `json:"id"`
	Project       string        `json:"project,omitempty"`
	Status        Status        `json:"status"`
	Executor      string        `json:"executor"`
	Folder        string        `json:"folder"`
	Session       string        `json:"session"`
	Runs          int           `json:"runs"`
	LastReceipt   string        `json:"last_receipt"`
	ReviewOf      string        `json:"review_of,omitempty"`
	ReviewTask    string        `json:"review_task,omitempty"`
	ReviewVerdict string        `json:"review_verdict,omitempty"`
	RunnerActive  *bool         `json:"runner_active,omitempty"`
	Note          string        `json:"note,omitempty"`
	Evidence      *EvidenceView `json:"evidence,omitempty"`
}

func view(t *Task) taskView {
	return taskView{
		ID: t.ID, Project: t.Project, Status: t.Status, Executor: t.Executor, Folder: t.Folder,
		Session: t.Session, Runs: len(t.Runs), LastReceipt: t.LastReceipt(),
		ReviewOf: t.ReviewOf, ReviewTask: t.ReviewTask, ReviewVerdict: t.ReviewVerdict,
		Evidence: Evidence(t),
	}
}

func toolText(value any, err error) map[string]any {
	message := ""
	if err != nil {
		message = err.Error()
	} else {
		b, e := json.Marshal(value)
		if e != nil {
			err = e
			message = e.Error()
		} else {
			message = string(b)
		}
	}
	result := map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}}
	if err != nil {
		result["isError"] = true
	}
	return result
}

func waitTimeout(seconds *int) (time.Duration, error) {
	if seconds == nil {
		return 300 * time.Second, nil
	}
	if *seconds < 0 {
		return 0, errors.New("timeout_seconds 不能为负数")
	}
	if *seconds > 540 {
		return 540 * time.Second, nil
	}
	return time.Duration(*seconds) * time.Second, nil
}

func callMCPTool(ctx context.Context, cfg Config, store Store, lead, cwd, name string, raw json.RawMessage) (any, error) {
	var args struct {
		Project         string   `json:"project"`
		Name            string   `json:"name"`
		Lead            string   `json:"lead"`
		Reviewer        string   `json:"reviewer"`
		IncludeArchived bool     `json:"include_archived"`
		Mine            bool     `json:"mine"`
		Executor        string   `json:"executor"`
		Folder          string   `json:"folder"`
		ID              string   `json:"id"`
		Card            string   `json:"card"`
		Status          string   `json:"status"`
		Text            string   `json:"text"`
		Timeout         *int     `json:"timeout_seconds"`
		Checks          []string `json:"checks"`
		Scope           []string `json:"scope"`
		SyncAgentsMD    bool     `json:"sync_agents_md"`
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if string(raw) == "null" || json.Unmarshal(raw, &args) != nil {
		return nil, errors.New("工具参数必须是有效对象")
	}
	switch name {
	case "list_agents":
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := make([]map[string]any, 0, len(names))
		for _, name := range names {
			a := cfg.Agents[name]
			_, e := a.Resolve(LoginPath(cfg.Dir, false))
			row := map[string]any{"name": name, "label": a.Label, "available": e == nil}
			if e != nil {
				row["reason"] = e.Error()
			}
			rows = append(rows, row)
		}
		return rows, nil
	case "runner_status":
		return map[string]bool{"active": RunnerActive(cfg.Dir)}, nil
	case "get_runner_status":
		return ReadRunnerStatus(cfg.Dir)
	case "create_project":
		projLead := args.Lead
		if projLead == "" {
			projLead = lead
		}
		return store.CreateProject(cfg, Project{
			ID: args.ID, Name: args.Name, Folder: args.Folder, Lead: projLead,
			Executor: args.Executor, Reviewer: args.Reviewer, Checks: args.Checks, Scope: args.Scope,
			SyncAgentsMD: args.SyncAgentsMD,
		})
	case "update_project":
		var patch struct {
			ID           string    `json:"id"`
			Name         *string   `json:"name"`
			Folder       *string   `json:"folder"`
			Lead         *string   `json:"lead"`
			Executor     *string   `json:"executor"`
			Reviewer     *string   `json:"reviewer"`
			Checks       *[]string `json:"checks"`
			Scope        *[]string `json:"scope"`
			SyncAgentsMD *bool     `json:"sync_agents_md"`
		}
		if json.Unmarshal(raw, &patch) != nil {
			return nil, errors.New("工具参数必须是有效对象")
		}
		return store.UpdateProject(cfg, patch.ID, ProjectPatch{
			Name: patch.Name, Folder: patch.Folder, Lead: patch.Lead,
			Executor: patch.Executor, Reviewer: patch.Reviewer,
			Checks: patch.Checks, Scope: patch.Scope, SyncAgentsMD: patch.SyncAgentsMD,
		})
	case "get_project":
		return store.GetProject(args.ID)
	case "list_projects":
		ps, e := store.ListProjects()
		if e != nil {
			return nil, e
		}
		out := []projectView{}
		for _, p := range ps {
			if !args.IncludeArchived && p.Archived {
				continue
			}
			if args.Mine && (lead == "" || (p.Lead != lead && p.Reviewer != lead)) {
				continue
			}
			out = append(out, projectView{Project: *p, ContainsCWD: folderContains(p.Folder, cwd)})
		}
		return out, nil
	case "dispatch_task":
		if args.Project == "" && args.Folder == "" && args.Executor == "" {
			p, e := store.ProjectForFolder(lead, cwd)
			if e != nil {
				return nil, e
			}
			if p == nil {
				return nil, fmt.Errorf("未指定 project 或 executor/folder，且当前目录 %s 不属于任何项目", cwd)
			}
			args.Project = p.ID
		} else if args.Project == "" && (args.Executor == "" || args.Folder == "") {
			return nil, errors.New("缺少 executor 或 folder（或指定 project）")
		}
		t, e := store.DispatchInProject(cfg, lead, args.Project, args.Executor, args.Folder, args.ID, args.Card, args.Checks, args.Scope)
		if e != nil {
			return nil, e
		}
		v := view(t)
		if !RunnerActive(cfg.Dir) {
			active := false
			v.RunnerActive = &active
			v.Note = "执行器未运行，请启动 codex-helper orchestra run / Start the runner"
		}
		return v, nil
	case "list_tasks":
		if args.Status != "" {
			switch Status(args.Status) {
			case Queued, Running, Reviewing, Receipt, Failed, QuotaExhausted, Cancelled, Accepted:
			default:
				return nil, errors.New("任务状态无效")
			}
		}
		ts, e := store.List()
		if e != nil {
			return nil, e
		}
		out := []taskView{}
		for _, t := range ts {
			if (args.Status == "" || string(t.Status) == args.Status) && (args.Project == "" || t.Project == args.Project) {
				out = append(out, view(t))
			}
		}
		return out, nil
	case "get_task", "cancel_task", "accept_task", "rework_task", "resume_task", "wait_task":
		if strings.TrimSpace(args.ID) == "" {
			return nil, errors.New("缺少任务编号")
		}
		var t *Task
		var e error
		switch name {
		case "get_task":
			t, e = store.Get(args.ID)
		case "cancel_task":
			t, e = store.Cancel(args.ID)
		case "accept_task":
			t, e = store.Accept(args.ID)
		case "rework_task":
			t, e = store.RequestRework(args.ID, args.Text)
		case "resume_task":
			t, e = store.Resume(args.ID)
		case "wait_task":
			d, e2 := waitTimeout(args.Timeout)
			if e2 != nil {
				return nil, e2
			}
			deadline := time.NewTimer(d)
			defer deadline.Stop()
			for {
				t, e = store.Get(args.ID)
				if e != nil || store.needsLead(t) {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-deadline.C:
					return view(t), nil
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if e != nil {
			return nil, e
		}
		return view(t), nil
	default:
		return nil, fmt.Errorf("未知工具：%s", name)
	}
}

// ServeMCP serves newline-delimited JSON-RPC over stdio. Tool calls run
// concurrently, so a long wait_task doesn't hold up other calls, and
// notifications/cancelled stops the matching call.
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, cfg Config, store Store, lead, cwd string) error {
	capturedCWD := captureCWD(cwd)
	// Bind the snapshot to the actual process cwd, even if its path was
	// replaced between Getwd and captureCWD.
	dev, ino, err := folderID(".")
	if err != nil || dev != capturedCWD.dev || ino != capturedCWD.ino {
		capturedCWD = cwdSnapshot{}
	}
	return serveMCP(ctx, in, out, cfg, store, lead, capturedCWD)
}

func serveMCP(ctx context.Context, in io.Reader, out io.Writer, cfg Config, store Store, lead string, capturedCWD cwdSnapshot) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	var mu sync.Mutex
	var calls sync.WaitGroup
	inflight := map[string]context.CancelFunc{}
	defer func() {
		mu.Lock()
		for _, cancel := range inflight {
			cancel()
		}
		mu.Unlock()
		calls.Wait()
	}()
	reply := func(id json.RawMessage, result any, e *mcpError) error {
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if e != nil {
			msg["error"] = e
		} else {
			msg["result"] = result
		}
		mu.Lock()
		defer mu.Unlock()
		return enc.Encode(msg)
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var req mcpRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if e := reply(nil, nil, &mcpError{-32700, "Parse error"}); e != nil {
				return e
			}
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			if e := reply(req.ID, nil, &mcpError{-32600, "Invalid Request"}); e != nil {
				return e
			}
			continue
		}
		if len(req.ID) == 0 {
			if req.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &p) == nil {
					mu.Lock()
					if cancel := inflight[string(p.RequestID)]; cancel != nil {
						cancel()
					}
					mu.Unlock()
				}
			}
			continue
		}
		var result any
		var rpcErr *mcpError
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &p) != nil || p.ProtocolVersion == "" {
				rpcErr = &mcpError{-32602, "Invalid params"}
			} else {
				ps, _ := store.ListProjects()
				result = map[string]any{
					"protocolVersion": p.ProtocolVersion,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "codex-helper-orchestra", "version": "0.1.0"},
					"instructions":    leadInstructions(lead, ps, capturedCWD.validPath(), RunnerActive(cfg.Dir)),
				}
			}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": mcpTools()}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
				result = toolText(nil, errors.New("缺少工具名称"))
				break
			}
			callCtx, cancel := context.WithCancel(ctx)
			key := string(req.ID)
			mu.Lock()
			inflight[key] = cancel
			mu.Unlock()
			calls.Add(1)
			go func(id json.RawMessage) {
				defer calls.Done()
				v, e := callMCPTool(callCtx, cfg, store, lead, capturedCWD.validPath(), p.Name, p.Arguments)
				mu.Lock()
				delete(inflight, key)
				mu.Unlock()
				cancelled := callCtx.Err() != nil
				cancel()
				if cancelled {
					return // cancelled by the client (MCP says not to answer) or by shutdown
				}
				reply(id, toolText(v, e), nil)
			}(req.ID)
			continue
		default:
			rpcErr = &mcpError{-32601, "Method not found"}
		}
		if err := reply(req.ID, result, rpcErr); err != nil {
			return err
		}
	}
	return scanner.Err()
}

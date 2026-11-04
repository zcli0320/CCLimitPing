package orchestra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeadInstructionsContentAndCap(t *testing.T) {
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	if err := os.MkdirAll(filepath.Join(alpha, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	projects := []*Project{
		{ID: "p-lead", Name: "Alpha", Folder: alpha, Lead: "claude", Executor: "codex", Reviewer: "antigravity"},
		{ID: "p-rev", Name: "Beta", Folder: "/work/beta", Lead: "codex", Executor: "codex", Reviewer: "claude"},
		{ID: "p-other", Name: "Other", Folder: "/work/other", Lead: "codex", Executor: "codex"},
		{ID: "p-arch", Name: "Arch", Folder: "/work/arch", Lead: "claude", Executor: "codex", Archived: true},
	}
	text := leadInstructions("claude", projects, filepath.Join(alpha, "sub"), false)
	for _, want := range []string{
		"dispatch_task", "wait_task", "accept_task", "rework_task",
		"p-lead", "Alpha", alpha, "codex", "antigravity", "role=lead",
		"p-rev", "Beta", "/work/beta", "role=reviewer",
		"当前目录属于项目 p-lead，dispatch_task 可省略 project 和 folder",
		"执行器未运行：任务只会排队，需在小助手『协作』窗口启动执行器",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	for _, unwant := range []string{"p-other", "p-arch"} {
		if strings.Contains(text, unwant) {
			t.Fatalf("unexpected %q in:\n%s", unwant, text)
		}
	}
	activeText := leadInstructions("claude", projects, "/outside", true)
	if strings.Contains(activeText, "执行器未运行") || strings.Contains(activeText, "当前目录属于项目") {
		t.Fatalf("unexpected lines in:\n%s", activeText)
	}

	var many []*Project
	for i := 1; i <= 18; i++ {
		many = append(many, &Project{
			ID: fmt.Sprintf("P%02d", i), Name: fmt.Sprintf("Proj %d", i),
			Folder: fmt.Sprintf("/work/p%d", i), Lead: "claude", Executor: "codex",
		})
	}
	capped := leadInstructions("claude", many, "", true)
	if !strings.Contains(capped, "P15") || strings.Contains(capped, "P16") || !strings.Contains(capped, "3") {
		t.Fatalf("cap failed:\n%s", capped)
	}
	if lines := strings.Split(capped, "\n"); len(lines) > 40 {
		t.Fatalf("too many lines (%d):\n%s", len(lines), capped)
	}
}

func TestProjectForFolder(t *testing.T) {
	e := setup(t)
	root := filepath.Dir(e.folder)
	dirA := filepath.Join(root, "a")
	dirAB := filepath.Join(dirA, "b")
	dirABC := filepath.Join(dirAB, "c")
	dirABc := filepath.Join(dirA, "bc")
	dirArch := filepath.Join(root, "arch")
	for _, d := range []string{dirABC, dirABc, dirArch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []Project{
		{ID: "PA", Name: "A", Folder: dirA, Executor: "codex"},
		{ID: "PAB", Name: "AB", Folder: dirAB, Executor: "codex"},
		{ID: "PARCH", Name: "Arch", Folder: dirArch, Executor: "codex"},
	} {
		if _, err := e.store.CreateProject(e.cfg, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.ArchiveProject("PARCH"); err != nil {
		t.Fatal(err)
	}
	linkABC := filepath.Join(t.TempDir(), "link-abc")
	if err := os.Symlink(dirABC, linkABC); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir  string
		want string
	}{
		{dirA, "PA"},
		{dirAB, "PAB"},
		{dirABC, "PAB"},
		{linkABC, "PAB"},
		{dirABc, "PA"},
		{dirArch, ""},
		{t.TempDir(), ""},
	} {
		got, err := e.store.ProjectForFolder("claude", tc.dir)
		if err != nil {
			t.Fatalf("ProjectForFolder(%s): %v", tc.dir, err)
		}
		id := ""
		if got != nil {
			id = got.ID
		}
		if id != tc.want {
			t.Fatalf("ProjectForFolder(%s) = %q, want %q", tc.dir, id, tc.want)
		}
	}
	// Two projects on one folder (seen in real use): the lead's own project
	// wins; with no way to choose, inference refuses instead of guessing.
	for _, p := range []Project{
		{ID: "PAB2", Name: "AB2", Folder: dirAB, Executor: "codex", Lead: "codex"},
	} {
		if _, err := e.store.CreateProject(e.cfg, p); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := e.store.ProjectForFolder("codex", dirABC); err != nil || got == nil || got.ID != "PAB2" {
		t.Fatalf("lead's project should win, got %v %v", got, err)
	}
	if got, err := e.store.ProjectForFolder("someone", dirABC); err == nil || got != nil || !strings.Contains(err.Error(), "多个项目") {
		t.Fatalf("shared folder without a lead match must fail, got %v %v", got, err)
	}
	instr := leadInstructions("someone", mustProjects(t, e.store), dirABC, true)
	if !strings.Contains(instr, "多个项目") || strings.Contains(instr, "当前目录属于项目") {
		t.Fatalf("instructions must flag the ambiguity: %s", instr)
	}
}

func mustProjects(t *testing.T, s Store) []*Project {
	t.Helper()
	ps, err := s.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestMCPRejectsUnavailableOrReplacedCWD(t *testing.T) {
	for _, mode := range []string{"missing-symlink", "replaced-directory"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			if _, err := e.store.CreateProject(e.cfg, Project{ID: "P1", Name: "One", Folder: e.folder, Lead: "claude", Executor: "codex"}); err != nil {
				t.Fatal(err)
			}
			cwd := filepath.Join(e.folder, "cwd")
			if mode == "missing-symlink" {
				if err := os.Symlink(t.TempDir(), cwd); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			f := startMCPAt(t, e, cwd)
			f.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
			f.receive(t) // ensures the server captured the original directory
			if err := os.Rename(cwd, cwd+"-old"); err != nil {
				t.Fatal(err)
			}
			if mode == "replaced-directory" {
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, failed := resultText(t, mcpCall(t, f, 2, "dispatch_task", map[string]any{"id": "T-INFER", "card": "TASK"}))
			if !failed {
				t.Fatal("inference accepted unavailable or replaced cwd")
			}
			_, failed = resultText(t, mcpCall(t, f, 3, "dispatch_task", map[string]any{"project": "P1", "id": "T-EXPLICIT", "card": "TASK"}))
			if failed {
				t.Fatal("explicit project dispatch should remain available")
			}
		})
	}
}

func TestServeMCPRejectsCWDNotMatchingProcess(t *testing.T) {
	e := setup(t)
	if _, err := e.store.CreateProject(e.cfg, Project{ID: "P1", Name: "One", Folder: e.folder, Lead: "claude", Executor: "codex"}); err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\"}}\n")
	var output bytes.Buffer
	if err := ServeMCP(context.Background(), input, &output, e.cfg, e.store, "claude", e.folder); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "当前目录属于项目") {
		t.Fatalf("server trusted a path different from its cwd: %s", output.String())
	}
}

func TestMCPDispatchInferenceListProjectsMineAndInitializeInstructions(t *testing.T) {
	e := setup(t)
	sub := filepath.Join(e.folder, "sub", "pkg")
	otherDir := filepath.Join(filepath.Dir(e.folder), "other")
	outsideDir := filepath.Join(filepath.Dir(e.folder), "outside")
	for _, d := range []string{sub, otherDir, outsideDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "One", Folder: e.folder, Lead: "claude", Executor: "codex", Reviewer: "antigravity",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P2", Name: "Two", Folder: otherDir, Lead: "codex", Executor: "codex",
	}); err != nil {
		t.Fatal(err)
	}

	// dispatch_task inference: cwd inside project -> task.Project set, folder = project folder, runner_active = false.
	raw, err := callMCPTool(context.Background(), e.cfg, e.store, "claude", sub, "dispatch_task", json.RawMessage(`{"id":"T-INFER","card":"TASK T-INFER"}`))
	if err != nil {
		t.Fatalf("inferred dispatch: %v", err)
	}
	tv := raw.(taskView)
	if tv.Project != "P1" || tv.Folder != realFolder(t, e.folder) || tv.RunnerActive == nil || *tv.RunnerActive != false {
		t.Fatalf("inferred taskView=%+v", tv)
	}
	if _, err := callMCPTool(context.Background(), e.cfg, e.store, "claude", outsideDir, "dispatch_task", json.RawMessage(`{"id":"T-OUT","card":"TASK"}`)); err == nil || !strings.Contains(err.Error(), "不属于任何项目") {
		t.Fatalf("outside cwd dispatch: %v", err)
	}

	// list_projects mine and contains_cwd.
	raw, err = callMCPTool(context.Background(), e.cfg, e.store, "claude", sub, "list_projects", json.RawMessage(`{"mine":true}`))
	if err != nil {
		t.Fatal(err)
	}
	mine := raw.([]projectView)
	if len(mine) != 1 || mine[0].ID != "P1" || !mine[0].ContainsCWD {
		t.Fatalf("mine projects=%+v", mine)
	}
	raw, err = callMCPTool(context.Background(), e.cfg, e.store, "claude", sub, "list_projects", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	all := raw.([]projectView)
	if len(all) != 2 || !all[0].ContainsCWD || all[1].ContainsCWD {
		t.Fatalf("all projects=%+v", all)
	}

	// initialize result contains "instructions".
	f := startMCP(t, e)
	f.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	r := f.receive(t)
	inst, ok := r["result"].(map[string]any)["instructions"].(string)
	if !ok || !strings.Contains(inst, "P1") || !strings.Contains(inst, "执行器未运行") {
		t.Fatalf("initialize instructions=%q", inst)
	}
}

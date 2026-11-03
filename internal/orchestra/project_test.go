package orchestra

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectCRUDAndArchive(t *testing.T) {
	e := setup(t)
	ps, err := e.store.ListProjects()
	if err != nil || ps != nil {
		t.Fatalf("empty list: %v %v", ps, err)
	}
	p, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "  My Project  ", Folder: e.folder, Lead: "claude",
		Executor: "codex", Reviewer: "claude", Checks: []string{"true"}, Scope: []string{"src/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "My Project" || p.FolderDev == 0 || p.Created.IsZero() || p.Updated.IsZero() {
		t.Fatalf("created=%+v", p)
	}
	got, err := e.store.GetProject("P1")
	if err != nil || got.Name != "My Project" || got.Reviewer != "claude" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	// Corrupt file skipped by ListProjects.
	if err := os.WriteFile(filepath.Join(e.store.Dir, "projects", "corrupt.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err = e.store.ListProjects()
	if err != nil || len(ps) != 1 || ps[0].ID != "P1" {
		t.Fatalf("list=%+v err=%v", ps, err)
	}
	arch, err := e.store.ArchiveProject("P1")
	if err != nil || !arch.Archived {
		t.Fatalf("archive=%+v err=%v", arch, err)
	}
	got, err = e.store.GetProject("P1")
	if err != nil || !got.Archived {
		t.Fatalf("after archive=%+v err=%v", got, err)
	}
}

func TestProjectValidationErrors(t *testing.T) {
	e := setup(t)
	base := Project{ID: "P1", Name: "Valid", Folder: e.folder, Executor: "codex"}
	cases := []struct {
		mod  func(*Project)
		want string
	}{
		{func(p *Project) { p.ID = "bad id" }, "项目编号无效"},
		{func(p *Project) { p.Name = "   " }, "项目名称无效"},
		{func(p *Project) { p.Name = strings.Repeat("字", 101) }, "项目名称无效"},
		{func(p *Project) { p.Executor = "nobody" }, "未知的执行代理"},
		{func(p *Project) { p.Reviewer = "nobody" }, "未知的审查代理"},
		{func(p *Project) { p.Lead = "bad lead" }, "负责代理无效"},
		{func(p *Project) { p.Scope = []string{"../escape"} }, "允许改动范围"},
	}
	for _, tc := range cases {
		p := base
		tc.mod(&p)
		if _, err := e.store.CreateProject(e.cfg, p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CreateProject(%+v): err=%v, want %q", p, err, tc.want)
		}
	}
	if _, err := e.store.CreateProject(e.cfg, base); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateProject(e.cfg, base); err == nil || !strings.Contains(err.Error(), "项目编号 P1 已存在") {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := e.store.GetProject("bad id"); err == nil || !strings.Contains(err.Error(), "项目编号无效") {
		t.Fatalf("bad get id: %v", err)
	}
	if _, err := e.store.GetProject("P404"); err == nil || !strings.Contains(err.Error(), "没有编号为 P404 的项目") {
		t.Fatalf("missing get: %v", err)
	}
}

func TestDispatchInProject(t *testing.T) {
	e := setup(t)
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj", Folder: e.folder, Lead: "claude",
		Executor: "codex", Checks: []string{"go test ./..."}, Scope: []string{"internal/"},
	}); err != nil {
		t.Fatal(err)
	}

	// Inherit defaults from project.
	t1, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Project != "P1" || t1.Lead != "claude" || t1.Executor != "codex" || t1.Folder != realFolder(t, e.folder) ||
		!reflect.DeepEqual(t1.Checks, []string{"go test ./..."}) || !reflect.DeepEqual(t1.Scope, []string{"internal/"}) {
		t.Fatalf("inherited task=%+v", t1)
	}

	// Explicit checks override project checks.
	t2, err := e.store.DispatchInProject(e.cfg, "codex", "P1", "claude", e.folder, "T2", "TASK T2", []string{"true"}, []string{"cmd/"})
	if err != nil {
		t.Fatal(err)
	}
	if t2.Lead != "codex" || t2.Executor != "claude" || !reflect.DeepEqual(t2.Checks, []string{"true"}) || !reflect.DeepEqual(t2.Scope, []string{"cmd/"}) {
		t.Fatalf("override task=%+v", t2)
	}

	// Mismatched folder rejected.
	other := filepath.Join(filepath.Dir(e.folder), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", other, "T3", "TASK T3", nil, nil); err == nil || !strings.Contains(err.Error(), "工作目录与项目 P1 不一致") {
		t.Fatalf("mismatched folder: %v", err)
	}

	// Replaced project folder rejected.
	moved := e.folder + "-moved"
	if err := os.Rename(e.folder, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T3b", "TASK", nil, nil); err == nil || !strings.Contains(err.Error(), "工作目录已变化") {
		t.Fatalf("replaced folder: %v", err)
	}
	if err := os.RemoveAll(e.folder); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, e.folder); err != nil {
		t.Fatal(err)
	}

	// Archived project rejected.
	if _, err := e.store.ArchiveProject("P1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T4", "TASK T4", nil, nil); err == nil || !strings.Contains(err.Error(), "项目 P1 已归档") {
		t.Fatalf("archived dispatch: %v", err)
	}

	// project == "" path unchanged.
	t5, err := e.store.DispatchInProject(e.cfg, "claude", "", "codex", e.folder, "T5", "TASK T5", nil, nil)
	if err != nil || t5.Project != "" || t5.Executor != "codex" {
		t.Fatalf("no-project task=%+v err=%v", t5, err)
	}
}

func TestAssignProject(t *testing.T) {
	e := setup(t)
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Proj 1", Folder: e.folder, Lead: "claude",
		Executor: "codex", Checks: []string{"true"}, Scope: []string{"src/"},
	}); err != nil {
		t.Fatal(err)
	}
	t1, err := e.store.Dispatch(e.cfg, "", "codex", e.folder, "T-UNASSIGNED", "TASK 1")
	if err != nil {
		t.Fatal(err)
	}
	if t1.Project != "" {
		t.Fatalf("expected unassigned task, got %+v", t1)
	}
	assigned, err := e.store.AssignProject("T-UNASSIGNED", "P1")
	if err != nil {
		t.Fatal(err)
	}
	if assigned.Project != "P1" || assigned.Lead != "claude" || !reflect.DeepEqual(assigned.Checks, []string{"true"}) || !reflect.DeepEqual(assigned.Scope, []string{"src/"}) {
		t.Fatalf("assigned=%+v", assigned)
	}
	unassigned, err := e.store.AssignProject("T-UNASSIGNED", "")
	if err != nil || unassigned.Project != "" {
		t.Fatalf("unassign=%+v err=%v", unassigned, err)
	}
	if _, err := e.store.ArchiveProject("P1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AssignProject("T-UNASSIGNED", "P1"); err == nil || !strings.Contains(err.Error(), "已归档") {
		t.Fatalf("assign to archived project should fail: %v", err)
	}
}

func realFolder(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

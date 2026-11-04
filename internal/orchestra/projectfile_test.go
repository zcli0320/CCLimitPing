package orchestra

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func strPtr(s string) *string       { return &s }
func boolPtr(b bool) *bool          { return &b }
func slicePtr(s []string) *[]string { return &s }

func TestUpdateProjectFieldsAndRefusals(t *testing.T) {
	e := setup(t)
	p, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Initial", Folder: e.folder, Lead: "claude",
		Executor: "codex", Reviewer: "claude", Checks: []string{"true"}, Scope: []string{"src/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	otherFolder := filepath.Join(filepath.Dir(e.folder), "proj2")
	if err := os.MkdirAll(otherFolder, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err = e.store.UpdateProject(e.cfg, "P1", ProjectPatch{
		Name:         strPtr("Renamed"),
		Lead:         strPtr("codex"),
		Executor:     strPtr("claude"),
		Reviewer:     strPtr(""),
		Checks:       slicePtr([]string{"go test ./..."}),
		Scope:        slicePtr([]string{"internal/", "cmd/"}),
		SyncAgentsMD: boolPtr(true),
		Folder:       strPtr(otherFolder),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Renamed" || p.Lead != "codex" || p.Executor != "claude" || p.Reviewer != "" ||
		!reflect.DeepEqual(p.Checks, []string{"go test ./..."}) ||
		!reflect.DeepEqual(p.Scope, []string{"internal/", "cmd/"}) ||
		!p.SyncAgentsMD || p.Folder != realFolder(t, otherFolder) || p.FolderDev == 0 || p.FolderIno == 0 {
		t.Fatalf("updated project=%+v", p)
	}
	// Clear checks and scope.
	p, err = e.store.UpdateProject(e.cfg, "P1", ProjectPatch{
		Checks: slicePtr([]string{}),
		Scope:  slicePtr([]string{}),
	})
	if err != nil || len(p.Checks) != 0 || len(p.Scope) != 0 {
		t.Fatalf("clear checks/scope: p=%+v err=%v", p, err)
	}

	// Queued task refuses folder change.
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T-QUEUED", "TASK", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{Folder: strPtr(e.folder)}); err == nil || !strings.Contains(err.Error(), "不能修改工作目录") {
		t.Fatalf("expected folder change refusal with queued task, got %v", err)
	}
	// Non-folder update still allowed while task is queued.
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{Name: strPtr("Still Editable")}); err != nil {
		t.Fatalf("non-folder update with queued task failed: %v", err)
	}
	if _, err := e.store.Cancel("T-QUEUED"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{Folder: strPtr(e.folder)}); err != nil {
		t.Fatalf("folder change after task cancelled failed: %v", err)
	}

	// Archived project refuses updates.
	if _, err := e.store.ArchiveProject("P1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{Name: strPtr("Nope")}); err == nil || !strings.Contains(err.Error(), "已归档") {
		t.Fatalf("expected archived refusal, got %v", err)
	}
}

func TestProjectRulesFileFrontMatterAndBodyPreserved(t *testing.T) {
	e := setup(t)
	p, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Alpha", Folder: e.folder, Lead: "claude",
		Executor: "codex", Reviewer: "claude", Checks: []string{"true"}, Scope: []string{"src/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := RulesPath(p)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# 由 Codex 小助手生成：角色请在小助手中修改，本段会被覆盖",
		"project: P1\n",
		"name: Alpha\n",
		"lead: claude\n",
		"executor: codex\n",
		"reviewer: claude\n",
		`checks: ["true"]`,
		`scope: ["src/"]`,
		defaultProjectRulesBody,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %q in:\n%s", want, raw)
		}
	}

	// User edits the body with custom bytes (including blank lines and no trailing newline).
	customBody := "\n## 团队规则\n\n1. 保持提交整洁\n2. 不要跳过测试\n--- 内嵌分隔线 ---\n末尾无换行"
	if err := os.WriteFile(path, []byte(projectFrontMatter(p)+customBody), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = e.store.UpdateProject(e.cfg, "P1", ProjectPatch{
		Name:     strPtr("Beta"),
		Lead:     strPtr(""),
		Reviewer: strPtr(""),
		Checks:   slicePtr([]string{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: Beta\n", "lead: 不指定\n", "reviewer: 无\n", "checks: []\n"} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("regenerated front matter missing %q:\n%s", want, after)
		}
	}
	if gotBody := extractProjectBody(after); gotBody != customBody {
		t.Fatalf("body not preserved byte-for-byte:\ngot:  %q\nwant: %q", gotBody, customBody)
	}

	// Cap at 8 KiB in ReadProjectRules and 2 KiB in leadInstructions.
	bigBody := strings.Repeat("规则内容ABCD", 1024) // > 8 KiB
	if err := os.WriteFile(path, []byte(projectFrontMatter(p)+bigBody), 0o600); err != nil {
		t.Fatal(err)
	}
	capped := ReadProjectRules(p)
	if !strings.HasSuffix(capped, "\n(truncated)") || len(capped) > maxProjectRulesBytes+32 {
		t.Fatalf("ReadProjectRules cap failed: len=%d suffix=%q", len(capped), capped[len(capped)-20:])
	}
	instr := leadInstructions("claude", []*Project{p}, p.Folder, true)
	if !strings.Contains(instr, "--- project P1 ---") || !strings.Contains(instr, "(truncated)") || len(instr) > 3000 {
		t.Fatalf("leadInstructions brief/cap failed: len=%d\n%s", len(instr), instr)
	}
}

func TestProjectRulesEditedInFileShowsInNextExecutorPrompt(t *testing.T) {
	e := setupReviewEnv(t)
	t.Setenv("REVIEW_VERDICT", "VERDICT: PASS")
	p, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "Alpha", Folder: e.folder, Lead: "lead", Executor: "codex", Reviewer: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, e)
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T1", "TASK T1", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitDone(t, e, "T1")

	// Edit the rules body on disk directly without touching the store/helper.
	editedRule := "## 项目规则\n\n- 必须包含标记 RULE_EDIT_LIVE_777\n"
	if err := os.WriteFile(RulesPath(p), []byte(projectFrontMatter(p)+editedRule), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DispatchInProject(e.cfg, "", "P1", "", "", "T2", "TASK T2", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitDone(t, e, "T2")

	prompts, err := os.ReadFile(e.args + ".prompts")
	if err != nil {
		t.Fatal(err)
	}
	pStr := string(prompts)
	idxBrief := strings.LastIndex(pStr, "--- project P1 ---")
	idxRule := strings.LastIndex(pStr, "RULE_EDIT_LIVE_777")
	idxCard := strings.LastIndex(pStr, "--- card ---\nTASK T2")
	if idxBrief < 0 || idxRule < idxBrief || idxCard < idxRule {
		t.Fatalf("edited rule not injected before card in executor prompt:\n%s", pStr)
	}
	revPrompt, err := os.ReadFile(e.args + ".reviewer_prompt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(revPrompt), "--- project P1 ---") || !strings.Contains(string(revPrompt), "RULE_EDIT_LIVE_777") {
		t.Fatalf("reviewer prompt missing project brief:\n%s", revPrompt)
	}
}

func TestSyncAgentsMDAddReplaceRemoveAndTwoProjects(t *testing.T) {
	e := setup(t)
	agentsPath := filepath.Join(e.folder, "AGENTS.md")
	initial := "# Existing Header\nDo not touch this line.\n\n# Existing Footer\nKeep intact.\n"
	if err := os.WriteFile(agentsPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default SyncAgentsMD=false does not touch AGENTS.md.
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P1", Name: "One", Folder: e.folder, Lead: "claude", Executor: "codex",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(agentsPath)
	if string(got) != initial {
		t.Fatalf("AGENTS.md modified when SyncAgentsMD=false:\n%s", got)
	}

	// Enable SyncAgentsMD on P1 and create P2 with SyncAgentsMD=true in the same folder.
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{SyncAgentsMD: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateProject(e.cfg, Project{
		ID: "P2", Name: "Two", Folder: e.folder, Lead: "codex", Executor: "claude", Reviewer: "codex", SyncAgentsMD: true,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agentsPath)
	text := string(got)
	for _, want := range []string{
		initial,
		"<!-- codex-helper:project P1 begin -->",
		"规则见 .codex-helper/projects/P1.md",
		"<!-- codex-helper:project P1 end -->",
		"<!-- codex-helper:project P2 begin -->",
		"规则见 .codex-helper/projects/P2.md",
		"<!-- codex-helper:project P2 end -->",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in AGENTS.md:\n%s", want, text)
		}
	}

	// Replace P1 block while keeping surrounding text and P2 block intact.
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{Name: strPtr("One Updated"), Reviewer: strPtr("claude")}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agentsPath)
	text = string(got)
	if !strings.Contains(text, "One Updated (P1)") || !strings.Contains(text, "审查=claude") ||
		!strings.Contains(text, "Two (P2)") || !strings.HasPrefix(text, initial) {
		t.Fatalf("replace P1 failed:\n%s", text)
	}

	// Turn SyncAgentsMD false on P1 -> removes P1 block, keeps P2 block and surrounding text.
	if _, err := e.store.UpdateProject(e.cfg, "P1", ProjectPatch{SyncAgentsMD: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agentsPath)
	text = string(got)
	if strings.Contains(text, "project P1") || !strings.Contains(text, "<!-- codex-helper:project P2 begin -->") || !strings.HasPrefix(text, initial) {
		t.Fatalf("remove P1 failed:\n%s", text)
	}

	// Archive P2 -> removes P2 block, keeps P2 rules file, restores AGENTS.md to exact initial content.
	p2, err := e.store.ArchiveProject("P2")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agentsPath)
	if string(got) != initial {
		t.Fatalf("after removing both blocks, AGENTS.md=%q, want %q", string(got), initial)
	}
	if _, err := os.Stat(RulesPath(p2)); err != nil {
		t.Fatalf("archive should keep rules file: %v", err)
	}
}

func TestRulesEdgeCasesCRLFNoFrontMatterFolderChangeAndArchive(t *testing.T) {
	e := setup(t)
	if got := RulesPath(&Project{ID: "../escape", Folder: e.folder}); got != "" {
		t.Fatalf("RulesPath allowed path traversal ID: %q", got)
	}
	if got := RulesPath(&Project{ID: "P1", Folder: "relative/path"}); got != "" {
		t.Fatalf("RulesPath allowed relative folder: %q", got)
	}

	p, err := e.store.CreateProject(e.cfg, Project{
		ID: "P-EDGE", Name: "Line1\nLine2", Folder: e.folder, Lead: "claude", Executor: "codex", SyncAgentsMD: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := RulesPath(p)

	// 1. CRLF front matter edited by hand preserves exact body without duplicating front matter.
	crlfFile := "---\r\nproject: P-EDGE\r\nname: Old\r\n---\r\n## CRLF Body\r\nLine A\r\n"
	if err := os.WriteFile(path, []byte(crlfFile), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = e.store.UpdateProject(e.cfg, "P-EDGE", ProjectPatch{Name: strPtr("Clean Name")})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody := extractProjectBody(mustReadFile(t, path)); gotBody != "## CRLF Body\r\nLine A\r\n" {
		t.Fatalf("CRLF body lost or duplicated: %q", gotBody)
	}

	// 2. File without front matter preserves all existing content as body.
	rawNoFM := "Custom rules without front matter\n- Rule 1"
	if err := os.WriteFile(path, []byte(rawNoFM), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = e.store.UpdateProject(e.cfg, "P-EDGE", ProjectPatch{Reviewer: strPtr("claude")})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody := extractProjectBody(mustReadFile(t, path)); gotBody != rawNoFM {
		t.Fatalf("no-front-matter body not preserved: %q", gotBody)
	}

	// 3. Moving project to a new folder carries over the edited rules body and removes old AGENTS.md block.
	newFolder := filepath.Join(filepath.Dir(e.folder), "moved-folder")
	if err := os.MkdirAll(newFolder, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err = e.store.UpdateProject(e.cfg, "P-EDGE", ProjectPatch{Folder: strPtr(newFolder)})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody := extractProjectBody(mustReadFile(t, RulesPath(p))); gotBody != rawNoFM {
		t.Fatalf("rules body not carried over to new folder: %q", gotBody)
	}
	oldAgents, _ := os.ReadFile(filepath.Join(e.folder, "AGENTS.md"))
	if strings.Contains(string(oldAgents), "P-EDGE") {
		t.Fatalf("old AGENTS.md still contains moved project block:\n%s", oldAgents)
	}

	// 4. AGENTS.md with CRLF around managed block and text without trailing newline.
	agentsPath := filepath.Join(p.Folder, "AGENTS.md")
	crlfAgents := "Header line\r\n<!-- codex-helper:project P-EDGE begin -->\r\nold\r\n<!-- codex-helper:project P-EDGE end -->\r\nFooter line"
	if err := os.WriteFile(agentsPath, []byte(crlfAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UpdateProject(e.cfg, "P-EDGE", ProjectPatch{SyncAgentsMD: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, agentsPath)); got != "Header line\r\nFooter line" {
		t.Fatalf("CRLF AGENTS.md removal left artifacts: %q", got)
	}

	// 5. Archive does not recreate a removed rules file or rewrite an existing rules file.
	customArchived := projectFrontMatter(p) + "DO NOT TOUCH ON ARCHIVE\n"
	if err := os.WriteFile(RulesPath(p), []byte(customArchived), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ArchiveProject("P-EDGE"); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadFile(t, RulesPath(p))); got != customArchived {
		t.Fatalf("ArchiveProject modified rules file:\ngot:  %q\nwant: %q", got, customArchived)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

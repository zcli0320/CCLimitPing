package orchestra

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Project struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Folder       string    `json:"folder"`
	FolderDev    uint64    `json:"folder_dev,omitempty"`
	FolderIno    uint64    `json:"folder_ino,omitempty"`
	Lead         string    `json:"lead,omitempty"`
	Executor     string    `json:"executor"`
	Reviewer     string    `json:"reviewer,omitempty"` // stored only; no review flow yet
	Checks       []string  `json:"checks,omitempty"`
	Scope        []string  `json:"scope,omitempty"`
	SyncAgentsMD bool      `json:"sync_agents_md,omitempty"`
	Archived     bool      `json:"archived,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	Warning      string    `json:"warning,omitempty"`
}

type ProjectPatch struct {
	Name         *string
	Folder       *string
	Lead         *string
	Executor     *string
	Reviewer     *string
	Checks       *[]string
	Scope        *[]string
	SyncAgentsMD *bool
}

func (s Store) projectPath(id string) string { return filepath.Join(s.Dir, "projects", id+".json") }

func (s Store) readProject(id string) (*Project, error) {
	data, err := os.ReadFile(s.projectPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("没有编号为 %s 的项目", id)
	}
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("项目 %s 的记录已损坏", id)
	}
	return &p, nil
}

func (s Store) writeProject(p *Project) error {
	if err := os.MkdirAll(filepath.Join(s.Dir, "projects"), 0o700); err != nil {
		return err
	}
	p.Updated = time.Now()
	p.Warning = ""
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.projectPath(p.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.projectPath(p.ID))
}

func validateProject(cfg Config, p *Project) error {
	if !taskID.MatchString(p.ID) {
		return fmt.Errorf("项目编号无效：%q", p.ID)
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len([]rune(p.Name)) > 100 {
		return errors.New("项目名称无效")
	}
	if _, ok := cfg.Agents[p.Executor]; !ok {
		return fmt.Errorf("未知的执行代理：%s", p.Executor)
	}
	if p.Reviewer != "" {
		if _, ok := cfg.Agents[p.Reviewer]; !ok && p.Reviewer != "claude" && p.Reviewer != "codex" {
			return fmt.Errorf("未知的审查代理：%s", p.Reviewer)
		}
	}
	if p.Lead != "" && !taskID.MatchString(p.Lead) {
		return fmt.Errorf("负责代理无效：%q", p.Lead)
	}
	return validateEvidence(p.Checks, p.Scope)
}

func (s Store) CreateProject(cfg Config, p Project) (*Project, error) {
	if err := validateProject(cfg, &p); err != nil {
		return nil, err
	}
	dir, err := cfg.allowedFolder(p.Folder)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Stat(s.projectPath(p.ID)); err == nil {
		return nil, fmt.Errorf("项目编号 %s 已存在", p.ID)
	}
	dev, ino, err := folderID(dir)
	if err != nil {
		return nil, fmt.Errorf("无法读取工作目录：%v", err)
	}
	p.Folder, p.FolderDev, p.FolderIno = dir, dev, ino
	p.Created = time.Now()
	if err := s.writeProject(&p); err != nil {
		return nil, err
	}
	syncProjectFiles(&p, "")
	return &p, nil
}

func (s Store) UpdateProject(cfg Config, id string, patch ProjectPatch) (*Project, error) {
	if !taskID.MatchString(id) {
		return nil, fmt.Errorf("项目编号无效：%q", id)
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := s.readProject(id)
	if err != nil {
		return nil, err
	}
	if p.Archived {
		return nil, fmt.Errorf("项目 %s 已归档", id)
	}
	oldFolder := p.Folder
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Lead != nil {
		p.Lead = strings.TrimSpace(*patch.Lead)
	}
	if patch.Executor != nil {
		p.Executor = strings.TrimSpace(*patch.Executor)
	}
	if patch.Reviewer != nil {
		p.Reviewer = strings.TrimSpace(*patch.Reviewer)
	}
	if patch.Checks != nil {
		p.Checks = append([]string(nil), (*patch.Checks)...)
	}
	if patch.Scope != nil {
		p.Scope = append([]string(nil), (*patch.Scope)...)
	}
	if patch.SyncAgentsMD != nil {
		p.SyncAgentsMD = *patch.SyncAgentsMD
	}
	if err := validateProject(cfg, p); err != nil {
		return nil, err
	}
	if patch.Folder != nil {
		dir, err := cfg.allowedFolder(*patch.Folder)
		if err != nil {
			return nil, err
		}
		dev, ino, err := folderID(dir)
		if err != nil {
			return nil, fmt.Errorf("无法读取工作目录：%v", err)
		}
		tasks, err := s.List()
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if t.Project == p.ID && (t.Status == Queued || t.Status == Running || t.Status == Reviewing) {
				return nil, fmt.Errorf("项目 %s 有未完成的任务（%s），不能修改工作目录", p.ID, t.ID)
			}
		}
		p.Folder, p.FolderDev, p.FolderIno = dir, dev, ino
	}
	if err := s.writeProject(p); err != nil {
		return nil, err
	}
	syncProjectFiles(p, oldFolder)
	return p, nil
}

func (s Store) GetProject(id string) (*Project, error) {
	if !taskID.MatchString(id) {
		return nil, fmt.Errorf("项目编号无效：%q", id)
	}
	return s.readProject(id)
}

func (s Store) ListProjects() ([]*Project, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "projects"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Project
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if p, err := s.readProject(strings.TrimSuffix(name, ".json")); err == nil {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func (s Store) ArchiveProject(id string) (*Project, error) {
	if !taskID.MatchString(id) {
		return nil, fmt.Errorf("项目编号无效：%q", id)
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := s.readProject(id)
	if err != nil {
		return nil, err
	}
	p.Archived = true
	if err := s.writeProject(p); err != nil {
		return nil, err
	}
	syncProjectFiles(p, "")
	return p, nil
}

func (s Store) DispatchInProject(cfg Config, lead, project, executor, folder, id, card string, checks, scope []string) (*Task, error) {
	if project == "" {
		return s.DispatchWithEvidence(cfg, lead, executor, folder, id, card, checks, scope)
	}
	p, err := s.GetProject(project)
	if err != nil {
		return nil, err
	}
	if p.Archived {
		return nil, fmt.Errorf("项目 %s 已归档", project)
	}
	if executor == "" {
		executor = p.Executor
	}
	if folder == "" {
		folder = p.Folder
	} else {
		dir, err := cfg.allowedFolder(folder)
		if err != nil {
			return nil, err
		}
		if dir != p.Folder {
			return nil, fmt.Errorf("工作目录与项目 %s 不一致", project)
		}
		folder = dir
	}
	if dev, ino, err := folderID(p.Folder); err != nil || dev != p.FolderDev || ino != p.FolderIno {
		return nil, fmt.Errorf("项目 %s 的工作目录已变化，请重新创建项目", project)
	}
	if checks == nil {
		checks = p.Checks
	}
	if scope == nil {
		scope = p.Scope
	}
	if lead == "" {
		lead = p.Lead
	}
	return s.dispatch(cfg, lead, p.ID, executor, folder, id, card, checks, scope, "")
}

func (s Store) AssignProject(taskIDStr, projectID string) (*Task, error) {
	if !taskID.MatchString(taskIDStr) {
		return nil, fmt.Errorf("任务编号无效：%q", taskIDStr)
	}
	if projectID != "" && !taskID.MatchString(projectID) {
		return nil, fmt.Errorf("项目编号无效：%q", projectID)
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	var p *Project
	if projectID != "" {
		p, err = s.readProject(projectID)
		if err != nil {
			return nil, err
		}
		if p.Archived {
			return nil, fmt.Errorf("项目 %s 已归档", projectID)
		}
	}
	t, err := s.read(taskIDStr)
	if err != nil {
		return nil, err
	}
	targetID := t.ID
	if t.ReviewOf != "" {
		if parent, perr := s.read(t.ReviewOf); perr == nil {
			t = parent
			targetID = parent.ID
		}
	}
	t.Project = projectID
	if p != nil {
		if t.Lead == "" && p.Lead != "" {
			t.Lead = p.Lead
		}
		if t.Status == Queued {
			if len(t.Checks) == 0 && len(p.Checks) > 0 {
				t.Checks = append([]string(nil), p.Checks...)
			}
			if len(t.Scope) == 0 && len(p.Scope) > 0 {
				t.Scope = append([]string(nil), p.Scope...)
			}
		}
	}
	if err := s.write(t); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(filepath.Join(s.Dir, "tasks")); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if id == targetID {
				continue
			}
			if child, cerr := s.read(id); cerr == nil && child.ReviewOf == targetID {
				child.Project = projectID
				_ = s.write(child)
			}
		}
	}
	return t, nil
}

func resolveDir(dir string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(r)
	}
	return ""
}

// cwdSnapshot keeps inference tied to the directory captured at server startup.
// A missing, redirected, or replaced path must never be used for inference.
type cwdSnapshot struct {
	path, resolved string
	dev, ino       uint64
}

func captureCWD(path string) cwdSnapshot {
	resolved := resolveDir(path)
	if resolved == "" {
		return cwdSnapshot{}
	}
	dev, ino, err := folderID(path)
	if err != nil {
		return cwdSnapshot{}
	}
	return cwdSnapshot{path: path, resolved: resolved, dev: dev, ino: ino}
}

func (c cwdSnapshot) validPath() string {
	if c.path == "" || resolveDir(c.path) != c.resolved {
		return ""
	}
	dev, ino, err := folderID(c.path)
	if err != nil || dev != c.dev || ino != c.ino {
		return ""
	}
	return c.path
}

func folderContains(folder, dir string) bool {
	folder, dir = resolveDir(folder), resolveDir(dir)
	if folder == "" || dir == "" {
		return false
	}
	if dir == folder {
		return true
	}
	prefix := strings.TrimRight(folder, string(os.PathSeparator)) + string(os.PathSeparator)
	return strings.HasPrefix(dir, prefix)
}

// projectsForFolder returns the non-archived projects with the deepest folder
// containing dir; several projects may share one folder.
func projectsForFolder(projects []*Project, dir string) []*Project {
	dir = resolveDir(dir)
	if dir == "" {
		return nil
	}
	var best []*Project
	bestLen := -1
	for _, p := range projects {
		if p == nil || p.Archived {
			continue
		}
		f := resolveDir(p.Folder)
		if !folderContains(f, dir) {
			continue
		}
		switch {
		case len(f) > bestLen:
			best, bestLen = []*Project{p}, len(f)
		case len(f) == bestLen:
			best = append(best, p)
		}
	}
	return best
}

// ProjectForFolder picks the project dir belongs to for lead: the deepest
// matching folder, preferring projects lead leads when a folder is shared.
// nil, nil means dir is in no project; several equal candidates is an error.
func (s Store) ProjectForFolder(lead, dir string) (*Project, error) {
	ps, err := s.ListProjects()
	if err != nil {
		return nil, err
	}
	return pickProject(projectsForFolder(ps, dir), lead)
}

func pickProject(candidates []*Project, lead string) (*Project, error) {
	if len(candidates) > 1 && lead != "" {
		var led []*Project
		for _, p := range candidates {
			if p.Lead == lead {
				led = append(led, p)
			}
		}
		if len(led) > 0 {
			candidates = led
		}
	}
	switch len(candidates) {
	case 0:
		return nil, nil
	case 1:
		return candidates[0], nil
	}
	ids := make([]string, len(candidates))
	for i, p := range candidates {
		ids[i] = p.ID
	}
	return nil, fmt.Errorf("当前目录同时属于多个项目（%s），请指定 project", strings.Join(ids, "、"))
}

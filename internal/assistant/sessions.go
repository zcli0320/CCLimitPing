package assistant

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Mirror CC Switch's precedence: distinct DB title > session_index rename >
// first meaningful user prompt > project. History source is never presented
// as proof of a live client, live identity or a safe control connection.
func (r Reader) sessions(ctx context.Context, root string, warnings []string) ([]Session, []string, bool) {
	titles, indexErr := indexTitles(filepath.Join(root, "session_index.jsonl"))
	if indexErr != nil {
		warnings = append(warnings, "会话标题索引读取受限，部分标题可能使用问题或项目名。")
	}
	complete := true
	byID := map[string]Session{}
	dbRoot := r.databaseRoot(root)
	entries, _ := os.ReadDir(dbRoot)
	var dbs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "state_") && strings.HasSuffix(e.Name(), ".sqlite") {
			dbs = append(dbs, filepath.Join(dbRoot, e.Name()))
		}
	}
	version := func(path string) int {
		v, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "state_"), ".sqlite"))
		return v
	}
	sort.Slice(dbs, func(i, j int) bool { return version(dbs[i]) > version(dbs[j]) })
	dbOK := false
	for dbIndex, db := range dbs {
		var rows []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			First     string `json:"first_prompt"`
			Project   string `json:"cwd"`
			UpdatedAt int64  `json:"updated_at"`
			Source    string `json:"source"`
			Archived  int    `json:"archived"`
			Path      string `json:"rollout_path"`
		}
		err := r.query(ctx, db, `SELECT id,
 substr(CASE WHEN TRIM(title)<>TRIM(COALESCE(first_user_message,'')) THEN title ELSE '' END,1,512) AS title,
 substr(first_user_message,1,8192) AS first_prompt, cwd, updated_at, source, archived, rollout_path
 FROM threads WHERE source NOT LIKE '%subagent%' ORDER BY updated_at DESC LIMIT 2000`, &rows)
		if err != nil {
			continue
		}
		dbOK = true
		if dbIndex > 0 {
			complete = false
			warnings = append(warnings, "最新会话数据库不可读取，当前展示较旧数据库；观察已暂停。")
		}
		if len(rows) == 2000 {
			complete = false
			warnings = append(warnings, "当前仅展示最近 2000 个会话。")
		}
		for _, row := range rows {
			if row.ID == "" {
				continue
			}
			title, kind := strings.TrimSpace(row.Title), "database"
			if title == "" {
				title, kind = titles[row.ID], "session_index"
			}
			if title == "" {
				title, kind = cleanPrompt(row.First), "first_prompt"
				if title == "" && trustedRollout(root, row.Path) {
					title = rolloutPrompt(row.Path)
				}
			}
			if title == "" {
				title, kind = projectTitle(row.Project), "project"
			}
			byID[row.ID] = Session{Key: sessionKey(root, row.ID), ID: row.ID, Title: shortText(title, 120), TitleSource: kind, Project: row.Project, UpdatedAt: row.UpdatedAt, Source: sourceLabel(row.Source), Archived: row.Archived != 0}
		}
		break // Older schema versions may contain stale copies of the same IDs.
	}
	if !dbOK {
		complete = false
		warnings = append(warnings, "会话数据库不可读取；已退回有限度读取历史文件，实时状态未确认。")
		byID, warnings = r.rolloutSessions(ctx, root, titles, warnings)
	}
	result := make([]Session, 0, len(byID))
	for _, s := range byID {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt == result[j].UpdatedAt {
			return result[i].Key < result[j].Key
		}
		return result[i].UpdatedAt > result[j].UpdatedAt
	})
	return result, warnings, complete
}

func (r Reader) databaseRoot(root string) string {
	dbRoot := root
	var config struct {
		SQLiteHome string `toml:"sqlite_home"`
	}
	if _, err := toml.DecodeFile(filepath.Join(root, "config.toml"), &config); err == nil && config.SQLiteHome != "" {
		dbRoot = config.SQLiteHome
		if dbRoot == "~" {
			dbRoot = r.Home
		} else if strings.HasPrefix(dbRoot, "~/") {
			dbRoot = filepath.Join(r.Home, dbRoot[2:])
		}
		if !filepath.IsAbs(dbRoot) {
			dbRoot = filepath.Join(root, dbRoot)
		}
	}
	return dbRoot
}

func sourceLabel(source string) string {
	switch source {
	case "cli":
		return "CLI（历史来源）"
	case "vscode", "desktop":
		return "IDE / Desktop（历史来源）"
	default:
		return "其他客户端（历史来源）"
	}
}

func indexTitles(path string) (map[string]string, error) {
	titles := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return titles, nil
		}
		return titles, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return titles, err
	}
	const limit int64 = 8 << 20
	var reader io.Reader = f
	truncated := info.Size() > limit
	if truncated {
		// The index is append-only. Read its tail so recent renames win.
		reader = io.NewSectionReader(f, info.Size()-limit, limit)
	}
	buffer := bufio.NewReader(reader)
	if truncated {
		_, _ = buffer.ReadBytes('\n')
	}
	scan := bufio.NewScanner(buffer)
	scan.Buffer(make([]byte, 4096), 256<<10)
	for scan.Scan() {
		var row struct {
			ID    string `json:"id"`
			Title string `json:"thread_name"`
		}
		if json.Unmarshal(scan.Bytes(), &row) == nil && row.ID != "" && strings.TrimSpace(row.Title) != "" {
			titles[row.ID] = shortText(row.Title, 120)
		}
	}
	if scan.Err() != nil {
		return titles, errors.New("索引读取失败")
	}
	if truncated {
		return titles, errors.New("索引仅读取最近部分")
	}
	return titles, nil
}

func shortText(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
func projectTitle(path string) string {
	if path == "" {
		return "未命名会话"
	}
	return filepath.Base(path)
}

func cleanPrompt(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"# AGENTS.md instructions", "<environment_context>", "<user_instructions>", "<turn_aborted>", "<external_codex_apps_open_page>", "<permissions instructions>", "<INSTRUCTIONS>"} {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	// CC Switch recognizes the explicit request in IDE context wrappers.
	if strings.HasPrefix(text, "# Context from my IDE setup:") {
		at := strings.Index(strings.ToLower(text), "my request for codex:")
		if at < 0 {
			return ""
		}
		text = strings.TrimSpace(text[at+len("my request for codex:"):])
	}
	return shortText(text, 120)
}

type rolloutRecord struct {
	Type    string `json:"type"`
	Payload struct {
		ID      string          `json:"id"`
		Cwd     string          `json:"cwd"`
		Source  json.RawMessage `json:"source"`
		Role    string          `json:"role"`
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"payload"`
}

func promptFromRecord(rec rolloutRecord) string {
	if rec.Type == "event_msg" && rec.Payload.Type == "user_message" {
		return cleanPrompt(rec.Payload.Message)
	}
	if rec.Type != "response_item" || rec.Payload.Type != "message" || rec.Payload.Role != "user" {
		return ""
	}
	var parts []string
	for _, c := range rec.Payload.Content {
		if c.Type == "input_text" || c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return cleanPrompt(strings.Join(parts, "\n"))
}

// Never follow an arbitrary path contained in a database into another source.
func trustedRollout(root, path string) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		base, err := filepath.EvalSymlinks(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(base, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func scanRollout(path string, visit func(rolloutRecord) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var reader io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return
		}
		defer gz.Close()
		reader = gz
	}
	scan := bufio.NewScanner(io.LimitReader(reader, 1<<20))
	scan.Buffer(make([]byte, 4096), 512<<10)
	for scan.Scan() {
		var rec rolloutRecord
		if json.Unmarshal(scan.Bytes(), &rec) == nil && visit(rec) {
			return
		}
	}
}

func rolloutPrompt(path string) string {
	var prompt string
	scanRollout(path, func(rec rolloutRecord) bool { prompt = promptFromRecord(rec); return prompt != "" })
	return prompt
}

func (r Reader) rolloutSessions(ctx context.Context, root string, titles map[string]string, warnings []string) (map[string]Session, []string) {
	result := map[string]Session{}
	count := 0
	files := &recentRollouts{}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(root, dir)
		// Unified history may make the root itself a symlink. Walk the resolved
		// root, while still checking each rollout's containment before reading.
		if real, err := filepath.EvalSymlinks(base); err == nil {
			base = real
		}
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasPrefix(d.Name(), "rollout-") || !(strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".jsonl.gz")) {
				return nil
			}
			count++
			info, err := d.Info()
			if err != nil {
				return nil
			}
			file := rolloutFile{path: path, archived: dir == "archived_sessions", modified: info.ModTime()}
			if files.Len() < 4000 {
				heap.Push(files, file)
			} else if (*files)[0].modified.Before(file.modified) {
				heap.Pop(files)
				heap.Push(files, file)
			}
			return nil
		})
	}
	// Bound content reads while keeping the newest files, rather than the
	// oldest 4000 encountered in lexicographic directory order.
	sort.Slice(*files, func(i, j int) bool { return (*files)[i].modified.After((*files)[j].modified) })
	for _, file := range *files {
		if ctx.Err() != nil {
			break
		}
		path := file.path
		if !trustedRollout(root, path) {
			continue
		}
		var id, project, source, prompt string
		scanRollout(path, func(rec rolloutRecord) bool {
			if rec.Type == "session_meta" {
				id, project = rec.Payload.ID, rec.Payload.Cwd
				_ = json.Unmarshal(rec.Payload.Source, &source)
				if strings.Contains(string(rec.Payload.Source), "subagent") {
					id = ""
					return true
				}
			}
			if prompt == "" {
				prompt = promptFromRecord(rec)
			}
			return id != "" && prompt != ""
		})
		if id == "" {
			continue
		}
		updated := file.modified.Unix()
		if old, ok := result[id]; ok && old.UpdatedAt >= updated {
			continue
		}
		title, kind := titles[id], "session_index"
		if title == "" {
			title, kind = prompt, "first_prompt"
		}
		if title == "" {
			title, kind = projectTitle(project), "project"
		}
		result[id] = Session{Key: sessionKey(root, id), ID: id, Title: title, TitleSource: kind, Project: project, UpdatedAt: updated, Source: sourceLabel(source), Archived: file.archived}
	}
	if count > 4000 {
		warnings = append(warnings, "历史文件回退仅读取最近 4000 个文件，列表可能不完整。")
	}
	if ctx.Err() != nil {
		warnings = append(warnings, "历史文件扫描已超时，列表可能不完整。")
	}
	return result, warnings
}

type rolloutFile struct {
	path     string
	archived bool
	modified time.Time
}
type recentRollouts []rolloutFile

func (h recentRollouts) Len() int           { return len(h) }
func (h recentRollouts) Less(i, j int) bool { return h[i].modified.Before(h[j].modified) }
func (h recentRollouts) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *recentRollouts) Push(v any)        { *h = append(*h, v.(rolloutFile)) }
func (h *recentRollouts) Pop() any          { a := *h; last := a[len(a)-1]; *h = a[:len(a)-1]; return last }

package orchestra

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	defaultProjectRulesBody = "## 项目规则\n\n- （在这里写给所有代理的规则）\n"
	maxProjectRulesBytes    = 8 * 1024
	maxLeadRulesBytes       = 2 * 1024
)

func RulesPath(p *Project) string {
	if p == nil || p.Folder == "" || !filepath.IsAbs(p.Folder) || !taskID.MatchString(p.ID) {
		return ""
	}
	return filepath.Join(p.Folder, ".codex-helper", "projects", p.ID+".md")
}

func cleanSingleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func displayLead(lead string) string {
	if strings.TrimSpace(lead) == "" {
		return "不指定"
	}
	return cleanSingleLine(lead)
}

func displayReviewer(reviewer string) string {
	if strings.TrimSpace(reviewer) == "" {
		return "无"
	}
	return cleanSingleLine(reviewer)
}

func marshalStringSlice(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func projectFrontMatter(p *Project) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("# 由 Codex 小助手生成：角色请在小助手中修改，本段会被覆盖\n")
	fmt.Fprintf(&b, "project: %s\n", p.ID)
	fmt.Fprintf(&b, "name: %s\n", cleanSingleLine(p.Name))
	fmt.Fprintf(&b, "folder: %s\n", p.Folder)
	fmt.Fprintf(&b, "lead: %s\n", displayLead(p.Lead))
	fmt.Fprintf(&b, "executor: %s\n", p.Executor)
	fmt.Fprintf(&b, "reviewer: %s\n", displayReviewer(p.Reviewer))
	fmt.Fprintf(&b, "checks: %s\n", marshalStringSlice(p.Checks))
	fmt.Fprintf(&b, "scope: %s\n", marshalStringSlice(p.Scope))
	b.WriteString("---\n")
	return b.String()
}

func extractProjectBody(data []byte) string {
	s := string(data)
	var rest string
	switch {
	case strings.HasPrefix(s, "---\n"):
		rest = s[len("---\n"):]
	case strings.HasPrefix(s, "---\r\n"):
		rest = s[len("---\r\n"):]
	default:
		return s
	}
	pos := 0
	for pos < len(rest) {
		nl := strings.IndexByte(rest[pos:], '\n')
		var line string
		nextPos := len(rest)
		if nl >= 0 {
			line = rest[pos : pos+nl]
			nextPos = pos + nl + 1
		} else {
			line = rest[pos:]
		}
		if strings.TrimSuffix(line, "\r") == "---" {
			return rest[nextPos:]
		}
		pos = nextPos
	}
	return s
}

func WriteProjectFile(p *Project) error {
	return writeProjectFileFrom(p, "")
}

func writeProjectFileFrom(p *Project, oldFolder string) error {
	path := RulesPath(p)
	if path == "" {
		return errors.New("项目路径无效")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body := defaultProjectRulesBody
	if existing, err := os.ReadFile(path); err == nil {
		body = extractProjectBody(existing)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if oldFolder != "" && oldFolder != p.Folder {
		oldCopy := *p
		oldCopy.Folder = oldFolder
		if oldPath := RulesPath(&oldCopy); oldPath != "" {
			if oldData, oerr := os.ReadFile(oldPath); oerr == nil {
				body = extractProjectBody(oldData)
			}
		}
	}
	content := projectFrontMatter(p) + body
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readProjectRulesCap(p *Project, maxBytes int) string {
	path := RulesPath(p)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	body := extractProjectBody(data)
	if maxBytes > 0 && len(body) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		return body[:cut] + "\n(truncated)"
	}
	return body
}

func ReadProjectRules(p *Project) string {
	return readProjectRulesCap(p, maxProjectRulesBytes)
}

func ProjectBrief(p *Project) string {
	return projectBriefCap(p, maxProjectRulesBytes)
}

func projectBriefCap(p *Project, maxBytes int) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- project %s ---\n", p.ID)
	fmt.Fprintf(&b, "name: %s\n", p.Name)
	fmt.Fprintf(&b, "folder: %s\n", p.Folder)
	fmt.Fprintf(&b, "lead: %s\n", displayLead(p.Lead))
	fmt.Fprintf(&b, "executor: %s\n", p.Executor)
	fmt.Fprintf(&b, "reviewer: %s\n", displayReviewer(p.Reviewer))
	if len(p.Checks) > 0 {
		fmt.Fprintf(&b, "checks: %s\n", marshalStringSlice(p.Checks))
	}
	if len(p.Scope) > 0 {
		fmt.Fprintf(&b, "scope: %s\n", marshalStringSlice(p.Scope))
	}
	if rules := strings.TrimSpace(readProjectRulesCap(p, maxBytes)); rules != "" {
		b.WriteString("\n")
		b.WriteString(rules)
		b.WriteString("\n")
	}
	return b.String()
}

func SyncAgentsBlock(p *Project) error {
	if p == nil || p.Folder == "" || !filepath.IsAbs(p.Folder) || !taskID.MatchString(p.ID) {
		return errors.New("项目路径无效")
	}
	agentsPath := filepath.Join(p.Folder, "AGENTS.md")
	begin := fmt.Sprintf("<!-- codex-helper:project %s begin -->", p.ID)
	end := fmt.Sprintf("<!-- codex-helper:project %s end -->", p.ID)
	data, err := os.ReadFile(agentsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := string(data)
	bi := strings.Index(text, begin)
	endPos := -1
	if bi >= 0 {
		if ei := strings.Index(text[bi:], end); ei >= 0 {
			endPos = bi + ei + len(end)
			if endPos < len(text) && text[endPos] == '\r' {
				endPos++
			}
			if endPos < len(text) && text[endPos] == '\n' {
				endPos++
			}
		}
	}
	writeAgents := func(content string) error {
		mode := os.FileMode(0o644)
		if info, statErr := os.Stat(agentsPath); statErr == nil {
			mode = info.Mode().Perm()
		}
		tmp := fmt.Sprintf("%s.%d.tmp", agentsPath, os.Getpid())
		if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
			return err
		}
		return os.Rename(tmp, agentsPath)
	}
	if !p.SyncAgentsMD || p.Archived {
		if errors.Is(err, os.ErrNotExist) || endPos < 0 {
			return nil
		}
		updated := text[:bi] + text[endPos:]
		return writeAgents(updated)
	}
	block := fmt.Sprintf("%s\n## Codex 小助手项目：%s (%s)\n- 角色：负责=%s 执行=%s 审查=%s\n- 规则见 .codex-helper/projects/%s.md\n%s\n",
		begin, cleanSingleLine(p.Name), p.ID, displayLead(p.Lead), p.Executor, displayReviewer(p.Reviewer), p.ID, end)
	var updated string
	if endPos >= 0 {
		prefix := text[:bi]
		if prefix != "" && !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		updated = prefix + block + text[endPos:]
	} else if text == "" || strings.HasSuffix(text, "\n") {
		updated = text + block
	} else {
		updated = text + "\n" + block
	}
	return writeAgents(updated)
}

func syncProjectFiles(p *Project, oldFolder string) {
	var warns []string
	if oldFolder != "" && oldFolder != p.Folder {
		oldCopy := *p
		oldCopy.Folder = oldFolder
		oldCopy.Archived = true
		if err := SyncAgentsBlock(&oldCopy); err != nil {
			warns = append(warns, err.Error())
		}
	}
	if !p.Archived {
		if err := writeProjectFileFrom(p, oldFolder); err != nil {
			warns = append(warns, err.Error())
		}
	}
	if err := SyncAgentsBlock(p); err != nil {
		warns = append(warns, err.Error())
	}
	p.Warning = strings.Join(warns, "；")
}

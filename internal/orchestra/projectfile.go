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
	if p == nil || p.Folder == "" || p.ID == "" {
		return ""
	}
	return filepath.Join(p.Folder, ".codex-helper", "projects", p.ID+".md")
}

func displayLead(lead string) string {
	if strings.TrimSpace(lead) == "" {
		return "不指定"
	}
	return lead
}

func displayReviewer(reviewer string) string {
	if strings.TrimSpace(reviewer) == "" {
		return "无"
	}
	return reviewer
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
	fmt.Fprintf(&b, "name: %s\n", p.Name)
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
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[len("---\n"):]
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return rest[i+len("\n---\n"):]
	}
	if strings.HasSuffix(rest, "\n---") {
		return ""
	}
	return s
}

func WriteProjectFile(p *Project) error {
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
	}
	content := projectFrontMatter(p) + body
	return os.WriteFile(path, []byte(content), 0o600)
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
	if p == nil || p.Folder == "" || p.ID == "" {
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
			if endPos < len(text) && text[endPos] == '\n' {
				endPos++
			}
		}
	}
	if !p.SyncAgentsMD || p.Archived {
		if errors.Is(err, os.ErrNotExist) || endPos < 0 {
			return nil
		}
		updated := text[:bi] + text[endPos:]
		return os.WriteFile(agentsPath, []byte(updated), 0o644)
	}
	block := fmt.Sprintf("%s\n## Codex 小助手项目：%s (%s)\n- 角色：负责=%s 执行=%s 审查=%s\n- 规则见 .codex-helper/projects/%s.md\n%s\n",
		begin, p.Name, p.ID, displayLead(p.Lead), p.Executor, displayReviewer(p.Reviewer), p.ID, end)
	var updated string
	if endPos >= 0 {
		updated = text[:bi] + block + text[endPos:]
	} else if text == "" || strings.HasSuffix(text, "\n") {
		updated = text + block
	} else {
		updated = text + "\n" + block
	}
	return os.WriteFile(agentsPath, []byte(updated), 0o644)
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
	if err := WriteProjectFile(p); err != nil {
		warns = append(warns, err.Error())
	}
	if err := SyncAgentsBlock(p); err != nil {
		warns = append(warns, err.Error())
	}
	p.Warning = strings.Join(warns, "；")
}

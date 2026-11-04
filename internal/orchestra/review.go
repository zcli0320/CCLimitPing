package orchestra

import (
	"fmt"
	"strings"
)

func reviewID(id string, n int) string {
	suffix := fmt.Sprintf("-r%d", n)
	if max := 64 - len(suffix); len(id) > max {
		id = id[:max]
	}
	return id + suffix
}

func parseVerdict(text string) string {
	verdict := "unavailable"
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(line)
		for strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
			s = strings.TrimSpace(s[2:])
		}
		s = strings.Map(func(r rune) rune {
			if r == '*' || r == '_' || r == '`' {
				return -1
			}
			return r
		}, s)
		s = strings.ToUpper(strings.Join(strings.Fields(s), " "))
		switch s {
		case "VERDICT: PASS":
			verdict = "pass"
		case "VERDICT: CHANGES":
			verdict = "changes"
		}
	}
	return verdict
}

func reviewCard(t *Task, proj ...*Project) string {
	var p *Project
	if len(proj) > 0 {
		p = proj[0]
	} else if t.Project != "" && t.Folder != "" {
		p = &Project{ID: t.Project, Name: t.Project, Folder: t.Folder, Lead: t.Lead, Executor: t.Executor}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are the reviewer for project %s. Review, do not modify files.\n\n", t.Project)
	if brief := ProjectBrief(p); brief != "" {
		b.WriteString(brief)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "--- original card (%s) ---\n%s\n\n", t.ID, t.Card)
	fmt.Fprintf(&b, "--- executor receipt ---\n%s\n\n", t.LastReceipt())
	fmt.Fprintf(&b, "Evidence: %s\n", Summary(t))
	if ev := Evidence(t); ev != nil && len(ev.Changed) > 0 {
		fmt.Fprintf(&b, "Changed files: %s\n", strings.Join(ev.Changed, ", "))
	}
	b.WriteString("\nInspect the working tree (git diff / git status) for those files, you may run read-only checks, then list concrete problems with file:line, and end with exactly one line `VERDICT: PASS` or `VERDICT: CHANGES`.\n")
	return b.String()
}

// pendingReview returns the reviewer that still has to see t's latest receipt.
func (s Store) pendingReview(t *Task) string {
	if t.Status != Receipt || t.Project == "" || t.ReviewOf != "" || len(t.Runs) <= t.ReviewedRuns {
		return ""
	}
	p, err := s.GetProject(t.Project)
	// Nobody reviews their own work: the lead accepts it, and an executor
	// reviewing its own receipt adds nothing but spent quota.
	if err != nil || p.Archived || p.Reviewer == t.Lead || p.Reviewer == t.Executor {
		return ""
	}
	return p.Reviewer
}

// skipReview records that t's latest receipt could not be reviewed.
func (r *Runner) skipReview(id string, runs int, why string) {
	_, _ = r.Store.Update(id, func(p *Task) error {
		if p.Status != Receipt || len(p.Runs) != runs {
			return errNotQueued
		}
		p.ReviewedRuns, p.ReviewTask, p.ReviewVerdict, p.ReviewText = runs, "", "unavailable", why
		return nil
	})
}

func (r *Runner) reviewTick(tasks []*Task) {
	for _, t := range tasks {
		reviewer := r.Store.pendingReview(t)
		if reviewer == "" {
			continue
		}
		runs := len(t.Runs)
		if _, err := r.Cfg.verifyFolder(t); err != nil {
			r.skipReview(t.ID, runs, "无法审查原工作目录："+err.Error())
			continue
		}
		if _, ok := r.Cfg.Agents[reviewer]; !ok {
			r.skipReview(t.ID, runs, "未知的审查代理："+reviewer)
			continue
		}
		childID := reviewID(t.ID, runs)
		p, _ := r.Store.GetProject(t.Project)
		child, err := r.Store.Get(childID)
		if err != nil {
			child, err = r.Store.dispatch(r.Cfg, "orchestra", t.Project, reviewer, t.Folder, childID, reviewCard(t, p), nil, nil, t.ID)
			if err != nil {
				r.skipReview(t.ID, runs, "无法派出审查任务："+err.Error())
				continue
			}
		} else if child.ReviewOf != t.ID {
			r.skipReview(t.ID, runs, "审查任务编号 "+childID+" 已被占用")
			continue
		}
		if child.Folder != t.Folder || child.FolderDev != t.FolderDev || child.FolderIno != t.FolderIno {
			_, _ = r.Store.Cancel(child.ID)
			r.skipReview(t.ID, runs, "审查任务的工作目录与原任务不一致")
			continue
		}
		_, err = r.Store.Update(t.ID, func(p *Task) error {
			if p.Status != Receipt || len(p.Runs) != runs {
				return errNotQueued
			}
			p.Status = Reviewing
			p.ReviewTask = childID
			p.ReviewedRuns = runs
			p.ReviewVerdict = ""
			p.ReviewText = ""
			return nil
		})
		if err != nil {
			_, _ = r.Store.Cancel(child.ID)
		}
	}

	for _, p := range tasks {
		if p.Status != Reviewing {
			continue
		}
		reviewTask := p.ReviewTask
		child, err := r.Store.Get(reviewTask)
		if err != nil {
			_, _ = r.Store.Update(p.ID, func(t *Task) error {
				if t.Status != Reviewing || t.ReviewTask != reviewTask {
					return errNotQueued
				}
				t.Status = Receipt
				t.ReviewVerdict = "unavailable"
				t.ReviewText = ""
				return nil
			})
			continue
		}
		if !child.Done() {
			continue
		}
		text := child.LastReceipt()
		verdict := "unavailable"
		if child.Status == Receipt {
			verdict = parseVerdict(text)
		}
		settings, settingsErr := ReadSettings(r.Cfg.Dir)
		if _, err := r.Store.Update(p.ID, func(t *Task) error {
			if t.Status != Reviewing || t.ReviewTask != reviewTask {
				return errNotQueued
			}
			t.ReviewVerdict = verdict
			t.ReviewText = text
			if verdict == "changes" && settingsErr == nil && t.AutoReworkCount < settings.AutoReworkMax && t.Session != "" {
				t.Rework = "The project reviewer requested changes:\n" + text + "\n\nFix these, then give a new receipt.\n" + AutoReworkLimits
				t.AutoReworkCount++
				t.AutoReworkPending = true
				t.Status = Queued
			} else {
				t.Status = Receipt
			}
			return nil
		}); err == nil && (child.Status == Receipt || child.Status == Failed) {
			_, _ = r.Store.Accept(child.ID)
		}
	}
}

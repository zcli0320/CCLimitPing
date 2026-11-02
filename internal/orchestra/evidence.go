package orchestra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type EvidenceView struct {
	Summary         string          `json:"summary"`
	Checks          []CheckResult   `json:"checks,omitempty"`
	Changes         *ChangeEvidence `json:"changes,omitempty"`
	AutoReworkCount int             `json:"auto_rework_count"`
	Phase           string          `json:"phase,omitempty"`
	Changed         []string        `json:"changed"`
	OutOfScope      []string        `json:"out_of_scope"`
	HeadChanged     bool            `json:"head_changed"`
}

func Evidence(t *Task) *EvidenceView {
	if len(t.Checks) == 0 && len(t.Scope) == 0 {
		return nil
	}
	v := &EvidenceView{Summary: Summary(t), AutoReworkCount: t.AutoReworkCount, Changed: []string{}, OutOfScope: []string{}}
	if len(t.Runs) > 0 {
		r := t.Runs[len(t.Runs)-1]
		v.Checks = r.Checks
		v.Changes = r.Changes
		if r.Changes != nil {
			v.Changed = append(v.Changed, r.Changes.Changed...)
			v.OutOfScope = append(v.OutOfScope, r.Changes.OutOfScope...)
			v.HeadChanged = r.Changes.HeadChanged
		} else {
			v.Changed = append(v.Changed, r.Changed...)
			v.OutOfScope = append(v.OutOfScope, r.OutOfScope...)
			v.HeadChanged = r.HeadChanged
		}
		v.Phase = r.Phase
	}
	return v
}

func assignChanges(run *Run, changes *ChangeEvidence) {
	run.Changes = changes
	if changes != nil {
		run.Changed = changes.Changed
		run.OutOfScope = changes.OutOfScope
		run.HeadChanged = changes.HeadChanged
	}
}
func Summary(t *Task) string {
	if len(t.Checks) == 0 && len(t.Scope) == 0 {
		return "暂无检查或范围证据"
	}
	pass, fail, unavailable := 0, 0, 0
	var c *ChangeEvidence
	if len(t.Runs) > 0 {
		run := t.Runs[len(t.Runs)-1]
		c = run.Changes
		for _, v := range run.Checks {
			switch v.Status {
			case "passed":
				pass++
			case "failed":
				fail++
			default:
				unavailable++
			}
		}
	}
	changes := "改动不可用"
	if c != nil && c.Status == "available" {
		changes = fmt.Sprintf("改动 %d 个文件，超出范围 %d 个", len(c.Changed), len(c.OutOfScope))
	}
	return fmt.Sprintf("检查 %d/%d 通过，失败 %d，不可用 %d；%s；自动返工 %d 次", pass, len(t.Checks), fail, unavailable, changes, t.AutoReworkCount)
}
func FailingChecks(t *Task) []CheckResult {
	var out []CheckResult
	if len(t.Runs) > 0 {
		for _, v := range t.Runs[len(t.Runs)-1].Checks {
			if v.Status == "failed" {
				out = append(out, v)
			}
		}
	}
	return out
}

type gitSnapshot struct {
	head   string
	hashes map[string]string
	status string
	reason string
}

func gitUnavailable(err error) string {
	if err != nil && strings.Contains(err.Error(), "too large") {
		return "too large"
	}
	return "unavailable"
}

type capBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.overflow = true
		return 0, fmt.Errorf("output limit")
	}
	return b.Buffer.Write(p)
}
func readGit(folder string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args = append([]string{"-c", "core.fsmonitor=false", "-C", folder}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var out capBuffer
	out.max = 4 << 20
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if out.overflow {
		return nil, fmt.Errorf("too large")
	}
	return out.Bytes(), err
}
func gitPaths(folder string) (string, map[string]bool, error) {
	rootBytes, e := readGit(folder, "rev-parse", "--show-toplevel")
	if e != nil {
		return "", nil, e
	}
	root := strings.TrimSuffix(string(rootBytes), "\n")
	resolvedRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", nil, e
	}
	resolvedFolder, e := filepath.EvalSymlinks(folder)
	if e != nil {
		return "", nil, e
	}
	relative, e := filepath.Rel(resolvedRoot, resolvedFolder)
	if e != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", nil, fmt.Errorf("invalid repository root")
	}
	prefix := ""
	if relative != "." {
		prefix = filepath.ToSlash(relative) + "/"
	}
	head, e := readGit(folder, "rev-parse", "--verify", "HEAD")
	if e != nil {
		return "", nil, e
	}
	raw, e := readGit(folder, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if e != nil {
		return "", nil, e
	}
	entries := bytes.Split(raw, []byte{0})
	paths := map[string]bool{}
	for i := 0; i < len(entries); i++ {
		b := entries[i]
		if len(b) == 0 {
			continue
		}
		if len(b) < 4 || b[2] != ' ' {
			return "", nil, fmt.Errorf("invalid git status")
		}
		p := string(b[3:])
		if strings.HasPrefix(p, prefix) {
			paths[strings.TrimPrefix(p, prefix)] = true
		}
		if b[0] == 'R' || b[0] == 'C' || b[1] == 'R' || b[1] == 'C' {
			i++
			if i >= len(entries) || len(entries[i]) == 0 {
				return "", nil, fmt.Errorf("invalid git rename")
			}
			previous := string(entries[i])
			if strings.HasPrefix(previous, prefix) {
				paths[strings.TrimPrefix(previous, prefix)] = true
			}
		}
		if len(paths) > 2000 {
			return "", nil, fmt.Errorf("too large")
		}
	}
	return strings.TrimSpace(string(head)), paths, nil
}
func hashPaths(folder string, paths map[string]bool) (map[string]string, error) {
	out := map[string]string{}
	var total int64
	for p := range paths {
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("unsafe path")
		}
		name := filepath.Join(folder, clean)
		info, e := os.Lstat(name)
		if os.IsNotExist(e) {
			out[p] = "missing"
			continue
		}
		if e != nil {
			return nil, e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(name)
			if e != nil {
				return nil, e
			}
			sum := sha256.Sum256([]byte("link:" + target))
			out[p] = hex.EncodeToString(sum[:])
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unsupported path")
		}
		total += info.Size()
		if total > 50<<20 {
			return nil, fmt.Errorf("too large")
		}
		f, e := os.Open(name)
		if e != nil {
			return nil, e
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, info.Size()+1))
		_ = f.Close()
		if e != nil || n != info.Size() {
			if e == nil {
				e = fmt.Errorf("file changed during snapshot")
			}
			return nil, e
		}
		out[p] = hex.EncodeToString(h.Sum(nil))
	}
	return out, nil
}
func snapshotGit(folder string) gitSnapshot {
	head, paths, e := gitPaths(folder)
	if e != nil {
		return gitSnapshot{status: "unavailable", reason: gitUnavailable(e)}
	}
	hashes, e := hashPaths(folder, paths)
	if e != nil {
		return gitSnapshot{status: "unavailable", reason: gitUnavailable(e)}
	}
	return gitSnapshot{head: head, hashes: hashes, status: "available"}
}
func changesSince(folder string, before gitSnapshot, scope []string) *ChangeEvidence {
	if before.status != "available" {
		return &ChangeEvidence{Status: "unavailable", Reason: before.reason}
	}
	head, paths, e := gitPaths(folder)
	if e != nil {
		return &ChangeEvidence{Status: "unavailable", Reason: gitUnavailable(e)}
	}
	for p := range before.hashes {
		paths[p] = true
	}
	if len(paths) > 2000 {
		return &ChangeEvidence{Status: "unavailable", Reason: "too large"}
	}
	after, e := hashPaths(folder, paths)
	if e != nil {
		return &ChangeEvidence{Status: "unavailable", Reason: gitUnavailable(e)}
	}
	result := &ChangeEvidence{Status: "available", HeadChanged: head != before.head}
	for p, h := range after {
		old, found := before.hashes[p]
		if !found || old != h {
			result.Changed = append(result.Changed, p)
			if len(scope) > 0 && !withinScope(p, scope) {
				result.OutOfScope = append(result.OutOfScope, p)
			}
		}
	}
	if result.HeadChanged {
		raw, err := readGit(folder, "diff", "--name-only", "--no-renames", "--relative", "-z", before.head, head, "--", ".")
		if err != nil {
			return &ChangeEvidence{Status: "unavailable", Reason: gitUnavailable(err)}
		}
		seen := map[string]bool{}
		for _, p := range result.Changed {
			seen[p] = true
		}
		for _, entry := range bytes.Split(raw, []byte{0}) {
			p := string(entry)
			if p == "" || seen[p] {
				continue
			}
			if _, alreadyDirty := before.hashes[p]; alreadyDirty {
				continue
			}
			seen[p] = true
			result.Changed = append(result.Changed, p)
			if len(scope) > 0 && !withinScope(p, scope) {
				result.OutOfScope = append(result.OutOfScope, p)
			}
			if len(seen) > 2000 {
				return &ChangeEvidence{Status: "unavailable", Reason: "too large"}
			}
		}
	}
	sort.Strings(result.Changed)
	sort.Strings(result.OutOfScope)
	return result
}
func withinScope(path string, scope []string) bool {
	for _, p := range scope {
		clean := filepath.ToSlash(filepath.Clean(p))
		if clean == "." || path == clean || strings.HasPrefix(path, clean+"/") {
			return true
		}
	}
	return false
}

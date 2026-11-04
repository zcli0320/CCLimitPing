package orchestra

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Status of a task card.
type Status string

const (
	Queued         Status = "queued"
	Running        Status = "running"
	Reviewing      Status = "reviewing" // receipt is with the project's reviewer
	Receipt        Status = "receipt"   // executor answered; waiting for the lead
	Failed         Status = "failed"    // executor ended without an answer
	QuotaExhausted Status = "quota_exhausted"
	Cancelled      Status = "cancelled" // stopped by the lead or the user
	Accepted       Status = "accepted"  // lead accepted the receipt
)

// Run is one executor process for a task (the first run or a rework).
type Run struct {
	Kind        string          `json:"kind"` // task | rework
	Started     time.Time       `json:"started"`
	Ended       time.Time       `json:"ended,omitempty"`
	Exit        int             `json:"exit"`
	PID         int             `json:"pid,omitempty"`    // 0 while starting
	Marker      string          `json:"marker,omitempty"` // identifies this run between the claim and launch steps
	Log         string          `json:"log"`
	Receipt     string          `json:"receipt,omitempty"`
	Error       string          `json:"error,omitempty"`
	Phase       string          `json:"phase,omitempty"`
	Checks      []CheckResult   `json:"checks,omitempty"`
	Changes     *ChangeEvidence `json:"changes,omitempty"`
	Changed     []string        `json:"changed,omitempty"`
	OutOfScope  []string        `json:"out_of_scope,omitempty"`
	HeadChanged bool            `json:"head_changed,omitempty"`
}

type CheckResult struct {
	Command  string  `json:"command"`
	Status   string  `json:"status"`
	Exit     int     `json:"exit"`
	Duration float64 `json:"duration_seconds"`
	TimedOut bool    `json:"timed_out,omitempty"`
	Log      string  `json:"log,omitempty"`
	Tail     string  `json:"tail,omitempty"`
}
type ChangeEvidence struct {
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	Changed     []string `json:"changed,omitempty"`
	OutOfScope  []string `json:"out_of_scope,omitempty"`
	HeadChanged bool     `json:"head_changed,omitempty"`
}

// Task is one card from a lead to an executor.
type Task struct {
	ID                string    `json:"id"`
	Project           string    `json:"project,omitempty"`
	Lead              string    `json:"lead"`
	Executor          string    `json:"executor"`
	Folder            string    `json:"folder"`
	FolderDev         uint64    `json:"folder_dev,omitempty"` // identity of Folder at dispatch
	FolderIno         uint64    `json:"folder_ino,omitempty"`
	Card              string    `json:"card"`
	Status            Status    `json:"status"`
	Session           string    `json:"session,omitempty"` // executor's own session id, for reworks
	Rework            string    `json:"rework,omitempty"`  // queued rework text
	CancelRequested   bool      `json:"cancel_requested,omitempty"`
	Unconfirmed       bool      `json:"unconfirmed,omitempty"` // a crashed run's executor may still be alive
	Created           time.Time `json:"created"`
	Updated           time.Time `json:"updated"`
	Runs              []Run     `json:"runs,omitempty"`
	Checks            []string  `json:"checks,omitempty"`
	Scope             []string  `json:"scope,omitempty"`
	AutoReworkCount   int       `json:"auto_rework_count,omitempty"`
	AutoReworkPending bool      `json:"auto_rework_pending,omitempty"`
	ReviewOf          string    `json:"review_of,omitempty"`      // on a review task: the task it reviews
	ReviewTask        string    `json:"review_task,omitempty"`    // on a reviewed task: current review task id
	ReviewVerdict     string    `json:"review_verdict,omitempty"` // pass | changes | unavailable
	ReviewText        string    `json:"review_text,omitempty"`    // reviewer's receipt
	ReviewedRuns      int       `json:"reviewed_runs,omitempty"`  // len(Runs) at the last review dispatch
}

// Done reports whether the task waits for nothing from the executor.
// A receipt the project reviewer has yet to see is waited on with needsLead.
func (t *Task) Done() bool {
	return t.Status != Queued && t.Status != Running && t.Status != Reviewing
}

// LastReceipt is the newest executor answer, if any.
func (t *Task) LastReceipt() string {
	if len(t.Runs) == 0 {
		return ""
	}
	r := t.Runs[len(t.Runs)-1]
	if r.Receipt != "" {
		return r.Receipt
	}
	return r.Error
}

// Store keeps tasks as one JSON file each under Dir/tasks.
type Store struct{ Dir string }

var taskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s Store) path(id string) string { return filepath.Join(s.Dir, "tasks", id+".json") }

func (s Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Join(s.Dir, "tasks"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "tasks.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (s Store) read(id string) (*Task, error) {
	data, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("没有编号为 %s 的任务", id)
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("任务 %s 的记录已损坏", id)
	}
	return &t, nil
}

func (s Store) write(t *Task) error {
	t.Updated = time.Now()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(t.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(t.ID))
}

// Get returns one task.
func (s Store) Get(id string) (*Task, error) {
	if !taskID.MatchString(id) {
		return nil, fmt.Errorf("任务编号无效：%q", id)
	}
	return s.read(id)
}

// List returns all tasks, oldest first.
func (s Store) List() ([]*Task, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "tasks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if t, err := s.read(strings.TrimSuffix(name, ".json")); err == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Update changes one task under the store lock.
func (s Store) Update(id string, fn func(*Task) error) (*Task, error) {
	if !taskID.MatchString(id) {
		return nil, fmt.Errorf("任务编号无效：%q", id)
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	t, err := s.read(id)
	if err != nil {
		return nil, err
	}
	if err := fn(t); err != nil {
		return nil, err
	}
	return t, s.write(t)
}

// reviewParentActive is called under the store lock when claiming or launching
// a task. An orphan review must never start after its parent has moved on.
func (s Store) reviewParentActive(t *Task) bool {
	if t.ReviewOf == "" {
		return true
	}
	parent, err := s.read(t.ReviewOf)
	return err == nil && parent.Status == Reviewing && parent.ReviewTask == t.ID
}

// Dispatch queues a new card for an executor.
func (s Store) Dispatch(cfg Config, lead, executor, folder, id, card string) (*Task, error) {
	return s.DispatchWithEvidence(cfg, lead, executor, folder, id, card, nil, nil)
}
func (s Store) DispatchWithEvidence(cfg Config, lead, executor, folder, id, card string, checks, scope []string) (*Task, error) {
	return s.dispatch(cfg, lead, "", executor, folder, id, card, checks, scope, "")
}

func (s Store) dispatch(cfg Config, lead, project, executor, folder, id, card string, checks, scope []string, reviewOf string) (*Task, error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.dispatchLocked(cfg, lead, project, executor, folder, id, card, checks, scope, reviewOf)
}

func (s Store) dispatchLocked(cfg Config, lead, project, executor, folder, id, card string, checks, scope []string, reviewOf string) (*Task, error) {
	if err := validateEvidence(checks, scope); err != nil {
		return nil, err
	}
	if !taskID.MatchString(id) {
		return nil, fmt.Errorf("任务编号无效：%q（字母数字开头，最多 64 个字符）", id)
	}
	if _, ok := cfg.Agents[executor]; !ok {
		return nil, fmt.Errorf("未知的执行代理：%s", executor)
	}
	dir, err := cfg.allowedFolder(folder)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(card) == "" {
		return nil, errors.New("任务卡内容为空")
	}
	if _, err := os.Stat(s.path(id)); err == nil {
		return nil, fmt.Errorf("任务编号 %s 已存在", id)
	}
	dev, ino, err := folderID(dir)
	if err != nil {
		return nil, fmt.Errorf("无法读取工作目录：%v", err)
	}
	now := time.Now()
	t := &Task{ID: id, Project: project, Lead: lead, Executor: executor, Folder: dir, FolderDev: dev, FolderIno: ino,
		Card: card, Status: Queued, Created: now, Checks: checks, Scope: scope, ReviewOf: reviewOf}
	return t, s.write(t)
}

func validateEvidence(checks, scope []string) error {
	if len(checks) > 10 {
		return errors.New("检查命令不能超过 10 条")
	}
	for _, c := range checks {
		if strings.TrimSpace(c) == "" || len([]rune(c)) > 500 || strings.ContainsRune(c, 0) {
			return errors.New("检查命令无效")
		}
	}
	for _, p := range scope {
		if p == "" || filepath.IsAbs(p) || strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
			return errors.New("允许改动范围无效")
		}
		for _, part := range strings.Split(p, "/") {
			if part == ".." {
				return errors.New("允许改动范围不能包含 ..")
			}
		}
	}
	return nil
}

// RequestRework queues a follow-up in the same executor session.
func (s Store) RequestRework(id, text string) (*Task, error) {
	wasLimited := false
	task, err := s.Update(id, func(t *Task) error {
		if t.Status != Receipt && t.Status != Failed && t.Status != QuotaExhausted {
			return fmt.Errorf("任务 %s 当前为 %s，只有已回执或失败的任务可以返工", id, t.Status)
		}
		if t.Unconfirmed {
			return fmt.Errorf("任务 %s 上次的执行进程可能仍在运行；请先确认它已停止（confirm-stopped）再返工", id)
		}
		if strings.TrimSpace(text) == "" {
			return errors.New("返工说明为空")
		}
		wasLimited = t.Status == QuotaExhausted
		t.Rework, t.Status, t.CancelRequested = text, Queued, false
		return nil
	})
	if err == nil && wasLimited {
		err = s.clearSeparatePause()
	}
	return task, err
}

// Resume continues the same quota-interrupted session.
func (s Store) Resume(id string) (*Task, error) {
	task, err := s.Update(id, func(t *Task) error {
		if t.Status != QuotaExhausted || t.Session == "" || t.Unconfirmed {
			return errors.New("任务没有可继续的已停止会话")
		}
		t.Rework, t.Status, t.CancelRequested = "Continue where you stopped.", Queued, false
		return nil
	})
	if err == nil {
		err = s.clearSeparatePause()
	}
	return task, err
}

func (s Store) clearSeparatePause() error {
	settings, err := ReadSettings(s.Dir)
	if err != nil {
		return err
	}
	if settings.Account != "separate" {
		return nil
	}
	status, err := ReadRunnerStatus(s.Dir)
	if err != nil {
		return err
	}
	if status.Account == "独立账号" && status.Paused {
		return writeJSON(s.Dir, "runner-status.json", RunnerStatus{Account: "独立账号", Reason: "独立账号：开始前不检查额度", CheckedAt: time.Now().UTC()})
	}
	return nil
}

// ConfirmStopped records the user's check that a crashed run's executor is gone.
func (s Store) ConfirmStopped(id string) (*Task, error) {
	return s.Update(id, func(t *Task) error {
		if !t.Unconfirmed {
			return fmt.Errorf("任务 %s 没有待确认的执行进程", id)
		}
		t.Unconfirmed = false
		return nil
	})
}

// Cancel stops a queued task at once, or asks the runner to stop a running one.
func (s Store) Cancel(id string) (*Task, error) {
	var child string
	t, err := s.Update(id, func(t *Task) error {
		switch t.Status {
		case Queued:
			t.Status = Cancelled
		case Running:
			t.CancelRequested = true
		case Reviewing:
			t.Status = Cancelled
			child = t.ReviewTask
		default:
			return fmt.Errorf("任务 %s 已结束（%s），无需停止", id, t.Status)
		}
		return nil
	})
	if err == nil && child != "" {
		_, _ = s.Cancel(child)
	}
	return t, err
}

// Accept closes a task the lead is satisfied with.
func (s Store) Accept(id string) (*Task, error) {
	var child string
	t, err := s.Update(id, func(t *Task) error {
		if t.Status != Receipt && t.Status != Failed && t.Status != Reviewing {
			return fmt.Errorf("任务 %s 当前为 %s，还不能验收", id, t.Status)
		}
		if t.Status == Reviewing {
			child = t.ReviewTask
		}
		t.Status = Accepted
		return nil
	})
	if err == nil && child != "" {
		_, _ = s.Cancel(child)
	}
	return t, err
}

// needsLead reports whether the lead has to act: done, and not waiting for a review.
func (s Store) needsLead(t *Task) bool { return t.Done() && s.pendingReview(t) == "" }

// Wait blocks until the task needs the lead again, or the timeout passes.
func (s Store) Wait(id string, timeout, poll time.Duration) (*Task, error) {
	deadline := time.Now().Add(timeout)
	for {
		t, err := s.Get(id)
		if err != nil || s.needsLead(t) || time.Now().After(deadline) {
			return t, err
		}
		time.Sleep(poll)
	}
}

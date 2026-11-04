package orchestra

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Settings struct {
	Account             string  `json:"account"`
	MinRemaining        float64 `json:"min_remaining"`
	AutoSwitch          bool    `json:"auto_switch"`
	AutoStartRunner     bool    `json:"auto_start_runner"`
	Revision            string  `json:"revision,omitempty"`
	AutoReworkMax       int     `json:"auto_rework_max"`
	CheckTimeoutSeconds int     `json:"check_timeout_seconds"`
}

func DefaultSettings() Settings {
	return Settings{Account: "desktop", MinRemaining: 10, AutoReworkMax: 1, CheckTimeoutSeconds: 600}
}
func ReadSettings(dir string) (Settings, error) {
	s := DefaultSettings()
	b, e := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return s, e
	}
	return s, s.validate()
}
func (s Settings) validate() error {
	if (s.Account != "desktop" && s.Account != "separate") || math.IsNaN(s.MinRemaining) || math.IsInf(s.MinRemaining, 0) || s.MinRemaining < 0 || s.MinRemaining > 100 || s.AutoReworkMax < 0 || s.AutoReworkMax > 2 || s.CheckTimeoutSeconds < 0 || s.CheckTimeoutSeconds > 3600 {
		return errors.New("执行账号或最低额度设置无效")
	}
	return nil
}
func WriteSettings(dir string, s Settings) error {
	unlock, e := lockSettings(dir)
	if e != nil {
		return e
	}
	defer unlock()
	if s.CheckTimeoutSeconds == 0 {
		s.CheckTimeoutSeconds = 600
	}
	if e := s.validate(); e != nil {
		return e
	}
	var revision [16]byte
	if _, e := rand.Read(revision[:]); e != nil {
		return e
	}
	s.Revision = hex.EncodeToString(revision[:])
	return writeJSON(dir, "settings.json", s)
}

func lockSettings(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "settings.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("自动切号进行中，请稍后修改设置")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

type RunnerStatus struct {
	Paused     bool          `json:"paused"`
	Reason     string        `json:"reason,omitempty"`
	Account    string        `json:"account,omitempty"`
	Remaining  *float64      `json:"remaining"`
	CheckedAt  time.Time     `json:"checked_at,omitempty"`
	AutoSwitch *SwitchStatus `json:"auto_switch,omitempty"`
}

type SwitchStatus struct {
	Stage          string            `json:"stage"`
	Source         string            `json:"source,omitempty"`
	Target         string            `json:"target,omitempty"`
	Route          string            `json:"route,omitempty"`
	AttemptID      string            `json:"attempt_id,omitempty"`
	StartedAt      time.Time         `json:"started_at,omitempty"`
	ReviewRequired bool              `json:"review_required,omitempty"`
	Tasks          []string          `json:"tasks,omitempty"`
	ResumeClaims   map[string]string `json:"resume_claims,omitempty"`
	Trigger        string            `json:"trigger,omitempty"`
	HelperPreview  bool              `json:"helper_preview,omitempty"`
}

func ReadSwitchStatus(dir string) (SwitchStatus, error) {
	var s SwitchStatus
	b, e := os.ReadFile(filepath.Join(dir, "auto-switch.json"))
	if errors.Is(e, os.ErrNotExist) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	e = json.Unmarshal(b, &s)
	return s, e
}

func ReadRunnerStatus(dir string) (RunnerStatus, error) {
	var s RunnerStatus
	b, e := os.ReadFile(filepath.Join(dir, "runner-status.json"))
	if errors.Is(e, os.ErrNotExist) {
		if sw, err := ReadSwitchStatus(dir); err != nil {
			return s, err
		} else if sw.Stage != "" {
			s.AutoSwitch = &sw
		}
		return s, nil
	}
	if e != nil {
		return s, e
	}
	e = json.Unmarshal(b, &s)
	if e == nil {
		sw, err := ReadSwitchStatus(dir)
		if err != nil {
			return s, err
		}
		if sw.Stage != "" {
			s.AutoSwitch = &sw
		}
	}
	return s, e
}
func writeJSON(dir, name string, v any) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".orchestra-*.tmp")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(dir, name)); e != nil {
		return fmt.Errorf("保存状态失败: %w", e)
	}
	if d, err := os.Open(dir); err == nil {
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return err
		}
	} else {
		return err
	}
	return nil
}

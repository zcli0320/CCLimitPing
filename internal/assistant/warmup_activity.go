package assistant

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Recent rollout activity is deliberately conservative. It is a reason to
// postpone a synthetic request, never proof that a particular live identity
// owns a task. A normal user request will start the window without our ping.
func (r Reader) recentActivity(sources []string, now time.Time) bool {
	busy, err := r.activityStatus(sources, now)
	return busy || err != nil
}

func (r Reader) activityStatus(sources []string, now time.Time) (bool, error) {
	if len(sources) == 0 {
		return false, errors.New("账号活动来源未知，暂缓预热")
	}
	// A stashed login does not identify a running client. Conservatively check
	// both session roots instead of assuming the backup account is idle.
	for _, source := range sources {
		if source == "CC Switch 暂存" || source == "小助手缓存" {
			return r.activityStatus([]string{"Desktop", "CLI"}, now)
		}
	}
	for _, source := range sources {
		if source != "Desktop" && source != "CLI" {
			return false, errors.New("账号活动来源未知，暂缓预热")
		}
		dir := ".codex"
		if source == "CLI" {
			dir = ".codex-cli"
		}
		root := filepath.Join(r.Home, dir, "sessions")
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			if os.IsNotExist(err) {
				// A genuinely absent history root is normal for an unused client.
				// An existing broken link or inaccessible directory is unknown.
				if _, statErr := os.Lstat(root); os.IsNotExist(statErr) {
					continue
				}
			}
			return false, errors.New(source + " 会话目录无法读取，活动状态未知；暂缓预热")
		}
		root = real
		busy := false
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if now.Sub(info.ModTime()) > 5*time.Minute {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			start := info.Size() - (256 << 10)
			if start < 0 {
				start = 0
			}
			if _, err := file.Seek(start, io.SeekStart); err != nil {
				_ = file.Close()
				return err
			}
			data, err := io.ReadAll(io.LimitReader(file, 256<<10))
			_ = file.Close()
			if err != nil {
				return err
			}
			found, open := false, false
			for _, line := range bytes.Split(data, []byte("\n")) {
				var event struct {
					Type    string `json:"type"`
					Payload struct {
						Type string `json:"type"`
					} `json:"payload"`
				}
				if json.Unmarshal(line, &event) != nil || event.Type != "event_msg" {
					continue
				}
				switch event.Payload.Type {
				case "task_started":
					found = true
					open = true
				case "task_complete", "turn_aborted":
					found = true
					open = false
				}
			}
			if !found || open {
				busy = true
				return fs.SkipAll
			}
			return nil
		})
		if walkErr != nil {
			return false, errors.New(source + " 会话记录无法读取，活动状态未知；暂缓预热")
		}
		if busy {
			return true, nil
		}
	}
	return false, nil
}

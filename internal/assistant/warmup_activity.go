package assistant

import (
	"bytes"
	"encoding/json"
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
	// A stashed login does not identify a running client. Conservatively check
	// both session roots instead of assuming the backup account is idle.
	for _, source := range sources {
		if source == "CC Switch 暂存" {
			return r.recentActivity([]string{"Desktop", "CLI"}, now)
		}
	}
	for _, source := range sources {
		dir := ".codex"
		if source == "CLI" {
			dir = ".codex-cli"
		}
		root := filepath.Join(r.Home, dir, "sessions")
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		busy := false
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil || now.Sub(info.ModTime()) > 5*time.Minute {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				busy = true
				return fs.SkipAll
			}
			start := info.Size() - (256 << 10)
			if start < 0 {
				start = 0
			}
			_, _ = file.Seek(start, io.SeekStart)
			data, err := io.ReadAll(io.LimitReader(file, 256<<10))
			_ = file.Close()
			if err != nil {
				busy = true
				return fs.SkipAll
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
		if busy {
			return true
		}
	}
	return false
}

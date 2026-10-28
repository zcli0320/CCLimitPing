package assistant

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
)

const resumeRolloutTail = 8 << 20

// rolloutProvesCompleted is an additional, read-only fence against a stale
// Desktop owner snapshot. It never treats the file alone as a runtime status.
// The source must be the exact session, stable during the read, and its latest
// turn must have a matching task_complete marker. Missing or partial evidence
// fails closed, including logs whose latest relevant event is outside the tail.
func rolloutProvesCompleted(root, path, sessionID, turnID string) error {
	invalid := func() error { return errors.New("原会话记录未确认准确轮次已结束") }
	if !validHandoffText(sessionID) || !validHandoffText(turnID) || !strings.HasSuffix(path, ".jsonl") || !trustedRollout(root, path) {
		return invalid()
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return invalid()
	}
	f := os.NewFile(uintptr(fd), "rollout")
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() == 0 {
		return invalid()
	}
	var lastByte [1]byte
	if _, err := f.ReadAt(lastByte[:], before.Size()-1); err != nil || lastByte[0] != '\n' {
		return invalid()
	}
	// Check the file's own session metadata rather than trusting its pathname.
	head := bufio.NewScanner(io.NewSectionReader(f, 0, min(before.Size(), resumeRolloutTail)))
	head.Buffer(make([]byte, 4096), resumeRolloutTail)
	if !head.Scan() {
		return invalid()
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(head.Bytes(), &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != sessionID {
		return invalid()
	}
	start := max(int64(0), before.Size()-resumeRolloutTail)
	reader := bufio.NewReader(io.NewSectionReader(f, start, before.Size()-start))
	if start > 0 {
		// The first bytes may be the middle of a large JSON line.
		if _, err := reader.ReadBytes('\n'); err != nil {
			return invalid()
		}
	}
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), resumeRolloutTail)
	latestStarted, latestCompleted, lastEvent := "", "", ""
	aborted := false
	for scan.Scan() {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			return invalid()
		}
		if record.Type != "event_msg" {
			continue
		}
		switch record.Payload.Type {
		case "task_started":
			latestStarted, latestCompleted, lastEvent = record.Payload.TurnID, "", "task_started"
			aborted = false
		case "task_complete", "turn_aborted":
			latestCompleted, lastEvent = record.Payload.TurnID, record.Payload.Type
			aborted = aborted || record.Payload.Type == "turn_aborted"
		}
	}
	if scan.Err() != nil || aborted || latestStarted != turnID || latestCompleted != turnID || lastEvent != "task_complete" {
		return invalid()
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return invalid()
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(after, pathInfo) {
		return invalid()
	}
	return nil
}

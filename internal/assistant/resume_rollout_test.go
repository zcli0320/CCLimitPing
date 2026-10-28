package assistant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeRolloutRequiresExactFinishedTurn(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "2026", "synthetic.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"session"}}` + "\n"
	start := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}` + "\n"
	complete := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}` + "\n"
	newStart := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"new"}}` + "\n"
	aborted := `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn"}}` + "\n"
	for _, tc := range []struct {
		name, data, session, turn string
		want                      bool
	}{
		{"matching completion", meta + start + complete, "session", "turn", true},
		{"newer running turn", meta + start + complete + newStart, "session", "turn", false},
		{"uncompleted turn", meta + start, "session", "turn", false},
		{"user interrupted", meta + start + aborted + complete, "session", "turn", false},
		{"different session", meta + start + complete, "different", "turn", false},
		{"different turn", meta + start + complete, "session", "different", false},
		{"partial final line", meta + start + complete + `{"type":`, "session", "turn", false},
		{"malformed complete line", meta + start + complete + `{broken}` + "\n", "session", "turn", false},
		{"no start marker", meta + complete, "session", "turn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFixture(t, path, tc.data)
			err := rolloutProvesCompleted(root, path, tc.session, tc.turn)
			if (err == nil) != tc.want {
				t.Fatalf("completion evidence accepted=%t, want %t", err == nil, tc.want)
			}
		})
	}
}

func TestResumeRolloutRejectsUntrustedAndSymlinkPaths(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "sessions", "fixture.jsonl")
	data := `{"type":"session_meta","payload":{"id":"session"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}` + "\n"
	writeFixture(t, good, data)
	other := filepath.Join(t.TempDir(), "outside.jsonl")
	writeFixture(t, other, data)
	if rolloutProvesCompleted(root, other, "session", "turn") == nil {
		t.Fatal("a rollout outside the selected home was accepted")
	}
	link := filepath.Join(root, "sessions", "link.jsonl")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if rolloutProvesCompleted(root, link, "session", "turn") == nil {
		t.Fatal("a symlink was accepted as a live rollout")
	}
}

func TestResumeRolloutReadsRecentTailOfLargeFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "large.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"session"}}` + "\n"
	large := `{"type":"response_item","payload":{"content":"` + strings.Repeat("x", resumeRolloutTail+100) + `"}}` + "\n"
	start := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}` + "\n"
	end := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}` + "\n"
	writeFixture(t, path, meta+large+start+end)
	if err := rolloutProvesCompleted(root, path, "session", "turn"); err != nil {
		t.Fatal("valid recent turn after a large line was not found")
	}
}

func TestResumeRejectsStaleSnapshotEvenWhenQuotaReady(t *testing.T) {
	r, f, d := resumeFixture(t)
	checks := 0
	d.verifyRollout = func(desktopConversation, resumeTurn) error {
		checks++
		if checks == 1 {
			return nil
		}
		return os.ErrInvalid // a new task started after the first observation
	}
	status, err := r.checkResume(t.Context(), d)
	if err != nil || status.Status != "waiting_client" || f.calls != 0 || len(status.Handled) != 0 {
		t.Fatal("a stale owner snapshot led to a continuation or durable claim")
	}
}

package assistant

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type brokenTerminal struct{}

func (brokenTerminal) Write([]byte) (int, error) { return 0, errors.New("closed PTY") }
func cliResumeFixture(t *testing.T) (Reader, ResumeReport) {
	t.Helper()
	r := warmupFixtureReader(t)
	c, e := r.scopeCredential("cli", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	p := ResumeReport{Enabled: true, Scope: "cli", ID: "cli-thread", Key: "cli-key", Identity: credentialIdentity(c), Handled: map[string]bool{}}
	if e = writeState(r.resumePath(), p); e != nil {
		t.Fatal(e)
	}
	return r, p
}
func TestCLIContinuationOnlyOwnedUneditedTerminalAndDedup(t *testing.T) {
	for _, kind := range []string{"send", "typing", "manual_input", "disabled", "different_target", "identity_changed", "canceled", "broken_pipe"} {
		t.Run(kind, func(t *testing.T) {
			r, p := cliResumeFixture(t)
			input := &cliInputState{epoch: 10}
			var output bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "typing":
				input.typing = true
			case "manual_input":
				input.epoch = 11
			case "disabled":
				p.Enabled = false
			case "different_target":
				p.ID = "other"
			case "identity_changed":
				p.Identity = "other"
			case "canceled":
				cancel()
			}
			_ = writeState(r.resumePath(), p)
			if kind == "broken_pipe" {
				_, _ = r.submitCLIResume(ctx, "cli-thread", "failed-turn", p.Identity, 10, brokenTerminal{}, input)
				if !r.ResumeStatus().Handled["failed-turn"] {
					t.Fatal("ambiguous PTY write not registered")
				}
				_, _ = r.submitCLIResume(ctx, "cli-thread", "failed-turn", p.Identity, 10, &output, input)
				if output.Len() != 0 {
					t.Fatal("ambiguous PTY write retried")
				}
				return
			}
			ok, e := r.submitCLIResume(ctx, "cli-thread", "failed-turn", p.Identity, 10, &output, input)
			if kind == "send" {
				if e != nil || !ok || !strings.HasSuffix(output.String(), "\r") || !strings.Contains(output.String(), "\x1b[200~") {
					t.Fatalf("paste/submit failed %v", e)
				}
				n := output.Len()
				_, _ = r.submitCLIResume(ctx, "cli-thread", "failed-turn", p.Identity, 10, &output, input)
				if output.Len() != n {
					t.Fatal("duplicate CLI continuation")
				}
			} else if output.Len() != 0 || ok {
				t.Fatal("unsafe CLI input submitted")
			}
		})
	}
}
func TestCLIThreadUsesStructuredErrorOnExactBackend(t *testing.T) {
	socket, close := fakeCLIWebSocket(t, true)
	defer close()
	turn, limited, e := cliThread(context.Background(), socket, "cli-thread", "openai")
	if e != nil || !limited || turn.ID != "failed-turn" {
		t.Fatalf("structured detection: %v", e)
	}
	if _, limited, e = cliThread(context.Background(), socket, "other-thread", "openai"); e == nil || limited {
		t.Fatal("wrong CLI thread accepted")
	}
}
func TestCLIEnvironmentKeepsSourceIsolated(t *testing.T) {
	t.Setenv("CODEX_HOME", "/wrong/home")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	t.Setenv("CODEX_THREAD_ID", "wrong-thread")
	joined := strings.Join(cliEnvironment("/test-home"), "\n")
	if !strings.Contains(joined, "CODEX_HOME=/test-home/.codex-cli") || strings.Contains(joined, "must-not-inherit") || strings.Contains(joined, "wrong-thread") || strings.Contains(joined, "/wrong/home") {
		t.Fatal("CLI environment not isolated")
	}
}

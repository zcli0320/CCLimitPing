package assistant

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnedBackendNeverRepeatsShutdownSignal(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	count := filepath.Join(dir, "signals")
	script := filepath.Join(dir, "fixture.py")
	program := `import signal,sys,time
ready,count=sys.argv[1:]
def handle(signum, frame):
    with open(count, 'a') as f: f.write('term\n')
signal.signal(signal.SIGTERM, handle)
open(ready, 'w').close()
while True: time.sleep(0.05)
`
	if err := os.WriteFile(script, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	p, _, err := startOwnedCLIProcess(exec.Command("/usr/bin/python3", script, ready, count), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = p.cmd.Process.Kill() // only this isolated, deliberately stuck fixture
		<-p.done
	}()
	for deadline := time.Now().Add(3 * time.Second); ; {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		if err := p.stopWithTimeout(context.Background(), 100*time.Millisecond); err == nil {
			t.Fatal("a backend that did not exit was treated as stopped")
		}
	}
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "term\n" {
		t.Fatalf("expected one shutdown signal, got %q (%v)", data, err)
	}
}

func TestStaleSupervisorCannotHideALiveBackend(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	if err := writeState(r.cliRunnerPath(), cliRunnerState{ID: "thread", PID: 0, ServerPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNoLivePreviousCLIRunner(); err == nil {
		t.Fatal("a live backend was accepted because its supervisor was gone")
	}
	if err := os.Remove(r.cliRunnerPath()); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNoLivePreviousCLIRunner(); err != nil {
		t.Fatal(err)
	}
}

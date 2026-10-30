package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDesktopRunnerCompletesIsolatedFlow(t *testing.T) {
	r, coordinator, runtime := desktopCoordinatorFixture(t)
	authorization := r.HandoffStatus().AuthorizationID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deps := desktopRunnerDependencies{
		makeRuntime:  func(context.Context, string) (desktopHandoffRuntime, error) { return runtime, nil },
		resetRuntime: func(context.Context, desktopHandoffRuntime) error { cancel(); return nil },
		quota:        coordinator.quota, tick: 10 * time.Millisecond, watchInterval: 10 * time.Millisecond,
	}
	err := r.runDesktopHandoff(ctx, "session", 10, authorization, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected runner result: %v stage=%s actions=%v", err, r.HandoffStatus().Stage, runtime.actions)
	}
	if !reflect.DeepEqual(runtime.actions, []string{"stop", "exit", "restart", "continue"}) {
		t.Fatalf("unexpected actions %v", runtime.actions)
	}
	raw, _ := os.ReadFile(filepath.Join(r.Home, ".codex/auth.json"))
	if !r.handoffCurrentMatchesAccount(raw, "managed-b") {
		t.Fatal("target credential was not installed")
	}
	if _, err := os.Stat(r.handoffAuthTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("verified recovery record retained")
	}
	if _, err := os.Stat(r.desktopRunnerPath()); !os.IsNotExist(err) {
		t.Fatal("runner status was not removed")
	}
}

func TestDesktopRunnerDoesNotRetryUncertainStop(t *testing.T) {
	r, coordinator, runtime := desktopCoordinatorFixture(t)
	runtime.fail = "stop"
	deps := desktopRunnerDependencies{makeRuntime: func(context.Context, string) (desktopHandoffRuntime, error) { return runtime, nil }, resetRuntime: func(context.Context, desktopHandoffRuntime) error { return nil }, quota: coordinator.quota, tick: 10 * time.Millisecond, watchInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.runDesktopHandoff(ctx, "session", 10, r.HandoffStatus().AuthorizationID, deps); err == nil {
		t.Fatal("ambiguous stop reported success")
	}
	if !reflect.DeepEqual(runtime.actions, []string{"stop"}) || r.HandoffStatus().Stage != "uncertain" {
		t.Fatal("ambiguous stop was repeated or cleared")
	}
}

func TestDesktopActivationRequiresExplicitSelectedSession(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	id := "00000000-0000-7000-8000-000000000001"
	if _, err := r.PrepareDesktopHandoff(context.Background(), id, 10); err == nil {
		t.Fatal("unavailable session was enabled")
	}
	if err := r.RunDesktopHandoff(context.Background(), id, 10, "test"); err == nil {
		t.Fatal("unauthorized runner was started")
	}
}

func TestDesktopActivationRequiresRuntimeProofAndLoadedSession(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preflight DesktopHandoffPreflight
		wantReady bool
	}{
		{"missing runtime proof", DesktopHandoffPreflight{Ready: true}, false},
		{"unready session", DesktopHandoffPreflight{RuntimeIdentityVerified: true}, false},
		{"ready", DesktopHandoffPreflight{Ready: true, RuntimeIdentityVerified: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requireReadyDesktopPreflight(tc.preflight) == nil; got != tc.wantReady {
				t.Fatalf("ready=%v; want %v", got, tc.wantReady)
			}
		})
	}
}

func TestDesktopRunnerRejectsDifferentAuthorizationBeforeNativeControl(t *testing.T) {
	r, coordinator, runtime := desktopCoordinatorFixture(t)
	called := false
	deps := desktopRunnerDependencies{makeRuntime: func(context.Context, string) (desktopHandoffRuntime, error) { called = true; return runtime, nil }, resetRuntime: func(context.Context, desktopHandoffRuntime) error { return nil }, quota: coordinator.quota, tick: time.Millisecond, watchInterval: time.Millisecond}
	if err := r.runDesktopHandoff(context.Background(), "session", 10, "different-authorization", deps); err == nil || called {
		t.Fatal("native control reached without matching authorization")
	}
	if err := r.runDesktopHandoff(context.Background(), "different-session", 10, r.HandoffStatus().AuthorizationID, deps); err == nil || called {
		t.Fatal("changed selection reached native control")
	}
}

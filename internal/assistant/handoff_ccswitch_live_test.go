package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Explicit opt-in real Desktop experiment at an adjusted configurable threshold.
// All quota responses, CC Switch actions, auth files and processes are real.
// Run independently of the Desktop process tree so it can verify exit.
func TestLiveDesktopViaCCSwitch(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_FULL_DESKTOP_THREAD")
	control := os.Getenv("CODEX_HELPER_LIVE_FULL_DESKTOP_CONTROL")
	if id == "" || control == "" || os.Getenv("CODEX_HELPER_LIVE_FULL_DESKTOP_CONFIRM") != "run-full-handoff-now" {
		t.Skip("explicit full Desktop experiment not requested")
	}
	trigger := os.Getenv("CODEX_HELPER_LIVE_FULL_DESKTOP_TRIGGER")
	if !desktopHandoffUUID.MatchString(id) || !filepath.IsAbs(control) || (trigger != "configured-threshold" && trigger != "simulate-source-low") {
		t.Fatal("explicit experiment parameters required")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("home unavailable")
	}
	if r.HandoffStatus().Enabled || r.ResumeStatus().Enabled {
		t.Fatal("another automatic operation is enabled")
	}
	source, target, err := r.desktopHandoffAccounts()
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		t.Fatal("provider unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	b := &desktopCCSwitch{r: r, control: control}
	b.run = func(ctx context.Context, action, name string) error {
		out, err := exec.CommandContext(ctx, control, "--ccswitch-"+action, name).Output()
		if err != nil || len(out) > 4096 {
			return errors.New("native control failed")
		}
		var reply struct {
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		}
		if json.Unmarshal(out, &reply) != nil || !reply.OK {
			return errors.New("native control was not ready")
		}
		return nil
	}
	if err := b.inspect(ctx, target); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := newDesktopHandoffLifecycle(ctx, r.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range lifecycle.oldTree {
		if p.PID == os.Getpid() {
			t.Fatal("experiment must be detached from the Desktop process tree")
		}
	}
	runtime := &desktopSingleSessionRuntime{r: r, id: id, provider: provider, lifecycle: lifecycle, wholeAppExitAccepted: true}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	credentials := r.quotaCredentials(time.Now())
	sourceCredential, ok := credentials[source]
	if !ok {
		t.Fatal("source access unavailable")
	}
	targetCredential, ok := credentials[target]
	if !ok {
		t.Fatal("real target access unavailable")
	}
	sourceQuota := queryAccountQuota(ctx, client, sourceCredential)
	targetQuota := queryAccountQuota(ctx, client, targetCredential)
	sourceRemaining, sourceKnown := handoffWindow(&sourceQuota, 18000)
	targetRemaining, targetKnown := handoffWindow(&targetQuota, 18000)
	threshold := (sourceRemaining + targetRemaining) / 2
	if trigger == "simulate-source-low" {
		threshold = 10
	}
	if !sourceKnown || !targetKnown || !validHandoffThreshold(threshold) || (trigger == "configured-threshold" && sourceRemaining >= threshold) || !handoffTargetQuotaOK(targetQuota, threshold) {
		t.Fatal("real target quota unavailable")
	}
	t.Logf("quota trigger mode=%s: real source=%.1f target=%.1f threshold=%.1f", trigger, sourceRemaining, targetRemaining, threshold)
	query := func(ctx context.Context, c quotaCredential) AccountQuota {
		q := queryAccountQuota(ctx, client, c)
		if trigger == "simulate-source-low" && q.Status == "ready" && credentialIdentity(c) == credentialIdentity(sourceCredential) {
			for i := range q.Windows {
				if q.Windows[i].Seconds == 18000 {
					q.Windows[i].UsedPercent = 99
				}
			}
		}
		return q
	}
	state, err := r.ConfigureHandoff("desktop", id, threshold, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r.HandoffStatus().Enabled {
			_, _ = r.ConfigureHandoff("", "", 0, false)
		}
	}()
	deps := desktopRunnerDependencies{ccswitch: b, makeRuntime: func(context.Context, string) (desktopHandoffRuntime, error) { return runtime, nil }, resetRuntime: func(context.Context, desktopHandoffRuntime) error { cancel(); return nil }, quota: query, tick: time.Second, watchInterval: time.Second}
	err = r.runDesktopHandoff(ctx, id, threshold, state.AuthorizationID, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("real experiment did not complete: %v", err)
	}
	if _, err := r.ccSwitchLiveMatches(target); err != nil {
		t.Fatal("new live account not confirmed")
	}
	if _, err := os.Stat(r.ccSwitchTransactionPath()); !os.IsNotExist(err) {
		t.Fatal("recovery record was not cleared after verification")
	}
	t.Log("real CC Switch handoff and original conversation continuation verified; trigger mode=" + trigger)
}

// On a failed experiment the external launcher can reopen the original chat
// and send one recovery message only if no turn is running. It never retries an
// unconfirmed delivery or changes authentication.
func TestLiveDesktopRecoveryResume(t *testing.T) {
	id := os.Getenv("CODEX_HELPER_LIVE_RECOVERY_THREAD")
	evidence := os.Getenv("CODEX_HELPER_LIVE_RECOVERY_EVIDENCE")
	if id == "" || evidence == "" {
		t.Skip("explicit recovery not requested")
	}
	if !desktopHandoffUUID.MatchString(id) || !filepath.IsAbs(evidence) {
		t.Fatal("invalid recovery parameters")
	}
	r, err := NewReader()
	if err != nil {
		t.Fatal("home unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < 20; i++ {
		c, err := connectDesktop(ctx, r.Home)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		v, owner, err := c.snapshot(id)
		if err != nil {
			c.conn.Close()
			time.Sleep(time.Second)
			continue
		}
		turn, ok := v.latest()
		if !ok || v.Runtime.Type == "active" || turn.Status == "inProgress" {
			c.conn.Close()
			t.Log("conversation already active; recovery message not sent")
			return
		}
		if v.Runtime.Type != "idle" || v.ResumeState != "resumed" || len(v.Requests) != 0 {
			c.conn.Close()
			t.Fatal("conversation is not safely idle")
		}
		marker := filepath.Join(evidence, "recovery-message-registered")
		f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			c.conn.Close()
			t.Fatal("recovery message already registered")
		}
		f.Close()
		_, err = c.startDesktopTurn(id, owner, turn, "继续本次小助手开发任务。自动切号实验已经结束，请先读取 "+evidence+" 中的结果和日志；未验证成功时保留禁用状态及恢复记录，不要重复切号。")
		c.conn.Close()
		if err != nil {
			t.Fatal("recovery delivery unconfirmed; will not resend")
		}
		return
	}
	t.Fatal("Desktop recovery connection unavailable")
}

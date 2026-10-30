package assistant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func authorizeWarmupA(t *testing.T, r Reader) {
	t.Helper()
	if _, err := r.ConfigureWarmup(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	p, _, err := r.readWarmupState()
	if err != nil {
		t.Fatal(err)
	}
	p.Allowed = map[string]bool{privateKey("managed-a"): true}
	if err = writeState(filepath.Join(r.warmupRoot(), "policy.json"), p); err != nil {
		t.Fatal(err)
	}
}

func TestWarmupUnknownResetCannotBecomeEligible(t *testing.T) {
	for _, mode := range []string{"absent", "invalid-timestamp", "negative-countdown", "oversized-countdown"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			q := testQuota(now, false)
			q.Windows[0].ResetAt, q.Windows[0].ResetAfter = nil, nil
			v := int64(0)
			switch mode {
			case "invalid-timestamp":
				q.Windows[0].ResetAt = &v
			case "negative-countdown":
				v = -1
				q.Windows[0].ResetAfter = &v
			case "oversized-countdown":
				v = 18001
				q.Windows[0].ResetAfter = &v
			}
			record := &WarmupAccount{IdleObserved: now.Add(-time.Minute)}
			for _, at := range []time.Time{now, now.Add(15 * time.Second)} {
				if decideWarmup(q, record, at, false) || record.Status != "window_unknown" || !record.IdleObserved.IsZero() {
					t.Fatal("unknown reset became eligible or retained stale idle evidence")
				}
			}
		})
	}
}

func TestWarmupRechecksCachedRunningWindow(t *testing.T) {
	r := warmupFixtureReader(t)
	authorizeWarmupA(t, r)
	now := time.Now()
	_, journal, err := r.readWarmupState()
	if err != nil {
		t.Fatal(err)
	}
	key := credentialIdentity(r.quotaCredentials(now)["managed-a"])
	journal.Accounts[key] = &WarmupAccount{
		Key: key, Status: "window_running", LastCheck: now.Add(-3 * time.Hour),
		NextCheck: now.Add(time.Hour),
	}
	if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), journal); err != nil {
		t.Fatal(err)
	}
	queries := 0
	d := warmupDependencies{now: func() time.Time { return now }, quota: func(context.Context, quotaCredential) AccountQuota {
		queries++
		return testQuota(now, false)
	}, busy: func([]string) bool { return false }, run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
		t.Fatal("one idle observation must not submit")
		return WarmupExecution{}, nil
	}}
	report, err := r.checkWarmup(context.Background(), "", d)
	if err != nil {
		t.Fatal(err)
	}
	if queries != 1 || len(report.Accounts) != 1 || report.Accounts[0].Status != "confirming_idle" {
		t.Fatalf("stale running status was not rechecked: queries=%d report=%+v", queries, report.Accounts)
	}
}

func TestWarmupZeroUsageRequiresStableOrMovingDeadlineEvidence(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	partial := func(at time.Time, remaining int64) AccountQuota {
		q := testQuota(at, false)
		reset := at.Add(time.Duration(remaining) * time.Second).Unix()
		q.Windows[0].ResetAt = &reset
		q.Windows[0].ResetAfter = &remaining
		return q
	}
	t.Run("moving placeholder is idle", func(t *testing.T) {
		r := &WarmupAccount{}
		if decideWarmup(partial(now, 17000), r, now, false) || r.Status == "window_running" {
			t.Fatal("one zero-usage countdown cannot prove a running window")
		}
		later := now.Add(time.Minute)
		if !decideWarmup(partial(later, 17000), r, later, false) {
			t.Fatalf("moving placeholder was not recognized as idle: %s", r.Status)
		}
		if !decideWarmup(partial(later, 17000), r, later, false) {
			t.Fatal("final same-tick pre-submit check lost the idle evidence")
		}
	})
	t.Run("fixed deadline is a running window", func(t *testing.T) {
		r := &WarmupAccount{}
		if decideWarmup(partial(now, 17000), r, now, false) || r.Status == "window_running" {
			t.Fatal("one zero-usage countdown cannot prove a running window")
		}
		later := now.Add(time.Minute)
		if decideWarmup(partial(later, 16940), r, later, false) || r.Status != "window_running" {
			t.Fatal("stable deadline was not recognized as a running window")
		}
	})
}

func TestWarmupReportDoesNotPresentStaleWindowAsCurrent(t *testing.T) {
	r := warmupFixtureReader(t)
	now := time.Now()
	j := warmupJournal{Version: 1, Accounts: map[string]*WarmupAccount{
		"test": {Status: "window_running", Message: "五小时窗口已运行", LastCheck: now.Add(-time.Hour)},
	}}
	report := r.warmupReport(warmupPolicy{Version: 1, Enabled: true}, j)
	if len(report.Accounts) != 1 || report.Accounts[0].Status == "window_running" {
		t.Fatal("stale status was displayed as a current running window")
	}
	if j.Accounts["test"].Status != "window_running" {
		t.Fatal("rendering a report changed the saved journal")
	}
}

func TestWarmupZeroUsagePostRequestProof(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	first := testQuota(now, false).Windows[0]
	placeholder := testQuota(now.Add(3*time.Second), false).Windows[0]
	if zeroUsageWindowStable(first, now, placeholder, now.Add(3*time.Second)) {
		t.Fatal("sliding placeholder was accepted as successful warmup evidence")
	}
	countdown := testQuota(now.Add(3*time.Second), false).Windows[0]
	after := int64(17997)
	reset := now.Add(5 * time.Hour).Unix()
	countdown.ResetAfter, countdown.ResetAt = &after, &reset
	if !zeroUsageWindowStable(first, now, countdown, now.Add(3*time.Second)) {
		t.Fatal("stable, decreasing countdown was not recognized")
	}
	if zeroUsageWindowStable(first, now, countdown, now.Add(time.Second)) {
		t.Fatal("insufficient observation time was accepted")
	}
	invalid := int64(18001)
	countdown.ResetAfter = &invalid
	if zeroUsageWindowStable(first, now, countdown, now.Add(3*time.Second)) {
		t.Fatal("invalid reset metadata was accepted as proof")
	}
}

func TestWarmupRetainsVerifiedCompletionOnlyForSameRequestAndWindow(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	remaining := int64(17880)
	reset := now.Add(time.Duration(remaining) * time.Second).Unix()
	q := testQuota(now, false)
	q.Windows[0].ResetAfter, q.Windows[0].ResetAt = &remaining, &reset
	for _, tc := range []struct {
		name      string
		confirmed int
		deadline  int64
		want      string
	}{
		{"same verified request and window", 1, reset, "confirmed"},
		{"older verified request", 2, reset, "window_running"},
		{"different window", 1, reset - 1000, "window_running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &WarmupAccount{Status: "confirmed", Attempts: 1, ConfirmedAttempt: tc.confirmed,
				LastCheck: now.Add(-time.Minute), LastAttempt: now.Add(-2 * time.Minute),
				WindowReset: tc.deadline, NoRetryBefore: now.Add(time.Hour),
				Execution: &WarmupExecution{Submitted: true, Completed: true, IdentityVerified: true}}
			// A previous fixed-deadline observation is enough for a fresh
			// comparison; otherwise establish the new sample explicitly.
			if tc.confirmed != 1 || tc.deadline != reset {
				r.ZeroSampleAt, r.ZeroDeadline = now.Add(-time.Minute), reset
			}
			if decideWarmup(q, r, now, false) || r.Status != tc.want {
				t.Fatalf("expected %s, got %s (%s)", tc.want, r.Status, r.Message)
			}
		})
	}
}

func TestWarmupStaleWindowRecheckRespectsPriorRequestCooldown(t *testing.T) {
	r := warmupFixtureReader(t)
	authorizeWarmupA(t, r)
	now := time.Now()
	_, journal, err := r.readWarmupState()
	if err != nil {
		t.Fatal(err)
	}
	key := credentialIdentity(r.quotaCredentials(now)["managed-a"])
	journal.Accounts[key] = &WarmupAccount{Key: key, Status: "window_running", LastCheck: now.Add(-time.Hour),
		NextCheck: now.Add(time.Hour), NoRetryBefore: now.Add(4 * time.Hour), Attempts: 1}
	if err := writeState(filepath.Join(r.warmupRoot(), "journal.json"), journal); err != nil {
		t.Fatal(err)
	}
	d := warmupDependencies{now: func() time.Time { return now }, quota: func(context.Context, quotaCredential) AccountQuota {
		return testQuota(now, false)
	}, busy: func([]string) bool { return false }, run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
		t.Fatal("recheck retried a previously submitted request")
		return WarmupExecution{}, nil
	}}
	report, err := r.checkWarmup(context.Background(), "", d)
	if err != nil || len(report.Accounts) != 1 || report.Accounts[0].Status != "verification_pending" || report.Accounts[0].Attempts != 1 {
		t.Fatalf("cooldown was not retained: report=%+v error=%v", report.Accounts, err)
	}
}

func TestWarmupRunningWindowDoesNotProveUnfinishedRequest(t *testing.T) {
	r := warmupFixtureReader(t)
	now := time.Now()
	d := warmupDependencies{now: func() time.Time { return now }, quota: func(context.Context, quotaCredential) AccountQuota {
		return testQuota(now, true)
	}, run: func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		if err := claim(); err != nil {
			return WarmupExecution{}, err
		}
		return WarmupExecution{Submitted: true, IdentityVerified: true}, nil
	}}
	report, err := r.checkWarmup(context.Background(), "managed-a", d)
	if err != nil || len(report.Accounts) != 1 || report.Accounts[0].Status != "window_running_uncertain_request" {
		t.Fatalf("an unfinished request was marked successful: report=%+v error=%v", report.Accounts, err)
	}
}

func TestWarmupUnconfirmedZeroUsageRequestIsNotReportedSuccessful(t *testing.T) {
	r := warmupFixtureReader(t)
	authorizeWarmupA(t, r)
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	submitted := false
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota {
		q := testQuota(now, false)
		if submitted {
			// This shorter zero-usage countdown used to be treated as
			// proof of success after a single reading.
			after := int64(17000)
			q.Windows[0].ResetAfter = &after
			cancel()
		}
		return q
	}, run: func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		if err := claim(); err != nil {
			return WarmupExecution{}, err
		}
		submitted = true
		return WarmupExecution{Submitted: true, Completed: true, IdentityVerified: true}, nil
	}}
	if _, err := r.checkWarmup(ctx, "", d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil || len(report.Accounts) != 1 || report.Accounts[0].Status != "verification_pending" || report.Accounts[0].Attempts != 1 {
		t.Fatalf("ambiguous zero-usage observation was reported successful: report=%+v error=%v", report.Accounts, err)
	}
}

func TestWarmupMissingActivityRootAndBrokenLinkAreDistinct(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	if busy, err := r.activityStatus([]string{"Desktop", "CLI"}, time.Now()); busy || err != nil {
		t.Fatal("unused client must not be an activity error")
	}
	root := filepath.Join(r.Home, ".codex", "sessions")
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.Home, "missing-target"), root); err != nil {
		t.Fatal(err)
	}
	if busy, err := r.activityStatus([]string{"Desktop"}, time.Now()); busy || err == nil || !r.recentActivity([]string{"Desktop"}, time.Now()) {
		t.Fatal("broken history link was accepted as idle")
	}
	if busy, err := r.activityStatus([]string{"小助手缓存"}, time.Now()); busy || err == nil {
		t.Fatal("cached credential ignored unreadable client history")
	}
}

func TestWarmupUnreadableActivityPostponesWithoutSubmitting(t *testing.T) {
	r := warmupFixtureReader(t)
	authorizeWarmupA(t, r)
	root := filepath.Join(r.Home, ".codex", "sessions")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if f, err := os.Open(root); err == nil {
		f.Close()
		t.Skip("process bypasses permission fixture")
	}
	now := time.Now()
	d := warmupDependencies{now: func() time.Time { return now }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, false) }, activity: func(s []string) (bool, error) { return r.activityStatus(s, now) }, run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
		t.Fatal("unreadable activity submitted a request")
		return WarmupExecution{}, nil
	}}
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(context.Background(), "", d)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].Status != "activity_unknown" || !strings.Contains(report.Accounts[0].Message, "未知") || report.Accounts[0].Attempts != 0 {
		t.Fatal("activity error was not retained as a blocked account status")
	}
}

func TestWarmupActivityBecomingUnreadableAtSubmitCancelsClaim(t *testing.T) {
	r := warmupFixtureReader(t)
	authorizeWarmupA(t, r)
	root := filepath.Join(r.Home, ".codex", "sessions")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	now := time.Now()
	d := warmupDependencies{now: func() time.Time { return now }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, false) }, activity: func(s []string) (bool, error) { return r.activityStatus(s, now) }}
	d.run = func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		if err := os.Chmod(root, 0000); err != nil {
			t.Fatal(err)
		}
		if f, err := os.Open(root); err == nil {
			f.Close()
			t.Skip("process bypasses permission fixture")
		}
		err := claim()
		if err == nil {
			t.Fatal("unknown activity was claimed as a billable request")
		}
		return WarmupExecution{}, err
	}
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(context.Background(), "", d)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].Status != "activity_unknown" || report.Accounts[0].Attempts != 0 || !report.Accounts[0].LastAttempt.IsZero() {
		t.Fatal("final activity failure lost its reason or recorded a request")
	}
}

func TestWarmupRefreshBypassesDeadlineButNeverSubmits(t *testing.T) {
	for _, mode := range []string{"running", "cooldown", "eligible", "outside-hours", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			r := warmupFixtureReader(t)
			authorizeWarmupA(t, r)
			now := time.Now()
			p, j, err := r.readWarmupState()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				p.Enabled = false
			}
			if mode == "outside-hours" {
				zone, _ := time.LoadLocation(warmupTimeZone)
				s := WarmupSchedule{Enabled: true, Start: now.In(zone).Add(time.Hour).Format("15:04"), End: now.In(zone).Add(2 * time.Hour).Format("15:04"), TimeZone: warmupTimeZone}
				p.Schedule = &s
			}
			if err = writeState(filepath.Join(r.warmupRoot(), "policy.json"), p); err != nil {
				t.Fatal(err)
			}
			policyBefore, _ := os.ReadFile(filepath.Join(r.warmupRoot(), "policy.json"))
			key := credentialIdentity(r.quotaCredentials(now)["managed-a"])
			original := WarmupAccount{Key: key, Status: "window_running", LastCheck: now.Add(-time.Hour), LastAttempt: now.Add(-30 * time.Minute), LastProbe: now.Add(-2 * time.Hour), NextCheck: now.Add(2 * time.Hour), Attempts: 3, IdleObserved: now.Add(-time.Minute)}
			if mode == "cooldown" {
				original.NoRetryBefore = now.Add(5 * time.Hour)
			}
			j.Accounts[key] = &original
			if err = writeState(filepath.Join(r.warmupRoot(), "journal.json"), j); err != nil {
				t.Fatal(err)
			}
			queries := 0
			d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota {
				queries++
				return testQuota(now, mode == "running")
			}, run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
				t.Fatal("read-only refresh submitted a request")
				return WarmupExecution{}, nil
			}}
			report, err := r.checkWarmupMode(context.Background(), "", true, d)
			if err != nil {
				t.Fatal(err)
			}
			if queries != 1 || len(report.Accounts) != 1 {
				t.Fatal("manual refresh did not query a cached account exactly once")
			}
			a := report.Accounts[0]
			if !a.LastCheck.Equal(now) || a.Attempts != original.Attempts || !a.LastAttempt.Equal(original.LastAttempt) || !a.LastProbe.Equal(original.LastProbe) || !a.NoRetryBefore.Equal(original.NoRetryBefore) {
				t.Fatal("refresh changed a request claim, cooldown, or probe limit")
			}
			if mode == "eligible" && a.Status != "ready_to_warmup" {
				t.Fatal("eligible refresh did not stay read-only")
			}
			if mode == "cooldown" && a.Status != "verification_pending" {
				t.Fatal("manual refresh bypassed request cooldown")
			}
			policyAfter, _ := os.ReadFile(filepath.Join(r.warmupRoot(), "policy.json"))
			if string(policyBefore) != string(policyAfter) || report.Enabled != p.Enabled {
				t.Fatal("refresh changed automatic policy")
			}
		})
	}
}

func TestWarmupResetMetadataLostBeforeSubmitCancelsClaim(t *testing.T) {
	r := warmupFixtureReader(t)
	authorizeWarmupA(t, r)
	now, preparing := time.Now(), false
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota {
		q := testQuota(now, false)
		if preparing {
			q.Windows[0].ResetAt, q.Windows[0].ResetAfter = nil, nil
		}
		return q
	}, run: func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		preparing = true
		err := claim()
		if err == nil {
			t.Fatal("unknown reset at final check was submitted")
		}
		return WarmupExecution{}, err
	}}
	if _, err := r.checkWarmup(context.Background(), "", d); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(context.Background(), "", d)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].Status != "window_unknown" || report.Accounts[0].Attempts != 0 || !report.Accounts[0].LastAttempt.IsZero() {
		t.Fatal("unknown reset failure lost its state or recorded a request")
	}
}

package assistant

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func scheduleTime(t *testing.T, text string) time.Time {
	t.Helper()
	zone, err := time.LoadLocation(warmupTimeZone)
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.ParseInLocation("2006-01-02 15:04:05", text, zone)
	if err != nil {
		t.Fatal(err)
	}
	return now
}
func TestWarmupScheduleDayAndOvernightBoundaries(t *testing.T) {
	for _, tc := range []struct {
		start, end, clock string
		allowed           bool
	}{
		{"08:00", "23:00", "07:59:59", false}, {"08:00", "23:00", "08:00:00", true}, {"08:00", "23:00", "22:59:59", true}, {"08:00", "23:00", "23:00:00", false},
		{"22:00", "08:00", "21:59:59", false}, {"22:00", "08:00", "22:00:00", true}, {"22:00", "08:00", "23:59:59", true}, {"22:00", "08:00", "00:00:00", true}, {"22:00", "08:00", "07:59:59", true}, {"22:00", "08:00", "08:00:00", false},
	} {
		s := WarmupSchedule{Enabled: true, Start: tc.start, End: tc.end, TimeZone: warmupTimeZone}
		if got := s.permits(scheduleTime(t, "2026-10-26 "+tc.clock)); got != tc.allowed {
			t.Fatalf("%s-%s at %s: %v", tc.start, tc.end, tc.clock, got)
		}
	}
	s := WarmupSchedule{Enabled: true, Start: "08:00", End: "23:00", TimeZone: warmupTimeZone}
	if !s.permits(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("schedule used host zone instead of UTC+8")
	}
	if got := s.nextOpening(scheduleTime(t, "2026-10-26 23:00:00")); !got.Equal(scheduleTime(t, "2026-10-27 08:00:00")) {
		t.Fatal("next opening not tomorrow")
	}
	s.Start, s.End = "22:00", "08:00"
	if got := s.nextOpening(scheduleTime(t, "2026-10-26 12:00:00")); !got.Equal(scheduleTime(t, "2026-10-26 22:00:00")) {
		t.Fatal("overnight next opening wrong")
	}
	if !defaultWarmupSchedule().permits(scheduleTime(t, "2026-10-26 02:00:00")) {
		t.Fatal("legacy policy must remain unrestricted")
	}
}
func TestWarmupScheduleValidationAndPreservesPolicy(t *testing.T) {
	r := warmupFixtureReader(t)
	if _, err := r.ConfigureWarmup(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{"24:00", "9:00", "08:60", "-1:00", "08:0x"} {
		if _, err := r.ConfigureWarmupSchedule(true, start, "23:00"); err == nil {
			t.Fatalf("accepted %s", start)
		}
	}
	if _, err := r.ConfigureWarmupSchedule(true, "08:00", "08:00"); err == nil {
		t.Fatal("equal bounds should be explicit all-day toggle")
	}
	report, err := r.ConfigureWarmupSchedule(true, "22:00", "08:00")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Enabled || report.Schedule.Start != "22:00" {
		t.Fatal("saving hours changed automatic switch")
	}
	p, _, err := r.readWarmupState()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allowed[privateKey("managed-a")] || !p.Allowed[privateKey("managed-b")] {
		t.Fatal("saving hours lost account scope")
	}
	report, err = r.ConfigureWarmupSchedule(false, "22:00", "08:00")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Enabled || report.Schedule.Enabled {
		t.Fatal("disabling time restriction must not disable automatic warmup")
	}
}
func TestWarmupOutsideScheduleMakesNoQuotaOrModelRequests(t *testing.T) {
	r := warmupFixtureReader(t)
	ctx := context.Background()
	_, _ = r.ConfigureWarmup(ctx, true)
	_, err := r.ConfigureWarmupSchedule(true, "22:00", "08:00")
	if err != nil {
		t.Fatal(err)
	}
	d := warmupDependencies{now: func() time.Time { return scheduleTime(t, "2026-10-26 12:00:00") }, quota: func(context.Context, quotaCredential) AccountQuota {
		t.Fatal("outside schedule queried quota")
		return AccountQuota{}
	}, run: func(context.Context, string, quotaCredential, func() error) (WarmupExecution, error) {
		t.Fatal("outside schedule sent model request")
		return WarmupExecution{}, nil
	}}
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if report.ScheduleAllowed || report.NextOpening == nil || !report.NextOpening.Equal(scheduleTime(t, "2026-10-26 22:00:00")) {
		t.Fatal("waiting state not reported")
	}
	if _, err := os.Stat(filepath.Join(r.warmupRoot(), "journal.json")); !os.IsNotExist(err) {
		t.Fatal("outside schedule changed request journal")
	}
}
func TestWarmupScheduleEditedDuringSetupStopsSubmission(t *testing.T) {
	r := warmupFixtureReader(t)
	ctx := context.Background()
	now := time.Now()
	_, _ = r.ConfigureWarmup(ctx, true)
	d := warmupDependencies{now: func() time.Time { return now }, quota: func(context.Context, quotaCredential) AccountQuota { return testQuota(now, false) }, busy: func([]string) bool { return false }}
	d.run = func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		zone, _ := time.LoadLocation(warmupTimeZone)
		start := now.In(zone).Add(time.Hour).Format("15:04")
		end := now.In(zone).Add(2 * time.Hour).Format("15:04")
		if _, err := r.ConfigureWarmupSchedule(true, start, end); err != nil {
			t.Fatal(err)
		}
		return WarmupExecution{}, claim()
	}
	_, _ = r.checkWarmup(ctx, "", d)
	now = now.Add(time.Minute)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range report.Accounts {
		if account.Attempts != 0 {
			t.Fatal("schedule change allowed a model submission")
		}
	}
}

func TestWarmupFinalQuotaQueryCrossingEndBoundaryStopsSubmission(t *testing.T) {
	r := warmupFixtureReader(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Minute)
	zone, _ := time.LoadLocation(warmupTimeZone)
	end := now.Add(time.Minute)
	_, _ = r.ConfigureWarmup(ctx, true)
	_, err := r.ConfigureWarmupSchedule(true, now.In(zone).Add(-time.Hour).Format("15:04"), end.In(zone).Format("15:04"))
	if err != nil {
		t.Fatal(err)
	}
	preparing := false
	d := warmupDependencies{now: func() time.Time { return now }, busy: func([]string) bool { return false }, quota: func(context.Context, quotaCredential) AccountQuota {
		if preparing {
			now = end
		}
		return testQuota(now, false)
	}, run: func(_ context.Context, _ string, _ quotaCredential, claim func() error) (WarmupExecution, error) {
		preparing = true
		return WarmupExecution{}, claim()
	}}
	_, _ = r.checkWarmup(ctx, "", d)
	now = now.Add(15 * time.Second)
	report, err := r.checkWarmup(ctx, "", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range report.Accounts {
		if account.Attempts != 0 {
			t.Fatal("a request crossed the end boundary after its last quota check")
		}
	}
}

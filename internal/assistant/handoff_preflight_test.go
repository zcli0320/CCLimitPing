package assistant

import "testing"

func TestHandoffQuotaEvidenceUsesRemainingAndStrictThreshold(t *testing.T) {
	quota := func(fiveUsed, weekUsed float64) *AccountQuota {
		return &AccountQuota{Status: "ready", Windows: []QuotaWindow{{Seconds: 18000, UsedPercent: fiveUsed}, {Seconds: 604800, UsedPercent: weekUsed}}}
	}
	accounts := []Account{{ID: "a", Name: "A", Quota: quota(90, 5)}, {ID: "b", Name: "B", Quota: quota(90, 5)}}
	out := HandoffPreflightReport{Threshold: 10}
	handoffQuotaEvidence(&out, accounts, "a")
	if out.SourceRemaining == nil || *out.SourceRemaining != 10 || out.TriggerReached || out.QuotaReady {
		t.Fatal("10% is neither below the source threshold nor above the target threshold")
	}
	accounts[0].Quota = quota(90.1, 5)
	accounts[1].Quota = quota(89.9, 100)
	out = HandoffPreflightReport{Threshold: 10}
	handoffQuotaEvidence(&out, accounts, "a")
	if !out.TriggerReached || out.QuotaReady {
		t.Fatal("source trigger or exhausted target week interpreted incorrectly")
	}
	accounts[1].Quota = quota(89.9, 99)
	out = HandoffPreflightReport{Threshold: 10}
	handoffQuotaEvidence(&out, accounts, "a")
	if !out.TriggerReached || !out.QuotaReady {
		t.Fatal("usable backup account not identified")
	}
	limited := true
	accounts[0].Quota = quota(100, 5)
	accounts[0].Quota.LimitReached = &limited
	accounts[1].Quota.LimitReached = &limited
	out = HandoffPreflightReport{Threshold: 10}
	handoffQuotaEvidence(&out, accounts, "a")
	if !out.TriggerReached || out.SourceRemaining == nil || *out.SourceRemaining != 0 || out.QuotaReady {
		t.Fatal("a limited source must show zero; a limited backup must not be considered usable")
	}
}

func TestHandoffQuotaEvidenceDoesNotTreatUnknownAsZero(t *testing.T) {
	accounts := []Account{{ID: "a", Quota: &AccountQuota{Status: "credential_unavailable"}}, {ID: "b", Quota: &AccountQuota{Status: "ready", Windows: []QuotaWindow{{Seconds: 18000, UsedPercent: 0}}}}}
	out := HandoffPreflightReport{Threshold: 10}
	handoffQuotaEvidence(&out, accounts, "a")
	if out.SourceRemaining != nil || out.TriggerReached || out.QuotaReady {
		t.Fatal("unknown source or missing weekly quota was treated as usable")
	}
}

func TestNativeDesktopPreflightRequiresIdentityAndUsableBackup(t *testing.T) {
	ready := DesktopHandoffPreflight{Ready: true, RuntimeIdentityVerified: true}
	if !desktopExecutionReady(ready, true, true, true) {
		t.Fatal("a verified Desktop with a usable backup cannot enable monitoring")
	}
	ready.RuntimeIdentityVerified = false
	if desktopExecutionReady(ready, true, true, true) {
		t.Fatal("disk identity alone enabled native Desktop monitoring")
	}
	ready.RuntimeIdentityVerified = true
	if desktopExecutionReady(ready, false, true, true) || desktopExecutionReady(ready, true, false, true) {
		t.Fatal("unready login or quota enabled monitoring")
	}
}

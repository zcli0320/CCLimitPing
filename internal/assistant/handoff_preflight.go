package assistant

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"
)

func nativePreflightQuotas(ctx context.Context, manager handoffAccountManager, scope, sourceID string, accounts []Account, ccSwitchOwns ...bool) (map[string]bool, []string) {
	ready := make(map[string]bool)
	var issues []string
	seen := make(map[string]bool)
	addIssue := func(message string) {
		if !seen[message] {
			issues = append(issues, message)
			seen[message] = true
		}
	}
	for i := range accounts {
		account := &accounts[i]
		if account.ID == sourceID || account.ExternalOnly {
			continue
		}
		var readyErr error
		if scope != "desktop" || len(ccSwitchOwns) == 0 || !ccSwitchOwns[0] {
			readyErr = manager.ready(ctx, account.ID, scope)
		}
		if err := readyErr; err != nil {
			message := "备用账号登录或当前端状态不可确认，请在授权管理中检查。"
			if strings.Contains(err.Error(), "CC Switch 也管理该端") {
				message = "CC Switch 正在管理同一目录，请从菜单栏完全退出后重新检查。"
			} else if strings.Contains(err.Error(), "该端登录已被外部修改") {
				message = "该端登录已被外部修改，请先核查账号绑定。"
			}
			addIssue(message)
			continue
		}
		q, err := manager.quota(ctx, account.ID)
		if err != nil || !resumeQuotaReady(q) {
			addIssue("备用账号额度无法确认，请检查登录和网络后重试。")
			continue
		}
		account.Quota = &q
		ready[account.ID] = true
	}
	return ready, issues
}

// HandoffPreflight is informational only: it does not grant authorization,
// interrupt any task, modify credentials, or claim that a running process has
// adopted the identity found in its on-disk auth file.
type HandoffPreflightReport struct {
	Scope           string   `json:"scope"`
	Threshold       float64  `json:"threshold"`
	Selected        bool     `json:"selected"`
	SourceName      string   `json:"source_name,omitempty"`
	SourceRemaining *float64 `json:"source_remaining,omitempty"`
	TargetName      string   `json:"target_name,omitempty"`
	TargetRemaining *float64 `json:"target_remaining,omitempty"`
	TriggerReached  bool     `json:"trigger_reached"`
	QuotaReady      bool     `json:"quota_ready"`
	CandidateReady  bool     `json:"candidate_ready"`
	CandidateSource string   `json:"candidate_source,omitempty"`
	ExecutionReady  bool     `json:"execution_ready"`
	Message         string   `json:"message"`
	Limitations     []string `json:"limitations"`
}

func handoffWindow(q *AccountQuota, duration int64) (float64, bool) {
	if q == nil || q.Status != "ready" {
		return 0, false
	}
	for _, w := range q.Windows {
		if w.Seconds == duration && !math.IsNaN(w.UsedPercent) && !math.IsInf(w.UsedPercent, 0) && w.UsedPercent >= 0 && w.UsedPercent <= 100 {
			return 100 - w.UsedPercent, true
		}
	}
	return 0, false
}

func handoffQuotaEvidence(out *HandoffPreflightReport, accounts []Account, sourceID string) string {
	for _, a := range accounts {
		if a.ID != sourceID {
			continue
		}
		out.SourceName = a.Name
		if remaining, ok := handoffWindow(a.Quota, 18000); ok {
			out.SourceRemaining = &remaining
			out.TriggerReached = remaining < out.Threshold
		}
		break
	}
	for _, a := range accounts {
		if a.ID == sourceID {
			continue
		}
		five, fOK := handoffWindow(a.Quota, 18000)
		week, wOK := handoffWindow(a.Quota, 604800)
		limited := a.Quota != nil && a.Quota.LimitReached != nil && *a.Quota.LimitReached
		if fOK && wOK && !limited && five > out.Threshold && week > 0 {
			out.TargetName, out.TargetRemaining, out.QuotaReady = a.Name, &five, true
			return a.ID
		}
	}
	return ""
}

func desktopExecutionReady(desktop DesktopHandoffPreflight, candidateReady, quotaReady, native bool) bool {
	return desktop.Ready && candidateReady && quotaReady && (!native || desktop.RuntimeIdentityVerified)
}

func (r Reader) CheckHandoffPreflight(ctx context.Context, scope, id string, threshold float64) (HandoffPreflightReport, error) {
	out := HandoffPreflightReport{Scope: scope, Threshold: threshold, Limitations: []string{}}
	if (scope != "desktop" && scope != "cli") || id == "" || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold >= 100 {
		return out, errors.New("请选择有效会话并输入大于 0、小于 100 的阈值")
	}
	if scope == "desktop" {
		owned, err := r.desktopCCSwitchOwner()
		if err != nil {
			out.Limitations = append(out.Limitations, err.Error())
			return out, err
		}
		if owned {
			out.Limitations = append(out.Limitations, "Desktop 由 CC Switch 管理，交接时使用 CC Switch 切换。")
			if notice := r.desktopBindingLimitation(); notice != "" {
				out.Limitations = append(out.Limitations, notice)
			}
		}
	}
	s := r.Snapshot(ctx)
	for _, session := range s.Sessions {
		if session.Scope == scope && session.ID == id && !session.Archived {
			out.Selected = true
			break
		}
	}
	if !out.Selected {
		return out, errors.New("目标会话不可用，请刷新后重新选择")
	}
	if _, ok := r.managedResumeProvider(scope); !ok {
		return out, errors.New("该端不是可确认的官方账号配置")
	}
	current, err := r.scopeCredential(scope, time.Now())
	if err != nil {
		return out, err
	}
	credentials := r.quotaCredentials(time.Now())
	sourceID := ""
	for id, credential := range credentials {
		if credentialIdentity(credential) == credentialIdentity(current) {
			if sourceID != "" {
				return out, errors.New("当前账号映射不唯一，已停止检查")
			}
			sourceID = id
		}
	}
	if sourceID == "" {
		return out, errors.New("当前目录账号无法匹配账号库身份")
	}
	r.ReadAccountQuotas(ctx, &s)
	var nativeReady map[string]bool
	if r.nativeAccountPreview() {
		manager, e := r.startNativeHandoffManager(ctx)
		if e != nil {
			out.Limitations = append(out.Limitations, "账号管理器检查未完成："+e.Error())
		} else {
			var issues []string
			nativeReady, issues = nativePreflightQuotas(ctx, manager, scope, sourceID, s.Accounts, scope == "desktop" && r.desktopWriterIsCCSwitch())
			out.Limitations = append(out.Limitations, issues...)
			manager.Close()
		}
	}
	targetID := handoffQuotaEvidence(&out, s.Accounts, sourceID)
	if scope != "desktop" || !r.nativeAccountPreview() {
		out.Limitations = append(out.Limitations, "磁盘认证不证明当前运行实例的实际账号；尚未核验。")
	}
	pairReady := true
	if scope == "desktop" && r.nativeAccountPreview() {
		source, target, e := r.desktopHandoffAccounts()
		if e != nil {
			pairReady = false
			out.Limitations = append(out.Limitations, e.Error())
		} else if source != sourceID || (targetID != "" && target != targetID) {
			pairReady = false
			out.Limitations = append(out.Limitations, "当前和备用账号映射不一致，请检查账号授权。")
		}
	}
	if r.nativeAccountPreview() && targetID != "" && nativeReady[targetID] && scope == "desktop" && r.desktopWriterIsCCSwitch() {
		bridge, e := newDesktopCCSwitch(r)
		if e == nil {
			e = bridge.inspect(ctx, targetID)
		}
		if e != nil {
			out.Limitations = append(out.Limitations, e.Error())
		} else {
			out.CandidateReady = true
			out.CandidateSource = "CC Switch 账号管理器（切换时生成完整登录记录）"
			out.Limitations = append(out.Limitations, "备用额度由小助手只读检查；退出 Desktop 后由 CC Switch 切换并核验登录记录。")
		}
	} else if r.nativeAccountPreview() && targetID != "" && nativeReady[targetID] {
		out.CandidateReady = true
		out.CandidateSource = "小助手账号管理器的现有有效登录"
		out.Limitations = append(out.Limitations, "切换前将再次核查登录、额度和目标端进程；不会独立刷新凭证。")
	} else if r.nativeAccountPreview() {
		out.Limitations = append(out.Limitations, "备用账号需要有效登录、五小时剩余额度高于阈值且周额度可用。")
	} else if targetID != "" && scope == "desktop" {
		bridge, e := newDesktopCCSwitch(r)
		if e == nil {
			e = bridge.inspect(ctx, targetID)
		}
		if e != nil {
			out.Limitations = append(out.Limitations, e.Error())
		} else {
			out.CandidateReady = true
			out.CandidateSource = "CC Switch 账号管理器（切换时生成完整登录记录）"
			out.Limitations = append(out.Limitations, "退出 Desktop 后调用 CC Switch 原有切换流程；写出目标登录记录后再次核验。")
		}
	} else if targetID != "" {
		candidate, e := r.findHandoffAuthCandidate(targetID, time.Now())
		if e != nil {
			out.Limitations = append(out.Limitations, e.Error())
		} else {
			client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			q := queryAccountQuota(ctx, client, candidate.quotaCredential())
			if handoffTargetQuotaOK(q, threshold) && resumeQuotaReady(q) {
				out.CandidateReady = true
				out.CandidateSource = candidate.source
				five, _ := handoffWindow(&q, 18000)
				out.TargetRemaining = &five
				out.Limitations = append(out.Limitations, "完整凭证已匹配并通过额度查询；刷新凭证仍须在实际使用中验证。")
			} else {
				out.Limitations = append(out.Limitations, "完整凭证未通过备用账号额度检查，请在 CC Switch 更新后重试。")
			}
		}
	} else {
		out.Limitations = append(out.Limitations, "未找到额度满足条件的备用账号；尚未检查完整认证。")
	}
	if scope == "desktop" {
		if desktop, e := r.CheckDesktopHandoffPreflight(ctx, id); e == nil {
			if r.nativeAccountPreview() && desktop.RuntimeIdentityVerified {
				out.Limitations = append(out.Limitations, "Desktop 当前运行账号已独立核验。")
			}
			for _, blocker := range desktop.Blockers {
				out.Limitations = append(out.Limitations, blocker)
			}
			out.ExecutionReady = pairReady && desktopExecutionReady(desktop, out.CandidateReady, out.QuotaReady, r.nativeAccountPreview())
		} else {
			out.Limitations = append(out.Limitations, "Desktop 会话检查未完成："+e.Error())
		}
	} else {
		if cli, e := r.CheckCLIHandoffPreflight(ctx, id); e == nil {
			out.Limitations = append(out.Limitations, cli.Blockers...)
		} else {
			out.Limitations = append(out.Limitations, "CLI 会话检查未完成："+e.Error())
		}
	}
	if scope == "cli" {
		out.Message = "只读检查完成；托管交接会在执行前再次核验。"
	} else if r.nativeAccountPreview() {
		out.Message = "只读检查完成；开启监控和切号时会再次核验。"
	} else {
		out.Message = "只读检查完成；手动开启后监控选中会话，触发时退出整个 Desktop。"
	}
	return out, nil
}

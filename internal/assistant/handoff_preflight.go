package assistant

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"
)

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

func (r Reader) CheckHandoffPreflight(ctx context.Context, scope, id string, threshold float64) (HandoffPreflightReport, error) {
	out := HandoffPreflightReport{Scope: scope, Threshold: threshold, Limitations: []string{}}
	if (scope != "desktop" && scope != "cli") || id == "" || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold >= 100 {
		return out, errors.New("请选择有效会话并输入大于 0、小于 100 的阈值")
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
		return out, errors.New("当前目录账号无法匹配 CC Switch 托管身份")
	}
	r.ReadAccountQuotas(ctx, &s)
	targetID := handoffQuotaEvidence(&out, s.Accounts, sourceID)
	out.Limitations = append(out.Limitations, "磁盘认证不证明当前运行实例的实际账号；尚未核验。")
	if targetID != "" && scope == "desktop" {
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
			out.Limitations = append(out.Limitations, desktop.Blockers...)
			out.ExecutionReady = desktop.Ready && out.CandidateReady && out.QuotaReady
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
	} else {
		out.Message = "只读检查完成；手动开启后监控选中会话，触发时退出整个 Desktop。"
	}
	return out, nil
}

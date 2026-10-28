package assistant

import (
	"context"
	"errors"
	"math"
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
	if targetID != "" {
		e := newNativeDualSwitcher(r).inspect(ctx, scope)
		if e != nil {
			out.Limitations = append(out.Limitations, e.Error())
		} else {
			out.CandidateReady = true
			out.CandidateSource = "原生双端账号协调器"
			out.Limitations = append(out.Limitations, "工作端使用候选账号，待用端分配原账号并保持停止；CC Switch 固定管理 CLI 目录。")
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

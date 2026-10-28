// Package assistant integrates CC Switch accounts and isolated Codex history.
// Credentials are never passed in subprocess arguments or independently
// refreshed. A handoff may temporarily keep a protected recovery copy.
package assistant

import "time"

const Version = "0.3.10"

type Capability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

type Account struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	ProviderNames []string      `json:"provider_names"`
	Selected      bool          `json:"selected"`
	Default       bool          `json:"default"`
	QuotaStatus   string        `json:"quota_status"`
	Quota         *AccountQuota `json:"quota,omitempty"`
}

type QuotaWindow struct {
	Seconds     int64   `json:"seconds"`
	UsedPercent float64 `json:"used_percent"`
	ResetAt     *int64  `json:"reset_at,omitempty"`
	ResetAfter  *int64  `json:"reset_after_seconds,omitempty"`
}
type AccountQuota struct {
	Status            string        `json:"status"`
	Message           string        `json:"message,omitempty"`
	QueriedAt         time.Time     `json:"queried_at"`
	CredentialSources []string      `json:"credential_sources"`
	Windows           []QuotaWindow `json:"windows"`
	LimitReached      *bool         `json:"limit_reached,omitempty"`
}

type Session struct {
	// Key includes the configured history root. Identical IDs in isolated
	// CODEX_HOMEs must not accidentally refer to the same managed instance.
	Key         string `json:"key"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	TitleSource string `json:"title_source"`
	Project     string `json:"project"`
	UpdatedAt   int64  `json:"updated_at"`
	Source      string `json:"source"`
	Archived    bool   `json:"archived"`
	Scope       string `json:"scope"`
}

type SessionSource struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Home     string   `json:"home"`
	Complete bool     `json:"complete"`
	Warnings []string `json:"warnings"`
}

type Snapshot struct {
	Version          string                `json:"version"`
	CapturedAt       time.Time             `json:"captured_at"`
	CodexHome        string                `json:"codex_home"`
	Accounts         []Account             `json:"accounts"`
	Sessions         []Session             `json:"sessions"`
	SessionsComplete bool                  `json:"sessions_complete"`
	Sources          []SessionSource       `json:"sources"`
	Capabilities     map[string]Capability `json:"capabilities"`
	Warnings         []string              `json:"warnings"`
	Warmup           *WarmupReport         `json:"warmup,omitempty"`
	Resume           *ResumeReport         `json:"resume,omitempty"`
	Handoff          *HandoffSummary       `json:"handoff,omitempty"`
	Allocation       *AccountAllocation    `json:"allocation,omitempty"`
}

// GUI consumers need status and binding only, never account ids or the
// persisted execution settings used by the coordinator.
type HandoffSummary struct {
	AuthorizationPending bool    `json:"authorization_pending,omitempty"`
	Enabled              bool    `json:"enabled"`
	Scope                string  `json:"scope"`
	SessionID            string  `json:"session_id"`
	Threshold            float64 `json:"threshold"`
	Stage                string  `json:"stage"`
	Message              string  `json:"message"`
	RecoveryPending      bool    `json:"recovery_pending,omitempty"`
	RunnerReady          bool    `json:"runner_ready,omitempty"`
}

func capabilities() map[string]Capability {
	return map[string]Capability{
		"account_quota":   {Available: true, Reason: "沿用 CC Switch 账号身份，匹配现有有效访问凭证并读取官方额度。"},
		"account_warmup":  {Available: true, Reason: "独立 app-server 临时会话、每账号调度、请求登记与额度回查；需有效访问凭证。"},
		"account_switch":  {Available: true, Reason: "为选定工作端在停稳后原生交换两端账号；待用端保持停止，CC Switch 固定管理 CLI。"},
		"cli_handoff":     {Available: true, Reason: "为选中会话启动托管 CLI 作为工作端；原生交换两端账号，待用 Desktop 须空闲。首版隔离流程已验证。"},
		"desktop_handoff": {Available: true, Reason: "选定 Desktop 会话作为工作端；原生交换两端账号，待用 CLI 保持停止。首版隔离流程已验证。"},
		"desktop_resume":  {Available: true, Reason: "手动绑定本机原会话，限额恢复后继续一次；不切换账号。"},
	}
}

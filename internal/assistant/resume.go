package assistant

import (
	"context"
	"errors"
	"github.com/BurntSushi/toml"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type ResumeReport struct {
	Enabled  bool            `json:"enabled"`
	Scope    string          `json:"scope"`
	ID       string          `json:"id"`
	Key      string          `json:"key"`
	Status   string          `json:"status"`
	Message  string          `json:"message"`
	Identity string          `json:"identity,omitempty"`
	Handled  map[string]bool `json:"handled,omitempty"`
}

func (r Reader) resumePath() string {
	return filepath.Join(r.Home, "Library/Application Support/CodexHelper/resume/policy.json")
}

// CC Switch names its official OAuth provider "custom" on this Mac. The
// name alone is not an endpoint or authentication boundary.
func (r Reader) managedResumeProvider(scope string) (string, bool) {
	dir := ".codex"
	if scope == "cli" {
		dir = ".codex-cli"
	} else if scope != "desktop" {
		return "", false
	}
	var cfg struct {
		Provider  string `toml:"model_provider"`
		Providers map[string]struct {
			Requires   bool              `toml:"requires_openai_auth"`
			Base       string            `toml:"base_url"`
			Key        string            `toml:"env_key"`
			Bearer     string            `toml:"experimental_bearer_token"`
			Headers    map[string]string `toml:"http_headers"`
			EnvHeaders map[string]string `toml:"env_http_headers"`
		} `toml:"model_providers"`
	}
	path := filepath.Join(r.Home, dir, "config.toml")
	if _, e := toml.DecodeFile(path, &cfg); e != nil {
		if _, e = os.Stat(path); !os.IsNotExist(e) {
			return "", false
		}
	}
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	if p, ok := cfg.Providers[cfg.Provider]; ok {
		return cfg.Provider, p.Requires && p.Base == "" && p.Key == "" && p.Bearer == "" && len(p.Headers) == 0 && len(p.EnvHeaders) == 0
	}
	return cfg.Provider, cfg.Provider == "openai"
}
func (r Reader) ResumeStatus() ResumeReport {
	p := ResumeReport{Status: "off", Message: "未开启指定会话续跑", Handled: map[string]bool{}}
	if readState(r.resumePath(), &p) != nil {
		return ResumeReport{Status: "error", Message: "续跑设置不可读取，已停止"}
	}
	if p.Handled == nil {
		p.Handled = map[string]bool{}
	}
	return p
}
func (r Reader) scopeCredential(scope string, now time.Time) (quotaCredential, error) {
	dir, label := ".codex", "Desktop"
	if scope == "cli" {
		dir, label = ".codex-cli", "CLI"
	} else if scope != "desktop" {
		return quotaCredential{}, errors.New("会话来源无效")
	}
	var auth struct {
		Tokens struct {
			Access string `json:"access_token"`
		} `json:"tokens"`
	}
	if readJSON(filepath.Join(r.Home, dir, "auth.json"), &auth) != nil {
		return quotaCredential{}, errors.New("凭证不可用或已过期，请在 CC Switch 更新该账号后重试")
	}
	for _, c := range r.quotaCredentialsFrom(now, dir) {
		// Require this source's exact token, not a newer token from another
		// client: a disk identity is only used to check the selected scope.
		if c.access == auth.Tokens.Access {
			for _, source := range c.sources {
				if source == label {
					return c, nil
				}
			}
		}
	}
	return quotaCredential{}, errors.New("凭证不可用或已过期，请在 CC Switch 更新该账号后重试")
}
func (r Reader) ConfigureResume(ctx context.Context, scope, id string, enabled bool) (ResumeReport, error) {
	unlock, err := lockWarmup(r.resumePath() + ".lock")
	if err != nil {
		return r.ResumeStatus(), err
	}
	defer unlock()
	p := r.ResumeStatus()
	if !enabled {
		p.Enabled = false
		p.Status = "off"
		p.Message = "指定会话续跑已关闭"
		return p, writeState(r.resumePath(), p)
	}
	if p.Status == "error" {
		return p, errors.New("续跑设置损坏；请先检查本机续跑状态，不能清除去重记录后自动重试")
	}
	if scope != "desktop" && scope != "cli" {
		return p, errors.New("会话来源无效")
	}
	provider, supported := r.managedResumeProvider(scope)
	if !supported {
		return p, errors.New("所选端没有使用可确认的 CC Switch / OpenAI 登录配置")
	}
	snapshot := r.Snapshot(ctx)
	found := false
	selectedKey := ""
	for _, s := range snapshot.Sessions {
		if s.ID == id && s.Scope == scope && !s.Archived {
			found = true
			selectedKey = s.Key
			break
		}
	}
	if !found {
		return p, errors.New("所选会话不可用，请刷新后重新选择")
	}
	c, err := r.scopeCredential(scope, time.Now())
	if err != nil {
		return p, err
	}
	if scope == "cli" {
		if !r.cliManaged(id) {
			return p, errors.New("请先用“在终端托管续跑”打开该 CLI 会话")
		}
	} else {
		ipc, e := connectDesktop(ctx, r.Home)
		if e != nil {
			return p, e
		}
		defer ipc.conn.Close()
		v, _, e := ipc.snapshot(id)
		if e != nil {
			return p, e
		}
		if v.Mode == "durable" || v.ModelProvider != provider {
			return p, errors.New("本版仅支持本机 OpenAI Codex 会话")
		}
	}
	p.Enabled = true
	p.Scope = scope
	p.ID = id
	p.Key = selectedKey
	p.Identity = credentialIdentity(c)
	p.Status = "watching"
	p.Message = "已开启：仅在此会话因限额停止且额度恢复后继续"
	return p, writeState(r.resumePath(), p)
}

type resumeConnection interface {
	snapshot(string) (desktopConversation, string, error)
	continueTurn(string, string, resumeTurn) error
}
type resumeDependencies struct {
	provider      string
	connect       func(context.Context) (resumeConnection, func(), error)
	credential    func() (quotaCredential, error)
	quota         func(context.Context, quotaCredential) AccountQuota
	verifyRollout func(desktopConversation, resumeTurn) error
}

func (r Reader) CheckResume(ctx context.Context) (ResumeReport, error) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return r.checkResume(ctx, resumeDependencies{
		provider: func() string {
			p, ok := r.managedResumeProvider("desktop")
			if !ok {
				return "unsupported"
			}
			return p
		}(),
		connect: func(ctx context.Context) (resumeConnection, func(), error) {
			c, e := connectDesktop(ctx, r.Home)
			if e != nil {
				return nil, func() {}, e
			}
			return c, func() { c.conn.Close() }, nil
		},
		credential: func() (quotaCredential, error) { return r.scopeCredential("desktop", time.Now()) },
		quota:      func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) },
		verifyRollout: func(v desktopConversation, turn resumeTurn) error {
			return rolloutProvesCompleted(filepath.Join(r.Home, ".codex"), v.RolloutPath, v.ID, turn.ID)
		},
	})
}
func resumeQuotaReady(q AccountQuota) bool {
	if q.Status != "ready" || q.LimitReached == nil || *q.LimitReached {
		return false
	}
	five, week := false, false
	for _, w := range q.Windows {
		if w.UsedPercent >= 100 {
			return false
		}
		if w.Seconds == 18000 {
			five = true
		}
		if w.Seconds == 604800 {
			week = true
		}
	}
	return five && week
}
func (r Reader) checkResume(ctx context.Context, d resumeDependencies) (ResumeReport, error) {
	// A separate execution lock allows the user to disable/change the target
	// while quota or IPC is waiting. Recheck policy at the submission boundary.
	unlock, err := lockWarmup(r.resumePath() + ".execution.lock")
	if err != nil {
		return r.ResumeStatus(), err
	}
	defer unlock()
	p := r.ResumeStatus()
	if !p.Enabled {
		return p, nil
	}
	if p.Scope == "cli" {
		if !r.cliManaged(p.ID) {
			p.Enabled = false
			p.Status = "off"
			p.Message = "托管终端已退出，续跑已停止"
			return p, writeState(r.resumePath(), p)
		}
		return p, nil
	}
	set := func(status, message string) (ResumeReport, error) {
		l, e := lockWarmup(r.resumePath() + ".lock")
		if e != nil {
			return p, e
		}
		defer l()
		fresh := r.ResumeStatus()
		if !fresh.Enabled || fresh.ID != p.ID || fresh.Key != p.Key || fresh.Scope != p.Scope || fresh.Identity != p.Identity {
			return fresh, nil
		}
		fresh.Status = status
		fresh.Message = message
		return fresh, writeState(r.resumePath(), fresh)
	}
	c, err := d.credential()
	if err != nil {
		return set("credential_error", "凭证不可用或已过期，请在 CC Switch 更新该账号后重试")
	}
	if credentialIdentity(c) != p.Identity {
		return set("account_changed", "账号已变化；请关闭后重新开启续跑以确认使用该账号")
	}
	ipc, close, err := d.connect(ctx)
	if err != nil {
		return set("waiting_client", err.Error())
	}
	defer close()
	v, owner, err := ipc.snapshot(p.ID)
	if err != nil {
		return set("waiting_client", err.Error())
	}
	expectedProvider := d.provider
	if expectedProvider == "" {
		expectedProvider = "openai"
	}
	if v.ID != p.ID || v.ModelProvider != expectedProvider {
		return set("provider_changed", "会话认证配置已变化，请重新确认 CC Switch 登录配置")
	}
	turn, limited := v.limitedTurn()
	if !limited {
		return set("watching", "等待所选会话的明确限额错误；正常结束、手动停止和网络错误均不续跑")
	}
	key := privateKey(p.Key + "\x00" + turn.ID)
	if p.Handled[key] {
		return set("already_submitted", "此轮已登记过继续请求；请查看原会话，不会重复发送")
	}
	if d.verifyRollout == nil || d.verifyRollout(v, turn) != nil {
		return set("waiting_client", "原会话记录未确认准确轮次已结束，暂不续跑")
	}
	q := d.quota(ctx, c)
	if !resumeQuotaReady(q) {
		if q.Status != "ready" {
			return set(q.Status, q.Message)
		}
		return set("waiting_quota", "所选会话已因限额停止，等待当前账号额度恢复")
	}
	// Re-read after the network call. A user's intervening message, completion,
	// approval dialog or different owner cancels this attempt.
	latest, latestOwner, err := ipc.snapshot(p.ID)
	if err != nil {
		return set("waiting_client", err.Error())
	}
	freshTurn, ok := latest.limitedTurn()
	if !ok || latest.ID != p.ID || freshTurn.ID != turn.ID || latestOwner != owner || latest.ModelProvider != expectedProvider {
		return set("watching", "原会话状态已变化，本次不发送继续")
	}
	newCredential, err := d.credential()
	if err != nil || credentialIdentity(newCredential) != p.Identity {
		return set("credential_error", "凭证或账号已变化，请在 CC Switch 更新后重新开启续跑")
	}
	if provider, ok := r.managedResumeProvider("desktop"); !ok || provider != expectedProvider {
		return set("provider_changed", "会话认证配置已变化，本次不发送继续")
	}
	lock, err := lockWarmup(r.resumePath() + ".lock")
	if err != nil {
		return p, err
	}
	fresh := r.ResumeStatus()
	if !fresh.Enabled || fresh.ID != p.ID || fresh.Key != p.Key || fresh.Scope != p.Scope || fresh.Identity != p.Identity || fresh.Handled[key] {
		lock()
		return fresh, nil
	}
	if ctx.Err() != nil {
		lock()
		return fresh, ctx.Err()
	}
	if d.verifyRollout == nil || d.verifyRollout(latest, freshTurn) != nil {
		fresh.Status = "waiting_client"
		fresh.Message = "原会话记录已变化或未确认停稳，暂不续跑"
		err = writeState(r.resumePath(), fresh)
		lock()
		return fresh, err
	}
	fresh.Handled[key] = true
	fresh.Status = "submitted"
	fresh.Message = "继续请求已登记，正在向原会话发送"
	err = writeState(r.resumePath(), fresh)
	lock()
	if err != nil {
		return p, err
	}
	if err = ipc.continueTurn(p.ID, owner, freshTurn); err != nil {
		return set("unconfirmed", "继续请求结果未确认；请查看原会话，不会自动重发")
	}
	return set("resumed", "已在原会话开始新一轮；后续只等待新的限额事件")
}

package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CC Switch persists refresh tokens and identity, while valid access tokens
// may be held in its manager or written to a Codex auth.json. Use the managed
// identity to resolve an already-issued token. Do not compete with its refresh
// mutex/rotation generation; we do not use refresh_token at all.
type managedQuotaStore struct {
	Version  int `json:"version"`
	Accounts map[string]struct {
		ID            string `json:"account_id"`
		Workspace     string `json:"chatgpt_account_id"`
		IdentityToken string `json:"id_token"`
	} `json:"accounts"`
}
type tokenClaims struct {
	Issuer    string `json:"iss"`
	Expires   int64  `json:"exp"`
	NotBefore int64  `json:"nbf"`
	Auth      struct {
		User      string `json:"chatgpt_user_id"`
		Workspace string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
}

// Claims are a local consistency check, not a cryptographic verification of a
// JWT. The official service validates the bearer token on each request.
func decodeTokenClaims(token string) (tokenClaims, bool) {
	var claims tokenClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 128<<10 {
		return claims, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(data, &claims) != nil {
		return tokenClaims{}, false
	}
	validIssuer := claims.Issuer == "https://auth.openai.com" || claims.Issuer == "https://auth.openai.com/"
	return claims, validIssuer && claims.Auth.User != "" && claims.Auth.Workspace != ""
}

type quotaCredential struct {
	access    string
	workspace string
	expires   int64
	sources   []string
}

type nativeQuotaAuth struct {
	Mode   string `json:"auth_mode"`
	Tokens struct {
		Access    string `json:"access_token"`
		Identity  string `json:"id_token"`
		Workspace string `json:"account_id"`
	} `json:"tokens"`
}

func (r Reader) quotaCredentials(now time.Time) map[string]quotaCredential {
	return r.quotaCredentialsFrom(now, "")
}

func (r Reader) quotaCredentialsFrom(now time.Time, only string) map[string]quotaCredential {
	result := map[string]quotaCredential{}
	var store managedQuotaStore
	if readJSON(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"), &store) != nil || store.Version != 2 {
		return result
	}
	add := func(auth nativeQuotaAuth, label string, fallback bool) {
		if auth.Mode != "" && auth.Mode != "chatgpt" {
			return
		}
		access, okA := decodeTokenClaims(auth.Tokens.Access)
		identity, okI := decodeTokenClaims(auth.Tokens.Identity)
		if !okA || !okI || access.Expires <= now.Add(30*time.Second).Unix() || access.NotBefore > now.Unix() || access.Auth != identity.Auth || access.Auth.Workspace != auth.Tokens.Workspace {
			return
		}
		for id, account := range store.Accounts {
			managed, ok := decodeTokenClaims(account.IdentityToken)
			if !ok || account.ID != id || account.Workspace != access.Auth.Workspace || managed.Auth != access.Auth {
				continue
			}
			previous := result[id]
			// Stashed logins are snapshots. A live credential always takes
			// priority, even if the snapshot has a later expiry.
			if fallback && previous.access != "" && previous.sources[0] != "CC Switch 暂存" {
				continue
			}
			sources := previous.sources
			if !fallback || len(sources) == 0 {
				sources = append(sources, label)
			}
			if previous.access == "" || access.Expires > previous.expires {
				result[id] = quotaCredential{access: auth.Tokens.Access, workspace: auth.Tokens.Workspace, expires: access.Expires, sources: sources}
			} else {
				previous.sources = sources
				result[id] = previous
			}
		}
	}
	for _, entry := range []struct{ dir, label string }{{".codex", "Desktop"}, {".codex-cli", "CLI"}} {
		if only != "" && entry.dir != only {
			continue
		}
		var auth nativeQuotaAuth
		if readJSON(filepath.Join(r.Home, entry.dir, "auth.json"), &auth) != nil {
			continue
		}
		add(auth, entry.label, false)
	}
	// The CC Switch device-local stash is a snapshot, not its OAuth manager.
	// Never use it to infer the running identity of a Desktop or CLI session.
	if only == "" {
		var stash struct {
			Version int                        `json:"version"`
			Logins  map[string]nativeQuotaAuth `json:"logins"`
		}
		if readJSON(filepath.Join(r.Home, ".cc-switch", "codex-login-stash.json"), &stash) == nil && stash.Version == 1 {
			for _, auth := range stash.Logins {
				add(auth, "CC Switch 暂存", true)
			}
		}
	}
	return result
}

const codexQuotaURL = "https://chatgpt.com/backend-api/wham/usage"

func (r Reader) ReadAccountQuotas(ctx context.Context, snapshot *Snapshot) {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	r.readAccountQuotas(ctx, snapshot, client)
}

func (r Reader) readAccountQuotas(ctx context.Context, snapshot *Snapshot, client *http.Client) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	credentials := r.quotaCredentials(time.Now())
	var group sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for i := range snapshot.Accounts {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			account := &snapshot.Accounts[index]
			credential, ok := credentials[account.ID]
			if !ok {
				account.Quota = &AccountQuota{Status: "credential_unavailable", Message: "未找到此账号匹配且有效的访问凭证；请在 CC Switch 更新该账号后重试。", QueriedAt: time.Now().UTC(), CredentialSources: []string{}, Windows: []QuotaWindow{}}
			} else {
				select {
				case semaphore <- struct{}{}:
					defer func() { <-semaphore }()
				case <-ctx.Done():
					account.Quota = &AccountQuota{Status: "network_error", Message: "额度查询超时，请稍后重试。", QueriedAt: time.Now().UTC(), CredentialSources: credential.sources, Windows: []QuotaWindow{}}
					account.QuotaStatus = account.Quota.Status
					return
				}
				quota := queryAccountQuota(ctx, client, credential)
				account.Quota = &quota
			}
			account.QuotaStatus = account.Quota.Status
		}(i)
	}
	group.Wait()
	snapshot.Capabilities["account_quota"] = Capability{Available: true, Reason: "按 CC Switch 账号身份匹配现有有效凭证，读取官方额度；不复制凭证或轮换刷新令牌。"}
}

func queryAccountQuota(ctx context.Context, client *http.Client, credential quotaCredential) AccountQuota {
	quota := AccountQuota{Status: "network_error", Message: "额度查询失败，请检查网络后重试。", QueriedAt: time.Now().UTC(), CredentialSources: credential.sources, Windows: []QuotaWindow{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexQuotaURL, nil)
	if err != nil {
		return quota
	}
	req.Header.Set("Authorization", "Bearer "+credential.access)
	req.Header.Set("ChatGPT-Account-Id", credential.workspace)
	req.Header.Set("User-Agent", "codex-cli")
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return quota
	}
	defer response.Body.Close()
	quota.QueriedAt = time.Now().UTC()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			quota.Status = "credential_rejected"
			quota.Message = "访问凭证被拒绝，请在 CC Switch 刷新认证后重试。"
		case http.StatusTooManyRequests:
			quota.Status = "rate_limited"
			quota.Message = "额度接口请求受限，请稍后重试；这不等于模型额度耗尽。"
		default:
			quota.Status = "service_error"
			quota.Message = "额度服务暂不可用，请稍后重试。"
		}
		return quota // Never echo error bodies, headers or token-bearing errors.
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		quota.Status = "invalid_response"
		quota.Message = "额度响应不可读取或超出上限。"
		return quota
	}
	windows, limit, ok := parseQuotaWindows(data)
	if !ok {
		quota.Status = "invalid_response"
		quota.Message = "额度响应缺少可识别的窗口数据。"
		return quota
	}
	quota.Status = "ready"
	quota.Message = ""
	quota.Windows = windows
	quota.LimitReached = limit
	return quota
}

func parseQuotaWindows(data []byte) ([]QuotaWindow, *bool, bool) {
	// Same named window fields and duration mapping as CC Switch's
	// services/subscription.rs. Missing/unknown windows stay unknown.
	type window struct {
		Used    *float64 `json:"used_percent"`
		Seconds *int64   `json:"limit_window_seconds"`
		Reset   *int64   `json:"reset_at"`
		After   *int64   `json:"reset_after_seconds"`
	}
	var response struct {
		Rate *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
			Limit     *bool   `json:"limit_reached"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(data, &response) != nil || response.Rate == nil {
		return nil, nil, false
	}
	windows := []QuotaWindow{}
	for _, w := range []*window{response.Rate.Primary, response.Rate.Secondary} {
		if w == nil {
			continue
		}
		if w.Used == nil || w.Seconds == nil || *w.Seconds <= 0 || *w.Used < 0 || *w.Used > 100 || math.IsNaN(*w.Used) || math.IsInf(*w.Used, 0) {
			return nil, nil, false
		}
		if w.Reset != nil && *w.Reset <= 0 {
			w.Reset = nil
		}
		windows = append(windows, QuotaWindow{Seconds: *w.Seconds, UsedPercent: *w.Used, ResetAt: w.Reset, ResetAfter: w.After})
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Seconds < windows[j].Seconds })
	return windows, response.Rate.Limit, len(windows) > 0
}

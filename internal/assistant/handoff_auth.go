package assistant

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Inspection is deliberately metadata only. Local consistency does not prove
// that a refresh token is accepted by the service or that a live client uses it.
type handoffAuthInspection struct {
	LocallyConsistent bool   `json:"locally_consistent"`
	CandidateReady    bool   `json:"candidate_ready"`
	Reason            string `json:"reason"`
}

type handoffManagedStore struct {
	Version  int `json:"version"`
	Accounts map[string]struct {
		ID        string `json:"account_id"`
		Workspace string `json:"chatgpt_account_id"`
		Email     string `json:"email"`
		Identity  string `json:"id_token"`
		Refresh   string `json:"refresh_token"`
	} `json:"accounts"`
}

type handoffNativeAuth struct {
	Mode        string `json:"auth_mode"`
	LastRefresh string `json:"last_refresh"`
	APIKey      string `json:"OPENAI_API_KEY"`
	Tokens      struct {
		Access    string `json:"access_token"`
		Identity  string `json:"id_token"`
		Refresh   string `json:"refresh_token"`
		Workspace string `json:"account_id"`
	} `json:"tokens"`
}

// Never serialize a candidate: it holds the exact original auth record in memory.
// Its digest binds preparation to the same complete record at commit time.
type handoffAuthCandidate struct {
	auth      handoffNativeAuth
	raw       []byte
	source    string
	refreshed time.Time
	digest    [32]byte
}

func readNativeHandoffAuth(path string) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := readJSON(path, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Candidates must be one whole, unexpired auth record for this managed user and
// workspace. If its refresh token disagrees with CC Switch's current record we
// cannot know which generation is valid; do not splice records together.
func (r Reader) findHandoffAuthCandidate(accountID string, now time.Time) (handoffAuthCandidate, error) {
	var none handoffAuthCandidate
	if strings.TrimSpace(accountID) == "" {
		return none, errors.New("未指定备用账号")
	}
	var store handoffManagedStore
	if readJSON(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"), &store) != nil || store.Version != 2 {
		return none, errors.New("CC Switch 账号信息不可读取")
	}
	account, ok := store.Accounts[accountID]
	if !ok || account.ID != accountID || account.Workspace == "" || account.Refresh == "" || account.Identity == "" {
		return none, errors.New("备用账号认证不完整，请在 CC Switch 更新")
	}
	managed, ok := decodeTokenClaims(account.Identity)
	if !ok || managed.Auth.Workspace != account.Workspace {
		return none, errors.New("备用账号身份不可确认，请在 CC Switch 更新")
	}
	type candidateSource struct {
		raw   json.RawMessage
		label string
	}
	var sources []candidateSource
	for _, source := range []struct{ path, label string }{{".codex/auth.json", "Desktop"}, {".codex-cli/auth.json", "CLI"}} {
		if raw, err := readNativeHandoffAuth(filepath.Join(r.Home, source.path)); err == nil {
			sources = append(sources, candidateSource{raw: raw, label: source.label})
		}
	}
	var stash struct {
		Version int                        `json:"version"`
		Logins  map[string]json.RawMessage `json:"logins"`
	}
	if readJSON(filepath.Join(r.Home, ".cc-switch", "codex-login-stash.json"), &stash) == nil && stash.Version == 1 {
		keys := make([]string, 0, len(stash.Logins))
		for key := range stash.Logins {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			sources = append(sources, candidateSource{raw: stash.Logins[key], label: "CC Switch 暂存"})
		}
	}
	var best handoffAuthCandidate
	stale := false
	for _, source := range sources {
		var auth handoffNativeAuth
		if json.Unmarshal(source.raw, &auth) != nil || (auth.Mode != "" && auth.Mode != "chatgpt") || auth.APIKey != "" || auth.Tokens.Refresh == "" || auth.Tokens.Workspace != account.Workspace {
			continue
		}
		access, validAccess := decodeTokenClaims(auth.Tokens.Access)
		identity, validIdentity := decodeTokenClaims(auth.Tokens.Identity)
		refreshed, err := time.Parse(time.RFC3339Nano, auth.LastRefresh)
		if !validAccess || !validIdentity || err != nil || access.Auth != managed.Auth || identity.Auth != managed.Auth || access.Expires <= now.Add(30*time.Second).Unix() || access.NotBefore > now.Unix() {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(auth.Tokens.Refresh), []byte(account.Refresh)) != 1 {
			stale = true
			continue
		}
		if best.raw == nil || refreshed.After(best.refreshed) {
			raw := append([]byte(nil), source.raw...)
			best = handoffAuthCandidate{auth: auth, raw: raw, source: source.label, refreshed: refreshed, digest: sha256.Sum256(raw)}
		}
	}
	if best.raw == nil {
		if stale {
			return none, errors.New("备用账号的本机登录快照与 CC Switch 当前记录不同步；请通过 CC Switch 正常切换生成最新完整登录记录，此情况不代表必须重新登录")
		}
		return none, errors.New("未找到备用账号的有效完整登录记录；需从 CC Switch 的正常切换流程取得，不能仅使用旧暂存副本")
	}
	return best, nil
}

func (r Reader) inspectHandoffAuthCandidate(accountID string, now time.Time) handoffAuthInspection {
	candidate, err := r.findHandoffAuthCandidate(accountID, now)
	if err != nil {
		return handoffAuthInspection{Reason: err.Error()}
	}
	_ = candidate
	return handoffAuthInspection{LocallyConsistent: true, CandidateReady: true, Reason: "找到身份一致的完整凭证；仍需服务端验证和切换时复查。"}
}

func (c handoffAuthCandidate) quotaCredential() quotaCredential {
	claims, _ := decodeTokenClaims(c.auth.Tokens.Access)
	return quotaCredential{access: c.auth.Tokens.Access, workspace: c.auth.Tokens.Workspace, expires: claims.Expires, sources: []string{c.source}}
}

// Prepare is read-only. It is tied to one scope, target account, current auth
// bytes and candidate. A production caller must first prove its owned client has
// stopped; this package does not treat a caller-provided PID as that proof.
type handoffAuthPreparation struct {
	scope         string
	accountID     string
	currentHash   [32]byte
	candidateHash [32]byte
}

func (r Reader) prepareHandoffAuth(scope, targetID string, now time.Time) (handoffAuthPreparation, handoffAuthCandidate, error) {
	var prepared handoffAuthPreparation
	path, err := r.handoffAuthPath(scope)
	if err != nil {
		return prepared, handoffAuthCandidate{}, err
	}
	current, err := readProtectedHandoffFile(path)
	if err != nil {
		return prepared, handoffAuthCandidate{}, err
	}
	candidate, err := r.findHandoffAuthCandidate(targetID, now)
	if err != nil {
		return prepared, candidate, err
	}
	prepared = handoffAuthPreparation{scope: scope, accountID: targetID, currentHash: sha256.Sum256(current), candidateHash: candidate.digest}
	return prepared, candidate, nil
}

func (r Reader) handoffAuthPath(scope string) (string, error) {
	// Two paths that resolve to the same directory are not isolated, whatever
	// their names. Do not accidentally switch both clients at once.
	if sameExistingPath(filepath.Join(r.Home, ".codex"), filepath.Join(r.Home, ".codex-cli")) {
		return "", errors.New("Desktop 与 CLI 认证目录相同，无法单独切换")
	}
	switch scope {
	case "desktop":
		return filepath.Join(r.Home, ".codex", "auth.json"), nil
	case "cli":
		return filepath.Join(r.Home, ".codex-cli", "auth.json"), nil
	default:
		return "", errors.New("不支持的认证目录")
	}
}

func (r Reader) handoffCurrentMatchesAccount(raw []byte, accountID string) bool {
	var auth handoffNativeAuth
	if json.Unmarshal(raw, &auth) != nil || (auth.Mode != "" && auth.Mode != "chatgpt") || auth.APIKey != "" {
		return false
	}
	access, okA := decodeTokenClaims(auth.Tokens.Access)
	identity, okI := decodeTokenClaims(auth.Tokens.Identity)
	var store handoffManagedStore
	if !okA || !okI || access.Auth != identity.Auth || auth.Tokens.Workspace != access.Auth.Workspace || readJSON(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"), &store) != nil || store.Version != 2 {
		return false
	}
	account, ok := store.Accounts[accountID]
	managed, valid := decodeTokenClaims(account.Identity)
	return ok && account.ID == accountID && valid && managed.Auth == access.Auth && account.Workspace == access.Auth.Workspace
}

func readProtectedHandoffFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil, errors.New("目标认证文件不存在、不可读取或不是普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 8<<20 {
		return nil, errors.New("目标认证文件不可读取")
	}
	return data, nil
}

// checkPreparedHandoffAuth re-reads all relevant state. It detects changes up
// to the check, but cannot lock independent CC Switch or native clients.
func (r Reader) checkPreparedHandoffAuth(ctx context.Context, p handoffAuthPreparation, now time.Time, verify func(context.Context, quotaCredential) AccountQuota) (handoffAuthCandidate, []byte, error) {
	path, err := r.handoffAuthPath(p.scope)
	if err != nil {
		return handoffAuthCandidate{}, nil, err
	}
	current, err := readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(current) != p.currentHash {
		return handoffAuthCandidate{}, nil, errors.New("当前认证已变化，请重新检查")
	}
	candidate, err := r.findHandoffAuthCandidate(p.accountID, now)
	if err != nil {
		return handoffAuthCandidate{}, nil, err
	}
	if candidate.digest != p.candidateHash {
		return handoffAuthCandidate{}, nil, errors.New("备用认证已变化，请重新检查")
	}
	if verify == nil {
		return handoffAuthCandidate{}, nil, errors.New("未验证备用账号额度")
	}
	q := verify(ctx, candidate.quotaCredential())
	if q.Status != "ready" || !resumeQuotaReady(q) {
		return handoffAuthCandidate{}, nil, errors.New("备用账号认证或额度不可用，请在 CC Switch 检查")
	}
	return candidate, current, nil
}

package assistant

// Codex OAuth generation rules and marker format are adapted from CC Switch
// 4.0.21 (c4db63c60d2a1ddcb4bd9f81152b9c6126413659), MIT. See THIRD_PARTY_NOTICES.md.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ClientAccount struct {
	Scope     string `json:"scope"`
	Home      string `json:"home"`
	AccountID string `json:"account_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

type AccountAllocation struct {
	Clients      []ClientAccount `json:"clients"`
	Distinct     bool            `json:"distinct"`
	WorkingScope string          `json:"working_scope,omitempty"`
	Message      string          `json:"message"`
}

type NativeAccountCheck struct {
	WorkingScope string            `json:"working_scope"`
	Ready        bool              `json:"ready"`
	Allocation   AccountAllocation `json:"allocation"`
	Message      string            `json:"message"`
}

func (r Reader) CheckNativeAccounts(ctx context.Context, scope string) NativeAccountCheck {
	out := NativeAccountCheck{WorkingScope: scope, Allocation: r.AccountAllocation()}
	if scope != "desktop" && scope != "cli" {
		out.Message = "工作端无效"
		return out
	}
	if err := newNativeDualSwitcher(r).inspect(ctx, scope); err != nil {
		out.Message = err.Error()
		return out
	}
	out.Ready = true
	out.Message = "原生双端交换检查通过；尚未修改凭证或启动任务"
	return out
}

func (r Reader) clientAccountID(scope string) (string, error) {
	path, err := r.handoffAuthPath(scope)
	if err != nil {
		return "", err
	}
	raw, err := readProtectedHandoffFile(path)
	if err != nil {
		return "", err
	}
	var store handoffManagedStore
	if readJSON(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), &store) != nil || store.Version != 2 {
		return "", errors.New("账号库不可读取")
	}
	id := ""
	for account := range store.Accounts {
		if r.handoffCurrentMatchesAccount(raw, account) {
			if id != "" {
				return "", errors.New("账号身份映射不唯一")
			}
			id = account
		}
	}
	if id == "" {
		return "", errors.New("该端登录身份无法匹配账号库")
	}
	return id, nil
}

func (r Reader) AccountAllocation() AccountAllocation {
	a := AccountAllocation{Clients: []ClientAccount{}}
	state := r.HandoffStatus()
	if state.Enabled {
		a.WorkingScope = state.Scope
	}
	var store handoffManagedStore
	_ = readJSON(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), &store)
	for _, scope := range []string{"desktop", "cli"} {
		path, _ := r.handoffAuthPath(scope)
		entry := ClientAccount{Scope: scope, Home: filepath.Dir(path), Status: "unknown", Message: "磁盘登录身份未确认；不代表运行实例身份"}
		if id, err := r.clientAccountID(scope); err == nil {
			entry.AccountID, entry.Name, entry.Status = id, maskName(store.Accounts[id].Email), "matched"
			entry.Message = "磁盘身份已匹配；运行实例需在交接时核验"
		}
		a.Clients = append(a.Clients, entry)
	}
	a.Distinct = a.Clients[0].AccountID != "" && a.Clients[1].AccountID != "" && a.Clients[0].AccountID != a.Clients[1].AccountID
	a.Message = "两端磁盘账号不同；仅工作端参与额度交接"
	if !a.Distinct {
		a.Message = "两端磁盘账号相同或未确认；原生交接会先修复分配，待用端保持停止"
	}
	return a
}

type nativeAccountStore struct {
	raw      map[string]json.RawMessage
	accounts map[string]map[string]json.RawMessage
	hash     string
}

func jsonField[T any](m map[string]json.RawMessage, key string) (v T) {
	_ = json.Unmarshal(m[key], &v)
	return
}
func setJSON(m map[string]json.RawMessage, key string, value any) { m[key], _ = json.Marshal(value) }

func (r Reader) readNativeStore() (*nativeAccountStore, error) {
	raw, err := readProtectedHandoffFile(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"))
	if err != nil {
		return nil, err
	}
	s := &nativeAccountStore{hash: digestString(raw)}
	if json.Unmarshal(raw, &s.raw) != nil || jsonField[int](s.raw, "version") != 2 || json.Unmarshal(s.raw["accounts"], &s.accounts) != nil || len(s.accounts) != 2 {
		return nil, errors.New("原生切号需要两个托管账号和受支持的账号库")
	}
	return s, nil
}

func (s *nativeAccountStore) account(id string) (handoffNativeAuth, int64, error) {
	m, ok := s.accounts[id]
	identity := jsonField[string](m, "id_token")
	claims, valid := decodeTokenClaims(identity)
	workspace := jsonField[string](m, "chatgpt_account_id")
	refresh := jsonField[string](m, "refresh_token")
	if !ok || jsonField[string](m, "account_id") != id || !valid || claims.Auth.Workspace != workspace || refresh == "" {
		return handoffNativeAuth{}, 0, errors.New("托管账号登录材料不完整或身份不匹配")
	}
	a := handoffNativeAuth{Mode: "chatgpt"}
	a.Tokens.Identity, a.Tokens.Refresh, a.Tokens.Workspace = identity, refresh, workspace
	return a, jsonField[int64](m, "token_updated_at_ms"), nil
}

func (s *nativeAccountStore) adopt(id string, auth handoffNativeAuth, timestamp int64) {
	m := s.accounts[id]
	setJSON(m, "refresh_token", auth.Tokens.Refresh)
	setJSON(m, "id_token", auth.Tokens.Identity)
	setJSON(m, "chatgpt_account_id", auth.Tokens.Workspace)
	setJSON(m, "token_updated_at_ms", timestamp)
}

func (r Reader) saveNativeStore(s *nativeAccountStore) error {
	path := filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json")
	raw, err := readProtectedHandoffFile(path)
	if err != nil || digestString(raw) != s.hash {
		return errors.New("账号库在准备期间发生变化")
	}
	setJSON(s.raw, "accounts", s.accounts)
	data, err := json.MarshalIndent(s.raw, "", "  ")
	if err != nil {
		return errors.New("账号库编码失败")
	}
	if err = nativeAtomicWrite(path, data); err != nil {
		return err
	}
	s.hash = digestString(data)
	return nil
}

func nativeAtomicWrite(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("写入目标不是普通文件")
	} else if err != nil && !os.IsNotExist(err) {
		return errors.New("写入目标不可检查")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("不能创建私有目录")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".native-")
	if err != nil {
		return errors.New("不能准备原子写入")
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("私有文件同步失败")
	}
	if os.Rename(f.Name(), path) != nil {
		return errors.New("私有文件提交失败")
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		defer d.Close()
		if d.Sync() != nil {
			return errors.New("目录同步失败")
		}
	}
	return nil
}

func (r Reader) nativeBundle(ctx context.Context, s *nativeAccountStore, id string, client *http.Client, endpoint string, beforeRefresh func() error, returned func(handoffNativeAuth) error) (handoffNativeAuth, error) {
	managed, generation, err := s.account(id)
	if err != nil {
		return managed, err
	}
	var candidates []handoffNativeAuth
	for _, scope := range []string{"desktop", "cli"} {
		path, e := r.handoffAuthPath(scope)
		if e != nil {
			return managed, e
		}
		raw, e := readProtectedHandoffFile(path)
		if e != nil {
			continue
		}
		var a handoffNativeAuth
		if json.Unmarshal(raw, &a) != nil || !r.handoffCurrentMatchesAccount(raw, id) || a.Tokens.Refresh == "" {
			continue
		}
		if _, e = time.Parse(time.RFC3339Nano, a.LastRefresh); e != nil {
			return managed, errors.New("原生登录缺少可靠的刷新时间")
		}
		candidates = append(candidates, a)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, candidates[i].LastRefresh)
		b, _ := time.Parse(time.RFC3339Nano, candidates[j].LastRefresh)
		return a.After(b)
	})
	for _, a := range candidates {
		stamp, _ := time.Parse(time.RFC3339Nano, a.LastRefresh)
		materialChanged := a.Tokens.Refresh != managed.Tokens.Refresh || a.Tokens.Identity != managed.Tokens.Identity
		if materialChanged {
			if generation <= 0 || stamp.UnixMilli() == generation {
				return managed, errors.New("凭证代次不明确，保留记录等待核查")
			}
			if stamp.UnixMilli() < generation {
				continue
			}
			managed = a
			generation = stamp.UnixMilli()
			s.adopt(id, a, generation)
			if err = r.saveNativeStore(s); err != nil {
				return managed, err
			}
		}
		claims, valid := decodeTokenClaims(a.Tokens.Access)
		if valid && claims.NotBefore <= time.Now().Unix() && claims.Expires > time.Now().Add(60*time.Second).Unix() && a.Tokens.Refresh == managed.Tokens.Refresh && a.Tokens.Identity == managed.Tokens.Identity {
			return a, nil
		}
	}
	if beforeRefresh == nil || beforeRefresh() != nil {
		return managed, errors.New("认证刷新未登记，已停止")
	}
	values := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {managed.Tokens.Refresh}, "client_id": {"app_EMoamEEZ73f0CkXaXp7hrann"}, "scope": {"openid profile email"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return managed, errors.New("认证请求无法创建")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(req)
	if err != nil {
		return managed, errors.New("认证刷新结果未确认；不会自动重发")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return managed, errors.New("认证刷新未成功，请保留恢复记录")
	}
	var result struct {
		Access   string `json:"access_token"`
		Identity string `json:"id_token"`
		Refresh  string `json:"refresh_token"`
	}
	dec := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if dec.Decode(&result) != nil || result.Access == "" {
		return managed, errors.New("认证刷新响应不完整")
	}
	a := managed
	a.Tokens.Access = result.Access
	if result.Identity != "" {
		a.Tokens.Identity = result.Identity
	}
	if result.Refresh != "" {
		a.Tokens.Refresh = result.Refresh
	}
	access, okA := decodeTokenClaims(a.Tokens.Access)
	identity, okI := decodeTokenClaims(a.Tokens.Identity)
	expected, _ := decodeTokenClaims(managed.Tokens.Identity)
	if !okA || !okI || access.Auth != expected.Auth || identity.Auth != expected.Auth || access.NotBefore > time.Now().Unix() || access.Expires <= time.Now().Add(30*time.Second).Unix() {
		return managed, errors.New("刷新后的账号身份或有效期不匹配")
	}
	a.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)
	if returned == nil || returned(a) != nil {
		return a, errors.New("刷新结果尚未安全保存；保留记录等待核查")
	}
	stamp, _ := time.Parse(time.RFC3339Nano, a.LastRefresh)
	s.adopt(id, a, stamp.UnixMilli())
	if err = r.saveNativeStore(s); err != nil {
		return a, err
	}
	return a, nil
}

func nativeAuthBytes(a handoffNativeAuth) []byte {
	raw, _ := json.MarshalIndent(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "last_refresh": a.LastRefresh, "tokens": a.Tokens}, "", "  ")
	return raw
}

func nativeMarker(a handoffNativeAuth, id string) ([]byte, error) {
	parts := strings.Split(a.Tokens.Identity, ".")
	if len(parts) != 3 {
		return nil, errors.New("账号标记身份缺失")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("账号标记身份缺失")
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Subject == "" {
		return nil, errors.New("账号标记缺少稳定身份")
	}
	return json.MarshalIndent(map[string]any{"version": 3, "account_id": id, "chatgpt_account_id": a.Tokens.Workspace, "user_identity": "sub:" + claims.Subject}, "", "  ")
}

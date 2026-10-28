package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const nativeBackend = "native-dual"

var errNativeAllocationRepaired = errors.New("两端账号分配已修复")
var errNativeNotReady = errors.New("待用账号额度不足")

type nativeSwitchEnvironment interface {
	inspect(context.Context, string) (bool, error)
	quiesce(context.Context, string) error
	verify(context.Context, string) error
	restore(context.Context, bool) error
}

type nativeFile struct {
	Path    string `json:"path"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Existed bool   `json:"existed"`
	Payload string `json:"payload"`
}

// Payloads live in the private transaction directory, never in this journal.
type nativeSwitchTransaction struct {
	Version       int          `json:"version"`
	Authorization string       `json:"authorization"`
	Scope         string       `json:"scope"`
	Source        string       `json:"source"`
	Target        string       `json:"target"`
	Phase         string       `json:"phase"`
	Directory     string       `json:"directory"`
	CCWasRunning  bool         `json:"cc_was_running"`
	CLIProvider   string       `json:"cli_provider,omitempty"`
	StoreHash     string       `json:"store_hash,omitempty"`
	Files         []nativeFile `json:"files,omitempty"`
}

type nativeDualSwitcher struct {
	r          Reader
	env        nativeSwitchEnvironment
	client     *http.Client
	endpoint   string
	query      func(context.Context, quotaCredential) AccountQuota
	prepared   handoffNativeAuth
	ready      bool
	release    func()
	afterWrite func(int) error // isolated fault injection; never configured by the CLI.
}

func (r Reader) nativeTransactionPath() string {
	return filepath.Join(filepath.Dir(r.handoffPath()), "native-transaction.json")
}

func (r Reader) configureNativeHandoff(scope, id string, threshold float64) (HandoffState, error) {
	return r.configureHandoff(scope, id, threshold, true, nativeBackend)
}
func (n *nativeDualSwitcher) txn() (nativeSwitchTransaction, error) {
	var t nativeSwitchTransaction
	if readJSON(n.r.nativeTransactionPath(), &t) != nil || t.Version != 1 || filepath.Base(t.Directory) != t.Directory || !strings.HasPrefix(t.Directory, "native-") {
		return t, errors.New("原生切号记录不可确认")
	}
	return t, nil
}
func (n *nativeDualSwitcher) save(t nativeSwitchTransaction) error {
	return writeState(n.r.nativeTransactionPath(), t)
}
func (n *nativeDualSwitcher) dir(t nativeSwitchTransaction) string {
	return filepath.Join(filepath.Dir(n.r.handoffPath()), "backups", t.Directory)
}

func newNativeDualSwitcher(r Reader) *nativeDualSwitcher {
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return &nativeDualSwitcher{r: r, env: &nativeSystemEnvironment{r: r}, client: client, endpoint: "https://auth.openai.com/oauth/token", query: func(ctx context.Context, c quotaCredential) AccountQuota { return queryAccountQuota(ctx, client, c) }}
}

func (r Reader) nativeAccountPair(scope string) (string, string, error) {
	s, err := r.readNativeStore()
	if err != nil {
		return "", "", err
	}
	source, err := r.clientAccountID(scope)
	if err != nil {
		return "", "", err
	}
	target := ""
	for id := range s.accounts {
		if id != source {
			target = id
		}
	}
	if target == "" {
		return "", "", errors.New("缺少另一个账号")
	}
	return source, target, nil
}

func (r Reader) nativeBinding(ctx context.Context, account string) (ccSwitchBinding, error) {
	var rows []struct{ ID, Name, Account, Config string }
	err := r.query(ctx, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `SELECT id,name,json_extract(meta,'$.authBinding.accountId') AS account,settings_config AS config FROM providers WHERE app_type='codex' AND json_extract(meta,'$.authBinding.authProvider')='codex_oauth' ORDER BY id`, &rows)
	if err != nil || len(rows) != 2 {
		return ccSwitchBinding{}, errors.New("需要两张唯一绑定的 Codex OAuth 卡片")
	}
	var first map[string]json.RawMessage
	var second map[string]json.RawMessage
	if json.Unmarshal([]byte(rows[0].Config), &first) != nil || json.Unmarshal([]byte(rows[1].Config), &second) != nil || jsonField[string](first, "config") != jsonField[string](second, "config") {
		return ccSwitchBinding{}, errors.New("首版只支持两个账号使用相同的直连配置")
	}
	var delta map[string]any
	if _, err := toml.Decode(jsonField[string](first, "config"), &delta); err != nil {
		return ccSwitchBinding{}, errors.New("账号卡片配置不是受支持的直连配置")
	}
	for key := range delta {
		if key != "model_provider" && key != "model" {
			return ccSwitchBinding{}, errors.New("首版账号卡片仅支持直连供应商选择；其他卡片配置暂时搁置")
		}
	}
	// CC Switch can persist a model-only card. Accept existing matching model
	// metadata without copying the card config over either isolated client.
	if value, exists := delta["model"]; exists {
		model, ok := value.(string)
		if !ok || model == "" {
			return ccSwitchBinding{}, errors.New("账号卡片模型配置无效")
		}
		for _, root := range []string{".codex", ".codex-cli"} {
			var config struct {
				Model string `toml:"model"`
			}
			if _, err := toml.DecodeFile(filepath.Join(r.Home, root, "config.toml"), &config); err != nil || config.Model != model {
				return ccSwitchBinding{}, errors.New("账号卡片模型与两端现有配置不一致，暂不交换账号")
			}
		}
	}
	if value, exists := delta["model_provider"]; exists {
		name, ok := value.(string)
		if !ok || name == "" {
			return ccSwitchBinding{}, errors.New("账号卡片供应商配置无效")
		}
		for _, scope := range []string{"desktop", "cli"} {
			current, ok := r.managedResumeProvider(scope)
			if !ok || current != name {
				return ccSwitchBinding{}, errors.New("账号卡片与两端的现有直连路由不一致")
			}
		}
	}
	if rows[0].Account == rows[1].Account {
		return ccSwitchBinding{}, errors.New("两张卡片绑定了同一账号")
	}
	for _, row := range rows {
		if row.Account == account && row.ID != "" {
			return ccSwitchBinding{ID: row.ID, Name: row.Name}, nil
		}
	}
	return ccSwitchBinding{}, errors.New("目标账号没有唯一供应商绑定")
}

func (n *nativeDualSwitcher) inspect(ctx context.Context, scope string) error {
	_, target, err := n.r.nativeAccountPair(scope)
	if err != nil {
		return err
	}
	if _, err = n.r.nativeBinding(ctx, target); err != nil {
		return err
	}
	var settings struct {
		Dir string `json:"codexConfigDir"`
	}
	if readJSON(filepath.Join(n.r.Home, ".cc-switch/settings.json"), &settings) != nil {
		return errors.New("CC Switch 目录设置不可读取")
	}
	if settings.Dir == "" {
		settings.Dir = filepath.Join(n.r.Home, ".codex")
	}
	if strings.HasPrefix(settings.Dir, "~/") {
		settings.Dir = filepath.Join(n.r.Home, strings.TrimPrefix(settings.Dir, "~/"))
	}
	if !sameExistingPath(settings.Dir, filepath.Join(n.r.Home, ".codex")) && !sameExistingPath(settings.Dir, filepath.Join(n.r.Home, ".codex-cli")) {
		return errors.New("CC Switch 正在管理其他目录，双端原生交换暂时搁置")
	}
	if _, err = os.Lstat(n.r.nativeTransactionPath()); !os.IsNotExist(err) {
		return errors.New("存在原生切号恢复记录，请先核查")
	}
	var state struct {
		Apps map[string]struct {
			Mode    string          `json:"mode"`
			Pending json.RawMessage `json:"pending"`
			Stack   struct {
				Enabled bool `json:"enabled"`
			} `json:"stack"`
		} `json:"apps"`
	}
	path := filepath.Join(n.r.Home, ".cc-switch/live-state.json")
	if _, err = os.Stat(path); err == nil {
		if readJSON(path, &state) != nil {
			return errors.New("CC Switch 模式状态不可读取")
		}
		c := state.Apps["codex"]
		if c.Mode == "proxy" || c.Stack.Enabled || (len(c.Pending) > 0 && string(c.Pending) != "null") {
			return errors.New("首版不接管代理、Stack 或未完成的 CC Switch 操作")
		}
	}
	_, err = n.env.inspect(ctx, scope)
	return err
}

func authQuota(a handoffNativeAuth) quotaCredential {
	c, _ := decodeTokenClaims(a.Tokens.Access)
	return quotaCredential{access: a.Tokens.Access, workspace: a.Tokens.Workspace, expires: c.Expires, sources: []string{"原生双端账号协调器"}}
}

func (n *nativeDualSwitcher) prepare(ctx context.Context, s HandoffState) error {
	source, target, pairErr := n.r.nativeAccountPair(s.Scope)
	if pairErr != nil || !s.Enabled || s.Backend != nativeBackend || !validHandoffText(s.AuthorizationID) || n.r.HandoffStatus() != s || n.r.handoffCancelled(s) || s.SourceAccount != source || s.TargetAccount != target || source == target {
		return errors.New("原生准备的工作端账号分配或授权不匹配")
	}
	if n.ready {
		t, err := n.txn()
		if err != nil || t.Phase != "prepared" || t.Authorization != s.AuthorizationID || t.Scope != s.Scope || t.Source != s.SourceAccount || t.Target != s.TargetAccount {
			return errors.New("已有准备记录与当前授权不匹配")
		}
		return n.env.verify(ctx, s.Scope)
	}
	if err := n.inspect(ctx, s.Scope); err != nil {
		return err
	}
	unlock, err := lockWarmup(n.r.nativeTransactionPath() + ".lock")
	if err != nil {
		return errors.New("已有账号交换事务")
	}
	warmUnlock, err := lockWarmup(filepath.Join(n.r.warmupRoot(), "execution.lock"))
	if err != nil {
		unlock()
		return errors.New("正在等待预热请求结束")
	}
	n.release = func() { warmUnlock(); unlock(); n.release = nil }
	ccRunning, err := n.env.inspect(ctx, s.Scope)
	if err != nil {
		n.release()
		return err
	}
	base := filepath.Join(filepath.Dir(n.r.handoffPath()), "backups")
	if os.MkdirAll(base, 0700) != nil {
		n.release()
		return errors.New("恢复目录不可创建")
	}
	dir, err := os.MkdirTemp(base, "native-")
	if err != nil {
		n.release()
		return errors.New("恢复目录不可创建")
	}
	t := nativeSwitchTransaction{Version: 1, Authorization: s.AuthorizationID, Scope: s.Scope, Source: s.SourceAccount, Target: s.TargetAccount, Phase: "preparing", Directory: filepath.Base(dir), CCWasRunning: ccRunning}
	if err = n.save(t); err != nil {
		return err
	}
	if err = n.env.quiesce(ctx, s.Scope); err != nil {
		return err
	}
	if err = n.env.verify(ctx, s.Scope); err != nil {
		return err
	}
	if n.r.handoffCancelled(s) {
		return errors.New("原生准备授权已取消")
	}
	store, err := n.r.readNativeStore()
	if err != nil {
		return err
	}
	storeBefore, err := readProtectedHandoffFile(filepath.Join(n.r.Home, ".cc-switch/codex_oauth_auth.json"))
	if err != nil {
		return err
	}
	if err = writeCCSwitchBackup(filepath.Join(dir, "store-before.json"), storeBefore); err != nil {
		return err
	}
	a, err := n.r.nativeBundle(ctx, store, s.TargetAccount, n.client, n.endpoint, func() error {
		if n.r.handoffCancelled(s) {
			return errors.New("已取消刷新")
		}
		t.Phase = "refreshing"
		return n.save(t)
	}, func(a handoffNativeAuth) error {
		if e := nativeAtomicWrite(filepath.Join(dir, "target-auth.json"), nativeAuthBytes(a)); e != nil {
			return e
		}
		t.Phase = "refreshed-target"
		return n.save(t)
	})
	if err != nil {
		return err
	}
	if err = nativeAtomicWrite(filepath.Join(dir, "target-auth.json"), nativeAuthBytes(a)); err != nil {
		return err
	}
	other, otherErr := n.r.clientAccountID(nativeScopeOther(s.Scope))
	if otherErr != nil {
		return otherErr
	}
	if other == s.SourceAccount {
		if err = n.repairPassive(ctx, s, &t, a); err != nil {
			return err
		}
		return errNativeAllocationRepaired
	}
	q := n.query(ctx, authQuota(a))
	if !resumeQuotaReady(q) || !handoffTargetQuotaOK(q, s.Threshold) {
		if err = n.abortPreparation(ctx, t); err != nil {
			return err
		}
		return errNativeNotReady
	}
	n.prepared, n.ready = a, true
	t.Phase = "prepared"
	return n.save(t)
}

func (n *nativeDualSwitcher) evidence(ctx context.Context, s HandoffState) (AccountQuota, bool) {
	if !n.ready {
		return AccountQuota{}, false
	}
	t, err := n.txn()
	if err != nil || t.Authorization != s.AuthorizationID || t.Scope != s.Scope || t.Source != s.SourceAccount || t.Target != s.TargetAccount || (t.Phase != "prepared" && t.Phase != "installed") || n.env.verify(ctx, s.Scope) != nil {
		return AccountQuota{}, false
	}
	q := n.query(ctx, authQuota(n.prepared))
	return q, resumeQuotaReady(q) && handoffTargetQuotaOK(q, s.Threshold)
}

func nativeScopeOther(scope string) string {
	if scope == "desktop" {
		return "cli"
	}
	return "desktop"
}

func (n *nativeDualSwitcher) switchAccount(ctx context.Context, s HandoffState, stopped func(context.Context) error) error {
	if !s.Enabled || s.Backend != nativeBackend || s.PendingAction != string(HandoffSwitch) || n.r.HandoffStatus() != s || !n.ready || stopped == nil || stopped(ctx) != nil || n.env.verify(ctx, s.Scope) != nil || n.r.handoffCancelled(s) {
		return errors.New("两端尚未确认停止或授权已取消")
	}
	t, err := n.txn()
	if err != nil || t.Phase != "prepared" || t.Authorization != s.AuthorizationID || t.Scope != s.Scope || t.Source != s.SourceAccount || t.Target != s.TargetAccount {
		return errors.New("账号交换准备记录不匹配")
	}
	store, err := n.r.readNativeStore()
	if err != nil {
		return err
	}
	currentSource, e := n.r.clientAccountID(s.Scope)
	if e != nil || currentSource != s.SourceAccount {
		return errors.New("工作端来源账号已变化")
	}
	managedTarget, _, e := store.account(s.TargetAccount)
	if e != nil || managedTarget.Tokens.Refresh != n.prepared.Tokens.Refresh || managedTarget.Tokens.Identity != n.prepared.Tokens.Identity {
		return errors.New("已准备的目标凭证代次已变化")
	}
	source, err := n.r.nativeBundle(ctx, store, s.SourceAccount, n.client, n.endpoint, func() error { t.Phase = "refreshing-source"; return n.save(t) }, func(a handoffNativeAuth) error {
		if e := nativeAtomicWrite(filepath.Join(n.dir(t), "source-auth.json"), nativeAuthBytes(a)); e != nil {
			return e
		}
		t.Phase = "refreshed-source"
		return n.save(t)
	})
	if err != nil {
		return err
	}
	// Always resolve both projections from one coherent bundle per account.
	t.StoreHash = store.hash
	assignments := map[string]string{s.Scope: s.TargetAccount, nativeScopeOther(s.Scope): s.SourceAccount}
	bundles := map[string]handoffNativeAuth{s.SourceAccount: source, s.TargetAccount: n.prepared}
	cliBinding, err := n.r.nativeBinding(ctx, assignments["cli"])
	if err != nil {
		return err
	}
	t.CLIProvider = cliBinding.ID
	settingsPath := filepath.Join(n.r.Home, ".cc-switch/settings.json")
	settingsRaw, err := readProtectedHandoffFile(settingsPath)
	if err != nil {
		return err
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(settingsRaw, &settings) != nil {
		return errors.New("CC Switch 设置不可读取")
	}
	setJSON(settings, "codexConfigDir", filepath.Join(n.r.Home, ".codex-cli"))
	setJSON(settings, "currentProviderCodex", cliBinding.ID)
	settingsData, _ := json.MarshalIndent(settings, "", "  ")
	marker, err := nativeMarker(bundles[assignments["cli"]], assignments["cli"])
	if err != nil {
		return err
	}
	allocation, _ := json.MarshalIndent(map[string]any{"version": 1, "desktop": assignments["desktop"], "cli": assignments["cli"], "working_scope": s.Scope}, "", "  ")
	data := map[string][]byte{".cc-switch/settings.json": settingsData, ".cc-switch/codex_managed_oauth_live_auth.json": marker, "Library/Application Support/CodexHelper/account-allocation.json": allocation}
	for _, scope := range []string{"desktop", "cli"} {
		path, e := n.r.handoffAuthPath(scope)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(n.r.Home, path)
		data[rel] = nativeAuthBytes(bundles[assignments[scope]])
	}
	// Config bytes are guarded and preserved, not rewritten by account swapping.
	for _, root := range []string{".codex", ".codex-cli"} {
		raw, e := readProtectedHandoffFile(filepath.Join(n.r.Home, root, "config.toml"))
		if e != nil {
			return e
		}
		data[filepath.Join(root, "config.toml")] = raw
	}
	paths := []string{".codex/auth.json", ".codex-cli/auth.json", ".cc-switch/settings.json", ".cc-switch/codex_managed_oauth_live_auth.json", "Library/Application Support/CodexHelper/account-allocation.json", ".codex/config.toml", ".codex-cli/config.toml"}
	for i, path := range paths {
		before, e := readProtectedHandoffFile(filepath.Join(n.r.Home, path))
		exists := e == nil
		if e != nil && !nativeMissing(filepath.Join(n.r.Home, path)) {
			return errors.New("交换目标不可读取")
		}
		if exists {
			if e = writeCCSwitchBackup(filepath.Join(n.dir(t), fmt.Sprintf("before-%d", i)), before); e != nil {
				return e
			}
		}
		payload := fmt.Sprintf("after-%d", i)
		if e = writeCCSwitchBackup(filepath.Join(n.dir(t), payload), data[path]); e != nil {
			return e
		}
		t.Files = append(t.Files, nativeFile{Path: path, Before: digestString(before), After: digestString(data[path]), Existed: exists, Payload: payload})
	}
	t.Phase = "publishing"
	if err = n.save(t); err != nil {
		return err
	}
	if stopped(ctx) != nil || n.env.verify(ctx, s.Scope) != nil || n.r.handoffCancelled(s) {
		return errors.New("发布前运行状态已变化")
	}
	for _, file := range t.Files {
		if !n.matchesBefore(file) {
			return errors.New("交换文件在发布前发生变化")
		}
	}
	storeRaw, err := readProtectedHandoffFile(filepath.Join(n.r.Home, ".cc-switch/codex_oauth_auth.json"))
	if err != nil || digestString(storeRaw) != t.StoreHash {
		return errors.New("发布前账号库代次已变化")
	}
	// Both clients are stopped. Remove the old projections first so no pair of
	// published auth files temporarily assigns one account to both clients.
	for _, root := range []string{".codex", ".codex-cli"} {
		if err = os.Remove(filepath.Join(n.r.Home, root, "auth.json")); err != nil && !os.IsNotExist(err) {
			return errors.New("旧登录投影无法移除")
		}
	}
	for i, file := range t.Files {
		if stopped(ctx) != nil || n.env.verify(ctx, s.Scope) != nil || n.r.handoffCancelled(s) {
			return errors.New("交换期间进程或授权发生变化")
		}
		payload, e := readProtectedHandoffFile(filepath.Join(n.dir(t), file.Payload))
		if e != nil || digestString(payload) != file.After {
			return errors.New("交换载荷不可确认")
		}
		if err = nativeAtomicWrite(filepath.Join(n.r.Home, file.Path), payload); err != nil {
			return err
		}
		if n.afterWrite != nil {
			if err = n.afterWrite(i); err != nil {
				return err
			}
		}
	}
	if err = n.commitProvider(ctx, cliBinding.ID); err != nil {
		return err
	}
	t.Phase = "installed"
	return n.save(t)
}

// readProtectedHandoffFile intentionally hides OS errors; inspect existence
// separately without ever accepting symlinks as an absent target.
func nativeMissing(path string) bool { _, err := os.Lstat(path); return os.IsNotExist(err) }

func (n *nativeDualSwitcher) matchesBefore(f nativeFile) bool {
	p := filepath.Join(n.r.Home, f.Path)
	if !f.Existed {
		return nativeMissing(p)
	}
	raw, err := readProtectedHandoffFile(p)
	return err == nil && digestString(raw) == f.Before
}

func sqlText(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func (n *nativeDualSwitcher) commitProvider(ctx context.Context, id string) error {
	statement := `BEGIN IMMEDIATE; UPDATE providers SET is_current=CASE WHEN id=` + sqlText(id) + ` THEN 1 ELSE 0 END WHERE app_type='codex'; COMMIT;`
	cmd := exec.CommandContext(ctx, n.r.SQLite, filepath.Join(n.r.Home, ".cc-switch/cc-switch.db"))
	cmd.Stdin = strings.NewReader(statement)
	if cmd.Run() != nil {
		return errors.New("CLI 当前供应商状态提交失败")
	}
	return nil
}

func (n *nativeDualSwitcher) installed(s HandoffState) bool {
	t, err := n.txn()
	if err != nil || t.Authorization != s.AuthorizationID || t.Scope != s.Scope || t.Source != s.SourceAccount || t.Target != s.TargetAccount || (t.Phase != "installed" && t.Phase != "verified") {
		return false
	}
	for _, f := range t.Files {
		raw, e := readProtectedHandoffFile(filepath.Join(n.r.Home, f.Path))
		if e != nil || digestString(raw) != f.After {
			return false
		}
	}
	d, err := n.r.clientAccountID("desktop")
	c, e := n.r.clientAccountID("cli")
	if err != nil || e != nil || d == c {
		return false
	}
	store, e := n.r.readNativeStore()
	if e != nil {
		return false
	}
	for scope, id := range map[string]string{"desktop": d, "cli": c} {
		managed, _, e := store.account(id)
		if e != nil {
			return false
		}
		path, _ := n.r.handoffAuthPath(scope)
		raw, e := readProtectedHandoffFile(path)
		var a handoffNativeAuth
		if e != nil || json.Unmarshal(raw, &a) != nil || a.Tokens.Refresh != managed.Tokens.Refresh || a.Tokens.Identity != managed.Tokens.Identity {
			return false
		}
	}
	active := d
	if s.Scope == "cli" {
		active = c
	}
	if active != s.TargetAccount {
		return false
	}
	b := &desktopCCSwitch{r: n.r}
	return b.providerCurrent(context.Background(), t.CLIProvider)
}

func (n *nativeDualSwitcher) finish(ctx context.Context, s HandoffState, verify func(context.Context, string) error) error {
	if !n.installed(s) || verify == nil || verify(ctx, s.TargetAccount) != nil {
		return errors.New("双端交换尚未通过新实例核验")
	}
	t, _ := n.txn()
	t.Phase = "verified"
	if err := n.save(t); err != nil {
		return err
	}
	if err := n.env.restore(ctx, t.CCWasRunning); err != nil {
		return err
	}
	if err := os.RemoveAll(n.dir(t)); err != nil {
		return errors.New("临时凭证恢复副本尚未清理")
	}
	if err := os.Remove(n.r.nativeTransactionPath()); err != nil {
		return errors.New("原生交接记录尚未清理")
	}
	n.ready = false
	n.prepared = handoffNativeAuth{}
	n.close()
	return nil
}

func (n *nativeDualSwitcher) close() {
	if n.release != nil {
		n.release()
	}
}

func (n *nativeDualSwitcher) abortPreparation(ctx context.Context, t nativeSwitchTransaction) error {
	if t.Phase == "publishing" || t.Phase == "installed" || t.Phase == "publishing-repair" {
		return errors.New("已经开始发布，不能丢弃恢复记录")
	}
	if err := n.env.restore(ctx, t.CCWasRunning); err != nil {
		return err
	}
	if err := os.RemoveAll(n.dir(t)); err != nil {
		return err
	}
	if err := os.Remove(n.r.nativeTransactionPath()); err != nil {
		return err
	}
	n.ready = false
	n.close()
	return nil
}

// Repair duplicate disk assignments before considering quota. The active
// client's login and task are untouched; only the stopped standby is assigned
// the other managed account. It never starts the standby client or a new turn.
func (n *nativeDualSwitcher) repairPassive(ctx context.Context, s HandoffState, t *nativeSwitchTransaction, target handoffNativeAuth) error {
	activePath, err := n.r.handoffAuthPath(s.Scope)
	if err != nil {
		return err
	}
	activeRaw, err := readProtectedHandoffFile(activePath)
	if err != nil {
		return err
	}
	if !n.r.handoffCurrentMatchesAccount(activeRaw, s.SourceAccount) {
		return errors.New("修复分配前工作端账号已变化")
	}
	var source handoffNativeAuth
	if json.Unmarshal(activeRaw, &source) != nil {
		return errors.New("工作端认证不可读取")
	}
	other := nativeScopeOther(s.Scope)
	otherPath, err := n.r.handoffAuthPath(other)
	if err != nil {
		return err
	}
	cliID, cliAuth := s.SourceAccount, source
	if other == "cli" {
		cliID, cliAuth = s.TargetAccount, target
	}
	binding, err := n.r.nativeBinding(ctx, cliID)
	if err != nil {
		return err
	}
	t.CLIProvider = binding.ID
	settingsPath := filepath.Join(n.r.Home, ".cc-switch/settings.json")
	settingsRaw, err := readProtectedHandoffFile(settingsPath)
	if err != nil {
		return err
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(settingsRaw, &settings) != nil {
		return errors.New("CC Switch 设置不可读取")
	}
	setJSON(settings, "codexConfigDir", filepath.Join(n.r.Home, ".codex-cli"))
	setJSON(settings, "currentProviderCodex", binding.ID)
	settingsData, _ := json.MarshalIndent(settings, "", "  ")
	marker, err := nativeMarker(cliAuth, cliID)
	if err != nil {
		return err
	}
	otherRel, _ := filepath.Rel(n.r.Home, otherPath)
	paths := []string{otherRel, ".cc-switch/settings.json", ".cc-switch/codex_managed_oauth_live_auth.json"}
	data := [][]byte{nativeAuthBytes(target), settingsData, marker}
	for i, path := range paths {
		before, e := readProtectedHandoffFile(filepath.Join(n.r.Home, path))
		exists := e == nil
		if e != nil && !nativeMissing(filepath.Join(n.r.Home, path)) {
			return errors.New("修复目标不可读取")
		}
		if exists {
			if e = writeCCSwitchBackup(filepath.Join(n.dir(*t), fmt.Sprintf("before-%d", i)), before); e != nil {
				return e
			}
		}
		payload := fmt.Sprintf("after-%d", i)
		if e = writeCCSwitchBackup(filepath.Join(n.dir(*t), payload), data[i]); e != nil {
			return e
		}
		t.Files = append(t.Files, nativeFile{Path: path, Before: digestString(before), After: digestString(data[i]), Existed: exists, Payload: payload})
	}
	t.Phase = "publishing-repair"
	if err = n.save(*t); err != nil {
		return err
	}
	for _, f := range t.Files {
		if !n.matchesBefore(f) {
			return errors.New("修复目标已被外部修改")
		}
	}
	for i, f := range t.Files {
		if n.env.verify(ctx, s.Scope) != nil || n.r.handoffCancelled(s) {
			return errors.New("修复期间待用端或授权发生变化")
		}
		current, e := readProtectedHandoffFile(activePath)
		if e != nil || digestString(current) != digestString(activeRaw) {
			return errors.New("修复期间工作端认证发生变化")
		}
		if err = nativeAtomicWrite(filepath.Join(n.r.Home, f.Path), data[i]); err != nil {
			return err
		}
		if n.afterWrite != nil {
			if err = n.afterWrite(i); err != nil {
				return err
			}
		}
	}
	if err = n.commitProvider(ctx, binding.ID); err != nil {
		return err
	}
	a := n.r.AccountAllocation()
	if !a.Distinct {
		return errors.New("两端账号分配尚未分开")
	}
	t.Phase = "repaired"
	if err = n.save(*t); err != nil {
		return err
	}
	if err = n.env.restore(ctx, t.CCWasRunning); err != nil {
		return err
	}
	if err = os.RemoveAll(n.dir(*t)); err != nil {
		return err
	}
	if err = os.Remove(n.r.nativeTransactionPath()); err != nil {
		return err
	}
	n.ready = false
	n.close()
	return nil
}

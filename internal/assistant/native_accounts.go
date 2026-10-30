package assistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	errNativeBindings       = errors.New("账号绑定状态不可读取，请先检查账号管理")
	errNativePending        = errors.New("账号操作结果待核查，暂不继续会话")
	errNativeReapply        = errors.New("账号重新登录后，请先重新应用到使用端")
	errDesktopCCSwitchOwner = errors.New("Desktop 由 CC Switch 管理，请在 CC Switch 中切换账号")
	errDesktopOwnerUnknown  = errors.New("CC Switch 设置不可读取或无效，无法确认 Desktop 管理方；已停止写入")
)

const staleDesktopBindingNotice = "Desktop 由 CC Switch 管理；检测到旧的小助手 Desktop 绑定，可在授权管理中核查并清除 Desktop 绑定。"

type nativeBinding struct {
	AccountID    string `json:"account_id"`
	NeedsReapply bool   `json:"needs_reapply"`
}

type nativeBindings struct {
	Version int            `json:"version"`
	Desktop *nativeBinding `json:"desktop"`
	CLI     *nativeBinding `json:"cli"`
	// Kept only to warn and to avoid reusing a potentially superseded token;
	// never treat this as proof of the current Desktop binding.
	staleDesktop *nativeBinding
}

func (b nativeBindings) reapplyBlocked() map[string]bool {
	blocked := map[string]bool{}
	// An ignored Desktop binding cannot establish ownership, but its reapply
	// flag is still a reason to reject potentially superseded credentials.
	for _, current := range []*nativeBinding{b.Desktop, b.CLI, b.staleDesktop} {
		if current != nil && current.NeedsReapply {
			blocked[current.AccountID] = true
		}
	}
	return blocked
}

func (b nativeBindings) allowsLiveCredential(account, source string) bool {
	if source == "Desktop" && b.staleDesktop != nil && b.staleDesktop.AccountID == account && b.staleDesktop.NeedsReapply {
		return false
	}
	if !b.reapplyBlocked()[account] {
		return true
	}
	var binding *nativeBinding
	switch source {
	case "Desktop":
		binding = b.Desktop
	case "CLI":
		binding = b.CLI
	}
	return binding != nil && binding.AccountID == account && !binding.NeedsReapply
}

// An account can be bound on both ends. Even when only one end has a usable
// credential after re-login, inspect both bound histories before warmup.
func (r Reader) warmupActivitySources(account string, sources []string) ([]string, error) {
	if !r.nativeAccountPreview() {
		return sources, nil
	}
	if r.nativeAccountPending() {
		return nil, errNativePending
	}
	bindings, err := r.readNativeBindings()
	if err != nil {
		return nil, err
	}
	blocked := bindings.reapplyBlocked()
	for _, source := range sources {
		if (source == "小助手缓存" && blocked[account]) ||
			((source == "Desktop" || source == "CLI") && !bindings.allowsLiveCredential(account, source)) {
			return nil, errNativeReapply
		}
	}
	result := append([]string(nil), sources...)
	for _, pair := range []struct {
		name    string
		binding *nativeBinding
	}{{"Desktop", bindings.Desktop}, {"CLI", bindings.CLI}} {
		if pair.binding == nil || pair.binding.AccountID != account {
			continue
		}
		found := false
		for _, source := range result {
			found = found || source == pair.name
		}
		if !found {
			result = append(result, pair.name)
		}
	}
	if bindings.staleDesktop != nil {
		// The ignored binding cannot tell us which account Desktop now uses.
		// Conservatively inspect Desktop activity even for a CLI-only source.
		found := false
		for _, source := range result {
			found = found || source == "Desktop"
		}
		if !found {
			result = append(result, "Desktop")
		}
	}
	return result, nil
}

// A targeted re-login invalidates the old bound credentials until each
// affected client is explicitly reapplied. Do not let the Go reader bypass
// the manager's needs_reapply gate by borrowing an old live auth.json.
func (r Reader) readNativeBindings() (nativeBindings, error) {
	bindings := nativeBindings{Version: 1}
	if !r.nativeAccountPreview() {
		return bindings, nil
	}
	path := filepath.Join(r.helperStateRoot(), "accounts", "bindings.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return bindings, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nativeBindings{}, errNativeBindings
	}
	if readJSON(path, &bindings) != nil || bindings.Version != 1 {
		return nativeBindings{}, errNativeBindings
	}
	for _, current := range []*nativeBinding{bindings.Desktop, bindings.CLI} {
		if current == nil {
			continue
		}
		if current.AccountID == "" {
			return nativeBindings{}, errNativeBindings
		}
	}
	if owned, err := r.desktopCCSwitchOwner(); owned || err != nil {
		bindings.staleDesktop = bindings.Desktop
		bindings.Desktop = nil
	}
	return bindings, nil
}

// Report the legacy binding without changing the source file. An unreadable
// file stays an error instead of being mistaken for an absent binding.
func (r Reader) desktopBindingLimitation() string {
	if !r.nativeAccountPreview() || !r.desktopWriterIsCCSwitch() {
		return ""
	}
	bindings, err := r.readNativeBindings()
	if err != nil {
		return err.Error()
	}
	if bindings.staleDesktop != nil {
		return staleDesktopBindingNotice
	}
	return ""
}

func (r Reader) requireNativeWriteOwner(scope string) error {
	if scope == "desktop" {
		owned, err := r.desktopCCSwitchOwner()
		if err != nil {
			return err
		}
		if owned {
			return errDesktopCCSwitchOwner
		}
	}
	return nil
}

// Fill missing live tokens from the one running account manager. Access tokens
// cross only the private local socket, remain in memory, and are never saved
// in warmup state or used to rotate credentials.
func (r Reader) warmupCredentials(ctx context.Context, now time.Time) map[string]quotaCredential {
	credentials := r.quotaCredentials(now)
	if !r.nativeAccountPreview() {
		return credentials
	}
	bindings, err := r.readNativeBindings()
	if err != nil || r.nativeAccountPending() {
		return map[string]quotaCredential{}
	}
	blocked := bindings.reapplyBlocked()
	for id, credential := range credentials {
		for _, source := range credential.sources {
			if !bindings.allowsLiveCredential(id, source) {
				delete(credentials, id)
				break
			}
		}
	}
	var store managedQuotaStore
	if readJSON(r.managedAccountsStorePath(), &store) != nil || store.Version != 2 {
		return credentials
	}
	missing := false
	for id := range store.Accounts {
		if blocked[id] && credentials[id].access == "" {
			continue
		}
		if _, ok := credentials[id]; !ok {
			missing = true
			break
		}
	}
	if !missing {
		return credentials
	}
	manager, err := r.connectNativeManager(ctx, false)
	if err != nil {
		return credentials
	}
	defer manager.Close()
	for id := range store.Accounts {
		if blocked[id] {
			continue
		}
		if _, ok := credentials[id]; ok {
			continue
		}
		if credential, err := manager.warmupCredential(ctx, id, now, store); err == nil {
			credentials[id] = credential
		}
	}
	return credentials
}

func (r Reader) automationAccounts(ctx context.Context) []Account {
	if r.nativeAccountPreview() {
		accounts, _ := r.nativeAccounts(nil)
		return accounts
	}
	accounts, _ := r.accounts(ctx, filepath.Join(r.Home, ".cc-switch"), "", nil)
	return accounts
}

// The preview has its own account and automation state. It never reads or
// changes the stable helper's policies or CC Switch's account store.
func (r Reader) nativeAccountPreview() bool { return os.Getenv("CODEX_HELPER_NATIVE_PREVIEW") == "1" }

func (r Reader) helperStateRoot() string {
	name := "CodexHelper"
	if r.nativeAccountPreview() {
		name = "CodexHelper-AccountPreview"
	}
	return filepath.Join(r.Home, "Library", "Application Support", name)
}

func (r Reader) managedAccountsStorePath() string {
	if r.nativeAccountPreview() {
		return filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json")
	}
	return filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json")
}

func (r Reader) nativeAccountPending() bool {
	if !r.nativeAccountPreview() {
		return false
	}
	_, err := os.Lstat(filepath.Join(r.helperStateRoot(), "accounts", "pending.json"))
	return !os.IsNotExist(err)
}

// Binding and file identity are necessary local checks, not evidence of the
// account used by a running client. The coordinator performs its own runtime
// checks before any continuation.
func (r Reader) nativeHandoffInstalled(scope, account string) bool {
	if r.requireNativeWriteOwner(scope) != nil {
		return false
	}
	root := filepath.Join(r.helperStateRoot(), "accounts")
	if r.nativeAccountPending() {
		return false
	}
	var bindings struct {
		Version int `json:"version"`
		Desktop *struct {
			AccountID    string `json:"account_id"`
			NeedsReapply bool   `json:"needs_reapply"`
		} `json:"desktop"`
		CLI *struct {
			AccountID    string `json:"account_id"`
			NeedsReapply bool   `json:"needs_reapply"`
		} `json:"cli"`
	}
	if readJSON(filepath.Join(root, "bindings.json"), &bindings) != nil || bindings.Version != 1 {
		return false
	}
	var id string
	var reapply bool
	switch scope {
	case "desktop":
		if bindings.Desktop == nil {
			return false
		}
		id, reapply = bindings.Desktop.AccountID, bindings.Desktop.NeedsReapply
	case "cli":
		if bindings.CLI == nil {
			return false
		}
		id, reapply = bindings.CLI.AccountID, bindings.CLI.NeedsReapply
	default:
		return false
	}
	if id != account || reapply {
		return false
	}
	path, err := r.handoffAuthPath(scope)
	if err != nil {
		return false
	}
	raw, err := readProtectedHandoffFile(path)
	return err == nil && r.handoffCurrentMatchesAccount(raw, account)
}

func (r Reader) nativeAccounts(warnings []string) ([]Account, []string) {
	var store struct {
		Version  int `json:"version"`
		Accounts map[string]struct {
			ID    string `json:"account_id"`
			Email string `json:"email"`
		} `json:"accounts"`
	}
	path := filepath.Join(r.helperStateRoot(), "accounts", "codex_oauth_auth.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []Account{}, warnings
	}
	if err := readJSON(path, &store); err != nil || store.Version != 2 {
		return []Account{}, append(warnings, "小助手账号库不可读取。")
	}
	accounts := make([]Account, 0, len(store.Accounts))
	for key, value := range store.Accounts {
		if key == "" || key != value.ID {
			return []Account{}, append(warnings, "小助手账号库索引无效。")
		}
		name := maskName(value.Email)
		if name == "" {
			name = "ChatGPT 账号"
		}
		accounts = append(accounts, Account{ID: key, Name: name, ProviderNames: []string{}})
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Name != accounts[j].Name {
			return accounts[i].Name < accounts[j].Name
		}
		return accounts[i].ID < accounts[j].ID
	})
	return accounts, warnings
}

// The manager provides a read-only union for display. Keep automationAccounts
// on nativeAccounts so listing a CC Switch account never grants it prewarm or
// switching authority, and do not import any credential into the helper.
func (r Reader) nativeDisplayAccounts(ctx context.Context, warnings []string) ([]Account, []string) {
	accounts, warnings := r.nativeAccounts(warnings)
	client, err := r.connectNativeManager(ctx, false)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json")); statErr == nil {
			warnings = append(warnings, "CC Switch 账号同步暂不可用；当前显示小助手账号。")
		}
		return accounts, warnings
	}
	defer client.Close()
	var result struct {
		Accounts []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Managed *bool  `json:"managed"`
		} `json:"accounts"`
		CCSwitchStatus string `json:"cc_switch_status"`
	}
	if err := client.call(ctx, "list", nil, &result); err != nil || result.Accounts == nil {
		return accounts, append(warnings, "CC Switch 账号同步暂不可用；当前显示小助手账号。")
	}
	if result.CCSwitchStatus == "unavailable" {
		warnings = append(warnings, "CC Switch 账号库暂不可读取，列表可能不完整。")
	}
	for _, entry := range result.Accounts {
		if entry.Managed == nil || *entry.Managed || !strings.HasPrefix(entry.ID, "cc:") || len(entry.ID) != 67 {
			continue
		}
		name := maskName(entry.Name)
		if name == "" {
			name = "ChatGPT 账号"
		}
		accounts = append(accounts, Account{ID: entry.ID, Name: name, ProviderNames: []string{"CC Switch"}, ExternalOnly: true})
	}
	return accounts, warnings
}

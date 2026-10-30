package assistant

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type ccSwitchBinding struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type ccSwitchTransaction struct {
	Version                                    int    `json:"version"`
	SessionKey                                 string `json:"session_key"`
	Source, Target, Provider, Phase, BackupDir string
	AuthBefore, ConfigBefore, AuthAfter        string
}
type desktopCCSwitch struct {
	r       Reader
	control string
	run     func(context.Context, string, string) error
}

func (r Reader) ccSwitchTransactionPath() string {
	return filepath.Join(filepath.Dir(r.handoffPath()), "ccswitch-transaction.json")
}

// An absent settings file means no CC Switch owner. An existing file defaults
// to .codex when the directory is omitted or empty. Invalid/unreadable settings
// are unknown, not permission to let another writer change Desktop.
func (r Reader) desktopCCSwitchOwner() (bool, error) {
	path := filepath.Join(r.Home, ".cc-switch/settings.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, errDesktopOwnerUnknown
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return false, errDesktopOwnerUnknown
	}
	f, err := os.Open(path)
	if err != nil {
		return false, errDesktopOwnerUnknown
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return false, errDesktopOwnerUnknown
	}
	rawSettings, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil || len(rawSettings) > 8<<20 || !utf8.Valid(rawSettings) || !validJSONSurrogates(rawSettings) {
		return false, errDesktopOwnerUnknown
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(rawSettings, &settings) != nil || settings == nil {
		return false, errDesktopOwnerUnknown
	}
	dir := ""
	if raw, ok := settings["codexConfigDir"]; ok {
		if string(raw) == "null" || json.Unmarshal(raw, &dir) != nil {
			return false, errDesktopOwnerUnknown
		}
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = filepath.Join(r.Home, ".codex")
	}
	if dir == "~" {
		dir = r.Home
	} else if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(r.Home, dir[2:])
	}
	if !filepath.IsAbs(dir) {
		return false, errDesktopOwnerUnknown
	}
	dir = filepath.Clean(dir)
	desktop := filepath.Clean(filepath.Join(r.Home, ".codex"))
	return dir == desktop || sameExistingPath(dir, desktop), nil
}

// Go's JSON decoder otherwise replaces unpaired UTF-16 surrogate escapes,
// while the Rust service rejects them. Treat either form of invalid Unicode as
// an unknown owner so both writers make the same fail-closed decision.
func validJSONSurrogates(data []byte) bool {
	hex4 := func(raw []byte) (uint16, bool) {
		if len(raw) != 4 {
			return 0, false
		}
		var value uint16
		for _, c := range raw {
			value <<= 4
			switch {
			case c >= '0' && c <= '9':
				value |= uint16(c - '0')
			case c >= 'a' && c <= 'f':
				value |= uint16(c-'a') + 10
			case c >= 'A' && c <= 'F':
				value |= uint16(c-'A') + 10
			default:
				return 0, false
			}
		}
		return value, true
	}
	inString := false
	for i := 0; i < len(data); {
		switch {
		case data[i] == '"':
			inString = !inString
			i++
		case inString && data[i] == '\\':
			if i+1 >= len(data) {
				return false
			}
			if data[i+1] != 'u' {
				i += 2
				continue
			}
			if i+6 > len(data) {
				return false
			}
			value, ok := hex4(data[i+2 : i+6])
			if !ok {
				return false
			}
			if value >= 0xd800 && value <= 0xdbff {
				if i+12 > len(data) || data[i+6] != '\\' || data[i+7] != 'u' {
					return false
				}
				low, ok := hex4(data[i+8 : i+12])
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 12
			} else {
				if value >= 0xdc00 && value <= 0xdfff {
					return false
				}
				i += 6
			}
		default:
			i++
		}
	}
	return true
}

func (r Reader) desktopWriterIsCCSwitch() bool {
	owned, err := r.desktopCCSwitchOwner()
	return err == nil && owned
}

// Resolve independent local key spaces by workspace and, where present,
// user/email. Ambiguity fails closed even if one key happens to be equal.
func (r Reader) ccSwitchAccountKey(account string) (string, error) {
	var native, cc handoffManagedStore
	if readJSON(r.managedAccountsStorePath(), &native) != nil || native.Version != 2 || readJSON(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), &cc) != nil || cc.Version != 2 {
		return "", errors.New("无法读取小助手或 CC Switch 账号信息")
	}
	a, ok := native.Accounts[account]
	if !ok || a.ID != account || a.Workspace == "" {
		return "", errors.New("小助手备用账号身份不可确认")
	}
	aClaims, aValid := decodeTokenClaims(a.Identity)
	if aValid && aClaims.Auth.Workspace != a.Workspace {
		return "", errors.New("小助手备用账号身份不一致")
	}
	key := ""
	for id, candidate := range cc.Accounts {
		if id == "" || candidate.ID != id || candidate.Workspace != a.Workspace {
			continue
		}
		bClaims, bValid := decodeTokenClaims(candidate.Identity)
		if bValid && bClaims.Auth.Workspace != candidate.Workspace {
			continue
		}
		if aValid && bValid && aClaims.Auth.User != bClaims.Auth.User {
			continue
		}
		if a.Email != "" && candidate.Email != "" && !strings.EqualFold(strings.TrimSpace(a.Email), strings.TrimSpace(candidate.Email)) {
			continue
		}
		if key != "" {
			return "", errors.New("备用账号匹配到多个 CC Switch 账号，无法确定切换目标")
		}
		key = id
	}
	if key == "" {
		return "", errors.New("未找到与备用账号匹配的 CC Switch 账号")
	}
	return key, nil
}

func (r Reader) ccSwitchBinding(ctx context.Context, account string) (ccSwitchBinding, error) {
	if !r.desktopWriterIsCCSwitch() {
		return ccSwitchBinding{}, errors.New("CC Switch 的配置目录必须为 Desktop 的 .codex")
	}
	key, err := r.ccSwitchAccountKey(account)
	if err != nil {
		return ccSwitchBinding{}, err
	}
	var rows []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Account string `json:"account"`
	}
	if err := r.query(ctx, filepath.Join(r.Home, ".cc-switch/cc-switch.db"), `SELECT id,name,json_extract(meta,'$.authBinding.accountId') AS account FROM providers WHERE app_type='codex' AND json_extract(meta,'$.authBinding.authProvider')='codex_oauth'`, &rows); err != nil {
		return ccSwitchBinding{}, err
	}
	var out ccSwitchBinding
	for _, row := range rows {
		if row.Account == key {
			if out.ID != "" {
				return out, errors.New("备用账号绑定了多张卡片，请保留唯一切换目标")
			}
			out = ccSwitchBinding{row.ID, row.Name}
		}
	}
	if out.ID == "" || out.Name == "" {
		return out, errors.New("未找到备用账号的 CC Switch 供应商卡片")
	}
	for _, row := range rows {
		if row.Name == out.Name && row.ID != out.ID {
			return ccSwitchBinding{}, errors.New("CC Switch 卡片名称不唯一")
		}
	}
	return out, nil
}

func newDesktopCCSwitch(r Reader) (*desktopCCSwitch, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, errors.New("无法定位本机控制程序")
	}
	control := filepath.Join(filepath.Dir(exe), "CodexHelper")
	if info, err := os.Stat(control); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("请从小助手应用内使用 CC Switch 自动切换")
	}
	b := &desktopCCSwitch{r: r, control: control}
	b.run = func(ctx context.Context, action, name string) error {
		out, err := exec.CommandContext(ctx, control, "--ccswitch-"+action, name).Output()
		if err != nil || len(out) > 4096 {
			return errors.New("CC Switch 本机控制未完成")
		}
		var reply struct {
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		}
		if json.Unmarshal(out, &reply) != nil {
			return errors.New("CC Switch 本机控制返回无效")
		}
		if !reply.OK {
			if reply.Code == "permissionRequired" {
				return errors.New("请在系统设置的辅助功能中允许 Codex 小助手控制 CC Switch")
			}
			return errors.New("CC Switch 卡片未找到或切换按钮不可用")
		}
		return nil
	}
	return b, nil
}

func (b *desktopCCSwitch) inspect(ctx context.Context, target string) error {
	row, err := b.r.ccSwitchBinding(ctx, target)
	if err != nil {
		return err
	}
	if b.run == nil {
		return errors.New("CC Switch 控制不可用")
	}
	return b.run(ctx, "inspect", row.Name)
}

// Read the newly generated live file, never synthesize a bundle from an old
// access token and a newer manager refresh token.
func (r Reader) ccSwitchLiveMatches(account string) ([]byte, error) {
	raw, err := readProtectedHandoffFile(filepath.Join(r.Home, ".codex/auth.json"))
	if err != nil {
		return nil, err
	}
	var native handoffNativeAuth
	var store handoffManagedStore
	if json.Unmarshal(raw, &native) != nil || !r.handoffCurrentMatchesAccount(raw, account) || readJSON(filepath.Join(r.Home, ".cc-switch/codex_oauth_auth.json"), &store) != nil {
		return nil, errors.New("CC Switch 写出的登录身份不匹配")
	}
	key, err := r.ccSwitchAccountKey(account)
	if err != nil {
		return nil, err
	}
	a, ok := store.Accounts[key]
	claims, valid := decodeTokenClaims(native.Tokens.Access)
	if !ok || !valid || claims.Expires <= time.Now().Add(30*time.Second).Unix() || native.Tokens.Refresh == "" || a.Refresh == "" || subtle.ConstantTimeCompare([]byte(native.Tokens.Refresh), []byte(a.Refresh)) != 1 {
		return nil, errors.New("CC Switch 写出的完整登录记录尚未同步")
	}
	return raw, nil
}

func digestString(data []byte) string { d := sha256.Sum256(data); return hex.EncodeToString(d[:]) }

func writeCCSwitchBackup(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	return closed
}

func (b *desktopCCSwitch) providerCurrent(ctx context.Context, id string) bool {
	var rows []struct {
		ID string `json:"id"`
	}
	if b.r.query(ctx, filepath.Join(b.r.Home, ".cc-switch/cc-switch.db"), `SELECT id FROM providers WHERE app_type='codex' AND is_current=1`, &rows) != nil {
		return false
	}
	return len(rows) == 1 && rows[0].ID == id
}

func (b *desktopCCSwitch) switchAccount(ctx context.Context, s HandoffState, stopped func(context.Context) error) error {
	if stopped == nil || stopped(ctx) != nil || !s.Enabled || s.Scope != "desktop" || s.PendingAction != string(HandoffSwitch) || b.r.handoffCancelled(s) {
		return errors.New("原实例或交接授权不可确认")
	}
	if _, err := os.Lstat(b.r.ccSwitchTransactionPath()); !os.IsNotExist(err) {
		return errors.New("存在待处理的 CC Switch 交接记录")
	}
	row, err := b.r.ccSwitchBinding(ctx, s.TargetAccount)
	if err != nil {
		return err
	}
	auth, err := readProtectedHandoffFile(filepath.Join(b.r.Home, ".codex/auth.json"))
	if err != nil || !b.r.handoffCurrentMatchesAccount(auth, s.SourceAccount) {
		return errors.New("来源账号已变化")
	}
	config, err := readProtectedHandoffFile(filepath.Join(b.r.Home, ".codex/config.toml"))
	if err != nil {
		return errors.New("Desktop 配置不可备份")
	}
	backupRoot := filepath.Join(filepath.Dir(b.r.handoffPath()), "backups")
	if os.MkdirAll(backupRoot, 0700) != nil {
		return errors.New("无法准备恢复目录")
	}
	dir, err := os.MkdirTemp(backupRoot, "ccswitch-")
	if err != nil {
		return errors.New("无法准备恢复目录")
	}
	if writeCCSwitchBackup(filepath.Join(dir, "auth.json"), auth) != nil || writeCCSwitchBackup(filepath.Join(dir, "config.toml"), config) != nil {
		return errors.New("无法保存恢复副本")
	}
	txn := ccSwitchTransaction{Version: 1, SessionKey: handoffAuthSessionKey(s), Source: s.SourceAccount, Target: s.TargetAccount, Provider: row.ID, Phase: "prepared", BackupDir: filepath.Base(dir), AuthBefore: digestString(auth), ConfigBefore: digestString(config)}
	if err := writeState(b.r.ccSwitchTransactionPath(), txn); err != nil {
		return err
	}
	if stopped(ctx) != nil || b.r.handoffCancelled(s) {
		return errors.New("退出状态或交接授权已变化")
	}
	again, err := readProtectedHandoffFile(filepath.Join(b.r.Home, ".codex/auth.json"))
	if err != nil || digestString(again) != txn.AuthBefore {
		return errors.New("来源认证已变化")
	}
	if err := b.run(ctx, "switch", row.Name); err != nil {
		return err
	}
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		if stopped(ctx) != nil || b.r.handoffCancelled(s) {
			return errors.New("切换期间实例或授权已变化")
		}
		if raw, err := b.r.ccSwitchLiveMatches(s.TargetAccount); err == nil && b.providerCurrent(ctx, row.ID) {
			if _, ok := b.r.managedResumeProvider("desktop"); !ok {
				return errors.New("切换后配置不是官方账号")
			}
			txn.Phase = "installed"
			txn.AuthAfter = digestString(raw)
			return writeState(b.r.ccSwitchTransactionPath(), txn)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("CC Switch 切号结果未确认")
		case <-timer.C:
		}
	}
}

func (b *desktopCCSwitch) installed(s HandoffState) bool {
	var txn ccSwitchTransaction
	if readJSON(b.r.ccSwitchTransactionPath(), &txn) != nil || txn.Version != 1 || txn.SessionKey != handoffAuthSessionKey(s) || (txn.Phase != "installed" && txn.Phase != "verified") || txn.Target != s.TargetAccount {
		return false
	}
	raw, err := b.r.ccSwitchLiveMatches(s.TargetAccount)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return err == nil && digestString(raw) == txn.AuthAfter && b.providerCurrent(ctx, txn.Provider)
}

func (b *desktopCCSwitch) finish(ctx context.Context, s HandoffState, verify func(context.Context, string) error) error {
	var txn ccSwitchTransaction
	if verify == nil || !b.installed(s) || readJSON(b.r.ccSwitchTransactionPath(), &txn) != nil || verify(ctx, s.TargetAccount) != nil {
		return errors.New("CC Switch 交接结果尚未核验")
	}
	if filepath.Base(txn.BackupDir) != txn.BackupDir || !strings.HasPrefix(txn.BackupDir, "ccswitch-") {
		return errors.New("恢复目录不可确认")
	}
	dir := filepath.Join(filepath.Dir(b.r.handoffPath()), "backups", txn.BackupDir)
	if txn.Phase == "installed" {
		auth, err := readProtectedHandoffFile(filepath.Join(dir, "auth.json"))
		if err != nil || digestString(auth) != txn.AuthBefore {
			return errors.New("认证恢复副本不匹配")
		}
		config, err := readProtectedHandoffFile(filepath.Join(dir, "config.toml"))
		if err != nil || digestString(config) != txn.ConfigBefore {
			return errors.New("配置恢复副本不匹配")
		}
		txn.Phase = "verified"
		if err := writeState(b.r.ccSwitchTransactionPath(), txn); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, "auth.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Join(dir, "config.toml")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(b.r.ccSwitchTransactionPath())
}

// Explicit review keeps the verified live target and archives the failed
// transaction without deleting its credential/config recovery copies. It does
// not mark the interrupted continuation successful or repeat any switch.
// Called under handoff state lock by the review command only.
func (r Reader) reviewCCSwitchHandoff(ctx context.Context, s HandoffState, verify func(context.Context, quotaCredential) AccountQuota) (HandoffState, error) {
	if s.Enabled || s.Scope != "desktop" || (s.Stage != "uncertain" && s.Stage != "completed") || verify == nil {
		return s, errors.New("请先停止 Desktop 交接")
	}
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "desktop-runner.lock"))
	if err != nil {
		return s, errors.New("Desktop 交接执行器仍在运行")
	}
	defer unlock()
	b := &desktopCCSwitch{r: r}
	var txn ccSwitchTransaction
	if readJSON(r.ccSwitchTransactionPath(), &txn) != nil || !b.installed(s) || filepath.Base(txn.BackupDir) != txn.BackupDir || !strings.HasPrefix(txn.BackupDir, "ccswitch-") {
		return s, errors.New("当前账号或切换记录无法核验，请保留恢复副本")
	}
	dir := filepath.Join(filepath.Dir(r.handoffPath()), "backups", txn.BackupDir)
	auth, err := readProtectedHandoffFile(filepath.Join(dir, "auth.json"))
	if err != nil || digestString(auth) != txn.AuthBefore {
		return s, errors.New("认证恢复副本无法核验")
	}
	config, err := readProtectedHandoffFile(filepath.Join(dir, "config.toml"))
	if err != nil || digestString(config) != txn.ConfigBefore {
		return s, errors.New("配置恢复副本无法核验")
	}
	c, err := r.scopeCredential("desktop", time.Now())
	if err != nil || verify(ctx, c).Status != "ready" {
		return s, errors.New("当前账号未通过服务端检查")
	}
	if !b.installed(s) || r.HandoffStatus() != s {
		return s, errors.New("核查期间账号或状态已变化")
	}
	if err := writeState(filepath.Join(dir, "reviewed-state.json"), s); err != nil {
		return s, err
	}
	archive := filepath.Join(dir, "reviewed-transaction.json")
	if _, err := os.Lstat(archive); !os.IsNotExist(err) {
		return s, errors.New("已有核查存档，请检查恢复目录")
	}
	if err := writeState(archive, txn); err != nil {
		return s, err
	}
	fresh := s
	fresh.Enabled = false
	fresh.Stage = "off"
	fresh.PendingAction = ""
	fresh.Message = "当前账号已核验并保留；旧交接记录和恢复副本已归档，继续结果仍未验收"
	if err := writeState(r.handoffPath(), fresh); err != nil {
		return s, err
	}
	if err := os.Remove(r.ccSwitchTransactionPath()); err != nil {
		return fresh, err
	}
	return fresh, nil
}

package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Snapshot/history/quota readers never write their sources and open SQLite
// read-only. Native switching uses separate, journaled write operations.
type Reader struct {
	Home   string
	SQLite string
}

func NewReader() (Reader, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Reader{}, errors.New("无法定位用户目录")
	}
	if testHome := os.Getenv("CODEX_HELPER_TEST_HOME"); testHome != "" {
		info, e := os.Stat(filepath.Join(testHome, ".codex-helper-test-root"))
		if !filepath.IsAbs(testHome) || e != nil || !info.Mode().IsRegular() {
			return Reader{}, errors.New("隔离测试目录无效，已停止读取")
		}
		resolved, e := filepath.EvalSymlinks(testHome)
		realHome, _ := filepath.EvalSymlinks(home)
		if e != nil || resolved == realHome {
			return Reader{}, errors.New("隔离测试目录不能指向真实用户目录")
		}
		home = resolved
	}
	return Reader{Home: home, SQLite: "/usr/bin/sqlite3"}, nil
}

func readJSON[T any](path string, dst *T) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("文件不可读取")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > 8<<20 {
		return errors.New("文件超出读取上限或不可读取")
	}
	d := json.NewDecoder(io.LimitReader(f, 8<<20))
	var value T
	if d.Decode(&value) != nil {
		return errors.New("文件格式不受支持")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return errors.New("文件格式不受支持")
	}
	*dst = value
	return nil
}

func (r Reader) query(ctx context.Context, path, query string, dst any) error {
	if _, err := os.Stat(path); err != nil {
		return errors.New("数据库不存在或不可读取")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.SQLite, "-readonly", "-json", "-cmd", ".timeout 2000", path, query)
	var output limitedBuffer
	output.limit = 16 << 20
	cmd.Stdout = &output
	if cmd.Run() != nil || output.overflow {
		return errors.New("数据库查询失败或超出读取上限")
	}
	if len(output.data) == 0 {
		return nil
	}
	if json.Unmarshal(output.data, dst) != nil {
		return errors.New("数据库返回格式不受支持")
	}
	return nil
}

type limitedBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.limit {
		b.overflow = true
		return 0, errors.New("output limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (r Reader) Snapshot(ctx context.Context) Snapshot {
	s := Snapshot{Version: Version, CapturedAt: time.Now().UTC(), Accounts: []Account{}, Sessions: []Session{}, Warnings: []string{}, Capabilities: capabilities()}
	var settings struct {
		CodexConfigDir  string `json:"codexConfigDir"`
		CurrentProvider string `json:"currentProviderCodex"`
	}
	ccRoot := filepath.Join(r.Home, ".cc-switch")
	if err := readJSON(filepath.Join(ccRoot, "settings.json"), &settings); err != nil {
		s.Warnings = append(s.Warnings, "CC Switch 设置不可读取；账号选择未确认，会话仍按两个独立目录读取。")
	}
	s.CodexHome = settings.CodexConfigDir
	if s.CodexHome == "" {
		s.CodexHome = filepath.Join(r.Home, ".codex")
	}
	if s.CodexHome == "~" {
		s.CodexHome = r.Home
	} else if strings.HasPrefix(s.CodexHome, "~/") {
		s.CodexHome = filepath.Join(r.Home, strings.TrimPrefix(s.CodexHome, "~/"))
	}
	if !filepath.IsAbs(s.CodexHome) {
		s.Warnings = append(s.Warnings, "CC Switch 配置目录不是绝对路径；会话仍按两个独立目录读取。")
		s.CodexHome = ""
	}
	s.Accounts, s.Warnings = r.accounts(ctx, ccRoot, settings.CurrentProvider, s.Warnings)
	allocation := r.AccountAllocation()
	s.Allocation = &allocation
	// The user deliberately split Desktop and CLI. CC Switch's configured
	// directory remains metadata, and must not redirect both session readers.
	s.Sources = []SessionSource{
		{ID: "desktop", Label: "Desktop", Home: filepath.Join(r.Home, ".codex"), Warnings: []string{}},
		{ID: "cli", Label: "CLI", Home: filepath.Join(r.Home, ".codex-cli"), Warnings: []string{}},
	}
	s.SessionsComplete = true
	for i := range s.Sources {
		source := &s.Sources[i]
		if info, err := os.Stat(source.Home); err != nil || !info.IsDir() {
			source.Warnings = append(source.Warnings, "该端的数据目录不存在或不可读取。")
			s.SessionsComplete = false
			continue
		}
		var sessions []Session
		sessions, source.Warnings, source.Complete = r.sessions(ctx, source.Home, source.Warnings)
		for j := range sessions {
			sessions[j].Scope = source.ID
			// Preserve existing Desktop selections, while namespacing CLI even
			// if someone later points the two roots at the same physical storage.
			if source.ID == "cli" {
				sessions[j].Key = "cli:" + sessions[j].Key
			}
		}
		s.Sessions = append(s.Sessions, sessions...)
		s.SessionsComplete = s.SessionsComplete && source.Complete
	}
	if sameExistingPath(s.Sources[0].Home, s.Sources[1].Home) || sameExistingPath(filepath.Join(s.Sources[0].Home, "sessions"), filepath.Join(s.Sources[1].Home, "sessions")) || sameExistingPath(r.databaseRoot(s.Sources[0].Home), r.databaseRoot(s.Sources[1].Home)) {
		for i := range s.Sources {
			s.Sources[i].Warnings = append(s.Sources[i].Warnings, "两端存在共享的数据目录；当前按入口分开展示，不代表历史或认证已隔离。")
		}
	}
	status := r.WarmupStatus()
	s.Warmup = &status
	resume := r.ResumeStatus()
	s.Resume = &resume
	handoff := r.HandoffStatus()
	_, recoveryErr := os.Lstat(r.handoffAuthTransactionPath())
	_, ccRecoveryErr := os.Lstat(r.ccSwitchTransactionPath())
	_, nativeRecoveryErr := os.Lstat(r.nativeTransactionPath())
	var runner cliRunnerState
	runnerReady := readJSON(r.cliRunnerPath(), &runner) == nil && runner.Ready && runner.ClientPID > 0 && syscall.Kill(runner.ClientPID, 0) == nil && r.cliManaged(handoff.SessionID)
	ready := (handoff.Scope == "cli" && runnerReady) || (handoff.Scope == "desktop" && r.desktopRunnerReady(handoff))
	s.Handoff = &HandoffSummary{Enabled: handoff.Enabled, Scope: handoff.Scope, SessionID: handoff.SessionID, Threshold: handoff.Threshold, Stage: handoff.Stage, Message: handoff.Message, RecoveryPending: recoveryErr == nil || !os.IsNotExist(recoveryErr) || !os.IsNotExist(ccRecoveryErr), RunnerReady: handoff.Enabled && ready}
	s.Handoff.RecoveryPending = s.Handoff.RecoveryPending || !os.IsNotExist(nativeRecoveryErr)
	if handoff.Enabled && handoff.Scope == "cli" && !r.cliManaged(handoff.SessionID) {
		s.Handoff.Enabled = false
		s.Handoff.Message = "托管终端未运行，自动切号已暂停；请检查原会话状态。"
	}
	if handoff.Enabled && handoff.Scope == "desktop" && !ready {
		s.Handoff.Enabled = false
		s.Handoff.AuthorizationPending = true
		s.Handoff.Stage = "error"
		s.Handoff.Message = "Desktop 执行器未就绪，自动切号已暂停；请先关闭本次授权，再重新开启监控。"
		if handoff.PendingAction != "" || (handoff.Stage != "watching" && handoff.Stage != "ready_stop") {
			s.Handoff.Stage = "uncertain"
			s.Handoff.Message = "Desktop 执行器已停止，交接结果待核查；请检查会话和账号后解除暂停。"
		}
	}
	return s
}

func sameExistingPath(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}

var emailPattern = regexp.MustCompile(`[a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

func maskName(s string) string {
	return emailPattern.ReplaceAllStringFunc(s, func(v string) string {
		parts := strings.SplitN(v, "@", 2)
		return string([]rune(parts[0])[0]) + "***@" + parts[1]
	})
}

func (r Reader) accounts(ctx context.Context, root, current string, warnings []string) ([]Account, []string) {
	// Unknown fields (including every credential) are discarded. This struct
	// can never serialize tokens into our snapshot or state.
	var store struct {
		Version  int    `json:"version"`
		Default  string `json:"default_account_id"`
		Accounts map[string]struct {
			ID    string `json:"account_id"`
			Email string `json:"email"`
		} `json:"accounts"`
	}
	accounts := []Account{}
	if readJSON(filepath.Join(root, "codex_oauth_auth.json"), &store) != nil {
		return accounts, append(warnings, "CC Switch 账号元数据不可读取。")
	}
	if store.Version != 2 {
		return accounts, append(warnings, "CC Switch 账号存储版本未经支持，已停止账号读取。")
	}
	var providers []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Current   int    `json:"is_current"`
		AccountID string `json:"account_id"`
	}
	err := r.query(ctx, filepath.Join(root, "cc-switch.db"), `SELECT id, name, is_current,
 json_extract(meta, '$.authBinding.accountId') AS account_id
 FROM providers WHERE app_type='codex' AND json_extract(meta, '$.authBinding.authProvider')='codex_oauth'
 ORDER BY COALESCE(sort_index,2147483647), name, id`, &providers)
	if err != nil {
		warnings = append(warnings, "供应商绑定不可读取；当前选择未确认。")
	}
	resolved := current
	if resolved == "" && err == nil {
		var marked []string
		for _, p := range providers {
			if p.Current == 1 {
				marked = append(marked, p.ID)
			}
		}
		if len(marked) == 1 {
			resolved = marked[0]
			warnings = append(warnings, "设置中没有当前供应商；暂显示数据库的唯一选择标记。")
		}
		if len(marked) > 1 {
			warnings = append(warnings, "数据库存在多个当前供应商标记，当前账号未确认。")
		}
	}
	providerOrder := map[string]int{}
	for i, p := range providers {
		if _, exists := providerOrder[p.AccountID]; !exists {
			providerOrder[p.AccountID] = i
		}
	}
	ids := make([]string, 0, len(store.Accounts))
	for id := range store.Accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		meta := store.Accounts[id]
		if meta.ID != "" && meta.ID != id {
			warnings = append(warnings, "发现不一致的账号标识，已跳过该记录。")
			continue
		}
		a := Account{ID: id, Name: fmt.Sprintf("Codex 账号 %d", i+1), Default: id == store.Default, ProviderNames: []string{}, QuotaStatus: "unavailable"}
		if meta.Email != "" {
			a.Name = maskName(meta.Email)
		}
		for _, p := range providers {
			if p.AccountID != id {
				continue
			}
			a.ProviderNames = append(a.ProviderNames, maskName(p.Name))
			// A stale DB current flag must not override the settings selection.
			if resolved != "" && p.ID == resolved {
				a.Selected = true
			}
		}
		accounts = append(accounts, a)
	}
	if err == nil && resolved != "" {
		selected := false
		for _, a := range accounts {
			selected = selected || a.Selected
		}
		if !selected {
			warnings = append(warnings, "当前供应商未匹配到已保存的 Codex OAuth 账号，账号身份未确认。")
		}
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		a, okA := providerOrder[accounts[i].ID]
		b, okB := providerOrder[accounts[j].ID]
		if okA != okB {
			return okA
		}
		if okA && a != b {
			return a < b
		}
		return accounts[i].Name < accounts[j].Name
	})
	return accounts, warnings
}

func sessionKey(root, id string) string {
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	h := sha256.Sum256([]byte(filepath.Clean(root) + "\x00" + id))
	return hex.EncodeToString(h[:])
}

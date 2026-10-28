package assistant

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HandoffAuthCommit holds no authentication material. The backup is private
// and must be removed after the new instance is verified, or retained for an
// explicit, checked recovery if the handoff stops before verification.
type handoffAuthCommit struct {
	backupPath string
	written    [32]byte
}

// Only file digests and the private backup filename are persisted. The
// prepared record is written before replacing auth.json, so a crash can be
// distinguished from an operation which was never started.
type handoffAuthTransaction struct {
	Version    int    `json:"version"`
	Scope      string `json:"scope"`
	SessionKey string `json:"session_key"`
	BackupName string `json:"backup_name"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Phase      string `json:"phase"`
}

func (r Reader) handoffAuthTransactionPath() string {
	return filepath.Join(filepath.Dir(r.handoffPath()), "credential-transaction.json")
}

func handoffAuthSessionKey(s HandoffState) string {
	return privateKey(s.Scope + "\x00" + s.SessionID + "\x00" + s.TurnID + "\x00" + s.SourceAccount + "\x00" + s.TargetAccount)
}

// Reconstruct the non-secret recovery coordinates after a process restart.
// The caller still has to check the actual file and the stopped instance.
func (r Reader) handoffAuthRecovery() (handoffAuthPreparation, handoffAuthCommit, error) {
	var p handoffAuthPreparation
	var c handoffAuthCommit
	var txn handoffAuthTransaction
	if readJSON(r.handoffAuthTransactionPath(), &txn) != nil || txn.Version != 1 || txn.SessionKey != handoffAuthSessionKey(r.HandoffStatus()) || (txn.Phase != "prepared" && txn.Phase != "installed") || !strings.HasPrefix(txn.BackupName, "previous-auth-") || filepath.Base(txn.BackupName) != txn.BackupName {
		return p, c, errors.New("没有可核验的认证恢复记录")
	}
	before, e1 := hex.DecodeString(txn.Before)
	after, e2 := hex.DecodeString(txn.After)
	if e1 != nil || e2 != nil || len(before) != sha256.Size || len(after) != sha256.Size {
		return p, c, errors.New("认证恢复记录损坏")
	}
	p.scope = txn.Scope
	copy(p.currentHash[:], before)
	copy(c.written[:], after)
	c.backupPath = filepath.Join(filepath.Dir(r.handoffPath()), "backups", txn.BackupName)
	return p, c, nil
}

func handoffTargetQuotaOK(q AccountQuota, threshold float64) bool {
	five, fiveOK := handoffWindow(&q, 18000)
	week, weekOK := handoffWindow(&q, 604800)
	return validHandoffThreshold(threshold) && fiveOK && weekOK && five > threshold && week > 0 && (q.LimitReached == nil || !*q.LimitReached)
}

// commitHandoffAuth is deliberately unexported: only an actual handoff adapter
// may call it after proving its own exact client is stopped. It does not stop a
// process, modify CC Switch or decide whether a chat should be restarted.
// Separate processes that do not share our lock can still race the final check
// and rename; the caller must verify actual runtime identity after restart.
func (r Reader) commitHandoffAuth(ctx context.Context, p handoffAuthPreparation, threshold float64, stopped func(context.Context) error, verify func(context.Context, quotaCredential) AccountQuota) (handoffAuthCommit, error) {
	var result handoffAuthCommit
	if stopped == nil || verify == nil {
		return result, errors.New("缺少停稳或额度检查")
	}
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "credential.lock"))
	if err != nil {
		return result, errors.New("已有认证切换正在进行")
	}
	defer unlock()
	state := r.HandoffStatus()
	if !state.Enabled || state.Stage != "pending" || state.PendingAction != string(HandoffSwitch) || state.Scope != p.scope || state.TargetAccount != p.accountID || state.SessionID == "" || state.TurnID == "" || state.SourceAccount == "" || state.SourceAccount == state.TargetAccount || state.Threshold != threshold {
		return result, errors.New("没有匹配的切号交接授权")
	}
	if _, err := os.Lstat(r.handoffAuthTransactionPath()); err == nil || !os.IsNotExist(err) {
		return result, errors.New("存在未处理的认证切换记录，请先核查")
	}
	if err := stopped(ctx); err != nil {
		return result, errors.New("尚未确认原实例已退出")
	}
	var q AccountQuota
	candidate, current, err := r.checkPreparedHandoffAuth(ctx, p, time.Now(), func(ctx context.Context, c quotaCredential) AccountQuota { q = verify(ctx, c); return q })
	if err != nil {
		return result, err
	}
	if !r.handoffCurrentMatchesAccount(current, state.SourceAccount) {
		return result, errors.New("当前认证与交接来源账号不一致")
	}
	if !handoffTargetQuotaOK(q, threshold) {
		return result, errors.New("备用账号额度未达到交接条件")
	}
	path, err := r.handoffAuthPath(p.scope)
	if err != nil {
		return result, err
	}
	backupDir := filepath.Join(filepath.Dir(r.handoffPath()), "backups")
	if os.MkdirAll(backupDir, 0700) != nil || os.Chmod(backupDir, 0700) != nil {
		return result, errors.New("无法保护认证备份目录")
	}
	backup, err := os.CreateTemp(backupDir, "previous-auth-")
	if err != nil {
		return result, errors.New("无法创建认证恢复副本")
	}
	result.backupPath = backup.Name()
	if err = backup.Chmod(0600); err == nil {
		_, err = backup.Write(current)
	}
	if err == nil {
		err = backup.Sync()
	}
	closeErr := backup.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(result.backupPath)
		return handoffAuthCommit{}, errors.New("无法保存认证恢复副本")
	}
	if dir, e := os.Open(backupDir); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	txn := handoffAuthTransaction{Version: 1, Scope: p.scope, SessionKey: handoffAuthSessionKey(state), BackupName: filepath.Base(result.backupPath), Before: hex.EncodeToString(p.currentHash[:]), After: hex.EncodeToString(candidate.digest[:]), Phase: "prepared"}
	if err := writeState(r.handoffAuthTransactionPath(), txn); err != nil {
		_ = os.Remove(result.backupPath)
		return handoffAuthCommit{}, errors.New("无法记录认证切换")
	}
	cleanup := func() {
		_ = os.Remove(result.backupPath)
		_ = os.Remove(r.handoffAuthTransactionPath())
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-helper-auth-")
	if err != nil {
		cleanup()
		return handoffAuthCommit{}, errors.New("无法准备目标认证")
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(candidate.raw)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr = tmp.Close()
	if err != nil || closeErr != nil {
		cleanup()
		return handoffAuthCommit{}, errors.New("无法同步目标认证")
	}
	// Recheck both sides after the network call and after preparing the files.
	if ctx.Err() != nil || stopped(ctx) != nil {
		cleanup()
		return handoffAuthCommit{}, errors.New("原实例状态已变化，取消切号")
	}
	currentAgain, err := readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(currentAgain) != p.currentHash || !r.handoffCurrentMatchesAccount(currentAgain, state.SourceAccount) {
		cleanup()
		return handoffAuthCommit{}, errors.New("当前认证已变化，取消切号")
	}
	fresh, err := r.findHandoffAuthCandidate(p.accountID, time.Now())
	if err != nil || fresh.digest != p.candidateHash {
		cleanup()
		return handoffAuthCommit{}, errors.New("备用认证已变化，取消切号")
	}
	if r.HandoffStatus() != state || r.handoffCancelled(state) {
		cleanup()
		return handoffAuthCommit{}, errors.New("交接授权已变化，取消切号")
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return handoffAuthCommit{}, errors.New("无法替换目标认证")
	}
	result.written = candidate.digest
	if dir, e := os.Open(filepath.Dir(path)); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	written, err := readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(written) != result.written {
		return result, errors.New("切换后的认证状态不明确，请检查")
	}
	txn.Phase = "installed"
	if err := writeState(r.handoffAuthTransactionPath(), txn); err != nil {
		return result, errors.New("认证已写入但交接记录未确认，请检查")
	}
	return result, nil
}

// Restore only if the current file is still precisely the value we wrote. A
// successfully restored old snapshot is not proof its credential is still valid.
func (r Reader) restoreHandoffAuth(p handoffAuthPreparation, commit handoffAuthCommit, stopped func(context.Context) error, ctx context.Context) error {
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "credential.lock"))
	if err != nil {
		return errors.New("已有认证切换正在进行")
	}
	defer unlock()
	if stopped == nil || stopped(ctx) != nil {
		return errors.New("尚未确认实例退出")
	}
	if filepath.Dir(commit.backupPath) != filepath.Join(filepath.Dir(r.handoffPath()), "backups") || filepath.Base(commit.backupPath) == "." {
		return errors.New("恢复副本路径无效")
	}
	var txn handoffAuthTransaction
	if readJSON(r.handoffAuthTransactionPath(), &txn) != nil || txn.Version != 1 || txn.Scope != p.scope || txn.SessionKey != handoffAuthSessionKey(r.HandoffStatus()) || txn.BackupName != filepath.Base(commit.backupPath) || !strings.HasPrefix(txn.BackupName, "previous-auth-") || txn.Before != hex.EncodeToString(p.currentHash[:]) || txn.After != hex.EncodeToString(commit.written[:]) || (txn.Phase != "prepared" && txn.Phase != "installed") {
		return errors.New("认证切换记录不匹配，不能恢复")
	}
	path, err := r.handoffAuthPath(p.scope)
	if err != nil {
		return err
	}
	live, err := readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(live) != commit.written {
		return errors.New("当前认证已变化，不能覆盖")
	}
	backup, err := readProtectedHandoffFile(commit.backupPath)
	if err != nil || sha256.Sum256(backup) != p.currentHash {
		return errors.New("认证恢复副本不可用")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-helper-restore-")
	if err != nil {
		return errors.New("无法准备认证恢复")
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(backup)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		return errors.New("无法同步认证恢复")
	}
	live, err = readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(live) != commit.written || stopped(ctx) != nil {
		return errors.New("当前认证或实例状态已变化，不能覆盖")
	}
	if os.Rename(tmp.Name(), path) != nil {
		return errors.New("无法恢复认证")
	}
	if dir, e := os.Open(filepath.Dir(path)); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	txn.Phase = "restored"
	if writeState(r.handoffAuthTransactionPath(), txn) != nil {
		return errors.New("认证已恢复但记录未确认，请检查")
	}
	return nil
}

// Remove the short-lived recovery copy only after the caller has verified the
// running instance and account. A future production adapter supplies that
// check; the credential module must never infer it from a file alone.
func (r Reader) finishHandoffAuth(ctx context.Context, verifyInstance func(context.Context, string) error) error {
	if verifyInstance == nil {
		return errors.New("尚未核验运行实例")
	}
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "credential.lock"))
	if err != nil {
		return errors.New("已有认证切换正在进行")
	}
	defer unlock()
	state := r.HandoffStatus()
	var txn handoffAuthTransaction
	if readJSON(r.handoffAuthTransactionPath(), &txn) != nil || txn.Version != 1 || txn.Scope != state.Scope || txn.SessionKey != handoffAuthSessionKey(state) || filepath.Base(txn.BackupName) != txn.BackupName || !strings.HasPrefix(txn.BackupName, "previous-auth-") {
		return errors.New("认证切换记录不匹配")
	}
	account := state.TargetAccount
	if txn.Phase == "restored" {
		account = state.SourceAccount
	} else if txn.Phase != "installed" {
		return errors.New("认证切换状态尚未确认")
	}
	path, err := r.handoffAuthPath(state.Scope)
	if err != nil {
		return err
	}
	live, err := readProtectedHandoffFile(path)
	if err != nil || !r.handoffCurrentMatchesAccount(live, account) || verifyInstance(ctx, account) != nil {
		return errors.New("运行实例或账号尚未核验")
	}
	backup := filepath.Join(filepath.Dir(r.handoffPath()), "backups", txn.BackupName)
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return errors.New("认证恢复副本无法清理")
	}
	if err := os.Remove(r.handoffAuthTransactionPath()); err != nil && !os.IsNotExist(err) {
		return errors.New("认证切换记录无法清理")
	}
	return nil
}

// resolveHandoffAuthAfterReview keeps the current authentication unchanged.
// It is called only after a user explicitly acknowledges the uncertain state
// and the owned terminal is closed. It verifies the complete current record
// against CC Switch and the quota service before discarding the recovery copy.
func (r Reader) resolveHandoffAuthAfterReview(ctx context.Context, state HandoffState, verify func(context.Context, quotaCredential) AccountQuota) error {
	unlock, err := lockWarmup(filepath.Join(filepath.Dir(r.handoffPath()), "credential.lock"))
	if err != nil {
		return errors.New("已有认证切换正在进行")
	}
	defer unlock()
	var txn handoffAuthTransaction
	if readJSON(r.handoffAuthTransactionPath(), &txn) != nil || txn.Version != 1 || txn.Scope != state.Scope || txn.SessionKey != handoffAuthSessionKey(state) || filepath.Base(txn.BackupName) != txn.BackupName || !strings.HasPrefix(txn.BackupName, "previous-auth-") || (txn.Phase != "prepared" && txn.Phase != "installed" && txn.Phase != "restored") {
		return errors.New("认证恢复记录不匹配，请手动检查")
	}
	before, e1 := hex.DecodeString(txn.Before)
	after, e2 := hex.DecodeString(txn.After)
	if e1 != nil || e2 != nil || len(before) != sha256.Size || len(after) != sha256.Size {
		return errors.New("认证恢复记录损坏，请手动检查")
	}
	path, err := r.handoffAuthPath(state.Scope)
	if err != nil {
		return err
	}
	current, err := readProtectedHandoffFile(path)
	if err != nil {
		return errors.New("当前认证不可读取，请在 CC Switch 更新")
	}
	checkCurrent := func(raw []byte) (quotaCredential, error) {
		var native handoffNativeAuth
		var store handoffManagedStore
		if json.Unmarshal(raw, &native) != nil || readJSON(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"), &store) != nil || store.Version != 2 {
			return quotaCredential{}, errors.New("当前认证不可确认，请在 CC Switch 更新")
		}
		matches := 0
		for _, id := range []string{state.SourceAccount, state.TargetAccount} {
			account, ok := store.Accounts[id]
			if ok && account.Refresh != "" && native.Tokens.Refresh != "" && subtle.ConstantTimeCompare([]byte(account.Refresh), []byte(native.Tokens.Refresh)) == 1 && r.handoffCurrentMatchesAccount(raw, id) {
				matches++
			}
		}
		credential, e := r.scopeCredential(state.Scope, time.Now())
		if matches != 1 || e != nil || credential.access != native.Tokens.Access {
			return quotaCredential{}, errors.New("当前完整认证与 CC Switch 不一致，请先更新")
		}
		return credential, nil
	}
	credential, err := checkCurrent(current)
	if err != nil {
		return err
	}
	if verify == nil || verify(ctx, credential).Status != "ready" {
		return errors.New("当前账号未通过额度查询，请在 CC Switch 检查")
	}
	backup := filepath.Join(filepath.Dir(r.handoffPath()), "backups", txn.BackupName)
	if _, err := os.Lstat(backup); err == nil {
		copyData, e := readProtectedHandoffFile(backup)
		digest := sha256.Sum256(copyData)
		if e != nil || hex.EncodeToString(digest[:]) != txn.Before {
			return errors.New("认证恢复副本不匹配，请手动检查")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("认证恢复副本不可检查")
	}
	// CC Switch or another native client does not share our lock. Refuse a
	// detected change; no auth file is ever written by this acknowledgement.
	again, err := readProtectedHandoffFile(path)
	if err != nil || sha256.Sum256(again) != sha256.Sum256(current) {
		return errors.New("当前认证已变化，请重新检查")
	}
	if _, err := checkCurrent(again); err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return errors.New("认证恢复副本无法清理")
	}
	if err := os.Remove(r.handoffAuthTransactionPath()); err != nil && !os.IsNotExist(err) {
		return errors.New("认证恢复记录无法清理")
	}
	return nil
}

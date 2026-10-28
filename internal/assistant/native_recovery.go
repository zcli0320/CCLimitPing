package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// ReviewNativePreparation only releases a fully prepared, unpublished attempt.
// It never refreshes a token, restores an older token, or repeats a switch.
// Published/ambiguous refresh phases stay blocked with their recovery payloads.
func (r Reader) ReviewNativePreparation(ctx context.Context) (HandoffState, error) {
	return r.reviewNativePreparation(ctx, newNativeDualSwitcher(r))
}

// ReviewManualNativePreparation is an explicit operator acknowledgement that a
// manual switch superseded this attempt. It only archives an unpublished
// preparation, leaving current credentials and application processes untouched.
// Automatic recovery must continue to use ReviewNativePreparation.
func (r Reader) ReviewManualNativePreparation(ctx context.Context) (HandoffState, error) {
	return r.reviewNativePreparationMode(ctx, newNativeDualSwitcher(r), true)
}

func (r Reader) reviewNativePreparation(ctx context.Context, n *nativeDualSwitcher) (HandoffState, error) {
	return r.reviewNativePreparationMode(ctx, n, false)
}

func (r Reader) reviewNativePreparationMode(ctx context.Context, n *nativeDualSwitcher, manualSwitch bool) (HandoffState, error) {
	unlock, err := lockWarmup(r.nativeTransactionPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), errors.New("原生执行器尚未退出，请等待当前步骤结束")
	}
	defer unlock()
	guard, err := lockWarmup(r.handoffPath() + ".execution.lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer guard()
	stateGuard, err := lockWarmup(r.handoffPath() + ".lock")
	if err != nil {
		return r.HandoffStatus(), err
	}
	defer stateGuard()
	s := r.HandoffStatus()
	t, err := n.txn()
	if err != nil {
		return s, err
	}
	if s.Enabled || s.Backend != nativeBackend || s.PendingAction != "" || !validHandoffText(t.Authorization) || t.Authorization != s.AuthorizationID || t.Scope != s.Scope || t.Source != s.SourceAccount || t.Target != s.TargetAccount || !validHandoffText(t.Source) || !validHandoffText(t.Target) || t.Source == t.Target || t.Phase != "prepared" || len(t.Files) != 0 {
		return s, errors.New("仅可自动核查尚未发布的已准备事务；其余情况保留记录待处理")
	}
	source, err := r.clientAccountID(t.Scope)
	if err != nil || (!manualSwitch && source != t.Source) || (source != t.Source && source != t.Target) {
		return s, errors.New("工作端账号已变化，保留恢复记录")
	}
	passive, err := r.clientAccountID(nativeScopeOther(t.Scope))
	if err != nil || (!manualSwitch && (passive != t.Target || source == passive)) || (passive != t.Source && passive != t.Target) {
		return s, errors.New("待用端账号分配不匹配")
	}
	if !manualSwitch {
		raw, err := readProtectedHandoffFile(filepath.Join(n.dir(t), "target-auth.json"))
		if err != nil {
			return s, err
		}
		var prepared handoffNativeAuth
		if json.Unmarshal(raw, &prepared) != nil {
			return s, errors.New("准备记录不完整")
		}
		store, err := r.readNativeStore()
		if err != nil {
			return s, err
		}
		managed, _, err := store.account(t.Target)
		if err != nil || managed.Tokens.Refresh != prepared.Tokens.Refresh || managed.Tokens.Identity != prepared.Tokens.Identity {
			return s, errors.New("准备凭证已发生变化，保留恢复记录")
		}
	}
	archive := filepath.Join(n.dir(t), "reviewed-native-transaction.json")
	if _, err = os.Lstat(archive); !os.IsNotExist(err) {
		return s, errors.New("已有核查存档，请检查原生恢复目录")
	}
	if err = writeState(filepath.Join(n.dir(t), "reviewed-handoff.json"), s); err != nil {
		return s, err
	}
	if manualSwitch {
		// Record what was acknowledged, without reading or restoring the old
		// prepared token generation. Both current identities are known above.
		if err = writeState(filepath.Join(n.dir(t), "manual-switch-review.json"), map[string]string{"reason": "operator-confirmed-manual-switch", "scope": t.Scope, "current_working_account": source, "current_passive_account": passive}); err != nil {
			return s, err
		}
	} else if err = n.env.restore(ctx, t.CCWasRunning); err != nil {
		return s, err
	}
	s.Enabled = false
	s.Stage = "off"
	s.PendingAction = ""
	s.Message = "已核查：本次未发布账号交换，原分配和最新凭证已保留；CC Switch 已按原状态恢复"
	if manualSwitch {
		s.Message = "已归档被手动切号替代的未发布准备；当前账号和应用保持，自动切号关闭"
	}
	if err = writeState(r.handoffPath(), s); err != nil {
		return s, err
	}
	if err = os.Rename(r.nativeTransactionPath(), archive); err != nil {
		return s, errors.New("核查存档未完成，恢复副本保留")
	}
	n.ready = false
	n.prepared = handoffNativeAuth{}
	return s, nil
}

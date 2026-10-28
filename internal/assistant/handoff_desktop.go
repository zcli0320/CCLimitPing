package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

const desktopHandoffStopText = "这是已授权的自动切号交接。请在当前操作的安全边界收尾，不要开始新的长任务。简要记录已完成、待完成及仍在运行的命令，然后结束当前轮次，等待小助手自动继续。本请求只收尾当前轮次，不取消或暂停长期目标。"

// requestHandoffStop is an experimental protocol adapter, not an authorization
// or a production runner. The installed Desktop follower interface selects its
// own active turn and can retry on a different turn. A matching reply is useful
// evidence but cannot make that selection atomic; a mismatch is ambiguous and
// must never lead to an automatic exit or credential switch.
func (c *desktopIPC) requestHandoffStop(id, owner, expectedTurn string) error {
	if !validHandoffText(id) || !validHandoffText(owner) || !validHandoffText(expectedTurn) {
		return errors.New("缺少原会话或准确轮次")
	}
	v, actualOwner, err := c.snapshot(id)
	if err != nil || actualOwner != owner || v.ID != id || v.ResumeState != "resumed" || v.Runtime.Type != "active" || v.Mode == "durable" || len(v.Requests) != 0 {
		return errors.New("原会话状态已变化，未请求暂停")
	}
	turn, ok := v.latest()
	if !ok || turn.ID != expectedTurn || turn.Status != "inProgress" {
		return errors.New("原轮次已变化，未请求暂停")
	}
	for _, submission := range v.UnconfirmedSubmissions {
		if !submission.Terminal {
			return errors.New("存在未确认提交，未请求暂停")
		}
	}
	messageID := handoffSteerClientID(id, expectedTurn)
	input := []any{map[string]any{"type": "text", "text": desktopHandoffStopText, "text_elements": []any{}}}
	params := map[string]any{
		"conversationId":      id,
		"input":               input,
		"restoreMessage":      map[string]any{"id": messageID, "context": map[string]any{}},
		"attachments":         []any{},
		"clientUserMessageId": messageID,
	}
	raw, _, err := c.call("thread-follower-steer-turn", params, 1, owner)
	if err != nil {
		return errors.New("暂停请求结果未确认，请检查原会话")
	}
	var response struct {
		Method string `json:"method"`
		Result struct {
			TurnID string `json:"turnId"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || (response.Method != "" && response.Method != "thread-follower-steer-turn") || response.Result.TurnID != expectedTurn {
		return errors.New("暂停请求的轮次未确认，请检查原会话")
	}
	return nil
}

// desktopHandoffStopObserved is only one part of the exit preconditions. It
// does not establish that queued follow-ups, other sessions, or detached
// processes are idle, and must not be used as an exit authorization on its own.
func desktopHandoffStopObserved(home, id, expectedTurn, expectedOwner, provider string, v desktopConversation, owner string) bool {
	if home == "" || !validHandoffText(id) || !validHandoffText(expectedTurn) || owner == "" || owner != expectedOwner || v.ID != id || v.ResumeState != "resumed" || v.ModelProvider != provider || provider == "" || v.Mode == "durable" || len(v.Requests) != 0 || (v.Goal != nil && v.Goal.Status == "active") {
		return false
	}
	for _, submission := range v.UnconfirmedSubmissions {
		if !submission.Terminal {
			return false
		}
	}
	turn, ok := v.latest()
	completed := v.Runtime.Type == "idle" && turn.Status == "completed"
	return ok && turn.ID == expectedTurn && (completed || desktopHandoffLimitedTurn(v)) && rolloutProvesCompleted(filepath.Join(home, ".codex"), v.RolloutPath, id, expectedTurn) == nil
}

// A structured quota error alone does not prove that outstanding tools stopped.
// Keep this narrower than the ordinary no-switch resume predicate, and combine
// it with the exact owner/turn and persisted completion fence above.
func desktopHandoffLimitedTurn(v desktopConversation) bool {
	turn, limited := v.limitedTurn()
	return limited && desktopTurnToolsStopped(turn)
}

func desktopTurnToolsStopped(turn resumeTurn) bool {
	if turn.Items == nil {
		return false
	}
	encoded, err := json.Marshal(turn.Items)
	tools := cliHandoffTurn{ItemsView: turn.ItemsView}
	if err != nil || json.Unmarshal(encoded, &tools.Items) != nil {
		return false
	}
	return tools.toolsStopped()
}

// The coordinator calls this only after verifying cold restart and config home.
func (c *desktopIPC) continueDesktopHandoff(home, id, expectedTurn, expectedOwner, provider string) (string, error) {
	v, owner, err := c.snapshot(id)
	if err != nil || !desktopHandoffStopObserved(home, id, expectedTurn, expectedOwner, provider, v, owner) {
		return "", errors.New("原会话或轮次已变化，未发送继续")
	}
	turn, _ := v.latest()
	turn, err = desktopPersistedTurn(home, v, turn)
	if err != nil {
		return "", err
	}
	idNew, err := c.startDesktopTurn(id, owner, turn, "继续之前为切换账号而暂停的任务。请先核对当前文件和运行状态，再接着工作。")
	if err != nil || idNew == expectedTurn {
		return "", errors.New("继续请求结果未确认，请检查原会话")
	}
	return idNew, nil
}

// interruptHandoffTurn addresses the exact owner and expected turn. The
// Desktop implementation may also interrupt descendants of that turn; the
// caller must subsequently verify task, tool and other-session state.
func (c *desktopIPC) interruptHandoffTurn(id, owner, expectedTurn string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(expectedTurn) == "" {
		return errors.New("缺少原会话或准确轮次")
	}
	raw, _, err := c.call("thread-follower-interrupt-turn", map[string]any{"conversationId": id, "mode": "system", "expectedTurnId": expectedTurn}, 4, owner)
	if err != nil {
		return err
	}
	var result struct {
		Method string `json:"method"`
		Result struct {
			OK                bool    `json:"ok"`
			InterruptedTurnID string  `json:"interruptedTurnId"`
			GoalPauseError    *string `json:"goalPauseError"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Method != "thread-follower-interrupt-turn" || !result.Result.OK || result.Result.InterruptedTurnID != expectedTurn || result.Result.GoalPauseError != nil {
		return errors.New("Desktop 中断结果未确认")
	}
	return nil
}

// DesktopHandoffPreflight reports only a read-only observation of the selected
// conversation. It must not be used as evidence of other conversations, the
// identity used by the running backend, or the state of background processes.
// A successful observation does not authorize an automatic handoff.
type DesktopHandoffPreflight struct {
	SessionID string   `json:"session_id"`
	TurnID    string   `json:"turn_id,omitempty"`
	TurnState string   `json:"turn_state,omitempty"`
	Runtime   string   `json:"runtime,omitempty"`
	Ready     bool     `json:"ready"`
	Blockers  []string `json:"blockers"`
}

type desktopHandoffSnapshot interface {
	snapshot(string) (desktopConversation, string, error)
}

// The installed Desktop implementation has a follower interrupt request with
// expectedTurnId, but successful interruption also interrupts descendant
// subagent threads. Its follower steer request has no expected-turn parameter.
// Neither request proves all tool/background work stopped. Until those effects,
// complete activity discovery, and the live backend identity are independently
// verified, fail closed rather than interrupt or restart the application.
func inspectDesktopHandoff(ipc desktopHandoffSnapshot, id, provider string) (DesktopHandoffPreflight, error) {
	result := DesktopHandoffPreflight{SessionID: id, Ready: false}
	if strings.TrimSpace(id) == "" {
		return result, errors.New("请先选择准确的 Desktop 会话")
	}
	v, owner, err := ipc.snapshot(id)
	if err != nil {
		return result, err
	}
	if owner == "" || v.ID != id {
		return result, errors.New("无法确认原会话及其当前控制者")
	}
	result.Runtime = v.Runtime.Type
	if turn, ok := v.latest(); ok {
		result.TurnID, result.TurnState = turn.ID, turn.Status
	}
	if v.ResumeState != "resumed" || v.Mode == "durable" || provider == "" || v.ModelProvider != provider {
		result.Blockers = append(result.Blockers, "无法确认该会话使用支持的本机 Codex 配置")
	}
	if result.TurnID == "" || result.TurnState == "" || result.Runtime == "" {
		result.Blockers = append(result.Blockers, "无法确认原会话的完整轮次状态")
	}
	result.Ready = len(result.Blockers) == 0 && (result.Runtime == "active" || result.Runtime == "idle" || result.Runtime == "systemError")
	result.Blockers = append(result.Blockers,
		"暂停指令不能原子绑定轮次；返回不匹配时停止交接，子代理可能受到影响",
		"触发时会退出整个 Desktop；其他会话及未提交输入可能受影响",
		"重启账号根据冷启动、配置目录及原生后端写出的完整凭证核验；待用端不自动继续",
	)
	return result, nil
}

// CheckDesktopHandoffPreflight does not send a steer, interruption, continuation,
// account switch, or application restart. The protocol subscription requests a
// snapshot from the already running owner, just like existing resume inspection.
func (r Reader) CheckDesktopHandoffPreflight(ctx context.Context, id string) (DesktopHandoffPreflight, error) {
	provider, ok := r.managedResumeProvider("desktop")
	if !ok {
		return DesktopHandoffPreflight{SessionID: id, Ready: false}, errors.New("Desktop 账号配置不可确认，自动切号已暂停")
	}
	ipc, err := connectDesktop(ctx, r.Home)
	if err != nil {
		return DesktopHandoffPreflight{SessionID: id, Ready: false}, err
	}
	defer ipc.conn.Close()
	out, err := inspectDesktopHandoff(ipc, id, provider)
	if err != nil {
		return out, err
	}
	if _, err := newDesktopHandoffLifecycle(ctx, r.Home); err != nil {
		out.Ready = false
		out.Blockers = append(out.Blockers, err.Error())
	}
	return out, nil
}

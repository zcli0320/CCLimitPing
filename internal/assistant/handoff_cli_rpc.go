package assistant

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

type cliHandoffTurn struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	ItemsView string          `json:"itemsView"`
	Error     json.RawMessage `json:"error"`
	StartedAt *int64          `json:"startedAt"`
	Items     []struct {
		Type      string  `json:"type"`
		Status    string  `json:"status"`
		ProcessID *string `json:"processId"`
		ExitCode  *int    `json:"exitCode"`
		ClientID  *string `json:"clientId"`
		Phase     *string `json:"phase"`
	} `json:"items"`
}
type cliHandoffThread struct {
	ID              string  `json:"id"`
	Path            *string `json:"path"`
	Cwd             string  `json:"cwd"`
	Model           *string `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	ModelProvider   string  `json:"modelProvider"`
	Status          struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	} `json:"status"`
	Turns []cliHandoffTurn `json:"turns"`
}

func (thread cliHandoffThread) latest() (cliHandoffTurn, bool) {
	if len(thread.Turns) == 0 {
		return cliHandoffTurn{}, false
	}
	turn := thread.Turns[len(thread.Turns)-1]
	return turn, turn.ID != ""
}
func (turn cliHandoffTurn) toolsStopped() bool {
	if turn.ItemsView != "" && turn.ItemsView != "full" {
		return false
	}
	for _, item := range turn.Items {
		switch item.Type {
		case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall":
			if item.Status == "" || item.Status == "inProgress" {
				return false
			}
			if item.Type == "commandExecution" && item.ProcessID != nil && *item.ProcessID != "" && item.ExitCode == nil && item.Status == "completed" {
				return false
			}
		}
		if item.Status == "inProgress" {
			return false
		}
	}
	return true
}

// A completed turn may have ended normally before processing the requested
// handoff. Require the exact accepted steer message and a subsequent assistant
// message; an explicit interruption is handled separately.
func (turn cliHandoffTurn) confirmsHandoffResponse(clientID string) bool {
	if clientID == "" || (turn.ItemsView != "" && turn.ItemsView != "full") {
		return false
	}
	seen := false
	for _, item := range turn.Items {
		if item.Type == "userMessage" && item.ClientID != nil && *item.ClientID == clientID {
			seen = true
			continue
		}
		if seen && item.Type == "agentMessage" {
			return true
		}
	}
	return false
}

func handoffSteerClientID(threadID, turnID string) string {
	return "helper-handoff-" + privateKey(threadID + "\x00" + turnID)[:32]
}

func withHandoffCLIRPC(ctx context.Context, socket string, fn func(*warmupRPC) error) error {
	c, err := connectUnixJSON(ctx, socket)
	if err != nil {
		return err
	}
	defer c.Close()
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), 32<<20)
	rpc := &warmupRPC{in: c, lines: scan, result: &WarmupExecution{}}
	if _, err = rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_helper_handoff", "version": Version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return err
	}
	if err = rpc.send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	return fn(rpc)
}
func readHandoffCLIThread(ctx context.Context, socket, id, provider string) (cliHandoffThread, error) {
	var thread cliHandoffThread
	err := withHandoffCLIRPC(ctx, socket, func(rpc *warmupRPC) error {
		raw, e := rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": true})
		if e != nil {
			return e
		}
		var response struct {
			Thread cliHandoffThread `json:"thread"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Thread.ID != id || response.Thread.ModelProvider != provider {
			return errors.New("托管会话或供应商不匹配")
		}
		thread = response.Thread
		return nil
	})
	return thread, err
}
func requestHandoffCLISteer(ctx context.Context, socket, id, turn string) error {
	params, err := cliHandoffSteerParams(id, turn, "请在当前操作的安全边界结束本轮，不要开始新的长任务。简要说明已完成、待完成和仍在运行的命令。")
	if err != nil {
		return err
	}
	params["clientUserMessageId"] = handoffSteerClientID(id, turn)
	return withHandoffCLIRPC(ctx, socket, func(rpc *warmupRPC) error {
		raw, e := rpc.call("turn/steer", params)
		if e != nil {
			return e
		}
		var response struct {
			TurnID string `json:"turnId"`
		}
		if json.Unmarshal(raw, &response) != nil || response.TurnID != turn {
			return errors.New("收尾指令的轮次未确认")
		}
		return nil
	})
}
func requestHandoffCLIInterrupt(ctx context.Context, socket, id, turn string) error {
	params, err := cliHandoffInterruptParams(id, turn)
	if err != nil {
		return err
	}
	return withHandoffCLIRPC(ctx, socket, func(rpc *warmupRPC) error { _, e := rpc.call("turn/interrupt", params); return e })
}

// A finished command item does not prove that a background terminal has
// exited. Never terminate these processes automatically; wait for the user or
// the task to finish them. An unsupported/incomplete response fails closed.
func handoffCLIBackgroundTerminalsIdle(rpc cliHandoffCaller, id string) (bool, error) {
	var cursor *string
	for page := 0; page < 100; page++ {
		raw, err := rpc.call("thread/backgroundTerminals/list", map[string]any{"threadId": id, "cursor": cursor, "limit": 100})
		if err != nil {
			return false, errors.New("无法确认后台终端状态")
		}
		var list struct {
			Data       json.RawMessage `json:"data"`
			NextCursor *string         `json:"nextCursor"`
		}
		var entries []json.RawMessage
		if json.Unmarshal(raw, &list) != nil || len(list.Data) == 0 || string(list.Data) == "null" || json.Unmarshal(list.Data, &entries) != nil {
			return false, errors.New("后台终端列表不完整")
		}
		if len(entries) != 0 {
			return false, nil
		}
		if list.NextCursor == nil || *list.NextCursor == "" {
			return true, nil
		}
		cursor = list.NextCursor
	}
	return false, errors.New("后台终端列表过长")
}

func otherHandoffCLIThreadsIdle(ctx context.Context, socket, id, provider string) (bool, error) {
	idle := true
	err := withHandoffCLIRPC(ctx, socket, func(rpc *warmupRPC) error {
		// Check the target even if a malformed loaded-thread list omits it.
		clear, e := handoffCLIBackgroundTerminalsIdle(rpc, id)
		if e != nil || !clear {
			idle = false
			return e
		}
		var cursor *string
		foundTarget := false
		for page := 0; page < 100; page++ {
			raw, e := rpc.call("thread/loaded/list", map[string]any{"cursor": cursor, "limit": 100})
			if e != nil {
				return e
			}
			var list struct {
				Data       json.RawMessage `json:"data"`
				NextCursor *string         `json:"nextCursor"`
			}
			var ids []string
			if json.Unmarshal(raw, &list) != nil || len(list.Data) == 0 || string(list.Data) == "null" || json.Unmarshal(list.Data, &ids) != nil {
				return errors.New("无法读取托管后端活动会话")
			}
			for _, other := range ids {
				if other == id {
					foundTarget = true
					continue
				}
				raw, e = rpc.call("thread/read", map[string]any{"threadId": other, "includeTurns": false})
				if e != nil {
					return e
				}
				var response struct {
					Thread cliHandoffThread `json:"thread"`
				}
				if json.Unmarshal(raw, &response) != nil || response.Thread.ID != other {
					return errors.New("活动会话状态不可读取")
				}
				if response.Thread.Status.Type != "idle" && response.Thread.Status.Type != "notLoaded" {
					idle = false
					return nil
				}
				clear, e := handoffCLIBackgroundTerminalsIdle(rpc, other)
				if e != nil || !clear {
					idle = false
					return e
				}
			}
			if list.NextCursor == nil || *list.NextCursor == "" {
				if !foundTarget {
					return errors.New("托管会话不在活动列表中")
				}
				return nil
			}
			cursor = list.NextCursor
		}
		return errors.New("托管后端会话过多")
	})
	return idle, err
}

type cliTurnSettings struct {
	TurnID                    string          `json:"turn_id"`
	Cwd                       string          `json:"cwd"`
	Model                     string          `json:"model"`
	Effort                    string          `json:"effort"`
	Approval                  json.RawMessage `json:"approval_policy"`
	Reviewer                  json.RawMessage `json:"approvals_reviewer"`
	Sandbox                   json.RawMessage `json:"sandbox_policy"`
	PermissionProfile         json.RawMessage `json:"permission_profile"`
	ActivePermissionProfile   json.RawMessage `json:"active_permission_profile"`
	Collaboration             json.RawMessage `json:"collaboration_mode"`
	CollaborationDigest       string          `json:"collaboration_digest,omitempty"`
	ServiceTier               json.RawMessage `json:"service_tier"`
	DisabledPlugins           json.RawMessage `json:"disabled_plugins"`
	collaborationInstructions *string
}

func canonicalSetting(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return canonical
}

// Rejoin the already loaded thread without overrides to obtain its live
// effective settings. This does not create a turn or change its configuration.
func readHandoffCLISettings(ctx context.Context, socket, threadID, turnID string, _ bool) (cliTurnSettings, error) {
	var selected cliTurnSettings
	if threadID == "" || turnID == "" {
		return selected, errors.New("原会话或轮次不可确认")
	}
	err := withHandoffCLIRPC(ctx, socket, func(rpc *warmupRPC) error {
		raw, e := rpc.call("thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
		if e != nil {
			return e
		}
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
			Cwd               string          `json:"cwd"`
			Model             string          `json:"model"`
			Effort            *string         `json:"reasoningEffort"`
			Approval          json.RawMessage `json:"approvalPolicy"`
			Reviewer          json.RawMessage `json:"approvalsReviewer"`
			Sandbox           json.RawMessage `json:"sandbox"`
			PermissionProfile json.RawMessage `json:"activePermissionProfile"`
			Collaboration     json.RawMessage `json:"collaborationMode"`
			ServiceTier       json.RawMessage `json:"serviceTier"`
			DisabledPlugins   json.RawMessage `json:"disabledPluginIds"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Thread.ID != threadID || result.Cwd == "" || result.Model == "" || len(result.Approval) == 0 || string(result.Approval) == "null" || len(result.Sandbox) == 0 || string(result.Sandbox) == "null" {
			return errors.New("无法确认该轮的目录、模型或权限设置")
		}
		effort := "default"
		if result.Effort != nil && *result.Effort != "" {
			effort = *result.Effort
		}
		var instructions *string
		digest := ""
		if len(result.Collaboration) > 0 && string(result.Collaboration) != "null" {
			var mode struct {
				Settings struct {
					Instructions *string `json:"developer_instructions"`
				} `json:"settings"`
			}
			if json.Unmarshal(result.Collaboration, &mode) != nil {
				return errors.New("自定义协作指令尚不能安全恢复")
			}
			if mode.Settings.Instructions != nil && *mode.Settings.Instructions != "" {
				instructions = mode.Settings.Instructions
				digest = fmt.Sprintf("%x", sha256.Sum256([]byte(*instructions)))
			}
			var value map[string]any
			if json.Unmarshal(result.Collaboration, &value) != nil {
				return errors.New("协作设置不可读取")
			}
			settings, ok := value["settings"].(map[string]any)
			if !ok {
				return errors.New("协作设置不可读取")
			}
			// Empty and absent bodies both mean there is no instruction
			// content. Persist only a digest for a nonempty body.
			settings["developer_instructions"] = nil
			result.Collaboration, _ = json.Marshal(value)
		}
		selected = cliTurnSettings{TurnID: turnID, Cwd: result.Cwd, Model: result.Model, Effort: effort, Approval: canonicalSetting(result.Approval), Reviewer: canonicalSetting(result.Reviewer), Sandbox: canonicalSetting(result.Sandbox), ActivePermissionProfile: canonicalSetting(result.PermissionProfile), Collaboration: canonicalSetting(result.Collaboration), CollaborationDigest: digest, collaborationInstructions: instructions, ServiceTier: canonicalSetting(result.ServiceTier), DisabledPlugins: canonicalSetting(result.DisabledPlugins)}
		return nil
	})
	return selected, err
}

// The instruction body lives only in the running supervisor, never in its
// persisted state. If the supervisor restarts, the pending operation stays
// uncertain and is not replayed with incomplete settings.
func collaborationForHandoff(settings cliTurnSettings) (json.RawMessage, error) {
	if settings.CollaborationDigest != "" && (settings.collaborationInstructions == nil || fmt.Sprintf("%x", sha256.Sum256([]byte(*settings.collaborationInstructions))) != settings.CollaborationDigest) {
		return nil, errors.New("原协作指令已不可用")
	}
	var value map[string]any
	if json.Unmarshal(settings.Collaboration, &value) != nil {
		return nil, errors.New("原协作设置不可读取")
	}
	inner, ok := value["settings"].(map[string]any)
	if !ok {
		return nil, errors.New("原协作设置不可读取")
	}
	if settings.CollaborationDigest == "" {
		// The server expands null into builtin mode instructions. An
		// explicit empty body preserves the original absence of content.
		inner["developer_instructions"] = ""
	} else {
		inner["developer_instructions"] = *settings.collaborationInstructions
	}
	raw, err := json.Marshal(value)
	return raw, err
}

func applyHandoffCLISettings(ctx context.Context, socket, threadID string, settings cliTurnSettings) error {
	if settings.TurnID == "" || settings.Cwd == "" || settings.Model == "" || len(settings.Approval) == 0 || len(settings.Sandbox) == 0 {
		return errors.New("原执行设置不完整")
	}
	params := map[string]any{"threadId": threadID, "cwd": settings.Cwd, "model": settings.Model, "approvalPolicy": settings.Approval}
	var profile struct {
		ID string `json:"id"`
	}
	if len(settings.ActivePermissionProfile) > 0 && json.Unmarshal(settings.ActivePermissionProfile, &profile) == nil && profile.ID != "" {
		params["permissions"] = profile.ID
	} else {
		params["sandboxPolicy"] = settings.Sandbox
	}
	if settings.Effort != "default" {
		params["effort"] = settings.Effort
	}
	if len(settings.Reviewer) > 0 && string(settings.Reviewer) != "null" {
		params["approvalsReviewer"] = settings.Reviewer
	}
	if len(settings.Collaboration) > 0 && string(settings.Collaboration) != "null" {
		mode, err := collaborationForHandoff(settings)
		if err != nil {
			return err
		}
		params["collaborationMode"] = mode
	}
	if len(settings.ServiceTier) > 0 && string(settings.ServiceTier) != "null" {
		params["serviceTier"] = settings.ServiceTier
	}
	if len(settings.DisabledPlugins) > 0 && string(settings.DisabledPlugins) != "null" {
		params["disabledPluginIds"] = settings.DisabledPlugins
	}
	return withHandoffCLIRPC(ctx, socket, func(rpc *warmupRPC) error { _, e := rpc.call("thread/settings/update", params); return e })
}

package assistant

// Desktop uses the already-running application's owner channel. We never
// start another writer for its history or override its permissions/auth.
import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type desktopIPC struct {
	conn   net.Conn
	client string
	serial int
}
type ipcMessage struct {
	Type       string          `json:"type"`
	RequestID  string          `json:"requestId"`
	Method     string          `json:"method"`
	Source     string          `json:"sourceClientId"`
	HandledBy  string          `json:"handledByClientId"`
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
	Params     json.RawMessage `json:"params"`
}

func connectDesktop(ctx context.Context, home string) (*desktopIPC, error) {
	path := filepath.Join(home, ".codex/ipc/ipc.sock")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("请先打开 Codex 桌面应用及所选会话")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	parent, parentErr := os.Stat(filepath.Dir(path))
	if !ok || stat.Uid != uint32(os.Getuid()) || parentErr != nil || parent.Mode().Perm()&0022 != 0 {
		return nil, errors.New("Codex 本机连接目录权限不正确")
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("Codex 桌面应用尚未连接")
	}
	deadline := time.Now().Add(8 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = c.SetDeadline(deadline)
	ipc := &desktopIPC{conn: c}
	r, _, err := ipc.call("initialize", map[string]any{"clientType": "codex-helper"}, 0, "")
	var result struct {
		Client string `json:"clientId"`
	}
	if err != nil || json.Unmarshal(r, &result) != nil || result.Client == "" {
		c.Close()
		return nil, errors.New("Codex 本机协议不兼容，续跑已暂停")
	}
	ipc.client = result.Client
	return ipc, nil
}
func (c *desktopIPC) send(value any) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > 32<<20 {
		return errors.New("续跑消息不可编码")
	}
	frame := make([]byte, len(b)+4)
	binary.LittleEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	for len(frame) > 0 {
		n, e := c.conn.Write(frame)
		if e != nil || n == 0 {
			return errors.New("Codex 本机连接中断")
		}
		frame = frame[n:]
	}
	return nil
}
func (c *desktopIPC) receive() (ipcMessage, error) {
	var m ipcMessage
	var head [4]byte
	if _, err := io.ReadFull(c.conn, head[:]); err != nil {
		return m, errors.New("Codex 本机连接超时或已关闭")
	}
	n := binary.LittleEndian.Uint32(head[:])
	if n == 0 || n > 32<<20 {
		return m, errors.New("Codex 本机消息超出上限")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return m, errors.New("Codex 本机消息不完整")
	}
	if json.Unmarshal(b, &m) != nil {
		return m, errors.New("Codex 本机消息格式不兼容")
	}
	return m, nil
}
func (c *desktopIPC) call(method string, params any, version int, target string) (json.RawMessage, string, error) {
	c.serial++
	id := fmt.Sprintf("helper-%d", c.serial)
	msg := map[string]any{"type": "request", "requestId": id, "method": method, "params": params, "version": version, "sourceClientId": c.client, "timeoutMs": 6000}
	if target != "" {
		msg["targetClientId"] = target
	}
	if err := c.send(msg); err != nil {
		return nil, "", err
	}
	for n := 0; n < 2000; n++ {
		m, err := c.receive()
		if err != nil {
			return nil, "", err
		}
		if m.Type == "response" && m.RequestID == id {
			if m.ResultType != "success" {
				return nil, "", errors.New("Codex 未接受操作，请打开原会话后重试；不会自动重复发送")
			}
			return m.Result, m.HandledBy, nil
		}
	}
	return nil, "", errors.New("Codex 本机消息过多")
}

type resumeTurn struct {
	ID        string                     `json:"turnId"`
	ItemsView string                     `json:"itemsView"`
	Status    string                     `json:"status"`
	Error     json.RawMessage            `json:"error"`
	Params    map[string]json.RawMessage `json:"params"`
	Items     []json.RawMessage          `json:"items"`
}
type desktopConversation struct {
	ID            string `json:"id"`
	Cwd           string `json:"cwd"`
	LatestModel   string `json:"latestModel"`
	LatestEffort  string `json:"latestReasoningEffort"`
	ResumeState   string `json:"resumeState"`
	Mode          string `json:"mode"`
	ModelProvider string `json:"modelProvider"`
	RolloutPath   string `json:"rolloutPath"`
	Runtime       struct {
		Type string `json:"type"`
	} `json:"threadRuntimeStatus"`
	Requests               []json.RawMessage `json:"requests"`
	Turns                  []resumeTurn      `json:"turns"`
	UnconfirmedSubmissions []struct {
		Terminal bool `json:"terminal"`
	} `json:"unconfirmedTurnSubmissions"`
	Goal *struct {
		Status string `json:"status"`
	} `json:"threadGoal"`
	History *struct {
		Kind    string `json:"kind"`
		History struct {
			Entities map[string]resumeTurn `json:"entitiesByKey"`
			Islands  []struct {
				Entries []struct {
					Value string `json:"value"`
				} `json:"entries"`
				Newer struct {
					Status string `json:"status"`
				} `json:"newerBoundary"`
			} `json:"islands"`
		} `json:"history"`
	} `json:"turnHistory"`
}

func (v desktopConversation) latest() (resumeTurn, bool) {
	if v.History != nil && v.History.Kind == "canonical" {
		h := v.History.History
		if len(h.Islands) == 0 {
			return resumeTurn{}, false
		}
		island := h.Islands[len(h.Islands)-1]
		if island.Newer.Status != "exhausted" || len(island.Entries) == 0 {
			return resumeTurn{}, false
		}
		t, ok := h.Entities[island.Entries[len(island.Entries)-1].Value]
		return t, ok
	}
	if len(v.Turns) == 0 {
		return resumeTurn{}, false
	}
	return v.Turns[len(v.Turns)-1], true
}
func (c *desktopIPC) snapshot(id string) (desktopConversation, string, error) {
	var v desktopConversation
	if err := c.send(map[string]any{"type": "broadcast", "method": "thread-stream-following-changed", "version": 1, "sourceClientId": c.client, "params": map[string]any{"hostId": "local", "conversationId": id, "following": true}}); err != nil {
		return v, "", err
	}
	for n := 0; n < 2000; n++ {
		m, err := c.receive()
		if err != nil {
			return v, "", err
		}
		if m.Type != "broadcast" || m.Method != "thread-stream-state-changed" {
			continue
		}
		var p struct {
			ID     string `json:"conversationId"`
			Host   string `json:"hostId"`
			Change struct {
				Type  string              `json:"type"`
				State desktopConversation `json:"conversationState"`
			} `json:"change"`
		}
		if json.Unmarshal(m.Params, &p) == nil && p.ID == id && p.Host == "local" && p.Change.Type == "snapshot" && p.Change.State.ID == id && m.Source != "" {
			return p.Change.State, m.Source, nil
		}
	}
	return v, "", errors.New("无法取得原会话的当前状态")
}
func usageLimitError(data json.RawMessage) bool {
	var e struct {
		Info json.RawMessage `json:"codexErrorInfo"`
	}
	if json.Unmarshal(data, &e) != nil {
		return false
	}
	var name string
	if json.Unmarshal(e.Info, &name) == nil {
		return name == "usageLimitExceeded" || name == "UsageLimitExceeded"
	}
	var tagged struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(e.Info, &tagged) == nil && (tagged.Type == "usageLimitExceeded" || tagged.Type == "UsageLimitExceeded")
}
func (v desktopConversation) limitedTurn() (resumeTurn, bool) {
	t, ok := v.latest()
	for _, submission := range v.UnconfirmedSubmissions {
		if !submission.Terminal {
			return t, false
		}
	}
	return t, ok && t.ID != "" && t.Status == "failed" && usageLimitError(t.Error) && v.ResumeState == "resumed" && (v.Runtime.Type == "idle" || v.Runtime.Type == "systemError") && len(v.Requests) == 0 && v.Mode != "durable" && (v.Goal == nil || v.Goal.Status != "active")
}
func (c *desktopIPC) continueTurn(id, owner string, turn resumeTurn) error {
	_, err := c.startDesktopTurn(id, owner, turn, "继续之前因额度限制中断的任务。")
	return err
}

func (c *desktopIPC) startDesktopTurn(id, owner string, turn resumeTurn, message string) (string, error) {
	if !validHandoffText(id) || !validHandoffText(owner) || !validHandoffText(message) {
		return "", errors.New("继续请求缺少准确会话")
	}
	request := map[string]any{"threadId": id, "input": []any{map[string]any{"type": "text", "text": message, "text_elements": []any{}}}}
	// Preserve execution settings only. Old messages, additional context and
	// attachments must not be resubmitted. The owner inherits current settings.
	for _, key := range []string{"cwd", "model", "effort", "serviceTier", "collaborationMode", "multiAgentMode", "summary", "personality", "outputSchema", "approvalPolicy", "approvalsReviewer", "sandboxPolicy", "permissions", "disabledPluginIds"} {
		if value, ok := turn.Params[key]; ok && string(value) != "null" {
			if key == "effort" && string(value) == `"default"` {
				continue
			}
			request[key] = value
		}
	}
	r, _, err := c.call("thread-follower-start-turn", map[string]any{"conversationId": id, "turnStart": map[string]any{"request": request, "context": map[string]any{"inheritThreadSettings": true}}}, 2, owner)
	if err != nil {
		return "", err
	}
	var result struct {
		Result struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		} `json:"result"`
	}
	if json.Unmarshal(r, &result) != nil || result.Result.Turn.ID == "" || result.Result.Turn.Status != "inProgress" {
		return "", errors.New("继续请求已登记，但新一轮状态未确认；请查看原会话，不会自动重发")
	}
	return result.Result.Turn.ID, nil
}

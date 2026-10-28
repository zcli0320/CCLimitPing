package assistant

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Read the exact turn's persisted execution context. Desktop's in-memory
// request may omit effective settings which become explicit after a reload.
// We compare the same saved context before and after restart, never downgrade
// high effort to "default" or equate unknown permissions with an empty object.
func desktopPersistedTurn(home string, v desktopConversation, turn resumeTurn) (resumeTurn, error) {
	invalid := errors.New("无法确认该轮已保存的执行设置")
	root := filepath.Join(home, ".codex")
	if v.ID == "" || turn.ID == "" || !trustedRollout(root, v.RolloutPath) {
		return turn, invalid
	}
	fd, err := syscall.Open(v.RolloutPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return turn, invalid
	}
	f := os.NewFile(uintptr(fd), "rollout")
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() == 0 {
		return turn, invalid
	}
	var final [1]byte
	if _, err = f.ReadAt(final[:], before.Size()-1); err != nil || final[0] != '\n' {
		return turn, invalid
	}
	head := bufio.NewScanner(io.NewSectionReader(f, 0, min(before.Size(), resumeRolloutTail)))
	head.Buffer(make([]byte, 4096), resumeRolloutTail)
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if !head.Scan() || json.Unmarshal(head.Bytes(), &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != v.ID {
		return turn, invalid
	}
	start := max(int64(0), before.Size()-resumeRolloutTail)
	r := bufio.NewReader(io.NewSectionReader(f, start, before.Size()-start))
	if start > 0 {
		if _, err = r.ReadBytes('\n'); err != nil {
			return turn, invalid
		}
	}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), resumeRolloutTail)
	var context map[string]json.RawMessage
	for scan.Scan() {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &record) != nil {
			return turn, invalid
		}
		if record.Type != "turn_context" {
			continue
		}
		var candidate map[string]json.RawMessage
		if json.Unmarshal(record.Payload, &candidate) != nil {
			return turn, invalid
		}
		var id string
		_ = json.Unmarshal(candidate["turn_id"], &id)
		if id == turn.ID {
			if context != nil {
				return turn, invalid
			}
			context = candidate
		}
	}
	after, err := f.Stat()
	pathInfo, pathErr := os.Lstat(v.RolloutPath)
	if scan.Err() != nil || context == nil || err != nil || pathErr != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(after, pathInfo) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return turn, invalid
	}
	params := map[string]json.RawMessage{}
	for from, to := range map[string]string{"cwd": "cwd", "model": "model", "effort": "effort", "approval_policy": "approvalPolicy", "approvals_reviewer": "approvalsReviewer", "sandbox_policy": "sandboxPolicy", "collaboration_mode": "collaborationMode", "service_tier": "serviceTier", "summary": "summary", "disabled_plugin_ids": "disabledPluginIds"} {
		if raw := context[from]; len(raw) > 0 && string(raw) != "null" {
			params[to] = raw
		}
	}
	// New Desktop turns can explicitly request the model's default effort in
	// collaboration settings while omitting the top-level persisted effort. This
	// is affirmative default intent, not permission to discard a known effort.
	if len(params["effort"]) == 0 {
		var mode struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		var snapshotEffort string
		_ = json.Unmarshal(turn.Params["effort"], &snapshotEffort)
		if json.Unmarshal(context["collaboration_mode"], &mode) != nil || mode.Settings == nil || string(mode.Settings["reasoning_effort"]) != "null" || (snapshotEffort != "" && snapshotEffort != "default") {
			return turn, invalid
		}
		params["effort"] = json.RawMessage(`"default"`)
		params["saved_default_effort"] = json.RawMessage(`true`)
	}
	// Persisted sandbox enums use snake/kebab case; turn/start uses camelCase.
	var sandbox map[string]json.RawMessage
	if json.Unmarshal(params["sandboxPolicy"], &sandbox) != nil {
		return turn, invalid
	}
	var typ string
	_ = json.Unmarshal(sandbox["type"], &typ)
	switch typ {
	case "danger-full-access":
		typ = "dangerFullAccess"
	case "read-only":
		typ = "readOnly"
	case "workspace-write":
		typ = "workspaceWrite"
	case "external-sandbox":
		typ = "externalSandbox"
	}
	if typ != "dangerFullAccess" && typ != "readOnly" && typ != "workspaceWrite" && typ != "externalSandbox" {
		return turn, invalid
	}
	sandbox["type"], _ = json.Marshal(typ)
	if raw, ok := sandbox["network_access"]; ok {
		sandbox["networkAccess"] = raw
		delete(sandbox, "network_access")
	}
	if raw, ok := sandbox["writable_roots"]; ok {
		sandbox["writableRoots"] = raw
		delete(sandbox, "writable_roots")
	}
	if raw, ok := sandbox["exclude_tmpdir_env_var"]; ok {
		sandbox["excludeTmpdirEnvVar"] = raw
		delete(sandbox, "exclude_tmpdir_env_var")
	}
	if raw, ok := sandbox["exclude_slash_tmp"]; ok {
		sandbox["excludeSlashTmp"] = raw
		delete(sandbox, "exclude_slash_tmp")
	}
	params["sandboxPolicy"], _ = json.Marshal(sandbox)
	var profile struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(context["active_permission_profile"], &profile) == nil && profile.ID != "" {
		params["permissions"], _ = json.Marshal(profile.ID)
	}
	for _, key := range []string{"cwd", "model", "effort", "approvalPolicy"} {
		var val string
		if json.Unmarshal(params[key], &val) != nil || val == "" {
			return turn, invalid
		}
	}
	// Include all remaining permission/plugin context in the comparison digest,
	// while keeping it out of the outbound request unless explicitly supported.
	for _, key := range []string{"permission_profile", "active_permission_profile", "disabled_plugin_ids", "multi_agent_version"} {
		if raw := context[key]; len(raw) > 0 {
			params["saved_"+key] = raw
		}
	}
	turn.Params = params
	return turn, nil
}

package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// desktopSingleSessionRuntime is only used for a user's explicitly selected
// Desktop session. Enabling it accepts that quitting the app affects all its
// windows; it does not claim to detect an unsent draft in a text field.
type desktopSingleSessionRuntime struct {
	r                                                                     Reader
	id, provider                                                          string
	lifecycle                                                             *desktopHandoffLifecycle
	wholeAppExitAccepted                                                  bool
	stopTurn, oldOwner                                                    string
	baselineUsers                                                         map[string]bool
	baselineDirectory, baselineModel, baselineEffort, baselinePermissions string
	launchRequested                                                       bool
	exitRequested                                                         bool
	continuedTurn                                                         string
}

func desktopSettings(v desktopConversation, turn resumeTurn) (string, string, string, string, bool) {
	value := func(key string) string {
		var s string
		_ = json.Unmarshal(turn.Params[key], &s)
		return strings.TrimSpace(s)
	}
	dir, model, effort := value("cwd"), value("model"), value("effort")
	if dir == "" {
		dir = v.Cwd
	}
	if model == "" {
		model = v.LatestModel
	}
	if effort == "" {
		effort = v.LatestEffort
	}
	if effort == "" {
		effort = "default"
	}
	if dir == "" || model == "" || turn.Params == nil {
		return "", "", "", "", false
	}
	settings := map[string]json.RawMessage{}
	for _, key := range []string{"approvalPolicy", "approvalsReviewer", "sandboxPolicy", "permissions", "collaborationMode", "serviceTier", "multiAgentMode"} {
		if raw, ok := turn.Params[key]; ok && len(raw) != 0 {
			settings[key] = raw
		}
	}
	for key, raw := range turn.Params {
		if strings.HasPrefix(key, "saved_") {
			settings[key] = raw
		}
	}
	encoded, err := json.Marshal(settings)
	if err != nil || len(settings) == 0 {
		return "", "", "", "", false
	}
	digest := sha256.Sum256(encoded)
	return dir, model, effort, hex.EncodeToString(digest[:]), true
}

func desktopUserItems(threadID string, turn resumeTurn) map[string]bool {
	result := map[string]bool{}
	for _, raw := range turn.Items {
		var item struct{ Type, ID, ClientID, ClientUserMessageID string }
		if json.Unmarshal(raw, &item) != nil || (item.Type != "userMessage" && item.Type != "steeringUserMessage") {
			continue
		}
		// The known assistant-generated prompt is not a manual intervention.
		ourID := handoffSteerClientID(threadID, turn.ID)
		if item.ClientID == ourID || item.ClientUserMessageID == ourID {
			continue
		}
		id := item.ID
		if id == "" {
			digest := sha256.Sum256(raw)
			id = hex.EncodeToString(digest[:])
		}
		result[id] = true
	}
	return result
}

func (d *desktopSingleSessionRuntime) observe(ctx context.Context, id string) (desktopRuntimeEvidence, error) {
	var e desktopRuntimeEvidence
	if id != d.id || d.lifecycle == nil {
		return e, errors.New("Desktop 目标不可确认")
	}
	e.WholeAppExitAccepted = d.wholeAppExitAccepted
	if d.lifecycle.oldExitConfirmed {
		e.OldExited = true
	} else {
		e.OldExited = d.lifecycle.observeExit(ctx)
	}
	if e.OldExited && !d.launchRequested {
		e.InterventionKnown = true
		return e, nil
	}
	// The IPC owner can disappear before the app and its process tree finish
	// exiting. Exit confirmation must depend on those captured processes, not
	// on a conversation snapshot which no longer exists during shutdown.
	if d.exitRequested && !d.launchRequested {
		e.InterventionKnown = true
		return e, nil
	}
	if d.launchRequested {
		e.ColdStartVerified = d.lifecycle.observeColdStart(ctx)
		e.ConfigHomeVerified = e.ColdStartVerified
		if !e.ColdStartVerified {
			e.InterventionKnown = true
			return e, nil
		}
	} else {
		procs, err := d.lifecycle.read(ctx)
		if err != nil {
			return e, err
		}
		pair, err := findDesktopProcessPair(procs, d.lifecycle.uid, d.lifecycle.appPath)
		if err != nil || pair != d.lifecycle.old || !d.lifecycle.usesHome(ctx, pair.Server, d.r.Home) {
			return e, errors.New("Desktop 进程或目录已变化")
		}
	}
	client, err := connectDesktop(ctx, d.r.Home)
	if err != nil {
		return e, err
	}
	defer client.conn.Close()
	e.View, e.Owner, err = client.snapshot(id)
	if err != nil {
		return e, err
	}
	turn, ok := e.View.latest()
	if !ok {
		return e, errors.New("会话轮次不可读取")
	}
	turn, err = desktopPersistedTurn(d.r.Home, e.View, turn)
	if err != nil {
		return e, err
	}
	for _, pending := range e.View.UnconfirmedSubmissions {
		if !pending.Terminal {
			return e, errors.New("存在未确认提交")
		}
	}
	if len(e.View.Requests) != 0 {
		return e, errors.New("会话等待审批或输入")
	}
	e.InterventionKnown = true // Only visible conversation state; unsent drafts are not observable.
	e.Directory, e.Model, e.Effort, e.Permissions, ok = desktopSettings(e.View, turn)
	if !ok {
		return e, errors.New("会话设置不可确认")
	}
	if d.baselineDirectory == "" && !d.launchRequested {
		d.baselineDirectory, d.baselineModel, d.baselineEffort, d.baselinePermissions = e.Directory, e.Model, e.Effort, e.Permissions
	}
	if d.launchRequested {
		e.SettingsVerified = e.Directory == d.baselineDirectory && e.Model == d.baselineModel && e.Effort == d.baselineEffort && e.Permissions == d.baselinePermissions
		e.ContinueTurnID = d.continuedTurn
	} else if d.stopTurn != "" {
		if e.Owner != d.oldOwner || turn.ID != d.stopTurn {
			e.UserIntervened = true
		}
		for item := range desktopUserItems(id, turn) {
			if !d.baselineUsers[item] {
				e.UserIntervened = true
			}
		}
	}
	return e, nil
}

func (d *desktopSingleSessionRuntime) requestStop(ctx context.Context, id, owner, turnID string) error {
	client, err := connectDesktop(ctx, d.r.Home)
	if err != nil {
		return err
	}
	defer client.conn.Close()
	v, actualOwner, err := client.snapshot(id)
	if err != nil || actualOwner != owner {
		return errors.New("原会话控制者已变化")
	}
	turn, ok := v.latest()
	if !ok || turn.ID != turnID {
		return errors.New("原轮次已变化")
	}
	resolved, err := desktopPersistedTurn(d.r.Home, v, turn)
	if err != nil {
		return err
	}
	d.baselineDirectory, d.baselineModel, d.baselineEffort, d.baselinePermissions, ok = desktopSettings(v, resolved)
	if !ok {
		return errors.New("执行设置不可确认")
	}
	d.baselineUsers = desktopUserItems(id, turn)
	if err := client.requestHandoffStop(id, owner, turnID); err != nil {
		return err
	}
	d.stopTurn, d.oldOwner = turnID, owner
	return nil
}

// Bind intervention/settings guards for an already stopped quota-limited turn.
// This reads current state and captures a baseline; it sends no pause message.
func (d *desktopSingleSessionRuntime) bindLimitedStop(ctx context.Context, id, owner, turnID string) error {
	e, err := d.observe(ctx, id)
	if err != nil || !e.InterventionKnown || e.UserIntervened || !desktopHandoffLimitedTurn(e.View) || !desktopHandoffStopObserved(d.r.Home, id, turnID, owner, d.provider, e.View, e.Owner) {
		return errors.New("额度停止的原轮次不可确认")
	}
	turn, _ := e.View.latest()
	d.baselineDirectory, d.baselineModel, d.baselineEffort, d.baselinePermissions = e.Directory, e.Model, e.Effort, e.Permissions
	d.baselineUsers = desktopUserItems(id, turn)
	d.stopTurn, d.oldOwner = turnID, owner
	return nil
}

func (d *desktopSingleSessionRuntime) requestExit(ctx context.Context) error {
	if err := d.lifecycle.requestExit(ctx); err != nil {
		return err
	}
	d.exitRequested = true
	return nil
}
func (d *desktopSingleSessionRuntime) requestRestart(ctx context.Context, id string) error {
	if err := d.lifecycle.requestRestart(ctx, id); err != nil {
		return err
	}
	d.launchRequested = true
	return nil
}
func (d *desktopSingleSessionRuntime) requestContinue(ctx context.Context, id, owner string, old resumeTurn) (string, error) {
	client, err := connectDesktop(ctx, d.r.Home)
	if err != nil {
		return "", err
	}
	defer client.conn.Close()
	idNew, err := client.continueDesktopHandoff(d.r.Home, id, old.ID, owner, d.provider)
	if err == nil {
		d.continuedTurn = idNew
	}
	return idNew, err
}

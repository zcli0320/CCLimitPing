package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIHandoffRejectsInputBeforeEveryDestructiveAction(t *testing.T) {
	input := &cliHandoffInput{epoch: 1}
	s := &cliHandoffSupervisor{input: input, armed: true, armedEpoch: 1}
	// A complete line entered after observation is still intervention.
	input.forward([]byte("user input\n"))
	for _, action := range []HandoffAction{HandoffInterrupt, HandoffExit, HandoffSwitch, HandoffRestart, HandoffContinue} {
		if err := s.Execute(context.Background(), action, HandoffState{}); err == nil {
			t.Fatalf("%s proceeded after manual input", action)
		}
	}
	if err := s.stoppedForHandoff(context.Background()); err == nil {
		t.Fatal("the final credential switch guard ignored manual input")
	}
}

type fakeCLIHandoffCaller struct {
	method string
	params map[string]any
	result json.RawMessage
	err    error
}

func TestOwnedCLISocketSupportsPrivateDaemonSymlink(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "helper-socket-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "codex-helper-cli-fixture")
	dir := filepath.Join(base, fmt.Sprintf("codex-daemon-%d", os.Getuid()))
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "control.sock")
	direct, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateCLISocketInTemp(path, base); err != nil {
		t.Fatalf("direct socket rejected: %v", err)
	}
	_ = direct.Close()
	target := filepath.Join(dir, "target")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err = validateCLISocketInTemp(path, base); err != nil {
		t.Fatalf("private daemon symlink rejected: %v", err)
	}
	if err = os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if validateCLISocketInTemp(path, base) == nil {
		t.Fatal("shared daemon directory accepted")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(base, "untrusted.sock"), path); err != nil {
		t.Fatal(err)
	}
	if validateCLISocketInTemp(path, base) == nil {
		t.Fatal("untrusted symlink target accepted")
	}
}

func TestHandoffRequiresEvidenceThatStopRequestWasAnswered(t *testing.T) {
	id := handoffSteerClientID("thread", "turn")
	var turn cliHandoffTurn
	if err := json.Unmarshal([]byte(`{"id":"turn","status":"completed","itemsView":"full","items":[{"type":"agentMessage"}]}`), &turn); err != nil {
		t.Fatal(err)
	}
	if turn.confirmsHandoffResponse(id) {
		t.Fatal("normal completion was mistaken for handoff")
	}
	payload, _ := json.Marshal(map[string]any{"id": "turn", "status": "completed", "itemsView": "full", "items": []any{map[string]any{"type": "userMessage", "clientId": id}, map[string]any{"type": "agentMessage", "phase": "final_answer"}}})
	if err := json.Unmarshal(payload, &turn); err != nil {
		t.Fatal(err)
	}
	if !turn.confirmsHandoffResponse(id) {
		t.Fatal("the exact steer and later response were not recognized")
	}
	turn.ItemsView = "summary"
	if turn.confirmsHandoffResponse(id) || turn.toolsStopped() {
		t.Fatal("partial turn items accepted as complete")
	}
}

func TestHandoffWaitsForTrackedCommandsAndFullItems(t *testing.T) {
	var turn cliHandoffTurn
	if json.Unmarshal([]byte(`{"itemsView":"full","items":[{"type":"commandExecution","status":"completed","processId":"fixture"}]}`), &turn) != nil {
		t.Fatal("fixture invalid")
	}
	if turn.toolsStopped() {
		t.Fatal("command with unknown exit state was treated as stopped")
	}
	if json.Unmarshal([]byte(`{"itemsView":"full","items":[{"type":"commandExecution","status":"completed","processId":"fixture","exitCode":0}]}`), &turn) != nil {
		t.Fatal("fixture invalid")
	}
	if !turn.toolsStopped() {
		t.Fatal("completed command with exit code was not recognized")
	}
}

func TestHandoffWaitsForBackgroundTerminals(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		idle bool
		err  bool
	}{
		{"empty", `{"data":[],"nextCursor":null}`, true, false},
		{"running", `{"data":[{"processId":"42"}],"nextCursor":null}`, false, false},
		{"missing list", `{}`, false, true},
		{"null list", `{"data":null}`, false, true},
		{"invalid list", `{"data":{}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &fakeCLIHandoffCaller{result: json.RawMessage(tc.body)}
			idle, err := handoffCLIBackgroundTerminalsIdle(rpc, "thread")
			if idle != tc.idle || (err != nil) != tc.err || rpc.method != "thread/backgroundTerminals/list" || rpc.params["threadId"] != "thread" {
				t.Fatalf("incorrect background check: idle=%t error=%v", idle, err)
			}
		})
	}
}

func TestHandoffCollaborationInstructionsStayInMemory(t *testing.T) {
	secret := "private fixture instruction"
	settings := cliTurnSettings{Collaboration: json.RawMessage(`{"mode":"default","settings":{"developer_instructions":null,"model":"fixture"}}`), CollaborationDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(secret))), collaborationInstructions: &secret}
	disk, err := json.Marshal(settings)
	if err != nil || strings.Contains(string(disk), secret) {
		t.Fatal("instruction body leaked to handoff state")
	}
	mode, err := collaborationForHandoff(settings)
	if err != nil || !strings.Contains(string(mode), secret) {
		t.Fatal("live supervisor could not restore its setting")
	}
	var afterRestart cliTurnSettings
	if json.Unmarshal(disk, &afterRestart) != nil {
		t.Fatal("state unreadable")
	}
	if _, err = collaborationForHandoff(afterRestart); err == nil {
		t.Fatal("missing in-memory instructions silently accepted")
	}
}

func TestHandoffPreservesAbsentCollaborationInstructions(t *testing.T) {
	settings := cliTurnSettings{Collaboration: json.RawMessage(`{"mode":"default","settings":{"developer_instructions":null,"model":"fixture"}}`)}
	raw, err := collaborationForHandoff(settings)
	if err != nil {
		t.Fatal(err)
	}
	var mode struct {
		Settings struct {
			Instructions *string `json:"developer_instructions"`
		} `json:"settings"`
	}
	if json.Unmarshal(raw, &mode) != nil || mode.Settings.Instructions == nil || *mode.Settings.Instructions != "" {
		t.Fatal("absent instruction body would expand into builtin mode instructions")
	}
}

func (f *fakeCLIHandoffCaller) call(method string, params any) (json.RawMessage, error) {
	f.method = method
	f.params, _ = params.(map[string]any)
	return f.result, f.err
}

func TestCLIHandoffInspectionOnlyReadsExactThread(t *testing.T) {
	rpc := &fakeCLIHandoffCaller{result: json.RawMessage(`{"thread":{"id":"thread-a","modelProvider":"openai","status":{"type":"active"},"turns":[{"id":"turn-a","status":"inProgress"}]}}`)}
	out, err := inspectCLIHandoff(rpc, "thread-a", "openai")
	if err != nil || out.SessionID != "thread-a" || out.TurnID != "turn-a" || out.TurnState != "inProgress" || out.Runtime != "active" || out.Ready {
		t.Fatal("read-only preflight did not preserve state or failed open")
	}
	if rpc.method != "thread/read" || rpc.params["threadId"] != "thread-a" || rpc.params["includeTurns"] != true {
		t.Fatal("preflight issued anything other than an exact read")
	}
	if len(out.Blockers) == 0 {
		t.Fatal("unsafe execution blockers omitted")
	}
}

func TestCLIHandoffInspectionRejectsWrongOwnerAndIncompleteEvidence(t *testing.T) {
	for _, fixture := range []string{
		`{"thread":{"id":"thread-other","modelProvider":"openai"}}`,
		`{"thread":{"id":"thread-a","modelProvider":"other"}}`,
		`not json`,
	} {
		if _, err := inspectCLIHandoff(&fakeCLIHandoffCaller{result: json.RawMessage(fixture)}, "thread-a", "openai"); err == nil {
			t.Fatal("invalid or mismatched thread accepted")
		}
	}
	out, err := inspectCLIHandoff(&fakeCLIHandoffCaller{result: json.RawMessage(`{"thread":{"id":"thread-a","modelProvider":"openai"}}`)}, "thread-a", "openai")
	if err != nil || out.Ready || len(out.Blockers) < 3 {
		t.Fatal("missing runtime/turn evidence did not block")
	}
	_, err = inspectCLIHandoff(&fakeCLIHandoffCaller{err: errors.New("SENSITIVE_BACKEND_RESPONSE")}, "thread-a", "openai")
	if err == nil || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatal("backend error leaked")
	}
}

func TestCLIHandoffActionParamsRequireExactThreadAndTurn(t *testing.T) {
	steer, err := cliHandoffSteerParams("thread-a", "turn-a", "在安全边界结束")
	if err != nil || steer["threadId"] != "thread-a" || steer["expectedTurnId"] != "turn-a" || steer["input"] == nil {
		t.Fatal("steer omitted target or expected active turn")
	}
	interrupt, err := cliHandoffInterruptParams("thread-a", "turn-a")
	if err != nil || interrupt["threadId"] != "thread-a" || interrupt["turnId"] != "turn-a" {
		t.Fatal("interrupt omitted exact target")
	}
	for _, tc := range [][2]string{{"", "turn-a"}, {"thread-a", ""}, {"thread-a", " turn-a"}, {" ", "turn-a"}} {
		if _, err := cliHandoffInterruptParams(tc[0], tc[1]); err == nil {
			t.Fatal("invalid target accepted")
		}
		if _, err := cliHandoffSteerParams(tc[0], tc[1], "stop"); err == nil {
			t.Fatal("invalid steer target accepted")
		}
	}
	if _, err := cliHandoffSteerParams("thread-a", "turn-a", "  "); err == nil {
		t.Fatal("blank steer message accepted")
	}
}

func TestCLIHandoffNeverConnectsWithoutManagedRunner(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	if _, err := ownedCLIHandoffSocket(r, "unmanaged-thread"); err == nil {
		t.Fatal("unmanaged CLI should never be probed")
	}
}

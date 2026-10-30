package assistant

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordingManagerInput struct{ bytes.Buffer }

func (*recordingManagerInput) Close() error { return nil }

func TestDesktopOwnerDefaultAndInvalidSettings(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r := Reader{Home: t.TempDir(), SQLite: "/usr/bin/sqlite3"}
	path := filepath.Join(r.Home, ".cc-switch", "settings.json")
	if err := os.MkdirAll(filepath.Join(r.Home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.Home, ".codex"), filepath.Join(r.Home, "desktop-alias")); err != nil {
		t.Fatal(err)
	}
	owned, err := r.desktopCCSwitchOwner()
	if owned || err != nil || r.requireNativeWriteOwner("desktop") != nil {
		t.Fatal("missing settings must permit the native route")
	}
	for _, tc := range []struct {
		name, content  string
		owned, invalid bool
	}{
		{"missing field", `{}`, true, false},
		{"paired unicode", `{"note":"\ud83d\ude80"}`, true, false},
		{"empty", `{"codexConfigDir":""}`, true, false},
		{"whitespace", `{"codexConfigDir":"  "}`, true, false},
		{"desktop", `{"codexConfigDir":"~/.codex"}`, true, false},
		{"clean path", `{"codexConfigDir":"~/.codex/../.codex"}`, true, false},
		{"desktop symlink", `{"codexConfigDir":"~/desktop-alias"}`, true, false},
		{"cli", `{"codexConfigDir":"~/.codex-cli"}`, false, false},
		{"invalid json", `{`, false, true},
		{"null root", `null`, false, true},
		{"array root", `[]`, false, true},
		{"null field", `{"codexConfigDir":null}`, false, true},
		{"numeric field", `{"codexConfigDir":42}`, false, true},
		{"unpaired high surrogate", `{"codexConfigDir":"\ud800"}`, false, true},
		{"unpaired low surrogate", `{"note":"\udc00"}`, false, true},
		{"invalid utf8", string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}), false, true},
		{"relative path", `{"codexConfigDir":"relative"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFixture(t, path, tc.content)
			owned, err := r.desktopCCSwitchOwner()
			if owned != tc.owned || (err != nil) != tc.invalid {
				t.Fatalf("owned=%t err=%v", owned, err)
			}
			if tc.invalid {
				if !errors.Is(r.requireNativeWriteOwner("desktop"), errDesktopOwnerUnknown) {
					t.Fatal("unknown owner permitted Desktop writes")
				}
				s := r.Snapshot(context.Background())
				if s.Sources[0].Label != "Desktop（管理方无法确认）" || s.Capabilities["desktop_handoff"].Available {
					t.Fatal("unknown owner was not surfaced")
				}
			} else if tc.owned && !errors.Is(r.requireNativeWriteOwner("desktop"), errDesktopCCSwitchOwner) {
				t.Fatal("CC Switch-owned Desktop permitted native writes")
			} else if !tc.owned && r.requireNativeWriteOwner("desktop") != nil {
				t.Fatal("non-owned Desktop was blocked")
			}
			if r.requireNativeWriteOwner("cli") != nil {
				t.Fatal("Desktop ownership changed CLI route")
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.desktopCCSwitchOwner(); !errors.Is(err, errDesktopOwnerUnknown) {
		t.Fatal("non-file settings must fail closed")
	}
}

func TestCCSwitchOwnerBlocksGoManagerDesktopApply(t *testing.T) {
	r := Reader{Home: t.TempDir()}
	settings := filepath.Join(r.Home, ".cc-switch", "settings.json")
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex"}`)
	input := &recordingManagerInput{}
	client := &nativeManagerClient{home: r.Home, in: input, lines: make(chan []byte, 2), done: make(chan struct{})}
	defer client.Close()
	ctx := context.Background()
	if err := client.apply(ctx, "fixture", "desktop"); !errors.Is(err, errDesktopCCSwitchOwner) {
		t.Fatalf("owned Desktop apply: %v", err)
	}
	if err := client.call(ctx, "apply", map[string]string{"scope": "desktop", "account_id": "fixture"}, nil); !errors.Is(err, errDesktopCCSwitchOwner) {
		t.Fatalf("direct apply: %v", err)
	}
	if input.Len() != 0 || client.next != 0 {
		t.Fatal("owned Desktop request reached the manager")
	}
	client.lines <- []byte(`{"id":1,"result":{"state":"installed"}}`)
	if err := client.apply(ctx, "fixture", "cli"); err != nil {
		t.Fatalf("CLI route changed: %v", err)
	}
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex-cli"}`)
	client.lines <- []byte(`{"id":2,"result":{"state":"installed"}}`)
	if err := client.apply(ctx, "fixture", "desktop"); err != nil {
		t.Fatalf("native Desktop route changed: %v", err)
	}
	if client.next != 2 || !strings.Contains(input.String(), `"scope":"desktop"`) {
		t.Fatal("allowed routes did not reach the manager")
	}
}

func TestCCSwitchOwnerChangeStopsPreparedNativeDesktopApply(t *testing.T) {
	r, coordinator, _, manager := nativeDesktopFixture(t)
	if _, ok := coordinator.targetEvidence(context.Background(), 10); !ok {
		t.Fatal("native fixture did not prepare")
	}
	writeFixture(t, filepath.Join(r.Home, ".cc-switch", "settings.json"), `{"codexConfigDir":"~/.codex"}`)
	state := r.HandoffStatus()
	state.SourceAccount, state.TargetAccount = coordinator.source, coordinator.target
	if err := coordinator.Execute(context.Background(), HandoffSwitch, state); !errors.Is(err, errDesktopCCSwitchOwner) {
		t.Fatalf("owner change did not stop apply: %v", err)
	}
	if manager.applies != 0 {
		t.Fatal("native manager was called after ownership changed")
	}
}

func TestCCSwitchOwnerStaleBindingIgnoredAndReported(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r := Reader{Home: t.TempDir(), SQLite: "/usr/bin/sqlite3"}
	quotaFixtures(t, r.Home)
	store, err := os.ReadFile(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	settings := filepath.Join(r.Home, ".cc-switch", "settings.json")
	bindingsPath := filepath.Join(r.helperStateRoot(), "accounts", "bindings.json")
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex"}`)
	writeFixture(t, bindingsPath, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":false},"cli":{"account_id":"managed-b","needs_reapply":false}}`)
	if r.nativeHandoffInstalled("desktop", "managed-a") {
		t.Fatal("CC Switch-owned Desktop accepted a stale native binding")
	}
	if !r.nativeHandoffInstalled("cli", "managed-b") {
		t.Fatal("CLI installed check changed")
	}
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex-cli"}`)
	if !r.nativeHandoffInstalled("desktop", "managed-a") {
		t.Fatal("native Desktop binding not recognized when CC Switch owns CLI")
	}
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex"}`)
	// A reapply marker on an ignored binding cannot authorize an older Desktop
	// token. It must not prevent another account's independently matched token.
	writeFixture(t, bindingsPath, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-b","needs_reapply":false}}`)
	before, err := os.ReadFile(bindingsPath)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := os.Stat(bindingsPath)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := r.readNativeBindings()
	if err != nil || bindings.Desktop != nil || bindings.staleDesktop == nil || !bindings.reapplyBlocked()["managed-a"] || bindings.CLI == nil {
		t.Fatalf("effective bindings: %+v, err=%v", bindings, err)
	}
	if bindings.allowsLiveCredential("managed-a", "Desktop") {
		t.Fatal("possibly superseded Desktop token accepted")
	}
	if _, err := r.scopeCredential("desktop", time.Now()); err == nil {
		t.Fatal("possibly superseded Desktop token reached scope checks")
	}
	if _, err := r.scopeCredential("cli", time.Now()); err != nil {
		t.Fatalf("CLI credential affected by Desktop binding: %v", err)
	}
	cliAuth, err := os.ReadFile(filepath.Join(r.Home, ".codex-cli", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.Home, ".codex", "auth.json"), string(cliAuth))
	if _, err := r.scopeCredential("desktop", time.Now()); err != nil {
		t.Fatalf("Desktop live identity matching another account was blocked: %v", err)
	}
	sources, err := r.warmupActivitySources("managed-b", []string{"CLI"})
	if err != nil || !strings.Contains(strings.Join(sources, ","), "Desktop") {
		t.Fatalf("Desktop activity not checked conservatively: %v, %v", sources, err)
	}
	snapshot := r.Snapshot(context.Background())
	if snapshot.Sources[0].Label != "Desktop（由 CC Switch 管理）" || !strings.Contains(snapshot.Capabilities["desktop_handoff"].Reason, "CC Switch") {
		t.Fatal("Desktop ownership missing from status")
	}
	if strings.Count(strings.Join(snapshot.Warnings, "\n"), staleDesktopBindingNotice) != 1 {
		t.Fatalf("missing or duplicate stale binding warning: %v", snapshot.Warnings)
	}
	report, err := r.CheckHandoffPreflight(context.Background(), "desktop", "missing-fixture-session", 10)
	if err == nil || strings.Count(strings.Join(report.Limitations, "\n"), staleDesktopBindingNotice) != 1 {
		t.Fatalf("early preflight did not report stale binding: %v, %v", report.Limitations, err)
	}
	after, err := os.ReadFile(bindingsPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale binding file was changed")
	}
	infoAfter, err := os.Stat(bindingsPath)
	if err != nil || !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatal("stale binding file was rewritten")
	}
}

func TestCCSwitchStaleReapplyKeepsIndependentCLI(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r := Reader{Home: t.TempDir()}
	quotaFixtures(t, r.Home)
	store, err := os.ReadFile(filepath.Join(r.Home, ".cc-switch", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, r.managedAccountsStorePath(), string(store))
	desktop, err := os.ReadFile(filepath.Join(r.Home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(r.Home, ".codex-cli", "auth.json"), string(desktop))
	writeFixture(t, filepath.Join(r.Home, ".cc-switch", "settings.json"), `{"codexConfigDir":"~/.codex"}`)
	bindingsPath := filepath.Join(r.helperStateRoot(), "accounts", "bindings.json")
	writeFixture(t, bindingsPath, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":null}`)
	if _, err := r.scopeCredential("cli", time.Now()); err == nil {
		t.Fatal("unbound CLI reused a possibly superseded credential")
	}
	writeFixture(t, bindingsPath, `{"version":1,"desktop":{"account_id":"managed-a","needs_reapply":true},"cli":{"account_id":"managed-a","needs_reapply":false}}`)
	if _, err := r.scopeCredential("desktop", time.Now()); err == nil {
		t.Fatal("unreapplied Desktop credential accepted")
	}
	if _, err := r.scopeCredential("cli", time.Now()); err != nil {
		t.Fatalf("independently bound CLI was blocked: %v", err)
	}
	credential, ok := r.warmupCredentials(context.Background(), time.Now())["managed-a"]
	if !ok || len(credential.sources) != 1 || credential.sources[0] != "CLI" {
		t.Fatal("warmup did not choose independent CLI credential")
	}
	sources, err := r.warmupActivitySources("managed-a", credential.sources)
	if err != nil || !strings.Contains(strings.Join(sources, ","), "Desktop") {
		t.Fatal("warmup did not conservatively inspect Desktop activity")
	}
}

func TestNativeDesktopCapabilityDescribesCurrentOwner(t *testing.T) {
	t.Setenv("CODEX_HELPER_NATIVE_PREVIEW", "1")
	r := Reader{Home: t.TempDir(), SQLite: "/usr/bin/sqlite3"}
	settings := filepath.Join(r.Home, ".cc-switch", "settings.json")
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex"}`)
	owned := r.Snapshot(context.Background())
	if !strings.Contains(owned.Capabilities["account_switch"].Reason, "Desktop 由 CC Switch 管理") || strings.Contains(owned.Capabilities["desktop_handoff"].Reason, "退出管理同端") {
		t.Fatal("CC Switch owner status is misleading")
	}
	writeFixture(t, settings, `{"codexConfigDir":"~/.codex-cli"}`)
	native := r.Snapshot(context.Background())
	if !strings.Contains(native.Capabilities["desktop_handoff"].Reason, "小助手账号管理器") || native.Sources[0].Label != "Desktop" {
		t.Fatal("native owner status is misleading")
	}
}

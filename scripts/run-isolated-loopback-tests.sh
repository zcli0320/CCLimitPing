#!/bin/bash
# Isolated tests whose Rust/Codex processes must reach 127.0.0.1 mock servers.
# They use synthetic credentials and temporary roots only. Run them from a
# shell whose process tree may open loopback ports (not a Claude Code session
# restricted by LuLu to its proxy port). A SKIP counts as a failure here.
set -u
cd "$(dirname "$0")/.."

GO_BIN=${GO_BIN:-go}
CARGO_BIN=${CARGO_BIN:-cargo}
export CARGO_NET_OFFLINE=true
CODEX_BACKEND=${CODEX_BACKEND:-/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex}
LOG=${1:-/tmp/cclimitping-loopback-$(date +%Y%m%d-%H%M%S)}
mkdir -p "$LOG"
echo "logs: $LOG"

if pgrep -f '/Codex 小助手.app/Contents/MacOS/' >/dev/null; then
	echo "完整版小助手正在运行，请先正常退出后再测试。" >&2
	exit 2
fi
if ! python3 -c "
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); s.listen()
c = socket.socket(); c.connect(s.getsockname())" 2>"$LOG/loopback-probe.txt"; then
	echo "本进程树无法连接本机临时端口（见 $LOG/loopback-probe.txt），测试结果将无效，已停止。" >&2
	exit 3
fi
test -x "$CODEX_BACKEND" || { echo "missing $CODEX_BACKEND" >&2; exit 2; }

failed=0
record() { # name status
	printf '%-72s %s\n' "$1" "$2" | tee -a "$LOG/summary.txt"
	[ "$2" = PASS ] || failed=1
}

"$CARGO_BIN" test --manifest-path native-accounts/Cargo.toml --locked --offline >"$LOG/cargo-test.log" 2>&1
if [ $? -eq 0 ] && grep -q '^test result: ok' "$LOG/cargo-test.log"; then
	record "rust: cargo test ($(grep '^test result' "$LOG/cargo-test.log" | head -1 | cut -d. -f2 | xargs))" PASS
else
	record "rust: cargo test" FAIL
fi

"$CARGO_BIN" test --manifest-path native-accounts/Cargo.toml --locked --offline \
	--lib --no-run --message-format=json >"$LOG/rust-test-build.jsonl" 2>"$LOG/rust-test-build.err"
export CODEX_HELPER_RUST_SERVICE_TEST_BIN="$(python3 - "$LOG/rust-test-build.jsonl" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    try:
        item = json.loads(line)
    except json.JSONDecodeError:
        continue
    if (item.get('reason') == 'compiler-artifact' and item.get('profile', {}).get('test')
            and 'lib' in item.get('target', {}).get('kind', []) and item.get('executable')):
        print(item['executable'])
PY
)"
if [ -z "$CODEX_HELPER_RUST_SERVICE_TEST_BIN" ]; then
	record "rust: service fixture build" FAIL
	exit 1
fi

run_go() { # test-name, then extra KEY=VALUE environment
	local name=$1; shift
	env "$@" "$GO_BIN" test -race ./internal/assistant -run "^${name}\$" -count=1 -v >"$LOG/$name.log" 2>&1
	if grep -q -- "^--- PASS: $name " "$LOG/$name.log"; then
		record "go: $name" PASS
	elif grep -q -- "^--- SKIP: $name " "$LOG/$name.log"; then
		record "go: $name" SKIP
	else
		record "go: $name" FAIL
	fi
}

run_go TestNativeCLISupervisorWithRealRustManagerAndLocalModel \
	CODEX_HELPER_NATIVE_HANDOFF_TEST=1 CODEX_HELPER_NATIVE_MANAGER_HANDOFF_TEST=1
run_go TestNativeDesktopCoordinatorWithRealRustManagerAndSimulatedRuntime \
	CODEX_HELPER_NATIVE_DESKTOP_MANAGER_TEST=1
run_go TestNativeDesktopManagerRefusesApplyWhileDesktopIsRunning \
	CODEX_HELPER_NATIVE_DESKTOP_GUARD_TEST=1
run_go TestDesktopRealBridgeProcessTreeColdRestart \
	CODEX_HELPER_DESKTOP_PROCESS_TEST=1 CODEX_HELPER_RUNTIME_PROXY_TEST_BIN="$CODEX_BACKEND"
run_go TestNativeLimitRolloutLifecycle \
	CODEX_HELPER_NATIVE_LIMIT_TEST=1

echo
echo "summary: $LOG/summary.txt"
exit $failed

#!/usr/bin/env python3
"""One detached, explicitly authorized native handoff; no credential rollback."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import time

p = argparse.ArgumentParser()
p.add_argument('--thread', required=True)
p.add_argument('--evidence', required=True)
p.add_argument('--test-binary', required=True)
p.add_argument('--helper', required=True)
p.add_argument('--runtime-account', required=True, help='Independently read from the running Desktop account API, not auth.json')
p.add_argument('--delay', type=int, default=15)
p.add_argument('--trigger', choices=['real-quota', 'inject-current-quota'], default='real-quota')
a = p.parse_args()
root = Path(a.evidence)
if not root.is_absolute() or not re.fullmatch(r'[0-9a-fA-F-]{36}', a.thread):
    raise SystemExit('Exact thread and absolute evidence path required')
root.mkdir(mode=0o700, parents=True, exist_ok=True)
with (root / 'started.json').open('x') as f:
    json.dump({'pid': os.getpid(), 'thread': a.thread, 'started_at': time.time()}, f)
env = {k: v for k, v in os.environ.items() if not k.startswith('CODEX_HELPER_LIVE_') and k != 'CODEX_HELPER_TEST_HOME'}
env['CODEX_HELPER_LIVE_NATIVE_FULL'] = 'run-native-dual-once'
env['CODEX_HELPER_LIVE_FULL_DESKTOP_THREAD'] = a.thread
env['CODEX_HELPER_LIVE_NATIVE_EVIDENCE'] = str(root)
env['CODEX_HELPER_LIVE_RUNTIME_ACCOUNT'] = a.runtime_account
if a.trigger == 'inject-current-quota':
    env['CODEX_HELPER_LIVE_NATIVE_TRIGGER'] = a.trigger
time.sleep(max(0, min(a.delay, 60)))
code = -1
with (root / 'experiment.log').open('w') as f:
    try:
        code = subprocess.run([a.test_binary, '-test.run=^TestLiveNativeDualDesktopHandoff$', '-test.v', '-test.timeout=4m'], env=env, stdout=f, stderr=subprocess.STDOUT, timeout=245).returncode
    except subprocess.TimeoutExpired:
        f.write('\nExternal deadline: preserving native recovery records.\n')
(root / 'result.json').write_text(json.dumps({'exit_code': code, 'finished_at': time.time()}, indent=2))
clean = {k: v for k, v in env.items() if not k.startswith('CODEX_HELPER_LIVE_')}
subprocess.run(['/usr/bin/open', '-a', '/Applications/ChatGPT.app', 'codex://threads/' + a.thread], env=clean, check=False)
try:
    native_result = json.loads((root / 'native-result.json').read_text())
except (OSError, ValueError):
    native_result = {}
# A preflight refusal has not paused a turn; do not send a recovery prompt.
if code != 0 and native_result.get('stage') != 'preflight':
    recovery = dict(clean)
    recovery['CODEX_HELPER_LIVE_RECOVERY_THREAD'] = a.thread
    recovery['CODEX_HELPER_LIVE_RECOVERY_EVIDENCE'] = str(root)
    with (root / 'recovery.log').open('w') as f:
        try:
            subprocess.run([a.test_binary, '-test.run=^TestLiveDesktopRecoveryResume$', '-test.v', '-test.timeout=45s'], env=recovery, stdout=f, stderr=subprocess.STDOUT, timeout=50)
        except subprocess.TimeoutExpired:
            f.write('\nRecovery unconfirmed; no automatic resend.\n')
subprocess.run(['/usr/bin/open', a.helper], env=clean, check=False)

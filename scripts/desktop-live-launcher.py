#!/usr/bin/env python3
"""Explicit, detached Desktop handoff experiment launcher; never stores tokens."""
import argparse
import hashlib
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
p.add_argument('--control', required=True)
p.add_argument('--helper', required=True)
p.add_argument('--trigger', choices=['configured-threshold', 'simulate-source-low'], default='configured-threshold')
a = p.parse_args()
if not re.fullmatch(r'[0-9a-fA-F-]{36}', a.thread):
    raise SystemExit('Invalid test conversation')
evidence = Path(a.evidence)
if not evidence.is_absolute() or not Path(a.test_binary).is_file() or not Path(a.control).is_file():
    raise SystemExit('Missing test artifacts')
evidence.mkdir(mode=0o700, parents=True, exist_ok=True)
os.chmod(evidence, 0o700)
if (evidence / 'started.json').exists():
    raise SystemExit('Experiment already registered; inspect its evidence first')
(evidence / 'started.json').write_text(json.dumps({'pid': os.getpid(), 'thread': a.thread, 'started_at': time.time()}))

def hashes():
    base = Path.home()
    names = ['.codex/auth.json', '.codex/config.toml', '.codex-cli/auth.json', '.codex-cli/config.toml', '.cc-switch/settings.json']
    return {n: hashlib.sha256((base / n).read_bytes()).hexdigest() if (base / n).is_file() else None for n in names}

(evidence / 'before-hashes.json').write_text(json.dumps(hashes(), indent=2))
env = dict(os.environ)
env.pop('CODEX_HELPER_TEST_HOME', None)
env['CODEX_HELPER_LIVE_FULL_DESKTOP_THREAD'] = a.thread
env['CODEX_HELPER_LIVE_FULL_DESKTOP_CONTROL'] = a.control
env['CODEX_HELPER_LIVE_FULL_DESKTOP_TRIGGER'] = a.trigger
env['CODEX_HELPER_LIVE_FULL_DESKTOP_CONFIRM'] = 'run-full-handoff-now'
code = -1
with (evidence / 'experiment.log').open('w') as log:
    try:
        result = subprocess.run([a.test_binary, '-test.run=^TestLiveDesktopViaCCSwitch$', '-test.v', '-test.timeout=4m'], env=env, stdout=log, stderr=subprocess.STDOUT, timeout=240)
        code = result.returncode
    except subprocess.TimeoutExpired:
        log.write('\nExternal experiment deadline reached; no automatic credential rollback.\n')
(evidence / 'after-hashes.json').write_text(json.dumps(hashes(), indent=2))
(evidence / 'result.json').write_text(json.dumps({'exit_code': code, 'trigger_mode': a.trigger, 'finished_at': time.time()}, indent=2))
# Always make the original chat reachable after failure. Do not overwrite auth
# or replay the switch. A recovery message is separately registered and sent only
# while the original conversation is idle.
clean_env = {k: v for k, v in os.environ.items() if not k.startswith('CODEX_HELPER_LIVE_') and k != 'CODEX_HELPER_TEST_HOME'}
subprocess.run(['/usr/bin/open', '-a', '/Applications/ChatGPT.app', 'codex://threads/' + a.thread], check=False, env=clean_env)
if code != 0:
    recovery_env = dict(clean_env)
    recovery_env['CODEX_HELPER_LIVE_RECOVERY_THREAD'] = a.thread
    recovery_env['CODEX_HELPER_LIVE_RECOVERY_EVIDENCE'] = str(evidence)
    with (evidence / 'recovery.log').open('w') as log:
        subprocess.run([a.test_binary, '-test.run=^TestLiveDesktopRecoveryResume$', '-test.v', '-test.timeout=45s'], env=recovery_env, stdout=log, stderr=subprocess.STDOUT, timeout=50)
if code == 0:
    subprocess.run(['/usr/bin/open', a.helper], check=False, env=clean_env)

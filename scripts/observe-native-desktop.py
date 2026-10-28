#!/usr/bin/env python3
"""Observe one installed-app native handoff; never initiates or retries it."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time

p = argparse.ArgumentParser()
p.add_argument('--thread', required=True)
p.add_argument('--return-thread', required=True)
p.add_argument('--helper', required=True)
p.add_argument('--evidence', required=True)
a = p.parse_args()
home = Path.home()
root = Path(a.evidence)
root.mkdir(mode=0o700, parents=True, exist_ok=True)
with (root / 'started.json').open('x') as f:
    json.dump({'pid': os.getpid(), 'thread': a.thread, 'return_thread': a.return_thread, 'started_at': time.time()}, f)
handoff = home / 'Library/Application Support/CodexHelper/handoff'

def read(path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return {}

def allocation():
    r = subprocess.run([a.helper, 'accounts', 'status'], capture_output=True, text=True, timeout=15)
    return json.loads(r.stdout)

def config_hashes():
    return {n: hashlib.sha256((home / n).read_bytes()).hexdigest() for n in ['.codex/config.toml', '.codex-cli/config.toml']}

before = allocation()
original = {v['scope']: v.get('account_id') for v in before['clients']}
baseline = config_hashes()
(root / 'before.json').write_text(json.dumps({'allocation': before, 'config_hashes': baseline}, ensure_ascii=False, indent=2))
deadline = time.monotonic() + 240
seen = False
events = []
result = {'success': False, 'kind': 'installed-app-native-dual-desktop', 'threshold_kind': 'adjusted-real-threshold'}
last = None
try:
    while time.monotonic() < deadline:
        s = read(handoff / 'state.json')
        if s.get('session_id') == a.thread and s.get('enabled') and s.get('backend') == 'native-dual':
            seen = True
        current = (s.get('stage'), s.get('pending_action'), s.get('message'))
        if current != last:
            events.append({'at': time.time(), 'stage': current[0], 'pending_action': current[1], 'message': current[2]})
            (root / 'stages.json').write_text(json.dumps(events, ensure_ascii=False, indent=2))
            last = current
        checkpoint = read(handoff / 'desktop-checkpoint.json')
        completed = (seen and s.get('stage') == 'watching' and s.get('message') == '交接完成，继续监控此会话'
                     and checkpoint.get('stage') == 'completed' and checkpoint.get('identity_verified')
                     and checkpoint.get('settings_verified') and checkpoint.get('continue_started')
                     and not (handoff / 'native-transaction.json').exists())
        if completed:
            after = allocation()
            final = {v['scope']: v.get('account_id') for v in after['clients']}
            if not after.get('distinct') or final.get('desktop') != original.get('cli') or final.get('cli') != original.get('desktop'):
                result['error'] = 'final account assignments mismatch'
                break
            if config_hashes() != baseline:
                result['error'] = 'client config changed'
                break
            runner = read(handoff / 'desktop-runner.json')
            os.kill(runner['pid'], 0)
            stopped = subprocess.run([a.helper, 'handoff', 'disable'], capture_output=True, text=True, timeout=10)
            off = json.loads(stopped.stdout)
            if off.get('enabled') or off.get('stage') != 'off':
                result['error'] = 'experiment monitor did not stop cleanly'
                break
            result.update(success=True, checkpoint=checkpoint, after=after, config_hashes=config_hashes(), threshold=s.get('threshold'))
            break
        if seen and (s.get('stage') in ['uncertain', 'error'] or not s.get('enabled')):
            result['error'] = s.get('message', 'operation stopped before verification')
            result['native_transaction'] = read(handoff / 'native-transaction.json')
            break
        time.sleep(1)
    else:
        result['error'] = 'observer deadline reached; recovery records retained'
except Exception as e:
    result['error'] = type(e).__name__
finally:
    result['finished_at'] = time.time()
    (root / 'result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2))
    clean = {k: v for k, v in os.environ.items() if not k.startswith('CODEX_HELPER_LIVE_') and k != 'CODEX_HELPER_TEST_HOME'}
    subprocess.run(['/usr/bin/open', '-a', '/Applications/ChatGPT.app', 'codex://threads/' + a.return_thread], check=False, env=clean)

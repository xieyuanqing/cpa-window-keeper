#!/usr/bin/env python3
"""Approved production install. Defaults to observation; --enable activates scheduler."""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import shutil
import time
import urllib.request
import urllib.error
import yaml
from probe_live import management

ROOT = pathlib.Path(os.environ.get('CPA_ROOT', '/root/CLIProxyAPI'))
ID = 'cpa-window-keeper'
EXPECTED = '73d1cb0f797bddb8bf694e94cb0b041b8bc48b4353ccb8e4d33b02b34f8e59ea'

def request(method, route, data):
    env = {}
    for line in pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env')).read_text().splitlines():
        if '=' in line and not line.lstrip().startswith('#'):
            k, v = line.split('=', 1)
            env[k.strip()] = v.strip().strip('\"\'')
    req = urllib.request.Request('http://127.0.0.1:8317/v0/management/' + route,
        method=method, data=json.dumps(data).encode(), headers={
            'Authorization': 'Bearer ' + env['CPA_MANAGEMENT_KEY'], 'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=70) as r:
        return json.load(r)

def wait_state(dry_run):
    deadline = time.monotonic() + 35
    while time.monotonic() < deadline:
        try:
            d = management(ID + '/status')
            if d.get('config', {}).get('dry_run') is dry_run:
                return d
        except urllib.error.HTTPError as e:
            if e.code not in (404, 503):
                raise
        time.sleep(.5)
    raise RuntimeError('Production plugin configuration readback timed out')

p = argparse.ArgumentParser()
p.add_argument('--enable', action='store_true')
p.add_argument('--resume-backup', type=pathlib.Path)
args = p.parse_args()
if args.enable:
    before = management('plugins/' + ID + '/config')
    assert before['enabled'] is True
    assert management(ID + '/status')['state']['accounts'], 'Observe accounts first'
    request('PATCH', 'plugins/' + ID + '/config', {'dry_run': False})
    print(json.dumps({'automatic_mode': wait_state(False)}, indent=2))
else:
    before = yaml.safe_load((args.resume_backup or (ROOT / 'config.yaml')).read_text())
    assert ID not in before['plugins']['configs'], 'Already configured; refusing overwrite'
    source = pathlib.Path(__file__).resolve().parents[1] / 'dist/cpa-window-keeper-v0.1.0.so'
    assert hashlib.sha256(source.read_bytes()).hexdigest() == EXPECTED
    dest = ROOT / 'plugins/linux/amd64' / source.name
    assert not dest.exists(), 'Refusing to overwrite a potentially loaded library'
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = args.resume_backup.parent if args.resume_backup else ROOT / 'backups' / ('window-keeper-' + stamp)
    if not args.resume_backup:
        backup.mkdir(parents=True, mode=0o700)
        shutil.copy2(ROOT / 'config.yaml', backup / 'config.yaml')
        os.chmod(backup / 'config.yaml', 0o600)
    config = dict(enabled=False, dry_run=True, codex_enabled=True, claude_enabled=True,
        codex_model='gpt-5.6-luna', claude_model='claude-haiku-4-5-20251001',
        poll_seconds=60, grace_seconds=30, account_allowlist='',
        state_path='plugins/data/cpa-window-keeper/state.json')
    # Save safe config before making the new library visible to discovery.
    request('PUT', 'plugins/' + ID + '/config', config)
    saved = management('plugins/' + ID + '/config')
    saved.setdefault('account_allowlist', '')  # CPA omits empty string config values.
    assert saved == config
    shutil.copy2(source, dest)
    os.chmod(dest, 0o644)
    request('PATCH', 'plugins/' + ID + '/enabled', {'enabled': True})
    config['enabled'] = True
    wait_state(True)
    after = yaml.safe_load((ROOT / 'config.yaml').read_text())
    added = after['plugins']['configs'].pop(ID)
    if 'discovery' not in before:
        discovery = after.pop('discovery', {})
        assert discovery == {'service-type': '_ai-gateway._tcp', 'subtypes': [
            '_chat-completions', '_responses', '_messages', '_generate-content', '_interactions']}, 'Unexpected discovery change'
    assert after == before, 'Unexpected production configuration change'
    added.setdefault('account_allowlist', '')
    assert added == config
    assert hashlib.sha256(dest.read_bytes()).hexdigest() == EXPECTED
    print(json.dumps({'installed': True, 'backup': str(backup / 'config.yaml'),
        'other_config_unchanged': True, 'dry_run': True,
        'check': management(ID + '/check', {})}, indent=2))

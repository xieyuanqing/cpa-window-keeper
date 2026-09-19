#!/usr/bin/env python3
"""Hot-upgrade the installed plugin to a newer versioned artifact.

Keeps the previous .so on disk for rollback. Refuses to run unless the plugin is
currently working (enabled, non-dry-run state preserved) so a bad upgrade is
detected immediately. Does not touch any other plugin or config key.
"""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import shutil
import time
import urllib.error
import urllib.request
import yaml

ROOT = pathlib.Path(os.environ.get('CPA_ROOT', '/root/CLIProxyAPI'))
ID = 'cpa-window-keeper'
PLUGIN_DIR = ROOT / 'plugins/linux/amd64'
BASE = 'http://127.0.0.1:8317/v0/management/'


def management(route):
    req = urllib.request.Request(BASE + route, headers=_auth(), method='GET')
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.load(r)


def request(method, route, data):
    req = urllib.request.Request(BASE + route, method=method, data=json.dumps(data).encode(),
        headers=_auth() | {'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=70) as r:
        return json.load(r)


def _auth():
    env = {}
    for line in pathlib.Path('/opt/cpa-manager-plus/.env').read_text().splitlines():
        if '=' in line and not line.lstrip().startswith('#'):
            k, v = line.split('=', 1)
            env[k.strip()] = v.strip().strip('"\'')
    return {'Authorization': 'Bearer ' + env['CPA_MANAGEMENT_KEY']}


def status():
    return management(ID + '/status')


def wait_for(predicate, description, timeout=60, missing_ok=False):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = status()
            if predicate(last):
                return last
        except urllib.error.HTTPError as e:
            last = {'http_error': e.code}
            if missing_ok and e.code == 404:
                return {'stopped': True}
            if e.code not in (404, 503):
                raise
        time.sleep(.5)
    raise RuntimeError('Timed out waiting for %s; last=%s' % (description, json.dumps(last)[:400]))


p = argparse.ArgumentParser()
p.add_argument('--so', type=pathlib.Path, required=True)
p.add_argument('--expect-version', required=True)
p.add_argument('--resume-backup', type=pathlib.Path)
args = p.parse_args()

source = args.so.resolve()
assert source.is_file(), source
digest = hashlib.sha256(source.read_bytes()).hexdigest()
dest = PLUGIN_DIR / source.name

if args.resume_backup:
    # Recovery path: a previous run already disabled the plugin and copied the artifact.
    backup = args.resume_backup
    before_version = (backup / 'previous_version.txt').read_text().strip()
    before_config = dict(yaml.safe_load((ROOT / 'config.yaml').read_text())['plugins']['configs'][ID])
    assert dest.exists() and hashlib.sha256(dest.read_bytes()).hexdigest() == digest, dest
    state_file = ROOT / ('plugins/data/%s/state.json' % ID)
    expected_accounts = sorted(json.loads(state_file.read_text())['accounts']) if state_file.exists() else []
else:
    before = status()
    assert before['config']['enabled'] is True, before
    before_config = dict(before['config'])
    assert before['state']['accounts'], 'Plugin must have live accounts before an upgrade'
    before_version = before['version']
    assert before_version != args.expect_version, 'Target version already active'
    expected_accounts = sorted(before['state']['accounts'])

    assert not dest.exists(), 'Refusing to overwrite an existing artifact: %s' % dest
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = ROOT / 'backups' / ('window-keeper-upgrade-' + stamp)
    backup.mkdir(parents=True, mode=0o700)
    shutil.copy2(ROOT / 'config.yaml', backup / 'config.yaml')
    os.chmod(backup / 'config.yaml', 0o600)
    (backup / 'previous_version.txt').write_text(before_version + '\n')

    shutil.copy2(source, dest)
    os.chmod(dest, 0o644)
    assert hashlib.sha256(dest.read_bytes()).hexdigest() == digest

    # A real false -> true transition forces rediscovery and a hot reload of the new artifact.
    request('PATCH', 'plugins/%s/enabled' % ID, {'enabled': False})
    wait_for(lambda d: d.get('running') is False or d.get('error'), 'plugin stop', missing_ok=True)

before_config.pop('enabled', None)
# state_path/store are installation metadata, not status-reportable operator settings.
for metadata_key in ('state_path', 'store', 'dir'):
    before_config.pop(metadata_key, None)
request('PATCH', 'plugins/%s/enabled' % ID, {'enabled': True})
after = wait_for(lambda d: d['version'] == args.expect_version and d['config']['enabled'],
                 'new version %s active' % args.expect_version)

# Config continuity: every operator-visible setting must survive the upgrade.
for key, value in before_config.items():
    assert after['config'].get(key) == value, ('config drift on ' + key, before_config, after['config'])
assert after['state']['accounts'], 'Plugin lost its accounts across the upgrade'
assert sorted(after['state']['accounts']) == expected_accounts, 'account set changed'

req = urllib.request.Request('http://127.0.0.1:8317/v0/resource/plugins/%s/dashboard' % ID, headers=_auth())
with urllib.request.urlopen(req, timeout=30) as r:
    html = r.read()
assert r.status == 200 and len(html) > 1000, ('dashboard', r.status, len(html))
assert b'cli-proxy-auth' in html, 'dashboard is not the panel-auth build'
assert b'type="password"' in html  # manual fallback still available when the panel forgot the key

print(json.dumps({
    'upgraded': True, 'from': before_version, 'to': after['version'],
    'sha256': digest, 'artifact': str(dest), 'backup': str(backup / 'config.yaml'),
    'config_preserved': True, 'dry_run': after['config']['dry_run'],
    'accounts': len(after['state']['accounts']), 'dashboard_bytes': len(html),
}, indent=2))

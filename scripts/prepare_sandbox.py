#!/usr/bin/env python3
"""Build an isolated CPA test config; never edit production config or refresh credentials."""
import json
import os
import pathlib
import secrets

PROJECT = pathlib.Path(__file__).resolve().parents[1]
PRIVATE = pathlib.Path('/root/.cache/cpa-window-keeper-smoke')
PRIVATE.mkdir(parents=True, exist_ok=True, mode=0o700)
os.chmod(PRIVATE, 0o700)
AUTH = PRIVATE / 'auth'
AUTH.mkdir(exist_ok=True, mode=0o700)
(DATA := PRIVATE / 'data').mkdir(exist_ok=True, mode=0o700)
# Deliberately remove refresh credentials. The sandbox cannot rotate production OAuth tokens.
count = 0
for src in pathlib.Path('/root/CLIProxyAPI/auths').glob('*.json'):
    data = json.loads(src.read_text())
    if data.get('type') not in ('codex', 'claude') or data.get('disabled'):
        continue
    data.pop('refresh_token', None)
    dst = AUTH / ('smoke-' + str(count) + '.json')
    dst.write_text(json.dumps(data))
    dst.chmod(0o600)
    count += 1
keys = {'management': secrets.token_urlsafe(32), 'api': secrets.token_urlsafe(32)}
(PRIVATE / 'keys.json').write_text(json.dumps(keys))
(PRIVATE / 'keys.json').chmod(0o600)
config = {
    'host': '0.0.0.0', 'port': 8317, 'auth-dir': '/smoke/auth',
    'api-keys': [keys['api']],
    'remote-management': {'allow-remote': True, 'secret-key': keys['management'], 'disable-control-panel': True},
    'request-retry': 0, 'max-retry-credentials': 1, 'max-retry-interval': 0,
    'logging-to-file': False, 'request-log': False, 'usage-statistics-enabled': True,
    'plugins': {'enabled': True, 'dir': '/plugins', 'configs': {
        'cpa-window-keeper': {'enabled': True, 'dry_run': True, 'poll_seconds': 30, 'grace_seconds': 10,
            'state_path': '/smoke/data/state.json', 'codex_model': 'gpt-5.6-luna',
            'claude_model': 'claude-haiku-4-5-20251001'}}},
}
(PRIVATE / 'config.yaml').write_text(json.dumps(config))
(PRIVATE / 'config.yaml').chmod(0o600)
print(json.dumps({'sandbox': str(PRIVATE), 'read_only_access_token_snapshots': count,
                  'refresh_tokens_copied': False, 'production_config_modified': False}))

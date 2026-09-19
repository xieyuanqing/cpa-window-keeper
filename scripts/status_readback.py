#!/usr/bin/env python3
"""Read back the live plugin status (key never printed)."""
import json
import os
import pathlib
import urllib.request

env = dict(
    line.split('=', 1)
    for line in pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env')).read_text().splitlines()
    if '=' in line and not line.lstrip().startswith('#')
)
key = env['CPA_MANAGEMENT_KEY'].strip().strip('"\'')
req = urllib.request.Request(
    'http://127.0.0.1:8317/v0/management/cpa-window-keeper/status',
    headers={'Authorization': 'Bearer ' + key},
)
with urllib.request.urlopen(req, timeout=20) as response:
    data = json.load(response)
accounts = data.get('state', {}).get('accounts', {})
print(json.dumps({
    'http': response.status,
    'version': data.get('version'),
    'enabled': data.get('config', {}).get('enabled'),
    'dry_run': data.get('config', {}).get('dry_run'),
    'sending_suspended': data.get('sending_suspended'),
    'error': data.get('error'),
    'accounts': {i: {'provider': a.get('provider'), 'status': a.get('status'),
                     'used_percent': a.get('quota', {}).get('used_percent'),
                     'reset_at': a.get('quota', {}).get('reset_at'),
                     'attempts': a.get('attempts'), 'successes': a.get('successes')}
                 for i, a in accounts.items()},
}, ensure_ascii=False, indent=2))

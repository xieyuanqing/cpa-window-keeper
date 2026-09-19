#!/usr/bin/env python3
"""Read-only quota/model probe via the existing CPA management API; redact identities."""
import concurrent.futures
import json
import os
import pathlib
import urllib.parse
import urllib.request


def management(path, payload=None):
    env = {}
    for line in pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env')).read_text().splitlines():
        if '=' in line and not line.lstrip().startswith('#'):
            k, v = line.split('=', 1)
            env[k.strip()] = v.strip().strip('\"\'')
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request('http://127.0.0.1:8317/v0/management/' + path, data=data,
        headers={'Authorization': 'Bearer ' + env['CPA_MANAGEMENT_KEY'], 'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=70) as r:
        return json.load(r)


def probe(item):
    i, a = item
    provider = a.get('provider') or a.get('type')
    result = {'account': f'{provider}-{i}', 'disabled': a.get('disabled'), 'status': a.get('status')}
    h = {'Authorization': 'Bearer $TOKEN$', 'Accept': 'application/json'}
    if provider == 'codex':
        url = 'https://chatgpt.com/backend-api/wham/usage'
        # Only use this exact file in memory; never print credential fields.
        p = pathlib.Path('/root/CLIProxyAPI/auths') / pathlib.Path(a['name']).name
        account_id = json.loads(p.read_text()).get('account_id')
        if account_id:
            h['Chatgpt-Account-Id'] = account_id
    else:
        url = 'https://api.anthropic.com/api/oauth/usage'
        h['anthropic-beta'] = 'oauth-2025-04-20'
        h['User-Agent'] = 'claude-cli/2.1.5 (external, cli)'
    response = management('api-call', {'auth_index': a['auth_index'], 'method': 'GET', 'url': url, 'header': h})
    result['quota_http'] = response['status_code']
    try:
        quota = json.loads(response['body'])
    except (ValueError, KeyError):
        quota = {}
    allow = ('rate_limit', 'plan_type', 'five_hour', 'seven_day', 'seven_day_sonnet', 'seven_day_opus')
    result['quota'] = {k: quota[k] for k in allow if k in quota}
    result['quota_fields'] = sorted(quota)
    if response['status_code'] != 200:
        result['error_type'] = (quota.get('error') or {}).get('type') if isinstance(quota.get('error'), dict) else 'upstream_error'
    models = management('auth-files/models?' + urllib.parse.urlencode({'name': a['name']}))
    result['small_models'] = [m for m in models.get('models', []) if any(x in m.get('id', '').lower() for x in ('mini', 'nano', 'haiku'))]
    return result


if __name__ == '__main__':
    files = management('auth-files')['files']
    items = [(i, a) for i, a in enumerate(files, 1) if (a.get('provider') or a.get('type')) in ('codex', 'claude')]
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        print(json.dumps(list(pool.map(probe, items)), ensure_ascii=False, indent=2))

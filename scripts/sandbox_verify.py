#!/usr/bin/env python3
"""Verify the isolated real CPA instance. --generate explicitly sends one tiny request per provider."""
import argparse
import json
import pathlib
import urllib.error
import urllib.request

BASE = 'http://127.0.0.1:18517'
KEYS = json.loads(pathlib.Path('/root/.cache/cpa-window-keeper-smoke/keys.json').read_text())


def call(path, payload=None, admin=False, authenticated=True):
    headers = {'Content-Type': 'application/json'}
    if authenticated:
        headers['Authorization'] = 'Bearer ' + KEYS['management' if admin else 'api']
    req = urllib.request.Request(BASE + path, data=None if payload is None else json.dumps(payload).encode(), headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            body = r.read()
            try:
                decoded = json.loads(body)
            except ValueError:
                decoded = None
            return r.status, decoded, len(body)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            decoded = json.loads(raw)
        except ValueError:
            decoded = {}
        # Error bodies contain provider messages, but no request headers or credentials.
        return e.code, decoded, len(raw)


def main():
    args = argparse.ArgumentParser()
    args.add_argument('--generate', action='store_true')
    opt = args.parse_args()
    code, models, _ = call('/v1/models')
    assert code == 200, ('readiness', code)
    code, plugins, _ = call('/v0/management/plugins', admin=True)
    assert code == 200
    target = next(p for p in plugins['plugins'] if p['id'] == 'cpa-window-keeper')
    assert target['registered'] and target['effective_enabled'], target
    code, _, size = call('/v0/resource/plugins/cpa-window-keeper/dashboard', authenticated=False)
    assert code == 200 and size > 1000, ('dashboard', code)
    unauth, _, _ = call('/v0/management/cpa-window-keeper/status', authenticated=False)
    assert unauth in (401, 403), ('management authentication', unauth)
    code, state, _ = call('/v0/management/cpa-window-keeper/status', admin=True)
    assert code == 200 and state['config']['dry_run'] is True, state
    check, _, _ = call('/v0/management/cpa-window-keeper/check', {}, admin=True)
    assert check == 202
    report = {'plugin_registered': True, 'native_dashboard_http': code,
        'unauthenticated_state_http': unauth, 'dry_run': state['config']['dry_run'],
        'native_account_status': {k: {'provider': v['provider'], 'status': v['status'], 'attempts': v['attempts'],
        'error': v.get('last_error', ''), 'reset_at': v['quota']['reset_at']} for k, v in state['state']['accounts'].items()},
        'real_generations': []}
    if opt.generate:
        requests = [
            ('codex', '/v1/responses', {'model': 'gpt-5.6-luna', 'input': [{'role': 'user', 'content': [{'type': 'input_text', 'text': 'Reply OK.'}]}],
                'instructions': '', 'reasoning': {'effort': 'low'}, 'text': {'verbosity': 'low'}, 'store': False, 'stream': False, 'tools': []}),
            ('claude', '/v1/messages', {'model': 'claude-haiku-4-5-20251001', 'max_tokens': 1, 'messages': [{'role': 'user', 'content': 'Hi'}],
                'thinking': {'type': 'disabled'}, 'stream': False}),
        ]
        for provider, path, payload in requests:
            status, result, _ = call(path, payload)
            report['real_generations'].append({'provider': provider, 'http': status,
                'model': payload['model'], 'usage': (result or {}).get('usage'),
                'error': (result or {}).get('error') if status != 200 else None})
    print(json.dumps(report, ensure_ascii=False, indent=2))
    assert all(x['http'] == 200 for x in report['real_generations']), 'real minimal generation failed'


if __name__ == '__main__':
    main()

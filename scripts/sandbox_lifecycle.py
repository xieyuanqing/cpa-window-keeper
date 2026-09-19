#!/usr/bin/env python3
"""Validate native enable/disable/reconfigure against the sandbox only (18517)."""
import json
import time
import urllib.request
from sandbox_verify import BASE, KEYS, call


def patch_enabled(value):
    req = urllib.request.Request(BASE + '/v0/management/plugins/cpa-window-keeper/enabled',
        data=json.dumps({'enabled': value}).encode(), method='PATCH',
        headers={'Authorization': 'Bearer ' + KEYS['management'], 'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=30) as r:
        assert r.status == 200


def wait_enabled(value):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        code, data, _ = call('/v0/management/plugins', admin=True)
        p = next(x for x in data['plugins'] if x['id'] == 'cpa-window-keeper')
        if code == 200 and p['effective_enabled'] == value:
            return p
        time.sleep(.1)
    raise AssertionError('plugin lifecycle readback timed out')


patch_enabled(False)
assert wait_enabled(False)['effective_enabled'] is False
code, _, _ = call('/v0/management/cpa-window-keeper/status', admin=True)
assert code == 404, ('disabled plugin endpoint remains active', code)
patch_enabled(True)
assert wait_enabled(True)['registered'] is True
code, data, _ = call('/v0/management/cpa-window-keeper/status', admin=True)
assert code == 200 and data['config']['dry_run'] is True
assert all(s['attempts'] == 0 for s in data['state']['accounts'].values())
print(json.dumps({'native_disable': 'passed', 'disabled_endpoint_http': 404,
                  'native_reenable': 'passed', 'persistent_state': 'passed',
                  'automatic_generations': 0}, indent=2))

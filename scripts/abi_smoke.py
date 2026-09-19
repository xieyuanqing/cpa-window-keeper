#!/usr/bin/env python3
"""Exercise the real .so C ABI with a clearly synthetic, credential-free CPA host."""
import base64
import ctypes as C
import datetime
import json
import pathlib
import re
import tempfile
import threading
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
libc = C.CDLL(None)
libc.malloc.argtypes = [C.c_size_t]
libc.malloc.restype = C.c_void_p
libc.free.argtypes = [C.c_void_p]


def newest_artifact():
    """Highest versioned artifact in dist/, mirroring CPA's own plugin discovery rule."""
    def rank(path):
        found = re.match(r'cpa-window-keeper-v(\d+)\.(\d+)\.(\d+)\.so$', path.name)
        return tuple(int(part) for part in found.groups()) if found else None

    candidates = [p for p in (ROOT / 'dist').glob('cpa-window-keeper-v*.so') if rank(p)]
    assert candidates, 'no versioned artifact in dist/ — run scripts/build.sh first'
    return max(candidates, key=rank)


ARTIFACT = newest_artifact()
EXPECTED_VERSION = re.match(r'cpa-window-keeper-v(.+)\.so$', ARTIFACT.name).group(1)


class Buffer(C.Structure):
    _fields_ = [('ptr', C.c_void_p), ('len', C.c_size_t)]


HOST_CALL = C.CFUNCTYPE(C.c_int, C.c_void_p, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
FREE = C.CFUNCTYPE(None, C.c_void_p, C.c_size_t)
PLUGIN_CALL = C.CFUNCTYPE(C.c_int, C.c_char_p, C.c_void_p, C.c_size_t, C.POINTER(Buffer))
SHUTDOWN = C.CFUNCTYPE(None)


class Host(C.Structure):
    _fields_ = [('abi', C.c_uint32), ('ctx', C.c_void_p), ('call', HOST_CALL), ('free', FREE)]


class Plugin(C.Structure):
    _fields_ = [('abi', C.c_uint32), ('call', PLUGIN_CALL), ('free', FREE), ('shutdown', SHUTDOWN)]


accounts = [{'id': 'synthetic-' + p, 'auth_index': p + '-fixture', 'provider': p} for p in ['codex', 'claude']]
sends = []
opened = {}
callback_errors = []
condition = threading.Condition()


def host_result(method, payload):
    if method == 'host.auth.list':
        return {'files': accounts}
    if method == 'host.auth.get_runtime':
        return {'auth': next(a for a in accounts if a['auth_index'] == payload['auth_index'])}
    if method == 'host.auth.get':
        return {'json': {'access_token': 'synthetic-not-a-real-token', 'account_id': 'synthetic-account'}}
    if method == 'host.http.do':
        provider = 'claude' if 'anthropic' in payload['url'] else 'codex'
        start = opened.get(provider)
        if provider == 'claude':
            quota = {'five_hour': {'utilization': 1 if start else 0,
                'resets_at': datetime.datetime.fromtimestamp(start + 18000, datetime.timezone.utc).isoformat() if start else None},
                'seven_day': {'utilization': 1, 'resets_at': datetime.datetime.fromtimestamp(time.time()+86400, datetime.timezone.utc).isoformat()}}
        else:
            quota = {'plan_type': 'plus', 'rate_limit': {'allowed': True, 'limit_reached': False,
                'primary_window': {'used_percent': 1 if start else 0, 'limit_window_seconds': 18000,
                    'reset_at': int(start + 18000) if start else 0, 'reset_after_seconds': 18000 if start else 0},
                'secondary_window': None}}
        return {'StatusCode': 200, 'Body': base64.b64encode(json.dumps(quota).encode()).decode()}
    if method == 'host.model.execute':
        provider = payload['forced_provider']
        assert payload['auth_id'] == 'synthetic-' + provider
        assert not payload['stream']
        body = json.loads(base64.b64decode(payload['body']))
        if provider == 'claude':
            assert body['thinking'] == {'type': 'disabled'} and body['max_tokens'] == 1
            assert body['model'] == 'claude-haiku-4-5-20251001'
        else:
            assert body['reasoning']['effort'] == 'low' and body['tools'] == []
            assert body['model'] == 'gpt-5.6-luna'
        with condition:
            assert provider not in opened, 'duplicate wake sent through the real ABI'
            opened[provider] = time.time()
            sends.append(provider)
            condition.notify_all()
        return {'status_code': 200, 'body': base64.b64encode(b'{"ok":true}').decode()}
    raise AssertionError('Unexpected callback: ' + method)


@HOST_CALL
def callback(ctx, method, request, size, output):
    try:
        result = host_result(method.decode(), json.loads(C.string_at(request, size)))
        data = json.dumps({'ok': True, 'result': result}).encode()
        code = 0
    except Exception as exc:
        callback_errors.append(str(exc))
        data = json.dumps({'ok': False, 'error': {'code': 'mock_error', 'message': str(exc)}}).encode()
        code = 1
    ptr = libc.malloc(len(data))
    C.memmove(ptr, data, len(data))
    output.contents.ptr, output.contents.len = ptr, len(data)
    return code


@FREE
def free(ptr, size):
    libc.free(ptr)


def main():
    library = C.CDLL(str(ARTIFACT))
    library.cliproxy_plugin_init.argtypes = [C.POINTER(Host), C.POINTER(Plugin)]
    library.cliproxy_plugin_init.restype = C.c_int
    host, plugin = Host(1, None, callback, free), Plugin()
    assert library.cliproxy_plugin_init(C.byref(host), C.byref(plugin)) == 0

    def invoke(method, payload):
        raw = json.dumps(payload).encode()
        buf = C.create_string_buffer(raw)
        output = Buffer()
        rc = plugin.call(method.encode(), C.cast(buf, C.c_void_p), len(raw), C.byref(output))
        try:
            data = json.loads(C.string_at(output.ptr, output.len))
        finally:
            plugin.free(output.ptr, output.len)
        assert rc == 0 and data['ok'], data
        return data['result']

    def status():
        r = invoke('management.handle', {'Method': 'GET', 'Path': '/cpa-window-keeper/status'})
        assert r['StatusCode'] == 200
        return json.loads(base64.b64decode(r['Body']))

    def check():
        r = invoke('management.handle', {'Method': 'POST', 'Path': '/cpa-window-keeper/check'})
        assert r['StatusCode'] == 202

    with tempfile.TemporaryDirectory(prefix='window-keeper-abi-') as tmp:
        config = {'enabled': True, 'dry_run': False, 'poll_seconds': 30, 'grace_seconds': 10,
                  'state_path': str(pathlib.Path(tmp) / 'state.json')}
        lifecycle = {'config_yaml': base64.b64encode(json.dumps(config).encode()).decode(), 'schema_version': 1}
        try:
            registration = invoke('plugin.register', lifecycle)
            assert registration['metadata']['Version'] == EXPECTED_VERSION
            assert registration['metadata']['GitHubRepository'].startswith('https://github.com/')
            assert invoke('management.register', {})['routes']
            html = invoke('management.handle', {'Method': 'GET', 'Path': '/dashboard'})
            assert b'synthetic-not-a-real-token' not in base64.b64decode(html['Body'])
            check()
            deadline = time.monotonic() + 5
            while len(status()['state']['accounts']) != 2 and time.monotonic() < deadline:
                time.sleep(.05)
            s = status()
            assert all(a['status'] == 'idle_grace' for a in s['state']['accounts'].values()), s
            assert sends == []
            # Exercise the real scheduler grace interval, not an invented host result.
            time.sleep(10.2)
            check()
            with condition:
                assert condition.wait_for(lambda: len(sends) == 2, timeout=5), (sends, callback_errors)
            assert sorted(sends) == ['claude', 'codex']
            # Quiesce releases the worker/state lock before a binary replacement.
            invoke('plugin.quiesce', {})
            assert status()['running'] is False
            # Reload joins the old worker and loads its durable send reservations.
            invoke('plugin.reconfigure', lifecycle)
            check()
            time.sleep(.2)
            assert len(sends) == 2
            assert not callback_errors, callback_errors
            s = status()
            assert all(a['attempts'] == 1 for a in s['state']['accounts'].values())
            print(json.dumps({'test_kind': 'synthetic C-ABI integration, not live provider evidence',
                'registration': 'passed', 'resources': 'passed', 'account_locking': 'passed',
                'minimal_payloads': 'passed', 'idle_grace': 'passed',
                'one_send_per_account': 'passed', 'reload_deduplication': 'passed',
                'sends': sends}, ensure_ascii=False, indent=2))
        finally:
            plugin.shutdown()
    print('C-ABI shutdown joined the worker successfully')


if __name__ == '__main__':
    main()

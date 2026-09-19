#!/usr/bin/env python3
"""Debug why the embedded dashboard stalls: capture console/errors and probe fetch."""
import json
import os
import pathlib
import tempfile
from patchright.sync_api import sync_playwright

PANEL = os.environ.get('CPAMP_PANEL_URL', 'http://127.0.0.1:18317/management.html')
ENV_FILE = pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env'))
env = {}
for line in ENV_FILE.read_text().splitlines():
    if '=' in line and not line.lstrip().startswith('#'):
        k, v = line.split('=', 1)
        env[k.strip()] = v.strip().strip('"\'')

with tempfile.TemporaryDirectory(prefix='wk-debug-') as profile:
    with sync_playwright() as pw:
        ctx = pw.chromium.launch_persistent_context(profile, channel='chrome', headless=False,
            viewport={'width': 1280, 'height': 900}, args=['--no-sandbox', '--disable-dev-shm-usage'])
        page = ctx.pages[0]
        logs = []
        page.on('console', lambda m: logs.append('console:' + m.type + ':' + m.text[:200]))
        page.on('pageerror', lambda e: logs.append('pageerror:' + str(e)[:300]))
        page.goto(PANEL, wait_until='domcontentloaded')
        page.locator('input[type=password]').first.wait_for(timeout=30000)
        page.locator('input[type=password]').first.fill(env['CPAMP_ADMIN_KEY'])
        label = page.locator('label').filter(has=page.locator('input[type=checkbox]')).first
        if label.count():
            label.click()
        page.get_by_role('button', name='Login', exact=True).click()
        page.get_by_text('5 小时自动开窗', exact=True).wait_for(timeout=30000)
        page.get_by_text('5 小时自动开窗', exact=True).click()
        frame = page.frame_locator('iframe')
        frame.locator('h1').wait_for(timeout=30000)
        probe = frame.locator('body').evaluate("""async () => {
          const out = {href: location.href, readyState: document.readyState,
            hasBoot: typeof window.boot, scripts: document.scripts.length,
            scriptTypes: Array.from(document.scripts).map(s => s.type)};
          try {
            const raw = localStorage.getItem('cli-proxy-auth');
            out.storedLen = raw ? raw.length : 0;
            out.prefix = raw ? raw.slice(0, 9) : null;
          } catch (e) { out.storageError = String(e); }
          try {
            const r = await fetch('/v0/management/cpa-window-keeper/status', {cache: 'no-store'});
            out.noAuthStatus = r.status;
          } catch (e) { out.fetchError = String(e); }
          return out;
        }""")
        print(json.dumps({'probe': probe, 'logs': logs[:20]}, ensure_ascii=False, indent=2))
        ctx.close()

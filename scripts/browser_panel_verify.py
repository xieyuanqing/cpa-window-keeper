#!/usr/bin/env python3
"""Exercise the real CPAMP login and the embedded plugin in a real browser.

Case A: panel login with "remember" -> the plugin must reuse the panel login state
        and show no key prompt.
Case B: panel login without "remember" -> the plugin has no key source; the manual
        fallback must be the only thing shown (documented limitation of CPAMP).
Credentials are read from the panel .env and never printed. Profiles are temporary.
"""
import argparse
import json
import os
import pathlib
import tempfile
from patchright.sync_api import sync_playwright

PANEL = os.environ.get('CPAMP_PANEL_URL', 'http://127.0.0.1:18317/management.html')
ENV_FILE = pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env'))


def load_env():
    env = {}
    for line in pathlib.Path('/opt/cpa-manager-plus/.env').read_text().splitlines():
        if '=' in line and not line.lstrip().startswith('#'):
            k, v = line.split('=', 1)
            env[k.strip()] = v.strip().strip('"\'')
    return env


def observe(frame):
    return {
        'password_visible': frame.locator('#manual:not(.hidden)').count() > 0,
        'password_fields': frame.locator('input[type=password]').count(),
        'session_badge': frame.locator('#session').inner_text(),
        'account_cards': frame.locator('#accounts h3').count(),
        'summary': frame.locator('#summary').inner_text()[:160],
        'message': frame.locator('#message').inner_text()[:160],
    }


def run(remember):
    env = load_env()
    with tempfile.TemporaryDirectory(prefix='window-keeper-browser-') as profile:
        with sync_playwright() as pw:
            ctx = pw.chromium.launch_persistent_context(
                profile, channel='chrome', headless=False,
                viewport={'width': 1280, 'height': 900},
                args=['--no-sandbox', '--disable-dev-shm-usage'])
            page = ctx.pages[0]
            page.goto(PANEL, wait_until='domcontentloaded')
            page.locator('input[type=password]').first.wait_for(timeout=30000)
            page.locator('input[type=password]').first.fill(env['CPAMP_ADMIN_KEY'])
            if remember:
                label = page.locator('label').filter(has=page.locator('input[type=checkbox]')).first
                if label.count():
                    label.click()
            page.get_by_role('button', name='Login', exact=True).click()
            stored = page.evaluate("!!localStorage.getItem('cli-proxy-auth')")
            link = page.get_by_text('5 小时自动开窗', exact=True)
            link.wait_for(timeout=30000)
            link.click()
            frame = page.frame_locator('iframe')
            frame.locator('#summary').wait_for(timeout=30000)
            result = {'remember_checked': remember, 'panel_auth_stored': stored}
            try:
                frame.locator('#accounts h3').first.wait_for(timeout=15000)
            except Exception:
                pass
            result.update(observe(frame))
            if remember:
                result['refresh'] = 'skipped'
                if result['account_cards'] >= 1:
                    frame.locator('#check').click()
                    frame.locator('#message').filter(has_text='已排队').wait_for(timeout=15000)
                    result['refresh'] = 'queue accepted'
            ctx.close()
            return result


p = argparse.ArgumentParser()
p.add_argument('--case', choices=['remember', 'no-remember', 'both'], default='both')
a = p.parse_args()
out = {}
if a.case in ('remember', 'both'):
    out['with_panel_remember'] = run(True)
if a.case in ('no-remember', 'both'):
    out['without_panel_remember'] = run(False)
print(json.dumps(out, ensure_ascii=False, indent=2))

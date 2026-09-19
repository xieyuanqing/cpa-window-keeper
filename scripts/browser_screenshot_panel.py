#!/usr/bin/env python3
"""Capture the embedded plugin panel through the real CPAMP sidebar (phone viewport)."""
import argparse
import os
import pathlib
import tempfile
import urllib.parse
from patchright.sync_api import sync_playwright

PANEL = os.environ.get('CPAMP_PANEL_URL', 'http://127.0.0.1:18317/management.html')
ENV_FILE = pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env'))
env = {}
for line in ENV_FILE.read_text().splitlines():
    if '=' in line and not line.lstrip().startswith('#'):
        k, v = line.split('=', 1)
        env[k.strip()] = v.strip().strip('"\'')

p = argparse.ArgumentParser()
p.add_argument('--out', required=True)
p.add_argument('--scheme', choices=['light', 'dark'], default='light')
a = p.parse_args()

with tempfile.TemporaryDirectory(prefix='wk-shot-') as profile:
    with sync_playwright() as pw:
        ctx = pw.chromium.launch_persistent_context(
            profile, channel='chrome', headless=False, color_scheme=a.scheme,
            viewport={'width': 430, 'height': 932}, device_scale_factor=2,
            args=['--no-sandbox', '--disable-dev-shm-usage'])
        page = ctx.pages[0]
        page.goto(PANEL, wait_until='domcontentloaded')
        page.locator('input[type=password]').first.wait_for(timeout=30000)
        page.locator('input[type=password]').first.fill(env['CPAMP_ADMIN_KEY'])
        label = page.locator('label').filter(has=page.locator('input[type=checkbox]')).first
        if label.count():
            label.click()
        page.get_by_role('button', name='Login', exact=True).click()
        nav = page.get_by_text('5 小时自动开窗', exact=True)
        nav.wait_for(timeout=30000)
        # On a phone-width viewport the sidebar drawer is off-screen, so resolve the
        # route from the nav link and navigate directly instead of clicking.
        href = nav.evaluate("el => { const a = el.closest('a'); return a ? a.getAttribute('href') : null }")
        if href:
            page.goto(urllib.parse.urljoin(page.url, href), wait_until='domcontentloaded')
        else:
            nav.click()
        frame = page.frame_locator('iframe')
        frame.locator('#accounts h3').first.wait_for(timeout=30000)
        page.wait_for_timeout(1200)
        panel_theme = page.evaluate(
            "() => document.documentElement.getAttribute('data-theme')"
            " || (document.documentElement.classList.contains('dark') ? 'dark' : 'light')")
        inner = frame.locator('#summary').evaluate(
            "() => document.documentElement.getAttribute('data-theme')")
        page.screenshot(path=a.out, full_page=False)
        page.locator('iframe').first.screenshot(path=a.out.replace('.png', '-frame.png'))
        audit = frame.locator('body').evaluate("""() => {
          const cs = el => el ? getComputedStyle(el) : null;
          const card = document.querySelector('.acct') || document.querySelector('.card');
          const c = cs(card), b = cs(document.body);
          const root = getComputedStyle(document.documentElement);
          return {
            surface_var: root.getPropertyValue('--app-surface').trim(),
            accent_var: root.getPropertyValue('--primary-color').trim() || '(none)',
            body_bg: b.backgroundColor, body_font: b.fontFamily.slice(0, 40),
            card_bg: c && c.backgroundColor, card_radius: c && c.borderRadius,
            card_blur: c && (c.backdropFilter || c.webkitBackdropFilter),
            card_shadow: c && c.boxShadow.slice(0, 46),
            h1_size: cs(document.querySelector('h1')) && cs(document.querySelector('h1')).fontSize,
            accounts: document.querySelectorAll('.acct').length,
            overflow_x: document.documentElement.scrollWidth > document.documentElement.clientWidth,
          };
        }""")
        print({'out': a.out, 'panel_theme': panel_theme, 'iframe_theme_attr': inner,
               'scheme': a.scheme, 'audit': audit})
        ctx.close()

#!/usr/bin/env python3
"""Capture the README screenshots (light + dark) of the dashboard page.

The plugin page is opened *standalone* on the panel origin, at a desktop viewport, and
captured full-page — the same shape as the sibling cpa-quota-cards shots, so the README
shows what the page looks like on a computer rather than a narrow phone strip.

The page takes its palette from CSS variables the panel injects into the sidebar iframe, so
the script first reads those variables (and the data-theme the panel sets) out of the live
panel for each scheme, then applies exactly those values to the standalone page. The shot is
only written if the theme provably rendered (background luminance), if every account
identifier was masked — auth index, fragments, e-mails — and if the last account card is
inside the image and the English chrome carries no CJK.

    CPAMP_PANEL_URL=https://<your-panel-host>/management.html \\
        /opt/browser-automation/run.sh scripts/make_readme_shots.py

Env overrides: SHOT_DIR (default docs), SHOT_WIDTH (downscale target, default 1400),
SHOT_VIEW_WIDTH/SHOT_VIEW_HEIGHT (default 1280x1200), SHOT_LANG (default en),
CPA_WINDOW_KEEPER_PATH, CPAMP_ENV_FILE.
"""
import json
import os
import pathlib
import re
import struct
import sys
import tempfile
from urllib.parse import urljoin

from patchright.sync_api import sync_playwright

PANEL = os.environ.get('CPAMP_PANEL_URL', 'http://127.0.0.1:18317/management.html')
ORIGIN = os.environ.get('CPAMP_ORIGIN') or re.match(r'https?://[^/]+', PANEL).group(0)
PAGE_URL = ORIGIN + os.environ.get(
    'CPA_WINDOW_KEEPER_PATH', '/v0/resource/plugins/cpa-window-keeper/dashboard')
ENV_FILE = pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env'))
OUT = pathlib.Path(os.environ.get('SHOT_DIR', 'docs'))
SHOT_WIDTH = int(os.environ.get('SHOT_WIDTH', '1400'))
VIEW_W = int(os.environ.get('SHOT_VIEW_WIDTH', '1280'))
VIEW_H = int(os.environ.get('SHOT_VIEW_HEIGHT', '1200'))
LANG = os.environ.get('SHOT_LANG', 'en')
MENU = os.environ.get('CPA_WINDOW_KEEPER_MENU', '5 小时自动开窗')
SCHEMES = ('light', 'dark')
IFRAME_SEL = "iframe[src*='cpa-window-keeper']"
# The panel's own CSS variables that the plugin page is built on.
PALETTE_VARS = ['--app-bg', '--app-surface', '--text-primary', '--text-secondary',
                '--text-tertiary', '--primary-color', '--border-color']
CJK = [(0x4E00, 0x9FFF), (0x3400, 0x4DBF), (0xF900, 0xFAFF)]

# In-place masking of account identifiers. Runs on text nodes and on the few
# attributes that can carry an identifier.
MASK_JS = r"""
() => {
  const D4 = '\u2022\u2022\u2022\u2022';
  const ELL = '\u2026';
  const mask = s => s
    .replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g, 'a\u2022\u2022\u2022@\u2022\u2022\u2022.com')
    // how the dashboard renders an auth index: first4 + ellipsis + last4
    .replace(/\b[0-9a-zA-Z]{4}\u2026[0-9a-zA-Z]{4}\b/g, D4 + ELL + D4)
    .replace(/\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, D4 + '-' + D4)
    .replace(/\b(?=[0-9a-f]*[a-f])[0-9a-f]{8,}\b/g, '\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022');
  let hits = 0;
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  let n;
  while ((n = walker.nextNode())) {
    const v = mask(n.nodeValue);
    if (v !== n.nodeValue) { n.nodeValue = v; hits++; }
  }
  for (const el of document.querySelectorAll('[title],[placeholder]')) {
    for (const a of ['title', 'placeholder']) {
      const cur = el.getAttribute(a);
      if (cur) { const v = mask(cur); if (v !== cur) { el.setAttribute(a, v); hits++; } }
    }
  }
  return {hits: hits, text: document.body.innerText,
          // The language toggle shows the *other* language's name on purpose, so the
          // "English chrome is free of CJK" check must skip it.
          chrome: document.body.innerText.split(
            (document.getElementById('lang') || {}).innerText || '\u0000').join(''),
          lang: document.documentElement.lang};
}
"""

# Post-mask audit: nothing identifying may survive anywhere a reader can see it.
AUDIT_JS = r"""
(ids) => {
  const blob = [document.body.innerText];
  for (const node of document.querySelectorAll('*')) {
    for (const a of ['title', 'placeholder', 'aria-label', 'data-id']) {
      const v = node.getAttribute(a);
      if (v) blob.push(v);
    }
  }
  const text = blob.join('\n');
  const bad = [];
  for (const id of ids) {
    for (const frag of [id, id.slice(0, 8), id.slice(0, 4), id.slice(-4)]) {
      if (frag.length >= 4 && text.includes(frag)) bad.push(frag);
    }
  }
  return {
    bad: [...new Set(bad)],
    mails: text.match(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g) || [],
    text: text,
  };
}
"""

LAYOUT_JS = r"""
() => {
  const cards = [...document.querySelectorAll('.acct')];
  const last = cards[cards.length - 1];
  const rect = last ? last.getBoundingClientRect() : null;
  return {
    content: document.documentElement.scrollHeight,
    viewport: window.innerHeight,
    cards: cards.length,
    last_card_bottom: rect ? Math.round(rect.bottom + window.scrollY) : null,
  };
}
"""

# Read the palette the panel injects into the plugin page inside the sidebar iframe.
READ_PALETTE_JS = (r"""
() => {
  const f = document.querySelector("%s");
  const d = f && f.contentDocument;
  if (!d) return null;
  const cs = getComputedStyle(d.documentElement);
  const vars = {};
  for (const n of %s) { const v = cs.getPropertyValue(n).trim(); if (v) vars[n] = v; }
  return {vars: vars, theme: d.documentElement.getAttribute('data-theme')};
}
""" % (IFRAME_SEL, json.dumps(PALETTE_VARS))).strip()

APPLY_PALETTE_JS = r"""
(p) => {
  for (const [k, v] of Object.entries(p.vars || {})) {
    document.documentElement.style.setProperty(k, v);
  }
  if (p.theme) document.documentElement.setAttribute('data-theme', p.theme);
  return Object.keys(p.vars || {}).length;
}
"""

# Verify the theme really rendered, not just that the attribute is set.
THEME_JS = r"""
() => {
  const lum = c => {
    const m = (c || '').match(/[\d.]+/g);
    if (!m) return null;
    if (m.length > 3 && parseFloat(m[3]) === 0) return null;  // fully transparent
    return Math.round(0.2126 * +m[0] + 0.7152 * +m[1] + 0.0722 * +m[2]);
  };
  const body = getComputedStyle(document.body).backgroundColor;
  const html = getComputedStyle(document.documentElement).backgroundColor;
  return {theme: document.documentElement.getAttribute('data-theme'),
          body_bg: body, html_bg: html,
          luminance: lum(body) !== null ? lum(body) : lum(html)};
}
"""


def panel_key():
    for line in ENV_FILE.read_text().splitlines():
        if line.startswith('CPAMP_ADMIN_KEY='):
            return line.split('=', 1)[1].strip().strip('"\'')
    raise SystemExit('CPAMP_ADMIN_KEY not found in ' + str(ENV_FILE))


def png_size(path):
    """Read a PNG's pixel size without Pillow (IHDR is at a fixed offset)."""
    with open(path, 'rb') as fh:
        head = fh.read(24)
    return struct.unpack('>II', head[16:24])


def has_cjk(text):
    return ''.join(ch for ch in text if any(lo <= ord(ch) <= hi for lo, hi in CJK))


def shrink(path, width):
    """Downscale a retina capture so the repository stays small."""
    from PIL import Image
    with Image.open(path) as im:
        if im.width <= width:
            return im.width
        im.resize((width, round(im.height * width / im.width)),
                  Image.LANCZOS).save(path, optimize=True)
    return width


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    report = {'panel_url': PANEL, 'page_url': PAGE_URL, 'language': LANG,
              'viewport': [VIEW_W, VIEW_H], 'palettes': {}, 'shots': [],
              'problems': []}

    with tempfile.TemporaryDirectory(prefix='wk-shots-') as profile:
        with sync_playwright() as pw:
            ctx = pw.chromium.launch_persistent_context(
                profile, channel='chrome', headless=False, color_scheme='light',
                viewport={'width': VIEW_W, 'height': VIEW_H}, device_scale_factor=2,
                args=['--no-sandbox', '--disable-dev-shm-usage'])
            page = ctx.pages[0]

            # Log in once on the panel origin: the standalone page reuses that session and,
            # being same-origin, still reads the panel's language and theme keys.
            page.goto(PANEL, wait_until='domcontentloaded')
            page.locator('input[type=password]').first.wait_for(timeout=30000)
            page.locator('input[type=password]').first.fill(panel_key())
            label = page.locator('label').filter(has=page.locator('input[type=checkbox]')).first
            if label.count():
                label.click()
            page.get_by_role('button', name='Login', exact=True).click()
            nav = page.get_by_text(MENU, exact=True).first
            nav.wait_for(timeout=30000)
            report['panel_stored'] = page.evaluate(
                "() => Object.fromEntries(Object.entries(localStorage)"
                " .filter(([k]) => k.includes('theme') || k.includes('language')))")
            page.evaluate(
                "c => localStorage.setItem('cli-proxy-language',"
                " JSON.stringify({state:{language:c},version:0}))", LANG)
            href = nav.evaluate(
                "el => { const a = el.closest('a'); return a ? a.getAttribute('href') : null }")
            panel_page = urljoin(page.url, href) if href else PANEL
            report['panel_page'] = panel_page

            key = panel_key()
            ids = page.evaluate(
                "async (key) => { const r = await fetch('/v0/management/cpa-window-keeper/status',"
                " {headers: {Authorization: 'Bearer ' + key}}); const d = await r.json();"
                " return Object.keys(d.state.accounts || {}); }", key)
            report['live_accounts'] = len(ids)
            if not ids:
                report['problems'].append('no live accounts to photograph')

            # Phase 1 — what the panel itself renders the page with, per scheme.
            for scheme in SCHEMES:
                page.emulate_media(color_scheme=scheme)
                page.goto(panel_page, wait_until='domcontentloaded')
                page.frame_locator(IFRAME_SEL).locator('#accounts h3').first.wait_for(
                    timeout=30000)
                page.wait_for_timeout(800)
                palette = page.evaluate(READ_PALETTE_JS)
                if not palette or not palette['vars']:
                    report['problems'].append(
                        '%s: the panel injected no palette into the sidebar iframe' % scheme)
                    continue
                palette['bg'] = palette['vars'].get('--app-bg')
                report['palettes'][scheme] = palette
                if not palette['bg']:
                    report['problems'].append('%s: the panel injected no --app-bg' % scheme)
            light_bg = report['palettes'].get('light', {}).get('bg')
            dark_bg = report['palettes'].get('dark', {}).get('bg')
            if light_bg and dark_bg and light_bg == dark_bg:
                report['problems'].append(
                    'the panel palette did not switch with the scheme (%s)' % light_bg)

            # Phase 2 — shoot the standalone page with that palette applied.
            retries = {}
            for scheme in SCHEMES:
                palette = report['palettes'].get(scheme)
                if not palette:
                    continue
                expected_theme = palette.get('theme')
                for attempt in range(1, 4):
                    page.emulate_media(color_scheme=scheme)
                    page.goto(PAGE_URL, wait_until='domcontentloaded')
                    page.locator('#accounts h3').first.wait_for(timeout=30000)
                    page.evaluate(APPLY_PALETTE_JS, palette)
                    page.wait_for_timeout(1500)

                    n0 = len(report['problems'])
                    masked = page.evaluate(MASK_JS)
                    audit = page.evaluate(AUDIT_JS, ids)
                    theme = page.evaluate(THEME_JS)
                    layout = page.evaluate(LAYOUT_JS)
                    shot = {'file': 'docs/dashboard-%s.png' % scheme, 'scheme': scheme,
                            'page_theme': theme['theme'], 'panel_theme': expected_theme,
                            'panel_bg': palette.get('bg'), 'body_bg': theme['body_bg'],
                            'luminance': theme['luminance'], 'masked_nodes': masked['hits'],
                            'cards': layout['cards'], 'lang_attr': masked['lang'],
                            'attempt': attempt, 'content_height': layout['content'],
                            'viewport_height': layout['viewport'],
                            'last_card_bottom': layout['last_card_bottom']}
                    if expected_theme and theme['theme'] != expected_theme:
                        report['problems'].append(
                            '%s shot: data-theme is %r, the panel uses %r'
                            % (scheme, theme['theme'], expected_theme))
                    if theme['luminance'] is None:
                        report['problems'].append('%s shot: no background colour' % scheme)
                    elif scheme == 'light' and theme['luminance'] < 180:
                        report['problems'].append(
                            '%s shot: the light theme did not render (background luminance %d)'
                            % (scheme, theme['luminance']))
                    elif scheme == 'dark' and theme['luminance'] > 90:
                        report['problems'].append(
                            '%s shot: the dark theme did not render (background luminance %d)'
                            % (scheme, theme['luminance']))
                    if masked['hits'] == 0:
                        report['problems'].append('%s shot: nothing was masked' % scheme)
                    if audit['bad']:
                        report['problems'].append('%s shot: identifier still visible %s'
                                                  % (scheme, audit['bad']))
                    if audit['mails']:
                        report['problems'].append('%s shot: e-mail still visible %s'
                                                  % (scheme, audit['mails']))
                    if layout['cards'] < 1:
                        report['problems'].append('%s shot: no account card rendered' % scheme)
                    if LANG == 'en' and has_cjk(masked['chrome']):
                        report['problems'].append('%s shot: English chrome contains CJK %r'
                                                  % (scheme, has_cjk(masked['chrome'])))
                    if len(report['problems']) > n0:
                        shot['written'] = False
                        shot['reason'] = report['problems'][n0]
                        report['shots'].append(shot)
                        break

                    # The whole page is published: capture the viewport when the dashboard
                    # fits in it, otherwise the full scrollable page.
                    needs_full = layout['content'] > layout['viewport'] + 4
                    path = OUT / ('dashboard-%s.png' % scheme)
                    page.screenshot(path=str(path), full_page=needs_full)
                    w_px, h_px = png_size(path)
                    shot['capture'] = 'page_full' if needs_full else 'page_viewport'
                    shot['image_before_shrink'] = [w_px, h_px]
                    # The README must show a desktop page, not a phone strip.
                    if w_px != VIEW_W * 2:
                        report['problems'].append(
                            '%s shot: captured %dpx wide, expected %dpx'
                            % (scheme, w_px, VIEW_W * 2))
                    # Never publish a shot that cuts the dashboard off.
                    if h_px < layout['content'] * 2 - 8 or \
                            (layout['last_card_bottom'] or 0) > h_px / 2 + 4:
                        path.unlink(missing_ok=True)
                        report['problems'].append(
                            '%s shot: image is %dx%d but the page needs %dpx '
                            '(last card bottom %s)'
                            % (scheme, w_px, h_px, layout['content'] * 2,
                               layout['last_card_bottom']))
                        shot['written'] = False
                        report['shots'].append(shot)
                        break
                    # The dashboard polls every 30 s and re-renders unmasked cards. Re-audit
                    # the very DOM the shot came from: a re-render in that window lands here.
                    after = page.evaluate(AUDIT_JS, ids)
                    if after['bad'] or after['mails']:
                        path.unlink(missing_ok=True)
                        retries[scheme] = retries.get(scheme, 0) + 1
                        if retries[scheme] < 3:
                            continue
                        report['problems'].append(
                            '%s shot: a re-render restored identifiers (%d attempts)'
                            % (scheme, retries[scheme]))
                        shot['written'] = False
                        report['shots'].append(shot)
                        break
                    shot['written'] = True
                    shot['width'] = shrink(path, SHOT_WIDTH)
                    shot['image'] = list(png_size(path))
                    shot['bytes'] = path.stat().st_size
                    report['shots'].append(shot)
                    break

            ctx.close()

    print(json.dumps(report, ensure_ascii=False, indent=2))
    if report['problems']:
        print('PROBLEMS FOUND — nothing written for the affected shots', file=sys.stderr)
        return 1
    print('ALL SHOTS MASKED AND CAPTURED')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())

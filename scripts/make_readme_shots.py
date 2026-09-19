#!/usr/bin/env python3
"""Capture the README screenshots (light + dark) through the real CPAMP sidebar.

Account identifiers are replaced in the DOM *before* the shot is taken, and the
masking is asserted: the script refuses to write an image while a live auth index
(or a fragment of one), an e-mail or a raw token is still visible. It also refuses
to write an English shot that still contains CJK chrome.

    CPAMP_PANEL_URL=https://<your-panel-host>/management.html \
        /opt/browser-automation/run.sh scripts/make_readme_shots.py

Env overrides: SHOT_DIR (default docs), SHOT_WIDTH, SHOT_LANG (default en),
CPAMP_ENV_FILE, CPA_WINDOW_KEEPER_MENU.
"""
import json
import os
import pathlib
import re
import struct
import sys
import tempfile
import urllib.parse

from patchright.sync_api import sync_playwright

PANEL = os.environ.get('CPAMP_PANEL_URL', 'http://127.0.0.1:18317/management.html')
ENV_FILE = pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env'))
OUT = pathlib.Path(os.environ.get('SHOT_DIR', 'docs'))
SHOT_WIDTH = int(os.environ.get('SHOT_WIDTH', '860'))
LANG = os.environ.get('SHOT_LANG', 'en')
MENU = os.environ.get('CPA_WINDOW_KEEPER_MENU', '5 小时自动开窗')
SCHEMES = ('light', 'dark')
# A dashboard with a few accounts is taller than a phone viewport; the README shots are full-page
# captures because a cropped card reads as a broken layout.
FULL_PAGE = os.environ.get('SHOT_FULL_PAGE', '1').lower() not in ('0', 'false', 'no')
# The plugin page lives in an iframe inside the panel; this is the element in the parent DOM.
IFRAME_SEL = "iframe[src*='cpa-window-keeper']"
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
# Note: Locator.evaluate passes the matched element first and `arg` second.
AUDIT_JS = r"""
(el, ids) => {
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
  const hint = document.querySelector('.hint');
  return {
    content: document.documentElement.scrollHeight,
    viewport: window.innerHeight,
    last_card_bottom: last ? Math.round(last.getBoundingClientRect().bottom) : null,
    hint_bottom: hint ? Math.round(hint.getBoundingClientRect().bottom) : null,
  };
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
    """Downscale a retina capture so the repository stays small (PIL optional)."""
    try:
        from PIL import Image
    except ImportError:
        return None
    with Image.open(path) as im:
        if im.width <= width:
            return im.width
        im.resize((width, round(im.height * width / im.width)),
                  Image.LANCZOS).save(path, optimize=True)
    return width


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    report = {'panel_url': PANEL, 'language': LANG, 'shots': [], 'problems': []}

    with tempfile.TemporaryDirectory(prefix='wk-shots-') as profile:
        with sync_playwright() as pw:
            ctx = pw.chromium.launch_persistent_context(
                profile, channel='chrome', headless=False, color_scheme='light',
                viewport={'width': 430, 'height': 932}, device_scale_factor=2,
                args=['--no-sandbox', '--disable-dev-shm-usage'])
            page = ctx.pages[0]
            page.goto(PANEL, wait_until='domcontentloaded')
            page.locator('input[type=password]').first.wait_for(timeout=30000)
            page.locator('input[type=password]').first.fill(panel_key())
            label = page.locator('label').filter(has=page.locator('input[type=checkbox]')).first
            if label.count():
                label.click()
            page.get_by_role('button', name='Login', exact=True).click()
            page.get_by_text(MENU, exact=True).wait_for(timeout=30000)
            report['panel_theme_stored'] = page.evaluate(
                "() => Object.fromEntries(Object.entries(localStorage)"
                " .filter(([k]) => k.includes('theme') || k.includes('language')))")
            page.evaluate(
                "c => localStorage.setItem('cli-proxy-language',"
                " JSON.stringify({state:{language:c},version:0}))", LANG)

            key = panel_key()
            ids = page.evaluate(
                "async (key) => { const r = await fetch('/v0/management/cpa-window-keeper/status',"
                " {headers: {Authorization: 'Bearer ' + key}}); const d = await r.json();"
                " return Object.keys(d.state.accounts || {}); }", key)
            report['live_accounts'] = len(ids)
            if not ids:
                report['problems'].append('no live accounts to photograph')

            retries = {}
            for scheme in SCHEMES:
                for attempt in range(1, 4):
                    page.emulate_media(color_scheme=scheme)
                    page.goto(PANEL, wait_until='domcontentloaded')
                    nav = page.get_by_text(MENU, exact=True).first
                    nav.wait_for(timeout=30000)
                    # Phone-width: the sidebar drawer is off-screen, follow the route directly.
                    href = nav.evaluate(
                        "el => { const a = el.closest('a'); return a ? a.getAttribute('href') : null }")
                    if href:
                        page.goto(urllib.parse.urljoin(page.url, href), wait_until='domcontentloaded')
                    else:
                        nav.click()
                    iframe_el = page.locator(IFRAME_SEL).first
                    frame = page.frame_locator(IFRAME_SEL)
                    frame.locator('#accounts h3').first.wait_for(timeout=30000)
                    page.wait_for_timeout(1500)

                    n0 = len(report['problems'])
                    masked = frame.locator('body').evaluate(MASK_JS)
                    audit = frame.locator('body').evaluate(AUDIT_JS, ids)
                    theme = frame.locator('body').evaluate(
                        "() => document.documentElement.getAttribute('data-theme')")
                    layout = frame.locator('body').evaluate(LAYOUT_JS)
                    shot = {'file': 'docs/dashboard-%s.png' % scheme, 'scheme': scheme,
                            'iframe_theme': theme, 'masked_nodes': masked['hits'],
                            'cards': frame.locator('#accounts h3').count(),
                            'lang_attr': masked['lang'], 'attempt': attempt,
                            'full_page': FULL_PAGE, 'content_height': layout['content'],
                            'viewport_height': layout['viewport'],
                            'last_card_bottom': layout['last_card_bottom']}
                    # The panel injects data-theme as "white" | "dark".
                    if theme not in {'light': ('white', 'light'), 'dark': ('dark',)}[scheme]:
                        report['problems'].append('%s shot: iframe theme is %r' % (scheme, theme))
                    if masked['hits'] == 0:
                        report['problems'].append('%s shot: nothing was masked' % scheme)
                    if audit['bad']:
                        report['problems'].append('%s shot: identifier still visible %s'
                                                  % (scheme, audit['bad']))
                    if audit['mails']:
                        report['problems'].append('%s shot: e-mail still visible %s'
                                                  % (scheme, audit['mails']))
                    if LANG == 'en' and has_cjk(masked['chrome']):
                        report['problems'].append('%s shot: English chrome contains CJK %r'
                                                  % (scheme, has_cjk(masked['chrome'])))
                    if len(report['problems']) > n0:
                        shot['written'] = False
                        shot['reason'] = report['problems'][n0]
                        report['shots'].append(shot)
                        break

                    # The dashboard scrolls *inside* a fixed-height panel iframe, so a plain page
                    # screenshot only shows the first screenful and cuts the last card. Grow the
                    # iframe to its content height for the shot; this browser profile is throwaway
                    # and nothing here is written back to the panel.
                    fit = layout['content'] + 32
                    GROW_JS = ("(el, h) => { el.style.height = h + 'px';"
                               " el.style.minHeight = h + 'px'; el.style.maxHeight = 'none'; }")
                    iframe_el.evaluate(GROW_JS, fit)
                    page.wait_for_timeout(600)
                    grown = frame.locator('body').evaluate(LAYOUT_JS)
                    if grown['content'] > fit:  # 100vh-based layout grew with the iframe
                        fit = grown['content'] + 32
                        iframe_el.evaluate(GROW_JS, fit)
                        page.wait_for_timeout(400)
                        grown = frame.locator('body').evaluate(LAYOUT_JS)
                    shot['content_height'] = grown['content']
                    shot['iframe_fitted_to'] = fit

                    path = OUT / ('dashboard-%s.png' % scheme)
                    page.screenshot(path=str(path), full_page=FULL_PAGE)
                    shot['capture'] = 'page'
                    w_px, h_px = png_size(path)
                    if h_px < grown['content'] * 2 - 8:
                        # The panel may clip its iframe container; the frame element itself is now
                        # tall enough, so capture that instead.
                        iframe_el.screenshot(path=str(path))
                        shot['capture'] = 'frame_element'
                        w_px, h_px = png_size(path)
                    shot['image'] = [w_px, h_px]
                    # Never publish a shot that cuts the dashboard off.
                    if h_px < grown['content'] * 2 - 8 or w_px < 400:
                        path.unlink(missing_ok=True)
                        report['problems'].append(
                            '%s shot: image is %dx%d but the dashboard needs %dx%d'
                            % (scheme, w_px, h_px, 860, grown['content'] * 2))
                        shot['written'] = False
                        report['shots'].append(shot)
                        break
                    # The dashboard polls every 30 s and re-renders unmasked cards. Re-audit
                    # the very DOM the shot came from: a re-render in that window lands here.
                    after = frame.locator('body').evaluate(AUDIT_JS, ids)
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

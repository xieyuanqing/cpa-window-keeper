#!/usr/bin/env python3
"""Assert the dashboard follows the panel language, on the real panel.

Three things are verified against the live CPAMP panel, not a fixture:

1. panellanguage -> dashboard language. The panel stores its choice in
   `cli-proxy-language`; the plugin iframe must boot in that language (English
   chrome must be free of CJK, Chinese chrome must actually be Chinese).
2. live follow. Changing the panel language while the dashboard is open must
   re-render it through the `storage` event, without a page reload.
3. manual override. The toolbar button switches the current view immediately and
   does not touch the panel's stored choice.

    CPAMP_PANEL_URL=https://<your-panel-host>/management.html \
        /opt/browser-automation/run.sh scripts/verify_i18n.py
"""
import json
import os
import pathlib
import re
import sys
import tempfile

from patchright.sync_api import sync_playwright

PANEL = os.environ.get('CPAMP_PANEL_URL', 'http://127.0.0.1:18317/management.html')
ENV_FILE = pathlib.Path(os.environ.get('CPAMP_ENV_FILE', '/opt/cpa-manager-plus/.env'))
MENU = os.environ.get('CPA_WINDOW_KEEPER_MENU', '5 小时自动开窗')
CJK = [(0x4E00, 0x9FFF), (0x3400, 0x4DBF), (0xF900, 0xFAFF)]
# Console noise produced by the panel's own Cloudflare/CSP setup, not by the plugin.
IGNORE_CONSOLE = ('Content Security Policy', 'cloudflare', 'rocket-loader')

EN_TITLE = '5-Hour Window Keeper'
ZH_TITLE = '5 小时自动开窗'
EN_SESSION = ('Panel login state reused', 'Connected · key held in this page only',
              'Reading the panel login state…', 'Key required', 'Refresh stopped')
EN_BUTTONS = ('Check schedule', 'Reload panel login state', 'Enter key manually')
# Every status label the dashboard can show, in English. A tag outside this set means a
# status key leaked through untranslated.
EN_TAGS = ('Account disabled · skipped', 'CPA cooldown', 'Quota check failed · backing off',
           'Not a 5-hour quota · skipped', 'Quota blocked · waiting for reset',
           'Counting down', 'Window start verified',
           'Window not confirmed · dedupe cooldown', 'Confirming idle state',
           'Idle detected · dry run, not sending', 'Send reserved (persisted)',
           'Request sent · awaiting quota readback',
           'Request failed · awaiting readback, no retry yet')


def panel_key():
    for line in ENV_FILE.read_text().splitlines():
        if line.startswith('CPAMP_ADMIN_KEY='):
            return line.split('=', 1)[1].strip().strip('"\'')
    raise SystemExit('CPAMP_ADMIN_KEY not found in ' + str(ENV_FILE))


def set_lang(page, code):
    """Write the panel's own language key from the panel document."""
    page.evaluate(
        "c => localStorage.setItem('cli-proxy-language',"
        " JSON.stringify({state:{language:c},version:0}))", code)


def has_cjk(text):
    return ''.join(ch for ch in text if any(lo <= ord(ch) <= hi for lo, hi in CJK))


PLACEHOLDER = re.compile(r'\{\d+\}')


def placeholder_leaks(snap):
    """Any {0}-style placeholder still on screen means a substitution was missed."""
    fields = [snap['title'], snap['sub'], snap['session'], snap['summary']]
    fields += [b for b in snap['buttons'] if b] + snap['tags'] + snap['labs'] + snap['caps']
    return [f for f in fields if f and PLACEHOLDER.search(f)]


def snapshot(frame):
    return frame.locator('body').evaluate("""() => ({
      lang: document.documentElement.getAttribute('lang'),
      title: document.getElementById('h1').innerText,
      sub: document.getElementById('sub').innerText,
      session: document.getElementById('session').innerText,
      summary: document.getElementById('summary').innerText,
      buttons: ['check', 'reconnect', 'manualToggle', 'lang'].map(i =>
        (document.getElementById(i) || {}).innerText || null),
      tags: [...document.querySelectorAll('.acct .tag')].map(e => e.innerText),
      labs: [...document.querySelectorAll('.acct .rows dt')].map(e => e.innerText),
      caps: [...document.querySelectorAll('.acct .meter-cap span')].map(e => e.innerText),
      cards: document.querySelectorAll('.acct').length,
      toggle: document.getElementById('lang').innerText,
      // The language toggle intentionally shows the *other* language's name, so the
      // "English chrome must be free of CJK" check has to skip it.
      chrome: document.body.innerText.split(document.getElementById('lang').innerText).join(''),
      text: document.body.innerText,
      mark: document.documentElement.dataset.i18nMark || null,
    })""")


def main():
    env = {}
    for line in ENV_FILE.read_text().splitlines():
        if '=' in line and not line.lstrip().startswith('#'):
            k, v = line.split('=', 1)
            env[k.strip()] = v.strip().strip('"\'')
    problems, report = [], {}

    with tempfile.TemporaryDirectory(prefix='wk-i18n-') as profile:
        with sync_playwright() as pw:
            ctx = pw.chromium.launch_persistent_context(
                profile, channel='chrome', headless=False,
                viewport={'width': 1280, 'height': 900},
                args=['--no-sandbox', '--disable-dev-shm-usage'])
            page = ctx.pages[0]
            console = []
            page.on('console', lambda m: console.append(m.type + ': ' + m.text[:200])
                    if m.type in ('error', 'warning') else None)
            page.on('pageerror', lambda e: console.append('pageerror: ' + str(e)[:200]))

            page.goto(PANEL, wait_until='domcontentloaded')
            page.locator('input[type=password]').first.wait_for(timeout=30000)
            page.locator('input[type=password]').first.fill(env['CPAMP_ADMIN_KEY'])
            label = page.locator('label').filter(has=page.locator('input[type=checkbox]')).first
            if label.count():
                label.click()
            page.get_by_role('button', name='Login', exact=True).click()
            page.get_by_text(MENU, exact=True).wait_for(timeout=30000)
            report['panel_key_before'] = page.evaluate(
                "() => localStorage.getItem('cli-proxy-language')")

            # --- 1. panel language -> dashboard language (English) -------------
            set_lang(page, 'en')
            page.get_by_text(MENU, exact=True).first.click()
            frame = page.frame_locator('iframe')
            frame.locator('#summary').wait_for(timeout=30000)
            frame.locator('#accounts h3').first.wait_for(timeout=30000)
            page.wait_for_timeout(800)
            en = snapshot(frame)
            report['en'] = {k: v for k, v in en.items() if k != 'text'}
            if en['title'] != EN_TITLE:
                problems.append('EN title is %r' % en['title'])
            if en['lang'] != 'en':
                problems.append('EN <html lang> is %r' % en['lang'])
            if has_cjk(en['chrome']):
                problems.append('EN view still has CJK: ' + has_cjk(en['chrome']))
            if en['session'] not in EN_SESSION:
                problems.append('EN session badge is %r' % en['session'])
            if en['buttons'][:3] != list(EN_BUTTONS):
                problems.append('EN toolbar is %r' % (en['buttons'][:3],))
            if en['buttons'][3] != '中文':
                problems.append('EN language toggle shows %r' % en['buttons'][3])
            if not en['summary'].startswith(('Mode:', 'Not running:')):
                problems.append('EN summary is %r' % en['summary'][:80])
            if '5-hour used' not in en['caps']:
                problems.append('EN meter cap missing, got %r' % (en['caps'],))
            if [t for t in en['tags'] if t not in EN_TAGS]:
                problems.append('EN account tags are %r' % (en['tags'],))

            # Mark the frame document so a reload is detectable below.
            frame.locator('body').evaluate(
                "() => { document.documentElement.dataset.i18nMark = 'kept'; }")

            # --- 2. the panel switching language must re-render it live --------
            set_lang(page, 'zh-CN')
            try:
                page.wait_for_function(
                    "() => { const f = document.querySelector('iframe');"
                    " return f && f.contentDocument &&"
                    " f.contentDocument.getElementById('h1').innerText === '5 小时自动开窗'; }",
                    timeout=10000)
            except Exception:
                problems.append('dashboard did not follow the panel switch to zh-CN')
            page.wait_for_timeout(500)
            zh = snapshot(frame)
            report['zh'] = {k: v for k, v in zh.items() if k != 'text'}
            if zh['title'] != ZH_TITLE:
                problems.append('zh title is %r' % zh['title'])
            if zh['lang'] != 'zh-CN':
                problems.append('zh <html lang> is %r' % zh['lang'])
            if not has_cjk(zh['text']):
                problems.append('zh view has no CJK at all')
            if zh['buttons'][3] != 'EN':
                problems.append('zh language toggle shows %r' % zh['buttons'][3])
            if zh['mark'] != 'kept':
                problems.append('the iframe reloaded instead of following the storage event')
            if zh['cards'] != en['cards']:
                problems.append('card count changed across the language switch: %s -> %s'
                                % (en['cards'], zh['cards']))
            if '5 小时已用' not in zh['caps']:
                problems.append('zh meter cap missing, got %r' % (zh['caps'],))

            # --- 3. toolbar override ------------------------------------------
            def stored_language():
                raw = page.evaluate("() => localStorage.getItem('cli-proxy-language')")
                return (json.loads(raw) or {}).get('state', {}).get('language')

            stored_before = stored_language()
            frame.locator('#lang').click()
            page.wait_for_timeout(400)
            override = snapshot(frame)
            report['override'] = {k: v for k, v in override.items() if k != 'text'}
            if override['title'] != EN_TITLE:
                problems.append('toolbar override did not switch to English: %r'
                                % override['title'])
            stored_after = stored_language()
            report['panel_language'] = {'before_override': stored_before,
                                        'after_override': stored_after}
            # The in-page toggle is view-only: it must not write the panel's own choice.
            if stored_after != stored_before:
                problems.append('the toolbar override rewrote the panel language: %r -> %r'
                                % (stored_before, stored_after))
            if stored_after != 'zh-CN':
                problems.append('panel language is %r, expected the zh-CN set by this test'
                                % (stored_after,))

            for name, snap in (('en', en), ('zh', zh), ('override', override)):
                leaks = placeholder_leaks(snap)
                if leaks:
                    problems.append('%s view leaked {n} placeholders: %r' % (name, leaks))

            report['accounts'] = [{'tags': en['tags'], 'labels': en['labs'], 'caps': en['caps']}]
            noisy = [c for c in console if not any(p.lower() in c.lower() for p in IGNORE_CONSOLE)]
            report['console'] = {'total': len(console), 'relevant': noisy}
            if noisy:
                problems.append('console errors: ' + json.dumps(noisy[:5], ensure_ascii=False))
            ctx.close()

    report['problems'] = problems
    print(json.dumps(report, ensure_ascii=False, indent=2))
    if problems:
        print('FAILED', file=sys.stderr)
        return 1
    print('ALL ASSERTIONS PASSED')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())

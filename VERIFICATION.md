# Verification report — 2026-09-19

Artifact: `dist/cpa-window-keeper-v0.1.0.so`

SHA-256: `73d1cb0f797bddb8bf694e94cb0b041b8bc48b4353ccb8e4d33b02b34f8e59ea`

## Automated results

- 28 top-level Go tests and 68 subtests passed; no failed tests.
- Go race detector enabled; `go vet` passed.
- Native C shared-library build succeeded using `golang:1.26-bookworm`.
- Dashboard JavaScript syntax check (`node --check`) passed.
- Real compiled C ABI tested with a clearly synthetic host: registration, static resource, exact account routing, minimal payloads, idle grace, one send per account, quiesce, reconfigure persistence and worker-joined shutdown all passed.
- Five-hour progression is covered with a mock clock, not a claim of having waited through repeated live cycles.

## Real isolated CPA results

Same image/version as production: v7.3.4 / commit 8335eac. The final binary was loaded in a fresh isolated container and its mounted SHA-256 matched the artifact above.

- Plugin registered and enabled in the sandbox.
- Public dashboard: HTTP 200.
- Unauthenticated status API: HTTP 401.
- Authenticated state: HTTP 200; real Codex and Claude quota windows detected as `counting_down`.
- Dry-run automatic send attempts: 0 for both accounts.
- Native disable: passed; disabled status endpoint HTTP 404.
- Native re-enable: passed; persistent state read back successfully.

Two **explicit protocol smoke requests**, separate from automatic scheduling, were made against the real providers through the isolated CPA instance:

- Codex `gpt-5.6-luna`, `low`: HTTP 200, input 305, output 5, reasoning 0.
- Claude `claude-haiku-4-5-20251001`, thinking disabled, max_tokens 1: HTTP 200, input 100, output 1.

Real quota readback after the protocol checks showed both providers counting down. This does not claim a witnessed idle-to-active Claude transition, or that every window shift was exclusively caused by the smoke request while other clients were active.

## Panel login-state reuse and hot upgrade — 2026-09-19 (v0.1.2)

Artifact: `dist/cpa-window-keeper-v0.1.2.so`, SHA-256 `3ecc9ec626300e04c0e900480269d8aa0e628c8fe008c94c4904b17c54117d28`.

- Upstream CPAMP commit `6d0e9bd5340eda3f73db3fc6e5f29056ff3cc4fe` was read to establish the credential path: `PluginResourcePage.tsx` embeds the resource in a same-origin iframe with no `sandbox` attribute and no token in the URL; `STORAGE_KEY_AUTH = 'cli-proxy-auth'`; `encryption.ts` (obfuscation scheme) and `secureStorage.ts` (zustand persistence) define the readable state. The deployed bundle was searched for a credential bridge: **0** `cpamp:*` matches, so no `postMessage` path exists in production.
- The served resource contained no `*-text/javascript` rewriting after the fix and preserved `data-cfasync="false"`.
- Hot upgrade via `scripts/upgrade_production.py`: `0.1.1 → 0.1.2`, config continuity asserted key by key, `dry_run: false` preserved, 3 accounts preserved, previous artifact retained for rollback.

Real-browser check (`scripts/browser_panel_verify.py`, Chrome through the actual CPAMP sidebar over the panel's public host):

- Panel login with 「记住密码」: panel auth persisted; badge 已沿用面板登录态; key prompt hidden; 3 account cards rendered; 「检查调度」 accepted → no repeated credential entry.
- Panel login without 「记住密码」: key prompt is the only fallback and states why. This is a CPAMP constraint, not a plugin regression.

## Production installation

Installed by `scripts/install_production.py` (v0.1.0/v0.1.1) and updated in place by `scripts/upgrade_production.py`; the plugin is enabled in production. Superseded text below records the pre-installation scope from the first verification pass.

Production plugin API was read back after testing: `cpa-quota-estimator` enabled; `quota-center` disabled; `cpa-window-keeper` not installed/enabled. Production CPA config and container were not modified/restarted. Installation and automatic generation remain pending explicit approval.

Private sandbox access-token snapshots were kept outside this project and no refresh tokens were copied. Release packaging uses an explicit file allowlist and checks the archive contents against current live credential values before delivery.

## Dashboard redesign and repository publication — 2026-09-19 (v0.1.3 / v0.1.4)

Artifact: `dist/cpa-window-keeper-v0.1.3.so`, SHA-256 `e530c7ce062c8336f6d2f5a59b6324f0b21df037c3311d249bb1f32507ddf100`.

- Dashboard rebuilt as a glass/IOS-style layout: `backdrop-filter: blur(28px) saturate(180%)`, 24px radii, ambient gradient orbs behind the cards. It consumes the theme variables CPAMP injects into the plugin iframe (`--primary-color`, `--app-surface`, `--text-primary`, `--border-color`) and follows `data-theme="white" | "dark"`.
- Verified in a real Chrome through the CPAMP sidebar at a 430×932 phone viewport, in both light and dark OS schemes: 3 account cards rendered, computed `border-radius: 24px`, computed `backdrop-filter: blur(28px) saturate(1.8)`, translucent card background, resolved accent `#3b82f6` (light) / `#60a5fa` (dark), no horizontal overflow.
- `scripts/browser_screenshot_panel.py` captures the embedded panel; on phone-width viewports the CPAMP sidebar is a slide-over drawer, so the script resolves the nav link's `href` and navigates directly instead of clicking.
- v0.1.4 changes metadata only: `GitHubRepository` moved from the `local://` marker to the published repository URL. Config continuity, account set and `dry_run: false` were preserved by the hot upgrade.

## Panel storage format `enc::v2::` support — 2026-09-19 (v0.1.5)

Artifact: `dist/cpa-window-keeper-v0.1.5.so`, SHA-256 `0de1b8d45acb61497a70c3b54b842fea1b6bd334c4a785ecc10ed1586cd66139`.

The dashboard decoded only `enc::v1::` panel storage. Current CPAMP builds write `cli-proxy-auth` as `enc::v2::`, whose XOR key no longer includes the user agent (`cli-proxy-api-webui::secure-storage|v2|<host>` instead of `...|secure-storage|<host>|<user-agent>`). With v1-only decoding the payload failed to parse, so the page silently degraded to 「需要密钥」 and asked for the management key by hand.

- `decodePanelStorage` now accepts both prefixes (both are 9 characters, so the payload slice is shared) and picks the salt by prefix: `|v2|` + host for `enc::v2::`, host + user agent for `enc::v1::`.
- The legacy `managementKey` fallback also accepts the object form (`{managementKey: ...}`) in addition to the bare string.
- Verified in a real Chrome against the production panel (`CPAMP_PANEL_URL=https://cliproxy.nijikit.com/management.html`) via `scripts/browser_panel_verify.py`:
  - panel login **with** 「记住密码」 → `password_visible: false`, badge `已沿用面板登录态`, 3 account cards, `refresh: queue accepted` (auto-window scheduling reached the plugin).
  - panel login **without** 「记住密码」 → `password_visible: true`, badge `需要密钥` — unchanged, still the documented CPAMP limitation.
- Upgrade: `python3 scripts/upgrade_production.py --so dist/cpa-window-keeper-v0.1.5.so --expect-version 0.1.5` → `upgraded: true` 0.1.4→0.1.5, `config_preserved: true`, `dry_run: false`, 3 accounts. Hot swap, no CPA restart.


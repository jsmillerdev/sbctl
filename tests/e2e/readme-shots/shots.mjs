// Captures the README screenshots of Supabase Studio (platform mode) running on the node that
// node.sh installed and seed.mjs filled. 1640x1025 at device scale factor 2, dark theme (THEMES=dark,light adds the light one).
//
//   SHOTS_DIR=/tmp/shots CHROME=/usr/bin/google-chrome node shots.mjs
//     [THEMES=dark,light] [ONLY=projects,table-editor] [OUT=/tmp/shots/raw]
//
// A shot is written to $OUT only when its page shows the expected content, no error toast, no spinner
// and no open dialog; otherwise the attempt goes to $SHOTS_DIR/debug with the page text, so a failed run
// shows why. Studio's theme is the `theme` key in localStorage (next-themes). Nothing is edited after
// capture: optimize.mjs only resizes and compresses.
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { chromium } from 'playwright-core'

const DIR = process.env.SHOTS_DIR ?? '/tmp/shots'
const E = JSON.parse(readFileSync(join(DIR, 'env.json'), 'utf8'))
const S = JSON.parse(readFileSync(join(DIR, 'seed.json'), 'utf8'))
const OUT = process.env.OUT ?? join(DIR, 'raw')
const DEBUG = join(DIR, 'debug')
const THEMES = (process.env.THEMES ?? 'dark').split(',')
const ONLY = process.env.ONLY ? process.env.ONLY.split(',') : null
for (const d of [OUT, DEBUG]) mkdirSync(d, { recursive: true })

const VIEW_W = 1640
const VIEW_H = 1025
const STUDIO = E.studioUrl
const store = S.projects.storefront
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a)

// ---- the shots ---------------------------------------------------------------------------
// expect: texts that must be visible. prepare: what a person does after the page loads.
const SHOTS = [
  { name: 'projects', path: `/org/${S.org}`, expect: ['storefront', 'analytics', 'agent-sandbox', 'support-desk', 'internal-tools'], clip: { x: 0, y: 0, width: VIEW_W, height: 660 } },
  {
    name: 'table-editor', path: `/project/${store}/editor`, expect: ['Ceramic Pour-Over Set', 'Walnut Cutting Board', 'Carbon-Steel Chef Knife'],
    async prepare(page) {
      await page.getByText('products', { exact: true }).first().click()
    },
  },
]

// ---- browser ----------------------------------------------------------------------------
const browser = await chromium.launch({
  headless: true,
  ...(process.env.CHROME ? { executablePath: process.env.CHROME } : {}),
  // The node serves plain HTTP on a made-up domain: do not let Chrome try HTTPS first.
  args: [
    // The document is rewritten by route.fulfill below, which leaves it without a network address; Chrome then
    // treats the node's loopback sub-resources as private-network requests from a public page and blocks them
    // (ERR_FAILED, "blocked by CORS policy: The request client is not a secure context ..."). Switch those checks off.
    '--disable-features=HttpsUpgrades,HttpsFirstBalancedModeAutoEnable,BlockInsecurePrivateNetworkRequests,PrivateNetworkAccessSendPreflights,PrivateNetworkAccessRespectPreflightResults,PrivateNetworkAccessForNavigations,PrivateNetworkAccessForWorkers,LocalNetworkAccessChecks',
    `--unsafely-treat-insecure-origin-as-secure=${STUDIO},${E.apiUrl}`,
    '--font-render-hinting=none',
  ],
})

const TELEMETRY = /posthog|sentry|usercentrics|consent|google-analytics|googletagmanager|gravatar|intercom|hcaptcha|stripe|incident\.io|statsig|api\.supabase\.com|segment|datadog|vercel-insights|vercel\/insights/i
const results = []

async function newPage(theme) {
  const ctx = await browser.newContext({
    viewport: { width: VIEW_W, height: VIEW_H },
    deviceScaleFactor: 2,
    colorScheme: theme,
    locale: 'en-US',
    timezoneId: 'UTC',
  })
  await ctx.route(TELEMETRY, (r) => r.abort())
  // Studio's CSP carries upgrade-insecure-requests, which turns every sub-resource of this plain-HTTP
  // node into an https request that fails (net::ERR_SSL_PROTOCOL_ERROR). A node with TLS has no
  // such problem; here the browser is told to skip that one directive.
  await ctx.route(`${STUDIO}/**`, async (route) => {
    if (route.request().resourceType() !== 'document') return route.fallback()
    const res = await route.fetch()
    const headers = { ...res.headers() }
    for (const k of ['content-security-policy', 'content-security-policy-report-only']) {
      if (headers[k]) headers[k] = headers[k].split(';').filter((d) => d.trim() !== 'upgrade-insecure-requests').join(';')
    }
    await route.fulfill({ response: res, headers })
  })
  await ctx.addInitScript((t) => {
    try { localStorage.setItem('theme', t) } catch {}
    // Toasts: keep their text for the checks, never show them in a shot.
    window.__toasts = []
    const css = document.createElement('style')
    css.textContent = '[data-sonner-toaster],#usercentrics-root{display:none!important}'
    const seen = new WeakSet()
    const start = () => {
      document.head.append(css)
      new MutationObserver(() => {
        document.querySelectorAll('[data-sonner-toast]').forEach((n) => {
          if (!seen.has(n)) { seen.add(n); window.__toasts.push(n.textContent || '') }
        })
      }).observe(document.body, { childList: true, subtree: true })
    }
    document.readyState === 'loading' ? document.addEventListener('DOMContentLoaded', start) : start()
  }, theme)
  const page = await ctx.newPage()
  page.setDefaultTimeout(30_000)
  page.__bad = []
  page.__console = []
  page.__net = []
  page.on('requestfailed', (r) => page.__net.push(`FAILED ${r.method()} ${r.url().slice(0, 160)} ${r.failure()?.errorText}`))
  page.on('response', (r) => {
    page.__net.push(`${r.status()} ${r.request().method()} ${r.url().slice(0, 160)}`)
    if (r.request().resourceType() === 'document') page.__net.push(`  csp: ${(r.headers()['content-security-policy'] ?? '').slice(0, 1500)}`)
  })
  page.on('response', (r) => { if (r.status() >= 400 && !/\/(_next|api\/(incident-banner|get-utc-time))/.test(r.url())) page.__bad.push(`${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`) })
  page.on('console', (m) => m.type() === 'error' && page.__console.push(m.text().slice(0, 700)))
  return { ctx, page }
}

async function signIn(page) {
  await page.goto(`${STUDIO}/sign-in`, { waitUntil: 'domcontentloaded' })
  const email = page.getByPlaceholder('you@example.com')
  const password = page.locator('input[type="password"]').first()
  for (let i = 0; i < 10; i++) {
    await email.fill(E.adminEmail)
    await password.fill(E.adminPassword)
    await page.waitForTimeout(400)
    if ((await email.inputValue()) === E.adminEmail && (await password.inputValue()) === E.adminPassword) break
  }
  await page.getByRole('button', { name: /^sign in$/i }).first().click()
  await page.waitForURL((u) => !u.pathname.startsWith('/sign-in'), { timeout: 60_000 })
}

// Studio's announcement cards (privacy notice, feature previews) float at the bottom right. Close them
// the way a person does: the icon-only button inside the fixed-position card.
async function dismissPopups(page) {
  for (let i = 0; i < 4; i++) {
    const closed = await page.evaluate(() => {
      const cards = [...document.querySelectorAll('div')].filter((d) => {
        const st = getComputedStyle(d)
        const r = d.getBoundingClientRect()
        return st.position === 'fixed' && r.width > 200 && r.width < 460 && r.left > innerWidth / 2 && r.top > innerHeight / 2 && /Learn more|Enable in preferences|NOTICE|NEW/.test(d.innerText || '')
      })
      for (const c of cards) {
        const b = [...c.querySelectorAll('button')].find((x) => !(x.innerText || '').trim())
        if (b) { b.click(); return true }
      }
      return false
    })
    if (!closed) break
    await page.waitForTimeout(600)
  }
}

const BUSY = '.animate-spin, [class*="shimmer"], [class*="skeleton"], [aria-busy="true"]'
async function settle(page) {
  await page.waitForLoadState('networkidle', { timeout: 10_000 }).catch(() => {})
  await page.waitForFunction((sel) => !document.querySelector(sel), BUSY, { timeout: 30_000 }).catch(() => {})
  await page.waitForTimeout(1500)
  await page.waitForFunction((sel) => !document.querySelector(sel), BUSY, { timeout: 15_000 }).catch(() => {})
}

async function capture(page, shot, theme) {
  const tag = `${shot.name}-${theme}`
  page.__bad.length = 0
  page.__console.length = 0
  page.__net.length = 0
  const problems = []
  try {
    await page.goto(`${STUDIO}${shot.path}`, { waitUntil: 'domcontentloaded' })
    await settle(page)
    if (shot.prepare) await shot.prepare(page)
    for (const t of shot.expect ?? []) {
      await page.getByText(t).first().waitFor({ state: 'visible', timeout: 30_000 }).catch(() => problems.push(`missing text ${t}`))
    }
    await page.keyboard.press('Escape')
    await dismissPopups(page)
    await page.mouse.move(VIEW_W - 1, VIEW_H - 1)
    await settle(page)
    const busy = await page.evaluate((sel) => document.querySelectorAll(sel).length, BUSY)
    if (busy) problems.push(`${busy} spinner/skeleton element(s) still on the page`)
    const dialogs = await page.evaluate(() => document.querySelectorAll('[role="dialog"],[role="alertdialog"]').length)
    if (dialogs) problems.push(`${dialogs} dialog(s) open`)
    const toasts = await page.evaluate(() => window.__toasts)
    const bad = toasts.filter((t) => /error|fail|unable|could not|couldn't|went wrong/i.test(t))
    if (bad.length) problems.push(`error toast: ${bad.join(' | ').slice(0, 200)}`)
    const text = await page.evaluate(() => document.body.innerText)
    const err = text.match(/something went wrong|failed to (load|fetch|retrieve)|error (fetching|loading|retrieving)|unexpected error|page not found/i)
    if (err) problems.push(`error text on the page: ${err[0]}`)
    const file = join(problems.length ? DEBUG : OUT, `${problems.length ? 'FAIL-' : ''}${tag}.png`)
    await page.screenshot({ path: file, ...(shot.clip ? { clip: shot.clip } : {}) })
    writeFileSync(join(DEBUG, `${tag}.txt`), [
      `url: ${page.url()}`, `problems: ${problems.join('; ') || 'none'}`, `toasts: ${JSON.stringify(toasts)}`,
      `bad responses: ${[...new Set(page.__bad)].slice(0, 20).join(' ; ')}`, `console errors: ${[...new Set(page.__console)].slice(0, 10).join(' ; ')}`,
      '--- text ---', text.slice(0, 4000), '--- network ---', ...page.__net.slice(-150),
    ].join('\n'))
    results.push({ shot: tag, ok: !problems.length, problems })
    log(problems.length ? `FAIL ${tag}: ${problems.join('; ')}` : `ok   ${tag}`)
  } catch (e) {
    await page.screenshot({ path: join(DEBUG, `ERROR-${tag}.png`) }).catch(() => {})
    problems.push(String(e.message).split('\n')[0])
    results.push({ shot: tag, ok: false, problems })
    log(`FAIL ${tag}: ${problems.join('; ')}`)
  }
}

for (const theme of THEMES) {
  const { ctx, page } = await newPage(theme)
  try {
    await signIn(page)
    writeFileSync(join(DEBUG, `sign-in-${theme}.txt`), [`url: ${page.url()}`, ...page.__net.slice(-100)].join('\n'))
  } catch (e) {
    await page.screenshot({ path: join(DEBUG, `ERROR-sign-in-${theme}.png`) }).catch(() => {})
    const body = await page.evaluate(() => document.body.innerText).catch(() => '')
    writeFileSync(join(DEBUG, `sign-in-${theme}.txt`), [`url: ${page.url()}`, `console: ${[...new Set(page.__console)].slice(0, 6).join(' ; ')}`, `text: ${body.slice(0, 1500)}`, ...page.__net.slice(-200)].join('\n'))
    log(`sign-in failed (${theme}): ${e.message.split('\n')[0]}`)
    results.push({ shot: `sign-in-${theme}`, ok: false, problems: [e.message.split('\n')[0]] })
    await ctx.close()
    continue
  }
  for (const shot of SHOTS) {
    if (ONLY && !ONLY.includes(shot.name)) continue
    // One retry: a first load can race a service that is still starting.
    await capture(page, shot, theme)
    if (!results.at(-1).ok) await capture(page, shot, theme)
  }
  await ctx.close()
}
await browser.close()
writeFileSync(join(DEBUG, 'report.json'), JSON.stringify(results, null, 2))
const failed = results.filter((r) => !r.ok)
console.log(`${results.length - failed.length}/${results.length} shots ok${failed.length ? `; failed: ${failed.map((f) => f.shot).join(', ')}` : ''}`)
process.exit(failed.length ? 1 : 0)

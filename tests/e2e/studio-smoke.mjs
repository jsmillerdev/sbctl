// Short browser check of Studio through sbctl's edge against the real Management API:
// sign in, project list, table editor and SQL editor of every project given.
//
//   STUDIO_URL=http://studio.127.0.0.1.sslip.io:36080 STUDIO_EMAIL=... STUDIO_PASSWORD=... \
//   STUDIO_PROJECTS='ref1:Name1,ref2:Name2' STUDIO_TABLE=todos STUDIO_ROW_TEXT='from anon' \
//   [CHROME=/path/to/chrome] [OUT=/dir for screenshots] node studio-smoke.mjs
//
// Needs `playwright-core` (npm install it in a scratch directory; this repo does not ship it)
// and a Chrome or Chromium: CHROME, or the one `npx playwright install chromium` fetched.
// Every project must hold STUDIO_TABLE with a row containing STUDIO_ROW_TEXT. Exits 1 when a
// step fails or when any API or pg-meta call answered 5xx.
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

import { chromium } from 'playwright-core'

const env = (k, d) => {
  const v = process.env[k] ?? d
  if (!v) throw new Error(`${k} is not set`)
  return v
}
const STUDIO = env('STUDIO_URL').replace(/\/$/, '')
const EMAIL = env('STUDIO_EMAIL')
const PASSWORD = env('STUDIO_PASSWORD')
const PROJECTS = env('STUDIO_PROJECTS').split(',').map((p) => p.split(':'))
const TABLE = env('STUDIO_TABLE', 'todos')
const ROW_TEXT = env('STUDIO_ROW_TEXT', 'from anon')
const OUT = process.env.OUT
if (OUT) mkdirSync(OUT, { recursive: true })

const browser = await chromium.launch({ headless: true, ...(process.env.CHROME ? { executablePath: process.env.CHROME } : {}) })
const page = await (await browser.newContext({ viewport: { width: 1440, height: 900 } })).newPage()
page.setDefaultTimeout(30_000)
const bad = []
const consoleErrors = []
let current = 'start'
page.on('response', (r) => {
  if (r.status() >= 500) bad.push(`${current}: ${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`)
})
page.on('console', (m) => m.type() === 'error' && consoleErrors.push(`${current}: ${m.text().slice(0, 160)}`))

let failed = 0
async function step(name, fn) {
  current = name
  const t0 = Date.now()
  try {
    await fn()
    console.log(`ok   ${name} (${Date.now() - t0} ms)`)
  } catch (e) {
    failed++
    console.log(`FAIL ${name}: ${String(e.message).split('\n')[0]}`)
  }
  if (OUT) await page.screenshot({ path: join(OUT, `${name.replace(/[^a-z0-9]+/gi, '-')}.png`) }).catch(() => {})
}
const goto = (p) => page.goto(`${STUDIO}${p}`, { waitUntil: 'domcontentloaded' })

await step('sign-in', async () => {
  await goto('/sign-in')
  // The form re-renders once while React hydrates and clears what was typed: fill until it sticks.
  const email = page.getByPlaceholder('you@example.com')
  const password = page.locator('input[type="password"]').first()
  for (let i = 0; i < 10; i++) {
    await email.fill(EMAIL)
    await password.fill(PASSWORD)
    await page.waitForTimeout(400)
    if ((await email.inputValue()) === EMAIL && (await password.inputValue()) === PASSWORD) break
  }
  await page.getByRole('button', { name: /^sign in$/i }).first().click()
  await page.waitForURL((u) => !u.pathname.startsWith('/sign-in'), { timeout: 60_000 })
  if (!(await page.evaluate(() => !!localStorage.getItem('supabase.dashboard.auth.token')))) throw new Error('no session after sign-in')
})

await step('project-list', async () => {
  await goto('/organizations')
  await page.getByText('Default Organization', { exact: true }).first().click()
  await page.waitForURL(/\/org\//)
  for (const [ref] of PROJECTS) await page.locator(`a[href*="/project/${ref}"]`).first().waitFor()
})

for (const [ref, name] of PROJECTS) {
  await step(`table-editor-${name}`, async () => {
    await goto(`/project/${ref}/editor`)
    await page.getByText(TABLE, { exact: true }).first().click()
    await page.getByText(ROW_TEXT).first().waitFor()
  })
  await step(`sql-editor-${name}`, async () => {
    await goto(`/project/${ref}/sql/new`)
    const editor = page.locator('.monaco-editor').first()
    await editor.waitFor()
    const sql = `select count(*) as sbctl_rows from public.${TABLE};`
    for (let i = 0; i < 10; i++) {
      await editor.locator('.view-lines').click()
      await page.keyboard.type(sql, { delay: 20 })
      if ((await editor.innerText()).includes('sbctl_rows')) break
      await page.waitForTimeout(500)
    }
    await page.getByRole('button', { name: /^Run/ }).first().click()
    await page.getByText('sbctl_rows', { exact: true }).first().waitFor()
  })
}

await step('no-5xx', async () => {
  if (bad.length) throw new Error(bad.slice(0, 5).join('; '))
})
if (consoleErrors.length) { const kinds = [...new Set(consoleErrors.map((e) => e.split(": ").slice(1).join(": ").slice(0, 110)))]; console.log(`note: ${consoleErrors.length} console errors, ${kinds.length} kinds:\n  ` + kinds.join("\n  ")) }
await browser.close()
console.log(failed ? `${failed} step(s) failed` : 'ALL PASS')
process.exit(failed ? 1 : 0)

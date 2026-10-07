// Drives the platform-mode Studio against the mock for studio/spike.sh. Exits 1 if any step fails.
// Environment: SPIKE_STUDIO_URL, SPIKE_EMAIL, SPIKE_PASSWORD, SPIKE_REFS (two refs, comma separated),
// SPIKE_MARKERS (a word that appears in each project's data, same order), SPIKE_DATABASES,
// SPIKE_OUT (results directory), SPIKE_REQUEST_LOG (the mock's JSON lines), SPIKE_CHROME (optional
// browser executable; otherwise the Chromium that `playwright install` fetched).
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { chromium } from 'playwright-core'

const env = (k) => {
  const v = process.env[k]
  if (!v) throw new Error(`${k} is not set`)
  return v
}
const STUDIO = env('SPIKE_STUDIO_URL').replace(/\/$/, '')
const EMAIL = env('SPIKE_EMAIL')
const PASSWORD = env('SPIKE_PASSWORD')
const REFS = env('SPIKE_REFS').split(',')
const MARKERS = env('SPIKE_MARKERS').split(',')
const DATABASES = env('SPIKE_DATABASES').split(',')
const OUT = env('SPIKE_OUT')
const REQUEST_LOG = env('SPIKE_REQUEST_LOG')
const SHOTS = join(OUT, 'screenshots')
mkdirSync(SHOTS, { recursive: true })

const steps = []
const warnings = []
const consoleErrors = []
const failedRequests = []
const pageErrors = []

const browser = await chromium.launch({
  headless: true,
  ...(process.env.SPIKE_CHROME ? { executablePath: process.env.SPIKE_CHROME } : {}),
})
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
const page = await context.newPage()
page.setDefaultTimeout(30_000)
page.on('console', (m) => {
  if (m.type() === 'error') consoleErrors.push({ step: current, text: m.text().slice(0, 300) })
})
page.on('pageerror', (e) => pageErrors.push({ step: current, text: String(e).slice(0, 300) }))
page.on('requestfailed', (r) => {
  failedRequests.push({ step: current, url: r.url().slice(0, 200), reason: r.failure()?.errorText })
})
// Studio's error boundary text; any step that shows it fails.
const CRASH = 'Sorry! An unexpected error occurred'

let current = 'start'
async function step(name, fn) {
  current = name
  const t0 = Date.now()
  const rec = { name, ok: false, ms: 0 }
  try {
    await fn()
    if (await page.getByText(CRASH).first().isVisible().catch(() => false)) {
      throw new Error('Studio showed its error boundary')
    }
    // Banners such as "Failed to load project usage" do not fail a step (they mean a stub was not
    // enough), but they are what section 9 of research/08 needs, so keep them.
    for (const el of await page.getByText(/^Failed to (load|retrieve|fetch)/i).all()) {
      if (await el.isVisible().catch(() => false)) {
        warnings.push({ step: name, text: (await el.innerText()).slice(0, 200) })
      }
    }
    rec.ok = true
  } catch (err) {
    rec.error = String(err.message ?? err).split('\n')[0].slice(0, 400)
  }
  rec.ms = Date.now() - t0
  try {
    await page.screenshot({ path: join(SHOTS, `${String(steps.length + 1).padStart(2, '0')}-${name.replace(/[^a-z0-9]+/gi, '-')}.png`) })
  } catch {
    // a screenshot failure must not hide the step result
  }
  steps.push(rec)
  console.log(`${rec.ok ? 'ok  ' : 'FAIL'} ${name} (${rec.ms} ms)${rec.error ? ' - ' + rec.error : ''}`)
}

const goto = async (path) => {
  await page.goto(`${STUDIO}${path}`, { waitUntil: 'domcontentloaded' })
}

await step('sign-in', async () => {
  await goto('/sign-in')
  // The form re-renders once while React hydrates and clears what was typed, so fill until it sticks.
  const email = page.getByPlaceholder('you@example.com')
  const password = page.locator('input[type="password"]').first()
  for (let attempt = 0; attempt < 10; attempt++) {
    await email.fill(EMAIL)
    await password.fill(PASSWORD)
    await page.waitForTimeout(400)
    if ((await email.inputValue()) === EMAIL && (await password.inputValue()) === PASSWORD) break
  }
  await page.getByRole('button', { name: /^sign in$/i }).first().click()
  await page.waitForURL((u) => !u.pathname.startsWith('/sign-in'), { timeout: 60_000 })
  const hasSession = await page.evaluate(() => !!localStorage.getItem('supabase.dashboard.auth.token'))
  if (!hasSession) throw new Error('no GoTrue session in localStorage after sign-in')
})

await step('project-list', async () => {
  await goto('/organizations')
  await page.getByText('Mock Org', { exact: true }).first().click()
  await page.waitForURL(/\/org\//, { timeout: 30_000 })
  await page.getByText('Alpha', { exact: true }).first().waitFor()
  await page.getByText('Beta', { exact: true }).first().waitFor()
})

for (const [i, ref] of REFS.entries()) {
  const marker = MARKERS[i]
  const db = DATABASES[i]

  await step(`table-editor-${ref}`, async () => {
    await goto(`/project/${ref}/editor`)
    await page.getByText('todos', { exact: true }).first().click()
    await page.getByText(`${marker}-task-1`).first().waitFor()
  })

  await step(`sql-editor-${ref}`, async () => {
    await goto(`/project/${ref}/sql/new`)
    const editor = page.locator('.monaco-editor').first()
    await editor.waitFor()
    // Monaco takes a moment to accept input; type until the text is in the editor.
    const sql = 'select current_database() as spike_db;'
    for (let attempt = 0; attempt < 10; attempt++) {
      await editor.locator('.view-lines').click()
      await page.keyboard.type(sql, { delay: 20 })
      if ((await editor.innerText()).includes('spike_db')) break
      await page.waitForTimeout(500)
    }
    await page.getByRole('button', { name: /^Run/ }).first().click()
    await page.getByText(db, { exact: true }).first().waitFor()
  })

  await step(`project-home-${ref}`, async () => {
    await goto(`/project/${ref}`)
    await page.getByText(i === 0 ? 'Alpha' : 'Beta', { exact: true }).first().waitFor()
    await page.getByText('Healthy', { exact: true }).first().waitFor()
  })

  await step(`auth-users-${ref}`, async () => {
    await goto(`/project/${ref}/auth/users`)
    // The list is a pg-meta query against the project's auth schema.
    await page.getByText(/Total: \d+ users|Failed to retrieve users/).first().waitFor()
    if (await page.getByText('Failed to retrieve users').first().isVisible()) {
      throw new Error('the Users page could not read auth.users')
    }
  })

  await step(`storage-${ref}`, async () => {
    await goto(`/project/${ref}/storage/files`)
    await page.getByText('Create a file bucket').first().waitFor()
  })

  await step(`database-tables-${ref}`, async () => {
    await goto(`/project/${ref}/database/tables`)
    await page.getByText('todos', { exact: true }).first().waitFor()
  })

  await step(`settings-general-${ref}`, async () => {
    await goto(`/project/${ref}/settings/general`)
    await page.getByRole('heading', { name: 'Project Settings' }).first().waitFor()
    // the project id is shown in a read-only input
    await page.waitForFunction((r) => [...document.querySelectorAll('input')].some((i) => i.value === r), ref)
  })

  await step(`settings-api-keys-${ref}`, async () => {
    await goto(`/project/${ref}/settings/api-keys`)
    await page.getByText('Create API keys').first().waitFor()
    await page.getByText('Legacy anon, service_role API keys').first().click()
    // the legacy anon key is shown in a read-only input
    await page.waitForFunction(() => [...document.querySelectorAll('input')].some((i) => i.value.startsWith('eyJ')))
  })
}

// What the mock saw: each project's data must have come through pg-meta, and nothing may have
// answered 5xx.
await step('mock-request-log', async () => {
  const lines = readFileSync(REQUEST_LOG, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l))
  for (const ref of REFS) {
    const hits = lines.filter((e) => e.template === '/platform/pg-meta/{ref}/query' && e.ref === ref && e.status === 200)
    if (hits.length === 0) throw new Error(`no successful pg-meta query for ${ref}`)
  }
  const bad = lines.filter((e) => e.status >= 500)
  if (bad.length > 0) throw new Error(`${bad.length} request(s) answered 5xx, first: ${bad[0].method} ${bad[0].path}`)
})

await browser.close()
const failed = steps.filter((s) => !s.ok)
writeFileSync(
  join(OUT, 'steps.json'),
  JSON.stringify({ steps, warnings, consoleErrors, pageErrors, failedRequests, ok: failed.length === 0 }, null, 2)
)
console.log(`${steps.length - failed.length}/${steps.length} steps passed; ${warnings.length} warnings, ${consoleErrors.length} console errors, ${failedRequests.length} failed requests`)
process.exit(failed.length === 0 ? 0 : 1)

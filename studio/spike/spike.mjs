// Drives the platform-mode Studio against the mock for studio/spike.sh. Exits 1 if any step fails.
// Environment: SPIKE_STUDIO_URL, SPIKE_EMAIL, SPIKE_PASSWORD, SPIKE_REFS (two refs, comma separated),
// SPIKE_MARKERS (a word that appears in each project's data, same order), SPIKE_DATABASES,
// SPIKE_OUT (results directory), SPIKE_REQUEST_LOG (the mock's JSON lines), SPIKE_CHROME (optional
// browser executable; otherwise the Chromium that `playwright install` fetched).
// For the OAuth and MCP steps: SPIKE_MCP_URL (NEXT_PUBLIC_MCP_URL of the Studio under test),
// SPIKE_MOCK_URL (the mock, which is also the Management API of Studio's /api/mcp route),
// SPIKE_AUTH_APPROVE and SPIKE_AUTH_DECLINE (ids of two pending authorization requests in the mock),
// SPIKE_AUTH_NAME (the name of the first request's client).
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
const MCP_URL = env('SPIKE_MCP_URL')
const MOCK_URL = env('SPIKE_MOCK_URL').replace(/\/$/, '')
const AUTH_APPROVE = env('SPIKE_AUTH_APPROVE')
const AUTH_DECLINE = env('SPIKE_AUTH_DECLINE')
const AUTH_NAME = env('SPIKE_AUTH_NAME')
const SHOTS = join(OUT, 'screenshots')
mkdirSync(SHOTS, { recursive: true })

const steps = []
const warnings = []
const consoleErrors = []
const failedRequests = []
const pageErrors = []
const badResponses = []
const healthRequests = []   // [step, path] of every project health poll, to detect a stuck 5 s refetch

const browser = await chromium.launch({
  headless: true,
  ...(process.env.SPIKE_CHROME ? { executablePath: process.env.SPIKE_CHROME } : {}),
})
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
const page = await context.newPage()
// Production answers this Studio route in supavise's proxy (internal/proxy, studio host): without
// an incident.io key it is a 500 that react-query retries for 21 s while the sign-in waits.
await page.route('**/api/incident-banner', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '{"incidents":[]}' }))
page.setDefaultTimeout(30_000)
page.on('console', (m) => {
  if (m.type() === 'error') consoleErrors.push({ step: current, text: m.text().slice(0, 300) })
})
page.on('pageerror', (e) => pageErrors.push({ step: current, text: String(e).slice(0, 300) }))
page.on('requestfailed', (r) => {
  failedRequests.push({ step: current, url: r.url().slice(0, 200), reason: r.failure()?.errorText })
})
page.on('request', (r) => {
  const u = new URL(r.url())
  if (/^\/v1\/projects\/[^/]+\/health$/.test(u.pathname)) healthRequests.push({ step: current, path: u.pathname })
})
page.on('response', (r) => {
  if (r.status() >= 400) {
    const u = new URL(r.url())
    badResponses.push({ step: current, status: r.status(), method: r.request().method(), host: u.host, path: u.pathname })
  }
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
    // enough), but docs/reference/studio-platform-calls.md ("Findings from running Studio")
    // draws on them, so keep them.
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
    // Studio re-polls /health every 5 s until every service reports ACTIVE_HEALTHY. Wait longer than
    // one interval; a handler that returns another status makes the count grow.
    const before = healthRequests.filter((h) => h.path.includes(`/${ref}/`)).length
    await page.waitForTimeout(7000)
    const polls = healthRequests.filter((h) => h.path.includes(`/${ref}/`)).length - before
    if (polls > 0) throw new Error(`/v1/projects/${ref}/health was polled ${polls} more time(s): a service is not ACTIVE_HEALTHY`)
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

// Connect > MCP shows the URL a node gives Studio (NEXT_PUBLIC_MCP_URL), not the build's placeholder
// and not the localhost default.
await step('connect-mcp-url', async () => {
  await goto(`/project/${REFS[0]}?showConnect=true&connectTab=mcp`)
  await page.getByText('Connect to your project').first().waitFor()
  const want = `${MCP_URL}?project_ref=${REFS[0]}`
  await page.waitForFunction((url) => document.body.innerText.includes(url), want)
  const text = await page.evaluate(() => document.body.innerText)
  if (text.includes('supavise-placeholder')) throw new Error('the Connect sheet shows a build placeholder')
})

// The consent page of OAuth sign-in for MCP clients (Studio's /authorize) against the mock's three
// authorization endpoints. The organization is preselected when the user has only one.
const organizationSelected = async () => {
  const picker = page.getByRole('combobox', { name: 'Organization to grant API access to' })
  await picker.waitFor()
  await page.waitForFunction(
    () => document.querySelector('[aria-label="Organization to grant API access to"]')?.textContent?.includes('Mock Org')
  )
}

await step('authorize-approve', async () => {
  await goto(`/authorize?auth_id=${AUTH_APPROVE}`)
  const approve = page.getByRole('button', { name: `Authorize ${AUTH_NAME}` })
  await approve.waitFor()
  await organizationSelected()
  await approve.click()
  // Studio sends the browser to the URL the approve call returned: the client's redirect with the
  // code, the state and the issuer.
  await page.waitForURL((u) => u.pathname === '/oauth-callback', { timeout: 30_000 })
  const u = new URL(page.url())
  for (const k of ['code', 'state', 'iss']) {
    if (!u.searchParams.get(k)) throw new Error(`the redirect after approving has no ${k}`)
  }
})

await step('authorize-approved-again', async () => {
  await goto(`/authorize?auth_id=${AUTH_APPROVE}`)
  await page.getByText('Authorization approved').first().waitFor()
})

await step('authorize-decline', async () => {
  await goto(`/authorize?auth_id=${AUTH_DECLINE}`)
  // an external redirect target is shown before the user decides
  await page.getByText('Authorizing will redirect you to').first().waitFor()
  await page.getByText('https://client.example.test/callback').first().waitFor()
  await organizationSelected()
  await page.getByRole('button', { name: 'Cancel' }).click()
  await page.getByText('Declined API authorization request').first().waitFor()
  await page.waitForURL(/\/organizations/, { timeout: 30_000 })
  await goto(`/authorize?auth_id=${AUTH_DECLINE}`)
  await page.getByText('Unable to load authorization').first().waitFor()
})

// Studio's own /api/mcp route (patch 0004): it calls the mock as the Management API with the bearer
// token it was given, here the dashboard session's, which the mock accepts like any bearer.
await step('mcp-route', async () => {
  await goto('/organizations')
  const token = await page.evaluate(() => JSON.parse(localStorage.getItem('supabase.dashboard.auth.token')).access_token)
  const rpc = async (query, body, bearer = token) => {
    const res = await fetch(`${STUDIO}/api/mcp${query}`, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        accept: 'application/json, text/event-stream',
        ...(bearer ? { authorization: `Bearer ${bearer}` } : {}),
      },
      body: JSON.stringify(body),
    })
    return { status: res.status, text: await res.text() }
  }
  const list = { jsonrpc: '2.0', id: 1, method: 'tools/list' }
  const call = (name, args) => ({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name, arguments: args } })
  const scoped = `?project_ref=${REFS[0]}&read_only=true`
  const need = (label, cond, r) => {
    if (!cond) throw new Error(`${label}: ${r.status} ${r.text.slice(0, 200)}`)
  }

  let r = await rpc('', list, '')
  need('no token must be 401', r.status === 401, r)
  r = await rpc('?read_only=maybe', list)
  need('a bad read_only must be 400', r.status === 400, r)
  r = await rpc(scoped, list)
  need('tools/list', r.status === 200 && r.text.includes('"execute_sql"'), r)
  need('a project-scoped server has no account tools', !r.text.includes('"list_organizations"'), r)
  need('a read-only server has no write tools', !r.text.includes('"apply_migration"'), r)
  r = await rpc(`?project_ref=${REFS[0]}`, call('get_project_url', {}))
  const mockPort = new URL(MOCK_URL).port
  need('get_project_url answers from the template', r.status === 200 && r.text.includes(`http://${REFS[0]}.localhost:${mockPort}`), r)
  // execute_sql: route -> Management API (the mock) -> postgres-meta -> the project's database
  r = await rpc(scoped, call('execute_sql', { query: 'select current_database() as spike_mcp_db' }))
  need('execute_sql reaches the project database', r.status === 200 && r.text.includes(DATABASES[0]) && !r.text.includes('isError'), r)
})

// What the mock saw: each project's data must have come through pg-meta, the consent calls and the
// MCP tool's query must have been served, and nothing may have answered 5xx.
await step('mock-request-log', async () => {
  const lines = readFileSync(REQUEST_LOG, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l))
  for (const ref of REFS) {
    const hits = lines.filter((e) => e.template === '/platform/pg-meta/{ref}/query' && e.ref === ref && e.status === 200)
    if (hits.length === 0) throw new Error(`no successful pg-meta query for ${ref}`)
  }
  const saw = (method, template, status) =>
    lines.some((e) => e.method === method && e.template === template && e.status === status)
  for (const [method, template, status] of [
    ['GET', '/platform/oauth/authorizations/{id}', 200],
    ['POST', '/platform/organizations/{slug}/oauth/authorizations/{id}', 201],
    ['DELETE', '/platform/organizations/{slug}/oauth/authorizations/{id}', 200],
    ['POST', '/v1/projects/{ref}/database/query', 200],
  ]) {
    if (!saw(method, template, status)) throw new Error(`the mock never answered ${method} ${template} with ${status}`)
  }
  const bad = lines.filter((e) => e.status >= 500)
  if (bad.length > 0) throw new Error(`${bad.length} request(s) answered 5xx, first: ${bad[0].method} ${bad[0].path}`)
})

await browser.close()
const failed = steps.filter((s) => !s.ok)
writeFileSync(
  join(OUT, 'steps.json'),
  JSON.stringify({ steps, warnings, badResponses, consoleErrors, pageErrors, failedRequests, ok: failed.length === 0 }, null, 2)
)
console.log(`${steps.length - failed.length}/${steps.length} steps passed; ${warnings.length} warnings, ${consoleErrors.length} console errors, ${failedRequests.length} failed requests`)
process.exit(failed.length === 0 ? 0 : 1)

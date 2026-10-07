// Shared helpers of the conformance suites. The driver (tests/conformance/run.sh) writes the
// file named by CONFORMANCE_CONFIG: the Management API URL, a personal access token and the
// projects the suites run against. Everything else (keys, users, tables) the suites create
// themselves, through the same public interfaces a customer would use.
import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'
import { createClient } from '@supabase/supabase-js'

export const config = JSON.parse(readFileSync(process.env.CONFORMANCE_CONFIG ?? new URL('./conformance.json', import.meta.url), 'utf8'))

// One random suffix per process keeps names apart from earlier runs against the same node.
export const suffix = randomBytes(4).toString('hex')

export { sleep }

// mgmt calls the Management API with the personal access token.
export async function mgmt(method, path, body, headers = {}) {
  const init = { method, headers: { Authorization: `Bearer ${config.pat}`, ...headers } }
  if (body instanceof FormData) {
    init.body = body
  } else if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  const res = await fetch(config.apiUrl + path, init)
  const text = await res.text()
  let json = text
  try {
    json = text ? JSON.parse(text) : null
  } catch {
    // not JSON: keep the text
  }
  return { status: res.status, body: json }
}

// sql runs a statement on a project through the Management API (database/query) and returns the rows.
export async function sql(project, query) {
  const r = await mgmt('POST', `/v1/projects/${project.ref}/database/query`, { query })
  if (r.status >= 300) throw new Error(`database/query answered ${r.status}: ${JSON.stringify(r.body)}`)
  return r.body
}

// projectKeys reads the keys of a project: the opaque publishable and secret keys and the legacy JWTs.
async function projectKeys(project) {
  const r = await mgmt('GET', `/v1/projects/${project.ref}/api-keys?reveal=true`)
  assert.equal(r.status, 200, `api-keys: ${JSON.stringify(r.body)}`)
  const keys = {}
  for (const k of r.body) {
    if (typeof k.api_key !== 'string') continue
    if (k.api_key.startsWith('sb_publishable_')) keys.publishable ??= k.api_key
    else if (k.api_key.startsWith('sb_secret_')) keys.secret ??= k.api_key
    else if (k.name === 'anon') keys.anon = k.api_key
    else if (k.name === 'service_role') keys.serviceRole = k.api_key
  }
  for (const n of ['publishable', 'secret', 'anon', 'serviceRole']) {
    assert.ok(keys[n], `the project has no ${n} key: ${JSON.stringify(r.body.map((k) => [k.name, k.type]))}`)
  }
  return keys
}

export const projects = config.projects
export const keys = { a: await projectKeys(projects.a), b: await projectKeys(projects.b) }

// kinds lists the two key families every suite runs with: the opaque keys of the new API key
// system and the legacy JWTs. `anon` is the low-privilege key, `admin` the one that bypasses RLS.
export function kinds(k) {
  return [
    { tag: 'opaque', label: 'publishable and secret keys', anon: k.publishable, admin: k.secret },
    { tag: 'legacy', label: 'legacy anon and service_role JWTs', anon: k.anon, admin: k.serviceRole },
  ]
}

// client builds a supabase-js client that keeps no state outside the process.
export function client(project, key, options = {}) {
  return createClient(project.url, key, {
    auth: { persistSession: false, autoRefreshToken: false, detectSessionInUrl: false },
    ...options,
  })
}

// waitFor polls fn until it returns something truthy and returns that.
export async function waitFor(what, fn, { timeout = 30_000, interval = 500 } = {}) {
  const end = Date.now() + timeout
  let last
  for (;;) {
    try {
      last = await fn()
      if (last) return last
    } catch (e) {
      last = e
    }
    if (Date.now() > end) {
      throw new Error(`timed out waiting for ${what}: ${last instanceof Error ? last.message : JSON.stringify(last)}`)
    }
    await sleep(interval)
  }
}

// withTimeout fails a promise that takes longer than ms.
export function withTimeout(promise, ms, what) {
  let timer
  const limit = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`timed out after ${ms} ms waiting for ${what}`)), ms)
  })
  return Promise.race([promise, limit]).finally(() => clearTimeout(timer))
}

// makeUser creates a confirmed user with the admin API and returns its credentials.
export async function makeUser(admin, label) {
  const email = `${label}-${suffix}@example.com`
  const password = `Conformance-${suffix}-pw`
  const { data, error } = await admin.auth.admin.createUser({ email, password, email_confirm: true })
  assert.ifError(error)
  return { id: data.user.id, email, password }
}

// signedIn returns a client whose requests carry the user's access token.
export async function signedIn(project, anonKey, user) {
  const c = client(project, anonKey)
  const { error } = await c.auth.signInWithPassword({ email: user.email, password: user.password })
  assert.ifError(error)
  return c
}

// jwtClaims decodes the payload of a JWT (no verification: the services verify).
export function jwtClaims(token) {
  return JSON.parse(Buffer.from(token.split('.')[1], 'base64url').toString('utf8'))
}

// waitForRest waits until PostgREST serves table, which it does once its schema cache has reloaded.
export async function waitForRest(project, key, table) {
  await waitFor(`${table} in the PostgREST schema cache`, async () => {
    const r = await fetch(`${project.url}/rest/v1/${table}?select=*&limit=1`, { headers: { apikey: key, Authorization: `Bearer ${key}` } })
    return r.status === 200
  }, { timeout: 60_000 })
}

// Edge Functions behind /functions/v1: a function deployed through the Management API's
// deploy endpoint (the one `supabase functions deploy --use-api` calls) and invoked with
// supabase-js, with and without JWT verification, with a secret, and listed, redeployed and deleted.
//
// A function with verify_jwt on is called with the legacy anon JWT and with a user's token. The
// opaque keys are not JWTs, so hosted Supabase documents that such a function must be deployed
// without JWT verification to accept them; the suite calls the function that opted out with both.
import assert from 'node:assert/strict'
import { before, describe, test } from 'node:test'
import { client, keys, makeUser, mgmt, projects, signedIn, sql, suffix, waitFor } from './lib.mjs'

const p = projects.a
const hello = `hello-${suffix}`
const open = `open-${suffix}`

const helloSource = `
Deno.serve(async (req) => {
  const { name = 'world' } = await req.json().catch(() => ({}))
  return new Response(JSON.stringify({ message: 'Hello ' + name + '!', version: Deno.env.get('CONF_VERSION') ?? 'v1' }), {
    headers: { 'Content-Type': 'application/json' },
  })
})
`

const envSource = `
Deno.serve(async (req) => {
  const url = Deno.env.get('SUPABASE_URL') ?? ''
  const service = Deno.env.get('SUPABASE_SERVICE_ROLE_KEY') ?? ''
  let rows = null
  if (new URL(req.url).searchParams.has('rows')) {
    const r = await fetch(url + '/rest/v1/conf_fn_rows?select=id', { headers: { apikey: service, Authorization: 'Bearer ' + service } })
    rows = r.ok ? (await r.json()).length : 'REST answered ' + r.status
  }
  return Response.json({
    secret: Deno.env.get('CONF_SECRET') ?? null,
    url,
    hasAnon: Boolean(Deno.env.get('SUPABASE_ANON_KEY')),
    hasService: Boolean(service),
    rows,
  })
})
`

// deploy uploads one function the way the CLI's --use-api flow does.
async function deploy(slug, source, verifyJwt) {
  const form = new FormData()
  form.append('metadata', new Blob([JSON.stringify({ entrypoint_path: 'index.ts', name: slug, verify_jwt: verifyJwt })], { type: 'application/json' }))
  form.append('file', new Blob([source], { type: 'application/typescript' }), 'index.ts')
  const r = await mgmt('POST', `/v1/projects/${p.ref}/functions/deploy?slug=${slug}`, form)
  assert.ok(r.status === 200 || r.status === 201, `deploy ${slug} answered ${r.status}: ${JSON.stringify(r.body)}`)
  return r.body
}

describe('Edge Functions', () => {
  const anon = client(p, keys.a.anon)
  const opaque = client(p, keys.a.publishable)
  let c1

  before(async () => {
    const admin = client(p, keys.a.serviceRole)
    c1 = await signedIn(p, keys.a.anon, await makeUser(admin, 'fn'))
    await sql(p, `
      create table if not exists public.conf_fn_rows (id int primary key);
      insert into public.conf_fn_rows values (1), (2), (3) on conflict do nothing;
      grant all on public.conf_fn_rows to service_role;
      notify pgrst, 'reload schema';
    `)
    await deploy(hello, helloSource, true)
    await deploy(open, envSource, false)
  })

  // ready waits until a deployed function answers: the runtime picks a new deployment up shortly after the upload.
  async function ready(slug, headers) {
    await waitFor(`${slug} to answer`, async () => (await fetch(`${p.url}/functions/v1/${slug}`, { method: 'POST', headers })).status === 200, { timeout: 90_000, interval: 1000 })
  }

  test('the function answers a supabase-js invoke with the anon JWT', async () => {
    await ready(hello, { apikey: keys.a.anon, Authorization: `Bearer ${keys.a.anon}` })
    const { data, error } = await anon.functions.invoke(hello, { body: { name: 'conformance' } })
    assert.ifError(error)
    assert.equal(data.message, 'Hello conformance!')
  })

  test('the function answers a signed-in user', async () => {
    const { data, error } = await c1.functions.invoke(hello, { body: { name: 'user' } })
    assert.ifError(error)
    assert.equal(data.message, 'Hello user!')
  })

  test('JWT verification: no token and a forged token are refused', async () => {
    const none = await fetch(`${p.url}/functions/v1/${hello}`, { method: 'POST' })
    assert.equal(none.status, 401)
    const forged = await fetch(`${p.url}/functions/v1/${hello}`, { method: 'POST', headers: { Authorization: 'Bearer eyJhbGciOiJIUzI1NiJ9.eyJyb2xlIjoiYW5vbiJ9.c2lnbmF0dXJl' } })
    assert.equal(forged.status, 401)
  })

  test('a function without JWT verification answers with no token and with the opaque key', async () => {
    await ready(open, {})
    const none = await fetch(`${p.url}/functions/v1/${open}`, { method: 'POST' })
    assert.equal(none.status, 200)
    const { data, error } = await opaque.functions.invoke(open, { body: {} })
    assert.ifError(error)
    assert.equal(data.hasService, true)
  })

  test('the function sees its project: SUPABASE_URL, keys, and its own database through REST', async () => {
    const r = await fetch(`${p.url}/functions/v1/${open}?rows`, { method: 'POST' })
    assert.equal(r.status, 200)
    const env = await r.json()
    assert.ok(env.url.includes(p.ref), `SUPABASE_URL is ${env.url}`)
    assert.equal(env.hasAnon, true)
    assert.equal(env.hasService, true)
    assert.equal(env.rows, 3)
  })

  test('a secret set through the Management API reaches the function', async () => {
    const set = await mgmt('POST', `/v1/projects/${p.ref}/secrets`, [{ name: 'CONF_SECRET', value: `s-${suffix}` }])
    assert.ok(set.status === 200 || set.status === 201, `secrets answered ${set.status}: ${JSON.stringify(set.body)}`)
    const list = await mgmt('GET', `/v1/projects/${p.ref}/secrets`)
    assert.equal(list.status, 200)
    assert.ok(list.body.some((s) => s.name === 'CONF_SECRET'), 'the secret is listed by name')
    assert.ok(list.body.every((s) => s.value !== `s-${suffix}`), 'the list does not return the value')
    await waitFor('the secret in the function', async () => {
      const r = await fetch(`${p.url}/functions/v1/${open}`, { method: 'POST' })
      return (await r.json()).secret === `s-${suffix}`
    }, { timeout: 60_000, interval: 1000 })
  })

  test('the function is listed, and a redeploy raises its version', async () => {
    const one = await mgmt('GET', `/v1/projects/${p.ref}/functions/${hello}`)
    assert.equal(one.status, 200)
    assert.equal(one.body.slug, hello)
    assert.equal(one.body.status, 'ACTIVE')
    assert.equal(one.body.verify_jwt, true)
    const v1 = one.body.version
    await deploy(hello, helloSource.replace("'v1'", "'v2'"), true)
    const list = await mgmt('GET', `/v1/projects/${p.ref}/functions`)
    assert.equal(list.status, 200)
    const now = list.body.find((f) => f.slug === hello)
    assert.ok(now, 'listed')
    assert.ok(now.version > v1, `version ${v1} -> ${now.version}`)
    await waitFor('the new code', async () => (await anon.functions.invoke(hello, { body: {} })).data?.version === 'v2', { timeout: 60_000, interval: 1000 })
  })

  test('a deleted function answers 404', async () => {
    const del = await mgmt('DELETE', `/v1/projects/${p.ref}/functions/${open}`)
    assert.ok(del.status === 200 || del.status === 204, `delete answered ${del.status}`)
    await waitFor('the function to be gone', async () => (await fetch(`${p.url}/functions/v1/${open}`, { method: 'POST' })).status === 404, { timeout: 60_000, interval: 1000 })
  })
})

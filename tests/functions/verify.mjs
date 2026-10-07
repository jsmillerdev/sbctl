// Checks of Edge Functions from the outside, with supabase-js and plain fetch, against a
// node that serves two projects (run.sh deploys the fixtures first).
//
//   node verify.mjs <config.json> main|secrets|redeploy|deleted
//
// The config names both projects, their keys and the URL each is served at. Every check
// prints "ok <name>" or throws; the exit status is non-zero on the first failure.
import { createHmac } from 'node:crypto'
import { readFileSync } from 'node:fs'
import assert from 'node:assert/strict'
import { createClient } from '@supabase/supabase-js'

const cfg = JSON.parse(readFileSync(process.argv[2], 'utf8'))
const phase = process.argv[3] ?? 'main'
const { a, b } = cfg.projects
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function ok(name) {
  console.log(`ok ${name}`)
}

const fnUrl = (p, slug = '') => `${p.url}/functions/v1/${slug}`

/** One request with its own timeout; returns status, headers and the body text. */
async function call(url, { headers = {}, method = 'GET', body, timeoutMs = 45_000 } = {}) {
  const t0 = Date.now()
  const res = await fetch(url, { method, headers, body, signal: AbortSignal.timeout(timeoutMs) })
  const text = await res.text()
  let json = null
  try {
    json = JSON.parse(text)
  } catch { /* not JSON */ }
  return { status: res.status, headers: res.headers, text, json, ms: Date.now() - t0 }
}

const bearer = (token) => ({ authorization: `Bearer ${token}` })

function hs256(secret, claims, header = { alg: 'HS256', typ: 'JWT' }) {
  const enc = (o) => Buffer.from(JSON.stringify(o)).toString('base64url')
  const data = `${enc(header)}.${enc(claims)}`
  return `${data}.${createHmac('sha256', secret).update(data).digest('base64url')}`
}

// supabase-js against the project's own URL: the call a client application makes.
async function invoke(p, slug, key = p.anon, options = {}) {
  const client = createClient(p.url, key, { auth: { persistSession: false } })
  return await client.functions.invoke(slug, options)
}

async function mainPhase() {
  // 1. The same slug in two projects answers with each project's own code, secrets and URL.
  for (const [p, who, code] of [[a, 'project-a', 'A1'], [b, 'project-b', 'B1']]) {
    const { data, error } = await invoke(p, 'hello')
    assert.equal(error, null, `${who}: ${error}`)
    assert.equal(data.who, who)
    assert.equal(data.code, code)
    assert.equal(data.secret, `secret-for-${p.name}`)
    assert.ok(data.supabaseUrl.includes(p.ref), `SUPABASE_URL ${data.supabaseUrl} is not of ${p.ref}`)
    assert.equal(data.otherSecret, null)
    assert.equal(data.path, '/hello')
    ok(`${who}: hello runs its own code with its own secret and SUPABASE_URL`)
  }
  {
    const { data } = await invoke(a, 'hello')
    assert.equal(data.hasServiceKey, true)
    assert.equal(data.hasDbUrl, true)
    assert.equal(data.sawTenantHeader, false, 'a function must not see the internal tenant header')
    assert.equal(data.sawSbApiKey, false)
    assert.equal(data.pathEnv, null, 'the main service environment leaked into a worker')
    ok('worker environment holds the project values and none of the main service')
  }
  {
    const { data } = await invoke(a, 'hello', undefined, { method: 'POST', body: { x: 1 }, headers: { 'x-extra': 'y' } })
    assert.equal(data.who, 'project-a')
    ok('functions.invoke with a body and headers')
  }

  // 2. verify_jwt on (the default).
  for (const [name, headers] of [
    ['anon key', bearer(a.anon)],
    ['service_role key', bearer(a.service)],
    ['publishable key (opaque)', { apikey: a.publishable }],
    ['secret key (opaque)', { apikey: a.secret }],
    ['publishable key as bearer', bearer(a.publishable)],
  ]) {
    const r = await call(fnUrl(a, 'hello'), { headers })
    assert.equal(r.status, 200, `${name}: ${r.status} ${r.text}`)
    assert.equal(r.json.who, 'project-a')
  }
  ok('verify_jwt: anon, service_role and opaque keys are accepted')

  const rejected = [
    ['no credentials', {}],
    ['garbage token', bearer('abc.def.ghi')],
    ['not a bearer', { authorization: 'Basic abc' }],
    ['token signed with another secret', bearer(hs256('not-the-secret', { role: 'anon', exp: 4102444800 }))],
    ['expired token with the right secret', bearer(hs256(a.jwtSecret, { role: 'anon', exp: 1 }))],
    ["project B's anon key on project A", bearer(b.anon)],
    ['alg none', bearer(hs256('x', { role: 'anon' }, { alg: 'none', typ: 'JWT' }))],
  ]
  for (const [name, headers] of rejected) {
    const r = await call(fnUrl(a, 'hello'), { headers })
    assert.equal(r.status, 401, `${name}: ${r.status} ${r.text}`)
    assert.ok(r.headers.get('sb-error-code'), `${name}: no sb-error-code`)
    assert.equal(r.json.msg, r.json.message)
  }
  assert.equal((await call(fnUrl(b, 'hello'), { headers: bearer(a.anon) })).status, 401)
  assert.equal((await call(fnUrl(b, 'hello'), { headers: bearer(hs256(a.jwtSecret, { role: 'service_role', exp: 4102444800 })) })).status, 401)
  ok('verify_jwt: bad, expired, foreign and project-crossing tokens are 401')

  // supabase-js reports a 401 as an error.
  {
    const { data, error } = await invoke(a, 'hello', b.anon)
    assert.equal(data, null)
    assert.equal(error?.name, 'FunctionsHttpError')
    assert.equal(error.context.status, 401)
    ok('supabase-js reports the 401 as FunctionsHttpError')
  }

  // 3. verify_jwt off.
  for (const [p, want] of [[a, 'open-a'], [b, 'open-b']]) {
    const r = await call(fnUrl(p, 'open'))
    assert.equal(r.status, 200, `${p.name}: ${r.status} ${r.text}`)
    assert.equal(r.text, want)
  }
  ok('functions deployed with --no-verify-jwt answer without credentials')

  // 4. Existence is per project.
  assert.equal((await call(fnUrl(a, 'onlya'), { headers: bearer(a.anon) })).text, 'only-a')
  for (const [p, slug] of [[b, 'onlya'], [a, 'nope'], [b, 'nope'], [a, '1bad'], [a, '.sbctl-function.json'], [a, '..']]) {
    const r = await call(fnUrl(p, slug), { headers: bearer(p.anon) })
    assert.equal(r.status, 404, `${p.name}/${slug}: ${r.status} ${r.text}`)
  }
  {
    const r = await call(fnUrl(b, 'onlya'), { headers: bearer(b.anon) })
    assert.deepEqual(r.json, { code: 'NOT_FOUND', message: 'Requested function was not found' })
    assert.equal(r.headers.get('sb-error-code'), 'NOT_FOUND')
  }
  ok("a function of one project is 404 in the other, unknown slugs are 404")

  // 5. The tenant is the proxy's, not the client's.
  {
    const r = await call(fnUrl(a, 'hello'), { headers: { ...bearer(a.anon), 'x-sbctl-project-ref': b.ref } })
    assert.equal(r.json?.who, 'project-a', 'a forged project header changed the tenant')
    const r2 = await call(fnUrl(b, 'onlya'), { headers: { ...bearer(b.anon), 'X-SBCTL-PROJECT-REF': a.ref } })
    assert.equal(r2.status, 404, 'a forged project header reached another project\'s function')
    const r3 = await call(fnUrl(a, 'hello'), { headers: { ...bearer(a.anon), 'sb-api-key': b.anon } })
    assert.equal(r3.json?.who, 'project-a')
    ok('a client-supplied tenant header or sb-api-key changes nothing')
  }
  if (cfg.runtimeUrl) {
    const r = await call(`${cfg.runtimeUrl}/hello`, { headers: bearer(a.anon) })
    assert.equal(r.status, 400, `the runtime answered ${r.status} without a project header`)
    ok('the runtime itself refuses a request without a project reference')
  }

  // A worker sees its module graph and nothing of the node's disk or the main service's
  // environment: not the node's files, not another project's keys.
  {
    const files = ['/etc/hosts', `${cfg.stateDir}/projects/${b.ref}/functions-env.json`, `${cfg.stateDir}/projects/${a.ref}/functions-env.json`]
    const q = files.map((f) => `p=${encodeURIComponent(f)}`).join('&')
    const r = await call(`${fnUrl(a, 'readfs')}?${q}`, { headers: bearer(a.anon) })
    assert.equal(r.status, 200, `readfs: ${r.status} ${r.text}`)
    for (const f of files) assert.match(r.json[f], /^ERROR: /, `a function read ${f}: ${r.json[f]}`)
    assert.equal(r.json.env, 'ENV: unset', 'the main service environment is visible to a worker')
    ok('a function cannot read the files of the node or the environment of the main service')
  }

  // 6. A function reaches its own project's database.
  // run.sh created public.fn_probe in both databases and asked PostgREST to reload.
  await sleep(2000)
  for (const p of [a, b]) {
    const r = await call(fnUrl(p, 'dbcheck'), { headers: bearer(p.service), timeoutMs: 90_000 })
    assert.equal(r.status, 200, `${p.name} dbcheck: ${r.status} ${r.text}`)
    assert.equal(r.json.sql.db, 'postgres')
    assert.equal(r.json.sql.role, 'postgres')
    assert.equal(r.json.sql.probes, 1)
    assert.equal(r.json.restError, null, `REST through SUPABASE_URL: ${r.json.restError}`)
    assert.deepEqual(r.json.rest, [{ who: `project-${p.name}` }])
  }
  ok('functions query their own database through SUPABASE_DB_URL and supabase-js with the service key')

  // 7. A crash, a runaway function and a memory hog in project A do not touch project B.
  {
    const r = await call(fnUrl(a, 'crash'), { headers: bearer(a.anon) })
    assert.ok([500, 503].includes(r.status), `crash: ${r.status} ${r.text}`)
    assert.ok(r.headers.get('sb-error-code'))
    const hb = await call(fnUrl(b, 'hello'), { headers: bearer(b.anon) })
    assert.equal(hb.json?.who, 'project-b')
    ok('a function that does not boot answers with an error; the other project is fine')
  }
  {
    const spin = call(fnUrl(a, 'spin'), { headers: bearer(a.anon), timeoutMs: 90_000 })
    await sleep(500)
    const during = []
    for (let i = 0; i < 5; i++) {
      during.push(await call(fnUrl(b, 'hello'), { headers: bearer(b.anon) }))
      during.push(await call(fnUrl(a, 'open')))
      await sleep(200)
    }
    for (const r of during) assert.equal(r.status, 200, `during the spin: ${r.status} ${r.text}`)
    assert.ok(Math.max(...during.map((r) => r.ms)) < 3000, `requests were slow during the spin: ${during.map((r) => r.ms)}`)
    const r = await spin
    assert.ok([500, 504, 546].includes(r.status), `spin: ${r.status} ${r.text}`)
    assert.ok(r.headers.get('sb-error-code'), 'spin: no sb-error-code')
    console.log(`   (the runaway function ended with ${r.status} ${r.headers.get('sb-error-code')} after ${r.ms} ms)`)
    ok('a runaway function ends by the runtime limits while both projects keep answering')
  }
  {
    const r = await call(fnUrl(a, 'hog'), { headers: bearer(a.anon), timeoutMs: 90_000 })
    assert.ok([500, 546].includes(r.status), `hog: ${r.status} ${r.text}`)
    assert.ok(r.headers.get('sb-error-code'))
    const ha = await invoke(a, 'hello')
    assert.equal(ha.data?.who, 'project-a')
    const hb = await invoke(b, 'hello')
    assert.equal(hb.data?.who, 'project-b')
    ok('a function over its memory limit is ended; both projects answer afterwards')
  }
}

async function secretsPhase() {
  const { data } = await invoke(a, 'hello')
  assert.equal(data.secret, 'rotated-secret-for-a')
  assert.equal((await invoke(b, 'hello')).data.secret, 'secret-for-b')
  ok('a changed secret reaches the next request of its project only')
}

async function redeployPhase() {
  const ra = await invoke(a, 'hello')
  assert.equal(ra.data.code, 'A2', `project A still runs ${ra.data.code}`)
  assert.equal((await invoke(b, 'hello')).data.code, 'B1')
  ok('a redeploy replaces the code of that project only')
}

async function deletedPhase() {
  const r = await call(fnUrl(a, 'hello'), { headers: bearer(a.anon) })
  assert.equal(r.status, 404, `deleted function: ${r.status} ${r.text}`)
  assert.equal((await invoke(b, 'hello')).data.who, 'project-b')
  ok('a deleted function is 404 and the other project is unaffected')
}

const phases = { main: mainPhase, secrets: secretsPhase, redeploy: redeployPhase, deleted: deletedPhase }
if (!phases[phase]) throw new Error(`unknown phase ${phase}`)
await phases[phase]()
console.log(`phase ${phase} passed`)

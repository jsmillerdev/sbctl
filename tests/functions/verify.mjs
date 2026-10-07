// Checks of Edge Functions from the outside, with supabase-js and plain fetch, against a
// node that serves two projects (run.sh deploys the fixtures first).
//
//   node verify.mjs <config.json> main|secrets|redeploy|deleted
//
// The config names both projects, their keys and the URL each is served at. Every check
// prints "ok <name>" or throws; the exit status is non-zero on the first failure.
import { createHmac } from 'node:crypto'
import { existsSync, readFileSync, realpathSync } from 'node:fs'
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
  for (const [p, slug] of [[b, 'onlya'], [a, 'nope'], [b, 'nope'], [a, '1bad'], [a, '.supavise-function.json'], [a, '..']]) {
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
    const r = await call(fnUrl(a, 'hello'), { headers: { ...bearer(a.anon), 'x-supavise-project-ref': b.ref } })
    assert.equal(r.json?.who, 'project-a', 'a forged project header changed the tenant')
    const r2 = await call(fnUrl(b, 'onlya'), { headers: { ...bearer(b.anon), 'X-SUPAVISE-PROJECT-REF': a.ref } })
    assert.equal(r2.status, 404, 'a forged project header reached another project\'s function')
    const r3 = await call(fnUrl(a, 'hello'), { headers: { ...bearer(a.anon), 'sb-api-key': b.anon } })
    assert.equal(r3.json?.who, 'project-a')
    ok('a client-supplied tenant header or sb-api-key changes nothing')
  }
  if (cfg.runtimeUrl) {
    // The runtime listens on loopback, which a function worker can reach too: it serves only a
    // caller that has the proxy's secret, whatever project it names.
    const r = await call(`${cfg.runtimeUrl}/hello`, { headers: bearer(a.anon) })
    assert.equal(r.status, 403, `the runtime answered ${r.status} without the proxy secret`)
    const r2 = await call(`${cfg.runtimeUrl}/hello`, {
      headers: { ...bearer(a.anon), 'x-supavise-project-ref': a.ref },
    })
    assert.equal(r2.status, 403, `the runtime served a project reference without the proxy secret: ${r2.status}`)
    const r3 = await call(`${cfg.runtimeUrl}/hello`, {
      headers: { ...bearer(a.anon), 'x-supavise-project-ref': a.ref, 'x-supavise-proxy-token': 'guess' },
    })
    assert.equal(r3.status, 403, `the runtime accepted a wrong secret: ${r3.status}`)
    if (cfg.proxyToken) {
      const t = { 'x-supavise-proxy-token': cfg.proxyToken }
      const r4 = await call(`${cfg.runtimeUrl}/hello`, { headers: { ...bearer(a.anon), ...t } })
      assert.equal(r4.status, 400, `no project reference, with the secret: ${r4.status}`)
      const r5 = await call(`${cfg.runtimeUrl}/hello`, {
        headers: { ...bearer(a.anon), ...t, 'x-supavise-project-ref': a.ref },
      })
      assert.equal(r5.json?.who, 'project-a', `with the secret: ${r5.status} ${r5.text}`)
    }
    ok('the runtime itself serves only callers that have the proxy secret')

    // A worker that reaches the runtime port under another name, claiming to be project B.
    const port = new URL(cfg.runtimeUrl).port
    const targets = [
      `http://127.0.0.1:${port}/hello`,
      `http://worker.127.0.0.1.sslip.io:${port}/hello`,
      `http://[::ffff:127.0.0.1]:${port}/hello`,
      `http://[::ffff:7f00:1]:${port}/hello`,
    ]
    const q = targets.map((u) => `u=${encodeURIComponent(u)}`).join('&')
    const c = await call(`${fnUrl(a, 'callout')}?ref=${b.ref}&${q}`, { headers: bearer(a.anon), timeoutMs: 60_000 })
    assert.equal(c.status, 200, `callout: ${c.status} ${c.text}`)
    for (const u of targets) {
      assert.ok(/^(403 |ERROR: )/.test(c.json[u]), `a worker reached the runtime as another project through ${u}: ${c.json[u]}`)
      assert.ok(!/project-b/.test(c.json[u]), `a worker got project B's function through ${u}`)
    }
    ok(`a function cannot call the runtime as another project, however it names the address (${targets.length} ways tried)`)
  }

  // A worker sees its module graph and nothing of the node's disk or the main service's
  // environment: not the node's files, not another project's keys.
  {
    const tenants = `${cfg.stateDir}/system/edge-runtime/tenants`
    // /proc/<pid>/environ and root are how a process of the same uid walks around a mount
    // namespace; a worker is an isolate inside the runtime process, whose file system is a
    // module graph and its own /tmp, so none of these exists for it.
    const files = [
      '/etc/hosts',
      `${tenants}/${b.ref}/functions-env.json`,
      `${tenants}/${a.ref}/functions-env.json`,
      '/proc/self/environ',
      '/proc/self/root/etc/hosts',
      `/proc/self/root${tenants}/${b.ref}/functions-env.json`,
      '/proc/1/environ',
      '/proc/self/cmdline',
    ]
    const q = files.map((f) => `p=${encodeURIComponent(f)}`).join('&')
    const r = await call(`${fnUrl(a, 'readfs')}?${q}`, { headers: bearer(a.anon) })
    assert.equal(r.status, 200, `readfs: ${r.status} ${r.text}`)
    for (const f of files) assert.match(r.json[f], /^ERROR: /, `a function read ${f}: ${r.json[f]}`)
    assert.equal(r.json.env, 'ENV: unset', 'the main service environment is visible to a worker')
    assert.match(r.json.readDir, /^ERROR: /, `a function listed /proc: ${r.json.readDir}`)
    assert.match(r.json.spawn, /^ERROR: /, `a function ran a child process: ${r.json.spawn}`)
    assert.match(r.json.envNames, /^ERROR: /, `the environment of a worker holds main service variables: ${r.json.envNames}`)
    // The one path a worker may resolve is its own /tmp, and the runtime answers with the real
    // directory behind it. A runtime upgrade could make that answer reveal more (the node's state
    // directory, a path under another project's tree, the directory of the runtime's own files):
    // it must stay a private directory the runtime made for this worker, outside the node's state.
    {
      const real = r.json.realTmp
      assert.equal(typeof real, 'string', 'readfs does not report realPathSync(/tmp)')
      if (real.startsWith('PATH: ')) {
        const path = real.slice('PATH: '.length)
        assert.match(path, /(^|\/)\.tmp[A-Za-z0-9]{6}$/, `Deno.realPathSync('/tmp') in a worker is no longer a private .tmpXXXXXX directory: ${path}`)
        for (const hidden of [cfg.stateDir, 'supavise', 'tenants', 'functions-env', a.ref, b.ref]) {
          assert.ok(!path.includes(hidden), `Deno.realPathSync('/tmp') in a worker reveals ${hidden}: ${path}`)
        }
      } else {
        assert.match(real, /^ERROR: /, `Deno.realPathSync('/tmp') in a worker: ${real}`)
      }
    }
    ok('a function cannot read the files of the node (/proc included), start a process, or see the environment of the main service; Deno.realPathSync(/tmp) names a private temp directory only')
  }

  // A worker's /tmp is its only writable place and is backed by the node's disk, which holds every
  // project's database: the node caps it ([functions] tmp_quota_mb, cfg.tmpQuotaMb). A function that
  // writes past the cap fails inside itself, and nothing else is affected.
  {
    const quota = cfg.tmpQuotaMb ?? 64
    const tmp = (p, mb) => call(`${fnUrl(p, 'tmpwrite')}?mb=${mb}`, { headers: bearer(p.anon), timeoutMs: 90_000 })
    const small = await tmp(a, 4)
    assert.equal(small.json?.ok, true, `a write within the quota: ${small.status} ${small.text}`)
    assert.equal(small.json.bytes, 4 * 1024 * 1024)
    const big = await tmp(a, quota + 16)
    assert.equal(big.status, 200, `tmpwrite: ${big.status} ${big.text}`)
    assert.equal(big.json?.ok, false, `a write of ${quota + 16} MiB to /tmp went through (quota ${quota} MiB): ${big.text}`)
    assert.match(big.json.error, /quota/i, `the write failed, but not on the quota: ${big.json.error}`)
    // The other project, and the same function again, are not affected.
    const again = await tmp(a, 4)
    assert.equal(again.json?.ok, true, `the function after its failed write: ${again.text}`)
    assert.equal((await invoke(b, 'hello')).data?.who, 'project-b')
    ok(`a function that writes past its /tmp quota (${quota} MiB) fails alone`)
  }

  // A bundled function cannot import files of the node either. A function that ran from
  // source files could: the module loader follows relative specifiers out of its directory,
  // which reached other projects' environment files and code (the reason the node bundles
  // uploaded sources in a sandbox and serves only bundles).
  {
    const roots = new Set([`${cfg.stateDir}/system/edge-runtime/tenants`])
    try {
      roots.add(`${realpathSync(cfg.stateDir)}/system/edge-runtime/tenants`)
    } catch { /* not readable from here */ }
    const targets = []
    for (const t of roots) {
      targets.push(`${t}/${b.ref}/functions-env.json`, `${t}/${b.ref}/functions/hello/.supavise-function.json`, `${t}/${a.ref}/functions-env.json`)
    }
    if (existsSync(targets[0])) ok('(the paths the function is asked to import exist)')
    const q = targets.map((t) => `t=${encodeURIComponent(t.replace(/^\//, ''))}`).join('&')
    const r = await call(`${fnUrl(a, 'escape')}?${q}`, { headers: bearer(a.anon), timeoutMs: 90_000 })
    assert.equal(r.status, 200, `escape: ${r.status} ${r.text}`)
    assert.match(r.json.control, /^IMPORTED: /, `the control import (a data: URL) failed, so the test proves nothing: ${r.json.control}`)
    const tried = Object.entries(r.json).filter(([spec]) => spec !== 'control')
    assert.ok(tried.length >= targets.length * 16, `only ${tried.length} imports were tried`)
    for (const [spec, res] of tried) assert.match(res, /^ERROR: /, `a function imported ${spec}: ${res}`)
    assert.ok(!r.text.includes(b.jwtSecret) && !r.text.includes(a.jwtSecret), 'a JWT secret reached a function response')
    ok('a function cannot import the environment files or code of any project, its own included')
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
    assert.equal(ha.data?.who, 'project-a', `A/hello after hog: ${ha.error?.name}: ${ha.error?.message} ${ha.error?.context?.status}`)
    const hb = await invoke(b, 'hello')
    assert.equal(hb.data?.who, 'project-b', `B/hello after hog: ${hb.error?.name}: ${hb.error?.message}`)
    ok('a function over its memory limit is ended; both projects answer afterwards')
  }

  // 8. One project cannot take the shared runtime. Requests over the per-project cap
  // (max_per_project; the node under test sets cfg.maxPerProject, 8, to make a small flood
  // enough: the default is 128) are refused at once with 503 PROJECT_AT_CAPACITY, and the other
  // project keeps answering while project A is flooded with runaway and memory-hungry calls.
  {
    const cap = cfg.maxPerProject ?? 0
    const flood = [
      ...Array.from({ length: cap > 0 ? cap + 4 : 12 }, () => call(fnUrl(a, 'spin'), { headers: bearer(a.anon), timeoutMs: 90_000 })),
      ...Array.from({ length: 2 }, () => call(fnUrl(a, 'hog'), { headers: bearer(a.anon), timeoutMs: 90_000 })),
    ]
    await sleep(1000)
    const during = []
    for (let i = 0; i < 6; i++) {
      during.push(await call(fnUrl(b, 'hello'), { headers: bearer(b.anon) }))
      during.push(await call(fnUrl(b, 'open')))
      await sleep(200)
    }
    for (const r of during) assert.equal(r.status, 200, `project B during the flood: ${r.status} ${r.text}`)
    assert.ok(Math.max(...during.map((r) => r.ms)) < 3000, `project B was slow during the flood: ${during.map((r) => r.ms)}`)
    const results = await Promise.all(flood)
    const refused = results.filter((r) => r.status === 503 && r.headers.get('sb-error-code') === 'PROJECT_AT_CAPACITY')
    if (cap > 0) assert.ok(refused.length >= 3, `expected the flood to be refused beyond the cap: ${results.map((r) => r.status)}`)
    for (const r of refused) {
      assert.ok(r.ms < 3000, `a refusal took ${r.ms} ms`)
      assert.equal(r.headers.get('retry-after'), '1')
    }
    await sleep(500)
    assert.equal((await invoke(a, 'hello')).data?.who, 'project-a', 'project A did not recover after the flood')
    assert.equal((await invoke(b, 'hello')).data?.who, 'project-b')
    console.log(`   (flood: ${results.map((r) => r.status).join(' ')})`)
    ok('a flood from one project is refused beyond its cap; the other project answers within bounds')
  }

  // 9. A streamed response keeps its place in the budgets until the stream ends. The function
  // answers with its headers at once and then talks for a few seconds, and the worker is busy for
  // that long. With max_per_project streams open, the project is at its cap: the next request is
  // refused, and accepted when the streams are over. (A node that released at the headers would
  // answer 200 here, and a project could hold more live workers than its budget behind them.)
  {
    const cap = cfg.maxPerProject ?? 0
    if (cap > 0) {
      // Warm the worker first. The runtime serializes the creation of a worker for a function that has
      // none: concurrent first requests wait for the first to finish and fail with "worker did not
      // respond in time" (measured with v1.77.4 locally and in CI, a property of the runtime and
      // not of the budget under test), which would make a cap's worth of long streams fail at once.
      const warm = await call(`${fnUrl(a, 'stream')}?secs=0`, { headers: bearer(a.anon) })
      assert.equal(warm.status, 200, `warming the stream function: ${warm.status} ${warm.text}`)
      const open = await Promise.all(
        Array.from({ length: cap }, () => fetch(`${fnUrl(a, 'stream')}?secs=6`, { headers: bearer(a.anon), signal: AbortSignal.timeout(60_000) })),
      )
      for (const r of open) assert.equal(r.status, 200, `stream: ${r.status}`)
      const during = await call(fnUrl(a, 'hello'), { headers: bearer(a.anon) })
      assert.equal(during.status, 503, `a request while ${cap} streams are open: ${during.status} ${during.text}`)
      assert.equal(during.headers.get('sb-error-code'), 'PROJECT_AT_CAPACITY')
      assert.equal((await invoke(b, 'hello')).data?.who, 'project-b', 'project B was affected by the streams of A')
      const texts = await Promise.all(open.map((r) => r.text()))
      for (const t of texts) assert.match(t, /tick 0\n.*tick 5\n$/s, `a stream ended early: ${JSON.stringify(t)}`)
      const after = await call(fnUrl(a, 'hello'), { headers: bearer(a.anon) })
      assert.equal(after.status, 200, `a request after the streams ended: ${after.status} ${after.text}`)
      ok(`${cap} open streams hold the project's request budget until they end`)
    }
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

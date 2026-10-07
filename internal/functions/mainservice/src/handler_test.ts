import {
  assert,
  assertEquals,
  assertFalse,
  assertRejects,
  assertStringIncludes,
} from 'jsr:@std/assert@1'
import {
  makeHandler,
  PROXY_TOKEN_HEADER,
  TENANT_HEADER,
  workerEnv,
  workerPermissions,
} from './handler.ts'
import { ProjectLimiter } from './limiter.ts'
import { ProjectStore } from './projects.ts'
import {
  draining,
  FakeError,
  FakeRuntime,
  REF_A,
  REF_B,
  signJWT,
  silent,
  writeBundle,
  writeFunction,
  writeProject,
} from './testutil.ts'
import type { Limits } from './types.ts'

const LIMITS: Limits = {
  memoryLimitMb: 128,
  workerTimeoutMs: 5000,
  requestIdleTimeoutMs: 7000,
  requestAbsentTimeoutMs: 3000,
  maxPerProject: 0,
  maxWorkers: 0,
  maxWorkersPerProject: 0,
  maxBundleBytes: 0,
  cpuTimeSoftLimitMs: 100,
  cpuTimeHardLimitMs: 200,
  tmpQuotaBytes: 8 * 1024 * 1024,
}
const TOKEN = 'proxy-token-0123456789abcdef'
const SECRET_A = 'secret-of-project-a-0123456789012345678901'
const SECRET_B = 'secret-of-project-b-0123456789012345678901'

interface Fixture {
  root: string
  rt: FakeRuntime
  /** The handler, with every response read to its end (see draining). */
  handle: (r: Request) => Promise<Response>
  /** The handler as it is: a response that is not read keeps its place in the budgets. */
  handleRaw: (r: Request) => Promise<Response>
  anonA: string
  anonB: string
  cleanup: () => Promise<void>
}

async function fixture(limits: Partial<Limits> = {}): Promise<Fixture> {
  const root = await Deno.realPath(await Deno.makeTempDir({ prefix: 'supavise-handler-test-' }))
  await writeProject(root, REF_A, SECRET_A, { MY_SECRET: 'a-secret', SUPABASE_URL: 'evil' }, {
    SUPABASE_ANON_KEY: 'anon-a',
    SUPABASE_DB_URL: 'postgres://a',
  })
  await writeProject(root, REF_B, SECRET_B, { MY_SECRET: 'b-secret' }, {
    SUPABASE_ANON_KEY: 'anon-b',
    SUPABASE_DB_URL: 'postgres://b',
  })
  await writeFunction(root, REF_A, 'hello', { verifyJwt: true })
  await writeFunction(root, REF_B, 'hello', { verifyJwt: true })
  await writeFunction(root, REF_A, 'open', { verifyJwt: false })
  const rt = new FakeRuntime()
  const handleRaw = makeHandler({
    store: new ProjectStore(root),
    runtime: rt,
    limits: { ...LIMITS, ...limits },
    port: '9000',
    proxyToken: TOKEN,
    log: silent,
  })
  return {
    root,
    rt,
    handle: draining(handleRaw),
    handleRaw,
    anonA: await signJWT(SECRET_A, { role: 'anon' }),
    anonB: await signJWT(SECRET_B, { role: 'anon' }),
    cleanup: () => Deno.remove(root, { recursive: true }),
  }
}

const req = (
  path: string,
  ref: string | null,
  headers: Record<string, string> = {},
  init: RequestInit = {},
) =>
  new Request(`http://runtime${path}`, {
    ...init,
    headers: {
      [PROXY_TOKEN_HEADER]: TOKEN,
      ...(ref ? { [TENANT_HEADER]: ref } : {}),
      ...headers,
    },
  })

Deno.test('the health path needs no project', async () => {
  const f = await fixture()
  try {
    const res = await f.handle(req('/_internal/health', null))
    assertEquals(res.status, 200)
    assertEquals(await res.json(), { message: 'ok' })
  } finally {
    await f.cleanup()
  }
})

Deno.test('a request without a valid project reference is refused', async () => {
  const f = await fixture()
  try {
    for (const ref of [null, '', 'system', 'AAAAAAAAAAAAAAAAAAAA', '../../etc']) {
      const res = await f.handle(req('/open', ref))
      assertEquals(res.status, 400, String(ref))
    }
    assertEquals(f.rt.created.length, 0)
  } finally {
    await f.cleanup()
  }
})

Deno.test('unknown functions and projects are 404 with the upstream error shape', async () => {
  const f = await fixture()
  try {
    for (const path of ['/', '/nope', '/.supavise-function.json', '/..', '/1abc']) {
      const res = await f.handle(req(path, REF_A, { authorization: `Bearer ${f.anonA}` }))
      assertEquals(res.status, 404, path)
      assertEquals(res.headers.get('sb-error-code'), 'NOT_FOUND')
      assertEquals(await res.json(), {
        code: 'NOT_FOUND',
        message: 'Requested function was not found',
      })
    }
    // A project that never deployed anything.
    const res = await f.handle(req('/hello', 'cccccccccccccccccccc'))
    assertEquals(res.status, 404)
    // A function of A is not found under B.
    assertEquals((await f.handle(req('/open', REF_B))).status, 404)
    assertEquals(f.rt.created.length, 0)
  } finally {
    await f.cleanup()
  }
})

Deno.test('verify_jwt checks the token against the secret of the project in the header', async () => {
  const f = await fixture()
  try {
    const none = await f.handle(req('/hello', REF_A))
    assertEquals(none.status, 401)
    assertEquals(none.headers.get('sb-error-code'), 'UNAUTHORIZED_NO_AUTH_HEADER')
    const body = await none.json()
    assertEquals(body.msg, body.message)
    const bad = await f.handle(req('/hello', REF_A, { authorization: 'Bearer not.a.jwt' }))
    assertEquals(bad.status, 401)
    // A's token on B's route is another project's token.
    const cross = await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonA}` }))
    assertEquals(cross.status, 401)
    assertEquals(cross.headers.get('sb-error-code'), 'UNAUTHORIZED_LEGACY_JWT')
    assertEquals(f.rt.created.length, 0)
    const ok = await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))
    assertEquals(ok.status, 200)
    assertEquals(await ok.text(), 'worker ok')
    // The opaque-key path: the proxy put the JWT in sb-api-key.
    const viaProxy = await f.handle(
      req('/hello', REF_B, { authorization: 'Bearer sb_publishable_x', 'sb-api-key': f.anonB }),
    )
    assertEquals(viaProxy.status, 200)
  } finally {
    await f.cleanup()
  }
})

Deno.test('a function deployed without verify_jwt takes any caller, and OPTIONS is never checked', async () => {
  const f = await fixture()
  try {
    assertEquals((await f.handle(req('/open', REF_A))).status, 200)
    assertEquals((await f.handle(req('/hello', REF_A, {}, { method: 'OPTIONS' }))).status, 200)
  } finally {
    await f.cleanup()
  }
})

Deno.test("each worker gets its own project's environment and nothing of the main service", async () => {
  const f = await fixture()
  Deno.env.set('SUPAVISE_TEST_MAIN_ONLY', 'must-not-leak')
  try {
    await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))
    await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))
    const [a, b] = f.rt.created
    const envA = Object.fromEntries(a.envVars)
    const envB = Object.fromEntries(b.envVars)
    assertEquals(envA.MY_SECRET, 'a-secret')
    assertEquals(envB.MY_SECRET, 'b-secret')
    assertEquals(envA.SUPABASE_ANON_KEY, 'anon-a')
    assertEquals(envB.SUPABASE_DB_URL, 'postgres://b')
    assertEquals(envA.SUPABASE_URL, `http://${REF_A}.api.test`) // not the secret named SUPABASE_URL
    assertEquals(envA.SUPABASE_FUNCTION_SLUG, 'hello')
    for (const vars of [envA, envB]) {
      assertFalse('SUPAVISE_TEST_MAIN_ONLY' in vars)
      assertFalse('PATH' in vars)
      assertFalse('HOME' in vars)
      assertFalse(
        Object.values(vars).some((v) => v.includes(SECRET_A) || v.includes(SECRET_B)),
        'jwt secret leaked',
      )
    }
    // Same slug, different projects: separate pools, separate paths, own project reference.
    assert(a.poolKey !== b.poolKey)
    assertStringIncludes(a.poolKey, REF_A)
    assertStringIncludes(b.poolKey, REF_B)
    assert(a.servicePath.includes(`/${REF_A}/`) && !a.servicePath.includes(REF_B))
    assert(b.servicePath.includes(`/${REF_B}/`) && !b.servicePath.includes(REF_A))
    assertEquals(a.context.projectRef, REF_A)
    assertEquals(b.context.projectRef, REF_B)
    assertEquals(a.maybeEntrypoint, 'file:///src/hello/index.ts')
  } finally {
    Deno.env.delete('SUPAVISE_TEST_MAIN_ONLY')
    await f.cleanup()
  }
})

Deno.test("workers get the node's limits, an explicit permission set and a fresh pool after a deployment", async () => {
  const f = await fixture()
  try {
    const call = () => f.handle(req('/open', REF_A))
    await call()
    const first = f.rt.created[0]
    assertEquals(first.memoryLimitMb, 128)
    assertEquals(first.workerTimeoutMs, 5000)
    assertEquals(first.cpuTimeSoftLimitMs, 100)
    assertEquals(first.cpuTimeHardLimitMs, 200)
    assertEquals(first.context.supervisor.requestAbsentTimeoutMs, 3000)
    assertEquals(first.forceCreate, false)
    assertEquals(first.permissions?.deny_net, [
      '127.0.0.1:9000',
      'localhost:9000',
      '[::1]:9000',
      '0.0.0.0:9000',
      '[::]:9000',
    ])
    await call()
    assertEquals(f.rt.created[1].poolKey, first.poolKey)
    await writeFunction(f.root, REF_A, 'open', { verifyJwt: false, version: 2 })
    await call()
    assert(
      f.rt.created[2].poolKey !== first.poolKey,
      'a new deployment must not reuse the old pool',
    )
    assertEquals(f.rt.created[2].poolKey.includes(':2:'), true)
    // New secrets reach new workers, too.
    await new Promise((r) => setTimeout(r, 15))
    await writeProject(f.root, REF_A, SECRET_A, { MY_SECRET: 'rotated' })
    await call()
    assert(f.rt.created[3].poolKey !== f.rt.created[2].poolKey)
    assertEquals(Object.fromEntries(f.rt.created[3].envVars).MY_SECRET, 'rotated')
  } finally {
    await f.cleanup()
  }
})

Deno.test('workerEnv and workerPermissions', () => {
  assertEquals(
    Object.fromEntries(
      workerEnv('s', {
        jwtSecret: 'x',
        supabase: { SUPABASE_URL: 'u' },
        secrets: { supabase_x: '1', K: 'v' },
      }),
    ),
    { SUPABASE_URL: 'u', K: 'v', SUPABASE_FUNCTION_SLUG: 's' },
  )
  assertEquals(workerPermissions('')?.deny_net, undefined)
  assertEquals(workerPermissions('x;y')?.deny_net, undefined)
  assertEquals(workerPermissions('9000')?.allow_net, [])
})

Deno.test('the worker sees neither internal headers nor the original request', async () => {
  const f = await fixture()
  try {
    await f.handle(
      req(
        '/open',
        REF_A,
        { 'sb-api-key': 'jwt', 'x-custom': 'keep', authorization: 'Bearer x' },
        { method: 'POST', body: 'payload' },
      ),
    )
    const seen = f.rt.forwarded[0]
    assertEquals(seen.headers.get('sb-api-key'), null)
    assertEquals(seen.headers.get(TENANT_HEADER), null)
    assertEquals(seen.headers.get('x-custom'), 'keep')
    assertEquals(await seen.text(), 'payload')
    assertEquals(f.rt.tags, 1)
  } finally {
    await f.cleanup()
  }
})

Deno.test('runtime failures become the upstream error responses', async () => {
  const f = await fixture()
  try {
    const cases: [string, number, string][] = [
      ['boot', 503, 'BOOT_ERROR'],
      ['cancelled', 546, 'WORKER_RESOURCE_LIMIT'],
      ['idle', 504, 'IDLE_TIMEOUT'],
      ['invalid', 500, 'WORKER_ERROR'],
    ]
    for (const [kind, status, code] of cases) {
      f.rt.failWith = new FakeError(kind)
      const res = await f.handle(req('/open', REF_A))
      assertEquals(res.status, status, kind)
      assertEquals(res.headers.get('sb-error-code'), code, kind)
    }
    f.rt.failWith = new FakeError(
      'invalid',
      'status 700 is not equal to 101 and outside the range [200, 599]',
    )
    assertEquals(
      (await f.handle(req('/open', REF_A))).headers.get('sb-error-code'),
      'INVALID_RESPONSE_STATUS_CODE',
    )
    f.rt.failWith = new RangeError(
      'init["status"] is not equal to 101 and outside the range [200, 599]',
    )
    assertEquals(
      (await f.handle(req('/open', REF_A))).headers.get('sb-error-code'),
      'INVALID_RESPONSE_STATUS_CODE',
    )
    f.rt.failWith = new Error('whatever')
    const other = await f.handle(req('/open', REF_A))
    assertEquals(other.status, 500)
    assertEquals(other.headers.get('sb-error-code'), 'EDGE_FUNCTION_ERROR')
    f.rt.failWith = new FakeError('idle')
    const idle = await f.handle(req('/open', REF_A))
    assertStringIncludes((await idle.json()).message, '(7s)')
  } finally {
    await f.cleanup()
  }
})

Deno.test('a retired worker is retried a bounded number of times', async () => {
  const f = await fixture()
  try {
    f.rt.failWith = new FakeError('retired')
    f.rt.failTimes = 2
    const ok = await f.handle(req('/open', REF_A, {}, { method: 'POST', body: 'again' }))
    assertEquals(ok.status, 200)
    assertEquals(f.rt.created.length, 3)
    assertEquals(await f.rt.forwarded[2].text(), 'again')
    f.rt.created.length = 0
    f.rt.failTimes = -1 // always fails
    const dead = await f.handle(req('/open', REF_A))
    assertEquals(dead.status, 500)
    assertEquals(dead.headers.get('sb-error-code'), 'WORKER_ERROR')
    assertEquals(f.rt.created.length, 4) // the first try and three retries
  } finally {
    await f.cleanup()
  }
})

Deno.test('a 5xx from the function is tagged, a 4xx is left alone', async () => {
  const f = await fixture()
  try {
    f.rt.respond = () =>
      new Response('boom', { status: 502, headers: { 'Access-Control-Expose-Headers': 'x-a' } })
    const bad = await f.handle(req('/open', REF_A))
    assertEquals(bad.status, 502)
    assertEquals(bad.headers.get('sb-error-code'), 'EDGE_FUNCTION_ERROR')
    assertEquals(bad.headers.get('Access-Control-Expose-Headers'), 'x-a, sb-error-code')
    assertEquals(await bad.text(), 'boom')
    f.rt.respond = () => new Response('nope', { status: 418 })
    const teapot = await f.handle(req('/open', REF_A))
    assertEquals(teapot.status, 418)
    assertEquals(teapot.headers.get('sb-error-code'), null)
  } finally {
    await f.cleanup()
  }
})

Deno.test('a broken deployment file is a boot error, not a crash', async () => {
  const f = await fixture()
  try {
    await Deno.writeTextFile(`${f.root}/${REF_B}/functions-env.json`, '{broken')
    const res = await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))
    assertEquals(res.status, 503)
    assertEquals(res.headers.get('sb-error-code'), 'BOOT_ERROR')
    // Project A is not affected.
    const a = await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))
    assertEquals(a.status, 200)
  } finally {
    await f.cleanup()
  }
})

Deno.test('a function runs from its bundle, in its own generation directory', async () => {
  const f = await fixture()
  try {
    await writeBundle(f.root, REF_A, 'bundled', new TextEncoder().encode('ESZIP2.3 bytes'), {
      verifyJwt: false,
    })
    assertEquals((await f.handle(req('/bundled', REF_A))).status, 200)
    const w = f.rt.created[0]
    assertEquals(new TextDecoder().decode(w.maybeEszip), 'ESZIP2.3 bytes')
    assertEquals(w.maybeEntrypoint, 'file:///src/bundled/index.ts')
    assert(w.servicePath.includes(`/${REF_A}/functions/.gen/`))
  } finally {
    await f.cleanup()
  }
})

Deno.test('a function that was stored as sources is not served (it could read other projects)', async () => {
  const f = await fixture()
  try {
    const gen = `${f.root}/${REF_A}/functions/.gen/src-1-x`
    await Deno.mkdir(gen, { recursive: true })
    await Deno.writeTextFile(`${gen}/index.ts`, 'Deno.serve(() => new Response("x"))')
    await Deno.writeTextFile(
      `${gen}/.supavise-function.json`,
      JSON.stringify({ slug: 'src', version: 1, verify_jwt: false, entrypoint: 'index.ts' }),
    )
    await Deno.symlink(gen, `${f.root}/${REF_A}/functions/src`)
    const res = await f.handle(req('/src', REF_A))
    assertEquals(res.status, 404)
    assertEquals(f.rt.created.length, 0)
  } finally {
    await f.cleanup()
  }
})

Deno.test('a project over its cap is refused at once and the other project is not affected', async () => {
  const f = await fixture({ maxPerProject: 2, maxWorkersPerProject: 3 })
  try {
    let open!: () => void
    const gate = new Promise<void>((r) => open = r)
    f.rt.respond = async () => {
      await gate
      return new Response('slow')
    }
    const slow = [1, 2].map(() =>
      f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))
    )
    await new Promise((r) => setTimeout(r, 20))
    const refused = await f.handle(req('/open', REF_A))
    assertEquals(refused.status, 503)
    assertEquals(refused.headers.get('sb-error-code'), 'PROJECT_AT_CAPACITY')
    assertEquals(refused.headers.get('retry-after'), '1')
    // Project B has its own budget.
    f.rt.respond = () => new Response('fast')
    const b = await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))
    assertEquals(b.status, 200)
    open()
    for (const r of await Promise.all(slow)) assertEquals(r.status, 200)
    // The slots are free again once the requests ended.
    assertEquals(
      (await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))).status,
      200,
    )
  } finally {
    await f.cleanup()
  }
})

Deno.test('a project cannot hold more live workers than its cap, and another project is unaffected', async () => {
  const f = await fixture({ maxWorkersPerProject: 1 })
  try {
    assertEquals(
      (await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))).status,
      200,
    )
    // The same function again reuses its worker; a second function would need another.
    assertEquals(
      (await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))).status,
      200,
    )
    const refused = await f.handle(req('/open', REF_A))
    assertEquals(refused.status, 503)
    assertEquals(refused.headers.get('sb-error-code'), 'PROJECT_AT_CAPACITY')
    assertEquals(
      (await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))).status,
      200,
    )
  } finally {
    await f.cleanup()
  }
})

Deno.test('a request without the proxy secret is refused, whatever project it names', async () => {
  const f = await fixture()
  try {
    const bearer = { authorization: `Bearer ${f.anonA}` }
    // What a function worker can do: reach the port and choose the project itself.
    for (const token of ['', 'wrong', TOKEN + 'x', TOKEN.slice(1)]) {
      const res = await f.handle(
        new Request('http://runtime/hello', {
          headers: { [TENANT_HEADER]: REF_A, [PROXY_TOKEN_HEADER]: token, ...bearer },
        }),
      )
      assertEquals(res.status, 403, `token ${JSON.stringify(token)}`)
    }
    const none = await f.handle(
      new Request('http://runtime/open', { headers: { [TENANT_HEADER]: REF_A } }),
    )
    assertEquals(none.status, 403)
    assertEquals(f.rt.created.length, 0, 'no worker was created for a refused request')
    // The health probe of the fleet carries no secret.
    assertEquals((await f.handle(new Request('http://runtime/_internal/health'))).status, 200)
    // With the secret the same request is served, and the worker never sees the secret.
    assertEquals((await f.handle(req('/hello', REF_A, bearer))).status, 200)
    const seen = f.rt.forwarded[0]
    assertFalse(seen.headers.has(PROXY_TOKEN_HEADER))
    assertFalse(seen.headers.has(TENANT_HEADER))
  } finally {
    await f.cleanup()
  }
})

Deno.test('the runtime-wide worker budget holds across projects: over-budget requests get 503, workers stay within it', async () => {
  // Budget of 6 workers for the whole runtime, at most 4 for one project. Two projects
  // warm 5 and 5 functions each: A gets 4 (its share), B gets 2 (what is left), and every
  // other function is refused before a worker is created.
  const f = await fixture({ maxWorkers: 6, maxWorkersPerProject: 4, memoryLimitMb: 250 })
  try {
    const slugs = ['f1', 'f2', 'f3', 'f4', 'f5']
    for (const ref of [REF_A, REF_B]) {
      for (const slug of slugs) await writeFunction(f.root, ref, slug, { verifyJwt: false })
    }
    const status = async (ref: string, slug: string) =>
      (await f.handle(req(`/${slug}`, ref))).status
    const a = []
    for (const slug of slugs) a.push(await status(REF_A, slug))
    assertEquals(a, [200, 200, 200, 200, 503])
    const b = []
    for (const slug of slugs) b.push(await status(REF_B, slug))
    assertEquals(b, [200, 200, 503, 503, 503])
    const keys = new Set(f.rt.created.map((o) => o.poolKey))
    assertEquals(keys.size, 6, 'six distinct workers in all, the budget')
    for (const o of f.rt.created) assertEquals(o.memoryLimitMb, 250)
    const refused = await f.handle(req('/f5', REF_B))
    assertEquals(refused.headers.get('sb-error-code'), 'PROJECT_AT_CAPACITY')
    assertStringIncludes((await refused.json()).message, 'runtime')
    // Warm functions keep working, in both projects, while the budget is spent.
    assertEquals(await status(REF_A, 'f1'), 200)
    assertEquals(await status(REF_B, 'f2'), 200)
    assertEquals(new Set(f.rt.created.map((o) => o.poolKey)).size, 6)
  } finally {
    await f.cleanup()
  }
})

Deno.test('a redeployment counts as a new worker until the old one has idled out', async () => {
  let t = 0
  const root = await Deno.realPath(await Deno.makeTempDir({ prefix: 'supavise-handler-test-' }))
  try {
    await writeProject(root, REF_A, SECRET_A)
    await writeFunction(root, REF_A, 'a', { verifyJwt: false })
    await writeFunction(root, REF_A, 'b', { verifyJwt: false })
    const rt = new FakeRuntime()
    const limiter = new ProjectLimiter({
      maxRequests: 0,
      maxWorkers: 2,
      workerTtlMs: 1000,
      now: () => t,
    })
    const handle = draining(makeHandler({
      store: new ProjectStore(root),
      runtime: rt,
      limits: LIMITS,
      limiter,
      log: silent,
    }))
    const get = async (slug: string) => (await handle(req(`/${slug}`, REF_A))).status
    assertEquals([await get('a'), await get('b')], [200, 200])
    await writeFunction(root, REF_A, 'a', { verifyJwt: false, version: 2 })
    assertEquals(await get('a'), 503, 'old and new generation of a, and b: three workers')
    t = 2000
    assertEquals(await get('a'), 200)
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

/** A response body the test ends, fails or cancels by hand: a function that streams. */
function controlledStream() {
  let ctl!: ReadableStreamDefaultController<Uint8Array>
  const enc = new TextEncoder()
  const state = { cancelled: false }
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      ctl = c
    },
    cancel() {
      state.cancelled = true
    },
  })
  return {
    state,
    response: () => new Response(stream, { headers: { 'content-type': 'text/event-stream' } }),
    chunk: (s: string) => ctl.enqueue(enc.encode(s)),
    end: () => ctl.close(),
    fail: (e: Error) => ctl.error(e),
  }
}

Deno.test('every worker gets a quota for its /tmp, unless the node switches it off', async () => {
  const f = await fixture()
  try {
    await f.handle(req('/open', REF_A))
    await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))
    assertEquals(f.rt.created.length, 2)
    for (const o of f.rt.created) assertEquals(o.tmpFsConfig, { quota: 8 * 1024 * 1024 })
  } finally {
    await f.cleanup()
  }
  const off = await fixture({ tmpQuotaBytes: 0 })
  try {
    await off.handle(req('/open', REF_A))
    assertEquals(off.rt.created[0].tmpFsConfig, undefined)
  } finally {
    await off.cleanup()
  }
})

Deno.test('a function that fails on its /tmp quota fails alone: the other project and the next request are fine', async () => {
  const f = await fixture()
  try {
    // What the runtime does when a worker writes past its quota: the write throws inside the
    // function, which answers with an error (or the worker dies); nothing else is affected.
    f.rt.respond = (r) =>
      new URL(r.url).pathname === '/open'
        ? Response.json({ error: 'filesystem quota exceeded' }, { status: 500 })
        : new Response('fine')
    const bad = await f.handle(req('/open', REF_A))
    assertEquals(bad.status, 500)
    assertEquals(bad.headers.get('sb-error-code'), 'EDGE_FUNCTION_ERROR')
    const other = await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))
    assertEquals(await other.text(), 'fine')
    assertEquals(
      (await f.handle(req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }))).status,
      200,
    )
  } finally {
    await f.cleanup()
  }
})

Deno.test('a streaming response keeps its request slot until the stream ends', async () => {
  const f = await fixture({ maxPerProject: 1 })
  try {
    const s = controlledStream()
    f.rt.respond = () => s.response()
    // The headers are back and the function is still talking.
    const first = await f.handleRaw(req('/open', REF_A))
    assertEquals(first.status, 200)
    s.chunk('data: 1\n\n')
    const refused = await f.handleRaw(req('/open', REF_A))
    assertEquals(refused.status, 503)
    assertEquals(refused.headers.get('sb-error-code'), 'PROJECT_AT_CAPACITY')
    // Another project is not affected.
    f.rt.respond = () => new Response('fast')
    assertEquals(
      (await f.handle(req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }))).status,
      200,
    )
    // The stream ends and the client has read it: the slot is free.
    s.chunk('data: 2\n\n')
    s.end()
    assertEquals(await first.text(), 'data: 1\n\ndata: 2\n\n')
    assertEquals((await f.handle(req('/open', REF_A))).status, 200)
  } finally {
    await f.cleanup()
  }
})

Deno.test('streams keep their workers in the budgets: max_workers_per_project and max_workers hold while they run', async () => {
  // One worker for the whole runtime, one per project, no cap on requests: every function that
  // would need another worker is refused for as long as a stream holds the only one.
  const f = await fixture({ maxWorkers: 1, maxWorkersPerProject: 1, maxPerProject: 0 })
  try {
    const s = controlledStream()
    f.rt.respond = () => s.response()
    const first = await f.handleRaw(req('/open', REF_A))
    assertEquals(first.status, 200)
    f.rt.respond = () => new Response('fast')
    const sameProject = await f.handleRaw(
      req('/hello', REF_A, { authorization: `Bearer ${f.anonA}` }),
    )
    assertEquals(sameProject.status, 503)
    assertStringIncludes((await sameProject.json()).message, 'too many functions')
    const otherProject = await f.handleRaw(
      req('/hello', REF_B, { authorization: `Bearer ${f.anonB}` }),
    )
    assertEquals(otherProject.status, 503)
    assertStringIncludes((await otherProject.json()).message, 'runtime')
    assertEquals(f.rt.created.length, 1, 'no second worker was created while the stream ran')
    s.end()
    await first.text()
    // Free again: the first function's worker is still the live one, and project B may now
    // not take a second one until it idles out, but the same function answers.
    f.rt.respond = () => new Response('again')
    assertEquals(await (await f.handle(req('/open', REF_A))).text(), 'again')
  } finally {
    await f.cleanup()
  }
})

Deno.test('a worker with a stream open is still live after its idle time; it is forgotten after the stream ends and the idle time passes', async () => {
  let t = 0
  const root = await Deno.realPath(await Deno.makeTempDir({ prefix: 'supavise-handler-test-' }))
  try {
    await writeProject(root, REF_A, SECRET_A)
    await writeFunction(root, REF_A, 'a', { verifyJwt: false })
    await writeFunction(root, REF_A, 'b', { verifyJwt: false })
    const rt = new FakeRuntime()
    const limiter = new ProjectLimiter({
      maxRequests: 0,
      maxWorkers: 1,
      workerTtlMs: 1000,
      now: () => t,
    })
    const raw = makeHandler({
      store: new ProjectStore(root),
      runtime: rt,
      limits: LIMITS,
      limiter,
      log: silent,
    })
    const s = controlledStream()
    rt.respond = () => s.response()
    const stream = await raw(req('/a', REF_A))
    rt.respond = () => new Response('b')
    // Far past the time after which an idle worker is assumed gone: this one is not idle.
    t = 600_000
    assertEquals((await draining(raw)(req('/b', REF_A))).status, 503)
    s.end()
    await stream.text()
    assertEquals(
      (await draining(raw)(req('/b', REF_A))).status,
      503,
      'the worker idles on after the stream',
    )
    t = 600_000 + 1001
    assertEquals((await draining(raw)(req('/b', REF_A))).status, 200)
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

Deno.test('a client that goes away mid-stream, or a worker that dies mid-stream, frees the slot', async () => {
  const f = await fixture({ maxPerProject: 1 })
  try {
    // The client cancels the response (the HTTP server does this when the connection closes).
    const s1 = controlledStream()
    f.rt.respond = () => s1.response()
    const first = await f.handleRaw(req('/open', REF_A))
    s1.chunk('x')
    assertEquals((await f.handleRaw(req('/open', REF_A))).status, 503)
    await first.body!.cancel('connection closed')
    assert(s1.state.cancelled, 'the worker was not told that the client left')
    f.rt.respond = () => new Response('next')
    assertEquals(await (await f.handle(req('/open', REF_A))).text(), 'next')

    // The worker is retired mid-stream: the body fails, the client sees a failed body, and the slot is free.
    const s2 = controlledStream()
    f.rt.respond = () => s2.response()
    const second = await f.handleRaw(req('/open', REF_A))
    assertEquals((await f.handleRaw(req('/open', REF_A))).status, 503)
    s2.fail(new Error('worker retired'))
    await assertRejects(() => second.text(), Error, 'worker retired')
    f.rt.respond = () => new Response('after')
    assertEquals(await (await f.handle(req('/open', REF_A))).text(), 'after')
  } finally {
    await f.cleanup()
  }
})

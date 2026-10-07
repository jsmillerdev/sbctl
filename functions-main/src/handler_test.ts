import { assert, assertEquals, assertFalse, assertStringIncludes } from 'jsr:@std/assert@1'
import { makeHandler, TENANT_HEADER, workerEnv, workerPermissions } from './handler.ts'
import { ProjectStore } from './projects.ts'
import {
  FakeError,
  FakeRuntime,
  REF_A,
  REF_B,
  signJWT,
  silent,
  writeFunction,
  writeProject,
} from './testutil.ts'
import type { Limits } from './types.ts'

const LIMITS: Limits = {
  memoryLimitMb: 128,
  workerTimeoutMs: 5000,
  requestIdleTimeoutMs: 7000,
  requestAbsentTimeoutMs: 3000,
  cpuTimeSoftLimitMs: 100,
  cpuTimeHardLimitMs: 200,
}
const SECRET_A = 'secret-of-project-a-0123456789012345678901'
const SECRET_B = 'secret-of-project-b-0123456789012345678901'

interface Fixture {
  root: string
  rt: FakeRuntime
  handle: (r: Request) => Promise<Response>
  anonA: string
  anonB: string
  cleanup: () => Promise<void>
}

async function fixture(): Promise<Fixture> {
  const root = await Deno.realPath(await Deno.makeTempDir({ prefix: 'sbctl-handler-test-' }))
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
  const handle = makeHandler({
    store: new ProjectStore(root),
    runtime: rt,
    limits: LIMITS,
    port: '9000',
    log: silent,
  })
  return {
    root,
    rt,
    handle,
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
    headers: { ...(ref ? { [TENANT_HEADER]: ref } : {}), ...headers },
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
    for (const path of ['/', '/nope', '/.sbctl-function.json', '/..', '/1abc']) {
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
  Deno.env.set('SBCTL_TEST_MAIN_ONLY', 'must-not-leak')
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
      assertFalse('SBCTL_TEST_MAIN_ONLY' in vars)
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
    assertEquals(a.maybeEntrypoint, `file://${a.servicePath}/index.ts`)
  } finally {
    Deno.env.delete('SBCTL_TEST_MAIN_ONLY')
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

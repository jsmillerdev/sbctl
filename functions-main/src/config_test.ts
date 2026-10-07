import { assertEquals, assertStringIncludes, assertThrows } from 'jsr:@std/assert@1'
import { loadConfig, NO_PROXY_TOKEN_ENV } from './config.ts'

// The defaults of everything but the secret, which has none: supavise always passes one.
const withToken = (vars: Record<string, string> = {}) => (n: string) =>
  ({ SUPAVISE_FUNCTIONS_PROXY_TOKEN: 'secret', ...vars })[n]

Deno.test('loadConfig applies the defaults', () => {
  const c = loadConfig(withToken())
  assertEquals(c.root, '/var/lib/supavise/system/edge-runtime/tenants')
  assertEquals(c.proxyToken, 'secret')
  assertEquals(c.limits, {
    memoryLimitMb: 256,
    workerTimeoutMs: 400_000,
    requestIdleTimeoutMs: 150_000,
    requestAbsentTimeoutMs: 60_000,
    maxPerProject: 128,
    maxWorkers: 16,
    maxWorkersPerProject: 8,
    maxBundleBytes: 192 * 1024 * 1024,
    cpuTimeSoftLimitMs: 1000,
    cpuTimeHardLimitMs: 2000,
    tmpQuotaBytes: 64 * 1024 * 1024,
  })
})

Deno.test('loadConfig reads the variables the unit passes', () => {
  const vars: Record<string, string> = {
    SUPAVISE_FUNCTIONS_ROOT: '/x/tenants/',
    SUPAVISE_FUNCTIONS_MEMORY_MB: '64',
    SUPAVISE_FUNCTIONS_WALL_CLOCK_SEC: '10',
    SUPAVISE_FUNCTIONS_IDLE_TIMEOUT_SEC: '5',
    SUPAVISE_FUNCTIONS_WORKER_IDLE_SEC: '2',
    SUPAVISE_FUNCTIONS_MAX_PER_PROJECT: '0',
    SUPAVISE_FUNCTIONS_MAX_WORKERS: '6',
    SUPAVISE_FUNCTIONS_MAX_WORKERS_PER_PROJECT: '3',
    SUPAVISE_FUNCTIONS_MAX_BUNDLE_MB: '10',
    SUPAVISE_FUNCTIONS_PROXY_TOKEN: ' tok\n',
    SUPAVISE_FUNCTIONS_CPU_SOFT_MS: '0',
    SUPAVISE_FUNCTIONS_CPU_HARD_MS: '',
    SUPAVISE_FUNCTIONS_TMP_QUOTA_MB: '5',
  }
  const c = loadConfig((n) => vars[n])
  assertEquals(c.root, '/x/tenants')
  assertEquals(c.limits.memoryLimitMb, 64)
  assertEquals(c.limits.workerTimeoutMs, 10_000)
  assertEquals(c.limits.requestIdleTimeoutMs, 5000)
  assertEquals(c.limits.requestAbsentTimeoutMs, 2000)
  assertEquals(c.limits.maxPerProject, 0)
  assertEquals(c.limits.maxWorkers, 6)
  assertEquals(c.limits.maxWorkersPerProject, 3)
  assertEquals(c.limits.maxBundleBytes, 10 * 1024 * 1024)
  assertEquals(c.proxyToken, 'tok')
  assertEquals(c.limits.cpuTimeSoftLimitMs, 0)
  assertEquals(c.limits.cpuTimeHardLimitMs, 2000)
  assertEquals(c.limits.tmpQuotaBytes, 5 * 1024 * 1024)
  // 0 switches the quota off.
  assertEquals(
    loadConfig(withToken({ SUPAVISE_FUNCTIONS_TMP_QUOTA_MB: '0' })).limits.tmpQuotaBytes,
    0,
  )
})

Deno.test('loadConfig rejects nonsense', () => {
  assertThrows(() => loadConfig(withToken({ SUPAVISE_FUNCTIONS_MEMORY_MB: 'lots' })))
  assertThrows(() => loadConfig(withToken({ SUPAVISE_FUNCTIONS_MEMORY_MB: '-1' })))
})

Deno.test('loadConfig refuses to start without the proxy secret unless a dev flag says so', () => {
  // Missing, empty and blank are all "no secret": the service would serve every caller that
  // can reach its port, and a function worker can.
  for (const token of [undefined, '', '  \n']) {
    const err = assertThrows(
      () => loadConfig((n) => (n === 'SUPAVISE_FUNCTIONS_PROXY_TOKEN' ? token : undefined)),
      Error,
      'SUPAVISE_FUNCTIONS_PROXY_TOKEN is empty',
    )
    assertStringIncludes(err.message, NO_PROXY_TOKEN_ENV)
  }
  // Only the explicit flag, with exactly "1", lets a development machine run without it.
  for (const flag of ['0', 'true', 'yes', '', ' 1']) {
    assertThrows(() => loadConfig((n) => (n === NO_PROXY_TOKEN_ENV ? flag : undefined)))
  }
  const dev = loadConfig((n) => (n === NO_PROXY_TOKEN_ENV ? '1' : undefined))
  assertEquals(dev.proxyToken, '')
  // A secret and the flag together: the secret still counts.
  assertEquals(
    loadConfig((n) => ({ SUPAVISE_FUNCTIONS_PROXY_TOKEN: 'tok', [NO_PROXY_TOKEN_ENV]: '1' })[n])
      .proxyToken,
    'tok',
  )
})

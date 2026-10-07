import { assertEquals, assertThrows } from 'jsr:@std/assert@1'
import { loadConfig } from './config.ts'

Deno.test('loadConfig applies the defaults', () => {
  const c = loadConfig(() => undefined)
  assertEquals(c.root, '/var/lib/sbctl/system/edge-runtime/tenants')
  assertEquals(c.proxyToken, '')
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
  })
})

Deno.test('loadConfig reads the variables the unit passes', () => {
  const vars: Record<string, string> = {
    SBCTL_FUNCTIONS_ROOT: '/x/tenants/',
    SBCTL_FUNCTIONS_MEMORY_MB: '64',
    SBCTL_FUNCTIONS_WALL_CLOCK_SEC: '10',
    SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC: '5',
    SBCTL_FUNCTIONS_WORKER_IDLE_SEC: '2',
    SBCTL_FUNCTIONS_MAX_PER_PROJECT: '0',
    SBCTL_FUNCTIONS_MAX_WORKERS: '6',
    SBCTL_FUNCTIONS_MAX_WORKERS_PER_PROJECT: '3',
    SBCTL_FUNCTIONS_MAX_BUNDLE_MB: '10',
    SBCTL_FUNCTIONS_PROXY_TOKEN: ' tok\n',
    SBCTL_FUNCTIONS_CPU_SOFT_MS: '0',
    SBCTL_FUNCTIONS_CPU_HARD_MS: '',
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
})

Deno.test('loadConfig rejects nonsense', () => {
  assertThrows(() => loadConfig((n) => (n === 'SBCTL_FUNCTIONS_MEMORY_MB' ? 'lots' : undefined)))
  assertThrows(() => loadConfig((n) => (n === 'SBCTL_FUNCTIONS_MEMORY_MB' ? '-1' : undefined)))
})

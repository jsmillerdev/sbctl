import { assertEquals, assertThrows } from 'jsr:@std/assert@1'
import { loadConfig } from './config.ts'

Deno.test('loadConfig applies the defaults', () => {
  const c = loadConfig(() => undefined)
  assertEquals(c.root, '/var/lib/sbctl/system/edge-runtime/tenants')
  assertEquals(c.limits, {
    memoryLimitMb: 256,
    workerTimeoutMs: 400_000,
    requestIdleTimeoutMs: 150_000,
    requestAbsentTimeoutMs: 60_000,
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
    SBCTL_FUNCTIONS_CPU_SOFT_MS: '0',
    SBCTL_FUNCTIONS_CPU_HARD_MS: '',
  }
  const c = loadConfig((n) => vars[n])
  assertEquals(c.root, '/x/tenants')
  assertEquals(c.limits.memoryLimitMb, 64)
  assertEquals(c.limits.workerTimeoutMs, 10_000)
  assertEquals(c.limits.requestIdleTimeoutMs, 5000)
  assertEquals(c.limits.requestAbsentTimeoutMs, 2000)
  assertEquals(c.limits.cpuTimeSoftLimitMs, 0)
  assertEquals(c.limits.cpuTimeHardLimitMs, 2000)
})

Deno.test('loadConfig rejects nonsense', () => {
  assertThrows(() => loadConfig((n) => (n === 'SBCTL_FUNCTIONS_MEMORY_MB' ? 'lots' : undefined)))
  assertThrows(() => loadConfig((n) => (n === 'SBCTL_FUNCTIONS_MEMORY_MB' ? '-1' : undefined)))
})

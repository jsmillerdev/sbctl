// Settings the unit passes in the environment of the runtime process (internal/fleet
// renders them from the [functions] section of config.toml).

import type { Limits } from './types.ts'

export interface MainConfig {
  /** Directory with one subdirectory per project ref (internal/functions writes it). */
  root: string
  limits: Limits
  /**
   * The secret the proxy sends in X-Sbctl-Proxy-Token. Without it a request is refused: a
   * function worker can reach this service's port and write any project reference. It is empty
   * only when NO_PROXY_TOKEN_ENV says so (development and tests); loadConfig refuses to start
   * otherwise, so a unit whose environment lost the secret does not serve every caller.
   */
  proxyToken: string
}

/**
 * Set to "1" to start without SBCTL_FUNCTIONS_PROXY_TOKEN, which makes the service answer any
 * caller that can reach its port, a function worker included. For a development machine that
 * runs the main service by hand; sbctl never sets it.
 */
export const NO_PROXY_TOKEN_ENV = 'SBCTL_FUNCTIONS_ALLOW_NO_PROXY_TOKEN'

function intEnv(get: (name: string) => string | undefined, name: string, fallback: number): number {
  const raw = get(name)
  if (raw === undefined || raw.trim() === '') return fallback
  const n = Number(raw)
  if (!Number.isFinite(n) || n < 0) throw new Error(`${name}=${raw} is not a non-negative number`)
  return n
}

export function loadConfig(get: (name: string) => string | undefined): MainConfig {
  const proxyToken = (get('SBCTL_FUNCTIONS_PROXY_TOKEN') ?? '').trim()
  if (proxyToken === '' && get(NO_PROXY_TOKEN_ENV) !== '1') {
    throw new Error(
      'SBCTL_FUNCTIONS_PROXY_TOKEN is empty: without it the service would answer every caller that ' +
        `reaches its port, function workers included (set ${NO_PROXY_TOKEN_ENV}=1 on a development ` +
        'machine to run without it)',
    )
  }
  return {
    proxyToken,
    root: (get('SBCTL_FUNCTIONS_ROOT') ?? '/var/lib/sbctl/system/edge-runtime/tenants').replace(
      /\/+$/,
      '',
    ),
    limits: {
      memoryLimitMb: intEnv(get, 'SBCTL_FUNCTIONS_MEMORY_MB', 256),
      workerTimeoutMs: intEnv(get, 'SBCTL_FUNCTIONS_WALL_CLOCK_SEC', 400) * 1000,
      requestIdleTimeoutMs: intEnv(get, 'SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC', 150) * 1000,
      requestAbsentTimeoutMs: intEnv(get, 'SBCTL_FUNCTIONS_WORKER_IDLE_SEC', 60) * 1000,
      maxPerProject: intEnv(get, 'SBCTL_FUNCTIONS_MAX_PER_PROJECT', 128),
      maxWorkers: intEnv(get, 'SBCTL_FUNCTIONS_MAX_WORKERS', 16),
      maxWorkersPerProject: intEnv(get, 'SBCTL_FUNCTIONS_MAX_WORKERS_PER_PROJECT', 8),
      maxBundleBytes: intEnv(get, 'SBCTL_FUNCTIONS_MAX_BUNDLE_MB', 192) * 1024 * 1024,
      cpuTimeSoftLimitMs: intEnv(get, 'SBCTL_FUNCTIONS_CPU_SOFT_MS', 1000),
      cpuTimeHardLimitMs: intEnv(get, 'SBCTL_FUNCTIONS_CPU_HARD_MS', 2000),
      tmpQuotaBytes: intEnv(get, 'SBCTL_FUNCTIONS_TMP_QUOTA_MB', 64) * 1024 * 1024,
    },
  }
}

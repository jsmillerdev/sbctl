// Settings the unit passes in the environment of the runtime process (internal/fleet
// renders them from the [functions] section of config.toml).

import type { Limits } from './types.ts'

export interface MainConfig {
  projectsDir: string
  limits: Limits
}

function intEnv(get: (name: string) => string | undefined, name: string, fallback: number): number {
  const raw = get(name)
  if (raw === undefined || raw.trim() === '') return fallback
  const n = Number(raw)
  if (!Number.isFinite(n) || n < 0) throw new Error(`${name}=${raw} is not a non-negative number`)
  return n
}

export function loadConfig(get: (name: string) => string | undefined): MainConfig {
  return {
    projectsDir: (get('SBCTL_PROJECTS_DIR') ?? '/var/lib/sbctl/projects').replace(/\/+$/, ''),
    limits: {
      memoryLimitMb: intEnv(get, 'SBCTL_FUNCTIONS_MEMORY_MB', 256),
      workerTimeoutMs: intEnv(get, 'SBCTL_FUNCTIONS_WALL_CLOCK_SEC', 400) * 1000,
      requestIdleTimeoutMs: intEnv(get, 'SBCTL_FUNCTIONS_IDLE_TIMEOUT_SEC', 150) * 1000,
      requestAbsentTimeoutMs: intEnv(get, 'SBCTL_FUNCTIONS_WORKER_IDLE_SEC', 60) * 1000,
      cpuTimeSoftLimitMs: intEnv(get, 'SBCTL_FUNCTIONS_CPU_SOFT_MS', 1000),
      cpuTimeHardLimitMs: intEnv(get, 'SBCTL_FUNCTIONS_CPU_HARD_MS', 2000),
    },
  }
}

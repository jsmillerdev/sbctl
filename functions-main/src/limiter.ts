// Budgets of the shared runtime. One Edge Runtime serves every project and its memory limit
// (the unit's MemoryMax) is shared, so the main service keeps three budgets and refuses a
// request that would break one of them before it touches a worker:
//
//   - live workers of the whole runtime (maxWorkers): edge-runtime's --max-parallelism is a
//     semaphore per pool key (v1.77.4, crates/base/src/worker/pool.rs), so it does not limit
//     the runtime; this does. maxWorkers x the cost of a worker is what the unit's MemoryMax
//     is derived from, so a runtime that obeys it is never killed for memory.
//   - live workers of one project (maxWorkersPerProject): its share of the above, so one
//     project cannot take every slot.
//   - requests of one project in flight (maxRequests) and the bundle bytes of the functions
//     it has in flight (maxBundleBytes): the main service holds a function's bundle in its
//     own heap while a request runs.
//
// A worker lives as long as it has requests and for workerTtlMs after the last one (the
// runtime retires it after its absent timeout), so a pool key counts as live until then.

export type Release = () => void

export type Refusal = 'requests' | 'workers' | 'project-workers' | 'bundle-bytes'

export type Admission = { release: Release } | { refused: Refusal }

export interface LimiterOptions {
  /** Requests one project may have in flight; 0 or less means no cap. */
  maxRequests: number
  /** Distinct live workers (pool keys) of the whole runtime; 0 or less means no cap. */
  maxWorkers: number
  /** Distinct live workers of one project; 0 or less means no cap. */
  maxWorkersPerProject?: number
  /** Bundle bytes of one project's functions in flight at once; 0 or less means no cap. */
  maxBundleBytes?: number
  /** How long a worker is assumed to live after its last request (the runtime retires it after the absent timeout). */
  workerTtlMs: number
  now?: () => number
}

const noop: Release = () => {}

interface Live {
  ref: string
  /** End of the last request that used the worker (or the start of the running one). */
  last: number
  inflight: number
  /** Bundle bytes charged while inflight > 0. */
  bytes: number
}

export class ProjectLimiter {
  /** pool key -> worker bookkeeping; the pool key starts with the project ref. */
  #live = new Map<string, Live>()
  /** ref -> live pool keys. */
  #perProject = new Map<string, number>()
  #inflight = new Map<string, number>()
  #bytes = new Map<string, number>()
  #lastSweep = 0
  readonly #now: () => number

  constructor(readonly opts: LimiterOptions) {
    this.#now = opts.now ?? Date.now
  }

  /** Live workers of the whole runtime, expired ones not counted. */
  liveWorkers(): number {
    this.sweep()
    return this.#live.size
  }

  /**
   * Admits a request of project ref for the worker poolKey, or says which cap refused it.
   * bundleBytes is the size of the function's bundle.
   */
  acquire(ref: string, poolKey: string, bundleBytes = 0): Admission {
    const { maxRequests, maxWorkers, maxWorkersPerProject = 0, maxBundleBytes = 0 } = this.opts
    if (maxRequests <= 0 && maxWorkers <= 0 && maxWorkersPerProject <= 0 && maxBundleBytes <= 0) {
      return { release: noop }
    }
    const t = this.#now()
    if (t - this.#lastSweep > 60_000) this.sweep()
    const inflight = this.#inflight.get(ref) ?? 0
    if (maxRequests > 0 && inflight >= maxRequests) return { refused: 'requests' }

    let live = this.#live.get(poolKey)
    if (!live) {
      // A new worker is about to be needed. The counts can include workers that have
      // retired since, so look again after dropping those before refusing.
      const over = () => {
        if (maxWorkersPerProject > 0 && (this.#perProject.get(ref) ?? 0) >= maxWorkersPerProject) {
          return 'project-workers' as const
        }
        if (maxWorkers > 0 && this.#live.size >= maxWorkers) return 'workers' as const
        return null
      }
      let refused = over()
      if (refused) {
        this.sweep()
        refused = over()
      }
      if (refused) return { refused }
    }
    const charge = live && live.inflight > 0 ? 0 : bundleBytes
    if (maxBundleBytes > 0 && charge > 0 && (this.#bytes.get(ref) ?? 0) + charge > maxBundleBytes) {
      return { refused: 'bundle-bytes' }
    }
    if (!live) {
      live = { ref, last: t, inflight: 0, bytes: 0 }
      this.#live.set(poolKey, live)
      this.#perProject.set(ref, (this.#perProject.get(ref) ?? 0) + 1)
    }
    live.inflight++
    live.last = t
    if (charge > 0) {
      live.bytes = charge
      this.#bytes.set(ref, (this.#bytes.get(ref) ?? 0) + charge)
    }
    this.#inflight.set(ref, inflight + 1)
    let released = false
    return {
      release: () => {
        if (released) return
        released = true
        const n = (this.#inflight.get(ref) ?? 1) - 1
        if (n <= 0) this.#inflight.delete(ref)
        else this.#inflight.set(ref, n)
        live.inflight--
        live.last = this.#now()
        if (live.inflight === 0 && live.bytes > 0) {
          const left = (this.#bytes.get(ref) ?? 0) - live.bytes
          if (left <= 0) this.#bytes.delete(ref)
          else this.#bytes.set(ref, left)
          live.bytes = 0
        }
      },
    }
  }

  /** Forgets the workers whose time is up (never one with a request in flight). */
  sweep(): void {
    const t = this.#now()
    this.#lastSweep = t
    for (const [key, w] of this.#live) {
      if (w.inflight > 0 || w.last + this.opts.workerTtlMs >= t) continue
      this.#live.delete(key)
      const n = (this.#perProject.get(w.ref) ?? 1) - 1
      if (n <= 0) this.#perProject.delete(w.ref)
      else this.#perProject.set(w.ref, n)
    }
  }
}

// Fairness between projects. One Edge Runtime serves every project, and its worker pool
// (--max-parallelism) and memory limit are shared: without a cap per project, one project
// that runs many slow requests or many functions holds every slot, the others wait out the
// runtime's request-wait timeout, and the heaps of its workers can push the unit over its
// memory limit, which kills the runtime for everybody. The limiter caps, per project, the
// requests in flight and the distinct live workers (pool keys); a request over either cap is
// refused at once, before a worker is touched.

export type Release = () => void

export type Admission = { release: Release } | { refused: 'requests' | 'workers' }

export interface LimiterOptions {
  /** Requests one project may have in flight; 0 or less means no cap. */
  maxRequests: number
  /** Distinct live workers one project may have; 0 or less means no cap. */
  maxWorkers: number
  /** How long a worker is assumed to live after its last request (the runtime retires it after the absent timeout). */
  workerTtlMs: number
  now?: () => number
}

const noop: Release = () => {}

export class ProjectLimiter {
  #inflight = new Map<string, number>()
  /** ref -> pool key -> end of the last request that used it. */
  #workers = new Map<string, Map<string, number>>()
  #lastSweep = 0
  readonly #now: () => number

  constructor(readonly opts: LimiterOptions) {
    this.#now = opts.now ?? Date.now
  }

  /** Admits a request of project ref for the worker poolKey, or says which cap refused it. */
  acquire(ref: string, poolKey: string): Admission {
    const { maxRequests, maxWorkers } = this.opts
    if (maxRequests <= 0 && maxWorkers <= 0) return { release: noop }
    const t = this.#now()
    if (t - this.#lastSweep > 60_000) {
      this.#lastSweep = t
      this.sweep()
    }
    const keys = this.#workers.get(ref)
    if (keys) this.#expire(keys, t)
    const inflight = this.#inflight.get(ref) ?? 0
    if (maxRequests > 0 && inflight >= maxRequests) return { refused: 'requests' }
    if (maxWorkers > 0 && !keys?.has(poolKey) && (keys?.size ?? 0) >= maxWorkers) {
      return { refused: 'workers' }
    }
    let mine = keys
    if (!mine) this.#workers.set(ref, mine = new Map())
    mine.set(poolKey, t)
    this.#inflight.set(ref, inflight + 1)
    let released = false
    return {
      release: () => {
        if (released) return
        released = true
        const n = (this.#inflight.get(ref) ?? 1) - 1
        if (n <= 0) this.#inflight.delete(ref)
        else this.#inflight.set(ref, n)
        this.#workers.get(ref)?.set(poolKey, this.#now())
      },
    }
  }

  #expire(keys: Map<string, number>, t: number): void {
    for (const [k, last] of keys) if (last + this.opts.workerTtlMs < t) keys.delete(k)
  }

  /** Forgets idle projects' bookkeeping; acquire calls it about once a minute. */
  sweep(): void {
    const t = this.#now()
    for (const [ref, keys] of this.#workers) {
      this.#expire(keys, t)
      if (keys.size === 0 && !this.#inflight.has(ref)) this.#workers.delete(ref)
    }
  }
}

// Types shared by the modules of the supavise Edge Functions main service.

/** What internal/functions writes to <root>/<ref>/functions-env.json. */
export interface ProjectEnv {
  /** Changes whenever the file does; part of the worker pool key, so new secrets reach new workers. */
  stamp?: string
  /** HS256 secret of the project's JWTs (never handed to a worker). */
  jwtSecret: string
  /** SUPABASE_* values of the project, as the workers see them. */
  supabase: Record<string, string>
  /** Secrets set with `supabase secrets set`, never named SUPABASE_*. */
  secrets: Record<string, string>
}

/**
 * The metadata file inside a function generation (.supavise-function.json). Only bundles
 * are served: the generation holds the eszip the Supabase CLI built, whose module
 * specifiers are virtual, so a function cannot import files of the node (see README).
 */
export interface FunctionMeta {
  slug: string
  version: number
  verifyJwt: boolean
  kind: 'eszip'
  /** The module specifier of the entrypoint inside the bundle (a file URL). */
  entrypoint: string
  /** Path of the bundle relative to the generation directory. */
  eszip: string
}

/** A function resolved on disk: absolute, symlink-free paths. */
export interface FunctionInfo extends FunctionMeta {
  dir: string
  /** The bundle file. */
  eszipPath: string
}

/** The limits every worker gets (from the node's [functions] config). */
export interface Limits {
  memoryLimitMb: number
  /** A worker is retired this long after it started, whatever it is doing. */
  workerTimeoutMs: number
  /** The runtime answers 504 when a request got no response for this long (a flag of the runtime). */
  requestIdleTimeoutMs: number
  /** A worker that has had no request for this long is retired. */
  requestAbsentTimeoutMs: number
  /** How many requests one project may have in flight at once (0: no cap). */
  maxPerProject: number
  /**
   * How many distinct live workers (functions) the whole runtime may have (0: no cap). The
   * runtime's own --max-parallelism is a semaphore per function, not a limit on the
   * runtime, so this is the only cap on the memory the workers can use together; the
   * unit's MemoryMax is derived from it.
   */
  maxWorkers: number
  /** How many distinct live workers one project may have (0: no cap): its share of maxWorkers. */
  maxWorkersPerProject: number
  /** Bundle bytes of one project's functions in flight at once (0: no cap). */
  maxBundleBytes: number
  cpuTimeSoftLimitMs: number
  cpuTimeHardLimitMs: number
  /**
   * Bytes a worker may write to /tmp, its only writable file system (0: no quota). The runtime
   * backs it with real files on the node's disk, so without a quota one function can fill the
   * disk that holds every project's database.
   */
  tmpQuotaBytes: number
}

/** The options handed to EdgeRuntime.userWorkers.create. */
export interface WorkerOptions {
  poolKey: string
  servicePath: string
  maybeEntrypoint: string
  /** The bundle of the function (plain eszip bytes). */
  maybeEszip: Uint8Array
  envVars: [string, string][]
  memoryLimitMb: number
  workerTimeoutMs: number
  cpuTimeSoftLimitMs: number
  cpuTimeHardLimitMs: number
  noModuleCache: boolean
  forceCreate: boolean
  /** The worker's /tmp (UserWorkerCreateOptions.tmp_fs_config of the runtime). */
  tmpFsConfig?: { quota: number }
  context: {
    projectRef: string
    supervisor: { requestAbsentTimeoutMs: number }
  }
  permissions?: Record<string, string[] | boolean>
}

export interface Worker {
  fetch(request: Request): Promise<Response>
}

/** The edge runtime, as far as the handler needs it; replaced in tests. */
export interface Runtime {
  createWorker(options: WorkerOptions): Promise<Worker>
  /** Copies the streaming tag of src onto dest (EdgeRuntime.applySupabaseTag). */
  applyTag(src: Request, dest: Request): void
  /** Error classes of the runtime, to map failures to responses. */
  errors: RuntimeErrors
}

export interface RuntimeErrors {
  isBootError(e: unknown): boolean
  isRequestCancelled(e: unknown): boolean
  isIdleTimeout(e: unknown): boolean
  isAlreadyRetired(e: unknown): boolean
  isInvalidResponse(e: unknown): boolean
}

export interface Logger {
  log(...args: unknown[]): void
  warn(...args: unknown[]): void
  error(...args: unknown[]): void
}

// Types shared by the modules of the sbctl Edge Functions main service.

/** What internal/functions writes to projects/<ref>/functions-env.json. */
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

/** The metadata file inside a function generation (.sbctl-function.json). */
export interface FunctionMeta {
  slug: string
  version: number
  verifyJwt: boolean
  /** "source": the generation holds the files; "eszip": it holds a bundle the CLI built. */
  kind: 'source' | 'eszip'
  /**
   * Source functions: the path of the entrypoint relative to the generation directory.
   * Bundles: the module specifier of the entrypoint inside the bundle (a file URL).
   */
  entrypoint: string
  /** Path of the import map relative to the generation directory, or "" (source only). */
  importMap: string
  /** Path of the bundle relative to the generation directory (bundles only). */
  eszip: string
}

/** A function resolved on disk: absolute, symlink-free paths. */
export interface FunctionInfo extends FunctionMeta {
  dir: string
  /** Source functions: the entrypoint file. Bundles: the same as entrypoint, a URL. */
  entrypointPath: string
  importMapPath: string
  /** Bundles: the eszip file. */
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
  cpuTimeSoftLimitMs: number
  cpuTimeHardLimitMs: number
}

/** The options handed to EdgeRuntime.userWorkers.create. */
export interface WorkerOptions {
  poolKey: string
  servicePath: string
  maybeEntrypoint: string
  /** The bundle of a function the CLI bundled (plain eszip bytes). */
  maybeEszip?: Uint8Array
  envVars: [string, string][]
  memoryLimitMb: number
  workerTimeoutMs: number
  cpuTimeSoftLimitMs: number
  cpuTimeHardLimitMs: number
  noModuleCache: boolean
  forceCreate: boolean
  context: {
    projectRef: string
    importMapPath?: string
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

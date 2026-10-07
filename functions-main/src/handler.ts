// The request handler of the main service: one Edge Runtime instance serves every
// project, so every decision here is made per project, from the X-Sbctl-Project-Ref
// header that sbctl's proxy sets (and overwrites when a client sends one).

import { authErrorResponse, ErrorCode, ErrorCodes, extractToken, verifyJWT } from './auth.ts'
import { releaseWhenDone } from './body.ts'
import { ProjectLimiter } from './limiter.ts'
import { ProjectStore, validRef, validSlug } from './projects.ts'
import type {
  FunctionInfo,
  Limits,
  Logger,
  ProjectEnv,
  Runtime,
  Worker,
  WorkerOptions,
} from './types.ts'

export const TENANT_HEADER = 'x-sbctl-project-ref'
/** The secret the proxy sends with every request (SBCTL_FUNCTIONS_PROXY_TOKEN). */
export const PROXY_TOKEN_HEADER = 'x-sbctl-proxy-token'
export const MAX_WORKER_RETRIES = 3
/** How long past the worker's wall clock a response may hold its place in the budget. */
export const HOLD_GRACE_MS = 5_000

export interface HandlerDeps {
  store: ProjectStore
  runtime: Runtime
  limits: Limits
  /** Per-project caps; built from limits when absent. */
  limiter?: ProjectLimiter
  /** Port of this runtime, denied to workers (see workerPermissions). */
  port?: string
  /**
   * The secret every request must carry in X-Sbctl-Proxy-Token; empty accepts requests
   * without one (tests only: sbctl always sets it).
   */
  proxyToken?: string
  log?: Logger
  now?: () => number
}

function failure(
  code: ErrorCode,
  message: string,
  status: number,
  extra: Record<string, string> = {},
): Response {
  return Response.json({ code, message }, {
    status,
    headers: {
      'sb-error-code': code,
      'Access-Control-Expose-Headers': 'sb-error-code',
      ...extra,
    },
  })
}

/** Compares two secrets without leaking where they differ. */
async function sameSecret(a: string, b: string): Promise<boolean> {
  const enc = new TextEncoder()
  const [x, y] = await Promise.all([
    crypto.subtle.digest('SHA-256', enc.encode(a)),
    crypto.subtle.digest('SHA-256', enc.encode(b)),
  ]).then((d) => d.map((buf) => new Uint8Array(buf)))
  let diff = 0
  for (let i = 0; i < x.length; i++) diff |= x[i] ^ y[i]
  return diff === 0
}

const notFound = () => failure(ErrorCodes.NotFound, 'Requested function was not found', 404)

/** Marks a 5xx answer of the function itself, as the hosted platform does. */
function tagWorkerResponse(res: Response): Response {
  if (res.status < 500) return res
  const headers = new Headers(res.headers)
  headers.set('sb-error-code', ErrorCodes.EdgeFunctionError)
  const exposed = (headers.get('Access-Control-Expose-Headers') ?? '')
    .split(',').map((s) => s.trim()).filter(Boolean)
  if (!exposed.some((n) => n.toLowerCase() === 'sb-error-code')) exposed.push('sb-error-code')
  headers.set('Access-Control-Expose-Headers', exposed.join(', '))
  return new Response(res.body, { status: res.status, statusText: res.statusText, headers })
}

/** The environment of one worker: this project's values only, never the main service's. */
export function workerEnv(slug: string, env: ProjectEnv): [string, string][] {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(env.secrets)) {
    if (!k.toUpperCase().startsWith('SUPABASE_')) out[k] = v
  }
  for (const [k, v] of Object.entries(env.supabase)) out[k] = v
  out.SUPABASE_FUNCTION_SLUG = slug
  return Object.entries(out)
}

/**
 * The permissions of a user worker: the runtime's own defaults for user workers (its
 * environment, the network, imports) plus a deny rule for the runtime's own port. The
 * rule matches host names as written (a name that resolves to loopback, such as
 * <x>.127.0.0.1.sslip.io, or an IPv4-mapped IPv6 literal, is not covered), so it is only a
 * first line: the guard is the proxy's secret. This service refuses every request that
 * lacks X-Sbctl-Proxy-Token, so a worker that reaches the port by any name cannot choose
 * a project reference. Files are not listed: a user worker sees only its module graph,
 * not the disk (verified, see the README).
 */
export function workerPermissions(port: string): WorkerOptions['permissions'] {
  const p: NonNullable<WorkerOptions['permissions']> = {
    allow_all: false,
    allow_env: [],
    allow_net: [],
    allow_read: [],
    allow_write: [],
    allow_import: [],
    allow_sys: ['hostname'],
  }
  if (/^\d+$/.test(port)) {
    p.deny_net = ['127.0.0.1', 'localhost', '[::1]', '0.0.0.0', '[::]'].map((h) => `${h}:${port}`)
  }
  return p
}

/**
 * One worker pool per project, function and generation: the key keeps projects apart
 * even if two of them were ever to share a path, and a new deployment or new secrets get
 * new workers while the old ones idle out.
 */
function poolKey(ref: string, slug: string, info: FunctionInfo, env: ProjectEnv): string {
  return `${ref}:${slug}:${info.version}:${info.dir.split('/').pop()}:${env.stamp ?? ''}`
}

export function makeHandler(deps: HandlerDeps): (req: Request) => Promise<Response> {
  const { store, runtime, limits } = deps
  const log: Logger = deps.log ?? console
  const limiter = deps.limiter ??
    new ProjectLimiter({
      maxRequests: limits.maxPerProject,
      maxWorkers: limits.maxWorkers,
      maxWorkersPerProject: limits.maxWorkersPerProject,
      maxBundleBytes: limits.maxBundleBytes,
      workerTtlMs: limits.requestAbsentTimeoutMs + 5_000,
    })

  async function callWorker(
    req: Request,
    ref: string,
    slug: string,
    info: FunctionInfo,
    env: ProjectEnv,
    retries = MAX_WORKER_RETRIES,
  ): Promise<Response> {
    // The body must be saved before the first attempt reads it; a retry replays it.
    const spare = retries > 0 ? req.clone() : null
    try {
      const worker: Worker = await runtime.createWorker({
        poolKey: poolKey(ref, slug, info, env),
        // The function runs from its bundle only: the module specifiers inside an eszip
        // are virtual, so it cannot import files of the node (other projects' included).
        // servicePath is the generation directory, which holds nothing but that bundle.
        servicePath: info.dir,
        maybeEntrypoint: info.entrypoint,
        maybeEszip: await store.eszip(info),
        envVars: workerEnv(slug, env),
        memoryLimitMb: limits.memoryLimitMb,
        workerTimeoutMs: limits.workerTimeoutMs,
        cpuTimeSoftLimitMs: limits.cpuTimeSoftLimitMs,
        cpuTimeHardLimitMs: limits.cpuTimeHardLimitMs,
        noModuleCache: false,
        forceCreate: false,
        permissions: workerPermissions(deps.port ?? ''),
        // A worker's /tmp is a real directory on the node's disk, shared with Postgres and its
        // WAL, and the runtime puts no limit on it unless asked to.
        ...(limits.tmpQuotaBytes > 0 ? { tmpFsConfig: { quota: limits.tmpQuotaBytes } } : {}),
        context: {
          projectRef: ref,
          supervisor: { requestAbsentTimeoutMs: limits.requestAbsentTimeoutMs },
        },
      })
      const userReq = new Request(req)
      // Internal headers are for this router only; a function never sees them.
      userReq.headers.delete('sb-api-key')
      userReq.headers.delete(TENANT_HEADER)
      userReq.headers.delete(PROXY_TOKEN_HEADER)
      runtime.applyTag(req, userReq)
      return tagWorkerResponse(await worker.fetch(userReq))
    } catch (e) {
      // A retired worker rejects before it runs anything, so the request is safe to replay.
      if (runtime.errors.isAlreadyRetired(e) && spare) {
        log.warn(`${ref}/${slug}: worker retired before dispatch; retrying (${retries} left)`)
        runtime.applyTag(req, spare)
        return await callWorker(spare, ref, slug, info, env, retries - 1)
      }
      log.error(`${ref}/${slug}:`, e)
      return runtimeFailure(e)
    }
  }

  function runtimeFailure(e: unknown): Response {
    const r = runtime.errors
    if (r.isBootError(e)) {
      return failure(ErrorCodes.BootError, 'Function failed to start (please check logs)', 503)
    }
    if (r.isRequestCancelled(e)) {
      return failure(
        ErrorCodes.WorkerResourceLimit,
        'Function failed due to not having enough compute resources (please check logs)',
        546,
      )
    }
    if (r.isIdleTimeout(e)) {
      return failure(
        ErrorCodes.IdleTimeout,
        `Request idle timeout limit (${Math.round(limits.requestIdleTimeoutMs / 1000)}s) reached`,
        504,
      )
    }
    if (
      (e instanceof RangeError || r.isInvalidResponse(e)) &&
      (e as Error).message.includes('is not equal to 101 and outside the range [200, 599]')
    ) {
      return failure(
        ErrorCodes.InvalidResponseStatusCode,
        'Function returned an invalid HTTP status code (please check logs)',
        500,
      )
    }
    if (r.isAlreadyRetired(e) || r.isInvalidResponse(e)) {
      return failure(
        ErrorCodes.WorkerError,
        'Function exited due to an error (please check logs)',
        500,
      )
    }
    return failure(ErrorCodes.EdgeFunctionError, 'Internal Server Error', 500)
  }

  return async function handle(req: Request): Promise<Response> {
    const url = new URL(req.url)
    if (url.pathname === '/_internal/health') return Response.json({ message: 'ok' })

    // Only the proxy may name a tenant. The port listens on loopback, which a function
    // worker can reach too, so the proxy proves itself with a secret that workers never see.
    if (deps.proxyToken) {
      const token = req.headers.get(PROXY_TOKEN_HEADER) ?? ''
      if (!await sameSecret(token, deps.proxyToken)) {
        return failure(ErrorCodes.BadRequest, 'Forbidden', 403)
      }
    }
    // A missing or malformed tenant header means the request did not come through the
    // proxy either, so there is no project to serve.
    const ref = req.headers.get(TENANT_HEADER) ?? ''
    if (!validRef(ref)) {
      return failure(ErrorCodes.BadRequest, 'Missing or invalid project reference', 400)
    }
    const slug = url.pathname.split('/')[1] ?? ''
    if (!slug || !validSlug(slug)) return notFound()

    let env: ProjectEnv | null
    let info: FunctionInfo | null
    try {
      env = await store.env(ref)
      info = env ? await store.fn(ref, slug) : null
    } catch (e) {
      log.error(`${ref}/${slug}: reading the deployment:`, e)
      return failure(ErrorCodes.BootError, 'Function failed to start (please check logs)', 503)
    }
    if (!env || !info) return notFound()

    if (req.method !== 'OPTIONS' && info.verifyJwt) {
      const token = extractToken(req)
      if (typeof token !== 'string') return authErrorResponse(token)
      const bad = await verifyJWT(token, env.jwtSecret, deps.now?.())
      if (bad) return authErrorResponse(bad)
    }
    let bundleBytes = 0
    try {
      bundleBytes = await store.eszipSize(info)
    } catch (e) {
      if (!(e instanceof Deno.errors.NotFound)) {
        log.error(`${ref}/${slug}: reading the bundle size:`, e)
        return failure(ErrorCodes.BootError, 'Function failed to start (please check logs)', 503)
      }
      return notFound()
    }
    const admission = limiter.acquire(ref, poolKey(ref, slug, info, env), bundleBytes)
    if ('refused' in admission) {
      log.warn(`${ref}/${slug}: refused, over the limit of ${admission.refused}`)
      return failure(
        ErrorCodes.ProjectAtCapacity,
        admission.refused === 'workers'
          ? 'The Edge Functions runtime is running as many functions as it can; try again shortly'
          : 'This project has too many functions or requests running at once; try again shortly',
        503,
        { 'Retry-After': '1' },
      )
    }
    const { release } = admission
    let res: Response
    try {
      res = await callWorker(req, ref, slug, info, env)
    } catch (e) {
      release()
      throw e
    }
    // The place in the budget is held until the body has been delivered: the worker stays
    // alive while a streamed response runs.
    return releaseWhenDone(
      res,
      release,
      limits.workerTimeoutMs > 0 ? limits.workerTimeoutMs + HOLD_GRACE_MS : 0,
    )
  }
}

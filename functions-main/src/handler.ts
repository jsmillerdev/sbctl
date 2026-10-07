// The request handler of the main service: one Edge Runtime instance serves every
// project, so every decision here is made per project, from the X-Sbctl-Project-Ref
// header that sbctl's proxy sets (and overwrites when a client sends one).

import { authErrorResponse, ErrorCode, ErrorCodes, extractToken, verifyJWT } from './auth.ts'
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
export const MAX_WORKER_RETRIES = 3

export interface HandlerDeps {
  store: ProjectStore
  runtime: Runtime
  limits: Limits
  /** Port of this runtime, denied to workers (see workerPermissions). */
  port?: string
  log?: Logger
  now?: () => number
}

function failure(code: ErrorCode, message: string, status: number): Response {
  return Response.json({ code, message }, {
    status,
    headers: { 'sb-error-code': code, 'Access-Control-Expose-Headers': 'sb-error-code' },
  })
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
 * environment, the network, imports) plus a deny rule for the runtime's own port, so a
 * function cannot call this service with a project reference of its choosing. The rule
 * matches host names as written, so it is defense in depth; the real guard is that the
 * port listens on loopback and a forged reference only reaches what the proxy would
 * serve to anyone, still behind the function's own JWT check. Files are not listed: a
 * user worker sees only its module graph, not the disk (verified, see the README).
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

export function makeHandler(deps: HandlerDeps): (req: Request) => Promise<Response> {
  const { store, runtime, limits } = deps
  const log: Logger = deps.log ?? console

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
      // A function the CLI bundled runs from its bundle; a source function from its files.
      const code: Pick<WorkerOptions, 'servicePath' | 'maybeEntrypoint' | 'maybeEszip'> =
        info.kind === 'eszip'
          ? {
            servicePath: info.dir,
            maybeEntrypoint: info.entrypoint,
            maybeEszip: await store.eszip(info),
          }
          : {
            servicePath: info.entrypointPath.slice(0, info.entrypointPath.lastIndexOf('/')),
            maybeEntrypoint: new URL(`file://${encodeURI(info.entrypointPath)}`).href,
          }
      const worker: Worker = await runtime.createWorker({
        // One worker pool per project, function and generation: the key keeps projects
        // apart even if two of them were ever to share a path, and a new deployment
        // or new secrets get new workers while the old ones idle out.
        poolKey: `${ref}:${slug}:${info.version}:${info.dir.split('/').pop()}:${env.stamp ?? ''}`,
        ...code,
        envVars: workerEnv(slug, env),
        memoryLimitMb: limits.memoryLimitMb,
        workerTimeoutMs: limits.workerTimeoutMs,
        cpuTimeSoftLimitMs: limits.cpuTimeSoftLimitMs,
        cpuTimeHardLimitMs: limits.cpuTimeHardLimitMs,
        noModuleCache: false,
        forceCreate: false,
        permissions: workerPermissions(deps.port ?? ''),
        context: {
          projectRef: ref,
          ...(info.importMapPath ? { importMapPath: info.importMapPath } : {}),
          supervisor: { requestAbsentTimeoutMs: limits.requestAbsentTimeoutMs },
        },
      })
      const userReq = new Request(req)
      // Internal headers are for this router only; a function never sees them.
      userReq.headers.delete('sb-api-key')
      userReq.headers.delete(TENANT_HEADER)
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

    // The tenant comes from the proxy only. A missing or malformed header means the
    // request did not come through it, so there is no project to serve.
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
    return await callWorker(req, ref, slug, info, env)
  }
}

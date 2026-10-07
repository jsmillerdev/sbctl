// Helpers for the tests: signing JWTs and laying out a functions root.

import type { FunctionMeta, Runtime, RuntimeErrors, Worker, WorkerOptions } from './types.ts'

const enc = new TextEncoder()

function b64url(bytes: Uint8Array): string {
  let s = ''
  for (const b of bytes) s += String.fromCharCode(b)
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export async function signJWT(
  secret: string,
  claims: Record<string, unknown>,
  header: Record<string, unknown> = { alg: 'HS256', typ: 'JWT' },
): Promise<string> {
  const h = b64url(enc.encode(JSON.stringify(header)))
  const p = b64url(enc.encode(JSON.stringify(claims)))
  const key = await crypto.subtle.importKey(
    'raw',
    enc.encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  )
  const sig = new Uint8Array(await crypto.subtle.sign('HMAC', key, enc.encode(`${h}.${p}`)))
  return `${h}.${p}.${b64url(sig)}`
}

export const REF_A = 'aaaaaaaaaaaaaaaaaaaa'
export const REF_B = 'bbbbbbbbbbbbbbbbbbbb'

/** Writes functions-env.json for ref. */
export async function writeProject(
  root: string,
  ref: string,
  jwtSecret: string,
  secrets: Record<string, string> = {},
  supabase: Record<string, string> = {},
): Promise<void> {
  await Deno.mkdir(`${root}/${ref}/functions/.gen`, { recursive: true })
  await Deno.writeTextFile(
    `${root}/${ref}/functions-env.json`,
    JSON.stringify({
      version: 1,
      jwt_secret: jwtSecret,
      supabase: { SUPABASE_URL: `http://${ref}.api.test`, ...supabase },
      secrets,
    }),
  )
}

export type { FunctionMeta }

/** A runtime that records what it was asked and answers with a canned worker. */
export class FakeRuntime implements Runtime {
  created: WorkerOptions[] = []
  forwarded: Request[] = []
  failWith: unknown = null
  /** How many calls fail; a negative number means all of them. */
  failTimes = -1
  respond: (req: Request) => Response | Promise<Response> = () => new Response('worker ok')
  tags = 0

  errors: RuntimeErrors = {
    isBootError: (e) => e instanceof FakeError && e.kind === 'boot',
    isRequestCancelled: (e) => e instanceof FakeError && e.kind === 'cancelled',
    isIdleTimeout: (e) => e instanceof FakeError && e.kind === 'idle',
    isAlreadyRetired: (e) => e instanceof FakeError && e.kind === 'retired',
    isInvalidResponse: (e) => e instanceof FakeError && e.kind === 'invalid',
  }

  createWorker(options: WorkerOptions): Promise<Worker> {
    this.created.push(options)
    return Promise.resolve({
      fetch: async (req: Request) => {
        this.forwarded.push(req)
        if (this.failWith && (this.failTimes < 0 || this.failTimes-- > 0)) throw this.failWith
        return await this.respond(req)
      },
    })
  }

  applyTag(_src: Request, _dest: Request): void {
    this.tags++
  }
}

export class FakeError extends Error {
  constructor(readonly kind: string, message = kind) {
    super(message)
  }
}

export const silent = { log() {}, warn() {}, error() {} }

/** Writes a bundled (eszip) function and points functions/<slug> at it. */
export async function writeBundle(
  root: string,
  ref: string,
  slug: string,
  eszip: Uint8Array,
  opts: Partial<{ verifyJwt: boolean; version: number }> = {},
): Promise<string> {
  const version = opts.version ?? 1
  const gen = `${root}/${ref}/functions/.gen/${slug}-${version}-bundle`
  await Deno.mkdir(gen, { recursive: true })
  await Deno.writeFile(`${gen}/bundle.eszip`, eszip)
  await Deno.writeTextFile(
    `${gen}/.sbctl-function.json`,
    JSON.stringify({
      slug,
      version,
      verify_jwt: opts.verifyJwt ?? true,
      kind: 'eszip',
      entrypoint: `file:///src/${slug}/index.ts`,
      eszip: 'bundle.eszip',
    }),
  )
  const link = `${root}/${ref}/functions/${slug}`
  try {
    await Deno.remove(`${link}.tmp`)
  } catch { /* none */ }
  await Deno.symlink(gen, `${link}.tmp`)
  await Deno.rename(`${link}.tmp`, link)
  return gen
}

/** Writes a bundled function with placeholder eszip bytes (the fake runtime never reads them). */
export function writeFunction(
  root: string,
  ref: string,
  slug: string,
  opts: Partial<{ verifyJwt: boolean; version: number }> = {},
): Promise<string> {
  return writeBundle(root, ref, slug, new TextEncoder().encode('ESZIP2.3 placeholder'), opts)
}

/**
 * Wraps a handler so that every response is read to its end, as the HTTP server does for a
 * client that is listening. The handler holds a request's place in its budgets until the
 * body has been delivered, so a test that only looks at the status would never free it. The
 * caller still gets an unread response.
 */
export function draining(
  handle: (r: Request) => Promise<Response>,
): (r: Request) => Promise<Response> {
  return async (req) => {
    const res = await handle(req)
    await res.clone().arrayBuffer()
    return res
  }
}

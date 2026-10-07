// Helpers for the tests: signing JWTs and laying out a projects directory.

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

/** Writes one generation of a function and points functions/<slug> at it. */
export async function writeFunction(
  root: string,
  ref: string,
  slug: string,
  opts: Partial<{ verifyJwt: boolean; version: number; importMap: string; entrypoint: string }> =
    {},
): Promise<string> {
  const version = opts.version ?? 1
  const gen = `${root}/${ref}/functions/.gen/${slug}-${version}-x`
  const entrypoint = opts.entrypoint ?? `supabase/functions/${slug}/index.ts`
  await Deno.mkdir(`${gen}/${entrypoint.split('/').slice(0, -1).join('/')}`, { recursive: true })
  await Deno.writeTextFile(`${gen}/${entrypoint}`, 'Deno.serve(() => new Response("ok"))')
  const meta = {
    slug,
    version,
    verify_jwt: opts.verifyJwt ?? true,
    entrypoint,
    import_map: opts.importMap ?? '',
  } satisfies Record<string, unknown>
  await Deno.writeTextFile(`${gen}/.sbctl-function.json`, JSON.stringify(meta))
  const link = `${root}/${ref}/functions/${slug}`
  const tmp = `${link}.tmp`
  try {
    await Deno.remove(tmp)
  } catch { /* none */ }
  await Deno.symlink(gen, tmp)
  await Deno.rename(tmp, link)
  return gen
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

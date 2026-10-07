// Reads what internal/functions materialized under <projects dir>/<ref>/:
//
//   functions-env.json            the project's JWT secret, SUPABASE_* values and secrets
//   functions/<slug>              a symlink to the current generation of the function
//   functions/.gen/<slug>-<n>/    the files of one generation plus .sbctl-function.json
//
// A deployment is a new generation and a rename of the symlink, so one request sees one
// generation: the request resolves the symlink once and uses the real path from then on.

import type { FunctionInfo, FunctionMeta, ProjectEnv } from './types.ts'

export const ENV_FILE = 'functions-env.json'
export const META_FILE = '.sbctl-function.json'

const REF_RE = /^[a-z]{20}$/
const SLUG_RE = /^[A-Za-z][A-Za-z0-9_-]*$/

export const validRef = (ref: string): boolean => REF_RE.test(ref)
export const validSlug = (slug: string): boolean => SLUG_RE.test(slug)

const isNotFound = (e: unknown): boolean => e instanceof Deno.errors.NotFound

/** Joins path elements with "/" (the runtime runs on Linux and macOS only). */
const join = (...parts: string[]): string => parts.join('/').replace(/\/{2,}/g, '/')

/** Resolves rel against base and returns it only when it stays inside base. */
export function containedPath(base: string, rel: string): string | null {
  if (!rel || rel.startsWith('/') || rel.includes('\\') || rel.includes('\0')) return null
  const out: string[] = []
  for (const seg of rel.split('/')) {
    if (seg === '' || seg === '.') continue
    if (seg === '..') {
      if (out.length === 0) return null
      out.pop()
    } else out.push(seg)
  }
  return out.length === 0 ? null : join(base, ...out)
}

function asStringMap(v: unknown): Record<string, string> | null {
  if (v === undefined || v === null) return {}
  if (typeof v !== 'object' || Array.isArray(v)) return null
  const out: Record<string, string> = {}
  for (const [k, val] of Object.entries(v)) {
    if (typeof val !== 'string') return null
    out[k] = val
  }
  return out
}

export function parseProjectEnv(text: string): ProjectEnv | null {
  let raw: Record<string, unknown>
  try {
    raw = JSON.parse(text)
  } catch {
    return null
  }
  if (!raw || typeof raw !== 'object' || typeof raw.jwt_secret !== 'string') return null
  const supabase = asStringMap(raw.supabase)
  const secrets = asStringMap(raw.secrets)
  if (!supabase || !secrets) return null
  return { jwtSecret: raw.jwt_secret, supabase, secrets }
}

export function parseMeta(text: string): FunctionMeta | null {
  let raw: Record<string, unknown>
  try {
    raw = JSON.parse(text)
  } catch {
    return null
  }
  if (!raw || typeof raw !== 'object') return null
  if (
    typeof raw.slug !== 'string' || typeof raw.version !== 'number' ||
    typeof raw.verify_jwt !== 'boolean' || typeof raw.entrypoint !== 'string' ||
    (raw.import_map !== undefined && typeof raw.import_map !== 'string')
  ) return null
  return {
    slug: raw.slug,
    version: raw.version,
    verifyJwt: raw.verify_jwt,
    entrypoint: raw.entrypoint,
    importMap: (raw.import_map as string | undefined) ?? '',
  }
}

/** ProjectStore reads project environments and function generations from disk. */
export class ProjectStore {
  #envs = new Map<string, { stamp: string; value: ProjectEnv }>()
  #fns = new Map<string, FunctionInfo>()
  #roots = new Map<string, string>()

  constructor(readonly projectsDir: string) {}

  /** The environment of ref, or null when the project has none (no functions yet). */
  async env(ref: string): Promise<ProjectEnv | null> {
    if (!validRef(ref)) return null
    const path = join(this.projectsDir, ref, ENV_FILE)
    let stat: Deno.FileInfo
    try {
      stat = await Deno.stat(path)
    } catch (e) {
      if (isNotFound(e)) {
        this.#envs.delete(ref)
        return null
      }
      throw e
    }
    const stamp = `${stat.mtime?.getTime() ?? 0}:${stat.size}:${stat.ino ?? 0}`
    const hit = this.#envs.get(ref)
    if (hit && hit.stamp === stamp) return hit.value
    const value = parseProjectEnv(await Deno.readTextFile(path))
    if (!value) throw new Error(`${path} is not a valid functions environment file`)
    value.stamp = stamp
    this.#envs.set(ref, { stamp, value })
    return value
  }

  /**
   * The current generation of slug in ref, or null when there is none. The result holds
   * real paths, all inside ref's functions directory.
   */
  async fn(ref: string, slug: string): Promise<FunctionInfo | null> {
    if (!validRef(ref) || !validSlug(slug)) return null
    const functionsDir = join(this.projectsDir, ref, 'functions')
    let dir: string
    try {
      dir = await Deno.realPath(join(functionsDir, slug))
    } catch (e) {
      if (isNotFound(e)) return null
      throw e
    }
    let root = this.#roots.get(ref)
    if (!root) {
      root = await Deno.realPath(functionsDir)
      this.#roots.set(ref, root)
    }
    // A link that leaves the project's functions directory is never followed.
    if (!dir.startsWith(root + '/')) return null
    const cached = this.#fns.get(dir)
    if (cached) return cached
    let text: string
    try {
      text = await Deno.readTextFile(join(dir, META_FILE))
    } catch (e) {
      if (isNotFound(e)) return null
      throw e
    }
    const meta = parseMeta(text)
    if (!meta || meta.slug !== slug) return null
    const entrypointPath = containedPath(dir, meta.entrypoint)
    if (!entrypointPath) return null
    let importMapPath = ''
    if (meta.importMap) {
      const p = containedPath(dir, meta.importMap)
      if (!p) return null
      importMapPath = p
    }
    const info: FunctionInfo = { ...meta, dir, entrypointPath, importMapPath }
    // Generations never change once written, so the real path is a stable cache key.
    if (this.#fns.size >= 2048) this.#fns.clear()
    this.#fns.set(dir, info)
    return info
  }

  /** Forgets everything cached about ref (tests, and a project that was removed). */
  forget(ref: string): void {
    this.#envs.delete(ref)
    this.#roots.delete(ref)
    for (const k of [...this.#fns.keys()]) {
      if (k.startsWith(join(this.projectsDir, ref) + '/')) this.#fns.delete(k)
    }
  }
}

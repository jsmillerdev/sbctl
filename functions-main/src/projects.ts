// Reads what internal/functions materialized under <root>/<ref>/ (the root is
// <state>/system/edge-runtime/tenants):
//
//   functions-env.json            the project's JWT secret, SUPABASE_* values and secrets
//   functions/<slug>              a symlink to the current generation of the function
//   functions/.gen/<slug>.<n>.<r>/  bundle.eszip of one generation plus .sbctl-function.json
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

/** Memory the main service spends on cached bundles. */
export const ESZIP_CACHE_BYTES = 128 * 1024 * 1024

const isNotFound = (e: unknown): boolean => e instanceof Deno.errors.NotFound

/**
 * Runs op, trying again a couple of times when it fails with anything but "not found":
 * macOS can fail a path walk through a symlink that a deployment replaces at that very
 * moment (EINVAL), and a retry then finds the new target. Linux does not need it.
 */
async function retrying<T>(op: () => Promise<T>, tries = 3): Promise<T> {
  for (let i = 1;; i++) {
    try {
      return await op()
    } catch (e) {
      if (isNotFound(e) || i >= tries) throw e
      await new Promise((r) => setTimeout(r, 5 * i))
    }
  }
}

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

/**
 * Parses a generation's metadata. Only bundles (kind "eszip") are accepted: a generation
 * of an older sbctl that holds source files is not served, because a source function
 * runs from real paths and its module loader can follow relative imports out of the
 * function's directory into other projects' files.
 */
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
    raw.kind !== 'eszip' || typeof raw.eszip !== 'string' || !raw.eszip || !raw.entrypoint
  ) return null
  return {
    slug: raw.slug,
    version: raw.version,
    verifyJwt: raw.verify_jwt,
    kind: 'eszip',
    entrypoint: raw.entrypoint,
    eszip: raw.eszip,
  }
}

/** ProjectStore reads project environments and function generations from disk. */
export class ProjectStore {
  #envs = new Map<string, { stamp: string; value: ProjectEnv }>()
  #fns = new Map<string, FunctionInfo>()
  #roots = new Map<string, string>()

  constructor(readonly root: string, readonly cacheBytes = ESZIP_CACHE_BYTES) {}

  /** The environment of ref, or null when the project has none (no functions yet). */
  async env(ref: string): Promise<ProjectEnv | null> {
    if (!validRef(ref)) return null
    const path = join(this.root, ref, ENV_FILE)
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
    const functionsDir = join(this.root, ref, 'functions')
    let dir: string
    try {
      dir = await retrying(() => Deno.realPath(join(functionsDir, slug)))
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
      text = await retrying(() => Deno.readTextFile(join(dir, META_FILE)))
    } catch (e) {
      if (isNotFound(e)) return null
      throw e
    }
    const meta = parseMeta(text)
    if (!meta || meta.slug !== slug) return null
    // The entrypoint is a module of the bundle, not a file; the bundle file is checked.
    const eszipPath = containedPath(dir, meta.eszip)
    if (!eszipPath) return null
    const info: FunctionInfo = { ...meta, dir, eszipPath }
    // Generations never change once written, so the real path is a stable cache key.
    if (this.#fns.size >= 2048) this.#fns.clear()
    this.#fns.set(dir, info)
    return info
  }

  // Insertion order is the LRU order: a hit moves the entry to the end.
  #eszips = new Map<string, Uint8Array>()
  #eszipBytes = 0
  #sizes = new Map<string, number>()

  /** The size of a function's bundle in bytes; generations never change, so it is read once. */
  async eszipSize(info: FunctionInfo): Promise<number> {
    const hit = this.#sizes.get(info.dir) ?? this.#eszips.get(info.dir)?.length
    if (hit !== undefined) return hit
    const size = (await retrying(() => Deno.stat(info.eszipPath))).size
    if (this.#sizes.size >= 2048) this.#sizes.clear()
    this.#sizes.set(info.dir, size)
    return size
  }

  /**
   * The bytes of a function's eszip, kept in memory (generations never change) while the
   * cache stays under its budget. The least recently used bundles make room for a new
   * one; a bundle that alone exceeds the budget is read on every call and never cached.
   */
  async eszip(info: FunctionInfo): Promise<Uint8Array> {
    const hit = this.#eszips.get(info.dir)
    if (hit) {
      this.#eszips.delete(info.dir)
      this.#eszips.set(info.dir, hit)
      return hit
    }
    const bytes = await retrying(() => Deno.readFile(info.eszipPath))
    if (bytes.length > this.cacheBytes) return bytes
    const raced = this.#eszips.get(info.dir) // another request read it meanwhile
    if (raced) {
      this.#eszips.delete(info.dir)
      this.#eszipBytes -= raced.length
    }
    for (const [k, v] of this.#eszips) {
      if (this.#eszipBytes + bytes.length <= this.cacheBytes) break
      this.#eszips.delete(k)
      this.#eszipBytes -= v.length
    }
    this.#eszips.set(info.dir, bytes)
    this.#eszipBytes += bytes.length
    return bytes
  }

  /** Forgets everything cached about ref (tests, and a project that was removed). */
  forget(ref: string): void {
    this.#envs.delete(ref)
    this.#roots.delete(ref)
    for (const k of [...this.#fns.keys()]) {
      if (k.startsWith(join(this.root, ref) + '/')) this.#fns.delete(k)
    }
    for (const k of [...this.#sizes.keys()]) {
      if (k.startsWith(join(this.root, ref) + '/')) this.#sizes.delete(k)
    }
    for (const [k, v] of [...this.#eszips]) {
      if (k.startsWith(join(this.root, ref) + '/')) {
        this.#eszips.delete(k)
        this.#eszipBytes -= v.length
      }
    }
  }
}

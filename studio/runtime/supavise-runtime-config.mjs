// Runtime configuration for the supavise platform-mode Studio build.
//
// Next.js inlines NEXT_PUBLIC_* values (and next.config.ts evaluates the CSP) at build time.
// build.sh therefore builds with unique placeholder strings (see placeholders.json) and this
// module swaps them for the real per-install values every time Studio starts.
//
// Contract:
//   - `prepare` (packaging time) finds every file that contains a placeholder, stores a pristine
//     copy next to it as `<file>.supavise-tpl` and records the list in runtime-config.json.
//   - `apply` (every start) rewrites each listed file from its `.supavise-tpl` copy. It never reads
//     the previous output, so it is idempotent and a later start with other values just works.
//     Each write is a temp file plus rename, so a crash cannot leave a half-written file.
//   - Values are validated against a conservative character set before use, so a value cannot
//     break out of a JS string, a JSON string or an HTML attribute that contains a placeholder.
//
// No dependencies besides node:*. Used by docker-entrypoint.mjs and by tests.

import {
  chmodSync,
  copyFileSync,
  existsSync,
  lstatSync,
  readdirSync,
  readFileSync,
  renameSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { join, relative } from 'node:path'

export const TEMPLATE_SUFFIX = '.supavise-tpl'
export const SPEC_VERSION = 1

// Characters allowed in a substituted value: no quotes, backslash, backtick, angle brackets,
// ampersand, whitespace, `$` or `?`/`#`, so the value is inert in JS, JSON, HTML and CSP.
const SAFE_VALUE = /^[A-Za-z0-9._~:/@%+,*=-]*$/
// A CSP host source: optional `*.` wildcard, a hostname, optional port. Same grammar as the
// filter in studio/patches/0003.
const HOST_SOURCE = /^(\*\.)?[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:\d{1,5})?$/i
// Studio feature keys are `area:name`, both parts [a-z0-9_-].
const FEATURE_KEY = /^[a-z0-9_-]+:[a-z0-9_-]+$/i

const MAX_TEMPLATE_BYTES = 64 * 1024 * 1024

function escapeRegExp(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/** Reads and sanity-checks a placeholders file. */
export function readPlaceholders(path) {
  const spec = JSON.parse(readFileSync(path, 'utf8'))
  if (spec.version !== SPEC_VERSION || !Array.isArray(spec.values)) {
    throw new Error(`${path}: unsupported placeholders file (want version ${SPEC_VERSION})`)
  }
  const seen = new Set()
  for (const v of spec.values) {
    for (const key of ['env', 'kind', 'placeholder']) {
      if (typeof v[key] !== 'string' || !v[key]) throw new Error(`${path}: value missing "${key}"`)
    }
    if (!['url', 'url-platform', 'string', 'csp-hosts'].includes(v.kind)) {
      throw new Error(`${path}: ${v.env}: unknown kind ${v.kind}`)
    }
    if (seen.has(v.placeholder)) throw new Error(`${path}: duplicate placeholder ${v.placeholder}`)
    seen.add(v.placeholder)
  }
  return spec
}

/** Every literal string that must not survive into a running build. */
export function allPlaceholderStrings(spec) {
  const out = []
  for (const v of spec.values) {
    out.push(v.placeholder)
    if (v.origin) out.push(v.origin)
  }
  return out
}

function normalizeUrl(env, raw, kind) {
  let u
  try {
    u = new URL(raw)
  } catch {
    throw new Error(`${env}: not a valid URL: ${JSON.stringify(raw)}`)
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') {
    throw new Error(`${env}: must be an http(s) URL, got ${u.protocol}`)
  }
  if (u.username || u.password) throw new Error(`${env}: must not contain credentials`)
  if (u.search || u.hash) throw new Error(`${env}: must not contain a query or fragment`)
  let value = raw.replace(/\/+$/, '')
  if (kind === 'url-platform' && !value.endsWith('/platform')) value += '/platform'
  return { value, origin: u.origin }
}

/**
 * Resolves the real value for each placeholder from `env`.
 * Returns { rules, errors, resolved } where rules is the list used by `rewrite`.
 */
export function resolveValues(spec, env) {
  const errors = []
  const rules = [] // { from, to } or { from, hosts } for csp-hosts
  const resolved = {} // env name -> value actually used (for the child process env)

  for (const v of spec.values) {
    const given = env[v.env]
    // A required value must be non-empty. An optional one that is set, even to the empty string,
    // wins over the default, so NEXT_PUBLIC_DISABLED_FEATURES= re-enables every feature.
    const isSet = v.required ? given !== undefined && given !== '' : given !== undefined
    if (!isSet && v.required) {
      errors.push(`${v.env} is required (${v.doc ?? 'no description'})`)
      continue
    }
    const raw = isSet ? given.trim() : (v.default ?? '')

    try {
      if (v.kind === 'url' || v.kind === 'url-platform') {
        const { value, origin } = normalizeUrl(v.env, raw, v.kind)
        if (!SAFE_VALUE.test(value)) throw new Error(`${v.env}: contains characters that are not allowed`)
        rules.push({ from: v.placeholder, to: value })
        if (v.origin) rules.push({ from: v.origin, to: origin })
        resolved[v.env] = value
      } else if (v.kind === 'csp-hosts') {
        const hosts = raw.split(/[\s,]+/).filter(Boolean)
        for (const h of hosts) {
          if (!HOST_SOURCE.test(h)) throw new Error(`${v.env}: not a host source: ${JSON.stringify(h)}`)
        }
        rules.push({ from: v.placeholder, hosts, scheme: true })
        resolved[v.env] = hosts.join(',')
      } else {
        if (!SAFE_VALUE.test(raw)) throw new Error(`${v.env}: contains characters that are not allowed`)
        if (v.env === 'NEXT_PUBLIC_DISABLED_FEATURES') {
          for (const k of raw.split(',').filter(Boolean)) {
            if (!FEATURE_KEY.test(k)) throw new Error(`${v.env}: not a feature key: ${JSON.stringify(k)}`)
          }
        }
        rules.push({ from: v.placeholder, to: raw })
        resolved[v.env] = raw
      }
    } catch (err) {
      errors.push(err.message)
    }
  }
  return { rules, errors, resolved }
}

/** Builds the single-pass rewrite function for a rule set. Longest placeholders match first. */
export function makeRewriter(rules) {
  const plain = new Map()
  const csp = []
  for (const r of rules) {
    if (r.hosts) csp.push(r)
    else plain.set(r.from, r.to)
  }
  const parts = []
  // csp-hosts placeholders only ever appear behind a scheme (https:// or wss://).
  for (const r of csp) parts.push(`(?:https|wss)://${escapeRegExp(r.from)}`)
  for (const from of [...plain.keys()].sort((a, b) => b.length - a.length)) {
    parts.push(escapeRegExp(from))
  }
  if (parts.length === 0) return (s) => s
  const re = new RegExp(parts.join('|'), 'g')
  return (text) =>
    text.replace(re, (match) => {
      if (plain.has(match)) return plain.get(match)
      const scheme = match.slice(0, match.indexOf('://'))
      const rule = csp.find((r) => match.endsWith(`://${r.from}`))
      return rule.hosts.map((h) => `${scheme}://${h}`).join(' ')
    })
}

function walk(dir, visit) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name)
    if (entry.isSymbolicLink()) continue
    if (entry.isDirectory()) walk(p, visit)
    else if (entry.isFile()) visit(p)
  }
}

/**
 * Packaging step: copies every file containing a placeholder to `<file>.supavise-tpl` and returns the
 * sorted list of paths relative to appRoot. Throws if a placeholder in `required` is not found
 * anywhere, which would mean the bundler dropped it and the runtime value could never apply.
 */
export function prepare(appRoot, spec) {
  const needles = allPlaceholderStrings(spec).map((s) => Buffer.from(s))
  const files = []
  const found = new Set()
  walk(appRoot, (p) => {
    if (p.endsWith(TEMPLATE_SUFFIX)) return
    const size = statSync(p).size
    if (size === 0 || size > MAX_TEMPLATE_BYTES) return
    const buf = readFileSync(p)
    let hit = false
    for (const n of needles) {
      if (buf.includes(n)) {
        hit = true
        found.add(n.toString())
      }
    }
    if (hit) {
      copyFileSync(p, p + TEMPLATE_SUFFIX)
      files.push(relative(appRoot, p))
    }
  })
  const missing = spec.values.filter((v) => !found.has(v.placeholder))
  if (missing.length > 0) {
    throw new Error(
      'placeholder not found in the build output (the bundler dropped or rewrote it): ' +
        missing.map((v) => `${v.env}=${v.placeholder}`).join(', ')
    )
  }
  return files.sort()
}

/**
 * Start step: rewrites every listed file from its template. Returns { written, unchanged }.
 * Throws a descriptive error when the tree is read-only or a template is missing.
 */
export function apply(appRoot, files, rewrite) {
  let written = 0
  let unchanged = 0
  for (const rel of files) {
    const target = join(appRoot, rel)
    const tpl = target + TEMPLATE_SUFFIX
    if (!existsSync(tpl)) {
      throw new Error(`template missing: ${tpl} (the artifact is incomplete; re-extract it)`)
    }
    const out = rewrite(readFileSync(tpl, 'utf8'))
    let current = null
    try {
      current = readFileSync(target, 'utf8')
    } catch {
      // missing target is rewritten below
    }
    if (current === out) {
      unchanged++
      continue
    }
    const tmp = `${target}.supavise-tmp-${process.pid}`
    try {
      writeFileSync(tmp, out)
      chmodSync(tmp, lstatSync(tpl).mode & 0o777)
      renameSync(tmp, target)
    } catch (err) {
      if (err.code === 'EACCES' || err.code === 'EROFS' || err.code === 'EPERM') {
        throw new Error(
          `cannot write ${target}: ${err.code}. The Studio app directory must be writable by the ` +
            'service user (systemd: ReadWritePaths= for the artifact directory).'
        )
      }
      throw err
    }
    written++
  }
  return { written, unchanged }
}

/**
 * Entry used by docker-entrypoint.mjs. `root` is the artifact root (contains app/ and share/).
 * Returns the env overrides for the child process. Throws Error with `.configError = true` for
 * bad operator input, which the caller turns into exit code 78.
 */
export function applyRuntimeConfig(root, env = process.env) {
  if (env.SUPAVISE_STUDIO_SKIP_RUNTIME_CONFIG === '1') return {}
  const specPath = join(root, 'share', 'supavise', 'runtime-config.json')
  if (!existsSync(specPath)) {
    console.warn('supavise-studio: no share/supavise/runtime-config.json; starting without substitution')
    return {}
  }
  const cfg = JSON.parse(readFileSync(specPath, 'utf8'))
  const spec = { version: cfg.version, values: cfg.values }
  const { rules, errors, resolved } = resolveValues(spec, env)
  if (errors.length > 0) {
    const err = new Error(`invalid Studio configuration:\n  - ${errors.join('\n  - ')}`)
    err.configError = true
    throw err
  }
  const { written, unchanged } = apply(join(root, 'app'), cfg.files, makeRewriter(rules))
  console.log(`supavise-studio: runtime config applied (${written} files written, ${unchanged} unchanged)`)
  return resolved
}

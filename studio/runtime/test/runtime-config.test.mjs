import assert from 'node:assert/strict'
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  TEMPLATE_SUFFIX,
  allPlaceholderStrings,
  apply,
  applyRuntimeConfig,
  makeRewriter,
  prepare,
  readPlaceholders,
  resolveValues,
} from '../supavise-runtime-config.mjs'

const placeholdersPath = fileURLToPath(new URL('../../placeholders.json', import.meta.url))
const spec = readPlaceholders(placeholdersPath)
const ph = Object.fromEntries(spec.values.map((v) => [v.env, v]))

const goodEnv = {
  NEXT_PUBLIC_API_URL: 'https://api.example.com',
  NEXT_PUBLIC_GOTRUE_URL: 'https://api.example.com/auth/v1/',
  NEXT_PUBLIC_SITE_URL: 'https://studio.example.com',
}

// A tree that looks like the Next output: a client chunk, a server chunk, prerendered HTML,
// the routes manifest with the CSP, and a file without placeholders.
function makeApp() {
  const root = mkdtempSync(join(tmpdir(), 'supavise-studio-test-'))
  const app = join(root, 'app')
  mkdirSync(join(app, 'apps/studio/.next/static/chunks'), { recursive: true })
  mkdirSync(join(app, 'apps/studio/.next/server/pages'), { recursive: true })
  const csp =
    `connect-src 'self' ${ph.NEXT_PUBLIC_API_URL.origin} ${ph.NEXT_PUBLIC_GOTRUE_URL.origin} ` +
    `https://${ph.CSP_EXTRA_PROJECT_HOSTS.placeholder} wss://${ph.CSP_EXTRA_PROJECT_HOSTS.placeholder}; ` +
    `img-src 'self' https://${ph.CSP_EXTRA_PROJECT_HOSTS.placeholder};`
  writeFileSync(
    join(app, 'apps/studio/.next/static/chunks/a.js'),
    `let API_URL="${ph.NEXT_PUBLIC_API_URL.placeholder}",G="${ph.NEXT_PUBLIC_GOTRUE_URL.placeholder}",` +
      `S=\`${ph.NEXT_PUBLIC_SITE_URL.placeholder}/x\`,K="${ph.NEXT_PUBLIC_HCAPTCHA_SITE_KEY.placeholder}",` +
      `D="${ph.NEXT_PUBLIC_DISABLED_FEATURES.placeholder}".split(",");`
  )
  writeFileSync(
    join(app, 'apps/studio/.next/server/pages/sign-in.html'),
    `<script>{"api":"${ph.NEXT_PUBLIC_API_URL.placeholder}"}</script>`
  )
  writeFileSync(
    join(app, 'apps/studio/.next/routes-manifest.json'),
    JSON.stringify({ headers: [{ key: 'Content-Security-Policy', value: csp }] })
  )
  writeFileSync(join(app, 'apps/studio/.next/BUILD_ID'), 'abc')
  return { root, app }
}

function packaged() {
  const { root, app } = makeApp()
  const files = prepare(app, spec)
  mkdirSync(join(root, 'share/supavise'), { recursive: true })
  writeFileSync(
    join(root, 'share/supavise/runtime-config.json'),
    JSON.stringify({ version: 1, values: spec.values, files })
  )
  return { root, app, files }
}

test('placeholders file is well formed', () => {
  assert.ok(allPlaceholderStrings(spec).length >= 7)
  for (const v of spec.values) {
    if (v.origin) assert.ok(v.placeholder.startsWith(v.origin), v.env)
  }
})

test('prepare lists only files with placeholders and writes templates', () => {
  const { root, app, files } = packaged()
  try {
    assert.deepEqual(files, [
      'apps/studio/.next/routes-manifest.json',
      'apps/studio/.next/server/pages/sign-in.html',
      'apps/studio/.next/static/chunks/a.js',
    ])
    for (const f of files) assert.ok(existsSync(join(app, f + TEMPLATE_SUFFIX)), f)
    assert.ok(!existsSync(join(app, 'apps/studio/.next/BUILD_ID' + TEMPLATE_SUFFIX)))
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('prepare fails when a placeholder did not survive the build', () => {
  const { root, app } = makeApp()
  try {
    writeFileSync(
      join(app, 'apps/studio/.next/static/chunks/a.js'),
      `let K="${ph.NEXT_PUBLIC_API_URL.placeholder}";`
    )
    assert.throws(() => prepare(app, spec), /placeholder not found/)
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('apply substitutes values, origins and CSP hosts', () => {
  const { root, app } = packaged()
  try {
    applyRuntimeConfig(root, {
      ...goodEnv,
      NEXT_PUBLIC_HCAPTCHA_SITE_KEY: 'abc-123',
      CSP_EXTRA_PROJECT_HOSTS: '*.api.example.com, db.example.net:8443',
    })
    const js = readFileSync(join(app, 'apps/studio/.next/static/chunks/a.js'), 'utf8')
    assert.match(js, /API_URL="https:\/\/api\.example\.com\/platform"/)
    assert.match(js, /G="https:\/\/api\.example\.com\/auth\/v1"/)
    assert.match(js, /`https:\/\/studio\.example\.com\/x`/)
    assert.match(js, /K="abc-123"/)
    assert.match(js, /D="dashboard_auth:sign_up,/)
    const manifest = JSON.parse(readFileSync(join(app, 'apps/studio/.next/routes-manifest.json'), 'utf8'))
    const csp = manifest.headers[0].value
    assert.ok(csp.includes("connect-src 'self' https://api.example.com https://api.example.com "))
    assert.ok(
      csp.includes('https://*.api.example.com https://db.example.net:8443 wss://*.api.example.com wss://db.example.net:8443;')
    )
    assert.ok(csp.includes("img-src 'self' https://*.api.example.com https://db.example.net:8443;"))
    const html = readFileSync(join(app, 'apps/studio/.next/server/pages/sign-in.html'), 'utf8')
    assert.equal(html, '<script>{"api":"https://api.example.com/platform"}</script>')
    for (const f of [js, csp, html]) {
      for (const s of allPlaceholderStrings(spec)) assert.ok(!f.includes(s), s)
    }
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('empty optional values: no captcha, default feature list, no extra CSP hosts', () => {
  const { root, app } = packaged()
  try {
    applyRuntimeConfig(root, goodEnv)
    const js = readFileSync(join(app, 'apps/studio/.next/static/chunks/a.js'), 'utf8')
    assert.match(js, /K="",/)
    assert.match(js, /D="dashboard_auth:sign_up,dashboard_auth:sign_in_with_github/)
    const csp = JSON.parse(readFileSync(join(app, 'apps/studio/.next/routes-manifest.json'), 'utf8'))
      .headers[0].value
    assert.ok(!csp.includes('placeholder'))
    assert.ok(!csp.includes('undefined'))
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('an optional value set to the empty string wins over its default', () => {
  const { root, app } = packaged()
  try {
    applyRuntimeConfig(root, { ...goodEnv, NEXT_PUBLIC_DISABLED_FEATURES: '' })
    const js = readFileSync(join(app, 'apps/studio/.next/static/chunks/a.js'), 'utf8')
    assert.match(js, /D="".split/)
    assert.ok(!js.includes('dashboard_auth:sign_up'))
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('apply is idempotent and picks up new values on the next start', () => {
  const { root, app } = packaged()
  try {
    const target = join(app, 'apps/studio/.next/static/chunks/a.js')
    applyRuntimeConfig(root, goodEnv)
    const first = readFileSync(target, 'utf8')
    applyRuntimeConfig(root, goodEnv)
    assert.equal(readFileSync(target, 'utf8'), first)
    applyRuntimeConfig(root, { ...goodEnv, NEXT_PUBLIC_API_URL: 'https://other.example.org/platform' })
    assert.match(readFileSync(target, 'utf8'), /API_URL="https:\/\/other\.example\.org\/platform"/)
    // and back again
    applyRuntimeConfig(root, goodEnv)
    assert.equal(readFileSync(target, 'utf8'), first)
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('second run with identical values writes nothing', () => {
  const { root, app, files } = packaged()
  try {
    const rules = resolveValues(spec, goodEnv).rules
    const rewrite = makeRewriter(rules)
    assert.deepEqual(apply(app, files, rewrite), { written: 3, unchanged: 0 })
    assert.deepEqual(apply(app, files, rewrite), { written: 0, unchanged: 3 })
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('missing required values and unsafe values are rejected', () => {
  const missing = resolveValues(spec, {})
  assert.equal(missing.errors.length, 3)
  const bad = resolveValues(spec, {
    ...goodEnv,
    NEXT_PUBLIC_API_URL: 'https://x.example.com/a";alert(1);//',
    NEXT_PUBLIC_SITE_URL: 'javascript:alert(1)',
    NEXT_PUBLIC_GOTRUE_URL: 'https://x.example.com/auth?x=1',
    NEXT_PUBLIC_HCAPTCHA_SITE_KEY: 'a"b',
    NEXT_PUBLIC_DISABLED_FEATURES: 'ok:key,not a key',
    CSP_EXTRA_PROJECT_HOSTS: "evil.com; script-src * 'unsafe-eval'",
  })
  assert.equal(bad.errors.length, 6, bad.errors.join('\n'))
})

test('applyRuntimeConfig reports bad input as a configError', () => {
  const { root } = packaged()
  try {
    assert.throws(
      () => applyRuntimeConfig(root, {}),
      (err) => err.configError === true && /NEXT_PUBLIC_API_URL is required/.test(err.message)
    )
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('read-only tree gives an actionable error', { skip: process.getuid?.() === 0 }, () => {
  const { root, app } = packaged()
  try {
    chmodSync(join(app, 'apps/studio/.next/static/chunks'), 0o555)
    assert.throws(() => applyRuntimeConfig(root, goodEnv), /must be writable by the service user/)
  } finally {
    chmodSync(join(app, 'apps/studio/.next/static/chunks'), 0o755)
    rmSync(root, { recursive: true })
  }
})

test('SUPAVISE_STUDIO_SKIP_RUNTIME_CONFIG skips everything', () => {
  const { root, app } = packaged()
  try {
    applyRuntimeConfig(root, { SUPAVISE_STUDIO_SKIP_RUNTIME_CONFIG: '1' })
    assert.ok(readFileSync(join(app, 'apps/studio/.next/static/chunks/a.js'), 'utf8').includes('supavise-placeholder'))
  } finally {
    rmSync(root, { recursive: true })
  }
})

test('url-platform keeps an existing /platform suffix and trims slashes', () => {
  const { resolved } = resolveValues(spec, { ...goodEnv, NEXT_PUBLIC_API_URL: 'http://127.0.0.1:7000/platform//' })
  assert.equal(resolved.NEXT_PUBLIC_API_URL, 'http://127.0.0.1:7000/platform')
})

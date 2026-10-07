import { assertEquals, assertNotEquals } from 'jsr:@std/assert@1'
import { ErrorCodes, extractToken, verifyJWT } from './auth.ts'
import { signJWT } from './testutil.ts'

const SECRET = 'a-project-secret-of-some-length-0123456789'
const NOW = 1_800_000_000_000

Deno.test('extractToken reads the bearer token', () => {
  const r = new Request('http://x/', { headers: { authorization: 'Bearer abc.def.ghi' } })
  assertEquals(extractToken(r), 'abc.def.ghi')
  const lower = new Request('http://x/', { headers: { authorization: 'bearer  abc.def.ghi ' } })
  assertEquals(extractToken(lower), 'abc.def.ghi')
})

Deno.test('extractToken falls back to sb-api-key for opaque keys', () => {
  const r = new Request('http://x/', {
    headers: { authorization: 'Bearer sb_publishable_xyz', 'sb-api-key': 'Bearer jwt.from.proxy' },
  })
  assertEquals(extractToken(r), 'jwt.from.proxy')
  const none = new Request('http://x/', { headers: { 'sb-api-key': 'jwt.only.header' } })
  assertEquals(extractToken(none), 'jwt.only.header')
})

Deno.test('extractToken reports missing and malformed headers', () => {
  const missing = extractToken(new Request('http://x/'))
  assertEquals(typeof missing === 'string' ? '' : missing.code, ErrorCodes.MissingAuthHeader)
  const basic = extractToken(new Request('http://x/', { headers: { authorization: 'Basic abc' } }))
  assertEquals(typeof basic === 'string' ? '' : basic.code, ErrorCodes.InvalidTokenFormat)
  const opaqueOnly = extractToken(
    new Request('http://x/', { headers: { authorization: 'Bearer sb_secret_1' } }),
  )
  assertEquals(typeof opaqueOnly === 'string' ? '' : opaqueOnly.code, ErrorCodes.InvalidTokenFormat)
})

Deno.test('verifyJWT accepts a token signed with the project secret', async () => {
  const t = await signJWT(SECRET, { role: 'anon', exp: NOW / 1000 + 60 })
  assertEquals(await verifyJWT(t, SECRET, NOW), null)
  const noExp = await signJWT(SECRET, { role: 'anon' })
  assertEquals(await verifyJWT(noExp, SECRET, NOW), null)
})

Deno.test("verifyJWT rejects another project's secret, expiry and nbf", async () => {
  const t = await signJWT(SECRET, { role: 'anon', exp: NOW / 1000 + 60 })
  assertEquals(
    (await verifyJWT(t, 'some-other-projects-secret', NOW))?.code,
    ErrorCodes.InvalidLegacyJWT,
  )
  const expired = await signJWT(SECRET, { exp: NOW / 1000 - 1 })
  assertEquals((await verifyJWT(expired, SECRET, NOW))?.code, ErrorCodes.InvalidLegacyJWT)
  const early = await signJWT(SECRET, { nbf: NOW / 1000 + 100 })
  assertEquals((await verifyJWT(early, SECRET, NOW))?.code, ErrorCodes.InvalidLegacyJWT)
  const badExp = await signJWT(SECRET, { exp: 'soon' })
  assertEquals((await verifyJWT(badExp, SECRET, NOW))?.code, ErrorCodes.InvalidLegacyJWT)
  assertEquals((await verifyJWT(t, '', NOW))?.code, ErrorCodes.InvalidLegacyJWT)
})

Deno.test('verifyJWT rejects tampered payloads and signatures', async () => {
  const t = await signJWT(SECRET, { role: 'anon' })
  const [h, , s] = t.split('.')
  const forged = `${h}.${btoa(JSON.stringify({ role: 'service_role' })).replace(/=+$/, '')}.${s}`
  assertNotEquals(await verifyJWT(forged, SECRET, NOW), null)
  assertNotEquals(await verifyJWT(`${t}x`, SECRET, NOW), null)
})

Deno.test('verifyJWT sorts out other algorithms and garbage', async () => {
  const none = await signJWT(SECRET, { role: 'anon' }, { alg: 'none' })
  assertEquals((await verifyJWT(none, SECRET, NOW))?.code, ErrorCodes.UnsupportedTokenAlgorithm)
  const es = await signJWT(SECRET, { role: 'anon' }, { alg: 'ES256' })
  assertEquals((await verifyJWT(es, SECRET, NOW))?.code, ErrorCodes.InvalidAsymmetricJWT)
  const hs512 = await signJWT(SECRET, { role: 'anon' }, { alg: 'HS512' })
  assertEquals((await verifyJWT(hs512, SECRET, NOW))?.code, ErrorCodes.UnsupportedTokenAlgorithm)
  for (const junk of ['', 'a.b', 'a.b.c.d', '***.***.***', 'e30.e30.']) {
    assertEquals((await verifyJWT(junk, SECRET, NOW))?.code, ErrorCodes.InvalidTokenFormat, junk)
  }
})

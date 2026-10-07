import {
  assertEquals,
  assertNotStrictEquals,
  assertRejects,
  assertStrictEquals,
} from 'jsr:@std/assert@1'
import {
  containedPath,
  ESZIP_CACHE_BYTES,
  parseMeta,
  parseProjectEnv,
  ProjectStore,
  validRef,
  validSlug,
} from './projects.ts'
import { REF_A, REF_B, writeBundle, writeFunction, writeProject } from './testutil.ts'

async function tmp(): Promise<string> {
  return await Deno.realPath(await Deno.makeTempDir({ prefix: 'sbctl-main-test-' }))
}

Deno.test('refs and slugs are validated', () => {
  assertEquals(validRef(REF_A), true)
  for (const bad of ['', 'system0', 'AAAAAAAAAAAAAAAAAAAA', '../aaaaaaaaaaaaaaaaaa', REF_A + 'a']) {
    assertEquals(validRef(bad), false, bad)
  }
  assertEquals(validSlug('hello-world_2'), true)
  for (const bad of ['', '1abc', '-x', 'a/b', '..', 'a.b', '.sbctl']) {
    assertEquals(validSlug(bad), false, bad)
  }
})

Deno.test('containedPath keeps a path inside its base', () => {
  assertEquals(containedPath('/b', 'a/b.ts'), '/b/a/b.ts')
  assertEquals(containedPath('/b', 'a/../c.ts'), '/b/c.ts')
  assertEquals(containedPath('/b', './a//c.ts'), '/b/a/c.ts')
  for (const bad of ['', '/etc/passwd', '../x', 'a/../../x', 'a\\b', '..', '.']) {
    assertEquals(containedPath('/b', bad), null, bad)
  }
})

Deno.test('parseProjectEnv and parseMeta check their shape', () => {
  assertEquals(
    parseProjectEnv('{"jwt_secret":"s","supabase":{"A":"1"},"secrets":{"B":"2"}}'),
    { jwtSecret: 's', supabase: { A: '1' }, secrets: { B: '2' } },
  )
  assertEquals(parseProjectEnv('{"jwt_secret":"s"}'), { jwtSecret: 's', supabase: {}, secrets: {} })
  assertEquals(parseProjectEnv('{"supabase":{}}'), null)
  assertEquals(parseProjectEnv('{"jwt_secret":"s","secrets":{"B":2}}'), null)
  assertEquals(parseProjectEnv('nope'), null)
  const bundle =
    '{"slug":"a","version":1,"verify_jwt":true,"kind":"eszip","entrypoint":"file:///src/i.ts","eszip":"bundle.eszip"}'
  assertEquals(parseMeta(bundle), {
    slug: 'a',
    version: 1,
    verifyJwt: true,
    kind: 'eszip',
    entrypoint: 'file:///src/i.ts',
    eszip: 'bundle.eszip',
  })
  assertEquals(parseMeta(bundle.replace(',"eszip":"bundle.eszip"', '')), null)
  assertEquals(parseMeta(bundle.replace('"eszip"', '"wasm"')), null)
  // Sources are not served: no kind (what an older sbctl wrote) or kind "source".
  const source = '{"slug":"a","version":2,"verify_jwt":false,"entrypoint":"i.ts"}'
  assertEquals(parseMeta(source), null)
  assertEquals(parseMeta(source.replace('"entrypoint"', '"kind":"source","entrypoint"')), null)
  assertEquals(parseMeta('{"slug":"a"}'), null)
})

Deno.test('ProjectStore.env follows the file and is empty without one', async () => {
  const root = await tmp()
  try {
    const store = new ProjectStore(root)
    assertEquals(await store.env(REF_A), null)
    await writeProject(root, REF_A, 'secret-1', { X: '1' })
    const first = await store.env(REF_A)
    assertEquals(first?.jwtSecret, 'secret-1')
    assertEquals(first?.secrets, { X: '1' })
    await new Promise((r) => setTimeout(r, 15))
    await writeProject(root, REF_A, 'secret-2-longer', { X: '2' })
    assertEquals((await store.env(REF_A))?.jwtSecret, 'secret-2-longer')
    assertEquals(await store.env('not-a-ref'), null)
    await Deno.writeTextFile(`${root}/${REF_A}/functions-env.json`, 'broken')
    await assertRejects(() => store.env(REF_A))
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

Deno.test('ProjectStore.fn resolves one generation and follows a new deployment', async () => {
  const root = await tmp()
  try {
    await writeProject(root, REF_A, 's')
    const store = new ProjectStore(root)
    assertEquals(await store.fn(REF_A, 'hello'), null)
    const gen1 = await writeFunction(root, REF_A, 'hello', { version: 1, verifyJwt: false })
    const one = await store.fn(REF_A, 'hello')
    assertEquals(one?.dir, gen1)
    assertEquals(one?.verifyJwt, false)
    assertEquals(one?.eszipPath, `${gen1}/bundle.eszip`)
    const gen2 = await writeFunction(root, REF_A, 'hello', { version: 2 })
    const two = await store.fn(REF_A, 'hello')
    assertEquals(two?.dir, gen2)
    assertEquals(two?.version, 2)
    // The other project has no such function.
    await writeProject(root, REF_B, 's')
    assertEquals(await store.fn(REF_B, 'hello'), null)
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

Deno.test('ProjectStore.fn never follows a link out of the project', async () => {
  const root = await tmp()
  try {
    await writeProject(root, REF_A, 's')
    await writeProject(root, REF_B, 's')
    const foreign = await writeFunction(root, REF_B, 'secretfn')
    await Deno.symlink(foreign, `${root}/${REF_A}/functions/stolen`)
    const store = new ProjectStore(root)
    assertEquals(await store.fn(REF_A, 'stolen'), null)
    // A directory without metadata, or with a slug that disagrees, is not a function.
    await Deno.mkdir(`${root}/${REF_A}/functions/.gen/empty`, { recursive: true })
    await Deno.symlink(`${root}/${REF_A}/functions/.gen/empty`, `${root}/${REF_A}/functions/empty`)
    assertEquals(await store.fn(REF_A, 'empty'), null)
    const other = await writeFunction(root, REF_A, 'other')
    await Deno.symlink(other, `${root}/${REF_A}/functions/alias`)
    assertEquals(await store.fn(REF_A, 'alias'), null)
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

Deno.test('ProjectStore resolves a bundled function and caches its eszip', async () => {
  const root = await tmp()
  try {
    await writeProject(root, REF_A, 's')
    const gen = await writeBundle(root, REF_A, 'bundled', new TextEncoder().encode('ESZIP2.3 fake'))
    const store = new ProjectStore(root)
    const info = await store.fn(REF_A, 'bundled')
    assertEquals(info?.kind, 'eszip')
    assertEquals(info?.dir, gen)
    assertEquals(info?.entrypoint, 'file:///src/bundled/index.ts')
    assertEquals(info?.eszipPath, `${gen}/bundle.eszip`)
    const bytes = await store.eszip(info!)
    assertEquals(new TextDecoder().decode(bytes), 'ESZIP2.3 fake')
    assertEquals(await store.eszip(info!), bytes) // the same array: cached
    // A bundle name that leaves the generation is refused.
    await Deno.writeTextFile(
      `${gen}/.sbctl-function.json`,
      JSON.stringify({
        slug: 'bundled',
        version: 1,
        verify_jwt: true,
        kind: 'eszip',
        entrypoint: 'file:///x',
        eszip: '../x',
      }),
    )
    assertEquals(await new ProjectStore(root).fn(REF_A, 'bundled'), null)
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

Deno.test('a bundle larger than the cache budget is read each time and never cached', async () => {
  const root = await tmp()
  try {
    await writeProject(root, REF_A, 's')
    const big = new Uint8Array(ESZIP_CACHE_BYTES + 1)
    const small = new TextEncoder().encode('ESZIP2.3 small')
    await writeBundle(root, REF_A, 'big', big)
    await writeBundle(root, REF_A, 'small', small)
    const store = new ProjectStore(root)
    const bigInfo = (await store.fn(REF_A, 'big'))!
    const first = await store.eszip(bigInfo)
    assertEquals(first.length, big.length)
    assertNotStrictEquals(await store.eszip(bigInfo), first)
    // It did not evict the small one either.
    const smallInfo = (await store.fn(REF_A, 'small'))!
    const cached = await store.eszip(smallInfo)
    await store.eszip(bigInfo)
    assertStrictEquals(await store.eszip(smallInfo), cached)
  } finally {
    await Deno.remove(root, { recursive: true })
  }
})

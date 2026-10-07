import { assert, assertEquals, assertRejects } from 'jsr:@std/assert@1'
import { releaseWhenDone } from './body.ts'

function counter() {
  const c = { n: 0, release: () => {} }
  c.release = () => c.n++
  return c
}

function source(chunks: string[], failAfter?: number) {
  const enc = new TextEncoder()
  let i = 0
  const state = { cancelled: false }
  const body = new ReadableStream<Uint8Array>({
    pull(c) {
      if (failAfter !== undefined && i >= failAfter) return c.error(new Error('worker retired'))
      if (i >= chunks.length) return c.close()
      c.enqueue(enc.encode(chunks[i++]))
    },
    cancel() {
      state.cancelled = true
    },
  })
  return { body, state }
}

Deno.test('a response without a body releases at once and is returned as it is', () => {
  const rel = counter()
  const res = new Response(null, { status: 204 })
  assert(releaseWhenDone(res, rel.release) === res)
  assertEquals(rel.n, 1)
})

Deno.test('a body releases when it has been read to its end, not before', async () => {
  const rel = counter()
  const { body } = source(['a', 'b', 'c'])
  const res = releaseWhenDone(
    new Response(body, { status: 201, statusText: 'Made', headers: { 'x-a': '1' } }),
    rel.release,
  )
  assertEquals([res.status, res.statusText, res.headers.get('x-a')], [201, 'Made', '1'])
  assertEquals(rel.n, 0, 'the headers are not the end of the response')
  const reader = res.body!.getReader()
  assertEquals((await reader.read()).done, false)
  assertEquals(rel.n, 0, 'a chunk is not the end either')
  while (!(await reader.read()).done);
  assertEquals(rel.n, 1)
})

Deno.test('the content passes through unchanged', async () => {
  const rel = counter()
  const res = releaseWhenDone(new Response(source(['he', 'llo ', 'world']).body), rel.release)
  assertEquals(await res.text(), 'hello world')
  assertEquals(rel.n, 1)
})

Deno.test('a body that fails releases, and the failure reaches the reader', async () => {
  const rel = counter()
  const res = releaseWhenDone(new Response(source(['a', 'b'], 1).body), rel.release)
  await assertRejects(() => res.text(), Error, 'worker retired')
  assertEquals(rel.n, 1)
})

Deno.test('a body that the client cancels releases and is cancelled at the source', async () => {
  const rel = counter()
  const { body, state } = source(['a', 'b', 'c'])
  const res = releaseWhenDone(new Response(body), rel.release)
  const reader = res.body!.getReader()
  await reader.read()
  assertEquals(rel.n, 0)
  await reader.cancel('client went away')
  assertEquals(rel.n, 1)
  assert(state.cancelled, 'the worker stream was not cancelled')
})

Deno.test('a consumer that neither reads nor cancels is let go at the backstop', async () => {
  const rel = counter()
  const { body } = source(['a', 'b'])
  const res = releaseWhenDone(new Response(body), rel.release, 30)
  assertEquals(rel.n, 0)
  await new Promise((r) => setTimeout(r, 80))
  assertEquals(rel.n, 1)
  await res.body!.cancel() // the test cleans up; release is idempotent in the limiter
})

Deno.test('the backstop timer is cleared when the body ends first', async () => {
  const rel = counter()
  const res = releaseWhenDone(new Response(source(['a']).body), rel.release, 60_000)
  await res.text()
  assertEquals(rel.n, 1)
  // The test runner reports a timer that was left running as a leak.
})

// Answers at once with headers and then streams one line a second for ?secs= seconds (the shape
// of server-sent events and token streams). The worker stays busy until the stream ends, so the
// node's worker budget must count the request until then, not until the headers.
Deno.serve((req) => {
  const secs = Number(new URL(req.url).searchParams.get('secs') ?? '5')
  let i = 0
  const enc = new TextEncoder()
  const body = new ReadableStream({
    async pull(controller) {
      if (i >= secs) return controller.close()
      await new Promise((r) => setTimeout(r, 1000))
      controller.enqueue(enc.encode(`tick ${i++}\n`))
    },
  })
  return new Response(body, { headers: { 'content-type': 'text/event-stream' } })
})

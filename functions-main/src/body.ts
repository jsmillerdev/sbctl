// Holding a request's share of the budget until its response has been delivered.

/**
 * Returns res with its body wrapped so that release() runs when the body has ended, failed or
 * been cancelled (the client went away), not when the headers were returned. A function that
 * streams (server-sent events, token streams) keeps its worker alive for as long as the
 * stream runs, up to the wall clock, so it must keep its place in the budgets for that long:
 * released at the headers, such a response would let a project hold more live workers than
 * max_workers_per_project and max_workers allow, behind a memory limit derived from them.
 *
 * A response without a body (HEAD, 204, 304, a WebSocket upgrade) releases at once.
 * maxHoldMs (0: none) is a backstop for a consumer that neither reads nor cancels: the worker
 * is retired at its wall clock, so holding a place for longer than that protects nothing.
 * release must be idempotent.
 */
export function releaseWhenDone(res: Response, release: () => void, maxHoldMs = 0): Response {
  if (!res.body) {
    release()
    return res
  }
  const reader = res.body.getReader()
  let timer: number | undefined
  const done = () => {
    if (timer !== undefined) clearTimeout(timer)
    release()
  }
  if (maxHoldMs > 0) {
    timer = setTimeout(release, maxHoldMs)
    // The backstop must not keep a process alive that has nothing else to do.
    if (typeof Deno.unrefTimer === 'function') Deno.unrefTimer(timer)
  }
  const body = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const chunk = await reader.read()
        if (chunk.done) {
          done()
          controller.close()
        } else {
          controller.enqueue(chunk.value)
        }
      } catch (e) {
        done()
        controller.error(e)
      }
    },
    cancel(reason) {
      done()
      return reader.cancel(reason)
    },
  })
  return new Response(body, {
    status: res.status,
    statusText: res.statusText,
    headers: res.headers,
  })
}

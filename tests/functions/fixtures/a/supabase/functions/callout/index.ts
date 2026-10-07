// Calls other addresses from inside a worker and reports what answered: the Edge Runtime's own
// port under names the worker permissions do not deny (a host name that resolves to loopback,
// an IPv4-mapped IPv6 literal), claiming to be another project. The main service must refuse
// every one of them, whatever the name: only the proxy has its secret. Query parameters:
// `u` (repeatable) the URLs, `ref` the project to claim to be.
Deno.serve(async (req) => {
  const q = new URL(req.url).searchParams
  const out: Record<string, string> = {}
  for (const target of q.getAll('u')) {
    try {
      const r = await fetch(target, {
        headers: { 'x-supavise-project-ref': q.get('ref') ?? '', 'x-supavise-proxy-token': 'guess' },
      })
      out[target] = `${r.status} ${(await r.text()).slice(0, 100)}`
    } catch (e) {
      out[target] = `ERROR: ${(e as Error).name}: ${(e as Error).message.slice(0, 100)}`
    }
  }
  return Response.json(out)
})

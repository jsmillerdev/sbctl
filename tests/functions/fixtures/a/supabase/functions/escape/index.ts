// Tries to import files of the node the way a function that runs from source files could:
// relative specifiers that climb out of its directory, absolute paths and file URLs, with
// specifiers computed at run time so the bundler cannot resolve (and inline) them. A bundled
// function must reach none of them. The `t` query parameters are paths without the leading slash.
Deno.serve(async (req) => {
  const out: Record<string, string> = {}
  const describe = (e: unknown) => `ERROR: ${(e as Error).name}: ${(e as Error).message.slice(0, 160)}`
  // Control: a dynamic import works at all.
  try {
    const m = await import(`data:text/javascript,${encodeURIComponent('export default {ok: true}')}`)
    out.control = `IMPORTED: ${JSON.stringify(m.default)}`
  } catch (e) {
    out.control = describe(e)
  }
  for (const t of new URL(req.url).searchParams.getAll('t')) {
    const specs = [`/${t}`, `file:///${t}`, ...Array.from({ length: 14 }, (_, i) => '../'.repeat(i + 1) + t)]
    for (const spec of specs) {
      try {
        const m = await import(spec, { with: { type: 'json' } })
        out[spec] = `IMPORTED: ${JSON.stringify(m.default).slice(0, 80)}`
      } catch (e) {
        out[spec] = describe(e)
      }
    }
  }
  return Response.json(out)
})

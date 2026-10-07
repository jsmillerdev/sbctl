// Tries to read files from the disk of the node: a worker must see its module graph only.
Deno.serve(async (req) => {
  const out: Record<string, string> = {}
  for (const path of new URL(req.url).searchParams.getAll('p')) {
    try {
      out[path] = `READ: ${(await Deno.readTextFile(path)).slice(0, 40)}`
    } catch (e) {
      out[path] = `ERROR: ${(e as Error).name}`
    }
  }
  try {
    out.env = `ENV: ${Deno.env.get('SBCTL_FUNCTIONS_ROOT') ?? 'unset'}`
  } catch (e) {
    out.env = `ERROR: ${(e as Error).name}`
  }
  return Response.json(out)
})

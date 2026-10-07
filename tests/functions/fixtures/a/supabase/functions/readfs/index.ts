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
  // Other ways to reach what is not in the module graph: a listing of /proc, a child process (which
  // would see the node's processes) and the whole environment.
  try {
    out.readDir = `LISTED: ${[...Deno.readDirSync('/proc')].length} entries`
  } catch (e) {
    out.readDir = `ERROR: ${(e as Error).name}`
  }
  try {
    const r = new Deno.Command('/bin/sh', { args: ['-c', 'cat /proc/self/environ'] }).outputSync()
    out.spawn = `RAN: ${new TextDecoder().decode(r.stdout).slice(0, 40)}`
  } catch (e) {
    out.spawn = `ERROR: ${(e as Error).name}`
  }
  try {
    const names = Object.keys(Deno.env.toObject())
    out.envNames = names.some((n) => n.startsWith('SBCTL_') || n === 'EDGE_RUNTIME_PORT')
      ? `ENV: ${names.join(',')}`
      : 'ERROR: none of the main service variables'
  } catch (e) {
    out.envNames = `ERROR: ${(e as Error).name}`
  }
  return Response.json(out)
})

// Writes ?mb= MiB to /tmp, the one place a worker can write. The node caps it ([functions]
// tmp_quota_mb): a write past the cap fails inside this function and nowhere else.
Deno.serve(async (req) => {
  const mb = Number(new URL(req.url).searchParams.get('mb') ?? '1')
  const chunk = new Uint8Array(1024 * 1024).fill(97)
  const path = '/tmp/sbctl-tmpwrite'
  try {
    const f = await Deno.open(path, { create: true, write: true, truncate: true })
    try {
      for (let i = 0; i < mb; i++) await f.write(chunk)
    } finally {
      f.close()
    }
    const bytes = (await Deno.stat(path)).size
    await Deno.remove(path)
    return Response.json({ ok: true, bytes })
  } catch (e) {
    try {
      await Deno.remove(path)
    } catch { /* nothing to remove */ }
    return Response.json({ ok: false, error: `${(e as Error).name}: ${(e as Error).message}` })
  }
})

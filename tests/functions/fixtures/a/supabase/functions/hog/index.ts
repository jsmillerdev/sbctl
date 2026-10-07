// Allocates until the worker's memory limit ends it.
Deno.serve(() => {
  const keep: number[][] = []
  for (;;) keep.push(new Array(1_000_000).fill(1))
})

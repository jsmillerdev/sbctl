// Never answers and never yields: only the runtime's limits end it.
Deno.serve(() => {
  while (true) { /* busy */ }
})

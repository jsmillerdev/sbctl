// Exists in project A only: project B must answer 404 for it.
Deno.serve(() => new Response('only-a'))

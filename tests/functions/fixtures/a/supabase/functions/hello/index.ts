// Project A's hello: verify_jwt on (the default). The answer names the project this
// code was deployed to, the secret it sees, and where its SUPABASE_URL points.
import { WHO } from 'shared/who.ts' // through the function's import map

Deno.serve((req) => {
  const url = new URL(req.url)
  return Response.json({
    who: WHO,
    code: 'A1',
    secret: Deno.env.get('MY_SECRET') ?? null,
    supabaseUrl: Deno.env.get('SUPABASE_URL'),
    path: url.pathname,
    hasServiceKey: Boolean(Deno.env.get('SUPABASE_SERVICE_ROLE_KEY')),
    hasDbUrl: Boolean(Deno.env.get('SUPABASE_DB_URL')),
    sawTenantHeader: req.headers.has('x-supavise-project-ref'),
    sawSbApiKey: req.headers.has('sb-api-key'),
    otherSecret: Deno.env.get('SECRET_OF_THE_OTHER_PROJECT') ?? null,
    pathEnv: Deno.env.get('PATH') ?? null,
  })
})

// Project B's hello: same slug as project A's, different code.
import { WHO } from '../_shared/who.ts'

Deno.serve((req) =>
  Response.json({
    who: WHO,
    code: 'B1',
    secret: Deno.env.get('MY_SECRET') ?? null,
    supabaseUrl: Deno.env.get('SUPABASE_URL'),
    path: new URL(req.url).pathname,
    otherSecret: Deno.env.get('SECRET_OF_THE_OTHER_PROJECT') ?? null,
  })
)

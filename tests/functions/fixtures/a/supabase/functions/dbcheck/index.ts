// Queries its own project's database two ways: SUPABASE_DB_URL with a Postgres client,
// and the REST API of the project with the service role key (supabase-js).
import postgres from 'npm:postgres@3.4.5'
import { createClient } from 'npm:@supabase/supabase-js@2.117.2'

Deno.serve(async () => {
  const sql = postgres(Deno.env.get('SUPABASE_DB_URL')!, { max: 1, connect_timeout: 10 })
  try {
    const [row] = await sql`select current_database() as db, current_user as role, (select count(*)::int from public.fn_probe) as probes`
    const supabase = createClient(
      Deno.env.get('SUPABASE_URL')!,
      Deno.env.get('SUPABASE_SERVICE_ROLE_KEY')!,
      { auth: { persistSession: false } },
    )
    const { data, error } = await supabase.from('fn_probe').select('who').order('id')
    return Response.json({ sql: row, rest: data, restError: error?.message ?? null })
  } catch (e) {
    return Response.json({ error: String(e) }, { status: 500 })
  } finally {
    await sql.end({ timeout: 2 })
  }
})

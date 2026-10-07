// pg_cron through the Management API's SQL endpoint, as a customer uses it from the SQL editor:
// enable the extension, schedule a job, and see the job run. A node whose pg_cron cannot connect
// back to its own cluster lists every run as failed ("connection failed"), so the check is a
// succeeded row in cron.job_run_details and the effect of the job, not only that cron.job has a row.
import assert from 'node:assert/strict'
import { after, before, describe, test } from 'node:test'
import { projects, sleep, sql, suffix, waitFor } from './lib.mjs'

const p = projects.a
const job = `conf-cron-${suffix}`
const table = `conf_cron_ticks_${suffix}`

describe('pg_cron', () => {
  before(async () => {
    await sql(p, `create table public.${table} (id bigint generated always as identity primary key, at timestamptz not null default now())`)
  })

  after(async () => {
    await sql(p, `select cron.unschedule('${job}')`).catch(() => {})
    await sql(p, `drop table if exists public.${table}`)
  })

  test('the extension is available and a job can be scheduled', async () => {
    await sql(p, 'create extension if not exists pg_cron')
    const id = await sql(p, `select cron.schedule('${job}', '5 seconds', $$insert into public.${table} default values$$) as jobid`)
    assert.ok(Number.isInteger(Number(id[0].jobid)), `cron.schedule returned ${JSON.stringify(id)}`)
    const rows = await sql(p, `select schedule, active, database from cron.job where jobname = '${job}'`)
    assert.deepEqual(rows, [{ schedule: '5 seconds', active: true, database: 'postgres' }])
  })

  test('the job runs: a succeeded run is recorded and its statement took effect', async () => {
    const run = await waitFor('a succeeded run in cron.job_run_details', async () => {
      const r = await sql(p, `select d.status, d.return_message from cron.job_run_details d join cron.job j using (jobid) where j.jobname = '${job}' and d.status = 'succeeded' limit 1`)
      return r[0]
    }, { timeout: 120_000, interval: 2000 }).catch(async (e) => {
      const last = await sql(p, `select d.status, d.return_message from cron.job_run_details d join cron.job j using (jobid) where j.jobname = '${job}' order by d.runid desc limit 3`)
      throw new Error(`${e.message}; last runs: ${JSON.stringify(last)}`)
    })
    assert.equal(run.status, 'succeeded')
    const ticks = await sql(p, `select count(*)::int as n from public.${table}`)
    assert.ok(ticks[0].n >= 1, 'the scheduled insert did not run')
  })

  test('an unscheduled job stops', async () => {
    await sql(p, `select cron.unschedule('${job}')`)
    assert.deepEqual(await sql(p, `select 1 from cron.job where jobname = '${job}'`), [])
    await sleep(1500) // a run that had started finishes
    const before = (await sql(p, `select count(*)::int as n from public.${table}`))[0].n
    await sleep(12_000)
    assert.equal((await sql(p, `select count(*)::int as n from public.${table}`))[0].n, before)
  })
})

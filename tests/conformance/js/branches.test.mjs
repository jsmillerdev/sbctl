// Branches through the Management API, the endpoints the Supabase CLI (`supabase branches`) and
// Studio call: create a schema-only branch of project A, list it, read it, see that it carries the
// parent's migrations and none of its data, and delete it.
import assert from 'node:assert/strict'
import { after, before, describe, test } from 'node:test'
import { mgmt, projects, sql, suffix, waitFor } from './lib.mjs'

const p = projects.a
const table = `conf_branch_probe_${suffix}`
const name = `conf-branch-${suffix}`

describe('branches', () => {
  let branch

  before(async () => {
    // A migration the branch replays, and a row it must not get: a schema-only branch has the
    // parent's schema and none of its data.
    const m = await mgmt('POST', `/v1/projects/${p.ref}/database/migrations`, {
      name: `conf_branch_${suffix}`,
      query: `create table public.${table} (id int primary key, note text)`,
    })
    assert.ok(m.status < 300, `apply migration: ${m.status} ${JSON.stringify(m.body)}`)
    await sql(p, `insert into public.${table} values (1, 'parent data')`)
  })

  after(async () => {
    if (branch) await mgmt('DELETE', `/v1/branches/${branch.id}`)
    await mgmt('POST', `/v1/projects/${p.ref}/database/query`, { query: `drop table if exists public.${table}` })
    await sql(p, `delete from supabase_migrations.schema_migrations where name = 'conf_branch_${suffix}'`).catch(() => {})
  })

  test('a schema-only branch is created and becomes ready', async () => {
    const r = await mgmt('POST', `/v1/projects/${p.ref}/branches`, { branch_name: name })
    assert.equal(r.status, 201, JSON.stringify(r.body))
    branch = r.body
    assert.equal(branch.name, name)
    assert.equal(branch.parent_project_ref, p.ref)
    assert.equal(branch.with_data, false)
    assert.equal(branch.is_default, false)
    assert.match(branch.project_ref, /^[a-z]{20}$/)
    // The branch's status (BranchResponse) is read from the project's branch list; the detail of a branch
    // (GET /v1/branches/{id}) reports the branch project's own status instead.
    branch = await waitFor('the branch to finish its migrations', async () => {
      const g = await mgmt('GET', `/v1/projects/${p.ref}/branches/${name}`)
      assert.equal(g.status, 200, JSON.stringify(g.body))
      assert.notEqual(g.body.status, 'MIGRATIONS_FAILED', JSON.stringify(g.body))
      return g.body.status === 'MIGRATIONS_PASSED' ? g.body : null
    }, { timeout: 200_000, interval: 3000 })
    const detail = await waitFor('the branch project to be ACTIVE_HEALTHY', async () => {
      const g = await mgmt('GET', `/v1/branches/${branch.id}`)
      assert.equal(g.status, 200, JSON.stringify(g.body))
      return g.body.status === 'ACTIVE_HEALTHY' ? g : null
    }, { timeout: 60_000, interval: 2000 })
    assert.equal(detail.body.ref, branch.project_ref)
    assert.ok(detail.body.db_host && detail.body.db_port, 'the detail names the database host and port')
  })

  test('the branch is listed next to the default branch, and found by name', async () => {
    const list = await mgmt('GET', `/v1/projects/${p.ref}/branches`)
    assert.equal(list.status, 200)
    const found = list.body.find((b) => b.id === branch.id)
    assert.ok(found, `the branch is not in the list: ${JSON.stringify(list.body.map((b) => b.name))}`)
    assert.equal(found.project_ref, branch.project_ref)
    assert.equal(list.body.filter((b) => b.is_default).length, 1, 'exactly one default branch')
    const byName = await mgmt('GET', `/v1/projects/${p.ref}/branches/${name}`)
    assert.equal(byName.status, 200)
    assert.equal(byName.body.id, branch.id)
  })

  test('the branch has the parent schema and none of its data', async () => {
    const b = { ref: branch.project_ref }
    const t = await sql(b, `select to_regclass('public.${table}') is not null as present`)
    assert.equal(t[0].present, true, 'the parent migration was not replayed in the branch')
    const n = await sql(b, `select count(*)::int as n from public.${table}`)
    assert.equal(n[0].n, 0, 'a schema-only branch has data of its parent')
    assert.equal((await sql(p, `select count(*)::int as n from public.${table}`))[0].n, 1, 'the parent lost its data')
  })

  test('deleting the branch removes it', async () => {
    const d = await mgmt('DELETE', `/v1/branches/${branch.id}`)
    assert.equal(d.status, 200, JSON.stringify(d.body))
    const gone = await waitFor('the branch to be gone', async () => {
      const list = await mgmt('GET', `/v1/projects/${p.ref}/branches`)
      return list.status === 200 && !list.body.some((b) => b.id === branch.id)
    }, { timeout: 120_000, interval: 2000 })
    assert.ok(gone)
    assert.equal((await mgmt('GET', `/v1/branches/${branch.id}`)).status, 404)
    assert.equal((await mgmt('GET', `/v1/projects/${p.ref}/branches/${name}`)).status, 404)
    branch = undefined
  })
})

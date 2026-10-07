// Seeds the node that node.sh installed with a small demo organization (Acme): five projects, and
// in "storefront" a products table with twelve rows and a read policy.
// Everything goes through the Management API, the way a customer's tooling would.
//
//   SHOTS_DIR=/tmp/shots node seed.mjs
//
// Reads $SHOTS_DIR/env.json (node.sh) and writes $SHOTS_DIR/seed.json (project refs and names for
// shots.mjs). Needs passwordless sudo to add each project's host name to /etc/hosts.
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'

const DIR = process.env.SHOTS_DIR ?? '/tmp/shots'
const E = JSON.parse(readFileSync(join(DIR, 'env.json'), 'utf8'))
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const hex = (n) => randomBytes(n).toString('hex')

async function api(method, path, body) {
  const r = await fetch(`${E.apiUrl}${path}`, {
    method,
    headers: { authorization: `Bearer ${E.pat}`, 'content-type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await r.text()
  if (!r.ok) throw new Error(`${method} ${path} -> ${r.status} ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}
const sql = (ref, query) => api('POST', `/v1/projects/${ref}/database/query`, { query })
const addHost = (host) => execFileSync('sudo', ['tee', '-a', '/etc/hosts'], { input: `127.0.0.1 ${host}\n`, stdio: ['pipe', 'ignore', 'inherit'] })

async function waitFor(what, fn, { tries = 120, every = 3000 } = {}) {
  let last
  for (let i = 0; i < tries; i++) {
    try {
      const v = await fn()
      if (v) return v
    } catch (e) {
      last = e
    }
    await sleep(every)
  }
  throw new Error(`timed out waiting for ${what}${last ? `: ${last.message}` : ''}`)
}

async function createProject(name) {
  log('project', name)
  const p = await api('POST', '/v1/projects', { name, organization_slug: E.org, db_pass: `Acme-${hex(12)}`, region: 'us-east-1' })
  const ref = p.ref
  addHost(`${ref}.api.${E.domain}`)
  await waitFor(`${name} to be healthy`, async () => {
    const s = (await api('GET', `/v1/projects/${ref}`)).status
    if (s === 'INIT_FAILED') throw new Error(`${name} is INIT_FAILED`)
    return s === 'ACTIVE_HEALTHY'
  })
  return { name, ref }
}

const PRODUCTS = [
  ['MUG-001', 'Ceramic Pour-Over Set', 'Kitchen', 48.0, 64],
  ['BRD-014', 'Walnut Cutting Board', 'Kitchen', 64.0, 31],
  ['BLK-220', 'Linen Throw Blanket', 'Home', 89.0, 22],
  ['LMP-007', 'Brass Desk Lamp', 'Home', 124.0, 15],
  ['TOT-031', 'Waxed Canvas Tote', 'Bags', 72.0, 48],
  ['BAG-105', 'Everyday Leather Backpack', 'Bags', 238.0, 9],
  ['NTB-042', 'Dot-Grid Notebook (3-pack)', 'Stationery', 24.0, 210],
  ['PEN-009', 'Brass Fountain Pen', 'Stationery', 58.0, 37],
  ['CND-118', 'Soy Candle, Cedar & Sage', 'Home', 28.0, 88],
  ['TEA-300', 'Single-Origin Tea Sampler', 'Pantry', 32.0, 120],
  ['SPC-204', 'Smoked Sea Salt Trio', 'Pantry', 19.0, 142],
  ['KNF-077', 'Carbon-Steel Chef Knife', 'Kitchen', 145.0, 12],
]
const q = (s) => `'${String(s).replace(/'/g, "''")}'`

async function seedStorefront(p) {
  log('storefront: products table')
  await sql(p.ref, `
    create table public.products (
      id bigint generated always as identity primary key,
      sku text not null unique,
      name text not null,
      category text not null,
      price numeric(10,2) not null check (price >= 0),
      stock integer not null default 0,
      is_active boolean not null default true
    );
    comment on table public.products is 'The catalog shown in the storefront.';
    insert into public.products (sku, name, category, price, stock)
    values ${PRODUCTS.map(([sku, name, cat, price, stock]) => `(${q(sku)}, ${q(name)}, ${q(cat)}, ${price}, ${stock})`).join(',\n')};
    alter table public.products enable row level security;
    create policy "Anyone can read active products" on public.products for select to anon, authenticated using (is_active);
  `)
  await sql(p.ref, `notify pgrst, 'reload schema';`)
}

async function seedSmall(p, ddl) {
  await sql(p.ref, ddl)
  await sql(p.ref, `notify pgrst, 'reload schema';`)
}

const storefront = await createProject('storefront')
const analytics = await createProject('analytics')
const sandbox = await createProject('agent-sandbox')
const support = await createProject('support-desk')
const internal = await createProject('internal-tools')

await seedStorefront(storefront)

await seedSmall(analytics, `
  create table public.page_views (id bigint generated always as identity primary key, path text not null, referrer text, country text, viewed_at timestamptz not null default now());
  insert into public.page_views (path, referrer, country, viewed_at)
  select (array['/','/pricing','/docs','/blog/launch','/changelog'])[1 + (g % 5)], (array['google','news.ycombinator.com','twitter','direct'])[1 + (g % 4)],
         (array['US','DE','GB','CA','JP','BR'])[1 + (g % 6)], now() - (g * interval '37 minutes')
  from generate_series(1, 200) g;
  alter table public.page_views enable row level security;`)
await seedSmall(sandbox, `
  create table public.agent_runs (id bigint generated always as identity primary key, task text not null, status text not null, started_at timestamptz not null default now());
  insert into public.agent_runs (task, status) values ('Add a coupon_code column to orders', 'succeeded'), ('Backfill product slugs', 'succeeded'), ('Draft RLS for order_items', 'running');
  alter table public.agent_runs enable row level security;`)

writeFileSync(join(DIR, 'seed.json'), JSON.stringify({
  org: E.org,
  projects: { storefront: storefront.ref, analytics: analytics.ref, 'agent-sandbox': sandbox.ref, 'support-desk': support.ref, 'internal-tools': internal.ref },
}, null, 2))
log('seeded', E.org, storefront.ref, analytics.ref, sandbox.ref)

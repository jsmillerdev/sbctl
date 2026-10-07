// Seeds the node that node.sh installed with a small demo organization (Acme): three projects, and
// in "storefront" a products/orders schema with twelve products, fourteen orders and RLS policies.
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

async function seedStorefront(p) {
  log('storefront: schema and data')
  await sql(p.ref, `
    create table public.products (
      id bigint generated always as identity primary key,
      sku text not null unique,
      name text not null,
      category text not null,
      price numeric(10,2) not null check (price >= 0),
      stock integer not null default 0,
      is_active boolean not null default true,
      created_at timestamptz not null default now()
    );
    create table public.orders (
      id bigint generated always as identity primary key,
      customer_email text not null,
      status text not null check (status in ('pending','paid','shipped','delivered','refunded')),
      total numeric(10,2) not null,
      created_at timestamptz not null default now()
    );
    create table public.order_items (
      id bigint generated always as identity primary key,
      order_id bigint not null references public.orders (id) on delete cascade,
      product_id bigint not null references public.products (id),
      quantity integer not null check (quantity > 0),
      unit_price numeric(10,2) not null
    );
    comment on table public.products is 'The catalog shown in the storefront.';
    comment on table public.orders is 'One row per checkout.';
    insert into public.products (sku, name, category, price, stock, created_at)
    values ${PRODUCTS.map(([sku, name, cat, price, stock], i) => `(${q(sku)}, ${q(name)}, ${q(cat)}, ${price}, ${stock}, now() - interval '${60 - i * 4} days')`).join(',\n')};
    alter table public.products enable row level security;
    alter table public.orders enable row level security;
    alter table public.order_items enable row level security;
    create policy "Anyone can read active products" on public.products for select to anon, authenticated using (is_active);
    create policy "Customers read their own orders" on public.orders for select to authenticated using ((select auth.jwt() ->> 'email') = customer_email);
    create policy "Customers read their own order items" on public.order_items for select to authenticated
      using (exists (select 1 from public.orders o where o.id = order_id and o.customer_email = (select auth.jwt() ->> 'email')));
    create index order_items_order_id_idx on public.order_items (order_id);
    create index orders_customer_email_idx on public.orders (customer_email);
  `)
  const statuses = ['delivered', 'delivered', 'shipped', 'paid', 'delivered', 'pending', 'shipped', 'delivered', 'refunded', 'paid', 'delivered', 'shipped', 'paid', 'delivered']
  const orders = statuses.map((status, i) => ({ email: CUSTOMERS[i % CUSTOMERS.length], status, daysAgo: 28 - i * 2, p1: (i * 5) % 12 + 1, p2: (i * 7 + 3) % 12 + 1, qty: (i % 3) + 1 }))
  await sql(p.ref, orders.map((o) => `
    with o as (
      insert into public.orders (customer_email, status, total, created_at)
      select ${q(o.email)}, ${q(o.status)}, (select price from public.products where id = ${o.p1}) * ${o.qty} + (select price from public.products where id = ${o.p2}),
             now() - interval '${o.daysAgo} days' - interval '${(o.qty * 37) % 600} minutes'
      returning id
    )
    insert into public.order_items (order_id, product_id, quantity, unit_price)
    select o.id, ${o.p1}, ${o.qty}, (select price from public.products where id = ${o.p1}) from o
    union all
    select o.id, ${o.p2}, 1, (select price from public.products where id = ${o.p2}) from o;`).join('\n'))
  await sql(p.ref, `notify pgrst, 'reload schema';`)
}

async function seedSmall(p, ddl) {
  await sql(p.ref, ddl)
  await sql(p.ref, `notify pgrst, 'reload schema';`)
}

const storefront = await createProject('storefront')
const analytics = await createProject('analytics')
const sandbox = await createProject('agent-sandbox')

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
  projects: { storefront: storefront.ref, analytics: analytics.ref, 'agent-sandbox': sandbox.ref },
}, null, 2))
log('seeded', E.org, storefront.ref, analytics.ref, sandbox.ref)

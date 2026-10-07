// Seeds the node that node.sh installed with a tasteful demo organization (Acme): three projects,
// and in "storefront" a products/orders schema with an RLS policy, five users, a Storage bucket
// with generated images, two Edge Functions and two branches. Everything goes through the
// Management API, SQL, supabase-js and the Supabase CLI, the way a customer would.
//
//   SHOTS_DIR=/tmp/shots SUPABASE_CLI=.../node_modules/.bin/supabase node seed.mjs
//
// Reads $SHOTS_DIR/env.json (node.sh) and writes $SHOTS_DIR/seed.json (project refs and names for
// shots.mjs). Needs passwordless sudo to add each project's host name to /etc/hosts.
import { execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { deflateSync } from 'node:zlib'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'

import { createClient } from '@supabase/supabase-js'

const DIR = process.env.SHOTS_DIR ?? '/tmp/shots'
const E = JSON.parse(readFileSync(join(DIR, 'env.json'), 'utf8'))
const CLI = process.env.SUPABASE_CLI ?? 'supabase'
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
  const keys = await api('GET', `/v1/projects/${ref}/api-keys?reveal=true`)
  const find = (pre) => keys.find((k) => String(k.api_key ?? '').startsWith(pre))?.api_key
  const legacy = (n) => keys.find((k) => k.name === n && k.type === 'legacy')?.api_key
  return {
    name, ref, url: `http://${ref}.api.${E.domain}`,
    publishable: find('sb_publishable_'), secret: find('sb_secret_'), serviceRole: legacy('service_role'),
  }
}

// ---- tiny PNG encoder: diagonal gradient with a soft shape, no dependencies ----------------
const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
  return c >>> 0
})
const crc32 = (buf) => {
  let c = 0xffffffff
  for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8)
  return (c ^ 0xffffffff) >>> 0
}
const chunk = (type, data) => {
  const t = Buffer.from(type)
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length)
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(Buffer.concat([t, data])))
  return Buffer.concat([len, t, data, crc])
}
function png(w, h, [c1, c2], shape) {
  const rows = []
  for (let y = 0; y < h; y++) {
    const row = Buffer.alloc(1 + w * 3)
    for (let x = 0; x < w; x++) {
      const t = (x / w + y / h) / 2
      let rgb = c1.map((v, i) => v + (c2[i] - v) * t)
      const dx = (x - w / 2) / (w * 0.22), dy = (y - h / 2) / (h * 0.3)
      const d = shape === 'round' ? Math.hypot(dx, dy) : Math.max(Math.abs(dx), Math.abs(dy))
      if (d < 1) rgb = rgb.map((v) => v + (255 - v) * 0.38)
      else if (d < 1.06) rgb = rgb.map((v) => v + (255 - v) * 0.15)
      rgb.forEach((v, i) => (row[1 + x * 3 + i] = Math.max(0, Math.min(255, Math.round(v)))))
    }
    rows.push(row)
  }
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(w, 0); ihdr.writeUInt32BE(h, 4); ihdr[8] = 8; ihdr[9] = 2
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), chunk('IDAT', deflateSync(Buffer.concat(rows))), chunk('IEND', Buffer.alloc(0))])
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
const USERS = [
  ['maya.chen@example.com', 'Maya Chen'],
  ['diego.alvarez@example.com', 'Diego Alvarez'],
  ['priya.nair@example.com', 'Priya Nair'],
  ['tom.becker@example.com', 'Tom Becker'],
  ['amara.okafor@example.com', 'Amara Okafor'],
]
const q = (s) => `'${String(s).replace(/'/g, "''")}'`

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
  const orders = statuses.map((status, i) => ({ email: USERS[i % USERS.length][0], status, daysAgo: 28 - i * 2, p1: (i * 5) % 12 + 1, p2: (i * 7 + 3) % 12 + 1, qty: (i % 3) + 1 }))
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

  log('storefront: auth users')
  const admin = createClient(p.url, p.secret ?? p.serviceRole, { auth: { persistSession: false, autoRefreshToken: false } })
  const passwords = {}
  for (const [email, name] of USERS) {
    passwords[email] = `Acme-${hex(10)}`
    let { error } = await admin.auth.admin.createUser({ email, password: passwords[email], email_confirm: true, user_metadata: { full_name: name } })
    if (error && p.serviceRole) {
      log('  secret key refused by auth admin, retrying with the legacy service_role key:', error.message)
      const legacy = createClient(p.url, p.serviceRole, { auth: { persistSession: false, autoRefreshToken: false } })
      ;({ error } = await legacy.auth.admin.createUser({ email, password: passwords[email], email_confirm: true, user_metadata: { full_name: name } }))
    }
    if (error) throw new Error(`createUser ${email}: ${error.message}`)
  }
  // Sign four of them in so "Last sign in" has values.
  for (const [email] of USERS.slice(0, 4)) {
    const c = createClient(p.url, p.publishable, { auth: { persistSession: false, autoRefreshToken: false } })
    const { error } = await c.auth.signInWithPassword({ email, password: passwords[email] })
    if (error) throw new Error(`sign-in ${email}: ${error.message}`)
  }

  log('storefront: storage')
  const st = createClient(p.url, p.secret ?? p.serviceRole, { auth: { persistSession: false, autoRefreshToken: false } })
  let { error: be } = await st.storage.createBucket('product-images', { public: true })
  if (be) throw new Error(`createBucket: ${be.message}`)
  await st.storage.createBucket('receipts', { public: false })
  const pal = [
    [[236, 196, 164], [201, 120, 92]], [[168, 140, 110], [96, 72, 52]], [[201, 214, 223], [112, 140, 160]],
    [[245, 214, 120], [176, 124, 40]], [[188, 205, 176], [92, 128, 96]], [[212, 178, 160], [120, 70, 60]],
    [[222, 222, 230], [132, 136, 160]], [[240, 200, 190], [190, 110, 130]],
  ]
  const files = ['ceramic-pour-over-set.png', 'walnut-cutting-board.png', 'linen-throw-blanket.png', 'brass-desk-lamp.png', 'waxed-canvas-tote.png', 'leather-backpack.png', 'banners/spring-sale.png', 'banners/new-arrivals.png']
  for (const [i, f] of files.entries()) {
    const big = f.startsWith('banners/')
    const { error } = await st.storage.from('product-images').upload(f, png(big ? 960 : 640, big ? 320 : 480, pal[i % pal.length], i % 2 ? 'round' : 'box'), { contentType: 'image/png', upsert: true })
    if (error) throw new Error(`upload ${f}: ${error.message}`)
  }
}

async function seedSmall(p, ddl) {
  await sql(p.ref, ddl)
  await sql(p.ref, `notify pgrst, 'reload schema';`)
}

function deployFunctions(p) {
  log('storefront: edge functions through the Supabase CLI')
  const dir = join(DIR, 'fn')
  const fns = {
    hello: `Deno.serve(async (req) => {
  const { name = 'world' } = await req.json().catch(() => ({}))
  return new Response(JSON.stringify({ message: \`Hello \${name}!\` }), {
    headers: { 'Content-Type': 'application/json' },
  })
})
`,
    'send-receipt': `// Called by a database webhook when an order is paid.
Deno.serve(async (req) => {
  const { record } = await req.json()
  console.log('receipt for order', record?.id)
  return new Response(JSON.stringify({ ok: true, order: record?.id ?? null }), {
    headers: { 'Content-Type': 'application/json' },
  })
})
`,
  }
  for (const [slug, code] of Object.entries(fns)) {
    mkdirSync(join(dir, 'supabase/functions', slug), { recursive: true })
    writeFileSync(join(dir, 'supabase/functions', slug, 'index.ts'), code)
  }
  for (const slug of Object.keys(fns)) {
    execFileSync(CLI, ['--profile', join(DIR, 'profile.yaml'), 'functions', 'deploy', slug, '--use-api', '--project-ref', p.ref], {
      cwd: dir, stdio: 'inherit',
      env: { ...process.env, HOME: join(DIR, 'cli-home'), SUPABASE_ACCESS_TOKEN: E.pat, SUPABASE_NO_KEYRING: '1', DO_NOT_TRACK: '1', SUPABASE_DISABLE_UPDATE_CHECK: '1' },
    })
  }
}

async function callFunction(p) {
  // Prove the deploy works end to end, and leave a few invocations in the logs.
  for (let i = 0; i < 4; i++) {
    const r = await fetch(`${p.url}/functions/v1/hello`, { method: 'POST', headers: { apikey: p.publishable, authorization: `Bearer ${p.publishable}`, 'content-type': 'application/json' }, body: JSON.stringify({ name: USERS[i][1].split(' ')[0] }) })
    if (!r.ok && i === 3) log('  hello answered', r.status, (await r.text()).slice(0, 120))
    else if (r.ok && i === 0) log('  hello says', (await r.text()).slice(0, 80))
  }
}

async function createBranch(p, name) {
  log('branch', name)
  const b = await api('POST', `/v1/projects/${p.ref}/branches`, { branch_name: name })
  if (b.project_ref) addHost(`${b.project_ref}.api.${E.domain}`)
  await waitFor(`branch ${name}`, async () => {
    const s = (await api('GET', `/v1/branches/${b.id}`)).status
    if (/FAILED/.test(s)) throw new Error(`branch ${name} is ${s}`)
    return s === 'MIGRATIONS_PASSED' || s === 'FUNCTIONS_DEPLOYED'
  })
  return b
}

const storefront = await createProject('storefront')
const analytics = await createProject('analytics')
const sandbox = await createProject('agent-sandbox')

await seedStorefront(storefront)
deployFunctions(storefront)
await callFunction(storefront)

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

const branches = [await createBranch(storefront, 'feature-checkout'), await createBranch(storefront, 'fix-inventory-sync')]

writeFileSync(join(DIR, 'seed.json'), JSON.stringify({
  org: E.org,
  projects: { storefront: storefront.ref, analytics: analytics.ref, 'agent-sandbox': sandbox.ref },
  branches: branches.map((b) => ({ name: b.name, ref: b.project_ref })),
  publishableKey: storefront.publishable,
}, null, 2))
log('seeded', E.org, storefront.ref, analytics.ref, sandbox.ref)

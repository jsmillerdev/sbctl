-- Cluster membership: the servers that share one registry (internal/cluster, internal/mesh).
-- Range 1300-1349 belongs to the read-replica and failover work.
--
-- Migrations from 1300 on are additive only (invariant I6): create table, create index, add a
-- column that has a default, insert seed rows, create a function or a trigger. A binary one
-- minor release behind must run against this schema, so nothing it reads or writes may change
-- shape. TestMigrationsFrom1300AreAdditive enforces it.
--
-- A node is one supavise server. The founder row exists on every registry, so
-- projects.node_id (not null, default 'n1') is valid on a registry from before clusters with no
-- backfill. The daemon fills the founder's name, region, hosts and provider at start.

create table supavise.nodes (
  id          text primary key check (id ~ '^n[0-9]{1,4}$'),
  name        text not null unique check (name ~ '^[a-z0-9][a-z0-9-]{0,40}$'),
  region      text not null default '',          -- a code from config.Regions
  public_host text not null default '',          -- clients use it for this node's pooler endpoints
  peer_addr   text not null default '',          -- host:port hint; sessions are symmetric
  provider    jsonb not null default '{}',       -- {"aws":{"instance_id","zone","region","allocation_id"}}
  version     text not null default '',
  state       text not null default 'active'
              check (state in ('joining','active','fenced','left')),
  cert_serial text not null default '',
  joined_at   timestamptz not null default now()
);
insert into supavise.nodes (id, name) values ('n1', 'primary');

-- One row. epoch counts leader changes; change_seq moves with every change to the tables the
-- other nodes read, because a standby cannot LISTEN and polls this column instead.
create table supavise.cluster (
  singleton       boolean primary key default true check (singleton),
  name            text   not null default '',
  epoch           bigint not null default 1,
  leader          text   not null default 'n1' references supavise.nodes (id),
  service_address jsonb  not null default '{}',  -- {"ip":"...","allocation_id":"eipalloc-..."}
  maintenance     jsonb  not null default '{}',  -- {"node","until","reason"}: suppresses automatic failover
  change_seq      bigint not null default 0,
  updated_at      timestamptz not null default now()
);
insert into supavise.cluster default values;

create table supavise.join_tokens (
  id          text primary key,
  secret_hash bytea not null,
  node_name   text not null default '',
  created_at  timestamptz not null default now(),
  expires_at  timestamptz not null,
  used_at     timestamptz
);

-- The home of a project: where its primary, GoTrue and PostgREST run.
alter table supavise.projects
  add column node_id text not null default 'n1' references supavise.nodes (id);

-- A statement trigger fires for every statement, also one that matches no row; the registry's
-- setters read first and skip a write that would change nothing.
create function supavise.bump_change_seq() returns trigger
language plpgsql as $$
begin
  update supavise.cluster set change_seq = change_seq + 1, updated_at = now();
  return null;
end $$;

-- Row changes of nodes and replicas reach Subscribe like the ones of projects and routes.
-- Payload: {"table","op","key"}; the key is the node id or the replica identifier.
create function supavise.notify_cluster_change() returns trigger
language plpgsql as $$
declare
  r record;
  k text;
begin
  if tg_op = 'DELETE' then r := old; else r := new; end if;
  if tg_table_name = 'nodes' then k := r.id; else k := r.identifier; end if;
  perform pg_notify('supavise_changes', json_build_object('table', tg_table_name, 'op', lower(tg_op), 'key', k)::text);
  return null;
end $$;

create trigger projects_bump after insert or update or delete on supavise.projects
  for each statement execute function supavise.bump_change_seq();
create trigger routes_bump after insert or update or delete on supavise.routes
  for each statement execute function supavise.bump_change_seq();
create trigger project_secrets_bump after insert or update or delete on supavise.project_secrets
  for each statement execute function supavise.bump_change_seq();
create trigger nodes_bump after insert or update or delete on supavise.nodes
  for each statement execute function supavise.bump_change_seq();
create trigger nodes_notify after insert or update or delete on supavise.nodes
  for each row execute function supavise.notify_cluster_change();

-- sbctl registry, database "sbctl" in the system cluster.
-- Migration files are applied in lexical order. Number ranges per area avoid
-- collisions between parallel work: 0001-0099 core, 0100-0199 api, 0200-0299 proxy,
-- 0300-0399 lifecycle/units, 0400-0499 backup, 0500-0599 fleet, 0600-0699 installer, 0700-0799 branching.

create table sbctl.organizations (
  id         bigint generated always as identity primary key,
  slug       text not null unique check (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  name       text not null,
  created_at timestamptz not null default now()
);

create table sbctl.projects (
  ref        text primary key check (ref = 'system' or ref ~ '^[a-z]{20}$'),
  org_id     bigint references sbctl.organizations (id) on delete restrict,
  seq        integer not null unique check (seq >= 0),
  name       text not null,
  region     text not null default 'local',
  engine     text not null default 'postgres' check (engine in ('postgres', 'file')),
  class      text not null default 'default',
  status     text not null,
  versions   jsonb not null default '{}'::jsonb,
  limits     jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
create index projects_org_id_idx on sbctl.projects (org_id);

create table sbctl.project_secrets (
  ref        text not null references sbctl.projects (ref) on delete cascade,
  name       text not null,
  ciphertext bytea not null,
  created_at timestamptz not null default now(),
  primary key (ref, name)
);

create table sbctl.access_tokens (
  id           bigint generated always as identity primary key,
  user_id      uuid not null,
  name         text not null,
  token_hash   bytea not null unique,
  token_prefix text not null,
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  expires_at   timestamptz
);
create index access_tokens_user_id_idx on sbctl.access_tokens (user_id);

create table sbctl.routes (
  host       text primary key,
  ref        text not null references sbctl.projects (ref) on delete cascade,
  kind       text not null default 'api',
  created_at timestamptz not null default now()
);
create index routes_ref_idx on sbctl.routes (ref);

create table sbctl.backups (
  id          bigint generated always as identity primary key,
  ref         text not null,
  kind        text not null default 'base' check (kind in ('base')),
  status      text not null check (status in ('running', 'completed', 'failed')),
  location    text not null default '',
  timeline    integer not null default 0,
  start_lsn   text not null default '',
  stop_lsn    text not null default '',
  size_bytes  bigint not null default 0,
  error       text not null default '',
  started_at  timestamptz not null default now(),
  finished_at timestamptz
);
-- No foreign key: backups outlive their project so a deleted project can be restored.
create index backups_ref_started_idx on sbctl.backups (ref, started_at desc);

create table sbctl.events (
  id         bigint generated always as identity primary key,
  ref        text,
  kind       text not null,
  payload    jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);
create index events_ref_idx on sbctl.events (ref, id desc);

-- Change feed for in-memory caches (proxy host table, API). Payload: {"table","op","key"}.
create function sbctl.notify_change() returns trigger
language plpgsql as $$
declare
  r record;
  k text;
begin
  if tg_op = 'DELETE' then r := old; else r := new; end if;
  if tg_table_name = 'routes' then k := r.host; else k := r.ref; end if;
  perform pg_notify('sbctl_changes', json_build_object('table', tg_table_name, 'op', lower(tg_op), 'key', k)::text);
  return null;
end $$;

create trigger projects_notify after insert or update or delete on sbctl.projects
  for each row execute function sbctl.notify_change();
create trigger routes_notify after insert or update or delete on sbctl.routes
  for each row execute function sbctl.notify_change();
create trigger project_secrets_notify after insert or update or delete on sbctl.project_secrets
  for each row execute function sbctl.notify_change();

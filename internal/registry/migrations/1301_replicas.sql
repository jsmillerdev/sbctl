-- Read replicas, their opt-outs and the log of failovers (internal/replicas, internal/failover).
-- Additive only, like 1300 (invariant I6).

-- One row per standby Postgres of a project on a node that is not the project's home. The
-- system cluster's standbys are rows too (ref 'system'); the platform listings hide them.
create table supavise.replicas (
  identifier text primary key
             check (identifier ~ '^(system|[a-z]{20})-rr-[a-z0-9-]+-[a-z0-9]{6}$'),
  ref        text not null references supavise.projects (ref) on delete cascade,
  node_id    text not null references supavise.nodes (id) on delete restrict,
  origin     text not null default 'manual' check (origin in ('manual','default','system')),
  status     text not null default 'INIT_READ_REPLICA',
  init_step  text not null default '0_requested',   -- 0_requested ... 6_completed_read_replica_setup
  init_error text not null default '',              -- '' or 1_..._failed ... 5_..._failed
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (ref, node_id)
);
create index replicas_node on supavise.replicas (node_id);

-- A replica the operator removed from a node while [replicas] default = "all" would put one
-- there: the default reconciler leaves the pair alone until a later setup clears the row.
create table supavise.replica_optouts (
  ref     text not null references supavise.projects (ref) on delete cascade,
  node_id text not null references supavise.nodes (id) on delete cascade,
  primary key (ref, node_id)
);

-- Audit and resume log of switchovers and failovers. No foreign key on ref: the log outlives
-- the project.
create table supavise.moves (
  id         bigint generated always as identity primary key,
  scope      text not null check (scope in ('project','server')),
  kind       text not null check (kind in ('switchover','failover')),
  ref        text,
  from_node  text not null,
  to_node    text not null,
  epoch      bigint not null,
  state      text not null default 'running' check (state in ('running','done','failed','aborted')),
  steps      jsonb not null default '[]',
  error      text not null default '',
  started_at timestamptz not null default now(),
  ended_at   timestamptz
);

create trigger replicas_bump after insert or update or delete on supavise.replicas
  for each statement execute function supavise.bump_change_seq();
create trigger replicas_notify after insert or update or delete on supavise.replicas
  for each row execute function supavise.notify_cluster_change();

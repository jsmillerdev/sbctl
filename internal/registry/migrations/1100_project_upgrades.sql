-- Project upgrades (internal/lifecycle/upgrade.go): one row per upgrade of a project's
-- Postgres, GoTrue and PostgREST versions. GET /v1/projects/{ref}/upgrade/status answers from
-- the newest row of the project, so Studio's upgrade screen and its failure banner survive a
-- daemon restart. status follows Studio's DatabaseUpgradeStatus: 0 upgrading, 1 upgraded, 2 failed.

create table supavise.project_upgrades (
  tracking_id       uuid primary key,
  ref               text not null references supavise.projects (ref) on delete cascade,
  from_versions     jsonb not null default '{}',
  to_versions       jsonb not null default '{}',
  target_version    text not null default '',
  status            smallint not null default 0 check (status in (0, 1, 2)),
  progress          text not null default '0_requested',
  error             text not null default '',
  detail            text not null default '',
  backup_id         bigint not null default 0,
  initiated_at      timestamptz not null default now(),
  latest_status_at  timestamptz not null default now()
);

create index project_upgrades_ref_idx on supavise.project_upgrades (ref, initiated_at desc);

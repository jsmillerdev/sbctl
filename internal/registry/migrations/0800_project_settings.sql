-- Per-project service settings (internal/projectconfig). Range 0800-0899 belongs to the
-- settings workstream.
--
-- One row per project and service holds only what the user changed: "values" has the
-- plain settings, "sealed" the secret ones (OAuth client secrets, the SMTP password, hook
-- secrets, ...) as base64 of secrets.Secrets.Seal, so a database dump or a read-only
-- registry user never sees them. A setting that has no entry takes its default, which is
-- what the unit renderer produces without any row. "version" counts the writes of the row;
-- a writer passes the version it read and the update applies only if it still matches, so
-- two concurrent saves cannot silently overwrite each other.
create table sbctl.project_settings (
  ref        text   not null references sbctl.projects (ref) on delete cascade,
  service    text   not null check (service in ('auth', 'postgrest', 'realtime', 'storage', 'postgres')),
  version    bigint not null default 1 check (version > 0),
  "values"   jsonb  not null default '{}'::jsonb check (jsonb_typeof("values") = 'object'),
  sealed     jsonb  not null default '{}'::jsonb check (jsonb_typeof(sealed) = 'object'),
  updated_at timestamptz not null default now(),
  primary key (ref, service)
);

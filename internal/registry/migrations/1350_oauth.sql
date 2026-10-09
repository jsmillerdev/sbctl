-- OAuth 2.1 authorization server of the Management API (internal/oauth, internal/api/oauth_*.go).
-- Range 1350-1379 belongs to OAuth. Additive only (invariant I6).
-- Tokens live here and never in access_tokens: a release without OAuth then finds no row for an
-- sbp_oauth_ token and answers 401, instead of reading it as a personal access token.
-- Types follow the supabase-postgres-best-practices skill: text, timestamptz, identity keys, an
-- index on every foreign key. Secrets, codes and tokens are stored as SHA-256 only.

create table supavise.oauth_apps (                         -- id is the client_id
  id                         uuid primary key default gen_random_uuid(),
  registration_type          text not null check (registration_type in ('manual','dynamic')),
  org_id                     bigint references supavise.organizations (id) on delete cascade,
  name                       text not null check (char_length(name) between 1 and 100),
  website                    text not null default '',
  icon                       text not null default '',
  redirect_uris              text[] not null check (cardinality(redirect_uris) between 1 and 10),
  scopes                     text[] not null check (cardinality(scopes) >= 1),
  token_endpoint_auth_method text not null default 'client_secret_basic'
                             check (token_endpoint_auth_method in ('none','client_secret_basic','client_secret_post')),
  created_by                 uuid,
  created_at                 timestamptz not null default now(),
  updated_at                 timestamptz not null default now(),
  last_authorized_at         timestamptz,
  deleted_at                 timestamptz,
  check ((registration_type = 'manual') = (org_id is not null))
);
create index oauth_apps_org_idx     on supavise.oauth_apps (org_id) where org_id is not null;
create index oauth_apps_dynamic_idx on supavise.oauth_apps (created_at) where registration_type = 'dynamic';

create table supavise.oauth_app_secrets (
  id           uuid primary key default gen_random_uuid(),
  app_id       uuid not null references supavise.oauth_apps (id) on delete cascade,
  alias        text not null,                                -- 'sba_1a2b********'
  secret_hash  bytea not null unique,
  created_by   uuid,
  created_at   timestamptz not null default now(),
  last_used_at timestamptz
);
create index oauth_app_secrets_app_idx on supavise.oauth_app_secrets (app_id);

create table supavise.oauth_grants (
  id             bigint generated always as identity primary key,
  app_id         uuid   not null references supavise.oauth_apps (id) on delete cascade,
  user_id        uuid   not null,                            -- GoTrue user; no FK, like org_members
  org_id         bigint not null references supavise.organizations (id) on delete cascade,
  scopes         text[] not null,
  resource       text   not null default '',
  created_at     timestamptz not null default now(),
  last_used_at   timestamptz,
  revoked_at     timestamptz,
  revoked_reason text not null default ''
                 check (revoked_reason in ('','user','admin','operator','client','app_deleted','superseded',
                                           'refresh_reuse','code_reuse','membership','user_removed'))
);
create index oauth_grants_app_idx  on supavise.oauth_grants (app_id);
create index oauth_grants_user_idx on supavise.oauth_grants (user_id);
create index oauth_grants_org_idx  on supavise.oauth_grants (org_id);
create index oauth_grants_live_idx on supavise.oauth_grants (user_id, org_id) where revoked_at is null;

create table supavise.oauth_authorizations (               -- a pending request, then a code
  id                uuid primary key,                      -- the auth_id Studio carries
  app_id            uuid not null references supavise.oauth_apps (id) on delete cascade,
  redirect_uri      text not null,                         -- as requested, loopback port included
  scopes            text[] not null,
  state             text not null default '',
  code_challenge    text not null default '',              -- S256 only
  resource          text not null default '',
  org_hint          text not null default '',
  created_at        timestamptz not null default now(),
  expires_at        timestamptz not null,
  status            text not null default 'pending'
                    check (status in ('pending','approved','declined','exchanged')),
  decided_by        uuid,
  decided_at        timestamptz,
  org_id            bigint references supavise.organizations (id) on delete cascade,
  code_hash         bytea unique,
  code_expires_at   timestamptz,
  code_used_at      timestamptz,
  grant_id          bigint references supavise.oauth_grants (id) on delete set null
);
create index oauth_authorizations_app_idx     on supavise.oauth_authorizations (app_id);
create index oauth_authorizations_expires_idx on supavise.oauth_authorizations (expires_at);
create index oauth_authorizations_org_idx     on supavise.oauth_authorizations (org_id) where org_id is not null;
create index oauth_authorizations_grant_idx   on supavise.oauth_authorizations (grant_id) where grant_id is not null;

create table supavise.oauth_tokens (
  id           bigint generated always as identity primary key,
  grant_id     bigint not null references supavise.oauth_grants (id) on delete cascade,
  kind         text   not null check (kind in ('access','refresh')),
  token_hash   bytea  not null unique,
  prefix       text   not null,
  created_at   timestamptz not null default now(),
  expires_at   timestamptz not null,
  last_used_at timestamptz,
  used_at      timestamptz,                                -- refresh: when it was first exchanged
  replaced_by  bigint
);
create index oauth_tokens_grant_idx   on supavise.oauth_tokens (grant_id);
create index oauth_tokens_expires_idx on supavise.oauth_tokens (expires_at);

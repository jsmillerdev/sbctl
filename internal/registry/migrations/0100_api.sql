-- Management API server state (internal/api). Range 0100-0199 belongs to the API.
-- Types follow the supabase-postgres-best-practices skill: text, timestamptz,
-- identity keys, indexes on every foreign key and filter column.

-- Dashboard users seen by the API (GoTrue supavise-gotrue@system owns the identities;
-- this is the profile data Studio and the CLI read back, and what PATs resolve to).
create table supavise.api_users (
  user_id      uuid primary key,
  -- Numeric id: Studio's profile, content and audit types carry numeric user ids.
  id           bigint generated always as identity unique,
  email        text not null default '',
  username     text not null default '',
  first_name   text not null default '',
  last_name    text not null default '',
  created_at   timestamptz not null default now(),
  last_seen_at timestamptz not null default now()
);

-- `supabase login` device flow: the dashboard creates the session after the user
-- authorizes it, the CLI polls it once with the 8-character verification code.
create table supavise.api_cli_login_sessions (
  session_id        uuid primary key,
  user_id           uuid not null,
  token_id          bigint references supavise.access_tokens (id) on delete cascade,
  server_public_key text not null,
  nonce             text not null,
  ciphertext        text not null,
  created_at        timestamptz not null default now(),
  expires_at        timestamptz not null
);
create index api_cli_login_sessions_expires_idx on supavise.api_cli_login_sessions (expires_at);
create index api_cli_login_sessions_token_idx on supavise.api_cli_login_sessions (token_id);

-- Edge Function deployments. The runtime is phase 2; the API stores what the CLI
-- uploads so `functions deploy|list|download|delete` work today.
create table supavise.api_functions (
  ref              text not null references supavise.projects (ref) on delete cascade,
  slug             text not null,
  id               uuid not null default gen_random_uuid(),
  name             text not null,
  version          integer not null default 1,
  status           text not null default 'ACTIVE',
  verify_jwt       boolean not null default true,
  entrypoint_path  text,
  import_map_path  text,
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  primary key (ref, slug)
);

create table supavise.api_function_files (
  ref     text not null,
  slug    text not null,
  path    text not null,
  content bytea not null,
  primary key (ref, slug, path),
  foreign key (ref, slug) references supavise.api_functions (ref, slug) on delete cascade
);

-- Edge Function secrets, sealed with the node master key.
create table supavise.api_function_secrets (
  ref        text not null references supavise.projects (ref) on delete cascade,
  name       text not null,
  ciphertext bytea not null,
  updated_at timestamptz not null default now(),
  primary key (ref, name)
);

-- Studio's saved SQL snippets, reports and folders (GET/PUT /platform/projects/{ref}/content).
create table supavise.api_content_folders (
  id         uuid primary key default gen_random_uuid(),
  ref        text not null references supavise.projects (ref) on delete cascade,
  parent_id  uuid references supavise.api_content_folders (id) on delete cascade,
  owner_id   bigint not null references supavise.api_users (id) on delete cascade,
  name       text not null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
create index api_content_folders_ref_idx on supavise.api_content_folders (ref);
create index api_content_folders_parent_idx on supavise.api_content_folders (parent_id);
create index api_content_folders_owner_idx on supavise.api_content_folders (owner_id);

create table supavise.api_content (
  id          uuid primary key default gen_random_uuid(),
  ref         text not null references supavise.projects (ref) on delete cascade,
  folder_id   uuid references supavise.api_content_folders (id) on delete set null,
  owner_id    bigint not null references supavise.api_users (id) on delete cascade,
  type        text not null,
  name        text not null,
  description text not null default '',
  visibility  text not null default 'user',
  favorite    boolean not null default false,
  content     jsonb not null default '{}'::jsonb,
  inserted_at timestamptz not null default now(),
  updated_at  timestamptz not null default now()
);
create index api_content_ref_type_idx on supavise.api_content (ref, type);
create index api_content_folder_idx on supavise.api_content (folder_id);
create index api_content_owner_idx on supavise.api_content (owner_id);

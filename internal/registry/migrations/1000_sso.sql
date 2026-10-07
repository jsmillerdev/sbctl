-- Single sign-on for the dashboard (workstream L, range 1000-1099).
--
-- sso_providers records the SAML identity providers that sbctl registered in sb-gotrue@system
-- (GoTrue's own tables hold the metadata; this table says which organization a provider belongs
-- to, which email domains it vouches for and which role its users get on a first sign-in). A
-- dashboard session of a user who signed in through a provider that is not listed here is
-- refused, so a provider created behind sbctl's back opens nothing.
create table sbctl.sso_providers (
  id           uuid        primary key,          -- GoTrue's auth.sso_providers.id
  org_id       bigint      not null references sbctl.organizations (id) on delete cascade,
  entity_id    text        not null,
  domains      text[]      not null default '{}',                  -- lower case
  default_role smallint    check (default_role between 1 and 4),   -- null: users wait for approval
  created_by   uuid,
  created_at   timestamptz not null default now()
);
create index sso_providers_org_idx on sbctl.sso_providers (org_id);

-- The dashboard users that signed in through SSO and what became of them: 'active' once they
-- belong to an organization (by the default role of their email domain, or approved by an
-- administrator), 'pending' while they wait. A pending user has no access to anything.
-- A user who loses every membership later goes back to 'pending' and is not granted the default
-- role again: the default role is for the first sign-in only.
create table sbctl.sso_users (
  user_id     uuid        primary key,
  provider_id uuid        not null,
  email       text        not null,
  state       text        not null check (state in ('pending', 'active')),
  first_seen  timestamptz not null default now(),
  last_seen   timestamptz not null default now()
);
create index sso_users_state_idx on sbctl.sso_users (state, provider_id);

-- One-time sign-up grants. sb-gotrue@system refuses to create any user the dashboard does not
-- accept (the before-user-created hook, internal/sso/hook.go), and an invitation sent by mail
-- is GoTrue creating a user: the daemon stores the hash of a random token for the invited
-- address, puts the token in the invite's user_metadata, and the hook consumes the grant, so a
-- person who merely knows the address cannot sign up with it first.
create table sbctl.signup_grants (
  token_hash bytea       primary key,
  email      text        not null,
  expires_at timestamptz not null
);
create index signup_grants_expires_idx on sbctl.signup_grants (expires_at);

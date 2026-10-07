-- Organization members, roles and invitations (internal/members). Range 0900-0999 belongs
-- to the roles workstream (K part 2). Types follow the supabase-postgres-best-practices
-- skill: text, timestamptz, identity keys, an index on every foreign key and filter column.
--
-- Roles are the four hosted organization roles, kept in code (internal/members/roles.go)
-- and referenced here by id: 1 Owner, 2 Administrator, 3 Developer, 4 Read-only.

-- One row per user and organization. role_id is the organization-wide role; null means the
-- member holds only project-scoped roles (org_project_roles) and sees only those projects.
create table sbctl.org_members (
  org_id     bigint      not null references sbctl.organizations (id) on delete cascade,
  user_id    uuid        not null,
  role_id    smallint    check (role_id between 1 and 4),
  created_at timestamptz not null default now(),
  primary key (org_id, user_id)
);
create index org_members_user_idx on sbctl.org_members (user_id);
-- The last-owner check counts the owners of one organization.
create index org_members_owner_idx on sbctl.org_members (org_id) where role_id = 1;

-- A project-scoped role: one base role held by one member on a set of projects. Ids start at
-- 1000 so they never collide with the four organization roles; Studio lists them next to
-- those in GET .../roles and refers to them in a member's role_ids.
create table sbctl.org_project_roles (
  id           bigint      generated always as identity (start with 1000) primary key,
  org_id       bigint      not null,
  user_id      uuid        not null,
  base_role_id smallint    not null check (base_role_id between 1 and 4),
  created_at   timestamptz not null default now(),
  unique (org_id, user_id, base_role_id),
  foreign key (org_id, user_id) references sbctl.org_members (org_id, user_id) on delete cascade
);
create index org_project_roles_user_idx on sbctl.org_project_roles (user_id);
create index org_project_roles_base_idx on sbctl.org_project_roles (org_id, base_role_id);

create table sbctl.org_project_role_refs (
  role_id bigint not null references sbctl.org_project_roles (id) on delete cascade,
  ref     text   not null references sbctl.projects (ref) on delete cascade,
  primary key (role_id, ref)
);
create index org_project_role_refs_ref_idx on sbctl.org_project_role_refs (ref);

-- Pending and accepted invitations. Only the SHA-256 of the token is stored; a token works
-- once (accepted_at) and expires (expires_at). At most one pending invitation per address
-- and organization: inviting again replaces it.
create table sbctl.org_invitations (
  id           bigint      generated always as identity primary key,
  org_id       bigint      not null references sbctl.organizations (id) on delete cascade,
  email        text        not null check (email = lower(email)),
  role_id      smallint    not null check (role_id between 1 and 4),
  -- Non-empty: the role is scoped to these projects (refs that no longer exist are ignored
  -- when the invitation is accepted).
  project_refs text[]      not null default '{}',
  token_hash   bytea       not null unique,
  invited_by   uuid,
  created_at   timestamptz not null default now(),
  expires_at   timestamptz not null,
  accepted_at  timestamptz,
  accepted_by  uuid
);
create unique index org_invitations_pending_idx on sbctl.org_invitations (org_id, email) where accepted_at is null;
create index org_invitations_email_idx on sbctl.org_invitations (email) where accepted_at is null;

-- "Require MFA" switch of an organization (Studio's team settings). Stored and reported;
-- the API enforces it on dashboard sessions that do not carry aal2.
create table sbctl.org_mfa (
  org_id     bigint      primary key references sbctl.organizations (id) on delete cascade,
  enforced   boolean     not null default false,
  updated_at timestamptz not null default now()
);

-- Default membership for a user who signs in through SSO for the first time (workstream L):
-- the organization and role granted by the email domain.
create table sbctl.sso_default_roles (
  domain     text        primary key check (domain = lower(domain)),
  org_id     bigint      not null references sbctl.organizations (id) on delete cascade,
  role_id    smallint    not null check (role_id between 1 and 4),
  created_at timestamptz not null default now()
);
create index sso_default_roles_org_idx on sbctl.sso_default_roles (org_id);

-- Accounts that existed before roles: they keep full access. "member_meta.legacy_cutoff" is
-- the time this migration ran; a dashboard account created before it and not yet checked
-- (member_legacy_checked) becomes Owner of every organization on its first request.
create table sbctl.member_meta (
  key text        primary key,
  at  timestamptz not null
);
create table sbctl.member_legacy_checked (
  user_id    uuid        primary key,
  checked_at timestamptz not null default now()
);

insert into sbctl.member_meta (key, at) values ('legacy_cutoff', now());
-- Every user the API has seen so far is Owner of every existing organization.
insert into sbctl.org_members (org_id, user_id, role_id)
  select o.id, u.user_id, 1 from sbctl.organizations o cross join sbctl.api_users u
  on conflict do nothing;
insert into sbctl.member_legacy_checked (user_id) select user_id from sbctl.api_users on conflict do nothing;

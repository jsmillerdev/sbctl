-- Branching (workstream I). A branch is a project with a parent: it has its own ref, keys,
-- units, host and fleet tenants. These columns are null on ordinary projects.

alter table sbctl.projects
  add column branch_id             uuid,
  add column parent_ref            text,
  add column branch_name           text,
  add column git_branch            text,
  add column persistent            boolean not null default false,
  add column with_data             boolean not null default false,
  add column expires_at            timestamptz,
  add column deletion_scheduled_at timestamptz,
  add column notify_url            text,
  add column branch_state          text,
  add column branch_detail         text,
  add column clone_method          text,
  add column review_requested_at   timestamptz;

-- A parent cannot be removed while it has branches: the row would go but not the units.
alter table sbctl.projects
  add constraint projects_parent_ref_fkey foreign key (parent_ref) references sbctl.projects (ref) on delete restrict,
  add constraint projects_branch_shape check (
    (parent_ref is null) = (branch_name is null) and (parent_ref is null) = (branch_id is null) and parent_ref is distinct from ref),
  add constraint projects_branch_state_check check (
    branch_state is null or branch_state in ('CREATING_PROJECT', 'RUNNING_MIGRATIONS', 'MIGRATIONS_PASSED', 'MIGRATIONS_FAILED', 'FUNCTIONS_DEPLOYED', 'FUNCTIONS_FAILED'));

create unique index projects_branch_id_idx on sbctl.projects (branch_id) where branch_id is not null;
create unique index projects_parent_branch_name_idx on sbctl.projects (parent_ref, branch_name) where parent_ref is not null;
create index projects_parent_ref_idx on sbctl.projects (parent_ref) where parent_ref is not null;
-- The sweeper looks for lapsed expiries; most projects have none.
create index projects_branch_expiry_idx on sbctl.projects (least(expires_at, deletion_scheduled_at))
  where expires_at is not null or deletion_scheduled_at is not null;

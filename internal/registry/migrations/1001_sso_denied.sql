-- Addresses an administrator refused at a single sign-on provider (workstream L, range 1000-1099).
--
-- Denying a waiting user, or removing an SSO account, deletes the GoTrue account. The next sign-in
-- through the identity provider creates a new account with a new user id, so the decision has to
-- be remembered by what the person keeps: the provider and the email address (lower case). While a
-- row is here, the provider's default role is not given to that address (the person waits for
-- approval like anyone else); approving the person, or `supavise sso allow`, deletes the row.
create table supavise.sso_denied (
  provider_id uuid        not null references supavise.sso_providers (id) on delete cascade,
  email       text        not null,
  denied_at   timestamptz not null default now(),
  primary key (provider_id, email)
);

-- Dashboard users that `supavise users remove` deleted (internal/api/claim.go). Range 0600-0699
-- belongs to the installer and account management.
--
-- A removed user's GoTrue access token stays valid until it expires (an hour), and a personal
-- access token is not tied to its owner's account, so deleting the account alone does not end
-- access. The API refuses every session and every token whose user is in this table, on every
-- request, so the removal takes effect at once. user_id is text: it is the JWT "sub" claim.
create table supavise.removed_users (
  user_id    text primary key,
  email      text,
  removed_at timestamptz not null default now()
);

-- Dashboard onboarding (internal/api/claim.go). Range 0600-0699 belongs to the installer.
--
-- A claim token creates the first dashboard administrator; an invite token creates one more
-- user for a fixed email address. Only the SHA-256 of a token is stored, a token works once
-- (used_at) and expires (expires_at). Types follow the supabase-postgres-best-practices
-- skill: text, timestamptz, identity keys, an index on every lookup column.
create table supavise.claim_tokens (
  id         bigint generated always as identity primary key,
  kind       text not null check (kind in ('claim', 'invite')),
  token_hash bytea not null unique,
  -- The address an invite is for; null for a claim token (the first admin picks it).
  email      text,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at    timestamptz,
  -- Email of the user the token created.
  used_by    text,
  check ((kind = 'invite') = (email is not null))
);
-- Issuing a token revokes the unused ones of its kind (and address); redeeming looks the
-- hash up through the unique index.
create index claim_tokens_unused_idx on supavise.claim_tokens (kind, email) where used_at is null;

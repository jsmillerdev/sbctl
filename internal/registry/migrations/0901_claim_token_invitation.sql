-- An invite claim token (sbi_..., internal/api/claim.go) creates the dashboard account of an
-- address that was invited to one organization. It is bound to that invitation: redeeming it
-- accepts that invitation and no other, and it dies with the invitation (replaced, revoked or
-- deleted with its organization). Range 0900-0999 belongs to the roles workstream.
--
-- Tokens issued before this column existed stay unbound: they create the account and accept
-- nothing.
alter table sbctl.claim_tokens
  add column invitation_id bigint references sbctl.org_invitations (id) on delete cascade;
create index claim_tokens_invitation_idx on sbctl.claim_tokens (invitation_id) where invitation_id is not null;

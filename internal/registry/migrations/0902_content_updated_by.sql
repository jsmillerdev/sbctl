-- Saved items (SQL snippets, reports) record who edited them last, apart from who owns them: a
-- member with the right to change another member's shared item leaves their own id here and
-- keeps the owner. Range 0900-0999 belongs to the roles workstream.
alter table sbctl.api_content
  add column updated_by bigint references sbctl.api_users (id) on delete set null;

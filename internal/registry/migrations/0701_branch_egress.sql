-- Branching: the outbound network policy of a branch cloned from its parent's data
-- (registry.BranchInfo.Egress: pending, denied, allowed or unenforced). Null on projects
-- that are not branches and on schema-only branches.
alter table sbctl.projects add column branch_egress text;

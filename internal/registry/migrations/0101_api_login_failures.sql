-- Wrong verification codes of a `supabase login` session are counted in place
-- (one atomic update) instead of taking the row out and putting it back.
alter table supavise.api_cli_login_sessions add column failures integer not null default 0;

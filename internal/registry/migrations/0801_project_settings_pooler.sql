-- The Supavisor tenant's pool settings (default_pool_size, max_client_conn) are saved per
-- project like the other services' settings (0800), under the service name "pooler".
alter table supavise.project_settings drop constraint project_settings_service_check;
alter table supavise.project_settings add constraint project_settings_service_check
  check (service in ('auth', 'postgrest', 'realtime', 'storage', 'postgres', 'pooler'));

-- Per-project compute sizes (internal/lifecycle, sizes). Range 1250-1279 belongs to the
-- compute-sizes workstream.
--
-- projects.class now holds a hosted compute size: nano, micro, small, medium, large, xlarge,
-- 2xlarge ... 16xlarge. The classes of earlier versions map onto them by what they were:
--
--   micro   (16 MB shared_buffers, 30 connections)  -> nano
--   default (32 MB, 60 connections, 1 GB cap)       -> micro   (the default size, as on hosted)
--   small, medium, large                            -> the same names
--
-- A row whose limits are still the node default of the time ({} or 1G and 100%) takes the
-- limits of its new size, so the size it shows and the cap it runs under agree. Limits someone
-- set by hand stay. The new MemoryMax and CPUQuota reach the units the next time the project
-- starts; its Postgres settings change then too. The system project keeps its own class.
--
-- The limits below are the sizes' MemoryMax and CPUQuota (lifecycle.Classes; a test in
-- internal/lifecycle, TestMigrationLimitsMatchTheSizeTable, pins them to the Go table).

update supavise.projects set
  limits = case
    when limits = '{}'::jsonb or limits = '{"memory_max": "1G", "cpu_quota": "100%"}'::jsonb then
      case class
        when 'micro'   then '{"memory_max": "512M", "cpu_quota": "100%"}'::jsonb
        when 'default' then '{"memory_max": "1G", "cpu_quota": "100%"}'::jsonb
        when 'small'   then '{"memory_max": "2G", "cpu_quota": "100%"}'::jsonb
        when 'medium'  then '{"memory_max": "4G", "cpu_quota": "200%"}'::jsonb
        when 'large'   then '{"memory_max": "8G", "cpu_quota": "200%"}'::jsonb
      end
    else limits
  end,
  class = case class
    when 'micro' then 'nano'
    when 'default' then 'micro'
    else class
  end
where class in ('micro', 'default', 'small', 'medium', 'large');

alter table supavise.projects alter column class set default 'micro';

# 09 - Footprint: method for the Linux measurement

Status: method written 2026-10-06; **no Linux numbers yet**. `DESIGN.md` section 12 lists
"Linux per-project RSS and cold start with the Supabase preload set at 10, 25 and 50
projects" as open. `tests/linux/footprint.sh` produces them on an ephemeral Ubuntu 24.04 VM
in CI; this file says what is measured and why, and holds the table once a run exists.

## What is measured

One node, one `sbctl` binary, real artifacts, the systemd backend, `micro` or `default`
project class (the class is part of the result: `micro` is 16 MB `shared_buffers` and 30
connections, `default` is 32 MB and 60). Each project runs `sb-postgres@`, `sb-gotrue@` and
`sb-postgrest@`. The system project (`sb-postgres@system`, `sb-gotrue@system`) is measured
separately because it exists once. The shared fleet (Supavisor, Realtime, Storage, pgmeta,
Studio) is not started by this script; it belongs to the fleet workstream and is added as
a fixed cost when it exists.

For each size N in 10, 25, 50 the script creates projects until N exist, waits 30 s for
idle, then records:

| Column | How |
|---|---|
| create avg (s) | wall time of `sbctl projects create` (initdb, the artifact's roles and migrations, role passwords, GoTrue migration, health checks), averaged over the projects added for this size |
| PSS per project, median (MB) | for each project, sum `Pss` from `/proc/<pid>/smaps_rollup` over every process in the cgroups of its three units (`/sys/fs/cgroup/sbctl.slice/<unit>/cgroup.procs`); median over projects |
| RSS per project, median (MB) | the same with `Rss` |
| system project PSS (MB) | the same for `sb-postgres@system` and `sb-gotrue@system` |
| sbctl.slice memory.current (MB) | cgroup v2 `memory.current` of the slice: everything sbctl runs, including page cache |
| disk per project (MB) | `du -sk` of one project directory (cluster, WAL, env files) |
| resume avg (s) | five projects are paused, then resumed one at a time through `sbctl projects resume`; wall time includes the Postgres start, GoTrue migration and the health checks, so it is the time to a usable project |
| node cold start (s) | all `sb-*` units stopped, then `sbctl system start` (system project first, then every active project one by one, each waited until healthy) |

### Why PSS

A Postgres cluster maps `shared_buffers` and the loaded libraries in every backend, so
summed RSS counts the same pages once per process and overstates a cluster several times
over. PSS divides each shared page among the processes that map it, so the sum over a unit
is what the unit costs the machine. PSS is the number to size from; RSS is kept because the
design documents quote it elsewhere. `memory.current` is reported for the whole slice only,
since it includes reclaimable page cache and would make a small project look large.

### What is not in the numbers

- Query load. Every project is idle apart from the health checks. An active project's
  working memory is `shared_buffers` plus per-connection memory (about 5 to 10 MB per
  active backend) plus page cache, which is the operator's to size through the class.
- Realtime, Supavisor, Storage, pgmeta, Studio, the daemon itself.
- Extensions loaded on demand. The preload set is the artifact's own
  (`pg_stat_statements, pgaudit, plpgsql, plpgsql_check, pg_cron, pg_net, pgsodium,
  auto_explain, pg_tle, plan_filter, supabase_vault` with `supautils` as a session preload),
  which every project pays at start.

## How to run

```
sudo SBCTL_BIN=./bin/sbctl-linux-amd64 tests/linux/footprint.sh --sizes "10 25 50" --class default --teardown
```

State the VM type with the result (vCPUs, RAM, disk type, kernel). Run it once per class
that matters (`micro` and `default`) and once per architecture (amd64, arm64). Results
belong in the table below with the date, the artifact tags from `versions.yaml` and the
sbctl commit. The VM needs roughly 4 GB of disk for 50 projects (about 60 MB each after
the migrations; the first CI run should confirm that number).

## Results

| Run | Host | Class | Projects | create avg (s) | PSS/project (MB) | RSS/project (MB) | system PSS (MB) | slice (MB) | disk/project (MB) | resume avg (s) | node cold start (s) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| [37573996289](https://github.com/jsmillerdev/sbctl/actions/runs/37573996289) | GitHub ubuntu-24.04, x86_64, 4 vCPU, 16 GB | default | 10 | 2.59 | 70.3 | 230.7 | 58.2 | 1547.5 | 56.5 | 0.50 | 5.01 |
| same | same | default | 25 | 2.48 | 66.5 | 230.5 | 55.9 | 3784.1 | 72.7 | 0.51 | 12.06 |
| same | same | default | 50 | 2.48 | 65.2 | 230.3 | 55.0 | 7451.2 | 72.7 | 0.50 | 23.70 |
| same | GitHub ubuntu-24.04-arm, aarch64, 4 vCPU, 16 GB | default | 10 | 2.69 | 68.4 | 236.4 | 56.6 | 1519.2 | 56.4 | 0.49 | 4.99 |
| same | same | default | 25 | 2.31 | 64.7 | 236.2 | 54.4 | 3685.9 | 72.7 | 0.49 | 11.98 |
| same | same | default | 50 | 2.26 | 63.1 | 235.9 | 53.7 | 7254.2 | 72.7 | 0.50 | 23.55 |

Measured 2026-10-07 at commit 83815eb, Ubuntu 24.04.5, kernel 6.17, under real systemd. The shared services (Supavisor, Realtime, Storage, postgres-meta, Studio) were not running in this run; their manifest idle RSS adds roughly 1 GB once per node, independent of project count.

Reading: a project's own memory (PSS) is about 65 MB idle; RSS counts shared pages once per process and overstates it about 3.5x. The slice's memory.current grows by about 148 MB per project between 10 and 50 projects, because it also charges page cache; use 150 MB per idle project plus about 1.5 GB fixed when sizing an instance. Fifty idle projects used 7.4 GB of a 16 GB host.

## One local data point (not a Linux number)

A developer Mac (Apple M4, Darwin 25.5, exec backend, darwin-arm64 artifacts,
`default` class) running the system project plus one project:

- system project (Postgres 17.11 with `max_connections=100`, `shared_buffers=64MB`, plus
  GoTrue v2.195.0): about 65 MB summed `ps` RSS.
- adding one project (Postgres 32 MB `shared_buffers`, GoTrue, PostgREST): the total went to
  about 124 MB, so about 59 MB summed RSS for the project.
- `sbctl projects create` returned a healthy project in about 6 s, `pause` in 0.2 s, `resume`
  in about 2 s, `system init` on an empty state directory in about 14 s.

macOS `ps` RSS counts shared pages per process and says nothing about Linux cgroup
accounting, so these figures only show the order of magnitude and that the default class is
small enough for a laptop. They are not evidence for the 10, 25 and 50 project claims in
`DESIGN.md`.

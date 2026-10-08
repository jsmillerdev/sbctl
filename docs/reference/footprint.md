# Footprint

Memory, cold start and disk use of a node at 10, 25 and 50 projects, under real systemd on Ubuntu 24.04.

## What is measured

One node, one `supavise` binary, real artifacts, the systemd backend and one project class (the class is part of the result). Each project runs `supavise-postgres@`, `supavise-gotrue@` and `supavise-postgrest@`. The system project (`supavise-postgres@system`, `supavise-gotrue@system`) is measured separately because it exists once. The shared services (Supavisor, Realtime, Storage, postgres-meta, Studio) are not started by the script; add their idle RSS from the artifact manifests (not measured here), roughly 1 GB once per node, as a fixed cost.

For each size N in 10, 25, 50 the script creates projects until N exist, waits 30 s for idle, then records:

| Column | How |
|---|---|
| create avg (s) | wall time of `supavise projects create` (initdb, roles and migrations, role passwords, GoTrue migration, health checks), averaged over the projects added for this size |
| PSS per project, median (MB) | per project, the sum of `Pss` from `/proc/<pid>/smaps_rollup` over every process in the cgroups of its three units; median over projects |
| RSS per project, median (MB) | the same with `Rss` |
| system project PSS (MB) | the same for the two system units |
| supavise.slice memory.current (MB) | cgroup v2 `memory.current` of the slice: everything Supavise runs, including page cache |
| disk per project (MB) | `du -sk` of one project directory (cluster, WAL, env files) |
| resume avg (s) | five projects are paused, then resumed one at a time with `supavise projects resume`; includes the Postgres start, GoTrue migration and health checks |
| node cold start (s) | all `supavise-*` units stopped, then `supavise system start` (system project first, then each active project, each waited until healthy) |

### Why PSS

A Postgres cluster maps `shared_buffers` and the loaded libraries in every backend, so summed RSS counts the same pages once per process and overstates a cluster several times over. PSS divides each shared page among the processes that map it, so the sum over a unit is what the unit costs the machine. Size from PSS; RSS is kept for comparison. `memory.current` is reported for the whole slice only, because it includes reclaimable page cache and would make a small project look large.

### Not in the numbers

- Query load. Every project is idle apart from health checks. An active project's working memory is `shared_buffers` plus per-connection memory (about 5 to 10 MB per active backend) plus page cache.
- Realtime, Supavisor, Storage, postgres-meta, Studio and the daemon itself.
- Extensions loaded on demand. The preload set is the artifact's own (`pg_stat_statements, pgaudit, plpgsql, plpgsql_check, pg_cron, pg_net, pgsodium, auto_explain, pg_tle, plan_filter, supabase_vault`, with `supautils` as a session preload), which every project pays at start.

## How to run

Run the manual `footprint` workflow (inputs `sizes`, default `10 25 50`, and `class`, default `default`), which runs the script on an amd64 and an arm64 runner. To run it on a VM:

```
sudo SUPAVISE_BIN=./bin/supavise-linux-amd64 tests/linux/footprint.sh --sizes "10 25 50" --class default --teardown
```

State the VM type with the result. Record new results in the table below with the date, the artifact tags from `internal/versions/versions.yaml` and the Supavise commit. The VM needs roughly 4 GB of disk for 50 projects.

## Results

The runs used class `default` as it was before compute sizes: 32 MB `shared_buffers` and 60 connections. Today a project has a hosted-style size (`internal/lifecycle/README.md`, "Compute sizes"), and `--class default` means Micro (256 MB `shared_buffers`, 60 connections, a 1 GB cap). Idle memory is what the cluster touches, not what `shared_buffers` reserves, so the numbers should hold, but no run at the current sizes is recorded.

| Run | Host | Projects | create avg (s) | PSS/project (MB) | RSS/project (MB) | system PSS (MB) | slice (MB) | disk/project (MB) | resume avg (s) | node cold start (s) |
|---|---|---|---|---|---|---|---|---|---|---|
| [37573996289](https://github.com/supavise/supavise/actions/runs/37573996289) | GitHub ubuntu-24.04, x86_64, 4 vCPU, 16 GB | 10 | 2.59 | 70.3 | 230.7 | 58.2 | 1547.5 | 56.5 | 0.50 | 5.01 |
| same | same | 25 | 2.48 | 66.5 | 230.5 | 55.9 | 3784.1 | 72.7 | 0.51 | 12.06 |
| same | same | 50 | 2.48 | 65.2 | 230.3 | 55.0 | 7451.2 | 72.7 | 0.50 | 23.70 |
| same | GitHub ubuntu-24.04-arm, aarch64, 4 vCPU, 16 GB | 10 | 2.69 | 68.4 | 236.4 | 56.6 | 1519.2 | 56.4 | 0.49 | 4.99 |
| same | same | 25 | 2.31 | 64.7 | 236.2 | 54.4 | 3685.9 | 72.7 | 0.49 | 11.98 |
| same | same | 50 | 2.26 | 63.1 | 235.9 | 53.7 | 7254.2 | 72.7 | 0.50 | 23.55 |

Measured 2026-10-07 at commit 83815eb, Ubuntu 24.04.5, kernel 6.17. The shared services were not running.

A project's own memory (PSS) is about 65 MB idle; RSS counts shared pages once per process and overstates it about 3.5 times. The slice's `memory.current` grows by about 148 MB per project between 10 and 50 projects because it also charges page cache. Size an instance at about 150 MB per idle project plus about 1.5 GB fixed. Fifty idle projects used 7.4 GB of a 16 GB host.

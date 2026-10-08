# Read replicas, failover and the second server

What a Supavise cluster is, what its names and ports are, what the dashboard shows, what each failure does and how much data a failure can cost. The task guides are the [guide](../guide.md#read-replicas-and-failover) and the [deploy guide](../../deploy/README.md#add-a-replica-server). The model and its invariants are in [design.md](../design.md#12-clusters-replicas-and-failover).

Each number on this page carries one of three labels:

- **Measured**: from a recorded run. The source is named.
- **Setting**: a default that a config key changes.
- **Designed**: a bound the design sets. No recorded run measures it.

## The parts

| Term | Meaning |
|---|---|
| node | One Supavise server. It has an id (`n1`, `n2`, ...) and a name. |
| cluster | The nodes that share one registry. A server that never joined another is a cluster of one and behaves as it did before. |
| leader | The node whose system Postgres cluster is a primary. It runs the writable registry, the Management API, Studio and the shared services (Realtime, Storage, postgres-meta, Edge Runtime). |
| follower | Every other node. Its system cluster is a hot standby of the leader's. |
| home | The node that runs a project's primary Postgres, Auth and REST. New projects are created on the leader. |
| replica | A standby Postgres of one project, with its own PostgREST, on a node that is not the project's home. |
| epoch | A number that grows at each promotion of the system cluster. A node that sees a higher epoch than its own stops leading. |

A follower runs Supavisor against the replicated pooler database, and the `supavise` daemon with its proxy, mesh port and agents. Realtime, Storage, postgres-meta, Edge Runtime and Studio are installed there and parked: their ports on a follower forward to the leader. A project homed on a follower runs its own Postgres, Auth and REST there.

## Names and ports

| Thing | Name or port |
|---|---|
| Replica identifier | `<ref>-rr-<region>-<id6>`: the project ref, `rr`, the region of the replica's node and six random characters. Studio's source selector needs the `-rr-`. |
| Replica API | `https://<identifier>.api.<domain>/rest/v1`. Only `/rest/v1` and `/graphql/v1` answer; any other path is `404`. |
| Load balancer | `https://<ref>-lb.api.<domain>`. It exists while the project has a replica. |
| Pooler of a replica | the replica's node's public host, ports 6543 (transaction) and 5432 (session), user `postgres.<identifier>` |
| Direct host | `db.<identifier>.api.<domain>`. A `*.api.<domain>` wildcard record does not cover it (two labels), so the pooler strings are the working path. The primary's `db.<ref>.api.<domain>` has the same gap. |
| Vanity names | A name that contains `-rr-` or ends in `-lb` cannot be a vanity subdomain, so no project can take another's replica or balancer host. |
| Mesh port | TCP 7443 between nodes, mutual TLS 1.3 (`[node] peer_listen`) |
| Replica ports | Postgres `replica_base + 3n`, PostgREST `replica_base + 3n + 2`, where `n` is the project's sequence number and `replica_base` is 10000 by default. The system cluster's standby uses `replica_base` itself. The range must end below `[ports] project_base` (20000). |
| Project ports | Postgres `project_base + 3n`, Auth `+1`, PostgREST `+2`. On every node each of these is the service itself (home) or a forwarder to the home, so the proxy, Supavisor, Realtime, Storage, Functions and the backups always dial `127.0.0.1:<port>`. |

Any node answers any project, replica or balancer host and forwards when it does not run the target. DNS decides which node a client reaches. Point `<identifier>.api.<domain>` at the replica's server to keep traffic local; until you do, a replica answers through the leader at the cost of one cross-server hop. `supavise node ls --dns` prints the records the cluster still needs.

## What the dashboard shows

The pinned Studio runs unpatched. Its Infrastructure page lists the primary, each replica with the seven setup steps, and an "API Load Balancer" node once a replica exists. The SQL editor's source selector, Reports, Connect and API settings list each replica with its own URLs, pooler strings and lag. [Studio platform calls](studio-platform-calls.md#read-replicas) lists the calls behind them.

The **Add read replica** button works when all of these hold:

- A second node is active. Until then the Studio profile keeps `infrastructure:read_replicas` in `disabled_features`, which hides the Infrastructure page and the replica rows.
- The project's compute size is Small or larger. Studio disables the button below that, and the Management API refuses with "Read replicas need a compute size of small or larger." `supavise replicas add` applies the same checks. `[replicas] default = "all"` does not apply the size and count limits.
- A node is joined in the region you pick, and that node is not the project's own server.
- The project has fewer replicas than its size allows: none up to Micro, four for Small to Large, five above, and never more than the other active nodes.
- The backup backend is S3 compatible (`s3://`), not a local directory.
- The project is not a branch.

Studio also needs Postgres 15 or later (the project reports `supabase-postgres-<version>`), a backup service on the node and a project that does not report high availability. Supavise's answers meet those rules.

Studio refuses the point-in-time restore of a project that has a replica, and tells the user to remove the replicas first. The Management API removes a project's replicas itself before it restores the project in place or deletes the project or its organization, and answers `409` while one cannot be removed. Replicas that `[replicas] default = "all"` makes come back when the project runs. `supavise projects delete` and `supavise backups restore` on the server do not do this: run `supavise replicas rm` for each replica first.

## Setup, status and lag

`POST /v1/projects/{ref}/read-replicas/setup`, the dashboard and `supavise replicas add` write one row. The replica controller on the leader does the rest in the background, and a restart of the daemon resumes from the step each row holds.

| Step | Work | Failure code |
|---|---|---|
| `0_requested` | The row exists. The controller admits it when a setup slot is free (`[replicas] concurrency`) and the node has room. | none; it waits |
| `1_started` | The newest base backup is used, or a new one is taken when it is older than `bootstrap_max_backup_age`. The node creates its directories and units. | `1_read_replica_instance_launch_failed` |
| `2_launched_read_replica_instance` | The node starts to seed. | `2_initiate_read_replica_setup_failed` |
| `3_initiated_read_replica_setup` | The node downloads and extracts the base backup from the backup store with its own credentials. | `3_download_base_backup_failed` |
| `4_downloaded_base_backup` | The standby replays the WAL archive, then streams from the primary. | `4_replay_wal_archives_failed` |
| `5_replayed_wal_archives` | The replica's PostgREST starts and the replica gets its pooler tenant. | `5_complete_read_replica_setup_failed` |
| `6_completed_read_replica_setup` | Done. | |

A failed step leaves `INIT_READ_REPLICA_FAILED`, raises `replica_unhealthy` and stops. Studio tells the user to remove the replica and add it again. A call that fails is retried for 10 minutes (after 10 seconds, doubling to 2 minutes) before the step fails. A node with no room answers before it creates anything; the row goes back to `0_requested`, the node's `replica_capacity` alert opens, and the controller asks again every minute.

| Status | Meaning |
|---|---|
| `ACTIVE_HEALTHY` | Postgres and PostgREST run, the receiver streams, and lag is at most `unhealthy_lag_seconds`. |
| `ACTIVE_UNHEALTHY` | The receiver has not streamed for 2 minutes, PostgREST or Postgres is down, lag is over the limit, or the node has been silent for 2 minutes. |
| `RESTARTING`, `RESIZING` | Until the next observation shows the replica well, or 10 minutes. |
| `GOING_DOWN` | Removal in progress. |
| `INIT_READ_REPLICA`, `INIT_READ_REPLICA_FAILED` | Setup running, or failed. |

Lag is `0` when the replica has replayed everything it received and the receiver streams, and otherwise the time since the last replayed transaction, as Studio computes it. The agent on each replica node reports every 10 seconds. The leader keeps a one-minute series for 24 hours per replica in memory, which Studio's replica page reads through `infra-monitoring`. A restart of the leader's daemon empties it. `supavise replicas ls` reads lag from `<state_dir>/replicas-status.json`, a snapshot of the controller's last pass (mode 0600, no LSNs and no secrets), so it shows no lag on a follower or while the daemon is down.

The replica's PostgREST reloads its schema cache when the primary notifies it and every `schema_reload_seconds` (**Setting**, 10), because a notification can arrive before the standby has replayed the change. Spike S3 (**Measured**) found that the notification alone missed some changes under heavy WAL: [design.md](../design.md#128-spike-outcomes) has the outcomes of the spikes.

## Replica API, load balancer and pooler

**Replica API.** It uses the project's keys, CORS settings and legacy-key guard. Every method reaches PostgREST, which refuses writes on the standby. A replica in `INIT_READ_REPLICA`, `INIT_READ_REPLICA_FAILED` or `GOING_DOWN` answers `503`.

**Load balancer.** On `<ref>-lb.api.<domain>`, `GET` and `HEAD` on `/rest/v1/*` go to the primary or to a replica. Everything else (other methods, `/graphql/v1`, Auth, Storage, Realtime, Functions) goes to the primary.

- The candidates are the primary and each `ACTIVE_HEALTHY` replica whose node has a live session. With `[replicas] lb_max_lag_seconds` above zero, a replica over the limit or with no lag reading is left out.
- The pick is the database on the node that received the request. Otherwise it is the node with the lowest round-trip time in the mesh heartbeats, otherwise the candidates in turn. A balancer name that DNS resolves to the primary's node therefore answers from the primary. Use latency-routed records that point at each node to spread reads.
- Every answer carries `X-Supavise-Route: <identifier>` (the ref for the primary), and the access log gets `load_balancer_redirect_identifier`.
- A read whose chosen replica refuses the connection or breaks before it answers is repeated once on the primary. A request with a body is not repeated and answers `502`.
- Custom and vanity hostnames always go to the primary.
- Lag readings live on the leader. On a follower, a nonzero `lb_max_lag_seconds` leaves every replica out and reads go to the primary.

**Pooler.** `postgres.<identifier>` on the replica's node logs in to the standby. The Supavisor tenant is a row of the replicated pooler database, so each node's Supavisor serves it.

## Roles and restarts

A role change restarts the daemon. When a node's system cluster is promoted or demoted, the `supavise` daemon stops and systemd starts it in the role the cluster has, so a switchover shows one short restart at each node. A node that is fenced answers HTTP `503` on every host with a message that says it is fenced, and starts no primary.

A follower's registry is read-only. The follower serves the proxy, the pooler and the replicas from its copy and forwards `api.<domain>` to the leader's Management API. The CLI commands that write the registry (`replicas add`, `node token`, `projects failover`) say to run on the leader.

## Failover

Both moves exist for one project and for the whole server. Run each with `--dry-run` first: it prints every precondition and what would happen.

| | Switchover (planned) | Failover (unplanned) |
|---|---|---|
| The old primary | alive, stops cleanly | dead or silent, fenced first |
| Data lost | none | what the replica had not received |
| Project | `supavise projects failover <ref> [--to NODE]` on the leader | the same command, which asks you to type the ref first |
| Server | `supavise failover [--to NODE]` on the node that takes over (or on the leader with `--to`) | `supavise failover` on a survivor |
| The old primary afterwards | becomes a replica in place | its data is set aside as `data.diverged-<epoch>`, and `supavise node rejoin` rebuilds it |

A project move needs a replica of that project. A server move needs one for every project, or `--restore-missing`, which seeds a standby from the WAL archive and loses up to `archive_timeout` of that project's writes. A server move also needs Storage on S3 (`[fleet] storage_backend = "s3"`; see `supavise storage migrate`). A project move does not.

The server move writes the new leader and epoch to `_node/leader.json` in the backup store, takes the service address (an Elastic IP on AWS, the operator's command elsewhere), promotes the system cluster, and then promotes each project's replica, four at a time. Nothing is promoted until the old leader cannot write. The daemon restarts once when the system cluster is promoted, and the daemon that starts continues the move. The CLI waits for it and goes on printing steps. A move that stopped is continued with `--resume`. A failed server move that has gone no further than stopping the leader is discarded with `--abort`.

**Fencing.** `[failover] fencing` chooses how the old primary is kept from writing:

- `aws`: the survivor stops the peer's EC2 instance, waits for `stopped`, and associates the service address's Elastic IP with itself. A peer that EC2 does not list is not taken for stopped. A survivor whose only private address carries an Elastic IP of its own is not touched: the move prints the DNS change instead.
- `command`: `fence_command` runs on the survivor and must exit 0 before any promotion. `takeover_command` then moves the address. Both get `OLD_NODE`, `NEW_NODE`, `OLD_NODE_ID`, `NEW_NODE_ID`, `OLD_NODE_ADDR`, `NEW_NODE_ADDR`, `EPOCH` and `PLANNED` in a short environment (`PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`, `TMPDIR`, `AWS_REGION`, `AWS_DEFAULT_REGION`), never the daemon's whole one.
- empty: nothing is fenced. A failover needs `--old-primary-is-down`, which is your statement that the old leader cannot write. The survivor still tries the cooperative fence first.

**Automatic modes.** `[failover] mode = "project"` lets the leader fail over one project whose primary stays `ACTIVE_UNHEALTHY` for `project_grace_seconds`. `"server"` also lets a follower take over a dead leader. Both need `fencing = "aws"` and an S3 backup store. The server mode acts only when the leader fails pings for `grace_seconds`, `GET /healthz` at the service address fails, and EC2 reports the leader stopped, terminated or failing its status checks. A leader that only misses pings is never stopped. A node whose fencing probe fails runs as manual and raises `failover_auto_off`. No automatic move runs while any of these is true: a planned move announced maintenance, a failover ran within `cooldown_minutes` (for one project in project mode), the two nodes run different releases or sit in different regions, or a replica's lag is unknown or over `max_lag_seconds`. There is no automatic failback.

**Failback.** A switchover moves the work back. After a planned move the old primary already follows as a replica, so run `supavise failover` (or `supavise projects failover <ref>`) on the node that should lead again. After an unplanned failover, run `supavise node rejoin` on the returned node first. It sets the old data aside for `keep_diverged_days` (3), rebuilds from the new leader's archive and keeps its identity.

**A node that returns.** At boot a node that led asks its peers for the epoch and reads `_node/leader.json`. A higher epoch, or another leader at the same epoch, fences it: no primary starts, the proxy answers `503`, `supavise status` shows FENCED and the critical alert `fenced` fires. A node that cannot reach either source starts only when its own record shows no demotion. A node that led and restarts before its copy of the registry has replayed a planned switchover reads itself as the replaced leader and is fenced; `supavise node rejoin` brings it back.

## Failure matrix

| Failure | Detection | Reaction | Operator step |
|---|---|---|---|
| Join with an expired or used token, wrong pin or wrong proof | the handshake | refused before any key is sent | create a new token |
| The joiner stops after its certificate was issued | the node stays `joining` | `node join --resume` continues; after an hour the leader removes the node and revokes its certificate | none, or a new token |
| Backup missing or store down during setup | a step call fails | retries for 10 minutes, then `INIT_READ_REPLICA_FAILED` with the step code | remove the replica and add it again |
| No room on the replica's node | admission on that node | the row waits at `0_requested`, `replica_capacity` opens, the controller asks each minute | free room or add a node |
| Streaming or mesh failure | the receiver status | the replica catches up from the WAL archive; `ACTIVE_UNHEALTHY` after 2 minutes; `replica_unhealthy` | none |
| Replica behind WAL that left the archive | the receiver cannot fetch a segment | none: the replica stays `ACTIVE_UNHEALTHY` and nothing rebuilds it | remove the replica and add it again |
| `pg_promote` run by hand on a replica | the WAL relay | a push without a matching `promote.ok` is refused with `412`, so nothing reaches the archive. The node also refuses to remove a cluster that has become a primary, so `supavise replicas rm` does not clear it. | clear the replica's data on that node by hand, then remove the replica and add it again |
| Replica's node goes silent | no report for 2 minutes | `ACTIVE_UNHEALTHY`; the balancer leaves it out | none |
| A replica stops answering between two status reports | a refused or broken connection | a balanced read repeats once on the primary; a read from the replica's own API answers `502` | none |
| Peer cannot be reached but runs | missed pings, public probe fine | manual: nothing. Automatic: no takeover, because EC2 shows the peer running | none |
| Leader dead | pings, probe and EC2 | server mode: fence, take the address, promote. Otherwise an operator runs `supavise failover` | run it, or wait |
| A project's home is a follower that is dead | the plan | no move covers it; the project stays down until the node returns and its replica holds the data | restore the node |
| Promotion fails midway | the move's log | the move ends `failed`, or `aborted` when nothing was asked of the node | `--resume`, or `--abort` for a server move that went no further |
| Two survivors act at once | the epoch marker | the one whose marker is not in the store stops before it touches the address | none |
| Old primary returns | boot epoch check | fenced; no primary starts | `supavise node rejoin` |
| A node was removed while it was down | its peers refuse it | it stays a stale follower | `supavise node join --reset` with a new token |
| Version skew | join, ping | the join is refused (same major, minors at most one apart); `node_version_skew` | upgrade the lagging node |
| `supavise upgrade` runs on a server | the release-match gate | automatic moves wait while the two nodes run different releases | upgrade the leader first, then each follower |

## RPO and RTO

| Case | Data lost (RPO) | Label |
|---|---|---|
| Planned switchover | none: the old primary stops cleanly and the replica replays to its final checkpoint before it is promoted | Designed, covered by tests that run real clusters for the replica steps; no recorded run of a full move |
| Unplanned, replica streaming | the replication lag at the failure. The lag alert opens at 60 seconds, and a move refuses a replica over `max_lag_seconds` without `--force`. | Designed; lag p99 was 0.10 to 0.41 s at 20 MB/s of WAL over 2 to 70 ms of round trip (**Measured** in spike S6) |
| Unplanned, stream broken | up to `archive_timeout` (**Setting**, `[backup] archive_timeout_seconds`, 300 s) plus the upload time | Designed |
| Project with no replica, `--restore-missing` | the same bound | Designed |
| Storage objects | none on S3. The file backend refuses a server move. | As built |
| Realtime events in flight | lost | Designed |

RTO is the time to promote and re-register. The design expects tens of seconds for the control plane and one to three minutes for tens of projects (**Designed**). No run records it. A move reports each step, so a run on your servers gives your own figure.

## Configuration

Cluster-scoped keys are the same on every node. The leader's values win: `supavise node join` writes them to `/etc/supavise/config.d/10-cluster.toml` (0600) and `supavise system converge` refreshes the file from the leader. Change them on the leader. They are `domain`, `tls.*`, `backup.*`, `fleet.storage_*`, `replicas.*` and `failover.*`. Everything else is node-local.

| Key | Default | Meaning |
|---|---|---|
| `[node] name` | the host name | the node's name |
| `[node] peer_listen` | `:7443` | where the mesh listens |
| `[node] peer_address` | public IP and port | the address other nodes dial; a hint, since a session works from whichever side can connect |
| `[node] region` | the top-level `region` | the node's region, one Studio knows |
| `[ports] replica_base` | 10000 | first replica port |
| `[replicas] default` | `off` | `all` gives every project a replica on every other node. Not for branches or paused projects. Needs S3 backups. |
| `[replicas] concurrency` | 2 | setups at once |
| `[replicas] bootstrap_max_backup_age` | `24h` | a base backup older than this is replaced before seeding |
| `[replicas] unhealthy_lag_seconds` | 300 | lag above this is `ACTIVE_UNHEALTHY` |
| `[replicas] lb_max_lag_seconds` | 0 | the balancer skips a replica over this lag; 0 ignores lag |
| `[replicas] schema_reload_seconds` | 10 | the replica's PostgREST schema reload interval |
| `[failover] mode` | `manual` | `manual`, `project` or `server` |
| `[failover] fencing` | empty | `aws`, `command` or empty |
| `[failover] fence_command`, `takeover_command` | empty | for `fencing = "command"` |
| `[failover] max_lag_seconds` | 30 | a replica lagging more is not promoted without `--force` |
| `[failover] grace_seconds` | 90 | leader silence before server mode acts |
| `[failover] project_grace_seconds` | 180 | primary unhealth before project mode acts |
| `[failover] stop_timeout_seconds` | 120 | the wait for the fence to stop the old instance |
| `[failover] cooldown_minutes` | 60 | no second automatic failover within this time |
| `[failover] keep_diverged_days` | 3 | how long set-aside data directories are kept |
| `[fleet] storage_s3_role_arn` | empty | an IAM role the daemon assumes for Storage. It excludes `storage_s3_access_key_id` and `storage_s3_secret_access_key`. |
| `[fleet] storage_credentials_port` | 4010 | the loopback port of the credential endpoint the daemon serves to Storage |
| `[aws] stack_name` | empty | the CloudFormation stack of this node; `supavise upgrade --aws` writes it to `config.d/20-aws.toml` |

A config check refuses `replicas.default = "all"` and any automatic failover mode when `[backup]` is a `file://` backend, and refuses an automatic mode without `fencing = "aws"`.

## Alerts

| Kind | When |
|---|---|
| `replica_unhealthy` | a replica's setup failed, its receiver is down for 2 minutes, its PostgREST does not answer, or lag is over `unhealthy_lag_seconds` |
| `replica_lag` | lag is over 60 seconds (or half the limit, if lower) and still within the limit |
| `replica_capacity` | a node has no room for a replica that is waiting |
| `node_unreachable`, `node_version_skew` | a peer does not answer the mesh, or runs a release outside the window |
| `failover_started`, `failover_completed`, `failover_failed` | a move begins, ends, or stops. They are sent when they happen and the hourly cap does not hold them back. |
| `failover_auto_off` | an automatic mode is set and this node cannot fence, so it runs as manual. The recovery message says it is armed again. |
| `fenced` | this node lost the leadership to a higher epoch and starts no primary (critical) |
| `standby_behind` | the registry is newer than this binary, which then leaves running instances alone |
| `infra_behind`, `host_not_converged` | the AWS stack or the host lacks what this release needs |

## Files

| File | Holds |
|---|---|
| `/etc/supavise/cluster/` | `node.key` (0600), `node.crt`, `ca.crt`, `join.json` while a join runs (0600), and `follower.json`, the mark of a server that joined a cluster that already existed (0600) |
| `/etc/supavise/config.d/` | `10-cluster.toml` (the leader's cluster keys), `20-aws.toml`, `30-storage-s3.toml` (0600) |
| `<state_dir>/fenced.json` | the record of being fenced or removed, with the peers' addresses |
| `<state_dir>/failover.json` | a server move on a node whose registry is a standby |
| `<state_dir>/failover-quiesce.json` | what a leader needs to undo or repeat a planned stop (0600) |
| `<state_dir>/system/failover/control.sock` | the CLI's way to the daemon (0600, in a 0700 directory) |
| `<state_dir>/replicas-status.json` | the controller's snapshot for `supavise replicas ls` |
| `<state_dir>/cluster-status.json` | the daemon's view of sessions and lag, for `supavise status` and `supavise node ls` |
| `<state_dir>/projects/<ref>/data.diverged-<epoch>` | the data a failover or rejoin set aside |
| `<backup>/_node/leader.json` | which node led at which epoch |
| `<backup>/_stack/<sha256>.yaml` | the template `supavise upgrade --aws` deploys |

## Limits

- A replica needs an S3-compatible backup backend, because the second server reads its first copy from it.
- A replica of a project is on a node other than the project's home. Two replicas of one project never share a node.
- `supavise projects delete` and `supavise backups restore` on the server leave a project's replicas where they are (see above).
- A project homed on a follower can be failed over by nothing while its node is down.
- A project that is homed on a follower (after a failover) keeps serving, with some limits. A restore or an upgrade of it is refused until it is homed on the leader again, and a follower cannot roll out its upgrade because that writes the read-only registry. Its Postgres settings, role passwords and extensions are not applied from the leader, which answers that it does not support them; the settings of Auth and REST reach the home and restart both services. Its nightly base backup runs on the follower, which cannot record the backup's row in its read-only registry, so a backup list on the leader can lag behind what the archive holds. WAL archiving is unaffected.
- A major Postgres upgrade of a project that has replicas is refused, and no option removes the replicas first: `supavise replicas rm` does. A release whose manifest says `wal_compat: false` makes `supavise upgrade` refuse a server that holds a primary while another server holds a standby of it on an older release; upgrade the servers that hold standbys first.
- Replicas follow the primary's size and saved settings only through the restarts that a resize or a settings save makes. A resize or a save made while the project is paused restarts no replica; restart it by hand.
- Logs have no per-replica routing in Studio.
- An old v0.1.x stack that has only one Elastic IP loses its stable address when it is the node that gives up the service address. The survivor still reaches it, and a failback restores it.
- Two independent deployments in one AWS account need distinct `ClusterName` values (see the deploy guide).

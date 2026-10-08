# internal/replicas

The read-replica controller (design 2.7). It turns `supavise.replicas` rows into running standbys on the other servers of the cluster, reports their setup steps, status and lag, restarts and removes them, and, with `[replicas] default = "all"`, keeps a replica of every project on every other node. It runs on the leader, drives the nodes only through `placement.InstanceOps` and takes base backups only through `backup.BaseBackupEnsurer`, so everything below is tested with fakes of the two.

The registry rows are the desired state. The controller is level triggered: each pass reads the rows and does what leads each one to its next step, so a restart of the daemon, a crash between two steps or a node that was away resumes from the step the row holds. Only status transitions are written to the registry. Lag and receiver state live in memory (invariant I5).

`Controller` (`New(Options)`) implements three interfaces of `contract.go`:

| Interface | Used by |
|---|---|
| `Service`: `Setup`, `SetupOn`, `Remove`, `Restart`, `List`, `Statuses`, `Lag` | the Management API handlers (`internal/api`), `supavise replicas` |
| `Remover`: `RemoveAll(ref)`, `RemoveOn(node)` | a project delete or in-place restore (replicas first), `supavise node rm`; neither records an opt-out |
| `ReportSink`: `HandleReport` | the peer API's `POST /peer/v1/report` intake (internal/mesh) |

`CreateSystemReplica(ctx, reg, nodeID)` is what the cluster join calls to record the standby of the system cluster on a joining node.

## Setup

`Setup` and `SetupOn` validate and write one row (`INIT_READ_REPLICA`, `0_requested`), then wake the controller. Refusals are `*UserError`s whose text is the 400 body Studio shows (design 2.7.2); nothing is written when one is returned:

- "No Supavise server is joined in <region>."
- "Read replicas on the same server as the primary are not offered."
- "This project already has a replica on <node>."
- "Read replicas need a compute size of small or larger."
- "The project already has the maximum of <n> read replicas." (`MaxReplicas`: 4 from small to large, 5 above, and at most the other active servers)
- "Not enough capacity on <node>."
- "Read replicas need S3-compatible backup storage."

and, beside the spec's list, a branch ("Read replicas are not offered for branches."), a node that has not finished joining, `config.CheckReplicaPorts` failing, and a project whose port sequence is above `config.MaxReplicaSeq()`. With several servers in a region the one holding the fewest projects and replicas is chosen. A later `Setup` clears the opt-out a removal left.

The controller then moves the row through the seven spec steps. Each pass a worker looks at the step and does the work that leads out of it:

| Row at | Work | Failure code |
|---|---|---|
| `0_requested` | The pass admits it: a free slot (`[replicas] concurrency` setups past this step), the project runs, `Admitter` says the node has room. Otherwise it stays here and the node gets a `replica_capacity` alert. | none: it waits |
| `1_started` | `EnsureBase(ref, bootstrap_max_backup_age)`, then `InstanceOps.Ensure` with the base backup id and the epoch. | `1_read_replica_instance_launch_failed` |
| `2_launched_read_replica_instance` | Observe the node until it reports seeding started. | `2_initiate_read_replica_setup_failed` |
| `3_initiated_read_replica_setup` | Observe until the base backup is downloaded. | `3_download_base_backup_failed` |
| `4_downloaded_base_backup` | Observe until the standby replayed the archive and streams. | `4_replay_wal_archives_failed` |
| `5_replayed_wal_archives` | When the node reports the standby up and PostgREST ready, `Pooler.EnsureReplicaTenant`. | `5_complete_read_replica_setup_failed` |
| `6_completed_read_replica_setup` | Status from the mapping below. | |

The row only moves forward. A failure leaves `INIT_READ_REPLICA_FAILED` with the step reached in `init_step` and the code in `init_error`, raises `replica_unhealthy` and stops: Studio tells the user to remove the replica and add it again. A setup fails when the node reports an error (a code that is not one of the five becomes the code of the current step), when a call has failed for `Timeouts.Retry` (10 minutes; a failed call is tried again after 10 seconds, doubling to 2 minutes), when the node answers a request with a 4xx that retrying cannot change, or when a step takes longer than its allowance (seeding to start 5 minutes, download 30 minutes or 4x the estimate, replay 2 hours or 4x the estimate, completion 10 minutes; `Options.Timeouts`). A node that lost the instance is asked for it again (`Ensure` is idempotent), and so is a node whose step has not changed for 2 minutes, which resumes a setup a restart of its daemon interrupted.

The standby of the system cluster (`origin = "system"`) is the exception: the joining node seeds it before its daemon runs, so the controller never launches it. It watches the row and completes it once the node is active and reports the standby streaming.

`Statuses` returns the row's status and `replicaInitializationStatus`: `in_progress` with the step and the estimations, `failed` with the code, or `completed`. The estimations are the stored size of the base backup over the measured download rate (an average of earlier downloads, 100 MB/s until one is measured) and the WAL between the standby's replay position and the furthest position any instance of the project reported over an assumed 50 MB/s; zero when unknown.

## Status and lag

For a replica past its setup, each pass takes the node's latest observation (a report, or a poll of `Observe` when the last report is older than the interval) and maps it (design 2.7.7):

- `ACTIVE_HEALTHY`: Postgres up, PostgREST ready, the receiver streaming, lag at most `unhealthy_lag_seconds`.
- `ACTIVE_UNHEALTHY`: the receiver not streaming for 2 minutes, PostgREST down, Postgres down, lag over the limit, or the node silent for 2 minutes. It raises `replica_unhealthy`, and resolves it when the replica is well again. A lag over 60 seconds (or half the limit, if lower) that is still within the limit raises `replica_lag`.
- `RESTARTING` (`Restart`) and `RESIZING` (set by the resize) last until the first observation made after they were set shows the replica well, and become `ACTIVE_UNHEALTHY` after 10 minutes.
- `GOING_DOWN`: see below.

A project that is paused or going away is not judged. A node that lost an active replica's instance is asked for it again, at most every 5 minutes.

The leader keeps a 24 hour ring per replica, one point per minute (the mean of that minute's samples), for `Lag(identifier, since)`. It is empty after a restart. The controller also writes `<state_dir>/replicas-status.json` every pass (identifiers, lag, receiver state, no secrets; mode 0644) so `supavise replicas ls` in another process can show the lag; `ReadSnapshot` ignores a file older than 2 minutes.

## Default replicas

With `[replicas] default = "all"`, each pass writes a missing row (`origin = "default"`) for every project that is not the system project or a branch and runs (`ACTIVE_*`, `RESTARTING`, `RESIZING`), on every active node other than its home, unless a `replica_optouts` row names that pair. The size and count limits of a manual request do not apply. It is skipped, with one log line, when `[backup]` is a `file://` backend or the replica ports do not fit, and for a project past `MaxReplicaSeq`. Admission and the concurrency limit are the setup's: a replica that does not fit waits at `0_requested`. A paused project's replicas appear when it runs again.

## Removal

`Remove(ref, identifier)` returns `ErrNotFound` for an identifier that is not a replica of `ref`, otherwise sets `GOING_DOWN` and returns. A replica of the default (or any replica while the default is `all`) writes an opt-out first, so the reconciler does not make it again. The controller then drops the Supavisor tenant, calls `InstanceOps.Remove` and deletes the row; a node that does not answer leaves the row `GOING_DOWN` and the controller tries again every pass, with a growing pause, and alerts after 15 minutes. A node whose state is `left` has nothing to reach and its rows are deleted. `RemoveAll` and `RemoveOn` do the same in the call and return a `*PendingError` listing what is left.

## Admission

`RoomAdmitter` judges a replica as a create judges a project: `lifecycle.ComputeCapacity` over the projects homed on the node plus a stand-in for each replica already admitted there, against the node's memory budget (`[compute] overcommit`) and cores, and the node's free disk against 1.25 times the base backup plus 1 GiB. A resource that is not known skips its check. The daemon knows its own machine only (`internal/app/wire_replicas.go`), so a remote node's replica is judged by the node when it is asked to create it.

## Wiring

`internal/app/wire_replicas.go` builds the controller from what earlier hooks provide: `cluster.Membership`, `placement.InstanceOps`, `backup.BaseBackupEnsurer` (or the Management API's backup service when it implements it) and `replicas.Pooler`. It provides `replicas.Service`, `replicas.Remover` and `replicas.ReportSink`. It starts `Run` only when both `InstanceOps` and `BaseBackupEnsurer` exist; otherwise the service only reads and writes rows, and a node with no cluster answers a setup request with "No Supavise server is joined". A cluster of one node with no replica rows is looked at once a minute.

Not wired from here: the API handlers take the service from `replicas.Service` (the API's `Deps` gains the field), the peer API's report intake calls `ReportSink.HandleReport`, `supavise node rm` and project delete call `Remover`, and the cluster join calls `CreateSystemReplica`.

## Tests

`go test ./internal/replicas/` runs the whole controller against a Memory registry and fakes of `InstanceOps` (a map of nodes whose instances move one step per observation), `BaseBackupEnsurer`, `Pooler` and `Admitter`, with a clock the tests move: every step and failure code, retry and timeout behavior, the 400 messages, the reconciler with opt-outs, capacity and the concurrency limit, status mapping, estimations, the lag ring, removal with a node that is away, restart, the system standby, and a run of the real loop over two nodes to `ACTIVE_HEALTHY`. Nothing needs Postgres or Linux.

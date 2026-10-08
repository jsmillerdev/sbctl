# internal/replicas

The read-replica controller (design 2.7). It turns `supavise.replicas` rows into running standbys on the other servers of the cluster, reports their setup steps, status and lag, restarts and removes them, and, with `[replicas] default = "all"`, keeps a replica of every project on every other node. It runs on the leader, drives the nodes only through `placement.InstanceOps` and takes base backups only through `backup.BaseBackupEnsurer`, so everything below is tested with fakes of the two.

The registry rows are the desired state. The controller is level triggered: each pass reads the rows and does what leads each one to its next step, so a restart of the daemon, a crash between two steps or a node that was away resumes from the step the row holds. Only status transitions are written to the registry. Lag and receiver state live in memory (invariant I5).

`Controller` (`New(Options)`) implements three interfaces of `contract.go`:

| Interface | Used by |
|---|---|
| `Service`: `Setup`, `SetupOn`, `Remove`, `Restart`, `List`, `Statuses`, `Lag` | the Management API handlers (`internal/api`), `supavise replicas` |
| `Remover`: `RemoveAll(ref)`, `RemoveOn(node)` | a project delete or in-place restore (replicas first), `supavise node rm`; neither records an opt-out |
| `ReportSink`: `HandleReport` | the peer API's `POST /peer/v1/report` intake (internal/mesh) |

`HandleReport` trusts `Report.Node`: it takes an instance from a report only when the row is on that node and, when the status names a project, the row's own. It reads the node's replicas with one query per report, however many instances it holds. The intake must therefore set `Node` from the node's mutually authenticated identity, never from the request body (the mesh intake does), and hand the report over with a closure that supplies the context: `reports.Subscribe(func(rep peerapi.Report) { sink.HandleReport(ctx, rep) })`.

The standby of the system cluster on a joining node is recorded by `replicaid.EnsureSystem` (`internal/replicas/replicaid`), which the cluster join calls. It lives in its own package because the controller depends on `internal/placement`, which depends on `internal/cluster`, so the join cannot import this package. `replicaid` also holds the naming rule (`Region`, `Short`, `Identifier`, `Create`) the controller and the CLI use.

## Setup

`Setup` and `SetupOn` validate and write one row (`INIT_READ_REPLICA`, `0_requested`), then wake the controller. Refusals are `*UserError`s whose text is the 400 body Studio shows (design 2.7.2); nothing is written when one is returned:

- "No Supavise server is joined in <region>."
- "Read replicas on the same server as the primary are not offered."
- "This project already has a replica on <node>."
- "Read replicas need a compute size of small or larger."
- "The project already has the maximum of <n> read replicas." (`MaxReplicas`: 4 from small to large, 5 above, and at most the other active servers)
- "Not enough capacity on <node>." (only for the node the daemon runs on: see Admission)
- "Read replicas need S3-compatible backup storage."

and, beside the spec's list, a branch ("Read replicas are not offered for branches."), a node that is joining, fenced or has left, a replica on the node that is still going down ("The replica on <node> is being removed; try again when it is gone."), `config.CheckReplicaPorts` failing, and a project whose port sequence is above `config.MaxReplicaSeq()`. With several servers in a region the one holding the fewest projects and replicas is chosen. A later `Setup` clears the opt-out a removal left.

`Setup` is where the limits are enforced; the checks the Management API makes in front of it are a fast path and cannot stop two requests that pass them together. The size cap, the limit of joined servers minus one and the home-node rule (I2) hold like this:

- Requests to one process take turns: `Setup` and `SetupOn` hold a lock from the first read to the end.
- The home-node rule is the registry's: `CreateReplica` refuses the project's home inside the transaction that inserts the row, so a failover that lands between the check and the insert is refused there, and `Setup` words the refusal ("Read replicas on the same server as the primary are not offered.").
- Requests of two processes (the daemon and `supavise replicas add`) share the registry and no lock, and the registry has no multi-statement transaction to hold the count and the insert together. The cap is therefore checked again after the insert: a row that leaves the project over its cap is deleted and refused with the "maximum" text. The request that inserts last always sees the others' rows, so the cap is never exceeded; two that insert together may both withdraw, and a retry succeeds. Replicas made by `[replicas] default = "all"` do not pass through this check. A withdrawn row exists for a few milliseconds. A daemon whose pass reads it in that time may start the setup of a replica that is then gone, and a node that got as far as creating the instance keeps it, with no row to remove it by, until it is removed by hand. That takes two requests that collide and a pass inside the window; a transaction in the registry would close it.

The controller then moves the row through the seven spec steps. Each pass a worker looks at the step and does the work that leads out of it:

| Row at | Work | Failure code |
|---|---|---|
| `0_requested` | The pass admits it: a free slot (`[replicas] concurrency` setups past this step), the project runs, `Admitter` says the node has room. Otherwise it stays here and the node gets a `replica_capacity` alert. | none: it waits |
| `1_started` | `EnsureBase(ref, bootstrap_max_backup_age)`, then `InstanceOps.Ensure` with the base backup id and the epoch. A node that refuses for lack of room (a `RoomError`) sends the row back to `0_requested`; see Admission. | `1_read_replica_instance_launch_failed` |
| `2_launched_read_replica_instance` | Observe the node until it reports seeding started. | `2_initiate_read_replica_setup_failed` |
| `3_initiated_read_replica_setup` | Observe until the base backup is downloaded. | `3_download_base_backup_failed` |
| `4_downloaded_base_backup` | Observe until the standby replayed the archive and streams. | `4_replay_wal_archives_failed` |
| `5_replayed_wal_archives` | When the node reports the standby up and PostgREST ready, `Pooler.EnsureReplicaTenant`. | `5_complete_read_replica_setup_failed` |
| `6_completed_read_replica_setup` | Status from the mapping below. | |

The row only moves forward. A failure leaves `INIT_READ_REPLICA_FAILED` with the step reached in `init_step` and the code in `init_error`, raises `replica_unhealthy` and stops: Studio tells the user to remove the replica and add it again. A setup fails when the node reports an error (a code that is not one of the five becomes the code of the current step), when a call has failed for `Timeouts.Retry` (10 minutes; a failed call is tried again after 10 seconds, doubling to 2 minutes), when the node answers a request with a 4xx that retrying cannot change, or when a step takes longer than its allowance (seeding to start 5 minutes, download 30 minutes or 4x the estimate, replay 2 hours or 4x the estimate, completion 10 minutes; `Options.Timeouts`). A node that lost the instance is asked for it again (`Ensure` is idempotent), and so is a node whose step has not changed for 2 minutes, which resumes a setup a restart of its daemon interrupted. A node that answers every request and still reports the instance absent runs out the step's allowance like any other.

The standby of the system cluster (`origin = "system"`) is the exception: the joining node seeds it before its daemon runs, so the controller never launches it. It watches the row and completes it once the node is active and reports the standby streaming.

`Statuses` returns the row's status and `replicaInitializationStatus`: `in_progress` with the step and the estimations, `failed` with the code, or `completed`. A replica that is going down keeps the outcome of its setup (failed when it failed, in progress when it never finished). The estimations are the stored size of the base backup over the measured download rate (an average of earlier downloads, 100 MB/s until one is measured) and the WAL between the standby's replay position and the furthest position any instance of the project reported over an assumed 50 MB/s; zero when unknown.

## Status and lag

For a replica past its setup, each pass takes the node's latest observation (a report, or a poll of `Observe` when the last report is older than the interval) and maps it (design 2.7.7):

- `ACTIVE_HEALTHY`: Postgres up, PostgREST ready, the receiver streaming, lag at most `unhealthy_lag_seconds`.
- `ACTIVE_UNHEALTHY`: the receiver not streaming for 2 minutes, PostgREST down, Postgres down, lag over the limit, or the node silent for 2 minutes. It raises `replica_unhealthy`, and resolves it when the replica is well again. A lag over 60 seconds (or half the limit, if lower) that is still within the limit raises `replica_lag`.
- `RESTARTING` (`Restart`) and `RESIZING` (set by the resize) last until the first observation made after they were set shows the replica well, and become `ACTIVE_UNHEALTHY` after 10 minutes. `Restart` does not block: it returns once the row says `RESTARTING` and the call to the node is under way in the background (`Timeouts.Restart`, 5 minutes). A call that fails, including one that times out, writes `ACTIVE_UNHEALTHY` and raises `replica_unhealthy` under a context of its own; a call cut off by the daemon stopping writes nothing, and the next daemon looks at the replica as it does after any restart. The Management API answers a replica restart as soon as `Restart` returns.
- `GOING_DOWN`: see below.

A project that is paused or going away is not judged. A node that lost an active replica's instance is asked for it again, at most every 5 minutes.

The leader keeps a 24 hour ring per replica, one point per minute (the mean of that minute's samples), for `Lag(identifier, since)`. It is empty after a restart. The controller also writes `<state_dir>/replicas-status.json` every pass so `supavise replicas ls` in another process can show the lag: for each replica its identifier, receiver status, lag and when it was observed, no LSNs and no secrets, mode 0600. It is a view of the last pass that nothing reads back; `ReadSnapshot` ignores a file older than 2 minutes. Only the leader's daemon writes it, so `ls` on a follower, or while the daemon is down, shows no lag.

This file is a deviation from I5 (agents report observed state to the leader, which keeps it in memory, and the registry holds desired state and status only). It puts observed state on disk, for one reader in another process, and the design does not list it. It stays because `supavise replicas ls` has no other way to show the lag, and it is bounded: mode 0600, rewritten atomically each pass, no LSNs, no secrets, nothing in the registry or the replication path reads it, and a daemon that is not running leaves a file the reader ignores after 2 minutes. If `ls` should ask the daemon instead, this file goes.

Alerts follow the replica's edges: `replica_unhealthy` and `replica_lag` open when the condition starts and close when it ends, each closing event repeating the title it opened with. Which alerts are open is kept in memory. After a restart of the daemon a replica that the registry holds as `ACTIVE_UNHEALTHY` and finds well closes its `replica_unhealthy` alert, and removing a replica whose setup failed closes the one that failure opened; an open `replica_lag` or `replica_capacity` alert that ended during the restart is not closed, because the controller cannot ask the notifier which alerts are open.

## Default replicas

With `[replicas] default = "all"`, each pass writes a missing row (`origin = "default"`) for every project that is not the system project or a branch and runs (`ACTIVE_*`, `RESTARTING`, `RESIZING`), on every active node other than its home, unless a `replica_optouts` row names that pair. The size and count limits of a manual request do not apply. It is skipped, with one log line, when `[backup]` is a `file://` backend or the replica ports do not fit, and for a project past `MaxReplicaSeq`. Admission and the concurrency limit are the setup's: a replica that does not fit waits at `0_requested`. A paused project's replicas appear when it runs again.

## Removal

`Remove(ref, identifier)` returns `ErrNotFound` for an identifier that is not a replica of `ref`, otherwise sets `GOING_DOWN` and returns. A replica of the default (or any replica while the default is `all`) writes an opt-out first, so the reconciler does not make it again. The controller then drops the Supavisor tenant, calls `InstanceOps.Remove` and deletes the row; a node that does not answer leaves the row `GOING_DOWN` and the controller tries again every pass, with a growing pause, and alerts after 15 minutes. A node whose state is `left` has nothing to reach and its rows are deleted.

`RemoveAll` and `RemoveOn` do the same in the call, after waiting up to `Timeouts.Busy` (30 seconds) for a setup that is acting on the replica, and return a `*PendingError` listing what is left. Only a row that is gone (`registry.ErrNotFound`) counts as removed. A registry that refuses the write (a follower's read-only registry, a connection that dropped) is no removal: `Remove` and `Restart` return that error, not "no such replica", and `RemoveAll` lists the replica as pending, so the guard on a project delete holds. Two rules fall on their callers:

- A project row owns its replica rows, so a project delete (or an in-place restore) must not go on while `RemoveAll` returns a `*PendingError`: the retry would vanish with the row and the instance would stay on the node. Refuse the request, or retry until it returns nil.
- `supavise node rm` must move the node out of the active state first and call `RemoveOn` after. While the node is active the default reconciler makes the replicas again, and `RemoveOn` writes no opt-out; it refuses an active node.

The row of the system standby (`origin = "system"`) is only deleted by a removal, never sent to the node as an instance to remove: that instance is the node's own copy of the registry, and removing it would wipe the cluster of a node that is fenced and alive. The retirement of a node takes its system cluster (`internal/cluster`).

The `GOING_DOWN` guard is a lock inside one process. `supavise replicas rm` runs in its own process and the registry write is not conditional, so a daemon that read the row just before the command wrote `GOING_DOWN` can write the next step over it within the same few milliseconds. The command's output says the daemon finishes the removal; if `replicas ls` still shows the replica, run `rm` again. A conditional write in the registry (`status <> 'GOING_DOWN'`) closes the window.

## Admission

`RoomAdmitter` judges a replica as a create judges a project: `lifecycle.ComputeCapacity` over the projects homed on the node plus a stand-in for each replica already admitted there, against the node's memory budget (`[compute] overcommit`) and cores, and the node's free disk against 1.25 times the base backup plus 1 GiB. A resource that is not known skips its check. The daemon knows its own machine only (`internal/app/wire_replicas.go`), so the leader judges its own node's room and the 400 "Not enough capacity on <node>." exists for that node alone. A replica for a remote node is accepted, and the node judges it when it is asked to create the instance.

A node that has no room answers `InstanceOps.Ensure` with `placement.ErrNoRoom` (the agent's 507, which the leader's remote calls give back wrapped so that `errors.Is` finds it), a `*lifecycle.CapacityError`, or another error that satisfies `RoomError` (`errors.As` finds a `NoRoom() bool` that returns true), before it creates anything. The controller then sends the row back to `0_requested`, keeps it out of the concurrency count, raises the node's `replica_capacity` alert (one per node) and asks again a minute later, for as long as the refusal lasts: the wait is not a failed call, so `Timeouts.Retry` does not bound it. The alert closes when no replica waits for the node any more or one the node never refused was admitted. A replica that the node refused and the pass admitted again still counts as waiting until its launch goes through, so the pass that follows the admission does not close an alert the next refusal would open again. The standby of the system cluster is not counted among the replicas a node holds: the capacity accounting leaves the system project out. A refusal that arrives as a failed instance status (`InstanceStatus.Error` set) is a failure of the node's setup, and ends it with `1_read_replica_instance_launch_failed`.

## Command line

`supavise replicas ls [ref] [--json]`, `add <ref> --region R [--node N]` and `rm <identifier>` (`cmd/supavise/cmd_replicas.go`) build a `Controller` over the registry with no node operations, so they only read and write rows; the daemon on the leader does the work, whether it was running when the row was written or not. `add` therefore does not judge capacity (the daemon does when it admits the replica) and `ls` takes lag from the snapshot file. On a node that follows, the registry is read-only and `add` and `rm` say to run on the leader.

## Wiring

`internal/app/wire_replicas.go` builds the controller from what earlier hooks provide: `cluster.Membership`, `placement.InstanceOps`, `backup.BaseBackupEnsurer` (or the Management API's backup service when it implements it) and `replicas.Pooler`. It provides `replicas.Service`, `replicas.Remover` and `replicas.ReportSink`. It starts `Run` only when both `InstanceOps` and `BaseBackupEnsurer` exist; otherwise the service only reads and writes rows, and a node with no cluster answers a setup request with "No Supavise server is joined". A cluster of one node with no replica rows is looked at once a minute.

When the node has joined others (more than one node row) and the controller cannot start, or it has no pooler, `wire_replicas.go` logs a warning; on a single server it stays quiet.

The base backup comes from `BaseBackupEnsurer`, which the daemon's backup service implements for projects homed on the leader. For a project homed on another node the backup has to be taken where its data directory is (`placement.BackupOps` and the backup service's `Options.TakeBase`, which `EnsureBase` calls when it needs a new backup); that routing is set where the backup service is built, not in this package, and until it is, a replica of such a project starts only when a base backup young enough already exists; otherwise taking one fails and the setup ends with `1_read_replica_instance_launch_failed`.

Not wired from here: the API handlers take the service from `replicas.Service` (the API's `Deps` gains the field), the peer API's report intake calls `ReportSink.HandleReport`, `supavise node rm` and project delete call `Remover` (see the rules under Removal), and the cluster join calls `replicaid.EnsureSystem`.

## Tests

`go test ./internal/replicas/` runs the whole controller against a Memory registry and fakes of `InstanceOps` (a map of nodes whose instances move one step per observation), `BaseBackupEnsurer`, `Pooler` and `Admitter`, with a clock the tests move: every step and failure code, retry and timeout behavior, the 400 messages, the reconciler with opt-outs, capacity (the leader's judgment and a node's refusal) and the concurrency limit, status mapping, estimations, the lag ring, removal with a node that is away or a worker that is busy, restart, alert titles, the worker bound, the system standby, and a run of the real loop over two nodes to `ACTIVE_HEALTHY`. `replicaid` has its own tests. Nothing needs Postgres or Linux.

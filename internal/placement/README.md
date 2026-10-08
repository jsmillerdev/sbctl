# internal/placement

Where a project's pieces run, and how an operation gets there. A project has one home node (its
primary Postgres, GoTrue and PostgREST; `registry.Project.NodeID`) and zero or more replicas on other
nodes (`registry.Replica`). The leader drives every project through one `lifecycle.Plane`; this package
makes that plane send each call to the project's home, and runs the replica operations on the node that
holds the replica (replica design, sections 2.5 to 2.7 and 2.10).

`contract.go` is the interface the other packages are written against (`Resolver`, `PlaneRouter`,
`InstanceOps`, `BackupOps`, `Agent`). The rest implements it.

## Plane router and remote plane

- `Router` (`router.go`) is the plane the Engine drives (`Engine.SetPlane`). A call for a project homed
  on this node goes to the node's own plane; any other goes to a `RemotePlane` for the home. A `Router`
  is a `lifecycle.HomeRouter`: an Engine drives a project homed elsewhere only through one, so the CLI,
  which opens the node's own plane, refuses a project that runs on another node. The home is
  the `node_id` of the project's row, so a project that moved is routed to its new home from the next
  call. `Route` always answers from the local plane: a canonical port is the service on the home and a
  forwarder to it elsewhere, so the answer is the same on every node. A delete or stop for a ref the
  registry no longer has runs locally (leftovers can only be here).
- The Engine finds optional capabilities of its plane by type assertion (`lifecycle.FullPlane` lists
  them). The router has all of them and runs each on the local plane; for a project homed elsewhere they
  answer `lifecycle.ErrNotSupported`, because its settings are applied on its home.
- `RemotePlane` (`remote.go`) implements `lifecycle.Plane` over `mesh.RPC`: `POST
  /peer/v1/projects/{ref}/plane/{method}` with a `peerapi.PlaneCall` (the cluster epoch and the JSON of
  the arguments) and a `PlaneResult`. `shapes.go` is the one place that says what each method carries:
  the project row and keys for `create`, `start`, `start_database`, `reconfigure`; the row for `health`;
  nothing for the rest. A seed (`DataSeeder`) is a function and does not travel (`ErrRemoteSeed`).
  `Health` has no error to return, so an unreachable node is every service unhealthy, with the cause.
- `POST /peer/v1/projects/{ref}/plane/final_checkpoint` is not a method of `lifecycle.Plane`: it reads the
  control file of a stopped cluster on its home (`lifecycle.ReadControl`, answered as `lifecycle.ControlInfo`),
  which a switchover needs after the old primary's fast shutdown to learn the position the new primary
  must replay to (`PromoteOptions.WaitLSN`). `Router` and `RemotePlane` implement `CheckpointReader`.
- Two more requests on the plane path serve what the leader's Engine runs for a project homed elsewhere
  and that is no plane call. `start_timer` and `stop_timer` start and stop the project's nightly base
  backup timer on its home, where its data is (`Router.Timers` is the `lifecycle.Timers` the daemon
  gives the Engine through `Engine.SetTimers`; a node with no timers, the exec backend, does nothing).
  `node_resources` reads the memory and cores of the home (`lifecycle.NodeResources`; zero when the node
  does not know them), which the Engine judges a resume or a resize against (`Router` is the
  `lifecycle.NodeResourcer` given to `Engine.SetRemoteNodes`).
- A test fails when `lifecycle.Plane` gains a method that lacks a `peerapi.PlaneMethod`, an entry in the
  agent's table (`planeCalls`) or a method on `RemotePlane`.

Errors cross the wire as a status and a message, and the sentinel comes back, so `errors.Is` works on the
leader as it did on the node:

| Status | Meaning | Error |
|---|---|---|
| 403 | the caller is not the cluster leader | `cluster.ErrNotLeader` |
| 404 | an unknown project or replica | `registry.ErrNotFound` |
| 409 | the request is under an older epoch than the node's | `ErrStaleEpoch` |
| 410 | no restorable state | `lifecycle.ErrNoRestorableState` |
| 412 | the directory holds a cluster | `lifecycle.ErrClusterExists` |
| 421 | the project is not homed on the node | `ErrNotHome` |
| 422 | not allowed in the current state | `lifecycle.ErrInvalidState`; the answer's code also tells `lifecycle.ErrFenced` (the node or the project is fenced), `ErrNotStandby` (both of these match `ErrInvalidState` too) and `ErrNotCleanShutdown` (it does not) apart |
| 501 | the node has no backup engine | `lifecycle.ErrNoSnapshot` |
| 504 | replay did not reach the position asked | `lifecycle.ErrReplayBehind` |
| 507 | the node has no room for the replica | `*NoRoomError`, which matches `ErrNoRoom` and has the `NoRoom() bool` method the replica controller looks for; on the leader's own node the admission's `*lifecycle.CapacityError` (or an error wrapping `lifecycle.ErrReplicaDisk`) comes through `ErrNoRoom` |

`Refused(err)` says whether an error is a node's refusal before it changed anything (not the leader, an
older epoch, not the home, not found, the state does not allow it, a fenced node, a standby that has not
replayed as far as asked, no room, no backup engine, nothing to restore). The failover orchestrator
treats a promotion that ends in anything else as possibly done: a status 500, a lost answer, a failure
in the middle of the work, or `ErrNotCleanShutdown`, which the node answers after it stopped the
cluster.

## Node agent and peer API endpoints

`Register` (`handlers.go`) registers, through `mesh.Handle`, what the leader calls on a node: `PUT`,
`GET` and `DELETE /peer/v1/instances/{identifier}`, `POST /peer/v1/instances/{identifier}/{action}`,
`POST /peer/v1/projects/{ref}/plane/{method}` and `POST /peer/v1/projects/{ref}/backup/{op}`. Every
handler admits the cluster leader only (the client certificate's node), refuses a request under an
older epoch than the node's (the removal of an instance, which has no body, carries it as `?epoch=`; a
request without one is judged by the leader check alone), and, for the plane and backup endpoints, one for a project that is not
homed on the node (the node's copy of the registry can trail the leader's by a moment; the leader asks
again; after the registry moves a home, the first plane call for the new home can arrive before the new
home's copy has the row, and `Router` and `RemotePlane` do not retry it: the caller that moved the
home does). `Register` refuses to be built without the agent, the membership and the resolver, which
the handlers check against. `Ops` implements `InstanceOps` and `BackupOps`: the agent itself for this
node, the peer API for another.

`NodeAgent` (`agent.go`) implements `Agent` over the node's plane (`ReplicaPlane`, which
`*lifecycle.PostgresPlane` satisfies):

- `Ensure` checks the replica (an identifier of the project, never on the project's home, the port
  sequence fits, one replica of the project per node, room for it: `Engine.AdmitReplica`) and sets it up
  in the background, answering at once; a repeated request reports what is there and starts nothing.
  The node finds the replica it holds by `replica.json` as well as in memory, so a request for a second
  one after a restart of the daemon is refused. A replica with no room for it (`ErrNoRoom`, 507: the
  memory budget or the disk) is recorded nowhere, so the leader asks again once there is room.
  The setup follows design 2.7.2 and records its step in `projects/<ref>/replica.json` (0600):

  | Step reached | Work that leads to the next | Failure code while at this step |
  |---|---|---|
  | `1_started` | directories, `pg_hba.conf`, root key | `1_read_replica_instance_launch_failed` |
  | `2_launched_read_replica_instance` | the seeder starts | `2_initiate_read_replica_setup_failed` |
  | `3_initiated_read_replica_setup` | the base backup downloads and extracts | `3_download_base_backup_failed` |
  | `4_downloaded_base_backup` | the standby starts, replays the archive and streams | `4_replay_wal_archives_failed` |
  | `5_replayed_wal_archives` | the replica's PostgREST starts | `5_complete_read_replica_setup_failed` |
  | `6_completed_read_replica_setup` | | |

  A failed setup stays failed until the replica is removed and added again. A setup the daemon's stop or
  restart cut off keeps its step and resumes at the next request: from the beginning before the base
  backup was extracted, from the start of the standby after. A standby that does not stream and replays
  nothing for 15 minutes fails at step 4. An archive-only standby (`NoUpstream`) completes without
  streaming and without PostgREST.
- `Observe` reports the step and error plus the live state of the standby (`InstanceStatus`).
- `Remove` stops a setup in flight, then the units, and deletes the directory. It removes only the
  replica the node holds (another identifier of the project is refused), never the standby of the system
  cluster (the node's own registry; it goes with the node when the node leaves), and refuses when the
  project is homed on this node, when the cluster here has become the primary, or when the data directory is a
  cluster without `standby.signal` (a promoted replica that is stopped does not answer), unless the
  setup it belongs to has not finished: removing a replica must never remove a home.
- `Do` runs `restart` (renders from `InstanceAction.Class` when the leader names the size, because the
  node's copy of the registry may not have it yet), `stop`, `start`, `promote` and `demote` (the failover
  orchestrator's steps 4 and 7 of a project switchover; `lifecycle.PromoteReplica` and
  `DemoteToReplica`). An action during a setup is refused. `start`, `restart` and `stop` are refused
  when the registry names this node the project's home, and `start` and `restart` also when the cluster
  here is a primary (no `standby.signal`) or follows as another replica: the replica's spec would be
  rendered over a writable cluster. A resume of an interrupted setup (`Ensure`) is refused on the home
  too, because it deletes the project's directory, and so is a `promote`, except one repeated after
  the home moved here, which finds the cluster a primary and ends the replica instance. A `demote` is
  refused while the registry still names this node the home: the registry moves the home first (design
  2.10.3, steps 5 and 7), and a demotion of the home would stop its primary. A promotion
  and a demotion take the project's ports from the mesh's forwarders while they run
  (`lifecycle.PlaneOptions.HoldPorts`; `internal/app/wire_placement.go` binds it to
  `mesh.Forwarders.Suspend`): on a replica's node the canonical ports are forwarders to the old home,
  and Postgres could not bind them.
- `StartLocal` starts the node's complete replicas after a restart, four at a time
  (`AgentOptions.Concurrency`); their units are not enabled for boot. `ObserveAll` reads them four at
  a time as well, and `ReportCache` probes the replicas and the projects side by side, so a follower
  that holds a replica of every project neither starts them one after another nor misses its report's
  interval.
  A cluster that is no standby of its replica (no `standby.signal`, or a standby that follows as another
  replica) is left alone: the node was promoted and the registry has not caught up, and the replica's spec
  would run a writable cluster on the replica port beside the real one.

`ReportCache` keeps the observation of the node's replicas and of the projects homed there in memory and
refreshes it every 10 seconds, so that a report answers from memory and never waits for a probe (each
replica is a SQL and an HTTP probe; the daemon checks the projects homed here only on a node that is not the
leader, which reports to nobody and reads its own). `Contribution` is its answer in the shape the cluster package's
reporter adds to the report it sends (`cluster.Contributor`); the cluster's reporter stores the report of
a leader too, which covers the replicas the leader holds. `Reporter` is the sender until then: it tells
the leader what the node observes every 10 seconds (`POST /peer/v1/report`) when the node is not the
leader, and logs once, not every tick, that a leader has no report endpoint (`ErrNoReportEndpoint`).

`Fleet` is the `lifecycle.ReplicaFleet` of the leader's Engine: it restarts a project's replicas through
`InstanceOps` when the project is resized.

Five more pieces are the wiring's to attach:

- `LocalPrimaries` (`localprimaries.go`) is the failover orchestrator's hand on the primaries homed here
  (it has the methods of `failover.LocalPrimaries`; this package does not import `internal/failover`). `Stop`
  stops the backup timer and the units and returns the cluster's latest checkpoint location from
  its control file, an error when it cannot be read or the cluster did not shut down cleanly; `Start`
  starts a primary (a fenced one answers `lifecycle.ErrFenced`); `Healthy`; `SetAside` refuses while a
  postmaster of the directory is alive and otherwise calls `MoveAside`, which the wiring binds to
  `failover.SetAsideDiverged`.
- `RoutedBackups` (`routedbackups.go`) takes a base backup on the node a project is homed on and records
  it on the leader: the home takes it without writing its registry (`backup.BackupOptions.NoRecord`;
  the backup handler always asks for that, because a follower's registry is a read-only copy), and the
  leader writes the row and the event (`backup.Service.RecordBase`). `TakeBase` is
  `backup.Options.TakeBase`, which `EnsureBase` uses to seed a replica; `FinalBackup` is
  `lifecycle.RemoteBackups`, which `Engine.SetRemoteBackups` takes for the final backup of a delete.
  Every other caller of `Ops.BaseBackup` for a project homed elsewhere (the failover orchestrator's backup on
  the new timeline) gets the same record from `Ops.Recorder` (the leader's `backup.Service`): the call
  returns once the row is written, and `RecordBase` writes a backup once, so `RoutedBackups`, which needs
  the row, finds the one `Ops` wrote.
- `ScheduledBackups` (`scheduledbackups.go`) takes the nightly backup of the projects homed on other nodes
  and runs on the leader. The timer of such a project cannot do it: `supavise backups create` writes the
  registry, which a follower's copy does not take, so a follower's `lifecycle.Timers` start nothing
  (`StartTimer` stops the timer, `lifecycle`'s `followerTimers`). A round lists the projects that are
  active and homed on another node; one whose newest completed base backup is older than `Every` (a day)
  gets the snapshot of its Storage objects and Edge Functions here, where the shared services keep them,
  and a base backup on its home through `RoutedBackups`. The timer's calendar is not read. A round can run
  for hours, so each project is read again just before its turn and left out if it was deleted, paused or
  moved to this node meanwhile, and its backup runs under a deadline of `Timeout` (two hours), so that a
  home that hangs does not hold up the projects after it. A failure leaves a `backup.failed` event and the
  project waits `Retry` (an hour). Retention is the node's prune timer's, which prunes every project of
  the registry.
- `SystemStandby` (`systemstandby.go`) adapts `lifecycle.PostgresPlane.SeedSystemStandby` to the join's
  `cluster.SeedFunc` and `Preflight`, for a server that joins before its daemon and its registry exist.
  `Joining` leaves the backend out of the preflight (the leader's settings replace it with the join).
  `cmd/supavise/cmd_node_seed.go` builds it for `node join` and `node rejoin`.
- `Router.ReconfigureService` sends the settings of GoTrue and PostgREST of a project homed elsewhere to
  its home as a `Reconfigure` (both services restart).

`internal/app/wire_placement.go` wires all of it when the mesh is up, and provides `placement.Resolver`,
`PlaneRouter`, `InstanceOps`, `BackupOps`, `Contribution` and the node's own backup timers as
`lifecycle.Timers` (for whoever starts or stops a primary on the node); a node with no cluster runs exactly
as before.

## Tests

`go test ./internal/placement/` runs against fakes: the remote plane through the handlers in one process
(every method, every status, authorization and epochs), the router, the agent's state machine (steps,
failures, resume, removal guards, actions), the timer and node-resource requests, `Ops`, `Fleet`,
`Reporter`, `ReportCache`, and the format of `promote.ok` against `internal/backup`.
`TestIntegrationReplicaBlocks` runs a seed, a stream, a PostgREST reload, a promotion, the move of the
home in the registry and a demotion on real clusters (exec backend); it counts the `pg_cron` and
`pg_net` workers separately on the primary, the standby and the promoted cluster. It needs
`SUPAVISE_TEST_UNPACKED` and runs in the linux job `replica-blocks` through
`tests/linux/replica-blocks.sh`. It turns the event
triggers that NOTIFY PostgREST off before the DDL change it makes, so that only the reload timer can make
the change visible, and checks that the change is not visible before the timer starts. One machine plays
two nodes there, with no mesh forwarders: the canonical port stands for the forwarder, and the hold on
the ports is covered by the unit tests of `internal/lifecycle`.

## Limits

- The backup handlers run the backup service of the node that is the home. A base backup there does not
  write the registry (the leader records it: `RoutedBackups`); a restore still drives the Manager of that
  node, whose registry is read-only on a follower, and is refused for a project homed elsewhere.
- A project homed on a follower is backed up once a day by the leader (`ScheduledBackups`), not at the
  calendar of `[backup] base_backup_on_calendar`; `supavise backups create` on a follower is refused by
  its read-only registry, and on the leader it takes the local data directory, which a project homed
  elsewhere does not have there.
- Create with a seed, and the optional capabilities of the Engine's plane (Postgres settings, role
  passwords, extensions, render checks), are not carried to another node.

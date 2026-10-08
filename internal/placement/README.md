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
| 422 | not allowed in the current state | `lifecycle.ErrInvalidState` |
| 501 | the node has no backup engine | `lifecycle.ErrNoSnapshot` |
| 504 | replay did not reach the position asked | `lifecycle.ErrReplayBehind` |
| 507 | the node has no room for the replica | `ErrNoRoom` |

## Node agent and peer API endpoints

`Register` (`handlers.go`) registers, through `mesh.Handle`, what the leader calls on a node: `PUT`,
`GET` and `DELETE /peer/v1/instances/{identifier}`, `POST /peer/v1/instances/{identifier}/{action}`,
`POST /peer/v1/projects/{ref}/plane/{method}` and `POST /peer/v1/projects/{ref}/backup/{op}`. Every
handler admits the cluster leader only (the client certificate's node), refuses a request under an
older epoch than the node's, and, for the plane and backup endpoints, one for a project that is not
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
  replica the node holds (another identifier of the project is refused) and refuses when the project is
  homed on this node, when the cluster here has become the primary, or when the data directory is a
  cluster without `standby.signal` (a promoted replica that is stopped does not answer), unless the
  setup it belongs to has not finished: removing a replica must never remove a home.
- `Do` runs `restart` (renders from `InstanceAction.Class` when the leader names the size, because the
  node's copy of the registry may not have it yet), `stop`, `start`, `promote` and `demote` (the failover
  orchestrator's steps 4 and 7 of a project switchover; `lifecycle.PromoteReplica` and
  `DemoteToReplica`). An action during a setup is refused. `start`, `restart` and `stop` are refused
  when the registry names this node the project's home, and so is a `promote`, except one repeated after
  the home moved here, which finds the cluster a primary and ends the replica instance. A promotion
  and a demotion take the project's ports from the mesh's forwarders while they run
  (`lifecycle.PlaneOptions.HoldPorts`; `internal/app/wire_placement.go` binds it to
  `mesh.Forwarders.Suspend`): on a replica's node the canonical ports are forwarders to the old home,
  and Postgres could not bind them.
- `StartLocal` starts the node's complete replicas after a restart; their units are not enabled for boot.

`ReportCache` keeps the observation of the node's replicas and of the projects homed there in memory and
refreshes it every 10 seconds, so that a report answers from memory and never waits for a probe (each
replica is a SQL and an HTTP probe). `Contribution` is its answer in the shape the cluster package's
reporter adds to the report it sends (`cluster.Contributor`); the cluster's reporter stores the report of
a leader too, which covers the replicas the leader holds. `Reporter` is the sender until then: it tells
the leader what the node observes every 10 seconds (`POST /peer/v1/report`) when the node is not the
leader, and logs once, not every tick, that a leader has no report endpoint (`ErrNoReportEndpoint`).

`Fleet` is the `lifecycle.ReplicaFleet` of the leader's Engine: it restarts a project's replicas through
`InstanceOps` when the project is resized.

`internal/app/wire_placement.go` wires all of it when the mesh is up; a node with no cluster runs
exactly as before.

## Tests

`go test ./internal/placement/` runs against fakes: the remote plane through the handlers in one process
(every method, every status, authorization and epochs), the router, the agent's state machine (steps,
failures, resume, removal guards, actions), `Ops`, `Fleet`, `Reporter`, `ReportCache`, and the format of
`promote.ok` against `internal/backup`. `TestIntegrationReplicaBlocks` runs a seed, a stream, a PostgREST
reload, a promotion and a demotion on real clusters (exec backend); it needs `SUPAVISE_TEST_UNPACKED` and
runs in the linux job `replica-blocks` through `tests/linux/replica-blocks.sh`. It turns the event
triggers that NOTIFY PostgREST off before the DDL change it makes, so that only the reload timer can make
the change visible, and checks that the change is not visible before the timer starts. One machine plays
two nodes there, with no mesh forwarders: the canonical port stands for the forwarder, and the hold on
the ports is covered by the unit tests of `internal/lifecycle`.

## Limits

- The backup handlers run the backup service of the node that is the home. On a follower, whose registry
  is read-only, a base backup cannot record its row.
- Create with a seed, and the optional capabilities of the Engine's plane (saved settings, role
  passwords, extensions), are not carried to another node.

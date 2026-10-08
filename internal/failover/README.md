# internal/failover

Moves one project's home, or the leadership of the whole server, to another node. With the old primary alive it is a switchover and loses nothing: the old primary stops cleanly and becomes a replica. With the old primary dead it is a failover: the old primary is fenced first, and what its replica had not received is lost. Both exist for a project and for the server, by hand (`supavise projects failover`, `supavise failover`) and, with `[failover] mode` and an AWS fencer, by the daemon.

```
supavise projects failover <ref> [--to NODE] [--force] [--dry-run] [--resume]
supavise failover [--to NODE] [--force] [--dry-run] [--resume] [--restore-missing] [--old-primary-is-down] [--yes]
GET /supavise/v1/failover/readiness
```

`contract.go` is what the CLI, the API and the other packages are written against (`Service`, `Plan`, `Check`, `Readiness`, `Fencer`, `AddressTaker`). Everything else is the orchestrator behind it, the two providers (`aws/`, `command/`), the fence record (`fenced/`) and the daemon-side parts: the automatic monitor, the boot check, the peer endpoints and the control socket.

## A move

The orchestrator never touches a database, a unit or a file of another node itself. It calls ports (below), which the daemon wires to `placement`, the mesh, `fleet` and `backup`. Every step is written to a log after it is done, and a step that is in the log is not done again, so `--resume` continues a move that stopped. Every step is also written so that doing it twice is harmless, because a crash can fall between the work and its record.

One move runs at a time on a node (`ErrBusy`). A project move holds the project's lock with the lifecycle operations (pause, resume, delete, upgrade).

### Project move

The leader runs it. The project shows `RESTARTING` while it moves. A project whose primary answers is switched over; one that does not is failed over. A project whose home node does not answer at all is a node failure: the plan refuses and names `supavise failover`.

| Switchover | What it does |
|---|---|
| `begin` | records the replica to promote, its origin and the project's status |
| `quiesce` | sets `RESTARTING` |
| `stop-old` | Realtime and the pooler let go of the database, then the old primary stops (PostgREST, GoTrue, a fast shutdown) and its final checkpoint LSN is recorded |
| `promote` | the replica replays to that LSN, then promotes (`promote.ok(epoch)`, `pg_promote`, restart as primary). If it cannot catch up while still a standby, the old primary starts again and the move ends `aborted` |
| `homed` | `SetProjectNode`, and a replica row for the old home with the same origin and a new identifier |
| `start-new` | GoTrue and PostgREST at the new home, and the backup timer |
| `tenant` | Supavisor and Realtime register the project again (Realtime recreates its slot) |
| `demote-old` | the old home becomes a replica of the new one, in place |
| `base-backup` | a base backup on the new timeline; a failure is a warning |

A failover has `begin`, `fence-old`, `promote` (drains the archive instead of waiting for an LSN), `homed`, `start-new`, `tenant`, `reseed-old` and `base-backup`. `fence-old` is the cooperative fence of that one project. `reseed-old` sets the old primary's data aside as `data.diverged-<epoch>` and asks the replica controller for a new replica there. A paused project is promoted like the others and stopped again.

### Server move

Run on the node that takes over (`--to` defaults to it). From the leader, `--to NODE` makes the leader ask that node to run a switchover and follow it (`delegate.go`); a failover and `--resume` run where the move is. The survivor's registry is a read-only standby until its system cluster is promoted, so the log is `failover.json` until then and the `moves` table after.

| Step | What it does |
|---|---|
| `begin` | the plan: the standby of the system cluster, and the node and replica of each project |
| `quiesce` (switchover) | the leader enters maintenance, stops the project clusters (8 at a time), the system GoTrue and the shared services, and last the system cluster, and reports each final LSN as `stopped:<ref>`. A failure undoes it: the leader starts again |
| `caught-up` (switchover) | the standby of the system cluster replays to the leader's last position. A failure undoes it too |
| `fence` (failover) | the cooperative fence of the old leader, then the provider's fence; a failed fence means no promotion |
| `marker` | `_node/leader.json` records the new leader and epoch. A store that already holds a higher epoch, or this epoch under another leader, ends the move `aborted`: another node was promoted. It is also what decides between two survivors that act at once; the loser stops before it touches the address |
| `address` | the service address moves to the survivor, before the promotion |
| `promote-system` | the system standby promotes |
| `leader` | the daemon becomes the leader (`Takeover`), the log moves into a `moves` row and `failover.json` is removed, `SetLeader`, the old leader is marked `fenced` after a failover, the system project is homed here, maintenance ends |
| `system-homed`, `p:<ref>:seed|promote|homed|started|tenant|done` | the projects, four at a time, in sequence order. A project with no replica on the new leader goes to the node holding its healthiest replica; with `--restore-missing` a project with no replica gets a standby seeded from the archive and drained (data loss up to `archive_timeout`) |
| `demote:<ref>` (switchover) | the old leader's clusters become replicas in place, the system cluster first; a project restored from the archive is rebuilt instead (`reseed:<ref>`) |
| `base-backups`, `dns` | base backups on the new timelines, and the DNS guidance |

Nothing is promoted before the old leader cannot write, and nothing is written to the marker before the old leader is beyond return, so a switchover that fails before then can still be undone. A project that fails does not stop the others: the move ends `failed`, names it, and a resume runs what is left.

The planned stop never touches the daemon. The WAL relay in it stays up until the cluster has stopped, because a cluster that cannot archive its last segments cannot finish its shutdown. `SetProjectNode` comes after the old primary has stopped, so the relay of the old home keeps serving until then.

## Preconditions

`PlanProject` and `PlanServer` return every check with its verdict, and `--dry-run` prints them. A failed blocking check refuses the move; `--force` overrides it unless the check is hard.

| Check | Blocking | Hard |
|---|---|---|
| the replica is a healthy standby, its lag is known and below `max_lag_seconds` | yes | no |
| same release on both nodes | yes | no |
| capacity of the target: the replica already counts against it (advice, from the project check) | no | no |
| the epoch marker store is reachable | yes | no |
| projects without a replica (unless `--restore-missing`) | yes | no |
| `[fleet] storage_backend = "s3"` | yes | yes |
| the marker does not already hold this epoch or a higher one | yes | yes |
| the target is active; the system cluster has a standby on it | yes | yes |
| no unfinished move (without `--resume`), and none to resume (with it) | yes | yes |
| a fence method: the provider's probe passes, or `--old-primary-is-down` with no provider | yes | yes |
| a leader that still answers is not asserted down when there is no fencer | yes | yes |
| only the leader moves a project; the project's home answers | yes | yes |

`Deps.Extra` adds checks the other parts own (certificates mirrored, shared-service artifacts present).

## Fencing and the address

`[failover] fencing` chooses the provider. Each implements `Fencer` and `AddressTaker`.

**aws** (`aws/`): through `internal/awsapi` with the instance role. On the survivor, before any promotion: describe the peer, `StopInstances`, poll until `stopped` for `stop_timeout_seconds`, `StopInstances` with `Force` and poll once more, then `AssociateAddress` of the service address with reassociation allowed. The private IP is the survivor's secondary address when its primary address already carries an Elastic IP of its own, else its primary address. A peer EC2 does not list is not taken for stopped. `Probe` asks with `DryRun` whether the role may do all of this, and says which `ec2:` action it may not call. An Elastic IP belongs to one region: across regions the address is left to the operator and the move prints the DNS change.

**command** (`command/`): `fence_command` and `takeover_command` run on the survivor through `/bin/sh` with `OLD_NODE`, `NEW_NODE` (names), `OLD_NODE_ID`, `NEW_NODE_ID`, `OLD_NODE_ADDR`, `NEW_NODE_ADDR`, `EPOCH` and `PLANNED`. A fence must exit 0; a timeout kills the command's process group. Without a takeover command the address is left to the operator.

**manual** (`Manual`): fences nothing. A failover needs `--old-primary-is-down`; the orchestrator tries the cooperative fence on its own first, and prints the DNS guidance at the end.

A planned switchover has nothing to fence: the clean stop takes its place. The address still moves.

## Automatic modes

`[failover] mode = "project"` lets the leader fail over a project; `"server"` adds the follower taking over a dead leader. Both need `fencing = "aws"` and a probe that passes at the start and is repeated; a node whose probe fails runs as manual, raises `failover_failed` (title "Automatic failover is off") once, and arms itself again when the probe passes. `Monitor.Tick` looks every 10 seconds. Every gate must be open:

| Gate | Server mode (the follower decides) | Project mode (the leader decides) |
|---|---|---|
| silence | the leader fails pings for `grace_seconds` | the project's primary stays `ACTIVE_UNHEALTHY` for `project_grace_seconds` |
| the public address | `GET /healthz` of the API host at the service address fails | |
| the cloud | EC2 says the leader is stopped, terminated or failing its status checks | |
| maintenance | `cluster.maintenance` is not set | same |
| cooldown | no unplanned failover started within `cooldown_minutes`, failed ones included | same |
| releases | both nodes run the same release | same |
| regions | both nodes in one region | same |
| lag | the plan passes with no `--force`: the lag of every replica needed is known and within `max_lag_seconds` | same |
| one move | none is running | same |

With three or more nodes every follower may decide at once; the marker lets one go on, and the others stop before they touch the address. A leader that merely misses pings, while its address answers or EC2 says it runs, is not stopped. Projects without a replica are restored from the archive by the automatic server mode, because refusing the whole server over one project would leave it down. A project whose home does not answer is left to the server mode. There is no automatic failback.

## The old primary's return

At boot, before the daemon lets a primary start, `BootCheck` asks the peers for the epoch and reads `_node/leader.json`. A higher epoch, or a different leader at the same epoch, fences the node: `fenced.json` is written (`fenced/`), the clusters systemd started are stopped, their launchers are removed (the unit's `ConditionPathExists` then keeps them down, also at boot), and the critical alert `fenced` is raised. With neither source reachable the node starts, because its own record shows no demotion.

The cooperative fence (`POST /peer/v1/fence`) does the same on request: the node that will lead asks, and the node records the fence first, removes the launchers and stops the primaries. A project fence (with a `ref`, in the current epoch, from the leader) does it for one project and writes `projects/<ref>/fenced.json`. The plane must not start a primary while `fenced.Blocks(paths, ref)` says so.

`SetAsideDiverged` moves a project's data to `data.diverged-<epoch>` with a `DIVERGED.json` (when, and the size of the lost tail when both LSNs are known) and clears the project's fence record; `SweepDiverged` removes what is older than `keep_diverged_days`, hourly in the daemon. A planned switchover needs no rejoin: the old leader's clusters are demoted in place.

## Ports

`ports.go` has the interfaces; `wire_failover.go` (`internal/app`) connects them.

| Port | Meaning | Implemented by |
|---|---|---|
| `Store` | the registry (the node's current one: read-only until its system cluster is promoted) | `registry.Registry` |
| `Instances` | ensure, observe, promote, demote of replica instances, the system standby included | `placement.InstanceOps` |
| `Primaries` | stop, start, health and set-aside of a project's primary on any node | `MeshPrimaries` over `LocalPrimaries` and the peer endpoint |
| `LocalPrimaries` | the same on this node | the node's plane (`wirePlacement` provides it) |
| `BaseBackups` | a base backup on a node | `placement.BackupOps` |
| `Fleet` | quiesce and re-register a project with Supavisor and Realtime | `wireFleet` provides it |
| `LocalServices` | stop and start the leader's shared services as one | `wireFleet` provides it |
| `Peers`, `Leader`, `Remote` | ping, fence, quiesce and resume the leader, delegate a switchover | `MeshPeers` over `mesh.RPC` |
| `Takeover` | the daemon runs as the leader | the daemon's role change; without one, the hook waits for the membership |
| `ReplicaSetup`, `Locker`, `ExtraChecks` | replica setup, the engine's project lock, more preflight checks | `replicas.Service`, the engine, `wireProxy` and `wireFleet` |
| `Provider` | fence and address | `aws.Provider`, `command.Provider`, `Manual` |

## Peer endpoints and the control socket

| Endpoint | Caller | What |
|---|---|---|
| `POST /peer/v1/fence` | the node that will lead | cooperative fence of the node or, with `ref`, of a project |
| `POST /peer/v1/failover/quiesce`, `.../resume` | the survivor | stop the leader for a switchover, and undo it |
| `POST /peer/v1/failover/primary/{ref}/{op}` | the leader | `stop`, `start`, `health`, `aside` on a project's primary |
| `POST` and `GET /peer/v1/failover/server` | the leader | run a switchover for it, and tell how it goes |

The CLI reaches the orchestrator through a unix socket of the daemon, `<state_dir>/system/failover/control.sock` (mode 0660), because a move needs the daemon's mesh sessions, plane and registry, and on a follower the admin port is a forwarder to the leader. `GET /v1/readiness`, `POST /v1/plan/{project,server}` and `POST /v1/run/{project,server}` (a stream of steps, one JSON object per line). A run belongs to the daemon: a CLI that goes away does not stop it.

## Readiness

`Readiness` is what `supavise status` shows and `GET /supavise/v1/failover/readiness` returns (Owners and Administrators): whether a server failover would be accepted now, for the node that would take over (this one when it follows, else the follower with the healthiest system standby). The failed blocking checks are the blockers; the rest are notes, among them the projects with no replica. The fencer probe is cached for a minute. A server with no other node answers `ErrNoCluster` (404, and no block in `status`).

## Alerts

`failover_started`, `failover_completed` and `failover_failed` announce each move (warning when it was undone and aborted, critical when it stopped). They are one-shot and the hourly cap does not hold them back. `fenced` is critical.

## Tests

`go test ./internal/failover/...` runs the orchestrator against a fake cluster (`world_test.go`) over the in-memory registry behind a gate that refuses writes while the node is a standby. Covered: every branch of both moves (a failed fence means no promotion, an epoch race, a promotion that fails halfway, partial project failures, restore of missing replicas, a third node, paused projects), a resume after a failure at each step with a fresh orchestrator, the order of the events, the preconditions and `--dry-run`, the automatic gates, the boot check, the peer endpoints, the control socket and the delegation. `aws/` runs the provider against awsapi's fake for the order stop, wait stopped, associate and promote, and for a partitioned peer that is never stopped. `pg_test.go` runs the main flows again over a Postgres registry when `SUPAVISE_TEST_DATABASE_URL` is set, as in CI.

## Limits

- The automatic modes need the AWS fencer. Without one, a failover is an operator's act.
- A project that is homed on a node that does not answer is failed over only by the server move.
- The boot check runs in the daemon's wiring, after the node opened: a cluster that systemd started before the daemon is stopped by it, not prevented.
- `supavise node rejoin` (workstream M) clears `fenced.json` and calls `SetAsideDiverged`; until it does, a fenced node stays fenced.

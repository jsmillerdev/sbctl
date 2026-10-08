# internal/failover

Moves one project's home, or the leadership of the whole server, to another node. With the old primary alive it is a switchover and loses nothing: the old primary stops cleanly and becomes a replica. With the old primary dead it is a failover: the old primary is fenced first, and what its replica had not received is lost. Both exist for a project and for the server, by hand (`supavise projects failover`, `supavise failover`) and, with `[failover] mode` and an AWS fencer, by the daemon.

```
supavise projects failover <ref> [--to NODE] [--force] [--dry-run] [--resume] [--yes]
supavise failover [--to NODE] [--force] [--dry-run] [--resume | --abort] [--restore-missing] [--old-primary-is-down] [--yes]
GET /supavise/v1/failover/readiness
```

`contract.go` is what the CLI, the API and the other packages are written against (`Service`, `Plan`, `Check`, `Readiness`, `Fencer`, `AddressTaker`). Everything else is the orchestrator behind it, the two providers (`aws/`, `command/`), the fence record (`fenced/`) and the daemon-side parts: the automatic monitor, the boot check, the peer endpoints and the control socket.

## A move

The orchestrator never touches a database, a unit or a file of another node itself. It calls ports (below), which the daemon wires to `placement`, the mesh, `fleet` and `backup`. Every step is written to a log after it is done, and a step that is in the log is not done again, so `--resume` continues a move that stopped. Every step is also written so that doing it twice is harmless, because a crash can fall between the work and its record.

One move runs at a time on a node (`ErrBusy`). A project move takes the project's lock, shared with the lifecycle operations (pause, resume, delete, upgrade), before it plans, so the status it records is the one the project has; a move that cannot get the lock leaves no row. A project move keeps the lock except around the registration with the shared services (the `tenant` step). A server move takes no project locks, and sets each project active before it registers it, for the same reason.

The plan the CLI showed is the plan that runs: `supavise failover` and `supavise projects failover` send the kind (and for a server move the epoch) they confirmed with the operator, and the daemon refuses with `ErrPlanChanged` when planning at the start of the run gives another, for instance a clean stop that has become a fence because the primary stopped answering in between. A resume sends none.

### Project move

The leader runs it. The project shows `RESTARTING` while it moves. A project whose primary answers is switched over; one that does not is failed over, and the CLI asks for the project's ref before it fences the old primary and sets its data aside (`--yes` skips the question; a switchover and `--resume` are never asked). A paused project (status `INACTIVE`) has no answering primary on purpose, so it is switched over too: its cluster is stopped already, its control file gives the position the replica must reach, and it is paused on the new home like it was. A project whose home node does not answer at all is a node failure, and the plan refuses (hard). For a home that is the leader, `supavise failover` on a survivor moves the project with the rest. For a home that is a follower no move applies (see Limits).

| Switchover | What it does |
|---|---|
| `begin` | records the replica to promote, its origin and the project's status |
| `quiesce` | sets `RESTARTING` |
| `stop-old` | Realtime and the pooler let go of the database, then the old primary stops (PostgREST, GoTrue, a fast shutdown) and its final checkpoint LSN is recorded. A stop that reports no LSN, or one that does not parse, is not recorded: the old primary starts again and the move ends `aborted` |
| `promote` | the orchestrator waits until the replica's replay is past that LSN (the checkpoint record starts there, so a replay position equal to it is one record short), then asks the node to promote (`promote.ok(epoch)`, `pg_promote`, restart as primary). If the replay does not get past it in `stop_timeout_seconds`, nothing was asked of the node: the old primary starts again and the move ends `aborted`. A promotion call that fails after the wait is not undone, because the node may still be inside `pg_promote` or its restart: the move ends `failed` with the old primary stopped, and `--resume` promotes again. The answer of the node says more. A refusal (the project is not homed there, the node is fenced, the cluster is no standby, the replica is still being set up, a standby that has not replayed as far as asked; `refusedByTheNode`) is an answer before the node changed anything: nothing was promoted, and the old primary starts again. "Stale epoch" means the node is at a higher epoch than the move: the move ends `failed` as `ErrEpochLost`, and the old primary stays down for the leader that holds the epoch to decide. "Not the leader" is asked again for about 15 seconds, because the node learns of a new leader from its registry copy, and then fails the move the same way. Any other error may have promoted. The node is asked to promote again on a resume even when it reports a primary already: its promotion is repeatable and finishes what an earlier try left, such as the restart on the canonical port |
| `homed` | `SetProjectNode`, and a replica row for the old home with the same origin and a new identifier |
| `start-new` | GoTrue and PostgREST at the new home, and the backup timer |
| `tenant` | Supavisor and Realtime register the project again (Realtime recreates its slot). The project is active again first, and the move lets go of the project's lock for this step: the engine registers a project only while it is active, and takes the same lock, which is not reentrant. The lock is taken again for the rest; a pause that got in meanwhile keeps its status |
| `demote-old` | the old home becomes a replica of the new one, in place |
| `base-backup` | a base backup on the new timeline; a failure is a warning |

A failover has `begin`, `fence-old`, `promote` (drains the archive instead of waiting for an LSN), `homed`, `start-new`, `tenant`, `reseed-old` and `base-backup`. `fence-old` is the cooperative fence of that one project: the home node records `projects/<ref>/fenced.json`, removes the launcher and stops the primary. A home that is the leader itself, the usual case, does this in its own process, because a node cannot call itself over the mesh. `reseed-old` sets the old primary's data aside as `data.diverged-<epoch>` and asks the replica controller for a new replica there. The old home refuses to set data aside while its copy of the registry still names it the home (`homed_here`), and the copy trails the leader's `SetProjectNode` by a moment, so the move asks again for about half a minute; a registry that cannot be read is a refusal too (`registry_unavailable`), never a yes. `SetAsideDiverged` refuses while the data directory's `postmaster.pid` names a live process.

### Server move

Run on the node that takes over (`--to` defaults to it). From the leader, `--to NODE` makes the leader ask that node to run a switchover and follow it (`delegate.go`); a failover, `--resume` and `--abort` run where the move is. While the leader waits for the node it asked, the leader's move slot belongs to that node: its quiesce and its resume arrive in the middle of the leader's own move and pass (`acquireForDelegate`), and nothing else gets through the slot. The survivor's registry is a read-only standby until its system cluster is promoted, so the log is `failover.json` until then and the `moves` table after.

| Step | What it does |
|---|---|
| `begin` | the plan: the standby of the system cluster, and the node and replica of each project |
| `quiesce` (switchover) | the leader enters maintenance, stops the project clusters (8 at a time), the system GoTrue and the shared services, and last the system cluster, and reports each final LSN as `stopped:<ref>`. A failure undoes it: the leader starts again |
| `caught-up` (switchover) | the standby of the system cluster replays to the leader's last position. A failure undoes it too |
| `fence` (failover) | the cooperative fence of the old leader, then the provider's fence; a failed fence means no promotion |
| `marker` | `_node/leader.json` records the new leader and epoch. A store that already holds a higher epoch, or this epoch under another leader, ends the move `aborted`: another node was promoted. It narrows the choice between two survivors that act at once: the loser stops before it touches the address. A marker that cannot be written ends the move `failed` before anything is promoted; `--force` goes on without it, and the promoted node still starts as the leader from its own `promote.ok` (below), but nothing then stops a second survivor |
| `address` | the service address moves to the survivor, before the promotion. The marker comes first, the reverse of the order in design 2.10.4 (steps 4 and 5), so that only a survivor whose marker stands reaches the address |
| `promote-system` | the system standby promotes |
| `leader` | the daemon becomes the leader (`Takeover`), the log moves into a `moves` row and `failover.json` is removed, `SetLeader`, the old leader is marked `fenced` after a failover, the system project is homed here, maintenance ends. The daemon restarts before this step runs (see "The daemon's restart"), and the daemon that starts runs it |
| `system-homed`, `p:<ref>:seed|promote|homed|started|tenant|done` | the projects, four at a time, in sequence order. A project with no replica on the new leader goes to the node holding its healthiest replica; with `--restore-missing` a project with no replica gets a standby seeded from the archive and drained (data loss up to `archive_timeout`) |
| `demote:<ref>` (switchover) | the old leader's clusters become replicas in place, the system cluster first; a project restored from the archive is rebuilt instead (`reseed:<ref>`). The old leader's daemon restarts once its system cluster is a standby, so the move waits until it answers over the mesh, and the projects wait until it reports the new epoch and leader (its copy of the registry has caught up); three minutes, then the step fails and `--resume` runs it again |
| `base-backups`, `dns` | base backups on the new timelines, one step per project (`base-backup:<ref>`) so that a project that finishes on a later resume gets its own, and the DNS guidance |

Nothing is promoted before the old leader cannot write, and nothing is written to the marker before the old leader is beyond return, so a switchover that fails before then can still be undone. A project that fails does not stop the others: the move ends `failed`, names it, and a resume runs what is left.

### The daemon's restart

A daemon takes a new role by stopping and starting again (`app.ErrRoleChanged`; the boot decision opens the registry writable, binds the leader's ports and starts the shared services), and it does so when its node's system cluster is promoted or demoted. A server move therefore cuts its own run once, between the promotion of the system cluster and the end of the move, and the cut is not a failure:

- The run ends with `ErrRestarting` and not with `failed`: the move stays `running` in `failover.json` and no `failover_failed` alert goes out. This applies to a server move whose context ends after its leader marker was written; before that, a cut run fails like any other.
- The daemon that starts continues the move. The daemon's wiring does it as soon as the shared services are up (it calls `FailoverServer` with `Resume`), because the move registers the projects with them again. `Monitor.Run` is the fallback for a wiring that does not: after three minutes (`resumeFallback`), whatever the mode, it continues the move to this node that is still `running` and passed its marker, when this node leads (`ResumeInterrupted`), and leaves alone what has been finished or is being run. A move that ended `failed` is the operator's (`--resume`) and is not started again by the fallback.
- The CLI follows. A connection that closes, or an `ErrRestarting` answer, makes `supavise failover` wait for the daemon (`Client.Follow`, `GET /v1/follow?epoch=N`) and go on printing the steps of the continued run. A daemon keeps no run in memory across a restart and reads the move from its log: `failover.json` while the system cluster is not promoted, the moves table after, which a daemon that restarted as a follower replicates, and that is how the old leader's CLI ends after a switchover it started with `--to`. Whoever continues the move, the wiring or the fallback, the log is what is followed.
- The leader that delegated the switchover waits for the node it asked: its looks fail while the node's daemon is down, and the daemon answers from the move's log once it is up (to the node the move leaves, and to no other); that is waited out for about three minutes.
- The membership shows the promotion before the daemon has restarted (it reads the epoch from the node's `promote.ok`), so the `Takeover` wait can end in the old process, and the next step writes to the registry handle of a standby, which refuses writes. A server move that meets `registry.ErrReadOnly` after its marker is cut the same way as one whose context ends: it returns `ErrRestarting`, stays `running`, and the daemon that starts continues it. The daemon that starts records the leadership (`cluster.AssumeLeadership`, from the epoch the boot decision settled on), so the wait ends at once there.
- If the daemon does not restart by itself, the move waits: restart supavise on that node, and the move continues at start.

The boot decision settles the epoch from the cluster row, the peers, the leader marker and the node's own `promote.ok`, which the promotion writes before `pg_promote`. A promoted node whose registry still names the old leader and that none of them names starts fenced, so the promotion must have written `promote.ok` before the daemon restarts; the marker adds the arbiter between two survivors.

The old leader of a switchover is spared the same way. It stopped its system cluster on purpose, and the survivor's first ping and the marker say that another node leads at the next epoch. `FenceOnHigherEpoch` leaves a leader alone when its quiesce record names that node and epoch.

### Undoing a move that stopped early

A failed server move leaves `failover.json`, which blocks every new plan on the node and the automatic mode (the check "unfinished move"). `supavise failover --abort` discards it when nothing has happened that cannot be taken back: the log holds only `begin`, `quiesce`, `stopped:<ref>` and `caught-up`. For a switchover that had stopped the leader it asks the leader to start again first, and it keeps the file if that fails; a leader that has nothing to undo (`no_quiesce`) is the state wanted. It refuses once the old leader was fenced, the marker was written, or the system cluster was promoted: those continue with `--resume`. It takes no other option and runs where the move is. A project move has no such file; its unfinished move is a row of the moves table that `--resume` finishes.

Retiring the leader is a move first: `supavise failover` makes another node the leader, and only then can `supavise node rm` or `node join --reset` take the old one out.

The leader keeps what it needs to answer a survivor that asks twice, and to undo the stop, in `<state_dir>/failover-quiesce.json` (0600): the node and epoch the quiesce is for, the project clusters it stops (listed before the first stop) and, once every cluster has stopped, where each stopped. The registry cannot hold it, because the quiesce stops the system cluster. A repeated request answers from the file, a request for another switchover is refused (`quiesce_pending`), and `resume` is authorized by it. A quiesce holds the leader's move slot while it stops clusters: a move of the leader's own is refused (`ErrBusy`), a quiesce that finds one running is refused (`busy`), and a repeated request or a `resume` that arrives during the stops waits for them and then answers from the record. The record ends with the undo, or when the cluster has reached its epoch, or after an hour like the maintenance announcement.

The planned stop never touches the daemon. The WAL relay in it stays up until the cluster has stopped, because a cluster that cannot archive its last segments cannot finish its shutdown. `SetProjectNode` comes after the old primary has stopped, so the relay of the old home keeps serving until then.

## Preconditions

`PlanProject` and `PlanServer` return every check with its verdict, and `--dry-run` prints them. A failed blocking check refuses the move; `--force` overrides it unless the check is hard.

| Check | Blocking | Hard |
|---|---|---|
| the replica is a healthy standby, its lag is known and below `max_lag_seconds` | yes | no |
| same release on both nodes | yes | no |
| capacity of the target: the replica already counts against it (advice, from the project check) | no | no |
| the leader marker can be read from the backup store | yes | no |
| projects without a replica (unless `--restore-missing`; the automatic server mode never passes it) | yes | yes |
| `[fleet] storage_backend = "s3"` | yes | yes |
| the marker does not already hold this epoch or a higher one; an unreadable marker is reported with what to do about a malformed one | yes | yes |
| the target is active; the system cluster has a standby on it | yes | yes |
| no unfinished move (without `--resume`; `--abort` discards one that stopped early), and none to resume (with it) | yes | yes |
| a fence method: the provider's probe passes, or `--old-primary-is-down` with no provider | yes | yes |
| a leader that still answers is not asserted down when there is no fencer | yes | yes |
| only the leader moves a project; the project's home answers | yes | yes |

`Deps.Extra` adds checks the other parts own (certificates mirrored, shared-service artifacts present).

## Fencing and the address

`[failover] fencing` chooses the provider. Each implements `Fencer` and `AddressTaker`.

**aws** (`aws/`): through `internal/awsapi` with the instance role. On the survivor, before any promotion: describe the peer, `StopInstances`, poll until `stopped` for `stop_timeout_seconds`, `StopInstances` with `Force` and poll once more, then `AssociateAddress` of the service address with reassociation allowed. The private IP is the survivor's secondary address when its primary address already carries an Elastic IP of its own, else its primary address. A survivor whose only private address carries an Elastic IP of its own is left alone: the association would take that address from it, so the address is left to the operator (`ErrNoTakeover`) and the move prints the DNS change. A peer EC2 does not list is not taken for stopped. `Probe` asks with `DryRun` whether the role may do all of this, and says which `ec2:` action it may not call; the association is asked with the secondary private address the move will use, because a policy that allows the instance and the address but not the survivor's network interface denies exactly that call. `ProbeTakeover` asks whether the survivor has an address to spare (`AddressProber`); the permissions stay green for a survivor that has none, and the automatic mode stays off with the reason. An Elastic IP belongs to one region: across regions the address is left to the operator and the move prints the DNS change.

**command** (`command/`): `fence_command` and `takeover_command` run on the survivor through `/bin/sh` with `OLD_NODE`, `NEW_NODE` (names), `OLD_NODE_ID`, `NEW_NODE_ID`, `OLD_NODE_ADDR`, `NEW_NODE_ADDR`, `EPOCH` and `PLANNED`. The environment is a short list of the daemon's own (`PATH`, `HOME`, `LANG`, `LC_ALL`, `TZ`, `TMPDIR`, `AWS_REGION`, `AWS_DEFAULT_REGION`) and the move: the daemon's environment can hold the master key, and the command is the operator's script. A fence must exit 0; a timeout kills the command's process group. Without a takeover command the address is left to the operator.

**manual** (`Manual`): fences nothing. A failover needs `--old-primary-is-down`; the orchestrator tries the cooperative fence on its own first, and prints the DNS guidance at the end.

The cooperative fence of a failover goes first and waits for its answer, except when the provider says what the cloud knows and the cloud says the old leader is stopped or terminated: nothing there hears a request, and waiting would add the timeout to the time the survivor takes to lead. The step then records "skipped".

A planned switchover has nothing to fence: the clean stop takes its place. The address still moves.

## Automatic modes

`[failover] mode = "project"` lets the leader fail over a project; `"server"` adds the follower taking over a dead leader. Both need `fencing = "aws"` and a probe that passes at the start and is repeated; a node whose probe fails runs as manual, raises `failover_auto_off` (title "Automatic failover is off") once, says so again when the probe passes, and arms itself again. It is a condition, not an announcement: the hourly cap holds it. `Monitor.Tick` looks every 10 seconds. Every gate must be open:

| Gate | Server mode (the follower decides) | Project mode (the leader decides) |
|---|---|---|
| silence | the leader fails pings for `grace_seconds` | the project's primary stays `ACTIVE_UNHEALTHY` for `project_grace_seconds` |
| the public address | `GET /healthz` of the API host at the service address fails | |
| the cloud | EC2 says the leader is stopped, terminated or failing its status checks | |
| maintenance | `cluster.maintenance` is not set | same |
| cooldown | no unplanned failover of the server started within `cooldown_minutes`, failed ones included (a failover of one project does not count) | no unplanned failover of this project, and none of the server, started within `cooldown_minutes`; another project's failover does not hold it back |
| releases | both nodes run the same release | same |
| regions | both nodes in one region | same |
| lag | the plan passes with no `--force`: the lag of every replica needed is known and within `max_lag_seconds` | same |
| one move | none is running | same |

With three or more nodes the followers take turns: the first by node id acts after the grace period, the next a grace period later, and so on, so that the one that acts first leads before the others look again. The epoch marker narrows the window when two still act at once: it is read before the write and again after it, and the node whose marker is not the one in the store stops before it touches the address. On a store that ignores conditional writes the window stays open between the second survivor's read and the first survivor's read-back; the turns and the mutual fence are what keep it small. A leader that merely misses pings, while its address answers or EC2 says it runs, is not stopped. The plan pings the leader once more; a leader that answers by then is left alone (the plan would be a switchover). The automatic server mode never restores a project from the archive: a project without a replica has an unknown lag, and a restore loses up to `archive_timeout` of its writes, which an operator chooses with `supavise failover --restore-missing`. While a project has no replica the plan refuses the whole server and the monitor says why; the readiness block notes it. In project mode the leader plans the due projects in registry order and starts the first that passes every gate, so a project that cannot move (no replica, a replica behind, a home that does not answer) does not hold back the ones after it; one move starts per look, and a look that starts none names the projects it refused. A project whose home does not answer is left to the server mode. There is no automatic failback.

## The old primary's return

At boot, before the daemon lets a primary start, `BootCheck` asks the peers for the epoch and reads `_node/leader.json`. A higher epoch, or a different leader at the same epoch, fences a node that led according to its own registry: `fenced.json` is written (`fenced/`), the clusters systemd started are stopped, their launchers are removed (the unit's `ConditionPathExists` then keeps them down, also at boot), and the critical alert `fenced` is raised. A node that does not lead is only checked for its record: a follower that was down while the leader changed has a registry copy that is behind, and nothing it homes moved, so fencing it would take its projects offline and set all its data aside at the rejoin. With neither source reachable the node starts, because its own record shows no demotion. A fence record that cannot be written (a full disk) does not leave the primaries running: they are stopped and their launchers removed, and the error is returned.

A leader that is already running learns of a replacement from its peers or the marker while it runs. `FenceOnHigherEpoch(ctx, source, epoch, leader)` is the boot verdict for that case (invariant I1): a caller that has seen a higher epoch hands it over, and a leader that is shown to be replaced fences itself the same way; a follower, or a leader that saw nothing newer, is left alone. The daemon takes both verdicts earlier and elsewhere: `cluster.DecideBoot` decides before the node opens (a fenced node runs `serveFenced` and never reaches this package), and `Live.ObserveEpoch` fences a running leader. `BootCheck` and `FenceOnHigherEpoch` are the orchestrator's equivalents, and by the time the hook runs they repeat what the boot decision found.

The cooperative fence (`POST /peer/v1/fence`) does the same on request: the node that will lead asks, and the node records the fence first, removes the launchers and stops the primaries and the shared services (`Deps.FenceNode`, the membership layer's `cluster.FenceLocal`, does all of it when the daemon wires it). The request names the epoch after the node's own, so a survivor whose registry copy lags is refused (the answer says `fenced: false` and the node's epoch), and it comes from an active node that holds a standby of the system cluster in this node's registry; a registry that cannot be read cannot say, and the request is served, because the cooperative fence is the polite half of a fence and the provider's is the one that counts. What is left open is that any such node can make a healthy leader fence itself by claiming the next epoch: that is inherent to a fence that a node which is not yet the leader asks for. The record is `<state_dir>/fenced.json`, the file the membership layer reads (`cluster.ReadFenced`), and carries the peers' addresses (`cluster.PeersOf`) so that `supavise node rejoin` finds the leader when the node's own registry is stopped. A project fence (with a `ref`, in the current epoch, from the leader) does it for one project and writes `projects/<ref>/fenced.json`. The stops run to the end when the caller gives up. A `ref` from a peer must be `system` or a 20-letter project ref before it builds a path, in the handlers and in `fenced` and `SetAsideDiverged`. The plane must not start a primary while `fenced.Blocks(paths, ref)` says so.

A peer's `aside` request moves a project's data directory only for a project the node's registry does not home on this node (409 `homed_here` otherwise): the leader homes the project on the replica's node before it asks.

`SetAsideDiverged` moves a project's data to `data.diverged-<epoch>` with a `DIVERGED.json` (when, and the size of the lost tail when both LSNs are known) and clears the project's fence record; `SweepDiverged` removes what is older than `keep_diverged_days`, hourly in the daemon. A planned switchover needs no rejoin: the old leader's clusters are demoted in place.

## What the fence depends on in other packages

The fence record is a file; it keeps a node down only if the parts that start primaries and answer clients honor it.

| Part | Must |
|---|---|
| the plane (`lifecycle`, workstream C) | call `fenced.Blocks(paths, ref)` before it renders or starts a primary, at boot and on a restart by the engine's health recovery; `LocalPrimaries.Stop` must also keep that recovery from starting the stopped cluster again |
| membership (`cluster`, workstream M) | read `fenced.json` at every look, not only at boot (`Live` takes the role `fenced` from the record the cooperative fence writes, and restarts the daemon into `serveFenced`); treat a system cluster that does not answer as not a primary, so that the old leader of a planned switchover is not fenced by the new leader's first ping; probe the recovery state even when the registry cannot be read, so that a leader whose system cluster was demoted in place restarts as a follower; take a system cluster as promoted only when it answers on the system port, after the restart on the canonical port that `PromoteReplica` ends with; keep admitting a node the registry marks fenced for `GET /peer/v1/ping`, so the boot check of a returning node has more than the backup store |
| the proxy (workstream G) | answer 503 for the projects of a fenced node |

## Ports

`ports.go` has the interfaces; `wire_failover.go` (`internal/app`) connects them. A port that no hook provides turns its part of a move into a no-op, which `New` logs by name (`Deps.Gaps`) and readiness notes where a service is left broken.

| Port | Meaning | Provided by |
|---|---|---|
| `Store` | the registry (the node's current one: read-only until its system cluster is promoted) | `registry.Registry` |
| `Instances` | ensure, observe, promote, demote of replica instances, the system standby included | `placement.InstanceOps` |
| `Primaries` | stop, start, health and set-aside of a project's primary on any node | `MeshPrimaries` over `LocalPrimaries` and the peer endpoint |
| `LocalPrimaries` | the same on this node; `Stop` returns `pg_controldata`'s latest checkpoint location and an error when it cannot read it, and keeps the engine's health recovery from starting the cluster again | to be provided by the wiring over the node's plane and timers (`wirePlacement` does not) |
| `BaseBackups` | a base backup on a node | `placement.BackupOps` |
| `Fleet` | quiesce and re-register a project with Supavisor and Realtime | to be provided by the wiring: `fleet.Fleet.QuiesceTenant`, and an `EnsureTenant` over the engine, which builds the tenant spec (`wireFleet` provides `fleet.Fleet`, a different type) |
| `LocalServices` | stop and start the leader's shared services as one | to be provided by the wiring over `fleet.Manager` |
| `Peers`, `Leader`, `Remote` | ping, fence, quiesce and resume the leader, delegate a switchover | `MeshPeers` over `mesh.RPC` |
| `Takeover` | the daemon runs as the leader | `membershipTakeover` in the hook, which waits for the membership to show the node as the leader at the epoch |
| `ReplicaSetup` | replica setup | `replicas.Service` |
| `Locker` | the engine's project lock | to be provided by the engine (its lock is not exported) |
| `ExtraChecks` | more preflight checks | to be provided by the proxy (certificates mirrored) and `fleet` (artifacts present) |
| `FenceNode` | the node's whole local fence | `cluster.FenceLocal` over the node's supervisor |
| `Provider` | fence and address | `aws.Provider`, `command.Provider`, `Manual` |

## Peer endpoints and the control socket

| Endpoint | Caller | What |
|---|---|---|
| `POST /peer/v1/fence` | the node that will lead | cooperative fence of the node or, with `ref`, of a project |
| `POST /peer/v1/failover/quiesce`, `.../resume` | the survivor | stop the leader for a switchover, and undo it |
| `POST /peer/v1/failover/primary/{ref}/{op}` | the leader | `stop`, `start`, `health`, `aside` on a project's primary |
| `POST` and `GET /peer/v1/failover/server` | the leader | run a switchover for it, and tell how it goes |

The CLI reaches the orchestrator through a unix socket of the daemon, `<state_dir>/system/failover/control.sock` (mode 0600 in a 0700 directory), because a move needs the daemon's mesh sessions, plane and registry, and on a follower the admin port is a forwarder to the leader. `GET /v1/readiness`, `POST /v1/plan/{project,server}`, `POST /v1/run/{project,server}` (a stream of steps, one JSON object per line) and `GET /v1/follow?from=N&epoch=E` (the run the daemon kept, or the server move of that epoch in the registry, as a `ServerStatus`). A run belongs to the daemon: a CLI that goes away does not stop it, and a CLI that is connected and does not read does not stall it (a step that finds the buffer full is in the log only). Without a socket the client says the server is not part of a cluster or supavise is not running; without permission, that the socket is for root and the user supavise runs as.

## Readiness

`Readiness` is what `supavise status` shows and `GET /supavise/v1/failover/readiness` returns (Owners and Administrators): whether a server failover would be accepted now, for the node that would take over (this one when it follows, else the follower with the healthiest system standby). The failed blocking checks are the blockers; the rest are notes, among them the projects with no replica. On the leader, which is this node and answers, the plan is a switchover to that follower and needs no fencer. The fencer probe is cached for a minute, and a probe that fails is a blocker even on a leader that answers (the plan is a switchover then and never asks the fencer, but a failover would be refused); a cluster with no fencer configured has none to fail. With `mode = "server"` and projects without a replica, a note says the automatic mode will not run. A server with no other node that has not left answers `ErrNoCluster` (404, and no block in `status`); a fenced or joining node still counts, so the new leader of a failover keeps its block while the old one is fenced, and so does the route on a server whose daemon built no orchestrator. A port that is not wired adds a note (Realtime and the pooler will not be re-registered; the shared services will keep running through a planned switchover). A marker that cannot be read says whether the store is down or the marker is malformed (`_node/leader.json` is then a read error on every node until an operator who has checked which node leads deletes it).

## Alerts

`failover_started`, `failover_completed` and `failover_failed` announce each move (warning when it was undone and aborted, critical when it stopped). They are one-shot and the hourly cap does not hold them back; `failover_auto_off` is a condition with a recovery message and stays under the cap. `fenced` is critical. A server move that the daemon's restart cut and the new daemon continued announces `failover_started` again (its text says it continues the move), and `failover_completed` or `failover_failed` once it ends; the cut itself raises nothing.

## Tests

`go test ./internal/failover/...` runs the orchestrator against a fake cluster (`world_test.go`) over the in-memory registry behind a gate that refuses writes while the node is a standby. Covered: the leader as the home of a project and the mesh refusing a call to itself (the fake peers do), the quiesce record with the registry stopped, every branch of both moves (a failed fence means no promotion, an epoch race, a promotion that fails halfway, partial project failures, restore of missing replicas, a third node, paused projects), a resume after a failure at each step with a fresh orchestrator, the order of the events, the preconditions and `--dry-run`, the automatic gates (a due project that cannot move does not hold back the next), the boot check and the runtime fence, the peer endpoints (a quiesce in flight against a repeat, a resume and a leader move), the control socket, the delegation (including the survivor's quiesce and resume reaching the leader that delegated), the move cut by the daemon's restart and continued by the daemon that starts, the wait for the old leader, the abort, the fence's epoch and claimant rules, and the plan's parallel reads of the replicas. `aws/` runs the provider against awsapi's fake for the order stop, wait stopped, associate and promote, for a partitioned peer that is never stopped, and for the association the probe asks about. `launcher_test.go` compares the unit's `ConditionPathExists` with the launcher path the engine renders, which is what the hard stop of a fence depends on; the systemd-level behavior (the unit refusing to start without the file, a restart by systemd) is covered only by the two-server scenarios. `pg_test.go` runs the main flows again over a Postgres registry when `SUPAVISE_TEST_DATABASE_URL` is set, as in CI.

## Limits

- The automatic modes need the AWS fencer. Without one, a failover is an operator's act.
- A project homed on a follower that does not answer is failed over by nothing. A project move fences the home through the mesh and needs it to answer, a server move moves only the projects homed on the old leader, and the monitor watches only the leader. The project stays down until its node returns; its replica holds the data, and the backup store has its WAL. Getting it to serve before then is by hand. A fence that can reach a dead node needs a node-level fenced state and an epoch bump, which no part of this package does.
- The old leader's system cluster is demoted through the node's agent (`Instances.Do` with `demote`), which reads the project row and the project's home from the node's registry, and the old leader's registry is the system cluster it stopped for the switchover. The demotion works only where the agent can render the system standby without that registry, or after the daemon has read it again. Until then the step `demote:system` fails with a note, the new leader runs, and the old leader's clusters stay stopped; nothing is lost. The old leader's daemon also has to notice that its system cluster is a standby and restart as a follower, which `Live` does only once it can read the registry. The way out is a restart of supavise on the old leader and `supavise failover --resume`, or a rebuild of the node (`supavise node rm`, then `supavise node join --reset` with a token).
- A node that led and reboots before its registry copy has replayed a planned switchover reads itself as the leader the cluster has replaced, and is fenced; `supavise node rejoin` brings it back.
- The boot check runs in the daemon's wiring, after the node opened: a cluster that systemd started before the daemon is stopped by it, not prevented.
- The Management API asks the orchestrator through `api.FailoverSource`, which the daemon passes in when the node belongs to a cluster; the readiness route answers 404 on a server whose daemon built none.
- A project move runs one at a time on the leader, and the project's lock is the engine's only when the wiring gives the orchestrator a `Locker`; without one, a pause, a resume or an upgrade of the same project is not kept apart from the move.
- A project that is restored from the archive in a server move (`--restore-missing`) loses up to `archive_timeout` of writes; the old leader's copy of it is rebuilt, not demoted.
- `supavise node rejoin` (workstream M) clears `fenced.json` and calls `SetAsideDiverged`; until it does, a fenced node stays fenced.

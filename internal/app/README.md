# internal/app

Composition: the pieces that cannot import each other (`backup` needs the lifecycle `Manager`,
`lifecycle` needs a base backuper, `api` and `proxy` need both) are joined here, and `supavise serve`
is the daemon that `supavise.service` runs.

- `LifecycleOptions(cfg, Options)` is `lifecycle.OpenOptions` with the backup package wired in:
  every cluster archives WAL through `backup.ArchiveCommand` with the configured
  `archive_timeout`, deleting a project takes the final base backup through the backup service
  (`FinalBackup`), and a restore reaches the Engine (`SetManager`). The backup service is built on
  first use, so commands that never back up do not open the backend. Every command that creates or
  deletes projects (`supavise projects`, `system`, `backups`, `serve`) goes through it.
- `NewBackupService` builds the service over the configured backend and the registry.
- `PGMetaCryptoKey` is the passphrase shared by supavise-pgmeta (its `CRYPTO_KEY`) and the Management
  API: `[api] pgmeta_crypto_key`, else the sealed system secret `pgmeta_crypto_key`, created on
  first use. Whoever renders the `supavise-pgmeta` unit must pass exactly it.
- `Serve(ctx, cfg, Options)` is the daemon: it opens the node (registry in the system cluster,
  master key, Engine), runs `Engine.Recover` (statuses a crash left behind), creates the pg-meta
  key, builds the Management API (`api.NewServer`, the Postgres store) and the edge proxy
  (`proxy.New`, `KeySource` = the Engine, no-op `Waker`), listens for the API on the loopback
  admin address and at `api.<domain>` through the proxy, starts the shared services (below) and
  every active project one at a time next to the listeners (the system project's backup timer and
  the prune timer too, on systemd). `ctx` ending (SIGTERM) first drains the API (`api.Server.Drain`: new lifecycle operations
  answer 503, running ones, including creates and a delete's final backup, finish, bounded by
  `StopBudget`, 10 minutes; `supavise.service` allows 660 s to stop). The edge proxy keeps serving
  project, `api.<domain>` and `studio.<domain>` traffic during the drain and stops only after it
  returns (`superviseStop`); then the admin listener and the registry close; project units belong to systemd and keep running. At boot it
  waits up to 2 minutes for the registry (`lifecycle.ErrRegistryUnreachable`), resumes projects
  whose restart was cut off after the pause (`Engine.ResumeRecovered`), and keeps finishing restored
  clones tagged `restore.cleanup_pending` (`backup.Service.FinishPendingRestores`). `StartActive`
  re-reads each project after taking its lock, so an API pause or delete that lands between the
  listing and the start is not undone. The system project must exist (`supavise system init`).

- **Health and alerts.** `Serve` builds one `health.Monitor` over `health.CheckNode` (`health.ForNode` with `InDaemon`, and a `fleet.Lazy` of its own for the tenant checks) and hands it to the Management API (`api.Deps.Health`: `GET /healthz`, `GET /healthz/detail`). It makes an `alerts.Notifier` the package default (`alerts.Notify` for the rest of the node) and runs an `alerts.Checker` next to the listeners: conditions from the health report every minute, the daily update check, and nothing while an upgrade or a maintenance window is on (`internal/health/README.md`, `internal/alerts/README.md`). The notifier exists before the node opens, and `lifecycle.OpenOptions.UpgradeNotify` is the Engine's hook for a project's upgrade events (`upgrade_alerts.go`: `upgrade_started`, `upgrade_succeeded`, `upgrade_failed`, each delivered on a goroutine of its own, so a slow webhook never holds up an upgrade, and waited for when the daemon stops); the upgrades that `Recover` closes at start are told too.
- **The shared services.** On a systemd node `Serve` calls `fleet.Setup` with `Start` set next to the
  projects (`startFleet`): it generates the services' sealed secrets on first use, renders the
  units, starts postgres-meta, Supavisor, Realtime, Storage and Studio in order and waits for each,
  so a reboot brings the whole node back from `supavise.service` alone (the units are not enabled for
  boot). The Engine registers, re-keys and removes tenants (Supavisor, Realtime, Storage) on project
  create, rotate-keys and delete through the same `fleet.Setup`, built lazily
  (`cmd/supavise/serve_fleet.go`: a `fleet.Lazy`, bound to the registry and master key right after
  `lifecycle.Open`, `Options.BindFleet`), so the daemon and the CLI register tenants the same way.
  A service whose artifact was never fetched fails at boot and is logged; the rest of the node
  comes up. The installer fetches the artifacts with `supavise fleet start` first. A service whose
  files changed (a release moved its pin) is restarted, one at a time, each waited for; while a
  `supavise upgrade` runs the first failure ends the roll (`fleet.Deps.HaltOnFailure`). When the
  services and `startProjects` are both done, `ensureTenants` registers every active project with
  the shared services again (`Engine.EnsureTenants`), which runs a new Storage or Realtime
  release's tenant migrations; with nothing changed it sends nothing.
- **The WAL relay.** With `[backup] wal_relay` on (the default under systemd) `Serve` starts
  `app.StartWALRelay` before it opens the registry and stops it after the lifecycle drain; it serves
  one unix socket per project through which the clusters archive and restore WAL without holding
  any backend credential (`internal/backup/README.md`, "The WAL relay"). The lifecycle engine
  tells it about a project before the cluster starts (`Options.ArchiveReady`). The same function
  serves the sockets nobody answers for a command-line process that waits for WAL
  (`supavise backups ...`, `projects delete`) while the daemon is down.

- **The role, decided before the node opens.** `Serve` first asks `cluster.DecideBoot` (`decideBoot` in `wire_mesh.go`) what this node is: the leader, a follower or fenced (`internal/cluster/README.md`, "Boot"). A server whose `/etc/supavise/cluster` holds no `node.crt` is a single server and gets the leader's role without a look at the database, the peers or the backup store, so it starts exactly as before. A follower opens its registry read-only (`registry.OpenReadOnly`, no `Migrate`, no advisory locks), does not bind the admin listener (that port is a forwarder to the leader's), and starts no Management API of its own. A fenced node, or one that was removed from its cluster and has not joined another, runs `serveFenced` instead of the daemon: it records the fence in `fenced.json`, raises the critical `fenced` alert, stops every project and shared-service unit and removes the run scripts (`cluster.FenceLocal`), and answers 503 with the reason on `[listen] http` until `supavise node rejoin` removes the record. A role that changes while the daemon runs (the system cluster is promoted or demoted, the node is fenced, a single server is given a cluster identity by `supavise node token`) ends `Serve` with `ErrRoleChanged`; systemd starts it again and the boot decision opens the node in the new role, so no part of the daemon switches roles in place. `openFollower` is the one place `lifecycle.OpenOptions` receives the read-only option (`ReadOnly`, and the standby's socket). A follower also skips what writes the registry or belongs to the leader: the branch expiry sweeper, the sweep of `functions dev` stand-ins, the system project's backup timers and the re-render of the system cluster's unit (a standby's unit is not a primary's).
- **Hooks.** The cluster features join `Serve` through hooks, one file each (`wire_mesh.go`, `wire_placement.go`, `wire_fleet.go`, `wire_replicas.go`, `wire_proxy.go`, `wire_failover.go`, `wire_storagemigrate.go`), run by `wire.go` in that order after the node opens and before the Management API and the edge proxy are built. A hook gets a `Wire`: the config, the logger, the open node, and the `api.Deps` and `proxy.Options` the two servers are built from, so it can set the collaborators it owns. It registers peer API handlers with `mesh.Handle`, starts background work with `w.Go` (the work runs in the daemon's group and its error stops the daemon), registers cleanups with `w.OnStop`, and passes what later hooks need with `Provide[T](w, v)`, read with `Get[T](w)`. `T` is the interface the consumer asks for, so a mesh is provided as `Provide[mesh.Mesh](w, m)`. Before any hook runs `Wire` already provides `cluster.Membership` (the founder node, leading at epoch 1) and `placement.Resolver` (over the registry); `Serve` also provides the boot decision (`cluster.BootDecision`), the node's `*health.Monitor` and, when the backend opens, the `*backup.Service`. `wireMesh` replaces the membership with a `*cluster.Live` on a server that belongs to a cluster, and provides `mesh.Mesh`, `*mesh.Forwarders`, `*cluster.Authority`, `*cluster.Reports` and `*cluster.Reporter` (a hook adds its observations to the node's report with `Reporter.Add`); on a single server it only watches for the cluster identity to appear. `mesh.DefaultMux` is reset when the hooks start, so a process that starts the daemon twice registers each peer endpoint once. A hook that has nothing to do returns nil, so a node that does not use the cluster features starts as it always did; `notimpl.Err` from a hook is skipped and logged at debug level.
- **Ports: provided, or off with a reason.** The packages of the cluster work meet at interfaces that one declares and another implements (`failover.Fleet`, `replicas.Pooler`, `placement.InstanceOps`, ...), connected here with `Provide` and `Get`. A `Get` that finds nothing would leave the feature to do nothing without a word, so `clusterPorts` (`wire_ports.go`) lists every port a node with a cluster identity must have, and `apiPorts` the `api.Deps` fields it must set. A hook either provides a port or calls `Wire.Off(feature, reason)`, which logs a warning with the reason and records it; `Wire.run` logs an error for each port that is neither, and `TestEveryClusterPortIsProvidedOrOff` fails for it. A single server has no cluster, so nothing is required of it and nothing is provided. What the backup service must implement for the cluster work (`EpochMarkerStore`, `ReplicaSeeder`, `BaseBackupEnsurer`) is asserted at compile time. Which hook provides what:

  | Hook | Provides |
  |---|---|
  | `wireMesh` | `mesh.Mesh`, `cluster.Membership` (a `*cluster.Live`), `*cluster.Reporter`, `*cluster.Reports`, `*mesh.Forwarders`, `*cluster.Authority` |
  | `wirePlacement` | `placement.Resolver`, `PlaneRouter`, `InstanceOps`, `BackupOps`, `Contribution`, `lifecycle.Timers`, `failover.LocalPrimaries` |
  | `wireFleet` | `fleet.Fleet`, `fleet.PeerRefresher`, `replicas.Pooler`, `failover.Fleet`, `failover.LocalServices`, a server check for the shared services' artifacts |
  | `wireReplicas` | `replicas.Service`, `Remover`, `ReportSink`; sets `Deps.Replicas` and `Deps.Placement`; subscribes the controller to the reports |
  | `wireProxy` | `*proxy.CertRole`; sets `proxy.Options.Cluster` and `Deps.LoadBalancers`; serves `GET /peer/v1/certs`; a server check that the mirrored certificates are the leader's |
  | `wireFailover` | `failover.Service`, `Locker`, `ExtraChecks`, `Takeover`; sets `Deps.Failover` |

  The ports that need something this tree does not offer say so when the node starts: `failover.LocalServices` on a supervisor that is not systemd, `lifecycle.Timers` where there are no backup timers, `failover.Marker` while the backup store does not open, and the replica controller itself while the node cannot reach the others or take base backups.
- **The peer API has one owner per endpoint.** `mesh.Handle` panics on a pattern registered twice, which stops a node at start. `cluster.PeerAPI` registers the membership endpoints through `registerPeerAPI`, which leaves out the patterns in `peerAPIServedByOthers` (`GET /peer/v1/certs`, which is the proxy's `CertsHandler` and answers the `etag` query with a 304); `TestNoPeerEndpointIsRegisteredTwice` runs every hook and looks at the mux.
- **What a node reports.** `cluster.Reporter` is the node's one sender of `POST /peer/v1/report`; `wirePlacement` adds the replicas' state and the health of the projects homed here with `Reporter.Add`. On the leader the report goes to `cluster.Reports`, and `wireReplicas` subscribes the replica controller (`reportIntake`: the latest report of each node, handed over on a goroutine of its own, because the subscriber runs on the peer API's request and the controller reads the registry). The node of a report is the one the certificate names: `Reports.Put` takes it from the connection, never from the body, and the peer API answers 403 to a body that names another node.
- **A server move spans two processes.** The survivor of a server move is a follower, so its daemon has a read-only registry and parked shared services, and no part of the daemon changes role in place. The orchestrator runs in that process up to the promotion of the system cluster; `handoffTakeover.BecomeLeader` then returns `ErrRoleChanged` (the daemon restarts, as it does on every role change) and holds back the `failover_failed` alert that the interrupted run would send. The process that boots as the leader records the leadership (`cluster.AssumeLeadership`), and once the shared services and the projects are up (`Wire.Ready`) `resumeServerMove` finishes the move: it resumes a move that is unfinished, goes to this node and finds this node the leader. `supavise failover --resume` remains the way to continue any other. `failover.Locker` is the project lock the engine takes (`projectLocker`: the same advisory lock, `pg_advisory_lock(hashtext('supavise:' || ref))`, because the engine exports no way to take its lock).
- **Projects homed on other nodes.** The shared services run on the leader, so the leader registers every active project with them, the ones homed elsewhere too (`ensureRemoteTenants`, after `Engine.EnsureTenants`, which leaves those to their home). A node's own health checks (`nodeSeenBy`) read the projects homed on that node, because the plane of a node has no units for the others and the checks would call each of them unhealthy.
- **The relay's push guard.** The WAL relay refuses to archive for a replica unless the promotion procedure wrote `promote.ok` for the current epoch (`backup.RelayOptions.Replica`, `Epoch`, `Refused`). `relayGuard` answers: on a server with no cluster identity, "not a replica" without a look at anything; on a node of a cluster, from the registry's replica rows for this node (read through a read-only connection of the guard's own, opened lazily, because the relay starts before the registry opens and a hand-made `pg_promote()` removes `standby.signal`), and from `standby.signal` while the registry cannot be read. `StartWALRelay` attaches it, so the relays of the command-line processes are guarded too. A refused push raises `replica_unhealthy` (critical) for the project.
- **A host that is behind.** While the host layer lags this release (`hostsetup.Status`, see `wire_host.go`), a server with no cluster identity does not become a cluster (`cluster` is off), and a node that belongs to one keeps its mesh and runs no replica controller and no failover monitor. `hostBehind` reads the state the daemon started with.

## Limits

- `Serve` does not supervise: a crashed project unit is systemd's to restart (`Restart=on-failure`)
  and shows as unhealthy through `Health`.
- The base backup a replica starts from is taken by the leader's backup service. For a project homed on
  a follower the home node would take it, and it records the backup in its registry copy, which is
  read-only there, so a replica of such a project cannot be set up until backups taken on another node
  are recorded by the leader (`backup.Options.TakeBase` and `placement.BackupOps` are not connected).
- The commands of the CLI that open the node (`openNode`, `openOptions`) open the registry as a
  leader's; a follower needs the read-only open that `openFollower` gives the daemon.
- A server move that the restart interrupts is finished by the new leader's daemon without the CLI
  that started it: the CLI's stream ends when the old daemon stops, and `supavise failover --resume`
  or the log shows the rest.

## Test

```
go test ./internal/app/                                          # wiring, the ports of a cluster node, pg-meta key
SUPAVISE_TEST_UNPACKED=<unpacked artifacts dir> go test -run Serve -v ./internal/app/
```

The gated test runs the daemon on real artifacts under the exec backend: system init, a project
created and stopped before the daemon starts, the daemon bringing it back at boot, requests
through the proxy (project API, bad key, Management API, dashboard GoTrue) and the admin
listener, and a graceful stop. It starts two PostgreSQL clusters, GoTrue and PostgREST.

```
SUPAVISE_TEST_UNPACKED=<unpacked artifacts dir> SUPAVISE_SETTINGS_E2E=1 \
  go test -run TestSettingsIntegration -v ./internal/app/       # about 25 s, under 1 GB
```

`TestSettingsIntegration` (ports 42000-42999, exec backend) runs the daemon with the system
cluster, one micro project, Supavisor and Storage, saves settings through the Management API
with a PAT and watches the services change: GoTrue's redirect allow list, sign-up toggle, SMTP and
a custom mail template reaching a mail sink; PostgREST serving a newly exposed schema; Storage's
upload limit; immediate refusal of a revoked key and of disabled legacy keys; the database
password (direct and through the pooler); Postgres settings with `ALTER SYSTEM`, and their reset.
With `SUPAVISE_SETTINGS_E2E_HOLD=<file>` it leaves the stack up and writes the connection details
there, for the real Supabase CLI (`supabase --profile=... config push`); delete the file to stop.

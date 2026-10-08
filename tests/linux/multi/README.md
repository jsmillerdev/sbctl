# tests/linux/multi

Two Incus nodes with a real systemd on one GitHub Actions runner, and the services on the runner that the nodes use. The `replication-spike` workflow (`.github/workflows/replication-spike.yml`) proves the harness on both architectures (design spike S9); the `replication` workflow (`.github/workflows/replication.yml`) runs the two-server release test on it (below).

## Outcome

Use virtual machines where `/dev/kvm` works (`ubuntu-24.04`, amd64) and privileged system containers with `security.nesting` where it does not (`ubuntu-24.04-arm`, arm64). In those modes two nodes boot, run the polkit rule and the unit sandboxes, install Supavise from this checkout, hold the instance metadata block, reach each other on port 7443 and reach the services on the bridge. `MULTI_MODE=auto` makes that choice.

| Runner | Nodes | Result |
|---|---|---|
| `ubuntu-24.04` (amd64) | virtual machines | All checks pass. `/dev/kvm` opens for root without the udev rule that `kvm_enable` writes. |
| `ubuntu-24.04` (amd64) | privileged system containers | All checks pass. |
| `ubuntu-24.04` (amd64) | unprivileged system containers | All checks pass except `imds`. |
| `ubuntu-24.04-arm` (arm64) | virtual machines | Not possible. The runner has no `/dev/kvm`; `spike.sh` stops at its first check, and software emulation was not tried. |
| `ubuntu-24.04-arm` (arm64) | privileged system containers | All checks pass. |
| `ubuntu-24.04-arm` (arm64) | unprivileged system containers | All checks pass except `imds`. |

The workflow runs all six, and a seventh and eighth with `MULTI_MODE=auto` (the choice `replication.yml` makes: a virtual machine where `/dev/kvm` works, else a privileged container) on each runner. A job is green when exactly the checks in its `expect_fail` fail, each for the reason given with it (`imds=can reach the instance metadata service`: the text its failure line must contain). It turns red when another check fails, when an expected check fails for another reason, and when an expected failure starts to pass, because this page would then be wrong.

`systemd-smoke.sh` passes unchanged on a third, fresh node in the modes where the metadata block holds: virtual machines on amd64 (150 s), privileged containers on amd64 and arm64 (about 115 s). In unprivileged containers it stops at the same metadata check, so nothing is known about the rest of it there. Run it with the workflow's `smoke` input.

**The instance metadata block needs a node that can load a cgroup BPF program.** In an unprivileged container, `IPAddressDeny=169.254.169.254 fd00:ec2::254` is accepted and does not filter: a `curl` moved into the cgroup of `supavise-postgres@system.service` reaches the metadata mock, which is what `imds_blocked_in` in `tests/linux/lib.sh` tests. Systemd's IP filter is a cgroup BPF program, which an unprivileged container cannot load (inferred, not traced). In virtual machines and in privileged containers the same six units block both addresses. Every other check, including the sandboxing of the units (`ProtectSystem=strict`, `TemporaryFileSystem`, `PrivateDevices`, `InaccessiblePaths`, `OOMScoreAdjust=-500`), passes in unprivileged containers too.

A privileged container shares the runner's kernel and root. That is acceptable on an ephemeral runner and is the reason to prefer virtual machines where they exist. The single-node `systemd-smoke` job already covers the metadata block on the arm64 runner, which is itself a virtual machine.

## What the checks prove

`spike.sh` records one PASS, FAIL or SKIP per check in `$LOG_DIR/results.md` (the workflow adds it to the job summary). A check is skipped when the check it depends on failed.

| Check | Proves |
|---|---|
| `incus` | Incus 6.0 from the Ubuntu archive installs, the bridge `incusbr0` has the address `10.213.213.1`, and Docker's forwarding rules are open for it. |
| `services` | Garage, the signed release server and the fake AWS service listen on the bridge address. |
| `launch`, `boot` | Both nodes start from `images:ubuntu/24.04` with fixed addresses (`.11`, `.12`); systemd is PID 1 and reaches `running`; cgroup v2; 8 GB or more free on the root file system. |
| `prep`, `net` | `apt` works in the node; the node reaches github.com, Garage, the release server and the fake AWS service. |
| `peer` | Each node reaches the other on port 7443 (a TCP sink as a transient unit), sends 256 MiB, and resolves `n2.incus`. |
| `rules` | A probe: with Docker's two `DOCKER-USER` rules removed, what the nodes still reach. It passes whatever it finds and puts the rules back. |
| `install` | `deploy/install.sh --binary` of the build from this checkout, with `--tls off`, Garage as the backup backend and no dashboard. Seven units (the daemon, the system Postgres and GoTrue, and the four shared services) are active and the claim page answers through the proxy. |
| `polkit` | The `supavise` user restarts `supavise-pgmeta.service`; `daemon-reload`, `systemd-journald.service` and `supavise-upgrade.service` are refused. The installer installed polkit itself. |
| `hardening` | From inside the mount namespace of `supavise-postgres@system`, its own cluster is readable and `/etc/supavise/master.key` and the backups are not. |
| `imds` | `lib.sh`'s metadata mock on `169.254.169.254` and `fd00:ec2::254` answers outside six units and not inside them. |
| `project` | A micro project becomes healthy and a base backup is written to and listed from Garage. |
| `stub` | The stamped `install.sh` from the release server verifies the signed release (`--verify-only`). |
| `fakeaws`, `fakeaws-log` | IMDSv2, `DescribeInstances`, a `DryRun` refusal (412) and `GetSecretValue` answer with each node's own identity, and the call log names both nodes. |
| `smoke` | Only with `MULTI_SMOKE=1` (the workflow's `smoke` input): `tests/linux/systemd-smoke.sh` unchanged, on a third fresh node of the same kind. |

## Setup time and size

Measured on the runners in the table (4 vCPU, about 16 GB), two nodes with 2 vCPU and 4 GiB each.

| Lowest and highest of the runs | Virtual machines (amd64) | Privileged containers (amd64) | Privileged containers (arm64) |
|---|---|---|---|
| Install Incus (3 runs) | 22 to 31 s | 13 to 15 s | 24 to 27 s |
| Create and start both nodes (3 runs) | 18 to 23 s | 11 to 14 s | 7 to 10 s |
| Start to systemd `running` (3 runs) | 10 to 17 s | 2 to 10 s | 3 to 8 s |
| `apt` packages in the node (3 runs) | 8 to 13 s | 5 to 7 s | 4 to 5 s |
| `install.sh --binary` (3 runs, both nodes at once) | 49 to 58 s | 34 to 40 s | 31 to 36 s |
| Whole `spike.sh` run (2 runs) | 197 to 206 s | 135 to 155 s | 143 to 150 s |

| After the install and one project per node | Virtual machines (amd64) | Privileged containers (amd64) |
|---|---|---|
| Runner memory used (1.1 GB before the nodes) | 8.6 GB | 3.5 GB |
| Memory used inside a node (`free`) | 1.3 GB | 0.9 GB |
| `supavise.slice` in a node (one project, no dashboard) | 1.2 GB | 1.3 GB |
| Root file system used in a node | 2.5 GB | shares the runner's disk |
| Incus storage on the runner (dir pool) | 6.1 GB | 4.8 GB |

A single TCP stream between the nodes carries 0.7 to 0.9 GiB/s with a 0.3 to 1 ms round trip in virtual machines, and 2.5 to 5.8 GiB/s with 0.05 to 0.07 ms in containers. The bridge is not the limit for the replication tests.

## Limits

- The VM image is 292 MB and the container image 134 MB, from `images.linuxcontainers.org`; Incus 6.0.0 comes from Ubuntu's universe repository, without Canonical security updates. Both are outside this repository.
- The nodes sit behind NAT on a private bridge. They are installed with `--tls off` and `--public-ip <bridge address>`; nothing needs DNS.
- Garage, the release server and the fake AWS service listen on the bridge address only. The spike installs the nodes independently, each with the prefix `n1` or `n2` in one bucket, because two independent installs both own the project `system`. A cluster shares one prefix: `[backup]` is a cluster-scoped setting, the join copies the leader's, and the replication test gives both servers the same `--s3-*` flags (prefix `cluster`), as the joiner stack of the AWS template gives the new server the bucket of the first. Storage's objects go to a second bucket, `supavise-objects`, after `supavise storage migrate --to s3`.
- `fake-aws.py` stands in for `AWS_ENDPOINT_URL_IMDS`, `_EC2` and `_SECRETSMANAGER`. It does not serve `169.254.169.254` (the daemon would use the override), does not check signatures and returns only the fields Supavise reads. `StopInstances` and `StartInstances` change the state the next describe call reports and nothing else.
- The runner image's Docker sets the `FORWARD` policy to `DROP`. `multi_docker_rules` adds `-i incusbr0 -j ACCEPT` and a related/established rule for traffic returning to the bridge to `DOCKER-USER`. The `rules` check takes them away for a moment: a node then has no route out (`curl https://github.com` answers 000), while node to node on 7443 still works, because that traffic is bridged and `br_netfilter` is not loaded.
- A virtual machine needs `qemu-system-*` and OVMF; the container modes skip them.
- The bridge has no IPv6 address.

## Files

| File | What |
|---|---|
| `lib-multi.sh` | Sourced helpers: `on NODE cmd...` (run in a node; a script goes in on standard input), `node_push`, `nsystemctl` (the `systemctl` retry wrapper of `upgrade-e2e.sh`, run in a node), `kvm_usable` and `kvm_enable`, `multi_incus_install`, `multi_incus_init`, `multi_docker_rules`, `multi_launch_node`, `multi_wait`, `multi_prep_node`, `multi_install` (copy the binary in and run `install.sh --binary`), `multi_push_tests`, `multi_kill_node` and `multi_start_node`, `garage_up` (two buckets), `release_server_up` (with `make_release`), `fake_aws_up`, `peer_listen` and `peer_send`, `mem_snapshot`, `multi_collect_logs`, `multi_down`. |
| `lib-checks.sh` | The check runner of both scripts (`check`, `check_each`, `needs`, `reached`, the results table, the expected failures with their reasons) and the checks that bring the nodes up (`incus`, `services`, `launch`, `boot`, `prep`, `net`). |
| `spike.sh` | The checks above. |
| `replication.sh` | The two-server release test. |
| `node-lib.sh` | What `replication.sh` runs inside a node (psql on the cluster of a project, the registry, the Management API with files for every secret, replica and writer helpers, `node_dump`). |
| `writer.py`, `writer-compare.py` | The continuous writer, and the comparison of what it saw with what the table holds. |
| `fence.sh` | The test's `fence_command`. |
| `fake-aws.py` | The fake instance metadata, EC2 and Secrets Manager service (standard library only). |

## The two-server test

`replication.sh` (workflow `replication.yml`, on pushes to `ws/rep-x2` and `integrate/**` and by hand) installs Supavise from the checkout on `n1`, joins `n2` to it and walks a project through everything a second server is for. Both servers run a build of the checkout: `v0.0.1`, which the release server signs and both nodes install, and `v0.0.2`, the same code under another version, which `supavise upgrade` moves them to. Each step is a check with its own log in `$LOG_DIR/checks`, a row in `results.md` and, for the steps that change the cluster, a snapshot of both nodes in `$LOG_DIR/snapshots` (units, configuration without secrets, `status`, `node ls`, the registry's nodes, projects, replicas and moves, the state of each PostgreSQL cluster, the fence log). The nodes are 2 vCPU and 5 GiB each.

| Check | Proves |
|---|---|
| `install` | `install.sh --binary` of the checkout on `n1`: Garage as the backup backend under the prefix `cluster`, Storage's bucket and endpoint in the configuration, `fence_command` set, domain `cluster.test` (the nodes of a cluster need one domain: left to default, each node uses its own address), no TLS. |
| `claim`, `projects` | The claim token, a personal access token, two `small` projects (replicas start at Small) with a table of 100 rows, the first with a 2 MiB Storage object. |
| `storage` | `supavise storage migrate --to s3`: the object is served from the bucket and the bucket holds objects. A server failover needs it. |
| `token`, `join` | `supavise node token`: the daemon restarts once and listens on 7443. `install.sh --join-token-file` on `n2` (the token travels in a 0600 file, never on a command line), with the same `--s3-*` flags as `n1`. The leader lists the node as `active` the moment the installer returns, which is what the joiner stack of the AWS template relies on when it signals success. |
| `cluster` | Both nodes agree on the leader, the epoch and two active nodes; a mesh session each way; `supavise status` exits 0 on both; `n2` serves the registry from its standby, runs the pooler and parks Realtime, Storage, pg-meta and the system GoTrue. |
| `replica-setup` | `POST /v1/projects/{ref}/read-replicas/setup` for both projects; `databases-statuses` walks to `ACTIVE_HEALTHY` (each change of steps is in the log); the replica is on `n2`. |
| `replica-shapes` | `GET /platform/projects/{ref}/databases` and `databases-statuses` list the primary first, with the fields Studio reads, the replica's identifier `<ref>-rr-<region>-<id6>`, placeholder connection strings, and `replicaInitializationStatus` `completed`. `supavise replicas ls` works on both nodes. |
| `replica-read` | A row written through the primary's Data API is read from the replica's endpoint (`<identifier>.api.<domain>`) within 20 s; a write to the replica is refused; the cluster on `n2` is a standby. |
| `replica-ddl` | A new table is served by the replica's endpoint within 60 s (the schema reload). |
| `pooler`, `lb` | `n2`'s Supavisor serves `postgres.<identifier>` as a standby and `postgres.<ref>` as the primary on `n1`; the `<ref>-lb` host answers with `X-Supavise-Route`. |
| `project-switchover`, `project-failback` | `supavise projects failover <ref>` with a writer running through `n2`'s proxy: the project is homed on `n2` (and back on `n1`), the move is `done`, the old home follows as a replica, the Data API and Storage serve the data through both nodes, and no row the writer saw acknowledged is missing. |
| `server-switchover`, `server-failback` | `supavise failover --to n2` run on `n1` (and `n1` again): the epoch rises by one, the move is `done`, the shared services start on the new leader and park on the old one, every project is homed on the new leader, the replicas come back on the old one, no acknowledged row is lost. The command's exit status is checked after the state, because the old leader's daemon restarts into its new role in the middle of the move it started. |
| `hard-failover` | `n1` stops at once (`incus stop --force`) while the writer runs. `supavise failover --force` on `n2` runs the fence command (`fence.sh` exits 0 when `n1` does not answer on its mesh port, and records its environment in the state directory), promotes, and `n1` is `fenced` in the registry. Printed: RPO, the acknowledged rows that are not on the new primary and the span of their acknowledgements, and RTO, the time from the stop to the writer's first acknowledgement. An unplanned failover on asynchronous replication loses the last moments by design, so the test bounds the loss by the lag budget (`RPO_BUDGET_S`, 5 s of writes: 100 rows at 20 rows a second; the replay lag measured before the stop is a fact of the run) and RTO by 300 s; nothing may be in the table that the writer did not write. |
| `fenced` | `n1` boots again: `fenced.json` names `n2`, no PostgreSQL unit runs, `postgres.run` is gone and the unit's `ConditionPathExists` keeps a start from running anything, port 80 answers 503, `status` says FENCED. This is the Linux-level cover of the hard stop in design 2.10.8. |
| `rejoin` | `supavise node rejoin`: the fence record goes, the node is `active`, its old data is kept as `data.diverged-<epoch>`, its replicas of both projects are `ACTIVE_HEALTHY`, both nodes are healthy. |
| `upgrade-leader`, `upgrade-follower` | `supavise upgrade --version v0.0.2` (against the release server, signed with the harness key) on the leader and then on the follower: the binary and the daemon report `v0.0.2`, `status` exits 0, the `InvocationID` and the postmaster start time of every PostgreSQL unit on the node (primaries, standbys and the system cluster) are the same before and after, the replicas stay healthy and a row reaches a replica; at the end the registry shows `v0.0.2` for both nodes. |

A check that a failed earlier check makes impossible is skipped. A state that a check may reach although it fails (a move that ended with an error but did move the leader) is marked with `reached`, so the later checks still run.

Secrets stay in 0600 files in the nodes (`/root/keys`, `/root/pat`, header files for `curl -H @file`) and in `$WORK` on the runner, which is removed; none is a command-line argument, and the configuration in the artifact has every value whose name suggests a secret removed.

## Running it

```
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o /tmp/supavise ./cmd/supavise
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.2" -o /tmp/supavise-next ./cmd/supavise
go build -o /tmp/releasetool ./deploy/releasetool
sudo MULTI_MODE=container-privileged SUPAVISE_BIN=/tmp/supavise SUPAVISE_RELEASETOOL=/tmp/releasetool tests/linux/multi/spike.sh
sudo MULTI_MODE=auto SUPAVISE_BIN=/tmp/supavise SUPAVISE_BIN_NEXT=/tmp/supavise-next SUPAVISE_RELEASETOOL=/tmp/releasetool tests/linux/multi/replication.sh
```

`MULTI_MODE` is `vm`, `container`, `container-privileged` or `auto` (a virtual machine when `/dev/kvm` works, else a privileged container). The binaries must report `v0.0.1` and `v0.0.2`: the release server signs them under those tags; only `replication.sh` needs the second. Run them on a disposable Ubuntu 24.04 machine with Docker; they install Incus, change iptables rules and start instances. `MULTI_MEM` and `MULTI_CPU` size the nodes (`replication.sh` defaults to 5 GiB; the spike to 4 GiB; both 2 vCPU). `LOG_DIR` (default `/tmp/supavise-multi-logs`) receives `results.md`, `facts.tsv`, `timings.tsv`, `memory.tsv`, one log per check, both nodes' journals (the whole journal, the daemon's and the PostgreSQL units'), Incus' own logs and the fake AWS call log; `replication.sh` adds `snapshots/` and each node's `writer/` directory.

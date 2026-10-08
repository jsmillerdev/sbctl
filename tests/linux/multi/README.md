# tests/linux/multi

Two Incus nodes with a real systemd on one GitHub Actions runner, and the services on the runner that the nodes use. The replication tests build on it; the `replication-spike` workflow (`.github/workflows/replication-spike.yml`) proves it on both architectures (design spike S9).

## Outcome

Use virtual machines where `/dev/kvm` works (`ubuntu-24.04`, amd64) and privileged system containers with `security.nesting` where it does not (`ubuntu-24.04-arm`, arm64). Two nodes boot, run the polkit rule and the unit sandboxes, install Supavise from this checkout, hold the instance metadata block, reach each other on port 7443 and reach the services on the bridge, in every mode below except where the table says otherwise.

| Runner | Nodes | Result |
|---|---|---|
| `ubuntu-24.04` (amd64) | virtual machines | All checks pass. `/dev/kvm` opens as it is; the udev rule that `kvm_enable` writes is not needed. |
| `ubuntu-24.04` (amd64) | privileged system containers | All checks pass. |
| `ubuntu-24.04` (amd64) | unprivileged system containers | All checks pass except `imds`. |
| `ubuntu-24.04-arm` (arm64) | virtual machines | Not possible. The runner has no `/dev/kvm`; `spike.sh` stops at its first check, and software emulation was not tried. |
| `ubuntu-24.04-arm` (arm64) | privileged system containers | All checks pass. |
| `ubuntu-24.04-arm` (arm64) | unprivileged system containers | All checks pass except `imds`. |

The workflow runs all six. A job is green when exactly the checks in its `expect_fail` fail; it turns red when another check fails and also when an expected failure starts to pass, because this page would then be wrong.

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
| `install` | `deploy/install.sh --binary` of the build from this checkout, with `--tls off`, Garage as the backup backend and no dashboard. The seven shared units are active and the claim page answers through the proxy. |
| `polkit` | The `supavise` user restarts `supavise-pgmeta.service`; `daemon-reload`, `systemd-journald.service` and `supavise-upgrade.service` are refused. The installer installed polkit itself. |
| `hardening` | From inside the mount namespace of `supavise-postgres@system`, its own cluster is readable and `/etc/supavise/master.key` and the backups are not. |
| `imds` | `lib.sh`'s metadata mock on `169.254.169.254` and `fd00:ec2::254` answers outside six units and not inside them. |
| `project` | A micro project becomes healthy and a base backup is written to and listed from Garage. |
| `stub` | The stamped `install.sh` from the release server verifies the signed release (`--verify-only`). |
| `fakeaws`, `fakeaws-log` | IMDSv2, `DescribeInstances`, a `DryRun` refusal (412) and `GetSecretValue` answer with each node's own identity, and the call log names both nodes. |
| `smoke` | Only with `MULTI_SMOKE=1` (the workflow's `smoke` input): `tests/linux/systemd-smoke.sh` unchanged, on a third fresh node of the same kind. |

## Setup time and size

Measured on the runners in the table (4 vCPU, about 16 GB), two nodes with 2 vCPU and 4 GiB each.

| | Virtual machines (amd64) | Privileged containers (amd64) | Privileged containers (arm64) |
|---|---|---|---|
| Install Incus | 22 s | 15 s | 25 s |
| Create and start both nodes | 18 s | 14 s | 9 s |
| Start to systemd `running` | 12 to 15 s | 2 to 4 s | 3 to 6 s |
| `apt` packages in the node | 8 s | 5 s | 5 s |
| `install.sh --binary` (both nodes at once) | 55 s | 35 s | 37 s |
| Whole `spike.sh` run | 197 s | 135 s | 143 s |

| After the install and one project per node | Virtual machines (amd64) | Privileged containers (amd64) |
|---|---|---|
| Runner memory used (1.1 GB before the nodes) | 8.6 GB | 3.5 GB |
| Memory used inside a node (`free`) | 1.3 GB | 0.9 GB |
| `supavise.slice` in a node (one project, no dashboard) | 1.2 GB | 1.3 GB |
| Root file system used in a node | 2.5 GB | shares the runner's disk |
| Incus storage on the runner (dir pool) | 6.1 GB | 4.8 GB |

A single TCP stream between the nodes carries 0.7 to 0.8 GiB/s with 0.3 to 1 ms round trip in virtual machines, and 2.5 to 5.8 GiB/s with 0.05 ms in containers. The bridge is not the limit for the replication tests.

## Limits

- The VM image is 292 MB and the container image 134 MB, from `images.linuxcontainers.org`; Incus 6.0.0 comes from Ubuntu's universe repository, without Canonical security updates. Both are outside this repository.
- The nodes sit behind NAT on a private bridge. They are installed with `--tls off` and `--public-ip <bridge address>`; nothing needs DNS.
- Garage, the release server and the fake AWS service listen on the bridge address only. The two nodes use the prefixes `n1` and `n2` in one bucket, because two independent installs both own the project `system`; a joined cluster shares one prefix.
- `fake-aws.py` stands in for `AWS_ENDPOINT_URL_IMDS`, `_EC2` and `_SECRETSMANAGER`. It does not serve `169.254.169.254` (the daemon would use the override), does not check signatures and returns only the fields Supavise reads. `StopInstances` and `StartInstances` change the state the next describe call reports and nothing else.
- The runner image's Docker sets the `FORWARD` policy to `DROP` and loads `br_netfilter`. `multi_docker_rules` adds `-i incusbr0 -j ACCEPT` and a related/established rule for traffic returning to the bridge to `DOCKER-USER`; without them the nodes have no route out and cannot reach each other.
- A virtual machine needs `qemu-system-*` and OVMF; the container modes skip them.
- The bridge has no IPv6 address.

## Files

| File | What |
|---|---|
| `lib-multi.sh` | Sourced helpers: `on NODE cmd...` (run in a node; a script goes in on standard input), `node_push`, `nsystemctl` (the `systemctl` retry wrapper of `upgrade-e2e.sh`, run in a node), `kvm_usable` and `kvm_enable`, `multi_incus_install`, `multi_incus_init`, `multi_docker_rules`, `multi_launch_node`, `multi_wait`, `multi_prep_node`, `garage_up`, `release_server_up` (with `make_release`), `fake_aws_up`, `peer_listen` and `peer_send`, `mem_snapshot`, `multi_collect_logs`, `multi_down`. |
| `spike.sh` | The checks above. |
| `fake-aws.py` | The fake instance metadata, EC2 and Secrets Manager service (standard library only). |

## Running it

```
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=v0.0.1" -o /tmp/supavise ./cmd/supavise
go build -o /tmp/releasetool ./deploy/releasetool
sudo MULTI_MODE=container-privileged SUPAVISE_BIN=/tmp/supavise SUPAVISE_RELEASETOOL=/tmp/releasetool tests/linux/multi/spike.sh
```

`MULTI_MODE` is `vm`, `container`, `container-privileged` or `auto` (a virtual machine when `/dev/kvm` works, else a container). The binary must report `v0.0.1`: the release server signs it under that tag. Run it on a disposable Ubuntu 24.04 machine with Docker; it installs Incus, changes iptables rules and starts instances. `LOG_DIR` (default `/tmp/supavise-multi-logs`) receives `results.md`, `facts.tsv`, `timings.tsv`, `memory.tsv`, one log per check, both nodes' journals, Incus' own logs and the fake AWS call log.

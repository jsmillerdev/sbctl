# internal/hostsetup

The host layer of an upgrade: `supavise system converge`, a list of idempotent root steps that bring a server's unit files, directories, mount protection, firewall and packages to what a release expects, and `supavise install --aws-first-boot`, which prepares an EC2 instance's data volume before the installer runs. `internal/nodeupgrade/README.md` ("The three layers") says where it sits in an upgrade; `deploy/README.md` is the operator's view.

## converge

`hostsetup.go` has the `Step` interface, `Converger` (`Check` and `Run`) and the revision (`Revision`, the number a release's manifest names as `host.converge_revision`). `steps.go` has the steps of this release, in the order they run:

| id | What it does | Pending when |
|---|---|---|
| `units` | writes the embedded unit files and the polkit rule (`deploy/systemd`), the backup and upgrade timers rendered from `config.toml` | a file differs |
| `directories` | `/etc/supavise/cluster` and `/etc/supavise/config.d`, mode 0750, owned by `supavise` | missing, or another mode or owner. Not applicable until the `supavise` user exists |
| `mounts` | `RequiresMountsFor=` drop-ins (`<unit>.d/10-supavise-data-mount.conf` for the state directory, `20-supavise-etc-mount.conf` for the config directory) for every embedded `supavise*.service` unit, when that directory is a mount point or lies on one other than `/`. The upgrade service keeps its state on the root disk and gets only the config one | a drop-in is missing or differs. The two units the CloudFormation first-boot script covered have the same files, so a node it made changes nothing |
| `ufw` | `ufw allow 7443/tcp` (the port of `[node] peer_listen`) | ufw is installed and active and lists no allow rule for the port. A rule narrowed to a source counts. Without root `--check` cannot read the rules and says so |
| `packages-*` | `apt-get install` of the packages in `DeclaredPackages` (none in this release) | `dpkg-query` does not report one installed |
| `config-d` | on a node in a cluster, refreshes `config.d/10-cluster.toml` from the leader through `ConfigSyncer` | the leader's settings differ. Left out of the list when `Options.ConfigSync` is nil |
| `marker` | `<state_dir>/converged`, the revision completed (`revision=N`, `version=`, `at=`) | the revision in the file is below `Revision` |

Every step checks for itself before it changes anything: `Run` calls each step's `Apply` whether or not `Check` found something, so a second run changes nothing, and a step that fails does not stop the others. The marker is written last, only when no step failed. `Activate` (a hook of the caller; `cmd/supavise/system_converge.go` re-reads systemd's unit files, enables the system units and starts or stops the upgrade timer) runs once after the steps.

`Titles()` is the release's `host_changes`: the titles of the steps, in order. A change to the step list raises `Revision`; the release manifest and `supavise release-info` carry the same number.

`--check` runs the same checks without the changes and needs no root. `supavise system converge --check --json` prints a list of `{id, title, pending, needs_root, detail}`; the last entry is the marker. A check that cannot be made is reported pending with the error as its detail, except where the reason is only that it needs root.

## The daemon

`marker.go` reads and writes the marker (`ReadMarker`, `WriteMarker`, `StatusOf`). `monitor.go` is `Monitor`: one minute after the daemon starts and every five minutes it compares the marker with `Revision` and tells `Raise`, which `internal/app/wire_host.go` turns into `host_not_converged` (warning, "run `sudo supavise system converge`") and its resolution. It does not judge the node while an upgrade runs, and an unreadable marker says nothing. The hook registers itself at the front of the daemon's hook list and provides `hostsetup.Status` (`Get[hostsetup.Status](w)`), so a hook that enables a cluster feature can stay off while `Status.Behind()`. It starts only for a daemon that runs in `supavise.service` (its control group says so) on the systemd supervisor.

## First boot

`firstboot.go` is `FirstBoot.Run`, which `supavise install --aws-first-boot` runs before the installer reads or writes anything under `/etc/supavise` (a replacement instance finds its `config.toml` and master key on the volume):

1. The metadata service must answer with an instance id; otherwise it stops before any command runs. The instance's `supavise:stack-name` tag, when it has one, is returned for `config.d/20-aws.toml`.
2. It finds the data volume: the whole-disk `nvme-Amazon_Elastic_Block_Store_*` links of `/dev/disk/by-id`, minus the disk that holds `/` (`findmnt`, `lsblk -no PKNAME`; if that is unknown it stops, because the root volume could be taken for the data volume). It waits up to ten minutes for the volume to be attached. Two candidates are an error that names `--data-device`.
3. It makes an XFS file system (`mkfs.xfs -L supavise`) only on a blank device: `blkid -p` exits 2 (nothing found) and `lsblk` lists no partition. Any other result of `blkid`, a signature, a partition table or a file system that is not XFS leaves the device alone, and the last two are errors.
4. It writes the `/etc/fstab` line (`UUID=… /var/lib/supavise xfs defaults,nofail,prjquota 0 2`), mounts the volume, runs `xfs_growfs`, binds `/var/lib/supavise/etc` to `/etc/supavise` (fstab line with `x-systemd.requires-mounts-for`), gives a volume that an earlier install owned its `supavise` user back with the ids that own the files (stopping with the way out when the image has taken the uid or gid), and runs the `mounts` step.

Each of these is skipped when it is already done. The tests use `awsapi/awsfake` as the metadata service and a fake `Runner` (`lsblk`, `blkid`, `mount`) over temporary directories.

## Tests

`go test ./internal/hostsetup/` covers idempotency over a temporary root (units, directories, mounts and ufw together, `Check` before and after), each step on its own (ownership and mode, drop-in contents, the mount-point parser, ufw output shapes and the root-less check), a declared package step with a fake runner, a failing step, the marker file, the monitor, and first boot (a blank, an XFS, an ext4 and a partitioned volume; a failing `blkid`; a volume that appears late; two candidates; a missing metadata service; the restored account). The `converge-e2e` job of `linux.yml` (`tests/linux/converge-e2e.sh`) runs converge on a node that a v0.1.x-shaped release installed.

## Limits

- Converge does not remove what an earlier revision wrote (a drop-in for a directory that stopped being a mount stays).
- The ufw step looks at `ufw status`, not at the rules files; a rule for a port range that includes the port, or one written as `7443,7444/tcp`, does not count and the step adds its own.
- `config-d` has no implementation here: the mesh package supplies the `ConfigSyncer`, and the hourly refresh is the daemon's.

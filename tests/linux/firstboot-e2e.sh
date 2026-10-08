#!/usr/bin/env bash
# First boot of an EC2 instance on loop devices: the code behind `supavise install --aws-first-boot`
# (internal/hostsetup, FirstBoot) with the real blkid, lsblk, findmnt, mkfs.xfs, mount and xfs_quota.
#
#   sudo SUPAVISE_HOSTSETUP_TEST=/path/to/hostsetup.test tests/linux/firstboot-e2e.sh
#
# SUPAVISE_HOSTSETUP_TEST is the test binary of internal/hostsetup (`go test -c -o <path>
# ./internal/hostsetup`); without it the script builds one with go. Needs root and a kernel with loop
# devices and XFS. Do not run it on a machine you care about: it attaches loop devices, mounts
# directories under a temporary one and edits /etc/fstab (the test puts the file back). Checked:
#
#  1. A blank loop device gets an XFS file system and is mounted with project quotas that are
#     enforced; the config directory is a bind mount of the volume's etc; fstab and the mount
#     drop-ins are in place. The device is found through a by-id directory that has two links per
#     disk, as Ubuntu 24.04 makes them, next to the runner's own root disk.
#  2. A second run formats, mounts and writes nothing again.
#  3. A device that holds ext4 is refused and left as it was.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need_root
cd "$REPO_ROOT" || exit 1

if ! command -v mkfs.xfs >/dev/null || ! command -v xfs_quota >/dev/null; then
  log "installing xfsprogs"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xfsprogs >/dev/null
fi
losetup --find >/dev/null || fail "no loop device is available"

TESTBIN=${SUPAVISE_HOSTSETUP_TEST:-}
if [[ -z $TESTBIN ]]; then
  command -v go >/dev/null || fail "no SUPAVISE_HOSTSETUP_TEST and no go toolchain"
  TESTBIN=$(mktemp -d)/hostsetup.test
  go test -c -o "$TESTBIN" ./internal/hostsetup
fi

log "first boot on loop devices"
out=$(SUPAVISE_FIRSTBOOT_E2E=1 "$TESTBIN" -test.run '^TestFirstBootOnLoopDevices$' -test.v -test.count=1 2>&1) || { printf '%s\n' "$out" >&2; fail "first boot on loop devices"; }
printf '%s\n' "$out" >&2
[[ $out == *"--- PASS: TestFirstBootOnLoopDevices"* ]] || fail "the first-boot test did not run: it was skipped or is missing"

log "firstboot-e2e passed"

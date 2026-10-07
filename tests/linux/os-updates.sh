#!/usr/bin/env bash
# `supavise system os-updates` on a bare Ubuntu or Debian image: the part of the installer that sets
# up unattended OS security updates, run where the install-e2e job (Ubuntu 24.04 only) cannot reach.
#
#   docker run --rm -v /path/to/supavise:/supavise:ro -v "$PWD":/repo:ro IMAGE bash /repo/tests/linux/os-updates.sh
#
# It installs unattended-upgrades and needrestart, writes the apt configuration, and asks apt and
# unattended-upgrade itself what they read: only the security origins (Debian's own configuration
# also takes point-release updates, Ubuntu's takes the base pocket, and we replace both lists), no
# automatic reboot. Then it runs the step again (nothing changes), turns the setting off (our files
# go, the packages stay) and checks that a file that is not ours is left alone. Runs as root; there
# is no systemd in the container, so the timers are not touched (the install-e2e job covers them).
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SV=${SUPAVISE_BIN:-/supavise}
[[ -x $SV ]] || fail "no supavise binary at $SV"
need_root
export DEBIAN_FRONTEND=noninteractive
# shellcheck disable=SC1091
. /etc/os-release
log "$PRETTY_NAME"

apt-get update -qq
log "enable"
"$SV" system os-updates --enable
assert_os_updates on
cfg=/etc/apt/apt.conf.d/52supavise-unattended-upgrades
sum=$(sha256sum "$cfg")
if [[ -d /etc/needrestart ]]; then grep -q 'override_rc.*supavise' /etc/needrestart/conf.d/50-supavise.conf || fail "needrestart drop-in"; fi

# The batch output the reboot check reads, from the needrestart that is installed here.
if command -v needrestart >/dev/null; then
  out=$(needrestart -b -k 2>&1 || true)
  grep -q '^NEEDRESTART-KSTA:' <<<"$out" || fail "needrestart -b -k prints no NEEDRESTART-KSTA line: $out"
fi

log "enable again changes nothing"
out=$("$SV" system os-updates --enable)
[[ $out == *"is up to date"* ]] || fail "second run: $out"
[[ $(sha256sum "$cfg") == "$sum" ]] || fail "the second run rewrote the configuration"

log "disable removes Supavise's files and keeps the packages"
"$SV" system os-updates --disable
assert_os_updates off
dpkg -s unattended-upgrades >/dev/null 2>&1 || fail "disable removed unattended-upgrades"

log "a file that is not Supavise's is left alone"
echo '// someone else wrote this' >"$cfg"
out=$("$SV" system os-updates --disable)
[[ -f $cfg && $out == *"not Supavise's file"* ]] || fail "disable removed a foreign file: $out"
rm -f "$cfg"

log "os-updates: all checks passed on $PRETTY_NAME"

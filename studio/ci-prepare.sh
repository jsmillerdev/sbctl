#!/usr/bin/env bash
# Prepares a fresh GitHub-hosted Ubuntu runner (about 8 GB RAM, 2 vCPU, 14 GB free disk) for
# studio/build.sh: removes preinstalled toolchains the build does not need, adds swap (next build
# peaks near 8 GB), and installs zstd. Needs sudo. Do not run it on a developer machine.
#
#   sudo studio/ci-prepare.sh [swap-gb]     (default 6)
set -euo pipefail

SWAP_GB="${1:-6}"
[[ "$(id -u)" -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
[[ -n "${GITHUB_ACTIONS:-}${SBCTL_CI:-}" ]] || { echo "refusing to run outside CI (set SBCTL_CI=1 to override)" >&2; exit 1; }

echo "before:"; df -h / | tail -1; free -m | sed -n 1,2p

# Disk: these are large and unused by the build.
rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /opt/hostedtoolcache/CodeQL /usr/local/share/boost \
       /usr/local/graalvm /usr/local/.ghcup /usr/share/swift 2>/dev/null || true
apt-get clean

# Swap on the root disk, after the cleanup above has made room.
if ! swapon --show | grep -q /sbctl-swap; then
  fallocate -l "${SWAP_GB}G" /sbctl-swap
  chmod 600 /sbctl-swap
  mkswap /sbctl-swap >/dev/null
  swapon /sbctl-swap
fi
sysctl -w vm.swappiness=60 >/dev/null

# Tools build.sh and verify.sh use.
need=()
for t in zstd curl git python3; do command -v "$t" >/dev/null 2>&1 || need+=("$t"); done
if [[ ${#need[@]} -gt 0 ]]; then apt-get update -qq && apt-get install -y --no-install-recommends "${need[@]}" ca-certificates; fi

echo "after:"; df -h / | tail -1; free -m | sed -n 1,3p

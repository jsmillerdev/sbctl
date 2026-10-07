#!/usr/bin/env bash
# Prepares a fresh GitHub-hosted Ubuntu runner (about 8 GB RAM, 2 vCPU, 14 GB free disk) for
# studio/build.sh: removes preinstalled toolchains the build does not need, adds swap (next build
# peaks near 8 GB), and installs zstd and a compiler when missing. Needs sudo. Do not run it on a developer machine.
#
#   sudo studio/ci-prepare.sh [swap-gb]     (default 6; shrunk if the root disk would drop under 9 GB free)
set -euo pipefail

SWAP_GB="${1:-6}"
[[ "$(id -u)" -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
[[ -n "${GITHUB_ACTIONS:-}${SBCTL_CI:-}" ]] || { echo "refusing to run outside CI (set SBCTL_CI=1 to override)" >&2; exit 1; }

echo "before:"; df -h / | tail -1; free -m | sed -n 1,2p

# Disk: these are large and unused by the build.
rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /opt/hostedtoolcache/CodeQL /usr/local/share/boost \
       /usr/local/graalvm /usr/local/.ghcup /usr/share/swift 2>/dev/null || true
apt-get clean

# Swap. build.sh needs 9 GB of free disk for the checkout and build, so swap must not eat into that.
# Prefer a separate mount (GitHub-hosted runners have a temporary /mnt disk); otherwise use the root
# disk and shrink the swap to what is left over 9 GB. The build wants RAM + swap of at least 11 GB.
free_gb() { df -Pk "$1" | awk 'NR==2 {printf "%d", $4/1048576}'; }
ram_gb=$(awk '/^MemTotal:/ {printf "%d", $2/1048576}' /proc/meminfo)
SWAPFILE=/sbctl-swap
if [[ -d /mnt && "$(df -Pk /mnt | awk 'NR==2 {print $1}')" != "$(df -Pk / | awk 'NR==2 {print $1}')" \
      && "$(free_gb /mnt)" -ge "$((SWAP_GB + 1))" ]]; then
  SWAPFILE=/mnt/sbctl-swap
else
  room=$(( $(free_gb /) - 9 ))
  if [[ "$room" -lt "$SWAP_GB" ]]; then SWAP_GB="$room"; fi
fi
if ! swapon --show | grep -q "$SWAPFILE"; then
  if [[ $((ram_gb + SWAP_GB)) -lt 11 ]]; then
    echo "not enough room: ${ram_gb} GB RAM + ${SWAP_GB} GB swap is under 11 GB, and the build also needs 9 GB of free disk (free: $(free_gb /) GB on /)" >&2
    exit 1
  fi
  fallocate -l "${SWAP_GB}G" "$SWAPFILE"
  chmod 600 "$SWAPFILE"
  mkswap "$SWAPFILE" >/dev/null
  swapon "$SWAPFILE"
fi
sysctl -w vm.swappiness=60 >/dev/null

# Tools build.sh and verify.sh use.
need=()
for t in zstd curl git python3 gcc make; do command -v "$t" >/dev/null 2>&1 || need+=("$t"); done
# gcc and make stand for build-essential: native Node modules may compile (upstream's Dockerfile installs it).
pkgs=()
for t in "${need[@]}"; do
  case "$t" in gcc|make) pkgs+=(build-essential) ;; *) pkgs+=("$t") ;; esac
done
if [[ ${#pkgs[@]} -gt 0 ]]; then
  apt-get update -qq && apt-get install -y --no-install-recommends "${pkgs[@]}" ca-certificates
fi

echo "after:"; df -h / | tail -1; free -m | sed -n 1,3p

#!/usr/bin/env bash
# CI only: the replica data plane on real Postgres clusters.
#
#   tests/linux/replica-blocks.sh
#
# One machine plays two nodes (internal/placement, TestIntegrationReplicaBlocks, exec supervisor):
#
# 1. Node A is a full node with a project that has data, a pg_cron job and pg_net. Node B has a state
#    directory of its own; its agent seeds a standby of the project from A's base backup (a file
#    store), replays the archive through restore_command and streams from A's primary on the
#    canonical port, which stands for the forwarder a second node has there.
# 2. The standby is a hot standby with the settings of the design (hot_standby and its feedback on),
#    serves the data from before and after the backup, and refuses writes. pg_cron and pg_net run on
#    the primary and idle on the standby.
# 3. PostgREST on the standby (a multi-host URI with target_session_attrs=read-only) reads the
#    standby, listens on the primary, refuses writes, and sees a table created on the primary within
#    the schema reload interval.
# 4. A switchover: the primary stops, B waits for the replay of its shutdown checkpoint and promotes
#    onto the canonical port (promote.ok written, recovery settings gone, PostgREST and GoTrue
#    start), pg_cron and pg_net wake without a restart, and A's cluster becomes a standby of B in
#    place and streams from it.
#
# It runs as an unprivileged user (initdb refuses root); no Docker, no systemd. Linux artifacts come
# from the releases pinned in internal/versions/versions.yaml, checked against the release's SHA256SUMS.
set -euo pipefail
cd "$(dirname "$0")/../.."

if [[ "$(id -u)" -eq 0 ]]; then
  echo "refusing to run as root: initdb will not start. Run this script as an unprivileged user." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64) platform=linux-amd64 ;;
  aarch64 | arm64) platform=linux-arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

work=$(mktemp -d)
unpacked=$work/unpacked
mkdir -p "$unpacked"
trap 'rm -rf "$work"' EXIT

fetch() { # fetch <artifact> : unpack the pinned release into $unpacked/<tag>-<platform>
  local name=$1 tag
  tag=$(awk -v re="^  $name:" '$0 ~ re {print $2; exit}' internal/versions/versions.yaml)
  [[ -n "$tag" ]] || { echo "no artifacts.$name in internal/versions/versions.yaml" >&2; exit 1; }
  local base="https://github.com/supabase/slim-services/releases/download/${tag}"
  curl -fsSL "$base/${tag}-${platform}.tar.zst" -o "$work/$name.tar.zst"
  curl -fsSL "$base/SHA256SUMS" -o "$work/$name.sums"
  local want
  want=$(awk -v f="${tag}-${platform}.tar.zst" '$2 == f || $2 == "*" f {print $1; exit}' "$work/$name.sums")
  [[ -n "$want" ]] || { echo "SHA256SUMS of ${tag} has no entry for ${tag}-${platform}.tar.zst" >&2; exit 1; }
  echo "${want}  $work/$name.tar.zst" | sha256sum -c -
  mkdir "$unpacked/${tag}-${platform}"
  zstd -dc "$work/$name.tar.zst" | tar -x -C "$unpacked/${tag}-${platform}"
  rm -f "$work/$name.tar.zst"
}
for a in postgres auth postgrest; do fetch "$a"; done
ls "$unpacked"

# The test builds the supavise binary the clusters archive and restore through.
CGO_ENABLED=0 go test -c -o "$work/placement.test" ./internal/placement

# The package directory is the working directory of a test run.
(cd internal/placement && \
  SUPAVISE_TEST_UNPACKED="$unpacked" \
  "$work/placement.test" -test.run '^TestIntegrationReplicaBlocks$' -test.v -test.timeout 30m)
echo "replica-blocks: done"

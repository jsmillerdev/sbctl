#!/usr/bin/env bash
# CI only: run the backup tests that need a real Linux Postgres artifact and, when the
# SUPAVISE_TEST_S3_* variables are set, a real S3-compatible service (a MinIO service
# container in CI). Not meant for the shared dev Mac: it downloads ~85 MB.
#
#   SUPAVISE_TEST_S3_ENDPOINT=http://127.0.0.1:9000 SUPAVISE_TEST_S3_BUCKET=supavise-test \
#   SUPAVISE_TEST_S3_ACCESS_KEY=minioadmin SUPAVISE_TEST_S3_SECRET_KEY=minioadmin \
#     internal/backup/ci-integration.sh
#
# The bucket must exist. Set SUPAVISE_TEST_PG_BIN to use an already unpacked artifact
# instead of downloading the one pinned in versions.yaml.
set -euo pipefail
cd "$(dirname "$0")/../.."

# initdb refuses to run as root, so the Postgres tests cannot run in a root shell
# (a bare `sudo` step or a root container). Run the job as an unprivileged user.
if [[ "$(id -u)" -eq 0 ]]; then
  echo "refusing to run as root: initdb will not start. Run this script as an unprivileged user." >&2
  exit 1
fi

if [[ -z "${SUPAVISE_TEST_PG_BIN:-}" ]]; then
  case "$(uname -m)" in
    x86_64) platform=linux-amd64 ;;
    aarch64 | arm64) platform=linux-arm64 ;;
    *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
  esac
  tag=$(awk '/^  postgres:/ {print $2; exit}' versions.yaml)
  [[ -n "$tag" ]] || { echo "no artifacts.postgres in versions.yaml" >&2; exit 1; }
  dir=$(mktemp -d)
  trap 'rm -rf "$dir"' EXIT
  url="https://github.com/supabase/slim-services/releases/download/${tag}/${tag}-${platform}.tar.zst"
  curl -fsSL "$url" -o "$dir/pg.tar.zst"
  # Always verify against the SHA256SUMS the release publishes next to the artifact.
  curl -fsSL "https://github.com/supabase/slim-services/releases/download/${tag}/SHA256SUMS" -o "$dir/SHA256SUMS"
  want=$(awk -v f="${tag}-${platform}.tar.zst" '$2 == f || $2 == "*" f {print $1; exit}' "$dir/SHA256SUMS")
  [[ -n "$want" ]] || { echo "SHA256SUMS of ${tag} has no entry for ${tag}-${platform}.tar.zst" >&2; exit 1; }
  echo "${want}  $dir/pg.tar.zst" | sha256sum -c -
  mkdir "$dir/pg"
  zstd -dc "$dir/pg.tar.zst" | tar -x -C "$dir/pg"
  export SUPAVISE_TEST_PG_BIN="$dir/pg/bin"
fi

go test -race -count=1 -timeout 15m -v \
  -run 'PointInTimeRestore|BaseBackupRefusals|WALCommandsExitStatuses|S3Store|RestoreRunningSourceToRecentTime|RestoreInPlaceRollsBackWhenRecovery' \
  ./internal/backup/

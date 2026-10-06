#!/usr/bin/env bash
# CI only: run the backup tests that need a real Linux Postgres artifact and, when the
# SBCTL_TEST_S3_* variables are set, a real S3-compatible service (a MinIO service
# container in CI). Not meant for the shared dev Mac: it downloads ~85 MB.
#
#   SBCTL_TEST_S3_ENDPOINT=http://127.0.0.1:9000 SBCTL_TEST_S3_BUCKET=sbctl-test \
#   SBCTL_TEST_S3_ACCESS_KEY=minioadmin SBCTL_TEST_S3_SECRET_KEY=minioadmin \
#     internal/backup/ci-integration.sh
#
# The bucket must exist. Set SBCTL_TEST_PG_BIN to use an already unpacked artifact
# instead of downloading the one pinned in versions.yaml.
set -euo pipefail
cd "$(dirname "$0")/../.."

if [[ -z "${SBCTL_TEST_PG_BIN:-}" ]]; then
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
  if [[ -n "${SBCTL_CI_PG_SHA256:-}" ]]; then
    echo "${SBCTL_CI_PG_SHA256}  $dir/pg.tar.zst" | sha256sum -c -
  fi
  mkdir "$dir/pg"
  zstd -dc "$dir/pg.tar.zst" | tar -x -C "$dir/pg"
  export SBCTL_TEST_PG_BIN="$dir/pg/bin"
fi

go test -race -count=1 -timeout 15m -v \
  -run 'PointInTimeRestore|BaseBackupRefusals|WALCommandsExitStatuses|S3Store' \
  ./internal/backup/

#!/usr/bin/env bash
# Builds the binaries that tests/linux/upgrade-e2e.sh upgrades between, into OUT_DIR:
#
#   prev   the previous release: origin/main (this checkout when origin/main is not there) with
#          older GoTrue, PostgREST, postgres-meta and Storage pins, as v0.0.1
#   v2     this checkout, as v0.0.2: the release the node is upgraded to
#   v4     this checkout, as v0.0.4, pinning a PostgREST release whose launcher the test makes exit
#   v5     this checkout, as v0.0.5, pinning a newer GoTrue release
#   releasetool   deploy/releasetool, which writes the signed manifests
#   *.versions.yaml   the versions.yaml each binary was built with (the manifest of its release is
#                     made from the same file)
#
#   tests/linux/upgrade-e2e-build.sh OUT_DIR
#
# Run it as the user that has the Go toolchain; the test itself runs as root. The pins are real
# slim-services releases (github.com/supabase/slim-services/releases) that exist for amd64 and arm64.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
out=${1:?usage: $0 OUT_DIR}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cd "$here"

OLD_AUTH=auth-v2.195.0-r0 OLD_REST=postgrest-v16.2-r0 OLD_META=pgmeta-v0.99.0-r0 OLD_STORAGE=storage-v1.79.35-r0
BAD_REST=postgrest-v99.0.0-r0
NEWER_AUTH=auth-v2.196.0-r0

# pins SRC DEST name=tag...: DEST is SRC with these artifact pins.
pins() {
  local src=$1 dest=$2; shift 2
  python3 - "$src" "$dest" "$@" <<'PY'
import re, sys
src, dest, *pairs = sys.argv[1:]
s = open(src).read()
for p in pairs:
    name, tag = p.split("=", 1)
    s, n = re.subn(r"(?m)^(  %s: ).*$" % re.escape(name), lambda m: m.group(1) + tag, s)
    if n != 1:
        sys.exit("versions.yaml has no pin for %s" % name)
open(dest, "w").write(s)
PY
}

# build TREE VERSION OUT: go build of the tree, stamped with the version.
build() {
  (cd "$1" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$2" -o "$3" ./cmd/supavise)
  [[ $("$3" --version) == *"$2"* ]] || { echo "$3 reports: $("$3" --version)" >&2; exit 1; }
}

work=$(mktemp -d)
trap 'git worktree remove --force "$work/prev" 2>/dev/null || true; git worktree remove --force "$work/this" 2>/dev/null || true; rm -rf "$work"' EXIT

# The previous release. A checkout that has no origin/main (a local run) stands in for it.
prev_ref=HEAD
if git fetch --no-tags --depth=1 origin main 2>/dev/null; then prev_ref=FETCH_HEAD; fi
git worktree add --detach "$work/prev" "$prev_ref" >/dev/null
pins "$work/prev/internal/versions/versions.yaml" "$out/prev.versions.yaml" \
  "auth=$OLD_AUTH" "postgrest=$OLD_REST" "pgmeta=$OLD_META" "storage=$OLD_STORAGE"
cp "$out/prev.versions.yaml" "$work/prev/internal/versions/versions.yaml"
build "$work/prev" v0.0.1 "$out/prev"

# This checkout, three times: as it is, and with the two pins the failure and rollback cases need.
git worktree add --detach "$work/this" HEAD >/dev/null
cp internal/versions/versions.yaml "$out/v2.versions.yaml"
build "$work/this" v0.0.2 "$out/v2"
pins internal/versions/versions.yaml "$out/v4.versions.yaml" "postgrest=$BAD_REST"
cp "$out/v4.versions.yaml" "$work/this/internal/versions/versions.yaml"
build "$work/this" v0.0.4 "$out/v4"
pins internal/versions/versions.yaml "$out/v5.versions.yaml" "auth=$NEWER_AUTH"
cp "$out/v5.versions.yaml" "$work/this/internal/versions/versions.yaml"
build "$work/this" v0.0.5 "$out/v5"

go build -o "$out/releasetool" ./deploy/releasetool
echo "built prev (from $prev_ref), v2, v4, v5 and releasetool in $out"

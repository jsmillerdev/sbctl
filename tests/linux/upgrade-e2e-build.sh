#!/usr/bin/env bash
# Builds the binaries that tests/linux/upgrade-e2e.sh upgrades between, into OUT_DIR:
#
#   prev   the previous release: origin/main as this checkout last merged it (the merge base of HEAD
#          and origin/main, which is origin/main itself on a branch that is up to date; this checkout
#          when there is no origin/main) with older GoTrue, PostgREST, postgres-meta and Storage
#          pins, as v0.0.1. A main that moved on without this checkout would render the units
#          differently and restart every project at the first start of the new daemon, which is a
#          fact about main and not what the test is about.
#   v2     this checkout, as v0.0.2: the release the node is upgraded to
#   v4     this checkout, as v0.0.4, pinning a PostgREST release whose launcher the test makes exit
#   v5     this checkout, as v0.0.5, pinning a newer GoTrue release
#   v6     this checkout, as v0.0.6, with one PostgREST setting (PGRST_DB_POOL) rendered differently
#          and the pins of v2: a release that moves no service and still restarts every project's PostgREST
#   v7     this checkout, as v0.0.10, pinning the GoTrue release of v5 and rendering the PostgreSQL units of
#          the projects differently (max_connections of the default class is 61): a release that restarts
#          every project's database, which the daemon leaves running and the rollout restarts
#   v8     this checkout, as v0.0.11, with the pins of v2: the release `supavise update run` (the maintenance
#          window's timer) upgrades the node to by itself, from v0.0.6, which is also a build of this checkout
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
if git fetch --no-tags origin main 2>/dev/null; then
  prev_ref=$(git merge-base HEAD FETCH_HEAD 2>/dev/null || git rev-parse FETCH_HEAD)
fi
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

# v6: the pins of this checkout, and a PostgREST environment variable the units render differently.
cp internal/versions/versions.yaml "$work/this/internal/versions/versions.yaml"
cp internal/versions/versions.yaml "$out/v6.versions.yaml"
python3 - "$work/this/internal/lifecycle/env.go" <<'PY'
import re, sys
path = sys.argv[1]
s = open(path).read()
s, n = re.subn(r'("PGRST_DB_POOL":\s+)"5"', r'\1"6"', s)
if n != 1:
    sys.exit("env.go has no PGRST_DB_POOL setting of 5 to change")
open(path, "w").write(s)
PY
build "$work/this" v0.0.6 "$out/v6"

# v7: the GoTrue pin of v5, and a project's PostgreSQL unit rendered differently (the system cluster has
# its own class and renders as before).
git -C "$work/this" checkout -- internal/lifecycle/env.go
cp "$out/v5.versions.yaml" "$out/v7.versions.yaml"
cp "$out/v7.versions.yaml" "$work/this/internal/versions/versions.yaml"
python3 - "$work/this/internal/lifecycle/classes.go" <<'PY'
import re, sys
path = sys.argv[1]
s = open(path).read()
s, n = re.subn(r'(newSize\("micro", "ci_micro", "Micro", 1024, 100, true, )60(, 5, 200\))', r'\g<1>61\2', s)
if n != 1:
    sys.exit("classes.go has no micro class with 60 connections to change")
open(path, "w").write(s)
PY
build "$work/this" v0.0.10 "$out/v7"

# v8: this checkout as it is.
git -C "$work/this" checkout -- internal/lifecycle/classes.go
cp internal/versions/versions.yaml "$work/this/internal/versions/versions.yaml"
cp internal/versions/versions.yaml "$out/v8.versions.yaml"
build "$work/this" v0.0.11 "$out/v8"

go build -o "$out/releasetool" ./deploy/releasetool
echo "built prev (from $prev_ref), v2, v4, v5, v6, v7, v8 and releasetool in $out"

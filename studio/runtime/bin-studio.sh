#!/bin/sh
# shellcheck shell=sh

# Launcher of the sbctl platform-mode Studio artifact. Identical contract to bin/studio of the
# slim-services studio artifact: a POSIX shell script that sources the profile next to it, then
# runs the bundled node on apps/studio/docker-entrypoint.mjs with cwd = app/. Configuration is
# environment only (PORT, HOSTNAME, and the per-install values listed in placeholders.json).
case "$0" in
  */*) SLIM_RUNTIME_DIR=${0%/*} ;;
  *) SLIM_RUNTIME_DIR=. ;;
esac
SLIM_RUNTIME_DIR="$(CDPATH='' cd -- "$SLIM_RUNTIME_DIR" && pwd -P)"
SLIM_RUNTIME_PROFILE="$SLIM_RUNTIME_DIR/.runtime-env.sh"
if [ -r "$SLIM_RUNTIME_PROFILE" ]; then
  # shellcheck disable=SC1090 # The profile is generated beside each launcher.
  . "$SLIM_RUNTIME_PROFILE"
fi
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
NODE_BIN="${SUPABASE_NODE:-$SCRIPT_DIR/../node/bin/node}"
cd "$SCRIPT_DIR/../app"
exec "$NODE_BIN" apps/studio/docker-entrypoint.mjs "$@"

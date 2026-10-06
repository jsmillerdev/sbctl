#!/bin/sh
# Re-pin the three Supabase Management API specs under internal/api/gen/specs/.
# Not part of `go generate`: run it on purpose, review the diff, then `go generate`.
set -eu
cd "$(dirname "$0")"
for n in v1 v2 platform; do
  curl -fsSL "https://api.supabase.com/api/$n-json" -o "specs/$n.json.tmp"
  mv "specs/$n.json.tmp" "specs/$n.json"
done

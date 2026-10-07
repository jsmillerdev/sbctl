#!/usr/bin/env bash
# Workstream A: builds upstream Studio in platform mode, applies studio/patches/*.patch, and packages
# it like the slim-services studio artifact (app/ bin/ node/ share/), so the systemd unit is unchanged.
#
#   studio/build.sh <platform>
#
# platform: linux-amd64 or linux-arm64 (the host must match; there is no cross build, because Next
# traces native modules such as sharp for the build host). darwin-arm64 works for local packaging
# checks together with STUDIO_PREBUILT; the real build needs ~8 GB RAM and is meant for CI
# (see studio/ci-prepare.sh and studio/README.md).
#
# Output: $STUDIO_OUT (default studio/dist)/sbctl-studio-<tag>-p<N>-<platform>.tar.zst plus a line in
# SHA256SUMS next to it.
#
# Environment (all optional):
#   STUDIO_COMMIT         full upstream commit to build (default: the pin below, checked against versions.yaml)
#   STUDIO_WORK           scratch directory (default studio/.build-cache/work-<platform>)
#   STUDIO_CACHE          download cache for node (default studio/.build-cache/dl)
#   STUDIO_OUT            output directory (default studio/dist)
#   STUDIO_BUILD_HEAP_MB  V8 heap cap per Next build process (default 4096)
#   STUDIO_BUILD_WORKERS  Next static-generation workers (default 1; Next's own default is CPU count - 1)
#   STUDIO_PREBUILT       an existing apps/studio directory that already holds .next/standalone, .next/static
#                         and public/: skip fetch, install and build, only package (used by tests)
#   STUDIO_PREBUILT_PLACEHOLDERS  placeholders.json that matches STUDIO_PREBUILT (default placeholders.json)
#   STUDIO_NODE_DIR       use this node distribution (needs bin/node) instead of downloading one
#   STUDIO_UNTIL=prune    stop after fetch, patch, prune and the lockfile check (a light preflight)
#   STUDIO_KEEP_WORK=1    keep the scratch directory
#   STUDIO_SKIP_VERIFY=1  do not start the packaged build to check it
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"

# ---- pins -------------------------------------------------------------------------------------
# Full commit of the upstream tag in versions.yaml (studio.tag = 2026.10.05-sha-94b8b06). The short
# sha in the tag must be a prefix of this value; bump both together.
PINNED_COMMIT=94b8b06eb294cf6b217c68d30357566cc8f146d9
# Build and runtime toolchain. NODE_VERSION is the latest 22.x at the time of writing; the slim
# artifact of the same Studio runs on 22.20.0. PNPM_VERSION and TURBO_VERSION are what upstream's
# apps/studio/Dockerfile uses at this commit.
NODE_VERSION=22.23.3
PNPM_VERSION=11.13.1
TURBO_VERSION=2.9.14
node_sha256() {
  case "$1" in
    linux-amd64)  echo 1084aa36196bba4c3a5e69a1ee388a6e4ff729dad09445fbcd434b28fe3c24af ;;
    linux-arm64)  echo 5ced2d48d1d7198739b7f86804de0171aefb6823b684b12341d3321afc3cb0b2 ;;
    darwin-arm64) echo 23b25245dcfb9af7262f8ff142e9e2e0af025368117329e7a7458a51e5922f53 ;;
    *) return 1 ;;
  esac
}
node_dist() { # platform -> nodejs.org name
  case "$1" in
    linux-amd64) echo linux-x64 ;; linux-arm64) echo linux-arm64 ;; darwin-arm64) echo darwin-arm64 ;;
  esac
}

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'build.sh: %s\n' "$*" >&2; exit 1; }

# copytree SRC/. DST: recursive copy that keeps symlinks and uses copy-on-write clones when the
# filesystem has them (APFS, btrfs, xfs), so local packaging tests do not duplicate the tree.
copytree() {
  if [[ "$(uname -s)" == Darwin ]]; then cp -cR "$1" "$2" 2>/dev/null || cp -R "$1" "$2"
  else cp -R --reflink=auto "$1" "$2"; fi
}

sha256_of() { # file -> hex
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# ---- arguments and preflight ------------------------------------------------------------------
PLATFORM="${1:-}"
[[ -n "$PLATFORM" ]] || die "usage: studio/build.sh <linux-amd64|linux-arm64>"
node_sha256 "$PLATFORM" >/dev/null || die "unsupported platform: $PLATFORM"

host_arch() {
  case "$(uname -s)-$(uname -m)" in
    Linux-x86_64) echo linux-amd64 ;; Linux-aarch64|Linux-arm64) echo linux-arm64 ;;
    Darwin-arm64) echo darwin-arm64 ;; *) echo unknown ;;
  esac
}
[[ "$(host_arch)" == "$PLATFORM" ]] || die "host is $(host_arch); build $PLATFORM on a $PLATFORM machine (no cross builds)"

for tool in tar zstd curl; do command -v "$tool" >/dev/null 2>&1 || die "missing tool: $tool"; done
[[ -n "${STUDIO_PREBUILT:-}" ]] || for tool in git python3; do command -v "$tool" >/dev/null 2>&1 || die "missing tool: $tool"; done

TAG="$(awk '/^studio:/{s=1;next} s&&/^[^ ]/{s=0} s&&/^[ ]+tag:/{print $2; exit}' "$REPO/versions.yaml")"
[[ -n "$TAG" ]] || die "studio.tag not found in versions.yaml"
SHORT_SHA="${TAG##*-sha-}"
COMMIT="${STUDIO_COMMIT:-$PINNED_COMMIT}"
case "$COMMIT" in "$SHORT_SHA"*) ;; *) die "commit $COMMIT does not match versions.yaml studio.tag $TAG; update PINNED_COMMIT in studio/build.sh" ;; esac
PATCHSET="$(tr -d '[:space:]' < "$HERE/PATCHSET")"
ARTIFACT="sbctl-studio-${TAG}-p${PATCHSET}-${PLATFORM}.tar.zst"

WORK="${STUDIO_WORK:-$HERE/.build-cache/work-$PLATFORM}"
CACHE="${STUDIO_CACHE:-$HERE/.build-cache/dl}"
OUT="${STUDIO_OUT:-$HERE/dist}"
HEAP_MB="${STUDIO_BUILD_HEAP_MB:-4096}"
WORKERS="${STUDIO_BUILD_WORKERS:-1}"
[[ "$WORKERS" =~ ^[1-9][0-9]*$ ]] || die "STUDIO_BUILD_WORKERS must be a positive integer, got '$WORKERS'"
mkdir -p "$WORK" "$CACHE" "$OUT"
touch "$WORK/.sbctl-studio-work"   # cleanup below only removes a directory carrying this marker

free_gb() { df -Pk "$1" | awk 'NR==2 {printf "%d", $4/1048576}'; }
if [[ -z "${STUDIO_PREBUILT:-}" ]]; then
  need=9
  [[ "$(free_gb "$WORK")" -ge $need ]] || die "need $need GB free in $WORK, have $(free_gb "$WORK") (run studio/ci-prepare.sh on a CI runner)"
  if [[ -r /proc/meminfo ]]; then
    mem_kb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo); swap_kb=$(awk '/^SwapTotal:/{print $2}' /proc/meminfo)
    if [[ $((mem_kb + swap_kb)) -lt $((11 * 1024 * 1024)) ]]; then
      printf 'warning: %d MB RAM + %d MB swap; next build peaks near 8 GB. Run studio/ci-prepare.sh to add swap.\n' \
        $((mem_kb / 1024)) $((swap_kb / 1024)) >&2
    fi
  fi
fi

log "building $ARTIFACT from supabase/supabase@$COMMIT"

# ---- toolchain: pinned node, pnpm, turbo ------------------------------------------------------
TOOLS="$WORK/tools"
NODE_DIR="${STUDIO_NODE_DIR:-}"
if [[ -z "$NODE_DIR" ]]; then
  dist="node-v${NODE_VERSION}-$(node_dist "$PLATFORM")"
  tarball="$CACHE/$dist.tar.gz"
  if [[ ! -f "$tarball" ]]; then
    log "downloading node v$NODE_VERSION"
    curl -fsSL --retry 3 -o "$tarball.part" "https://nodejs.org/dist/v${NODE_VERSION}/$dist.tar.gz"
    mv "$tarball.part" "$tarball"
  fi
  [[ "$(sha256_of "$tarball")" == "$(node_sha256 "$PLATFORM")" ]] || { rm -f "$tarball"; die "node tarball checksum mismatch"; }
  NODE_DIR="$TOOLS/$dist"
  if [[ ! -x "$NODE_DIR/bin/node" ]]; then
    mkdir -p "$TOOLS"
    if [[ -n "${STUDIO_PREBUILT:-}" ]]; then   # packaging only: the node binary and its license are enough
      tar -C "$TOOLS" -xzf "$tarball" "$dist/bin/node" "$dist/LICENSE"
    else
      tar -C "$TOOLS" -xzf "$tarball"         # the build also needs npm, to install pnpm
    fi
  fi
fi
[[ -x "$NODE_DIR/bin/node" ]] || die "no node binary at $NODE_DIR/bin/node"
export PATH="$NODE_DIR/bin:$PATH"
log "node $(node --version)"

# ---- fetch, patch, install, build -------------------------------------------------------------
if [[ -z "${STUDIO_PREBUILT:-}" ]]; then
  export NEXT_TELEMETRY_DISABLED=1 TURBO_TELEMETRY_DISABLED=1 DO_NOT_TRACK=1 CI=1 npm_config_update_notifier=false
  PNPM_HOME_DIR="$TOOLS/pnpm"
  if [[ ! -x "$PNPM_HOME_DIR/node_modules/.bin/pnpm" ]]; then
    log "installing pnpm@$PNPM_VERSION"
    npm install --silent --no-audit --no-fund --prefix "$PNPM_HOME_DIR" "pnpm@$PNPM_VERSION"
  fi
  export PATH="$PNPM_HOME_DIR/node_modules/.bin:$PATH"

  SRC="$WORK/src"
  APP="$WORK/prune"
  # Fetch when the marker is missing or stale, or when the source tree was consumed by an earlier
  # "turbo prune" (apps/ and packages/ are deleted afterwards) and the pruned copy is gone too.
  if [[ ! -f "$SRC/.sbctl-fetched" || "$(cat "$SRC/.sbctl-fetched")" != "$COMMIT+p$PATCHSET" \
        || ( ! -d "$SRC/apps/studio" && ! -d "$APP/apps/studio" ) ]]; then
    log "sparse fetch of $COMMIT"
    rm -rf "$SRC" "$APP"; mkdir -p "$SRC"   # a pruned copy of an older fetch would be stale
    git -C "$SRC" init -q
    git -C "$SRC" remote add origin https://github.com/supabase/supabase.git
    git -C "$SRC" config user.name sbctl; git -C "$SRC" config user.email sbctl@users.noreply.invalid
    # Only what Studio's dependency graph needs: root manifests and lockfile, patches/, every
    # package (small, internal deps of studio), studio itself and the manifests of the other
    # workspace projects so the frozen lockfile still matches.
    git -C "$SRC" sparse-checkout init --no-cone
    git -C "$SRC" sparse-checkout set \
      '/*' '!/apps/' '!/packages/' '!/blocks/' '!/e2e/' '!/examples/' '!/i18n/' '!/docker/' '!/supabase/' \
      '/apps/studio/' '/packages/' '/patches/' '/apps/*/package.json' '/blocks/*/package.json' '/e2e/*/package.json'
    git -C "$SRC" fetch -q --depth 1 --filter=blob:none origin "$COMMIT"
    git -C "$SRC" checkout -q --detach FETCH_HEAD
    [[ "$(git -C "$SRC" rev-parse HEAD)" == "$COMMIT" ]] || die "fetched commit is not $COMMIT"

    log "applying patches"
    git -C "$SRC" am --3way "$HERE"/patches/*.patch
    echo "$COMMIT+p$PATCHSET" > "$SRC/.sbctl-fetched"
  fi

  # The platform build must not inherit the self-hosted defaults that upstream keeps in a dotenv
  # file next to Studio (Next loads it at build time and would bake its URLs into the bundle and
  # the CSP). Everything is passed explicitly below instead.
  UPSTREAM_DOTENV="$SRC/apps/studio/.e""nv"
  rm -f "$UPSTREAM_DOTENV"

  if [[ ! -d "$APP/apps/studio" ]]; then
    log "turbo prune studio"
    rm -rf "$APP"
    (cd "$SRC" && pnpm dlx "turbo@$TURBO_VERSION" prune studio --out-dir "$APP")
    cp -a "$SRC/patches" "$APP/patches"
    cp "$SRC/LICENSE" "$WORK/UPSTREAM-LICENSE"
    rm -rf "$SRC/apps" "$SRC/packages"   # frees ~200 MB; the pruned copy is the build tree now
  fi

  # Cheap early failure: the pruned lockfile must satisfy the pruned manifests.
  log "lockfile check"
  (cd "$APP" && pnpm install --frozen-lockfile --lockfile-only --reporter=append-only)
  if [[ "${STUDIO_UNTIL:-}" == prune ]]; then log "stopping after prune (STUDIO_UNTIL=prune)"; exit 0; fi

  log "pnpm install (frozen lockfile)"
  (cd "$APP" && pnpm install --frozen-lockfile --filter 'studio...' \
      --store-dir "$WORK/pnpm-store" --network-concurrency 8 --child-concurrency 2 --reporter=append-only)

  # Placeholder build-time values from placeholders.json, then the fixed platform settings. The
  # build must not pick up anything from the runner's environment that Studio would inline.
  log "next build (platform mode)"
  for v in $(compgen -e | grep -E '^(NEXT_PUBLIC_|SUPABASE_|VERCEL|SENTRY_|CSP_|NIMBUS_|NEXT_RUNTIME|MAINTENANCE_MODE|ANALYZE|FORCE_ASSET_CDN)' || true); do unset "$v"; done
  while IFS='=' read -r key value; do export "$key=$value"; done < <(
    node -e '
      const s = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
      for (const v of s.values) console.log(v.env + "=" + v.placeholder);
    ' "$HERE/placeholders.json")
  export NEXT_PUBLIC_IS_PLATFORM=true
  # prod: hides internal-only UI and keeps consent-gated telemetry off. Never local or staging,
  # which auto-grant telemetry consent (research/05 section 4.5).
  export NEXT_PUBLIC_ENVIRONMENT=prod
  export STUDIO_FRAMEWORK=next
  # Heap cap for each node process (the next build driver and its prerender workers). Turbopack's
  # own memory is native and not covered by it; peak RSS of the whole build is about 8 GB, which
  # is why ci-prepare.sh adds swap. The cap is a ceiling, not a reservation.
  export NODE_OPTIONS="--max-old-space-size=$HEAP_MB"
  # Worker count. Next computes experimental.cpus as max(1, (CIRCLE_NODE_TOTAL || os.cpus().length) - 1)
  # (next@16.3.6 dist/server/config-shared.js), so on a bigger host it would start N-1 workers that
  # each may grow to the heap cap above. Studio's next.config does not set cpus. CIRCLE_NODE_TOTAL
  # is Next's own override for this, so WORKERS + 1 yields exactly WORKERS workers on any host.
  export CIRCLE_NODE_TOTAL=$((WORKERS + 1))
  time_cmd=()
  if [[ -x /usr/bin/time ]] && /usr/bin/time -v true >/dev/null 2>&1; then time_cmd=(/usr/bin/time -v -o "$WORK/next-build.time"); fi
  (cd "$APP" && ${time_cmd[@]+"${time_cmd[@]}"} pnpm --filter studio exec next build)
  if [[ -f "$WORK/next-build.time" ]]; then
    awk -F': ' '/Maximum resident set size|Elapsed \(wall clock\)/ {print "next build: " $0}' "$WORK/next-build.time" >&2
  fi
  unset NODE_OPTIONS CIRCLE_NODE_TOTAL
  # The verify step starts Studio from this shell. The build-time placeholders must not leak into
  # it as if they were configuration (CI run: NEXT_PUBLIC_DISABLED_FEATURES was still
  # "sbctl-placeholder-disabled-features" and the entrypoint refused it).
  for v in $(compgen -e | grep -E '^(NEXT_PUBLIC_|CSP_)' || true); do unset "$v"; done
  unset STUDIO_FRAMEWORK

  PREBUILT="$APP/apps/studio"
  PLACEHOLDERS="$HERE/placeholders.json"
  LICENSE_FILE="$WORK/UPSTREAM-LICENSE"
else
  PREBUILT="$STUDIO_PREBUILT"
  PLACEHOLDERS="${STUDIO_PREBUILT_PLACEHOLDERS:-$HERE/placeholders.json}"
  LICENSE_FILE="${STUDIO_LICENSE_FILE:-}"
fi
[[ -f "$PREBUILT/.next/standalone/apps/studio/server.js" ]] || die "no standalone output in $PREBUILT/.next/standalone"

# ---- package ----------------------------------------------------------------------------------
log "packaging"
STAGE="$WORK/stage"
rm -rf "$STAGE"; mkdir -p "$STAGE/app" "$STAGE/bin" "$STAGE/node/bin" "$STAGE/share/licenses/app" "$STAGE/share/sbctl"

# app/: Next's standalone output is the app root; static assets and public/ go alongside it.
# (Same assembly as upstream's apps/studio/Dockerfile.) cp -R keeps the symlinks pnpm and Next use.
copytree "$PREBUILT/.next/standalone/." "$STAGE/app/"
mkdir -p "$STAGE/app/apps/studio/.next"
copytree "$PREBUILT/.next/static" "$STAGE/app/apps/studio/.next/static"
copytree "$PREBUILT/public" "$STAGE/app/apps/studio/public"
rm -rf "$STAGE/app/apps/studio/.next/cache"
# The standalone output may carry a dotenv from the build tree; the platform artifact ships none.
rm -f "$STAGE/app/apps/studio/.e""nv" "$STAGE/app/.e""nv"

# node/: the bundled runtime, same place the slim launcher looks (node/bin/node).
cp "$NODE_DIR/bin/node" "$STAGE/node/bin/node"
if [[ -f "$NODE_DIR/LICENSE" ]]; then cp "$NODE_DIR/LICENSE" "$STAGE/share/licenses/NODE-LICENSE"; fi

# bin/: launcher and profile (identical contract to the slim artifact).
cp "$HERE/runtime/bin-studio.sh" "$STAGE/bin/studio"
cp "$HERE/runtime/runtime-env.sh" "$STAGE/bin/.runtime-env.sh"
chmod 755 "$STAGE/bin/studio" "$STAGE/node/bin/node"
cp "$HERE/runtime/docker-entrypoint.mjs" "$HERE/runtime/sbctl-runtime-config.mjs" "$STAGE/app/apps/studio/"
chmod 755 "$STAGE/app/apps/studio/docker-entrypoint.mjs"

# Runtime substitution: find every file that holds a placeholder, keep a pristine copy of each,
# and record the list. Fails if a placeholder did not survive the build.
log "placeholder scan"
node "$HERE/runtime/prepare-cli.mjs" "$PLACEHOLDERS" "$STAGE/app" "$STAGE/share/sbctl/runtime-config.json"

# share/: licenses and build info.
if [[ -n "$LICENSE_FILE" && -f "$LICENSE_FILE" ]]; then cp "$LICENSE_FILE" "$STAGE/share/licenses/UPSTREAM-LICENSE"; fi
if [[ -f "$REPO/LICENSE" ]]; then cp "$REPO/LICENSE" "$STAGE/share/licenses/SBCTL-LICENSE"; fi
mkdir -p "$STAGE/share/sbctl/patches"; cp "$HERE"/patches/*.patch "$STAGE/share/sbctl/patches/"
node -e '
  const fs = require("fs");
  const [out, tag, commit, patchset, platform, nodev, pnpmv] = process.argv.slice(1);
  fs.writeFileSync(out, JSON.stringify({
    service: "sbctl-studio", upstream_tag: tag, upstream_commit: commit, upstream_repository: "https://github.com/supabase/supabase",
    patchset: Number(patchset), platform, node_version: nodev, pnpm_version: pnpmv,
    framework: "next", env: { NEXT_PUBLIC_IS_PLATFORM: "true", NEXT_PUBLIC_ENVIRONMENT: "prod" },
    entrypoint: "bin/studio", cmd: ["node/bin/node", "apps/studio/server.js"],
    runtime_config: "share/sbctl/runtime-config.json"
  }, null, 1) + "\n");
' "$STAGE/share/sbctl/build-info.json" "$TAG" "$COMMIT" "$PATCHSET" "$PLATFORM" "$(node --version)" "$PNPM_VERSION"

# ---- archive, verify the archive, publish ------------------------------------------------------
log "archive"
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
  tar_opts=(--sort=name --owner=0 --group=0 --numeric-owner --mtime=@0)
else
  tar_opts=(--uid 0 --gid 0 --numeric-owner)
fi
tmp="$OUT/$ARTIFACT.part"
tar "${tar_opts[@]}" -C "$STAGE" -cf - . | zstd -q -19 -T2 -f -o "$tmp"
rm -rf "$STAGE"

if [[ "${STUDIO_SKIP_VERIFY:-}" != "1" ]]; then
  log "verify (extracts the archive and starts Studio on a loopback port)"
  "$HERE/verify.sh" "$tmp" || { rm -f "$tmp"; die "verification failed; no artifact written"; }
fi

mv "$tmp" "$OUT/$ARTIFACT"
sum="$(sha256_of "$OUT/$ARTIFACT")"
touch "$OUT/SHA256SUMS"
grep -v " $ARTIFACT\$" "$OUT/SHA256SUMS" > "$OUT/SHA256SUMS.new" || true
printf '%s  %s\n' "$sum" "$ARTIFACT" >> "$OUT/SHA256SUMS.new"
mv "$OUT/SHA256SUMS.new" "$OUT/SHA256SUMS"

if [[ "${STUDIO_KEEP_WORK:-}" != "1" && -f "$WORK/.sbctl-studio-work" ]]; then rm -rf "$WORK"; fi
log "done: $OUT/$ARTIFACT ($(du -h "$OUT/$ARTIFACT" | awk '{print $1}'))"
echo "$sum  $ARTIFACT"

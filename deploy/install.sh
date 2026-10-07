#!/usr/bin/env bash
# sbctl installer: checks the host, downloads the sbctl release binary, verifies its
# SHA-256 and the ed25519 signature of the release's checksum list, installs it and runs
# `sbctl install`, which does the rest (user, config, units, firewall, system project,
# shared services, sbctl.service) and prints the dashboard URL and the claim token.
#
#   curl -fsSL https://github.com/jsmillerdev/sbctl/releases/latest/download/install.sh | sudo bash -s -- \
#       --domain example.com --dns cloudflare --dns-credentials-file /root/cf.env --email you@example.com
#
# Idempotent: re-running keeps the master key, the registry and every project, and a flag
# you leave out keeps its value in /etc/sbctl/config.toml. Flags this script does not know
# are passed to `sbctl install` (run `sbctl install --help` for them).
#
# Own flags:
#   --binary PATH    install this sbctl binary instead of downloading one (no verification:
#                    you supply the file); --binary-sha256 HEX checks it
#   --version TAG    install this release (default: the latest)
#   --repo OWNER/NAME  GitHub repository to download from (default below)
#   --verify-only    download and verify the release, print what it holds, install nothing
#   -h, --help
#
# Test hooks (environment): SBCTL_INSTALL_BASE_URL replaces https://github.com/<repo>/releases
# and SBCTL_INSTALL_PUBKEY_B64 replaces the embedded release key (base64 of its PEM).
set -euo pipefail

REPO=${SBCTL_INSTALL_REPO:-jsmillerdev/sbctl}
# The release workflow replaces this marker with the base64 of the release public key
# (internal/selfupdate/release_key.pem) in the install.sh it attaches to each release.
RELEASE_PUBKEY_B64=${SBCTL_INSTALL_PUBKEY_B64:-__SBCTL_RELEASE_PUBKEY_B64__}
BIN_PATH=/usr/local/bin/sbctl

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die()  { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: install.sh [own flags] [flags for `sbctl install`]

Own flags:
  --binary PATH      install this sbctl binary instead of downloading one (not verified:
                     you supply the file); --binary-sha256 HEX checks it
  --version TAG      install this release (default: the latest)
  --repo OWNER/NAME  GitHub repository to download from
  --verify-only      download and verify the release, print what it holds, install nothing
  -h, --help

Everything else goes to `sbctl install` (--domain, --dns, --email, --s3-bucket, ...);
run `sbctl install --help` after installing for the list. Re-running keeps secrets.
USAGE
}

# Everything runs inside main, called on the last line, so that bash has parsed the whole
# script before it executes anything: with `curl | bash` a command that reads stdin could
# otherwise swallow the rest of the script.
main() {
  BINARY="" BINARY_SHA="" TAG="" VERIFY_ONLY=0
  PASS=()
  while [[ $# -gt 0 ]]; do
    case $1 in
      -h|--help) usage; exit 0 ;;
      --binary)        [[ $# -ge 2 ]] || die "--binary needs a path"; BINARY=$2; shift 2 ;;
      --binary=*)      BINARY=${1#*=}; shift ;;
      --binary-sha256) [[ $# -ge 2 ]] || die "--binary-sha256 needs a value"; BINARY_SHA=$2; shift 2 ;;
      --binary-sha256=*) BINARY_SHA=${1#*=}; shift ;;
      --version)       [[ $# -ge 2 ]] || die "--version needs a tag"; TAG=$2; shift 2 ;;
      --version=*)     TAG=${1#*=}; shift ;;
      --repo)          [[ $# -ge 2 ]] || die "--repo needs owner/name"; REPO=$2; shift 2 ;;
      --repo=*)        REPO=${1#*=}; shift ;;
      --verify-only)   VERIFY_ONLY=1; shift ;;
      *) PASS+=("$1"); shift ;;
    esac
  done
  BASE_URL=${SBCTL_INSTALL_BASE_URL:-https://github.com/$REPO/releases}

  [[ $(id -u) -eq 0 ]] || die "run as root: curl -fsSL <url>/install.sh | sudo bash -s -- <flags>"
  [[ $(uname -s) == Linux ]] || die "this installs a Linux server; $(uname -s) is not supported"

  # ---- host checks ------------------------------------------------------------------
  case $(uname -m) in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported CPU architecture $(uname -m): amd64 or arm64 is required" ;;
  esac

  [[ -r /etc/os-release ]] || die "cannot read /etc/os-release"
  # Read it in a subshell: it defines VERSION, PRETTY_NAME and more that must not leak here.
  ID=$(. /etc/os-release; printf '%s' "${ID:-}")
  VERSION_ID=$(. /etc/os-release; printf '%s' "${VERSION_ID:-}")
  PRETTY_NAME=$(. /etc/os-release; printf '%s' "${PRETTY_NAME:-}")
  skip_os=0
  for a in "${PASS[@]+"${PASS[@]}"}"; do [[ $a == --skip-os-check ]] && skip_os=1; done
  os_major=${VERSION_ID:-}; os_major=${os_major%%.*}
  case ${ID:-} in
    ubuntu) [[ ${os_major:-0} =~ ^[0-9]+$ && $os_major -ge 24 ]] || [[ $skip_os -eq 1 ]] || die "Ubuntu ${VERSION_ID:-?} is too old: 24.04 or later is required (22.04 ships a polkit that ignores the rule sbctl needs)" ;;
    debian) [[ ${os_major:-0} =~ ^[0-9]+$ && $os_major -ge 12 ]] || [[ $skip_os -eq 1 ]] || die "Debian ${VERSION_ID:-?} is too old: 12 or later is required" ;;
    *) [[ $skip_os -eq 1 ]] || die "${PRETTY_NAME:-this distribution} is not supported: Ubuntu 24.04+ or Debian 12+ is required (--skip-os-check tries anyway)" ;;
  esac

  glibc=$(ldd --version 2>&1 | sed -n 1p | grep -Eo '[0-9]+\.[0-9]+$' || true)
  [[ -n $glibc ]] || die "cannot read the glibc version (is this a glibc system?)"
  if [[ $(printf '%s\n2.35\n' "$glibc" | sort -V | head -n1) != 2.35 ]]; then
    die "glibc $glibc is too old: 2.35 or later is required (the service artifacts need it)"
  fi
  [[ -d /run/systemd/system ]] || die "systemd is not the init system of this host"
  log "host ok: ${PRETTY_NAME:-$ID $VERSION_ID}, $ARCH, glibc $glibc"

  # ---- prerequisites ----------------------------------------------------------------
  need=()
  command -v curl >/dev/null || need+=(curl)
  [[ -e /etc/ssl/certs/ca-certificates.crt ]] || need+=(ca-certificates)
  if [[ -z $BINARY ]]; then
    command -v openssl >/dev/null || need+=(openssl)
    command -v sha256sum >/dev/null || need+=(coreutils)
  fi
  command -v runuser >/dev/null || need+=(util-linux)
  if ((${#need[@]})); then
    log "installing ${need[*]}"
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq || warn "apt-get update failed; trying the install anyway"
    apt-get install -y -qq "${need[@]}"
  fi

  tmp=$(mktemp -d)
  VERIFY_BIN=$(dirname "$BIN_PATH")/.sbctl.verify
  trap 'rm -rf "$tmp"; rm -f "$VERIFY_BIN"' EXIT

  # ---- obtain the binary ------------------------------------------------------------
  STUDIO_ARGS=()
  if [[ -n $BINARY ]]; then
    [[ -f $BINARY ]] || die "--binary $BINARY: no such file"
    if [[ -n $BINARY_SHA ]]; then
      echo "$BINARY_SHA  $BINARY" | sha256sum -c - >/dev/null || die "$BINARY does not match --binary-sha256"
    fi
    SRC=$BINARY
    [[ $VERIFY_ONLY -eq 0 ]] || die "--verify-only checks a release; it cannot be combined with --binary"
  else
    case $RELEASE_PUBKEY_B64 in
      ''|__*) die "this copy of install.sh has no release signing key. Use the install.sh attached to a release, or pass --binary with a file you trust." ;;
    esac
    printf '%s' "$RELEASE_PUBKEY_B64" | base64 -d >"$tmp/release.pem" 2>/dev/null || die "the embedded release key is not valid base64"
    openssl pkey -pubin -in "$tmp/release.pem" -noout 2>/dev/null || die "the embedded release key is not a public key"

    if [[ -z $TAG ]]; then
      # Resolve "latest" to a tag once, so every file below comes from the same release.
      final=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$BASE_URL/latest") || die "cannot reach $BASE_URL/latest"
      TAG=${final##*/}
      [[ $TAG =~ ^v[0-9] ]] || die "could not determine the latest release (got '$TAG'); pass --version vX.Y.Z"
    fi
    rel=$BASE_URL/download/$TAG
    log "release $TAG"
    fetch() { curl -fsSL --retry 3 --connect-timeout 15 -o "$tmp/$1" "$rel/$1" || die "download $rel/$1 failed"; }
    fetch SHA256SUMS
    fetch SHA256SUMS.sig
    # The signature covers SHA256SUMS; the checksums then cover every other file.
    if ! openssl pkeyutl -verify -rawin -pubin -inkey "$tmp/release.pem" -sigfile "$tmp/SHA256SUMS.sig" -in "$tmp/SHA256SUMS" >/dev/null 2>&1; then
      die "the signature of SHA256SUMS does not verify against the release key: refusing to install"
    fi
    log "signature of SHA256SUMS verified"
    asset=sbctl-linux-$ARCH
    line=$(grep -E "^[0-9a-f]{64} [ *]$asset\$" "$tmp/SHA256SUMS" || true)
    [[ -n $line ]] || die "release $TAG lists no $asset"
    fetch "$asset"
    (cd "$tmp" && printf '%s\n' "$line" | sha256sum -c - >/dev/null) || die "$asset does not match its checksum: refusing to install"
    log "$asset matches its checksum"
    # The signature covers the checksums, not the tag in GitHub's metadata. A binary that does not
    # name the tag it was published under is an older signed release attached to a newer tag.
    # Run it from next to its destination, not from $tmp: /tmp is often mounted noexec, and
    # that must not look like a downgrade.
    install -d -m 0755 "$(dirname "$BIN_PATH")"
    install -m 0755 "$tmp/$asset" "$VERIFY_BIN"
    if ! reported=$("$VERIFY_BIN" --version 2>&1 | head -n1) || [[ -z $reported ]]; then
      die "the downloaded $asset does not run on this host (is $(dirname "$BIN_PATH") mounted noexec?)"
    fi
    [[ " $reported " == *" $TAG "* ]] || die "release $TAG ships a binary that reports '$reported': refusing to install (an older signed binary under a newer tag would be a downgrade)"
    rm -f "$VERIFY_BIN"
    SRC=$tmp/$asset
    # Studio ships as a release asset too; hand its URL and checksum (from the verified
    # list) to `sbctl install` unless the caller chose their own.
    studio_given=0
    for a in "${PASS[@]+"${PASS[@]}"}"; do
      case $a in --studio-url|--studio-url=*|--no-studio) studio_given=1 ;; esac
    done
    studio_line=$(grep -E "^[0-9a-f]{64} [ *]sbctl-studio-.*-linux-$ARCH\.tar\.zst\$" "$tmp/SHA256SUMS" | head -n1 || true)
    if [[ $studio_given -eq 0 && -n $studio_line ]]; then
      studio_name=${studio_line##* }; studio_name=${studio_name#\*}
      STUDIO_ARGS=(--studio-url "$rel/$studio_name" --studio-sha256 "${studio_line%% *}")
      log "dashboard build: $studio_name"
    elif [[ $studio_given -eq 0 ]]; then
      warn "release $TAG has no dashboard (Studio) build for $ARCH; the node will run without it"
    fi
    if [[ $VERIFY_ONLY -eq 1 ]]; then
      echo "verified $TAG $asset $(sha256sum "$SRC" | cut -d' ' -f1)"
      exit 0
    fi
  fi

  # ---- install and hand over --------------------------------------------------------
  # Replace the binary atomically: a running sbctl.service keeps its old inode until restarted.
  install -d -m 0755 "$(dirname "$BIN_PATH")"
  install -m 0755 "$SRC" "$BIN_PATH.new"
  mv -f "$BIN_PATH.new" "$BIN_PATH"
  "$BIN_PATH" --version >/dev/null || die "the installed binary does not run on this host"
  log "installed $BIN_PATH ($("$BIN_PATH" --version | head -n1))"

  rm -rf "$tmp"; rm -f "$VERIFY_BIN"; trap - EXIT
  exec "$BIN_PATH" install "${STUDIO_ARGS[@]+"${STUDIO_ARGS[@]}"}" "${PASS[@]+"${PASS[@]}"}"
}

main "$@"

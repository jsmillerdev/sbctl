#!/usr/bin/env bash
# Builds the signed part of a release from a directory of build outputs.
#
#   deploy/release-assets.sh DIST_DIR PRIVATE_KEY.pem PUBLIC_KEY.pem
#
# DIST_DIR holds supavise-linux-amd64, supavise-linux-arm64 and any supavise-studio-*-linux-*.tar.zst.
# The script writes into DIST_DIR:
#   supavise-release.json  the release manifest (version, min_upgrade_from, the pinned Supabase
#                    releases, the host converge revision and the AWS stack revision with the SHA-256
#                    of the template), written when SUPAVISE_RELEASE_TAG is set; deploy/releasetool
#                    makes it, deploy/MIN_UPGRADE_FROM says the oldest version that upgrades straight
#                    to this one. The converge revision is read from the built binary of this machine's
#                    architecture (`supavise release-info`, which the script makes executable first: a
#                    downloaded workflow artifact is not); a binary that cannot run here gives 0 and a
#                    warning, or, with SUPAVISE_REQUIRE_CONVERGE=1, stops the release: set it in the
#                    job that signs a release, because a manifest that says 0 weakens the
#                    host-not-converged gating.
#                    deploy/MIN_PEER_FROM says the oldest release a joined server may run alongside this
#                    one (min_peer_from; SUPAVISE_MIN_PEER_FROM overrides the file) and
#                    SUPAVISE_WAL_COMPAT=false marks a release whose PostgreSQL cannot read the WAL of
#                    the releases before it (wal_compat: false; the servers that hold standbys upgrade first)
#                    SUPAVISE_RELEASETOOL names a built releasetool; without it the script uses `go run`;
#                    SUPAVISE_VERSIONS_FILE names the versions.yaml to read instead of the checkout's
#                    (the upgrade-e2e job signs releases whose binaries pin other versions)
#   install.sh       deploy/install.sh with the public key stamped in
#   supavise.yaml    the CloudFormation template with the public key stamped in (the replica server
#                    verifies the installer with it); with SUPAVISE_RELEASE_TAG set (v1.2.3), its
#                    SupaviseVersion default names that tag instead of "latest", so the template
#                    of a release installs that release
#   supavise-aws-deploy.sh  deploy/aws/deploy.sh, for people with the AWS CLI: the one-command deploy
#                    and the stack update. The public key and the tag are stamped in; `update`
#                    verifies the template it downloads against the signed list with that key
#   SHA256SUMS       sha256sum of every file above (binaries, Studio archives, install.sh, the
#                    template, the script and the manifest), so the signature covers them
#   SHA256SUMS.sig   raw ed25519 signature of SHA256SUMS (what install.sh and `supavise self-update` verify)
# It refuses when the private key is not the one the public key names, and it verifies its own
# signature before it returns. The signing key is the one whose public half is the committed
# internal/selfupdate/release_key.pem (the current key; deploy/README.md, "Rotating the release
# signing key", says when that changes). The release workflow runs it with the repository secret; the
# install-e2e job runs it with a throwaway key so that the same code is tested.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
[[ $# -eq 3 ]] || { echo "usage: $0 DIST_DIR PRIVATE_KEY.pem PUBLIC_KEY.pem" >&2; exit 2; }
abs() { (cd "$(dirname "$1")" && printf '%s/%s' "$(pwd)" "$(basename "$1")"); }
dist=$1 priv=$(abs "$2") pub=$(abs "$3")
[[ -d $dist ]] || { echo "no such directory: $dist" >&2; exit 2; }

der() { openssl pkey -pubin -in "$1" -pubout -outform DER | openssl dgst -sha256 | awk '{print $NF}'; }
openssl pkey -pubin -in "$pub" -noout 2>/dev/null || { echo "$pub is not a PEM public key" >&2; exit 1; }
[[ $(openssl pkey -in "$priv" -pubout -outform DER | openssl dgst -sha256 | awk '{print $NF}') == "$(der "$pub")" ]] \
  || { echo "the signing key does not belong to the public key $pub" >&2; exit 1; }

dist=$(cd "$dist" && pwd)
tag=${SUPAVISE_RELEASE_TAG:-}
if [[ -n $tag ]]; then
  [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "SUPAVISE_RELEASE_TAG $tag is not vMAJOR.MINOR.PATCH[-suffix]" >&2; exit 1; }
fi

cd "$dist"
for f in supavise-linux-amd64 supavise-linux-arm64; do
  [[ -s $f ]] || { echo "missing $f in $dist" >&2; exit 1; }
done
files=(supavise-linux-amd64 supavise-linux-arm64)
for f in supavise-studio-*-linux-*.tar.zst; do [[ -e $f ]] && files+=("$f"); done

# ---- the files that carry the release: they are stamped first, so that the manifest and the
# checksum list describe the bytes that are attached
b64=$(base64 <"$pub" | tr -d '\n')
stamp_key() { # FILE: put the public key in the marker; refuse a file that still has it
  sed "s|__SUPAVISE_RELEASE_PUBKEY_B64__|$b64|" "$1" >"$1.new" && mv "$1.new" "$1"
  if grep -q '__SUPAVISE_RELEASE_PUBKEY_B64__' "$1"; then echo "the key marker is still in $1" >&2; exit 1; fi
}
cp "$here/install.sh" install.sh
stamp_key install.sh
chmod 0755 install.sh
cp "$here/cloudformation/supavise.yaml" supavise.yaml
[[ $(grep -c '__SUPAVISE_RELEASE_PUBKEY_B64__' supavise.yaml) -eq 1 ]] || { echo "supavise.yaml must hold the key marker once (the replica server's user data)" >&2; exit 1; }
stamp_key supavise.yaml
if [[ -n $tag ]]; then
  # The SupaviseVersion parameter is the only place the template says "Default: latest".
  sed "s|^    Default: latest\$|    Default: $tag|" supavise.yaml >supavise.yaml.new && mv supavise.yaml.new supavise.yaml
  [[ $(grep -c "^    Default: $tag\$" supavise.yaml) -eq 1 ]] || { echo "could not stamp $tag into supavise.yaml" >&2; exit 1; }
fi
cp "$here/aws/deploy.sh" supavise-aws-deploy.sh
stamp_key supavise-aws-deploy.sh
if [[ -n $tag ]]; then
  sed "s|__SUPAVISE_RELEASE_TAG__|$tag|" supavise-aws-deploy.sh >supavise-aws-deploy.sh.new && mv supavise-aws-deploy.sh.new supavise-aws-deploy.sh
  if grep -q '__SUPAVISE_RELEASE_TAG__' supavise-aws-deploy.sh; then echo "the tag marker is still in supavise-aws-deploy.sh" >&2; exit 1; fi
fi
chmod 0755 supavise-aws-deploy.sh
files+=(install.sh supavise.yaml supavise-aws-deploy.sh)

if [[ -n $tag ]]; then
  # SUPAVISE_MIN_UPGRADE_FROM overrides the file (the install-e2e job tests a release with a floor).
  min=${SUPAVISE_MIN_UPGRADE_FROM:-$(grep -v '^[[:space:]]*#' "$here/MIN_UPGRADE_FROM" | tr -d '[:space:]')}
  peer=${SUPAVISE_MIN_PEER_FROM:-$(grep -v '^[[:space:]]*#' "$here/MIN_PEER_FROM" | tr -d '[:space:]')}
  wal=${SUPAVISE_WAL_COMPAT:-true}
  case $wal in true | false) ;; *) echo "SUPAVISE_WAL_COMPAT $wal is not true or false" >&2; exit 1 ;; esac
  extra=(-min-peer-from "$peer" "-wal-compat=$wal")
  [[ ${SUPAVISE_REQUIRE_CONVERGE:-} != 1 ]] || extra+=(-require-converge)
  # The binary that reports the host converge revision is the one this machine can run.
  case $(uname -m) in aarch64|arm64) probe=supavise-linux-arm64 ;; *) probe=supavise-linux-amd64 ;; esac
  # upload-artifact and download-artifact do not keep the execute bit, so the binary of the release
  # workflow arrives as 0644 and could not report its revision; the mode of an asset is no one's concern.
  chmod 0755 "$dist/$probe"
  rm -f supavise-release.json
  if [[ -n ${SUPAVISE_RELEASETOOL:-} ]]; then
    "$SUPAVISE_RELEASETOOL" manifest -version "$tag" -min-upgrade-from "$min" "${extra[@]}" -versions "${SUPAVISE_VERSIONS_FILE:-$here/../internal/versions/versions.yaml}" -binary "$dist/$probe" -template "$dist/supavise.yaml" -out supavise-release.json
  else
    (cd "$here/.." && go run ./deploy/releasetool manifest -version "$tag" -min-upgrade-from "$min" "${extra[@]}" -versions "${SUPAVISE_VERSIONS_FILE:-internal/versions/versions.yaml}" -binary "$dist/$probe" -template "$dist/supavise.yaml" -out "$dist/supavise-release.json")
  fi
  [[ -s supavise-release.json ]] || { echo "the release manifest was not written" >&2; exit 1; }
  files+=(supavise-release.json)
else
  rm -f supavise-release.json
  echo "SUPAVISE_RELEASE_TAG is not set: no release manifest (self-update refuses a release without one)" >&2
fi
sha256sum "${files[@]}" >SHA256SUMS
openssl pkeyutl -sign -rawin -inkey "$priv" -in SHA256SUMS -out SHA256SUMS.sig
[[ $(wc -c <SHA256SUMS.sig | tr -d ' ') -eq 64 ]] || { echo "unexpected signature size" >&2; exit 1; }
openssl pkeyutl -verify -rawin -pubin -inkey "$pub" -sigfile SHA256SUMS.sig -in SHA256SUMS >/dev/null \
  || { echo "the new signature does not verify" >&2; exit 1; }
echo "signed ${#files[@]} files; key sha256 $(der "$pub")"

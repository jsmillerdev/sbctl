#!/usr/bin/env bash
# Builds the signed part of a release from a directory of build outputs.
#
#   deploy/release-assets.sh DIST_DIR PRIVATE_KEY.pem PUBLIC_KEY.pem
#
# DIST_DIR holds supavise-linux-amd64, supavise-linux-arm64 and any supavise-studio-*-linux-*.tar.zst.
# The script writes into DIST_DIR:
#   supavise-release.json  the release manifest (version, min_upgrade_from, the pinned Supabase
#                    releases), written when SUPAVISE_RELEASE_TAG is set; deploy/releasetool makes it,
#                    deploy/MIN_UPGRADE_FROM says the oldest version that upgrades straight to this one.
#                    SUPAVISE_RELEASETOOL names a built releasetool; without it the script uses `go run`
#   SHA256SUMS       sha256sum of every file above and the manifest, so the signature covers it
#   SHA256SUMS.sig   raw ed25519 signature of SHA256SUMS (what install.sh and `supavise self-update` verify)
#   install.sh       deploy/install.sh with the public key stamped in
#   supavise.yaml       the CloudFormation template; with SUPAVISE_RELEASE_TAG set (v1.2.3), its
#                    SupaviseVersion default names that tag instead of "latest", so the template
#                    of a release installs that release
#   supavise-aws-deploy.sh  deploy/aws/deploy.sh, the one-command deploy for people with the AWS CLI
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
if [[ -n $tag ]]; then
  # SUPAVISE_MIN_UPGRADE_FROM overrides the file (the install-e2e job tests a release with a floor).
  min=${SUPAVISE_MIN_UPGRADE_FROM:-$(grep -v '^[[:space:]]*#' "$here/MIN_UPGRADE_FROM" | tr -d '[:space:]')}
  rm -f supavise-release.json
  if [[ -n ${SUPAVISE_RELEASETOOL:-} ]]; then
    "$SUPAVISE_RELEASETOOL" manifest -version "$tag" -min-upgrade-from "$min" -versions "$here/../internal/versions/versions.yaml" -out supavise-release.json
  else
    (cd "$here/.." && go run ./deploy/releasetool manifest -version "$tag" -min-upgrade-from "$min" -versions internal/versions/versions.yaml -out "$dist/supavise-release.json")
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

b64=$(base64 <"$pub" | tr -d '\n')
sed "s|__SUPAVISE_RELEASE_PUBKEY_B64__|$b64|" "$here/install.sh" >install.sh
chmod 0755 install.sh
if grep -q '__SUPAVISE_RELEASE_PUBKEY_B64__' install.sh; then echo "the key marker is still in install.sh" >&2; exit 1; fi
cp "$here/cloudformation/supavise.yaml" supavise.yaml
if [[ -n $tag ]]; then
  # The SupaviseVersion parameter is the only place the template says "Default: latest".
  sed "s|^    Default: latest\$|    Default: $tag|" supavise.yaml >supavise.yaml.new && mv supavise.yaml.new supavise.yaml
  [[ $(grep -c "^    Default: $tag\$" supavise.yaml) -eq 1 ]] || { echo "could not stamp $tag into supavise.yaml" >&2; exit 1; }
fi
cp "$here/aws/deploy.sh" supavise-aws-deploy.sh
chmod 0755 supavise-aws-deploy.sh
echo "signed ${#files[@]} files; key sha256 $(der "$pub")"

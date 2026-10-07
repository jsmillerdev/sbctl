#!/usr/bin/env bash
# Builds the signed part of a release from a directory of build outputs.
#
#   deploy/release-assets.sh DIST_DIR PRIVATE_KEY.pem PUBLIC_KEY.pem
#
# DIST_DIR holds sbctl-linux-amd64, sbctl-linux-arm64 and any sbctl-studio-*-linux-*.tar.zst.
# The script writes into DIST_DIR:
#   SHA256SUMS       sha256sum of every file above
#   SHA256SUMS.sig   raw ed25519 signature of SHA256SUMS (what install.sh and `sbctl self-update` verify)
#   install.sh       deploy/install.sh with the public key stamped in
#   sbctl.yaml       the CloudFormation template
# It refuses when the private key is not the one the public key names, and it verifies its own
# signature before it returns. The release workflow runs it with the repository secret; the
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

cd "$dist"
for f in sbctl-linux-amd64 sbctl-linux-arm64; do
  [[ -s $f ]] || { echo "missing $f in $dist" >&2; exit 1; }
done
files=(sbctl-linux-amd64 sbctl-linux-arm64)
for f in sbctl-studio-*-linux-*.tar.zst; do [[ -e $f ]] && files+=("$f"); done
sha256sum "${files[@]}" >SHA256SUMS
openssl pkeyutl -sign -rawin -inkey "$priv" -in SHA256SUMS -out SHA256SUMS.sig
[[ $(wc -c <SHA256SUMS.sig | tr -d ' ') -eq 64 ]] || { echo "unexpected signature size" >&2; exit 1; }
openssl pkeyutl -verify -rawin -pubin -inkey "$pub" -sigfile SHA256SUMS.sig -in SHA256SUMS >/dev/null \
  || { echo "the new signature does not verify" >&2; exit 1; }

b64=$(base64 <"$pub" | tr -d '\n')
sed "s|__SBCTL_RELEASE_PUBKEY_B64__|$b64|" "$here/install.sh" >install.sh
chmod 0755 install.sh
if grep -q '__SBCTL_RELEASE_PUBKEY_B64__' install.sh; then echo "the key marker is still in install.sh" >&2; exit 1; fi
cp "$here/cloudformation/sbctl.yaml" sbctl.yaml
echo "signed ${#files[@]} files; key sha256 $(der "$pub")"

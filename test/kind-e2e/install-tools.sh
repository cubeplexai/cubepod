#!/usr/bin/env bash
# Optional, isolated tool installation. Never switches Docker or kube contexts.
set -euo pipefail
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$HERE/versions.env"
DEST=${1:-$HERE/../../.kind-e2e/tools}
mkdir -p "$DEST"
DEST=$(cd "$DEST" && pwd)
[[ $(uname -s) == Linux ]] || { echo 'Linux required'; exit 1; }
case $(uname -m) in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; *) exit 1 ;; esac
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"
base=https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION
curl -fsSLo kind "$base/kind-linux-$ARCH"
curl -fsSLo kind.sha256 "$base/kind-linux-$ARCH.sha256sum"
echo "$(awk '{print $1}' kind.sha256)  kind" | sha256sum -c -
base=https://dl.k8s.io/release/v1.35.5/bin/linux/$ARCH
curl -fsSLo kubectl "$base/kubectl"
curl -fsSLo kubectl.sha256 "$base/kubectl.sha256"
echo "$(<kubectl.sha256)  kubectl" | sha256sum -c -
archive=helm-v3.19.0-linux-$ARCH.tar.gz
curl -fsSLO "https://get.helm.sh/$archive"
curl -fsSLO "https://get.helm.sh/$archive.sha256sum"
sha256sum -c "$archive.sha256sum"
tar -xzf "$archive" "linux-$ARCH/helm"
install -m 0755 kind kubectl "linux-$ARCH/helm" "$DEST/"
echo "export PATH=$DEST:\$PATH"

#!/usr/bin/env bash
# Build only; no host kernel/module changes and no registry publication.
set -Eeuo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DEST=${1:?usage: build.sh NEW_ARTIFACT_DIRECTORY}
[[ ! -e "$DEST" ]] || { echo 'Refusing existing artifact directory' >&2; exit 1; }
mkdir -p "$DEST"; DEST=$(cd "$DEST" && pwd)
CACHE=${KERNEL_DOWNLOAD_CACHE:-$DEST/downloads}; mkdir -p "$CACHE" "$DEST/sources" "$DEST/custom" "$DEST/boot"
fetch() {
  local url=$1 file=$2 sha=$3
  if [[ ! -f "$file" ]]; then curl --fail --location --retry 2 --max-time 900 "$url" -o "$file"; fi
  printf '%s  %s\n' "$sha" "$file" | sha256sum -c -
}
fetch https://github.com/kata-containers/kata-containers/archive/refs/tags/4.4.0.tar.gz "$CACHE/kata-4.4.0.tar.gz" 649abcdf4377601453955cf77ce77bef939f905883521a6922be8b7262ed7b28
fetch https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.18.62.tar.xz "$CACHE/linux-6.18.62.tar.xz" 490a7aedf1b355c565d3a6c05333ccccb5da82064eca46b590448a7099d7d651
fetch https://github.com/kata-containers/kata-containers/releases/download/4.4.0/kata-go-static-4.4.0-amd64.tar.zst "$CACHE/kata-go-static-4.4.0-amd64.tar.zst" 240723fd052e538acf2c95a0126abda77d41078702a65783ebc2967f4829a0d6
# Only the exact stock config, root disk and Firecracker executable needed below.
tar --zstd -xf "$CACHE/kata-go-static-4.4.0-amd64.tar.zst" -C "$DEST/boot" \
 ./opt/kata/share/kata-containers/config-6.18.62-211 \
 ./opt/kata/share/kata-containers/kata-containers.img \
 ./opt/kata/share/kata-containers/kata-ubuntu-resolute.image ./opt/kata/bin/firecracker
cp "$DEST/boot/opt/kata/share/kata-containers/config-6.18.62-211" "$DEST/custom/stock.config"
printf '%s  %s\n' 0127c096083a883f9701a44249cb343f1c33f137d05f6e6184d1b3b833f26de6 "$DEST/custom/stock.config" | sha256sum -c -
cp "$HERE/nfs.config" "$DEST/custom/nfs.config"
tar -xzf "$CACHE/kata-4.4.0.tar.gz" -C "$DEST/sources"
K="$DEST/sources/kata-containers-4.4.0"
TOOLS="cubepod-guest-kernel-tools:local-$$"; NAME="cubepod-guest-kernel-compile-$$"
! docker inspect "$NAME" >/dev/null 2>&1 || exit 1
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT
# Use the actual Kata 4.4.0 compiler recipe with an immutable base image.
CTX="$DEST/compiler-context"; mkdir -p "$CTX"
cp "$K/tools/packaging/static-build/kernel/Dockerfile" "$CTX/Dockerfile"
sed -i 's|^FROM ubuntu:22.04$|FROM ubuntu:22.04@sha256:f64b2ffc2f10ab8f8505aa56b2664bd846cbb9df87c7e2532e30f7667021c0e6|' "$CTX/Dockerfile"
cp "$K/tools/packaging/static-build/kernel/install_yq.sh" "$CTX/install_yq.sh"
printf '%s\n' 'RUN echo "638c4b251c49201fc94b598834b715f8f1c6e9b1854d2820772d2c79f0289002  /usr/local/bin/yq" | sha256sum -c -' >> "$CTX/Dockerfile"
# Optional Azure/CI Ubuntu mirror and short apt retries, inserted before Kata's apt-get.
# Empty UBUNTU_MIRROR leaves the image's default archive.ubuntu.com sources in place.
python3 - "$CTX/Dockerfile" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
anchor = "COPY install_yq.sh /usr/bin/install_yq.sh\n"
if anchor not in text:
    raise SystemExit("Kata compiler Dockerfile is missing the install_yq COPY; refusing to patch apt sources")
if "99-ci-retries" in text:
    raise SystemExit("compiler Dockerfile already has apt retry config; refusing to double-patch")
insert = """ARG UBUNTU_MIRROR=""
RUN if [ -n \"$UBUNTU_MIRROR\" ]; then \\
      if [ -f /etc/apt/sources.list.d/ubuntu.sources ]; then \\
        sed -i -e \"s|http://archive.ubuntu.com/ubuntu|${UBUNTU_MIRROR}|g\" -e \"s|http://security.ubuntu.com/ubuntu|${UBUNTU_MIRROR}|g\" /etc/apt/sources.list.d/ubuntu.sources; \\
      fi; \\
      if [ -f /etc/apt/sources.list ]; then \\
        sed -i -e \"s|http://archive.ubuntu.com/ubuntu|${UBUNTU_MIRROR}|g\" -e \"s|http://security.ubuntu.com/ubuntu|${UBUNTU_MIRROR}|g\" /etc/apt/sources.list; \\
      fi; \\
    fi && \\
    printf 'Acquire::Retries \"3\";\\nAcquire::http::Timeout \"30\";\\nAcquire::https::Timeout \"30\";\\nAcquire::http::Pipeline-Depth \"0\";\\n' >/etc/apt/apt.conf.d/99-ci-retries
"""
path.write_text(text.replace(anchor, insert + "\n" + anchor, 1))
PY
docker build --build-arg UBUNTU_MIRROR="${UBUNTU_MIRROR:-}" -t "$TOOLS" "$CTX"
printf '%s\n' "$TOOLS" > "$DEST/tools-image.txt"
docker image inspect "$TOOLS" --format '{{.Id}}' > "$DEST/tools-image-id.txt"
docker create --name "$NAME" --network none --cpus=4 --memory=5g --label io.cubepod.guest-kernel=compile \
 -e KBUILD_BUILD_TIMESTAMP='2026-11-23 00:00:00 UTC' -e KBUILD_BUILD_USER=cubepod -e KBUILD_BUILD_HOST=kernel-builder \
 "$TOOLS" bash -ec 'mkdir -p /art/custom; mv /stock.config /nfs.config /art/custom/; mv /kata-4.4.0.tar.gz /linux-6.18.62.tar.xz /art/; exec bash /compile.sh'
for name in stock.config nfs.config; do docker cp "$DEST/custom/$name" "$NAME:/$name"; done
docker cp "$HERE/compile.sh" "$NAME:/compile.sh"
for name in kata-4.4.0.tar.gz linux-6.18.62.tar.xz; do docker cp -L "$CACHE/$name" "$NAME:/$name"; done
docker start -a "$NAME"
[[ $(docker inspect --format '{{.State.ExitCode}}' "$NAME") == 0 ]]
docker cp "$NAME:/art/custom/." "$DEST/custom/"
( cd "$DEST/custom"; sha256sum -c SHA256SUMS )
# The build is source/config pinned, not a promise of bit-identical apt toolchains.
python3 - "$DEST" <<'PY'
import hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1]); c=p/'custom'
sha=lambda n:hashlib.sha256((c/n).read_bytes()).hexdigest()
(c/'manifest.json').write_text(json.dumps({
 'schema':1,'status':'experimental-not-volume-validated','kata_version':'4.4.0',
 'kernel_release':(c/'kernelrelease.txt').read_text().strip(),
 'linux_source_sha256':'490a7aedf1b355c565d3a6c05333ccccb5da82064eca46b590448a7099d7d651',
 'kata_source_sha256':'649abcdf4377601453955cf77ce77bef939f905883521a6922be8b7262ed7b28',
 'kernel_sha256':sha('vmlinux-6.18.62-cubepod-nfs1'),'config_sha256':sha('resolved.config')},indent=2)+'\n')
PY

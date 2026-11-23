#!/usr/bin/env bash
set -Eeuo pipefail
A=/art
echo '649abcdf4377601453955cf77ce77bef939f905883521a6922be8b7262ed7b28  /art/kata-4.4.0.tar.gz' | sha256sum -c -
echo '490a7aedf1b355c565d3a6c05333ccccb5da82064eca46b590448a7099d7d651  /art/linux-6.18.62.tar.xz' | sha256sum -c -
mkdir -p "$A/sources" "$A/custom"
tar -xzf "$A/kata-4.4.0.tar.gz" -C "$A/sources"
K="$A/sources/kata-containers-4.4.0"
cd "$A"
echo '490a7aedf1b355c565d3a6c05333ccccb5da82064eca46b590448a7099d7d651  linux-6.18.62.tar.xz' > linux-6.18.62.tar.xz.sha256
bash "$K/tools/packaging/kernel/build-kernel.sh" -a x86_64 -v 6.18.62 -c "$A/custom/stock.config" -k "$A/sources/linux-nfs" setup
cd "$A/sources/linux-nfs"
scripts/kconfig/merge_config.sh -m .config "$A/custom/nfs.config"
make olddefconfig
cp .config "$A/custom/resolved.config"
for symbol in NFS_FS NFS_V3 SUNRPC LOCKD; do grep -qx "CONFIG_${symbol}=y" .config || exit 1; done
grep -qx 'CONFIG_LOCALVERSION="-cubepod-nfs1"' .config
# kernelrelease depends on generated auto.conf; a pre-build query can still see
# setup's pre-overlay config. Assert the actual compiled release before copying.
make -j"${KERNEL_JOBS:-4}" vmlinux
release=$(make -s kernelrelease)
printf 'COMPILED_KERNEL_RELEASE=%s\n' "$release"
[[ "$release" == 6.18.62-cubepod-nfs1 ]]
cp vmlinux "$A/custom/vmlinux-6.18.62-cubepod-nfs1"
cp System.map "$A/custom/System.map"
make kernelrelease > "$A/custom/kernelrelease.txt"
( cd "$A/custom"; sha256sum stock.config nfs.config resolved.config vmlinux-6.18.62-cubepod-nfs1 > SHA256SUMS )
gcc --version > "$A/custom/compiler.txt"
dpkg-query -W > "$A/custom/compiler-packages.txt"
echo LOCAL_KERNEL_BUILD_PASS

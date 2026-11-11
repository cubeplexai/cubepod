#!/bin/bash
# Prepare one storage node for Longhorn V2 AIO. Run this in the host namespaces
# (see node-prep-job.yaml). It does not restart kubelet unless RESTART_KUBELET=1.
set -euo pipefail

# install.sh injects these. The defaults stay so a re-run on a host that
# already attached /storage/cubepod/longhorn-v2.img does not create a second disk.
# Set the variables before the first install on a machine without that path.
IMG="${CUBEPOD_LONGHORN_IMG:-/storage/cubepod/longhorn-v2.img}"
LINK="${CUBEPOD_LONGHORN_DEV:-/dev/cubepod/longhorn-v2}"
SIZE="${CUBEPOD_LONGHORN_SIZE:-80G}"
PAGES="${CUBEPOD_HUGEPAGES:-1024}"

modprobe nvme-tcp || true
modprobe nvme-fabrics || true
modprobe uio || true
modprobe uio_pci_generic || true
modprobe vfio-pci || true

mkdir -p "$(dirname "$IMG")" /dev/cubepod
if [[ ! -f "$IMG" ]]; then
  truncate -s "$SIZE" "$IMG"
fi

if ! losetup -j "$IMG" | grep -q .; then
  if ! losetup --find --show --direct-io=on "$IMG"; then
    losetup --find --show "$IMG"
  fi
fi

dev="$(losetup -j "$IMG" | head -n1 | cut -d: -f1)"
if [[ -z "$dev" || ! -b "$dev" ]]; then
  echo "loop device for $IMG is missing" >&2
  exit 1
fi
ln -sfn "$dev" "$LINK"

current="$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages)"
if [[ "$current" -lt "$PAGES" ]]; then
  echo "$PAGES" > /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages
fi
got="$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages)"
if [[ "$got" -lt "$PAGES" ]]; then
  echo "allocated ${got} hugepages, need ${PAGES}; memory is fragmented" >&2
  exit 1
fi

install -d /etc/systemd/system
cat > /etc/systemd/system/cubepod-longhorn-loop.service <<EOF
[Unit]
Description=Attach CubePod Longhorn V2 AIO loop disk
After=local-fs.target
Before=kubelet.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash -c 'img="$IMG"; if ! losetup -j "\$img" | grep -q .; then losetup --find --show --direct-io=on "\$img" || losetup --find --show "\$img"; fi; dev=\$(losetup -j "\$img" | head -n1 | cut -d: -f1); mkdir -p /dev/cubepod; ln -sfn "\$dev" "$LINK"'

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable cubepod-longhorn-loop.service

if [[ "${RESTART_KUBELET:-0}" == "1" ]]; then
  systemctl restart kubelet
fi

echo "loop=$dev link=$LINK hugepages=$got restart_kubelet=${RESTART_KUBELET:-0}"
grep -E 'HugePages_Total|HugePages_Free' /proc/meminfo

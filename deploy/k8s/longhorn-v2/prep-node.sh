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

install -d /usr/local/sbin
cat > /usr/local/sbin/cubepod-spdk-sched-idle <<'EOF'
#!/bin/bash
set -euo pipefail

# Longhorn V2 runs spdk_tgt with v2-data-engine-guaranteed-instance-manager-cpu >= 1000
# in a burstable cgroup. Under Linux EEVDF hierarchical scheduling, a 100%-polling
# SCHED_OTHER reactor prevents root-cgroup per-CPU kworker threads from flushing work
# (for example during dmsetup/nvme teardown). Mark reactor_* threads SCHED_IDLE and keep
# a 95% per-reactor cpu.max floor on the spdk_tgt container cgroup.
for pid_dir in /proc/[0-9]*; do
  [[ -r "$pid_dir/cmdline" ]] || continue
  argv0=""
  IFS= read -r -d '' argv0 < "$pid_dir/cmdline" 2>/dev/null || true
  [[ "${argv0##*/}" == "spdk_tgt" ]] || continue
  pid="${pid_dir#/proc/}"

  reactors=0
  for task_dir in "/proc/$pid/task/"*; do
    [[ -r "$task_dir/comm" && -r "$task_dir/stat" ]] || continue
    tcomm="$(cat "$task_dir/comm" 2>/dev/null || true)"
    case "$tcomm" in
      reactor_*)
        reactors=$((reactors + 1))
        tid="${task_dir##*/}"
        policy="$(awk '{print $41}' "$task_dir/stat" 2>/dev/null || true)"
        if [[ "$policy" != "5" ]]; then
          /usr/bin/chrt -i -p 0 "$tid" >/dev/null 2>&1 || true
        fi
        ;;
    esac
  done

  if [[ "$reactors" -gt 0 && -r "/proc/$pid/cgroup" ]]; then
    cg_rel="$(awk -F: '$1 == "0" {print $3}' "/proc/$pid/cgroup" 2>/dev/null || true)"
    cpu_max_file="/sys/fs/cgroup${cg_rel}/cpu.max"
    if [[ -n "$cg_rel" && -w "$cpu_max_file" ]]; then
      want="$((reactors * 95000)) 100000"
      cur="$(cat "$cpu_max_file" 2>/dev/null || true)"
      if [[ "$cur" != "$want" ]]; then
        printf '%s\n' "$want" > "$cpu_max_file" || true
      fi
    fi
  fi
done
EOF
chmod 0755 /usr/local/sbin/cubepod-spdk-sched-idle

cat > /etc/systemd/system/cubepod-spdk-sched-idle.service <<'EOF'
[Unit]
Description=Set SCHED_IDLE and cpu.max guard on Longhorn V2 SPDK reactor threads

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/cubepod-spdk-sched-idle
EOF

cat > /etc/systemd/system/cubepod-spdk-sched-idle.timer <<'EOF'
[Unit]
Description=Periodically ensure SCHED_IDLE and cpu.max on Longhorn V2 SPDK reactors

[Timer]
OnBootSec=5s
OnUnitActiveSec=5s
AccuracySec=1s

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now cubepod-spdk-sched-idle.timer
/usr/local/sbin/cubepod-spdk-sched-idle

if [[ "${RESTART_KUBELET:-0}" == "1" ]]; then
  systemctl restart kubelet
fi

echo "loop=$dev link=$LINK hugepages=$got restart_kubelet=${RESTART_KUBELET:-0}"
grep -E 'HugePages_Total|HugePages_Free' /proc/meminfo

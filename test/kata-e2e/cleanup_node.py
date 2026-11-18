"""Detach only loop files belonging to THIS node's named test directory.

Run before deleting the owning kind node; device-mapper and loops are not
namespaced. Never remove_all, never force-remove a pool or detach all loops.
"""
import json
from pathlib import Path
import re
import subprocess
import sys

cluster = sys.argv[1]
assert re.fullmatch(r'cubepod-[a-z0-9-]+', cluster)
base = Path('/var/lib/cubepod-kata-e2e') / cluster
if not base.exists():
    raise SystemExit(0)
def run(args, **kwargs):
    return subprocess.run(args, check=True, timeout=40, **kwargs)
run(['systemctl', 'stop', 'kubelet'])
# All containers in this checked, newly owned kind node are disposable.
pods = subprocess.check_output(['crictl', 'pods', '-q'], text=True).split()
if pods:
    run(['crictl', 'stopp', *pods])
run(['systemctl', 'stop', 'containerd'])
mounts = subprocess.check_output(['findmnt', '-rn', '-o', 'TARGET'], text=True).splitlines()
for target in sorted(mounts, key=len, reverse=True):
    if target.startswith(('/var/lib/kubelet/pods/', '/var/lib/kubelet/plugins/', '/var/lib/containerd/io.containerd.snapshotter.v1.devmapper/')):
        run(['umount', target])
pool_file = base / 'pool'
if pool_file.exists():
    pool = pool_file.read_text()
    assert pool == cluster + '-pool'
    uuid = subprocess.check_output(['dmsetup', 'info', '-c', '--noheadings', '-o', 'uuid', pool], text=True).strip()
    assert uuid == 'CUBEPOD_E2E_' + cluster, 'Pool ownership UUID mismatch'
    # containerd names thin snapshots with its configured unique pool prefix.
    pool_device = subprocess.check_output(['dmsetup', 'info', '-c', '--noheadings', '--separator', ':', '-o', 'major,minor', pool], text=True).strip()
    table = subprocess.check_output(['dmsetup', 'ls', '--target', 'thin'], text=True)
    for line in table.splitlines():
        name = line.split()[0]
        if name.startswith(pool + '-'):
            mapping = subprocess.check_output(['dmsetup', 'table', name], text=True).split()
            assert mapping[2] == 'thin' and mapping[3] == pool_device, 'Thin device belongs to a different pool'
            run(['dmsetup', '--noudevsync', 'remove', name])
    run(['dmsetup', '--noudevsync', 'remove', pool])
# --associated matches backing inode/device, not an unsafe textual prefix on
# globally shared /dev/loop*. This includes CSI-created raw-volume files.
for path in base.rglob('*'):
    if path.is_file() and not path.is_symlink() and path.resolve().is_relative_to(base):
        devices = subprocess.check_output(['losetup', '--associated', str(path), '--output', 'NAME', '--noheadings'], text=True).split()
        for device in devices:
            assert re.fullmatch(r'/dev/loop\d+', device)
            run(['losetup', '--detach', device])
            print(f'Detached owned loop {device}: {path}')

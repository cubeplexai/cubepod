"""Detach only loop files belonging to THIS node's named test directory.

Run before deleting the owning kind node; device-mapper and loops are not
namespaced. Never remove_all, never force-remove a pool or detach all loops.
"""
from pathlib import Path
import re
import subprocess
import sys


def validate_pool(cluster, pool, uuid):
    assert pool == cluster + '-pool', 'Pool name mismatch'
    assert uuid == 'CUBEPOD_E2E_' + cluster, 'Pool ownership UUID mismatch'


def validate_thin(pool_device, fields):
    assert len(fields) >= 4 and fields[2] == 'thin' and fields[3] == pool_device, 'Thin device belongs to a different pool'


def owned_files(base):
    for path in base.rglob('*'):
        if path.is_file() and not path.is_symlink() and path.resolve().is_relative_to(base.resolve()):
            yield path


def run(args, **kwargs):
    return subprocess.run(args, check=True, timeout=40, **kwargs)


def main(cluster):
    assert re.fullmatch(r'cubepod-[a-z0-9-]+', cluster)
    base = Path('/var/lib/cubepod-kata-e2e') / cluster
    if not base.exists():
        return
    run(['systemctl', 'stop', 'kubelet'])
    # All containers in this checked, newly owned kind node are disposable.
    pods = subprocess.run(['crictl', 'pods', '-q'], capture_output=True, text=True, timeout=40)
    if pods.returncode == 0:
        if pods.stdout.split():
            run(['crictl', 'stopp', *pods.stdout.split()])
    else:
        # A broken snapshotter can disable CRI before any Kata Pod starts.
        # Kill only tasks from THIS owned node's containerd, never host tasks.
        print('CRI unavailable during cleanup:', pods.stderr, flush=True)
        tasks = subprocess.check_output(['ctr', '-n', 'k8s.io', 'tasks', 'list', '-q'], text=True, timeout=40).split()
        for task in tasks:
            run(['ctr', '-n', 'k8s.io', 'tasks', 'kill', '--signal', 'SIGKILL', '--all', task])
    run(['systemctl', 'stop', 'containerd'])
    mounts = subprocess.check_output(['findmnt', '-rn', '-o', 'TARGET'], text=True).splitlines()
    for target in sorted(mounts, key=len, reverse=True):
        if target.startswith(('/var/lib/kubelet/pods/', '/var/lib/kubelet/plugins/', '/var/lib/containerd/io.containerd.snapshotter.v1.devmapper/')):
            run(['umount', target])
    pool_file = base / 'pool'
    if pool_file.exists():
        pool = pool_file.read_text()
        assert pool == cluster + '-pool', 'Pool name mismatch'
        present = subprocess.check_output(['dmsetup', 'info', '-c', '--noheadings', '-o', 'name'], text=True).split()
        if pool in present:
            uuid = subprocess.check_output(['dmsetup', 'info', '-c', '--noheadings', '-o', 'uuid', pool], text=True).strip()
            validate_pool(cluster, pool, uuid)
            # containerd names thin snapshots with its configured unique pool prefix.
            pool_device = subprocess.check_output(['dmsetup', 'info', '-c', '--noheadings', '--separator', ':', '-o', 'major,minor', pool], text=True).strip()
            table = subprocess.check_output(['dmsetup', 'ls', '--target', 'thin'], text=True)
            for line in table.splitlines():
                name = line.split()[0]
                if name.startswith(pool + '-'):
                    mapping = subprocess.check_output(['dmsetup', 'table', name], text=True).split()
                    validate_thin(pool_device, mapping)
                    run(['dmsetup', '--noudevsync', 'remove', name])
            run(['dmsetup', '--noudevsync', 'remove', pool])
    # --associated matches backing inode/device, not an unsafe textual prefix on
    # globally shared /dev/loop*. This includes CSI-created raw-volume files.
    for path in owned_files(base):
        if path.is_file():
            devices = subprocess.check_output(['losetup', '--associated', str(path), '--output', 'NAME', '--noheadings'], text=True).split()
            for device in devices:
                assert re.fullmatch(r'/dev/loop\d+', device)
                run(['losetup', '--detach', device])
                print(f'Detached owned loop {device}: {path}')


if __name__ == "__main__":
    main(sys.argv[1])

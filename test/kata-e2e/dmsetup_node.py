"""Synchronous publication of OWNED thin nodes, not a host udev daemon.

containerd 2.3.1 invokes dmsetup create for every thin snapshot. kind has a
private tmpfs /dev, so host udev cannot publish those paths inside the node.
Only intercept owned thin creates; validate pool UUID and kernel table before
creating a device entry. Never scan/publish arbitrary host devices.
"""
import os
import json
from pathlib import Path
import re
import stat
import subprocess
import sys

REAL = '/usr/sbin/dmsetup'


def validate(cluster, name, uuid, fields, parent):
    assert re.fullmatch(r'cubepod-[a-z0-9-]+', cluster)
    assert re.fullmatch(re.escape(cluster + '-pool-snap-') + r'[0-9]+', name), 'Not an owned thin name'
    assert uuid == 'CUBEPOD_E2E_' + cluster, 'Foreign pool UUID'
    assert len(fields) >= 5 and fields[2] == 'thin' and fields[3] == parent, 'Foreign thin parent'


def query(*args):
    return subprocess.check_output([REAL, *args], text=True, timeout=30).strip()


def main(cluster, args):
    if not args or args[0] != 'create':
        os.execv(REAL, [REAL, *args])
    name = args[1]
    pool = cluster + '-pool'
    base = Path('/var/lib/cubepod-kata-e2e') / cluster
    assert (base / 'pool').read_text() == pool
    uuid = query('info', '-c', '--noheadings', '-o', 'uuid', pool)
    fields = args[args.index('--table') + 1].split()
    validate(cluster, name, uuid, fields, '/dev/mapper/' + pool)
    subprocess.run([REAL, '--noudevsync', *args], check=True, timeout=30)
    # Validate the actual kernel table, not merely caller-supplied arguments.
    parent = query('info', '-c', '--noheadings', '--separator', ':', '-o', 'major,minor', pool)
    uuid = query('info', '-c', '--noheadings', '-o', 'uuid', pool)
    validate(cluster, name, uuid, query('table', name).split(), parent)
    major, minor = query('info', '-c', '--noheadings', '--separator', ':', '-o', 'major,minor', name).split(':')
    device = os.makedev(int(major), int(minor))
    canonical = query('info', '-c', '--noheadings', '-o', 'blkdevname', name)
    assert canonical == 'dm-' + str(int(minor)), 'Unexpected kernel block-device name'
    assert int(major) == int(parent.split(':')[0]), 'Thin and pool device majors differ'
    # Kata resolves source devices through sysfs DEVNAME (/dev/dm-N), not only
    # the friendly mapper alias. Both entries name THIS verified owned mapping.
    for path in (Path('/dev/mapper') / name, Path('/dev') / canonical):
        if path.is_symlink():
            path.unlink()  # Private node /dev; only a verified owned device.
        if not path.exists():
            os.mknod(path, stat.S_IFBLK | 0o600, device)
        actual = path.stat()
        assert stat.S_ISBLK(actual.st_mode) and actual.st_rdev == device
    with (base / 'device-publication.log').open('a') as log:
        log.write(json.dumps({'name': name, 'pool_uuid': uuid, 'parent': parent,
                              'major': int(major), 'minor': int(minor), 'canonical': canonical}) + '\n')


if __name__ == '__main__':
    main(sys.argv[1], sys.argv[2:])

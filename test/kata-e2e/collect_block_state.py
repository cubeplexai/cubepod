"""Read-only FC diagnostics, invoked only inside a verified owned kind node.

Never mount/fsck/resize/discard or inspect an unrelated block device. Stale or
foreign aliases are rejected before blkid/dumpe2fs/blockdev reads. These logs
are diagnostics, not substitutes for the real SDK or strict cleanup assertions.
"""
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time


def command(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=3)
    return {"command": list(args), "rc": result.returncode,
            "stdout": result.stdout, "stderr": result.stderr}


def owned_thin(cluster, name, uuid, table, parent):
    assert re.fullmatch(re.escape(cluster + '-pool-snap-') + r'[0-9]+', name)
    assert uuid.strip() == 'CUBEPOD_E2E_' + cluster
    fields = table.split()
    assert len(fields) == 5 and fields[2] == 'thin' and fields[3] == parent


def collect(cluster):
    assert re.fullmatch(r'cubepod-[a-z0-9-]+', cluster)
    base = Path('/var/lib/cubepod-kata-e2e') / cluster
    pool = (base / 'pool').read_text().strip()
    assert pool == cluster + '-pool'
    uuid = command('/usr/sbin/dmsetup', 'info', '-c', '--noheadings', '-o', 'uuid', pool)
    assert uuid['rc'] == 0 and uuid['stdout'].strip() == 'CUBEPOD_E2E_' + cluster
    parent = command('/usr/sbin/dmsetup', 'info', '-c', '--noheadings', '--separator', ':', '-o', 'major,minor', pool)
    assert parent['rc'] == 0
    parent = parent['stdout'].strip()
    print(json.dumps(command('/usr/sbin/dmsetup', 'status', pool)), flush=True)
    print(json.dumps(command('/usr/sbin/dmsetup', 'table', pool)), flush=True)
    end = time.monotonic() + 45
    for path in sorted(Path('/dev/mapper').glob(pool + '-snap-*')):
        if time.monotonic() >= end:
            print('Diagnostic capture time budget exhausted (not a test success)', flush=True)
            break
        try:
            table = command('/usr/sbin/dmsetup', 'table', path.name)
            if table['rc'] != 0:
                print(json.dumps(table), flush=True)
                continue  # Removed snapshot; never read its stale private alias.
            owned_thin(cluster, path.name, uuid['stdout'], table['stdout'], parent)
            info = command('/usr/sbin/dmsetup', 'info', '-c', '--noheadings', '--separator', ':', '-o', 'major,minor', path.name)
            assert info['rc'] == 0
            major, minor = map(int, info['stdout'].strip().split(':'))
            current = path.stat()
            assert stat.S_ISBLK(current.st_mode) and current.st_rdev == os.makedev(major, minor)
            print(json.dumps(table), flush=True)
            for args in [('blockdev', '--getsize64'), ('blockdev', '--getro'),
                         ('blkid', '-p'), ('dumpe2fs', '-h')]:
                print(json.dumps(command(*args, str(path))), flush=True)
        except (OSError, AssertionError, subprocess.TimeoutExpired) as error:
            print(json.dumps({'rejected_or_unavailable': str(path), 'error': str(error)}), flush=True)


if __name__ == '__main__':
    collect(sys.argv[1])

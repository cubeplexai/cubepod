"""Boot a real pinned Linux guest in FC/CLH, not just /dev/kvm checks.

Run in a disposable, KVM-device-only container mounting the verified Kata bundle
read-only at /opt/kata. The printed marker is assembled INSIDE the guest, so a
terminal echo of the submitted command cannot produce a false positive.
"""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import subprocess
import sys
import tempfile
import time
import tomllib

vmm = sys.argv[1]
assert vmm in ("fc", "clh")
fd = os.open('/dev/kvm', os.O_RDWR | os.O_CLOEXEC)
assert fcntl.ioctl(fd, 0xAE00, 0) == 12
vm = fcntl.ioctl(fd, 0xAE01, 0)
vcpu = fcntl.ioctl(vm, 0xAE41, 0)
for device in (vcpu, vm, fd):
    os.close(device)
config = tomllib.loads(Path(f'/opt/kata/share/defaults/kata-containers/configuration-{vmm}.toml').read_text())
hv = next(iter(config['hypervisor'].values()))
cmdline = 'console=ttyS0 reboot=k panic=1 init=/bin/sh root=/dev/vda1 rootfstype=erofs ro'
with tempfile.TemporaryDirectory(prefix='cubepod-guest-') as directory:
    if vmm == 'fc':
        fc_config = {
            'boot-source': {'kernel_image_path': hv['kernel'], 'boot_args': cmdline},
            'drives': [{'drive_id': 'rootfs', 'path_on_host': hv['image'],
                        'is_root_device': False, 'is_read_only': True}],
            'machine-config': {'vcpu_count': 1, 'mem_size_mib': 256},
        }
        path = Path(directory) / 'firecracker.json'
        path.write_text(json.dumps(fc_config))
        args = [hv['path'], '--no-api', '--config-file', str(path)]
    else:
        args = [hv['path'], '--kernel', hv['kernel'], '--disk', f'path={hv["image"]},readonly=on,image_type=raw',
                '--cpus', 'boot=1', '--memory', 'size=256M', '--cmdline', cmdline,
                '--serial', 'tty', '--console', 'off']
    print(json.dumps({'vmm': vmm, 'argv': args}), flush=True)
    master, slave = pty.openpty()
    proc = subprocess.Popen(args, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)
    output = bytearray()
    deadline = time.monotonic() + 60
    next_input = time.monotonic() + 2
    try:
        while time.monotonic() < deadline:
            if time.monotonic() >= next_input:
                os.write(master, b"printf 'CUBEPOD_%s_%s\\n' LINUX_GUEST BOOT_OK\n")
                next_input = time.monotonic() + 2
            if select.select([master], [], [], 0.2)[0]:
                try:
                    chunk = os.read(master, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output.extend(chunk)
                sys.stdout.buffer.write(chunk)
                sys.stdout.buffer.flush()
            if b'Linux version ' in output and b'CUBEPOD_LINUX_GUEST_BOOT_OK' in output:
                print(f'\nREAL_LINUX_GUEST_PASS vmm={vmm}', flush=True)
                break
            if proc.poll() is not None:
                raise RuntimeError(f'VMM exited early: {proc.returncode}')
        else:
            raise TimeoutError('Linux guest boot/command did not complete in 60 seconds')
        assert b'Linux version ' in output and b'CUBEPOD_LINUX_GUEST_BOOT_OK' in output, 'Guest proof missing'
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)
        os.close(master)

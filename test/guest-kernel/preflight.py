"""Boot the pinned Kata Firecracker guest and prove in-kernel NFS is present.

init=/bin/sh does not mount /proc. The rootfs is erofs and read-only, so proc
is mounted on a writable devtmpfs path. Markers are assembled inside the guest.
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

fd = os.open("/dev/kvm", os.O_RDWR | os.O_CLOEXEC)
assert fcntl.ioctl(fd, 0xAE00, 0) == 12
vm = fcntl.ioctl(fd, 0xAE01, 0)
vcpu = fcntl.ioctl(vm, 0xAE41, 0)
for device in (vcpu, vm, fd):
    os.close(device)

artifact = Path(sys.argv[1]).resolve()
hv = {'kernel': str(artifact/'custom/vmlinux-6.18.62-cubepod-nfs1'),
      'image': str(artifact/'boot/opt/kata/share/kata-containers/kata-containers.img'),
      'path': str(artifact/'boot/opt/kata/bin/firecracker')}
cmdline = "console=ttyS0 reboot=k panic=1 init=/bin/sh root=/dev/vda1 rootfstype=erofs ro"
with tempfile.TemporaryDirectory(prefix="cubepod-nfs-guest-") as directory:
    fc_config = {
        "boot-source": {"kernel_image_path": hv["kernel"], "boot_args": cmdline},
        "drives": [{
            "drive_id": "rootfs",
            "path_on_host": hv["image"],
            "is_root_device": False,
            "is_read_only": True,
        }],
        "machine-config": {"vcpu_count": 1, "mem_size_mib": 256},
    }
    path = Path(directory) / "firecracker.json"
    path.write_text(json.dumps(fc_config))
    args = [hv["path"], "--no-api", "--config-file", str(path)]
    print(json.dumps({"vmm": "fc", "argv": args, "kernel": hv["kernel"]}), flush=True)
    master, slave = pty.openpty()
    proc = subprocess.Popen(args, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)
    output = bytearray()
    deadline = time.monotonic() + 60
    next_input = time.monotonic() + 2
    command = (
        b"uname -r; printf 'CUBEPOD_%s_%s\\n' LINUX_GUEST BOOT_OK; "
        b"mkdir -p /dev/procfs && (mountpoint -q /dev/procfs || mount -t proc proc /dev/procfs) && "
        b"cat /dev/procfs/filesystems && "
        b"if grep -E '^[[:space:]]*(nodev[[:space:]]+)?nfs[[:space:]]*$' /dev/procfs/filesystems >/dev/null; "
        b"then printf 'CUBEPOD_%s_%s\\n' NFS_FS PRESENT; "
        b"else printf 'CUBEPOD_%s_%s\\n' NFS_FS MISSING; fi\n"
    )
    try:
        while time.monotonic() < deadline:
            now = time.monotonic()
            if now >= next_input:
                os.write(master, command)
                next_input = now + 3
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
            if b"CUBEPOD_LINUX_GUEST_BOOT_OK" in output and (
                b"CUBEPOD_NFS_FS_PRESENT" in output or b"CUBEPOD_NFS_FS_MISSING" in output
            ):
                break
            if proc.poll() is not None:
                raise RuntimeError(f"VMM exited early: {proc.returncode}")
        else:
            raise TimeoutError("Linux guest NFS preflight did not complete in 60 seconds")
        text = output.decode("utf-8", "replace")
        assert "6.18.62-cubepod-nfs1" in text.replace("\r", "").splitlines(), "Wrong guest kernel identity"
        if "CUBEPOD_NFS_FS_MISSING" in text or "CUBEPOD_NFS_FS_PRESENT" not in text:
            raise SystemExit("GUEST_KERNEL_NFS_MISSING: /proc/filesystems has no nfs entry")
        print("\nREAL_FIRECRACKER_GUEST_NFS_PASS", flush=True)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)
        os.close(master)

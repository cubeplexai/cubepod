"""Invoked only through the owning kind-node guard in hooks.sh."""
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tomllib

vmm, cluster = sys.argv[1:]
assert vmm in ('fc', 'clh') and re.fullmatch(r'cubepod-[a-z0-9-]+', cluster)
base = Path('/var/lib/cubepod-kata-e2e') / cluster
base.mkdir(parents=True, exist_ok=True)
config = Path(f'/opt/kata/share/defaults/kata-containers/configuration-{vmm}.toml').read_text()
config = re.sub(r'^default_memory\s*=.*$', 'default_memory = 512', config, flags=re.M)
config = re.sub(r'^default_maxvcpus\s*=.*$', 'default_maxvcpus = 1', config, flags=re.M)
config = re.sub(r'^enable_debug\s*=.*$', 'enable_debug = true', config, flags=re.M)
# Firecracker's optional jailer adds host cgroup/chroot assumptions unnecessary
# inside a dedicated privileged kind node. VMM isolation still uses real KVM.
if vmm == 'fc':
    config = re.sub(r'^jailer_path\s*=.*$', 'jailer_path = ""', config, flags=re.M)
Path('/etc/kata-containers').mkdir(exist_ok=True)
Path('/etc/kata-containers/configuration-e2e.toml').write_text(config)
shim = Path('/usr/local/bin/containerd-shim-kata-e2e-v2')
shim.write_text('#!/bin/sh\nexport KATA_CONF_FILE=/etc/kata-containers/configuration-e2e.toml\nexec /opt/kata/bin/containerd-shim-kata-v2 "$@"\n')
shim.chmod(0o755)
original = subprocess.check_output(['containerd', 'config', 'migrate'], text=True)
snapshotter = 'overlayfs'
extra = ''
if vmm == 'fc':
    # Never load modules on the shared host or touch an existing device/pool.
    assert 'device-mapper' in Path('/proc/misc').read_text(), 'Host has no device-mapper; do not modprobe shared host'
    targets = subprocess.check_output(['dmsetup', 'targets'], text=True)
    assert 'thin-pool' in targets, 'Host thin-pool target unavailable; no shared-host changes permitted'
    pool = cluster + '-pool'
    existing = subprocess.run(['dmsetup', 'info', pool], capture_output=True)
    assert existing.returncode != 0, 'Refusing pre-existing thin-pool'
    dm = base / 'dm'; dm.mkdir()
    devices = {}
    for name, size in [('data', 8 * 1024**3), ('meta', 128 * 1024**2)]:
        path = dm / name
        with path.open('xb') as file:
            file.truncate(size)
        device = subprocess.check_output(['losetup', '--find', '--show', str(path)], text=True).strip()
        devices[name] = device
        (base / 'loops.json').write_text(json.dumps(devices))
        associated = subprocess.check_output(['losetup', '--associated', str(path), '--output', 'NAME', '--noheadings'], text=True).split()
        assert associated == [device], f'Loop backing-file identity mismatch: {path}'
    sectors = (8 * 1024**3) // 512
    table = f'0 {sectors} thin-pool {devices["meta"]} {devices["data"]} 128 1024'
    (base / 'pool').write_text(pool)
    subprocess.run(['dmsetup', '--noudevsync', 'create', pool, '--uuid', 'CUBEPOD_E2E_' + cluster, '--table', table], check=True, timeout=30)
    # No udev daemon runs inside kind. Materialize ONLY this newly created,
    # UUID-verified pool's device in the node's private /dev mount.
    info = subprocess.check_output(['dmsetup', 'info', '-c', '--noheadings', '--separator', ':', '-o', 'uuid,major,minor', pool], text=True).strip().split(':')
    info = [field.strip() for field in info]
    assert info[0] == 'CUBEPOD_E2E_' + cluster, 'Pool ownership changed'
    device = os.makedev(int(info[1]), int(info[2]))
    pool_path = Path('/dev/mapper') / pool
    if pool_path.is_symlink():
        pool_path.unlink()
    if not pool_path.exists():
        os.mknod(pool_path, stat.S_IFBLK | 0o600, device)
    actual = pool_path.stat()
    assert stat.S_ISBLK(actual.st_mode) and actual.st_rdev == device
    snapshotter = 'devmapper'
    extra = f'''\n[plugins."io.containerd.snapshotter.v1.devmapper"]
root_path = "/var/lib/containerd/io.containerd.snapshotter.v1.devmapper"
pool_name = "{pool}"
base_image_size = "1GB"
discard_blocks = true
'''
    # Preserve overlayfs while enabling devmapper in the containerd 2.x transfer
    # unpacker: https://github.com/containerd/containerd/issues/11606
    assert not tomllib.loads(original)['plugins']['io.containerd.transfer.v1.local'].get('unpack_config'), 'Unexpected pre-existing transfer unpack configuration'
    extra += '''
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
platform = "linux/amd64"
snapshotter = "overlayfs"
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
platform = "linux/amd64"
snapshotter = "devmapper"
'''
original += f'''
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-{vmm}-e2e]
runtime_type = "io.containerd.kata-e2e.v2"
runtime_path = "/usr/local/bin/containerd-shim-kata-e2e-v2"
privileged_without_host_devices = true
privileged_without_host_devices_all_devices_allowed = true
snapshotter = "{snapshotter}"
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-{vmm}-e2e.options]
ConfigPath = "/etc/kata-containers/configuration-e2e.toml"
'''
Path('/etc/containerd/config.toml').write_text(original + extra)

if vmm == 'fc':
    # Install after creating our pool; containerd creates only child snapshots.
    # Official containerd source: plugins/snapshots/devmapper/dmsetup/dmsetup.go.
    assert Path('/usr/sbin/dmsetup').is_file()
    wrapper = Path('/usr/local/bin/dmsetup')
    assert not wrapper.exists(), 'Refusing an existing dmsetup wrapper'
    wrapper.write_text('#!/bin/sh\nexec /usr/bin/python3 /usr/local/libexec/cubepod-kata-e2e/dmsetup_node.py ' + cluster + ' "$@"\n')
    wrapper.chmod(0o755)

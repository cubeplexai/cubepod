"""Validate the immutable OCI + content checksum deployment contract."""
import hashlib
import json
from pathlib import Path
import re
import sys

RELEASE = '6.18.62-cubepod-nfs1'
IMAGE = r'ghcr\.io/[a-z0-9][a-z0-9-]*/cubepod-fc-kernel@sha256:[a-f0-9]{64}'

def read_pin(path):
    pin=json.loads(Path(path).read_text())
    if pin.get('schema')!=1 or pin.get('kernel_release')!=RELEASE:
        raise ValueError('Unsupported kernel pin schema/release')
    if not re.fullmatch(IMAGE,pin.get('image','')):
        raise ValueError('Expected a GHCR cubepod-fc-kernel image pinned by sha256 digest')
    for field in ('kernel_sha256','config_sha256'):
        if not re.fullmatch('[a-f0-9]{64}',pin.get(field,'')):
            raise ValueError(f'Invalid {field}')
    return pin

def verify(pin,path):
    path=Path(path)
    for field,name in [('kernel_sha256','vmlinux'),('config_sha256','config')]:
        if hashlib.sha256((path/name).read_bytes()).hexdigest()!=pin[field]:
            raise ValueError(f'{name} checksum mismatch')
    manifest=json.loads((path/'manifest.json').read_text())
    for key,expected in {
        'kata_version':'4.4.0',
        'linux_source_sha256':'490a7aedf1b355c565d3a6c05333ccccb5da82064eca46b590448a7099d7d651',
        'kata_source_sha256':'649abcdf4377601453955cf77ce77bef939f905883521a6922be8b7262ed7b28',
    }.items():
        if manifest.get(key)!=expected:
            raise ValueError(f'Unsupported manifest {key}')
    for key in ('kernel_release','kernel_sha256','config_sha256'):
        if manifest.get(key)!=pin[key]:
            raise ValueError(f'Manifest {key} mismatch')
    config=(path/'config').read_text().splitlines()
    for symbol in ('NFS_FS','NFS_V3','SUNRPC','LOCKD'):
        if f'CONFIG_{symbol}=y' not in config:
            raise ValueError(f'{symbol} must be built in')
    if '# CONFIG_MODULES is not set' not in config:
        raise ValueError('Expected the pinned built-in-only Kata module contract')

if __name__=='__main__':
    mode=sys.argv[1]; pin=read_pin(sys.argv[2])
    if mode=='read': print(pin['image']); print(pin['kernel_release'])
    elif mode=='verify': verify(pin,sys.argv[3]); print('KERNEL_DIGEST_AND_CONTENT_VERIFIED')
    else: raise ValueError('Unsupported mode')

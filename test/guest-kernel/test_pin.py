"""Pure unit tests of the deployment artifact trust boundary, not guest proof."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

source=Path(__file__).resolve().parents[2]/'deploy/k8s/kata/guest-kernel/pin.py'
spec=importlib.util.spec_from_file_location('kernel_pin',source)
pin=importlib.util.module_from_spec(spec); spec.loader.exec_module(pin)

class KernelPinTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)
        self.kernel=b'unit-test-only-not-a-guest'
        self.config='\n'.join(f'CONFIG_{s}=y' for s in ('NFS_FS','NFS_V3','SUNRPC','LOCKD'))+'\n# CONFIG_MODULES is not set\n'
        self.meta={'schema':1,'kernel_release':pin.RELEASE,'image':'ghcr.io/gammakfinn/cubepod-fc-kernel@sha256:'+'a'*64,
                   'kata_version':'4.4.0','linux_source_sha256':'490a7aedf1b355c565d3a6c05333ccccb5da82064eca46b590448a7099d7d651','kata_source_sha256':'649abcdf4377601453955cf77ce77bef939f905883521a6922be8b7262ed7b28',
                   'kernel_sha256':hashlib.sha256(self.kernel).hexdigest(),'config_sha256':hashlib.sha256(self.config.encode()).hexdigest()}
        self.lock=self.root/'pin.json'; self.write()
    def write(self):
        self.lock.write_text(json.dumps(self.meta))
        (self.root/'vmlinux').write_bytes(self.kernel)
        (self.root/'config').write_text(self.config)
        (self.root/'manifest.json').write_text(json.dumps(self.meta))
    def test_valid_pin_and_contents(self):
        self.assertEqual(pin.read_pin(self.lock),self.meta); pin.verify(self.meta,self.root)
    def test_mutable_tag_rejected(self):
        self.meta['image']='ghcr.io/gammakfinn/cubepod-fc-kernel:latest'; self.write()
        with self.assertRaises(ValueError): pin.read_pin(self.lock)
    def test_non_ghcr_rejected(self):
        self.meta['image']=self.meta['image'].replace('ghcr.io','evil.test'); self.write()
        with self.assertRaises(ValueError): pin.read_pin(self.lock)
    def test_wrong_release_rejected(self):
        self.meta['kernel_release']='6.18.62'; self.write()
        with self.assertRaises(ValueError): pin.read_pin(self.lock)
    def test_wrong_schema_rejected(self):
        self.meta['schema']=2; self.write()
        with self.assertRaises(ValueError): pin.read_pin(self.lock)
    def test_missing_digest_rejected(self):
        del self.meta['config_sha256']; self.write()
        with self.assertRaises(ValueError): pin.read_pin(self.lock)
    def test_invalid_digest_rejected(self):
        self.meta['kernel_sha256']='x'*64; self.write()
        with self.assertRaises(ValueError): pin.read_pin(self.lock)
    def test_kernel_tamper_rejected(self):
        (self.root/'vmlinux').write_bytes(b'tampered')
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)
    def test_config_tamper_rejected(self):
        (self.root/'config').write_text('tampered')
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)
    def test_manifest_mismatch_rejected(self):
        other=dict(self.meta,kernel_release='6.18.62')
        (self.root/'manifest.json').write_text(json.dumps(other))
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)
    def test_wrong_kata_manifest_rejected(self):
        self.meta['kata_version']='4.3.0'; self.write()
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)
    def test_wrong_source_manifest_rejected(self):
        self.meta['linux_source_sha256']='0'*64; self.write()
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)
    def test_module_nfs_is_not_builtin(self):
        self.config=self.config.replace('CONFIG_NFS_FS=y','CONFIG_NFS_FS=m')
        self.meta['config_sha256']=hashlib.sha256(self.config.encode()).hexdigest(); self.write()
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)
    def test_modular_kernel_is_not_contract(self):
        self.config=self.config.replace('# CONFIG_MODULES is not set','CONFIG_MODULES=y')
        self.meta['config_sha256']=hashlib.sha256(self.config.encode()).hexdigest(); self.write()
        with self.assertRaises(ValueError): pin.verify(self.meta,self.root)

if __name__=='__main__': unittest.main()

"""Unit checks complement real guest/SDK execution, never replace it."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from cleanup_node import owned_files, validate_pool, validate_thin
from verify_guest_log import guest_ready

class RuntimeProofTest(unittest.TestCase):
    def test_guest_proof_requires_both_exact_kernel_and_output(self):
        for text in ('', '6.18.62\n', 'KATA_GUEST_READY\n', '6.12.113\nKATA_GUEST_READY\n'):
            self.assertFalse(guest_ready(text))
        self.assertTrue(guest_ready('6.18.62\r\nKATA_GUEST_READY\r\n'))

    def verify(self, runtime='kata-clh-e2e', count=12, expected='kata-clh-e2e'):
        events = [{'type':'ADDED', 'object':{'kind':'Pod','metadata':{'name':f'sandbox-{i}'},'spec':{'runtimeClassName':runtime}}} for i in range(count)]
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'events.json'
            path.write_text('\n'.join(json.dumps(e) for e in events))
            return subprocess.run([sys.executable,str(Path(__file__).with_name('verify_pods.py')),str(path),expected],capture_output=True).returncode
    def test_all_sandboxes_use_requested_runtime(self):
        self.assertEqual(self.verify(),0)
    def test_runc_fallback_fails(self):
        self.assertNotEqual(self.verify(runtime='none'),0)
    def test_partial_or_empty_execution_fails(self):
        for count in (0,1,11):self.assertNotEqual(self.verify(count=count),0)



class DeviceOwnershipTest(unittest.TestCase):
    def test_pool_name_and_uuid_must_both_match(self):
        validate_pool('cubepod-own', 'cubepod-own-pool', 'CUBEPOD_E2E_cubepod-own')
        for name, uuid in [('other-pool', 'CUBEPOD_E2E_cubepod-own'), ('cubepod-own-pool', 'FOREIGN')]:
            with self.assertRaises(AssertionError):
                validate_pool('cubepod-own', name, uuid)

    def test_thin_device_parent_must_be_owned_pool(self):
        validate_thin('253:1', ['0', '1000', 'thin', '253:1', '2'])
        with self.assertRaises(AssertionError):
            validate_thin('253:1', ['0', '1000', 'thin', '253:9', '2'])

    def test_symlink_cannot_select_external_backing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            base = root / 'owned'; base.mkdir()
            own = base / 'data'; own.write_text('owned')
            external = root / 'foreign'; external.write_text('never detach')
            (base / 'link').symlink_to(external)
            self.assertEqual(list(owned_files(base)), [own])


if __name__ == '__main__':
    unittest.main()

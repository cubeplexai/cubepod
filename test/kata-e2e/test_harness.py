"""Unit checks complement real guest/SDK execution, never replace it."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

class RuntimeProofTest(unittest.TestCase):
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

if __name__=='__main__':unittest.main()

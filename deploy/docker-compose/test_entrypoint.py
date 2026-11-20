"""Dependency-free regression tests for the shipped POSIX sh entrypoint."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class EntrypointTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.secret = self.root / "state" / "panel-secret"
        self.gateway = self.root / "cubepod-gateway"
        self.gateway.write_text("#!/usr/bin/env python3\nimport hashlib,json,os,sys\nprint(json.dumps([hashlib.sha256(os.environ['CUBEPOD_PANEL_SECRET'].encode()).hexdigest(),sys.argv[1:]]))\n")
        self.gateway.chmod(0o755)
        # Relocate only the hardcoded binary path in an isolated fixture; the
        # shipped entrypoint and secret-generation logic run unchanged under sh.
        script = Path(__file__).with_name("gateway-entrypoint.sh").read_text()
        self.script = self.root / "entrypoint.sh"
        self.script.write_text(script.replace('/usr/local/bin/cubepod-gateway', str(self.gateway)))
        self.env = dict(os.environ, CUBEPOD_PANEL_SECRET="", CUBEPOD_PANEL_SECRET_FILE=str(self.secret))

    def run_entry(self, args=(), env=None):
        return subprocess.run(["sh", str(self.script), *args], env=env or self.env, capture_output=True, text=True)

    def test_generate_reuse_and_both_argv_forms(self):
        flags = ["--live-pod-routing=false", "--e2b-template-api=false"]
        first = self.run_entry(flags)
        self.assertEqual(first.returncode, 0, first.stderr)
        before = self.secret.read_bytes()
        self.assertEqual(len(before.strip()), 64)
        self.assertEqual(self.secret.stat().st_mode & 0o777, 0o600)
        second = self.run_entry([str(self.gateway), *flags])
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(json.loads(first.stdout), json.loads(second.stdout))
        self.assertEqual(json.loads(first.stdout)[1], flags)
        self.assertEqual(self.secret.read_bytes(), before)
        self.assertEqual(list(self.secret.parent.glob("panel-secret.*")), [])

    def test_explicit_env_no_file_and_no_args(self):
        result = self.run_entry(env=dict(self.env, CUBEPOD_PANEL_SECRET="explicit-test-key"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.secret.exists())
        self.assertEqual(json.loads(result.stdout), [hashlib.sha256(b"explicit-test-key").hexdigest(), []])

    def test_concurrent_first_start(self):
        procs = [subprocess.Popen(["sh", str(self.script), "--test"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) for _ in range(12)]
        results = [p.communicate(timeout=20) for p in procs]
        for proc, (_, stderr) in zip(procs, results):
            self.assertEqual(proc.returncode, 0, stderr)
        self.assertEqual(len({json.loads(out)[0] for out, _ in results}), 1)
        self.assertEqual(list(self.secret.parent.glob("panel-secret.*")), [])

    def test_empty_existing_file_is_not_rotated(self):
        self.secret.parent.mkdir()
        self.secret.touch(mode=0o600)
        result = self.run_entry()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.secret.read_bytes(), b"")

    def test_random_failure_publishes_nothing(self):
        fakebin = self.root / "bin"
        fakebin.mkdir()
        od = fakebin / "od"
        od.write_text("#!/bin/sh\nexit 1\n")
        od.chmod(0o755)
        result = self.run_entry(env=dict(self.env, PATH=str(fakebin)+":"+self.env["PATH"]))
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.secret.exists())

    def test_publication_failure_cleans_temp_file(self):
        fakebin = self.root / "bin"
        fakebin.mkdir()
        link = fakebin / "ln"
        link.write_text("#!/bin/sh\nexit 1\n")
        link.chmod(0o755)
        result = self.run_entry(env=dict(self.env, PATH=str(fakebin)+":"+self.env["PATH"]))
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.secret.exists())
        self.assertEqual(list(self.secret.parent.glob("panel-secret.*")), [])

    def test_interrupted_publication_cleans_temp_file(self):
        fakebin = self.root / "bin"
        fakebin.mkdir()
        link = fakebin / "ln"
        link.write_text('#!/bin/sh\nkill -TERM "$PPID"\nexit 1\n')
        link.chmod(0o755)
        result = self.run_entry(env=dict(self.env, PATH=str(fakebin)+":"+self.env["PATH"]))
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.secret.exists())
        self.assertEqual(list(self.secret.parent.glob("panel-secret.*")), [])


if __name__ == "__main__":
    unittest.main()

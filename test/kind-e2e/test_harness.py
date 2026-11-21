"""Runner contract checks, not substitutes for the real kind SDK tests."""
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

CHECK = Path(__file__).with_name("check_reports.py")


class ReportsTest(unittest.TestCase):
    def run_check(self, body, env=None):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "report.xml"
            report.write_text("<testsuite>" + body + "</testsuite>")
            run_env = os.environ.copy()
            run_env.pop("KATA_VMM", None)
            if env:
                run_env.update(env)
            return subprocess.run(
                [sys.executable, str(CHECK), str(report), str(report)],
                capture_output=True, env=run_env).returncode

    def test_all_five_pass(self):
        self.assertEqual(self.run_check("<testcase/>" * 5), 0)

    def test_empty_fails(self):
        self.assertNotEqual(self.run_check(""), 0)

    def test_skips_fail(self):
        self.assertNotEqual(self.run_check("<testcase><skipped/></testcase>" + "<testcase/>" * 4), 0)

    def test_failures_fail(self):
        self.assertNotEqual(self.run_check("<testcase><failure/></testcase>" + "<testcase/>" * 4), 0)

    def test_errors_fail(self):
        self.assertNotEqual(self.run_check("<testcase><error/></testcase>" + "<testcase/>" * 4), 0)

    def test_missing_report_fails(self):
        result = subprocess.run([sys.executable, str(CHECK), "/nonexistent/report.xml", "/nonexistent/other.xml"], capture_output=True)
        self.assertNotEqual(result.returncode, 0)

    def test_documented_fc_skips_allowed(self):
        body = (
            "<testcase name='a'/><testcase name='b'/><testcase name='c'/>"
            "<testcase name='volume'><skipped message='https://github.com/cubeplexai/cubepod/issues/122: volume'/></testcase>"
            "<testcase name='create'><skipped message='https://github.com/cubeplexai/cubepod/issues/120: eio'/></testcase>"
        )
        self.assertEqual(self.run_check(body, env={"KATA_VMM": "fc"}), 0)

    def test_documented_skip_still_fails_off_firecracker(self):
        body = "<testcase><skipped message='https://github.com/cubeplexai/cubepod/issues/122: volume'/></testcase>" + "<testcase/>" * 4
        self.assertNotEqual(self.run_check(body), 0)
        self.assertNotEqual(self.run_check(body, env={"KATA_VMM": "clh"}), 0)

    def test_unlabeled_skip_fails_on_firecracker(self):
        self.assertNotEqual(self.run_check("<testcase><skipped/></testcase>" + "<testcase/>" * 4, env={"KATA_VMM": "fc"}), 0)

    def test_failures_still_fail_on_firecracker(self):
        self.assertNotEqual(self.run_check("<testcase><failure/></testcase>" + "<testcase/>" * 4, env={"KATA_VMM": "fc"}), 0)


if __name__ == "__main__":
    unittest.main()

"""Runner contract checks, not substitutes for the real kind SDK tests."""
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

CHECK = Path(__file__).with_name("check_reports.py")


class ReportsTest(unittest.TestCase):
    def run_check(self, body):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / "report.xml"
            report.write_text("<testsuite>" + body + "</testsuite>")
            return subprocess.run([sys.executable, str(CHECK), str(report), str(report)], capture_output=True).returncode

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

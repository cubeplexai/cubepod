"""Unit tests for scripts/resolve-vnext.py."""

from contextlib import redirect_stderr, redirect_stdout
from importlib.util import module_from_spec, spec_from_file_location
from io import StringIO
from pathlib import Path
import tempfile
import unittest

SCRIPT_PATH = Path(__file__).resolve().parents[1] / "resolve-vnext.py"
SPEC = spec_from_file_location("resolve_vnext", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
resolve_vnext = module_from_spec(SPEC)
SPEC.loader.exec_module(resolve_vnext)


def write(root: Path, rel: str, content: str) -> Path:
    target = root / rel
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding="utf-8")
    return target


def run(argv):
    out, err = StringIO(), StringIO()
    with redirect_stdout(out), redirect_stderr(err):
        code = resolve_vnext.main(argv)
    return code, out.getvalue(), err.getvalue()


class ResolveVNextTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.root = Path(self._tmp.name)
        self.en = write(self.root, "website/src/content/docs/sdks/go.md", "### Mounts *(Added in vNEXT)*\n- flag (*Changed in vNEXT*)\n")
        self.zh = write(self.root, "website/src/content/docs/zh/sdks/go.md", "### 挂载 *(vNEXT 新增)*\n")
        self.values = write(self.root, "deploy/helm/cubepod/values.yaml", "  # Mode (Added in vNEXT)\n  mode: a\n")
        self.untouched = write(self.root, "pkg/other.md", "Added in vNEXT\n")
        self.word = write(self.root, "website/src/content/docs/other.md", "vNEXTGEN and myvNEXT stay\n")

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_check_reports_placeholders(self) -> None:
        code, _, err = run(["--check", "--root", str(self.root)])
        self.assertEqual(code, 1)
        self.assertIn("website/src/content/docs/sdks/go.md:1:", err)
        self.assertIn("website/src/content/docs/zh/sdks/go.md:1:", err)
        self.assertIn("deploy/helm/cubepod/values.yaml:1:", err)
        self.assertNotIn("pkg/other.md", err)
        self.assertNotIn("other.md:1: vNEXTGEN", err)

    def test_resolve_replaces_scoped_placeholders(self) -> None:
        code, out, _ = run(["v0.9.0", "--root", str(self.root)])
        self.assertEqual(code, 0)
        self.assertIn("updated 3 file(s)", out)
        self.assertEqual(self.en.read_text(encoding="utf-8"), "### Mounts *(Added in v0.9.0)*\n- flag (*Changed in v0.9.0*)\n")
        self.assertEqual(self.zh.read_text(encoding="utf-8"), "### 挂载 *(v0.9.0 新增)*\n")
        self.assertIn("(Added in v0.9.0)", self.values.read_text(encoding="utf-8"))
        self.assertEqual(self.untouched.read_text(encoding="utf-8"), "Added in vNEXT\n")
        self.assertEqual(self.word.read_text(encoding="utf-8"), "vNEXTGEN and myvNEXT stay\n")

        code, out, _ = run(["--check", "--root", str(self.root)])
        self.assertEqual(code, 0)
        self.assertIn("no unresolved vNEXT markers", out)

    def test_resolve_rejects_invalid_tag(self) -> None:
        for tag in ("0.9.0", "v0.9", "v0.9.0-rc.1", "vNEXT"):
            with self.subTest(tag=tag):
                with self.assertRaises(SystemExit):
                    run([tag, "--root", str(self.root)])
        self.assertIn("vNEXT", self.en.read_text(encoding="utf-8"))


if __name__ == "__main__":
    unittest.main()

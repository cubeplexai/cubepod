"""Unit tests for scripts/release-notes.py."""

from importlib.util import module_from_spec, spec_from_file_location
from pathlib import Path
import tempfile
import unittest

SCRIPT_PATH = Path(__file__).resolve().parents[1] / "release-notes.py"
SPEC = spec_from_file_location("release_notes", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
release_notes = module_from_spec(SPEC)
SPEC.loader.exec_module(release_notes)


def write_doc(root: Path, rel: str, content: str) -> None:
    target = root / "website/src/content/docs" / rel
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding="utf-8")


class RenderReleaseNotesTests(unittest.TestCase):
    def test_renders_and_rewrites_internal_links(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_doc(
                root,
                "changelog/0.4.2.md",
                "---\ntitle: '0.4.2'\n---\n\n# CubePod 0.4.2\n\n"
                "See [Helm guide](/deployment/kubernetes-helm/) and [API](../reference/api-overview.md#auth).\n\n"
                "```markdown\n[literal](/not-rewritten/)\n```\n",
            )
            write_doc(
                root,
                "zh/changelog/0.4.2.md",
                "---\ntitle: '0.4.2'\n---\n\n# CubePod 0.4.2\n\n中文说明。\n",
            )

            out = release_notes.render_release_notes(root, "v0.4.2")
            self.assertNotIn("title: '0.4.2'", out)
            self.assertIn(
                "[Helm guide](https://cubepod.cubeplex.ai/deployment/kubernetes-helm/)",
                out,
            )
            self.assertIn(
                "[API](https://cubepod.cubeplex.ai/reference/api-overview/#auth)",
                out,
            )
            self.assertIn("[literal](/not-rewritten/)", out)
            self.assertIn(
                "[English changelog](https://cubepod.cubeplex.ai/changelog/0.4.2/)",
                out,
            )
            self.assertIn(
                "[中文更新日志](https://cubepod.cubeplex.ai/zh/changelog/0.4.2/)",
                out,
            )

    def test_requires_chinese_changelog(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_doc(root, "changelog/0.4.2.md", "---\ntitle: '0.4.2'\n---\n\nBody.\n")
            with self.assertRaises(FileNotFoundError):
                release_notes.render_release_notes(root, "v0.4.2")

    def test_rejects_missing_or_unclosed_frontmatter_and_empty_body(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            write_doc(root, "changelog/0.4.2.md", "# No frontmatter\n")
            write_doc(root, "zh/changelog/0.4.2.md", "---\ntitle: '0.4.2'\n---\n\nBody.\n")
            with self.assertRaisesRegex(ValueError, "Missing frontmatter"):
                release_notes.render_release_notes(root, "v0.4.2")

            write_doc(root, "changelog/0.4.2.md", "---\ntitle: '0.4.2'\n# unclosed\n")
            with self.assertRaisesRegex(ValueError, "Unclosed frontmatter"):
                release_notes.render_release_notes(root, "v0.4.2")

            write_doc(root, "changelog/0.4.2.md", "---\ntitle: '0.4.2'\n---\n\n")
            with self.assertRaisesRegex(ValueError, "Empty changelog"):
                release_notes.render_release_notes(root, "v0.4.2")

    def test_rejects_invalid_tag_format(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaisesRegex(ValueError, "v<major>.<minor>.<patch>"):
                release_notes.render_release_notes(Path(tmp), "0.4.2")


if __name__ == "__main__":
    unittest.main()

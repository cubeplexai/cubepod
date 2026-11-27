#!/usr/bin/env python3
"""Resolve or check the `vNEXT` documentation version placeholder.

Authors mark unreleased user-facing changes as `*Added in vNEXT*` (or
`*vNEXT 新增*`) because the next release version is usually unknown when the
change is written. The release version-bump PR runs:

    python3 scripts/resolve-vnext.py v<semver>

to replace every placeholder with the real version, and release tooling runs:

    python3 scripts/resolve-vnext.py --check

which exits non-zero while any placeholder remains.
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

PLACEHOLDER = "vNEXT"
PLACEHOLDER_RE = re.compile(r"\bvNEXT\b")
SEMVER_TAG_RE = re.compile(r"^v\d+\.\d+\.\d+$")

# Paths (relative to the repo root) whose user-facing text may carry markers.
# Directories are searched recursively for SCAN_SUFFIXES files.
SCAN_PATHS = (
    "website/src/content/docs",
    "deploy/helm/cubepod/values.yaml",
)
SCAN_SUFFIXES = {".md", ".mdx", ".yaml", ".yml"}


def iter_files(root: Path):
    for rel in SCAN_PATHS:
        target = root / rel
        if target.is_file():
            yield target
        elif target.is_dir():
            for path in sorted(target.rglob("*")):
                if path.is_file() and path.suffix in SCAN_SUFFIXES:
                    yield path


def find_placeholders(root: Path) -> list[tuple[Path, int, str]]:
    """Return (path, line number, line) for every remaining placeholder."""
    hits = []
    for path in iter_files(root):
        text = path.read_text(encoding="utf-8")
        if PLACEHOLDER not in text:
            continue
        for lineno, line in enumerate(text.splitlines(), start=1):
            if PLACEHOLDER_RE.search(line):
                hits.append((path, lineno, line.strip()))
    return hits


def resolve(root: Path, tag: str) -> list[Path]:
    """Replace every placeholder with tag; return the files that changed."""
    if not SEMVER_TAG_RE.match(tag):
        raise ValueError(f"expected a v<major>.<minor>.<patch> tag, got {tag!r}")
    changed = []
    for path in iter_files(root):
        text = path.read_text(encoding="utf-8")
        new = PLACEHOLDER_RE.sub(tag, text)
        if new != text:
            path.write_text(new, encoding="utf-8")
            changed.append(path)
    return changed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("tag", nargs="?", help="release tag to substitute, e.g. v0.9.0")
    group.add_argument("--check", action="store_true", help="fail if any vNEXT placeholder remains")
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1], help=argparse.SUPPRESS)
    args = parser.parse_args(argv)

    if args.check:
        hits = find_placeholders(args.root)
        for path, lineno, line in hits:
            print(f"{path.relative_to(args.root)}:{lineno}: {line}", file=sys.stderr)
        if hits:
            print(
                f"found {len(hits)} unresolved {PLACEHOLDER} marker(s); "
                "run `python3 scripts/resolve-vnext.py v<semver>` in the version-bump PR",
                file=sys.stderr,
            )
            return 1
        print(f"no unresolved {PLACEHOLDER} markers")
        return 0

    try:
        changed = resolve(args.root, args.tag)
    except ValueError as exc:
        parser.error(str(exc))
    for path in changed:
        print(f"resolved {PLACEHOLDER} -> {args.tag}: {path.relative_to(args.root)}")
    print(f"updated {len(changed)} file(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

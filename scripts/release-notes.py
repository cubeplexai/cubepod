#!/usr/bin/env python3
"""Render a version's documentation changelog as GitHub release notes."""

import argparse
import re
from pathlib import Path
from urllib.parse import urljoin

SITE_BASE = "https://cubepod.cubeplex.ai"


def read_changelog(path: Path) -> str:
    """Read a Starlight changelog file, strip YAML frontmatter, and require non-empty body."""
    content = path.read_text(encoding="utf-8")
    lines = content.splitlines(keepends=True)
    if not lines or lines[0].strip() != "---":
        raise ValueError(f"Missing frontmatter: {path}")
    for index, line in enumerate(lines[1:], start=1):
        if line.strip() == "---":
            body = "".join(lines[index + 1 :]).strip()
            if not body:
                raise ValueError(f"Empty changelog: {path}")
            return body
    raise ValueError(f"Unclosed frontmatter: {path}")


def render_release_notes(root: Path, tag: str) -> str:
    """Validate EN and ZH changelogs for tag and return rendered GitHub release notes."""
    if re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag) is None:
        raise ValueError("Expected a release tag in the form v<major>.<minor>.<patch>")
    version = tag[1:]
    docs = root / "website/src/content/docs"
    body = read_changelog(docs / f"changelog/{version}.md")
    read_changelog(docs / f"zh/changelog/{version}.md")

    en_base = f"{SITE_BASE}/changelog/{version}"
    en_url = f"{en_base}/"
    zh_url = f"{SITE_BASE}/zh/changelog/{version}/"

    def replace_link(match: re.Match[str]) -> str:
        target = match.group(2)
        if target.startswith("/"):
            target = f"{SITE_BASE}{target}"
        elif target.startswith(("./", "../")):
            target = re.sub(r"\.md(?=#|\?|$)", "/", urljoin(en_base, target))
        return f"{match.group(1)}({target})"

    rendered: list[str] = []
    fence: str | None = None
    for line in body.splitlines():
        marker = re.match(r"^\s*(`{3,}|~{3,})", line)
        if marker:
            if fence is None:
                fence = marker.group(1)
            elif marker.group(1)[0] == fence[0] and len(marker.group(1)) >= len(fence):
                fence = None
            rendered.append(line)
        elif fence is not None:
            rendered.append(line)
        else:
            rendered.append(re.sub(r"(!?\[[^\]\n]*\])\(([^\s)]+)\)", replace_link, line))

    links = f"[English changelog]({en_url}) · [中文更新日志]({zh_url})"
    return "\n".join(rendered) + f"\n\n{links}\n"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    args = parser.parse_args()
    try:
        print(render_release_notes(Path(__file__).resolve().parents[1], args.tag), end="")
    except (OSError, ValueError) as error:
        parser.exit(1, f"Cannot prepare release notes: {error}\n")


if __name__ == "__main__":
    main()

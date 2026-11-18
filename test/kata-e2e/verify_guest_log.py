"""Positive smoke proof; Pod Ready does not guarantee CRI logs have flushed."""
from pathlib import Path
import sys


def guest_ready(text):
    lines = set(text.splitlines())
    return {"6.18.62", "KATA_GUEST_READY"}.issubset(lines)


if __name__ == "__main__":
    raise SystemExit(0 if guest_ready(Path(sys.argv[1]).read_text()) else 1)

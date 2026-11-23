"""Positive smoke proof; Pod Ready does not guarantee CRI logs have flushed."""
from pathlib import Path
import sys
import os


def guest_ready(text, expected_kernel="6.18.62"):
    lines = set(text.splitlines())
    return {expected_kernel, "KATA_GUEST_READY"}.issubset(lines)


if __name__ == "__main__":
    raise SystemExit(0 if guest_ready(Path(sys.argv[1]).read_text(), os.environ.get("E2E_GUEST_KERNEL", "6.18.62")) else 1)

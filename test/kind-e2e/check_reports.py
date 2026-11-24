"""Fail closed: both real SDK suites must execute every scenario without silent skips.

On Kata Firecracker only, a skip is allowed when its reason cites issue #120
(#122 is fixed by CEP-0031 FirecrackerVolumeNFS). Any other skip, and every failure or error, still fails the check. kind,
Compose and Cloud Hypervisor continue to reject every skip.
"""
import os
import sys
import xml.etree.ElementTree as ET

if len(sys.argv) != 3:
    raise SystemExit("Expected Python and TypeScript report paths")

ALLOWED_FC_SKIP_MARKERS = ("#120", "issues/120")


def skip_reason(case):
    skipped = case.find("skipped")
    if skipped is None:
        return None
    return (skipped.get("message") or (skipped.text or "")).strip()


def allowed_fc_skip(reason):
    return os.environ.get("KATA_VMM") == "fc" and reason and any(
        marker in reason for marker in ALLOWED_FC_SKIP_MARKERS)


errors = []
for report in sys.argv[1:]:
    try:
        root = ET.parse(report).getroot()
        cases = list(root.iter("testcase"))
        if len(cases) != 5:
            errors.append(f"{report}: expected 5 scenarios, got {len(cases)}")
        bad = []
        skipped = 0
        for case in cases:
            name = case.get("name", "unnamed")
            reason = skip_reason(case)
            if reason is not None:
                skipped += 1
                if not allowed_fc_skip(reason):
                    bad.append(name)
            elif any(case.find(tag) is not None for tag in ("failure", "error")):
                bad.append(name)
        if bad:
            errors.append(f"{report}: unsuccessful scenarios: {', '.join(bad)}")
        elif len(cases) == 5:
            print(f"{report}: {len(cases) - skipped} passed, {skipped} skipped")
    except (OSError, ET.ParseError) as exc:
        errors.append(f"{report}: {exc}")
if errors:
    raise SystemExit("\n".join(errors))

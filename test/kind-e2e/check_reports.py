"""Fail closed: both real SDK suites must execute every scenario without skips."""
import sys
import xml.etree.ElementTree as ET

if len(sys.argv) != 3:
    raise SystemExit("Expected Python and TypeScript report paths")

errors = []
for report in sys.argv[1:]:
    try:
        root = ET.parse(report).getroot()
        cases = list(root.iter("testcase"))
        if len(cases) != 5:
            errors.append(f"{report}: expected 5 scenarios, got {len(cases)}")
        bad = [case.get("name", "unnamed") for case in cases
               if any(case.find(tag) is not None for tag in ("skipped", "failure", "error"))]
        if bad:
            errors.append(f"{report}: unsuccessful scenarios: {', '.join(bad)}")
        elif len(cases) == 5:
            print(f"{report}: 5 passed, 0 skipped")
    except (OSError, ET.ParseError) as exc:
        errors.append(f"{report}: {exc}")
if errors:
    raise SystemExit("\n".join(errors))

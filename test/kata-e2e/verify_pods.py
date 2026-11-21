"""Require actual SDK-created Pods to select the chosen Kata handler."""
import json
import sys
import xml.etree.ElementTree as ET
from pathlib import Path

VOLUME_TESTS = {
    "test_volume_mount_persistence",
    "volume mounts survive destruction and recreation",
}


def expected_from_reports(reports):
    expected = 0
    for report in reports:
        root = ET.parse(report).getroot()
        for case in root.iter("testcase"):
            if case.find("skipped") is not None:
                continue
            name = case.get("name", "")
            expected += 2 if name in VOLUME_TESTS else 1
    return expected


source, expected_runtime, *reports = sys.argv[1:]
text = Path(source).read_text()
decoder = json.JSONDecoder()
names = set()
while text.strip():
    text = text.lstrip()
    try:
        event, end = decoder.raw_decode(text)
    except json.JSONDecodeError:
        # SIGTERM can interrupt the final watch update; never count a partial one.
        break
    text = text[end:]
    pod = event.get('object', {})
    if pod.get('kind') != 'Pod':
        continue
    assert pod['spec'].get('runtimeClassName') == expected_runtime, pod['metadata']['name']
    names.add(pod['metadata']['name'])
required = expected_from_reports(reports) if reports else 12
assert len(names) >= required, (
    f'Only observed {len(names)}/{required} SDK sandbox Pods; missing real runtime proof')
print(f'{len(names)} real SDK Pods selected {expected_runtime}; both SDKs additionally assert guest uname')

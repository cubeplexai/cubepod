"""Require all twelve actual SDK-created Pods to select the chosen Kata handler."""
import json
from pathlib import Path
import sys

source, expected = sys.argv[1:]
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
    assert pod['spec'].get('runtimeClassName') == expected, pod['metadata']['name']
    names.add(pod['metadata']['name'])
assert len(names) >= 12, f'Only observed {len(names)}/12 SDK sandbox Pods; missing real runtime proof'
print(f'{len(names)} real SDK Pods selected {expected}; both SDKs additionally assert guest uname')

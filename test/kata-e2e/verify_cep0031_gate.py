"""Opt-in CEP-0031 gate (CEP0031_FC_NFS_GATE=1, Kata FC only): fail closed unless real logs prove it.

Reads only artifacts written by this Kata Firecracker run:
- python.xml / typescript.xml: both official SDK volume persistence tests PASSED (no skip);
- cep0031-gate-{python,typescript}.jsonl: in-guest probes for create AND recreate showing
  the pinned guest release, /data mounted as kernel nfs/nfs3 (nosuid,nodev) and no
  mount.nfs / nfs-common userspace helper in the sandbox image;
- cep0031-volume-nfs.log: every admitted NFS MOUNT came from a real SDK sandbox Pod IP
  (never a node IP, i.e. not SNATed), at least one per sandbox;
- sdk-pods-watch.json / cep0031-nodes.json: Pod and node IPs from the API server.
"""
import ipaddress
import json
import os
import sys
import xml.etree.ElementTree as ET
from pathlib import Path

VOLUME_TESTS = {
    "python.xml": "test_volume_mount_persistence",
    "typescript.xml": "volume mounts survive destruction and recreation",
}


# dpkg status values that mean the package's files are NOT on disk.
DPKG_ABSENT = {"not-installed", "config-files"}


def nfs_helper_findings(probe):
    """Return evidence of a userspace NFS helper in the guest probe output, or [].

    The probe prints, per line, either a found mount.nfs* path (command -v / ls) or
    dpkg-query's "<package> <want> <eflag> <status>" for nfs-common. Only a present
    binary or an nfs-common status other than not-installed/config-files counts;
    "unknown ok not-installed", "deinstall ok config-files" and "no packages found"
    mean the helper is absent. Any unrecognised line fails closed.
    """
    findings = []
    for line in probe.splitlines():
        line = line.strip()
        if not line or line.startswith("dpkg-query: no packages found matching"):
            continue
        fields = line.split()
        if fields[0] == "nfs-common" and len(fields) == 4:
            if fields[3] not in DPKG_ABSENT:
                findings.append(f"nfs-common status {' '.join(fields[1:])}")
            continue
        findings.append(f"unexpected helper probe line {line!r}")
    return findings


def fail(message):
    raise SystemExit(f"CEP0031_GATE_FAIL: {message}")


def host(addr):
    addr = addr.strip()
    if addr.startswith("["):
        return addr[1:addr.index("]")]
    return addr.rsplit(":", 1)[0] if addr.count(":") == 1 else addr


def watch_pods(path):
    return set(watch_pod_ips(path).values())


def watch_pod_ips(path):
    """Map SDK sandbox Pod name (== sandbox ID) to its API-server Pod IP."""
    text, decoder, ips = path.read_text(), json.JSONDecoder(), {}
    while text.strip():
        text = text.lstrip()
        try:
            event, end = decoder.raw_decode(text)
        except json.JSONDecodeError:
            break
        text = text[end:]
        obj = event.get("object", {})
        if obj.get("status", {}).get("podIP"):
            ips[obj["metadata"]["name"]] = obj["status"]["podIP"]
    return ips


def main(artifacts):
    expected_release = os.environ.get("E2E_GUEST_KERNEL", "")
    if expected_release != "6.18.62-cubepod-nfs1":
        fail(f"unexpected pinned release {expected_release!r}")
    for report, name in VOLUME_TESTS.items():
        cases = [c for c in ET.parse(artifacts / report).getroot().iter("testcase") if c.get("name") == name]
        if len(cases) != 1:
            fail(f"{report}: expected exactly one {name!r}, got {len(cases)}")
        bad = [tag for tag in ("skipped", "failure", "error") if cases[0].find(tag) is not None]
        if bad:
            fail(f"{report}: {name!r} did not pass: {bad}")
    sandboxes = []
    for sdk in ("python", "typescript"):
        records = [json.loads(line) for line in (artifacts / f"cep0031-gate-{sdk}.jsonl").read_text().splitlines() if line.strip()]
        if sorted(r["phase"] for r in records) != ["create", "recreate"]:
            fail(f"{sdk}: expected create+recreate probes, got {[r['phase'] for r in records]}")
        if len({r["sandbox_id"] for r in records}) != 2:
            fail(f"{sdk}: recreate did not use a new sandbox")
        for r in records:
            if r["uname"] != expected_release:
                fail(f"{sdk}/{r['phase']}: guest uname {r['uname']!r}")
            fields = r["data_mounts"][-1].split()
            if fields[2] not in ("nfs", "nfs3") or not {"nosuid", "nodev"} <= set(fields[3].split(",")):
                fail(f"{sdk}/{r['phase']}: /data is not a kernel nfs mount: {fields}")
            if findings := nfs_helper_findings(r["nfs_helpers"]):
                fail(f"{sdk}/{r['phase']}: userspace NFS helper present: {findings}")
            sandboxes.append(r)
    pod_ips = watch_pods(artifacts / "sdk-pods-watch.json")
    node_ips = {a["address"] for n in json.loads((artifacts / "cep0031-nodes.json").read_text())["items"]
                for a in n["status"].get("addresses", []) if a["type"] in ("InternalIP", "ExternalIP")}
    if not pod_ips or pod_ips & node_ips:
        fail(f"SDK pods missing or using node IPs: pods={sorted(pod_ips)} nodes={sorted(node_ips)}")
    admitted, denied = [], []
    for line in (artifacts / "cep0031-volume-nfs.log").read_text().splitlines():
        try:
            entry = json.loads(line[line.index("{"):])
        except ValueError:
            continue
        if entry.get("msg") == "volume nfs mount admitted":
            admitted.append(entry)
        elif entry.get("msg") == "volume nfs mount denied":
            denied.append(entry)
    if len(admitted) < len(sandboxes):
        fail(f"only {len(admitted)} admitted NFS mounts for {len(sandboxes)} sandboxes")
    sources = {host(e["sourceIP"]) for e in admitted}
    for source in sources:
        ipaddress.ip_address(source)
        if source in node_ips or source not in pod_ips:
            fail(f"NFS server saw {source}, not an SDK sandbox Pod IP (pods={sorted(pod_ips)}, nodes={sorted(node_ips)})")
    # Per sandbox: the lease in its own /data export must have been admitted from
    # exactly that sandbox Pod's IP (one-to-one, not merely "some pod IP").
    by_name = watch_pod_ips(artifacts / "sdk-pods-watch.json")
    by_lease = {}
    for e in admitted:
        by_lease.setdefault(e["leaseID"], set()).add(host(e["sourceIP"]))
    for r in sandboxes:
        lease = r["data_mounts"][-1].split()[0].split(":", 1)[1].strip("/").split("/")[0]
        pod_ip = by_name.get(r["sandbox_id"])
        if not pod_ip or by_lease.get(lease) != {pod_ip}:
            fail(f"{r['sdk']}/{r['phase']}: lease {lease} admitted from {sorted(by_lease.get(lease, []))}, sandbox Pod IP {pod_ip}")
        r["pod_ip"], r["lease"] = pod_ip, lease
    print(json.dumps({
        "CEP0031_GATE": "PASS",
        "guest_release": expected_release,
        "probes": sandboxes,
        "admitted_mounts": [{k: e.get(k) for k in ("time", "leaseID", "volumeID", "sourceIP")} for e in admitted],
        "denied_mounts": len(denied),
        "sdk_pod_ips": sorted(pod_ips),
        "node_ips": sorted(node_ips),
    }, indent=2))


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: verify_cep0031_gate.py ARTIFACTS")
    main(Path(sys.argv[1]))

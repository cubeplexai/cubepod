#!/usr/bin/env python3
"""Reproducible WarmPool & cold-start benchmark suite for CubePod staging/live clusters.

Compares StorageClasses (e.g., longhorn-v2-block-thin vs cubepod-lvm-block) across:
  1. Cold Create (optional, with Kubernetes Pod phase breakdown)
  2. WarmPool Pre-Warm latency
  3. WarmPool Single Claim latency + in-guest command RTT + single-slot replenishment latency
  4. WarmPool Burst Concurrency Claim latency + burst replenishment latency

All temporary PodTemplates, PodWarmPools, Sandboxes, Pods, and PVCs created by a
benchmark run are cleaned up automatically in a finally block.
"""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import time
from typing import Any
import urllib.error
import urllib.request


PROXY_ENV_VARS = ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY")


def strip_proxy_env(env: dict[str, str] | None = None) -> dict[str, str]:
    """Return a copy of env (or os.environ) with HTTP(S) proxy variables removed."""
    base = dict(os.environ if env is None else env)
    for key in PROXY_ENV_VARS:
        base.pop(key, None)
    return base


def parse_k8s_timestamp(ts: str | None) -> float | None:
    """Parse an RFC3339 Kubernetes timestamp into epoch seconds."""
    if not ts:
        return None
    return datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp()


def percentile_nearest_rank(values: list[float], p: float) -> float:
    """Compute nearest-rank percentile (0 < p <= 1) matching test/k8s-e2e/benchmark.go."""
    if not values:
        return 0.0
    ordered = sorted(values)
    if p <= 0:
        return ordered[0]
    if p >= 1:
        return ordered[-1]
    idx = math.ceil(p * len(ordered)) - 1
    idx = max(0, min(idx, len(ordered) - 1))
    return ordered[idx]


def is_command_success(cmd_res: dict[str, Any] | None, expected_substring: str) -> bool:
    """Validate /commands/run JSON output across exit_code (canonical) and exitCode."""
    if not isinstance(cmd_res, dict):
        return False
    code = cmd_res.get("exit_code")
    if code is None:
        code = cmd_res.get("exitCode")
    output = cmd_res.get("output", "")
    return code == 0 and expected_substring in output


def summarize_samples(operation: str, samples: list[dict[str, Any]]) -> dict[str, Any]:
    """Compute min/p50/p95/max/mean statistics for a list of sample dicts."""
    total = len(samples)
    ok_samples = [s for s in samples if s.get("success", False)]
    fail_count = total - len(ok_samples)
    durations = [float(s["duration_ms"]) for s in ok_samples]
    if not durations and samples:
        durations = [float(s["duration_ms"]) for s in samples]
    if not durations:
        return {
            "operation": operation,
            "count": 0,
            "success_count": 0,
            "failure_count": 0,
            "min_ms": 0.0,
            "p50_ms": 0.0,
            "p95_ms": 0.0,
            "max_ms": 0.0,
            "mean_ms": 0.0,
        }
    return {
        "operation": operation,
        "count": total,
        "success_count": len(ok_samples),
        "failure_count": fail_count,
        "min_ms": min(durations),
        "p50_ms": percentile_nearest_rank(durations, 0.50),
        "p95_ms": percentile_nearest_rank(durations, 0.95),
        "max_ms": max(durations),
        "mean_ms": sum(durations) / len(durations),
    }


def build_pod_template_manifest(
    name: str,
    namespace: str,
    storage_class: str,
    image: str,
    podd_image: str,
    egress_image: str,
    egress_exchange_url: str,
    runtime_class: str,
    rootfs_mb: int,
    workspace_size: str,
    cpu_count: int = 2,
    memory_mb: int = 2048,
) -> str:
    """Build a PodTemplate YAML manifest for benchmarking a specific StorageClass."""
    egress_block = (
        f"  egress:\n"
        f"    enabled: true\n"
        f"    image: {egress_image}\n"
        f"    exchangeUrl: {egress_exchange_url}\n"
        if egress_image
        else "  egress:\n    enabled: false\n"
    )
    return (
        f"apiVersion: cubepod.io/v1alpha1\n"
        f"kind: PodTemplate\n"
        f"metadata:\n"
        f"  name: {name}\n"
        f"  namespace: {namespace}\n"
        f"spec:\n"
        f"  names: [{name}]\n"
        f"  runtimeClassName: {runtime_class}\n"
        f"  image: {image}\n"
        f"  poddImage: {podd_image}\n"
        f"  cpuCount: {cpu_count}\n"
        f"  memoryMB: {memory_mb}\n"
        f"  diskSizeMB: {rootfs_mb}\n"
        f"  resources:\n"
        f'    cpuRequest: "1"\n'
        f'    cpuLimit: "{cpu_count}"\n'
        f'    memoryRequest: "2Gi"\n'
        f'    memoryLimit: "4Gi"\n'
        f"  storage:\n"
        f"    storageClassName: {storage_class}\n"
        f"    rootfsSize: {rootfs_mb}Mi\n"
        f"    workspaceSize: {workspace_size}\n"
        f"{egress_block}"
    )


def extract_pod_phase_breakdown(pod: dict[str, Any]) -> dict[str, float]:
    """Extract phase durations (in ms) from a Kubernetes Pod status object."""
    meta_ts = parse_k8s_timestamp(pod.get("metadata", {}).get("creationTimestamp"))
    status = pod.get("status", {})
    conds = {
        c["type"]: parse_k8s_timestamp(c.get("lastTransitionTime"))
        for c in status.get("conditions", [])
        if c.get("type")
    }
    init_cs = {c["name"]: c for c in status.get("initContainerStatuses", []) if "name" in c}
    cs = {c["name"]: c for c in status.get("containerStatuses", []) if "name" in c}

    init_started = parse_k8s_timestamp(
        init_cs.get("podd-installer", {}).get("state", {}).get("terminated", {}).get("startedAt")
    )
    init_finished = parse_k8s_timestamp(
        init_cs.get("podd-installer", {}).get("state", {}).get("terminated", {}).get("finishedAt")
    )
    sb_started = parse_k8s_timestamp(
        cs.get("sandbox", {}).get("state", {}).get("running", {}).get("startedAt")
    )
    eg_started = parse_k8s_timestamp(
        cs.get("egress", {}).get("state", {}).get("running", {}).get("startedAt")
    )
    sched_ts = conds.get("PodScheduled")
    init_cond = conds.get("Initialized")
    ready_ts = conds.get("Ready")

    out: dict[str, float] = {}
    if meta_ts and sched_ts:
        out["create_to_scheduled_ms"] = (sched_ts - meta_ts) * 1000.0
    if sched_ts and init_started:
        out["scheduled_to_init_ms"] = (init_started - sched_ts) * 1000.0
    if init_started and init_finished:
        out["init_duration_ms"] = (init_finished - init_started) * 1000.0
    if meta_ts and init_cond:
        out["create_to_initialized_ms"] = (init_cond - meta_ts) * 1000.0
    if meta_ts and sb_started:
        out["create_to_main_start_ms"] = (sb_started - meta_ts) * 1000.0
    if meta_ts and eg_started:
        out["create_to_egress_start_ms"] = (eg_started - meta_ts) * 1000.0
    if sb_started and ready_ts:
        out["main_start_to_ready_ms"] = (ready_ts - sb_started) * 1000.0
    if meta_ts and ready_ts:
        out["create_to_ready_ms"] = (ready_ts - meta_ts) * 1000.0
    return out


class GatewayClient:
    """Minimal HTTP client for the CubePod E2B-compatible gateway API."""

    def __init__(self, base_url: str, timeout: float = 300.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        # Bypass any ambient HTTP(S)_PROXY for direct cluster NodePort access.
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request_json(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        timeout: float | None = None,
    ) -> tuple[int, Any]:
        data = json.dumps(body).encode("utf-8") if body is not None else None
        headers = {"Content-Type": "application/json"} if data is not None else {}
        req = urllib.request.Request(
            self.base_url + path,
            data=data,
            headers=headers,
            method=method,
        )
        try:
            with self._opener.open(req, timeout=timeout or self.timeout) as resp:
                raw = resp.read().decode("utf-8")
                return resp.status, json.loads(raw) if raw else None
        except urllib.error.HTTPError as exc:
            err_body = exc.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"HTTP {exc.code} {method} {path}: {err_body}") from exc


@dataclass
class KubeHelper:
    """Wrapper around kubectl with proxy environment stripped."""

    namespace: str
    env: dict[str, str] = field(default_factory=strip_proxy_env)

    def run(self, args: list[str], input_text: str | None = None, check: bool = True) -> str:
        proc = subprocess.run(
            ["kubectl", *args],
            input=input_text,
            text=True,
            capture_output=True,
            env=self.env,
        )
        if check and proc.returncode != 0:
            raise RuntimeError(f"kubectl {' '.join(args)} failed: {proc.stderr.strip()}")
        return proc.stdout

    def apply_ready_template(
        self,
        name: str,
        storage_class: str,
        image: str,
        podd_image: str,
        egress_image: str,
        egress_exchange_url: str,
        runtime_class: str,
        rootfs_mb: int,
        workspace_size: str,
    ) -> None:
        manifest = build_pod_template_manifest(
            name=name,
            namespace=self.namespace,
            storage_class=storage_class,
            image=image,
            podd_image=podd_image,
            egress_image=egress_image,
            egress_exchange_url=egress_exchange_url,
            runtime_class=runtime_class,
            rootfs_mb=rootfs_mb,
            workspace_size=workspace_size,
        )
        self.run(["apply", "-f", "-"], input_text=manifest)
        status_patch = json.dumps({
            "status": {
                "templateID": name,
                "readyBuildID": "bld_bench",
                "latestBuildID": "bld_bench",
                "readyImage": image,
                "buildStatus": "ready",
            }
        })
        self.run([
            "-n", self.namespace,
            "patch", "podtemplates.cubepod.io", name,
            "--subresource=status", "--type=merge", "-p", status_patch,
        ])

    def delete_template(self, name: str) -> None:
        self.run(
            ["-n", self.namespace, "delete", "podtemplates.cubepod.io", name, "--ignore-not-found"],
            check=False,
        )

    def delete_warmpool(self, name: str) -> None:
        self.run(
            ["-n", self.namespace, "delete", "podwarmpools.cubepod.io", name, "--ignore-not-found"],
            check=False,
        )

    def cleanup_pool_resources(self, pool_name: str, timeout_s: float = 120.0) -> None:
        """Delete PodWarmPool CR and any leftover warm pods/PVCs, then wait for deletion."""
        self.delete_warmpool(pool_name)
        label = f"cubepod.io/warmpool={pool_name}"
        self.run(
            ["-n", self.namespace, "delete", "pod", "-l", label, "--ignore-not-found", "--wait=false"],
            check=False,
        )
        self.run(
            ["-n", self.namespace, "delete", "pvc", "-l", label, "--ignore-not-found", "--wait=false"],
            check=False,
        )
        # Also sweep PVCs matching <pool_name>-warm-* which may lack warmpool labels.
        raw_pvcs = self.run(["-n", self.namespace, "get", "pvc", "-o", "name"], check=False)
        for line in raw_pvcs.splitlines():
            pvc = line.strip().removeprefix("persistentvolumeclaim/")
            if pvc.startswith(f"{pool_name}-warm-"):
                self.run(
                    ["-n", self.namespace, "delete", "pvc", pvc, "--ignore-not-found", "--wait=false"],
                    check=False,
                )
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            rem_pods = self.run(
                ["-n", self.namespace, "get", "pod", "-l", label, "--no-headers"],
                check=False,
            ).strip()
            rem_pvcs = [
                ln.strip()
                for ln in self.run(["-n", self.namespace, "get", "pvc", "-o", "name"], check=False).splitlines()
                if ln.strip().removeprefix("persistentvolumeclaim/").startswith(f"{pool_name}-warm-")
            ]
            if not rem_pods and not rem_pvcs:
                return
            time.sleep(0.5)

    def wait_box_deleted(self, box_id: str, timeout_s: float = 90.0) -> None:
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            rem = self.run(
                ["-n", self.namespace, "get", "pod,pvc", "-l", f"pod.cubeplex.ai/box={box_id}", "--no-headers"],
                check=False,
            ).strip()
            if not rem:
                return
            time.sleep(0.5)

    def get_pod(self, pod_name: str) -> dict[str, Any] | None:
        raw = self.run(["-n", self.namespace, "get", "pod", pod_name, "-o", "json"], check=False)
        if not raw.strip():
            return None
        return json.loads(raw)

    def count_ready_unclaimed_warm_pods(self, pool_name: str) -> int:
        """Count Ready unclaimed standby Pods for pool_name directly from Kubernetes."""
        raw = self.run([
            "-n", self.namespace, "get", "pod",
            "-l", f"cubepod.io/warmpool={pool_name},cubepod.io/claimed=false",
            "-o", "json",
        ], check=False)
        if not raw.strip():
            return 0
        items = json.loads(raw).get("items", [])
        ready = 0
        for pod in items:
            if pod.get("metadata", {}).get("deletionTimestamp"):
                continue
            status = pod.get("status", {})
            if status.get("phase") != "Running" or not status.get("podIP"):
                continue
            conds = {c["type"]: c["status"] for c in status.get("conditions", []) if "type" in c}
            if conds.get("Ready") == "True":
                ready += 1
        return ready


def wait_for_warm_replicas(
    kube: KubeHelper,
    pool_name: str,
    target_replicas: int,
    timeout_s: float = 300.0,
    poll_interval_s: float = 0.5,
) -> float:
    """Poll Kubernetes until target_replicas unclaimed warm pods are Ready; return elapsed ms."""
    t0 = time.monotonic()
    deadline = t0 + timeout_s
    while time.monotonic() < deadline:
        ready = kube.count_ready_unclaimed_warm_pods(pool_name)
        if ready >= target_replicas:
            return (time.monotonic() - t0) * 1000.0
        time.sleep(poll_interval_s)
    raise TimeoutError(
        f"WarmPool {pool_name} did not reach {target_replicas} Ready unclaimed replicas within {timeout_s}s"
    )


def run_storage_class_benchmark(
    client: GatewayClient,
    kube: KubeHelper,
    storage_class: str,
    args: argparse.Namespace,
) -> dict[str, Any]:
    """Run Cold (optional) + WarmPool Prewarm/SingleClaim/Replenish/Burst benchmarks for one StorageClass."""
    run_id = int(time.time()) % 100000
    sc_slug = storage_class.replace("_", "-").lower()
    tpl_name = f"bench-tpl-{sc_slug}-{run_id}"
    pool_name = f"bench-wp-{sc_slug}-{run_id}"
    created_boxes: list[str] = []
    samples: list[dict[str, Any]] = []

    def delete_sandbox_and_wait(sb_id: str) -> None:
        try:
            client.request_json("DELETE", f"/sandboxes/{sb_id}?deleteVolumes=true", timeout=60.0)
        except Exception as exc:
            print(f"  [warn] delete sandbox {sb_id}: {exc}", file=sys.stderr)
        kube.wait_box_deleted(sb_id, timeout_s=90.0)

    def record(op: str, iteration: int, duration_ms: float, success: bool = True, **meta: Any) -> None:
        entry = {
            "storage_class": storage_class,
            "operation": op,
            "iteration": iteration,
            "duration_ms": round(duration_ms, 2),
            "success": success,
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "metadata": meta,
        }
        samples.append(entry)
        status = "OK" if success else "FAIL"
        meta_str = " ".join(f"{k}={v}" for k, v in meta.items())
        print(f"[{storage_class}] {op} iter={iteration} {status} {duration_ms:.1f}ms {meta_str}".rstrip())

    print(f"\n=== Benchmarking StorageClass: {storage_class} (replicas={args.replicas}, rootfs={args.rootfs_mb}Mi, ws={args.workspace_size}) ===")
    kube.apply_ready_template(
        name=tpl_name,
        storage_class=storage_class,
        image=args.sandbox_image,
        podd_image=args.podd_image,
        egress_image=args.egress_image,
        egress_exchange_url=args.egress_exchange_url,
        runtime_class=args.runtime_class,
        rootfs_mb=args.rootfs_mb,
        workspace_size=args.workspace_size,
    )
    # Wait for controller syncInterval (5s) + gateway discovery.
    time.sleep(6.0)

    try:
        # 1. Optional Cold Create baseline with pod phase breakdown
        for i in range(1, args.cold_iterations + 1):
            t0 = time.monotonic()
            sb_id = ""
            try:
                _, rec = client.request_json(
                    "POST",
                    "/sandboxes",
                    {"templateID": tpl_name, "metadata": {"bench": "cold", "sc": storage_class}},
                    timeout=180.0,
                )
                elapsed_ms = (time.monotonic() - t0) * 1000.0
                sb_id = rec["sandboxID"]
                created_boxes.append(sb_id)
                pod = kube.get_pod(sb_id)
                phases = extract_pod_phase_breakdown(pod) if pod else {}
                record("cold_create", i, elapsed_ms, reason=rec.get("reason"), **{k: round(v, 1) for k, v in phases.items()})
            except Exception as exc:
                record("cold_create", i, (time.monotonic() - t0) * 1000.0, success=False, error=str(exc))
            finally:
                if sb_id:
                    delete_sandbox_and_wait(sb_id)

        # 2. WarmPool Pre-warm
        t0 = time.monotonic()
        _, wp_resp = client.request_json(
            "POST",
            "/warmpools",
            {"name": pool_name, "templateID": tpl_name, "replicas": args.replicas},
            timeout=600.0,
        )
        # Ensure all standby pods are Ready in K8s (CreateWarmPool waits for PodRunning+IP).
        wait_for_warm_replicas(kube, pool_name, args.replicas, timeout_s=300.0)
        prewarm_ms = (time.monotonic() - t0) * 1000.0
        record("warmpool_prewarm", 1, prewarm_ms, replicas=args.replicas, api_avail=wp_resp.get("availableReplicas"))

        # 3. Single Claim #1 + Exec RTT + Single Replenishment
        def do_single_claim(iter_idx: int) -> str:
            t_claim = time.monotonic()
            _, rec = client.request_json(
                "POST",
                "/sandboxes",
                {"templateID": tpl_name, "metadata": {"bench": "warm-single", "iter": str(iter_idx)}},
                timeout=120.0,
            )
            claim_ms = (time.monotonic() - t_claim) * 1000.0
            sb_id = rec["sandboxID"]
            created_boxes.append(sb_id)
            reason = rec.get("reason", "")
            is_warm = reason == "WarmPoolClaimed"
            record("warmpool_single_claim", iter_idx, claim_ms, success=is_warm, reason=reason, sandbox=sb_id)

            t_exec = time.monotonic()
            _, cmd_res = client.request_json(
                "POST",
                f"/sandboxes/{sb_id}/commands/run",
                {"command": "echo warm-ok"},
                timeout=30.0,
            )
            exec_ms = (time.monotonic() - t_exec) * 1000.0
            ok_exec = is_command_success(cmd_res, "warm-ok")
            record("warmpool_claim_exec_rtt", iter_idx, exec_ms, success=ok_exec)
            return sb_id

        first_sb = do_single_claim(1)
        delete_sandbox_and_wait(first_sb)

        # Measure single-slot replenishment latency (controller syncInterval + pod provisioning)
        t_rep = time.monotonic()
        rep_ms = wait_for_warm_replicas(kube, pool_name, args.replicas, timeout_s=300.0)
        record("warmpool_replenish_single", 1, rep_ms, replicas=args.replicas)

        # 4. Remaining sequential single claims (without waiting between claims)
        seq_boxes: list[str] = []
        for idx in range(2, args.single_claims + 1):
            seq_boxes.append(do_single_claim(idx))
        for sb_id in seq_boxes:
            delete_sandbox_and_wait(sb_id)

        # Replenish back to full pool before burst concurrency test
        if args.single_claims > 1:
            wait_for_warm_replicas(kube, pool_name, args.replicas, timeout_s=300.0)

        # 5. Burst Concurrency Claim (N concurrent claims against N-replica pool)
        burst_n = args.replicas
        burst_results: list[tuple[int, float, dict[str, Any] | None, str | None]] = []

        def burst_worker(worker_idx: int) -> tuple[int, float, dict[str, Any] | None, str | None]:
            t_b = time.monotonic()
            try:
                _, rec = client.request_json(
                    "POST",
                    "/sandboxes",
                    {"templateID": tpl_name, "metadata": {"bench": "warm-burst", "worker": str(worker_idx)}},
                    timeout=180.0,
                )
                return worker_idx, (time.monotonic() - t_b) * 1000.0, rec, None
            except Exception as exc:
                return worker_idx, (time.monotonic() - t_b) * 1000.0, None, str(exc)

        t_burst_wall = time.monotonic()
        with ThreadPoolExecutor(max_workers=burst_n) as pool_exec:
            burst_results = list(pool_exec.map(burst_worker, range(1, burst_n + 1)))
        burst_wall_ms = (time.monotonic() - t_burst_wall) * 1000.0

        burst_boxes: list[str] = []
        warm_hits = 0
        for worker_idx, dur_ms, rec, err in sorted(burst_results, key=lambda x: x[0]):
            if rec is not None:
                sb_id = rec["sandboxID"]
                burst_boxes.append(sb_id)
                created_boxes.append(sb_id)
                reason = rec.get("reason", "")
                is_warm = reason == "WarmPoolClaimed"
                if is_warm:
                    warm_hits += 1
                record("warmpool_burst_claim", worker_idx, dur_ms, success=is_warm, reason=reason, sandbox=sb_id)
            else:
                record("warmpool_burst_claim", worker_idx, dur_ms, success=False, error=err)
        record("warmpool_burst_wall", 1, burst_wall_ms, success=(warm_hits == burst_n), concurrency=burst_n, warm_hits=warm_hits)

        # Delete burst sandboxes so thick LVM has capacity to replenish, then measure burst replenishment
        for sb_id in burst_boxes:
            delete_sandbox_and_wait(sb_id)

        burst_rep_ms = wait_for_warm_replicas(kube, pool_name, args.replicas, timeout_s=360.0)
        record("warmpool_replenish_burst", 1, burst_rep_ms, replicas=args.replicas)

    finally:
        print(f"[{storage_class}] Cleaning up temporary sandboxes, WarmPool, and PodTemplate...")
        for sb_id in created_boxes:
            try:
                client.request_json("DELETE", f"/sandboxes/{sb_id}?deleteVolumes=true", timeout=30.0)
            except Exception:
                pass
        for sb_id in created_boxes:
            kube.wait_box_deleted(sb_id, timeout_s=60.0)
        kube.cleanup_pool_resources(pool_name, timeout_s=120.0)
        kube.delete_template(tpl_name)

    op_order: list[str] = []
    by_op: dict[str, list[dict[str, Any]]] = {}
    for s in samples:
        op = s["operation"]
        if op not in by_op:
            op_order.append(op)
            by_op[op] = []
        by_op[op].append(s)

    summaries = [summarize_samples(op, by_op[op]) for op in op_order]
    return {
        "storage_class": storage_class,
        "template_name": tpl_name,
        "pool_name": pool_name,
        "summaries": summaries,
        "samples": samples,
    }


def render_markdown_report(results: list[dict[str, Any]], metadata: dict[str, Any]) -> str:
    """Render a Markdown comparison report across StorageClasses."""
    lines = [
        "# CubePod Staging WarmPool & StorageClass Benchmark Report",
        "",
        f"- **Generated At**: `{metadata['generated_at']}`",
        f"- **Gateway URL**: `{metadata['gateway_url']}`",
        f"- **Namespace**: `{metadata['namespace']}`",
        f"- **RuntimeClass**: `{metadata['runtime_class']}`",
        f"- **WarmPool Replicas**: `{metadata['replicas']}`",
        f"- **Volume Sizes**: rootfs `{metadata['rootfs_mb']}Mi`, workspace `{metadata['workspace_size']}`",
        "",
    ]

    for res in results:
        sc = res["storage_class"]
        lines.extend([
            f"## StorageClass: `{sc}`",
            "",
            "| Operation | Samples (OK/Total) | Min (ms) | p50 (ms) | p95 (ms) | Max (ms) | Mean (ms) |",
            "| :--- | :---: | ---: | ---: | ---: | ---: | ---: |",
        ])
        for s in res["summaries"]:
            lines.append(
                f"| `{s['operation']}` | {s['success_count']}/{s['count']} | "
                f"{s['min_ms']:.2f} | {s['p50_ms']:.2f} | {s['p95_ms']:.2f} | "
                f"{s['max_ms']:.2f} | {s['mean_ms']:.2f} |"
            )
        lines.append("")

    if len(results) > 1:
        lines.extend([
            "## Side-by-Side Comparison (p50 / Mean ms)",
            "",
            "| Operation | " + " | ".join(f"`{r['storage_class']}` (p50 / mean)" for r in results) + " |",
            "| :--- | " + " | ".join("---:" for _ in results) + " |",
        ])
        all_ops: list[str] = []
        lookup: dict[tuple[str, str], dict[str, Any]] = {}
        for r in results:
            for s in r["summaries"]:
                if s["operation"] not in all_ops:
                    all_ops.append(s["operation"])
                lookup[(r["storage_class"], s["operation"])] = s
        for op in all_ops:
            cells = []
            for r in results:
                s = lookup.get((r["storage_class"], op))
                cells.append(f"{s['p50_ms']:.1f} / {s['mean_ms']:.1f}" if s else "-")
            lines.append(f"| `{op}` | " + " | ".join(cells) + " |")
        lines.append("")

    return "\n".join(lines)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="CubePod staging WarmPool & StorageClass benchmark")
    parser.add_argument("--gateway-url", default="http://192.168.1.214:32381")
    parser.add_argument("--namespace", default="cubepod-staging-workloads")
    parser.add_argument(
        "--storage-classes",
        nargs="+",
        default=["longhorn-v2-block-thin", "cubepod-lvm-block"],
    )
    parser.add_argument("--replicas", type=int, default=3)
    parser.add_argument("--single-claims", type=int, default=3)
    parser.add_argument("--cold-iterations", type=int, default=1)
    parser.add_argument("--rootfs-mb", type=int, default=2048)
    parser.add_argument("--workspace-size", default="2Gi")
    parser.add_argument("--runtime-class", default="kata-clh")
    parser.add_argument(
        "--sandbox-image",
        default="hub.sensedeal.vip/library/cubeplex-sandbox:0.8.0-260917",
    )
    parser.add_argument(
        "--podd-image",
        default="hub.sensedeal.vip/library/cubepod-podd:v0.8.0",
    )
    parser.add_argument(
        "--egress-image",
        default="hub.sensedeal.vip/library/cubepod-egress:v0.8.0",
    )
    parser.add_argument(
        "--egress-exchange-url",
        default="https://cubeplex-backend.cubeplex.svc:8443/api/v1/internal/egress/exchange",
    )
    parser.add_argument("--json-out", default="")
    parser.add_argument("--md-out", default="")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    for key in PROXY_ENV_VARS:
        os.environ.pop(key, None)

    client = GatewayClient(args.gateway_url)
    st, health = client.request_json("GET", "/health", timeout=10.0)
    if st != 200 or health.get("status") != "ok":
        raise RuntimeError(f"gateway health check failed: status={st}, body={health}")

    kube = KubeHelper(namespace=args.namespace)
    results = [run_storage_class_benchmark(client, kube, sc, args) for sc in args.storage_classes]

    metadata = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "gateway_url": args.gateway_url,
        "namespace": args.namespace,
        "runtime_class": args.runtime_class,
        "replicas": args.replicas,
        "single_claims": args.single_claims,
        "cold_iterations": args.cold_iterations,
        "rootfs_mb": args.rootfs_mb,
        "workspace_size": args.workspace_size,
        "sandbox_image": args.sandbox_image,
    }
    report_md = render_markdown_report(results, metadata)
    print("\n" + report_md)

    if args.md_out:
        out_path = Path(args.md_out)
        out_path.parent.mkdir(parents=True, exist_ok=True)
        out_path.write_text(report_md, encoding="utf-8")
    if args.json_out:
        out_path = Path(args.json_out)
        out_path.parent.mkdir(parents=True, exist_ok=True)
        out_path.write_text(
            json.dumps({"metadata": metadata, "results": results}, indent=2),
            encoding="utf-8",
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

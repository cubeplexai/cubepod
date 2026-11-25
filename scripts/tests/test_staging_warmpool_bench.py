#!/usr/bin/env python3
"""Unit tests for scripts/staging-warmpool-bench.py."""

import importlib.util
from pathlib import Path
import sys
import unittest


SCRIPT_PATH = Path(__file__).resolve().parent.parent / "staging-warmpool-bench.py"
SPEC = importlib.util.spec_from_file_location("staging_warmpool_bench", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
bench = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = bench
SPEC.loader.exec_module(bench)


class TestStagingWarmPoolBench(unittest.TestCase):
    def test_strip_proxy_env(self) -> None:
        cleaned = bench.strip_proxy_env({
            "PATH": "/usr/bin",
            "http_proxy": "http://127.0.0.1:7890",
            "HTTPS_PROXY": "http://127.0.0.1:7890",
        })
        self.assertEqual(cleaned, {"PATH": "/usr/bin"})

    def test_percentile_nearest_rank_and_summary(self) -> None:
        samples = [
            {"duration_ms": 100.0, "success": True},
            {"duration_ms": 200.0, "success": True},
            {"duration_ms": 300.0, "success": True},
            {"duration_ms": 9999.0, "success": False},
        ]
        summary = bench.summarize_samples("warmpool_single_claim", samples)
        self.assertEqual(summary["count"], 4)
        self.assertEqual(summary["success_count"], 3)
        self.assertEqual(summary["failure_count"], 1)
        self.assertAlmostEqual(summary["min_ms"], 100.0)
        self.assertAlmostEqual(summary["p50_ms"], 200.0)
        self.assertAlmostEqual(summary["p95_ms"], 300.0)
        self.assertAlmostEqual(summary["max_ms"], 300.0)
        self.assertAlmostEqual(summary["mean_ms"], 200.0)

    def test_extract_pod_phase_breakdown(self) -> None:
        pod = {
            "metadata": {"creationTimestamp": "2026-11-25T09:34:21Z"},
            "status": {
                "conditions": [
                    {"type": "PodScheduled", "lastTransitionTime": "2026-11-25T09:34:24Z"},
                    {"type": "Initialized", "lastTransitionTime": "2026-11-25T09:34:28Z"},
                    {"type": "Ready", "lastTransitionTime": "2026-11-25T09:34:29Z"},
                ],
                "initContainerStatuses": [{
                    "name": "podd-installer",
                    "state": {"terminated": {
                        "startedAt": "2026-11-25T09:34:28Z",
                        "finishedAt": "2026-11-25T09:34:28Z",
                    }},
                }],
                "containerStatuses": [
                    {"name": "sandbox", "state": {"running": {"startedAt": "2026-11-25T09:34:28Z"}}},
                    {"name": "egress", "state": {"running": {"startedAt": "2026-11-25T09:34:28Z"}}},
                ],
            },
        }
        phases = bench.extract_pod_phase_breakdown(pod)
        self.assertAlmostEqual(phases["create_to_scheduled_ms"], 3000.0)
        self.assertAlmostEqual(phases["scheduled_to_init_ms"], 4000.0)
        self.assertAlmostEqual(phases["main_start_to_ready_ms"], 1000.0)
        self.assertAlmostEqual(phases["create_to_ready_ms"], 8000.0)

    def test_build_pod_template_manifest(self) -> None:
        yaml_text = bench.build_pod_template_manifest(
            name="bench-tpl-lvm",
            namespace="cubepod-staging-workloads",
            storage_class="cubepod-lvm-block",
            image="example/sandbox:v1",
            podd_image="example/podd:v1",
            egress_image="example/egress:v1",
            egress_exchange_url="https://backend/exchange",
            runtime_class="kata-clh",
            rootfs_mb=2048,
            workspace_size="2Gi",
        )
        self.assertIn("storageClassName: cubepod-lvm-block", yaml_text)
        self.assertIn("diskSizeMB: 2048", yaml_text)
        self.assertIn("workspaceSize: 2Gi", yaml_text)
        self.assertIn("runtimeClassName: kata-clh", yaml_text)

    def test_is_command_success(self) -> None:
        self.assertTrue(bench.is_command_success({"exit_code": 0, "output": "warm-ok\n"}, "warm-ok"))
        self.assertTrue(bench.is_command_success({"exitCode": 0, "output": "warm-ok"}, "warm-ok"))
        self.assertFalse(bench.is_command_success({"exit_code": 1, "output": "warm-ok"}, "warm-ok"))
        self.assertFalse(bench.is_command_success({"exit_code": 0, "output": "other"}, "warm-ok"))

    def test_render_markdown_report(self) -> None:
        md = bench.render_markdown_report(
            [
                {
                    "storage_class": "longhorn-v2-block-thin",
                    "summaries": [{
                        "operation": "warmpool_single_claim",
                        "count": 3,
                        "success_count": 3,
                        "min_ms": 40.0,
                        "p50_ms": 50.0,
                        "p95_ms": 60.0,
                        "max_ms": 60.0,
                        "mean_ms": 50.0,
                    }],
                },
                {
                    "storage_class": "cubepod-lvm-block",
                    "summaries": [{
                        "operation": "warmpool_single_claim",
                        "count": 3,
                        "success_count": 3,
                        "min_ms": 35.0,
                        "p50_ms": 45.0,
                        "p95_ms": 55.0,
                        "max_ms": 55.0,
                        "mean_ms": 45.0,
                    }],
                },
            ],
            {
                "generated_at": "2026-11-25T09:50:00+00:00",
                "gateway_url": "http://192.168.1.214:32381",
                "namespace": "cubepod-staging-workloads",
                "runtime_class": "kata-clh",
                "replicas": 3,
                "rootfs_mb": 2048,
                "workspace_size": "2Gi",
            },
        )
        self.assertIn("## StorageClass: `longhorn-v2-block-thin`", md)
        self.assertIn("## Side-by-Side Comparison", md)
        self.assertIn("50.0 / 50.0", md)


if __name__ == "__main__":
    unittest.main()

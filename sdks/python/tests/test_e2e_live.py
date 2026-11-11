#!/usr/bin/env python3
"""End-to-End Live Integration Test for CubePod Python SDK against running cubepod-gateway."""

import argparse
import asyncio
import os
import sys

import httpx

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

from cubepod import AsyncCubePod


async def run_live_e2e(gateway_url: str, upstream_url: str | None = None) -> None:
    print(f"[e2e-live] Connecting AsyncCubePod to gateway at {gateway_url}...")
    async with AsyncCubePod(base_url=gateway_url) as client:
        # 1. Create sandbox
        print("[e2e-live] Creating sandbox...")
        sb = await client.create(
            template_id="cubeplex-ubuntu24-desktop",
            volume_key="ws-live-e2e",
            network_policy={"defaultAction": "Allow"},
            secret_bindings=[
                {
                    "envName": "LIVE_API_KEY",
                    "allowedHosts": ["*.live.example.com"],
                }
            ],
        )
        assert sb.sandbox_id, "sandbox_id should not be empty"
        assert sb.state == "Running", f"expected Running state, got {sb.state}"
        assert sb.rootfs_generation == 1, f"expected generation 1, got {sb.rootfs_generation}"
        sb_id = sb.sandbox_id
        print(f"[e2e-live] Created sandbox {sb_id}")

        # 2. Foreground command
        print("[e2e-live] Testing foreground command execution...")
        exec_res = await client.execute(
            sb_id,
            "echo 'hello-live-e2e-python'",
            workdir="/workspace",
            envs={"E2E_ENV": "active"},
        )
        assert exec_res.exit_code == 0, f"expected exit_code 0, got {exec_res.exit_code}"
        assert "hello-live-e2e-python" in exec_res.output, f"unexpected output: {exec_res.output}"
        print("[e2e-live] Foreground command OK")

        # 3. Background command & cursor logs polling & kill
        print("[e2e-live] Testing background command and cursor log polling...")
        bg_cmd = await client.start_command(
            sb_id,
            "for i in $(seq 1 5); do echo tick $i; sleep 0.05; done",
            workdir="/workspace",
        )
        assert bg_cmd.id, "background command id must not be empty"
        assert bg_cmd.running, "background command should initially be running"

        await asyncio.sleep(0.15)
        logs = await client.poll_command(sb_id, bg_cmd.id, cursor=0)
        assert len(logs.content) > 0, "expected some log output from background command"
        assert "tick" in logs.content, f"expected 'tick' in logs: {logs.content}"
        assert logs.cursor > 0, f"expected cursor > 0, got {logs.cursor}"

        kill_res = await client.kill_command(sb_id, bg_cmd.id)
        assert (
            kill_res.get("killed") is True or kill_res.get("status") == "killed"
        ), f"kill_command response unexpected: {kill_res}"
        print("[e2e-live] Background command & logs OK")

        # 4. File Write and Read
        print("[e2e-live] Testing file I/O...")
        write_res = await client.write_file(
            sb_id,
            "/workspace/notes.txt",
            "cubepod persistent workspace file test",
            uid=1000,
            gid=1000,
            mode="0644",
        )
        assert write_res["path"], "write_file path should be returned"
        file_bytes = await client.read_file(sb_id, "/workspace/notes.txt")
        assert file_bytes == b"cubepod persistent workspace file test", (
            f"unexpected file content: {file_bytes}"
        )
        print("[e2e-live] File I/O OK")

        # 5. Archive Sync (In-Memory tar.gz)
        print("[e2e-live] Testing in-memory archive_sync...")
        sync_res = await client.archive_sync(
            sb_id,
            files={
                "skills/agent.py": "print('live skill initialized')",
                "skills/manifest.json": '{"name": "demo", "version": "1.0"}',
            },
            dest="/workspace",
        )
        assert (
            sync_res.get("extracted", 0) == 2 or sync_res.get("status") == "extracted" or sync_res.get("filesExtracted") == 2
        ), f"unexpected archive_sync response: {sync_res}"

        skill_bytes = await client.read_file(sb_id, "/workspace/skills/agent.py")
        assert skill_bytes == b"print('live skill initialized')", f"unexpected skill content: {skill_bytes}"
        print("[e2e-live] Archive sync OK")

        # 6. Panel Endpoint & Reverse Proxy
        print("[e2e-live] Testing signed panel endpoint...")
        panel = await client.get_panel_endpoint(
            sb_id,
            port=8080,
            expires_in_seconds=3600,
            target_url=upstream_url,
        )
        assert panel.url.startswith("/panel/"), f"expected url starting with /panel/, got {panel.url}"
        assert panel.token, "panel token must not be empty"

        if upstream_url:
            panel_full_url = gateway_url.rstrip("/") + panel.url + "live-status"
            async with httpx.AsyncClient() as hc:
                pr = await hc.get(panel_full_url)
                assert pr.status_code == 200, f"panel proxy GET failed: {pr.status_code} {pr.text}"
                assert "sandbox-panel-alive" in pr.text, f"unexpected panel body: {pr.text}"
            print("[e2e-live] Panel reverse proxy verified against live upstream")
        print("[e2e-live] Panel endpoint OK")

        # 7. Timeout renewal & lease lock
        print("[e2e-live] Testing timeout and lease...")
        t_res = await client.renew_timeout(sb_id, timeout_seconds=1800)
        assert t_res, "renew_timeout response must not be empty"
        l_res = await client.acquire_lease(sb_id, lease_seconds=600)
        assert l_res, "acquire_lease response must not be empty"
        print("[e2e-live] Timeout and lease OK")

        # 8. Pause and Auto-Resume / Connect
        print("[e2e-live] Testing pause and connect...")
        paused_sb = await client.pause(sb_id)
        assert paused_sb.state == "Paused", f"expected Paused state, got {paused_sb.state}"

        resumed_sb = await client.connect(sb_id)
        assert resumed_sb.state == "Running", f"expected Running state, got {resumed_sb.state}"
        print("[e2e-live] Pause and connect OK")

        # 9. Reset RootFS (preserving /workspace)
        print("[e2e-live] Testing reset_rootfs with /workspace preservation...")
        reset_sb = await client.reset_rootfs(sb_id)
        assert reset_sb.rootfs_generation == 2, f"expected generation 2, got {reset_sb.rootfs_generation}"
        assert reset_sb.state == "Running", f"expected Running state, got {reset_sb.state}"

        preserved_file = await client.read_file(sb_id, "/workspace/notes.txt")
        assert preserved_file == b"cubepod persistent workspace file test", (
            f"/workspace/notes.txt content was lost after reset_rootfs: {preserved_file}"
        )
        print("[e2e-live] Reset rootfs and /workspace preservation OK")

        # 10. Copy-on-Write Fork
        print("[e2e-live] Testing CoW fork...")
        child_sb = await client.fork(sb_id)
        assert child_sb.sandbox_id != sb_id, "child sandbox must have unique ID"
        assert child_sb.state == "Running", f"expected child state Running, got {child_sb.state}"
        assert child_sb.rootfs_generation == 1, f"expected child generation 1, got {child_sb.rootfs_generation}"
        print(f"[e2e-live] CoW fork OK: {child_sb.sandbox_id}")

        # 11. Delete
        print("[e2e-live] Testing sandbox delete...")
        await client.delete(child_sb.sandbox_id)
        await client.delete(sb_id)
        print("[e2e-live] Cleanup delete OK")

    print("[e2e-live] ALL PYTHON SDK LIVE E2E TESTS PASSED!")


def main() -> None:
    parser = argparse.ArgumentParser(description="Live E2E test for CubePod Python SDK")
    parser.add_argument(
        "--gateway-url",
        default=os.getenv("CUBEPOD_GATEWAY_URL", "http://127.0.0.1:8080"),
        help="CubePod gateway base URL",
    )
    parser.add_argument(
        "--upstream-url",
        default=os.getenv("CUBEPOD_UPSTREAM_URL", None),
        help="Mock upstream URL for panel reverse proxy verification",
    )
    args = parser.parse_args()

    asyncio.run(run_live_e2e(args.gateway_url, args.upstream_url))


if __name__ == "__main__":
    main()

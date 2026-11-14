from collections.abc import AsyncIterator, Iterator
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import unittest
from unittest import mock
from typing import Any

# Ensure sdks/python is on sys.path
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import httpx

from cubepod import (
    AsyncCubePod,
    CubePod,
    CubePodFileTooLargeError,
    DesktopSession,
    SyncCubePod,
    CommandInfo,
    CommandLogs,
    ExecutionResult,
    FileEntry,
    PanelEndpoint,
    SandboxInfo,
    _build_tar_gz_bytes,
)


class TestAsyncCubePod(unittest.IsolatedAsyncioTestCase):
    async def test_lifecycle_and_fork(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            method = request.method
            if method == "POST" and url_str.endswith("/sandboxes"):
                data = json.loads(request.content)
                self.assertEqual(data["templateID"], "cubeplex-desktop")
                self.assertEqual(data["extension"]["volumeKey"], "vol-123")
                self.assertNotIn("networkPolicy", data)
                self.assertIn("networkPolicy", data["extension"])
                return httpx.Response(
                    201,
                    json={
                        "sandboxID": "cpod-test-1",
                        "templateID": "cubeplex-desktop",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if method == "POST" and url_str.endswith("/sandboxes/cpod-test-1/connect"):
                return httpx.Response(
                    200,
                    json={
                        "sandboxID": "cpod-test-1",
                        "templateID": "cubeplex-desktop",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if method == "POST" and url_str.endswith("/sandboxes/cpod-test-1/pause"):
                return httpx.Response(
                    200,
                    json={
                        "sandboxID": "cpod-test-1",
                        "templateID": "cubeplex-desktop",
                        "state": "Paused",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if method == "POST" and url_str.endswith("/sandboxes/cpod-test-1/reset-rootfs"):
                return httpx.Response(
                    200,
                    json={
                        "sandboxID": "cpod-test-1",
                        "templateID": "cubeplex-desktop",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 2,
                    },
                )
            if method == "POST" and url_str.endswith("/sandboxes/cpod-test-1/fork"):
                return httpx.Response(
                    201,
                    json={
                        "sandboxID": "cpod-test-1-fork",
                        "templateID": "cubeplex-desktop",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if method == "GET" and url_str.endswith("/sandboxes/cpod-test-1"):
                return httpx.Response(
                    200,
                    json={
                        "sandboxID": "cpod-test-1",
                        "templateID": "cubeplex-desktop",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if method == "DELETE" and url_str.endswith("/sandboxes/cpod-test-1"):
                return httpx.Response(204)
            return httpx.Response(404, json={"error": "not found"})

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            info = await client.create(
                template_id="cubeplex-desktop",
                volume_key="vol-123",
                network_policy={"defaultAction": "Allow"},
                secret_bindings=[
                    {
                        "envName": "API_KEY",
                        "allowedHosts": ["*.example.com"],
                    }
                ],
            )
            self.assertEqual(info.sandbox_id, "cpod-test-1")
            self.assertEqual(info.state, "Running")
            self.assertEqual(info.rootfs_generation, 1)

            get_info = await client.get("cpod-test-1")
            self.assertEqual(get_info.sandbox_id, "cpod-test-1")

            connected = await client.connect("cpod-test-1")
            self.assertEqual(connected.state, "Running")

            paused = await client.pause("cpod-test-1")
            self.assertEqual(paused.state, "Paused")

            reset = await client.reset_rootfs("cpod-test-1")
            self.assertEqual(reset.rootfs_generation, 2)

            forked = await client.fork("cpod-test-1")
            self.assertEqual(forked.sandbox_id, "cpod-test-1-fork")

            await client.delete("cpod-test-1")

    async def test_timeout_and_lease(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "POST" and url_str.endswith("/sandboxes/cpod-1/timeout"):
                data = json.loads(request.content)
                self.assertEqual(data["timeoutSeconds"], 1800)
                return httpx.Response(200, json={"status": "renewed", "timeoutSeconds": 1800})
            if request.method == "POST" and url_str.endswith("/sandboxes/cpod-1/lease"):
                data = json.loads(request.content)
                self.assertEqual(data["leaseSeconds"], 600)
                return httpx.Response(200, json={"status": "acquired", "leaseSeconds": 600})
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            t_res = await client.renew_timeout("cpod-1", timeout_seconds=1800)
            self.assertEqual(t_res["status"], "renewed")

            l_res = await client.acquire_lease("cpod-1", lease_seconds=600)
            self.assertEqual(l_res["status"], "acquired")

    async def test_get_panel_endpoint(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "POST" and url_str.endswith("/sandboxes/cpod-1/endpoints/8080"):
                data = json.loads(request.content)
                self.assertEqual(data["expiresInSeconds"], 3600)
                return httpx.Response(
                    200,
                    json={
                        "url": "/panel/tok_abc123/",
                        "token": "tok_abc123",
                        "expiresAt": "2026-11-11T00:00:00Z",
                    },
                )
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            endpoint = await client.get_panel_endpoint("cpod-1", port=8080, expires_in_seconds=3600)
            self.assertIsInstance(endpoint, PanelEndpoint)
            self.assertEqual(endpoint.url, "/panel/tok_abc123/")
            self.assertEqual(endpoint.token, "tok_abc123")
            self.assertEqual(endpoint.expires_at, "2026-11-11T00:00:00Z")

    async def test_execute_foreground(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "POST" and url_str.endswith("/sandboxes/cpod-1/commands/run"):
                data = json.loads(request.content)
                self.assertFalse(data["background"])
                self.assertEqual(data["command"], "echo hello world")
                self.assertEqual(data["workdir"], "/workspace")
                self.assertEqual(data["envs"]["TEST_VAR"], "val")
                self.assertEqual(data["uid"], 1000)
                self.assertEqual(data["gid"], 1000)
                return httpx.Response(
                    200,
                    json={
                        "id": "proc-foreground-1",
                        "pid": 101,
                        "running": False,
                        "exit_code": 0,
                        "output": "hello world\n",
                        "cursor": 12,
                    },
                )
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            res = await client.execute(
                "cpod-1",
                "echo hello world",
                workdir="/workspace",
                envs={"TEST_VAR": "val"},
            )
            self.assertIsInstance(res, ExecutionResult)
            self.assertEqual(res.id, "proc-foreground-1")
            self.assertEqual(res.pid, 101)
            self.assertFalse(res.running)
            self.assertEqual(res.exit_code, 0)
            self.assertEqual(res.output, "hello world\n")
            self.assertEqual(res.cursor, 12)

    async def test_background_command_lifecycle(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "POST" and url_str.endswith("/sandboxes/cpod-1/commands/run"):
                data = json.loads(request.content)
                self.assertTrue(data["background"])
                self.assertEqual(data["command"], "python3 -m http.server 8000")
                return httpx.Response(
                    200,
                    json={
                        "id": "proc-bg-1",
                        "pid": 202,
                        "running": True,
                        "exit_code": None,
                        "cursor": 0,
                    },
                )
            if request.method == "GET" and "/sandboxes/cpod-1/commands/proc-bg-1/logs" in url_str:
                cursor = request.url.params.get("cursor")
                self.assertEqual(cursor, "0")
                return httpx.Response(
                    200,
                    json={
                        "id": "proc-bg-1",
                        "content": "Serving HTTP on 0.0.0.0 port 8000\n",
                        "cursor": 34,
                        "running": True,
                        "exit_code": None,
                    },
                )
            if request.method == "POST" and url_str.endswith("/sandboxes/cpod-1/commands/proc-bg-1/kill"):
                return httpx.Response(200, json={"status": "killed", "id": "proc-bg-1"})
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            cmd = await client.start_command("cpod-1", "python3 -m http.server 8000")
            self.assertIsInstance(cmd, CommandInfo)
            self.assertEqual(cmd.id, "proc-bg-1")
            self.assertEqual(cmd.pid, 202)
            self.assertTrue(cmd.running)

            logs = await client.poll_command("cpod-1", cmd.id, cursor=0)
            self.assertIsInstance(logs, CommandLogs)
            self.assertEqual(logs.id, "proc-bg-1")
            self.assertEqual(logs.content, "Serving HTTP on 0.0.0.0 port 8000\n")
            self.assertEqual(logs.cursor, 34)
            self.assertTrue(logs.running)

            kill_res = await client.kill_command("cpod-1", cmd.id)
            self.assertEqual(kill_res["status"], "killed")

    async def test_file_read_write(self) -> None:
        stored_files: dict[str, bytes] = {}
        seen_params: list[dict[str, str | None]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "PUT" and "/sandboxes/cpod-1/files" in url_str:
                path = request.url.params.get("path")
                uid = request.url.params.get("uid")
                gid = request.url.params.get("gid")
                mode = request.url.params.get("mode")
                seen_params.append({"uid": uid, "gid": gid, "mode": mode})
                self.assertEqual(mode, "0644")
                assert path is not None
                stored_files[path] = request.content
                return httpx.Response(200, json={"path": path, "size": len(request.content)})
            if request.method == "GET" and "/sandboxes/cpod-1/files" in url_str:
                path = request.url.params.get("path")
                assert path is not None
                if path in stored_files:
                    return httpx.Response(200, content=stored_files[path])
                return httpx.Response(404)
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            write_res = await client.write_file(
                "cpod-1",
                "/workspace/test.txt",
                "hello from cubepod sdk",
            )
            self.assertEqual(write_res["path"], "/workspace/test.txt")
            self.assertEqual(write_res["size"], len(b"hello from cubepod sdk"))
            self.assertIsNone(seen_params[0]["uid"])
            self.assertIsNone(seen_params[0]["gid"])

            await client.write_file(
                "cpod-1",
                "/workspace/root.txt",
                "root-owned",
                uid=0,
                gid=0,
            )
            self.assertEqual(seen_params[1]["uid"], "0")
            self.assertEqual(seen_params[1]["gid"], "0")

            content = await client.read_file("cpod-1", "/workspace/test.txt")
            self.assertEqual(content, b"hello from cubepod sdk")

    async def test_archive_sync(self) -> None:
        seen_params: list[dict[str, str | None]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "POST" and "/sandboxes/cpod-1/files/archive-sync" in url_str:
                dest = request.url.params.get("dest")
                self.assertEqual(dest, "/workspace")
                seen_params.append({
                    "uid": request.url.params.get("uid"),
                    "gid": request.url.params.get("gid"),
                })
                # Decompress and verify in-memory tar.gz contents
                bio = io.BytesIO(request.content)
                with tarfile.open(fileobj=bio, mode="r:gz") as tar:
                    names = tar.getnames()
                    self.assertIn("skills/test/SKILL.md", names)
                    self.assertIn("scripts/run.sh", names)

                    f1 = tar.extractfile("skills/test/SKILL.md")
                    assert f1 is not None
                    self.assertEqual(f1.read(), b"# Test Skill")

                    f2 = tar.extractfile("scripts/run.sh")
                    assert f2 is not None
                    self.assertEqual(f2.read(), b"echo skill running")

                return httpx.Response(200, json={"status": "extracted", "dest": dest, "filesExtracted": 2})
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            res = await client.archive_sync(
                "cpod-1",
                files={
                    "/skills/test/SKILL.md": "# Test Skill",
                    "scripts/run.sh": b"echo skill running",
                },
                dest="/workspace",
            )
            self.assertEqual(res["status"], "extracted")
            self.assertEqual(res["filesExtracted"], 2)
            self.assertIsNone(seen_params[0]["uid"])
            self.assertIsNone(seen_params[0]["gid"])

            await client.archive_sync(
                "cpod-1",
                files={
                    "/skills/test/SKILL.md": "# Test Skill",
                    "scripts/run.sh": b"echo skill running",
                },
                dest="/workspace",
                uid=0,
                gid=0,
            )
            self.assertEqual(seen_params[1]["uid"], "0")
            self.assertEqual(seen_params[1]["gid"], "0")

    async def test_file_and_command_extensions(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            method = request.method
            if method == "GET" and "/sandboxes/cpod-1/files/list" in url_str:
                self.assertEqual(request.url.params.get("path"), "/workspace")
                return httpx.Response(
                    200,
                    json={
                        "entries": [
                            {"name": "test.txt", "path": "/workspace/test.txt", "is_dir": False, "size": 12, "mode": "0644"},
                            {"name": "src", "path": "/workspace/src", "is_dir": True, "size": 4096, "mode": "0755"},
                        ]
                    },
                )
            if method == "DELETE" and "/sandboxes/cpod-1/files" in url_str:
                self.assertEqual(request.url.params.get("path"), "/workspace/old.txt")
                return httpx.Response(200, json={"status": "deleted", "path": "/workspace/old.txt"})
            if method == "GET" and "/sandboxes/cpod-1/commands/cmd-abc/status" in url_str:
                return httpx.Response(
                    200,
                    json={"id": "cmd-abc", "pid": 1234, "running": False, "exit_code": 0, "cursor": 50, "output": "done"},
                )
            if method == "GET" and "/sandboxes/cpod-1/commands/cmd-stream/logs" in url_str:
                cursor = int(request.url.params.get("cursor", 0))
                if cursor == 0:
                    return httpx.Response(200, json={"id": "cmd-stream", "content": "part1\n", "cursor": 6, "running": True})
                return httpx.Response(200, json={"id": "cmd-stream", "content": "part2\n", "cursor": 12, "running": False, "exit_code": 0})
            if method == "POST" and url_str.endswith("/warmpools"):
                data = json.loads(request.content)
                self.assertEqual(data["name"], "wp-test")
                return httpx.Response(201, json={"name": "wp-test", "replicas": 2, "readyReplicas": 2})
            if method == "GET" and url_str.endswith("/warmpools"):
                return httpx.Response(200, json=[{"name": "wp-test", "replicas": 2, "readyReplicas": 2}])
            if method == "GET" and url_str.endswith("/templates"):
                return httpx.Response(200, json=[{"id": "cubeplex-desktop", "image": "ubuntu:24.04"}])
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            entries = await client.list_files("cpod-1", "/workspace")
            self.assertEqual(len(entries), 2)
            self.assertEqual(entries[0].name, "test.txt")
            self.assertFalse(entries[0].is_dir)
            self.assertTrue(entries[1].is_dir)

            del_res = await client.delete_file("cpod-1", "/workspace/old.txt")
            self.assertEqual(del_res["status"], "deleted")

            status = await client.get_command_status("cpod-1", "cmd-abc")
            self.assertEqual(status.id, "cmd-abc")
            self.assertEqual(status.exit_code, 0)
            self.assertFalse(status.running)

            chunks = []
            async for chunk in client.stream_command("cpod-1", "cmd-stream", poll_interval=0.01):
                chunks.append(chunk.content)
            self.assertEqual(chunks, ["part1\n", "part2\n"])

            wp = await client.create_warmpool("wp-test", replicas=2)
            self.assertEqual(wp["name"], "wp-test")

            wps = await client.list_warmpools()
            self.assertEqual(len(wps), 1)

            tpls = await client.list_templates()
            self.assertEqual(len(tpls), 1)

    async def test_transient_retry(self) -> None:
        attempts = 0

        def handler(request: httpx.Request) -> httpx.Response:
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                return httpx.Response(503, text="Service Unavailable")
            return httpx.Response(200, json={"sandboxID": "cpod-retry", "templateID": "t1", "state": "Running"})

        async with AsyncCubePod(transport=httpx.MockTransport(handler), max_retries=2) as client:
            info = await client.get("cpod-retry")
            self.assertEqual(info.sandbox_id, "cpod-retry")
            self.assertEqual(attempts, 2)

    async def test_template_catalog_async(self) -> None:
        recorded: list[dict[str, Any]] = []
        legacy_list = False
        status_seq: list[dict[str, Any]] = []
        status_idx = 0

        def handler(request: httpx.Request) -> httpx.Response:
            nonlocal status_idx
            url_str = str(request.url)
            raw_path = request.url.raw_path.decode("ascii", errors="ignore")
            method = request.method
            body = None
            if request.content:
                try:
                    body = json.loads(request.content)
                except Exception:
                    body = request.content
            recorded.append({
                "method": method,
                "url": url_str,
                "raw_path": raw_path,
                "headers": dict(request.headers),
                "body": body,
                "raw": request.content.decode("utf-8", errors="ignore"),
            })

            if method == "GET" and request.url.path == "/templates":
                if legacy_list:
                    return httpx.Response(200, json={"templates": [{"id": "cubeplex-desktop", "name": "Desktop"}]})
                return httpx.Response(200, json=[{
                    "templateID": "tpl_test_async",
                    "buildID": "bld_1",
                    "cpuCount": 2,
                    "memoryMB": 2048,
                    "diskSizeMB": 10240,
                    "public": True,
                    "aliases": ["py-async"],
                    "names": ["py-async"],
                    "buildStatus": "uploaded",
                    "envdVersion": "cubepod-podd",
                    "spawnCount": 3,
                    "buildCount": 1,
                }])

            if method == "GET" and request.url.path == "/templates/tpl_test_async":
                return httpx.Response(200, json={
                    "templateID": "tpl_test_async",
                    "buildID": "bld_1",
                    "buildStatus": "ready",
                })

            if method == "GET" and request.url.path == "/templates/not-found":
                return httpx.Response(404, json={"code": 404, "message": "not found"})

            if method == "GET" and raw_path.startswith("/templates/aliases/"):
                return httpx.Response(200, json={
                    "templateID": "tpl_test_async",
                    "aliases": ["py-async"],
                })

            if method == "POST" and request.url.path == "/v3/templates":
                return httpx.Response(202, json={
                    "templateID": "tpl_created_async",
                    "buildID": "bld_created_async",
                    "aliases": ["new-async"],
                    "names": ["new-async"],
                    "tags": ["v1"],
                })

            if method == "POST" and "/builds/" in request.url.path:
                return httpx.Response(202)

            if method == "GET" and request.url.path.endswith("/status"):
                if status_seq:
                    idx = min(status_idx, len(status_seq) - 1)
                    status_idx += 1
                    return httpx.Response(200, json=status_seq[idx])
                return httpx.Response(200, json={
                    "templateID": "tpl_created_async",
                    "buildID": "bld_created_async",
                    "status": "ready",
                    "logs": [],
                    "logEntries": [],
                })

            if method == "DELETE" and request.url.path.startswith("/templates/"):
                if "conflict" in request.url.path:
                    return httpx.Response(409, json={"code": 409, "message": "template in use"})
                return httpx.Response(204)

            if method == "PATCH" and request.url.path.startswith("/templates/"):
                return httpx.Response(200, json={"names": ["py-async"]})

            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            # 1. list_templates (array shape)
            tpls = await client.list_templates()
            self.assertEqual(len(tpls), 1)
            self.assertEqual(tpls[0]["templateID"], "tpl_test_async")
            self.assertEqual(tpls[0]["buildStatus"], "uploaded")
            self.assertEqual(tpls[0]["cpuCount"], 2)

            # 2. list_templates (legacy wrapper)
            legacy_list = True
            leg_tpls = await client.list_templates()
            self.assertEqual(len(leg_tpls), 1)
            self.assertEqual(leg_tpls[0]["id"], "cubeplex-desktop")

            # 3. get_template
            tpl = await client.get_template("tpl_test_async")
            self.assertEqual(tpl["templateID"], "tpl_test_async")

            with self.assertRaises(httpx.HTTPStatusError) as ctx_err:
                await client.get_template("not-found")
            self.assertEqual(ctx_err.exception.response.status_code, 404)

            # 4. get_template_by_alias (URL-escaped org/tpl:v1)
            alias_tpl = await client.get_template_by_alias("org/tpl:v1")
            self.assertEqual(alias_tpl["templateID"], "tpl_test_async")
            self.assertEqual(recorded[-1]["raw_path"], "/templates/aliases/org%2Ftpl%3Av1")

            # 5. create_template_from_image with 202 polling (waiting -> building -> ready)
            status_seq = [
                {"templateID": "tpl_created_async", "buildID": "bld_created_async", "status": "waiting"},
                {"templateID": "tpl_created_async", "buildID": "bld_created_async", "status": "building"},
                {"templateID": "tpl_created_async", "buildID": "bld_created_async", "status": "ready"},
            ]
            status_idx = 0
            created = await client.create_template_from_image(
                from_image="python:3.13",
                alias="new-async",
                tags=["v1"],
                from_image_registry={"type": "registry", "username": "ci", "password": "secret"},
                idempotency_key="idem-async-1",
                cpu_count=2,
                memory_mb=2048,
                min_free_disk_mb=10240,
                poll_interval=0.001,
            )
            self.assertEqual(created["templateID"], "tpl_created_async")
            self.assertEqual(created["buildID"], "bld_created_async")
            self.assertIn("new-async", created["aliases"])
            self.assertEqual(created["tags"], ["v1"])
            self.assertEqual(status_idx, 3)

            # Check v3 request
            v3_call = recorded[-5]
            self.assertEqual(v3_call["method"], "POST")
            self.assertTrue(v3_call["url"].endswith("/v3/templates"))
            self.assertEqual(v3_call["headers"].get("idempotency-key"), "idem-async-1")
            self.assertEqual(v3_call["body"]["alias"], "new-async")
            self.assertEqual(v3_call["body"]["tags"], ["v1"])
            self.assertEqual(v3_call["body"]["cpuCount"], 2)
            self.assertEqual(v3_call["body"]["memoryMB"], 2048)
            self.assertEqual(v3_call["body"]["minFreeDiskMb"], 10240)
            self.assertNotIn("name", v3_call["body"])

            # Check v2 build request
            v2_call = recorded[-4]
            self.assertEqual(v2_call["method"], "POST")
            self.assertTrue("/v2/templates/tpl_created_async/builds/bld_created_async" in v2_call["url"])
            self.assertEqual(v2_call["body"]["fromImage"], "python:3.13")
            self.assertEqual(v2_call["body"]["fromImageRegistry"]["username"], "ci")
            self.assertEqual(v2_call["body"]["steps"], [])
            self.assertNotIn("dockerfile", v2_call["raw"].lower())

            # wait=False does not poll
            before_nowait = len(recorded)
            await client.create_template_from_image(from_image="python:3.13", alias="no-wait", wait=False)
            self.assertEqual(len(recorded) - before_nowait, 2)

            # 6. delete_template (204 No Content, force=False and force=True)
            await client.delete_template("tpl_created_async", force=False)
            self.assertEqual(recorded[-1]["method"], "DELETE")
            self.assertNotIn("force=true", recorded[-1]["url"])

            await client.delete_template("tpl_created_async", force=True)
            self.assertIn("force=true", recorded[-1]["url"])

            # 409 conflict surfaces error body
            with self.assertRaises(httpx.HTTPStatusError) as conflict_ctx:
                await client.delete_template("conflict-tpl")
            self.assertEqual(conflict_ctx.exception.response.status_code, 409)
            self.assertIn("template in use", conflict_ctx.exception.response.text)

            # 7. set_template_public (True and False)
            await client.set_template_public("tpl_created_async")
            self.assertEqual(recorded[-1]["method"], "PATCH")
            self.assertEqual(recorded[-1]["body"], {"public": True})

            await client.set_template_public("org/tpl:v1", public=False)
            self.assertEqual(recorded[-1]["raw_path"], "/templates/org%2Ftpl%3Av1")
            self.assertEqual(recorded[-1]["body"], {"public": False})

            # 8. build_template (ready + error with object reason)
            status_seq = []
            bld_res = await client.build_template(
                name="async-builder",
                alias="async-bld",
                from_image="ubuntu:24.04",
                steps=[{"type": "RUN", "args": ["echo hi"]}],
                start_cmd="echo start",
                ready_cmd="true",
                force=True,
                poll_interval=0.001,
            )
            self.assertEqual(bld_res["templateID"], "tpl_created_async")
            self.assertEqual(recorded[-2]["body"]["steps"], [{"type": "RUN", "args": ["echo hi"]}])
            self.assertEqual(recorded[-2]["body"]["startCmd"], "echo start")
            self.assertEqual(recorded[-2]["body"]["readyCmd"], "true")
            self.assertTrue(recorded[-2]["body"]["force"])

            status_seq = [
                {"templateID": "tpl_created_async", "buildID": "bld_created_async", "status": "building"},
                {
                    "templateID": "tpl_created_async",
                    "buildID": "bld_created_async",
                    "status": "error",
                    "reason": {"message": "pip install failed", "step": "RUN pip install"},
                },
            ]
            status_idx = 0
            with self.assertRaises(RuntimeError) as err_ctx:
                await client.build_template(
                    alias="async-err",
                    from_image="ubuntu:24.04",
                    steps=[{"type": "RUN", "args": ["pip install"]}],
                    poll_interval=0.001,
                )
            self.assertIn("pip install failed", str(err_ctx.exception))
            self.assertIn("RUN pip install", str(err_ctx.exception))


class TestSyncCubePod(unittest.TestCase):
    def test_sync_client_complete(self) -> None:
        attempts = 0

        def handler(request: httpx.Request) -> httpx.Response:
            nonlocal attempts
            url_str = str(request.url)
            method = request.method

            if method == "POST" and url_str.endswith("/sandboxes"):
                return httpx.Response(
                    201,
                    json={"sandboxID": "cpod-sync-1", "templateID": "cubeplex-desktop", "state": "Running", "envdPort": 49983, "rootfsGeneration": 1},
                )
            if method == "GET" and url_str.endswith("/sandboxes"):
                return httpx.Response(
                    200,
                    json=[{"sandboxID": "cpod-sync-1", "templateID": "cubeplex-desktop", "state": "Running", "envdPort": 49983, "rootfsGeneration": 1}],
                )
            if method == "GET" and url_str.endswith("/sandboxes/cpod-sync-1"):
                attempts += 1
                if attempts == 1:
                    return httpx.Response(502, text="Bad Gateway")
                return httpx.Response(
                    200,
                    json={"sandboxID": "cpod-sync-1", "templateID": "cubeplex-desktop", "state": "Running", "envdPort": 49983, "rootfsGeneration": 1},
                )
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/pause"):
                return httpx.Response(200, json={"sandboxID": "cpod-sync-1", "templateID": "cubeplex-desktop", "state": "Paused"})
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/connect"):
                return httpx.Response(200, json={"sandboxID": "cpod-sync-1", "templateID": "cubeplex-desktop", "state": "Running"})
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/reset-rootfs"):
                return httpx.Response(200, json={"sandboxID": "cpod-sync-1", "templateID": "cubeplex-desktop", "state": "Running", "rootfsGeneration": 2})
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/fork"):
                return httpx.Response(201, json={"sandboxID": "cpod-sync-fork", "templateID": "cubeplex-desktop", "state": "Running"})
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/timeout"):
                return httpx.Response(200, json={"status": "ok", "timeoutSeconds": 3600})
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/lease"):
                return httpx.Response(200, json={"status": "ok", "leaseSeconds": 300})
            if method == "POST" and "/sandboxes/cpod-sync-1/endpoints/8080" in url_str:
                return httpx.Response(200, json={"url": "http://proxy/panel/tok", "token": "tok"})
            if method == "POST" and url_str.endswith("/sandboxes/cpod-sync-1/commands/run"):
                return httpx.Response(200, json={"id": "cmd-1", "output": "hello world", "exit_code": 0, "running": False})
            if method == "GET" and "/sandboxes/cpod-sync-1/commands/cmd-1/status" in url_str:
                return httpx.Response(200, json={"id": "cmd-1", "exit_code": 0, "running": False})
            if method == "GET" and "/sandboxes/cpod-sync-1/commands/cmd-1/logs" in url_str:
                return httpx.Response(200, json={"id": "cmd-1", "content": "logs", "cursor": 4, "running": False})
            if method == "POST" and "/sandboxes/cpod-sync-1/commands/cmd-1/kill" in url_str:
                return httpx.Response(200, json={"status": "killed"})
            if method == "PUT" and "/sandboxes/cpod-sync-1/files" in url_str:
                return httpx.Response(200, json={"status": "ok"})
            if method == "GET" and "/sandboxes/cpod-sync-1/files/list" in url_str:
                return httpx.Response(200, json={"entries": [{"name": "app.py", "path": "/workspace/app.py", "is_dir": False, "size": 100}]})
            if method == "GET" and "/sandboxes/cpod-sync-1/files" in url_str:
                return httpx.Response(200, content=b"code content")
            if method == "DELETE" and "/sandboxes/cpod-sync-1/files" in url_str:
                return httpx.Response(200, json={"status": "ok"})
            if method == "POST" and "/sandboxes/cpod-sync-1/files/archive-sync" in url_str:
                return httpx.Response(200, json={"status": "extracted"})
            if method == "POST" and url_str.endswith("/warmpools"):
                return httpx.Response(201, json={"name": "wp1", "replicas": 3})
            if method == "GET" and url_str.endswith("/warmpools"):
                return httpx.Response(200, json=[{"name": "wp1", "replicas": 3}])
            if method == "GET" and url_str.endswith("/templates"):
                return httpx.Response(200, json=[{"id": "cubeplex-desktop"}])
            if method == "DELETE" and url_str.endswith("/sandboxes/cpod-sync-1"):
                return httpx.Response(204)
            return httpx.Response(404)

        # Test SyncCubePod alias as well
        self.assertIs(CubePod, SyncCubePod)

        with CubePod(transport=httpx.MockTransport(handler), max_retries=2) as client:
            sb = client.create("cubeplex-desktop")
            self.assertEqual(sb.sandbox_id, "cpod-sync-1")

            sbs = client.list()
            self.assertEqual(len(sbs), 1)

            # Test retry logic on 502
            sb_get = client.get("cpod-sync-1")
            self.assertEqual(sb_get.sandbox_id, "cpod-sync-1")
            self.assertEqual(attempts, 2)

            client.pause("cpod-sync-1")
            client.connect("cpod-sync-1")
            client.reset_rootfs("cpod-sync-1")
            client.fork("cpod-sync-1")
            client.renew_timeout("cpod-sync-1", 3600)
            client.acquire_lease("cpod-sync-1", 300)

            panel = client.get_panel_endpoint("cpod-sync-1", 8080)
            self.assertEqual(panel.token, "tok")

            res = client.execute("cpod-sync-1", "echo hi")
            self.assertEqual(res.output, "hello world")

            cmd_info = client.get_command_status("cpod-sync-1", "cmd-1")
            self.assertEqual(cmd_info.exit_code, 0)

            logs = client.poll_command("cpod-sync-1", "cmd-1")
            self.assertEqual(logs.content, "logs")

            chunks = list(client.stream_command("cpod-sync-1", "cmd-1"))
            self.assertEqual(len(chunks), 1)

            client.kill_command("cpod-sync-1", "cmd-1")

            client.write_file("cpod-sync-1", "/workspace/test", b"content")
            data = client.read_file("cpod-sync-1", "/workspace/test")
            self.assertEqual(data, b"code content")

            files = client.list_files("cpod-sync-1", "/workspace")
            self.assertEqual(len(files), 1)
            self.assertEqual(files[0].name, "app.py")

            client.delete_file("cpod-sync-1", "/workspace/test")
            client.archive_sync("cpod-sync-1", {"test.txt": "abc"})

            wp = client.create_warmpool("wp1", replicas=3)
            self.assertEqual(wp["name"], "wp1")

            wps = client.list_warmpools()
            self.assertEqual(len(wps), 1)

            tpls = client.list_templates()
            self.assertEqual(len(tpls), 1)

            client.delete("cpod-sync-1")

    def test_template_catalog_sync(self) -> None:
        recorded: list[dict[str, Any]] = []
        legacy_list = False
        status_seq: list[dict[str, Any]] = []
        status_idx = 0

        def handler(request: httpx.Request) -> httpx.Response:
            nonlocal status_idx
            url_str = str(request.url)
            raw_path = request.url.raw_path.decode("ascii", errors="ignore")
            method = request.method
            body = None
            if request.content:
                try:
                    body = json.loads(request.content)
                except Exception:
                    body = request.content
            recorded.append({
                "method": method,
                "url": url_str,
                "raw_path": raw_path,
                "headers": dict(request.headers),
                "body": body,
                "raw": request.content.decode("utf-8", errors="ignore"),
            })

            if method == "GET" and request.url.path == "/templates":
                if legacy_list:
                    return httpx.Response(200, json={"templates": [{"id": "cubeplex-desktop", "name": "Desktop"}]})
                return httpx.Response(200, json=[{
                    "templateID": "tpl_test_sync",
                    "buildID": "bld_sync_1",
                    "cpuCount": 4,
                    "memoryMB": 4096,
                    "diskSizeMB": 20480,
                    "public": False,
                    "aliases": ["py-sync"],
                    "names": ["py-sync"],
                    "buildStatus": "uploaded",
                    "envdVersion": "cubepod-podd",
                    "spawnCount": 1,
                    "buildCount": 1,
                }])

            if method == "GET" and request.url.path == "/templates/tpl_test_sync":
                return httpx.Response(200, json={
                    "templateID": "tpl_test_sync",
                    "buildID": "bld_sync_1",
                    "buildStatus": "ready",
                })

            if method == "GET" and request.url.path == "/templates/not-found":
                return httpx.Response(404, json={"code": 404, "message": "not found"})

            if method == "GET" and raw_path.startswith("/templates/aliases/"):
                return httpx.Response(200, json={
                    "templateID": "tpl_test_sync",
                    "aliases": ["py-sync"],
                })

            if method == "POST" and request.url.path == "/v3/templates":
                return httpx.Response(202, json={
                    "templateID": "tpl_created_sync",
                    "buildID": "bld_created_sync",
                    "aliases": ["new-sync"],
                    "names": ["new-sync"],
                    "tags": ["v1"],
                })

            if method == "POST" and "/builds/" in request.url.path:
                return httpx.Response(202)

            if method == "GET" and request.url.path.endswith("/status"):
                if status_seq:
                    idx = min(status_idx, len(status_seq) - 1)
                    status_idx += 1
                    return httpx.Response(200, json=status_seq[idx])
                return httpx.Response(200, json={
                    "templateID": "tpl_created_sync",
                    "buildID": "bld_created_sync",
                    "status": "ready",
                    "logs": [],
                    "logEntries": [],
                })

            if method == "DELETE" and request.url.path.startswith("/templates/"):
                if "conflict" in request.url.path:
                    return httpx.Response(409, json={"code": 409, "message": "template in use"})
                return httpx.Response(204)

            if method == "PATCH" and request.url.path.startswith("/templates/"):
                return httpx.Response(200, json={"names": ["py-sync"]})

            return httpx.Response(404)

        with SyncCubePod(transport=httpx.MockTransport(handler)) as client:
            # 1. list_templates (array shape)
            tpls = client.list_templates()
            self.assertEqual(len(tpls), 1)
            self.assertEqual(tpls[0]["templateID"], "tpl_test_sync")
            self.assertEqual(tpls[0]["buildStatus"], "uploaded")
            self.assertEqual(tpls[0]["cpuCount"], 4)

            # 2. list_templates (legacy wrapper)
            legacy_list = True
            leg_tpls = client.list_templates()
            self.assertEqual(len(leg_tpls), 1)
            self.assertEqual(leg_tpls[0]["id"], "cubeplex-desktop")

            # 3. get_template
            tpl = client.get_template("tpl_test_sync")
            self.assertEqual(tpl["templateID"], "tpl_test_sync")

            with self.assertRaises(httpx.HTTPStatusError) as ctx_err:
                client.get_template("not-found")
            self.assertEqual(ctx_err.exception.response.status_code, 404)

            # 4. get_template_by_alias (URL-escaped org/tpl:v1)
            alias_tpl = client.get_template_by_alias("org/tpl:v1")
            self.assertEqual(alias_tpl["templateID"], "tpl_test_sync")
            self.assertEqual(recorded[-1]["raw_path"], "/templates/aliases/org%2Ftpl%3Av1")

            # 5. create_template_from_image with 202 polling (waiting -> building -> ready)
            status_seq = [
                {"templateID": "tpl_created_sync", "buildID": "bld_created_sync", "status": "waiting"},
                {"templateID": "tpl_created_sync", "buildID": "bld_created_sync", "status": "building"},
                {"templateID": "tpl_created_sync", "buildID": "bld_created_sync", "status": "ready"},
            ]
            status_idx = 0
            created = client.create_template_from_image(
                from_image="python:3.13",
                alias="new-sync",
                tags=["v1"],
                from_image_registry={"type": "registry", "username": "ci", "password": "secret"},
                idempotency_key="idem-sync-1",
                cpu_count=4,
                memory_mb=4096,
                min_free_disk_mb=20480,
                poll_interval=0.001,
            )
            self.assertEqual(created["templateID"], "tpl_created_sync")
            self.assertEqual(created["buildID"], "bld_created_sync")
            self.assertIn("new-sync", created["aliases"])
            self.assertEqual(created["tags"], ["v1"])
            self.assertEqual(status_idx, 3)

            # Check v3 request
            v3_call = recorded[-5]
            self.assertEqual(v3_call["method"], "POST")
            self.assertTrue(v3_call["url"].endswith("/v3/templates"))
            self.assertEqual(v3_call["headers"].get("idempotency-key"), "idem-sync-1")
            self.assertEqual(v3_call["body"]["alias"], "new-sync")
            self.assertEqual(v3_call["body"]["tags"], ["v1"])
            self.assertEqual(v3_call["body"]["cpuCount"], 4)
            self.assertEqual(v3_call["body"]["memoryMB"], 4096)
            self.assertEqual(v3_call["body"]["minFreeDiskMb"], 20480)
            self.assertNotIn("name", v3_call["body"])

            # Check v2 build request
            v2_call = recorded[-4]
            self.assertEqual(v2_call["method"], "POST")
            self.assertTrue("/v2/templates/tpl_created_sync/builds/bld_created_sync" in v2_call["url"])
            self.assertEqual(v2_call["body"]["fromImage"], "python:3.13")
            self.assertEqual(v2_call["body"]["fromImageRegistry"]["username"], "ci")
            self.assertEqual(v2_call["body"]["steps"], [])
            self.assertNotIn("dockerfile", v2_call["raw"].lower())

            # wait=False does not poll
            before_nowait = len(recorded)
            client.create_template_from_image(from_image="python:3.13", alias="no-wait", wait=False)
            self.assertEqual(len(recorded) - before_nowait, 2)

            # 6. delete_template (204 No Content, force=False and force=True)
            client.delete_template("tpl_created_sync", force=False)
            self.assertEqual(recorded[-1]["method"], "DELETE")
            self.assertNotIn("force=true", recorded[-1]["url"])

            client.delete_template("tpl_created_sync", force=True)
            self.assertIn("force=true", recorded[-1]["url"])

            # 409 conflict surfaces error body
            with self.assertRaises(httpx.HTTPStatusError) as conflict_ctx:
                client.delete_template("conflict-tpl")
            self.assertEqual(conflict_ctx.exception.response.status_code, 409)
            self.assertIn("template in use", conflict_ctx.exception.response.text)

            # 7. set_template_public (True and False)
            client.set_template_public("tpl_created_sync")
            self.assertEqual(recorded[-1]["method"], "PATCH")
            self.assertEqual(recorded[-1]["body"], {"public": True})

            client.set_template_public("org/tpl:v1", public=False)
            self.assertEqual(recorded[-1]["raw_path"], "/templates/org%2Ftpl%3Av1")
            self.assertEqual(recorded[-1]["body"], {"public": False})

            # 8. build_template (ready + error with object reason)
            status_seq = []
            bld_res = client.build_template(
                name="sync-builder",
                alias="sync-bld",
                from_image="ubuntu:24.04",
                steps=[{"type": "RUN", "args": ["echo sync"]}],
                start_cmd="echo start",
                ready_cmd="true",
                force=True,
                poll_interval=0.001,
            )
            self.assertEqual(bld_res["templateID"], "tpl_created_sync")
            self.assertEqual(recorded[-2]["body"]["steps"], [{"type": "RUN", "args": ["echo sync"]}])
            self.assertEqual(recorded[-2]["body"]["startCmd"], "echo start")
            self.assertEqual(recorded[-2]["body"]["readyCmd"], "true")
            self.assertTrue(recorded[-2]["body"]["force"])

            status_seq = [
                {"templateID": "tpl_created_sync", "buildID": "bld_created_sync", "status": "building"},
                {
                    "templateID": "tpl_created_sync",
                    "buildID": "bld_created_sync",
                    "status": "error",
                    "reason": {"message": "apt-get failed", "step": "RUN apt-get"},
                },
            ]
            status_idx = 0
            with self.assertRaises(RuntimeError) as err_ctx:
                client.build_template(
                    alias="sync-err",
                    from_image="ubuntu:24.04",
                    steps=[{"type": "RUN", "args": ["apt-get"]}],
                    poll_interval=0.001,
                )
            self.assertIn("apt-get failed", str(err_ctx.exception))
            self.assertIn("RUN apt-get", str(err_ctx.exception))


class _TrackingAsyncStream(httpx.AsyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks
        self.closed = False

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for chunk in self._chunks:
            yield chunk

    async def aclose(self) -> None:
        self.closed = True


class _TrackingSyncStream(httpx.SyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks
        self.closed = False

    def __iter__(self) -> Iterator[bytes]:
        for chunk in self._chunks:
            yield chunk

    def close(self) -> None:
        self.closed = True


class TestV04FeaturesAsync(unittest.IsolatedAsyncioTestCase):
    async def test_url_normalization_and_trust_env_default(self) -> None:
        with mock.patch.dict(os.environ, {"HTTP_PROXY": "http://127.0.0.1:9", "ALL_PROXY": "http://127.0.0.1:9"}):
            async with AsyncCubePod(base_url="gw.local:8080/") as client:
                self.assertEqual(str(client._client.base_url), "http://gw.local:8080")
                self.assertFalse(client._client._trust_env)
                self.assertEqual(client._client.timeout.read, 60.0)

            async with AsyncCubePod(base_url="https://gw.example.com/", timeout=15.0, trust_env=True) as client2:
                self.assertEqual(str(client2._client.base_url), "https://gw.example.com")
                self.assertTrue(client2._client._trust_env)
                self.assertEqual(client2._client.timeout.read, 15.0)

    async def test_create_fork_get_raw_and_sandbox_info_properties(self) -> None:
        recorded: list[dict[str, Any]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            body = json.loads(request.content) if request.content else None
            recorded.append({"method": request.method, "path": request.url.path, "body": body})
            if request.method == "POST" and request.url.path == "/sandboxes":
                return httpx.Response(
                    201,
                    json={
                        "sandboxID": "sb-v04",
                        "templateID": "tpl-1",
                        "state": "Running",
                        "metadata": {"tenant": "acme", "env": "prod"},
                        "customField": "preserved",
                    },
                )
            if request.method == "POST" and request.url.path == "/sandboxes/sb-v04/fork":
                return httpx.Response(
                    201,
                    json={
                        "sandboxID": "sb-v04-fork",
                        "templateID": "tpl-1",
                        "state": "Stopped",
                        "metadata": {"forked": "true"},
                    },
                )
            if request.method == "GET" and request.url.path == "/sandboxes/sb-v04":
                return httpx.Response(
                    200,
                    json={
                        "sandboxID": "sb-v04",
                        "templateID": "tpl-1",
                        "state": "Terminated",
                        "metadata": {"tenant": "acme"},
                        "extra": {"k": 1},
                    },
                )
            return httpx.Response(404)

        async with AsyncCubePod(base_url="gw.local:8080", transport=httpx.MockTransport(handler)) as client:
            created = await client.create(
                template_id="tpl-1",
                volume_key="vol-1",
                warm_pool_ref="pool-fast",
                resource={"cpu": "4", "memory": "8Gi"},
                timeout_seconds=900,
                metadata={"tenant": "acme"},
            )
            self.assertEqual(recorded[-1]["body"]["timeoutSeconds"], 900)
            self.assertEqual(
                recorded[-1]["body"]["extension"],
                {
                    "volumeKey": "vol-1",
                    "warmPoolRef": "pool-fast",
                    "resource": {"cpu": "4", "memory": "8Gi"},
                },
            )
            self.assertEqual(created.metadata, {"tenant": "acme", "env": "prod"})
            self.assertEqual(created.raw["customField"], "preserved")
            self.assertFalse(created.is_terminal)

            forked = await client.fork("sb-v04", volume_key="vol-fork", metadata={"forked": "true"})
            self.assertEqual(
                recorded[-1]["body"],
                {"metadata": {"forked": "true"}, "extension": {"volumeKey": "vol-fork"}},
            )
            self.assertTrue(forked.is_terminal)

            # Bodyless fork remains supported
            await client.fork("sb-v04")
            self.assertIsNone(recorded[-1]["body"])

            raw = await client.get_raw("sb-v04")
            self.assertEqual(raw["extra"], {"k": 1})

        for state in ("Stopped", "Terminated", "Failed", "Killed", "Succeed"):
            self.assertTrue(SandboxInfo("id", "tpl", state).is_terminal)
        for state in ("Running", "Paused", "Creating", "Starting"):
            self.assertFalse(SandboxInfo("id", "tpl", state).is_terminal)

    async def test_execute_and_start_command_optional_uid_gid_and_timeout(self) -> None:
        recorded_bodies: list[dict[str, Any]] = []
        recorded_timeouts: list[Any] = []

        def handler(request: httpx.Request) -> httpx.Response:
            recorded_bodies.append(json.loads(request.content))
            recorded_timeouts.append(request.extensions.get("timeout"))
            return httpx.Response(200, json={"id": "c1", "exit_code": 0, "output": "ok", "running": False})

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            await client.execute("sb-1", "whoami", uid=None, gid=None, timeout=12.5)
            self.assertNotIn("uid", recorded_bodies[0])
            self.assertNotIn("gid", recorded_bodies[0])
            self.assertEqual(recorded_timeouts[0]["read"], 12.5)

            await client.start_command("sb-1", "sleep 1", uid=None, gid=0, timeout=5.0)
            self.assertNotIn("uid", recorded_bodies[1])
            self.assertEqual(recorded_bodies[1]["gid"], 0)
            self.assertEqual(recorded_timeouts[1]["read"], 5.0)

    async def test_archive_sync_prebuilt_bytes_and_remove_paths(self) -> None:
        prebuilt = _build_tar_gz_bytes({"main.py": "print(1)\n"})
        seen: list[tuple[bytes, str | None]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            seen.append((request.content, request.url.params.get("remove_paths")))
            return httpx.Response(200, json={"status": "extracted"})

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            res = await client.archive_sync(
                "sb-1",
                prebuilt,
                dest="/workspace",
                remove_paths=["stale.txt", "", "old/a.py"],
            )
            self.assertEqual(res["status"], "extracted")
            self.assertEqual(seen[0][0], prebuilt)
            self.assertEqual(seen[0][1], "stale.txt,old/a.py")

    async def test_stream_file_read_text_file_and_list_files_modified_at(self) -> None:
        streams: list[_TrackingAsyncStream] = []

        def handler(request: httpx.Request) -> httpx.Response:
            if request.url.path == "/sandboxes/sb-1/files/list":
                return httpx.Response(
                    200,
                    json={
                        "entries": [
                            {"name": "a.txt", "path": "/workspace/a.txt", "is_dir": False, "size": 5, "modified_at": "2026-11-13T10:00:00Z"},
                            {"name": "b.txt", "path": "/workspace/b.txt", "is_dir": False, "size": 5, "modifiedAt": "2026-11-13T10:01:00Z"},
                            {"name": "c.txt", "path": "/workspace/c.txt", "is_dir": False, "size": 5, "mtime": "1763028120"},
                        ]
                    },
                )
            path = request.url.params.get("path")
            if path == "/missing":
                err_stream = _TrackingAsyncStream([b"not found"])
                streams.append(err_stream)
                return httpx.Response(404, stream=err_stream)
            if path == "/big.txt":
                big_stream = _TrackingAsyncStream([b"12345", b"67890", b"!"])
                streams.append(big_stream)
                return httpx.Response(200, stream=big_stream)
            ok_stream = _TrackingAsyncStream([b"hello ", b"world"])
            streams.append(ok_stream)
            return httpx.Response(200, stream=ok_stream)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            entries = await client.list_files("sb-1", "/workspace")
            self.assertEqual(entries[0].modified_at, "2026-11-13T10:00:00Z")
            self.assertEqual(entries[1].modified_at, "2026-11-13T10:01:00Z")
            self.assertEqual(entries[2].modified_at, "1763028120")

            # 404 raises before returning generator and closes response
            with self.assertRaises(httpx.HTTPStatusError) as exc_ctx:
                await client.stream_file("sb-1", "/missing")
            self.assertEqual(exc_ctx.exception.response.status_code, 404)
            self.assertTrue(streams[-1].closed)

            # Successful stream_file yields all chunks and closes response
            stream = await client.stream_file("sb-1", "/ok.txt")
            got = [chunk async for chunk in stream]
            self.assertEqual(b"".join(got), b"hello world")
            self.assertTrue(streams[-1].closed)

            # read_text_file within max_bytes
            text = await client.read_text_file("sb-1", "/ok.txt", max_bytes=11)
            self.assertEqual(text, "hello world")
            self.assertTrue(streams[-1].closed)

            # read_text_file exceeding max_bytes aborts, closes stream, raises CubePodFileTooLargeError
            with self.assertRaises(CubePodFileTooLargeError):
                await client.read_text_file("sb-1", "/big.txt", max_bytes=8)
            self.assertTrue(streams[-1].closed)

    async def test_desktop_routes(self) -> None:
        recorded: list[dict[str, Any]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            body = json.loads(request.content) if request.content else None
            recorded.append({"method": request.method, "path": request.url.path, "body": body})
            if request.url.path == "/sandboxes/sb-1/desktop/start":
                return httpx.Response(
                    200,
                    json={
                        "status": "running",
                        "desktopUrl": "/panel/tok1/vnc.html",
                        "terminalUrl": "/panel/tok2/",
                        "cdpUrl": "/panel/tok3/",
                        "expiresAt": "2026-11-13T18:00:00Z",
                    },
                )
            if request.url.path == "/sandboxes/sb-1/desktop/status":
                return httpx.Response(200, json={"status": "running", "display": ":99"})
            if request.url.path == "/sandboxes/sb-1/desktop/stop":
                return httpx.Response(200, json={"status": "stopped"})
            return httpx.Response(404)

        async with AsyncCubePod(transport=httpx.MockTransport(handler)) as client:
            sess = await client.start_desktop(
                "sb-1",
                resolution="1920x1080x24",
                initial_url="https://example.com",
                expires_in_seconds=1800,
            )
            self.assertIsInstance(sess, DesktopSession)
            self.assertEqual(sess.status, "running")
            self.assertEqual(sess.desktop_url, "/panel/tok1/vnc.html")
            self.assertEqual(sess.terminal_url, "/panel/tok2/")
            self.assertEqual(sess.cdp_url, "/panel/tok3/")
            self.assertEqual(sess.expires_at, "2026-11-13T18:00:00Z")
            self.assertEqual(
                recorded[0]["body"],
                {
                    "expiresInSeconds": 1800,
                    "resolution": "1920x1080x24",
                    "initialUrl": "https://example.com",
                },
            )

            st = await client.get_desktop_status("sb-1")
            self.assertEqual(st["status"], "running")

            stopped = await client.stop_desktop("sb-1")
            self.assertEqual(stopped["status"], "stopped")


class TestV04FeaturesSync(unittest.TestCase):
    def test_sync_v04_enhancements(self) -> None:
        streams: list[_TrackingSyncStream] = []
        recorded: list[dict[str, Any]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            if request.url.path == "/sandboxes/sb-1/files/archive-sync":
                recorded.append({
                    "method": request.method,
                    "path": request.url.path,
                    "remove_paths": request.url.params.get("remove_paths"),
                    "content": request.content,
                })
                return httpx.Response(200, json={"status": "extracted"})

            body = json.loads(request.content) if request.content else None
            recorded.append({"method": request.method, "path": request.url.path, "body": body})

            if request.method == "POST" and request.url.path == "/sandboxes":
                return httpx.Response(
                    201,
                    json={"sandboxID": "sb-1", "templateID": "tpl-1", "state": "Running", "metadata": {"k": "v"}},
                )
            if request.method == "GET" and request.url.path == "/sandboxes/sb-1":
                return httpx.Response(
                    200,
                    json={"sandboxID": "sb-1", "templateID": "tpl-1", "state": "Stopped", "rawKey": 42},
                )
            if request.method == "POST" and request.url.path == "/sandboxes/sb-1/fork":
                return httpx.Response(
                    201,
                    json={"sandboxID": "sb-1-fork", "templateID": "tpl-1", "state": "Running"},
                )
            if request.method == "POST" and request.url.path == "/sandboxes/sb-1/commands/run":
                return httpx.Response(200, json={"id": "c1", "exit_code": 0, "output": "ok", "running": False})
            if request.url.path == "/sandboxes/sb-1/files":
                if request.url.params.get("path") == "/big":
                    s = _TrackingSyncStream([b"abcdef", b"ghijkl"])
                    streams.append(s)
                    return httpx.Response(200, stream=s)
                if request.url.params.get("path") == "/missing":
                    s = _TrackingSyncStream([b"err"])
                    streams.append(s)
                    return httpx.Response(404, stream=s)
                s = _TrackingSyncStream([b"sync ", b"text"])
                streams.append(s)
                return httpx.Response(200, stream=s)
            if request.url.path == "/sandboxes/sb-1/desktop/start":
                return httpx.Response(
                    200,
                    json={"status": "running", "desktopUrl": "/panel/d/", "terminalUrl": "/panel/t/"},
                )
            if request.url.path == "/sandboxes/sb-1/desktop/status":
                return httpx.Response(200, json={"status": "running"})
            if request.url.path == "/sandboxes/sb-1/desktop/stop":
                return httpx.Response(204)
            return httpx.Response(404)

        with CubePod(base_url="gw.local:8080", transport=httpx.MockTransport(handler)) as client:
            self.assertEqual(str(client._client.base_url), "http://gw.local:8080")
            self.assertFalse(client._client._trust_env)

            sb = client.create("tpl-1", warm_pool_ref="wp-1", resource={"cpu": "2"}, timeout_seconds=120)
            self.assertEqual(sb.metadata, {"k": "v"})
            self.assertEqual(recorded[-1]["body"]["timeoutSeconds"], 120)
            self.assertEqual(recorded[-1]["body"]["extension"], {"warmPoolRef": "wp-1", "resource": {"cpu": "2"}})

            raw = client.get_raw("sb-1")
            self.assertEqual(raw["rawKey"], 42)

            client.fork("sb-1", volume_key="vk-2", metadata={"m": "1"})
            self.assertEqual(recorded[-1]["body"], {"metadata": {"m": "1"}, "extension": {"volumeKey": "vk-2"}})

            client.execute("sb-1", "id", uid=None, gid=None, timeout=9.0)
            self.assertNotIn("uid", recorded[-1]["body"])
            self.assertNotIn("gid", recorded[-1]["body"])

            prebuilt = _build_tar_gz_bytes({"x.txt": "hi"})
            client.archive_sync("sb-1", prebuilt, remove_paths=["old.txt"])
            self.assertEqual(recorded[-1]["content"], prebuilt)
            self.assertEqual(recorded[-1]["remove_paths"], "old.txt")

            with self.assertRaises(httpx.HTTPStatusError):
                client.stream_file("sb-1", "/missing")
            self.assertTrue(streams[-1].closed)

            self.assertEqual(client.read_text_file("sb-1", "/ok", max_bytes=20), "sync text")
            self.assertTrue(streams[-1].closed)

            with self.assertRaises(CubePodFileTooLargeError):
                client.read_text_file("sb-1", "/big", max_bytes=5)
            self.assertTrue(streams[-1].closed)

            sess = client.start_desktop("sb-1")
            self.assertEqual(sess.desktop_url, "/panel/d/")
            self.assertEqual(client.get_desktop_status("sb-1")["status"], "running")
            self.assertEqual(client.stop_desktop("sb-1")["status"], "stopped")


class TestProviderPrerequisites(unittest.IsolatedAsyncioTestCase):
    async def test_async_network_policy_delete_volumes_and_empty_archive_sync(self) -> None:
        recorded: list[dict[str, Any]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            recorded.append({
                "method": request.method,
                "path": request.url.path,
                "params": dict(request.url.params),
                "content": request.content,
            })
            if request.method == "POST" and request.url.path == "/sandboxes":
                return httpx.Response(
                    201,
                    json={
                        "sandboxID": "cpod-np-1",
                        "templateID": "base",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if request.method == "POST" and request.url.path == "/sandboxes/cpod-np-1/files/archive-sync":
                return httpx.Response(200, json={"status": "ok", "extracted": 0})
            if request.method == "DELETE" and request.url.path == "/sandboxes/cpod-np-1":
                return httpx.Response(204)
            return httpx.Response(404)

        policy = {
            "defaultAction": "Deny",
            "egress": [
                {"action": "allow", "target": "api.github.com"},
                {"action": "deny", "target": "*"},
            ],
        }
        async with AsyncCubePod("http://127.0.0.1:8000", transport=httpx.MockTransport(handler)) as client:
            sb = await client.create(
                "base",
                volume_key="vol-prereq",
                network_policy=policy,
                secret_bindings=[{"envName": "GH_TOKEN", "placeholder": "cpodref_1"}],
            )
            self.assertEqual(sb.sandbox_id, "cpod-np-1")

            create_body = json.loads(recorded[-1]["content"])
            self.assertNotIn("networkPolicy", create_body)
            self.assertEqual(create_body["extension"]["volumeKey"], "vol-prereq")
            self.assertEqual(create_body["extension"]["networkPolicy"]["defaultAction"], "Deny")
            self.assertEqual(
                create_body["extension"]["networkPolicy"]["egress"],
                [
                    {"action": "allow", "target": "api.github.com"},
                    {"action": "deny", "target": "*"},
                ],
            )
            self.assertEqual(
                create_body["extension"]["networkPolicy"]["secretBindings"],
                [{"envName": "GH_TOKEN", "placeholder": "cpodref_1"}],
            )

            sync_res = await client.archive_sync(
                "cpod-np-1",
                b"",
                dest="/workspace/proj",
                remove_paths=["old.py", "sub/dir"],
            )
            self.assertEqual(sync_res, {"status": "ok", "extracted": 0})
            self.assertEqual(recorded[-1]["content"], b"")
            self.assertEqual(
                recorded[-1]["params"],
                {"dest": "/workspace/proj", "remove_paths": "old.py,sub/dir"},
            )

            await client.delete("cpod-np-1")
            self.assertEqual(recorded[-1]["params"], {})

            await client.delete("cpod-np-1", delete_volumes=True)
            self.assertEqual(recorded[-1]["params"], {"deleteVolumes": "true"})

    def test_sync_network_policy_and_delete_volumes(self) -> None:
        recorded: list[dict[str, Any]] = []

        def handler(request: httpx.Request) -> httpx.Response:
            recorded.append({
                "method": request.method,
                "path": request.url.path,
                "params": dict(request.url.params),
                "content": request.content,
            })
            if request.method == "POST" and request.url.path == "/sandboxes":
                return httpx.Response(
                    201,
                    json={
                        "sandboxID": "cpod-np-sync",
                        "templateID": "base",
                        "state": "Running",
                        "envdPort": 49983,
                        "rootfsGeneration": 1,
                    },
                )
            if request.method == "DELETE" and request.url.path == "/sandboxes/cpod-np-sync":
                return httpx.Response(204)
            return httpx.Response(404)

        with SyncCubePod("http://127.0.0.1:8000", transport=httpx.MockTransport(handler)) as client:
            client.create(
                "base",
                network_policy={
                    "defaultAction": "Deny",
                    "egress": [{"action": "allow", "target": "*.pypi.org"}],
                },
            )
            create_body = json.loads(recorded[-1]["content"])
            self.assertNotIn("networkPolicy", create_body)
            self.assertEqual(
                create_body["extension"]["networkPolicy"],
                {
                    "defaultAction": "Deny",
                    "egress": [{"action": "allow", "target": "*.pypi.org"}],
                },
            )

            client.delete("cpod-np-sync")
            self.assertEqual(recorded[-1]["params"], {})

            client.delete("cpod-np-sync", delete_volumes=True)
            self.assertEqual(recorded[-1]["params"], {"deleteVolumes": "true"})


if __name__ == "__main__":
    unittest.main()

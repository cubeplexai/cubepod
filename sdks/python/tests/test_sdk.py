import io
import json
from pathlib import Path
import sys
import tarfile
import unittest
from typing import Any

# Ensure sdks/python is on sys.path
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import httpx

from cubepod import (
    AsyncCubePod,
    CubePod,
    SyncCubePod,
    CommandInfo,
    CommandLogs,
    ExecutionResult,
    FileEntry,
    PanelEndpoint,
    SandboxInfo,
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
                self.assertIn("networkPolicy", data)
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

        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "PUT" and "/sandboxes/cpod-1/files" in url_str:
                path = request.url.params.get("path")
                uid = request.url.params.get("uid")
                gid = request.url.params.get("gid")
                mode = request.url.params.get("mode")
                self.assertEqual(uid, "1000")
                self.assertEqual(gid, "1000")
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

            content = await client.read_file("cpod-1", "/workspace/test.txt")
            self.assertEqual(content, b"hello from cubepod sdk")

    async def test_archive_sync(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
            if request.method == "POST" and "/sandboxes/cpod-1/files/archive-sync" in url_str:
                dest = request.url.params.get("dest")
                self.assertEqual(dest, "/workspace")
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

        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
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

            if method == "GET" and request.url.path == "/templates/aliases/py-async":
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
                })

            if method == "POST" and "/builds/" in request.url.path:
                return httpx.Response(200, json={})

            if method == "DELETE" and request.url.path.startswith("/templates/"):
                if "conflict" in request.url.path:
                    return httpx.Response(409, json={"code": 409, "message": "template in use"})
                return httpx.Response(200, json={})

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

            # 4. get_template_by_alias
            alias_tpl = await client.get_template_by_alias("py-async")
            self.assertEqual(alias_tpl["templateID"], "tpl_test_async")

            # 5. create_template_from_image
            created = await client.create_template_from_image(
                from_image="python:3.13",
                alias="new-async",
                cpu_count=2,
                memory_mb=2048,
                min_free_disk_mb=10240,
            )
            self.assertEqual(created["templateID"], "tpl_created_async")
            self.assertEqual(created["buildID"], "bld_created_async")
            self.assertIn("new-async", created["aliases"])

            # Check v3 request
            v3_call = recorded[-2]
            self.assertEqual(v3_call["method"], "POST")
            self.assertTrue(v3_call["url"].endswith("/v3/templates"))
            self.assertEqual(v3_call["body"]["alias"], "new-async")
            self.assertEqual(v3_call["body"]["cpuCount"], 2)
            self.assertEqual(v3_call["body"]["memoryMB"], 2048)
            self.assertEqual(v3_call["body"]["minFreeDiskMb"], 10240)
            self.assertNotIn("name", v3_call["body"])

            # Check v2 build request
            v2_call = recorded[-1]
            self.assertEqual(v2_call["method"], "POST")
            self.assertTrue("/v2/templates/tpl_created_async/builds/bld_created_async" in v2_call["url"])
            self.assertEqual(v2_call["body"]["fromImage"], "python:3.13")
            self.assertEqual(v2_call["body"]["steps"], [])
            self.assertNotIn("dockerfile", v2_call["raw"].lower())

            # 6. delete_template (force=False and force=True)
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

            # 7. set_template_public
            await client.set_template_public("tpl_created_async")
            self.assertEqual(recorded[-1]["method"], "PATCH")
            self.assertEqual(recorded[-1]["body"], {"public": True})

            # 8. build_template
            bld_res = await client.build_template(
                name="async-builder",
                alias="async-bld",
                from_image="ubuntu:24.04",
                steps=[{"type": "RUN", "args": ["echo hi"]}],
                start_cmd="echo start",
                ready_cmd="true",
                force=True,
            )
            self.assertEqual(bld_res["templateID"], "tpl_created_async")
            self.assertEqual(recorded[-1]["body"]["steps"], [{"type": "RUN", "args": ["echo hi"]}])
            self.assertEqual(recorded[-1]["body"]["startCmd"], "echo start")
            self.assertEqual(recorded[-1]["body"]["readyCmd"], "true")
            self.assertTrue(recorded[-1]["body"]["force"])


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

        def handler(request: httpx.Request) -> httpx.Response:
            url_str = str(request.url)
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

            if method == "GET" and request.url.path == "/templates/aliases/py-sync":
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
                })

            if method == "POST" and "/builds/" in request.url.path:
                return httpx.Response(200, json={})

            if method == "DELETE" and request.url.path.startswith("/templates/"):
                if "conflict" in request.url.path:
                    return httpx.Response(409, json={"code": 409, "message": "template in use"})
                return httpx.Response(200, json={})

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

            # 4. get_template_by_alias
            alias_tpl = client.get_template_by_alias("py-sync")
            self.assertEqual(alias_tpl["templateID"], "tpl_test_sync")

            # 5. create_template_from_image
            created = client.create_template_from_image(
                from_image="python:3.13",
                alias="new-sync",
                cpu_count=4,
                memory_mb=4096,
                min_free_disk_mb=20480,
            )
            self.assertEqual(created["templateID"], "tpl_created_sync")
            self.assertEqual(created["buildID"], "bld_created_sync")
            self.assertIn("new-sync", created["aliases"])

            # Check v3 request
            v3_call = recorded[-2]
            self.assertEqual(v3_call["method"], "POST")
            self.assertTrue(v3_call["url"].endswith("/v3/templates"))
            self.assertEqual(v3_call["body"]["alias"], "new-sync")
            self.assertEqual(v3_call["body"]["cpuCount"], 4)
            self.assertEqual(v3_call["body"]["memoryMB"], 4096)
            self.assertEqual(v3_call["body"]["minFreeDiskMb"], 20480)
            self.assertNotIn("name", v3_call["body"])

            # Check v2 build request
            v2_call = recorded[-1]
            self.assertEqual(v2_call["method"], "POST")
            self.assertTrue("/v2/templates/tpl_created_sync/builds/bld_created_sync" in v2_call["url"])
            self.assertEqual(v2_call["body"]["fromImage"], "python:3.13")
            self.assertEqual(v2_call["body"]["steps"], [])
            self.assertNotIn("dockerfile", v2_call["raw"].lower())

            # 6. delete_template (force=False and force=True)
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

            # 7. set_template_public
            client.set_template_public("tpl_created_sync")
            self.assertEqual(recorded[-1]["method"], "PATCH")
            self.assertEqual(recorded[-1]["body"], {"public": True})

            # 8. build_template
            bld_res = client.build_template(
                name="sync-builder",
                alias="sync-bld",
                from_image="ubuntu:24.04",
                steps=[{"type": "RUN", "args": ["echo sync"]}],
                start_cmd="echo start",
                ready_cmd="true",
                force=True,
            )
            self.assertEqual(bld_res["templateID"], "tpl_created_sync")
            self.assertEqual(recorded[-1]["body"]["steps"], [{"type": "RUN", "args": ["echo sync"]}])
            self.assertEqual(recorded[-1]["body"]["startCmd"], "echo start")
            self.assertEqual(recorded[-1]["body"]["readyCmd"], "true")
            self.assertTrue(recorded[-1]["body"]["force"])


if __name__ == "__main__":
    unittest.main()

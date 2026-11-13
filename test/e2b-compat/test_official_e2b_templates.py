"""Official E2B Python SDK Template Conformance Tests (CEP-0019).

Verifies end-to-end interoperability between the official `e2b` Python SDK and
CubePod Gateway (`--e2b-template-api=true --template-build=true`):
- Alpha: `Template().from_image("ubuntu:24.04")` + `Template.build(...)` + catalog listing + `Sandbox.create(alias)`
- Same-name rebuild reusing `template_id` while advancing `build_id`
- Beta: `Template().from_image("busybox:latest").set_envs(...).copy(...).run_cmd(...)` via `LocalDockerBuilder`
- Negative cases: `/workspace` `COPY` and `WORKDIR` rejected (`400`), deleting referenced template rejected (`409`), unreferenced delete returns `204`.
"""

from __future__ import annotations

import importlib.metadata
import json
import os
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
import uuid
from typing import Any, Dict, Tuple

import pytest

_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

PINNED_E2B_SDK_VERSION = importlib.metadata.version("e2b")


def _http_json(
    method: str, url: str, body: Dict[str, Any] | None = None
) -> Tuple[int, Any, Dict[str, str]]:
    data = json.dumps(body).encode("utf-8") if body is not None else None
    headers = {"Accept": "application/json"}
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read().decode("utf-8")
            parsed = json.loads(raw) if raw else None
            return resp.status, parsed, dict(resp.headers.items())
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8")
        try:
            parsed = json.loads(raw) if raw else None
        except Exception:
            parsed = raw
        return exc.code, parsed, dict(exc.headers.items())


def _delete_template(gateway_url: str, template_id: str, force: bool = False) -> int:
    suffix = "?force=true" if force else ""
    status, _, _ = _http_json("DELETE", f"{gateway_url}/templates/{template_id}{suffix}")
    return status


def test_official_e2b_sdk_version_recorded() -> None:
    """Verify the installed official e2b SDK version is recorded for conformance."""
    assert PINNED_E2B_SDK_VERSION, "e2b package version must be non-empty"
    major = int(PINNED_E2B_SDK_VERSION.split(".")[0])
    assert major >= 2, f"expected e2b>=2.0.0, got {PINNED_E2B_SDK_VERSION}"


def test_alpha_image_only_template_build_and_sandbox_create(gateway_url: str) -> None:
    """Alpha: register image-only template via e2b.Template.build, verify catalog & Sandbox.create."""
    from e2b import Sandbox, Template

    alias = f"cep19-alpha-{uuid.uuid4().hex[:8]}"
    tpl = Template().from_image("ubuntu:24.04")
    info = Template.build(
        tpl,
        name=alias,
        cpu_count=4,
        memory_mb=4096,
        api_url=gateway_url,
        api_key="test-key",
    )
    try:
        assert info.template_id.startswith("tpl_")
        assert info.build_id.startswith("bld_")
        assert Template.exists(alias, api_url=gateway_url, api_key="test-key")

        # Verify GET /templates and GET /v2/templates arrays contain our template
        for path in ("/templates", "/v2/templates"):
            status, items, _ = _http_json("GET", f"{gateway_url}{path}")
            assert status == 200
            assert isinstance(items, list), f"expected JSON array from {path}, got {type(items)}"
            matched = [it for it in items if it.get("templateID") == info.template_id]
            assert len(matched) == 1, f"template {info.template_id} missing from {path}"
            item = matched[0]
            assert item["buildStatus"] == "ready"
            assert item["cpuCount"] == 4
            assert item["memoryMB"] == 4096
            assert item["envdVersion"] == "cubepod-podd"
            assert alias in item["names"]

        # Create sandbox from alias and verify resources + command execution
        sbx = Sandbox.create(
            alias,
            timeout=120,
            api_url=gateway_url,
            sandbox_url=gateway_url,
            api_key="test-key",
        )
        try:
            st, sbx_info, _ = _http_json("GET", f"{gateway_url}/sandboxes/{sbx.sandbox_id}")
            assert st == 200
            assert sbx_info["cpuCount"] == 4
            assert sbx_info["memoryMB"] == 4096

            res = sbx.commands.run("echo alpha-template-ok")
            assert res.exit_code == 0
            assert "alpha-template-ok" in res.stdout
        finally:
            sbx.kill()
    finally:
        _delete_template(gateway_url, info.template_id, force=True)


def test_alpha_same_name_rebuild_reuses_template_id(gateway_url: str) -> None:
    """Rebuilding with the same name reuses template_id and advances build_id."""
    from e2b import Template

    alias = f"cep19-rebuild-{uuid.uuid4().hex[:8]}"
    info1 = Template.build(
        Template().from_image("ubuntu:24.04"),
        name=alias,
        cpu_count=2,
        memory_mb=2048,
        api_url=gateway_url,
        api_key="test-key",
    )
    try:
        info2 = Template.build(
            Template().from_image("debian:12"),
            name=alias,
            cpu_count=4,
            memory_mb=4096,
            api_url=gateway_url,
            api_key="test-key",
        )
        assert info2.template_id == info1.template_id
        assert info2.build_id != info1.build_id

        st, detail, _ = _http_json("GET", f"{gateway_url}/templates/{info2.template_id}")
        assert st == 200
        assert detail["buildID"] == info2.build_id
        assert detail["cpuCount"] == 4
        assert detail["memoryMB"] == 4096
        assert detail["buildCount"] >= 2
        build_ids = [b["buildID"] for b in detail.get("builds", [])]
        assert info1.build_id in build_ids
        assert info2.build_id in build_ids
    finally:
        _delete_template(gateway_url, info1.template_id, force=True)


def test_beta_recipe_copy_env_run_with_local_docker(gateway_url: str) -> None:
    """Beta: Template.build with set_envs + copy + run_cmd builds an OCI image via LocalDockerBuilder."""
    from e2b import Sandbox, Template

    # Ensure Docker daemon and busybox:latest are present
    probe = subprocess.run(
        ["docker", "image", "inspect", "busybox:latest"],
        capture_output=True,
        text=True,
    )
    if probe.returncode != 0:
        pytest.skip("busybox:latest not available locally in Docker daemon")

    alias = f"cep19-beta-{uuid.uuid4().hex[:8]}"
    built_tag = ""
    template_id = ""
    with tempfile.TemporaryDirectory(prefix="cep19-ctx-") as ctx_dir:
        src_file = os.path.join(ctx_dir, "copied.txt")
        with open(src_file, "w", encoding="utf-8") as f:
            f.write("cep19-copied-content\n")

        tpl = (
            Template(file_context_path=ctx_dir)
            .from_image("busybox:latest")
            .set_envs({"CEP19_ENV": "val with space"})
            .copy("copied.txt", "/etc/copied.txt")
            .run_cmd(
                'echo "$CEP19_ENV" > /etc/from_env.txt && cat /etc/copied.txt > /etc/out.txt'
            )
        )
        info = Template.build(
            tpl,
            name=alias,
            cpu_count=2,
            memory_mb=2048,
            api_url=gateway_url,
            api_key="test-key",
        )
        template_id = info.template_id
        built_tag = f"cubepod-templates/{alias}:{info.build_id}"

        try:
            # Verify Config.Env does NOT contain CEP19_ENV (build-time only)
            insp = subprocess.run(
                ["docker", "inspect", "--format", "{{json .Config.Env}}", built_tag],
                capture_output=True,
                text=True,
                check=True,
            )
            env_list = json.loads(insp.stdout.strip() or "[]")
            assert not any(
                e.startswith("CEP19_ENV=") for e in env_list
            ), f"build-time env leaked into Config.Env: {env_list}"

            # Verify /etc/out.txt and /etc/from_env.txt inside the built image
            out_txt = subprocess.run(
                ["docker", "run", "--rm", built_tag, "cat", "/etc/out.txt"],
                capture_output=True,
                text=True,
                check=True,
            )
            assert out_txt.stdout.strip() == "cep19-copied-content"

            env_txt = subprocess.run(
                ["docker", "run", "--rm", built_tag, "cat", "/etc/from_env.txt"],
                capture_output=True,
                text=True,
                check=True,
            )
            assert env_txt.stdout.strip() == "val with space"

            # Verify Sandbox.create works against the built template
            sbx = Sandbox.create(
                alias,
                timeout=120,
                api_url=gateway_url,
                sandbox_url=gateway_url,
                api_key="test-key",
            )
            try:
                assert sbx.sandbox_id
            finally:
                sbx.kill()
        finally:
            if built_tag:
                subprocess.run(["docker", "rmi", "-f", built_tag], capture_output=True)
            if template_id:
                _delete_template(gateway_url, template_id, force=True)


def test_negative_workspace_paths_and_referenced_delete(gateway_url: str) -> None:
    """Reject /workspace COPY/WORKDIR (400) and referenced template delete (409), allow unreferenced (204)."""
    from e2b import BuildException, Sandbox, Template

    # 1. COPY to /workspace rejected with 400
    with tempfile.TemporaryDirectory(prefix="cep19-neg-") as ctx_dir:
        with open(os.path.join(ctx_dir, "f.txt"), "w", encoding="utf-8") as f:
            f.write("data")

        alias_copy = f"cep19-neg-copy-{uuid.uuid4().hex[:8]}"
        tpl_copy = (
            Template(file_context_path=ctx_dir)
            .from_image("busybox:latest")
            .copy("f.txt", "/workspace/f.txt")
        )
        with pytest.raises(BuildException) as exc_copy:
            Template.build(
                tpl_copy,
                name=alias_copy,
                api_url=gateway_url,
                api_key="test-key",
            )
        assert "/workspace" in str(exc_copy.value)
        _delete_template(gateway_url, alias_copy, force=True)

    # 2. WORKDIR under /workspace rejected with 400
    alias_wd = f"cep19-neg-wd-{uuid.uuid4().hex[:8]}"
    tpl_wd = Template().from_image("busybox:latest").set_workdir("/workspace/sub")
    with pytest.raises(BuildException) as exc_wd:
        Template.build(
            tpl_wd,
            name=alias_wd,
            api_url=gateway_url,
            api_key="test-key",
        )
    assert "/workspace" in str(exc_wd.value)
    _delete_template(gateway_url, alias_wd, force=True)

    # 3. Referenced delete returns 409, unreferenced delete returns 204
    alias_ref = f"cep19-neg-ref-{uuid.uuid4().hex[:8]}"
    info = Template.build(
        Template().from_image("ubuntu:24.04"),
        name=alias_ref,
        api_url=gateway_url,
        api_key="test-key",
    )
    sbx = Sandbox.create(
        alias_ref,
        timeout=120,
        api_url=gateway_url,
        sandbox_url=gateway_url,
        api_key="test-key",
    )
    try:
        assert _delete_template(gateway_url, info.template_id) == 409
    finally:
        sbx.kill()

    assert _delete_template(gateway_url, info.template_id) == 204

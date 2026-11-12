"""Pytest fixtures for official E2B Python SDK Compatibility Test Suite.
Conforms to CEP-0018:
- Provides automated ephemeral LiveGateway or binds to live CUBEPOD_GATEWAY_URL
- Configures E2B_API_URL, E2B_SANDBOX_URL, E2B_API_KEY
- Provides sync and async sandbox lifecycle fixtures with automatic teardown
"""

from __future__ import annotations

import os
import sys
from typing import Any, AsyncGenerator, Generator
import pytest

# Ensure local test directory is on sys.path
_DIR = os.path.dirname(os.path.abspath(__file__))
if _DIR not in sys.path:
    sys.path.insert(0, _DIR)

from live_gateway import LiveGateway
from mock_server import MockE2BGateway


@pytest.fixture(scope="session")
def gateway_url() -> Generator[str, None, None]:
    """Provide Gateway base URL (live CUBEPOD_GATEWAY_URL or embedded LiveGateway)."""
    live_url = os.getenv("CUBEPOD_GATEWAY_URL")
    if live_url:
        os.environ["E2B_API_URL"] = live_url
        os.environ["E2B_SANDBOX_URL"] = live_url
        os.environ.setdefault("E2B_API_KEY", "cpod_test_api_key")
        yield live_url
        return

    gateway = LiveGateway()
    url = gateway.start()
    os.environ["E2B_API_URL"] = url
    os.environ["E2B_SANDBOX_URL"] = url
    os.environ.setdefault("E2B_API_KEY", "cpod_test_api_key")

    try:
        yield url
    finally:
        gateway.stop()


@pytest.fixture(scope="session")
def live_gateway(gateway_url: str) -> str:
    """Convenience alias for gateway_url fixture."""
    return gateway_url


@pytest.fixture
def sync_sandbox(gateway_url: str) -> Generator[Any, None, None]:
    """Provide an official e2b.Sandbox instance with automatic cleanup."""
    from e2b import Sandbox

    sbx = Sandbox.create("base", timeout=300, envs={"TEST": "1"})
    try:
        yield sbx
    finally:
        try:
            sbx.kill()
        except Exception:
            pass


@pytest.fixture
async def async_sandbox(gateway_url: str) -> AsyncGenerator[Any, None]:
    """Provide an official e2b.AsyncSandbox instance with automatic cleanup."""
    from e2b import AsyncSandbox

    sbx = await AsyncSandbox.create("base", timeout=300, envs={"TEST": "1"})
    try:
        yield sbx
    finally:
        try:
            await sbx.kill()
        except Exception:
            pass

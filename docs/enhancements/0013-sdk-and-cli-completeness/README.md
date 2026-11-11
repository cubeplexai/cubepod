# CEP-0013: Full-Featured Python SDK (`Sync` + `Async`), Go Client SDK (`pkg/client`), and `cubepod` CLI (`cmd/cubepod`)

- **CEP Number**: `0013`
- **Status**: `implementable`
- **Target Version**: `v0.3.0`
- **Components**: `sdks/python/cubepod/`, `pkg/client/`, `cmd/cubepod/`

---

## 1. Summary

Upgrades the CubePod client ecosystem to production completeness across both Python and Go:
1. **Python SDK (`sdks/python/cubepod`)**:
   - Adds `CubePod` (`SyncCubePod`) synchronous client alongside `AsyncCubePod`.
   - Adds `list_files(sandbox_id, path)`, `delete_file(sandbox_id, path)`, `get_command_status(sandbox_id, cmd_id)`, `stream_command(sandbox_id, cmd_id)` (SSE generator), `create_warmpool(...)`, `list_warmpools()`, and `list_templates()`.
   - Adds transient HTTP retry/backoff (`max_retries=2` on `502`/`503`/`504`) and high-level `AsyncSandboxSession` / `SandboxSession` context managers (`async with client.session(...) as sb:`).
2. **Go Client SDK (`pkg/client/client.go`)**:
   - Idiomatic, zero-external-dependency Go client covering all Sandbox, Command, File (`WriteFile`, `ReadFile`, `DeleteFile`, `ListFiles`, `ArchiveSync`, `ArchiveSyncDir`), Panel Token, and WarmPool APIs.
3. **`cubepod` CLI (`cmd/cubepod/main.go`)**:
   - First-class CLI tool (`bin/cubepod`) for developers and CI pipelines (`cubepod sandbox create|list|get|exec|write-file|read-file|ls|rm|archive-sync|pause|connect|reset-rootfs|fork|delete`, `cubepod warmpool create|list`, `cubepod health`).

---

## 2. Motivation

### Goals
- Provide both `async` and `sync` Python ergonomics so any Python agent framework (LangGraph, CrewAI, AutoGen, CubePlex) can integrate without event-loop friction.
- Provide a native Go SDK (`pkg/client`) and `cubepod` CLI binary (`bin/cubepod`) for Go services, Kubernetes E2E tests, and terminal workflows.

### Non-Goals
- TypeScript/Rust SDKs in `v0.3.0`.

---

## 3. Design Details

### 3.1 Python SDK (`sdks/python/cubepod/__init__.py`)
- `AsyncCubePod` additions:
  - `list_files(sandbox_id: str, path: str = "/workspace") -> list[FileEntry]`
  - `delete_file(sandbox_id: str, path: str) -> str`
  - `get_command_status(sandbox_id: str, command_id: str) -> CommandInfo`
  - `stream_command(sandbox_id: str, command_id: str) -> AsyncIterator[CommandLogs]`
  - `create_warmpool(name: str, template_id: str, replicas: int = 2) -> dict[str, Any]`
  - `list_warmpools() -> list[dict[str, Any]]`
- `CubePod` (`SyncCubePod`):
  - Uses `httpx.Client` with identical method signatures (synchronous) and `with CubePod(...) as client:`.

### 3.2 Go Client SDK (`pkg/client/client.go` & `pkg/client/client_test.go`)
- `type Client struct { baseURL string; apiKey string; httpClient *http.Client }`
- Methods: `Health`, `CreateSandbox`, `GetSandbox`, `ListSandboxes`, `DeleteSandbox`, `PauseSandbox`, `ConnectSandbox`, `ResetRootFS`, `ForkSandbox`, `SetTimeout`, `AcquireLease`, `RunCommand`, `GetCommandStatus`, `GetCommandLogs`, `KillCommand`, `WriteFile`, `ReadFile`, `DeleteFile`, `ListFiles`, `ArchiveSync`, `GetPanelEndpoint`, `CreateWarmPool`, `ListWarmPools`.

### 3.3 `cubepod` CLI (`cmd/cubepod/main.go`)
- Uses `--gateway-url` (or `CUBEPOD_GATEWAY_URL`, default `http://127.0.0.1:8080`) and outputs clean JSON or human-readable output.

---

## 4. Test Plan
- Unit tests in `sdks/python/tests/test_sdk.py` testing both `AsyncCubePod` and `CubePod` (`SyncCubePod`), including `list_files`, `delete_file`, `get_command_status`, `warmpools`, and retry handling.
- Unit tests in `pkg/client/client_test.go` testing all Go SDK methods against `httptest.Server`.

---

## 5. Graduation Criteria & PRR
- `bin/cubepod` built by `make build`.
- 100% test pass rate across Go and Python client suites.

---

## 6. Alternatives Considered
- **Code-generated OpenAPI clients**: Hand-crafted `httpx` and `net/http` clients have zero bloat, zero external codegen dependencies, and first-class streaming/`tar.gz` helpers.

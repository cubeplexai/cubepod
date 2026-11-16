---
title: E2B Compatibility & Protocol Differences
description: Compatibility matrix and behavioral differences between official E2B SDKs and CubePod Gateway, limited to interfaces reviewed in this page. This is not a complete audit of the upstream E2B API surface.
---

This reference summarizes compatibility and behavioral differences between official E2B SDKs and CubePod Gateway (`cubepod-gateway`) for the interfaces reviewed below. It is not a complete inventory of the upstream E2B API or SDK surface.

CubePod implements core E2B REST and Connect-RPC endpoints to allow teams using official E2B client SDKs to redirect traffic to a self-hosted CubePod instance. However, CubePod is **not** an identical replica of hosted E2B Cloud: it is built on a persistent dual-volume Kata Containers MicroVM architecture with distinct lifecycle semantics, a two-volume storage model, and extension APIs.

:::note[Verification and Test Status]
- **Integration Test Suite**: The repository contains automated Python compatibility tests under `test/e2b-compat/` (`test_official_e2b_sync.py`, `test_official_e2b_async.py`, `test_official_code_interpreter.py`, `test_official_e2b_templates.py`, `test_streaming_realtime.py`, `test_timeout_and_zombie_kill.py`, `test_multimodal_data_science.py`, `test_concurrent_sandboxes_isolation.py`).
- **Test Execution**: The existing tests in the repository demonstrate test coverage authored for CEP-0018 and CEP-0019; **they do not represent an end-to-end verification run performed as part of this documentation update**. Maintainers can execute the suite locally using `make test-e2b-compat` (`bash test/e2b-compat/run_tests.sh`).
- **CI Pipeline Scope**: In `.github/workflows/ci.yml`, `make test-e2b-compat` is **not** currently wired into the default automated GitHub Actions pipeline (which runs unit tests, SDK unit tests, and multi-process `make test-e2e`).
- **Official JS/TS SDK**: Zero automated tests exist in the repository for the official `@e2b/code-interpreter` or `e2b` npm packages. Official TypeScript/JavaScript E2B SDK compatibility is therefore marked **Untested**.
- **Agent Frameworks**: Higher-level orchestration frameworks (LangChain, LlamaIndex, AutoGen, CrewAI, OpenHands, Smolagents) lack dedicated integration test suites in this repository and are marked **Untested**.
:::

---

## Architectural Layers

To understand compatibility, you must distinguish between three distinct architectural layers. They should never be conflated:

```
+---------------------------------------------------------------------------------------+
| Layer 1: Official E2B SDK Drop-in                                                     |
| Unmodified upstream packages ('e2b', 'e2b-code-interpreter', '@e2b/code-interpreter') |
| Pointed via E2B_DOMAIN or E2B_API_URL + E2B_SANDBOX_URL + E2B_API_KEY                 |
+---------------------------------------------------------------------------------------+
                                           |
                                           v
+---------------------------------------------------------------------------------------+
| Layer 3: Protocol & Gateway Wire Translation                                          |
| 'cubepod-gateway' (:8080) REST routes & Connect-RPC handlers                          |
| Translates /sandboxes, /process.Process/*, /filesystem.Filesystem/*, /templates       |
+---------------------------------------------------------------------------------------+
                                           ^
                                           |
+---------------------------------------------------------------------------------------+
| Layer 2: CubePod Native SDKs (Functionally Similar Superset)                          |
| Native packages named 'cubepod' ('sdks/python/', 'sdks/typescript/', 'pkg/client')    |
| NOT drop-in for 'from e2b import Sandbox'; expose fork, reset-rootfs, panels, secrets  |
+---------------------------------------------------------------------------------------+
```

1. **Layer 1: Official E2B SDK Drop-in**:
   This refers strictly to using the official upstream Python packages (`e2b`, `e2b-code-interpreter`) or npm packages (`@e2b/code-interpreter`, `e2b`) without modification, pointing them to CubePod using environment variables. This is the **only** layer that qualifies as "official SDK compatibility".
2. **Layer 2: CubePod Native SDKs (Functional Overlap & Superset)**:
   The `cubepod` client libraries (`sdks/python/` and `sdks/typescript/`, alongside the Go client in `pkg/client` and CLI in `cmd/cubepod`) provide an idiomatic interface with namespaces (`.commands`, `.files`, `.code`) inspired by modern sandbox ergonomics. However, they are **native clients**, not drop-in aliases for `from e2b import Sandbox`. They target native `/sandboxes/{id}/*` endpoints and expose CubePod-exclusive enterprise features (`fork`, `reset-rootfs`, `archive-sync`, `secretBindings`, desktop/panel proxies).
3. **Layer 3: Gateway Protocol Route Presence**:
   The registration of a route on `cubepod-gateway` (HTTP REST or Connect-RPC in `pkg/gateway/server.go` and `pkg/gateway/connectrpc.go`) confirms protocol translation exists in code. However, protocol presence alone does **not** guarantee that every upstream SDK method or version works seamlessly under all runtime conditions.

---

## Pointing Official E2B SDKs at CubePod

Official E2B SDKs can be directed to `cubepod-gateway` through standard environment variables:

### Production HTTPS with Wildcard DNS Ingress

When `cubepod-gateway` sits behind an ingress controller with wildcard DNS (`*.<E2B_DOMAIN>`):

```bash
export E2B_DOMAIN="cubepod.example.com"
export E2B_API_KEY="cpod_live_secret"
```

The upstream E2B SDK automatically sends control-plane requests to `api.<E2B_DOMAIN>` (or the root domain) and routes sandbox port traffic to `<port>-<sandboxID>.<E2B_DOMAIN>`. `cubepod-gateway` matches the `Host` header against `^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$` to route port traffic directly to the sandbox MicroVM.

### Direct HTTP / Local Gateway

When developing locally without TLS or wildcard DNS (such as against `http://127.0.0.1:8080`):

```bash
export E2B_API_URL="http://127.0.0.1:8080"
export E2B_SANDBOX_URL="http://127.0.0.1:8080"
export E2B_API_KEY="cpod_test_api_key"
```

Setting both `E2B_API_URL` and `E2B_SANDBOX_URL` forces the SDK to send both control-plane and data-plane calls directly to the specified gateway address. This is the configuration used by `test/e2b-compat/`.

---

## Compatibility Matrix

### Status Legend

- **Compatible**: Official Python SDK tests in `test/e2b-compat/` cover the listed basic scenarios for this method. That is not a claim that every parameter or edge case is implemented or tested, and it does not mean those tests were executed as part of writing this page.
- **Partial**: A handler or protocol path exists, but there are known omissions, semantic differences, or required feature flags.
- **Untested**: A code path or route may exist, or an official SDK helper may not need a dedicated CubePod route, but this repository has no automated coverage of that official SDK method.
- **Unsupported**: No corresponding CubePod handler, route, or equivalent protocol support was found, and the upstream surface is not merely an SDK wrapper around another covered API.

Version-constraint cells such as `e2b>=2.0.0` are dependency floors declared by `test/e2b-compat/run_tests.sh` (the template suite also asserts that the installed package major version is `>= 2`). They are not a complete set of versions that have been measured, and they are not a live-test result from this documentation change.

---

### 1. Sandbox Lifecycle

| Official E2B Interface | CubePod Gateway Route / Protocol | Status | Evidence / Implementation Pointer | Repo test declaration / version constraint | Behavioral Differences & Limitations |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `Sandbox.create(...)` [Python] | `POST /sandboxes`<br/>`POST /v2/sandboxes` | **Compatible** | `pkg/gateway/sandboxes.go:handleCreateSandbox`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`test/e2b-compat/test_concurrent_sandboxes_isolation.py` | `e2b>=2.0.0` (dependency floor only) | Populates mandatory E2B response fields (`sandboxID`, `templateID`, `clientID`, `envdVersion`, `state`, `startedAt`, `endAt`, `cpuCount`, `memoryMB`, `diskSizeMB`). Claims standby pod from `PodWarmPool` if available. Accepts optional CubePod extension fields. |
| `Sandbox.create(...)` [JS/TS] | `POST /sandboxes`<br/>`POST /v2/sandboxes` | **Untested** | `pkg/gateway/sandboxes.go:handleCreateSandbox` | None in repo | No automated tests exist for official `@e2b/code-interpreter` or `e2b` npm packages. |
| `Sandbox.get_info(...)` | `GET /sandboxes/{id}` | **Compatible** | `pkg/gateway/sandboxes.go:handleGetSandbox`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Returns sandbox status (`running` / `paused`), resource metadata, and lease expiration. |
| `Sandbox.list()` | `GET /v2/sandboxes`<br/>`GET /sandboxes` | **Compatible** | `pkg/gateway/sandboxes.go:handleListSandboxesV2`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Returns active sandboxes with lowercase state on `/v2/sandboxes`. |
| `sandbox.kill()` / `delete` | `DELETE /sandboxes/{id}` | **Compatible** | `pkg/gateway/sandboxes.go:handleDeleteSandbox`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py` | `e2b>=2.0.0` (dependency floor only) | Releases compute pod. Ephemeral sandboxes reclaim their temporary storage on delete; named persistent workspaces are retained unless deletion is requested. |
| `sandbox.set_timeout(...)` / refreshes | `POST /sandboxes/{id}/refreshes`<br/>`POST /sandboxes/{id}/timeout` | **Compatible** | `pkg/gateway/sandboxes.go:handleRefreshSandbox`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`pkg/gateway/e2b_conformance_test.go` | `e2b>=2.0.0` (dependency floor only) | Renews sandbox TTL and resets the idle reaper timer. |
| `Sandbox.connect(...)` | `POST /v2/sandboxes/{id}/connect`<br/>`POST /sandboxes/{id}/connect` | **Compatible** | `pkg/gateway/sandboxes.go:handleConnectSandbox`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Reconnects to running sandbox or transparently resumes a paused sandbox (without preserving in-memory processes). |

---

### 2. Commands & Process Execution

| Official E2B Interface | CubePod Gateway Route / Protocol | Status | Evidence / Implementation Pointer | Repo test declaration / version constraint | Behavioral Differences & Limitations |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.commands.run(...)` (sync) | `POST /sandboxes/{id}/commands`<br/>`POST /sandboxes/{id}/commands/run`<br/>Connect-RPC `/process.Process/Start` | **Compatible** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py` | `e2b>=2.0.0` (dependency floor only) | Executes inside guest via `podd` as unprivileged UID/GID `1000:1000` (`cubeplex`) with working directory `/workspace`. Returns `stdout`, `stderr`, and `exitCode`. Foreground execution only. |
| `sandbox.commands.run(..., background=True)` | `POST /sandboxes/{id}/commands` (`background: true`)<br/>Connect-RPC `/process.Process/Start` | **Compatible** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Spawns detached process group in guest and returns a handle with `pid`. |
| Output streaming & callbacks (`on_stdout`, `on_stderr`) | `GET /sandboxes/{id}/commands/{cmdId}/logs`<br/>`GET /sandboxes/{id}/commands/{cmdId}/stream`<br/>Connect-RPC `/process.Process/Connect` | **Compatible** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Streams incremental command output to `on_stdout` / `on_stderr` callbacks. |
| `sandbox.commands.kill(pid)` | `POST /sandboxes/{id}/commands/{cmdId}/kill`<br/>Connect-RPC `/process.Process/SendSignal` | **Compatible** | `pkg/gateway/commands.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Terminates the background process group and returns `True` when stopped. |
| `sandbox.pty` / Interactive Terminal | Connect-RPC `/process.Process/{Start,Connect,SendInput,StreamInput,Update,SendSignal}` (`pty: true`) | **Partial** | `pkg/gateway/connectrpc.go`<br/>`crates/podd/src/commands.rs`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | **User impact:** Interactive PTY spawn, stdin, resize, signals, and 64 KiB reconnect replay work, but closing stdin half-duplex (`CloseStdin`) is not supported and returns `501 Not Implemented`. |

---

### 3. Filesystem Operations

| Official E2B Interface | CubePod Gateway Route / Protocol | Status | Evidence / Implementation Pointer | Repo test declaration / version constraint | Behavioral Differences & Limitations |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.files.write(path, data)` | `PUT /sandboxes/{id}/files`<br/>`PUT /files`<br/>Connect-RPC `/filesystem.Filesystem/Write` | **Compatible** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`test/e2b-compat/test_concurrent_sandboxes_isolation.py` | `e2b>=2.0.0` (dependency floor only) | Writes a file and can create parent directories. Gateway/podd path checks in `pkg/gateway/files.go` are intended to reject `..` segments; `test/e2b-compat` covers writes under `/workspace` (text and binary) and does not enumerate traversal cases. Uploaded files default to `1000:1000` ownership. |
| `sandbox.files.read(path)` | `GET /sandboxes/{id}/files`<br/>`GET /files`<br/>Connect-RPC `/filesystem.Filesystem/Read` | **Compatible** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_sync.py`<br/>`test/e2b-compat/test_official_e2b_async.py`<br/>`test/e2b-compat/test_concurrent_sandboxes_isolation.py` | `e2b>=2.0.0` (dependency floor only) | Reads raw file bytes (text and binary `format="bytes"`) from guest `/workspace` or RootFS. |
| `sandbox.files.list(path)` | `GET /sandboxes/{id}/files/list`<br/>`GET /files/list`<br/>Connect-RPC `/filesystem.Filesystem/{List,ListDir}` | **Compatible** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Returns directory entries with metadata (`name`, `size`, `isDir`, `permissions`). |
| `sandbox.files.remove(path)` | `DELETE /sandboxes/{id}/files`<br/>`DELETE /files`<br/>Connect-RPC `/filesystem.Filesystem/Remove` | **Compatible** | `pkg/gateway/files.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Recursively removes file or directory inside guest. |
| `sandbox.files.make_dir(path)` | Connect-RPC `/filesystem.Filesystem/MakeDir` | **Compatible** | `pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Creates directory recursively (`0755`, owned by `1000:1000`). |
| `sandbox.files.watch_dir(path)` | Connect-RPC `/filesystem.Filesystem/{WatchDir,CreateWatcher,GetWatcherEvents,RemoveWatcher}` | **Compatible** | `pkg/gateway/watch.go`<br/>`pkg/gateway/connectrpc.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Streams and polls live filesystem create/write/remove events. |

---

### 4. Code Interpreter (`e2b-code-interpreter`)

| Official E2B Interface | CubePod Gateway Route / Protocol | Status | Evidence / Implementation Pointer | Repo test declaration / version constraint | Behavioral Differences & Limitations |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.run_code(code)` [Python REPL] | `POST /sandboxes/{id}/code/execute`<br/>`POST /sandboxes/{id}/execute`<br/>`POST /execute` | **Compatible** | `pkg/gateway/server.go:handleCodeExecute`<br/>`crates/podd/src/interpreter.rs`<br/>`test/e2b-compat/test_official_code_interpreter.py`<br/>`test/e2b-compat/test_multimodal_data_science.py`<br/>`test/e2b-compat/test_streaming_realtime.py`<br/>`test/e2b-compat/test_timeout_and_zombie_kill.py` | `e2b-code-interpreter>=2.0.0` (dependency floor only) | Executes Python snippets inside `podd` with persistent memory across runs. Streams NDJSON events with `stdout`, `stderr`, rich MIME `result` (`text`, `png` base64 charts, `html`, `json`), and runtime error tracebacks. Real-time `on_stdout` callbacks are verified in `test_streaming_realtime.py`, multimodal outputs in `test_multimodal_data_science.py`, and timeout kill in `test_timeout_and_zombie_kill.py`. |
| Multi-Language Kernels (`bash`, `js`, `ts`, `r`, `java`) | `POST /sandboxes/{id}/code/execute` | **Partial** | `crates/podd/src/interpreter.rs`<br/>`crates/podd/src/tests.rs` | Unit tests in `crates/podd` | In-guest `podd` daemon natively implements 6 stateful REPL kernels. The official Python `e2b-code-interpreter` SDK primarily tests Python. |
| Contexts Management (`/contexts`) | In-guest `podd` (`:49983`) | **Partial** | `crates/podd/src/main.rs:147`<br/>`crates/podd/src/tests.rs` | Unit tests in `crates/podd` | In-guest `podd` handles `POST /contexts`, `GET /contexts`, `DELETE /contexts/{id}`, and `POST /contexts/{id}/restart`. **Protocol difference**: Gateway does not register `/sandboxes/{id}/code/contexts` as a public REST route. Instead, clients pass `context_id` in the JSON body to `POST /sandboxes/{id}/code/execute` or call `podd` directly. |

:::note[Code Interpreter Streaming & Timeout Attribution]
Real-time stdout chunk delivery (`on_stdout`) is specifically exercised by `test/e2b-compat/test_streaming_realtime.py` via `e2b_code_interpreter.Sandbox.run_code(..., on_stdout=...)`. Similarly, execution timeout enforcement and zombie process cleanup are exercised by `test/e2b-compat/test_timeout_and_zombie_kill.py` via `run_code(..., timeout=2)`. These tests cover code interpreter REPL execution, not lower-level `sandbox.commands` background processes.
:::

---

### 5. Template Catalog & Custom Builds (`E2BTemplateAPI`)

| Official E2B Interface | CubePod Gateway Route / Protocol | Status | Evidence / Implementation Pointer | Repo test declaration / version constraint | Behavioral Differences & Limitations |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `Template.list()` | `GET /templates`<br/>`GET /v2/templates` | **Partial** | `pkg/gateway/templates.go`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | **User impact:** Requires the template API feature to be enabled on the gateway; otherwise `GET /templates` returns a legacy non-E2B format. |
| `Template.build(...)` | `POST /templates`<br/>`POST /v2/templates`<br/>`POST /v3/templates`<br/>`POST /v2/templates/{id}/builds/{buildID}` | **Partial** | `pkg/gateway/templates.go`<br/>`pkg/controller/builder.go`<br/>`test/e2b-compat/test_official_e2b_templates.py` | `e2b>=2.0.0` | **User impact:** Requires template build features to be enabled (otherwise returns `501`). Builds do not capture VM memory snapshots (`startCmd` runs fresh on every boot), and `COPY`/`WORKDIR` instructions targeting `/workspace` are rejected with HTTP `400`. |
| Build status & logs | `GET /templates/{id}/builds/{buildID}/status`<br/>`GET /templates/{id}/builds/{buildID}/logs` | **Compatible** | `pkg/gateway/templates.go`<br/>`test/e2b-compat/test_official_e2b_templates.py` | `e2b>=2.0.0` | Returns build status (`building`, `ready`, `error`) and build logs. |
| `Template.exists(alias)` / Tags & Aliases | `GET /templates/aliases/{alias}`<br/>`POST /templates/tags`<br/>`DELETE /templates/tags` | **Partial** | `pkg/gateway/templates.go`<br/>`test/e2b-compat/test_official_e2b_templates.py` | `e2b>=2.0.0` | **User impact:** `Template.exists(alias)` works when the template API is enabled; tag assignment/removal endpoints are not covered by official SDK tests. |

---

### 6. Port Ingress & Host Routing

| Official E2B Interface | CubePod Gateway Route / Protocol | Status | Evidence / Implementation Pointer | Repo test declaration / version constraint | Behavioral Differences & Limitations |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `sandbox.get_host(port)` | `GET /sandboxes/{id}/endpoints/{port}` | **Compatible** | `pkg/gateway/server.go:handleGetEndpoint`<br/>`test/e2b-compat/test_official_e2b_extended.py` | `e2b>=2.0.0` | Returns `<port>-<sandboxID>.<domain>` for accessing exposed sandbox ports. |
| Subdomain Ingress Routing | `Host: <port>-<sandboxID>.<domain>` | **Compatible** | `pkg/gateway/server.go:hostRouter`<br/>`pkg/gateway/server_test.go` | Go unit/integration tests | Routes `<port>-<sandboxID>.<domain>` traffic to the sandbox and automatically resumes paused sandboxes when traffic arrives. |

---

### 7. Untested or Partial Ecosystem Surfaces

| Upstream E2B Surface | Status | User-Facing Limitations & Guidance |
| :--- | :--- | :--- |
| **Official JS/TS SDK** (`@e2b/code-interpreter`, `e2b`) | **Untested** | No automated test suite in this repository exercises the official JS/TS npm packages. |
| **Agent Framework Integrations** (LangChain, LlamaIndex, AutoGen, CrewAI, OpenHands, Smolagents) | **Untested** | No end-to-end integration tests run these third-party frameworks directly. |
| **Official E2B Volumes API** (`POST /volumes`, `/volumecontent/{id}/*`, `volumeMounts`) | **Partial** | **User impact:** Volume CRUD and content upload/download work with `e2b>=2.0.0`, but `vol.write_file()` / `vol.read_file()` require passing `api_url` explicitly against self-hosted gateways (otherwise the SDK targets `e2b.app`). On Kubernetes, mounting volumes into sandboxes returns `501 Not Implemented` unless cluster-wide RWX storage is configured. |
| **Official E2B Secrets API** (`POST /secrets`, `GET /secrets`, `POST /secrets/{id}`, `DELETE /secrets/{id}`, `network.rules`) | **Partial** | **User impact:** Write-only secret CRUD (`Secret.create`/`list`/`get_info`/`update`/`destroy`/`fill`) and `network.rules` header injection work on single-gateway deployments. Multi-replica Kubernetes HA secret sync is not yet available, and local Docker sandboxes do not run the egress sidecar so outbound header injection does not apply in local Docker mode. |
| **Official E2B `sandbox.git` helper** | **Untested** | Upstream docs describe `sandbox.git` as a deprecated SDK helper bound to the command runner (`Git` is constructed with a `Commands` object), not a dedicated HTTP git API. No `test/e2b-compat` case calls `sandbox.git`. Running `git` through `sandbox.commands.run()` is a shell-command path and does not demonstrate official helper compatibility (clone options, credential helpers, `dangerously_authenticate`, structured status/branch APIs, and similar). |

---

## Key Behavioral & Architectural Differences

### 1. Pause & Resume vs. Memory Snapshots

A fundamental architectural difference between hosted E2B and CubePod is how sandboxes are suspended:

- **Upstream E2B Cloud**: Official E2B documentation states that pausing saves both the filesystem state and **guest memory (RAM)**, preserving running background processes and in-memory variables across resumes.
- **CubePod**: Implements cloud-native Kubernetes compute scale-to-zero. When paused (`POST /sandboxes/{id}/pause`), the compute Pod is terminated, but **both persistent CSI raw block volumes are retained**:
  - `rootfs-pvc`: Attaches the OverlayFS `upperdir` used for RootFS-level package and OS changes.
  - `workspace-pvc`: Stores the persistent `/workspace` filesystem.
- **Consequence upon Resume (`connect` or inbound traffic)**:
  - CubePod starts a new compute Pod and reattaches the retained `rootfs` and `workspace` block volumes. Resume latency is deployment- and storage-dependent and is not a measured SLA on this page.
  - Filesystem changes that were persisted on those volumes are still present after reattach; this page does not claim bit-identical preservation of every in-guest state.
  - Guest RAM and processes that existed only in the previous compute Pod are not restored. Daemons needed after resume must be defined in the template `startCmd` or started again by the agent.

### 2. Template Builds & `/workspace` Rejection

When using `Template.build()` under `--e2b-template-api=true`:

- CubePod builds a standard OCI container image using rootless BuildKit (in Kubernetes) or the local Docker daemon.
- It does **not** produce a pre-paused VM memory snapshot; `startCmd` executes freshly every time a new sandbox is spawned from the template.
- **Crucial Invariant**: Any Dockerfile `COPY` or `WORKDIR` directive that targets `/workspace` is **explicitly rejected with HTTP `400 Bad Request`**. Because `/workspace` is mounted from an independent persistent block PVC (`workspace-pvc`) at runtime, files baked into the image under `/workspace` would be permanently hidden beneath the volume mount. Place template files in `/opt/`, `/usr/local/`, or custom system paths.

### 3. Code Context Management (`/contexts`)

In hosted E2B, code interpreter contexts may be managed via dedicated context endpoints.

- In CubePod, stateful execution contexts for 6 languages (`python`, `bash`, `javascript`, `typescript`, `r`, `java`) are managed directly inside the in-guest Rust daemon `podd` (`:49983`).
- `cubepod-gateway` does **not** register `/sandboxes/{id}/code/contexts` as a separate public route.
- To execute code within a specific stateful context, pass `context_id` inside the request body of `POST /sandboxes/{id}/code/execute` (or `POST /sandboxes/{id}/execute`). The gateway forwards the context identifier directly to `podd`.

### 4. Error Response Envelopes & Exception Mapping

Error serialization across `cubepod-gateway` differs by subsystem:

- **Template API Handlers** (`pkg/gateway/templates.go`): Return JSON objects via `writeError`:
  ```json
  {
    "code": 404,
    "message": "template not found"
  }
  ```
- **Connect-RPC Handlers** (`pkg/gateway/connectrpc.go`): Return Connect-RPC JSON envelopes via `writeConnectError`:
  ```json
  {
    "code": "not_found",
    "message": "sandbox not found"
  }
  ```
- **Standard REST Sandbox Handlers** (`pkg/gateway/sandboxes.go`): Return plain-text error bodies via standard Go `http.Error`:
  ```
  sandbox not found
  ```

Because different error envelopes are emitted depending on the subsystem, upstream SDK exception parsing (e.g., catching specific E2B Python exception classes) may not map identically to hosted E2B across all error conditions.

---

## CubePod Superset Capabilities (Extensions)

CubePod provides enterprise capabilities that do not exist in standard E2B. These are exposed through CubePod's native SDKs (`sdks/python/`, `sdks/typescript/`, Go client in `pkg/client`, and CLI in `cmd/cubepod`) and REST superset endpoints:

1. **Persistent Writable RootFS & Non-Destructive Reset**:
   `POST /sandboxes/{id}/reset-rootfs` increments `spec.rootfsGeneration` and allocates a new OS `upperdir` PVC. This operation does not replace the sandbox's `workspace-pvc`.
2. **Copy-on-Write Sandbox Forking**:
   `POST /sandboxes/{id}/fork` quiesces guest filesystems via `FIFREEZE`, takes a storage-level CoW clone of both `rootfs` and `workspace` block volumes, and executes `FITHAW`, creating an independent child sandbox. Duration depends on the volume backend and is not a measured SLA on this page.
3. **Zero-Trust Egress Credential Substitution (`cpodref_`)**:
   Pass `secretBindings` when creating a sandbox. The sandbox runtime only ever sees opaque placeholders (such as `cpodref_openai_key`), which are swapped for real upstream API tokens strictly at the `cubepod-egress` sidecar proxy over mTLS.
4. **Streaming Workspace Synchronization (`archive-sync`)**:
   `POST /sandboxes/{id}/files/archive-sync` streams `.tar` / `.tar.gz` directory trees into the sandbox. Archive unpacking in `pkg/gateway/files.go` includes checks intended to reject `..` path segments; this page does not treat that as a fully audited security guarantee. The API is meant for bulk sync instead of one HTTP request per file.
5. **On-Demand Desktops & Authenticated Panel Proxies**:
   `POST /sandboxes/{id}/desktop/start` lazily initializes `Xvfb`, Chromium CDP, `noVNC`, and `ttyd`, returning HMAC-signed `/panel/{token}/` URLs for live browser viewing and terminal interaction.

---

## Migration Checklist

When transitioning from hosted E2B to self-hosted CubePod:

1. **Verify Environment Variables**:
   Ensure `E2B_DOMAIN` (with wildcard DNS) or both `E2B_API_URL` and `E2B_SANDBOX_URL` are exported before importing the official SDK.
2. **Check Template Feature Flags** (both binaries define `--e2b-template-api` / `--feature-gate=E2BTemplateAPI=true` and `--template-build` in `cmd/cubepod-gateway/main.go` and `cmd/cubepod-controller/main.go`):
   - Enable `--e2b-template-api=true` (or the equivalent feature gate) on **cubepod-gateway** to register the E2B template HTTP routes.
   - Enable `--template-build=true` on **cubepod-gateway** as well; otherwise build-related routes (context upload, logs, start build) return HTTP 501 before any forwarding.
   - In Kubernetes, also enable `--template-build=true` on **cubepod-controller**, which runs the BuildKit or Docker build pipeline. The controller also accepts `--e2b-template-api`.
   - In local/single-process mode (no Kubernetes client and no `--controller-url`), the gateway's in-process reconciler uses `LocalDockerBuilder`; a separate controller is not required.
3. **Audit `/workspace` Paths in Templates**:
   Review any Dockerfiles used in `Template.build()` to confirm they do not `COPY` files into `/workspace`.
4. **Account for Process Lifecycles across Pauses**:
   Do not rely on in-memory daemon processes surviving a sandbox pause. Structure agent tasks so persistent state is written to `/workspace` or RootFS.
5. **Run the Compatibility Test Harness**:
   Maintainers can validate their gateway deployment against the official Python SDK test suite:
   ```bash
   make test-e2b-compat
   ```

---

## Next Steps

- Explore the complete REST and streaming contract in the **[API Overview](/reference/api-overview/)**.
- Test endpoints directly in the **[Interactive OpenAPI Explorer](/api/)**.
- Learn about native enterprise SDKs in the **[Python SDK Guide](/sdks/python/)** and **[TypeScript SDK Guide](/sdks/typescript/)**.
- Review Kubernetes deployment and storage configuration in **[Kubernetes & Helm Deployment](/deployment/kubernetes-helm/)**.

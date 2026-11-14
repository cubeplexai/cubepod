---
title: API Overview
description: Complete HTTP, SSE, NDJSON, Connect-RPC, WebSocket/WebRTC Panel Proxy, and Template Build API reference grounded in api/openapi/cubepod-v1.yaml and pkg/gateway.
---

The CubePod Gateway (`cmd/cubepod-gateway`) exposes an **E2B-compatible superset** HTTP/streaming API documented in `api/openapi/cubepod-v1.yaml` (OpenAPI `3.1.0`, spec version `0.4.1`). All official SDKs ([Python](/sdks/python/), [TypeScript](/sdks/typescript/), [Go & CLI](/sdks/go-and-cli/)) communicate with this Gateway surface.

For interactive schema exploration and copy-paste request testing, open the [Interactive API Reference](/api/) or fetch the raw OpenAPI YAML at [`/openapi.yaml`](/openapi.yaml) (also served live by every running Gateway at `GET /openapi.yaml`). For LLM agents ingesting CubePod documentation, fetch [`/llms.txt`](/llms.txt) or [`/llms-full.txt`](/llms-full.txt).

## Base URL, Authentication & Subdomain Routing

- **Authentication**: Pass an `Authorization: Bearer <token>` header when Gateway auth is configured, or omit for local unauthenticated dev instances.
- **Health & Observability**:
  - `GET /health` — Gateway liveness/readiness JSON.
  - `GET /metrics` — Prometheus metrics.
  - `GET /openapi.yaml` — Serves the canonical OpenAPI 3.1 YAML specification.
- **Subdomain Host Router**: Requests matching the regex `^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$` (for example `https://3000-sbx-abc123.sandboxes.example.com/`) extract `port` (`3000`) and `sandboxID` (`sbx-abc123`), automatically resume the sandbox if it is in `Paused` state, and reverse-proxy HTTP or WebSocket traffic directly to `PodIP:<port>` (with built-in local fallbacks for port `49999` `/execute` and port `49983` Connect-RPC).

## Sandbox Lifecycle

| Method & Path | Purpose | Key Query / Body Parameters |
| :--- | :--- | :--- |
| `POST /sandboxes` (`POST /v2/sandboxes`) | Create a sandbox (claiming from WarmPool if available) | `templateID`, `timeout` / `ttlSeconds`, `cpuCount`, `memoryMB`, `envVars` (`cpodref_` supported), `workspacePvcSize`, `metadata` |
| `GET /sandboxes` | List sandboxes | Query filters: `state`, `templateID` |
| `GET /sandboxes/{id}` | Inspect current sandbox status, IPs, generation, and state | Path: `id` |
| `DELETE /sandboxes/{id}` | Destroy sandbox | Query: `deleteVolumes` (`false` retains PVCs; `true` deletes PVCs) |
| `POST /sandboxes/{id}/pause` | Scale compute to zero while preserving `rootfs` and `/workspace` PVCs | Path: `id` |
| `POST /sandboxes/{id}/connect` | Resume a `Paused` sandbox and wait until `podd` is healthy | Optional timeout update |
| `POST /sandboxes/{id}/timeout` | Set/extend sandbox timeout | Body: `{ "timeoutSeconds": <int> }` |
| `POST /sandboxes/{id}/refreshes` | Refresh lease / extend TTL (E2B alias) | Body: `{ "duration": <int> }` |
| `POST /sandboxes/{id}/lease` | Renew sandbox TTL lease | Body: lease duration / TTL |
| `POST /sandboxes/{id}/reset-rootfs` | Increment `rootfsGeneration` to clear OS rootfs while keeping `/workspace` intact | Body: optional `template` / reset options |
| `POST /sandboxes/{id}/fork` | Clone sandbox via CSI snapshot (`cow_fork`) into an independent child sandbox | Optional fork parameters |

## Command Execution, Cursor Logs & SSE Streaming

Every foreground or background process executes inside the guest via the Rust `podd` daemon (`:49983`), which indexes stdout/stderr lines by monotonic cursor for reliable reconnection.

| Method & Path | Purpose | Notes |
| :--- | :--- | :--- |
| `POST /sandboxes/{id}/commands` (`POST /sandboxes/{id}/commands/run`) | Run a command | Supports `cmd`/`command`, `args`, `cwd`, `envs`/`env`, `background`, `timeout`/`timeoutSeconds`, `uid`, `gid` |
| `GET /sandboxes/{id}/commands/{cmdID}/status` | Fetch process status and exit code | Returns `exitCode`, `running`, signal metadata |
| `GET /sandboxes/{id}/commands/{cmdID}/logs` | Fetch incremental log lines | Query: `cursor=<int>`; returns lines + `nextCursor` |
| `GET /sandboxes/{id}/commands/{cmdID}/stream` | Stream output as Server-Sent Events (`text/event-stream`) | Gateway polls `podd` logs every `50ms` and emits live SSE frames until process exit |
| `POST /sandboxes/{id}/commands/{cmdID}/kill` | Terminate a running process | Sends signal to process inside guest |

## Files & Archive Sync

| Method & Path | Purpose | Notes |
| :--- | :--- | :--- |
| `PUT /sandboxes/{id}/files` (`PUT /files`) | Write raw file bytes | Query: `path`, optional `uid`, `gid`, `mode` |
| `GET /sandboxes/{id}/files` (`GET /files`) | Read raw file bytes | Query: `path` |
| `DELETE /sandboxes/{id}/files` (`DELETE /files`) | Remove file or directory | Query: `path` |
| `GET /sandboxes/{id}/files/list` (`GET /files/list`) | List directory entries | Query: `path` |
| `POST /sandboxes/{id}/files/archive-sync` | Stream compressed `tar.gz` workspace updates | Query: `dest` / `destination`, `uid`, `gid`, `remove_paths` / `removePaths`; `podd` validates relative entries and rejects `..` or leading `/` traversal |

:::tip
The direct `/files` and `/files/list` routes resolve the target sandbox via `E2b-Sandbox-Id`, `X-Sandbox-ID`, query params, or the subdomain router host header for E2B `envd` client parity.
:::

## Code Interpreter (`NDJSON` & Persistent Kernels)

CubePod embeds stateful code interpreter execution directly inside `podd` (`crates/podd/src/interpreter.rs`) supporting six kernels: **`python`**, **`bash`**, **`javascript`**, **`typescript`**, **`r`**, and **`java`**.

- **Execute Code**: `POST /sandboxes/{id}/code/execute` (also `POST /sandboxes/{id}/execute` and `POST /execute`).
  - Streams `application/x-ndjson` lines containing `stdout`, `stderr`, `result` (supporting `text`, `png` base64 images, `html`, `json`), `error` (`name`, `value`, `traceback`), and `number_of_executions`.
- **Stateful Context Management**: In-guest `podd` exposes `/contexts` and `/v1/code/contexts` (with Gateway paths at `/sandboxes/{id}/code/contexts` and `/sandboxes/{id}/code/contexts/{contextId}`) to create, inspect, restart, and delete persistent stateful language sessions.

## Desktop & Signed Panel Proxy

For desktop-enabled images (such as `cubeplex-ubuntu24-desktop`), the Gateway coordinates Xvfb/x11vnc/noVNC/Selkies startup and mints short-lived HMAC-signed reverse-proxy URLs:

| Method & Path | Purpose | Notes |
| :--- | :--- | :--- |
| `POST /sandboxes/{id}/desktop/start` | Start display server & desktop session | Validates `resolution` (`^[0-9]{3,5}x[0-9]{3,5}x(16\|24\|32)$`), optional `initialUrl`, `expiresInSeconds`; returns `422 desktop_not_installed` if the image lacks desktop binaries. Aliased under `/v1/sandboxes/{id}/desktop/start` |
| `GET /sandboxes/{id}/desktop/status` | Inspect desktop status (`desktopStatus`, panel URLs) | Also aliased under `/v1/sandboxes/{id}/desktop/status` |
| `POST /sandboxes/{id}/desktop/stop` | Stop desktop services | Also aliased under `/v1/sandboxes/{id}/desktop/stop` |
| `GET /sandboxes/{id}/endpoints/{port}` & `POST /sandboxes/{id}/endpoints/{port}` | Mint or query a signed proxy token for any sandbox TCP/WebSocket port | Token format: `base64url(sandboxID:port:expUnix).hmacSha256Sig` |
| `ANY /panel/{token}/...` | HMAC-verified HTTP, WebSocket, and WebRTC proxy | Strips `/panel/{token}` prefix and proxies to `PodIP:<port>` inside the sandbox |

See [Desktop & Panel Proxy](/concepts/desktop-and-panel-proxy/) for architecture details.

## E2B Connect-RPC Compatibility Endpoints

For drop-in E2B SDK interoperability (`pkg/gateway/connectrpc.go`), the Gateway translates Connect-RPC service calls—whether rooted at `/` (resolved via `E2b-Sandbox-Id`, `X-Sandbox-ID`, query param, or subdomain host) or nested at `/sandboxes/{id}/`:

- **Process Service**:
  - `POST /process.Process/Start`
  - `POST /process.Process/SendSignal`
  - `POST /process.Process/Connect`
  - `POST /process.Process/List`
- **Filesystem Service**:
  - `POST /filesystem.Filesystem/Read`
  - `POST /filesystem.Filesystem/Write`
  - `POST /filesystem.Filesystem/List`
  - `POST /filesystem.Filesystem/ListDir`
  - `POST /filesystem.Filesystem/Stat`
  - `POST /filesystem.Filesystem/Remove`
  - `POST /filesystem.Filesystem/MakeDir`

Both plain `application/json` payloads and framed `application/connect+json` streaming envelopes (`1-byte flag` where `0` = message frame and `2` = end-of-stream + `4-byte big-endian uint32 length`) are handled natively.

## WarmPools

| Method & Path | Purpose |
| :--- | :--- |
| `POST /warmpools` | Create or update a `PodWarmPool` resource with target template and replica count |
| `GET /warmpools` | List all configured WarmPools and ready/available pod counts |

## Template Catalog & Builds (`E2BTemplateAPI`)

When `--e2b-template-api=true` is enabled on the Gateway and Controller, the full template registry and BuildKit build pipeline become available:

| Method & Path | Purpose | Key Headers / Constraints |
| :--- | :--- | :--- |
| `GET /templates` & `GET /v2/templates` | List templates | `/v2/templates` supports cursor pagination returning `X-Next-Token` |
| `POST /templates` & `POST /v2/templates` | Trigger template build | Returns build ID & status |
| `POST /v3/templates` | Idempotent async template build (v3) | Honors `Idempotency-Key`, returns `202 Accepted` |
| `GET /templates/{id}`, `PATCH /templates/{id}`, `PATCH /v2/templates/{id}`, `DELETE /templates/{id}` | Inspect, update (`public`), or delete template | — |
| `GET /templates/aliases/{alias}` | Resolve human alias to template ID & metadata | — |
| `GET /templates/{id}/files/{hash}` & `PUT /templates/{id}/files/{hash}` | Check or upload SHA-256 content-addressed build context tarball | `{hash}` validated against `^[0-9a-f]{64}$` |
| `POST /v2/templates/{id}/builds/{buildID}` | Submit Dockerfile / build step instructions for execution | Supported with local Docker or in-cluster BuildKit |
| `GET /templates/{id}/builds/{buildID}/status` | Inspect build state & `kubeTemplateName` / `imageRef` | — |
| `GET /templates/{id}/builds/{buildID}/logs` | Fetch structured build log stream | — |
| `POST /templates/tags`, `DELETE /templates/tags`, `GET /templates/{id}/tags` | Assign, remove, or inspect template tags | — |

## Internal Zero-Trust Egress Exchange

| Method & Path | Purpose |
| :--- | :--- |
| `POST /internal/v1/egress/exchange` | Called over mTLS by the `cubepod-egress` sidecar to resolve `cpodref_` credential placeholders into real upstream secrets just before outbound transmission |

## Next Steps

- Try the interactive Swagger/Scalar playground at [Interactive API Reference](/api/).
- Explore the Python, TypeScript, and Go client implementations in [Python SDK](/sdks/python/), [TypeScript SDK](/sdks/typescript/), and [Go SDK & CLI](/sdks/go-and-cli/).

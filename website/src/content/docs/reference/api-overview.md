---
title: API Overview
description: Complete HTTP, SSE, NDJSON, Connect-RPC, WebSocket/WebRTC Panel Proxy, and Template Build API reference grounded in api/openapi/cubepod-v1.yaml and pkg/gateway.
---

The CubePod Gateway (`cmd/cubepod-gateway`) exposes an **E2B-compatible superset** HTTP/streaming API documented in `api/openapi/cubepod-v1.yaml` (OpenAPI `3.1.0`, spec version `0.6.0`). All official SDKs ([Python](/sdks/python/), [TypeScript](/sdks/typescript/), [Go & CLI](/sdks/go-and-cli/)) communicate with this Gateway surface.

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
| `POST /sandboxes/{id}/fork` *(Changed in v0.6.0)* | Clone sandbox via CSI (`pvc-clone` or `snapshot` mode) / reflink into an independent child sandbox | Optional fork parameters |
| `GET /sandboxes/{sandboxID}/metrics` *(Added in v0.6.0)* | Fetch time-series `SandboxMetric` snapshots for one sandbox | Query: `start`, `end` (Unix seconds or RFC3339), `autoResume` (`true` default; `false` reads cached ring buffer) |
| `GET /sandboxes/metrics` *(Added in v0.6.0)* | Fetch latest `SandboxMetric` snapshot per sandbox | Query: `sandboxIds` (comma-separated or repeated); returns `{"sandboxes": {...}}` without waking paused sandboxes |

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
- **Stateful Context Management**: In-guest `podd` exposes `/contexts` and `/v1/code/contexts` internally (reachable directly via the subdomain port router on port `49983`) to create, inspect, restart, and delete persistent language sessions. When invoking code via the Gateway route `/sandboxes/{id}/code/execute`, clients pass `context_id` in the execution payload to target an existing context.

## Desktop & Signed Panel Proxy

For desktop-enabled images (such as `cubeplex-ubuntu24-desktop`), the Gateway coordinates Xvfb/x11vnc/noVNC/Selkies startup and mints short-lived HMAC-signed reverse-proxy URLs:

| Method & Path | Purpose | Notes |
| :--- | :--- | :--- |
| `POST /sandboxes/{id}/desktop/start` | Start display server & desktop session | Validates `resolution` (`^[0-9]{3,5}x[0-9]{3,5}x(16\|24\|32)$`), optional `initialUrl`, `expiresInSeconds`, and `transport` (`"webrtc" \| "novnc"`, *Added in v0.6.0*); returns `422 desktop_not_installed` if the image lacks desktop binaries. Aliased under `/v1/sandboxes/{id}/desktop/start` |
| `GET /sandboxes/{id}/desktop/status` | Inspect desktop status (`desktopStatus`, panel URLs) | Also aliased under `/v1/sandboxes/{id}/desktop/status` |
| `POST /sandboxes/{id}/desktop/stop` | Stop desktop services | Also aliased under `/v1/sandboxes/{id}/desktop/stop` |
| `GET /sandboxes/{id}/endpoints/{port}` & `POST /sandboxes/{id}/endpoints/{port}` | Mint or query a signed proxy token for any sandbox TCP/WebSocket port | Token format: `base64url(sandboxID:port:expUnix).hmacSha256Sig` |
| `ANY /panel/{token}/...` | HMAC-verified HTTP, WebSocket, and WebRTC proxy | Strips `/panel/{token}` prefix and proxies to `PodIP:<port>` inside the sandbox |

See [Desktop & Panel Proxy](/concepts/desktop-and-panel-proxy/) for architecture details.

## E2B Connect-RPC Compatibility Endpoints

For drop-in E2B SDK interoperability (`pkg/gateway/connectrpc.go`), the Gateway translates Connect-RPC service calls—whether rooted at `/` (resolved via `E2b-Sandbox-Id`, `X-Sandbox-ID`, query param, or subdomain host) or nested at `/sandboxes/{id}/` (see the [E2B Compatibility Reference](/reference/e2b-compatibility/) for the comprehensive per-method matrix):

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

## Secrets, Volumes, Events & Webhooks

- **E2B Secrets (`E2BSecretsAPI`, *Changed in v0.6.0*)**: `POST /secrets`, `GET /secrets`, `GET /secrets/{secretID}`, `POST /secrets/{secretID}`, `DELETE /secrets/{secretID}` — tenant-isolated write-only secret store (`KubeSecretBackend` HA or AES-256-GCM file store) resolved via `${e2b.secrets.<name>}` (`Secret.fill()`).
- **E2B Volumes (*Changed in v0.7.0*)**: `POST /volumes`, `GET /volumes`, `GET /volumes/{volumeID}`, `DELETE /volumes/{volumeID}`, `POST /volumes/rotate-key` (*Added in v0.7.0* — migrates tenant volume & mount ownership from `{"previousApiKey": "<oldKey>"}` to `X-API-Key: <newKey>`, revoking `<oldKey>` via the RWX `flock`-guarded `tenants.json` keyring), plus content endpoints `/volumecontent/{volumeID}/{file,dir,path}` and `volumeMounts` on `POST /sandboxes` (K8s Kata RWX mounts gated by `K8sE2BVolumeMounts` / Helm `e2bVolumes.rwxStorageClassName`). Supports envelope encryption of volume content tokens via `CUBEPOD_E2B_VOLUMES_KEK` or `CUBEPOD_E2B_VOLUMES_KMS_URL` (`CUBEPOD_E2B_VOLUMES_KMS_TOKEN`), per-tenant byte/count quotas (`CUBEPOD_E2B_MAX_TENANT_BYTES`, `CUBEPOD_E2B_MAX_VOLUMES_PER_TENANT`), and `PrometheusRule` alerts in `deploy/monitoring/e2b-volumes-alerts.yaml` (*Added in v0.7.0*).
- **Lifecycle Events & Webhooks (`SandboxEventsAndWebhooks`, *Added in v0.6.0*)**: `GET /events`, `GET /sandboxes/{sandboxID}/events`, `POST /webhooks`, `GET /webhooks`, `GET /webhooks/{webhookID}`, `PATCH /webhooks/{webhookID}`, `DELETE /webhooks/{webhookID}`.

## Feature Gates (`--feature-gate`) *(Added in v0.6.0)*

Both `cubepod-gateway` and `cubepod-controller` accept `--feature-gate=GateA=true,GateB=false`:

| Feature Gate | Default | Components | Purpose |
| :--- | :--- | :--- | :--- |
| `E2BTemplateAPI` | `false` | `gateway`, `controller` | Enables E2B JSON-array `GET /templates` and `/v2`–`/v3` template APIs (`--e2b-template-api` also toggles it). |
| `K8sE2BVolumeMounts` | `false` | `gateway` | Enables Kubernetes RWX E2B `volumeMounts` (auto-enabled in Helm when `e2bVolumes.rwxStorageClassName` is set). |
| `TransparentEgress` | `true` | `gateway`, `controller` | Controls `CUBEPOD_TRANSPARENT=1` + `NET_ADMIN` `nftables` interception and `/internal/v1/egress/exchange`. |
| `DesktopStack` | `true` | `gateway` | Enables `/sandboxes/{id}/desktop/{start,status,stop}` routes. |
| `StatelessGatewayHA` | `true` | `gateway` | Enables Kubernetes `PodBox` cache-miss reconstruction across gateway replicas. |
| `E2BFilesystemWatch` | `true` | `gateway` | Enables Connect-RPC `WatchDir` / watcher endpoints. |
| `E2BSecretsAPI` | `true` | `gateway` | Enables `/secrets` CRUD and `${e2b.secrets.<name>}` resolution. |
| `SandboxEventsAndWebhooks` | `true` | `gateway` | Enables `/events` and `/webhooks` APIs and async HMAC webhook dispatch. |

## Prometheus Observability & `podd` Metrics *(Added in v0.6.0)*

### In-Guest `podd` Endpoints (`:49983`)

- **`GET /metrics`**: Returns a JSON `SandboxMetric` snapshot (`timestamp`, `timestampUnix`, `cpuCount`, `cpuUsedPct`, `memUsed`, `memTotal`, `memCache`, `diskUsed`, `diskTotal`) sampled from `/proc/stat`, `/proc/meminfo`, cgroup v2/v1 limits, and `statvfs("/workspace")`. Passing `?format=prometheus` or `Accept: text/plain` returns Prometheus text exposition.
- **`GET /v1/prometheus`**: Exposes Prometheus `0.0.4` gauges:
  - `cubepod_podd_active_processes`
  - `cubepod_podd_init_duration_milliseconds` (from `/run/cubepod/init-state.json` `duration_ms`)
  - `cubepod_podd_init_e2b_bind_duration_ms` (from `/run/cubepod/init-state.json` `e2b_bind_duration_ms`)
  - `cubepod_podd_cpu_used_percent`, `cubepod_podd_memory_used_bytes`, `cubepod_podd_memory_total_bytes`, `cubepod_podd_disk_used_bytes`, `cubepod_podd_disk_total_bytes`
  - *Added in v0.7.0.* Watcher metrics: `cubepod_podd_watchers_active` (gauge), `cubepod_podd_watch_events_emitted_total` (counter), and `cubepod_podd_watch_overflows_total{kind="inotify_queue|ring_buffer|subscriber_queue"}` (counter). The same series are returned by `GET /metrics?format=prometheus`.

### Gateway (`:8080/metrics`), Controller (`:8081/metrics`) & Egress (`:19090/metrics`)

| Component | Metric Name | Type | Description / Labels |
| :--- | :--- | :--- | :--- |
| **Gateway** | `cubepod_gateway_http_requests_total` / `cubepod_gateway_requests_total` | Counter | Total HTTP requests handled by `cubepod-gateway` |
| **Gateway** | `cubepod_e2b_requests_total` | Counter | Normalized E2B API requests (`endpoint`, `status`) |
| **Gateway** | `cubepod_e2b_request_duration_seconds` | Histogram | Latency histogram of E2B API requests |
| **Gateway** | `cubepod_gateway_sandboxes_created_total` | Counter | Total sandboxes created |
| **Gateway** | `cubepod_gateway_autoresume_total` / `cubepod_gateway_auto_resumes_total` | Counter | Total transparent sandbox auto-resume events |
| **Gateway** | `cubepod_gateway_commands_executed_total` | Counter | Total commands executed in sandboxes |
| **Gateway** | `cubepod_gateway_sandboxes_active` | Gauge | Active sandboxes by `state` (`Running`, `Paused`) |
| **Gateway** | `cubepod_gateway_warmpools_total` | Gauge | Total configured WarmPools |
| **Gateway** | `cubepod_gateway_crd_resyncs_total` | Counter | Total `PodBox`/`PodWarmPool` CRD resyncs |
| **Gateway** | `cubepod_sandbox_metrics_samples_total` | Counter | Background sandbox metrics sampler results (`result="ok|error"`). *Added in v0.7.0.* |
| **Gateway** | `cubepod_sandbox_cpu_count` / `cubepod_sandbox_cpu_used_pct` / `cubepod_sandbox_memory_used_bytes` / `cubepod_sandbox_memory_total_bytes` / `cubepod_sandbox_disk_used_bytes` / `cubepod_sandbox_disk_total_bytes` | Gauge | Latest sampled per-sandbox resource usage for Running sandboxes (`sandbox_id` label). *Added in v0.7.0.* |
| **Gateway** | `cubepod_panel_active_sessions` | Gauge | Active `/panel/{token}/` HTTP/WebSocket proxy sessions |
| **Gateway** | `cubepod_desktop_activations_total` | Counter | Desktop activations by `result` (`ok`, `unsupported`, `error`) |
| **Gateway** | `cubepod_secrets_operations_total` | Counter | Secret store operations by `op` and `status` |
| **Gateway** | `cubepod_secrets_store_count` | Gauge | Total stored E2B secrets across tenants |
| **Gateway** | `cubepod_secrets_operation_duration_seconds` *(Added in v0.7.0)* | Histogram | Secret store latency by `operation` (`create`, `list`, `get`, `update`, `delete`, `resolve`); buckets 0.005s–10s |
| **Gateway** | `cubepod_watch_streams_active` / `cubepod_watchers_active` *(Added in v0.7.0)* | Gauge | Live `WatchDir` streams and active poll watchers (`CreateWatcher`) |
| **Gateway** | `cubepod_watch_events_total` / `cubepod_watch_overflows_total` *(Added in v0.7.0)* | Counter | Watch events emitted and `resource_exhausted` watch overflows |
| **Gateway** | `cubepod_e2b_volume_mounts_total` / `cubepod_e2b_volume_mount_errors_total` | Counter | E2B volume mounts attached and mount planning/attach errors |
| **Gateway** | `cubepod_e2b_volume_quota_rejections_total` *(Added in v0.7.0)* | Counter | E2B volume creation or upload requests rejected (`429`) due to tenant volume-count or byte quotas |
| **Gateway** | `cubepod_e2b_volumes_storage_capacity_bytes` / `cubepod_e2b_volumes_storage_used_bytes` / `cubepod_e2b_volumes_storage_available_bytes` *(Added in v0.7.0)* | Gauge | Filesystem capacity, used bytes, and available bytes (`statfs`) of the E2B volumes store directory |
| **Gateway** | `cubepod_e2b_volumes_total` *(Added in v0.7.0)* | Gauge | Total active E2B volumes stored across tenants |
| **Gateway** | `cubepod_events_recorded_total` | Counter | Sandbox lifecycle events recorded (when events manager is enabled) |
| **Gateway** | `cubepod_webhook_deliveries_total` / `cubepod_webhook_queue_depth` | Counter / Gauge | Webhook delivery outcomes (`result`: `ok`, `failed`, `dropped`) and queue depth |
| **Gateway & Controller** | `cubepod_sandbox_forks_total` / `cubepod_sandbox_fork_duration_seconds` | Counter / Histogram | CoW fork attempts (`mode`, `result`) and provisioning duration |
| **Gateway & Controller** | `cubepod_templates` / `cubepod_gateway_templates_total` | Gauge | Templates by `status` (when `E2BTemplateAPI=true`) or total templates |
| **Controller** | `cubepod_active_sandboxes` / `cubepod_paused_sandboxes` | Gauge | Reconciled `Running` and `Paused` `PodBox` counts |
| **Controller** | `cubepod_warmpool_ready` / `cubepod_warmpool_available_replicas` | Gauge | Ready and unclaimed standby WarmPool replicas |
| **Controller** | `cubepod_template_builds_total` / `cubepod_template_build_duration_seconds` | Counter / Histogram | Template build completions (`result`) and duration |
| **Egress** | `cubepod_egress_requests_total` | Counter | Outbound requests (`mode`, `decision`) |
| **Egress** | `cubepod_egress_secret_exchanges_total` | Counter | Per-pod mTLS `cpodref_` exchanges (`result`: `ok`, `denied`, `error`) |
| **Egress** | `cubepod_egress_blocked_hosts_total` | Counter | Blocked outbound hosts (`host`) |

### Alerting Rules *(Added in v0.7.0)*

[`deploy/monitoring/watcher-secret-alerts.yaml`](https://github.com/cubeplexai/cubepod/blob/main/deploy/monitoring/watcher-secret-alerts.yaml) is a Prometheus Operator `PrometheusRule` providing `CubePodSecretStoreHighErrorRate`, `CubePodSecretStoreHighLatency` (p99 > 0.5s), `CubePodSecretExchangeErrors`, and `CubePodWatchOverflowDetected`.

## Internal Zero-Trust Egress Exchange

| Method & Path | Purpose |
| :--- | :--- |
| `POST /internal/v1/egress/exchange` | Called over strict per-pod mTLS (dedicated `--egress-mtls-addr` or main listener) by the `cubepod-egress` sidecar to resolve `cpodref_` credential placeholders into real upstream secrets just before outbound transmission |

## Next Steps

- Consult the **[E2B Compatibility Reference](/reference/e2b-compatibility/)** for the full official SDK method matrix, tested version constraints, and architectural differences.
- Try the interactive Swagger/Scalar playground at [Interactive API Reference](/api/).
- Explore the Python, TypeScript, and Go client implementations in [Python SDK](/sdks/python/), [TypeScript SDK](/sdks/typescript/), and [Go SDK & CLI](/sdks/go-and-cli/).

---
title: E2B Compatibility Guide
description: Point official E2B Python and TypeScript SDKs to CubePod using standard environment variables.
---

CubePod implements the core E2B REST and Connect-RPC endpoints, allowing existing E2B SDK workflows to target a self-hosted CubePod cluster by configuring standard environment variables.

:::tip[Detailed Compatibility Matrix & Architectural Differences]
This page provides a concise setup guide for environment variables and minimal code examples. For the full per-method compatibility matrix, architectural differences (such as compute scale-to-zero vs memory snapshots), tested version constraints, and untested areas, see the **[E2B Compatibility Reference](/reference/e2b-compatibility/)**.
:::

## Pointing Official E2B SDKs at CubePod

Configure your environment according to whether `cubepod-gateway` sits behind HTTPS wildcard ingress or is accessed over local HTTP:

### Production HTTPS + Wildcard Ingress

```bash
export E2B_DOMAIN="cubepod.example.com"
export E2B_API_KEY="cpod_live_secret"
```

When `E2B_DOMAIN` is set, official E2B SDKs send control-plane calls to `api.<E2B_DOMAIN>` and sandbox port traffic to `<port>-<sandboxID>.<E2B_DOMAIN>` (`cubepod-gateway` matches `^([0-9]+)-([a-zA-Z0-9_-]+)\.(.*)$` in the `Host` header).

### Local HTTP / Direct Gateway

When testing against `http://127.0.0.1:8080` without TLS or wildcard DNS, set both `E2B_API_URL` and `E2B_SANDBOX_URL` (the same configuration used by CubePod's `test/e2b-compat/` suite):

```bash
export E2B_API_URL="http://127.0.0.1:8080"
export E2B_SANDBOX_URL="http://127.0.0.1:8080"
export E2B_API_KEY="cpod_test_api_key"
```

## Official E2B SDK Examples

### Official `e2b` Python SDK (Sync & Async)

```python
from e2b import Sandbox

# Automatically reads E2B_API_URL / E2B_SANDBOX_URL / E2B_DOMAIN
with Sandbox.create("base", timeout=300) as sbx:
    sbx.files.write("/workspace/data.txt", "hello from e2b sdk")
    result = sbx.commands.run("cat /workspace/data.txt")
    print(result.stdout)
```

### Official `e2b-code-interpreter` SDK

`podd` exposes stateful execution contexts for 6 languages (`python`, `bash`, `javascript`, `typescript`, `r`, `java`) over NDJSON streams compatible with `e2b-code-interpreter`:

```python
from e2b_code_interpreter import Sandbox

with Sandbox.create() as sbx:
    sbx.run_code("x = 21 * 2")
    execution = sbx.run_code("print(f'result={x}')")
    print(execution.logs.stdout)
```

## Protocol Surface Implemented by `cubepod-gateway`

`cubepod-gateway` (`pkg/gateway/`) implements core E2B control-plane and data-plane routes (including Connect-RPC `envd` semantics on port `49983`):

<div class="cp-nowrap-col2">

| Surface | Endpoints Implemented | Behavior in CubePod |
| :--- | :--- | :--- |
| **Control Plane** | `POST /sandboxes`<br/>`POST / GET /v2/sandboxes`<br/>`GET /sandboxes`<br/>`GET /sandboxes/{id}`<br/>`DELETE /sandboxes/{id}` | Provisions or lists `PodBox` sandboxes; populates mandatory E2B fields (`sandboxID`, `templateID`, `clientID`, `envdVersion`, `state`, `startedAt`, `endAt`, `cpuCount`, `memoryMB`, `diskSizeMB`). |
| **Lifecycle & Leases** | `POST /sandboxes/{id}/refreshes`<br/>`POST /sandboxes/{id}/timeout`<br/>`POST /sandboxes/{id}/pause`<br/>`POST /sandboxes/{id}/connect`<br/>`POST /v2/sandboxes/{id}/connect` | Renews idle timers/leases, tears down compute on pause while retaining PVCs, and transparently auto-resumes paused sandboxes on `connect`. |
| **Commands & Streaming** | `POST /sandboxes/{id}/commands`<br/>`POST /sandboxes/{id}/commands/run`<br/>`GET .../logs?cursor=N`<br/>`POST .../kill`<br/>Connect-RPC `/process.Process/*`:<br/>`Start`, `SendSignal`, `Connect`, `List`,<br/>`SendInput`, `StreamInput`, `Update` | Executes commands and interactive PTY sessions inside the guest as unprivileged UID/GID `1000:1000` (`cubeplex`) with cursor-based incremental streaming, 64 KiB PTY replay, resize, and PGID cleanup. |
| **Filesystem & Sync** | `PUT / GET / DELETE /sandboxes/{id}/files`<br/>`GET .../files/list`<br/>`POST .../files/archive-sync`<br/>Connect-RPC `/filesystem.Filesystem/*`:<br/>`Read`, `Write`, `List`, `ListDir`, `Stat`,<br/>`Remove`, `MakeDir`, `WatchDir`,<br/>`CreateWatcher`, `GetWatcherEvents`,<br/>`RemoveWatcher` | Performs direct guest I/O, recursive directory ops, `inotify` directory watching, and path-traversal-safe `.tar`/`.tar.gz` sync against persistent `/workspace` or RootFS paths. |
| **Volumes & Content** | `POST / GET / DELETE /volumes`<br/>`/volumecontent/{id}/file`<br/>`/volumecontent/{id}/dir`<br/>`/volumecontent/{id}/path`<br/>`volumeMounts` on `POST /sandboxes` | Manages tenant-isolated persistent volumes and content upload/download, attached to Docker or K8s Kata sandboxes (*Changed in v0.8.0:* requires `CUBEPOD_K8S_VOLUME_MOUNTS=1` + `CUBEPOD_VOLUMES_SHARED_PVC` on live K8s). |
| **Sandbox Metrics** *(Added in v0.6.0)* | `GET /sandboxes/{sandboxID}/metrics`<br/>`GET /sandboxes/metrics?sandboxIds=...` | Queries guest CPU, memory, cache, and `/workspace` disk utilization from `podd` (`GET /metrics`), with optional `start`/`end` time filtering and bounded per-sandbox ring-buffer caching (`120` samples). Compatible with `sbx.get_metrics()` / `Sandbox.get_metrics()`. |
| **Secrets & Network Rules** *(HA in v0.6.0)* | `POST / GET /secrets`<br/>`GET / POST / DELETE /secrets/{id}`<br/>`network.rules.*.transform.headers`<br/>(`${e2b.secrets.<name>}` via `Secret.fill()`) | Stores write-only secrets (`KubeSecretBackend` HA across replicas or AES-256-GCM file store), resolves `Secret.fill()` (`${e2b.secrets.<name>}`) into per-sandbox `cpodref_` placeholders, and swaps them over strict mTLS in `cubepod-egress`. |
| **Lifecycle Events & Webhooks** *(Added in v0.6.0)* | `POST / GET /webhooks`<br/>`GET / PATCH / DELETE /webhooks/{id}`<br/>`GET /events`<br/>`GET /sandboxes/{id}/events` | Records E2B `v1` lifecycle events (`created`, `paused`, `resumed`, `updated`, `forked`, `killed`)—including async Kubernetes reaper transitions—and delivers SSRF-safe, HMAC-signed (`e2b-signature`) webhooks. |
| **Code Interpreter** | `POST /sandboxes/{id}/execute`<br/>`POST /sandboxes/{id}/code/execute`<br/>`POST /execute` | Manages stateful multi-language REPL kernels inside `podd` (`python`, `bash`, `javascript`, `typescript`, `r`, `java`). Contexts are managed inside `podd`. |
| **Subdomain Port Routing** | `Host: <port>-<sandboxID>.<domain>`<br/>`GET / POST /sandboxes/{id}/endpoints/{port}` | Auto-resumes paused sandboxes if needed, rewrites `/envd/*` to `podd` (`:49983`), and proxies arbitrary guest app ports or mints signed `/panel/{token}/` URLs. |
| **Template Catalog & Builds** | `/templates`, `/v2/templates`, `/v3/templates`<br/>`/templates/{id}/builds/{buildID}/status`<br/>`/templates/{id}/files/{hash}` | Requires `--e2b-template-api=true` and `--template-build=true` (BuildKit or local Docker). |

</div>

## CubePod Superset Capabilities Beyond E2B

When you use CubePod's native SDKs ([Python](/sdks/python/), [TypeScript](/sdks/typescript/), [Go & CLI](/sdks/go-and-cli/)) or REST superset endpoints, you unlock features not present in standard E2B:

- **Persistent Writable OS RootFS & Non-Destructive OS Reset**: `POST /sandboxes/{id}/reset-rootfs` increments `spec.rootfsGeneration`, allocating a fresh OS `upperdir` PVC while preserving `/workspace`.
- **1-Second CoW Forking**: `POST /sandboxes/{id}/fork` triggers `FIFREEZE` quiesce -> CSI/reflink clone -> `FITHAW` to branch both OS and `/workspace` state without disturbing the running parent.
- **Zero-Trust `cpodref_` Egress Vault**: Pass `secretBindings` at sandbox creation so agents only ever see `cpodref_` placeholders while `cubepod-egress` injects real credentials at the network boundary.
- **Lazy Interactive Desktops**: Call `/sandboxes/{id}/desktop/{start,status,stop}` to spin up `Xvfb`, Chromium CDP, `noVNC`, and `ttyd` on demand behind signed `/panel/{token}/` endpoints.

## Next Steps

- Consult the **[E2B Compatibility Reference](/reference/e2b-compatibility/)** for full matrix, tested constraints, and memory vs. compute differences.
- See the **[API Reference](/api/)** for the full OpenAPI 3.1 contract (`api/openapi/cubepod-v1.yaml`).
- Compare native clients in **[Python SDK](/sdks/python/)** and **[TypeScript SDK](/sdks/typescript/)**.
- Inspect **[CRD Reference](/reference/crds/)** for Kubernetes `PodBox` and `PodTemplate` schemas.

---
title: E2B Compatibility Guide
description: Point official E2B Python and TypeScript SDKs to CubePod using standard environment variables.
---

CubePod implements core E2B REST and Connect-RPC endpoints as designed in [CEP-0018](/ceps/0018-e2b-protocol-compatibility/) and [CEP-0019](/ceps/0019-e2b-template-api/). Official E2B Python and TypeScript SDK clients can be redirected from hosted E2B to a self-hosted CubePod instance by configuring environment variables.

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

| Surface | Endpoints Implemented | Behavior in CubePod |
| :--- | :--- | :--- |
| **Control Plane** | `POST /sandboxes`, `POST/GET /v2/sandboxes`, `GET /sandboxes`, `GET /sandboxes/{id}`, `DELETE /sandboxes/{id}` | Provisions or lists `PodBox` sandboxes; populates mandatory E2B fields (`sandboxID`, `templateID`, `clientID`, `envdVersion`, `state`, `startedAt`, `endAt`, `cpuCount`, `memoryMB`, `diskSizeMB`). |
| **Lifecycle & Leases** | `POST /sandboxes/{id}/refreshes`, `POST /sandboxes/{id}/timeout`, `POST /sandboxes/{id}/pause`, `POST /sandboxes/{id}/connect`, `POST /v2/sandboxes/{id}/connect` | Renews idle timers/leases, tears down compute on pause while retaining PVCs, and transparently auto-resumes paused sandboxes on `connect`. |
| **Commands & Streaming** | `POST /sandboxes/{id}/commands`, `POST /sandboxes/{id}/commands/run`, `GET .../logs?cursor=N`, `POST .../kill`, Connect-RPC `/process.Process/{Start,SendSignal,Connect,List,SendInput,StreamInput,Update}` | Executes commands and interactive PTY sessions inside the guest as unprivileged UID/GID `1000:1000` (`cubeplex`) with cursor-based incremental streaming, 64 KiB PTY replay, resize, and PGID cleanup. |
| **Filesystem & Sync** | `PUT/GET/DELETE /sandboxes/{id}/files`, `GET .../files/list`, `POST .../files/archive-sync`, Connect-RPC `/filesystem.Filesystem/{Read,Write,List,ListDir,Stat,Remove,MakeDir,WatchDir,CreateWatcher,GetWatcherEvents,RemoveWatcher}` | Performs direct guest I/O, recursive directory ops, `inotify` directory watching, and path-traversal-safe `.tar`/`.tar.gz` sync against persistent `/workspace` or RootFS paths. |
| **Volumes & Content** | `POST/GET/DELETE /volumes`, `/volumecontent/{id}/{file,dir,path}`, `volumeMounts` on `POST /sandboxes` | Manages tenant-isolated persistent E2B volumes and content upload/download, attached to Docker or K8s Kata sandboxes (requires `CUBEPOD_K8S_E2B_VOLUME_MOUNTS=1` + `CUBEPOD_E2B_SHARED_PVC` on live K8s). |
| **Secrets & Network Rules** | `POST/GET /secrets`, `GET/POST/DELETE /secrets/{id}`, `network.rules.*.transform.headers` (`${e2b.secrets.<name>}`) | Stores write-only AES-256-GCM secrets and resolves `${e2b.secrets.<name>}` into per-sandbox `cpodref_` placeholders injected over mTLS by `cubepod-egress`. |
| **Code Interpreter** | `POST /sandboxes/{id}/execute`, `POST /sandboxes/{id}/code/execute`, `POST /execute` | Manages stateful multi-language REPL kernels inside `podd` (`python`, `bash`, `javascript`, `typescript`, `r`, `java`). Contexts are managed inside `podd`. |
| **Subdomain Port Routing** | `Host: <port>-<sandboxID>.<domain>` and `GET/POST /sandboxes/{id}/endpoints/{port}` | Auto-resumes paused sandboxes if needed, rewrites `/envd/*` to `podd` (`:49983`), and proxies arbitrary guest app ports or mints signed `/panel/{token}/` URLs. |
| **Template Catalog & Builds** | `/templates`, `/v2/templates`, `/v3/templates`, `/templates/{id}/builds/{buildID}/status`, `/templates/{id}/files/{hash}` | Requires `--e2b-template-api=true` and `--template-build=true` (BuildKit or local Docker). |

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

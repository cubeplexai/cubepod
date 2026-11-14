---
title: E2B Drop-in Compatibility
description: Connect official E2B Python and TypeScript SDKs (e2b, e2b-code-interpreter, @e2b/code-interpreter) and AI agent frameworks to CubePod without code changes.
---

CubePod provides **wire-level drop-in compatibility** with official E2B SDKs (`e2b`, `e2b-code-interpreter`, `@e2b/code-interpreter`) as specified in [CEP-0018](/ceps/0018-e2b-protocol-compatibility/) and [CEP-0019](/ceps/0019-e2b-template-api/). Existing LangChain, LlamaIndex, AutoGen, CrewAI, OpenHands, and Smolagents pipelines can switch from hosted E2B to self-hosted persistent Kata MicroVMs simply by setting environment variables.

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

## Unmodified Official E2B Examples

### Official `e2b` Python SDK (Sync & Async)

```python
from e2b import Sandbox

# Uses E2B_API_URL / E2B_SANDBOX_URL / E2B_DOMAIN automatically
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

`cubepod-gateway` (`pkg/gateway/`) implements both E2B's control plane and data plane (including Connect-RPC `envd` semantics on port `49983`):

| Surface | Endpoints Implemented | Behavior in CubePod |
| :--- | :--- | :--- |
| **Control Plane** | `POST /sandboxes`, `POST /v2/sandboxes`, `GET /sandboxes`, `GET /v2/sandboxes`, `GET /sandboxes/{id}`, `DELETE /sandboxes/{id}` | Provisions or lists `PodBox` sandboxes; always populates mandatory E2B fields (`sandboxID`, `templateID`, `clientID`, `envdVersion`, `state`, `startedAt`, `endAt`, `cpuCount`, `memoryMB`, `diskSizeMB`). |
| **Lifecycle & Leases** | `POST /sandboxes/{id}/refreshes`, `POST /sandboxes/{id}/timeout`, `POST /sandboxes/{id}/pause`, `POST /sandboxes/{id}/connect` | Renews idle timers/leases, tears down compute on pause while retaining PVCs, and transparently auto-resumes paused sandboxes on `connect`. |
| **Commands & Streaming** | `POST /sandboxes/{id}/commands`, `POST /sandboxes/{id}/commands/run`, `GET .../logs?cursor=N`, `POST .../kill`, Connect-RPC `/process.Process/{Start,SendSignal,Connect,List}` | Executes commands inside the guest as unprivileged UID/GID `1000:1000` (`cubeplex`) with cursor-based incremental streaming and PGID cleanup. |
| **Filesystem & Sync** | `PUT/GET/DELETE /sandboxes/{id}/files`, `GET .../files/list`, `POST .../files/archive-sync`, Connect-RPC `/filesystem.Filesystem/{Read,Write,List,ListDir,Stat,Remove,MakeDir}` | Performs direct guest I/O and path-traversal-safe `.tar`/`.tar.gz` directory sync against persistent `/workspace` or RootFS paths. |
| **Code Interpreter** | `POST /sandboxes/{id}/execute`, `POST /sandboxes/{id}/code/execute`, `/contexts` | Manages stateful multi-language REPL kernels inside `podd` (`python`, `bash`, `javascript`, `typescript`, `r`, `java`). |
| **Subdomain Port Routing** | `Host: <port>-<sandboxID>.<domain>` and `GET/POST /sandboxes/{id}/endpoints/{port}` | Auto-resumes paused sandboxes if needed, rewrites `/envd/*` to `podd` (`:49983`), and proxies arbitrary guest app ports or mints signed `/panel/{token}/` URLs. |
| **Template Catalog & Builds** | `/templates`, `/v2/templates`, `/v3/templates`, `/templates/{id}/builds/{buildID}/status`, `/templates/{id}/files/{hash}` | Enabled via `--e2b-template-api=true` (or `-feature-gate E2BTemplateAPI=true`) for `e2b template build` workflows. |

## CubePod Superset Capabilities Beyond E2B

When you use CubePod's native SDKs ([Python](/sdks/python/), [TypeScript](/sdks/typescript/), [Go & CLI](/sdks/go-and-cli/)) or REST superset endpoints, you unlock features not present in standard E2B:

- **Persistent Writable OS RootFS & Non-Destructive OS Reset**: `POST /sandboxes/{id}/reset-rootfs` increments `spec.rootfsGeneration`, allocating a fresh OS `upperdir` PVC while preserving `/workspace`.
- **1-Second CoW Forking**: `POST /sandboxes/{id}/fork` triggers `FIFREEZE` quiesce -> CSI/reflink clone -> `FITHAW` to branch both OS and `/workspace` state without disturbing the running parent.
- **Zero-Trust `cpodref_` Egress Vault**: Pass `secretBindings` at sandbox creation so agents only ever see `cpodref_` placeholders while `cubepod-egress` injects real credentials at the network boundary.
- **Lazy Interactive Desktops**: Call `/sandboxes/{id}/desktop/{start,status,stop}` to spin up `Xvfb`, Chromium CDP, `noVNC`, and `ttyd` on demand behind signed `/panel/{token}/` endpoints.

## Next Steps

- See the **[API Reference](/api/)** for the full OpenAPI 3.1 contract (`api/openapi/cubepod-v1.yaml`).
- Compare native clients in **[Python SDK](/sdks/python/)** and **[TypeScript SDK](/sdks/typescript/)**.
- Inspect **[CRD Reference](/reference/crds/)** for Kubernetes `PodBox` and `PodTemplate` schemas.

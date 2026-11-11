# CEP-0012: Complete Gateway API Surface and Canonical OpenAPI 3.1 Specification

- **CEP Number**: `0012`
- **Status**: `implementable`
- **Target Version**: `v0.3.0`
- **Components**: `pkg/gateway/server.go`, `pkg/gateway/server_test.go`, `api/openapi/cubepod-v1.yaml`

---

## 1. Summary

Completes the `cubepod-gateway` HTTP REST/SSE API surface so every capability of `podd` and `cubepod-controller` is exposed over the unified Gateway contract, and brings [`api/openapi/cubepod-v1.yaml`](../../../api/openapi/cubepod-v1.yaml) to 100% parity with the implementation.

---

## 2. Motivation

### Goals
1. Expose missing file management routes on `cubepod-gateway`:
   - `DELETE /sandboxes/{sandboxID}/files?path=...` (forwards to `podd` `DELETE /v1/files` or deletes from local sandbox workspace).
   - `GET /sandboxes/{sandboxID}/files/list?path=...` (forwards to `podd` `GET /v1/files/list` or lists local sandbox workspace directory entries with `name`, `path`, `is_dir`, `size`, `mode`).
2. Expose missing command & streaming routes on `cubepod-gateway`:
   - `GET /sandboxes/{sandboxID}/commands/{cmdID}/status` (returns `id`, `pid`, `running`, `exit_code`, `cursor`).
   - `GET /sandboxes/{sandboxID}/commands/{cmdID}/stream` (Server-Sent Events `text/event-stream` streaming `data: {"content":...,"cursor":...,"running":...}\n\n` events until the command exits).
3. Expose WarmPool and Template management routes on `cubepod-gateway`:
   - `POST /warmpools` (registers/reconciles a `PodWarmPoolSpec` via `reconciler.ReconcileWarmPool`).
   - `GET /warmpools` (lists active warm pool specs and ready standby counts).
   - `GET /templates` (returns available sandbox templates).
   - `GET /openapi.yaml` (serves the embedded or on-disk `api/openapi/cubepod-v1.yaml` specification).
4. Update `api/openapi/cubepod-v1.yaml` to document all 20+ routes and schemas.

### Non-Goals
- GraphQL or gRPC control-plane APIs.

---

## 3. Design Details

### 3.1 File Management Routes (`DELETE /sandboxes/{sandboxID}/files`, `GET /sandboxes/{sandboxID}/files/list`)
- Both routes trigger `ensureRunning(ctx, rec)` (Auto-Resume if `Paused`).
- If `rec.PoddURL != ""`, forward to `poddURL + "/v1/files"` (`DELETE`) or `poddURL + "/v1/files/list"` (`GET`).
- Otherwise execute against `rec.WorkspaceDir` using `resolveFilePath` (rejecting `..` traversal) and return the identical JSON schema as `podd` (`{"deleted":"..."}` and `{"entries":[...]}`).

### 3.2 Command Status & SSE Streaming (`GET /sandboxes/{sandboxID}/commands/{cmdID}/status`, `/stream`)
- `GET /sandboxes/{sandboxID}/commands/{cmdID}/status`: Forwards to `poddURL + "/v1/commands/" + cmdID + "/status"` when `rec.PoddURL != ""`, or returns the local `commandEntry` status.
- `GET /sandboxes/{sandboxID}/commands/{cmdID}/stream`: Sets `Content-Type: text/event-stream`, `Cache-Control: no-cache`, and polls incremental logs (`cursor`) every 50ms, flushing SSE `data: {...}\n\n` frames via `http.Flusher` until `running == false`.

### 3.3 WarmPool & Template Routes (`POST /warmpools`, `GET /warmpools`, `GET /templates`)
- `POST /warmpools`: Accepts `{"name":"default-pool","templateRef":"cubeplex-ubuntu24-desktop","replicas":2}`, calls `reconciler.ReconcileWarmPool`, and returns `201 Created` with pool status.
- `GET /warmpools`: Returns all registered warm pools and `warmPoolReadyCount`.

---

## 4. Test Plan
- Unit tests in `pkg/gateway/server_test.go` covering `DELETE /sandboxes/{id}/files`, `GET /sandboxes/{id}/files/list`, `GET /sandboxes/{id}/commands/{cmdID}/status`, SSE `/stream`, `POST/GET /warmpools`, and `GET /templates`.

---

## 5. Graduation Criteria & PRR
- All routes documented in `api/openapi/cubepod-v1.yaml` and verified by automated tests.

---

## 6. Alternatives Considered
- **WebSockets for command log streaming**: SSE (`text/event-stream`) works natively over standard HTTP/1.1 and HTTP/2 proxies and `httpx`/`curl` without handshake complexity.

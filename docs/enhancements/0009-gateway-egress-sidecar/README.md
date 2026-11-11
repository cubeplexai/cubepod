# CEP-0009: Gateway Live `podd` Proxy Engine and Zero-Trust Egress Sidecar Daemon

| Field | Value |
| :--- | :--- |
| **CEP Number** | `0009` |
| **Title** | Gateway Live `podd` Proxy Engine and Zero-Trust Egress Sidecar Daemon |
| **Authors** | `@cubeplex-arch` |
| **Status** | `implemented` |
| **Target Version** | `v0.2.0` |
| **Created** | `2026-11-10` |
| **Last Updated** | `2026-11-10` |

---

## Summary

This CEP completes the end-to-end data plane and zero-trust egress plane across `pkg/gateway`, `pkg/egress`, and a new `cmd/cubepod-egress` binary:
1. `pkg/gateway` adds upstream `podd` HTTP forwarding (`RegisterPoddURL(sandboxID, poddURL)`): whenever a sandbox has a live `podd` URL registered, `POST /commands/run`, `GET /commands/{id}/logs`, `POST /commands/{id}/kill`, `PUT /files`, `GET /files`, and `POST /files/archive-sync` are proxied directly to the sandbox's `podd` daemon (`:49983`).
2. `pkg/gateway` and `pkg/egress` expose the mTLS secret exchange HTTP handler (`POST /internal/v1/egress/exchange`) and secret binding registration during `POST /sandboxes`.
3. `pkg/egress/proxy.go` and `cmd/cubepod-egress/main.go` implement the Zero-Trust Egress Proxy sidecar that intercepts outbound requests from the sandbox, enforces `AllowedHosts`/`DeniedHosts` policy, scans headers for `cbxref_<32-char-base32>` tokens, exchanges them over mTLS against the Gateway Vault, and injects the real API key on the wire to the upstream destination.

---

## Motivation

In `v0.1.0`, `pkg/egress` provided the cryptographic CA, policy matcher, and `Vault` struct, while `pkg/gateway` executed commands locally. Connecting `pkg/gateway` to live `podd` instances and providing the runnable `cubepod-egress` HTTP forward/reverse proxy completes the entire security and execution loop.

### Goals

- **Live `podd` Data-Plane Forwarding in `pkg/gateway`**:
  - Add `SetPoddURL(sandboxID, poddURL string)` (and automatic `podd` routing when `rec.PoddURL != ""`).
  - Forward `/commands/run`, `/commands/{cmdID}/logs`, `/commands/{cmdID}/kill`, `/files`, and `/files/archive-sync` to `rec.PoddURL` (`/v1/commands/...`, `/v1/files...`) when `rec.PoddURL != ""`, while retaining the local fallback when `rec.PoddURL == ""`.
  - Call `POST <PoddURL>/v1/lifecycle/prepare-stop` during `POST /sandboxes/{sandboxID}/pause` when `rec.PoddURL != ""`.
- **Zero-Trust Egress Exchange & Sidecar (`pkg/egress/proxy.go` & `cmd/cubepod-egress/main.go`)**:
  - Implement `VaultExchangeHandler(vault *Vault, ca *CA) http.HandlerFunc` for `POST /internal/v1/egress/exchange` that extracts `sandboxID` from the mTLS peer certificate `CommonName` (or `X-CubePod-Sandbox-ID` when running over local loopback trust in tests), validates `(sandboxID, placeholder, sniHost, headerName)` via `vault.Exchange(...)`, and returns `{"secret": "<real-value>"}` or `403 Forbidden`.
  - Implement `EgressProxy` (`pkg/egress/proxy.go`) and `cmd/cubepod-egress/main.go`:
    - Checks destination host against `AllowedHosts` and `DeniedHosts` (`403 Forbidden` if blocked).
    - Scans outbound HTTP request headers for `cbxref_` tokens (e.g. `Authorization: Bearer cbxref_...` or `X-Api-Key: cbxref_...`), calls the Exchange endpoint over mTLS (`ca.ClientTLSConfig`), replaces the `cbxref_` substring with the real secret in flight, and forwards the request to the upstream target without ever logging or persisting the secret.

### Non-Goals

- Kernel-level eBPF packet filtering (handled at the Pod `iptables` / `NetworkPolicy` layer).

---

## Proposal

### Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
| :--- | :--- | :--- | :--- |
| Exfiltration via header reflection echo service | High | Low | `vault.Exchange` strictly verifies both `sniHost` and `headerName` against the `SecretBinding` allowlist before releasing the secret. |

---

## Design Details

### Wire Protocol for `/internal/v1/egress/exchange`

- **Request**:
  ```json
  {
    "sandboxID": "cpod-123",
    "placeholder": "cbxref_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567",
    "sniHost": "api.openai.com",
    "headerName": "Authorization"
  }
  ```
  When served over mTLS (`r.TLS != nil && len(r.TLS.PeerCertificates) > 0`), `sandboxID` is cryptographically enforced from `r.TLS.PeerCertificates[0].Subject.CommonName`.
- **Response (`200 OK`)**:
  ```json
  {
    "secret": "sk-proj-real-openai-key"
  }
  ```

### Test Plan

- **Unit & Integration Tests**:
  - `pkg/egress/proxy_test.go`:
    - Start a mock upstream LLM server (`httptest.NewServer`) that asserts `Authorization: Bearer sk-real-secret-999`.
    - Start a real mTLS Vault Exchange server using `CA` and `Vault` (`httptest.NewUnstartedServer` with `ca.ServerTLSConfig()`).
    - Start `EgressProxy` configured with the sandbox's mTLS client cert (`ca.MintSandboxCert("sbx-1")`).
    - Send an HTTP request through `EgressProxy` carrying `Authorization: Bearer cbxref_<token>`.
    - Verify the upstream server receives `Authorization: Bearer sk-real-secret-999`, while a request to an unauthorized host (`evil.com`) or wrong header (`X-Leak`) is rejected with `403 Forbidden`!
  - `pkg/gateway/server_test.go`:
    - Verify `Server` with a mock/live `podd` HTTP server (`SetPoddURL`) forwards `/commands/run`, `/logs`, `/kill`, `/files`, `/files/archive-sync`, and `/v1/lifecycle/prepare-stop` on `/pause`.

### Graduation Criteria

- `go test -v -race ./pkg/egress/... ./pkg/gateway/...` and `go build ./cmd/cubepod-egress` pass with 0 errors.

### Upgrade / Downgrade Strategy

- Fully backward-compatible with `v0.1.0` Gateway tests.

---

## Production Readiness Review (PRR)

- **Security**: Real secrets never touch disk or logs; mTLS client certificate `CommonName` binds every exchange request to its originating sandbox.

---

## Alternatives Considered

- **Passing secrets via environment variables at container start**: Explicitly forbidden by CubePod Security Invariant #3.

# CEP-0006: `podd` Native Streaming `archive-sync` and Process Output Tail Enhancements

| Field | Value |
| :--- | :--- |
| **CEP Number** | `0006` |
| **Title** | `podd` Native Streaming `archive-sync` and Process Output Tail Enhancements |
| **Authors** | `@cubeplex-arch` |
| **Status** | `implementable` |
| **Target Version** | `v0.2.0` |
| **Created** | `2026-11-10` |
| **Last Updated** | `2026-11-10` |

---

## Summary

This CEP extends the in-guest Rust daemon `podd` (`crates/podd`) with a native `POST /v1/files/archive-sync` endpoint so that `.tar.gz` and `.tar` workspace synchronization archives are unpacked directly inside the sandbox guest (Kata MicroVM or Docker container) with path-traversal protection, optional pre-extraction file removal (`remove_paths`), and automatic `1000:1000` (`cubeplex:cubeplex`) ownership assignment.

---

## Motivation

In `v0.1.0`, `POST /sandboxes/{sandboxID}/files/archive-sync` was unpacked by `cubepod-gateway` into a local workspace directory. In production Kubernetes (`Kata + CSI`) and isolated Docker containers, `cubepod-gateway` has no direct host filesystem access to the sandbox's `/workspace` or `/` mount namespace; all file synchronization must travel over HTTP to `podd` (`:49983`) inside the guest.

### Goals

- Implement `POST /v1/files/archive-sync?dest=/workspace&uid=1000&gid=1000&remove_paths=a,b` in `crates/podd/src/main.rs`.
- Support both `.tar.gz` (`application/gzip` or magic bytes `0x1f 0x8b`) and uncompressed `.tar` (`application/x-tar`) payloads.
- Reject any archive entry containing `..` components or absolute paths (`400 Bad Request`) before modifying disk state.
- Apply `chown(uid, gid)` to extracted files and directories when `geteuid() == 0`.
- Support `DELETE /v1/files?path=/workspace/foo` so individual files or directories can be removed via `podd`.

### Non-Goals

- Adding external Rust crate dependencies (`flate2`/`tar`); `podd` uses either safe in-tree IEEE POSIX ustar parsing + system `tar`/`gzip` fallback so the binary stays zero-dependency and tiny (<400 KB).

---

## Proposal

### User Stories

#### Story 1: Remote Multi-File Sync into Isolated MicroVM
As `cubepod-gateway`, when a user calls `archive_sync()` via the Python SDK, I can forward the raw `.tar.gz` HTTP request body directly to `http://<sandbox-ip>:49983/v1/files/archive-sync?dest=/workspace&uid=1000&gid=1000` and receive `{"status":"synced","dest":"/workspace","extracted":N}`.

### Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
| :--- | :--- | :--- | :--- |
| Malicious tar entry (`../../etc/shadow` or symlink escape) | High | Medium | Inspect every tar entry header prior to extraction; reject any entry with `..`, leading `/`, or symlink/hardlink type pointing outside `dest` with `400 Bad Request`. |

---

## Design Details

### API / Wire Contract on `podd` (`:49983`)

1. **`POST /v1/files/archive-sync`**:
   - Query params:
     - `dest` (default `/workspace`)
     - `uid` (default `1000`)
     - `gid` (default `1000`)
     - `remove_paths` (optional comma-separated relative paths under `dest` to remove before extraction)
   - Request body: raw `.tar.gz` (magic bytes `0x1f 0x8b`) or `.tar` bytes.
   - Validation:
     - Parse tar entries (decompressing via `gzip -dc` pipe when gzip magic bytes `0x1f 0x8b` are present, or pure-Rust tar block reader) and validate every entry path before writing to `dest`.
     - If any entry path is absolute or contains `..` path segments, abort immediately and return `400 Bad Request` (`{"error":"invalid_archive_path"}`).
   - Response (`200 OK`):
     ```json
     {"status":"synced","dest":"/workspace","extracted":3}
     ```
2. **`DELETE /v1/files?path=/workspace/old.txt`**:
   - Removes a file or directory tree (`remove_file` / `remove_dir_all`) inside the guest and returns `200 OK` (`{"deleted":"/workspace/old.txt"}`), or `404 Not Found` if missing.

### Runtime Behavior (Production Kubernetes vs. Local Development)

- **Production Mode (`Kata + CSI Raw Block`)**: Unpacks archives directly onto the mounted `/dev/cubepod-workspace` (`ext4`) or persistent RootFS `upperdir` inside the MicroVM.
- **Local Development Mode (`Docker + Loopback ext4`)**: Identical behavior inside the Docker container.

### Test Plan

- **Unit & Integration Tests (`crates/podd/src/main.rs`)**:
  - `test_archive_sync_tar_gz_and_remove_paths`: Create a real `.tar.gz` archive containing nested files (`src/main.py`, `README.md`), upload to `POST /v1/files/archive-sync?dest=<tmp>&remove_paths=stale.txt`, verify `stale.txt` is removed, `src/main.py` and `README.md` are extracted with exact contents, and `extracted == 2`.
  - `test_archive_sync_rejects_path_traversal`: Craft a tar archive containing `../escape.txt` and verify `POST /v1/files/archive-sync` returns `400 Bad Request` and does not create the file.
  - `test_file_delete_endpoint`: Verify `DELETE /v1/files?path=...` deletes files and returns `404` on missing paths.

### Graduation Criteria

- `cargo test -p podd` and `cargo clippy -p podd -- -D warnings` pass with 0 warnings.

### Upgrade / Downgrade Strategy

- Backward-compatible additive endpoints on `podd`.

---

## Production Readiness Review (PRR)

- **Observability**: Returns structured JSON counts of extracted files and error codes.
- **Security**: Strict pre-extraction tar header path traversal validation.

---

## Alternatives Considered

- **Multiple `PUT /v1/files` calls from Gateway**: Rejected because high-latency per-file round-trips degrade agent workspace initialization when syncing dozens of source files.

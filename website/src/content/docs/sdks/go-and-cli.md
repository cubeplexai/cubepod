---
title: Go SDK & CLI
description: Idiomatic Go client library (pkg/client) and the cubepod command-line interface for sandbox lifecycle, archive sync, template builds, and WarmPools.
---

CubePod provides an idiomatic, zero-external-dependency Go client library (`github.com/cubeplexai/cubepod/pkg/client`) alongside the `cubepod` CLI binary (`cmd/cubepod`).

## Part 1: Go Client SDK (`pkg/client`)

### Installation

```bash
go get github.com/cubeplexai/cubepod/pkg/client
```

### Initializing the Client

`client.New(baseURL, opts...)` defaults to `http://127.0.0.1:8080` and a `60s` HTTP timeout:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
)

func main() {
	ctx := context.Background()
	c := client.New(
		"http://localhost:8080",
		client.WithAPIKey("your-api-key"),
		client.WithTimeout(60*time.Second),
	)

	// Provision a persistent sandbox
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: "cubeplex-ubuntu24-desktop",
		VolumeKey:  "agent-ws-01",
		Metadata:   map[string]string{"owner": "team-ai"},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.DeleteSandbox(ctx, sb.SandboxID)

	res, err := c.RunCommand(ctx, sb.SandboxID, client.RunCommandRequest{
		Command: "uname -a",
		Workdir: "/workspace",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Output)
}
```

Available constructor options:
- `client.WithAPIKey(apiKey string)`: Attaches `Authorization: Bearer <apiKey>` and `X-API-KEY: <apiKey>` (*Changed in vNEXT* to also send `X-API-KEY`).
- `client.WithTimeout(timeout time.Duration)`: Overrides the default `60s` `http.Client.Timeout`.
- `client.WithHTTPClient(httpClient *http.Client)`: Supplies a custom `*http.Client`.

### Named Volume Mounts *(Added in vNEXT)*

Set `CreateSandboxRequest.VolumeMounts` to mount an existing named volume. Each entry contains the volume name and the absolute path inside the sandbox. Configure `client.WithAPIKey(...)` so requests include the `X-API-KEY` header required by the gateway for named mounts.

```go
c := client.New("http://localhost:8080", client.WithAPIKey("your-api-key"))
req := client.CreateSandboxRequest{
    TemplateID: "base",
    VolumeMounts: []client.SandboxVolumeMount{
        {Name: "datasets", Path: "/data"},
    },
}
```

Legacy `VolumeKey` requests are unchanged. Do not combine `VolumeKey` with a block volume mount at `/workspace`.

### Sandbox Lifecycle, Reset & CoW Fork

```go
// Inspect and list
sb, err := c.GetSandbox(ctx, sandboxID)
list, err := c.ListSandboxes(ctx, "cubeplex-ubuntu24-desktop") // "" for all

// Timeout & exclusive lease protection
err = c.SetTimeout(ctx, sandboxID, 3600)
err = c.AcquireLease(ctx, sandboxID, 300)

// Pause (deletes Pod, retains dual PVCs) & Auto-Resume Connect
sb, err = c.PauseSandbox(ctx, sandboxID)
sb, err = c.ConnectSandbox(ctx, sandboxID)

// Reset RootFS Upper while keeping /workspace intact
sb, err = c.ResetRootFS(ctx, sandboxID)

// 1-second Copy-on-Write clone of both RootFS and Workspace
child, err := c.ForkSandbox(ctx, sandboxID)

// Delete (with option to permanently delete PVCs)
err = c.DeleteSandbox(ctx, sandboxID)
err = c.DeleteSandboxWithOptions(ctx, child.SandboxID, true)
```

### Commands & UID/GID Handling

Because Go's `0` is the zero value for `int` (`omitempty`), `RunCommandRequest` provides `RunAsRoot`, `UIDPtr`, and `GIDPtr` so you can distinguish between *"let the server/template default decide"* and *"explicitly run as root (`uid=0, gid=0`)"*:

```go
// Background command
bg, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
	Command:    "python3 -u server.py",
	Background: true,
	Workdir:    "/workspace",
	Envs:       map[string]string{"PORT": "8080"},
})

// Explicit root execution
rootRes, err := c.RunCommand(ctx, sandboxID, client.RunCommandRequest{
	Command:   "apt-get update",
	RunAsRoot: true,
})

// Poll status, incremental cursor logs, or kill process group
status, err := c.GetCommandStatus(ctx, sandboxID, bg.ID)
logs, err := c.GetCommandLogs(ctx, sandboxID, bg.ID, 0) // pass logs.Cursor next
err = c.KillCommand(ctx, sandboxID, bg.ID)
```

### Files & Recursive `ArchiveSyncDir`

```go
// Write / read / list / delete files
err := c.WriteFile(ctx, sandboxID, "/workspace/hello.txt", []byte("hi\n"), "0644")

// Custom UID/GID ownership
uid, gid := 1000, 1000
err = c.WriteFileWithOwner(ctx, sandboxID, "/workspace/bin/run", data, "0755", &uid, &gid)

raw, err := c.ReadFile(ctx, sandboxID, "/workspace/hello.txt")
entries, err := c.ListFiles(ctx, sandboxID, "/workspace")
err = c.DeleteFile(ctx, sandboxID, "/workspace/hello.txt")

// In-memory tar.gz sync
err = c.ArchiveSync(ctx, sandboxID, "/workspace", map[string][]byte{
	"app/main.go": []byte("package main\n"),
})

// Walk a local directory and upload all files in a single tar.gz request
err = c.ArchiveSyncDir(ctx, sandboxID, "/workspace/skills", "./local-skills")
```

### Endpoints, WarmPools & Templates

```go
// Signed HMAC-SHA256 panel URL
ep, err := c.GetPanelEndpoint(ctx, sandboxID, 8080, 3600)

// WarmPools
wp, err := c.CreateWarmPool(ctx, "fast-pool", "cubeplex-ubuntu24-desktop", 3)
pools, err := c.ListWarmPools(ctx)

// Templates (E2B-compatible catalog & builds)
templates, err := c.ListTemplates(ctx)
tpl, err := c.GetTemplateByAlias(ctx, "base")

// Register an existing OCI image without BuildKit
created, err := c.CreateTemplateFromImage(ctx, client.CreateTemplateFromImageRequest{
	FromImage: "python:3.13-slim",
	Alias:     "py313-slim",
	CPUCount:  2,
	MemoryMB:  2048,
})

// Build via rootless BuildKit
built, err := c.BuildTemplate(ctx, client.BuildTemplateRequest{
	Alias:     "custom-agent",
	FromImage: "ubuntu:24.04",
	Steps: []client.BuildStep{
		{Type: "RUN", Args: []string{"apt-get update && apt-get install -y curl"}},
	},
})
```

### Error Handling

Non-2xx/3xx HTTP responses return `*client.APIError`:

```go
var apiErr *client.APIError
if errors.As(err, &apiErr) {
	fmt.Printf("HTTP %d: %s\n", apiErr.StatusCode, apiErr.Body)
}
```

---

## Part 2: `cubepod` CLI

### Building or Installing

```bash
# From the repository root
make build-cli
./bin/cubepod --help

# Or via Go toolchain
go install github.com/cubeplexai/cubepod/cmd/cubepod@latest
```

### Global Flags & Environment Variables

:::caution[Flag Placement]
Global flags **must appear before** the subcommand (for example, `cubepod --json sandbox list`), because the CLI parses global flags from leading arguments prior to dispatching the command.
:::

| Flag | Environment Variable | Default | Description |
| :--- | :--- | :--- | :--- |
| `--gateway-url` | `CUBEPOD_GATEWAY_URL` | `http://127.0.0.1:8080` | CubePod Gateway base URL. |
| `--api-key` | `CUBEPOD_API_KEY` | `""` | API Bearer token. |
| `--json` | — | `false` | Emit structured JSON output for scripting. |

### Command Reference

#### Health & MCP Server

```bash
cubepod health
cubepod mcp    # Starts MCP JSON-RPC 2.0 server over stdio
```

#### Sandbox Commands

| Command | Description |
| :--- | :--- |
| `cubepod sandbox create [--template <id>] [--volume-key <key>] [--volume-mount NAME:PATH ...]` | Provision a new sandbox (default template: `cubeplex-ubuntu24-desktop`). `--volume-mount` is repeatable and mounts an existing named volume (*Added in vNEXT*). |
| `cubepod sandbox list [--template <id>]` | List active and paused sandboxes, optionally filtered by template. |
| `cubepod sandbox get <sandbox-id>` | Inspect state, `rootfsGeneration`, and `volumeKey`. |
| `cubepod sandbox exec <sandbox-id> <cmd...>` | Run a foreground command and exit with the remote exit code. |
| `cubepod sandbox write-file <id> <path> <file\|text>` | Write a local file (if the path exists locally) or literal string to `<path>`. |
| `cubepod sandbox read-file <id> <path>` | Stream remote file bytes to `stdout`. |
| `cubepod sandbox ls <id> [path]` | List entries in `[path]` (default `/workspace`). |
| `cubepod sandbox rm <id> <path>` | Delete a file or directory inside the sandbox. |
| `cubepod sandbox archive-sync <id> <dest> <localdir>` | Sync a local directory into `<dest>` via atomic `tar.gz` extraction. |
| `cubepod sandbox pause <id>` | Pause compute (`0` CPU/RAM) while retaining persistent PVCs. |
| `cubepod sandbox connect <id>` | Auto-resume a paused sandbox to `Running`. |
| `cubepod sandbox reset-rootfs <id>` | Reset RootFS Upper while keeping `/workspace` intact. |
| `cubepod sandbox fork <id>` | Create a ~1s CoW clone of both volumes. |
| `cubepod sandbox delete <id>` | Terminate and delete a sandbox. |

Named volume mounts (*Added in vNEXT*) require an API key (the global `--api-key` flag, placed before the subcommand, or `CUBEPOD_API_KEY`); the CLI exits with an error before calling the gateway when none is configured. Each `--volume-mount` value is `NAME:PATH`, split on the first `:`; surrounding whitespace is trimmed and `PATH` must be absolute. Other rules are enforced by the gateway: the volume must exist, mount paths must be unique and outside reserved system paths, block volumes may only be mounted at `/workspace`, and a block `/workspace` mount cannot be combined with `--volume-key`:

```bash
cubepod --api-key "$CUBEPOD_API_KEY" sandbox create --template base \
  --volume-mount datasets:/data \
  --volume-mount assets:/mnt/assets
```

> Because the CLI sends `X-API-KEY` whenever an API key is configured (*Changed in vNEXT*), a gateway with `BlockWorkspaceVolumes` and lazy legacy volume claims enabled may adopt a `--volume-key` workspace as a block `/workspace` volume owned by that API key, and rejects keys whose volume belongs to another tenant with `403`.

#### Template Commands

Requires `featureGates.e2bTemplateAPI=true` on the Gateway for catalog/registration endpoints:

| Command | Description |
| :--- | :--- |
| `cubepod template list` | List available templates (`TEMPLATE ID`, `ALIASES`, `STATUS`, `CPU`, `MEMORY`). |
| `cubepod template get <id-or-alias> [--alias]` | Inspect a template by ID or alias (auto-falls back to alias lookup on 404 or `:`/`/`). |
| `cubepod template create --from-image <img> --alias <alias> [--name <name>] [--cpu <n>] [--memory-mb <n>] [--disk-mb <n>]` | Register a template from an existing OCI image without BuildKit. |
| `cubepod template update <id> --public=true\|false` | Update template public visibility. |
| `cubepod template delete <id> [--force]` | Delete a template (`--force` deletes even if referenced). |

#### WarmPool Commands

| Command | Description |
| :--- | :--- |
| `cubepod warmpool create <name> [--template <id>] [--replicas <n>]` | Create or scale a `PodWarmPool` (default template `cubeplex-ubuntu24-desktop`, replicas `2`). |
| `cubepod warmpool list` | List all active WarmPools and their ready replica counts. |

#### Volume Migration Commands (*Added in v0.8.0*)

| Command | Description |
| :--- | :--- |
| `cubepod volume migrate-legacy --volumes-dir <dir> [--namespace <ns>] [--owner-map <file.json>] [--default-api-key <key>] [--workspace-base-dir <dir>] [--clone-to-canonical] [--dry-run]` | Idempotently adopt pre-`v0.8.0` legacy `/workspace` PVCs and local workspace directories into `/volumes` (`type: "block"`). Defaults to zero-copy in-place adoption; pass `--clone-to-canonical` to clone via CSI `VolumeSnapshot` into `vol-<id>` PVCs while keeping originals intact. |

### CLI Workflow Example

```bash
export CUBEPOD_GATEWAY_URL="http://localhost:8080"

# Create a sandbox with a deterministic workspace key
SANDBOX_ID=$(cubepod --json sandbox create --template cubeplex-ubuntu24-desktop --volume-key ws-demo | jq -r '.sandboxID')

# Sync local skills directory and run an agent script
cubepod sandbox archive-sync "$SANDBOX_ID" /workspace/skills ./skills
cubepod sandbox exec "$SANDBOX_ID" "ls -la /workspace/skills && python3 --version"

# Reset OS packages without touching /workspace
cubepod sandbox reset-rootfs "$SANDBOX_ID"

# Fork into an independent sandbox
FORK_ID=$(cubepod --json sandbox fork "$SANDBOX_ID" | jq -r '.sandboxID')
```

To expose CubePod directly to MCP-compatible IDEs and desktop assistants, continue to [MCP Server Integration](/sdks/mcp-server/).

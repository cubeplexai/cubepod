---
title: TypeScript SDK
description: Zero-dependency TypeScript and Node.js 20+ SDK (cubepod) with high-level Sandbox and low-level CubePodClient APIs, streaming Code Interpreter, archive sync, and template builds.
---

The official CubePod TypeScript / JavaScript SDK (`cubepod`) is a **zero-dependency** ESM library built exclusively on Web and Node.js 20+ standard APIs (`fetch`, `ReadableStream`, `TextDecoder`, `Headers`, `URLSearchParams`, and `CompressionStream`). It ships complete `.d.ts` type declarations and runs on Node.js 20+, Deno, Bun, and modern edge runtimes.

- **Package**: `cubepod` (`0.4.1`, Apache-2.0)
- **Module format**: Native ESM (`"type": "module"`)
- **Engines**: `node >= 20.0.0`

## Installation

```bash
npm install cubepod
```

## Two Ways to Use the SDK

1. **`Sandbox` (Object-Oriented)**: High-level handle with `.commands`, `.code`, and `.files` namespaces—similar to E2B's `Sandbox` ergonomics.
2. **`CubePodClient` (Stateless HTTP Client)**: Direct client covering all sandbox, command, code interpreter, filesystem, template catalog/build, and WarmPool endpoints.

```ts
import { CubePodClient, Sandbox } from 'cubepod';

const sandbox = await Sandbox.create({
  baseUrl: 'http://localhost:8080',
  apiKey: process.env.CUBEPOD_API_KEY,
  templateId: 'cubeplex-ubuntu24-desktop',
  volumeKey: 'workspace-user-42',
});

const result = await sandbox.commands.run('ls -la /workspace');
console.log(result.stdout);

await sandbox.kill();
```

### Configuration & Environment Fallbacks

Both `new CubePodClient(options)` and `Sandbox.create(options)` accept `CubePodClientOptions`:

| Option | Default / Env Fallback | Description |
| :--- | :--- | :--- |
| `baseUrl` | `CUBEPOD_BASE_URL` → `CUBEPOD_API_URL` → `'http://localhost:8080'` | Gateway base URL. |
| `apiKey` | `CUBEPOD_API_KEY` → `E2B_API_KEY` → `undefined` | Bearer token for `Authorization: Bearer ...`. |
| `timeoutMs` | `60_000` | Per-request timeout in milliseconds (can also pass `AbortSignal` via `options.signal`). |
| `maxRetries` | `2` | Retries on transient `502`, `503`, `504` (skips retry on `503` terminal `readyCmd` probe timeout). |
| `retryDelayMs` | `10` | Initial exponential backoff delay in milliseconds. |
| `fetch` | `globalThis.fetch` | Custom `fetch` implementation for proxies or unit testing. |
| `headers` | `{}` | Default headers appended to every request. |

## Sandbox Lifecycle, CoW Fork & RootFS Reset

```ts
import { Sandbox } from 'cubepod';

const sandbox = await Sandbox.create('ubuntu:24.04', {
  baseUrl: 'http://localhost:8080',
  timeout: 1800,
  autoPause: true,
  autoResume: true,
  rootfsSize: '10Gi',
  workspaceSize: '20Gi',
});

// Suspend compute (0 CPU/RAM) while retaining RootFS Upper + Workspace volumes
await sandbox.pause();

// Auto-resume from Paused state
const resumed = await Sandbox.connect(sandbox.sandboxId, {
  baseUrl: 'http://localhost:8080',
});

// Renew idle timeout & protect against idle auto-pause with an exclusive lease
await resumed.setTimeout(3600);
await resumed.acquireLease(600);

// Refresh sandbox snapshot & metadata from the Gateway
await resumed.refresh();

// Wipe RootFS Upper changes while keeping /workspace intact
await resumed.resetRootfs();

// ~1s Copy-on-Write clone of both RootFS and Workspace volumes
const child = await resumed.fork({
  volumeKey: 'workspace-user-42-exp1',
  metadata: { branch: 'experiment-1' },
});

// Permanently delete child sandbox and its PVCs
await child.kill({ deleteVolumes: true });
```

`CubePodClient` also exposes `refreshSandbox(sandboxId, durationSeconds)` (`POST /sandboxes/{id}/refreshes`), `listSandboxes({ state, templateId })`, and `health()`.

## Endpoints & Signed Panel URLs

```ts
// E2B getHost compatibility (returns "<port>-<sandboxID>.<domain>")
const host = await sandbox.getHost(8080);
const endpointInfo = await sandbox.getEndpointHost(8080);
console.log(endpointInfo.url);

// Mint an HMAC-SHA256 signed /panel/{token}/ URL for web, noVNC, ttyd, or WebRTC views
const panel = await sandbox.createEndpoint(8080, { expiresInSeconds: 3600 });
console.log(panel.url, panel.expiresAt);
```

## Commands & Files (UID/GID Preservation)

:::note[Default User Preservation]
Unlike clients that force `uid=1000` on every call, the TypeScript SDK **omits `uid` and `gid` by default** in `commands.run` and `files.write` so the template's configured `USER` is preserved on the server. Pass `uid` / `gid` explicitly (including `uid: 0, gid: 0` for `root`) when overriding ownership.
:::

```ts
// Foreground command (uses template USER by default)
const res = await sandbox.commands.run('id');

// Background command + stateless cursor-based log polling
const bg = await sandbox.commands.run('python3 -u worker.py', {
  background: true,
  workdir: '/workspace',
  envs: { PYTHONUNBUFFERED: '1' },
});
const status = await sandbox.commands.getStatus(bg.id);
const logs = await sandbox.commands.getLogs(bg.id, { cursor: 0 });
await sandbox.commands.kill(bg.id);

// File write, read, list, and delete
await sandbox.files.write('/workspace/app.py', 'print("hello")\n');
await sandbox.files.write('/etc/app.conf', new Uint8Array([1, 2, 3]), {
  uid: 0,
  gid: 0,
  mode: '0600',
});
const text = await sandbox.files.readText('/workspace/app.py');
const bytes = await sandbox.files.readBytes('/workspace/app.py');
const entries = await sandbox.files.list('/workspace');
await sandbox.files.remove('/workspace/app.py');

// Atomic archive-sync (pass a file map or prebuilt tar.gz Uint8Array/ArrayBuffer)
await sandbox.files.archiveSync(
  { 'my-skill/SKILL.md': '# My Skill\n' },
  { dest: '/workspace/skills', removePaths: ['deprecated-skill'] },
);
```

The SDK also exports low-level archive helpers `buildTarBytes(files, options?)`, `gzipBytes(input)`, and `buildTarGzBytes(files, options?)`.

## Code Interpreter (Streaming NDJSON & Contexts)

`sandbox.code` streams NDJSON chunks from the in-guest interpreter and invokes callbacks as output arrives—supporting rich `text`, base64 `png`, and `html` results alongside stateful multi-turn contexts (`python`, `bash`, `javascript`, `typescript`, `r`, `java`).

```ts
const ctx = await sandbox.code.createContext({
  language: 'python',
  cwd: '/workspace',
});

const execution = await sandbox.code.runCode(
  'import math\nprint("pi =", math.pi)\nmath.sqrt(16)',
  {
    contextId: ctx.id,
    onStdout: (msg) => process.stdout.write(msg.text),
    onStderr: (msg) => process.stderr.write(msg.text),
    onResult: (res) => console.log('Result:', res.text, res.png ? '[png]' : ''),
    onError: (err) => console.error(err.name, err.value, err.traceback),
  },
);

console.log(execution.text); // "4.0"

const contexts = await sandbox.code.listContexts();
await sandbox.code.restartContext(ctx.id);
await sandbox.code.deleteContext(ctx.id);
```

For custom streaming parsers, `readNdjsonStream<T>(stream, onItem)` is exported directly from `cubepod`.

## Zero-Trust Egress & `secretBindings`

Pass `networkPolicy` and `secretBindings` when provisioning a sandbox to keep raw credentials outside the MicroVM:

```ts
const sandbox = await Sandbox.create('cubeplex-ubuntu24-desktop', {
  networkPolicy: {
    defaultAction: 'Deny',
    allowHosts: ['api.openai.com'],
  },
  secretBindings: [
    {
      envName: 'OPENAI_API_KEY',
      rawSecret: process.env.REAL_OPENAI_KEY,
      allowedHosts: ['api.openai.com'],
      allowedHeaders: ['Authorization'],
    },
  ],
});
```

## Template Catalog, Builds & WarmPools

`CubePodClient` covers the full E2B-compatible v1/v2/v3 template lifecycle as well as WarmPools:

```ts
const client = new CubePodClient({ baseUrl: 'http://localhost:8080' });

// List templates (v1 and paginated v2 via X-Next-Token)
const templates = await client.listTemplates();
const page = await client.listTemplatesV2({ limit: 50 });

// Inspect by ID or alias (automatically URI-encoded for org/name:v1)
const tpl = await client.getTemplate('org/name:v1');
const byAlias = await client.getTemplateByAlias('base');

// Create and start a template build (v3/v2)
const req = await client.createTemplate({
  name: 'python-data:v1',
  cpuCount: 2,
  memoryMB: 2048,
});

await client.startTemplateBuild(req.templateID, req.buildID, {
  fromImage: 'python:3.13',
  steps: [{ type: 'RUN', args: ['pip install pandas numpy'] }],
});

const buildStatus = await client.getTemplateBuildStatus(req.templateID, req.buildID);
if (buildStatus.status === 'error') {
  console.error(buildStatus.reason?.step, buildStatus.reason?.message);
}

await client.assignTemplateTags(`${req.templateID}:default`, ['prod', 'latest']);
await client.updateTemplateVisibility(req.templateID, true);
await client.deleteTemplate(req.templateID, { force: false });

// WarmPools
await client.createWarmPool({
  name: 'python-pool',
  templateRef: 'python-data:v1',
  replicas: 2,
});
const pools = await client.listWarmPools();
```

See [Template Catalog & Builds](/sdks/templates/) for context upload (`getTemplateFileUploadLink`, `uploadTemplateFileContext`) and Dockerfile step rules.

## Error Handling

All SDK errors extend `CubePodError`. HTTP `>= 400` responses throw `CubePodAPIError` with structured diagnostic properties:

```ts
import { CubePodAPIError, CubePodError } from 'cubepod';

try {
  await client.getTemplate('non-existent');
} catch (err) {
  if (err instanceof CubePodAPIError) {
    console.error({
      statusCode: err.statusCode,
      code: err.code,
      sandboxId: err.sandboxId,
      logPath: err.logPath,
      step: err.step,
      details: err.details,
      message: err.message,
    });
  } else if (err instanceof CubePodError) {
    console.error(err.message);
  }
}
```

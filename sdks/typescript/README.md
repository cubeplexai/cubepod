# `@cubeplex/cubepod`

Official TypeScript / JavaScript SDK for **CubePod** — persistent, E2B-compatible MicroVM agent sandboxes with dual-volume RootFS + `/workspace` isolation, 1-second Copy-on-Write forks, streaming Code Interpreter, zero-trust egress, and template builds.

- **Zero runtime dependencies** — uses native `fetch`, `ReadableStream`, `TextDecoder`, `Headers`, `URLSearchParams`, and `CompressionStream` (Node.js 20+, Deno, Bun, and modern edge runtimes).
- **Full TypeScript declarations** bundled in `src/*.d.ts`.

---

## Installation

```bash
npm install @cubeplex/cubepod
```

## Quick Start

```ts
import { CubePodClient, Sandbox } from '@cubeplex/cubepod';

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

---

## Sandbox Lifecycle, CoW Fork & RootFS Reset

```ts
import { Sandbox } from '@cubeplex/cubepod';

const sandbox = await Sandbox.create('ubuntu:24.04', {
  baseUrl: 'http://localhost:8080',
  timeout: 1800,
});

// Pause compute (0 CPU/RAM) while retaining RootFS + Workspace volumes
await sandbox.pause();

// Auto-resume from Paused state
const resumed = await Sandbox.connect(sandbox.sandboxId, {
  baseUrl: 'http://localhost:8080',
});

// Keepalive timeout & idle-pause lease protection
await resumed.setTimeout(3600);
await resumed.acquireLease(600);

// Reset RootFS Upper to a clean base image while keeping /workspace intact
await resumed.resetRootfs();

// Clone both RootFS and Workspace volumes via ~1s Copy-on-Write
const child = await resumed.fork({ metadata: { branch: 'experiment-1' } });
await child.kill({ deleteVolumes: true });
```

## Endpoints & Signed Panel URLs

```ts
// E2B getHost compatibility
const host = await sandbox.getHost(8080);
console.log(`https://${host}`);

// Mint an HMAC-SHA256 signed /panel/{token}/ URL for web/noVNC/WebRTC views
const panel = await sandbox.createEndpoint(8080, { expiresInSeconds: 3600 });
console.log(panel.url);
```

## Commands & Files

`uid` and `gid` are omitted by default so the template's configured `USER` is preserved on the server. Pass `uid` / `gid` explicitly (including `uid: 0` for `root`) when overriding ownership.

```ts
// Foreground command (uses template USER by default)
const res = await sandbox.commands.run('id');

// Background command + cursor-based log polling
const bg = await sandbox.commands.run('python3 -u worker.py', {
  background: true,
  workdir: '/workspace',
  envs: { PYTHONUNBUFFERED: '1' },
});
const status = await sandbox.commands.getStatus(bg.id);
const logs = await sandbox.commands.getLogs(bg.id, { cursor: 0 });
await sandbox.commands.kill(bg.id);

// File read / write / list / remove
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

// Atomic archive-sync (pass tar.gz Uint8Array or a file map)
await sandbox.files.archiveSync(
  { 'my-skill/SKILL.md': '# My Skill\n' },
  { dest: '/workspace/skills', removePaths: ['deprecated-skill'] },
);
```

## Code Interpreter (Streaming NDJSON & Contexts)

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
    onError: (err) => console.error(err.name, err.value),
  },
);

console.log(execution.text); // "4.0"

await sandbox.code.restartContext(ctx.id);
await sandbox.code.deleteContext(ctx.id);
```

## Template Catalog, Builds & WarmPools

```ts
const client = new CubePodClient({ baseUrl: 'http://localhost:8080' });

// List templates (v1 and paginated v2 with X-Next-Token)
const templates = await client.listTemplates();
const page = await client.listTemplatesV2({ limit: 50 });

// Inspect by ID or alias (automatically URI-encoded for org/name:v1)
const tpl = await client.getTemplate('org/name:v1');

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

await client.updateTemplateVisibility(req.templateID, true);
await client.deleteTemplate(req.templateID);

// WarmPools
await client.createWarmPool({
  name: 'python-pool',
  templateRef: 'python-data:v1',
  replicas: 2,
});
const pools = await client.listWarmPools();
```

## Error Handling

```ts
import { CubePodAPIError, CubePodError } from '@cubeplex/cubepod';

try {
  await client.getTemplate('non-existent');
} catch (err) {
  if (err instanceof CubePodAPIError) {
    console.error(err.statusCode, err.code, err.sandboxId, err.message);
  } else if (err instanceof CubePodError) {
    console.error(err.message);
  }
}
```

## Running Tests

```bash
cd sdks/typescript
npm test
```

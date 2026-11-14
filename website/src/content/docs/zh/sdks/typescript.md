---
title: TypeScript SDK
description: 零依赖 TypeScript 与 Node.js 20+ SDK（cubepod），提供面向对象 Sandbox 与底层 CubePodClient API、流式 Code Interpreter、archiveSync 目录同步与模板构建。
---

CubePod 官方 TypeScript / JavaScript SDK（`cubepod`）是一款**零运行时依赖**的纯 ESM 库，完全基于 Web 与 Node.js 20+ 标准 API（`fetch`、`ReadableStream`、`TextDecoder`、`Headers`、`URLSearchParams` 与 `CompressionStream`）实现，自带完整 `.d.ts` 类型定义，适用于 Node.js 20+、Deno、Bun 与现代边缘运行时。

- **包名**：`cubepod`（`0.4.0`，Apache-2.0）
- **模块格式**：原生 ESM（`"type": "module"`）
- **运行环境**：`node >= 20.0.0`

## 安装

```bash
npm install cubepod
```

## 两种使用方式

1. **`Sandbox`（面向对象句柄）**：提供 `.commands`、`.code` 与 `.files` 子命名空间，交互体验与 E2B `Sandbox` 保持高度一致。
2. **`CubePodClient`（无状态 HTTP 客户端）**：直接覆盖沙箱、命令、Code Interpreter、文件系统、模板目录/构建及 WarmPool 全部端点。

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

### 配置项与环境变量回退链

`new CubePodClient(options)` 与 `Sandbox.create(options)` 均接收 `CubePodClientOptions`：

| 选项 | 默认值与环境变量回退链 | 说明 |
| :--- | :--- | :--- |
| `baseUrl` | `CUBEPOD_BASE_URL` → `CUBEPOD_API_URL` → `'http://localhost:8080'` | Gateway 服务基址。 |
| `apiKey` | `CUBEPOD_API_KEY` → `E2B_API_KEY` → `undefined` | 作为 `Authorization: Bearer ...` 发送的令牌。 |
| `timeoutMs` | `60_000` | 单次请求超时毫秒数（也可通过 `options.signal` 传入 `AbortSignal`）。 |
| `maxRetries` | `2` | 遇到瞬时 `502`、`503`、`504` 时的最大重试次数（若 `503` 为终止状态 `readyCmd` 探活超时则不会重试）。 |
| `retryDelayMs` | `10` | 指数退避初始延迟毫秒数。 |
| `fetch` | `globalThis.fetch` | 用于代理或单元测试的自定义 `fetch` 实现。 |
| `headers` | `{}` | 每次请求默认附加的 HTTP 头。 |

## 沙箱生命周期、CoW Fork 与 RootFS 重置

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

// 挂起计算资源（0 CPU/内存占用），同时保留 RootFS Upper 与 Workspace 卷
await sandbox.pause();

// 从 Paused 状态自动唤醒并连接
const resumed = await Sandbox.connect(sandbox.sandboxId, {
  baseUrl: 'http://localhost:8080',
});

// 续期空闲超时，并通过独占租约防止空闲自动暂停
await resumed.setTimeout(3600);
await resumed.acquireLease(600);

// 从 Gateway 刷新当前沙箱状态与元数据快照
await resumed.refresh();

// 清空 RootFS Upper 层修改，同时完好保留 /workspace
await resumed.resetRootfs();

// 对 RootFS 与 Workspace 双卷执行约 1 秒的写时复制（CoW）克隆
const child = await resumed.fork({
  volumeKey: 'workspace-user-42-exp1',
  metadata: { branch: 'experiment-1' },
});

// 彻底删除子沙箱及其持久化 PVC
await child.kill({ deleteVolumes: true });
```

此外，`CubePodClient` 还提供 `refreshSandbox(sandboxId, durationSeconds)`（`POST /sandboxes/{id}/refreshes`）、`listSandboxes({ state, templateId })` 与 `health()`。

## 端点域名与签名 Panel URL

```ts
// E2B getHost 兼容方法（返回 "<port>-<sandboxID>.<domain>"）
const host = await sandbox.getHost(8080);
const endpointInfo = await sandbox.getEndpointHost(8080);
console.log(endpointInfo.url);

// 为 Web 服务、noVNC、ttyd 或 WebRTC 画面签发 HMAC-SHA256 /panel/{token}/ URL
const panel = await sandbox.createEndpoint(8080, { expiresInSeconds: 3600 });
console.log(panel.url, panel.expiresAt);
```

## 命令执行与文件操作（保留默认 UID/GID）

:::note[默认用户保留策略]
不同于在每次调用中强制指定 `uid=1000` 的客户端，TypeScript SDK 在 `commands.run` 和 `files.write` 中**默认省略 `uid` 与 `gid`**，以便服务端遵循模板本身配置的 `USER`。需要覆盖归属时，可显式传入 `uid` / `gid`（例如 `uid: 0, gid: 0` 代表 `root`）。
:::

```ts
// 前台执行（默认遵循模板 USER）
const res = await sandbox.commands.run('id');

// 后台执行 + 无状态游标日志轮询
const bg = await sandbox.commands.run('python3 -u worker.py', {
  background: true,
  workdir: '/workspace',
  envs: { PYTHONUNBUFFERED: '1' },
});
const status = await sandbox.commands.getStatus(bg.id);
const logs = await sandbox.commands.getLogs(bg.id, { cursor: 0 });
await sandbox.commands.kill(bg.id);

// 文件写入、读取、列举与删除
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

// 原子级 archive-sync（支持文件映射对象或预构建 tar.gz Uint8Array/ArrayBuffer）
await sandbox.files.archiveSync(
  { 'my-skill/SKILL.md': '# My Skill\n' },
  { dest: '/workspace/skills', removePaths: ['deprecated-skill'] },
);
```

SDK 同时导出了底层归档工具函数 `buildTarBytes(files, options?)`、`gzipBytes(input)` 与 `buildTarGzBytes(files, options?)`。

## Code Interpreter（流式 NDJSON 与多语言上下文）

`sandbox.code` 会从沙箱内解释器流式读取 NDJSON 块并实时触发回调，支持 `text`、Base64 `png` 与 `html` 富输出，以及持久化多轮语言上下文（`python`、`bash`、`javascript`、`typescript`、`r`、`java`）：

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

如需编写自定义流式解析器，SDK 还直接导出了 `readNdjsonStream<T>(stream, onItem)`。

## Zero-Trust 出站防火墙与 `secretBindings`

创建沙箱时传入 `networkPolicy` 与 `secretBindings`，即可让真实凭证永远停留在 MicroVM 外部：

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

## 模板目录、构建与 WarmPool

`CubePodClient` 完整覆盖 E2B v1/v2/v3 模板生命周期及 WarmPool API：

```ts
const client = new CubePodClient({ baseUrl: 'http://localhost:8080' });

// 查询模板（v1 与基于 X-Next-Token 分页的 v2）
const templates = await client.listTemplates();
const page = await client.listTemplatesV2({ limit: 50 });

// 按 ID 或别名查询（会自动对 org/name:v1 执行 URI 编码）
const tpl = await client.getTemplate('org/name:v1');
const byAlias = await client.getTemplateByAlias('base');

// 创建并启动模板构建（v3/v2）
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

上下文上传（`getTemplateFileUploadLink`、`uploadTemplateFileContext`）与 Dockerfile 指令校验规则请参阅[模板目录与构建](/zh/sdks/templates/)。

## 错误处理

所有 SDK 异常均继承自 `CubePodError`。当 HTTP 状态码 `>= 400` 时，客户端会抛出携带结构化诊断字段的 `CubePodAPIError`：

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

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const websiteDir = path.resolve(__dirname, '..');
const repoRoot = path.resolve(websiteDir, '..');

function ensureDir(dir) {
  fs.mkdirSync(dir, { recursive: true });
}

function escapeMdCell(val) {
  if (val === undefined || val === null || val === '') return '—';
  return String(val).replace(/\|/g, '\\|').replace(/\r?\n/g, ' ').trim();
}

// 1. Copy OpenAPI 3.1 spec
function syncOpenAPI() {
  const src = path.join(repoRoot, 'api', 'openapi', 'cubepod-v1.yaml');
  const dest = path.join(websiteDir, 'public', 'openapi.yaml');
  ensureDir(path.dirname(dest));
  fs.copyFileSync(src, dest);
  console.log('[sync] copied api/openapi/cubepod-v1.yaml -> public/openapi.yaml');
}

// 2. Copy @scalar/api-reference standalone browser bundle locally (zero external CDN dependency)
function syncScalarBundle() {
  const pkgRoot = path.join(websiteDir, 'node_modules', '@scalar', 'api-reference');
  const pkgJsonPath = path.join(pkgRoot, 'package.json');
  const destDir = path.join(websiteDir, 'public', 'vendor');
  ensureDir(destDir);

  let candidates = [
    path.join(pkgRoot, 'dist', 'browser', 'standalone.js'),
    path.join(pkgRoot, 'dist', 'browser', 'standalone.min.js'),
  ];

  if (fs.existsSync(pkgJsonPath)) {
    const pkg = JSON.parse(fs.readFileSync(pkgJsonPath, 'utf8'));
    if (pkg.exports && typeof pkg.exports === 'object') {
      for (const [key, val] of Object.entries(pkg.exports)) {
        if (key.includes('standalone') || JSON.stringify(val).includes('standalone')) {
          const target = typeof val === 'string' ? val : val?.default || val?.browser || val?.import;
          if (target) candidates.unshift(path.resolve(pkgRoot, target));
        }
      }
    }
    if (pkg.browser && typeof pkg.browser === 'string') {
      candidates.push(path.resolve(pkgRoot, pkg.browser));
    }
  }

  const found = candidates.find((p) => fs.existsSync(p));
  if (!found) {
    throw new Error(
      `Could not find @scalar/api-reference standalone bundle. Checked:\n${candidates.join('\n')}`
    );
  }
  const dest = path.join(destDir, 'scalar-api-reference.js');
  fs.copyFileSync(found, dest);
  console.log(`[sync] copied ${path.relative(websiteDir, found)} -> public/vendor/scalar-api-reference.js`);
}

// 3. Parse CRDs in deploy/helm/cubepod/crds/*.yaml and emit reference/crds.md (EN + ZH)
const CRD_FIELD_DESCRIPTIONS = {
  // PodBox
  'spec.volumeMounts': 'Persistent RWX volume mounts attached to the sandbox container.',
  'spec.volumeMounts[].volumeID': '32-character hexadecimal volume identifier.',
  'spec.volumeMounts[].pvcName': 'Shared ReadWriteMany PVC name hosting persistent volumes.',
  'spec.volumeMounts[].subPath': 'Subdirectory path inside the shared PVC for this volume.',
  'spec.volumeMounts[].path': 'Absolute mount path inside the sandbox container.',
  'spec.templateRef': 'Name of the `PodTemplate` defining runtime class, base OCI image, storage, and egress defaults.',
  'spec.warmPoolRef': 'Optional `PodWarmPool` name to claim a pre-attached standby MicroVM in `< 50ms`.',
  'spec.desiredState': 'Target sandbox compute state (`Running` or `Paused`). Setting `Paused` deletes the Pod (0 CPU/RAM) while retaining PVCs.',
  'spec.rootfsGeneration': 'Incremented monotonically to wipe and re-image the persistent RootFS `upperdir` PVC without touching `/workspace`.',
  'spec.forkFrom': 'Copy-on-Write parent source to clone both RootFS and Workspace block volumes from.',
  'spec.forkFrom.podBoxName': 'Name of the source `PodBox` whose dual volumes are cloned via CSI VolumeSnapshot.',
  'spec.lifecycle': 'Idle auto-pause, paused TTL expiration, and lease lock controls.',
  'spec.lifecycle.idleTimeoutSeconds': 'Seconds of inactivity before `cubepod-controller` transitions the sandbox to `Paused`.',
  'spec.lifecycle.pausedTTLSeconds': 'Retention window (seconds) for PVCs while `Paused` before garbage collection.',
  'spec.lifecycle.leaseExpiresAt': 'RFC3339 timestamp until which an active agent lease prevents idle auto-pause.',
  'spec.networkPolicy': 'Zero-Trust outbound firewall rules and `cpodref_` credential bindings.',
  'spec.networkPolicy.defaultAction': 'Default egress posture when a destination host matches neither `allowHosts` nor `denyHosts` (`Allow` or `Deny`).',
  'spec.networkPolicy.allowHosts': 'List of permitted destination hostnames or wildcard patterns (e.g., `*.github.com`).',
  'spec.networkPolicy.denyHosts': 'List of blocked destination hostnames or wildcard patterns.',
  'spec.networkPolicy.upstreamProxy': 'Optional upstream HTTP/HTTPS proxy URL chained behind `cubepod-egress`.',
  'spec.networkPolicy.secretBindings': 'Array of `cpodref_` placeholder bindings swapped by `cubepod-egress` over per-pod mTLS.',
  'spec.networkPolicy.secretBindings[].envName': 'Environment variable name injected inside the MicroVM (holding only the opaque placeholder).',
  'spec.networkPolicy.secretBindings[].placeholder': 'Opaque `cpodref_...` token exposed to guest processes instead of the real credential.',
  'spec.networkPolicy.secretBindings[].allowedHosts': 'Destination hostnames permitted to receive the substituted real credential.',
  'spec.networkPolicy.secretBindings[].allowedHeaders': 'Optional HTTP headers allowed for substitution (e.g., `Authorization`, `x-api-key`).',
  'spec.networkPolicy.headerTransforms': 'Host-scoped HTTP header transformations applied by `cubepod-egress`.',
  'spec.networkPolicy.secretRefBindings': 'Tenant-scoped zero-trust secret reference bindings resolved from vault over per-pod mTLS.',
  'status.phase': 'Observed `PodBox` phase (`Pending`, `Provisioning`, `Running`, `Pausing`, `Paused`, `Resuming`, `ResettingRootFS`, `Failed`, `Terminated`).',
  'status.podName': 'Name of the currently bound Kubernetes Kata Pod (empty when `Paused`).',
  'status.podIP': 'Cluster IP of the active sandbox Pod used by `cubepod-gateway` to proxy `podd` (`:49983`) and panel ports.',
  'status.nodeName': 'Kubernetes node currently hosting the sandbox Pod.',
  'status.rootfsPVCName': 'Bound PersistentVolumeClaim name for the OverlayFS `upperdir` block/filesystem volume.',
  'status.workspacePVCName': 'Bound PersistentVolumeClaim name for `/workspace` (`1000:1000`).',
  'status.observedRootfsGeneration': 'Latest `rootfsGeneration` reconciled by the controller.',
  'status.lastActivityAt': 'Timestamp of last command, file operation, or panel traffic observed by gateway/controller.',
  'status.pausedAt': 'Timestamp when the sandbox entered `Paused` phase.',
  'status.reason': 'Machine-readable reason code for phase transitions or failures.',
  'status.message': 'Human-readable status message.',
  // PodTemplate
  'spec.runtimeClassName': 'Kubernetes `RuntimeClass` for hardware isolation (e.g., `kata-qemu` or `kata-clh`).',
  'spec.image': 'User/workload OCI container image (kept clean; `podd-init` and `podd` are injected via initContainer).',
  'spec.poddImage': 'Override injector image containing `/opt/cubepod/bin/{podd-init,podd}`.',
  'spec.resources': 'CPU and memory requests/limits for the sandbox MicroVM.',
  'spec.resources.cpuRequest': 'Kubernetes CPU request (e.g., `500m`).',
  'spec.resources.cpuLimit': 'Kubernetes CPU limit (e.g., `2`).',
  'spec.resources.memoryRequest': 'Kubernetes memory request (e.g., `1Gi`).',
  'spec.resources.memoryLimit': 'Kubernetes memory limit (e.g., `4Gi`).',
  'spec.storage': 'CSI storage defaults for the dual `rootfs` and `workspace` PVCs.',
  'spec.storage.storageClassName': 'CSI StorageClass name supporting `volumeMode: Block` (or `Filesystem`).',
  'spec.storage.rootfsSize': 'Size of the RootFS `upperdir` PVC (e.g., `10Gi`).',
  'spec.storage.workspaceSize': 'Size of the `/workspace` PVC (e.g., `20Gi`).',
  'spec.egress': 'Per-template zero-trust egress sidecar configuration.',
  'spec.egress.enabled': 'Whether `cubepod-egress` sidecar and `nftables` interception are injected.',
  'spec.egress.image': 'Container image for `cubepod-egress`.',
  'spec.egress.exchangeUrl': 'Control-plane vault URL queried over per-pod mTLS to exchange `cpodref_` tokens.',
  'spec.names': 'E2B-compatible template aliases and version tags (e.g., `ubuntu:24.04`, `python-data:v1`).',
  'spec.cpuCount': 'E2B integer vCPU count view synced with `resources`.',
  'spec.memoryMB': 'E2B memory size in MiB synced with `resources`.',
  'spec.diskSizeMB': 'E2B disk size in MiB synced with `storage.rootfsSize`.',
  'spec.public': 'Whether the template is visible publicly in the E2B template catalog.',
  'spec.startCmd': 'Optional command launched by `podd` after `pivot_root` completes.',
  'spec.readyCmd': 'Readiness probe command run during template build verification and sandbox startup.',
  'spec.runAsUser': 'Numeric UID from OCI `USER` instruction (`nil` defaults to `1000`, `0` preserves root).',
  'spec.runAsGroup': 'Numeric GID when OCI `USER` specifies `<uid>:<gid>`.',
  'spec.runAsUsername': 'Named user resolved inside the container image when `USER` is non-numeric.',
  'status.templateID': 'Canonical template identifier returned in E2B Template API responses.',
  'status.readyBuildID': 'Build ID of the latest successful BuildKit template build.',
  'status.readyImage': 'Built OCI image reference currently served by this template.',
  'status.latestBuildID': 'Most recent BuildKit build ID initiated for this template.',
  'status.buildStatus': 'Latest build status (`waiting`, `building`, `uploaded`, `ready`, `error`).',
  'status.failedStep': 'Dockerfile/build step that failed if `buildStatus == error`.',
  'status.buildCount': 'Total build attempts tracked on this template.',
  'status.spawnCount': 'Total sandboxes spawned from this template.',
  'status.createdAt': 'RFC3339 creation timestamp.',
  'status.updatedAt': 'RFC3339 last update timestamp.',
  'status.lastSpawnedAt': 'RFC3339 timestamp of the last sandbox creation.',
  // PodWarmPool
  'spec.replicas': 'Target count of pre-formatted, pre-pivoted standby MicroVM Pods kept ready to claim.',
  'status.readyReplicas': 'Number of standby Pods that passed `podd` readiness checks.',
  'status.availableReplicas': 'Number of standby Pods immediately available for `< 50ms` sandbox claiming.',
  // SandboxWebhook
  'spec.tenantHash': 'SHA-256 tenant hash scoping the webhook subscription.',
  'spec.name': 'Human-readable webhook subscription name.',
  'spec.url': 'HTTPS (or private HTTP when allowed) destination URL receiving signed lifecycle events.',
  'spec.events': 'Subscribed sandbox lifecycle event types.',
  'spec.enabled': 'Whether webhook delivery is active.',
  'spec.secretName': 'Kubernetes Secret name holding the HMAC signing secret.',
  'status.lastDeliveryTime': 'Timestamp of the most recent webhook delivery attempt.',
  'status.consecutiveFailures': 'Number of consecutive failed webhook deliveries.',
};

function flattenSchema(schema, prefix, requiredSet, rows) {
  if (!schema || typeof schema !== 'object') return;
  const props = schema.properties || {};
  const req = new Set(schema.required || []);

  for (const [key, prop] of Object.entries(props)) {
    const fieldPath = prefix ? `${prefix}.${key}` : key;
    const isRequired = req.has(key) || (requiredSet && requiredSet.has(fieldPath));
    let typeStr = prop.type || 'any';
    if (typeStr === 'array' && prop.items) {
      typeStr = `[]${prop.items.type || 'object'}`;
    }
    if (prop.format) {
      typeStr += ` (${prop.format})`;
    }
    const constraints = [];
    if (Array.isArray(prop.enum)) constraints.push(`enum: ${prop.enum.map((v) => `\`${v}\``).join(', ')}`);
    if (prop.minimum !== undefined) constraints.push(`min: ${prop.minimum}`);
    const desc = prop.description || CRD_FIELD_DESCRIPTIONS[fieldPath] || '—';

    rows.push({
      field: `\`${fieldPath}\``,
      type: `\`${typeStr}\``,
      required: isRequired ? 'Yes' : 'No',
      constraints: constraints.length > 0 ? constraints.join('; ') : '—',
      description: desc,
    });

    if (prop.type === 'object' && prop.properties) {
      flattenSchema(prop, fieldPath, null, rows);
    } else if (prop.type === 'array' && prop.items && prop.items.type === 'object' && prop.items.properties) {
      flattenSchema(prop.items, `${fieldPath}[]`, null, rows);
    }
  }
}

function renderCRDMarkdown(locale) {
  const crdFiles = [
    'cubepod.io_podboxes.yaml',
    'cubepod.io_podtemplates.yaml',
    'cubepod.io_podwarmpools.yaml',
    'cubepod.io_sandboxwebhooks.yaml',
  ];
  const isZh = locale === 'zh';

  const lines = [
    '---',
    `title: ${isZh ? 'Kubernetes CRD 字段字典 (v1alpha1)' : 'Kubernetes CRDs (v1alpha1)'}`,
    `description: ${
      isZh
        ? '自动同步自 deploy/helm/cubepod/crds/ 的 PodBox、PodTemplate、PodWarmPool 与 SandboxWebhook 完整字段字典。'
        : 'Auto-generated field reference for PodBox, PodTemplate, PodWarmPool, and SandboxWebhook from deploy/helm/cubepod/crds/.'
    }`,
    'editUrl: false',
    '---',
    '',
    isZh
      ? '> 本文档在站点构建期直接解析仓库内 [`deploy/helm/cubepod/crds/`](https://github.com/cubeplexai/cubepod/tree/main/deploy/helm/cubepod/crds) 与 [`api/v1alpha1/types.go`](https://github.com/cubeplexai/cubepod/blob/main/api/v1alpha1/types.go) 自动生成，确保与真实集群 Schema 完全一致。'
      : '> Auto-generated at build time from [`deploy/helm/cubepod/crds/`](https://github.com/cubeplexai/cubepod/tree/main/deploy/helm/cubepod/crds) and [`api/v1alpha1/types.go`](https://github.com/cubeplexai/cubepod/blob/main/api/v1alpha1/types.go).',
    '',
  ];

  for (const file of crdFiles) {
    const raw = fs.readFileSync(path.join(repoRoot, 'deploy', 'helm', 'cubepod', 'crds', file), 'utf8');
    const crd = YAML.parse(raw);
    const ver = crd.spec.versions[0];
    const kind = crd.spec.names.kind;
    const group = crd.spec.group;
    const shortNames = (crd.spec.names.shortNames || []).map((s) => `\`${s}\``).join(', ');

    lines.push(`## \`${kind}\` (\`${group}/${ver.name}\`)`);
    lines.push('');
    lines.push(
      isZh
        ? `- **Resource**: \`${crd.metadata.name}\` | **Scope**: \`${crd.spec.scope}\` | **Short Names**: ${shortNames || '—'}`
        : `- **Resource**: \`${crd.metadata.name}\` | **Scope**: \`${crd.spec.scope}\` | **Short Names**: ${shortNames || '—'}`
    );
    lines.push('');

    const specSchema = ver.schema?.openAPIV3Schema?.properties?.spec;
    const statusSchema = ver.schema?.openAPIV3Schema?.properties?.status;

    if (specSchema) {
      const rows = [];
      flattenSchema(specSchema, 'spec', null, rows);
      lines.push(`### ${kind} \`.spec\``);
      lines.push('');
      lines.push(isZh ? '| 字段路径 | 类型 | 必填 | 约束 | 说明 |' : '| Field | Type | Required | Constraints | Description |');
      lines.push('| :--- | :--- | :--- | :--- | :--- |');
      for (const r of rows) {
        lines.push(`| ${r.field} | ${r.type} | ${r.required} | ${r.constraints} | ${escapeMdCell(r.description)} |`);
      }
      lines.push('');
    }

    if (statusSchema) {
      const rows = [];
      flattenSchema(statusSchema, 'status', null, rows);
      lines.push(`### ${kind} \`.status\``);
      lines.push('');
      lines.push(isZh ? '| 字段路径 | 类型 | 约束 | 说明 |' : '| Field | Type | Constraints | Description |');
      lines.push('| :--- | :--- | :--- | :--- |');
      for (const r of rows) {
        lines.push(`| ${r.field} | ${r.type} | ${r.constraints} | ${escapeMdCell(r.description)} |`);
      }
      lines.push('');
    }

    if (Array.isArray(ver.additionalPrinterColumns) && ver.additionalPrinterColumns.length > 0) {
      lines.push(isZh ? `### \`kubectl get ${crd.spec.names.plural}\` 输出列` : `### \`kubectl get ${crd.spec.names.plural}\` Columns`);
      lines.push('');
      lines.push('| Column | Type | JSONPath |');
      lines.push('| :--- | :--- | :--- |');
      for (const col of ver.additionalPrinterColumns) {
        lines.push(`| \`${col.name}\` | \`${col.type}\` | \`${col.jsonPath}\` |`);
      }
      lines.push('');
    }
  }

  lines.push(isZh ? '## 快速校验命令' : '## Inspecting CRDs on a Cluster');
  lines.push('');
  lines.push('```bash');
  lines.push('kubectl get crds | grep cubepod.io');
  lines.push('kubectl get podboxes,podtemplates,podwarmpools -n cubepod-workloads');
  lines.push('```');
  lines.push('');

  return lines.join('\n');
}

function syncCRDs() {
  const enDest = path.join(websiteDir, 'src', 'content', 'docs', 'reference', 'crds.md');
  const zhDest = path.join(websiteDir, 'src', 'content', 'docs', 'zh', 'reference', 'crds.md');
  ensureDir(path.dirname(enDest));
  ensureDir(path.dirname(zhDest));
  fs.writeFileSync(enDest, renderCRDMarkdown('en'), 'utf8');
  fs.writeFileSync(zhDest, renderCRDMarkdown('zh'), 'utf8');
  console.log('[sync] generated reference/crds.md & zh/reference/crds.md');
}

// 4. Parse deploy/helm/cubepod/values.yaml (preserving inline comments) -> reference/helm-values.md
function extractYamlComments(raw) {
  const doc = YAML.parseDocument(raw);
  const comments = {};
  function walk(node, prefix) {
    if (!node || !node.items) return;
    for (const pair of node.items) {
      const key = String(pair.key?.value ?? pair.key);
      const fullKey = prefix ? `${prefix}.${key}` : key;
      const comment = [pair.key?.commentBefore, pair.value?.comment]
        .filter(Boolean)
        .map((s) => s.trim())
        .join(' ');
      if (comment) comments[fullKey] = comment;
      if (pair.value && Array.isArray(pair.value.items)) {
        walk(pair.value, fullKey);
      }
    }
  }
  walk(doc.contents, '');
  return { parsed: doc.toJS(), comments };
}

function flattenValues(obj, prefix, comments, rows) {
  for (const [k, v] of Object.entries(obj)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v !== null && typeof v === 'object' && !Array.isArray(v) && Object.keys(v).length > 0) {
      flattenValues(v, key, comments, rows);
    } else {
      const displayVal =
        v === ''
          ? '`""`'
          : Array.isArray(v)
          ? `\`${JSON.stringify(v)}\``
          : typeof v === 'object'
          ? `\`${JSON.stringify(v)}\``
          : `\`${String(v)}\``;
      rows.push({
        key: `\`${key}\``,
        type: Array.isArray(v) ? 'array' : v === null ? 'null' : typeof v,
        defaultVal: displayVal,
        desc: comments[key] || '—',
      });
    }
  }
}

const HELM_VALUE_DESCRIPTIONS = {
  'gateway.podDisruptionBudget.enabled': {
    en: '*Added in v0.6.0.* Enable PodDisruptionBudget (`pdb.yaml`) for `cubepod-gateway`. When `null`/omitted, auto-enabled whenever `gateway.replicaCount > 1`.',
    zh: '*v0.6.0 新增。* 是否为 `cubepod-gateway` 创建 PodDisruptionBudget（`pdb.yaml`）。为 `null` 或省略时，当 `gateway.replicaCount > 1` 会自动启用。',
  },
  'gateway.podDisruptionBudget.minAvailable': {
    en: '*Added in v0.6.0.* Minimum available gateway Pods enforced by the PDB when `maxUnavailable` is unset.',
    zh: '*v0.6.0 新增。* 未设置 `maxUnavailable` 时，PDB 要求的最少可用 Gateway Pod 数量。',
  },
  'gateway.secretsKeySecret': {
    en: 'Legacy fallback Kubernetes Secret name (key `key`) for `CUBEPOD_SECRETS_KEY` when `secrets.existingSecret` is unset.',
    zh: '当未配置 `secrets.existingSecret` 时，作为 `CUBEPOD_SECRETS_KEY` 的旧版回退 Secret 名称（键名 `key`）。',
  },
  'gateway.eventsKeySecret': {
    en: 'Optional Kubernetes Secret name (key `key`) holding the 32-byte encryption key (`CUBEPOD_EVENTS_KEY`) for local persisted event/webhook state.',
    zh: '可选 Kubernetes Secret 名称（键名 `key`），用于注入本地事件与 Webhook 持久化所需的 32 字节加密密钥（`CUBEPOD_EVENTS_KEY`）。',
  },
  'egress.enabled': {
    en: 'Inject the `cubepod-egress` sidecar container by default on seeded and fallback templates.',
    zh: '是否在默认与兜底模板中默认注入 `cubepod-egress` 出站边车容器。',
  },
  'egress.transparent': {
    en: '*Changed in v0.6.0.* Enables `CUBEPOD_TRANSPARENT=1` and `NET_ADMIN` `nftables` interception in `cubepod-egress` sidecars (effective `TransparentEgress` requires both `egress.transparent` and `featureGates.transparentEgress` to be `true`).',
    zh: '*v0.6.0 变更。* 是否在 `cubepod-egress` 边车中启用 `CUBEPOD_TRANSPARENT=1` 与 `NET_ADMIN` `nftables` 透明拦截（需同时开启 `egress.transparent` 与 `featureGates.transparentEgress`）。',
  },
  'egress.image.registry': {
    en: 'OCI registry hosting the `cubepod-egress` sidecar image.',
    zh: '`cubepod-egress` 边车镜像所在的 OCI 仓库地址。',
  },
  'egress.image.repository': {
    en: 'Image repository name for `cubepod-egress`.',
    zh: '`cubepod-egress` 边车镜像仓库路径。',
  },
  'egress.image.tag': {
    en: 'Image tag for `cubepod-egress`.',
    zh: '`cubepod-egress` 边车镜像标签。',
  },
  'egress.exchangeUrl': {
    en: 'Default mTLS vault URL queried by `cubepod-egress` to exchange `cpodref_` placeholders.',
    zh: '`cubepod-egress` 通过 mTLS 置换 `cpodref_` 占位符时默认访问的 Vault 地址。',
  },
  'secrets.enabled': {
    en: '*Added in v0.6.0.* Enable the `/secrets` API store in `cubepod-gateway` (combined with `featureGates.e2bSecretsAPI`).',
    zh: '*v0.6.0 新增。* 是否在 `cubepod-gateway` 中启用 `/secrets` 密钥存储（与 `featureGates.e2bSecretsAPI` 共同生效）。',
  },
  'secrets.backend': {
    en: '*Added in v0.6.0.* Secret persistence backend: `"k8s"` (`KubeSecretBackend` for multi-replica HA via Kubernetes Secrets), `"file"`, or `"disabled"`.',
    zh: '*v0.6.0 新增。* 密钥持久化后端：`"k8s"`（基于 Kubernetes Secret 的 `KubeSecretBackend` 多副本高可用）、`"file"` 或 `"disabled"`。',
  },
  'secrets.dir': {
    en: '*Added in v0.6.0.* Filesystem directory (`CUBEPOD_SECRETS_DIR`) used when `secrets.backend` is `"file"`.',
    zh: '*v0.6.0 新增。* 当 `secrets.backend` 为 `"file"` 时使用的本地密钥目录（`CUBEPOD_SECRETS_DIR`）。',
  },
  'secrets.existingSecret': {
    en: '*Added in v0.6.0.* Existing Kubernetes Secret containing a 32-byte base64/64-hex key under `data.key` (overrides `gateway.secretsKeySecret`).',
    zh: '*v0.6.0 新增。* 已存在的 Kubernetes Secret 名称（需在 `data.key` 中存放 32 字节 Base64 或 64 位 Hex 密钥，优先级高于 `gateway.secretsKeySecret`）。',
  },
  'secrets.encryptionKey': {
    en: '*Added in v0.6.0.* Optional inline 32-byte base64/64-hex encryption key; creates `<fullname>-secrets-key` Secret when set without `existingSecret`.',
    zh: '*v0.6.0 新增。* 可选内联 32 字节 Base64/64-Hex 加密密钥；未指定 `existingSecret` 时自动创建 `<fullname>-secrets-key` Secret。',
  },
  'featureGates.e2bTemplateAPI': {
    en: '*Changed in v0.7.0.* Defaults to `true` (GA). Wired into `--feature-gate=E2BTemplateAPI=...` and `--e2b-template-api`; set `false` to roll back `GET /templates` to the legacy `{"templates":[...]}` wrapper.',
    zh: '*v0.7.0 变更。* 默认值升级为 `true`（GA）。同步注入 `--feature-gate=E2BTemplateAPI=...` 与 `--e2b-template-api`；设为 `false` 可将 `GET /templates` 回滚为旧版 `{"templates":[...]}` 包装响应。',
  },
  'templateBuild.contextStore.backend': {
    en: '*Added in v0.7.0.* Context archive and build log backend: `"local"` (single-replica `cubepod-template-context` RWO PVC, forces `controller.replicaCount=1`) or `"s3"` (shared S3-compatible object storage allowing HA controller replicas).',
    zh: '*v0.7.0 新增。* 模板构建上下文与日志存储后端：`"local"`（单副本 `cubepod-template-context` RWO PVC，强制 `controller.replicaCount=1`）或 `"s3"`（共享 S3 兼容对象存储，支持 Controller 多副本高可用）。',
  },
  'templateBuild.contextStore.s3.endpoint': {
    en: '*Added in v0.7.0.* S3-compatible endpoint URL (e.g., `https://minio.cubepod-system.svc:9000` or empty for AWS S3) when `contextStore.backend=s3`.',
    zh: '*v0.7.0 新增。* 当 `contextStore.backend=s3` 时使用的 S3 兼容对象存储端点 URL（例如 MinIO 地址或留空使用 AWS S3）。',
  },
  'templateBuild.contextStore.s3.bucket': {
    en: '*Added in v0.7.0.* S3 bucket storing context tarballs (`<prefix>contexts/<hash>.tar`) and chunked build logs (`<prefix>logs/<buildID>/*.jsonl`).',
    zh: '*v0.7.0 新增。* 用于存放上下文压缩包（`<prefix>contexts/<hash>.tar`）与分片构建日志（`<prefix>logs/<buildID>/*.jsonl`）的 S3 Bucket 名称。',
  },
  'templateBuild.contextStore.s3.region': {
    en: '*Added in v0.7.0.* AWS SigV4 region used when signing requests against S3-compatible object storage.',
    zh: '*v0.7.0 新增。* 对 S3 兼容对象存储请求执行 AWS SigV4 签名时使用的 Region。',
  },
  'templateBuild.contextStore.s3.prefix': {
    en: '*Added in v0.7.0.* Optional object key prefix inside the S3 context store bucket.',
    zh: '*v0.7.0 新增。* S3 Bucket 内的可选对象键前缀。',
  },
  'templateBuild.contextStore.s3.forcePathStyle': {
    en: '*Added in v0.7.0.* Enable path-style S3 requests (`<endpoint>/<bucket>/<key>`) for MinIO, SeaweedFS, or Cloudflare R2.',
    zh: '*v0.7.0 新增。* 是否强制使用路径风格（Path-Style）S3 请求（适用于 MinIO、SeaweedFS 或 Cloudflare R2）。',
  },
  'templateBuild.contextStore.s3.credentialsSecretName': {
    en: '*Added in v0.7.0.* Existing Kubernetes Secret (`accessKeyId`, `secretAccessKey`, optional `sessionToken`) injected as `CUBEPOD_S3_*` env vars on Controller and Gateway Pods.',
    zh: '*v0.7.0 新增。* 已存在的 Kubernetes Secret 名称（需含 `accessKeyId`、`secretAccessKey` 及可选 `sessionToken` 键），以环境变量形式安全注入 `CUBEPOD_S3_*`。',
  },
  'templateBuild.buildkit.replicaCount': {
    en: '*Added in v0.7.0.* Number of `buildkitd` replicas. Setting `> 1` switches `cubepod-buildkit` from a single-replica `Deployment` to a `StatefulSet` with per-replica cache `volumeClaimTemplates` and a headless Service.',
    zh: '*v0.7.0 新增。* `buildkitd` 副本数。设为 `> 1` 时，`cubepod-buildkit` 从单副本 `Deployment` 切换为带每副本 `volumeClaimTemplates` 与 Headless Service 的 `StatefulSet`。',
  },
  'templateBuild.buildkit.tls.enabled': {
    en: '*Added in v0.7.0.* Enable mutual TLS (`:1234`) between `cubepod-controller` and `cubepod-buildkit`.',
    zh: '*v0.7.0 新增。* 是否在 `cubepod-controller` 与 `cubepod-buildkit`（`:1234`）之间启用双向 mTLS 认证。',
  },
  'templateBuild.buildkit.tls.secretName': {
    en: '*Added in v0.7.0.* Kubernetes Secret holding `ca.crt`, `tls.crt`, and `tls.key` mounted into `buildkitd` and `cubepod-controller` when `buildkit.tls.enabled=true`.',
    zh: '*v0.7.0 新增。* 当 `buildkit.tls.enabled=true` 时挂载至 `buildkitd` 与 `cubepod-controller` 的 Secret 名称（需包含 `ca.crt`、`tls.crt` 与 `tls.key`）。',
  },
  'templateBuild.buildkit.tls.serverName': {
    en: '*Added in v0.7.0.* TLS ServerName (SAN) verified by `cubepod-controller` when dialing `cubepod-buildkit` over mTLS.',
    zh: '*v0.7.0 新增。* `cubepod-controller` 通过 mTLS 连接 `cubepod-buildkit` 时校验证书 SAN 使用的 `serverName`。',
  },
  'featureGates.transparentEgress': {
    en: '*Added in v0.6.0.* Wired into `--feature-gate=TransparentEgress=...` (combined with `egress.transparent`).',
    zh: '*v0.6.0 新增。* 注入 `--feature-gate=TransparentEgress=...`（与 `egress.transparent` 联合生效）。',
  },
  'featureGates.desktopStack': {
    en: '*Added in v0.6.0.* Wired into `--feature-gate=DesktopStack=...`; controls `/sandboxes/{id}/desktop/*` routes.',
    zh: '*v0.6.0 新增。* 注入 `--feature-gate=DesktopStack=...`，控制 `/sandboxes/{id}/desktop/*` 接口。',
  },
  'featureGates.statelessGatewayHA': {
    en: '*Added in v0.6.0.* Wired into `--feature-gate=StatelessGatewayHA=...`; controls Kubernetes CRD cache-miss reconstruction across gateway replicas.',
    zh: '*v0.6.0 新增。* 注入 `--feature-gate=StatelessGatewayHA=...`，控制多副本网关基于 Kubernetes CRD 的缓存未命中重建。',
  },
  'featureGates.e2bFilesystemWatch': {
    en: '*Added in v0.6.0.* Wired into `--feature-gate=E2BFilesystemWatch=...`; controls Connect-RPC filesystem directory watching.',
    zh: '*v0.6.0 新增。* 注入 `--feature-gate=E2BFilesystemWatch=...`，控制 Connect-RPC 目录变更监听功能。',
  },
  'featureGates.e2bSecretsAPI': {
    en: '*Added in v0.6.0.* Wired into `--feature-gate=E2BSecretsAPI=...`; controls `/secrets` management and `${e2b.secrets.<name>}` resolution.',
    zh: '*v0.6.0 新增。* 注入 `--feature-gate=E2BSecretsAPI=...`，控制 `/secrets` 管理与 `${e2b.secrets.<name>}` 占位符解析。',
  },
  'featureGates.sandboxEventsAndWebhooks': {
    en: '*Added in v0.6.0; changed in v0.7.0.* Wired into `--feature-gate=SandboxEventsAndWebhooks=...`; controls `/events`, `/webhooks`, and (since v0.7.0) `/webhooks/dead-letters` APIs. Events are disabled when this gate or `events.enabled` is `false`.',
    zh: '*v0.6.0 新增，v0.7.0 变更。* 注入 `--feature-gate=SandboxEventsAndWebhooks=...`，控制 `/events`、`/webhooks` 以及（自 v0.7.0 起）`/webhooks/dead-letters` 接口。该开关或 `events.enabled` 为 `false` 时禁用事件功能。',
  },
  'events.enabled': {
    en: '*Added in v0.7.0.* Enable sandbox lifecycle events, webhooks, and dead letters in `cubepod-gateway` (combined with `featureGates.sandboxEventsAndWebhooks`).',
    zh: '*v0.7.0 新增。* 是否在 `cubepod-gateway` 中启用沙箱生命周期事件、Webhook 与死信（与 `featureGates.sandboxEventsAndWebhooks` 共同生效）。',
  },
  'events.backend': {
    en: '*Added in v0.7.0.* Events backend (`CUBEPOD_EVENTS_BACKEND`): `"k8s"` (`SandboxWebhook` CRs + companion Secrets and PodBox Kubernetes Events for multi-replica HA), `"file"`, or `"disabled"`.',
    zh: '*v0.7.0 新增。* 事件后端（`CUBEPOD_EVENTS_BACKEND`）：`"k8s"`（基于 `SandboxWebhook` CR + 配套 Secret 与 PodBox Kubernetes Event 的多副本高可用）、`"file"` 或 `"disabled"`。',
  },
  'events.dir': {
    en: '*Added in v0.7.0.* Filesystem directory (`CUBEPOD_EVENTS_DIR`) used only when `events.backend` is `"file"`.',
    zh: '*v0.7.0 新增。* 仅当 `events.backend` 为 `"file"` 时使用的本地事件目录（`CUBEPOD_EVENTS_DIR`）。',
  },
  'featureGates.k8sVolumeMounts': {
    en: '*Changed in v0.8.0.* Wired into `--feature-gate=K8sVolumeMounts=...`; auto-enabled whenever `volumes.enabled` is `true` or `volumes.rwxStorageClassName` is non-empty.',
    zh: '*v0.8.0 变更。* 注入 `--feature-gate=K8sVolumeMounts=...`；当 `volumes.enabled` 为 `true` 或 `volumes.rwxStorageClassName` 非空时会自动开启。',
  },
  'volumes.enabled': {
    en: 'Mount the shared RWX persistent volumes PVC (`CUBEPOD_VOLUMES_DIR`) into `cubepod-gateway`. Automatically activated when `volumes.rwxStorageClassName` is set (*Changed in v0.8.0*).',
    zh: '是否在 `cubepod-gateway` 中挂载共享 RWX 持久卷 PVC（`CUBEPOD_VOLUMES_DIR`）。配置 `volumes.rwxStorageClassName` 时会自动激活（*v0.8.0 变更*）。',
  },
  'volumes.pvcName': {
    en: 'Shared `ReadWriteMany` PVC name mounted by `cubepod-gateway` and Kata sandbox Pods.',
    zh: '供 `cubepod-gateway` 与 Kata 沙箱 Pod 共同挂载的 `ReadWriteMany` PVC 名称。',
  },
  'volumes.rwxStorageClassName': {
    en: '*Changed in v0.8.0.* StorageClass for the shared `ReadWriteMany` volumes PVC (`volumes-pvc.yaml`). When non-empty, automatically activates `volumes` and enables `K8sVolumeMounts`.',
    zh: '*v0.8.0 变更。* 用于创建共享 `ReadWriteMany` 卷 PVC（`volumes-pvc.yaml`）的 StorageClass。一旦设置为非空值，将自动激活 `volumes` 并开启 `K8sVolumeMounts`。',
  },
  'volumes.size': {
    en: 'Capacity requested for the shared `ReadWriteMany` volumes PVC.',
    zh: '共享 `ReadWriteMany` 卷 PVC 申请的存储容量。',
  },
};

function syncHelmValues() {
  const raw = fs.readFileSync(path.join(repoRoot, 'deploy', 'helm', 'cubepod', 'values.yaml'), 'utf8');
  const chartRaw = fs.readFileSync(path.join(repoRoot, 'deploy', 'helm', 'cubepod', 'Chart.yaml'), 'utf8');
  const chart = YAML.parse(chartRaw);
  const { parsed, comments } = extractYamlComments(raw);
  const rows = [];
  flattenValues(parsed, '', comments, rows);

  for (const locale of ['en', 'zh']) {
    const isZh = locale === 'zh';
    const dest =
      locale === 'en'
        ? path.join(websiteDir, 'src', 'content', 'docs', 'reference', 'helm-values.md')
        : path.join(websiteDir, 'src', 'content', 'docs', 'zh', 'reference', 'helm-values.md');
    ensureDir(path.dirname(dest));

    const lines = [
      '---',
      `title: ${isZh ? 'Helm Chart 参数全表' : 'Helm Chart Values'}`,
      `description: ${
        isZh
          ? `同步自 deploy/helm/cubepod/values.yaml (Chart v${chart.version} / App ${chart.appVersion}) 的全量配置参数。`
          : `Complete configuration parameters synced from deploy/helm/cubepod/values.yaml (Chart v${chart.version} / App ${chart.appVersion}).`
      }`,
      'editUrl: false',
      '---',
      '',
      isZh
        ? `> 同步自 [\`deploy/helm/cubepod/values.yaml\`](https://github.com/cubeplexai/cubepod/blob/main/deploy/helm/cubepod/values.yaml)（Chart \`${chart.version}\` · App \`${chart.appVersion}\`）。`
        : `> Synced directly from [\`deploy/helm/cubepod/values.yaml\`](https://github.com/cubeplexai/cubepod/blob/main/deploy/helm/cubepod/values.yaml) (Chart \`${chart.version}\` · App \`${chart.appVersion}\`).`,
      '',
      isZh
        ? '> **v0.6.0 配置变更：** 新增 `gateway.podDisruptionBudget.*`（`pdb.yaml`）、`egress.transparent`、`secrets.*`（支持 `"k8s"` 多副本 HA 后端与密钥引用），统一通过 `--feature-gate` 注入 `featureGates.*`，且配置 RWX StorageClass 后会自动激活持久卷并开启 Kubernetes 挂载。'
        : '> **Changed / Added in v0.6.0:** Added `gateway.podDisruptionBudget.*` (`pdb.yaml`), `egress.transparent`, and `secrets.*` (`"k8s"` HA backend + encryption key Secret wiring); unified `featureGates.*` `--feature-gate` flags; and made setting an RWX StorageClass automatically activate shared persistent volumes.',
      '',
      isZh
        ? '> **v0.7.0 配置变更：** 新增 `events.*`（`events.enabled`、`events.backend`，默认 `"k8s"` 多副本 HA 后端，以及 `events.dir`），用于沙箱生命周期事件、Webhook 与死信。'
        : '> **Added in v0.7.0:** Added `events.*` (`events.enabled`, `events.backend` with the default `"k8s"` multi-replica HA backend, and `events.dir`) for sandbox lifecycle events, webhooks, and dead letters.',
      '',
      isZh ? '## 参数列表' : '## Parameter Reference',
      '',
      isZh ? '| 参数键 | 类型 | 默认值 | 说明 |' : '| Key | Type | Default | Description |',
      '| :--- | :--- | :--- | :--- |',
    ];
    for (const r of rows) {
      const plainKey = r.key.replace(/^`|`$/g, '');
      const overrideDesc = HELM_VALUE_DESCRIPTIONS[plainKey]?.[locale];
      const finalDesc = overrideDesc || r.desc;
      lines.push(`| ${r.key} | \`${r.type}\` | ${r.defaultVal} | ${escapeMdCell(finalDesc)} |`);
    }
    lines.push('');
    lines.push(isZh ? '## 原始 `values.yaml`' : '## Default `values.yaml`');
    lines.push('');
    lines.push('```yaml');
    lines.push(raw.trim());
    lines.push('```');
    lines.push('');

    fs.writeFileSync(dest, lines.join('\n'), 'utf8');
  }
  console.log('[sync] generated reference/helm-values.md & zh/reference/helm-values.md');
}

// 5. Sync docs/enhancements/0001-*..0022-* -> src/content/docs/ceps/
function rewriteCepLinks(md, currentSlug) {
  // Strip first H1 heading (`# CEP-XXXX: ...`) so Starlight renders frontmatter title cleanly
  let out = md.replace(/^#\s+CEP-\d+[^\n]*\n+/, '');
  // Strip manual <!-- toc --> list blocks at top
  out = out.replace(/<!--\s*toc\s*-->[\s\S]*?(?=\n---|\n##\s)/i, '');

  // Rewrite sibling CEP links: (../0018-.../README.md) or (../0018-.../) or (0018-.../README.md)
  out = out.replace(
    /\]\((?:\.\.\/)?(\d{4}-[a-z0-9-]+)(?:\/README\.md)?(#[^)]+)?\)/gi,
    (_, slug, hash) => `](/ceps/${slug}/${hash || ''})`
  );

  // Rewrite links to docs/architecture.md
  out = out.replace(
    /\]\((?:\.\.\/)*architecture\.md(#[^)]+)?\)/gi,
    (_, hash) => `](/concepts/architecture/${hash || ''})`
  );

  // Rewrite links pointing to source code or deploy manifests relative to docs/enhancements/<slug>/
  out = out.replace(/\]\(\.\.\/\.\.\/([^)#\s]+)(#[^)]+)?\)/g, (_, relPath, hash) => {
    return `](https://github.com/cubeplexai/cubepod/blob/main/${relPath}${hash || ''})`;
  });

  // Ensure any remaining relative README.md links to current dir work
  out = out.replace(/\]\(\.\/README\.md(#[^)]+)?\)/g, (_, hash) => `](/ceps/${currentSlug}/${hash || ''})`);

  return out.trim() + '\n';
}

function syncCEPs() {
  const enhRoot = path.join(repoRoot, 'docs', 'enhancements');
  const enCepsDir = path.join(websiteDir, 'src', 'content', 'docs', 'ceps');
  const zhCepsDir = path.join(websiteDir, 'src', 'content', 'docs', 'zh', 'ceps');

  fs.rmSync(enCepsDir, { recursive: true, force: true });
  fs.rmSync(zhCepsDir, { recursive: true, force: true });
  ensureDir(enCepsDir);
  ensureDir(zhCepsDir);

  const dirs = fs
    .readdirSync(enhRoot, { withFileTypes: true })
    .filter((d) => d.isDirectory() && /^\d{4}-/.test(d.name))
    .map((d) => d.name)
    .sort();

  const indexEntries = [];

  for (const dirName of dirs) {
    const cepYamlPath = path.join(enhRoot, dirName, 'cep.yaml');
    const readmePath = path.join(enhRoot, dirName, 'README.md');
    if (!fs.existsSync(readmePath)) continue;

    let meta = {};
    if (fs.existsSync(cepYamlPath)) {
      meta = YAML.parse(fs.readFileSync(cepYamlPath, 'utf8')) || {};
    }

    const num = String(meta['cep-number'] || dirName.slice(0, 4)).padStart(4, '0');
    const title = meta.title || `CEP-${num}`;
    const status = meta.status || 'implemented';
    const area = meta['owning-area'] || 'core';
    const milestone = meta['latest-milestone'] || meta['target-version'] || meta.version || meta.stage || '—';

    indexEntries.push({ dirName, num, title, status, area, milestone });

    const rawMd = fs.readFileSync(readmePath, 'utf8');
    const body = rewriteCepLinks(rawMd, dirName);

    const fmTitle = JSON.stringify(title);
    const fmDesc = JSON.stringify(`CubePod Enhancement Proposal ${num} (${status}, owning area: ${area}).`);
    const fmLabel = JSON.stringify(`CEP-${num}: ${title.replace(/^CEP-\d+:\s*/i, '')}`);

    const frontmatter = [
      '---',
      `title: ${fmTitle}`,
      `description: ${fmDesc}`,
      'editUrl: false',
      'sidebar:',
      `  label: ${fmLabel}`,
      `  order: ${parseInt(num, 10)}`,
      '---',
      '',
      `> - **Source**: [\`docs/enhancements/${dirName}/README.md\`](https://github.com/cubeplexai/cubepod/blob/main/docs/enhancements/${dirName}/README.md)`,
      `> - **Status**: \`${status}\``,
      `> - **Area**: \`${area}\``,
      `> - **Milestone**: \`${milestone}\``,
      '',
      '',
    ].join('\n');

    fs.writeFileSync(path.join(enCepsDir, `${dirName}.md`), frontmatter + body, 'utf8');
  }

  // Emit EN & ZH CEP Index pages
  const enIndex = [
    '---',
    'title: CubePod Enhancement Proposals (CEPs)',
    `description: Complete index of all CubePod architecture and subsystem Enhancement Proposals (CEP-0001 through CEP-${indexEntries[indexEntries.length - 1]?.num || '0029'}).`,
    'editUrl: false',
    'sidebar:',
    '  label: CEP Index',
    '  order: 0',
    '---',
    '',
    'CubePod uses **CubePod Enhancement Proposals (CEPs)** modeled after Kubernetes KEPs to specify every kernel/VMM boundary, storage driver, wire protocol, and security invariant before implementation.',
    '',
    '> Source of truth: [`docs/enhancements/`](https://github.com/cubeplexai/cubepod/tree/main/docs/enhancements)',
    '',
    '| CEP | Title | Status | Area | Milestone |',
    '| :--- | :--- | :--- | :--- | :--- |',
    ...indexEntries.map(
      (e) =>
        `| **[CEP-${e.num}](/ceps/${e.dirName}/)** | ${escapeMdCell(e.title.replace(/^CEP-\d+:\s*/i, ''))} | \`${e.status}\` | \`${e.area}\` | \`${e.milestone}\` |`
    ),
    '',
  ].join('\n');

  const zhIndex = [
    '---',
    'title: CubePod 架构演进提案 (CEPs)',
    `description: 同步自 docs/enhancements/ 的全部 CubePod 架构与子系统设计提案索引（CEP-0001 ~ CEP-${indexEntries[indexEntries.length - 1]?.num || '0029'}）。`,
    'editUrl: false',
    'sidebar:',
    '  label: CEP 提案总览',
    '  order: 0',
    '---',
    '',
    'CubePod 借鉴 Kubernetes KEP 流程，所有核心子系统、CRD/API 变更与安全边界设计均通过 **CubePod Enhancement Proposals (CEPs)** 评审后落地。点击下方提案可直接阅读英文原版完整设计规范：',
    '',
    '> 仓库目录：[`docs/enhancements/`](https://github.com/cubeplexai/cubepod/tree/main/docs/enhancements)',
    '',
    '| 编号 | 提案标题 | 状态 | 所属域 | 里程碑 |',
    '| :--- | :--- | :--- | :--- | :--- |',
    ...indexEntries.map(
      (e) =>
        `| **[CEP-${e.num}](/ceps/${e.dirName}/)** | ${escapeMdCell(e.title.replace(/^CEP-\d+:\s*/i, ''))} | \`${e.status}\` | \`${e.area}\` | \`${e.milestone}\` |`
    ),
    '',
  ].join('\n');

  fs.writeFileSync(path.join(enCepsDir, 'index.md'), enIndex, 'utf8');
  fs.writeFileSync(path.join(zhCepsDir, 'index.md'), zhIndex, 'utf8');
  console.log(`[sync] synced ${indexEntries.length} CEPs -> src/content/docs/ceps/`);
}

syncOpenAPI();
syncScalarBundle();
syncCRDs();
syncHelmValues();
syncCEPs();

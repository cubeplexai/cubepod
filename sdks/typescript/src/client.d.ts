export interface RequestOptions {
  signal?: AbortSignal;
}

export interface CubePodClientOptions {
  baseUrl?: string;
  apiKey?: string;
  timeoutMs?: number;
  maxRetries?: number;
  retryDelayMs?: number;
  fetch?: typeof globalThis.fetch;
  headers?: Record<string, string>;
}

export interface HealthStatus {
  status: string;
  service: string;
  sandboxes: number;
  autoResumeCount: number;
}

export interface SecretBinding {
  envName: string;
  placeholder?: string;
  rawSecret?: string;
  allowedHosts?: string[];
  allowedHeaders?: string[];
}

export interface NetworkEgressRule {
  action: 'allow' | 'deny';
  target: string;
}

export interface NetworkPolicySpec {
  defaultAction?: 'Allow' | 'Deny';
  allowHosts?: string[];
  denyHosts?: string[];
  egress?: NetworkEgressRule[];
  upstreamProxy?: string;
  secretBindings?: SecretBinding[];
}

export interface CreateSandboxOptions extends RequestOptions {
  templateId?: string;
  timeout?: number;
  timeoutSeconds?: number;
  metadata?: Record<string, string>;
  envVars?: Record<string, string>;
  autoPause?: boolean;
  autoResume?: boolean;
  warmPoolRef?: string;
  volumeKey?: string;
  rootfsSize?: string;
  workspaceSize?: string;
  networkPolicy?: NetworkPolicySpec;
  secretBindings?: SecretBinding[];
}

export type SandboxState =
  | 'Running'
  | 'Paused'
  | 'Provisioning'
  | 'Pausing'
  | 'Resuming'
  | 'Failed'
  | 'Terminated'
  | (string & {});

export interface SandboxInfo {
  sandboxId: string;
  templateId: string;
  state: SandboxState;
  clientId?: string;
  envdVersion?: string;
  startedAt?: string;
  endAt?: string;
  cpuCount?: number;
  memoryMB?: number;
  diskSizeMB?: number;
  envdPort: number;
  poddUrl?: string;
  rootfsGeneration: number;
  volumeKey?: string;
  metadata?: Record<string, string>;
  createdAt?: string;
  timeoutSeconds?: number;
  leaseSeconds?: number;
  autoResumeCount?: number;
  reason?: string;
}

export interface ListSandboxesOptions extends RequestOptions {
  state?: SandboxState;
  templateId?: string;
}

export interface ForkSandboxOptions extends RequestOptions {
  volumeKey?: string;
  metadata?: Record<string, string>;
}

export interface DeleteSandboxOptions extends RequestOptions {
  deleteVolumes?: boolean;
}

export interface EndpointHost {
  host: string;
  url: string;
  endpoint: string;
  port: number;
  sandboxId: string;
}

export interface CreateEndpointOptions extends RequestOptions {
  expiresInSeconds?: number;
  targetUrl?: string;
}

export interface PanelEndpoint {
  url: string;
  token: string;
  expiresAt?: string;
}

export interface RunCommandOptions extends RequestOptions {
  background?: boolean;
  workdir?: string;
  envs?: Record<string, string>;
  uid?: number;
  gid?: number;
}

export interface CommandResult {
  id: string;
  cmdId: string;
  running: boolean;
  pid?: number;
  exitCode?: number;
  stdout: string;
  stderr: string;
  output: string;
  cursor: number;
}

export interface GetCommandLogsOptions extends RequestOptions {
  cursor?: number;
}

export interface CommandLogs {
  id: string;
  cmdId: string;
  content: string;
  cursor: number;
  running: boolean;
  exitCode?: number;
}

export interface CodeOutputMessage {
  text: string;
  line: string;
  timestamp?: number;
  error: boolean;
}

export interface CodeResult {
  text?: string;
  png?: string;
  html?: string;
  isMainResult: boolean;
  raw: Record<string, unknown>;
}

export interface CodeExecutionError {
  name: string;
  value: string;
  traceback: string;
  exitCode?: number;
}

export interface RunCodeOptions extends RequestOptions {
  contextId?: string;
  language?: string;
  envVars?: Record<string, string>;
  onStdout?: (msg: CodeOutputMessage) => void | Promise<void>;
  onStderr?: (msg: CodeOutputMessage) => void | Promise<void>;
  onResult?: (result: CodeResult) => void | Promise<void>;
  onError?: (error: CodeExecutionError) => void | Promise<void>;
}

export interface ExecutionResult {
  logs: {
    stdout: string[];
    stderr: string[];
  };
  stdout: string;
  stderr: string;
  results: CodeResult[];
  error?: CodeExecutionError;
  executionCount?: number;
  text?: string;
}

export interface CreateContextOptions extends RequestOptions {
  id?: string;
  language?: string;
  cwd?: string;
}

export interface CodeContext {
  id: string;
  language: string;
  cwd: string;
}

export interface WriteFileOptions extends RequestOptions {
  uid?: number;
  gid?: number;
  mode?: string;
}

export interface FileEntry {
  name: string;
  path: string;
  isDir: boolean;
  size: number;
  mode: string;
}

export interface ArchiveSyncOptions extends RequestOptions {
  dest?: string;
  removePaths?: string[];
  uid?: number;
  gid?: number;
}

export type TemplateBuildStatus = 'waiting' | 'building' | 'ready' | 'error';

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

export interface BuildLogEntry {
  timestamp: string;
  level: LogLevel;
  message: string;
}

export interface BuildStatusReason {
  message: string;
  step?: string;
  logEntries?: BuildLogEntry[];
}

export interface E2BTemplate {
  templateID: string;
  buildID: string;
  cpuCount: number;
  memoryMB: number;
  diskSizeMB: number;
  public: boolean;
  aliases: string[];
  names: string[];
  createdAt: string;
  updatedAt: string;
  createdBy: string | null;
  lastSpawnedAt: string | null;
  spawnCount: number;
  buildCount: number;
  envdVersion: string;
  buildStatus: TemplateBuildStatus;
}

export interface E2BTemplateBuildSummary {
  buildID: string;
  templateID?: string;
  status: TemplateBuildStatus;
  createdAt: string;
  updatedAt: string;
  cpuCount: number;
  memoryMB: number;
  reason?: BuildStatusReason;
}

export interface TemplateWithBuilds extends E2BTemplate {
  builds: E2BTemplateBuildSummary[];
}

export interface ListTemplatesOptions extends RequestOptions {
  teamId?: string;
}

export interface ListTemplatesV2Options extends RequestOptions {
  limit?: number;
  nextToken?: string;
}

export interface ListTemplatesV2Response {
  templates: E2BTemplate[];
  nextToken?: string;
}

export interface GetTemplateOptions extends RequestOptions {
  byAlias?: boolean;
}

export interface CreateTemplateRequest {
  name?: string;
  alias?: string;
  tags?: string[];
  cpuCount?: number;
  memoryMB?: number;
  minFreeDiskMb?: number;
}

export interface CreateTemplateOptions extends RequestOptions {
  idempotencyKey?: string;
}

export interface TemplateRequestBuildResponse {
  templateID: string;
  buildID: string;
  public: boolean;
  aliases: string[];
  names: string[];
  tags: string[];
}

export interface TemplateBuildStep {
  type: 'RUN' | 'COPY' | 'ADD' | 'WORKDIR' | 'USER' | 'ENV';
  args: string[];
  filesHash?: string;
  force?: boolean;
}

export interface FromImageRegistry {
  type?: string;
  username?: string;
  password?: string;
}

export interface StartTemplateBuildRequest {
  fromImage?: string;
  fromTemplate?: string;
  fromImageRegistry?: FromImageRegistry;
  force?: boolean;
  startCmd?: string;
  readyCmd?: string;
  steps?: TemplateBuildStep[];
}

export interface GetTemplateBuildStatusOptions extends RequestOptions {
  logsOffset?: number;
  limit?: number;
  level?: LogLevel;
}

export interface TemplateBuildInfo {
  templateID: string;
  buildID: string;
  status: TemplateBuildStatus;
  reason?: BuildStatusReason;
  logs: string[];
  logEntries: BuildLogEntry[];
}

export interface GetTemplateBuildLogsOptions extends RequestOptions {
  cursor?: number;
  limit?: number;
  direction?: 'forward' | 'backward';
  level?: LogLevel;
}

export interface TemplateBuildFileUpload {
  present: boolean;
  url?: string;
  headers?: Record<string, string>;
}

export interface DeleteTemplateOptions extends RequestOptions {
  force?: boolean;
}

export interface TemplateTagInfo {
  tag: string;
  buildID: string;
}

export interface CreateWarmPoolRequest {
  name?: string;
  templateRef?: string;
  templateId?: string;
  replicas?: number;
}

export interface WarmPoolInfo {
  name: string;
  poolName?: string;
  templateRef: string;
  replicas: number;
  readyReplicas?: number;
  availableReplicas?: number;
}

export function normalizeSandboxInfo(raw: Record<string, unknown>): SandboxInfo;

export class CubePodClient {
  readonly baseUrl: string;
  readonly apiKey?: string;
  readonly timeoutMs: number;
  readonly maxRetries: number;
  readonly retryDelayMs: number;

  constructor(options?: CubePodClientOptions);

  health(options?: RequestOptions): Promise<HealthStatus>;

  createSandbox(options?: CreateSandboxOptions): Promise<SandboxInfo>;
  getSandbox(sandboxId: string, options?: RequestOptions): Promise<SandboxInfo>;
  listSandboxes(filter?: ListSandboxesOptions, options?: RequestOptions): Promise<SandboxInfo[]>;
  connectSandbox(sandboxId: string, options?: RequestOptions): Promise<SandboxInfo>;
  pauseSandbox(sandboxId: string, options?: RequestOptions): Promise<SandboxInfo>;
  setSandboxTimeout(sandboxId: string, timeoutSeconds?: number, options?: RequestOptions): Promise<SandboxInfo>;
  refreshSandbox(sandboxId: string, durationSeconds?: number, options?: RequestOptions): Promise<void>;
  acquireLease(sandboxId: string, leaseSeconds?: number, options?: RequestOptions): Promise<SandboxInfo>;
  resetRootfs(sandboxId: string, options?: RequestOptions): Promise<SandboxInfo>;
  forkSandbox(sandboxId: string, forkOptions?: ForkSandboxOptions, options?: RequestOptions): Promise<SandboxInfo>;
  deleteSandbox(sandboxId: string, deleteOptions?: DeleteSandboxOptions, options?: RequestOptions): Promise<void>;
  getEndpointHost(sandboxId: string, port: number, options?: RequestOptions): Promise<EndpointHost>;
  getHost(sandboxId: string, port: number, options?: RequestOptions): Promise<string>;
  createEndpoint(
    sandboxId: string,
    port: number,
    endpointOptions?: CreateEndpointOptions,
    options?: RequestOptions,
  ): Promise<PanelEndpoint>;

  runCommand(sandboxId: string, command: string, options?: RunCommandOptions): Promise<CommandResult>;
  getCommandStatus(sandboxId: string, cmdId: string, options?: RequestOptions): Promise<CommandResult>;
  getCommandLogs(
    sandboxId: string,
    cmdId: string,
    options?: number | GetCommandLogsOptions,
  ): Promise<CommandLogs>;
  killCommand(sandboxId: string, cmdId: string, options?: RequestOptions): Promise<{ id: string; killed: boolean }>;

  runCode(sandboxId: string, code: string, options?: RunCodeOptions): Promise<ExecutionResult>;
  createContext(
    sandboxId: string,
    contextOptions?: CreateContextOptions,
    options?: RequestOptions,
  ): Promise<CodeContext>;
  listContexts(sandboxId: string, options?: RequestOptions): Promise<CodeContext[]>;
  getContext(sandboxId: string, contextId: string, options?: RequestOptions): Promise<CodeContext>;
  restartContext(sandboxId: string, contextId: string, options?: RequestOptions): Promise<CodeContext>;
  deleteContext(sandboxId: string, contextId: string, options?: RequestOptions): Promise<void>;

  writeFile(
    sandboxId: string,
    path: string,
    content: string | Uint8Array | ArrayBuffer,
    options?: WriteFileOptions,
  ): Promise<{ status?: string; path: string; size: number }>;
  readFileBytes(sandboxId: string, path: string, options?: RequestOptions): Promise<Uint8Array>;
  readFileText(sandboxId: string, path: string, options?: RequestOptions): Promise<string>;
  removeFile(
    sandboxId: string,
    path: string,
    options?: RequestOptions,
  ): Promise<{ status?: string; deleted?: string }>;
  listFiles(sandboxId: string, path?: string, options?: RequestOptions): Promise<FileEntry[]>;
  archiveSync(
    sandboxId: string,
    archiveOrFiles: Uint8Array | ArrayBuffer | Record<string, string | Uint8Array | ArrayBuffer>,
    options?: ArchiveSyncOptions,
  ): Promise<{ status: string; extracted?: number }>;

  listTemplates(options?: ListTemplatesOptions): Promise<E2BTemplate[]>;
  listTemplatesV2(options?: ListTemplatesV2Options): Promise<ListTemplatesV2Response>;
  getTemplate(templateIdOrAlias: string, options?: GetTemplateOptions): Promise<TemplateWithBuilds>;
  getTemplateByAlias(alias: string, options?: RequestOptions): Promise<E2BTemplate>;
  createTemplate(
    request: CreateTemplateRequest,
    options?: CreateTemplateOptions,
  ): Promise<TemplateRequestBuildResponse>;
  startTemplateBuild(
    templateId: string,
    buildId: string,
    request: StartTemplateBuildRequest,
    options?: RequestOptions,
  ): Promise<void>;
  getTemplateBuildStatus(
    templateId: string,
    buildId: string,
    options?: GetTemplateBuildStatusOptions,
  ): Promise<TemplateBuildInfo>;
  getTemplateBuildLogs(
    templateId: string,
    buildId: string,
    options?: GetTemplateBuildLogsOptions,
  ): Promise<{ logs: BuildLogEntry[] }>;
  getTemplateFileUploadLink(
    templateId: string,
    hash: string,
    options?: RequestOptions,
  ): Promise<TemplateBuildFileUpload>;
  uploadTemplateFileContext(
    uploadUrl: string,
    tarBytes: Uint8Array | ArrayBuffer,
    options?: { headers?: Record<string, string>; signal?: AbortSignal },
  ): Promise<void>;
  deleteTemplate(templateId: string, options?: DeleteTemplateOptions): Promise<void>;
  updateTemplateVisibility(
    templateId: string,
    isPublic: boolean,
    options?: RequestOptions,
  ): Promise<{ names: string[] }>;
  assignTemplateTags(target: string, tags: string[], options?: RequestOptions): Promise<void>;
  deleteTemplateTags(name: string, tags: string[], options?: RequestOptions): Promise<void>;
  getTemplateTags(templateId: string, options?: RequestOptions): Promise<TemplateTagInfo[]>;

  createWarmPool(request: CreateWarmPoolRequest, options?: RequestOptions): Promise<WarmPoolInfo>;
  listWarmPools(options?: RequestOptions): Promise<WarmPoolInfo[]>;
}

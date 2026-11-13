import {
  ArchiveSyncOptions,
  CodeContext,
  CommandLogs,
  CommandResult,
  CreateContextOptions,
  CreateEndpointOptions,
  CreateSandboxOptions,
  CubePodClient,
  CubePodClientOptions,
  DeleteSandboxOptions,
  EndpointHost,
  ExecutionResult,
  FileEntry,
  ForkSandboxOptions,
  GetCommandLogsOptions,
  PanelEndpoint,
  RequestOptions,
  RunCodeOptions,
  RunCommandOptions,
  SandboxInfo,
  SandboxState,
  WriteFileOptions,
} from './client.js';

export interface SandboxClientConfig extends CubePodClientOptions {
  client?: CubePodClient;
}

export interface SandboxCreateOptions extends CreateSandboxOptions, SandboxClientConfig {}

export interface SandboxConnectOptions extends RequestOptions, SandboxClientConfig {}

export interface SandboxCommands {
  run(command: string, options?: RunCommandOptions): Promise<CommandResult>;
  getStatus(cmdId: string, options?: RequestOptions): Promise<CommandResult>;
  getLogs(cmdId: string, options?: number | GetCommandLogsOptions): Promise<CommandLogs>;
  kill(cmdId: string, options?: RequestOptions): Promise<{ id: string; killed: boolean }>;
}

export interface SandboxCode {
  runCode(code: string, options?: RunCodeOptions): Promise<ExecutionResult>;
  createContext(contextOptions?: CreateContextOptions, options?: RequestOptions): Promise<CodeContext>;
  listContexts(options?: RequestOptions): Promise<CodeContext[]>;
  getContext(contextId: string, options?: RequestOptions): Promise<CodeContext>;
  restartContext(contextId: string, options?: RequestOptions): Promise<CodeContext>;
  deleteContext(contextId: string, options?: RequestOptions): Promise<void>;
}

export interface SandboxFiles {
  write(
    path: string,
    content: string | Uint8Array | ArrayBuffer,
    options?: WriteFileOptions,
  ): Promise<{ status?: string; path: string; size: number }>;
  readText(path: string, options?: RequestOptions): Promise<string>;
  readBytes(path: string, options?: RequestOptions): Promise<Uint8Array>;
  remove(path: string, options?: RequestOptions): Promise<{ status?: string; deleted?: string }>;
  list(path?: string, options?: RequestOptions): Promise<FileEntry[]>;
  archiveSync(
    archiveOrFiles: Uint8Array | ArrayBuffer | Record<string, string | Uint8Array | ArrayBuffer>,
    options?: ArchiveSyncOptions,
  ): Promise<{ status: string; extracted?: number }>;
}

export class Sandbox {
  readonly client: CubePodClient;
  readonly sandboxId: string;
  info: SandboxInfo;
  readonly commands: SandboxCommands;
  readonly code: SandboxCode;
  readonly files: SandboxFiles;

  constructor(client: CubePodClient, info: SandboxInfo);

  get templateId(): string;
  get state(): SandboxState;

  static create(options?: SandboxCreateOptions): Promise<Sandbox>;
  static create(templateId: string, options?: SandboxCreateOptions): Promise<Sandbox>;
  static connect(sandboxId: string, options?: SandboxConnectOptions): Promise<Sandbox>;

  refresh(options?: RequestOptions): Promise<SandboxInfo>;
  connect(options?: RequestOptions): Promise<SandboxInfo>;
  pause(options?: RequestOptions): Promise<SandboxInfo>;
  setTimeout(timeoutSeconds: number, options?: RequestOptions): Promise<SandboxInfo>;
  acquireLease(leaseSeconds?: number, options?: RequestOptions): Promise<SandboxInfo>;
  resetRootfs(options?: RequestOptions): Promise<SandboxInfo>;
  fork(forkOptions?: ForkSandboxOptions, options?: RequestOptions): Promise<Sandbox>;
  kill(deleteOptions?: DeleteSandboxOptions, options?: RequestOptions): Promise<void>;
  getHost(port: number, options?: RequestOptions): Promise<string>;
  getEndpointHost(port: number, options?: RequestOptions): Promise<EndpointHost>;
  createEndpoint(
    port: number,
    endpointOptions?: CreateEndpointOptions,
    options?: RequestOptions,
  ): Promise<PanelEndpoint>;
}

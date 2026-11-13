import { CubePodClient } from './client.js';

/**
 * Resolve a CubePodClient from options, reusing `options.client` when provided.
 *
 * @param {import('./sandbox.d.ts').SandboxClientConfig} [options]
 * @returns {CubePodClient}
 */
function resolveClient(options = {}) {
  if (options.client) return options.client;
  return new CubePodClient({
    baseUrl: options.baseUrl,
    apiKey: options.apiKey,
    timeoutMs: options.timeoutMs,
    maxRetries: options.maxRetries,
    retryDelayMs: options.retryDelayMs,
    fetch: options.fetch,
    headers: options.headers,
  });
}

/**
 * Stateful Sandbox handle with scoped `.commands`, `.code`, and `.files` namespaces.
 */
export class Sandbox {
  /** @type {CubePodClient} */
  client;
  /** @type {string} */
  sandboxId;
  /** @type {import('./client.d.ts').SandboxInfo} */
  info;
  /** @type {import('./sandbox.d.ts').SandboxCommands} */
  commands;
  /** @type {import('./sandbox.d.ts').SandboxCode} */
  code;
  /** @type {import('./sandbox.d.ts').SandboxFiles} */
  files;

  /**
   * @param {CubePodClient} client
   * @param {import('./client.d.ts').SandboxInfo} info
   */
  constructor(client, info) {
    this.client = client;
    this.sandboxId = info.sandboxId;
    this.info = info;

    this.commands = {
      run: (command, options) => this.client.runCommand(this.sandboxId, command, options),
      getStatus: (cmdId, options) => this.client.getCommandStatus(this.sandboxId, cmdId, options),
      getLogs: (cmdId, options) => this.client.getCommandLogs(this.sandboxId, cmdId, options),
      kill: (cmdId, options) => this.client.killCommand(this.sandboxId, cmdId, options),
    };

    this.code = {
      runCode: (code, options) => this.client.runCode(this.sandboxId, code, options),
      createContext: (contextOptions, options) =>
        this.client.createContext(this.sandboxId, contextOptions, options),
      listContexts: (options) => this.client.listContexts(this.sandboxId, options),
      getContext: (contextId, options) =>
        this.client.getContext(this.sandboxId, contextId, options),
      restartContext: (contextId, options) =>
        this.client.restartContext(this.sandboxId, contextId, options),
      deleteContext: (contextId, options) =>
        this.client.deleteContext(this.sandboxId, contextId, options),
    };

    this.files = {
      write: (path, content, options) =>
        this.client.writeFile(this.sandboxId, path, content, options),
      readText: (path, options) => this.client.readFileText(this.sandboxId, path, options),
      readBytes: (path, options) => this.client.readFileBytes(this.sandboxId, path, options),
      remove: (path, options) => this.client.removeFile(this.sandboxId, path, options),
      list: (path, options) => this.client.listFiles(this.sandboxId, path, options),
      archiveSync: (archiveOrFiles, options) =>
        this.client.archiveSync(this.sandboxId, archiveOrFiles, options),
    };
  }

  /** @returns {string} */
  get templateId() {
    return this.info.templateId;
  }

  /** @returns {import('./client.d.ts').SandboxState} */
  get state() {
    return this.info.state;
  }

  /**
   * Create a new sandbox and return a connected Sandbox handle.
   *
   * @param {string | import('./sandbox.d.ts').SandboxCreateOptions} [templateOrOptions]
   * @param {import('./sandbox.d.ts').SandboxCreateOptions} [extraOptions]
   * @returns {Promise<Sandbox>}
   */
  static async create(templateOrOptions, extraOptions) {
    /** @type {import('./sandbox.d.ts').SandboxCreateOptions} */
    const opts =
      typeof templateOrOptions === 'string'
        ? { ...(extraOptions ?? {}), templateId: templateOrOptions }
        : { ...(templateOrOptions ?? {}) };

    const client = resolveClient(opts);
    const info = await client.createSandbox(opts);
    return new Sandbox(client, info);
  }

  /**
   * Connect to an existing sandbox (auto-resuming if Paused).
   *
   * @param {string} sandboxId
   * @param {import('./sandbox.d.ts').SandboxConnectOptions} [options]
   * @returns {Promise<Sandbox>}
   */
  static async connect(sandboxId, options = {}) {
    const client = resolveClient(options);
    const info = await client.connectSandbox(sandboxId, { signal: options.signal });
    return new Sandbox(client, info);
  }

  /**
   * Refresh cached sandbox metadata from the Gateway.
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async refresh(options = {}) {
    this.info = await this.client.getSandbox(this.sandboxId, options);
    return this.info;
  }

  /**
   * Connect / resume this sandbox if currently Paused.
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async connect(options = {}) {
    this.info = await this.client.connectSandbox(this.sandboxId, options);
    return this.info;
  }

  /**
   * Pause the sandbox, stopping compute while preserving RootFS & Workspace PVCs.
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async pause(options = {}) {
    this.info = await this.client.pauseSandbox(this.sandboxId, options);
    return this.info;
  }

  /**
   * Set the idle auto-pause timeout in seconds.
   *
   * @param {number} timeoutSeconds
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async setTimeout(timeoutSeconds, options = {}) {
    this.info = await this.client.setSandboxTimeout(this.sandboxId, timeoutSeconds, options);
    return this.info;
  }

  /**
   * Acquire or renew a lease protecting against automatic idle pause.
   *
   * @param {number} [leaseSeconds=300]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async acquireLease(leaseSeconds = 300, options = {}) {
    this.info = await this.client.acquireLease(this.sandboxId, leaseSeconds, options);
    return this.info;
  }

  /**
   * Reset RootFS Upper to a clean base image while keeping `/workspace` intact.
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async resetRootfs(options = {}) {
    this.info = await this.client.resetRootfs(this.sandboxId, options);
    return this.info;
  }

  /**
   * Fork this sandbox via Copy-on-Write block snapshots and return the child Sandbox.
   *
   * @param {import('./client.d.ts').ForkSandboxOptions} [forkOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<Sandbox>}
   */
  async fork(forkOptions = {}, options = {}) {
    const childInfo = await this.client.forkSandbox(this.sandboxId, forkOptions, options);
    return new Sandbox(this.client, childInfo);
  }

  /**
   * Kill and delete this sandbox.
   *
   * @param {import('./client.d.ts').DeleteSandboxOptions} [deleteOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async kill(deleteOptions = {}, options = {}) {
    await this.client.deleteSandbox(this.sandboxId, deleteOptions, options);
  }

  /**
   * Get the public ingress host string for a sandbox port (E2B `getHost` compatible).
   *
   * @param {number} port
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<string>}
   */
  async getHost(port, options = {}) {
    return this.client.getHost(this.sandboxId, port, options);
  }

  /**
   * Get full ingress host/URL metadata for a sandbox port.
   *
   * @param {number} port
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').EndpointHost>}
   */
  async getEndpointHost(port, options = {}) {
    return this.client.getEndpointHost(this.sandboxId, port, options);
  }

  /**
   * Mint an HMAC-SHA256 signed `/panel/{token}/` proxy endpoint for a sandbox port.
   *
   * @param {number} port
   * @param {import('./client.d.ts').CreateEndpointOptions} [endpointOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').PanelEndpoint>}
   */
  async createEndpoint(port, endpointOptions = {}, options = {}) {
    return this.client.createEndpoint(this.sandboxId, port, endpointOptions, options);
  }
}

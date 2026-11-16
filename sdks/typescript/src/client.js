import { buildTarGzBytes } from './archive.js';
import { CubePodError, parseAPIError } from './errors.js';
import { readNdjsonStream } from './ndjson.js';

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder('utf-8');

/**
 * Safely read an environment variable across Node, Deno, Bun, and browser runtimes.
 *
 * @param {...string} names
 * @returns {string | undefined}
 */
function readEnv(...names) {
  if (typeof process === 'undefined' || !process.env) return undefined;
  for (const name of names) {
    const val = process.env[name];
    if (val) return val;
  }
  return undefined;
}

/**
 * Sleep for `ms` milliseconds, rejecting immediately if `signal` aborts.
 *
 * @param {number} ms
 * @param {AbortSignal} [signal]
 * @returns {Promise<void>}
 */
function sleep(ms, signal) {
  if (signal?.aborted) {
    return Promise.reject(signal.reason ?? new CubePodError('Request aborted'));
  }
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason ?? new CubePodError('Request aborted'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

/**
 * Combine an optional caller AbortSignal and an optional timeout into one AbortSignal.
 *
 * @param {number} timeoutMs
 * @param {AbortSignal} [callerSignal]
 * @returns {{ signal?: AbortSignal; cleanup: () => void }}
 */
function createRequestSignal(timeoutMs, callerSignal) {
  const hasTimeout = Number.isFinite(timeoutMs) && timeoutMs > 0;
  if (!hasTimeout && !callerSignal) {
    return { signal: undefined, cleanup: () => {} };
  }
  if (!hasTimeout) {
    return { signal: callerSignal, cleanup: () => {} };
  }

  const controller = new AbortController();
  const timer = setTimeout(() => {
    controller.abort(new CubePodError(`Request timed out after ${timeoutMs}ms`));
  }, timeoutMs);

  const onCallerAbort = () => {
    controller.abort(callerSignal?.reason ?? new CubePodError('Request aborted'));
  };

  if (callerSignal) {
    if (callerSignal.aborted) {
      clearTimeout(timer);
      controller.abort(callerSignal.reason ?? new CubePodError('Request aborted'));
      return { signal: controller.signal, cleanup: () => {} };
    }
    callerSignal.addEventListener('abort', onCallerAbort, { once: true });
  }

  return {
    signal: controller.signal,
    cleanup: () => {
      clearTimeout(timer);
      callerSignal?.removeEventListener('abort', onCallerAbort);
    },
  };
}

/**
 * Normalize raw gateway SandboxInfo JSON into SDK SandboxInfo shape.
 *
 * @param {Record<string, any>} raw
 * @returns {import('./client.d.ts').SandboxInfo}
 */
export function normalizeSandboxInfo(raw) {
  return {
    sandboxId: raw.sandboxID ?? raw.sandboxId ?? '',
    templateId: raw.templateID ?? raw.templateId ?? '',
    state: raw.state ?? '',
    clientId: raw.clientID ?? raw.clientId,
    envdVersion: raw.envdVersion,
    startedAt: raw.startedAt,
    endAt: raw.endAt,
    cpuCount: raw.cpuCount,
    memoryMB: raw.memoryMB,
    diskSizeMB: raw.diskSizeMB,
    envdPort: raw.envdPort ?? 49983,
    poddUrl: raw.poddURL ?? raw.poddUrl,
    rootfsGeneration: raw.rootfsGeneration ?? 1,
    volumeKey: raw.volumeKey,
    metadata: raw.metadata,
    createdAt: raw.createdAt,
    timeoutSeconds: raw.timeoutSeconds,
    leaseSeconds: raw.leaseSeconds,
    autoResumeCount: raw.autoResumeCount,
    reason: raw.reason,
  };
}

/**
 * Normalize command run/status response JSON.
 *
 * @param {Record<string, any>} raw
 * @param {string} [fallbackId]
 * @returns {import('./client.d.ts').CommandResult}
 */
function normalizeCommandResult(raw, fallbackId = '') {
  const id = raw.id ?? raw.cmdID ?? fallbackId;
  const exitCode =
    raw.exit_code !== undefined
      ? raw.exit_code
      : raw.exitCode !== undefined
        ? raw.exitCode
        : undefined;
  const stdout = raw.stdout ?? raw.output ?? '';
  const stderr = raw.stderr ?? '';
  const output = raw.output ?? stdout;
  return {
    id,
    cmdId: raw.cmdID ?? id,
    running: raw.running ?? false,
    pid: raw.pid,
    exitCode,
    stdout,
    stderr,
    output,
    cursor: raw.cursor ?? 0,
  };
}

/**
 * Parse JSON if body is non-empty, otherwise return undefined.
 *
 * @template T
 * @param {Response} response
 * @returns {Promise<T>}
 */
async function readJsonOrUndefined(response) {
  const text = await response.text();
  if (!text) return /** @type {T} */ (undefined);
  return JSON.parse(text);
}

/**
 * Statically & dynamically typed HTTP client for CubePod Control & Data Plane.
 */

/**
 * Return the official E2B secret placeholder ``${e2b.secrets.<name>}``.
 *
 * @param {string} name
 * @returns {string}
 */
export function fillSecret(name) {
  return `\${e2b.secrets.${name}}`;
}

export const Secret = {
  fill: fillSecret,
};

/**
 * Normalize SecretInfo JSON from the Gateway.
 *
 * @param {Record<string, any>} raw
 * @returns {import('./client.d.ts').SecretInfo}
 */
function normalizeSecretInfo(raw) {
  const secretId = raw.secretID ?? raw.id ?? '';
  const currentVersion = raw.currentVersion ?? raw.version ?? 1;
  return {
    secretId,
    id: secretId,
    name: raw.name ?? '',
    currentVersion,
    version: currentVersion,
    metadata: raw.metadata ?? {},
    createdAt: raw.createdAt,
    updatedAt: raw.updatedAt,
  };
}

/**
 * Normalize VolumeInfo JSON from the Gateway.
 *
 * @param {Record<string, any>} raw
 * @returns {import('./client.d.ts').VolumeInfo}
 */
function normalizeVolumeInfo(raw) {
  return {
    volumeId: raw.volumeID ?? raw.volumeId ?? '',
    name: raw.name ?? '',
    token: raw.token,
  };
}

/**
 * Format a metric start/end timestamp for query parameters.
 *
 * @param {number | string | Date | undefined} value
 * @returns {string | undefined}
 */
function formatMetricTime(value) {
  if (value === undefined) return undefined;
  if (value instanceof Date) return value.toISOString();
  if (typeof value === 'number') return String(Math.trunc(value));
  return String(value);
}

export class CubePodClient {
  /** @type {string} */
  baseUrl;
  /** @type {string | undefined} */
  apiKey;
  /** @type {number} */
  timeoutMs;
  /** @type {number} */
  maxRetries;
  /** @type {number} */
  retryDelayMs;
  /** @type {typeof globalThis.fetch} */
  _fetch;
  /** @type {Record<string, string>} */
  defaultHeaders;

  /**
   * @param {import('./client.d.ts').CubePodClientOptions} [options]
   */
  constructor(options = {}) {
    const rawBaseUrl =
      options.baseUrl ??
      readEnv('CUBEPOD_BASE_URL', 'CUBEPOD_API_URL') ??
      'http://localhost:8080';
    this.baseUrl = rawBaseUrl.replace(/\/+$/, '');
    this.apiKey = options.apiKey ?? readEnv('CUBEPOD_API_KEY', 'E2B_API_KEY');
    this.timeoutMs = options.timeoutMs ?? 60_000;
    this.maxRetries = options.maxRetries ?? 2;
    this.retryDelayMs = options.retryDelayMs ?? 10;
    this._fetch = options.fetch ?? globalThis.fetch;
    this.defaultHeaders = { ...(options.headers ?? {}) };

    if (typeof this._fetch !== 'function') {
      throw new CubePodError('No fetch implementation available in this runtime');
    }
  }

  /**
   * Build a full request URL from a path and optional query params.
   *
   * @param {string} path
   * @param {Record<string, string | number | boolean | undefined>} [query]
   * @returns {string}
   */
  _buildUrl(path, query) {
    const base = path.startsWith('http://') || path.startsWith('https://')
      ? path
      : `${this.baseUrl}${path.startsWith('/') ? path : `/${path}`}`;
    if (!query) return base;

    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined) {
        params.set(key, String(value));
      }
    }
    const qs = params.toString();
    if (!qs) return base;
    return base.includes('?') ? `${base}&${qs}` : `${base}?${qs}`;
  }

  /**
   * Execute an HTTP request with bearer auth, timeout, and transient 502/503/504 retries.
   *
   * @param {string} method
   * @param {string} path
   * @param {{
   *   query?: Record<string, string | number | boolean | undefined>;
   *   json?: unknown;
   *   body?: BodyInit | Uint8Array | null;
   *   headers?: Record<string, string>;
   *   signal?: AbortSignal;
   *   sandboxId?: string;
   *   includeAuth?: boolean;
   *   retry?: boolean;
   * }} [options]
   * @returns {Promise<Response>}
   */
  async _request(method, path, options = {}) {
    const url = this._buildUrl(path, options.query);
    const headers = new Headers(this.defaultHeaders);
    if (options.includeAuth !== false && this.apiKey) {
      headers.set('Authorization', `Bearer ${this.apiKey}`);
      headers.set('X-API-KEY', this.apiKey);
    }
    if (options.headers) {
      for (const [k, v] of Object.entries(options.headers)) {
        headers.set(k, v);
      }
    }

    /** @type {BodyInit | undefined} */
    let body;
    if (options.json !== undefined) {
      if (!headers.has('Content-Type')) {
        headers.set('Content-Type', 'application/json');
      }
      body = JSON.stringify(options.json);
    } else if (options.body !== undefined && options.body !== null) {
      body = /** @type {BodyInit} */ (options.body);
    }

    const maxRetries = options.retry === false ? 0 : this.maxRetries;
    let attempt = 0;

    while (true) {
      const { signal, cleanup } = createRequestSignal(this.timeoutMs, options.signal);
      /** @type {Response} */
      let response;
      try {
        response = await this._fetch(url, {
          method,
          headers,
          body,
          signal,
        });
      } catch (err) {
        cleanup();
        throw err instanceof CubePodError
          ? err
          : new CubePodError(err instanceof Error ? err.message : String(err), { cause: err });
      }
      cleanup();

      if (response.ok) {
        return response;
      }

      const apiErr = await parseAPIError(response, { sandboxId: options.sandboxId });
      const isRetryableStatus =
        apiErr.statusCode === 502 || apiErr.statusCode === 504 || apiErr.statusCode === 503;
      const isTerminalReadyTimeout =
        apiErr.statusCode === 503 &&
        Boolean(apiErr.sandboxId || apiErr.logPath);

      if (isRetryableStatus && !isTerminalReadyTimeout && attempt < maxRetries) {
        attempt += 1;
        await sleep(this.retryDelayMs * 2 ** (attempt - 1), options.signal);
        continue;
      }

      throw apiErr;
    }
  }

  /**
   * Check Gateway health and cluster counts.
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').HealthStatus>}
   */
  async health(options = {}) {
    const resp = await this._request('GET', '/health', { signal: options.signal });
    return readJsonOrUndefined(resp);
  }

  // ---------------------------------------------------------------------------
  // Sandbox Lifecycle
  // ---------------------------------------------------------------------------

  /**
   * Create or claim a sandbox.
   *
   * @param {import('./client.d.ts').CreateSandboxOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async createSandbox(options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {
      templateID: options.templateId ?? 'cubeplex-ubuntu24-desktop',
    };
    if (options.timeoutSeconds !== undefined) {
      payload.timeout = options.timeoutSeconds;
    } else if (options.timeout !== undefined) {
      payload.timeout = options.timeout;
    }
    if (options.metadata !== undefined) {
      payload.metadata = options.metadata;
    }
    if (options.envVars !== undefined) {
      payload.envVars = options.envVars;
    }
    if (options.autoPause !== undefined) {
      payload.autoPause = options.autoPause;
    }
    if (options.autoResume !== undefined) {
      payload.autoResume = options.autoResume;
    }
    if (options.warmPoolRef !== undefined) {
      payload.warmPoolRef = options.warmPoolRef;
    }
    if (options.network !== undefined) {
      payload.network = options.network;
    }
    if (options.volumeMounts !== undefined) {
      payload.volumeMounts = options.volumeMounts;
    }

    /** @type {Record<string, unknown>} */
    const extension = {};
    if (options.volumeKey !== undefined) extension.volumeKey = options.volumeKey;
    if (options.rootfsSize !== undefined) extension.rootfsSize = options.rootfsSize;
    if (options.workspaceSize !== undefined) extension.workspaceSize = options.workspaceSize;
    if (options.networkPolicy !== undefined) extension.networkPolicy = options.networkPolicy;
    if (options.secretBindings !== undefined) extension.secretBindings = options.secretBindings;
    if (Object.keys(extension).length > 0) {
      payload.extension = extension;
    }

    const resp = await this._request('POST', '/sandboxes', {
      json: payload,
      signal: options.signal,
    });
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Fetch status and metadata for an existing sandbox.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async getSandbox(sandboxId, options = {}) {
    const resp = await this._request('GET', `/sandboxes/${encodeURIComponent(sandboxId)}`, {
      signal: options.signal,
      sandboxId,
    });
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * List sandboxes, optionally filtered by state and/or templateId.
   *
   * @param {import('./client.d.ts').ListSandboxesOptions} [filter]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo[]>}
   */
  async listSandboxes(filter = {}, options = {}) {
    const resp = await this._request('GET', '/sandboxes', {
      query: {
        state: filter.state,
        templateID: filter.templateId,
      },
      signal: options.signal ?? filter.signal,
    });
    const data = await readJsonOrUndefined(resp);
    const items = Array.isArray(data) ? data : (data?.sandboxes ?? []);
    return items.map(normalizeSandboxInfo);
  }

  /**
   * Connect to a sandbox, automatically resuming it if Paused.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async connectSandbox(sandboxId, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/connect`,
      { signal: options.signal, sandboxId },
    );
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Suspend compute while retaining RootFS Upper and Workspace block PVCs.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async pauseSandbox(sandboxId, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/pause`,
      { signal: options.signal, sandboxId },
    );
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Set or renew the sandbox idle auto-pause timeout.
   *
   * @param {string} sandboxId
   * @param {number} [timeoutSeconds=1800]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async setSandboxTimeout(sandboxId, timeoutSeconds = 1800, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/timeout`,
      {
        json: { timeoutSeconds },
        signal: options.signal,
        sandboxId,
      },
    );
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Refresh sandbox idle timeout via E2B-standard `/refreshes`.
   *
   * @param {string} sandboxId
   * @param {number} [durationSeconds=300]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async refreshSandbox(sandboxId, durationSeconds = 300, options = {}) {
    await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/refreshes`,
      {
        json: { duration: durationSeconds },
        signal: options.signal,
        sandboxId,
      },
    );
  }

  /**
   * Acquire or renew a lease protecting the sandbox against idle pause.
   *
   * @param {string} sandboxId
   * @param {number} [leaseSeconds=300]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async acquireLease(sandboxId, leaseSeconds = 300, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/lease`,
      {
        json: { leaseSeconds },
        signal: options.signal,
        sandboxId,
      },
    );
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Reset the persistent RootFS Upper PVC to a clean Base Image while preserving `/workspace`.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async resetRootfs(sandboxId, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/reset-rootfs`,
      { signal: options.signal, sandboxId },
    );
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Clone an existing sandbox via Copy-on-Write block snapshots.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').ForkSandboxOptions} [forkOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxInfo>}
   */
  async forkSandbox(sandboxId, forkOptions = {}, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {};
    if (forkOptions.volumeKey !== undefined) payload.volumeKey = forkOptions.volumeKey;
    if (forkOptions.metadata !== undefined) payload.metadata = forkOptions.metadata;

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/fork`,
      {
        json: Object.keys(payload).length > 0 ? payload : undefined,
        signal: options.signal ?? forkOptions.signal,
        sandboxId,
      },
    );
    const data = await readJsonOrUndefined(resp);
    return normalizeSandboxInfo(data ?? {});
  }

  /**
   * Terminate and delete a sandbox.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').DeleteSandboxOptions} [deleteOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteSandbox(sandboxId, deleteOptions = {}, options = {}) {
    await this._request('DELETE', `/sandboxes/${encodeURIComponent(sandboxId)}`, {
      query:
        deleteOptions.deleteVolumes !== undefined
          ? { deleteVolumes: deleteOptions.deleteVolumes }
          : undefined,
      signal: options.signal ?? deleteOptions.signal,
      sandboxId,
    });
  }

  /**
   * Get public ingress host/URL metadata for a sandbox port.
   *
   * @param {string} sandboxId
   * @param {number} port
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').EndpointHost>}
   */
  async getEndpointHost(sandboxId, port, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/endpoints/${port}`,
      { signal: options.signal, sandboxId },
    );
    const data = (await readJsonOrUndefined(resp)) ?? {};
    return {
      host: data.host ?? data.endpoint ?? '',
      url: data.url ?? (data.host ? `http://${data.host}` : ''),
      endpoint: data.endpoint ?? data.host ?? '',
      port: data.port ?? port,
      sandboxId: data.sandboxID ?? data.sandboxId ?? sandboxId,
    };
  }

  /**
   * Get the public host string for a sandbox port (E2B `get_host` compatible).
   *
   * @param {string} sandboxId
   * @param {number} port
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<string>}
   */
  async getHost(sandboxId, port, options = {}) {
    const info = await this.getEndpointHost(sandboxId, port, options);
    return info.host;
  }

  /**
   * Mint an HMAC-SHA256 signed `/panel/{token}/` reverse-proxy endpoint for a sandbox port.
   *
   * @param {string} sandboxId
   * @param {number} port
   * @param {import('./client.d.ts').CreateEndpointOptions} [endpointOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').PanelEndpoint>}
   */
  async createEndpoint(sandboxId, port, endpointOptions = {}, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {};
    if (endpointOptions.expiresInSeconds !== undefined) {
      payload.expiresInSeconds = endpointOptions.expiresInSeconds;
    }
    if (endpointOptions.targetUrl !== undefined) {
      payload.targetUrl = endpointOptions.targetUrl;
    }

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/endpoints/${port}`,
      {
        json: payload,
        signal: options.signal ?? endpointOptions.signal,
        sandboxId,
      },
    );
    const data = (await readJsonOrUndefined(resp)) ?? {};
    return {
      url: data.url ?? '',
      token: data.token ?? '',
      expiresAt: data.expiresAt,
    };
  }

  // ---------------------------------------------------------------------------
  // Commands
  // ---------------------------------------------------------------------------

  /**
   * Run a foreground or background shell command inside the sandbox.
   *
   * IMPORTANT: `uid` and `gid` are omitted from the JSON body unless explicitly
   * passed by the caller so the server/template `USER` is preserved.
   *
   * @param {string} sandboxId
   * @param {string} command
   * @param {import('./client.d.ts').RunCommandOptions} [options]
   * @returns {Promise<import('./client.d.ts').CommandResult>}
   */
  async runCommand(sandboxId, command, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {
      command,
      background: Boolean(options.background),
    };
    if (options.workdir !== undefined) payload.workdir = options.workdir;
    if (options.envs !== undefined) payload.envs = options.envs;
    if (options.uid !== undefined) payload.uid = options.uid;
    if (options.gid !== undefined) payload.gid = options.gid;

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/commands/run`,
      {
        json: payload,
        signal: options.signal,
        sandboxId,
      },
    );
    const data = (await readJsonOrUndefined(resp)) ?? {};
    return normalizeCommandResult(data);
  }

  /**
   * Inspect background command process status, PID, exit code, and log cursor.
   *
   * @param {string} sandboxId
   * @param {string} cmdId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').CommandResult>}
   */
  async getCommandStatus(sandboxId, cmdId, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/commands/${encodeURIComponent(cmdId)}/status`,
      { signal: options.signal, sandboxId },
    );
    const data = (await readJsonOrUndefined(resp)) ?? {};
    return normalizeCommandResult(data, cmdId);
  }

  /**
   * Read incremental command logs via stateless byte-offset `cursor`.
   *
   * @param {string} sandboxId
   * @param {string} cmdId
   * @param {number | import('./client.d.ts').GetCommandLogsOptions} [options]
   * @returns {Promise<import('./client.d.ts').CommandLogs>}
   */
  async getCommandLogs(sandboxId, cmdId, options = {}) {
    const opts = typeof options === 'number' ? { cursor: options } : options;
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/commands/${encodeURIComponent(cmdId)}/logs`,
      {
        query: opts.cursor !== undefined ? { cursor: opts.cursor } : undefined,
        signal: opts.signal,
        sandboxId,
      },
    );
    const data = (await readJsonOrUndefined(resp)) ?? {};
    const exitCode =
      data.exit_code !== undefined
        ? data.exit_code
        : data.exitCode !== undefined
          ? data.exitCode
          : undefined;
    return {
      id: data.id ?? data.cmdID ?? cmdId,
      cmdId: data.cmdID ?? data.id ?? cmdId,
      content: data.content ?? '',
      cursor: data.cursor ?? opts.cursor ?? 0,
      running: data.running ?? false,
      exitCode,
    };
  }

  /**
   * Send SIGKILL to a running command process group.
   *
   * @param {string} sandboxId
   * @param {string} cmdId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<{ id: string; killed: boolean }>}
   */
  async killCommand(sandboxId, cmdId, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/commands/${encodeURIComponent(cmdId)}/kill`,
      { signal: options.signal, sandboxId },
    );
    const data = (await readJsonOrUndefined(resp)) ?? {};
    return {
      id: data.id ?? cmdId,
      killed: Boolean(data.killed ?? true),
    };
  }

  // ---------------------------------------------------------------------------
  // Code Interpreter
  // ---------------------------------------------------------------------------

  /**
   * Execute code via the in-guest Jupyter/Code Interpreter, streaming NDJSON events.
   *
   * @param {string} sandboxId
   * @param {string} code
   * @param {import('./client.d.ts').RunCodeOptions} [options]
   * @returns {Promise<import('./client.d.ts').ExecutionResult>}
   */
  async runCode(sandboxId, code, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = { code };
    if (options.contextId !== undefined) payload.context_id = options.contextId;
    if (options.language !== undefined) payload.language = options.language;
    if (options.envVars !== undefined) payload.env_vars = options.envVars;

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/code/execute`,
      {
        json: payload,
        headers: { Accept: 'application/x-ndjson, application/json' },
        signal: options.signal,
        sandboxId,
      },
    );

    /** @type {{ stdout: string[]; stderr: string[] }} */
    const logs = { stdout: [], stderr: [] };
    /** @type {import('./client.d.ts').CodeResult[]} */
    const results = [];
    /** @type {import('./client.d.ts').CodeExecutionError | undefined} */
    let error;
    /** @type {number | undefined} */
    let executionCount;

    /**
     * @param {Record<string, any>} event
     */
    const handleEvent = async (event) => {
      if (!event || typeof event !== 'object') return;
      switch (event.type) {
        case 'stdout': {
          const text = typeof event.text === 'string' ? event.text : '';
          logs.stdout.push(text);
          if (options.onStdout) {
            await options.onStdout({
              text,
              line: text,
              timestamp: event.timestamp,
              error: false,
            });
          }
          break;
        }
        case 'stderr': {
          const text = typeof event.text === 'string' ? event.text : '';
          logs.stderr.push(text);
          if (options.onStderr) {
            await options.onStderr({
              text,
              line: text,
              timestamp: event.timestamp,
              error: true,
            });
          }
          break;
        }
        case 'result': {
          /** @type {import('./client.d.ts').CodeResult} */
          const resultObj = {
            text: event.text ?? undefined,
            png: event.png ?? undefined,
            html: event.html ?? undefined,
            isMainResult: event.is_main_result ?? event.isMainResult ?? true,
            raw: event,
          };
          results.push(resultObj);
          if (options.onResult) {
            await options.onResult(resultObj);
          }
          break;
        }
        case 'error': {
          error = {
            name: event.name ?? 'ExecutionError',
            value: event.value ?? '',
            traceback: event.traceback ?? '',
            exitCode: event.exitCode ?? event.exit_code,
          };
          if (options.onError) {
            await options.onError(error);
          }
          break;
        }
        case 'number_of_executions': {
          executionCount = event.execution_count ?? event.executionCount;
          break;
        }
        default:
          break;
      }
    };

    const contentType = resp.headers.get('content-type') ?? '';
    if (resp.body && !contentType.includes('application/json')) {
      await readNdjsonStream(resp.body, handleEvent);
    } else {
      const text = await resp.text();
      if (text.trim()) {
        try {
          const parsed = JSON.parse(text);
          if (Array.isArray(parsed)) {
            for (const item of parsed) {
              await handleEvent(item);
            }
          } else {
            await handleEvent(parsed);
          }
        } catch {
          for (const line of text.split('\n')) {
            const trimmed = line.trim();
            if (trimmed) {
              await handleEvent(JSON.parse(trimmed));
            }
          }
        }
      }
    }

    const stdout = logs.stdout.join('');
    const stderr = logs.stderr.join('');
    const lastResultText = results.length > 0 ? results[results.length - 1].text : undefined;

    return {
      logs,
      stdout,
      stderr,
      results,
      error,
      executionCount,
      text: lastResultText ?? (stdout || undefined),
    };
  }

  /**
   * Create a stateful Code Interpreter execution context.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').CreateContextOptions} [contextOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').CodeContext>}
   */
  async createContext(sandboxId, contextOptions = {}, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {};
    if (contextOptions.id !== undefined) payload.id = contextOptions.id;
    if (contextOptions.language !== undefined) payload.language = contextOptions.language;
    if (contextOptions.cwd !== undefined) payload.cwd = contextOptions.cwd;

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/code/contexts`,
      {
        json: payload,
        signal: options.signal ?? contextOptions.signal,
        sandboxId,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * List active Code Interpreter contexts inside a sandbox.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').CodeContext[]>}
   */
  async listContexts(sandboxId, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/code/contexts`,
      { signal: options.signal, sandboxId },
    );
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.contexts ?? []);
  }

  /**
   * Get metadata for a Code Interpreter context.
   *
   * @param {string} sandboxId
   * @param {string} contextId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').CodeContext>}
   */
  async getContext(sandboxId, contextId, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/code/contexts/${encodeURIComponent(contextId)}`,
      { signal: options.signal, sandboxId },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Restart a Code Interpreter context (clearing in-memory variables).
   *
   * @param {string} sandboxId
   * @param {string} contextId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').CodeContext>}
   */
  async restartContext(sandboxId, contextId, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/code/contexts/${encodeURIComponent(contextId)}/restart`,
      { signal: options.signal, sandboxId },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Delete a Code Interpreter context and terminate its worker process.
   *
   * @param {string} sandboxId
   * @param {string} contextId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteContext(sandboxId, contextId, options = {}) {
    await this._request(
      'DELETE',
      `/sandboxes/${encodeURIComponent(sandboxId)}/code/contexts/${encodeURIComponent(contextId)}`,
      { signal: options.signal, sandboxId },
    );
  }

  // ---------------------------------------------------------------------------
  // Files
  // ---------------------------------------------------------------------------

  /**
   * Write a file inside the sandbox.
   *
   * IMPORTANT: `uid` and `gid` query parameters are omitted unless explicitly passed
   * by the caller so the server/template `USER` is preserved.
   *
   * @param {string} sandboxId
   * @param {string} path
   * @param {string | Uint8Array | ArrayBuffer} content
   * @param {import('./client.d.ts').WriteFileOptions} [options]
   * @returns {Promise<{ status?: string; path: string; size: number }>}
   */
  async writeFile(sandboxId, path, content, options = {}) {
    /** @type {Uint8Array} */
    let data;
    if (typeof content === 'string') {
      data = textEncoder.encode(content);
    } else if (content instanceof Uint8Array) {
      data = content;
    } else if (content instanceof ArrayBuffer) {
      data = new Uint8Array(content);
    } else {
      throw new TypeError('content must be a string, Uint8Array, or ArrayBuffer');
    }

    /** @type {Record<string, string | number | undefined>} */
    const query = { path };
    if (options.uid !== undefined) query.uid = options.uid;
    if (options.gid !== undefined) query.gid = options.gid;
    if (options.mode !== undefined) query.mode = options.mode;

    const resp = await this._request(
      'PUT',
      `/sandboxes/${encodeURIComponent(sandboxId)}/files`,
      {
        query,
        body: data,
        headers: { 'Content-Type': 'application/octet-stream' },
        signal: options.signal,
        sandboxId,
      },
    );
    const res = (await readJsonOrUndefined(resp)) ?? {};
    return {
      status: res.status,
      path: res.path ?? path,
      size: res.size ?? data.byteLength,
    };
  }

  /**
   * Read raw file bytes from inside the sandbox.
   *
   * @param {string} sandboxId
   * @param {string} path
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<Uint8Array>}
   */
  async readFileBytes(sandboxId, path, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/files`,
      {
        query: { path },
        signal: options.signal,
        sandboxId,
      },
    );
    return new Uint8Array(await resp.arrayBuffer());
  }

  /**
   * Read a UTF-8 text file from inside the sandbox.
   *
   * @param {string} sandboxId
   * @param {string} path
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<string>}
   */
  async readFileText(sandboxId, path, options = {}) {
    const bytes = await this.readFileBytes(sandboxId, path, options);
    return textDecoder.decode(bytes);
  }

  /**
   * Remove a file or directory inside the sandbox.
   *
   * @param {string} sandboxId
   * @param {string} path
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<{ status?: string; deleted?: string }>}
   */
  async removeFile(sandboxId, path, options = {}) {
    const resp = await this._request(
      'DELETE',
      `/sandboxes/${encodeURIComponent(sandboxId)}/files`,
      {
        query: { path },
        signal: options.signal,
        sandboxId,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? { status: 'ok', deleted: path };
  }

  /**
   * List files and directories at `path` inside the sandbox.
   *
   * @param {string} sandboxId
   * @param {string} [path='/workspace']
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').FileEntry[]>}
   */
  async listFiles(sandboxId, path = '/workspace', options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/files/list`,
      {
        query: { path },
        signal: options.signal,
        sandboxId,
      },
    );
    const data = await readJsonOrUndefined(resp);
    const entries = Array.isArray(data) ? data : (data?.entries ?? []);
    return entries.map((/** @type {Record<string, any>} */ entry) => ({
      name: entry.name ?? '',
      path: entry.path ?? '',
      isDir: Boolean(entry.is_dir ?? entry.isDir ?? false),
      size: entry.size ?? 0,
      mode: entry.mode ?? '0644',
    }));
  }

  /**
   * Extract a tar.gz archive (or file map) into the sandbox in a single HTTP request.
   *
   * @param {string} sandboxId
   * @param {Uint8Array | ArrayBuffer | Record<string, string | Uint8Array | ArrayBuffer>} archiveOrFiles
   * @param {import('./client.d.ts').ArchiveSyncOptions} [options]
   * @returns {Promise<{ status: string; extracted?: number }>}
   */
  async archiveSync(sandboxId, archiveOrFiles, options = {}) {
    /** @type {Uint8Array} */
    let payloadBytes;
    if (archiveOrFiles instanceof Uint8Array) {
      payloadBytes = archiveOrFiles;
    } else if (archiveOrFiles instanceof ArrayBuffer) {
      payloadBytes = new Uint8Array(archiveOrFiles);
    } else if (archiveOrFiles && typeof archiveOrFiles === 'object') {
      payloadBytes = await buildTarGzBytes(archiveOrFiles, {
        uid: options.uid,
        gid: options.gid,
      });
    } else {
      throw new TypeError('archiveOrFiles must be a Uint8Array, ArrayBuffer, or file map');
    }

    /** @type {Record<string, string | number | undefined>} */
    const query = {
      dest: options.dest ?? '/workspace',
    };
    if (options.removePaths && options.removePaths.length > 0) {
      query.remove_paths = options.removePaths.join(',');
    }
    if (options.uid !== undefined) query.uid = options.uid;
    if (options.gid !== undefined) query.gid = options.gid;

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/files/archive-sync`,
      {
        query,
        body: payloadBytes,
        headers: { 'Content-Type': 'application/gzip' },
        signal: options.signal,
        sandboxId,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? { status: 'ok' };
  }

  // ---------------------------------------------------------------------------
  // Templates & WarmPools (CEP-0019)
  // ---------------------------------------------------------------------------

  /**
   * List templates from `/templates` (supports both E2B array and legacy wrapper).
   *
   * @param {import('./client.d.ts').ListTemplatesOptions} [options]
   * @returns {Promise<import('./client.d.ts').E2BTemplate[]>}
   */
  async listTemplates(options = {}) {
    const resp = await this._request('GET', '/templates', {
      query: options.teamId !== undefined ? { teamID: options.teamId } : undefined,
      signal: options.signal,
    });
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.templates ?? []);
  }

  /**
   * List templates with cursor pagination via `/v2/templates` and `X-Next-Token`.
   *
   * @param {import('./client.d.ts').ListTemplatesV2Options} [options]
   * @returns {Promise<import('./client.d.ts').ListTemplatesV2Response>}
   */
  async listTemplatesV2(options = {}) {
    const resp = await this._request('GET', '/v2/templates', {
      query: {
        limit: options.limit,
        nextToken: options.nextToken,
      },
      signal: options.signal,
    });
    const templates = (await readJsonOrUndefined(resp)) ?? [];
    const rawNextToken = resp.headers.get('X-Next-Token');
    return {
      templates: Array.isArray(templates) ? templates : (templates.templates ?? []),
      nextToken: rawNextToken ? rawNextToken : undefined,
    };
  }

  /**
   * Get a template (and its builds) by ID or alias, safely escaping slashes/colons.
   *
   * @param {string} templateIdOrAlias
   * @param {import('./client.d.ts').GetTemplateOptions} [options]
   * @returns {Promise<import('./client.d.ts').TemplateWithBuilds>}
   */
  async getTemplate(templateIdOrAlias, options = {}) {
    const path = options.byAlias
      ? `/templates/aliases/${encodeURIComponent(templateIdOrAlias)}`
      : `/templates/${encodeURIComponent(templateIdOrAlias)}`;
    const resp = await this._request('GET', path, { signal: options.signal });
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Resolve a template directly via `/templates/aliases/{alias}`.
   *
   * @param {string} alias
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').E2BTemplate>}
   */
  async getTemplateByAlias(alias, options = {}) {
    const resp = await this._request(
      'GET',
      `/templates/aliases/${encodeURIComponent(alias)}`,
      { signal: options.signal },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Request template creation or rebuild (`POST /v3/templates`, 202 Accepted).
   *
   * @param {import('./client.d.ts').CreateTemplateRequest} request
   * @param {import('./client.d.ts').CreateTemplateOptions} [options]
   * @returns {Promise<import('./client.d.ts').TemplateRequestBuildResponse>}
   */
  async createTemplate(request, options = {}) {
    /** @type {Record<string, string>} */
    const headers = {};
    if (options.idempotencyKey) {
      headers['Idempotency-Key'] = options.idempotencyKey;
    }

    const resp = await this._request('POST', '/v3/templates', {
      json: request,
      headers,
      signal: options.signal,
    });
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Start an image or recipe build for a template build ID (`POST /v2/templates/{templateID}/builds/{buildID}`, 202).
   *
   * @param {string} templateId
   * @param {string} buildId
   * @param {import('./client.d.ts').StartTemplateBuildRequest} request
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async startTemplateBuild(templateId, buildId, request, options = {}) {
    await this._request(
      'POST',
      `/v2/templates/${encodeURIComponent(templateId)}/builds/${encodeURIComponent(buildId)}`,
      {
        json: request,
        signal: options.signal,
      },
    );
  }

  /**
   * Poll build status and incremental logs (`GET /templates/{templateID}/builds/{buildID}/status`).
   *
   * @param {string} templateId
   * @param {string} buildId
   * @param {import('./client.d.ts').GetTemplateBuildStatusOptions} [options]
   * @returns {Promise<import('./client.d.ts').TemplateBuildInfo>}
   */
  async getTemplateBuildStatus(templateId, buildId, options = {}) {
    const resp = await this._request(
      'GET',
      `/templates/${encodeURIComponent(templateId)}/builds/${encodeURIComponent(buildId)}/status`,
      {
        query: {
          logsOffset: options.logsOffset,
          limit: options.limit,
          level: options.level,
        },
        signal: options.signal,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Read build log entries (`GET /templates/{templateID}/builds/{buildID}/logs`).
   *
   * @param {string} templateId
   * @param {string} buildId
   * @param {import('./client.d.ts').GetTemplateBuildLogsOptions} [options]
   * @returns {Promise<{ logs: import('./client.d.ts').BuildLogEntry[] }>}
   */
  async getTemplateBuildLogs(templateId, buildId, options = {}) {
    const resp = await this._request(
      'GET',
      `/templates/${encodeURIComponent(templateId)}/builds/${encodeURIComponent(buildId)}/logs`,
      {
        query: {
          cursor: options.cursor,
          limit: options.limit,
          direction: options.direction,
          level: options.level,
        },
        signal: options.signal,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? { logs: [] };
  }

  /**
   * Check build context cache presence and obtain an upload URL.
   *
   * @param {string} templateId
   * @param {string} hash
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').TemplateBuildFileUpload>}
   */
  async getTemplateFileUploadLink(templateId, hash, options = {}) {
    const resp = await this._request(
      'GET',
      `/templates/${encodeURIComponent(templateId)}/files/${encodeURIComponent(hash)}`,
      { signal: options.signal },
    );
    return (await readJsonOrUndefined(resp)) ?? { present: false };
  }

  /**
   * Upload a tar or gzip-compressed tar build context to the signed upload URL.
   *
   * @param {string} uploadUrl
   * @param {Uint8Array | ArrayBuffer} tarBytes
   * @param {{ headers?: Record<string, string>; signal?: AbortSignal }} [options]
   * @returns {Promise<void>}
   */
  async uploadTemplateFileContext(uploadUrl, tarBytes, options = {}) {
    const body = tarBytes instanceof ArrayBuffer ? new Uint8Array(tarBytes) : tarBytes;
    await this._request('PUT', uploadUrl, {
      body,
      headers: {
        'Content-Type': 'application/gzip',
        ...(options.headers ?? {}),
      },
      includeAuth: false,
      signal: options.signal,
    });
  }

  /**
   * Delete a template (`DELETE /templates/{templateID}`, 204 No Content).
   *
   * @param {string} templateId
   * @param {import('./client.d.ts').DeleteTemplateOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteTemplate(templateId, options = {}) {
    await this._request('DELETE', `/templates/${encodeURIComponent(templateId)}`, {
      query: options.force !== undefined ? { force: options.force } : undefined,
      signal: options.signal,
    });
  }

  /**
   * Update template public visibility (`PATCH /v2/templates/{templateID}`).
   *
   * @param {string} templateId
   * @param {boolean} isPublic
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<{ names: string[] }>}
   */
  async updateTemplateVisibility(templateId, isPublic, options = {}) {
    const resp = await this._request(
      'PATCH',
      `/v2/templates/${encodeURIComponent(templateId)}`,
      {
        json: { public: isPublic },
        signal: options.signal,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? { names: [] };
  }

  /**
   * Assign tags to a template build (`POST /templates/tags`, 201).
   *
   * @param {string} target
   * @param {string[]} tags
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async assignTemplateTags(target, tags, options = {}) {
    await this._request('POST', '/templates/tags', {
      json: { target, tags },
      signal: options.signal,
    });
  }

  /**
   * Remove tags from a template (`DELETE /templates/tags`, 204).
   *
   * @param {string} name
   * @param {string[]} tags
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteTemplateTags(name, tags, options = {}) {
    await this._request('DELETE', '/templates/tags', {
      json: { name, tags },
      signal: options.signal,
    });
  }

  /**
   * List assigned tags for a template (`GET /templates/{templateID}/tags`).
   *
   * @param {string} templateId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').TemplateTagInfo[]>}
   */
  async getTemplateTags(templateId, options = {}) {
    const resp = await this._request(
      'GET',
      `/templates/${encodeURIComponent(templateId)}/tags`,
      { signal: options.signal },
    );
    return (await readJsonOrUndefined(resp)) ?? [];
  }

  /**
   * Create or scale a PodWarmPool (`POST /warmpools`, 201).
   *
   * @param {import('./client.d.ts').CreateWarmPoolRequest} request
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').WarmPoolInfo>}
   */
  async createWarmPool(request, options = {}) {
    const resp = await this._request('POST', '/warmpools', {
      json: {
        name: request.name,
        templateRef: request.templateRef ?? request.templateId,
        replicas: request.replicas,
      },
      signal: options.signal,
    });
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * List active PodWarmPools (`GET /warmpools`).
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').WarmPoolInfo[]>}
   */
  async listWarmPools(options = {}) {
    const resp = await this._request('GET', '/warmpools', { signal: options.signal });
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.warmPools ?? data?.warmpools ?? []);
  }

  // ---------------------------------------------------------------------------
  // Desktop Stack (CEP-0021)
  // ---------------------------------------------------------------------------

  /**
   * Start or attach to the interactive desktop stack inside a sandbox.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').StartDesktopOptions} [desktopOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').DesktopSession>}
   */
  async startDesktop(sandboxId, desktopOptions = {}, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {
      expiresInSeconds: desktopOptions.expiresInSeconds ?? 3600,
    };
    if (desktopOptions.resolution !== undefined) payload.resolution = desktopOptions.resolution;
    if (desktopOptions.initialUrl !== undefined) payload.initialUrl = desktopOptions.initialUrl;
    if (desktopOptions.transport !== undefined) payload.transport = desktopOptions.transport;

    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/desktop/start`,
      {
        json: payload,
        signal: options.signal ?? desktopOptions.signal,
        sandboxId,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Fetch interactive desktop daemon status for a sandbox.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<Record<string, unknown>>}
   */
  async getDesktopStatus(sandboxId, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/desktop/status`,
      { signal: options.signal, sandboxId },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Stop the interactive desktop stack inside a sandbox.
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<Record<string, unknown>>}
   */
  async stopDesktop(sandboxId, options = {}) {
    const resp = await this._request(
      'POST',
      `/sandboxes/${encodeURIComponent(sandboxId)}/desktop/stop`,
      { signal: options.signal, sandboxId },
    );
    return (await readJsonOrUndefined(resp)) ?? { status: 'stopped' };
  }

  // ---------------------------------------------------------------------------
  // Sandbox Metrics
  // ---------------------------------------------------------------------------

  /**
   * Fetch resource usage metrics for a sandbox (`GET /sandboxes/{sandboxID}/metrics`).
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').GetMetricsOptions} [metricsOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SandboxMetric[]>}
   */
  async getMetrics(sandboxId, metricsOptions = {}, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/metrics`,
      {
        query: {
          start: formatMetricTime(metricsOptions.start),
          end: formatMetricTime(metricsOptions.end),
        },
        signal: options.signal ?? metricsOptions.signal,
        sandboxId,
      },
    );
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.metrics ?? []);
  }

  // ---------------------------------------------------------------------------
  // Secrets (CEP-0027)
  // ---------------------------------------------------------------------------

  /**
   * Return the official E2B secret placeholder ``${e2b.secrets.<name>}``.
   *
   * @param {string} name
   * @returns {string}
   */
  fillSecret(name) {
    return fillSecret(name);
  }

  /**
   * Create a new write-only secret (`POST /secrets`).
   *
   * @param {string} name
   * @param {string} value
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SecretInfo>}
   */
  async createSecret(name, value, options = {}) {
    const resp = await this._request('POST', '/secrets', {
      json: { name, value },
      signal: options.signal,
    });
    return normalizeSecretInfo((await readJsonOrUndefined(resp)) ?? {});
  }

  /**
   * List tenant secrets (`GET /secrets`, metadata only).
   *
   * @param {import('./client.d.ts').ListSecretsOptions} [listOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SecretInfo[]>}
   */
  async listSecrets(listOptions = {}, options = {}) {
    const resp = await this._request('GET', '/secrets', {
      query: {
        limit: listOptions.limit,
        nextToken: listOptions.nextToken,
      },
      signal: options.signal ?? listOptions.signal,
    });
    const data = await readJsonOrUndefined(resp);
    const items = Array.isArray(data) ? data : (data?.secrets ?? []);
    return items.map(normalizeSecretInfo);
  }

  /**
   * Get secret metadata (`GET /secrets/{secretID}`).
   *
   * @param {string} secretId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SecretInfo>}
   */
  async getSecret(secretId, options = {}) {
    const resp = await this._request('GET', `/secrets/${encodeURIComponent(secretId)}`, {
      signal: options.signal,
    });
    return normalizeSecretInfo((await readJsonOrUndefined(resp)) ?? {});
  }

  /**
   * Rotate/update a secret value (`POST /secrets/{secretID}`).
   *
   * @param {string} secretId
   * @param {string} value
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').SecretInfo>}
   */
  async updateSecret(secretId, value, options = {}) {
    const resp = await this._request('POST', `/secrets/${encodeURIComponent(secretId)}`, {
      json: { value },
      signal: options.signal,
    });
    return normalizeSecretInfo((await readJsonOrUndefined(resp)) ?? {});
  }

  /**
   * Delete a secret (`DELETE /secrets/{secretID}`).
   *
   * @param {string} secretId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteSecret(secretId, options = {}) {
    await this._request('DELETE', `/secrets/${encodeURIComponent(secretId)}`, {
      signal: options.signal,
    });
  }

  // ---------------------------------------------------------------------------
  // Volumes (CEP-0025 / CEP-0026)
  // ---------------------------------------------------------------------------

  /**
   * Create a durable E2B volume (`POST /volumes`).
   *
   * @param {string} name
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').VolumeInfo>}
   */
  async createVolume(name, options = {}) {
    const resp = await this._request('POST', '/volumes', {
      json: { name },
      signal: options.signal,
    });
    return normalizeVolumeInfo((await readJsonOrUndefined(resp)) ?? {});
  }

  /**
   * List durable E2B volumes (`GET /volumes`).
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').VolumeInfo[]>}
   */
  async listVolumes(options = {}) {
    const resp = await this._request('GET', '/volumes', { signal: options.signal });
    const data = await readJsonOrUndefined(resp);
    const items = Array.isArray(data) ? data : (data?.volumes ?? []);
    return items.map(normalizeVolumeInfo);
  }

  /**
   * Get volume metadata and content token (`GET /volumes/{volumeID}`).
   *
   * @param {string} volumeId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').VolumeInfo>}
   */
  async getVolume(volumeId, options = {}) {
    const resp = await this._request('GET', `/volumes/${encodeURIComponent(volumeId)}`, {
      signal: options.signal,
    });
    return normalizeVolumeInfo((await readJsonOrUndefined(resp)) ?? {});
  }

  /**
   * Delete an E2B volume (`DELETE /volumes/{volumeID}`).
   *
   * @param {string} volumeId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteVolume(volumeId, options = {}) {
    await this._request('DELETE', `/volumes/${encodeURIComponent(volumeId)}`, {
      signal: options.signal,
    });
  }

  /**
   * Write a file inside an E2B volume (`PUT /volumecontent/{volumeID}/file`).
   *
   * @param {string} volumeId
   * @param {string} path
   * @param {string | Uint8Array | ArrayBuffer} content
   * @param {import('./client.d.ts').WriteVolumeFileOptions} [options]
   * @returns {Promise<import('./client.d.ts').VolumeEntry>}
   */
  async writeVolumeFile(volumeId, path, content, options = {}) {
    let volToken = options.token;
    if (!volToken) {
      const vol = await this.getVolume(volumeId, { signal: options.signal });
      if (!vol.token) {
        throw new CubePodError(`Volume ${volumeId} did not return a content token`);
      }
      volToken = vol.token;
    }
    const body =
      typeof content === 'string'
        ? textEncoder.encode(content)
        : content instanceof ArrayBuffer
          ? new Uint8Array(content)
          : content;

    const resp = await this._request(
      'PUT',
      `/volumecontent/${encodeURIComponent(volumeId)}/file`,
      {
        query: {
          path,
          uid: options.uid,
          gid: options.gid,
          mode: options.mode,
          force: options.force ? 'true' : undefined,
        },
        body,
        headers: {
          Authorization: `Bearer ${volToken}`,
          'Content-Type': 'application/octet-stream',
        },
        includeAuth: false,
        signal: options.signal,
      },
    );
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Read raw bytes from a file inside an E2B volume (`GET /volumecontent/{volumeID}/file`).
   *
   * @param {string} volumeId
   * @param {string} path
   * @param {import('./client.d.ts').ReadVolumeFileOptions} [options]
   * @returns {Promise<Uint8Array>}
   */
  async readVolumeFile(volumeId, path, options = {}) {
    let volToken = options.token;
    if (!volToken) {
      const vol = await this.getVolume(volumeId, { signal: options.signal });
      if (!vol.token) {
        throw new CubePodError(`Volume ${volumeId} did not return a content token`);
      }
      volToken = vol.token;
    }
    const resp = await this._request(
      'GET',
      `/volumecontent/${encodeURIComponent(volumeId)}/file`,
      {
        query: { path },
        headers: {
          Authorization: `Bearer ${volToken}`,
        },
        includeAuth: false,
        signal: options.signal,
      },
    );
    const buf = await resp.arrayBuffer();
    return new Uint8Array(buf);
  }

  // ---------------------------------------------------------------------------
  // Events & Webhooks (CEP-0028)
  // ---------------------------------------------------------------------------

  /**
   * List tenant sandbox lifecycle events (`GET /events`).
   *
   * @param {import('./client.d.ts').ListEventsOptions} [filterOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').LifecycleEvent[]>}
   */
  async listEvents(filterOptions = {}, options = {}) {
    const resp = await this._request('GET', '/events', {
      query: {
        sandboxID: filterOptions.sandboxId,
        eventType: filterOptions.eventType,
        limit: filterOptions.limit,
        nextToken: filterOptions.nextToken,
      },
      signal: options.signal ?? filterOptions.signal,
    });
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.events ?? []);
  }

  /**
   * List lifecycle events for a single sandbox (`GET /sandboxes/{sandboxID}/events`).
   *
   * @param {string} sandboxId
   * @param {import('./client.d.ts').ListSandboxEventsOptions} [filterOptions]
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').LifecycleEvent[]>}
   */
  async listSandboxEvents(sandboxId, filterOptions = {}, options = {}) {
    const resp = await this._request(
      'GET',
      `/sandboxes/${encodeURIComponent(sandboxId)}/events`,
      {
        query: {
          eventType: filterOptions.eventType,
          limit: filterOptions.limit,
          nextToken: filterOptions.nextToken,
        },
        signal: options.signal ?? filterOptions.signal,
        sandboxId,
      },
    );
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.events ?? []);
  }

  /**
   * Register a webhook subscription (`POST /webhooks`).
   *
   * @param {import('./client.d.ts').CreateWebhookRequest} request
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').WebhookInfo>}
   */
  async createWebhook(request, options = {}) {
    const resp = await this._request('POST', '/webhooks', {
      json: {
        name: request.name,
        url: request.url,
        events: request.events,
        enabled: request.enabled ?? true,
      },
      signal: options.signal,
    });
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * List webhook subscriptions (`GET /webhooks`).
   *
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').WebhookInfo[]>}
   */
  async listWebhooks(options = {}) {
    const resp = await this._request('GET', '/webhooks', { signal: options.signal });
    const data = await readJsonOrUndefined(resp);
    return Array.isArray(data) ? data : (data?.webhooks ?? []);
  }

  /**
   * Get a single webhook subscription (`GET /webhooks/{webhookID}`).
   *
   * @param {string} webhookId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').WebhookInfo>}
   */
  async getWebhook(webhookId, options = {}) {
    const resp = await this._request('GET', `/webhooks/${encodeURIComponent(webhookId)}`, {
      signal: options.signal,
    });
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Update a webhook subscription or rotate its signing key (`PATCH /webhooks/{webhookID}`).
   *
   * @param {string} webhookId
   * @param {import('./client.d.ts').UpdateWebhookRequest} request
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<import('./client.d.ts').WebhookInfo>}
   */
  async updateWebhook(webhookId, request, options = {}) {
    /** @type {Record<string, unknown>} */
    const payload = {};
    if (request.url !== undefined) payload.url = request.url;
    if (request.events !== undefined) payload.events = request.events;
    if (request.enabled !== undefined) payload.enabled = request.enabled;
    if (request.rotateSecret !== undefined) payload.rotateSecret = request.rotateSecret;

    const resp = await this._request('PATCH', `/webhooks/${encodeURIComponent(webhookId)}`, {
      json: payload,
      signal: options.signal,
    });
    return (await readJsonOrUndefined(resp)) ?? {};
  }

  /**
   * Delete a webhook subscription (`DELETE /webhooks/{webhookID}`).
   *
   * @param {string} webhookId
   * @param {import('./client.d.ts').RequestOptions} [options]
   * @returns {Promise<void>}
   */
  async deleteWebhook(webhookId, options = {}) {
    await this._request('DELETE', `/webhooks/${encodeURIComponent(webhookId)}`, {
      signal: options.signal,
    });
  }
}

export const CubepodClient = CubePodClient;

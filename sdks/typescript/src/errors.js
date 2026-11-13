/**
 * Typed error hierarchy for the CubePod SDK.
 */

export class CubePodError extends Error {
  /**
   * @param {string} message
   * @param {{ cause?: unknown }} [options]
   */
  constructor(message, options = {}) {
    super(message, options.cause !== undefined ? { cause: options.cause } : undefined);
    this.name = 'CubePodError';
  }
}

export class CubePodAPIError extends CubePodError {
  /** @type {number} */
  statusCode;
  /** @type {string | number | undefined} */
  code;
  /** @type {string | undefined} */
  sandboxId;
  /** @type {unknown} */
  body;
  /** @type {string | undefined} */
  logPath;
  /** @type {string | undefined} */
  details;
  /** @type {string | undefined} */
  step;

  /**
   * @param {string} message
   * @param {{
   *   statusCode: number;
   *   code?: string | number;
   *   sandboxId?: string;
   *   body?: unknown;
   *   logPath?: string;
   *   details?: string;
   *   step?: string;
   *   cause?: unknown;
   * }} options
   */
  constructor(message, options) {
    super(message, { cause: options.cause });
    this.name = 'CubePodAPIError';
    this.statusCode = options.statusCode;
    this.code = options.code;
    this.sandboxId = options.sandboxId;
    this.body = options.body;
    this.logPath = options.logPath;
    this.details = options.details;
    this.step = options.step;
  }
}

/**
 * Parse an error Response into a CubePodAPIError.
 *
 * @param {Response} response
 * @param {{ sandboxId?: string }} [context]
 * @returns {Promise<CubePodAPIError>}
 */
export async function parseAPIError(response, context = {}) {
  let rawText = '';
  try {
    rawText = await response.text();
  } catch {
    rawText = '';
  }

  /** @type {unknown} */
  let body = rawText;
  /** @type {string | number | undefined} */
  let code;
  /** @type {string | undefined} */
  let sandboxId = context.sandboxId;
  /** @type {string | undefined} */
  let logPath;
  /** @type {string | undefined} */
  let details;
  /** @type {string | undefined} */
  let step;
  let message = rawText.trim() || response.statusText || `HTTP ${response.status}`;

  if (rawText) {
    try {
      const parsed = JSON.parse(rawText);
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        body = parsed;
        if (typeof parsed.code === 'string' || typeof parsed.code === 'number') {
          code = parsed.code;
        } else if (typeof parsed.error === 'string') {
          code = parsed.error;
        }
        if (typeof parsed.sandboxID === 'string') {
          sandboxId = parsed.sandboxID;
        } else if (typeof parsed.sandbox_id === 'string') {
          sandboxId = parsed.sandbox_id;
        }
        if (typeof parsed.logPath === 'string') {
          logPath = parsed.logPath;
        } else if (typeof parsed.log_path === 'string') {
          logPath = parsed.log_path;
        }
        if (typeof parsed.details === 'string') {
          details = parsed.details;
        }
        if (parsed.reason && typeof parsed.reason === 'object') {
          if (typeof parsed.reason.step === 'string') {
            step = parsed.reason.step;
          }
          if (typeof parsed.reason.message === 'string' && parsed.reason.message) {
            message = parsed.reason.message;
          }
        } else if (typeof parsed.message === 'string' && parsed.message) {
          message = parsed.message;
        } else if (typeof parsed.error === 'string' && parsed.error) {
          message = parsed.error;
        }
      }
    } catch {
      // Keep plain-text response body as message.
    }
  }

  return new CubePodAPIError(message, {
    statusCode: response.status,
    code,
    sandboxId,
    body,
    logPath,
    details,
    step,
  });
}

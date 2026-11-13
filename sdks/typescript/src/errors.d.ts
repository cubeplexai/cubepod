export class CubePodError extends Error {
  constructor(message: string, options?: { cause?: unknown });
}

export interface CubePodAPIErrorOptions {
  statusCode: number;
  code?: string | number;
  sandboxId?: string;
  body?: unknown;
  logPath?: string;
  details?: string;
  step?: string;
  cause?: unknown;
}

export class CubePodAPIError extends CubePodError {
  readonly statusCode: number;
  readonly code?: string | number;
  readonly sandboxId?: string;
  readonly body?: unknown;
  readonly logPath?: string;
  readonly details?: string;
  readonly step?: string;

  constructor(message: string, options: CubePodAPIErrorOptions);
}

export function parseAPIError(
  response: Response,
  context?: { sandboxId?: string },
): Promise<CubePodAPIError>;

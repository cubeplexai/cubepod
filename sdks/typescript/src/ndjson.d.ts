export function readNdjsonStream<T = Record<string, unknown>>(
  stream: ReadableStream<Uint8Array>,
  onItem: (item: T) => void | Promise<void>,
): Promise<void>;

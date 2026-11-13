/**
 * True streaming application/x-ndjson line reader handling arbitrary TCP chunk
 * boundaries and multi-byte UTF-8 sequences.
 */

import { CubePodError } from './errors.js';

/**
 * Stream and parse NDJSON objects from a ReadableStream<Uint8Array>.
 *
 * @template T
 * @param {ReadableStream<Uint8Array>} stream
 * @param {(item: T) => void | Promise<void>} onItem
 * @returns {Promise<void>}
 */
export async function readNdjsonStream(stream, onItem) {
  const reader = stream.getReader();
  const decoder = new TextDecoder('utf-8');
  let buffer = '';

  /**
   * @param {string} rawLine
   */
  const dispatchLine = async (rawLine) => {
    const trimmed = rawLine.trim();
    if (!trimmed) return;
    /** @type {T} */
    let parsed;
    try {
      parsed = JSON.parse(trimmed);
    } catch (cause) {
      throw new CubePodError(`Invalid NDJSON frame: ${trimmed}`, { cause });
    }
    await onItem(parsed);
  };

  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) {
        buffer += decoder.decode();
        break;
      }
      if (value && value.byteLength > 0) {
        buffer += decoder.decode(value, { stream: true });
        let nlIdx = buffer.indexOf('\n');
        while (nlIdx !== -1) {
          const line = buffer.slice(0, nlIdx);
          buffer = buffer.slice(nlIdx + 1);
          await dispatchLine(line);
          nlIdx = buffer.indexOf('\n');
        }
      }
    }

    if (buffer.trim().length > 0) {
      await dispatchLine(buffer);
    }
  } finally {
    reader.releaseLock();
  }
}

/**
 * Zero-dependency POSIX ustar tar.gz archive builder using Web CompressionStream.
 */

const textEncoder = new TextEncoder();

/**
 * Write an ASCII string into a fixed-length tar header slice.
 *
 * @param {Uint8Array} buf
 * @param {number} offset
 * @param {number} length
 * @param {string} value
 */
function writeString(buf, offset, length, value) {
  const encoded = textEncoder.encode(value);
  const count = Math.min(encoded.length, length);
  buf.set(encoded.subarray(0, count), offset);
}

/**
 * Write a zero-padded octal field into a tar header slice.
 *
 * @param {Uint8Array} buf
 * @param {number} offset
 * @param {number} length
 * @param {number} value
 */
function writeOctal(buf, offset, length, value) {
  const digits = Math.max(0, Math.floor(value)).toString(8);
  const maxDigits = length - 1;
  const padded = digits.slice(-maxDigits).padStart(maxDigits, '0');
  writeString(buf, offset, maxDigits, padded);
  buf[offset + maxDigits] = 0;
}

/**
 * Build an uncompressed POSIX ustar tar buffer from a path -> content map.
 *
 * @param {Record<string, string | Uint8Array | ArrayBuffer>} files
 * @param {{ uid?: number; gid?: number; mode?: number; mtimeSeconds?: number }} [options]
 * @returns {Uint8Array}
 */
export function buildTarBytes(files, options = {}) {
  const uid = options.uid ?? 1000;
  const gid = options.gid ?? 1000;
  const mode = options.mode ?? 0o644;
  const mtimeSeconds = options.mtimeSeconds ?? Math.floor(Date.now() / 1000);

  /** @type {Uint8Array[]} */
  const blocks = [];

  for (const [rawPath, content] of Object.entries(files)) {
    const relPath = rawPath.replace(/^\/+/, '');
    if (!relPath) continue;

    /** @type {Uint8Array} */
    let data;
    if (typeof content === 'string') {
      data = textEncoder.encode(content);
    } else if (content instanceof Uint8Array) {
      data = content;
    } else if (content instanceof ArrayBuffer) {
      data = new Uint8Array(content);
    } else {
      throw new TypeError(`Unsupported archive entry content for "${rawPath}"`);
    }

    const header = new Uint8Array(512);
    const pathBytes = textEncoder.encode(relPath);
    if (pathBytes.length <= 100) {
      header.set(pathBytes, 0);
    } else {
      const splitIdx = relPath.lastIndexOf('/');
      if (splitIdx <= 0) {
        throw new RangeError(`Archive entry path exceeds 100 bytes: "${relPath}"`);
      }
      const prefix = relPath.slice(0, splitIdx);
      const name = relPath.slice(splitIdx + 1);
      if (textEncoder.encode(prefix).length > 155 || textEncoder.encode(name).length > 100) {
        throw new RangeError(`Archive entry path exceeds ustar limits: "${relPath}"`);
      }
      writeString(header, 0, 100, name);
      writeString(header, 345, 155, prefix);
    }

    writeOctal(header, 100, 8, mode);
    writeOctal(header, 108, 8, uid);
    writeOctal(header, 116, 8, gid);
    writeOctal(header, 124, 12, data.length);
    writeOctal(header, 136, 12, mtimeSeconds);
    // Checksum field filled with spaces during checksum calculation.
    header.fill(0x20, 148, 156);
    header[156] = 0x30; // '0' regular file
    writeString(header, 257, 6, 'ustar\0');
    writeString(header, 263, 2, '00');

    let checksum = 0;
    for (let i = 0; i < 512; i += 1) {
      checksum += header[i];
    }
    const cksumStr = checksum.toString(8).padStart(6, '0');
    writeString(header, 148, 6, cksumStr);
    header[154] = 0;
    header[155] = 0x20;

    blocks.push(header);
    if (data.length > 0) {
      blocks.push(data);
      const remainder = data.length % 512;
      if (remainder !== 0) {
        blocks.push(new Uint8Array(512 - remainder));
      }
    }
  }

  // Two 512-byte zero blocks terminate the tar archive.
  blocks.push(new Uint8Array(1024));

  const totalLength = blocks.reduce((acc, part) => acc + part.length, 0);
  const tar = new Uint8Array(totalLength);
  let offset = 0;
  for (const part of blocks) {
    tar.set(part, offset);
    offset += part.length;
  }
  return tar;
}

/**
 * Compress a Uint8Array with gzip using Web Streams CompressionStream.
 *
 * @param {Uint8Array} input
 * @returns {Promise<Uint8Array>}
 */
export async function gzipBytes(input) {
  const cs = new CompressionStream('gzip');
  const writer = cs.writable.getWriter();
  await writer.write(/** @type {BufferSource} */ (input));
  await writer.close();

  const reader = cs.readable.getReader();
  /** @type {Uint8Array[]} */
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      if (value) {
        chunks.push(value);
        total += value.length;
      }
    }
  } finally {
    reader.releaseLock();
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

/**
 * Pack a map of file paths and contents into a gzip-compressed tar archive.
 *
 * @param {Record<string, string | Uint8Array | ArrayBuffer>} files
 * @param {{ uid?: number; gid?: number; mode?: number; mtimeSeconds?: number }} [options]
 * @returns {Promise<Uint8Array>}
 */
export async function buildTarGzBytes(files, options = {}) {
  const tar = buildTarBytes(files, options);
  return gzipBytes(tar);
}

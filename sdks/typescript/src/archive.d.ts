export interface TarArchiveOptions {
  uid?: number;
  gid?: number;
  mode?: number;
  mtimeSeconds?: number;
}

export function buildTarBytes(
  files: Record<string, string | Uint8Array | ArrayBuffer>,
  options?: TarArchiveOptions,
): Uint8Array;

export function gzipBytes(input: Uint8Array): Promise<Uint8Array>;

export function buildTarGzBytes(
  files: Record<string, string | Uint8Array | ArrayBuffer>,
  options?: TarArchiveOptions,
): Promise<Uint8Array>;

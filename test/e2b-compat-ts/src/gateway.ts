import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

export interface GatewayHandle {
  url: string;
  stop: () => Promise<void>;
}

function findFreePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const addr = srv.address();
      if (!addr || typeof addr === 'string') {
        srv.close(() => reject(new Error('failed to allocate TCP port')));
        return;
      }
      const { port } = addr;
      srv.close((err) => (err ? reject(err) : resolve(port)));
    });
  });
}

function findGatewayBinary(): string {
  const candidates = [
    path.resolve(process.cwd(), '../../bin/cubepod-gateway'),
    path.resolve(process.cwd(), 'bin/cubepod-gateway'),
    path.resolve(import.meta.dirname, '../../../bin/cubepod-gateway'),
    path.resolve(import.meta.dirname, '../../../../bin/cubepod-gateway'),
  ];
  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) {
      return candidate;
    }
  }
  throw new Error(`cubepod-gateway binary not found; checked: ${candidates.join(', ')}`);
}

async function waitForHealth(url: string, proc: ChildProcess, stderrChunks: string[]): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (proc.exitCode !== null) {
      throw new Error(`cubepod-gateway exited early (${proc.exitCode}): ${stderrChunks.join('')}`);
    }
    try {
      const resp = await fetch(`${url}/health`);
      if (resp.status === 200) {
        return;
      }
    } catch {
      // Gateway still binding
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`cubepod-gateway health check timed out: ${stderrChunks.join('')}`);
}

export async function startDirectGateway(): Promise<GatewayHandle> {
  const existing = process.env.CUBEPOD_GATEWAY_URL;
  if (existing) {
    process.env.E2B_API_URL = existing;
    process.env.E2B_SANDBOX_URL = existing;
    process.env.E2B_VOLUME_API_URL = existing;
    process.env.E2B_API_KEY ??= 'cpod_test_api_key';
    return {
      url: existing,
      stop: async () => {},
    };
  }

  const gwBin = findGatewayBinary();
  const port = await findFreePort();
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cubepod-ts-gw-'));

  const env: NodeJS.ProcessEnv = {
    ...process.env,
    TMPDIR: tempDir,
    CUBEPOD_WORKSPACE_DIR: path.join(tempDir, 'workspaces'),
    CUBEPOD_STATE: path.join(tempDir, 'state'),
    CUBEPOD_E2B_VOLUME_DIR: path.join(tempDir, 'volumes'),
    CUBEPOD_SECRETS_DIR: path.join(tempDir, 'secrets'),
    KUBECONFIG: path.join(tempDir, 'nonexistent-kubeconfig'),
  };
  delete env.KUBERNETES_SERVICE_HOST;
  delete env.KUBERNETES_SERVICE_PORT;
  delete env.E2B_DOMAIN;
  delete env.E2B_ACCESS_TOKEN;
  delete env.CUBEPOD_RUNTIME;
  delete env.CUBEPOD_SECRETS_BACKEND;
  delete env.CUBEPOD_EGRESS_MTLS_ADDR;
  delete env.CUBEPOD_API_KEYS;

  const proc = spawn(
    gwBin,
    [
      `-addr=127.0.0.1:${port}`,
      '-live-pod-routing=false',
      '-namespace=cubepod-test',
      '-e2b-template-api=true',
      '-template-build=false',
      `-state-dir=${tempDir}`,
    ],
    { env, stdio: ['ignore', 'ignore', 'pipe'] },
  );

  const stderrChunks: string[] = [];
  proc.stderr?.on('data', (chunk: Buffer) => stderrChunks.push(chunk.toString('utf8')));

  const killAndWait = async () => {
    if (proc.exitCode === null && proc.signalCode === null) {
      proc.kill('SIGTERM');
      await new Promise<void>((resolve) => {
        const timer = setTimeout(() => {
          if (proc.exitCode === null && proc.signalCode === null) {
            proc.kill('SIGKILL');
          }
          resolve();
        }, 3_000);
        proc.once('exit', () => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
    fs.rmSync(tempDir, { recursive: true, force: true });
  };

  const url = `http://127.0.0.1:${port}`;
  try {
    await waitForHealth(url, proc, stderrChunks);
  } catch (err) {
    if (proc.exitCode === null && proc.signalCode === null) {
      proc.kill('SIGKILL');
      await new Promise<void>((resolve) => proc.once('exit', () => resolve()));
    }
    fs.rmSync(tempDir, { recursive: true, force: true });
    throw err;
  }

  const prevEnv = {
    E2B_API_URL: process.env.E2B_API_URL,
    E2B_SANDBOX_URL: process.env.E2B_SANDBOX_URL,
    E2B_VOLUME_API_URL: process.env.E2B_VOLUME_API_URL,
    E2B_API_KEY: process.env.E2B_API_KEY,
  };

  process.env.E2B_API_URL = url;
  process.env.E2B_SANDBOX_URL = url;
  process.env.E2B_VOLUME_API_URL = url;
  process.env.E2B_API_KEY = 'cpod_test_api_key';

  return {
    url,
    stop: async () => {
      for (const [k, v] of Object.entries(prevEnv)) {
        if (v === undefined) {
          delete process.env[k];
        } else {
          process.env[k] = v;
        }
      }
      await killAndWait();
    },
  };
}

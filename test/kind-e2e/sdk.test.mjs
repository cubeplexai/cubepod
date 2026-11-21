// Official SDK, real remote gateway only. Patterns mirror test/e2b-compat-ts.
// On Kata Firecracker only, failures caused by issue #120 or #122 are documented
// skips and must be re-enabled when those issues are fixed. Other runtimes stay
// fail-closed: a skip still fails the check.
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { Sandbox, Volume, CommandExitError } from 'e2b';
import http from 'node:http';

// Refuse a direct invocation that could use real E2B Cloud credentials.
const localURL = new URL(process.env.E2B_API_URL || 'http://invalid');
assert.equal(localURL.protocol, 'http:');
assert.equal(localURL.hostname, '127.0.0.1');
assert.ok(localURL.port);
assert.equal(process.env.E2B_SANDBOX_URL, process.env.E2B_API_URL);
assert.equal(process.env.E2B_VOLUME_API_URL, process.env.E2B_API_URL);
assert.equal(process.env.E2B_API_KEY, 'cpod_kind_test_only');

const FC_ISSUE_120 = 'https://github.com/cubeplexai/cubepod/issues/120';
const FC_ISSUE_122 = 'https://github.com/cubeplexai/cubepod/issues/122';
const issue120Markers = [
  'failed to mount /dev/vdb',
  'EIO: I/O error',
  'EIO',
  'failed to ensure Pod',
  'timed out waiting for pod',
];

function isFirecracker() {
  return process.env.KATA_VMM === 'fc';
}

function exceptionText(err) {
  const parts = [];
  const seen = new Set();
  let current = err;
  while (current && !seen.has(current)) {
    seen.add(current);
    parts.push(String(current && current.message ? current.message : current));
    if (current.cause && current.cause !== current) {
      parts.push(String(current.cause.message || current.cause));
    }
    current = current.cause;
  }
  return parts.join('\n');
}

function isIssue120(err) {
  const text = exceptionText(err);
  return issue120Markers.some(marker => text.includes(marker));
}

async function withSandbox(t, fn, options = {}) {
  let sbx;
  try {
    sbx = await Sandbox.create('kind-e2e', { timeoutMs: 120000, requestTimeoutMs: 90000, ...options });
  } catch (err) {
    if (isFirecracker() && isIssue120(err)) {
      t.skip(`${FC_ISSUE_120}: Firecracker virtio-mmio hotplug race: ${err}`);
      return;
    }
    throw err;
  }
  try { await fn(sbx); } finally { assert.equal(await sbx.kill(), true); }
}

test('lifecycle, connect, stdout/stderr and nonzero exit', { timeout: 120000 }, async (t) => {
  await withSandbox(t, async (original) => {
    const sbx = await Sandbox.connect(original.sandboxId);
    assert.equal((await sbx.commands.run('hostname')).stdout.trim(), sbx.sandboxId);
    if (process.env.E2E_GUEST_KERNEL) assert.equal((await sbx.commands.run('uname -r')).stdout.trim(), process.env.E2E_GUEST_KERNEL);
    const result = await sbx.commands.run('printf ts-kind; printf err >&2');
    assert.equal(result.exitCode, 0);
    assert.equal(result.stdout, 'ts-kind');
    assert.equal(result.stderr, 'err');
    await assert.rejects(() => sbx.commands.run('exit 42'), (e) => e instanceof CommandExitError && e.exitCode === 42);
    assert.equal((await sbx.getInfo()).state, 'running');
  });
});

test('files mkdir/write/read/list and watch', { timeout: 120000 }, async (t) => {
  await withSandbox(t, async (sbx) => {
    const dir = '/workspace/ts-files';
    assert.equal(await sbx.files.makeDir(dir), true);
    const events = [];
    const watcher = await sbx.files.watchDir(dir, e => events.push(e));
    try {
      await sbx.files.write(`${dir}/hello.txt`, 'ts-kind');
      for (let i = 0; i < 100 && events.length === 0; i++) await new Promise(r => setTimeout(r, 100));
      assert.ok(events.length > 0, 'watchDir did not observe a real file write');
    } finally { await watcher.stop(); }
    assert.equal(await sbx.files.read(`${dir}/hello.txt`), 'ts-kind');
    assert.ok((await sbx.files.list(dir)).some(e => e.name === 'hello.txt'));
  });
});

test('background commands and PTY', { timeout: 120000 }, async (t) => {
  await withSandbox(t, async (sbx) => {
    const bg = await sbx.commands.run('sleep 60', { background: true });
    assert.ok(bg.pid > 0);
    assert.equal(await sbx.commands.kill(bg.pid), true);
    const chunks = [];
    const handle = await sbx.pty.create({ cols: 80, rows: 24, onData: d => chunks.push(Buffer.from(d)) });
    await sbx.pty.resize(handle.pid, { cols: 120, rows: 40 });
    await sbx.pty.sendInput(handle.pid, Buffer.from('echo TS_PTY_OK; exit\n'));
    const result = await handle.wait();
    assert.equal(result.exitCode, 0);
    assert.match(Buffer.concat(chunks).toString(), /TS_PTY_OK/);
  });
});

function requestPort(host) {
  return new Promise((resolve, reject) => {
    const req = http.get(`${process.env.E2B_API_URL}/probe`, { headers: { Host: host }, timeout: 2000 }, res => {
      let data = ''; res.on('data', chunk => data += chunk); res.on('end', () => resolve({ status: res.statusCode, data }));
    });
    req.on('error', reject); req.on('timeout', () => req.destroy(new Error('port request timeout')));
  });
}

test('outbound network and real exposed port', { timeout: 120000 }, async (t) => {
  await withSandbox(t, async (sbx) => {
    const result = await sbx.commands.run('curl --fail --max-time 10 http://network-target.cubepod-kind.svc:8080/hostname');
    assert.equal(result.exitCode, 0); assert.match(result.stdout, /network-target-/);
    await sbx.files.write('/workspace/probe', 'TS_PORT_OK');
    const server = await sbx.commands.run('python3 -u -m http.server 8080 --directory /workspace', { background: true });
    try {
      let response;
      for (let i = 0; i < 50; i++) {
        try { response = await requestPort(sbx.getHost(8080)); if (response.status === 200) break; } catch {}
        await new Promise(r => setTimeout(r, 100));
      }
      assert.deepEqual(response, { status: 200, data: 'TS_PORT_OK' });
    } finally { await sbx.commands.kill(server.pid); }
  });
});

test('volume mounts survive destruction and recreation', { timeout: 240000 }, async (t) => {
  if (isFirecracker()) {
    t.skip(`${FC_ISSUE_122}: E2B filesystem volume mounts do not persist on Kata Firecracker`);
    return;
  }
  const volume = await Volume.create(`kind-ts-${Date.now()}`);
  try {
    await withSandbox(t, async sbx => {
      await sbx.files.write('/data/persist.txt', 'persisted-ts');
      assert.equal((await sbx.commands.run('cat /data/persist.txt')).stdout, 'persisted-ts');
    }, { volumeMounts: { '/data': volume } });
    await withSandbox(t, async sbx => {
      assert.equal(await sbx.files.read('/data/persist.txt'), 'persisted-ts');
    }, { volumeMounts: { '/data': volume } });
  } finally { await Volume.destroy(volume.volumeId); }
});

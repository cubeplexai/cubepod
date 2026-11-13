import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { gunzipSync } from 'node:zlib';

import {
  CubePodAPIError,
  CubePodClient,
  CubePodError,
  Sandbox,
} from '../src/index.js';

/**
 * Read full request body as Buffer from an IncomingMessage.
 *
 * @param {http.IncomingMessage} req
 * @returns {Promise<Buffer>}
 */
function readBody(req) {
  return new Promise((resolve, reject) => {
    /** @type {Buffer[]} */
    const chunks = [];
    req.on('data', (chunk) => chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/**
 * Start a mock HTTP server and return { server, baseUrl, close }.
 *
 * @param {(req: http.IncomingMessage, res: http.ServerResponse) => void | Promise<void>} handler
 */
async function startServer(handler) {
  const server = http.createServer((req, res) => {
    Promise.resolve(handler(req, res)).catch((err) => {
      res.statusCode = 500;
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ message: err instanceof Error ? err.message : String(err) }));
    });
  });

  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === 'object');
  const baseUrl = `http://127.0.0.1:${addr.port}`;

  return {
    server,
    baseUrl,
    close: () => new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve()))),
  };
}

test('Sandbox lifecycle, resetRootfs, fork, lease, panel endpoint, and getHost(port)', async () => {
  /** @type {Array<{ method: string; url: string; auth?: string; body?: any }>} */
  const requests = [];
  let rootfsGen = 1;

  const srv = await startServer(async (req, res) => {
    const rawBody = await readBody(req);
    const bodyText = rawBody.toString('utf8');
    const parsed = bodyText ? JSON.parse(bodyText) : undefined;
    requests.push({
      method: req.method ?? '',
      url: req.url ?? '',
      auth: req.headers.authorization,
      body: parsed,
    });

    const u = new URL(req.url ?? '/', 'http://127.0.0.1');
    res.setHeader('Content-Type', 'application/json');

    if (req.method === 'GET' && u.pathname === '/health') {
      res.end(JSON.stringify({ status: 'ok', service: 'cubepod-gateway', sandboxes: 1, autoResumeCount: 0 }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes') {
      res.statusCode = 201;
      res.end(
        JSON.stringify({
          sandboxID: 'cpod-1',
          templateID: parsed.templateID,
          state: 'Running',
          envdPort: 49983,
          rootfsGeneration: rootfsGen,
          volumeKey: parsed.extension?.volumeKey,
          metadata: parsed.metadata,
        }),
      );
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes') {
      res.end(
        JSON.stringify([
          { sandboxID: 'cpod-1', templateID: 'ubuntu:24.04', state: 'Running', rootfsGeneration: rootfsGen },
        ]),
      );
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-1') {
      res.end(
        JSON.stringify({
          sandboxID: 'cpod-1',
          templateID: 'ubuntu:24.04',
          state: 'Running',
          rootfsGeneration: rootfsGen,
        }),
      );
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/pause') {
      res.end(JSON.stringify({ sandboxID: 'cpod-1', templateID: 'ubuntu:24.04', state: 'Paused', rootfsGeneration: rootfsGen }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/connect') {
      res.end(JSON.stringify({ sandboxID: 'cpod-1', templateID: 'ubuntu:24.04', state: 'Running', autoResumeCount: 1, rootfsGeneration: rootfsGen }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/timeout') {
      res.end(JSON.stringify({ sandboxID: 'cpod-1', templateID: 'ubuntu:24.04', state: 'Running', timeoutSeconds: parsed.timeoutSeconds }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/refreshes') {
      res.statusCode = 204;
      res.end();
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/lease') {
      res.end(JSON.stringify({ sandboxID: 'cpod-1', templateID: 'ubuntu:24.04', state: 'Running', leaseSeconds: parsed.leaseSeconds }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/reset-rootfs') {
      rootfsGen += 1;
      res.end(JSON.stringify({ sandboxID: 'cpod-1', templateID: 'ubuntu:24.04', state: 'Running', rootfsGeneration: rootfsGen }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/fork') {
      res.statusCode = 201;
      res.end(
        JSON.stringify({
          sandboxID: 'cpod-1-fork',
          templateID: 'ubuntu:24.04',
          state: 'Running',
          rootfsGeneration: rootfsGen,
          volumeKey: parsed?.volumeKey,
          metadata: parsed?.metadata,
        }),
      );
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-1/endpoints/8080') {
      res.end(
        JSON.stringify({
          host: '8080-cpod-1.cubepod.local',
          url: 'http://8080-cpod-1.cubepod.local',
          endpoint: '8080-cpod-1.cubepod.local',
          port: 8080,
          sandboxID: 'cpod-1',
        }),
      );
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-1/endpoints/8080') {
      res.end(
        JSON.stringify({
          url: '/panel/abc.sig/',
          token: 'abc.sig',
          expiresAt: '2026-11-13T10:00:00Z',
        }),
      );
      return;
    }
    if (req.method === 'DELETE' && u.pathname === '/sandboxes/cpod-1') {
      res.statusCode = 204;
      res.end();
      return;
    }

    res.statusCode = 404;
    res.end(JSON.stringify({ message: 'not found' }));
  });

  try {
    const client = new CubePodClient({ baseUrl: srv.baseUrl, apiKey: 'test-token-123' });

    const health = await client.health();
    assert.equal(health.status, 'ok');

    const sandbox = await Sandbox.create({
      client,
      templateId: 'ubuntu:24.04',
      volumeKey: 'ws-user-42',
      metadata: { env: 'ci' },
      timeout: 900,
    });
    assert.equal(sandbox.sandboxId, 'cpod-1');
    assert.equal(sandbox.templateId, 'ubuntu:24.04');
    assert.equal(sandbox.state, 'Running');
    assert.equal(sandbox.info.volumeKey, 'ws-user-42');

    const listed = await client.listSandboxes({ state: 'Running', templateId: 'ubuntu:24.04' });
    assert.equal(listed.length, 1);
    assert.equal(listed[0].sandboxId, 'cpod-1');

    await sandbox.pause();
    assert.equal(sandbox.state, 'Paused');

    const reconnected = await Sandbox.connect('cpod-1', { client });
    assert.equal(reconnected.state, 'Running');
    assert.equal(reconnected.info.autoResumeCount, 1);

    const timed = await reconnected.setTimeout(1200);
    assert.equal(timed.timeoutSeconds, 1200);

    await client.refreshSandbox('cpod-1', 600);

    const leased = await reconnected.acquireLease(450);
    assert.equal(leased.leaseSeconds, 450);

    const reset = await reconnected.resetRootfs();
    assert.equal(reset.rootfsGeneration, 2);

    const child = await reconnected.fork({ volumeKey: 'ws-child', metadata: { parent: 'cpod-1' } });
    assert.ok(child instanceof Sandbox);
    assert.equal(child.sandboxId, 'cpod-1-fork');
    assert.equal(child.info.volumeKey, 'ws-child');

    const host = await reconnected.getHost(8080);
    assert.equal(host, '8080-cpod-1.cubepod.local');

    const epHost = await reconnected.getEndpointHost(8080);
    assert.equal(epHost.url, 'http://8080-cpod-1.cubepod.local');
    assert.equal(epHost.port, 8080);

    const panel = await reconnected.createEndpoint(8080, { expiresInSeconds: 1800, targetUrl: 'http://127.0.0.1:8080' });
    assert.equal(panel.url, '/panel/abc.sig/');
    assert.equal(panel.token, 'abc.sig');
    assert.equal(panel.expiresAt, '2026-11-13T10:00:00Z');

    await reconnected.kill({ deleteVolumes: true });

    const delReq = requests.find((r) => r.method === 'DELETE' && r.url.startsWith('/sandboxes/cpod-1'));
    assert.ok(delReq);
    assert.equal(delReq.url, '/sandboxes/cpod-1?deleteVolumes=true');
    assert.ok(requests.every((r) => r.auth === 'Bearer test-token-123'));
  } finally {
    await srv.close();
  }
});

test('Commands and Files omit uid/gid by default and preserve explicit uid: 0 / gid: 0', async () => {
  /** @type {Array<{ method: string; path: string; searchParams: URLSearchParams; jsonBody?: any; rawBody: Buffer }>} */
  const captured = [];

  const srv = await startServer(async (req, res) => {
    const rawBody = await readBody(req);
    const u = new URL(req.url ?? '/', 'http://127.0.0.1');
    const isJson = (req.headers['content-type'] ?? '').includes('application/json');
    captured.push({
      method: req.method ?? '',
      path: u.pathname,
      searchParams: u.searchParams,
      jsonBody: isJson && rawBody.length > 0 ? JSON.parse(rawBody.toString('utf8')) : undefined,
      rawBody,
    });

    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-2/commands/run') {
      res.setHeader('Content-Type', 'application/json');
      const body = JSON.parse(rawBody.toString('utf8'));
      if (body.background) {
        res.end(JSON.stringify({ id: 'cmd-bg-1', cmdID: 'cmd-bg-1', running: true, pid: 4242 }));
      } else {
        res.end(JSON.stringify({ id: 'cmd-fg-1', exitCode: 0, stdout: 'ok\n', stderr: '' }));
      }
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-2/commands/cmd-bg-1/status') {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ id: 'cmd-bg-1', pid: 4242, running: true, cursor: 12 }));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-2/commands/cmd-bg-1/logs') {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ id: 'cmd-bg-1', content: 'line2\n', cursor: 18, running: false, exitCode: 0 }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-2/commands/cmd-bg-1/kill') {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ id: 'cmd-bg-1', killed: true }));
      return;
    }
    if (req.method === 'PUT' && u.pathname === '/sandboxes/cpod-2/files') {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ status: 'ok', path: u.searchParams.get('path'), size: rawBody.length }));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-2/files') {
      res.setHeader('Content-Type', 'application/octet-stream');
      res.end(Buffer.from([0xde, 0xad, 0xbe, 0xef]));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-2/files/list') {
      res.setHeader('Content-Type', 'application/json');
      res.end(
        JSON.stringify({
          path: u.searchParams.get('path'),
          entries: [{ name: 'hello.txt', path: '/workspace/hello.txt', is_dir: false, size: 5, mode: '0644' }],
        }),
      );
      return;
    }
    if (req.method === 'DELETE' && u.pathname === '/sandboxes/cpod-2/files') {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ status: 'ok', deleted: u.searchParams.get('path') }));
      return;
    }

    res.statusCode = 404;
    res.end();
  });

  try {
    const client = new CubePodClient({ baseUrl: srv.baseUrl });
    const sandbox = new Sandbox(client, {
      sandboxId: 'cpod-2',
      templateId: 'custom-tpl',
      state: 'Running',
      envdPort: 49983,
      rootfsGeneration: 1,
    });

    // 1. Command without uid/gid -> omitted from JSON body
    const fg = await sandbox.commands.run('whoami');
    assert.equal(fg.exitCode, 0);
    assert.equal(fg.stdout, 'ok\n');
    const cmdDefaultReq = captured[0];
    assert.equal('uid' in cmdDefaultReq.jsonBody, false);
    assert.equal('gid' in cmdDefaultReq.jsonBody, false);
    assert.equal(cmdDefaultReq.jsonBody.background, false);

    // 2. Command with explicit uid: 0, gid: 0 -> preserved!
    await sandbox.commands.run('id', { uid: 0, gid: 0, workdir: '/root', envs: { A: '1' } });
    const cmdRootReq = captured[1];
    assert.equal(cmdRootReq.jsonBody.uid, 0);
    assert.equal(cmdRootReq.jsonBody.gid, 0);
    assert.equal(cmdRootReq.jsonBody.workdir, '/root');
    assert.deepEqual(cmdRootReq.jsonBody.envs, { A: '1' });

    // 3. Background command status, cursor logs, kill
    const bg = await sandbox.commands.run('tail -f /var/log/app.log', { background: true });
    assert.equal(bg.running, true);
    assert.equal(bg.pid, 4242);

    const status = await sandbox.commands.getStatus(bg.id);
    assert.equal(status.cursor, 12);

    const logs = await sandbox.commands.getLogs(bg.id, { cursor: 12 });
    assert.equal(captured[4].searchParams.get('cursor'), '12');
    assert.equal(logs.content, 'line2\n');
    assert.equal(logs.cursor, 18);
    assert.equal(logs.exitCode, 0);

    const killed = await sandbox.commands.kill(bg.id);
    assert.equal(killed.killed, true);

    // 4. File write without uid/gid -> query params omitted!
    await sandbox.files.write('/workspace/hello.txt', 'hello');
    const fileDefaultReq = captured[6];
    assert.equal(fileDefaultReq.searchParams.get('path'), '/workspace/hello.txt');
    assert.equal(fileDefaultReq.searchParams.has('uid'), false);
    assert.equal(fileDefaultReq.searchParams.has('gid'), false);
    assert.equal(fileDefaultReq.searchParams.has('mode'), false);
    assert.equal(fileDefaultReq.rawBody.toString('utf8'), 'hello');

    // 5. File write with explicit uid: 0, gid: 0, mode: '0600' and Uint8Array
    await sandbox.files.write('/etc/secret.conf', new Uint8Array([1, 2, 3]), {
      uid: 0,
      gid: 0,
      mode: '0600',
    });
    const fileRootReq = captured[7];
    assert.equal(fileRootReq.searchParams.get('uid'), '0');
    assert.equal(fileRootReq.searchParams.get('gid'), '0');
    assert.equal(fileRootReq.searchParams.get('mode'), '0600');
    assert.deepEqual([...fileRootReq.rawBody], [1, 2, 3]);

    // 6. readBytes, readText, list, remove
    const bytes = await sandbox.files.readBytes('/workspace/bin.dat');
    assert.deepEqual([...bytes], [0xde, 0xad, 0xbe, 0xef]);

    const list = await sandbox.files.list('/workspace');
    assert.equal(list.length, 1);
    assert.equal(list[0].isDir, false);
    assert.equal(list[0].name, 'hello.txt');

    const removed = await sandbox.files.remove('/workspace/hello.txt');
    assert.equal(removed.deleted, '/workspace/hello.txt');
  } finally {
    await srv.close();
  }
});

test('readText decodes UTF-8, transient 502 retries succeed, and positional Sandbox.create works', async () => {
  let getAttempts = 0;

  const srv = await startServer(async (req, res) => {
    const rawBody = await readBody(req);
    const u = new URL(req.url ?? '/', 'http://127.0.0.1');
    res.setHeader('Content-Type', 'application/json');

    if (req.method === 'POST' && u.pathname === '/sandboxes') {
      const parsed = JSON.parse(rawBody.toString('utf8'));
      res.statusCode = 201;
      res.end(JSON.stringify({ sandboxID: 'cpod-pos', templateID: parsed.templateID, state: 'Running' }));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-pos/files') {
      getAttempts += 1;
      if (getAttempts === 1) {
        res.statusCode = 502;
        res.end(JSON.stringify({ message: 'bad gateway' }));
        return;
      }
      res.setHeader('Content-Type', 'text/plain; charset=utf-8');
      res.end('hello utf8 🚀');
      return;
    }
    if (req.method === 'GET' && u.pathname === '/templates') {
      res.end(JSON.stringify({ templates: [{ id: 'cubeplex-ubuntu24-desktop', name: 'Desktop' }] }));
      return;
    }
    res.statusCode = 404;
    res.end();
  });

  try {
    const sandbox = await Sandbox.create('python-tools', { baseUrl: srv.baseUrl, retryDelayMs: 1 });
    assert.equal(sandbox.templateId, 'python-tools');

    const text = await sandbox.files.readText('/workspace/greeting.txt');
    assert.equal(text, 'hello utf8 🚀');
    assert.equal(getAttempts, 2);

    const legacyTemplates = await sandbox.client.listTemplates();
    assert.equal(legacyTemplates.length, 1);
  } finally {
    await srv.close();
  }
});

test('archiveSync sends binary tar.gz payload and remove_paths query param', async () => {
  /** @type {Array<{ url: string; contentType?: string; body: Buffer }>} */
  const syncRequests = [];

  const srv = await startServer(async (req, res) => {
    const rawBody = await readBody(req);
    syncRequests.push({
      url: req.url ?? '',
      contentType: req.headers['content-type'],
      body: rawBody,
    });
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ status: 'ok', extracted: 2 }));
  });

  try {
    const client = new CubePodClient({ baseUrl: srv.baseUrl });
    const sandbox = new Sandbox(client, {
      sandboxId: 'cpod-3',
      templateId: 'base',
      state: 'Running',
      envdPort: 49983,
      rootfsGeneration: 1,
    });

    // Raw binary tar.gz bytes
    const rawArchive = new Uint8Array([0x1f, 0x8b, 0x08, 0x00, 0x01, 0x02]);
    const res1 = await sandbox.files.archiveSync(rawArchive, {
      dest: '/workspace/skills',
      removePaths: ['old-skill', 'stale.txt'],
    });
    assert.equal(res1.extracted, 2);

    const u1 = new URL(syncRequests[0].url, 'http://127.0.0.1');
    assert.equal(u1.pathname, '/sandboxes/cpod-3/files/archive-sync');
    assert.equal(u1.searchParams.get('dest'), '/workspace/skills');
    assert.equal(u1.searchParams.get('remove_paths'), 'old-skill,stale.txt');
    assert.equal(u1.searchParams.has('uid'), false);
    assert.equal(syncRequests[0].contentType, 'application/gzip');
    assert.deepEqual([...syncRequests[0].body], [...rawArchive]);

    // Object map auto-packed to valid tar.gz
    await sandbox.files.archiveSync(
      { 'my-skill/SKILL.md': '# Skill\nhello' },
      { dest: '/workspace' },
    );
    const tarBuf = gunzipSync(syncRequests[1].body);
    assert.ok(tarBuf.toString('utf8').includes('my-skill/SKILL.md'));
    assert.ok(tarBuf.toString('utf8').includes('# Skill\nhello'));
  } finally {
    await srv.close();
  }
});

test('runCode streams NDJSON across mid-line TCP chunk splits and parses png/html/callbacks + contexts', async () => {
  const srv = await startServer(async (req, res) => {
    const rawBody = await readBody(req);
    const u = new URL(req.url ?? '/', 'http://127.0.0.1');

    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-4/code/execute') {
      const payload = JSON.parse(rawBody.toString('utf8'));
      assert.equal(payload.code, 'print("hi 🚀")\nplot()');
      assert.equal(payload.context_id, 'ctx-9');

      res.statusCode = 200;
      res.setHeader('Content-Type', 'application/x-ndjson');

      const lines = [
        JSON.stringify({ type: 'stdout', text: 'hi 🚀\n', timestamp: 1000 }),
        JSON.stringify({ type: 'stderr', text: 'warn: deprecated\n', timestamp: 1001 }),
        JSON.stringify({
          type: 'result',
          text: '<Figure size 640x480>',
          png: 'iVBORw0KGgoAAAANSUhEUg==',
          html: '<div>chart</div>',
        }),
        JSON.stringify({
          type: 'error',
          name: 'ZeroDivisionError',
          value: 'division by zero',
          traceback: 'Traceback...',
        }),
        JSON.stringify({ type: 'number_of_executions', execution_count: 3 }),
      ].join('\n'); // No trailing newline to verify final buffer flush!

      // Intentionally split mid-JSON-line and mid-UTF-8 emoji!
      const bytes = Buffer.from(lines, 'utf8');
      const splitPoints = [11, 27, 30, 58, 120, 195];
      let prev = 0;
      for (const pt of splitPoints) {
        if (pt < bytes.length) {
          res.write(bytes.subarray(prev, pt));
          await new Promise((r) => setTimeout(r, 2));
          prev = pt;
        }
      }
      res.end(bytes.subarray(prev));
      return;
    }

    res.setHeader('Content-Type', 'application/json');
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-4/code/contexts') {
      res.end(JSON.stringify({ id: 'ctx-9', language: 'python', cwd: '/workspace' }));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-4/code/contexts') {
      res.end(JSON.stringify([{ id: 'ctx-9', language: 'python', cwd: '/workspace' }]));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/sandboxes/cpod-4/code/contexts/ctx-9') {
      res.end(JSON.stringify({ id: 'ctx-9', language: 'python', cwd: '/workspace' }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes/cpod-4/code/contexts/ctx-9/restart') {
      res.end(JSON.stringify({ id: 'ctx-9', language: 'python', cwd: '/workspace' }));
      return;
    }
    if (req.method === 'DELETE' && u.pathname === '/sandboxes/cpod-4/code/contexts/ctx-9') {
      res.end(JSON.stringify({ status: 'ok', id: 'ctx-9' }));
      return;
    }

    res.statusCode = 404;
    res.end();
  });

  try {
    const client = new CubePodClient({ baseUrl: srv.baseUrl });
    const sandbox = new Sandbox(client, {
      sandboxId: 'cpod-4',
      templateId: 'base',
      state: 'Running',
      envdPort: 49983,
      rootfsGeneration: 1,
    });

    const ctx = await sandbox.code.createContext({ id: 'ctx-9', language: 'python', cwd: '/workspace' });
    assert.equal(ctx.id, 'ctx-9');
    assert.equal((await sandbox.code.listContexts()).length, 1);
    assert.equal((await sandbox.code.getContext('ctx-9')).cwd, '/workspace');

    /** @type {string[]} */
    const seenStdout = [];
    /** @type {string[]} */
    const seenStderr = [];
    /** @type {any[]} */
    const seenResults = [];
    /** @type {any[]} */
    const seenErrors = [];

    const execution = await sandbox.code.runCode('print("hi 🚀")\nplot()', {
      contextId: 'ctx-9',
      onStdout: (m) => { seenStdout.push(m.text); },
      onStderr: (m) => { seenStderr.push(m.text); },
      onResult: (r) => { seenResults.push(r); },
      onError: (e) => { seenErrors.push(e); },
    });

    assert.deepEqual(seenStdout, ['hi 🚀\n']);
    assert.deepEqual(seenStderr, ['warn: deprecated\n']);
    assert.equal(seenResults.length, 1);
    assert.equal(seenResults[0].png, 'iVBORw0KGgoAAAANSUhEUg==');
    assert.equal(seenResults[0].html, '<div>chart</div>');
    assert.equal(seenErrors.length, 1);
    assert.equal(seenErrors[0].name, 'ZeroDivisionError');

    assert.equal(execution.stdout, 'hi 🚀\n');
    assert.equal(execution.stderr, 'warn: deprecated\n');
    assert.equal(execution.results[0].png, 'iVBORw0KGgoAAAANSUhEUg==');
    assert.equal(execution.results[0].html, '<div>chart</div>');
    assert.equal(execution.error?.value, 'division by zero');
    assert.equal(execution.executionCount, 3);
    assert.equal(execution.text, '<Figure size 640x480>');

    await sandbox.code.restartContext('ctx-9');
    await sandbox.code.deleteContext('ctx-9');
  } finally {
    await srv.close();
  }
});

test('Template v2/v3 catalog, encodeURIComponent alias escaping, X-Next-Token, 202/204, WarmPools, and typed errors', async () => {
  /** @type {string[]} */
  const rawUrls = [];

  const srv = await startServer(async (req, res) => {
    const rawBody = await readBody(req);
    rawUrls.push(`${req.method} ${req.url}`);
    const u = new URL(req.url ?? '/', 'http://127.0.0.1');
    res.setHeader('Content-Type', 'application/json');

    if (req.method === 'GET' && u.pathname === '/templates') {
      res.end(
        JSON.stringify([
          {
            templateID: 'tpl_1',
            buildID: 'bld_1',
            cpuCount: 2,
            memoryMB: 2048,
            diskSizeMB: 10240,
            public: false,
            aliases: ['my-tpl'],
            names: ['my-tpl:v1'],
            createdAt: '2026-11-13T00:00:00Z',
            updatedAt: '2026-11-13T00:00:00Z',
            createdBy: null,
            lastSpawnedAt: null,
            spawnCount: 0,
            buildCount: 1,
            envdVersion: 'cubepod-podd',
            buildStatus: 'ready',
          },
        ]),
      );
      return;
    }
    if (req.method === 'GET' && u.pathname === '/v2/templates') {
      if (!u.searchParams.get('nextToken')) {
        res.setHeader('X-Next-Token', 'cursor-page-2');
      }
      res.end(JSON.stringify([{ templateID: 'tpl_page1', buildID: 'bld_1', buildStatus: 'ready' }]));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/templates/org%2Fname%3Av1') {
      res.end(
        JSON.stringify({
          templateID: 'tpl_org',
          buildID: 'bld_org',
          names: ['org/name:v1'],
          builds: [{ buildID: 'bld_org', status: 'ready' }],
        }),
      );
      return;
    }
    if (req.method === 'GET' && u.pathname === '/templates/aliases/org%2Fname%3Av1') {
      res.end(JSON.stringify({ templateID: 'tpl_org', aliases: ['org/name:v1'] }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/v3/templates') {
      assert.equal(req.headers['idempotency-key'], 'idem-123');
      res.statusCode = 202;
      res.end(
        JSON.stringify({
          templateID: 'tpl_new',
          buildID: 'bld_new',
          public: false,
          aliases: ['py-tools'],
          names: ['py-tools', 'py-tools:v1'],
          tags: ['v1'],
        }),
      );
      return;
    }
    if (req.method === 'POST' && u.pathname === '/v2/templates/tpl_new/builds/bld_new') {
      res.statusCode = 202;
      res.end();
      return;
    }
    if (req.method === 'GET' && u.pathname === '/templates/tpl_new/builds/bld_new/status') {
      res.end(
        JSON.stringify({
          templateID: 'tpl_new',
          buildID: 'bld_new',
          status: 'error',
          reason: {
            message: 'pip install failed with exit code 1',
            step: 'RUN pip install -r requirements.txt',
          },
          logs: ['step 1 failed'],
          logEntries: [{ timestamp: '2026-11-13T00:00:00Z', level: 'error', message: 'step 1 failed' }],
        }),
      );
      return;
    }
    if (req.method === 'PATCH' && u.pathname === '/v2/templates/tpl_new') {
      const parsed = JSON.parse(rawBody.toString('utf8'));
      assert.equal(parsed.public, true);
      res.end(JSON.stringify({ names: ['py-tools', 'py-tools:v1'] }));
      return;
    }
    if (req.method === 'DELETE' && u.pathname === '/templates/tpl_new') {
      assert.equal(u.searchParams.get('force'), 'true');
      res.statusCode = 204;
      res.end();
      return;
    }
    if (req.method === 'POST' && u.pathname === '/warmpools') {
      res.statusCode = 201;
      res.end(JSON.stringify({ name: 'pool-a', templateRef: 'py-tools', replicas: 3, readyReplicas: 3 }));
      return;
    }
    if (req.method === 'GET' && u.pathname === '/warmpools') {
      res.end(JSON.stringify({ warmPools: [{ name: 'pool-a', templateRef: 'py-tools', replicas: 3 }], totalReady: 3 }));
      return;
    }
    if (req.method === 'POST' && u.pathname === '/sandboxes') {
      res.statusCode = 503;
      res.end(
        JSON.stringify({
          code: 503,
          message: 'template ready command timed out',
          sandboxID: 'cpod-failed-1',
          logPath: '/var/log/cubepod/start-cmd.log',
        }),
      );
      return;
    }
    if (req.method === 'GET' && u.pathname === '/templates/missing') {
      res.statusCode = 404;
      res.end(JSON.stringify({ code: 'template_not_found', reason: { message: 'template "missing" not found', step: 'lookup' } }));
      return;
    }

    res.statusCode = 404;
    res.end(JSON.stringify({ message: 'unhandled' }));
  });

  try {
    const client = new CubePodClient({ baseUrl: srv.baseUrl });

    const list1 = await client.listTemplates();
    assert.equal(list1.length, 1);
    assert.equal(list1[0].templateID, 'tpl_1');

    const page1 = await client.listTemplatesV2({ limit: 50 });
    assert.equal(page1.templates.length, 1);
    assert.equal(page1.nextToken, 'cursor-page-2');

    const page2 = await client.listTemplatesV2({ limit: 50, nextToken: page1.nextToken });
    assert.equal(page2.nextToken, undefined);

    const tplByEscaped = await client.getTemplate('org/name:v1');
    assert.equal(tplByEscaped.templateID, 'tpl_org');
    assert.equal(tplByEscaped.builds[0].status, 'ready');
    assert.ok(rawUrls.includes('GET /templates/org%2Fname%3Av1'));

    const byAlias = await client.getTemplateByAlias('org/name:v1');
    assert.equal(byAlias.templateID, 'tpl_org');
    assert.ok(rawUrls.includes('GET /templates/aliases/org%2Fname%3Av1'));

    const created = await client.createTemplate(
      { name: 'py-tools', tags: ['v1'], cpuCount: 2, memoryMB: 2048 },
      { idempotencyKey: 'idem-123' },
    );
    assert.equal(created.templateID, 'tpl_new');
    assert.equal(created.buildID, 'bld_new');

    await client.startTemplateBuild('tpl_new', 'bld_new', {
      fromImage: 'python:3.13',
      steps: [{ type: 'RUN', args: ['pip install -r requirements.txt'] }],
    });

    const buildStatus = await client.getTemplateBuildStatus('tpl_new', 'bld_new', { logsOffset: 0, limit: 50 });
    assert.equal(buildStatus.status, 'error');
    assert.equal(buildStatus.reason?.message, 'pip install failed with exit code 1');
    assert.equal(buildStatus.reason?.step, 'RUN pip install -r requirements.txt');

    const vis = await client.updateTemplateVisibility('tpl_new', true);
    assert.deepEqual(vis.names, ['py-tools', 'py-tools:v1']);

    await client.deleteTemplate('tpl_new', { force: true });

    const wp = await client.createWarmPool({ name: 'pool-a', templateRef: 'py-tools', replicas: 3 });
    assert.equal(wp.readyReplicas, 3);
    const wps = await client.listWarmPools();
    assert.equal(wps.length, 1);

    // Verify 503 with sandboxID is NOT retried and exposes rich error metadata
    await assert.rejects(
      () => client.createSandbox({ templateId: 'slow-ready' }),
      (err) => {
        assert.ok(err instanceof CubePodError);
        assert.ok(err instanceof CubePodAPIError);
        assert.equal(err.statusCode, 503);
        assert.equal(err.code, 503);
        assert.equal(err.sandboxId, 'cpod-failed-1');
        assert.equal(err.logPath, '/var/log/cubepod/start-cmd.log');
        assert.match(err.message, /timed out/);
        return true;
      },
    );
    assert.equal(rawUrls.filter((u) => u === 'POST /sandboxes').length, 1);

    // Verify structured reason.message extraction on API error
    await assert.rejects(
      () => client.getTemplate('missing'),
      (err) => {
        assert.ok(err instanceof CubePodAPIError);
        assert.equal(err.statusCode, 404);
        assert.equal(err.code, 'template_not_found');
        assert.equal(err.step, 'lookup');
        assert.equal(err.message, 'template "missing" not found');
        return true;
      },
    );
  } finally {
    await srv.close();
  }
});

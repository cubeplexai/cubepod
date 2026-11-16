import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import { CommandExitError, Sandbox, Secret, Template, Volume } from 'e2b';
import { type GatewayHandle, startDirectGateway } from './gateway.js';

describe('Official E2B TypeScript SDK Compatibility Suite', () => {
  let gw: GatewayHandle;

  before(async () => {
    gw = await startDirectGateway();
  });

  after(async () => {
    await gw.stop();
  });

  it('supports Sandbox.create, commands.run (stdout/stderr/exitCode), getInfo, getMetrics, and kill', async () => {
    const sbx = await Sandbox.create('base', {
      timeoutMs: 300_000,
      envs: { TS_COMPAT: 'ok' },
      metadata: { suite: 'e2b-compat-ts' },
    });

    try {
      const stdoutChunks: string[] = [];
      const stderrChunks: string[] = [];
      const res = await sbx.commands.run(
        'echo "out:$TS_COMPAT" && echo "err:warn" >&2',
        {
          envs: { TS_COMPAT: 'ok' },
          onStdout: (data) => { stdoutChunks.push(data); },
          onStderr: (data) => { stderrChunks.push(data); },
        },
      );
      assert.equal(res.exitCode, 0);
      assert.match(res.stdout, /out:ok/);
      assert.match(res.stderr, /err:warn/);
      assert.match(stdoutChunks.join(''), /out:ok/);
      assert.match(stderrChunks.join(''), /err:warn/);

      await assert.rejects(
        () => sbx.commands.run('exit 42'),
        (err: unknown) => {
          assert.ok(err instanceof CommandExitError);
          assert.equal(err.exitCode, 42);
          return true;
        },
      );

      const bg = await sbx.commands.run('sleep 30', { background: true });
      assert.ok(bg.pid > 0);
      assert.equal(await sbx.commands.kill(bg.pid), true);

      const info = await sbx.getInfo();
      assert.equal(info.sandboxId, sbx.sandboxId);
      assert.equal(info.state, 'running');
      assert.equal(info.metadata.suite, 'e2b-compat-ts');

      const listed = await Sandbox.list().nextItems();
      assert.ok(listed.some((item) => item.sandboxId === sbx.sandboxId));

      const instanceMetrics = await sbx.getMetrics();
      assert.ok(instanceMetrics.length >= 1);
      const staticMetrics = await Sandbox.getMetrics(sbx.sandboxId);
      assert.ok(staticMetrics.length >= 1);

      const m = staticMetrics[staticMetrics.length - 1];
      assert.ok(m.timestamp instanceof Date);
      assert.ok(m.cpuCount > 0);
      assert.ok(m.cpuUsedPct >= 0);
      assert.ok(m.memTotal > 0);
      assert.ok(m.memUsed >= 0 && m.memUsed <= m.memTotal);
      assert.ok(m.diskTotal > 0);
      assert.ok(m.diskUsed >= 0);

      const host = sbx.getHost(8080);
      assert.ok(host.includes(sbx.sandboxId) && host.includes('8080'));
    } finally {
      const killed = await sbx.kill();
      assert.equal(killed, true);
    }
  });

  it('supports filesystem makeDir, write, read, list, exists, and remove', async () => {
    const sbx = await Sandbox.create('base', { timeoutMs: 300_000 });
    try {
      const dir = '/workspace/ts_fs_test';
      assert.equal(await sbx.files.makeDir(dir), true);

      await sbx.files.write(`${dir}/hello.txt`, 'hello from ts');
      assert.equal(await sbx.files.exists(`${dir}/hello.txt`), true);

      const content = await sbx.files.read(`${dir}/hello.txt`);
      assert.equal(content, 'hello from ts');

      const entries = await sbx.files.list(dir);
      assert.ok(entries.some((entry) => entry.name === 'hello.txt'));

      await sbx.files.remove(dir);
      assert.equal(await sbx.files.exists(dir), false);
    } finally {
      await sbx.kill();
    }
  });

  it('supports sandbox.pause() and Sandbox.connect() auto-resume with file persistence', async () => {
    const sbx = await Sandbox.create('base', { timeoutMs: 300_000 });
    try {
      await sbx.files.write('/workspace/persist.txt', 'survived-pause');
      const paused = await sbx.pause();
      assert.equal(paused, true);

      const pausedInfo = await Sandbox.getInfo(sbx.sandboxId);
      assert.equal(pausedInfo.state, 'paused');

      const resumed = await Sandbox.connect(sbx.sandboxId);
      assert.equal(resumed.sandboxId, sbx.sandboxId);

      const resumedInfo = await resumed.getInfo();
      assert.equal(resumedInfo.state, 'running');

      const content = await resumed.files.read('/workspace/persist.txt');
      assert.equal(content, 'survived-pause');
    } finally {
      await sbx.kill();
    }
  });

  it('supports Volume CRUD and content read/write', async () => {
    const volName = `ts-compat-vol-${Date.now()}`;
    const vol = await Volume.create(volName);
    try {
      assert.ok(vol.volumeId);
      assert.equal(vol.name, volName);

      const listed = await Volume.list();
      assert.ok(listed.some((item) => item.volumeId === vol.volumeId));

      const info = await Volume.getInfo(vol.volumeId);
      assert.equal(info.volumeId, vol.volumeId);

      await vol.writeFile('/hello.txt', 'ts-volume-content', { apiUrl: gw.url });
      const data = await vol.readFile('/hello.txt', { apiUrl: gw.url });
      assert.equal(data, 'ts-volume-content');
    } finally {
      await Volume.destroy(vol.volumeId);
    }
  });

  it('supports Secret CRUD/fill, network egress rules, and Template catalog', async () => {
    const secName = `ts_compat_sec_${Date.now()}`;
    const info = await Secret.create(secName, 'sk_ts_initial');
    try {
      assert.ok(info.secretId.startsWith('sec_'));
      assert.equal(info.name, secName);
      assert.equal(info.version, 1);

      const listed = await Secret.list().nextItems();
      assert.ok(listed.some((item) => item.secretId === info.secretId));

      const fetched = await Secret.getInfo(info.secretId);
      assert.equal(fetched.secretId, info.secretId);

      const rotated = await Secret.update(info.secretId, 'sk_ts_rotated');
      assert.equal(rotated.version, 2);

      const placeholder = Secret.fill(secName);
      assert.equal(placeholder, `\${e2b.secrets.${secName}}`);

      const netSbx = await Sandbox.create('base', {
        timeoutMs: 300_000,
        network: {
          allowOut: ['api.example.com'],
          denyOut: ['0.0.0.0/0'],
          rules: {
            'api.example.com': [
              {
                transform: {
                  headers: {
                    Authorization: `Bearer ${placeholder}`,
                  },
                },
              },
            ],
          },
        },
      });
      try {
        const netInfo = await netSbx.getInfo();
        assert.equal(netInfo.sandboxId, netSbx.sandboxId);
      } finally {
        await netSbx.kill();
      }
    } finally {
      await Secret.destroy(info.secretId);
    }

    const templates = await Template.list().nextItems();
    assert.ok(templates.length > 0);

    const assigned = await Template.assignTags('base:default', ['ts-prod']);
    assert.ok(assigned.buildId);
    assert.ok(assigned.tags.includes('ts-prod'));

    const tags = await Template.getTags('base');
    assert.ok(tags.some((t) => t.tag === 'ts-prod'));

    await Template.removeTags('base', ['ts-prod']);
    const tagsAfter = await Template.getTags('base');
    assert.ok(!tagsAfter.some((t) => t.tag === 'ts-prod'));
  });
});

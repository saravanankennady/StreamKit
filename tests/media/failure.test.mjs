// What happens when things break: workers and media processes killed in the middle of a job. Nothing here is mocked. The
// processes are really killed.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createReadStream, statSync } from 'node:fs';
import { createServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { networkInterfaces } from 'node:os';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { promisify } from 'node:util';
import pg from 'pg';
import {
  RTMP_PORT, Api, WORKER_DB_URL, assertAudioFlows, assertVideoFlows, audioStats, buildBinaries, connectDb, download, ffprobe, joinRoom, prepareTmp, setUp, sleep, startEgressWorker,
  startIngressWorker, startServer, stopProcess, tearDown, tmp, videoStats, waitFor, waitForPort, waitForStatus, waitForTracks, waitForWorker, writeConfig, zombiesBelow,
} from './lib.mjs';

const run = promisify(execFile);
let stack;
let api;
let db;
let counter = 0;
const nextRoom = () => `fail-${process.pid}-${++counter}`;
const FILE = { kind: 'file', format: 'mp4' };

before(async () => {
  prepareTmp();
  await buildBinaries();
  stack = await setUp();
  api = stack.api;
  db = await connectDb();
}, { timeout: 180000 });

after(async () => {
  await db?.end();
  await tearDown(stack);
});

async function twoPeople(room) {
  const a = await joinRoom(stack, api, 'alice', room);
  const b = await joinRoom(stack, api, 'bob', room);
  await waitForTracks(a, 'bob', ['audio', 'video']);
  await waitForTracks(b, 'alice', ['audio', 'video']);
  return [a, b];
}

// Every process whose command line mentions a job, found the way an operator would.
async function processesOf(text) {
  const { stdout } = await run('ps', ['-eo', 'pid=,args=', '--cols', '4000'], { maxBuffer: 1 << 24 });
  return stdout.split('\n').filter((l) => l.trim() && l.includes(text) && !l.includes('ps -eo')).map((l) => {
    const m = l.trim().match(/^(\d+)\s+(.*)$/);
    return { pid: Number(m[1]), args: m[2] };
  });
}

const kill9 = (pid) => { try { process.kill(pid, 'SIGKILL'); } catch {} };

async function restartEgress(args = [], opts = {}) {
  await stopProcess(stack.egress);
  stack.egress = startEgressWorker(stack.config, args, opts);
  await waitForWorker(api, 'egress_worker');
}

describe('a worker dying', () => {
  it('fails the job through its lease, leaves no processes, recovers the recording, and a retry makes a new file', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(8000);
      assert.equal((await api.ok('GET', `/egress/${job.id}`)).status, 'active');
      const before = await processesOf(`work/${job.id}`);
      assert.ok(before.length > 0, 'Chrome should be running for this job');

      // No warning at all.
      stack.egress.kill('SIGKILL');
      const killedAt = Date.now();

      // Nobody is stuck: another worker (streamkit-ingress here), or the server, notices the lease run out.
      const failed = await waitForStatus(api, job.id, 'failed', 30000);
      console.log(`  marked failed ${(Date.now() - killedAt) / 1000}s after the kill: ${failed.error}`);
      assert.match(failed.error, /stopped responding/);
      assert.ok(failed.ended_at);

      // Chrome must not be left running with nobody watching it.
      await waitFor('Chrome to exit', async () => (await processesOf(`work/${job.id}`)).length === 0, { timeout: 15000 });

      // A new worker finds what was left behind.
      stack.egress = startEgressWorker(stack.config);
      await waitForWorker(api, 'egress_worker');
      const recovered = await waitFor('the recording to be recovered', async () => {
        const j = await api.ok('GET', `/egress/${job.id}`);
        return j.file_size > 0 ? j : null;
      }, { timeout: 45000 });
      assert.equal(recovered.status, 'failed', 'recovering it does not change what happened');
      assert.match(recovered.error, /Recovered \d+\.\d+ seconds/);
      const path = await download(api, job.id, `${job.id}-recovered.mp4`);
      const info = await ffprobe(path);
      assert.ok(info.video && info.audio);
      assert.ok(info.duration > 5, `recovered only ${info.duration}s of about 8s`);
      const whole = await videoStats(path);
      assert.ok(whole.frames > 100 && whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
      console.log(`  recovered ${info.duration.toFixed(2)}s: ${recovered.error}`);
      assert.equal(existsSync(join(tmp, 'work', job.id)), false, 'the working directory should be cleaned up');

      const retried = await api.ok('POST', `/recordings/${job.id}/retry`, {}, [201]);
      assert.notEqual(retried.id, job.id);
      assert.equal(retried.retry_of, job.id);
      await waitForStatus(api, retried.id, 'active', 45000);
      await sleep(5000);
      await api.ok('POST', `/egress/${retried.id}/stop`, {});
      const done = await waitForStatus(api, retried.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      assert.notEqual(done.output_url, recovered.output_url);
      const newPath = await download(api, retried.id, `${retried.id}.mp4`);
      assert.ok((await ffprobe(newPath)).video);
      // The original is still there.
      assert.ok((await ffprobe(await download(api, job.id, `${job.id}-still.mp4`))).video);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('with no worker, starting returns 503, and a job left in the queue fails rather than waiting forever', async () => {
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    // A clean shutdown says so, so the API knows at once.
    await stopProcess(stack.egress);
    const r = await api.req('POST', '/egress', { type: 'room_composite', room, output: FILE });
    assert.equal(r.status, 503, r.text);
    assert.equal(r.body.error.code, 'egress_unavailable');
    assert.ok(r.body.error.message.length > 0);

    // A row in the queue with nobody to take it, as if a worker had died straight after the API checked.
    const inserted = await db.query(`INSERT INTO egresses (id, project_id, room_name, type, status, format, output_url, created_at)
      VALUES ('EG_orphan', $1, $2, 'room_composite', 'starting', 'mp4', '/api/v1/recordings/EG_orphan/file', now() - interval '2 minutes') RETURNING id`, [api.project, room]);
    assert.equal(inserted.rowCount, 1);
    const failed = await waitForStatus(api, 'EG_orphan', 'failed', 15000);
    assert.match(failed.error, /No worker/);

    stack.egress = startEgressWorker(stack.config);
    await waitForWorker(api, 'egress_worker');
    const ok = await api.req('POST', '/egress', { type: 'room_composite', room, output: FILE });
    assert.equal(ok.status, 201, ok.text);
    await api.ok('POST', `/egress/${ok.body.id}/stop`, {});
    await waitForStatus(api, ok.body.id, ['aborted', 'complete', 'failed'], 30000);
  });

  it('a clean shutdown finishes running recordings first', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(5000);
      await stopProcess(stack.egress, 'SIGTERM');
      const done = await api.ok('GET', `/egress/${job.id}`);
      assert.equal(done.status, 'complete', done.error);
      assert.ok(done.file_size > 0);
      assert.ok((await ffprobe(await download(api, job.id, `${job.id}.mp4`))).video);
      stack.egress = startEgressWorker(stack.config);
      await waitForWorker(api, 'egress_worker');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('media processes crashing', () => {
  it('Chrome killed mid-recording fails the job with a useful error, and saves what it had', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(6000);
      const procs = await processesOf(`work/${job.id}/chrome`);
      const main = procs.find((p) => !p.args.includes('--type='));
      assert.ok(main, JSON.stringify(procs));
      kill9(main.pid);
      const failed = await waitForStatus(api, job.id, 'failed', 30000);
      console.log(`  ${failed.error}`);
      assert.match(failed.error, /Chrome|recorder page/);
      assert.match(failed.error, /Saved \d+\.\d+ seconds/);
      assert.ok(failed.file_size > 0);
      assert.ok((await ffprobe(await download(api, job.id, `${job.id}.mp4`))).duration > 3);
      await waitFor('Chrome to be gone', async () => (await processesOf(`work/${job.id}`)).length === 0);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('only the tab crashing is noticed too', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(4000);
      const procs = await processesOf(`work/${job.id}/chrome`);
      const renderers = procs.filter((p) => p.args.includes('--type=renderer'));
      assert.ok(renderers.length > 0, JSON.stringify(procs.map((p) => p.args.slice(0, 80))));
      renderers.forEach((p) => kill9(p.pid));
      const failed = await waitForStatus(api, job.id, 'failed', 40000);
      console.log(`  ${failed.error}`);
      assert.match(failed.error, /Lost the recorder page|Chrome|stopped producing/);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('ffmpeg failing while it finishes the file fails the job with its reason, and the capture is kept', async () => {
    const fake = join(tmp, 'broken-ffmpeg.sh');
    // It answers the worker's health check. A worker with an ffmpeg that cannot even do that takes no jobs at all.
    writeFileSync(fake, '#!/bin/sh\n[ "$1" = "-version" ] && exec ffmpeg -version\necho "Conversion failed: simulated by the test" >&2\nexit 1\n');
    chmodSync(fake, 0o755);
    await restartEgress([], { env: { STREAMKIT_FFMPEG: fake } });
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(3000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const failed = await waitForStatus(api, job.id, 'failed', 30000);
      assert.match(failed.error, /simulated by the test/);
      const dir = join(tmp, 'work', job.id);
      assert.ok(readdirSync(dir).some((f) => f.startsWith('capture.') && f !== 'capture.json'), 'the capture should be kept');
    } finally {
      await a.close();
      await b.close();
      await restartEgress();
    }
  });

  it('a stream destination that cannot be used when recording starts costs only that stream, not the recording', async () => {
    // The API took the address, and by the time the recording starts, it points somewhere it may not go.
    const strict = writeConfig('config-strict.yaml', '', { allowPrivate: false });
    await stopProcess(stack.egress);
    stack.egress = startEgressWorker(strict);
    await waitForWorker(api, 'egress_worker');
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE, stream_urls: ['rtmp://127.0.0.1:9/live/key-1234567890abc'] }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(4000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      assert.ok((await ffprobe(await download(api, job.id, `${job.id}-strict.mp4`))).duration > 2);
      const logs = await api.ok('GET', `/logs?category=recording&room=${room}`);
      const failed = logs.items.find((l) => l.type === 'egress_stream_failed');
      assert.ok(failed, JSON.stringify(logs.items.map((l) => l.type)));
      assert.ok(!JSON.stringify(failed).includes('1234567890'), 'the key leaked');
      // A job with nothing but that stream has nothing left to do.
      const only = await api.ok('POST', '/egress', { type: 'room_composite', room, stream_urls: ['rtmp://127.0.0.1:9/live/key-1234567890abc'] }, [201]);
      const ended = await waitForStatus(api, only.id, 'failed', 45000);
      assert.match(ended.error, /Every stream destination/);
      assert.ok(!ended.error.includes('1234567890'), ended.error);
    } finally {
      await a.close();
      await b.close();
      await restartEgress();
    }
  }, { timeout: 180000 });

  it('nothing secret is on any command line while recording and streaming', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const sink = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-listen', '1', '-f', 'flv', '-i', 'rtmp://127.0.0.1:57941/live/whatever', '-c', 'copy', '-f', 'null', '-'], { stdio: 'ignore' });
    await sleep(500);
    try {
      const key = 'secret-stream-key-98765';
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE, stream_urls: [`rtmp://127.0.0.1:57941/live/${key}?auth=also-secret-54321`] }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      const everything = (await processesOf('')).map((p) => p.args);
      for (const secret of [key, 'also-secret-54321', '#token=', 'media-tests-worker-password', 'postgres://']) {
        const found = everything.filter((args) => args.includes(secret));
        assert.deepEqual(found, [], `${secret} is on a command line`);
      }
      assert.ok(everything.some((args) => args.includes(`work/${job.id}/chrome`)), 'Chrome should be running');
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
    } finally {
      sink.kill('SIGKILL');
      await a.close();
      await b.close();
    }
  });

  it('ffmpeg killed while streaming fails a stream-only job, but a job with a file carries on', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const sink = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-listen', '1', '-f', 'flv', '-i', 'rtmp://127.0.0.1:57938/live/key-1234567890abc', '-c', 'copy', '-f', 'null', '-'], { stdio: 'ignore' });
    const sink2 = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-listen', '1', '-f', 'flv', '-i', 'rtmp://127.0.0.1:57939/live/key-1234567890abc', '-c', 'copy', '-f', 'null', '-'], { stdio: 'ignore' });
    await sleep(500);
    try {
      const streamOnly = await api.ok('POST', '/egress', { type: 'room_composite', room, stream_urls: ['rtmp://127.0.0.1:57938/live/key-1234567890abc'] }, [201]);
      const both = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE, stream_urls: ['rtmp://127.0.0.1:57939/live/key-1234567890abc'] }, [201]);
      await waitForStatus(api, streamOnly.id, 'active', 45000);
      await waitForStatus(api, both.id, 'active', 45000);
      await sleep(4000);

      // The sinks have the key on their command lines, because that is how this test runs them. What the worker starts must not.
      const leaked = (await processesOf('key-1234567890abc')).filter((p) => !p.args.includes('-listen'));
      assert.deepEqual(leaked, [], 'a stream key is on a command line');
      const pushers = await processesOf('-rtmp_tcurl rtmp://127.0.0.1:5793');
      const toKill = pushers.filter((p) => p.args.includes(' -f flv') && !p.args.includes('-listen'));
      assert.equal(toKill.length, 2, JSON.stringify(toKill));
      toKill.forEach((p) => kill9(p.pid));

      const failed = await waitForStatus(api, streamOnly.id, 'failed', 30000);
      console.log(`  ${failed.error}`);
      assert.match(failed.error, /stream destination/);
      assert.ok(!failed.error.includes('1234567890'), `the key leaked: ${failed.error}`);

      await sleep(3000);
      assert.equal((await api.ok('GET', `/egress/${both.id}`)).status, 'active');
      await api.ok('POST', `/egress/${both.id}/stop`, {});
      const done = await waitForStatus(api, both.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      assert.ok((await ffprobe(await download(api, both.id, `${both.id}.mp4`))).duration > 5);
      const logs = await api.ok('GET', `/logs?category=recording&room=${room}`);
      assert.ok(logs.items.some((l) => l.type === 'egress_stream_failed'), JSON.stringify(logs.items.map((l) => l.type)));
    } finally {
      sink.kill('SIGKILL');
      sink2.kill('SIGKILL');
      await a.close();
      await b.close();
    }
  });
});

describe('the recorder losing the room', () => {
  it('being removed from a live room fails the job and keeps what it had, rather than calling it complete', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(5000);
      await api.ok('DELETE', `/participants/${room}/${job.id}`, undefined, [204]);
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      console.log(`  ${done.status}: ${done.error}`);
      assert.equal(done.status, 'failed', 'the room is still going, so this recording is not complete');
      assert.match(done.error, /Saved \d+\.\d+ seconds/);
      assert.ok(done.file_size > 0);
      assert.ok((await ffprobe(await download(api, job.id, `${job.id}-removed.mp4`))).duration > 3);
      assert.equal((await api.ok('GET', `/rooms/${room}`)).name, room, 'the room should still be open');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a server that comes back quickly costs a recording a moment, and it carries on', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(4000);
      await stopProcess(stack.server);
      stack.server = await startServer(stack.config);
      await sleep(8000);
      assert.equal((await api.ok('GET', `/egress/${job.id}`)).status, 'active');
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      assert.ok((await ffprobe(await download(api, job.id, `${job.id}-restart.mp4`))).duration > 10);
    } finally {
      await a.close();
      await b.close();
    }
  }, { timeout: 180000 });

  it('a server that stays away fails the job, rather than calling it complete', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(4000);
      await stopProcess(stack.server);
      // The recorder tries for a while, and nothing here can answer for the room, so it is the database that says how it ended.
      let row;
      await waitFor('the job to end', async () => {
        ({ rows: [row] } = await db.query('SELECT status, error, file_size FROM egresses WHERE id = $1', [job.id]));
        return ['complete', 'failed', 'aborted'].includes(row.status);
      }, { timeout: 180000, every: 1000 });
      console.log(`  ${row.status}: ${row.error}`);
      assert.equal(row.status, 'failed', 'the meeting did not end, the server did');
      assert.match(row.error, /lost its connection/);
      assert.ok(Number(row.file_size) > 0);
    } finally {
      stack.server = await startServer(stack.config);
      await a.close();
      await b.close();
    }
  }, { timeout: 300000 });
});

describe('what a media process can reach', () => {
  it('nothing of the same user can read a worker\'s environment or memory', () => {
    for (const worker of [stack.egress, stack.ingress]) {
      for (const file of ['environ', 'mem']) {
        assert.throws(() => readFileSync(`/proc/${worker.pid}/${file}`), { code: 'EACCES' }, `${worker.name}: ${file}`);
      }
    }
  });

  it('ffmpeg cannot read a worker\'s environment, finished recordings, or the configuration', async () => {
    const recording = join(tmp, 'recordings', 'canary.txt');
    writeFileSync(recording, 'another project\'s recording');
    // In a directory of its own, because the directory a binary is in is something it may read.
    const fakeDir = join(tmp, 'probing');
    mkdirSync(fakeDir, { recursive: true });
    const fake = join(fakeDir, 'ffmpeg.sh');
    // What a compromised ffmpeg would try. It answers the health check, and fails the job so its directory stays to look at.
    writeFileSync(fake, `#!/bin/sh
[ "$1" = "-version" ] && exec ffmpeg -version
for last in "$@"; do :; done
dir=$(dirname "$last")
{
  cat /proc/$PPID/environ
  cat ${recording}
  cat ${stack.config}
  cat /proc/1/cmdline
} > "$dir/probe.txt" 2>&1
echo reached the end >> "$dir/probe.txt"
exit 1
`);
    chmodSync(fake, 0o755);
    await restartEgress([], { env: { STREAMKIT_FFMPEG: fake } });
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(2000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      await waitForStatus(api, job.id, 'failed', 30000);
      const probe = readFileSync(join(tmp, 'work', job.id, 'probe.txt'), 'utf8');
      console.log(`  ${probe.split('\n').slice(0, 4).join(' | ')}`);
      assert.match(probe, /reached the end/, 'the script should have run');
      for (const secret of ['STREAMKIT_', 'worker-password', 'master_key', 'another project']) {
        assert.ok(!probe.includes(secret), `ffmpeg could read ${secret}: ${probe}`);
      }
    } finally {
      await a.close();
      await b.close();
      await restartEgress();
    }
  });

  it('the workers\' database role is not a superuser, and can reach only what they need', async () => {
    const worker = new pg.Client({ connectionString: WORKER_DB_URL });
    await worker.connect();
    try {
      const { rows } = await worker.query(`SELECT rolsuper, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname = current_user`);
      assert.deepEqual(rows[0], { rolsuper: false, rolcreatedb: false, rolcreaterole: false });
      for (const statement of ['SELECT * FROM users', 'SELECT * FROM sessions', 'SELECT name FROM projects', 'SELECT * FROM organizations', 'DELETE FROM egresses', 'DROP TABLE egresses', 'CREATE TABLE stolen (x int)', 'UPDATE api_keys SET revoked_at = NULL']) {
        await assert.rejects(worker.query(statement), /permission denied|must be owner/, statement);
      }
      await worker.query('SELECT count(*) FROM egresses');
    } finally {
      await worker.end();
    }
  });
});

describe('jobs that could hold a slot forever', () => {
  async function restartWith(egress) {
    await stopProcess(stack.egress);
    stack.egress = startEgressWorker(writeConfig('config-limits.yaml', '', { egress }));
    await waitForWorker(api, 'egress_worker');
  }
  after(() => restartEgress());

  it('a job gives up if nothing arrives to record, and its slot is free again', async () => {
    await restartWith('    wait_for_media_seconds: 5');
    const room = nextRoom();
    const silent = await joinRoom(stack, api, 'silent', room, { publish: false });
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      const done = await waitForStatus(api, job.id, ['aborted', 'failed', 'complete'], 40000);
      assert.equal(done.status, 'aborted', done.error);
      assert.match(done.error, /Nothing to record arrived/);
      assert.equal(done.file_size, 0);
    } finally {
      await silent.close();
    }
  }, { timeout: 90000 });

  it('a recording ends by itself at the longest it may be, with a good file', async () => {
    await restartWith('    max_duration_seconds: 6');
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      const info = await ffprobe(await download(api, job.id, `${job.id}-limit.mp4`));
      assert.ok(info.duration > 4 && info.duration < 10, `${info.duration}`);
      const logs = await api.ok('GET', `/logs?category=recording&room=${room}`);
      assert.ok(logs.items.some((l) => /longest a recording may be/.test(l.message)), JSON.stringify(logs.items.map((l) => l.message)));
    } finally {
      await a.close();
      await b.close();
    }
  }, { timeout: 120000 });

  it('a recorder page that stops running before anything arrives is noticed', async () => {
    await restartEgress();
    const room = nextRoom();
    const silent = await joinRoom(stack, api, 'silent', room, { publish: false });
    let frozen = [];
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitFor('the recorder to join', async () => (await api.ok('GET', `/rooms/${room}`)).active_recording === true, { timeout: 30000 });
      await sleep(3000);
      frozen = (await processesOf(`work/${job.id}/chrome`)).filter((p) => p.args.includes('--type=renderer')).map((p) => p.pid);
      assert.ok(frozen.length > 0);
      frozen.forEach((pid) => process.kill(pid, 'SIGSTOP'));
      const done = await waitForStatus(api, job.id, ['failed', 'aborted', 'complete'], 60000);
      assert.equal(done.status, 'failed');
      assert.match(done.error, /stopped responding/);
    } finally {
      frozen.forEach((pid) => { try { process.kill(pid, 'SIGCONT'); } catch {} });
      await silent.close();
    }
  }, { timeout: 120000 });
});

describe('a worker that stalls for a moment', () => {
  it('keeps its job if it comes back soon after its lease ran out', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(2000);
      // The test's lease is six seconds. Nobody may decide from the clock alone that this worker has died.
      stack.egress.kill('SIGSTOP');
      await sleep(9000);
      stack.egress.kill('SIGCONT');
      await sleep(4000);
      assert.equal((await api.ok('GET', `/egress/${job.id}`)).status, 'active');
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
    } finally {
      await a.close();
      await b.close();
    }
  }, { timeout: 120000 });

  it('a new job is not left waiting while a worker recovers a long recording', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const fake = join(tmp, 'slow-ffmpeg.sh');
    // Finishing a file takes a long time here, as it does for a long recording that has to be encoded again.
    writeFileSync(fake, '#!/bin/sh\ncase "$*" in\n  *output.*) sleep 40 ;;\nesac\nexec ffmpeg "$@"\n');
    chmodSync(fake, 0o755);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(5000);
      stack.egress.kill('SIGKILL');
      await waitForStatus(api, job.id, 'failed', 40000);

      stack.egress = startEgressWorker(stack.config, [], { env: { STREAMKIT_FFMPEG: fake } });
      await waitForWorker(api, 'egress_worker');
      await waitFor('recovery to start', async () => (await processesOf(`work/${job.id}/output.`)).length > 0, { timeout: 30000 });
      const started = Date.now();
      const next = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, next.id, 'active', 30000);
      console.log(`  a new job went active ${(Date.now() - started) / 1000}s in, during recovery`);
      await api.ok('POST', `/egress/${next.id}/stop`, {});
      await waitForStatus(api, next.id, ['complete', 'failed'], 90000);

      // Being asked to stop in the middle of it says nothing about the capture, so the next worker can try.
      await waitFor('recovery to still be going', async () => (await processesOf(`work/${job.id}/output.`)).length > 0 || (await api.ok('GET', `/egress/${job.id}`)).file_size > 0, { timeout: 5000 });
      await stopProcess(stack.egress);
      if ((await api.ok('GET', `/egress/${job.id}`)).file_size === 0) {
        stack.egress = startEgressWorker(stack.config);
        await waitForWorker(api, 'egress_worker');
      } else {
        stack.egress = startEgressWorker(stack.config);
      }
      const recovered = await waitFor('the recording to be recovered', async () => {
        const j = await api.ok('GET', `/egress/${job.id}`);
        return j.file_size > 0 ? j : null;
      }, { timeout: 60000 });
      assert.match(recovered.error, /Recovered/);
    } finally {
      await a.close();
      await b.close();
      await restartEgress();
    }
  }, { timeout: 360000 });

  it('recovering a recording is tried again after storage has failed', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const recordings = join(tmp, 'recordings');
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(5000);
      stack.egress.kill('SIGKILL');
      await waitForStatus(api, job.id, 'failed', 40000);
      // The project's directory cannot be written, as if storage were down.
      const dir = join(recordings, api.project);
      mkdirSync(dir, { recursive: true });
      chmodSync(dir, 0o500);
      stack.egress = startEgressWorker(stack.config);
      await waitFor('the first attempt to fail', () => existsSync(join(tmp, 'work', job.id, 'salvage.json')), { timeout: 40000 });
      assert.equal((await api.ok('GET', `/egress/${job.id}`)).file_size, 0);
      chmodSync(dir, 0o750);
      const recovered = await waitFor('the recording to be recovered', async () => {
        const j = await api.ok('GET', `/egress/${job.id}`);
        return j.file_size > 0 ? j : null;
      }, { timeout: 120000, every: 1000 });
      assert.match(recovered.error, /Recovered/);
    } finally {
      chmodSync(join(recordings, api.project), 0o750);
      await a.close();
      await b.close();
      await restartEgress();
    }
  }, { timeout: 300000 });

  it('two workers sharing a volume do not both recover one recording', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const fake = join(tmp, 'slow-ffmpeg.sh');
    let second;
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(5000);
      stack.egress.kill('SIGKILL');
      await waitForStatus(api, job.id, 'failed', 40000);
      stack.egress = startEgressWorker(stack.config, [], { env: { STREAMKIT_FFMPEG: fake } });
      second = startEgressWorker(stack.config, [], { name: 'egress2', env: { STREAMKIT_FFMPEG: fake } });
      await waitFor('recovery to start', async () => (await processesOf(`work/${job.id}/output.`)).length > 0, { timeout: 40000 });
      // Each pass of each worker gets a look at it, and they come every thirty seconds.
      for (let i = 0; i < 8; i++) {
        const finishing = (await processesOf(`work/${job.id}/output.`)).filter((p) => p.args.includes('sleep') || p.args.includes('ffmpeg'));
        assert.ok(finishing.length <= 1, JSON.stringify(finishing));
        await sleep(5000);
      }
    } finally {
      await a.close();
      await b.close();
      if (second) await stopProcess(second);
      await restartEgress();
    }
  }, { timeout: 300000 });

  it('a recorder cannot be sent to an address a project chose', async () => {
    const hits = [];
    const turn = createNetServer((c) => { hits.push(c.remoteAddress); c.destroy(); });
    await new Promise((r) => turn.listen(0, '0.0.0.0', r));
    // Whatever address this machine has now. A browser will not use a TURN server on loopback, so that would show nothing.
    const address = Object.values(networkInterfaces()).flat().find((i) => i.family === 'IPv4' && !i.internal)?.address;
    assert.ok(address, 'this test needs a network interface other than loopback');
    await api.ok('PATCH', `/projects/${api.project}`, { settings: { ice_servers: [{ urls: [`turn:${address}:${turn.address().port}?transport=tcp`], username: 'x', credential: 'y' }] } });
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitFor('the recorder to join', async () => (await api.ok('GET', `/rooms/${room}`)).active_recording === true, { timeout: 30000 });
      await sleep(6000);
      assert.equal(hits.length, 0, 'the recorder connected to an ICE server from project settings');
      // Someone in the project does get it, or none of this shows anything.
      const person = await joinRoom(stack, api, 'person', room, { publish: false });
      await waitFor('a browser to use the address', () => hits.length > 0, { timeout: 20000 }).finally(() => person.close());
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      await waitForStatus(api, job.id, ['aborted', 'failed', 'complete'], 30000);
    } finally {
      turn.close();
      await api.ok('PATCH', `/projects/${api.project}`, { settings: {} });
    }
  });
});

describe('leaving nothing behind', () => {
  it('a worker has no dead processes left after its jobs end', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      for (let i = 0; i < 2; i++) {
        const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
        await waitForStatus(api, job.id, 'active', 45000);
        await sleep(2000);
        await api.ok('POST', `/egress/${job.id}/stop`, {});
        await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      }
      await waitFor('dead processes to be cleared', () => zombiesBelow(stack.egress.pid).length === 0, { timeout: 20000 });
    } finally {
      await a.close();
      await b.close();
    }
  }, { timeout: 180000 });
});

describe('capacity', () => {
  it('a worker takes no more than its limit, and the API refuses the rest', async () => {
    await restartEgress(['--max-jobs', '2']);
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    const body = { type: 'room_composite', room, output: FILE };
    try {
      const j1 = await api.ok('POST', '/egress', body, [201]);
      const j2 = await api.ok('POST', '/egress', body, [201]);
      const refused = await api.req('POST', '/egress', body);
      assert.equal(refused.status, 503, refused.text);
      assert.equal(refused.body.error.code, 'egress_unavailable');
      await waitFor('both to be claimed', async () => {
        const { rows } = await db.query(`SELECT count(*)::int AS n FROM egresses WHERE id = ANY($1) AND worker_id IS NOT NULL`, [[j1.id, j2.id]]);
        return rows[0].n === 2;
      });
      // A slot frees up as a job ends.
      await api.ok('POST', `/egress/${j1.id}/stop`, {});
      await waitForStatus(api, j1.id, ['aborted', 'failed'], 30000);
      const j3 = await api.ok('POST', '/egress', body, [201]);
      for (const id of [j2.id, j3.id]) {
        await api.ok('POST', `/egress/${id}/stop`, {});
        await waitForStatus(api, id, ['aborted', 'failed'], 30000);
      }
    } finally {
      await restartEgress();
    }
  });

  it('a draining worker takes no new jobs', async () => {
    const { items } = await api.ok('GET', '/servers');
    const worker = items.find((s) => s.stats.kind === 'egress_worker' && Date.now() - Date.parse(s.last_seen_at) < 15000);
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    await api.ok('POST', `/servers/${worker.node_id}/drain`, { draining: true });
    const refused = await api.req('POST', '/egress', { type: 'room_composite', room, output: FILE });
    assert.equal(refused.status, 503);
    await api.ok('POST', `/servers/${worker.node_id}/drain`, { draining: false });
    await waitFor('the worker to take jobs again', async () => {
      const r = await api.req('POST', '/egress', { type: 'room_composite', room, output: FILE });
      if (r.status !== 201) return null;
      await api.ok('POST', `/egress/${r.body.id}/stop`, {});
      await waitForStatus(api, r.body.id, ['aborted', 'failed'], 30000);
      return true;
    }, { timeout: 20000 });
  });
});

describe('the ingress worker dying', () => {
  function push(key) {
    return spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
      '-c:v', 'libx264', '-preset', 'veryfast', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${key}`], { stdio: 'ignore' });
  }

  it('ends in error through its lease, frees the key, leaves no transcoder, and streams resume after a restart', async () => {
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
    const pusher = push(created.stream_key);
    const viewer = await joinRoom(stack, api, 'viewer', room, { publish: false });
    try {
      await waitFor('publishing', async () => (await api.ok('GET', `/ingress/${created.id}`)).status === 'publishing', { timeout: 30000 });
      await waitForTracks(viewer, 'stage', ['video']);
      await assertVideoFlows(viewer, 'stage');
      // Only this worker's own: other things on this machine may be running transcoders too.
      const { stdout } = await run('ps', ['-o', 'pid=', '--ppid', String(stack.ingress.pid)]);
      const transcoders = stdout.split('\n').map((l) => Number(l.trim())).filter(Boolean);
      assert.ok(transcoders.length >= 1, 'the worker should have a transcoder running');

      stack.ingress.kill('SIGKILL');
      pusher.kill('SIGKILL');
      const failed = await waitFor('the lease to run out', async () => {
        const i = await api.ok('GET', `/ingress/${created.id}`);
        return i.status === 'error' ? i : null;
      }, { timeout: 30000 });
      assert.match(failed.error, /stopped responding/);
      await waitFor('the transcoder to exit', () => transcoders.every((pid) => !existsSync(`/proc/${pid}`)), { timeout: 15000 });

      stack.ingress = startIngressWorker(stack.config);
      await waitForPort(RTMP_PORT);
      const again = push(created.stream_key);
      try {
        await waitFor('publishing again', async () => (await api.ok('GET', `/ingress/${created.id}`)).status === 'publishing', { timeout: 30000 });
        await waitForTracks(viewer, 'stage', ['audio', 'video']);
        await assertVideoFlows(viewer, 'stage');
        await assertAudioFlows(viewer, 'stage');
      } finally {
        again.kill('SIGKILL');
      }
    } finally {
      pusher.kill('SIGKILL');
      await viewer.close();
    }
  });

  it('with no ingress worker, creating still works, and the status stays honest', async () => {
    await stopProcess(stack.ingress);
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
    assert.equal(created.status, 'inactive');
    const pusher = push(created.stream_key);
    const result = await new Promise((resolve) => pusher.on('exit', (code) => resolve(code)));
    assert.notEqual(result, 0, 'there is nothing to connect to');
    assert.equal((await api.ok('GET', `/ingress/${created.id}`)).status, 'inactive');
    const url = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage2', source_url: 'http://127.0.0.1:9/x.mp4' }, [201]);
    assert.equal(url.status, 'inactive');
    await sleep(2000);
    assert.equal((await api.ok('GET', `/ingress/${url.id}`)).status, 'inactive');
    await api.ok('DELETE', `/ingress/${url.id}`, undefined, [204]);
    stack.ingress = startIngressWorker(stack.config);
    await waitForPort(RTMP_PORT);
  });
});

describe('URL inputs and a worker going away', () => {
  let origin;
  let sourceUrl;
  before(async () => {
    const path = join(tmp, 'long-source.mp4');
    await run('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-f', 'lavfi', '-i', 'testsrc2=s=320x240:r=15', '-f', 'lavfi', '-i', 'sine=frequency=330:r=48000', '-t', '180',
      '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-movflags', '+faststart', path]);
    const size = statSync(path).size;
    origin = createServer((req, res) => {
      const range = /bytes=(\d+)-(\d*)/.exec(req.headers.range ?? '');
      res.setHeader('content-type', 'video/mp4');
      res.setHeader('accept-ranges', 'bytes');
      if (range) {
        const start = Number(range[1]);
        const end = range[2] ? Number(range[2]) : size - 1;
        res.statusCode = 206;
        res.setHeader('content-range', `bytes ${start}-${end}/${size}`);
        createReadStream(path, { start, end }).pipe(res);
      } else {
        res.setHeader('content-length', size);
        createReadStream(path).pipe(res);
      }
    });
    await new Promise((r) => origin.listen(0, '127.0.0.1', r));
    sourceUrl = `http://127.0.0.1:${origin.address().port}/long-source.mp4`;
  }, { timeout: 120000 });
  after(() => origin?.close());

  const publishing = (id) => waitFor('publishing', async () => (await api.ok('GET', `/ingress/${id}`)).status === 'publishing', { timeout: 40000 });

  it('a clean restart of the worker picks the pull up again', async () => {
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: sourceUrl }, [201]);
    try {
      await publishing(created.id);
      await stopProcess(stack.ingress);
      assert.equal((await api.ok('GET', `/ingress/${created.id}`)).status, 'inactive');
      stack.ingress = startIngressWorker(stack.config);
      await waitForPort(RTMP_PORT);
      await publishing(created.id);
    } finally {
      await api.ok('DELETE', `/ingress/${created.id}`, undefined, [204]);
    }
  }, { timeout: 120000 });

  it('a worker dying leaves the pull for the next one', async () => {
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: sourceUrl }, [201]);
    try {
      await publishing(created.id);
      stack.ingress.kill('SIGKILL');
      await waitFor('the lease to run out', async () => (await api.ok('GET', `/ingress/${created.id}`)).status === 'error', { timeout: 40000 });
      stack.ingress = startIngressWorker(stack.config);
      await waitForPort(RTMP_PORT);
      await publishing(created.id);
    } finally {
      await api.ok('DELETE', `/ingress/${created.id}`, undefined, [204]);
    }
  }, { timeout: 150000 });

  it('a pull that someone stopped stays stopped across a restart', async () => {
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: sourceUrl }, [201]);
    try {
      await publishing(created.id);
      await api.ok('POST', `/ingress/${created.id}/stop`, {});
      await waitFor('inactive', async () => (await api.ok('GET', `/ingress/${created.id}`)).status === 'inactive');
      await stopProcess(stack.ingress);
      stack.ingress = startIngressWorker(stack.config);
      await waitForPort(RTMP_PORT);
      await sleep(4000);
      assert.equal((await api.ok('GET', `/ingress/${created.id}`)).status, 'inactive');
    } finally {
      await api.ok('DELETE', `/ingress/${created.id}`, undefined, [204]);
    }
  }, { timeout: 120000 });

  it('moving a running pull to another room restarts it there', async () => {
    const room = nextRoom();
    const other = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: sourceUrl }, [201]);
    const viewer = await joinRoom(stack, api, 'viewer', other, { publish: false });
    try {
      await publishing(created.id);
      await api.ok('PATCH', `/ingress/${created.id}`, { room: other });
      await waitForTracks(viewer, 'stage', ['video'], 40000);
      await assertVideoFlows(viewer, 'stage');
    } finally {
      await viewer.close();
      await api.ok('DELETE', `/ingress/${created.id}`, undefined, [204]);
    }
  }, { timeout: 120000 });
});

describe('a machine stalling', () => {
  const pushTo = (key) => spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
    '-c:v', 'libx264', '-preset', 'veryfast', '-tune', 'zerolatency', '-g', '30', '-b:v', '800k', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${key}`], { stdio: 'ignore' });
  const ingressNow = (id) => api.ok('GET', `/ingress/${id}`);
  const childrenOf = async (pid) => {
    const { stdout } = await run('ps', ['-o', 'pid=', '--ppid', String(pid)]).catch(() => ({ stdout: '' }));
    return stdout.split('\n').map((l) => Number(l.trim())).filter(Boolean);
  };

  // A worker whose lease outlasts the freezes below, and whose stall timeout they exceed, on the worker's own terms.
  async function restartIngress({ lease, stall }) {
    await stopProcess(stack.ingress);
    const config = writeConfig('config-stall.yaml', '', { lease, stall });
    stack.ingress = startIngressWorker(config);
    await waitForPort(RTMP_PORT);
  }
  after(async () => {
    await stopProcess(stack.ingress);
    stack.ingress = startIngressWorker(stack.config);
    await waitForPort(RTMP_PORT);
  });

  it('a server too slow to answer does not cost a publisher its lease while it joins', async () => {
    await restartIngress({ lease: 6, stall: 60 });
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
    stack.server.kill('SIGSTOP');
    let pusher;
    try {
      pusher = pushTo(created.stream_key);
      await sleep(10000);
    } finally {
      stack.server.kill('SIGCONT');
    }
    try {
      const live = await waitFor('publishing', async () => {
        const i = await ingressNow(created.id);
        assert.notEqual(i.status, 'error', i.error);
        return i.status === 'publishing' ? i : null;
      }, { timeout: 30000 });
      assert.equal(live.error, '');
    } finally {
      pusher?.kill('SIGKILL');
    }
    await waitFor('the stream to end', async () => (await ingressNow(created.id)).status === 'inactive', { timeout: 20000 });
  }, { timeout: 120000 });

  it('everything freezing for longer than the stall timeout does not end a stream', async () => {
    await restartIngress({ lease: 60, stall: 8 });
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
    const viewer = await joinRoom(stack, api, 'viewer', room, { publish: false });
    const pusher = pushTo(created.stream_key);
    try {
      await waitFor('publishing', async () => (await ingressNow(created.id)).status === 'publishing', { timeout: 30000 });
      await waitForTracks(viewer, 'stage', ['video']);
      await assertVideoFlows(viewer, 'stage');
      const transcoders = await childrenOf(stack.ingress.pid);
      assert.ok(transcoders.length > 0);
      // What memory pressure does to a machine: nothing runs, and then everything does.
      stack.ingress.kill('SIGSTOP');
      transcoders.forEach((pid) => process.kill(pid, 'SIGSTOP'));
      await sleep(12000);
      transcoders.forEach((pid) => process.kill(pid, 'SIGCONT'));
      stack.ingress.kill('SIGCONT');
      await sleep(4000);
      const after = await ingressNow(created.id);
      assert.equal(after.status, 'publishing', after.error);
      await assertVideoFlows(viewer, 'stage');
    } finally {
      pusher.kill('SIGKILL');
      await viewer.close();
    }
  }, { timeout: 120000 });

  it('a transcoder that stops producing while the source still sends ends the stream, and says which it was', async () => {
    await restartIngress({ lease: 60, stall: 8 });
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
    const pusher = pushTo(created.stream_key);
    try {
      await waitFor('publishing', async () => (await ingressNow(created.id)).status === 'publishing', { timeout: 30000 });
      const transcoders = await childrenOf(stack.ingress.pid);
      assert.ok(transcoders.length > 0);
      transcoders.forEach((pid) => process.kill(pid, 'SIGSTOP'));
      const failed = await waitFor('the watchdog', async () => {
        const i = await ingressNow(created.id);
        return i.status === 'error' ? i : null;
      }, { timeout: 30000 });
      console.log(`  ${failed.error}`);
      assert.match(failed.error, /transcoder/i);
      await waitFor('the transcoder to be gone', () => transcoders.every((pid) => !existsSync(`/proc/${pid}`)), { timeout: 15000 });
      // The key still works: the stream failed, not the ingress.
      const again = pushTo(created.stream_key);
      try {
        await waitFor('publishing again', async () => (await ingressNow(created.id)).status === 'publishing', { timeout: 30000 });
      } finally {
        again.kill('SIGKILL');
      }
    } finally {
      pusher.kill('SIGKILL');
    }
  }, { timeout: 120000 });

  it('a source that goes quiet is told apart from a transcoder that has stopped', async () => {
    await restartIngress({ lease: 60, stall: 8 });
    const room = nextRoom();
    // A server that accepts the connection and then says nothing, which is how a hung origin looks to ffmpeg.
    const silent = createNetServer(() => {});
    await new Promise((r) => silent.listen(0, '127.0.0.1', r));
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: `rtmp://127.0.0.1:${silent.address().port}/live/stream` }, [201]);
    try {
      const failed = await waitFor('the watchdog', async () => {
        const i = await ingressNow(created.id);
        return i.status === 'error' ? i : null;
      }, { timeout: 40000 });
      console.log(`  ${failed.error}`);
      assert.match(failed.error, /source/i);
      assert.doesNotMatch(failed.error, /transcoder has/i);
    } finally {
      silent.close();
    }
  }, { timeout: 120000 });

  it('a freeze longer than a recording\'s stall limit does not fail the recording', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      await stopProcess(stack.egress);
      stack.egress = startEgressWorker(writeConfig('config-long-lease.yaml', '', { lease: 90 }));
      await waitForWorker(api, 'egress_worker');
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(3000);
      // The worker only: Chrome keeps recording, into a socket nobody reads for longer than the 20 seconds a recording
      // may go quiet.
      stack.egress.kill('SIGSTOP');
      await sleep(24000);
      stack.egress.kill('SIGCONT');
      await sleep(5000);
      assert.equal((await api.ok('GET', `/egress/${job.id}`)).status, 'active');
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      const info = await ffprobe(await download(api, job.id, `${job.id}-frozen.mp4`));
      assert.ok(info.duration > 25, `only ${info.duration}s: what Chrome recorded during the freeze should still be there`);
    } finally {
      await a.close();
      await b.close();
      await restartEgress();
    }
  }, { timeout: 240000 });
});

describe('workers reporting on themselves', () => {
  const workerRow = async (kind) => {
    const { items } = await api.ok('GET', '/servers');
    return items.filter((s) => s.stats.kind === kind).sort((x, y) => Date.parse(y.last_seen_at) - Date.parse(x.last_seen_at))[0];
  };

  it('a recording worker that cannot write to its working directory says so, and takes nothing until that is fixed', async () => {
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    await restartEgress();
    const work = join(tmp, 'work');
    assert.equal((await workerRow('egress_worker')).stats.healthy, true);
    chmodSync(work, 0o500);
    try {
      const sick = await waitFor('the worker to notice', async () => {
        const row = await workerRow('egress_worker');
        return row.stats.healthy === false ? row : null;
      }, { timeout: 30000 });
      console.log(`  ${sick.stats.unhealthy_reason}`);
      assert.match(sick.stats.unhealthy_reason, /working directory/);
      const refused = await api.req('POST', '/egress', { type: 'room_composite', room, output: FILE });
      assert.equal(refused.status, 503, refused.text);
      assert.equal(refused.body.error.code, 'egress_unavailable');
    } finally {
      chmodSync(work, 0o750);
    }
    await waitFor('the worker to recover', async () => (await workerRow('egress_worker')).stats.healthy === true, { timeout: 30000 });
    const ok = await api.req('POST', '/egress', { type: 'room_composite', room, output: FILE });
    assert.equal(ok.status, 201, ok.text);
    await api.ok('POST', `/egress/${ok.body.id}/stop`, {});
    await waitForStatus(api, ok.body.id, ['aborted', 'failed'], 30000);
  }, { timeout: 120000 });

  it('an ingest worker that cannot run ffmpeg says so, and refuses publishers until it can', async () => {
    const ffmpeg = join(tmp, 'ffmpeg-for-health.sh');
    writeFileSync(ffmpeg, '#!/bin/sh\nexec ffmpeg "$@"\n');
    chmodSync(ffmpeg, 0o755);
    await stopProcess(stack.ingress);
    stack.ingress = startIngressWorker(stack.config, [], { env: { STREAMKIT_FFMPEG: ffmpeg } });
    await waitForPort(RTMP_PORT);
    await waitFor('the worker to be healthy', async () => (await workerRow('ingress_worker')).stats.healthy === true, { timeout: 30000 });
    const created = await api.ok('POST', '/ingress', { input_type: 'rtmp', room: nextRoom(), participant_identity: 'stage' }, [201]);
    writeFileSync(ffmpeg, '#!/bin/sh\nexit 1\n');
    try {
      const sick = await waitFor('the worker to notice', async () => {
        const row = await workerRow('ingress_worker');
        return row.stats.healthy === false ? row : null;
      }, { timeout: 30000 });
      console.log(`  ${sick.stats.unhealthy_reason}`);
      assert.match(sick.stats.unhealthy_reason, /ffmpeg/);
      const refused = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=320x240:r=30', '-c:v', 'libx264', '-preset', 'ultrafast', '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${created.stream_key}`], { stdio: 'ignore' });
      const code = await new Promise((resolve) => refused.on('exit', resolve));
      assert.notEqual(code, 0, 'a worker that cannot transcode should refuse a publisher');
      assert.equal((await api.ok('GET', `/ingress/${created.id}`)).status, 'inactive');
    } finally {
      writeFileSync(ffmpeg, '#!/bin/sh\nexec ffmpeg "$@"\n');
    }
    await waitFor('the worker to recover', async () => (await workerRow('ingress_worker')).stats.healthy === true, { timeout: 30000 });
    await stopProcess(stack.ingress);
    stack.ingress = startIngressWorker(stack.config);
    await waitForPort(RTMP_PORT);
  }, { timeout: 120000 });
});

describe('isolation with real files', () => {
  it('another project cannot read, download, stop, retry or delete', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const other = await Api.signUp('other');
    try {
      const running = await api.ok('POST', '/egress', { type: 'room_composite', room, output: FILE }, [201]);
      await waitForStatus(api, running.id, 'active', 45000);
      for (const [method, path] of [['GET', `/egress/${running.id}`], ['POST', `/egress/${running.id}/stop`]]) {
        const r = await other.req(method, path, method === 'POST' ? {} : undefined);
        assert.equal(r.status, 404, `${method} ${path}: ${r.text}`);
      }
      await sleep(3000);
      assert.equal((await api.ok('GET', `/egress/${running.id}`)).status, 'active', 'it should not have been stopped');
      await api.ok('POST', `/egress/${running.id}/stop`, {});
      const done = await waitForStatus(api, running.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);

      for (const [method, path] of [
        ['GET', `/recordings/${done.id}`], ['GET', `/recordings/${done.id}/file`], ['POST', `/recordings/${done.id}/retry`], ['DELETE', `/recordings/${done.id}`],
      ]) {
        const r = await other.req(method, path, method === 'POST' ? {} : undefined);
        assert.equal(r.status, 404, `${method} ${path}: ${r.text}`);
      }
      // Even with the project named and the real path known, the other user has no access.
      const sneaky = await other.req('GET', `/recordings/${done.id}/file`, undefined, { 'X-Project-ID': api.project });
      assert.equal(sneaky.status, 404);
      const lists = await Promise.all(['/egress', '/recordings'].map((p) => other.ok('GET', p)));
      lists.forEach((l) => assert.equal(l.items.length, 0));
      assert.equal((await other.req('POST', '/egress', { type: 'room_composite', room, output: FILE })).status, 404, 'room names are per project');

      const ingress = await api.ok('POST', '/ingress', { input_type: 'rtmp', room, participant_identity: 'stage' }, [201]);
      for (const [method, path] of [
        ['GET', `/ingress/${ingress.id}`], ['PATCH', `/ingress/${ingress.id}`], ['DELETE', `/ingress/${ingress.id}`], ['POST', `/ingress/${ingress.id}/stop`],
        ['POST', `/ingress/${ingress.id}/rotate-key`], ['GET', `/ingress/${ingress.id}/logs`],
      ]) {
        const r = await other.req(method, path, method === 'POST' || method === 'PATCH' ? {} : undefined);
        assert.equal(r.status, 404, `${method} ${path}: ${r.text}`);
      }
      // And it all still works for its owner.
      assert.ok((await ffprobe(await download(api, done.id, `${done.id}.mp4`))).video);
      await api.ok('GET', `/ingress/${ingress.id}`);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

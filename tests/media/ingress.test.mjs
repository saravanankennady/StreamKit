// Ingest, end to end: streams pushed and pulled by a real ffmpeg, and published by a real browser, then received by a real
// Chrome subscriber that has to decode moving pictures and hear sound.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { createReadStream, mkdirSync, readFileSync, statSync } from 'node:fs';
import { createServer } from 'node:http';
import { connect } from 'node:net';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { promisify } from 'node:util';
import { execFile } from 'node:child_process';
import {
  BASE, RTMP_PORT, assertAudioFlows, assertVideoFlows, buildBinaries, connectDb, joinRoom, prepareTmp, received, setUp, sleep, tearDown, tmp, waitFor, waitForTracks,
} from './lib.mjs';

const run = promisify(execFile);
let stack;
let api;
let counter = 0;
const nextRoom = () => `ing-${process.pid}-${++counter}`;

const pushers = new Set();

// A real encoder: H.264 with B-frames and AAC, which a browser could not take as it is.
function push(key, { seconds = 300, size = '640x360', extra = [] } = {}) {
  const child = spawn('ffmpeg', [
    '-hide_banner', '-loglevel', 'error', '-re',
    '-f', 'lavfi', '-i', `testsrc2=s=${size}:r=30`,
    '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
    '-t', String(seconds),
    '-c:v', 'libx264', '-preset', 'veryfast', '-bf', '2', '-g', '60', '-b:v', '1500k', '-pix_fmt', 'yuv420p',
    '-c:a', 'aac', '-b:a', '128k', ...extra,
    '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${key}`,
  ], { stdio: ['ignore', 'ignore', 'pipe'] });
  child.stderrText = '';
  child.stderr.on('data', (d) => { child.stderrText += d; });
  child.exited = new Promise((resolve) => child.on('exit', (code, signal) => { pushers.delete(child); resolve({ code, signal }); }));
  pushers.add(child);
  return child;
}

async function stopPush(child) {
  if (!pushers.has(child)) return;
  child.kill('SIGKILL');
  await child.exited;
}

const ingressStatus = (id) => api.ok('GET', `/ingress/${id}`);

async function waitForIngress(id, status, timeout = 30000) {
  return waitFor(`ingress ${id} to be ${status}`, async () => {
    const i = await ingressStatus(id);
    if (i.status === status) return i;
    if (i.status === 'error' && status !== 'error') {
      const e = new Error(`ingress ${id} is in error: ${i.error}`);
      e.final = true;
      throw e;
    }
    return null;
  }, { timeout, every: 250 });
}

async function createRTMP(room) {
  return api.ok('POST', '/ingress', { name: 'Main stage', input_type: 'rtmp', room, participant_identity: 'stage', participant_name: 'Main stage' }, [201]);
}

async function subscriber(room, identity = 'viewer') {
  const c = await joinRoom(stack, api, identity, room, { publish: false });
  return c;
}

async function assertReceiving(c, identity = 'stage') {
  await waitForTracks(c, identity, ['audio', 'video'], 30000);
  const video = await assertVideoFlows(c, identity);
  await assertAudioFlows(c, identity);
  return video;
}

before(async () => {
  prepareTmp();
  await buildBinaries();
  stack = await setUp({ egress: Boolean(process.env.INGEST_BUSY) });
  api = stack.api;
}, { timeout: 180000 });

after(async () => {
  for (const p of [...pushers]) await stopPush(p);
  await tearDown(stack);
});

describe('RTMP', () => {
  it('starts every time', async () => {
    // Set INGEST_REPEAT to go looking for something that only happens now and then.
    const repeat = Number(process.env.INGEST_REPEAT ?? 8);
    const slow = [];
    // INGEST_BUSY pushes into one room that is already in a call and being recorded, the way a real stage is used.
    const busy = process.env.INGEST_BUSY ? nextRoom() : null;
    const people = [];
    if (busy) {
      people.push(await joinRoom(stack, api, 'alice', busy), await joinRoom(stack, api, 'bob', busy));
      await waitForTracks(people[0], 'bob', ['video']);
      await api.ok('POST', '/egress', { type: 'room_composite', room: busy, output: { kind: 'file', format: 'mp4' } }, [201]);
      await api.ok('POST', '/egress', { type: 'participant', room: busy, participant: 'bob', output: { kind: 'file', format: 'mp4' }, stream_urls: ['rtmp://127.0.0.1:1/live/nowhere-1234567890'] }, [201]);
    }
    for (let i = 0; i < repeat; i++) {
      const room = busy ?? nextRoom();
      const created = await createRTMP(room);
      const started = Date.now();
      const pusher = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
        '-c:v', 'libx264', '-preset', 'veryfast', '-tune', 'zerolatency', '-g', '30', '-b:v', '800k', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-b:a', '96k', '-f', 'flv',
        `rtmp://${busy ? 'localhost' : '127.0.0.1'}:${RTMP_PORT}/live/${created.stream_key}`], { stdio: 'ignore' });
      try {
        await waitForIngress(created.id, 'publishing', 15000);
        slow.push(Date.now() - started);
      } catch (e) {
        const logs = await api.ok('GET', `/ingress/${created.id}/logs`);
        throw new Error(`push ${i + 1} of ${repeat} did not publish: ${e.message}: ${JSON.stringify(await ingressStatus(created.id))} ${JSON.stringify(logs.items.map((l) => l.message))}`);
      } finally {
        pusher.kill('SIGKILL');
      }
      await waitForIngress(created.id, 'inactive', 20000);
      await api.ok('DELETE', `/ingress/${created.id}`, undefined, [204]);
    }
    await Promise.all(people.map((c) => c.close()));
    slow.sort((a, b) => a - b);
    console.log(`  ${repeat} pushes, time to publishing: median ${slow[slow.length >> 1]} ms, worst ${slow.at(-1)} ms`);
  }, { timeout: 15 * 60 * 1000 });

  it('a pushed stream reaches a subscriber as moving video and audible sound, from a participant of kind INGRESS', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    assert.equal(created.status, 'inactive');
    assert.ok(created.stream_key, 'the response to create carries the stream key');
    assert.equal(created.publish_url, `rtmp://127.0.0.1:${RTMP_PORT}/live`);
    assert.equal(created.enabled, true);

    // It is shown once and never again.
    const again = await ingressStatus(created.id);
    assert.equal(again.stream_key, undefined);
    const list = await api.ok('GET', '/ingress');
    assert.ok(list.items.every((i) => i.stream_key === undefined));

    const viewer = await subscriber(room);
    const pusher = push(created.stream_key);
    try {
      const live = await waitForIngress(created.id, 'publishing');
      const video = await assertReceiving(viewer);
      assert.equal(video.participantKind, 1, 'the publisher should have kind INGRESS');
      assert.equal(video.width, 640);
      assert.equal(video.height, 360);
      console.log(`  subscriber decoded ${video.frames} frames in 2.5s, ${video.width}x${video.height}`);
      assert.deepEqual((await viewer.page.evaluate(() => window.harness.identities())), ['stage']);

      // Health shows while streaming.
      const healthy = await waitFor('stream health', async () => {
        const i = await ingressStatus(created.id);
        return i.stats.bitrate_in > 0 && i.stats.fps > 0 ? i : null;
      });
      assert.equal(healthy.stats.width, 640);
      assert.equal(healthy.stats.height, 360);
      assert.equal(healthy.stats.video_codec, 'h264');
      assert.equal(healthy.stats.audio_codec, 'aac');
      assert.ok(healthy.stats.started_at);
      assert.ok(healthy.stats.bitrate_in > 100_000 && healthy.stats.bitrate_in < 10_000_000, `bitrate_in ${healthy.stats.bitrate_in}`);
      assert.ok(healthy.stats.fps > 15 && healthy.stats.fps < 45, `fps ${healthy.stats.fps}`);
      console.log(`  stats: ${JSON.stringify(healthy.stats)}`);

      const participants = await api.ok('GET', `/participants?room=${room}`);
      const stage = participants.items.find((p) => p.identity === 'stage');
      assert.ok(stage, JSON.stringify(participants));
      assert.equal(live.status, 'publishing');

      // Logs have real content, newest first.
      const logs = await api.ok('GET', `/ingress/${created.id}/logs`);
      assert.ok(logs.items.some((l) => l.type === 'ingress_publishing'), JSON.stringify(logs.items));
      assert.ok(logs.items.some((l) => l.type === 'ingress_connected'));
      assert.ok(logs.items[0].id > logs.items[logs.items.length - 1].id);
      assert.ok(logs.items.every((l) => l.category === 'recording'));
      assert.ok(!JSON.stringify(logs.items).includes(created.stream_key), 'the stream key leaked into the logs');
    } finally {
      await stopPush(pusher);
      await viewer.close();
    }
    await waitForIngress(created.id, 'inactive');
  });

  it('a wrong key is refused, and nothing joins the room', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const pusher = push('sk_not-the-right-key');
    const result = await pusher.exited;
    assert.notEqual(result.code, 0, 'ffmpeg should fail against a wrong key');
    const i = await ingressStatus(created.id);
    assert.equal(i.status, 'inactive');
    assert.equal((await api.req('GET', `/rooms/${room}`)).status, 404, 'a refused publisher must not open a room');
  });

  it('a second publisher on one key is refused, and the first keeps going', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const first = push(created.stream_key);
    const viewer = await subscriber(room);
    try {
      await waitForIngress(created.id, 'publishing');
      const second = push(created.stream_key);
      const result = await Promise.race([second.exited, sleep(15000).then(() => 'still running')]);
      assert.notEqual(result, 'still running', 'the second publisher should have been refused');
      assert.notEqual(result.code, 0);
      await assertReceiving(viewer);
      const i = await ingressStatus(created.id);
      assert.equal(i.status, 'publishing');
      const logs = await api.ok('GET', `/ingress/${created.id}/logs`);
      assert.ok(logs.items.some((l) => l.type === 'ingress_rejected' && l.level === 'warn'));
    } finally {
      await stopPush(first);
      await viewer.close();
    }
  });

  it('a disabled ingress refuses publishers, and enabling it lets them back in', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const off = await api.ok('PATCH', `/ingress/${created.id}`, { enabled: false });
    assert.equal(off.enabled, false);
    const pusher = push(created.stream_key);
    const result = await pusher.exited;
    assert.notEqual(result.code, 0);
    await api.ok('PATCH', `/ingress/${created.id}`, { enabled: true });
    const pusher2 = push(created.stream_key);
    try {
      await waitForIngress(created.id, 'publishing');
    } finally {
      await stopPush(pusher2);
    }
  });

  it('stop ends the stream, and the ingress can be used again', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const viewer = await subscriber(room);
    const pusher = push(created.stream_key);
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
      await api.ok('POST', `/ingress/${created.id}/stop`, {});
      await waitForIngress(created.id, 'inactive');
      const ended = await Promise.race([pusher.exited, sleep(10000).then(() => 'still running')]);
      assert.notEqual(ended, 'still running', 'stopping should disconnect the publisher');
      await waitFor('the participant to leave', async () => (await viewer.page.evaluate(() => window.harness.identities())).length === 0);

      const pusher2 = push(created.stream_key);
      try {
        await waitForIngress(created.id, 'publishing');
        await assertReceiving(viewer);
      } finally {
        await stopPush(pusher2);
      }
    } finally {
      await stopPush(pusher);
      await viewer.close();
    }
  });

  it('rotating the key invalidates the old one, ends its stream, and the new one works', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const oldKey = created.stream_key;
    const pusher = push(oldKey);
    await waitForIngress(created.id, 'publishing');
    const rotated = await api.ok('POST', `/ingress/${created.id}/rotate-key`, {});
    assert.ok(rotated.stream_key);
    assert.notEqual(rotated.stream_key, oldKey);
    const ended = await Promise.race([pusher.exited, sleep(10000).then(() => 'still running')]);
    assert.notEqual(ended, 'still running', 'a rotated key should not stay on the air');
    await waitForIngress(created.id, 'inactive');

    const stale = push(oldKey);
    const refused = await stale.exited;
    assert.notEqual(refused.code, 0, 'the old key should be refused');

    const fresh = push(rotated.stream_key);
    const viewer = await subscriber(room);
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
    } finally {
      await stopPush(fresh);
      await viewer.close();
    }
  });

  it('after a dropped connection, the publisher can reconnect and viewers get the stream back', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const viewer = await subscriber(room);
    const first = push(created.stream_key);
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
      // No goodbye: the encoder dies, as if the network or the machine had gone.
      await stopPush(first);
      await waitForIngress(created.id, 'inactive', 20000);
      const second = push(created.stream_key);
      try {
        await waitForIngress(created.id, 'publishing');
        await assertReceiving(viewer);
      } finally {
        await stopPush(second);
      }
    } finally {
      await stopPush(first);
      await viewer.close();
    }
  });

  it('deleting an ingress stops its stream', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const pusher = push(created.stream_key);
    await waitForIngress(created.id, 'publishing');
    await api.ok('DELETE', `/ingress/${created.id}`, undefined, [204]);
    const ended = await Promise.race([pusher.exited, sleep(10000).then(() => 'still running')]);
    assert.notEqual(ended, 'still running');
    const gone = await api.req('GET', `/ingress/${created.id}`);
    assert.equal(gone.status, 404);
  });

  it('a stream with only video, and one with only audio, are both published', async () => {
    const room = nextRoom();
    const created = await createRTMP(room);
    const viewer = await subscriber(room);
    const videoOnly = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=320x240:r=30', '-c:v', 'libx264', '-preset', 'veryfast', '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${created.stream_key}`], { stdio: 'ignore' });
    try {
      await waitForIngress(created.id, 'publishing');
      await waitForTracks(viewer, 'stage', ['video']);
      await assertVideoFlows(viewer, 'stage');
    } finally {
      videoOnly.kill('SIGKILL');
    }
    await waitForIngress(created.id, 'inactive', 20000);
    const audioOnly = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000', '-c:a', 'aac', '-f', 'flv', `rtmp://127.0.0.1:${RTMP_PORT}/live/${created.stream_key}`], { stdio: 'ignore' });
    try {
      await waitForIngress(created.id, 'publishing');
      await waitForTracks(viewer, 'stage', ['audio']);
      await assertAudioFlows(viewer, 'stage');
    } finally {
      audioOnly.kill('SIGKILL');
      await viewer.close();
    }
  });
});

describe('the RTMP port with strangers on it', () => {
  it('a connection that never gets to publishing is dropped, however slowly it keeps sending', async () => {
    const socket = connect(RTMP_PORT, '127.0.0.1');
    let closedAt = 0;
    socket.on('close', () => { closedAt = Date.now(); });
    socket.on('error', () => {});
    const started = Date.now();
    const drip = setInterval(() => { if (!socket.destroyed) socket.write(Buffer.from([3])); }, 2000);
    try {
      await waitFor('the connection to be dropped', () => closedAt > 0, { timeout: 40000, every: 500 });
      const seconds = (closedAt - started) / 1000;
      console.log(`  dropped after ${seconds}s`);
      assert.ok(seconds < 30, `took ${seconds}s`);
    } finally {
      clearInterval(drip);
      socket.destroy();
    }
  }, { timeout: 60000 });

  it('one address cannot fill the port, and encoders still get in once it has gone', async () => {
    const sockets = [];
    const open = () => new Promise((resolve) => {
      const s = connect(RTMP_PORT, '127.0.0.1');
      s.on('error', () => {});
      s.on('close', () => { s.closedEarly = true; });
      s.on('connect', () => resolve(s));
    });
    for (let i = 0; i < 64; i++) sockets.push(await open());
    await sleep(1000);
    const dropped = sockets.filter((s) => s.closedEarly).length;
    console.log(`  ${dropped} of ${sockets.length} connections from one address were closed straight away`);
    assert.ok(dropped >= 16, `${dropped} were refused`);
    sockets.forEach((s) => s.destroy());
    const room = nextRoom();
    const created = await createRTMP(room);
    const pusher = push(created.stream_key);
    try {
      await waitForIngress(created.id, 'publishing');
    } finally {
      await stopPush(pusher);
    }
  }, { timeout: 60000 });
});

describe('URL input', () => {
  let fileServer;
  let fileUrl;

  before(async () => {
    const path = join(tmp, 'source.mp4');
    await run('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=330:r=48000', '-t', '12',
      '-c:v', 'libx264', '-preset', 'veryfast', '-bf', '2', '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-movflags', '+faststart', path]);
    const size = statSync(path).size;
    fileServer = createServer((req, res) => {
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
    await new Promise((r) => fileServer.listen(0, '127.0.0.1', r));
    fileUrl = `http://127.0.0.1:${fileServer.address().port}/source.mp4`;
  });

  after(() => fileServer?.close());

  it('pulls a file over HTTP into the room, and ends when the file does', async () => {
    const room = nextRoom();
    const viewer = await subscriber(room);
    const created = await api.ok('POST', '/ingress', { name: 'From a file', input_type: 'url', room, participant_identity: 'stage', source_url: fileUrl }, [201]);
    assert.equal(created.stream_key, undefined);
    assert.equal(created.publish_url, '');
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
      const i = await ingressStatus(created.id);
      assert.equal(i.stats.width, 640);
      assert.equal(i.stats.video_codec, 'h264');
      // Twelve seconds of file, played in real time, then done.
      await waitForIngress(created.id, 'inactive', 30000);
      assert.equal((await ingressStatus(created.id)).error, '');
    } finally {
      await viewer.close();
    }
  });

  it('stop ends a pull, and setting enabled starts it again', async () => {
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: fileUrl }, [201]);
    await waitForIngress(created.id, 'publishing');
    await api.ok('POST', `/ingress/${created.id}/stop`, {});
    await waitForIngress(created.id, 'inactive');
    await sleep(2000);
    assert.equal((await ingressStatus(created.id)).status, 'inactive', 'a stopped pull must not restart by itself');
    await api.ok('PATCH', `/ingress/${created.id}`, { enabled: true });
    await waitForIngress(created.id, 'publishing');
    await api.ok('POST', `/ingress/${created.id}/stop`, {});
    await waitForIngress(created.id, 'inactive');
  });

  it('an unreachable source ends in error with a useful message', async () => {
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: 'http://127.0.0.1:9/nothing.mp4' }, [201]);
    const failed = await waitFor('the pull to fail', async () => {
      const i = await ingressStatus(created.id);
      return i.status === 'error' ? i : null;
    }, { timeout: 30000 });
    assert.ok(failed.error.length > 0);
    assert.ok(!failed.error.includes('127.0.0.1:9/nothing.mp4'), `the source URL should not be in the message: ${failed.error}`);
    console.log(`  error: ${failed.error.slice(0, 160)}`);
  });

  it('a source\'s credentials are kept sealed, and stay out of responses, logs and command lines', async () => {
    const secrets = ['hunter2-password', 'signed-token-abcdef'];
    let auth;
    const guarded = createServer((req, res) => {
      auth = req.headers.authorization;
      const range = /bytes=(\d+)-(\d*)/.exec(req.headers.range ?? '');
      res.setHeader('content-type', 'video/mp4');
      res.setHeader('accept-ranges', 'bytes');
      const size = statSync(join(tmp, 'source.mp4')).size;
      if (range) {
        const start = Number(range[1]);
        const end = range[2] ? Number(range[2]) : size - 1;
        res.statusCode = 206;
        res.setHeader('content-range', `bytes ${start}-${end}/${size}`);
        createReadStream(join(tmp, 'source.mp4'), { start, end }).pipe(res);
      } else {
        res.setHeader('content-length', size);
        createReadStream(join(tmp, 'source.mp4')).pipe(res);
      }
    });
    await new Promise((r) => guarded.listen(0, '127.0.0.1', r));
    const raw = `http://operator:${secrets[0]}@127.0.0.1:${guarded.address().port}/source.mp4?token=${secrets[1]}`;
    const room = nextRoom();
    const viewer = await subscriber(room);
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: raw }, [201]);
    const db = await connectDb();
    try {
      const seen = async () => JSON.stringify([await ingressStatus(created.id), await api.ok('GET', '/ingress'), created]);
      for (const secret of secrets) assert.ok(!(await seen()).includes(secret), `${secret} is in a response`);
      const { rows } = await db.query('SELECT source_url, source_url_sealed FROM ingresses WHERE id = $1', [created.id]);
      for (const secret of secrets) assert.ok(!rows[0].source_url.includes(secret), 'stored in the clear');
      assert.ok(rows[0].source_url_sealed?.length > 0);

      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
      assert.equal(auth, `Basic ${Buffer.from(`operator:${secrets[0]}`).toString('base64')}`, 'the source should get its password');
      const { stdout } = await run('ps', ['-eo', 'args=', '--cols', '4000'], { maxBuffer: 1 << 24 });
      for (const secret of secrets) assert.ok(!stdout.includes(secret), `${secret} is on a command line`);
      await api.ok('POST', `/ingress/${created.id}/stop`, {});
      await waitForIngress(created.id, 'inactive');
    } finally {
      await db.end();
      guarded.close();
      await viewer.close();
    }
    const logs = readFileSync(join(tmp, 'ingress.log'), 'utf8') + readFileSync(join(tmp, 'server.log'), 'utf8');
    for (const secret of secrets) assert.ok(!logs.includes(secret), `${secret} is in a log`);
  });

  it('a pull over RTMP that fails says so, without its key anywhere', async () => {
    const key = 'pull-key-secret-1234567';
    const room = nextRoom();
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: `rtmp://127.0.0.1:9/live/${key}` }, [201]);
    const failed = await waitFor('the pull to fail', async () => {
      const i = await ingressStatus(created.id);
      return i.status === 'error' ? i : null;
    }, { timeout: 40000 });
    console.log(`  error: ${failed.error.slice(0, 200)}`);
    assert.ok(!JSON.stringify(failed).includes(key), JSON.stringify(failed));
    assert.ok(!readFileSync(join(tmp, 'ingress.log'), 'utf8').includes(key), 'the key is in a log');
  });

  it('an RTMP pull really reaches the server, key and all', async () => {
    // Our own RTMP port takes the stream from one encoder, and a second ingress pulls... there is nothing to pull from there,
    // so this uses ffmpeg listening as the source, and checks the real name reached it.
    const port = 57942;
    const sender = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'info', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=320x240:r=15', '-f', 'lavfi', '-i', 'sine=frequency=220:r=48000',
      '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', '-g', '15', '-c:a', 'aac', '-f', 'flv', '-listen', '1', `rtmp://127.0.0.1:${port}/live/pull-me-12345`], { stdio: 'ignore' });
    await sleep(1000);
    const room = nextRoom();
    const viewer = await subscriber(room);
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: `rtmp://127.0.0.1:${port}/live/pull-me-12345` }, [201]);
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
      const { stdout } = await run('ps', ['-eo', 'args=', '--cols', '4000'], { maxBuffer: 1 << 24 });
      const lines = stdout.split('\n').filter((l) => l.includes('pull-me-12345') && !l.includes('-listen'));
      assert.deepEqual(lines, [], 'the key is on a command line');
    } finally {
      await api.ok('POST', `/ingress/${created.id}/stop`, {}).catch(() => {});
      sender.kill('SIGKILL');
      await viewer.close();
    }
  });

  it('a concat list is refused', async () => {
    const asked = [];
    const lists = createServer((req, res) => {
      asked.push(req.url);
      if (req.url.includes('.ffconcat')) res.end('ffconcat version 1.0\nfile source.mp4\n');
      else createReadStream(join(tmp, 'source.mp4')).pipe(res);
    });
    await new Promise((r) => lists.listen(0, '127.0.0.1', r));
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room: nextRoom(), participant_identity: 'stage', source_url: `http://127.0.0.1:${lists.address().port}/list.ffconcat` }, [201]);
    try {
      const failed = await waitFor('the pull to fail', async () => {
        const i = await ingressStatus(created.id);
        return i.status === 'error' ? i : null;
      }, { timeout: 30000 });
      console.log(`  error: ${failed.error.slice(0, 200)}`);
      assert.deepEqual(asked.filter((u) => !u.includes('.ffconcat')), [], 'ffmpeg followed the list');
    } finally {
      lists.close();
    }
  });

  it('pulls an HLS playlist, whose segments come through the proxy', async () => {
    const dir = join(tmp, 'hls');
    mkdirSync(dir, { recursive: true });
    await run('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-f', 'lavfi', '-i', 'testsrc2=s=320x240:r=15', '-f', 'lavfi', '-i', 'sine=frequency=330:r=48000', '-t', '20',
      '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p', '-g', '15', '-c:a', 'aac', '-f', 'hls', '-hls_time', '2', '-hls_list_size', '0', join(dir, 'index.m3u8')]);
    const hls = createServer((req, res) => createReadStream(join(dir, new URL(req.url, 'http://x').pathname.slice(1))).on('error', () => { res.statusCode = 404; res.end(); }).pipe(res));
    await new Promise((r) => hls.listen(0, '127.0.0.1', r));
    const room = nextRoom();
    const viewer = await subscriber(room);
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: `http://127.0.0.1:${hls.address().port}/index.m3u8?signed=abc123` }, [201]);
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
    } finally {
      await api.ok('POST', `/ingress/${created.id}/stop`, {}).catch(() => {});
      hls.close();
      await viewer.close();
    }
  });

  it('only allowed schemes are accepted', async () => {
    for (const source_url of ['file:///etc/passwd', 'ftp://example.com/x', 'concat:a|b', '/etc/passwd', 'udp://127.0.0.1:1234', 'srt://1.2.3.4:9000?mode=listener', 'rtmp://user:pw@1.2.3.4/live/key']) {
      const r = await api.req('POST', '/ingress', { input_type: 'url', room: 'x', participant_identity: 'y', source_url });
      assert.equal(r.status, 422, `${source_url}: ${r.text}`);
      assert.ok(r.body.error.fields.source_url, r.text);
    }
  });
});

describe('SRT', () => {
  it('pulls from an srt:// listener through URL input', async () => {
    const room = nextRoom();
    const port = 57936;
    const sender = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=220:r=48000',
      '-c:v', 'libx264', '-preset', 'veryfast', '-pix_fmt', 'yuv420p', '-g', '30', '-c:a', 'aac', '-f', 'mpegts', `srt://127.0.0.1:${port}?mode=listener&pkt_size=1316`], { stdio: 'ignore' });
    const viewer = await subscriber(room);
    await sleep(500);
    const created = await api.ok('POST', '/ingress', { input_type: 'url', room, participant_identity: 'stage', source_url: `srt://127.0.0.1:${port}?mode=caller` }, [201]);
    try {
      await waitForIngress(created.id, 'publishing');
      await assertReceiving(viewer);
    } finally {
      await api.ok('POST', `/ingress/${created.id}/stop`, {}).catch(() => {});
      sender.kill('SIGKILL');
      await viewer.close();
    }
  });
});

describe('WHIP', () => {
  // A WHIP client is only this: plain WebRTC, and one HTTP request.
  async function whipPublisher(key, endpoint, { videoOnly = false } = {}) {
    const ctx = await stack.chrome.browser.newContext({ permissions: ['camera', 'microphone'] });
    const page = await ctx.newPage();
    await page.goto(stack.chrome.base);
    const result = await page.evaluate(async ({ key, endpoint, videoOnly }) => {
      const pc = new RTCPeerConnection();
      window.pc = pc;
      const stream = await navigator.mediaDevices.getUserMedia({ audio: !videoOnly, video: { width: 640, height: 360 } });
      for (const track of stream.getTracks()) pc.addTransceiver(track, { direction: 'sendonly', streams: [stream] });
      await pc.setLocalDescription(await pc.createOffer());
      const res = await fetch(endpoint, { method: 'POST', headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/sdp' }, body: pc.localDescription.sdp });
      const answer = await res.text();
      if (res.status !== 201) return { status: res.status, body: answer };
      const out = { status: res.status, location: res.headers.get('location'), contentType: res.headers.get('content-type'), answer };
      await pc.setRemoteDescription({ type: 'answer', sdp: answer });
      return out;
    }, { key, endpoint, videoOnly });
    return { ctx, page, ...result, close: () => ctx.close() };
  }

  async function createWHIP(room) {
    return api.ok('POST', '/ingress', { name: 'Browser', input_type: 'whip', room, participant_identity: 'whip-pub', participant_name: 'Browser' }, [201]);
  }

  it('a browser publishes with plain RTCPeerConnection and fetch, and a subscriber decodes it', async () => {
    const room = nextRoom();
    const created = await createWHIP(room);
    assert.equal(created.publish_url, `${BASE}/whip/v1`);
    assert.ok(created.stream_key);
    const viewer = await subscriber(room);
    const pub = await whipPublisher(created.stream_key, created.publish_url);
    try {
      assert.equal(pub.status, 201, pub.body);
      assert.match(pub.location, /^\/whip\/v1\/WH_/);
      assert.match(pub.contentType, /application\/sdp/);
      assert.match(pub.answer, /a=candidate:/, 'the answer must carry candidates, since a WHIP client may not trickle');
      assert.match(pub.answer, /a=end-of-candidates|a=candidate:/);
      await waitFor('the browser to connect', () => pub.page.evaluate(() => window.pc.connectionState === 'connected'), { timeout: 20000 });
      await waitForIngress(created.id, 'publishing');
      const video = await assertReceiving(viewer, 'whip-pub');
      assert.equal(video.participantKind, 1, 'the publisher should have kind INGRESS');
      console.log(`  subscriber decoded ${video.frames} frames of a WHIP publisher, ${video.width}x${video.height}`);

      // Health: what can be seen without decoding.
      const healthy = await waitFor('stream health', async () => {
        const i = await ingressStatus(created.id);
        return i.stats.bitrate_in > 0 && i.stats.video_codec ? i : null;
      });
      console.log(`  stats: ${JSON.stringify(healthy.stats)}`);

      // Refused without the key.
      const noKey = await fetch(`${BASE}${pub.location}`, { method: 'DELETE' });
      assert.equal(noKey.status, 401);
      const wrongKey = await fetch(`${BASE}${pub.location}`, { method: 'DELETE', headers: { Authorization: 'Bearer nope' } });
      assert.equal(wrongKey.status, 401);
      assert.equal((await ingressStatus(created.id)).status, 'publishing');

      const del = await fetch(`${BASE}${pub.location}`, { method: 'DELETE', headers: { Authorization: `Bearer ${created.stream_key}` } });
      assert.equal(del.status, 200);
      await waitForIngress(created.id, 'inactive');
      await waitFor('the participant to leave', async () => (await viewer.page.evaluate(() => window.harness.identities())).length === 0);
      const missing = await fetch(`${BASE}${pub.location}`, { method: 'DELETE', headers: { Authorization: `Bearer ${created.stream_key}` } });
      assert.equal(missing.status, 404);
    } finally {
      await pub.close();
      await viewer.close();
    }
  });

  it('refuses a wrong key, a second publisher, a disabled ingress and a bad offer', async () => {
    const room = nextRoom();
    const created = await createWHIP(room);
    const wrong = await whipPublisher('sk_wrong', created.publish_url);
    assert.equal(wrong.status, 401);
    await wrong.close();

    const noAuth = await fetch(created.publish_url, { method: 'POST', headers: { 'Content-Type': 'application/sdp' }, body: 'v=0' });
    assert.equal(noAuth.status, 401);
    const badType = await fetch(created.publish_url, { method: 'POST', headers: { Authorization: `Bearer ${created.stream_key}`, 'Content-Type': 'text/plain' }, body: 'v=0' });
    assert.equal(badType.status, 415);
    const badOffer = await fetch(created.publish_url, { method: 'POST', headers: { Authorization: `Bearer ${created.stream_key}`, 'Content-Type': 'application/sdp' }, body: 'not an offer' });
    assert.equal(badOffer.status, 400, await badOffer.text());
    assert.equal((await ingressStatus(created.id)).status, 'error');

    const first = await whipPublisher(created.stream_key, created.publish_url);
    assert.equal(first.status, 201, first.body);
    const second = await whipPublisher(created.stream_key, created.publish_url);
    assert.equal(second.status, 409);
    await second.close();
    await waitForIngress(created.id, 'publishing');

    // An RTMP encoder cannot use a WHIP key.
    const crossed = push(created.stream_key);
    assert.notEqual((await crossed.exited).code, 0);

    await api.ok('POST', `/ingress/${created.id}/stop`, {});
    await waitForIngress(created.id, 'inactive');
    await first.close();

    await api.ok('PATCH', `/ingress/${created.id}`, { enabled: false });
    const disabled = await whipPublisher(created.stream_key, created.publish_url);
    assert.equal(disabled.status, 403);
    await disabled.close();
  });

  it('turns away someone trying keys at speed', async () => {
    const statuses = [];
    for (let i = 0; i < 60; i++) {
      const res = await fetch(`${BASE}/whip/v1`, { method: 'POST', headers: { Authorization: `Bearer sk_guess-${i}`, 'Content-Type': 'application/sdp' }, body: 'v=0' });
      statuses.push(res.status);
    }
    assert.ok(statuses.includes(401), 'wrong keys should be told so');
    assert.ok(statuses.includes(429), `no attempts were refused: ${[...new Set(statuses)]}`);
    await sleep(15000); // Leave the budget for the tests after this one.
  }, { timeout: 60000 });

  it('works for the project that static API keys belong to', async () => {
    // The project has no row in the database, so it has no key there either.
    const encode = (o) => Buffer.from(JSON.stringify(o)).toString('base64url');
    const head = encode({ alg: 'HS256', typ: 'JWT' });
    const body = encode({ iss: 'devkey', sub: 'tests', nbf: 0, exp: Math.floor(Date.now() / 1000) + 300, video: { ingressAdmin: true } });
    const signature = createHmac('sha256', 'secret').update(`${head}.${body}`).digest('base64url');
    const res = await fetch(`${BASE}/api/v1/ingress`, {
      method: 'POST', headers: { Authorization: `Bearer ${head}.${body}.${signature}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ input_type: 'whip', room: nextRoom(), participant_identity: 'whip-pub' }),
    });
    const created = await res.json();
    assert.equal(res.status, 201, JSON.stringify(created));
    assert.equal(created.project_id, 'default');
    const pub = await whipPublisher(created.stream_key, created.publish_url);
    try {
      assert.equal(pub.status, 201, pub.body);
    } finally {
      await pub.close();
    }
  });

  it('the browser going away ends the session, without a DELETE', async () => {
    const room = nextRoom();
    const created = await createWHIP(room);
    const pub = await whipPublisher(created.stream_key, created.publish_url);
    assert.equal(pub.status, 201, pub.body);
    await waitForIngress(created.id, 'publishing');
    await pub.close();
    // ICE takes a while to give up, then the session has the same window to come back as any other client.
    await waitForIngress(created.id, 'inactive', 90000);
  });
});

// Recording, end to end: real participants in real Chrome, a real worker, and the output checked by decoding it.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { createServer } from 'node:http';
import { after, before, describe, it } from 'node:test';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import {
  BASE, assertAudioFlows, assertVideoFlows, audioStats, buildBinaries, download, ffprobe, joinRoom, memoryBelowMB, prepareTmp, received, setUp, sleep, tearDown, tmp, videoStats,
  waitFor, waitForStatus, waitForTracks,
} from './lib.mjs';

const run = promisify(execFile);
let stack;
let api;
let roomCounter = 0;
const nextRoom = () => `rec-${process.pid}-${++roomCounter}`;

before(async () => {
  prepareTmp();
  await buildBinaries();
  stack = await setUp();
  api = stack.api;
}, { timeout: 180000 });

after(async () => {
  await tearDown(stack);
});

// Two people, publishing, seen by each other: from here on, the room has something to record.
async function twoPeople(room) {
  const a = await joinRoom(stack, api, 'alice', room);
  const b = await joinRoom(stack, api, 'bob', room);
  await waitForTracks(a, 'bob', ['audio', 'video']);
  await waitForTracks(b, 'alice', ['audio', 'video']);
  await assertVideoFlows(a, 'bob');
  return [a, b];
}

async function record(body, seconds) {
  const job = await api.ok('POST', '/egress', body, [201]);
  assert.equal(job.status, 'starting');
  const active = await waitForStatus(api, job.id, 'active', 45000);
  assert.ok(active.started_at, 'an active job has started_at');
  await sleep(seconds * 1000);
  const stopped = await api.ok('POST', `/egress/${job.id}/stop`, {});
  assert.equal(stopped.id, job.id);
  return waitForStatus(api, job.id, ['complete', 'failed', 'aborted'], 60000);
}

function assertDuration(actual, seconds) {
  // The recording starts within moments of going active, and stopping takes up to a second to notice.
  assert.ok(actual > seconds - 1.5 && actual < seconds + 4, `duration ${actual}s, expected about ${seconds}s`);
}

// Plays a recording in Chrome through the API, as the dashboard would: Range must work, or a <video> cannot seek.
async function assertPlaysInChrome(job, { audioOnly = false, seekTo } = {}) {
  const ctx = await stack.chrome.browser.newContext();
  const [name, value] = api.cookie.split('=');
  const url = new URL(BASE);
  await ctx.addCookies([{ name, value, domain: url.hostname, path: '/' }]);
  const page = await ctx.newPage();
  const ranges = [];
  page.on('response', (res) => {
    if (res.url().includes(`/recordings/${job.id}/file`)) ranges.push({ status: res.status(), range: res.headers()['content-range'] });
  });
  await page.goto(`${BASE}/`);
  const src = `${BASE}${job.output_url}?project_id=${api.project}`;
  const tag = audioOnly ? 'audio' : 'video';
  const result = await page.evaluate(async ({ src, tag, seekTo }) => {
    const el = document.createElement(tag);
    el.muted = true;
    document.body.appendChild(el);
    el.src = src;
    await new Promise((resolve, reject) => {
      el.onloadedmetadata = resolve;
      el.onerror = () => reject(new Error(`cannot load: ${el.error?.code} ${el.error?.message}`));
    });
    await el.play();
    const start = el.currentTime;
    await new Promise((r) => setTimeout(r, 1500));
    const advanced = el.currentTime - start;
    const seek = seekTo ?? el.duration - 2;
    el.currentTime = seek;
    await new Promise((resolve, reject) => {
      el.onseeked = resolve;
      setTimeout(() => reject(new Error('seek timed out')), 10000);
    });
    const seekedTo = el.currentTime;
    await new Promise((r) => setTimeout(r, 500));
    const quality = el.getVideoPlaybackQuality?.();
    return {
      duration: el.duration, advanced, seek, seekedTo, width: el.videoWidth, height: el.videoHeight,
      frames: quality?.totalVideoFrames ?? null, currentTime: el.currentTime,
    };
  }, { src, tag, seekTo });
  await ctx.close();
  assert.ok(result.advanced > 0.5, `currentTime did not advance: ${JSON.stringify(result)}`);
  assert.ok(Math.abs(result.seekedTo - result.seek) < 0.5, `seek landed in the wrong place: ${JSON.stringify(result)}`);
  assert.ok(ranges.some((r) => r.status === 206 && r.range), `no Range responses: ${JSON.stringify(ranges)}`);
  if (!audioOnly) {
    assert.equal(result.width, 1280);
    assert.equal(result.height, 720);
    assert.ok(result.frames > 5, `decoded only ${result.frames} frames`);
  }
  return { ...result, ranges };
}

describe('room composite', () => {
  it('records two participants to MP4, with both visible, moving, and audible', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'room_composite', room, layout: 'grid', output: { kind: 'file', format: 'mp4' } }, 8);
      assert.equal(done.status, 'complete', done.error);
      assert.equal(done.error, '');
      assert.equal(done.format, 'mp4');
      assert.equal(done.output_url, `/api/v1/recordings/${done.id}/file`);
      assert.ok(done.file_size > 10000, `file_size ${done.file_size}`);
      assert.ok(done.ended_at);
      assert.ok(done.room_sid);
      assertDuration(done.duration_ms / 1000, 8);

      const path = await download(api, done.id, `${done.id}.mp4`);
      assert.equal(statSync(path).size, done.file_size);
      const info = await ffprobe(path);
      assert.equal(info.video?.codec_name, 'h264', JSON.stringify(info.video));
      assert.equal(info.audio?.codec_name, 'aac', JSON.stringify(info.audio));
      assert.equal(info.video.width, 1280);
      assert.equal(info.video.height, 720);
      assertDuration(info.duration, 8);

      const whole = await videoStats(path);
      assert.ok(whole.frames > 8 * 15, `only ${whole.frames} frames in 8 seconds`);
      assert.ok(whole.contrast > 30, `the picture is one flat colour: ${JSON.stringify(whole)}`);
      assert.ok(whole.ydif > 0.05, `the picture does not move: ${JSON.stringify(whole)}`);

      // A grid of two puts one person on each half: each half must hold a picture, in colour, moving.
      for (const [name, crop] of [['left', 'crop=620:340:10:190'], ['right', 'crop=620:340:650:190']]) {
        const half = await videoStats(path, crop);
        assert.ok(half.contrast > 30, `${name} half is flat: ${JSON.stringify(half)}`);
        assert.ok(half.sat > 5, `${name} half has no colour, so it holds no fake camera: ${JSON.stringify(half)}`);
        assert.ok(half.ydif > 0.05, `${name} half does not move: ${JSON.stringify(half)}`);
      }

      const sound = await audioStats(path);
      assert.ok(sound.peak > -30, `audio is silent: ${JSON.stringify(sound)}`);
      console.log(`  composite: ${whole.frames} frames, ${info.duration.toFixed(2)}s, ${done.file_size} bytes, ${JSON.stringify(whole)}, audio ${JSON.stringify(sound)}`);

      const played = await assertPlaysInChrome(done);
      console.log(`  played in Chrome: advanced ${played.advanced.toFixed(2)}s, seeked to ${played.seekedTo.toFixed(2)}s of ${played.duration.toFixed(2)}s, ${played.frames} frames, responses ${JSON.stringify(played.ranges.map((r) => r.status))}`);

      // The recorder never appeared to anyone.
      for (const c of [a, b]) {
        const ids = await c.page.evaluate(() => window.harness.identities());
        assert.ok(!ids.includes(done.id), `${c.identity} can see the recorder: ${ids}`);
        assert.equal(ids.length, 1);
      }
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('other recording types', () => {
  // A full-canvas tile leaves nothing dark at the top. A grid of two would.
  const TOP_BAND = 'crop=1280:120:0:0';

  it('records one participant, filling the canvas, with no one else in it', async () => {
    const room = nextRoom();
    // Alice has no microphone, so if Bob is really left out, this recording is silent.
    const alice = await joinRoom(stack, api, 'alice', room, { publish: false });
    await alice.page.evaluate(() => window.harness.publishVideoOnly());
    const bob = await joinRoom(stack, api, 'bob', room);
    try {
      await waitForTracks(bob, 'alice', ['video']);
      await waitForTracks(alice, 'bob', ['audio', 'video']);
      const done = await record({ type: 'participant', room, participant: 'alice', output: { kind: 'file', format: 'mp4' } }, 6);
      assert.equal(done.status, 'complete', done.error);
      assert.equal(done.type, 'participant');
      const path = await download(api, done.id, `${done.id}.mp4`);
      const info = await ffprobe(path);
      assert.equal(info.video.codec_name, 'h264');
      assert.equal(info.video.width, 1280);
      assertDuration(info.duration, 6);
      const top = await videoStats(path, TOP_BAND);
      assert.ok(top.sat > 5, `the top of the frame should hold the one participant, not a dark band: ${JSON.stringify(top)}`);
      const whole = await videoStats(path);
      assert.ok(whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
      const sound = await audioStats(path);
      assert.ok(sound.peak < -50, `Bob's audio got into Alice's recording: ${JSON.stringify(sound)}`);
    } finally {
      await alice.close();
      await bob.close();
    }
  });

  it('a participant recording with audio has that person\'s audio', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'participant', room, participant: 'bob', output: { kind: 'file', format: 'mp4' } }, 5);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}.mp4`);
      assert.ok((await audioStats(path)).peak > -30);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('records audio only, to MP4 with AAC and no video', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'audio_only', room, output: { kind: 'file', format: 'mp4' } }, 6);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}.m4a`);
      const info = await ffprobe(path);
      assert.equal(info.video, undefined, 'audio only should have no video stream');
      assert.equal(info.audio.codec_name, 'aac');
      assertDuration(info.duration, 6);
      const sound = await audioStats(path);
      assert.ok(sound.peak > -30, `silent: ${JSON.stringify(sound)}`);
      await assertPlaysInChrome(done, { audioOnly: true });
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('records one participant\'s audio only, to WebM with Opus', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'audio_only', room, participant: 'alice', output: { kind: 'file', format: 'webm' } }, 5);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}.webm`);
      const info = await ffprobe(path);
      assert.equal(info.video, undefined);
      assert.equal(info.audio.codec_name, 'opus');
      assert.ok((await audioStats(path)).peak > -30);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('records video only, with no audio stream', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'video_only', room, output: { kind: 'file', format: 'mp4' } }, 6);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}.mp4`);
      const info = await ffprobe(path);
      assert.equal(info.audio, undefined, 'video only should have no audio stream');
      assert.equal(info.video.codec_name, 'h264');
      assert.equal(info.video.width, 1280);
      assertDuration(info.duration, 6);
      const whole = await videoStats(path);
      assert.ok(whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('records one participant\'s video only', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'video_only', room, participant: 'bob', output: { kind: 'file', format: 'webm' } }, 5);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}.webm`);
      const info = await ffprobe(path);
      assert.equal(info.audio, undefined);
      assert.equal(info.video.codec_name, 'vp8');
      assert.ok((await videoStats(path, TOP_BAND)).sat > 5);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('records to WebM with VP8 and Opus, seekable in Chrome', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'room_composite', room, layout: 'speaker', output: { kind: 'file', format: 'webm' } }, 8);
      assert.equal(done.status, 'complete', done.error);
      assert.equal(done.format, 'webm');
      const path = await download(api, done.id, `${done.id}.webm`);
      const info = await ffprobe(path);
      assert.equal(info.video.codec_name, 'vp8');
      assert.equal(info.audio.codec_name, 'opus');
      assert.equal(info.video.width, 1280);
      assertDuration(info.duration, 8);
      const whole = await videoStats(path);
      assert.ok(whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
      assert.ok((await audioStats(path)).peak > -30);
      // Speaker layout: one large tile, so the top band holds a picture, unlike a grid of two.
      assert.ok((await videoStats(path, TOP_BAND)).sat > 5, 'speaker layout should give the speaker the full width');
      const played = await assertPlaysInChrome(done);
      console.log(`  WebM played: seeked to ${played.seekedTo.toFixed(2)}s of ${played.duration.toFixed(2)}s, responses ${JSON.stringify(played.ranges.map((r) => r.status))}`);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('streaming to RTMP', () => {
  function rtmpSink(port, out) {
    const child = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', '-listen', '1', '-f', 'flv', '-i', `rtmp://127.0.0.1:${port}/live/secret-key-1234567890`, '-c', 'copy', '-f', 'flv', out], { stdio: ['ignore', 'ignore', 'pipe'] });
    child.exited = new Promise((resolve) => child.on('exit', (code, signal) => resolve({ code, signal })));
    return child;
  }

  it('pushes to a real RTMP sink, and the sink receives moving video and sound', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const out = join(tmp, 'rtmp-sink.flv');
    const sink = rtmpSink(57937, out);
    await sleep(500);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, stream_urls: ['rtmp://127.0.0.1:57937/live/secret-key-1234567890'] }, [201]);
      assert.equal(job.format, '');
      assert.equal(job.output_url, '');
      assert.deepEqual(job.stream_urls, ['rtmp://127.0.0.1:57937/live/{sec...890}']);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(8000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      await Promise.race([sink.exited, sleep(10000)]);
      const info = await ffprobe(out);
      assert.equal(info.video.codec_name, 'h264');
      assert.equal(info.audio.codec_name, 'aac');
      assert.ok(info.duration > 5, `only ${info.duration}s reached the sink`);
      const whole = await videoStats(out);
      assert.ok(whole.frames > 100 && whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
      assert.ok((await audioStats(out)).peak > -30);
      console.log(`  sink received ${info.duration.toFixed(2)}s: ${whole.frames} frames`);
      // Stream-only jobs have nothing to download.
      assert.equal((await api.req('GET', `/recordings/${job.id}`)).status, 404);
    } finally {
      sink.kill('SIGKILL');
      await a.close();
      await b.close();
    }
  });

  it('records to a file and streams into our own ingress at once, and a viewer of the second room sees it', async () => {
    const room = nextRoom();
    const relay = nextRoom();
    const [a, b] = await twoPeople(room);
    const ingress = await api.ok('POST', '/ingress', { name: 'Relay', input_type: 'rtmp', room: relay, participant_identity: 'relayed' }, [201]);
    const viewer = await joinRoom(stack, api, 'viewer', relay, { publish: false });
    try {
      const job = await api.ok('POST', '/egress', {
        type: 'room_composite', room, output: { kind: 'file', format: 'mp4' }, stream_urls: [`${ingress.publish_url}/${ingress.stream_key}`],
      }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await waitForTracks(viewer, 'relayed', ['audio', 'video'], 30000).catch(async (e) => {
        // This has failed now and then, and what it says on its own is not enough to go on.
        const now = await api.ok('GET', `/ingress/${ingress.id}`);
        const logs = await api.ok('GET', `/ingress/${ingress.id}/logs`);
        const seenNow = await received(viewer);
        throw new Error(`${e.message}: ingress ${now.status} ${now.error} ${JSON.stringify(now.stats)}, viewer has ${JSON.stringify(seenNow.map((r) => [r.identity, r.kind]))}, logs ${JSON.stringify(logs.items.map((l) => l.message))}`);
      });
      const seen = await assertVideoFlows(viewer, 'relayed');
      assert.equal(seen.width, 1280);
      assert.equal(seen.height, 720);
      await assertAudioFlows(viewer, 'relayed');
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}.mp4`);
      const info = await ffprobe(path);
      assert.equal(info.video.codec_name, 'h264');
      assert.equal(info.audio.codec_name, 'aac');
      await waitFor('the relay to end', async () => (await api.ok('GET', `/ingress/${ingress.id}`)).status === 'inactive');
    } finally {
      await a.close();
      await b.close();
      await viewer.close();
    }
  });

  it('WebM to a file and H.264 to RTMP at once, which needs a live transcode', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const out = join(tmp, 'rtmp-sink-webm.flv');
    const sink = rtmpSink(57940, out);
    await sleep(500);
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'webm' }, stream_urls: ['rtmp://127.0.0.1:57940/live/secret-key-1234567890'] }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      await sleep(7000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      await Promise.race([sink.exited, sleep(10000)]);
      const file = await ffprobe(await download(api, done.id, `${done.id}.webm`));
      assert.equal(file.video.codec_name, 'vp8');
      assert.equal(file.audio.codec_name, 'opus');
      const stream = await ffprobe(out);
      assert.equal(stream.video.codec_name, 'h264');
      assert.equal(stream.audio.codec_name, 'aac');
      const whole = await videoStats(out);
      assert.ok(whole.frames > 60 && whole.contrast > 30 && whole.ydif > 0.05, JSON.stringify(whole));
    } finally {
      sink.kill('SIGKILL');
      await a.close();
      await b.close();
    }
  });

  it('a destination that cannot be reached fails a stream-only job, without its key in the error', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const key = 'very-secret-key-1234567890';
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, stream_urls: [`rtmp://127.0.0.1:9/live/${key}`] }, [201]);
      const done = await waitForStatus(api, job.id, ['failed', 'complete'], 60000);
      assert.equal(done.status, 'failed');
      assert.ok(done.error.includes('stream destination'), done.error);
      assert.ok(!done.error.includes('secret'), `the key leaked: ${done.error}`);
      console.log(`  error: ${done.error.slice(0, 200)}`);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('rooms changing during a recording', () => {
  const BAND = 'crop=1280:120:0:0';
  const bandSat = async (path, from, to) => {
    const part = join(tmp, `band-${from}.mp4`);
    await run('ffmpeg', ['-hide_banner', '-v', 'error', '-y', '-ss', String(from), '-to', String(to), '-i', path, '-an', '-c', 'copy', part]);
    return (await videoStats(part, BAND)).sat;
  };

  it('updates the layout as people join and leave', async () => {
    const room = nextRoom();
    const a = await joinRoom(stack, api, 'alice', room);
    let b;
    try {
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
      const active = await waitForStatus(api, job.id, 'active', 45000);
      const startedAt = Date.parse(active.started_at);
      const elapsed = () => (Date.now() - startedAt) / 1000;

      // Alone: one tile, the full canvas.
      await sleep(5000);
      b = await joinRoom(stack, api, 'bob', room);
      await waitForTracks(a, 'bob', ['video']);
      const bobIn = elapsed();
      await sleep(6000);
      const aliceOut = elapsed();
      await a.close();
      await sleep(5000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);

      const path = await download(api, done.id, `${done.id}.mp4`);
      const alone = await bandSat(path, 1, Math.floor(bobIn) - 1);
      const together = await bandSat(path, Math.ceil(bobIn) + 2, Math.floor(aliceOut) - 1);
      const bobAlone = await bandSat(path, Math.ceil(aliceOut) + 2, Math.ceil(aliceOut) + 4);
      console.log(`  colour in the top band: alone ${alone.toFixed(1)}, two people ${together.toFixed(1)}, Bob alone ${bobAlone.toFixed(1)}`);
      assert.ok(alone > 5, `one person should fill the canvas: ${alone}`);
      assert.ok(together < 2, `two people should leave the top band dark: ${together}`);
      assert.ok(bobAlone > 5, `after Alice leaves, Bob should fill the canvas: ${bobAlone}`);
    } finally {
      await a.close().catch(() => {});
      await b?.close();
    }
  });

  it('when the room closes, the recording finishes by itself, with a good file', async () => {
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room, departure_timeout: 2, empty_timeout: 300 }, [200, 201]);
    const [a, b] = await twoPeople(room);
    const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
    await waitForStatus(api, job.id, 'active', 45000);
    await sleep(5000);
    // Nobody stops it. Everyone leaves, and the recorder must not hold the room open.
    await a.close();
    await b.close();
    const done = await waitForStatus(api, job.id, ['complete', 'failed'], 45000);
    assert.equal(done.status, 'complete', done.error);
    const path = await download(api, done.id, `${done.id}.mp4`);
    const info = await ffprobe(path);
    assert.ok(info.duration > 4, `${info.duration}`);
    assert.ok(info.video && info.audio);
    assert.equal((await api.req('GET', `/rooms/${room}`)).status, 404, 'the room should have closed');
  });

  it('when the room is deleted, the recording finishes by itself', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'webm' } }, [201]);
    await waitForStatus(api, job.id, 'active', 45000);
    await sleep(3000);
    await api.ok('DELETE', `/rooms/${room}`, undefined, [200, 204]);
    const done = await waitForStatus(api, job.id, ['complete', 'failed'], 45000);
    assert.equal(done.status, 'complete', done.error);
    assert.ok(done.file_size > 0);
    await a.close();
    await b.close();
  });

  it('shows the room as recording only while it is', async () => {
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      assert.equal((await api.ok('GET', `/rooms/${room}`)).active_recording, false);
      const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
      await waitForStatus(api, job.id, 'active', 45000);
      const during = await api.ok('GET', `/rooms/${room}`);
      assert.equal(during.active_recording, true);
      assert.equal(during.participants, 2, 'the recorder must not count as a participant');
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      await waitFor('active_recording to clear', async () => (await api.ok('GET', `/rooms/${room}`)).active_recording === false);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('an empty room', () => {
  it('waits, then aborts with no file if stopped before anyone publishes', async () => {
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
    await waitFor('the recorder to join', async () => (await api.ok('GET', `/rooms/${room}`)).active_recording === true, { timeout: 30000 });
    await sleep(3000);
    const waiting = await api.ok('GET', `/egress/${job.id}`);
    assert.equal(waiting.status, 'starting', 'no media means it has not started');
    assert.equal(waiting.started_at, null);
    await api.ok('POST', `/egress/${job.id}/stop`, {});
    const done = await waitForStatus(api, job.id, ['aborted', 'complete', 'failed'], 30000);
    assert.equal(done.status, 'aborted');
    assert.equal(done.file_size, 0);
    assert.ok(done.error.length > 0);
    const file = await api.req('GET', `/recordings/${job.id}/file`);
    assert.equal(file.status, 404);
    assert.equal(file.body.error.code, 'recording_not_ready');
  });

  it('starts recording when someone arrives', async () => {
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room }, [200, 201]);
    const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
    await waitFor('the recorder to join', async () => (await api.ok('GET', `/rooms/${room}`)).active_recording === true, { timeout: 30000 });
    await sleep(2000);
    assert.equal((await api.ok('GET', `/egress/${job.id}`)).status, 'starting');
    const a = await joinRoom(stack, api, 'alice', room);
    try {
      await waitForStatus(api, job.id, 'active', 30000);
      await sleep(5000);
      await api.ok('POST', `/egress/${job.id}/stop`, {});
      const done = await waitForStatus(api, job.id, ['complete', 'failed'], 40000);
      assert.equal(done.status, 'complete', done.error);
      const info = await ffprobe(await download(api, done.id, `${done.id}.mp4`));
      assert.ok(info.video && info.audio);
      assertDuration(info.duration, 5);
    } finally {
      await a.close();
    }
  });

  it('aborts if the room closes with nobody having come', async () => {
    const room = nextRoom();
    await api.ok('POST', '/rooms', { name: room, empty_timeout: 6 }, [200, 201]);
    const job = await api.ok('POST', '/egress', { type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, [201]);
    const done = await waitForStatus(api, job.id, ['aborted', 'failed', 'complete'], 45000);
    assert.equal(done.status, 'aborted', done.error);
    assert.match(done.error, /room closed/i);
    assert.equal((await api.req('GET', `/rooms/${room}`)).status, 404);
  });

  it('a room that does not exist is refused', async () => {
    const r = await api.req('POST', '/egress', { type: 'room_composite', room: nextRoom(), output: { kind: 'file', format: 'mp4' } });
    assert.equal(r.status, 404);
    assert.equal(r.body.error.code, 'room_not_found');
  });
});

describe('audio and video sync', () => {
  // The times a signal crosses its midpoint on the way up.
  const rising = (times, values) => {
    const hi = Math.max(...values);
    const lo = Math.min(...values);
    const mid = (hi + lo) / 2;
    const out = [];
    for (let i = 1; i < values.length; i++) if (values[i - 1] < mid && values[i] >= mid) out.push(times[i]);
    return out;
  };

  async function flashes(path) {
    const { stdout } = await run('ffprobe', ['-v', 'error', '-f', 'lavfi', '-i', `movie=${path},signalstats`, '-show_entries', 'frame=pts_time:frame_tags=lavfi.signalstats.YAVG', '-of', 'csv=p=0'], { maxBuffer: 1 << 28 });
    const times = [];
    const values = [];
    for (const line of stdout.split('\n')) {
      const [t, y] = line.split(',').map(Number);
      if (Number.isFinite(t) && Number.isFinite(y)) {
        times.push(t);
        values.push(y);
      }
    }
    return rising(times, values);
  }

  async function beeps(path) {
    const { stdout } = await run('ffmpeg', ['-hide_banner', '-v', 'error', '-i', path, '-vn', '-ac', '1', '-ar', '8000', '-f', 's16le', '-'], { maxBuffer: 1 << 28, encoding: 'buffer' });
    const WINDOW = 40; // 5 ms
    const times = [];
    const values = [];
    for (let i = 0; i + WINDOW * 2 <= stdout.length; i += WINDOW * 2) {
      let sum = 0;
      for (let j = 0; j < WINDOW; j++) sum += stdout.readInt16LE(i + j * 2) ** 2;
      times.push(i / 2 / 8000);
      values.push(Math.sqrt(sum / WINDOW));
    }
    return rising(times, values);
  }

  it('keeps a flash and a beep sent together within a small offset, from start to finish', async () => {
    const room = nextRoom();
    const c = await joinRoom(stack, api, 'pattern', room, { publish: false });
    await c.page.evaluate(() => window.harness.publishSyncPattern());
    const viewer = await joinRoom(stack, api, 'viewer', room, { publish: false });
    try {
      await waitForTracks(viewer, 'pattern', ['audio', 'video']);
      const seconds = Number(process.env.SYNC_SECONDS ?? 60);
      const format = process.env.SYNC_FORMAT ?? 'mp4';
      // For long runs, memory is watched while it records: growth here is what would end a long recording.
      const memory = [];
      const watch = setInterval(() => memory.push(Math.round(memoryBelowMB(stack.egress.pid))), 30000);
      // A recording has come out silent now and then. This says where along the way the sound stopped, if it does.
      const trail = [];
      const watchSound = setInterval(async () => {
        const publisher = await c.page.evaluate(() => {
          const ctx = window.harness.syncContext;
          return { state: ctx?.state, time: ctx?.currentTime };
        }).catch(() => null);
        const heard = (await received(viewer).catch(() => [])).find((r) => r.kind === 'audio');
        trail.push({ publisher, viewerPackets: heard?.packetsReceived, viewerEnergy: heard?.totalAudioEnergy });
      }, 10000);
      const done = await record({ type: 'participant', room, participant: 'pattern', output: { kind: 'file', format } }, seconds);
      clearInterval(watch);
      clearInterval(watchSound);
      if (process.env.SYNC_VERBOSE) console.log(`  memory every 30s, MB: ${memory.join(' ')}`);
      assert.equal(done.status, 'complete', done.error);
      const path = await download(api, done.id, `${done.id}-sync.${format}`);
      const v = await flashes(path);
      const a = await beeps(path);
      assert.ok(v.length > seconds - 5, `only ${v.length} flashes in ${seconds}s`);
      if (a.length <= seconds - 5) {
        const log = readFileSync(join(tmp, 'egress.log'), 'utf8').split('\n').filter((l) => l.includes(done.id));
        console.log(`  publisher and viewer over time: ${JSON.stringify(trail)}`);
        console.log(`  what the recorder said:\n${log.join('\n')}`);
      }
      assert.ok(a.length > seconds - 5, `only ${a.length} beeps in ${seconds}s`);
      const offsets = v.map((t) => {
        const nearest = a.reduce((best, x) => (Math.abs(x - t) < Math.abs(best - t) ? x : best), a[0]);
        return (nearest - t) * 1000;
      }).filter((o) => Math.abs(o) < 500);
      assert.ok(offsets.length > seconds - 8, `only ${offsets.length} flashes had a beep nearby`);
      const mean = (x) => x.reduce((p, q) => p + q, 0) / x.length;
      // Jitter buffers take a while to settle, so drift is judged after the first twenty seconds.
      const settled = offsets.slice(20);
      const half = Math.floor(settled.length / 2);
      const first = mean(settled.slice(0, half));
      const last = mean(settled.slice(half));
      const all = mean(offsets);
      const spread = Math.sqrt(mean(offsets.map((o) => (o - all) ** 2)));
      if (process.env.SYNC_VERBOSE) {
        const series = [];
        for (let i = 0; i < offsets.length; i += 10) series.push(mean(offsets.slice(i, i + 10)).toFixed(0));
        console.log(`  every ten seconds: ${series.join(' ')}`);
        const info = await ffprobe(path);
        console.log(`  streams: video ${info.video.duration} s, audio ${info.audio.duration} s, format ${info.duration} s, audio frames ${info.audio.nb_frames}`);
      }
      console.log(`  audio minus video: ${all.toFixed(0)} ms overall, ${first.toFixed(0)} ms in the first half after settling, ${last.toFixed(0)} ms in the second, spread ${spread.toFixed(0)} ms, over ${offsets.length} flashes`);
      // ITU-R BT.1359 puts the edge of what people notice at +45 to -125 ms, and of what they accept at +90 to -185 ms.
      assert.ok(all > -185 && all < 185, `out of sync by ${all.toFixed(0)} ms`);
      assert.ok(Math.abs(last - first) < 40, `drifted from ${first.toFixed(0)} to ${last.toFixed(0)} ms`);
    } finally {
      await c.close();
      await viewer.close();
    }
  }, { timeout: Number(process.env.SYNC_SECONDS ?? 60) * 1000 + 120000 });
});

describe('reporting', () => {
  it('delivers webhooks in order, keeps events, and counts usage', async () => {
    const received = [];
    const hook = createServer((req, res) => {
      let body = '';
      req.on('data', (d) => { body += d; });
      req.on('end', () => {
        received.push(JSON.parse(body));
        res.end('ok');
      });
    });
    await new Promise((r) => hook.listen(0, '127.0.0.1', r));
    const keys = await api.ok('GET', '/keys');
    const made = await api.ok('POST', '/webhooks', {
      url: `http://127.0.0.1:${hook.address().port}/hook`, api_key_id: keys.items[0].id, events: ['egress_started', 'egress_updated', 'egress_ended'],
    }, [201]);
    const room = nextRoom();
    const [a, b] = await twoPeople(room);
    try {
      const done = await record({ type: 'room_composite', room, output: { kind: 'file', format: 'mp4' } }, 13);
      assert.equal(done.status, 'complete', done.error);
      const mine = () => received.filter((e) => e.egressInfo?.egressId === done.id);
      await waitFor('the last webhook', () => mine().some((e) => e.event === 'egress_ended'), { timeout: 20000 });
      // This protocol's JSON leaves out default values, and EGRESS_STARTING is the default of its enum.
      const seen = mine().map((e) => `${e.event}:${e.egressInfo.status ?? 'EGRESS_STARTING'}`);
      console.log(`  webhooks: ${seen.join(', ')}`);
      assert.deepEqual(seen, ['egress_started:EGRESS_STARTING', 'egress_updated:EGRESS_ACTIVE', 'egress_updated:EGRESS_ENDING', 'egress_ended:EGRESS_COMPLETE']);
      const last = mine().at(-1).egressInfo;
      assert.ok(last.fileResults?.[0]?.size > 0, JSON.stringify(last));
      assert.equal(last.roomName, room);

      const logs = await api.ok('GET', `/logs?category=recording&room=${room}`);
      const types = logs.items.map((l) => l.type);
      for (const t of ['egress_started', 'egress_active', 'egress_completed']) assert.ok(types.includes(t), `${t} is not in ${types}`);
      const ended = logs.items.find((l) => l.type === 'egress_completed');
      assert.ok(ended.details.cpu_seconds > 0, JSON.stringify(ended.details));
      console.log(`  job cost: ${JSON.stringify(ended.details)}`);

      // Usage comes in ten-second steps while a room shows as recording.
      const usage = await waitFor('recording minutes in usage', async () => {
        const u = await api.ok('GET', `/analytics/overview`);
        return u.totals.recording_minutes > 0 ? u : null;
      }, { timeout: 25000 });
      console.log(`  usage: ${usage.totals.recording_minutes} recording minutes`);
    } finally {
      hook.close();
      await api.ok('DELETE', `/webhooks/${made.id}`, undefined, [204]);
      await a.close();
      await b.close();
    }
  });

  it('the worker shows in GET /servers', async () => {
    const servers = await api.ok('GET', '/servers');
    const worker = servers.items.find((s) => s.stats.kind === 'egress_worker');
    assert.ok(worker, JSON.stringify(servers));
    assert.ok(worker.stats.max_jobs > 0);
    assert.equal(worker.stats.healthy, true);
    assert.ok(servers.items.some((s) => s.stats.kind === 'ingress_worker'));
  });
});


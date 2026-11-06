// Interoperability suite for Elnova StreamKit's own SDK: real Chrome with fake capture devices, against a real
// server, with the official client in the same rooms. Nothing is simulated. Media goes through WebRTC and
// real decoders, and assertions read getStats and changing pixels.
//
//   node --test sdk.test.mjs                                       starts bin/streamkit-server --dev
//   TARGET_WS=ws://127.0.0.1:7880 node --test sdk.test.mjs         uses a running server
//
// Build the server first: go build -o bin/streamkit-server ./server/cmd/streamkit-server
// Run it against the reference server too. A test the reference fails tests the wrong thing.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createServer as createHttpServer } from 'node:http';
import { createServer as createTcpServer, connect as tcpConnect } from 'node:net';
import { dirname, join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';
import { buildHarness } from './sdk-harness/build.mjs';
import { mintToken, startFixtureServer } from './sdk-harness/fixture.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const apiKey = process.env.API_KEY ?? 'devkey';
const apiSecret = process.env.API_SECRET ?? 'secret';
const ws = process.env.TARGET_WS ?? 'ws://127.0.0.1:7880';
const http = ws.replace(/^ws/, 'http');
const target = new URL(ws);

let serverProcess;
let fixture;
let browser;
let roomCounter = 0;
const nextRoom = () => `sdk-${process.pid}-${++roomCounter}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function serverResponds() {
  try {
    return (await fetch(`${http}/`)).ok;
  } catch {
    return false;
  }
}

before(async () => {
  await buildHarness();
  if (!process.env.TARGET_WS) {
    // Otherwise this would quietly test whatever is already listening there, perhaps another server entirely.
    assert.equal(await serverResponds(), false, `something is already listening on ${http}. Stop it, or set TARGET_WS`);
    serverProcess = spawn(process.env.STREAMKIT_SERVER ?? join(here, '../../bin/streamkit-server'), ['--dev'], { stdio: 'ignore' });
  }
  for (let i = 0; i < 100 && !(await serverResponds()); i++) await sleep(100);
  assert.ok(await serverResponds(), 'server did not start');
  fixture = await startFixtureServer({ apiKey, apiSecret });
  browser = await chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: [
      // Three cameras, because the second produces a format WebRTC cannot encode. See the device test.
      '--use-fake-device-for-media-stream=device-count=3',
      '--use-fake-ui-for-media-stream',
      '--autoplay-policy=no-user-gesture-required',
      '--auto-select-desktop-capture-source=Entire screen',
      '--enable-usermedia-screen-capturing',
    ],
  });
});

after(async () => {
  await browser?.close();
  fixture?.server.close();
  serverProcess?.kill('SIGTERM');
});

const pageFor = { streamkit: 'streamkit.html', official: 'official.html', react: 'react.html' };

/**
 * Opens one client. sdk is which client to run: ours, the official one, or the React page. The official
 * harness and ours share a surface, so everything below drives either.
 */
async function connect(identity, room, opts = {}) {
  const { sdk = 'streamkit', publish = true, autosub = true, waitConnected = true, via = ws, mode = 'v1', ...params } = opts;
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'], viewport: { width: 1280, height: 720 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const query = { ws: via, identity, room, mode, publish: publish ? '1' : '0', autosub: autosub ? '1' : '0' };
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined) query[k] = typeof v === 'object' ? JSON.stringify(v) : String(v);
  }
  await page.goto(`http://127.0.0.1:${fixture.port}/${pageFor[sdk]}?${new URLSearchParams(query)}`);
  const client = {
    page,
    ctx,
    identity,
    sdk,
    errors,
    close: async () => {
      await ctx.close();
      // An uncaught error in the page is a bug whether or not this test noticed it another way.
      if (sdk !== 'official') assert.deepEqual(errors, [], `${identity}: uncaught errors in the page`);
    },
  };
  if (waitConnected) {
    await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
    const state = await page.$eval('#state', (e) => `${e.dataset.state} ${e.textContent}`);
    assert.ok(state.startsWith('connected'), `${identity} failed to connect: ${state}`);
  }
  return client;
}

/** Opens a page with the SDK loaded and nothing connected, for tests of connecting itself. */
async function idlePage() {
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'] });
  const page = await ctx.newPage();
  await page.goto(`http://127.0.0.1:${fixture.port}/streamkit.html?autoconnect=0`);
  await page.waitForSelector('#state[data-state="idle"]', { state: 'attached' });
  return { page, close: () => ctx.close() };
}

const until = (c, fn, arg, timeout = 15000, what = '') =>
  c.page.waitForFunction(fn, arg, { timeout, polling: 100 }).catch(async (e) => {
    throw new Error(`${c.identity}: timed out waiting for ${what || fn}: ${e.message.split('\n')[0]}`);
  });

const stats = (c) => c.page.evaluate(() => window.harness.subscribedStats());
const forSource = (rows, identity, source) => rows.find((r) => r.identity === identity && r.source === source);
const trackSid = (c, identity, source) => c.page.evaluate(([who, s]) => window.harness.publication(who, s)?.trackSid, [identity, source]);

const waitForSubscribed = (c, identity, sources) =>
  until(
    c,
    ([who, want]) => {
      const have = window.harness.subscribedSources(who);
      return want.every((s) => have.includes(s));
    },
    [identity, sources],
    20000,
    `${identity} sources ${sources}`,
  );

// Proves real, moving video is being decoded: the decoder advances and pixels change. With withinMs, that only
// has to happen by a deadline, for a track that can legitimately sit still for a while first.
async function assertVideoFlows(c, identity, source = 'camera', { minFrames = 15, windowMs = 2500, withinMs } = {}) {
  const sid = await trackSid(c, identity, source);
  await until(c, (id) => window.harness.frameHash(id) !== null, sid, 15000, `first decoded frame of ${identity}/${source}`);
  const start = forSource(await stats(c), identity, source);
  const hash1 = await c.page.evaluate((id) => window.harness.frameHash(id), sid);
  const deadline = Date.now() + (withinMs ?? windowMs);
  let end;
  let hash2;
  do {
    await sleep(withinMs ? 500 : windowMs);
    end = forSource(await stats(c), identity, source);
    hash2 = await c.page.evaluate((id) => window.harness.frameHash(id), sid);
  } while (withinMs && Date.now() < deadline && (end.framesDecoded - start.framesDecoded < minFrames || hash1.hash === hash2.hash));

  const frames = end.framesDecoded - start.framesDecoded;
  assert.ok(frames >= minFrames, `${c.identity} decoded only ${frames} frames of ${identity}/${source}`);
  assert.ok(end.bytesReceived > start.bytesReceived, 'no bytes received');
  assert.ok(hash2.width > 0 && hash2.height > 0, 'frame has no size');
  assert.notEqual(hash1.hash, hash2.hash, `${c.identity}: pixels of ${identity}/${source} did not change`);
  return end;
}

// Chrome can hold back the top layer of a new screen share for about three seconds while the layer below keeps
// going. It happens with either SDK publishing, and a fixed window can land entirely inside it.
const SCREEN_SHARE_FLOW = { minFrames: 2, withinMs: 10000 };

async function assertAudioFlows(c, identity, { windowMs = 2000 } = {}) {
  const a = forSource(await stats(c), identity, 'microphone');
  await sleep(windowMs);
  const b = forSource(await stats(c), identity, 'microphone');
  const samples = b.totalSamplesReceived - a.totalSamplesReceived;
  assert.ok(b.packetsReceived - a.packetsReceived > 20, `${c.identity}: audio packets not arriving`);
  // 48kHz stereo, so samples * 2 per second; allow a generous margin for a slow start.
  assert.ok(samples > 24000, `${c.identity}: only ${samples} audio samples of ${identity} in ${windowMs}ms`);
}

const assertMediaFlows = async (c, identity) => {
  await assertVideoFlows(c, identity);
  await assertAudioFlows(c, identity);
};

const received = (c) => c.page.evaluate(() => window.harness.received);

/**
 * Sits between a client's signalling and the server, so a test can do to that connection what a network
 * does, with no help from the client. Only signalling goes through here: media takes its own path, as it
 * does when a client's WebSocket drops while its UDP keeps working.
 */
async function startSignalProxy() {
  const live = new Set();
  let refusing = false;
  const server = createTcpServer((client) => {
    if (refusing) {
      client.destroy();
      return;
    }
    const upstream = tcpConnect(Number(target.port), target.hostname);
    const pair = { client, upstream, muted: false };
    live.add(pair);
    client.on('data', (d) => {
      if (!pair.muted) upstream.write(d);
    });
    upstream.on('data', (d) => {
      if (!pair.muted) client.write(d);
    });
    const gone = () => {
      live.delete(pair);
      client.destroy();
      upstream.destroy();
    };
    client.on('close', gone);
    upstream.on('close', gone);
    client.on('error', gone);
    upstream.on('error', gone);
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  return {
    url: `ws://127.0.0.1:${server.address().port}`,
    /** Drops the connections at once, as if a route had gone. */
    cut() {
      for (const p of [...live]) {
        p.client.destroy();
        p.upstream.destroy();
      }
    },
    /** Leaves the connections open but carrying nothing. New connections are unaffected. */
    silence() {
      for (const p of live) p.muted = true;
    },
    /** Turns new connections away, as a network that has gone down does. */
    refuse(on) {
      refusing = on;
    },
    connections: () => live.size,
    close() {
      this.cut();
      server.close();
    },
  };
}

const adminToken = (room) =>
  mintToken({ apiKey, apiSecret, identity: 'api', room, grants: { roomJoin: false, roomAdmin: true, roomCreate: true, roomList: true } });

/** Calls the room service, as a backend would. */
async function api(method, room, body) {
  const r = await fetch(`${http}/twirp/livekit.RoomService/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${adminToken(room)}` },
    body: JSON.stringify(body),
  });
  return { status: r.status, text: await r.text() };
}

describe('media exchange', () => {
  it('two clients publish and receive real audio and video', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room);
    try {
      await waitForSubscribed(a, 'bob', ['microphone', 'camera']);
      await waitForSubscribed(b, 'alice', ['microphone', 'camera']);
      await assertMediaFlows(a, 'bob');
      await assertMediaFlows(b, 'alice');
      // Connected means the primary connection, per the brief. The order of these events is the contract.
      const events = await a.page.evaluate(() => window.harness.events.map((e) => e.name));
      const order = ['signalConnected', 'connected'].map((n) => events.indexOf(n));
      assert.ok(order[0] >= 0 && order[0] < order[1], `signalConnected must precede connected: ${order}`);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a late joiner receives tracks published before it arrived, without seeing them as new', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    await sleep(1500);
    const late = await connect('late', room, { publish: false });
    try {
      await waitForSubscribed(late, 'alice', ['microphone', 'camera']);
      await assertMediaFlows(late, 'alice');
      // People already there at the join are not arrivals.
      assert.equal(await late.page.evaluate(() => window.harness.eventCount('participantConnected')), 0);
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('participantConnected')), 1);
    } finally {
      await a.close();
      await late.close();
    }
  });

  it('three clients all receive each other', async () => {
    const room = nextRoom();
    const clients = [];
    try {
      for (const id of ['c1', 'c2', 'c3']) clients.push(await connect(id, room));
      for (const c of clients) {
        for (const other of clients) {
          if (other !== c) await waitForSubscribed(c, other.identity, ['microphone', 'camera']);
        }
      }
      await Promise.all(clients.flatMap((c) => clients.filter((o) => o !== c).map((o) => assertVideoFlows(c, o.identity))));
    } finally {
      await Promise.all(clients.map((c) => c.close()));
    }
  });

  it('what a subscriber decodes is what the publisher sent', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await a.page.evaluate(async () => {
        const h = window.harness;
        await h.room.localParticipant.publishTrack(h.canvasTrack([200, 40, 40]), { source: h.TrackSource.Camera, simulcast: false });
      });
      await waitForSubscribed(b, 'alice', ['camera']);
      const sid = await trackSid(b, 'alice', 'camera');
      await until(b, (id) => window.harness.frameHash(id) !== null, sid, 15000, 'first frame');
      const colour = await b.page.evaluate((id) => window.harness.centerColour(id), sid);
      assert.ok(colour[0] > 150 && colour[1] < 90 && colour[2] < 90, `expected red, decoded ${colour}`);
    } finally {
      await a.close();
      await b.close();
    }
  });

  // At low bitrates Chrome probes for bandwidth by resending recent packets over RTX. One of those is an old
  // keyframe packet, and a subscriber that starts from it has skipped the packets since. Nothing can resend
  // those, so its decoder waits for the next keyframe, which takes seconds.
  it('video starts playing straight away for a new subscriber to a low-bitrate track', async () => {
    const stalls = [];
    for (let attempt = 0; attempt < 12; attempt++) {
      const room = nextRoom();
      const a = await connect('alice', room, { publish: false });
      const b = await connect('bob', room, { publish: false });
      try {
        await a.page.evaluate(async () => {
          const h = window.harness;
          await h.room.localParticipant.publishTrack(h.canvasTrack([200, 40, 40]), { source: h.TrackSource.Camera, simulcast: false });
        });
        await waitForSubscribed(b, 'alice', ['camera']);
        const sid = await trackSid(b, 'alice', 'camera');
        await until(b, (id) => window.harness.frameHash(id) !== null, sid, 15000, 'first frame');
        const start = forSource(await stats(b), 'alice', 'camera').framesDecoded;
        await sleep(1500);
        const frames = forSource(await stats(b), 'alice', 'camera').framesDecoded - start;
        if (frames < 10) stalls.push(`attempt ${attempt}: ${frames} frames in 1.5 s`);
      } finally {
        await a.close();
        await b.close();
      }
    }
    assert.deepEqual(stalls, []);
  });

  it('screen sharing reaches other participants as its own track, and stopping removes it', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await a.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(true));
      await waitForSubscribed(b, 'alice', ['screen_share']);
      await assertVideoFlows(b, 'alice', 'screen_share', SCREEN_SHARE_FLOW);
      assert.equal(await a.page.evaluate(() => window.harness.room.localParticipant.isScreenShareEnabled), true);
      await a.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(false));
      await until(b, () => !window.harness.subscribedSources('alice').includes('screen_share'), undefined, 10000, 'screen share to stop');
      assert.equal(await a.page.evaluate(() => window.harness.room.localParticipant.isScreenShareEnabled), false);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('audio goes out as Opus with RED where both ends allow it, and without when a track opts out', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      // A server reports the codec it actually receives, once the first packets have arrived.
      await until(b, () => /audio\/red/i.test(window.harness.publication('alice', 'microphone')?.mimeType ?? ''), undefined, 10000, 'RED');
      await assertAudioFlows(b, 'alice');

      const c = await connect('carol', room, { publish: false });
      try {
        await c.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true, undefined, { red: false }));
        await waitForSubscribed(b, 'carol', ['microphone']);
        await until(b, () => /audio\/opus/i.test(window.harness.publication('carol', 'microphone')?.mimeType ?? ''), undefined, 10000, 'plain Opus');
        await assertAudioFlows(b, 'carol');
      } finally {
        await c.close();
      }
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('codecs', () => {
  for (const [codec, mime] of [['h264', /h264/i], ['vp9', /vp9/i], ['av1', /av1/i]]) {
    it(`publishes and receives ${codec}`, async () => {
      const room = nextRoom();
      const a = await connect('alice', room, { codec });
      const b = await connect('bob', room, { publish: false });
      try {
        await waitForSubscribed(b, 'alice', ['camera']);
        await assertVideoFlows(b, 'alice');
        const seen = await b.page.evaluate(() => window.harness.publication('alice', 'camera').mimeType);
        assert.match(seen, mime);
      } finally {
        await a.close();
        await b.close();
      }
    });
  }

  // One RTP stream holds all the layers, and the server chooses frame by frame, so a viewer gets the size it asks for only
  // if this SDK has asked for the dependency descriptor. The server cannot choose among AV1's layers without it.
  for (const codec of ['vp9', 'av1']) {
    it(`a viewer gets the ${codec} layer it asks for`, async () => {
      const room = nextRoom();
      const a = await connect('alice', room, { codec });
      const b = await connect('bob', room, { publish: false });
      try {
        await waitForSubscribed(b, 'alice', ['camera']);
        await assertVideoFlows(b, 'alice');
        const sid = await trackSid(b, 'alice', 'camera');
        for (const [quality, height] of [['LOW', 180], ['HIGH', 720], ['MEDIUM', 360]]) {
          await b.page.evaluate((q) => {
            const h = window.harness;
            h.publication('alice', 'camera').setVideoQuality(h.VideoQuality[q]);
          }, quality);
          await until(b, ([id, want]) => window.harness.frameHash(id)?.height === want, [sid, height], 20000, `height ${height}`);
          await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
        }
      } finally {
        await a.close();
        await b.close();
      }
    });
  }
});

describe('simulcast', () => {
  it('a camera publishes three layers of the right sizes', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await a.page.waitForFunction(
        async () => {
          const rows = await window.harness.outboundStats('camera');
          return rows.length === 3 && rows.every((r) => r.framesSent > 0);
        },
        undefined,
        { timeout: 20000, polling: 250 },
      );
      const rows = await a.page.evaluate(() => window.harness.outboundStats('camera'));
      const byRid = Object.fromEntries(rows.map((r) => [r.rid, r]));
      assert.deepEqual(Object.keys(byRid).sort(), ['f', 'h', 'q']);
      assert.ok(byRid.q.width <= 320, `q is ${byRid.q.width} wide`);
      assert.ok(byRid.h.width <= 640 && byRid.h.width > byRid.q.width, `h is ${byRid.h.width} wide`);
      assert.ok(byRid.f.width > byRid.h.width, `f is ${byRid.f.width} wide`);
      assert.deepEqual(
        (await a.page.evaluate(() => window.harness.encodingStates('camera'))).map((e) => [e.rid, e.maxBitrate]),
        [['q', 160000], ['h', 450000], ['f', 1700000]],
      );
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a subscriber can switch layers, and video keeps playing across each switch', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
      const sid = await trackSid(b, 'alice', 'camera');
      const setQuality = (q) =>
        b.page.evaluate((q) => {
          const h = window.harness;
          h.publication('alice', 'camera').setVideoQuality(h.VideoQuality[q]);
        }, q);
      const waitForWidth = (max, min) =>
        until(b, ([id, max, min]) => {
          const f = window.harness.frameHash(id);
          return f && f.width <= max && f.width >= min;
        }, [sid, max, min], 20000, `video width within ${min}-${max}`);

      await setQuality('LOW');
      await waitForWidth(400, 1);
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
      await setQuality('HIGH');
      await waitForWidth(5000, 500);
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
      await setQuality('MEDIUM');
      await waitForWidth(800, 400);
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('dynacast', () => {
  const activeRids = (c) => c.page.evaluate(() => window.harness.encodingStates('camera').filter((e) => e.active).map((e) => e.rid).sort());
  const waitForActive = (c, want, timeout = 25000) =>
    until(
      c,
      (want) => JSON.stringify(window.harness.encodingStates('camera').filter((e) => e.active).map((e) => e.rid).sort()) === JSON.stringify(want),
      want,
      timeout,
      `active encodings ${want}`,
    );

  it('stops encoding layers nobody watches, and starts them when someone does', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
      await b.page.evaluate(() => {
        const h = window.harness;
        h.publication('alice', 'camera').setVideoQuality(h.VideoQuality.LOW);
      });
      await waitForActive(a, ['q']);

      // Encoders that are off stop producing frames, which is the point of all this.
      const framesOf = async (rid) => (await a.page.evaluate(() => window.harness.outboundStats('camera'))).find((r) => r.rid === rid)?.framesSent ?? 0;
      await sleep(500);
      const before = { f: await framesOf('f'), q: await framesOf('q') };
      await sleep(2000);
      assert.equal(await framesOf('f'), before.f, 'the top layer kept encoding after dynacast turned it off');
      assert.ok((await framesOf('q')) > before.q, 'the low layer stopped too');
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });

      await b.page.evaluate(() => {
        const h = window.harness;
        h.publication('alice', 'camera').setVideoQuality(h.VideoQuality.HIGH);
      });
      await waitForActive(a, ['f', 'h', 'q']);
      const sid = await trackSid(b, 'alice', 'camera');
      await until(b, (id) => (window.harness.frameHash(id)?.width ?? 0) > 500, sid, 20000, 'high layer to arrive');
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a publisher with no subscribers stops, and a new subscriber gets video straight away', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    try {
      await waitForActive(a, []);
      const late = await connect('late', room, { publish: false });
      try {
        await waitForSubscribed(late, 'alice', ['camera']);
        await assertVideoFlows(late, 'alice');
        assert.notDeepEqual(await activeRids(a), []);
      } finally {
        await late.close();
      }
    } finally {
      await a.close();
    }
  });

  it('turned off, a publisher keeps every layer running', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { dynacast: 0 });
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await b.page.evaluate(() => {
        const h = window.harness;
        h.publication('alice', 'camera').setVideoQuality(h.VideoQuality.LOW);
      });
      await sleep(8000);
      assert.deepEqual(await activeRids(a), ['f', 'h', 'q']);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('adaptive stream', () => {
  it('receives by the size and visibility of the element showing the video', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false, adaptive: 1, attach: 0 });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      const sid = await trackSid(b, 'alice', 'camera');
      await b.page.evaluate((sid) => {
        const v = window.harness.publication('alice', 'camera').track.attach(document.createElement('video'));
        v.muted = true;
        v.dataset.sid = sid;
        v.dataset.kind = 'video';
        v.id = 'adaptive';
        v.style.cssText = 'width:160px;height:90px;display:block';
        document.getElementById('media').appendChild(v);
      }, sid);

      await until(b, (id) => { const f = window.harness.frameHash(id); return f && f.width <= 400; }, sid, 20000, 'low layer for a small element');
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });

      await b.page.evaluate(() => { document.getElementById('adaptive').style.cssText = 'width:1200px;height:675px;display:block'; });
      await until(b, (id) => (window.harness.frameHash(id)?.width ?? 0) >= 1000, sid, 20000, 'high layer for a large element');

      // Out of sight, the server stops sending.
      await b.page.evaluate(() => { document.getElementById('adaptive').style.display = 'none'; });
      await sleep(1500);
      const hidden1 = forSource(await stats(b), 'alice', 'camera');
      await sleep(2000);
      const hidden2 = forSource(await stats(b), 'alice', 'camera');
      assert.ok(hidden2.bytesReceived - hidden1.bytesReceived < 5000, `${hidden2.bytesReceived - hidden1.bytesReceived} bytes arrived for a hidden video`);

      await b.page.evaluate(() => { document.getElementById('adaptive').style.cssText = 'width:320px;height:180px;display:block'; });
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('track lifecycle', () => {
  it('muting and unmuting the microphone reaches other participants', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(false));
      await until(b, () => window.harness.publication('alice', 'microphone')?.isMuted === true, undefined, 10000, 'remote mute');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackMuted'))) >= 1);
      assert.equal(await a.page.evaluate(() => window.harness.room.localParticipant.isMicrophoneEnabled), false);
      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await until(b, () => window.harness.publication('alice', 'microphone')?.isMuted === false, undefined, 10000, 'remote unmute');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackUnmuted'))) >= 1);
      await assertAudioFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('muting the camera releases it, and unmuting brings video back on the same publication', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
      const sid = await trackSid(b, 'alice', 'camera');
      await a.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(false));
      await until(b, () => window.harness.publication('alice', 'camera')?.isMuted === true, undefined, 10000, 'remote camera mute');
      assert.equal(
        await a.page.evaluate(() => window.harness.room.localParticipant.getTrackPublication('camera').track.mediaStreamTrack.readyState),
        'ended',
      );
      await a.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await until(b, () => window.harness.publication('alice', 'camera')?.isMuted === false, undefined, 10000, 'remote camera unmute');
      assert.equal(await trackSid(b, 'alice', 'camera'), sid);
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('unpublishing removes the track for others, and publishing again restores it under a new ID', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      const firstSid = await trackSid(b, 'alice', 'camera');
      await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        await lp.unpublishTrack(lp.getTrackPublication('camera').track);
      });
      await until(b, () => !window.harness.publication('alice', 'camera'), undefined, 15000, 'camera publication to go away');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackUnpublished'))) >= 1);
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackUnsubscribed'))) >= 1);
      assert.ok((await a.page.evaluate(() => window.harness.eventCount('localTrackUnpublished'))) >= 1);

      await a.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      assert.notEqual(firstSid, await trackSid(b, 'alice', 'camera'), 'a republished track gets a new sid');
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a capture the browser ends is unpublished, as when someone stops sharing from the browser toolbar', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await a.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(true));
      await waitForSubscribed(b, 'alice', ['screen_share']);
      await a.page.evaluate(() => {
        const t = window.harness.room.localParticipant.getTrackPublication('screen_share').track.mediaStreamTrack;
        t.stop();
        // The browser fires this when it ends a track itself. Stopping it from script does not.
        t.dispatchEvent(new Event('ended'));
      });
      await until(a, () => !window.harness.room.localParticipant.isScreenShareEnabled, undefined, 5000, 'screen share to end locally');
      await until(b, () => !window.harness.publication('alice', 'screen_share'), undefined, 15000, 'screen share to go at bob');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a quality chosen before subscribing applies once subscribed', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false, autosub: false });
    try {
      await until(b, () => !!window.harness.publication('alice', 'camera'), undefined, 15000, 'publication to appear');
      await b.page.evaluate(() => {
        const h = window.harness;
        const pub = h.publication('alice', 'camera');
        pub.setVideoQuality(h.VideoQuality.LOW);
        pub.setSubscribed(true);
      });
      await waitForSubscribed(b, 'alice', ['camera']);
      const sid = await trackSid(b, 'alice', 'camera');
      await until(b, (id) => { const f = window.harness.frameHash(id); return f && f.width <= 400; }, sid, 20000, 'low layer');
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('switching devices keeps the publication, and what goes out comes from the new device', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera', 'microphone']);
      const sid = await trackSid(b, 'alice', 'camera');
      const devices = await a.page.evaluate(async () => {
        const R = window.harness.Room;
        const lp = window.harness.room.localParticipant;
        return {
          // Chrome's fake_device_1 sends Y16 frames, which no plain RTCPeerConnection will encode either.
          video: (await R.getLocalDevices('videoinput')).filter((d) => d.label !== 'fake_device_1').map((d) => d.deviceId),
          audio: (await R.getLocalDevices('audioinput')).map((d) => d.deviceId),
          currentVideo: lp.getTrackPublication('camera').track.getDeviceId(),
          currentAudio: lp.getTrackPublication('microphone').track.getDeviceId(),
        };
      });
      assert.ok(devices.video.length >= 2, `need two usable fake cameras, have ${devices.video.length}`);
      const nextVideo = devices.video.find((d) => d !== devices.currentVideo);
      const switched = await a.page.evaluate(async (id) => {
        const r = window.harness.room;
        const ok = await r.switchActiveDevice('videoinput', id);
        return { ok, now: r.localParticipant.getTrackPublication('camera').track.getDeviceId(), active: r.getActiveDevice('videoinput') };
      }, nextVideo);
      assert.deepEqual(switched, { ok: true, now: nextVideo, active: nextVideo });
      assert.ok((await a.page.evaluate(() => window.harness.eventCount('activeDeviceChanged'))) >= 1);
      assert.equal(await trackSid(b, 'alice', 'camera'), sid, 'switching device must not publish a new track');
      await assertVideoFlows(b, 'alice');

      const nextAudio = devices.audio.find((d) => d !== devices.currentAudio && d !== 'default' && d !== 'communications');
      if (nextAudio) {
        await a.page.evaluate((id) => window.harness.room.switchActiveDevice('audioinput', id), nextAudio);
        assert.equal(await a.page.evaluate(() => window.harness.room.localParticipant.getTrackPublication('microphone').track.getDeviceId()), nextAudio);
        await assertAudioFlows(b, 'alice');
      }
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('subscriptions', () => {
  it('with auto-subscribe off, nothing arrives until asked for, and it stops when released', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false, autosub: false });
    try {
      await until(b, () => !!window.harness.publication('alice', 'camera'), undefined, 15000, 'publication to appear');
      await sleep(1000);
      assert.deepEqual(await b.page.evaluate(() => window.harness.subscribedSources('alice')), []);
      assert.equal(await b.page.evaluate(() => window.harness.publication('alice', 'camera').subscriptionStatus), 'unsubscribed');

      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setSubscribed(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      assert.equal(await b.page.evaluate(() => window.harness.publication('alice', 'camera').subscriptionStatus), 'subscribed');
      await assertVideoFlows(b, 'alice');

      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setSubscribed(false));
      await until(b, () => window.harness.subscribedSources('alice').length === 0, undefined, 10000, 'unsubscribe');
      assert.equal(await b.page.evaluate(() => window.harness.publication('alice', 'camera').subscriptionStatus), 'unsubscribed');
      // The media goes when the server has renegotiated it away, a moment after the status changes.
      await until(b, () => window.harness.eventCount('trackUnsubscribed') >= 1, undefined, 10000, 'media to go');

      // Subscribing again has to work on whatever the server does with the old section.
      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setSubscribed(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('setEnabled pauses delivery and resumes it without giving up the subscription', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setEnabled(false));
      await sleep(1500);
      const s1 = forSource(await stats(b), 'alice', 'camera');
      await sleep(2000);
      const s2 = forSource(await stats(b), 'alice', 'camera');
      assert.ok(s2.bytesReceived - s1.bytesReceived < 5000, `${s2.bytesReceived - s1.bytesReceived} bytes arrived for a disabled track`);
      assert.deepEqual(await b.page.evaluate(() => window.harness.subscribedSources('alice')).then((s) => s.includes('camera')), true);
      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setEnabled(true));
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it("a publisher's permissions control who can subscribe", async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await a.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(false, []));
      await until(b, () => !window.harness.subscribedSources('alice').includes('camera'), undefined, 10000, 'subscription to be revoked');
      await until(b, () => window.harness.publication('alice', 'camera')?.permissionStatus === 'not_allowed', undefined, 10000, 'publication to report not allowed');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackSubscriptionPermissionChanged'))) >= 1);

      await a.page.evaluate(() =>
        window.harness.room.localParticipant.setTrackSubscriptionPermissions(false, [{ participantIdentity: 'bob', allowAll: true }]),
      );
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');

      await a.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(false, []));
      await until(b, () => !window.harness.subscribedSources('alice').includes('camera'), undefined, 10000, 'subscription to be revoked again');
      await a.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('token permissions', () => {
  it('a token that cannot publish is refused before anything reaches the server, with a status', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false, grants: { canPublish: false } });
    try {
      const failure = await a.page.evaluate(async () => {
        try {
          await window.harness.room.localParticipant.setCameraEnabled(true);
          return 'published';
        } catch (e) {
          return { name: e.name, status: e.status };
        }
      });
      assert.deepEqual(failure, { name: 'PublishTrackError', status: 403 });
      assert.equal(await a.page.evaluate(() => window.harness.room.localParticipant.getTrackPublications().length), 0);
    } finally {
      await a.close();
    }
  });

  it('a token limited to some sources can publish those, and the server enforces it too', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false, grants: { canPublishSources: ['microphone'] } });
    const b = await connect('bob', room, { publish: false });
    try {
      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await waitForSubscribed(b, 'alice', ['microphone']);
      const refused = await a.page.evaluate(async () => {
        try {
          await window.harness.room.localParticipant.setCameraEnabled(true);
          return 'published';
        } catch (e) {
          return e.status;
        }
      });
      assert.equal(refused, 403);

      // With the client's own check out of the way, the server still has to refuse.
      const forced = await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        lp.permissions = { ...lp.permissions, canPublishSources: [] };
        try {
          await lp.setCameraEnabled(true);
          return 'published';
        } catch (e) {
          return { name: e.name };
        }
      });
      assert.notEqual(forced, 'published', 'the server accepted a source the token does not allow');
      assert.equal(await b.page.evaluate(() => !!window.harness.publication('alice', 'camera')), false);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a token that cannot subscribe connects, and receives no media', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false, grants: { canSubscribe: false } });
    try {
      await until(b, () => !!window.harness.publication('alice', 'camera'), undefined, 15000, 'publication to appear');
      await sleep(2000);
      assert.deepEqual(await b.page.evaluate(() => window.harness.subscribedSources('alice')), []);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a token that cannot publish data has its data dropped, and one that can publish only data needs no tracks', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false, grants: { canPublishData: false } });
    const b = await connect('bob', room, { publish: false, grants: { canPublish: false, canPublishData: true } });
    const c = await connect('carol', room, { publish: false });
    try {
      await until(c, () => window.harness.remoteIdentities().length === 2, undefined, 15000, 'peers');
      await sleep(500);
      await a.page.evaluate(() => window.harness.room.localParticipant.publishData(new TextEncoder().encode('from-alice'))).catch(() => {});
      await b.page.evaluate(() => window.harness.room.localParticipant.publishData(new TextEncoder().encode('from-bob')));
      await until(c, () => window.harness.received.some((m) => m.text === 'from-bob'), undefined, 15000, 'data from bob');
      await sleep(1000);
      assert.ok(!(await received(c)).some((m) => m.text === 'from-alice'), 'data from a token without the grant was delivered');
    } finally {
      await a.close();
      await b.close();
      await c.close();
    }
  });
});

describe('data', () => {
  it('delivers reliable and lossy messages, with topics, destinations and the sender stamped by the server', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    const c = await connect('carol', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().length === 2, undefined, 15000, 'peers');
      await until(c, () => window.harness.remoteIdentities().length === 2, undefined, 15000, 'peers');
      await sleep(500);
      await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        const send = (text, opts) => lp.publishData(new TextEncoder().encode(text), opts);
        await send('reliable-all', { reliable: true, topic: 'chat' });
        await send('lossy-all', { reliable: false });
        await send('only-bob', { reliable: true, destinationIdentities: ['bob'] });
        await send('only-bob-lossy', { reliable: false, destinationIdentities: ['bob'] });
      });
      await until(b, () => window.harness.received.length >= 4, undefined, 10000, 'bob to get messages');
      await until(c, () => window.harness.received.length >= 2, undefined, 10000, 'carol to get messages');
      await sleep(500);

      const atBob = await received(b);
      const atCarol = await received(c);
      const reliable = atBob.find((m) => m.text === 'reliable-all');
      assert.equal(reliable.from, 'alice');
      assert.equal(reliable.topic, 'chat');
      assert.equal(reliable.kind, 0);
      assert.equal(atBob.find((m) => m.text === 'lossy-all').kind, 1);
      assert.ok(atBob.some((m) => m.text === 'only-bob'));
      assert.ok(atBob.some((m) => m.text === 'only-bob-lossy'));
      assert.ok(!atCarol.some((m) => m.text.startsWith('only-bob')), 'carol received a message addressed only to bob');
      assert.equal((await received(a)).length, 0, 'sender received its own message');
    } finally {
      await a.close();
      await b.close();
      await c.close();
    }
  });

  it('delivers reliable messages in order and exactly once under a burst', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().length === 1, undefined, 15000, 'peer');
      await sleep(300);
      await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        const padding = 'x'.repeat(2000);
        await Promise.all(Array.from({ length: 500 }, (_, i) => lp.publishData(new TextEncoder().encode(`${i}:${padding}`))));
      });
      await until(b, () => window.harness.received.length >= 500, undefined, 20000, 'all 500 messages');
      const got = (await received(b)).map((m) => Number(m.text.split(':')[0]));
      assert.deepEqual(got, Array.from({ length: 500 }, (_, i) => i));
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('refuses a message too large to send, and says so', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    try {
      const err = await a.page.evaluate(async () => {
        try {
          await window.harness.room.localParticipant.publishData(new Uint8Array(70000));
          return 'sent';
        } catch (e) {
          return e.name;
        }
      });
      assert.equal(err, 'PublishDataError');
    } finally {
      await a.close();
    }
  });

  it('a client with auto-subscribe off still receives data', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false, autosub: false });
    try {
      await until(a, () => window.harness.remoteIdentities().length === 1, undefined, 15000, 'peer');
      await sleep(1000);
      await a.page.evaluate(() => window.harness.room.localParticipant.publishData(new TextEncoder().encode('hello'), { reliable: true }));
      await until(b, () => window.harness.received.some((m) => m.text === 'hello'), undefined, 10000, 'data to reach a manual-subscribe client');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('connecting', () => {
  const attempt = async (url, token, opts) => {
    const c = await idlePage();
    try {
      return await c.page.evaluate(
        async ([url, token, opts]) => {
          const started = performance.now();
          const r = new window.harness.Room();
          const states = [];
          r.on('connectionStateChanged', (s) => states.push(s));
          try {
            await r.connect(url, token, opts);
            await r.disconnect();
            return { connected: true };
          } catch (e) {
            return { name: e.name, reason: e.reason, status: e.status, message: e.message, ms: performance.now() - started, states, state: r.state };
          }
        },
        [url, token, opts],
      );
    } finally {
      await c.close();
    }
  };
  const token = (overrides = {}) => mintToken({ apiKey, apiSecret, identity: 'x', room: nextRoom(), ...overrides });

  it('a bad token is refused with the reason, and the status behind it', async () => {
    const r = await attempt(ws, token({ apiSecret: 'not-the-secret' }));
    assert.equal(r.name, 'ConnectionError');
    assert.equal(r.reason, 'not_allowed');
    assert.equal(r.status, 401);
    assert.ok(r.message.length > 0);
    assert.equal(r.state, 'disconnected');
    assert.deepEqual(r.states, ['connecting', 'disconnected']);
  });

  it('a token for an unknown key, and garbage, are refused the same way', async () => {
    for (const t of [token({ apiKey: 'nobody' }), 'garbage']) {
      const r = await attempt(ws, t);
      assert.equal(r.reason, 'not_allowed', r.message);
      assert.equal(r.status, 401);
    }
  });

  it('a server that cannot be reached says so, promptly', async () => {
    const r = await attempt('ws://127.0.0.1:9', token());
    assert.equal(r.reason, 'server_unreachable', r.message);
    assert.ok(r.ms < 5000, `took ${r.ms} ms`);
  });

  it('a URL that is not a signalling server says so, and is not retried', async () => {
    let upgrades = 0;
    let validates = 0;
    const plain = createHttpServer((req, res) => {
      if (req.url.startsWith('/rtc/validate')) validates++;
      res.setHeader('Access-Control-Allow-Origin', '*');
      res.statusCode = 404;
      res.end('nothing here');
    });
    plain.on('upgrade', (_req, socket) => {
      upgrades++;
      socket.end('HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n');
    });
    await new Promise((r) => plain.listen(0, '127.0.0.1', r));
    try {
      const r = await attempt(`ws://127.0.0.1:${plain.address().port}`, token());
      assert.equal(r.reason, 'service_not_found', r.message);
      assert.equal(r.status, 404);
      assert.equal(upgrades, 1, 'should try the one signalling path, once');
      assert.equal(validates, 1);
    } finally {
      plain.close();
    }
  });

  it('a server that never answers the upgrade times out', async () => {
    const dead = createTcpServer(() => {});
    await new Promise((r) => dead.listen(0, '127.0.0.1', r));
    try {
      const r = await attempt(`ws://127.0.0.1:${dead.address().port}`, token(), { websocketTimeout: 1000 });
      assert.equal(r.reason, 'timeout', r.message);
      assert.ok(r.ms < 4000, `took ${r.ms} ms`);
    } finally {
      dead.close();
    }
  });

  it('peer connections that cannot come up fail the connect, within the timeout', async () => {
    // Relay only, with no relay on offer: signalling works, and ICE has nothing to try.
    const r = await attempt(ws, token(), { peerConnectionTimeout: 2000, rtcConfig: { iceTransportPolicy: 'relay' } });
    assert.equal(r.name, 'ConnectionError');
    assert.match(r.message, /could not establish pc connection/);
    assert.ok(r.ms >= 1500 && r.ms < 6000, `took ${r.ms} ms`);
    assert.equal(r.state, 'disconnected');
  });

  it('disconnecting mid-connect cancels it, and the room can connect again straight away', async () => {
    const c = await idlePage();
    const t = token();
    try {
      const r = await c.page.evaluate(
        async ([url, t]) => {
          const room = new window.harness.Room();
          const first = room.connect(url, t).then(() => 'connected', (e) => e.reason);
          await room.disconnect();
          const again = await room.connect(url, t).then(() => 'connected', (e) => e.message);
          const state = room.state;
          const outcome = await first;
          await room.disconnect();
          return { outcome, again, state };
        },
        [ws, t],
      );
      assert.deepEqual(r, { outcome: 'cancelled', again: 'connected', state: 'connected' });
    } finally {
      await c.close();
    }
  });

  it('connect resolves once the primary connection is up, and a second call returns the same result', async () => {
    const c = await idlePage();
    const t = token();
    try {
      const r = await c.page.evaluate(
        async ([url, t]) => {
          const room = new window.harness.Room();
          const [a, b] = await Promise.all([room.connect(url, t), room.connect(url, t)]);
          const pc = room.engine.subscriberPrimary ? room.engine.subscriber.pc : room.engine.publisher.pc;
          const result = { a, b, pc: pc.connectionState, state: room.state, version: room.serverInfo.version };
          await room.disconnect();
          return result;
        },
        [ws, t],
      );
      assert.equal(r.pc, 'connected');
      assert.equal(r.state, 'connected');
      assert.ok(r.version, 'server version should be known after connecting');
    } finally {
      await c.close();
    }
  });
});

describe('session lifecycle', () => {
  it('leaving is seen by others at once, with the reason', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await a.page.evaluate(() => window.harness.room.disconnect());
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'disconnected');
      assert.equal(await a.page.evaluate(() => window.harness.disconnectReason()), 1 /* CLIENT_INITIATED */);
      await until(b, () => window.harness.remoteIdentities().length === 0, undefined, 10000, 'alice to leave');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('participantDisconnected'))) >= 1);
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackUnsubscribed'))) >= 2);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('stays connected past the ping timeout, and measures the round trip', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    try {
      await sleep(21000);
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'connected');
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('signalReconnecting') + window.harness.eventCount('reconnecting')), 0);
      assert.ok((await a.page.evaluate(() => window.harness.rtt())) >= 0);
    } finally {
      await a.close();
    }
  });

  it('a lost signalling connection resumes, and media keeps flowing throughout', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { via: proxy.url });
    const b = await connect('bob', room);
    try {
      await waitForSubscribed(b, 'alice', ['camera', 'microphone']);
      await waitForSubscribed(a, 'bob', ['camera', 'microphone']);
      await assertVideoFlows(b, 'alice');
      const sid = await a.page.evaluate(() => window.harness.localSid());
      const before = forSource(await stats(b), 'alice', 'camera').framesDecoded;
      const incomingBefore = forSource(await stats(a), 'bob', 'camera').framesDecoded;

      proxy.cut();
      const cutAt = Date.now();
      await until(a, () => window.harness.eventCount('signalReconnecting') >= 1, undefined, 10000, 'signalReconnecting event');
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'signalReconnecting');
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      const resumeMs = Date.now() - cutAt;

      // Video kept going during the outage, in both directions, by the decoders' own count.
      const during = forSource(await stats(b), 'alice', 'camera').framesDecoded - before;
      const expected = (resumeMs / 1000) * 10;
      assert.ok(during >= expected, `bob decoded ${during} frames in ${resumeMs} ms of resume`);
      assert.ok(forSource(await stats(a), 'bob', 'camera').framesDecoded > incomingBefore, 'incoming video stopped during the resume');

      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'connected');
      assert.equal(await a.page.evaluate(() => window.harness.localSid()), sid, 'a resume keeps the session');
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('reconnecting')), 0, 'should not have needed a full reconnect');
      assert.deepEqual(await b.page.evaluate(() => window.harness.remoteIdentities()), ['alice']);
      assert.equal(await b.page.evaluate(() => window.harness.eventCount('participantDisconnected')), 0);
      await assertMediaFlows(b, 'alice');
      await assertMediaFlows(a, 'bob');
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('after a resume, tracks published later still flow both ways', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url });
    const b = await connect('bob', room, { publish: false });
    try {
      await sleep(500);
      proxy.cut();
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      await a.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await b.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      await waitForSubscribed(a, 'bob', ['camera']);
      await assertVideoFlows(b, 'alice');
      await assertVideoFlows(a, 'bob');
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('a publish during a resume waits for it, and goes through', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url });
    const b = await connect('bob', room, { publish: false });
    try {
      await sleep(500);
      proxy.cut();
      await until(a, () => window.harness.eventCount('signalReconnecting') >= 1, undefined, 10000, 'resume to start');
      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('reconnected') >= 1), true, 'publish should not finish before the resume');
      await waitForSubscribed(b, 'alice', ['microphone']);
      await assertAudioFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('a resume with a subscriber offer unanswered still delivers that track, and the one queued behind it', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().includes('bob'), undefined, 15000, 'bob');
      await sleep(500);
      // The socket stays open but carries nothing, so the server's offer for this track is never answered, and
      // the offer for the next has to wait behind it.
      proxy.silence();
      await b.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await sleep(500);
      await b.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await sleep(1000);
      proxy.cut();
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('reconnecting')), 0, 'should not have needed a full reconnect');
      await waitForSubscribed(a, 'bob', ['camera', 'microphone']);
      await assertMediaFlows(a, 'bob');
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('a connection gone silent is found by the ping timeout, and resumes', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url });
    try {
      await sleep(500);
      proxy.silence();
      await until(a, () => window.harness.eventCount('signalReconnecting') >= 1, undefined, 25000, 'ping timeout to start a resume');
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'connected');
    } finally {
      await a.close();
      proxy.close();
    }
  });

  it('a resume uses the token the server refreshed, after the original has expired', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url, ttl: 7 });
    try {
      const refreshed = await a.page.evaluate(() => window.harness.token());
      const original = await a.page.evaluate(async () => (await fetch('/token?identity=x&room=y&ttl=1')).text());
      assert.ok(refreshed && refreshed !== original);
      await sleep(8000);
      proxy.cut();
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'resume with a refreshed token');
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('reconnecting')), 0, 'resume should not have failed over');
    } finally {
      await a.close();
      proxy.close();
    }
  });

  it('reliable data sent around a resume arrives once, in order', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().length === 1, undefined, 15000, 'peer');
      await sleep(500);
      const send = (from, to) =>
        a.page.evaluate(
          async ([from, to]) => {
            const lp = window.harness.room.localParticipant;
            for (let i = from; i < to; i++) await lp.publishData(new TextEncoder().encode(String(i)));
          },
          [from, to],
        );
      await send(0, 20);
      proxy.cut();
      await until(a, () => window.harness.eventCount('signalReconnecting') >= 1, undefined, 10000, 'resume to start');
      // publishData settles while the connection is down, and these go out afterwards.
      await send(20, 40);
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      await send(40, 60);
      await until(b, () => window.harness.received.length >= 60, undefined, 15000, 'all 60 messages');
      await sleep(1000);
      const got = (await received(b)).map((m) => Number(m.text));
      assert.deepEqual(got, Array.from({ length: 60 }, (_, i) => i));
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('events during a resume reach the application after it hears the connection is back', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { publish: false, via: proxy.url });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().includes('bob'), undefined, 15000, 'bob');
      proxy.cut();
      await until(a, () => window.harness.eventCount('signalReconnecting') >= 1, undefined, 10000, 'resume to start');
      await b.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      await waitForSubscribed(a, 'bob', ['microphone']);
      const names = await a.page.evaluate(() => window.harness.events.map((e) => e.name));
      const at = names.indexOf('reconnected');
      assert.ok(names.indexOf('trackPublished') > at, 'trackPublished arrived before reconnected');
      assert.ok(names.indexOf('trackSubscribed') > at, 'trackSubscribed arrived before reconnected');
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('a resume the server refuses becomes a full reconnect', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const a = await connect('alice', room, { via: proxy.url });
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      const oldSid = await a.page.evaluate(() => window.harness.localSid());
      // Keeping the client away while its session goes makes it the client of a server that has forgotten it.
      proxy.refuse(true);
      proxy.cut();
      await until(a, () => window.harness.eventCount('signalReconnecting') >= 1, undefined, 10000, 'resume to start');
      const removed = await api('RemoveParticipant', room, { room, identity: 'alice' });
      assert.equal(removed.status, 200, removed.text);
      await until(b, () => window.harness.remoteIdentities().length === 0, undefined, 10000, 'session to end');
      proxy.refuse(false);

      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 40000, 'reconnected event');
      assert.ok((await a.page.evaluate(() => window.harness.eventCount('reconnecting'))) >= 1, 'should have gone to a full reconnect');
      assert.notEqual(await a.page.evaluate(() => window.harness.localSid()), oldSid);
      await until(b, () => !!window.harness.publication('alice', 'camera')?.track, undefined, 20000, 'camera back at bob');
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
      proxy.close();
    }
  });

  it('a full reconnect makes a new session, publishes everything again, and data still works', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room);
    try {
      await waitForSubscribed(b, 'alice', ['camera', 'microphone']);
      await waitForSubscribed(a, 'bob', ['camera', 'microphone']);
      const oldSid = await a.page.evaluate(() => window.harness.localSid());
      const oldTrack = await trackSid(b, 'alice', 'camera');
      await a.page.evaluate(() => window.harness.room.localParticipant.publishData(new TextEncoder().encode('before')));
      await until(b, () => window.harness.received.some((m) => m.text === 'before'), undefined, 10000, 'data before reconnect');

      await a.page.evaluate(() => window.harness.room.simulateScenario('full-reconnect'));
      await until(a, () => window.harness.eventCount('reconnecting') >= 1, undefined, 10000, 'reconnecting event');
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 30000, 'reconnected event');
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'connected');
      assert.notEqual(await a.page.evaluate(() => window.harness.localSid()), oldSid, 'a full reconnect is a new session');

      await until(b, (old) => { const p = window.harness.publication('alice', 'camera'); return p?.track && p.trackSid !== old; }, oldTrack, 20000, 'republished camera');
      await waitForSubscribed(b, 'alice', ['camera', 'microphone']);
      await waitForSubscribed(a, 'bob', ['camera', 'microphone']);
      await assertMediaFlows(b, 'alice');
      await assertMediaFlows(a, 'bob');

      // Sequence numbers start over. If receivers took that for repeats, this would go missing.
      await a.page.evaluate(() => window.harness.room.localParticipant.publishData(new TextEncoder().encode('after')));
      await until(b, () => window.harness.received.some((m) => m.text === 'after'), undefined, 10000, 'data after reconnect');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a second session with the same identity replaces the first, and others keep receiving', async () => {
    const room = nextRoom();
    const first = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    let second;
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      second = await connect('alice', room);
      await until(first, () => window.harness.connectionState() === 'disconnected', undefined, 15000, 'first session to end');
      assert.equal(await first.page.evaluate(() => window.harness.disconnectReason()), 2 /* DUPLICATE_IDENTITY */);
      await until(b, () => window.harness.remoteIdentities().includes('alice'), undefined, 5000, 'alice to remain');
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
    } finally {
      await first.close();
      await b.close();
      await second?.close();
    }
  });

  it('a leave telling the client to reconnect is obeyed', async (t) => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    try {
      const oldSid = await a.page.evaluate(() => window.harness.localSid());
      await a.page.evaluate(() => window.harness.room.simulateScenario('leave-full-reconnect'));
      await sleep(3000);
      // A development-only scenario, which the reference server does not implement.
      if ((await a.page.evaluate(() => window.harness.eventCount('reconnecting'))) === 0) {
        t.skip('this server ignores the scenario');
        return;
      }
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 30000, 'reconnected event');
      assert.notEqual(await a.page.evaluate(() => window.harness.localSid()), oldSid);
    } finally {
      await a.close();
    }
  });

  it('a server telling a client to go is obeyed, with its reason', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    try {
      await a.page.evaluate(() => window.harness.room.simulateScenario('server-leave'));
      await until(a, () => window.harness.connectionState() === 'disconnected', undefined, 10000, 'session to end');
      assert.equal(await a.page.evaluate(() => window.harness.disconnectReason()), 3 /* SERVER_SHUTDOWN */);
      await sleep(1000);
      assert.equal(await a.page.evaluate(() => window.harness.eventCount('reconnecting')), 0, 'should not try to come back');
    } finally {
      await a.close();
    }
  });
});

describe('signals', () => {
  it('reports active speakers and connection quality', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      // Chrome's fake microphone beeps once a second.
      await until(b, () => window.harness.eventCount('activeSpeakersChanged') >= 1, undefined, 15000, 'active speakers');
      await until(b, () => window.harness.eventCount('connectionQualityChanged') >= 1, undefined, 15000, 'connection quality');
      const quality = await b.page.evaluate(() => window.harness.room.remoteParticipants.get('alice').connectionQuality);
      assert.ok(['excellent', 'good'].includes(quality), `quality on loopback is ${quality}`);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a participant can change its own metadata, name and attributes, and others see it', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(b, () => window.harness.remoteIdentities().includes('alice'), undefined, 15000, 'alice');
      await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        await lp.setMetadata('role=host');
        await lp.setName('Alice A.');
        await lp.setAttributes({ team: 'blue' });
      });
      await until(
        b,
        () => {
          const p = window.harness.room.remoteParticipants.get('alice');
          return p.metadata === 'role=host' && p.name === 'Alice A.' && p.attributes.team === 'blue';
        },
        undefined,
        10000,
        'changes to reach bob',
      );
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('participantMetadataChanged'))) >= 1);
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('participantNameChanged'))) >= 1);
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('participantAttributesChanged'))) >= 1);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('room API, seen from the SDK', () => {
  it('removal and room deletion end the session with their reasons', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(b, () => window.harness.remoteIdentities().includes('alice'), undefined, 15000, 'alice');
      assert.equal((await api('RemoveParticipant', room, { room, identity: 'alice' })).status, 200);
      await until(a, () => window.harness.connectionState() === 'disconnected', undefined, 10000, 'alice to be removed');
      assert.equal(await a.page.evaluate(() => window.harness.disconnectReason()), 4 /* PARTICIPANT_REMOVED */);
      await until(b, () => window.harness.remoteIdentities().length === 0, undefined, 10000, 'alice to leave');

      assert.equal((await api('DeleteRoom', room, { room })).status, 200);
      await until(b, () => window.harness.connectionState() === 'disconnected', undefined, 10000, 'room to close');
      assert.equal(await b.page.evaluate(() => window.harness.disconnectReason()), 5 /* ROOM_DELETED */);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a mute from the server applies to the local track, and updates reach everyone', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      const sid = await trackSid(b, 'alice', 'microphone');
      const muted = await api('MutePublishedTrack', room, { room, identity: 'alice', track_sid: sid, muted: true });
      assert.equal(muted.status, 200, muted.text);
      await until(a, () => window.harness.room.localParticipant.getTrackPublication('microphone').isMuted, undefined, 10000, 'local mute');
      await until(b, () => window.harness.publication('alice', 'microphone').isMuted, undefined, 10000, 'remote mute');
      assert.equal(await a.page.evaluate(() => window.harness.room.localParticipant.getTrackPublication('microphone').track.mediaStreamTrack.enabled), false);

      const updated = await api('UpdateParticipant', room, { room, identity: 'alice', metadata: 'from-api', permission: { can_subscribe: true, can_publish: false, can_publish_data: true } });
      assert.equal(updated.status, 200, updated.text);
      await until(a, () => window.harness.room.localParticipant.metadata === 'from-api' && window.harness.room.localParticipant.permissions?.canPublish === false, undefined, 10000, 'update at alice');
      await until(b, () => window.harness.room.remoteParticipants.get('alice').metadata === 'from-api', undefined, 10000, 'update at bob');

      const meta = await api('UpdateRoomMetadata', room, { room, metadata: 'topic=standup' });
      assert.equal(meta.status, 200, meta.text);
      await until(a, () => window.harness.room.metadata === 'topic=standup', undefined, 10000, 'room metadata');
      await until(a, () => window.harness.eventCount('roomMetadataChanged') >= 1, undefined, 5000, 'room metadata event');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('data sent from the server arrives, with its topic', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await sleep(1000);
      const r = await api('SendData', room, { room, data: Buffer.from('from-server').toString('base64'), kind: 'RELIABLE', topic: 'announcements', destination_identities: ['bob'] });
      assert.equal(r.status, 200, r.text);
      await until(b, () => window.harness.received.length >= 1, undefined, 10000, 'server message');
      const [got] = await received(b);
      assert.equal(got.text, 'from-server');
      assert.equal(got.topic, 'announcements');
      await sleep(300);
      assert.equal((await received(a)).length, 0);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

// The same room, two SDKs. The official client runs with its defaults, which on this server means whichever
// path it settles on, unless a test asks for the dual connection path that matches ours.
describe('mixed rooms with the official client', () => {
  for (const mode of ['v1', 'dual']) {
    it(`audio and video flow both ways (official client path ${mode})`, async () => {
      const room = nextRoom();
      const ours = await connect('ours', room);
      const theirs = await connect('theirs', room, { sdk: 'official', mode });
      try {
        await waitForSubscribed(ours, 'theirs', ['microphone', 'camera']);
        await waitForSubscribed(theirs, 'ours', ['microphone', 'camera']);
        await assertMediaFlows(ours, 'theirs');
        await assertMediaFlows(theirs, 'ours');
      } finally {
        await ours.close();
        await theirs.close();
      }
    });
  }

  it('a late joiner on either SDK receives what the other published', async () => {
    const room = nextRoom();
    const first = await connect('first', room, { sdk: 'official' });
    const second = await connect('second', room);
    try {
      await sleep(1500);
      const lateOurs = await connect('late-ours', room, { publish: false });
      const lateTheirs = await connect('late-theirs', room, { publish: false, sdk: 'official' });
      try {
        await waitForSubscribed(lateOurs, 'first', ['camera']);
        await waitForSubscribed(lateOurs, 'second', ['camera']);
        await waitForSubscribed(lateTheirs, 'first', ['camera']);
        await waitForSubscribed(lateTheirs, 'second', ['camera']);
        await assertVideoFlows(lateOurs, 'first');
        await assertVideoFlows(lateTheirs, 'second');
      } finally {
        await lateOurs.close();
        await lateTheirs.close();
      }
    } finally {
      await first.close();
      await second.close();
    }
  });

  it('simulcast layers from each SDK can be selected from the other', async () => {
    const room = nextRoom();
    const ours = await connect('ours', room);
    const theirs = await connect('theirs', room, { sdk: 'official' });
    try {
      const check = async (viewer, publisher) => {
        await waitForSubscribed(viewer, publisher, ['camera']);
        const sid = await trackSid(viewer, publisher, 'camera');
        const set = (q) =>
          viewer.page.evaluate(
            ([who, q]) => {
              const h = window.harness;
              h.publication(who, 'camera').setVideoQuality(h.VideoQuality[q]);
            },
            [publisher, q],
          );
        await set('LOW');
        await until(viewer, ([id]) => { const f = window.harness.frameHash(id); return f && f.width <= 400; }, [sid], 20000, `${publisher} low layer`);
        await set('HIGH');
        await until(viewer, ([id]) => (window.harness.frameHash(id)?.width ?? 0) >= 500, [sid], 20000, `${publisher} high layer`);
        await assertVideoFlows(viewer, publisher, 'camera', { minFrames: 10 });
      };
      await check(ours, 'theirs');
      await check(theirs, 'ours');
    } finally {
      await ours.close();
      await theirs.close();
    }
  });

  it('data passes between the SDKs, with topics and destinations', async () => {
    const room = nextRoom();
    const ours = await connect('ours', room, { publish: false });
    const theirs = await connect('theirs', room, { publish: false, sdk: 'official' });
    const third = await connect('third', room, { publish: false, sdk: 'official' });
    try {
      await until(ours, () => window.harness.remoteIdentities().length === 2, undefined, 15000, 'peers');
      await until(theirs, () => window.harness.remoteIdentities().length === 2, undefined, 15000, 'peers');
      await sleep(500);
      const send = (c, text, opts) => c.page.evaluate(([text, opts]) => window.harness.room.localParticipant.publishData(new TextEncoder().encode(text), opts), [text, opts]);
      await send(ours, 'ours-reliable', { reliable: true, topic: 'chat' });
      await send(ours, 'ours-lossy', { reliable: false });
      await send(ours, 'ours-only-theirs', { reliable: true, destinationIdentities: ['theirs'] });
      await send(theirs, 'theirs-reliable', { reliable: true, topic: 'chat' });
      await send(theirs, 'theirs-lossy', { reliable: false });
      await send(theirs, 'theirs-only-ours', { reliable: true, destinationIdentities: ['ours'] });

      await until(theirs, () => window.harness.received.length >= 3, undefined, 10000, 'official client to receive');
      await until(ours, () => window.harness.received.length >= 3, undefined, 10000, 'our client to receive');
      await sleep(500);
      const atTheirs = await received(theirs);
      const atOurs = await received(ours);
      const atThird = await received(third);
      const fromOurs = atTheirs.find((m) => m.text === 'ours-reliable');
      assert.equal(fromOurs.from, 'ours');
      assert.equal(fromOurs.topic, 'chat');
      assert.ok(atTheirs.some((m) => m.text === 'ours-lossy'));
      assert.ok(atTheirs.some((m) => m.text === 'ours-only-theirs'));
      const fromTheirs = atOurs.find((m) => m.text === 'theirs-reliable');
      assert.equal(fromTheirs.from, 'theirs');
      assert.equal(fromTheirs.topic, 'chat');
      assert.ok(atOurs.some((m) => m.text === 'theirs-lossy'));
      assert.ok(atOurs.some((m) => m.text === 'theirs-only-ours'));
      assert.ok(!atThird.some((m) => m.text.includes('only')), 'a third party received addressed messages');
    } finally {
      await ours.close();
      await theirs.close();
      await third.close();
    }
  });

  it('mute, unpublish, screen share and leaving are seen across SDKs', async () => {
    const room = nextRoom();
    const ours = await connect('ours', room);
    const theirs = await connect('theirs', room, { sdk: 'official' });
    try {
      await waitForSubscribed(theirs, 'ours', ['microphone', 'camera']);
      await waitForSubscribed(ours, 'theirs', ['microphone', 'camera']);

      await ours.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(false));
      await until(theirs, () => window.harness.publication('ours', 'microphone')?.isMuted === true, undefined, 10000, 'mute seen by official client');
      await ours.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await until(theirs, () => window.harness.publication('ours', 'microphone')?.isMuted === false, undefined, 10000, 'unmute seen by official client');
      await theirs.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(false));
      await until(ours, () => window.harness.publication('theirs', 'microphone')?.isMuted === true, undefined, 10000, 'mute seen by our client');
      await theirs.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await until(ours, () => window.harness.publication('theirs', 'microphone')?.isMuted === false, undefined, 10000, 'unmute seen by our client');

      await ours.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(true));
      await waitForSubscribed(theirs, 'ours', ['screen_share']);
      await assertVideoFlows(theirs, 'ours', 'screen_share', SCREEN_SHARE_FLOW);
      await ours.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(false));
      await until(theirs, () => !window.harness.subscribedSources('ours').includes('screen_share'), undefined, 10000, 'our screen share to stop');

      await theirs.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(true));
      await waitForSubscribed(ours, 'theirs', ['screen_share']);
      await assertVideoFlows(ours, 'theirs', 'screen_share', SCREEN_SHARE_FLOW);
      await theirs.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(false));
      await until(ours, () => !window.harness.subscribedSources('theirs').includes('screen_share'), undefined, 10000, 'their screen share to stop');

      await ours.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        await lp.unpublishTrack(lp.getTrackPublication('camera').track);
      });
      await until(theirs, () => !window.harness.publication('ours', 'camera'), undefined, 15000, 'our camera to go away');
      await ours.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await waitForSubscribed(theirs, 'ours', ['camera']);
      await assertVideoFlows(theirs, 'ours');

      await theirs.page.evaluate(() => window.harness.room.disconnect());
      await until(ours, () => !window.harness.remoteIdentities().includes('theirs'), undefined, 10000, 'official client to leave');
      await ours.page.evaluate(() => window.harness.room.disconnect());
    } finally {
      await ours.close();
      await theirs.close();
    }
  });

  it('a resume by either client leaves the other receiving', async () => {
    const room = nextRoom();
    const proxy = await startSignalProxy();
    const ours = await connect('ours', room, { via: proxy.url });
    const theirs = await connect('theirs', room, { sdk: 'official' });
    try {
      await waitForSubscribed(theirs, 'ours', ['camera', 'microphone']);
      await waitForSubscribed(ours, 'theirs', ['camera', 'microphone']);
      proxy.cut();
      await until(ours, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'our client to resume');
      await assertMediaFlows(theirs, 'ours');
      await assertMediaFlows(ours, 'theirs');

      await theirs.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
      await until(theirs, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'official client to resume');
      await assertVideoFlows(ours, 'theirs');
      await assertVideoFlows(theirs, 'ours');
    } finally {
      await ours.close();
      await theirs.close();
      proxy.close();
    }
  });

  it('a track from our SDK and a subscription permission from the official one work together', async () => {
    const room = nextRoom();
    const ours = await connect('ours', room, { publish: false });
    const theirs = await connect('theirs', room);
    const official = await connect('official', room, { sdk: 'official' });
    try {
      await waitForSubscribed(ours, 'official', ['camera']);
      await official.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(false, []));
      await until(ours, () => !window.harness.subscribedSources('official').includes('camera'), undefined, 10000, 'permission revoked');
      await until(ours, () => window.harness.publication('official', 'camera')?.permissionStatus === 'not_allowed', undefined, 10000, 'not allowed reported');
      await official.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(true));
      await waitForSubscribed(ours, 'official', ['camera']);
      await assertVideoFlows(ours, 'official');
      await waitForSubscribed(official, 'theirs', ['camera']);
      await assertVideoFlows(official, 'theirs');
    } finally {
      await ours.close();
      await theirs.close();
      await official.close();
    }
  });
});

describe('browsers that hold audio back', () => {
  it('the room says so, and startAudio from a click starts it', async () => {
    const strict = await chromium.launch({
      executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
      headless: true,
      args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=document-user-activation-required'],
    });
    const room = nextRoom();
    const a = await connect('alice', room);
    // Without capture permissions, because a page holding those is allowed to play whatever it likes.
    const ctx = await strict.newContext();
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    try {
      await page.goto(`http://127.0.0.1:${fixture.port}/streamkit.html?${new URLSearchParams({ ws, identity: 'bob', room, publish: '0', unmuted: '1' })}`);
      // Everything Playwright runs in a page counts as a user gesture, which would lift the very restriction
      // this is about, so nothing can touch the page until its audio has already tried to play.
      await sleep(6000);
      const held = await page.evaluate(() => ({
        can: window.harness.room.canPlaybackAudio,
        paused: document.querySelector('audio')?.paused,
        events: window.harness.eventCount('audioPlaybackChanged'),
      }));
      assert.deepEqual(held, { can: false, paused: true, events: 1 });

      await page.evaluate(() => {
        const button = document.createElement('button');
        button.id = 'start';
        button.textContent = 'start';
        button.onclick = () => window.harness.room.startAudio();
        document.body.appendChild(button);
      });
      await page.click('#start');
      await page.waitForFunction(() => window.harness.room.canPlaybackAudio === true, undefined, { timeout: 5000 });
      assert.equal(await page.evaluate(() => document.querySelector('audio').paused), false);
      assert.deepEqual(errors, []);
    } finally {
      await ctx.close();
      await strict.close();
      await a.close();
    }
  });
});

describe('React package', () => {
  const dom = (c, fn, arg) => c.page.evaluate(fn, arg);
  const reactFlows = async (c, identity, source = 'camera') => {
    await until(c, ([i, s]) => window.harness.frameHash(i, s) !== null, [identity, source], 20000, `first frame of ${identity}`);
    const hash = () => c.page.evaluate(([i, s]) => window.harness.frameHash(i, s), [identity, source]);
    const h1 = await hash();
    const deadline = Date.now() + (source === 'screen_share' ? SCREEN_SHARE_FLOW.withinMs : 2000);
    let h2;
    do {
      await sleep(500);
      h2 = await hash();
    } while (h2.hash === h1.hash && Date.now() < deadline);
    assert.ok(h2.width > 0, 'frame has no size');
    assert.notEqual(h1.hash, h2.hash, `pixels of ${identity} did not change in the page`);
    assert.equal(h2.paused, false, 'the video element is paused');
  };

  it('a meeting page shows state, participants and live video from its hooks', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { sdk: 'react' });
    const b = await connect('bob', room, { sdk: 'react' });
    try {
      await until(a, () => document.getElementById('connection')?.textContent === 'connected', undefined, 15000, 'connected in the page');
      await until(a, () => document.querySelectorAll('#participants li').length === 2, undefined, 15000, 'two participants');
      await until(b, () => document.querySelectorAll('#participants li').length === 2, undefined, 15000, 'two participants');
      assert.equal(await dom(a, () => document.querySelector('#participants li').dataset.identity), 'alice', 'the local participant comes first');

      await until(a, () => document.getElementById('local').dataset.camera === 'true' && document.getElementById('local').dataset.microphone === 'true', undefined, 15000, 'local flags');
      await until(a, () => document.querySelector('[data-streamkit-participant-tile][data-identity="bob"] video'), undefined, 20000, 'bob tile with video');
      await reactFlows(a, 'bob');
      await reactFlows(b, 'alice');
      // One's own video shows in its tile too.
      await reactFlows(a, 'alice');

      // Remote audio plays through the renderer, and never one's own.
      await until(a, () => window.harness.audioElements().length === 1, undefined, 15000, 'audio element');
      const [audio] = await dom(a, () => window.harness.audioElements());
      assert.deepEqual(audio, { identity: 'bob', live: true, paused: false });
      const p1 = await dom(a, () => window.harness.audioStats('bob'));
      await sleep(1000);
      assert.ok((await dom(a, () => window.harness.audioStats('bob'))) > p1, 'audio packets are not arriving');

      await until(a, () => document.getElementById('quality').textContent === 'excellent' || document.getElementById('quality').textContent === 'good', undefined, 15000, 'connection quality');
      await until(a, () => Number(document.getElementById('devices').textContent) >= 1, undefined, 5000, 'devices listed');
    } finally {
      await a.close();
      await b.close();
    }
  });

  // One listener only: two pages in one browser playing each other's beeps cancel out under echo cancellation.
  it('useActiveSpeakers names whoever is speaking', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { sdk: 'react' });
    const b = await connect('bob', room, { sdk: 'react', publish: false });
    try {
      await until(b, () => document.getElementById('speakers').textContent === 'alice', undefined, 20000, 'alice to show as speaking');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('source props control publishing, and others see the change', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { sdk: 'react' });
    const b = await connect('bob', room, { sdk: 'react', publish: false });
    try {
      await until(b, () => document.querySelector('[data-streamkit-participant-tile][data-identity="alice"] video'), undefined, 20000, 'alice video');
      await a.page.evaluate(() => window.harness.setProps({ video: false }));
      await until(a, () => document.getElementById('local').dataset.camera === 'false', undefined, 10000, 'camera off locally');
      await until(b, () => document.querySelector('[data-streamkit-participant-tile][data-identity="alice"]').dataset.videoMuted === 'true', undefined, 10000, 'camera off at bob');
      await a.page.evaluate(() => window.harness.setProps({ video: true }));
      await until(b, () => document.querySelector('[data-streamkit-participant-tile][data-identity="alice"]').dataset.videoMuted === 'false', undefined, 15000, 'camera back at bob');
      await reactFlows(b, 'alice');

      await a.page.evaluate(() => window.harness.setProps({ screen: true }));
      await until(b, () => document.querySelector('video[data-identity="alice"][data-source="screen_share"]'), undefined, 20000, 'screen share tile');
      await reactFlows(b, 'alice', 'screen_share');
      await a.page.evaluate(() => window.harness.setProps({ screen: false }));
      await until(b, () => !document.querySelector('video[data-identity="alice"][data-source="screen_share"]'), undefined, 15000, 'screen share tile to go');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('useDataChannel sends and receives by topic', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { sdk: 'react', publish: false });
    const b = await connect('bob', room, { sdk: 'react', publish: false });
    try {
      await until(a, () => document.querySelectorAll('#participants li').length === 2, undefined, 15000, 'two participants');
      await sleep(500);
      await a.page.evaluate(() => window.harness.send('hello from react'));
      await until(b, () => document.getElementById('last-message').textContent === 'hello from react', undefined, 10000, 'message in the page');
      assert.deepEqual(await dom(b, () => window.harness.received), ['hello from react']);
      // Another topic is someone else's traffic.
      await a.page.evaluate(() => window.harness.localParticipant.publishData(new TextEncoder().encode('other'), { topic: 'other' }));
      await sleep(500);
      assert.deepEqual(await dom(b, () => window.harness.received), ['hello from react']);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('leaving by prop and by unmount both tell the room, and coming back works', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { sdk: 'react', publish: false });
    const b = await connect('bob', room, { sdk: 'react', publish: false });
    try {
      await until(b, () => document.querySelectorAll('#participants li').length === 2, undefined, 15000, 'two participants');
      await a.page.evaluate(() => window.harness.setProps({ connect: false }));
      await until(a, () => document.getElementById('connection').textContent === 'disconnected', undefined, 10000, 'disconnected in the page');
      await until(b, () => document.querySelectorAll('#participants li').length === 1, undefined, 10000, 'alice gone at bob');

      await a.page.evaluate(() => window.harness.setProps({ connect: true }));
      await until(a, () => document.getElementById('connection').textContent === 'connected', undefined, 15000, 'back in the page');
      await until(b, () => document.querySelectorAll('#participants li').length === 2, undefined, 15000, 'alice back at bob');

      await a.page.evaluate(() => window.harness.setProps({ mounted: false }));
      await until(b, () => document.querySelectorAll('#participants li').length === 1, undefined, 10000, 'alice gone after unmount');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a page built on the React package and one on the official client share a room', async () => {
    const room = nextRoom();
    const r = await connect('reacts', room, { sdk: 'react' });
    const o = await connect('official', room, { sdk: 'official' });
    try {
      await waitForSubscribed(o, 'reacts', ['camera', 'microphone']);
      await assertMediaFlows(o, 'reacts');
      await until(r, () => document.querySelector('[data-streamkit-participant-tile][data-identity="official"] video'), undefined, 20000, 'official tile');
      await reactFlows(r, 'official');
    } finally {
      await r.close();
      await o.close();
    }
  });
});

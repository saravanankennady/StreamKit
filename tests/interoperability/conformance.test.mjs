// Conformance suite: the official livekit-client in real Chrome, with fake capture devices, against a
// server. Nothing here is simulated: media goes through real WebRTC and decoders.
//
//   node --test conformance.test.mjs                       starts bin/streamkit-server --dev
//   TARGET_WS=ws://127.0.0.1:7880 node --test conformance.test.mjs     uses a running server
//
// Run it against the reference server too. A test the reference fails tests the wrong thing.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';
import { mintToken, startFixtureServer } from './oracle/fixture-server.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const apiKey = process.env.API_KEY ?? 'devkey';
const apiSecret = process.env.API_SECRET ?? 'secret';
const ws = process.env.TARGET_WS ?? 'ws://127.0.0.1:7880';
const http = ws.replace(/^ws/, 'http');
const mode = process.env.CLIENT_MODE ?? 'v1'; // v1 lets the client choose its path and fall back

let serverProcess;
let fixture;
let browser;
let roomCounter = 0;
const nextRoom = () => `conf-${process.pid}-${++roomCounter}`;
const signalPaths = {};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitForServer() {
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`${http}/`);
      if (r.ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error('server did not start');
}

before(async () => {
  if (!process.env.TARGET_WS) {
    serverProcess = spawn(join(here, '../../bin/streamkit-server'), ['--dev'], { stdio: 'ignore' });
  }
  await waitForServer();
  fixture = await startFixtureServer({ apiKey, apiSecret });
  browser = await chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: [
      '--use-fake-device-for-media-stream',
      '--use-fake-ui-for-media-stream',
      '--autoplay-policy=no-user-gesture-required',
      '--auto-select-desktop-capture-source=Entire screen',
      '--enable-usermedia-screen-capturing',
    ],
  });
  // A client asked for /rtc/v1 falls back to /rtc on a 404 without telling the page, so a pass in v1 mode says nothing about
  // /rtc/v1 unless these counts say it was used. Pages without a mode are left out: their client chooses for itself.
  const newContext = browser.newContext.bind(browser);
  browser.newContext = async (...args) => {
    const ctx = await newContext(...args);
    ctx.on('page', (page) =>
      page.on('websocket', (socket) => {
        if (!new URL(page.url()).searchParams.has('mode')) return;
        const path = new URL(socket.url()).pathname;
        signalPaths[path] = (signalPaths[path] ?? 0) + 1;
      }),
    );
    return ctx;
  };
});

after(async () => {
  await browser?.close();
  fixture?.server.close();
  serverProcess?.kill('SIGTERM');
});

async function connect(identity, room, { publish = true, autosub = true, waitConnected = true, dynacast = false, init } = {}) {
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'], viewport: { width: 1280, height: 720 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  if (init) await page.addInitScript(init);
  const q = new URLSearchParams({ ws, identity, room, mode, publish: publish ? '1' : '0', autosub: autosub ? '1' : '0', dynacast: dynacast ? '1' : '0' });
  await page.goto(`http://127.0.0.1:${fixture.port}/?${q}`);
  if (waitConnected) {
    await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
    const state = await page.$eval('#state', (e) => e.dataset.state + ' ' + e.textContent);
    assert.ok(state.startsWith('connected'), `${identity} failed to connect: ${state}`);
  }
  return { page, ctx, identity, errors, close: () => ctx.close() };
}

const until = (c, fn, arg, timeout = 15000, what = '') =>
  c.page.waitForFunction(fn, arg, { timeout, polling: 100 }).catch((e) => {
    throw new Error(`${c.identity}: timed out waiting for ${what || fn}: ${e.message}`);
  });

const stats = (c) => c.page.evaluate(() => window.harness.subscribedStats());
const forSource = (rows, identity, source) => rows.find((r) => r.identity === identity && r.source === source);

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

// Proves real, moving video is being decoded: the decoder advances and pixels change.
async function assertVideoFlows(c, identity, source = 'camera', { minFrames = 15, windowMs = 2500 } = {}) {
  const sid = await c.page.evaluate(([who, s]) => window.harness.publication(who, s)?.trackSid, [identity, source]);
  await until(
    c,
    (id) => window.harness.frameHash(id) !== null,
    sid,
    15000,
    `first decoded frame of ${identity}/${source}`,
  );
  const before = forSource(await stats(c), identity, source);
  const hash1 = await c.page.evaluate((id) => window.harness.frameHash(id), sid);
  await sleep(windowMs);
  const afterStats = forSource(await stats(c), identity, source);
  const hash2 = await c.page.evaluate((id) => window.harness.frameHash(id), sid);

  const frames = afterStats.framesDecoded - before.framesDecoded;
  assert.ok(frames >= minFrames, `${c.identity} decoded only ${frames} frames of ${identity}/${source}`);
  assert.ok(afterStats.bytesReceived > before.bytesReceived, 'no bytes received');
  assert.ok(hash2.width > 0 && hash2.height > 0, 'frame has no size');
  assert.notEqual(hash1.hash, hash2.hash, `${c.identity}: pixels of ${identity}/${source} did not change`);
  return afterStats;
}

async function assertAudioFlows(c, identity, { windowMs = 2000 } = {}) {
  const a = forSource(await stats(c), identity, 'microphone');
  await sleep(windowMs);
  const b = forSource(await stats(c), identity, 'microphone');
  const samples = b.totalSamplesReceived - a.totalSamplesReceived;
  assert.ok(b.packetsReceived - a.packetsReceived > 20, `${c.identity}: audio packets not arriving`);
  // 48kHz stereo, so samples * 2 per second; allow a generous margin for a slow start.
  assert.ok(samples > 24000, `${c.identity}: only ${samples} audio samples of ${identity} in ${windowMs}ms`);
}

describe('media exchange', () => {
  it('two clients publish and receive real audio and video', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room);
    try {
      await waitForSubscribed(a, 'bob', ['microphone', 'camera']);
      await waitForSubscribed(b, 'alice', ['microphone', 'camera']);
      await assertVideoFlows(a, 'bob');
      await assertVideoFlows(b, 'alice');
      await assertAudioFlows(a, 'bob');
      await assertAudioFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a late joiner receives tracks published before it arrived', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    await sleep(1500);
    const late = await connect('late', room, { publish: false });
    try {
      await waitForSubscribed(late, 'alice', ['microphone', 'camera']);
      await assertVideoFlows(late, 'alice');
      await assertAudioFlows(late, 'alice');
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
      await Promise.all(
        clients.flatMap((c) => clients.filter((o) => o !== c).map((o) => assertVideoFlows(c, o.identity))),
      );
    } finally {
      await Promise.all(clients.map((c) => c.close()));
    }
  });

  it('screen sharing reaches other participants as its own track', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await a.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(true));
      await waitForSubscribed(b, 'alice', ['screen_share']);
      await assertVideoFlows(b, 'alice', 'screen_share', { minFrames: 2 });
      await a.page.evaluate(() => window.harness.room.localParticipant.setScreenShareEnabled(false));
      await until(b, () => !window.harness.subscribedSources('alice').includes('screen_share'), undefined, 10000, 'screen share to stop');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('simulcast', () => {
  it('a subscriber can switch layers, and video keeps playing across each switch', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');

      const setQuality = (q) =>
        b.page.evaluate((q) => {
          const h = window.harness;
          h.publication('alice', 'camera').setVideoQuality(h.VideoQuality[q]);
        }, q);
      const waitForWidth = (test, what) =>
        until(
          b,
          async ([sid, max, min]) => {
            const f = window.harness.frameHash(sid);
            return f && f.width <= max && f.width >= min;
          },
          what,
          20000,
          `video width within ${what[2]}-${what[1]}`,
        );
      const sid = await b.page.evaluate(() => window.harness.publication('alice', 'camera').trackSid);

      await setQuality('LOW');
      await waitForWidth(null, [sid, 400, 1]);
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });

      await setQuality('HIGH');
      await waitForWidth(null, [sid, 5000, 500]);
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });

      await setQuality('MEDIUM');
      await waitForWidth(null, [sid, 800, 400]);
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('track lifecycle', () => {
  it('mute and unmute reach other participants', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(false));
      await until(b, () => window.harness.publication('alice', 'microphone')?.isMuted === true, undefined, 10000, 'remote mute');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackMuted'))) >= 1);
      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await until(b, () => window.harness.publication('alice', 'microphone')?.isMuted === false, undefined, 10000, 'remote unmute');
      await assertAudioFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('unpublishing removes the track for others, and publishing again restores it', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      const firstSid = await b.page.evaluate(() => window.harness.publication('alice', 'camera').trackSid);
      await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        const pub = lp.getTrackPublication('camera');
        await lp.unpublishTrack(pub.track);
      });
      await until(b, () => !window.harness.publication('alice', 'camera'), undefined, 15000, 'camera publication to go away');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('trackUnpublished'))) >= 1);

      await a.page.evaluate(() => window.harness.room.localParticipant.setCameraEnabled(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      const secondSid = await b.page.evaluate(() => window.harness.publication('alice', 'camera').trackSid);
      assert.notEqual(firstSid, secondSid, 'a republished track gets a new sid');
      await assertVideoFlows(b, 'alice');
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

      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setSubscribed(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');

      await b.page.evaluate(() => window.harness.publication('alice', 'camera').setSubscribed(false));
      await until(b, () => window.harness.subscribedSources('alice').length === 0, undefined, 10000, 'unsubscribe');
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
      await until(
        b,
        () => window.harness.publication('alice', 'camera')?.permissionStatus === 'not_allowed',
        undefined,
        10000,
        'publication to report not allowed',
      );
      await a.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(true));
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('data', () => {
  it('delivers reliable and lossy messages, with the sender stamped by the server', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    const c = await connect('carol', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().length === 2, undefined, 10000, 'peers');
      await sleep(500);
      await a.page.evaluate(async () => {
        const lp = window.harness.room.localParticipant;
        const send = (text, opts) => lp.publishData(new TextEncoder().encode(text), opts);
        await send('reliable-all', { reliable: true, topic: 'chat' });
        await send('lossy-all', { reliable: false });
        await send('only-bob', { reliable: true, destinationIdentities: ['bob'] });
      });
      await until(b, () => window.harness.received.length >= 3, undefined, 10000, 'bob to get messages');
      await until(c, () => window.harness.received.length >= 2, undefined, 10000, 'carol to get messages');
      await sleep(500);

      const atBob = await b.page.evaluate(() => window.harness.received);
      const atCarol = await c.page.evaluate(() => window.harness.received);
      const reliable = atBob.find((m) => m.text === 'reliable-all');
      assert.equal(reliable.from, 'alice');
      assert.equal(reliable.topic, 'chat');
      assert.ok(atBob.some((m) => m.text === 'lossy-all'));
      assert.ok(atBob.some((m) => m.text === 'only-bob'));
      assert.ok(!atCarol.some((m) => m.text === 'only-bob'), 'carol received a message addressed only to bob');
      assert.equal((await a.page.evaluate(() => window.harness.received)).length, 0, 'sender received its own message');
    } finally {
      await a.close();
      await b.close();
      await c.close();
    }
  });
});

describe('session lifecycle', () => {
  it('leaving is seen by others', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      await a.page.evaluate(() => window.harness.room.disconnect());
      await until(b, () => window.harness.remoteIdentities().length === 0, undefined, 10000, 'alice to leave');
      assert.ok((await b.page.evaluate(() => window.harness.eventCount('participantDisconnected'))) >= 1);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('a lost signalling connection resumes without interrupting media', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room);
    try {
      await waitForSubscribed(b, 'alice', ['camera', 'microphone']);
      await waitForSubscribed(a, 'bob', ['camera', 'microphone']);
      await a.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
      await until(a, () => window.harness.eventCount('signalReconnecting') + window.harness.eventCount('reconnecting') >= 1, undefined, 10000, 'reconnecting event');
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'reconnected event');
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'connected');
      assert.deepEqual(await b.page.evaluate(() => window.harness.remoteIdentities()), ['alice']);
      await assertVideoFlows(b, 'alice');
      await assertVideoFlows(a, 'bob');
      await assertAudioFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('after a resume, tracks published later still flow both ways', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await a.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
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
      const quality = await b.page.evaluate(() => {
        const r = window.harness.room;
        return r.remoteParticipants.get('alice').connectionQuality;
      });
      assert.ok(['excellent', 'good'].includes(quality), `quality on loopback is ${quality}`);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('authentication', () => {
  const validate = (token) => fetch(`${http}/rtc/validate?access_token=${token}`);
  const token = (overrides = {}) =>
    mintToken({ apiKey, apiSecret, identity: 'x', room: 'auth-room', ...overrides });

  it('accepts a valid token', async () => {
    const r = await validate(token());
    assert.equal(r.status, 200);
    assert.equal(await r.text(), 'success');
  });

  it('rejects a bad signature', async () => {
    assert.equal((await validate(token({ apiSecret: 'not-the-secret' }))).status, 401);
  });

  it('rejects an unknown API key', async () => {
    assert.equal((await validate(token({ apiKey: 'nobody' }))).status, 401);
  });

  it('rejects garbage', async () => {
    assert.equal((await validate('garbage')).status, 401);
  });

  it('rejects a token without the room join grant', async () => {
    assert.equal((await validate(token({ grants: { roomJoin: false } }))).status, 401);
  });

  it('tells a client with a bad token why, rather than failing silently', async () => {
    const room = nextRoom();
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    try {
      await page.goto(`http://127.0.0.1:${fixture.port}/`);
      const message = await page.evaluate(
        async ([url, bad]) => {
          const r = new window.harness.room.constructor();
          try {
            await r.connect(url, bad);
            return 'connected';
          } catch (e) {
            return String(e.message ?? e);
          }
        },
        [ws, token({ apiSecret: 'not-the-secret', room })],
      );
      assert.notEqual(message, 'connected');
      assert.match(message, /invalid|unauthorized|token/i);
    } finally {
      await ctx.close();
    }
  });
});

describe('resume races', () => {
  // Regression: when a resume met an unanswered subscriber offer, the ICE restart and queued tracks were lost, and
  // the subscriber stayed connected but silent. It happened about one run in eight, so this repeats.
  it('two clients resuming at once right after subscribing both keep receiving', async () => {
    for (let run = 0; run < 3; run++) {
      const room = nextRoom();
      const a = await connect('alice', room);
      const b = await connect('bob', room);
      try {
        await waitForSubscribed(b, 'alice', ['camera']);
        await Promise.all([a, b].map((c) => c.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'))));
        await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'alice to reconnect');
        await until(b, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'bob to reconnect');
        await waitForSubscribed(a, 'bob', ['camera']);
        await waitForSubscribed(b, 'alice', ['camera']);
        await assertVideoFlows(a, 'bob');
        await assertVideoFlows(b, 'alice');
      } finally {
        await a.close();
        await b.close();
      }
    }
  });

  // A client can lose signalling at any moment, including while tracks are being negotiated.
  it('resuming while tracks are still being negotiated loses nothing', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      // Publish on both sides and drop signalling without waiting for negotiation to finish.
      await b.page.evaluate(() => {
        window.publishing = window.harness.room.localParticipant.setCameraEnabled(true);
      });
      await a.page.evaluate(() => {
        window.publishing = window.harness.room.localParticipant.setCameraEnabled(true);
        return window.harness.room.simulateScenario('signal-reconnect');
      });
      await b.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'alice to reconnect');
      await until(b, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'bob to reconnect');
      // The client cancels a publish that a resume interrupts, on the reference server too. Publishing again is
      // what an application would do, and the server must not be left confused by the abandoned attempt.
      for (const c of [a, b]) {
        await c.page.evaluate(async () => {
          await window.publishing.catch(() => {});
          await window.harness.room.localParticipant.setCameraEnabled(true);
        });
      }
      await waitForSubscribed(a, 'bob', ['camera']);
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(a, 'bob');
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('token grants', () => {
  // Connects with a token carrying these grants, instead of the harness default.
  async function connectWithGrants(identity, room, grants) {
    const ctx = await browser.newContext({ permissions: ['camera', 'microphone'] });
    const page = await ctx.newPage();
    await page.goto(`http://127.0.0.1:${fixture.port}/?${new URLSearchParams({ ws, identity, room, mode, publish: '0', noconnect: '1' })}`);
    const token = mintToken({ apiKey, apiSecret, identity, room, grants });
    await page.evaluate(([url, t]) => window.harness.room.connect(url, t), [ws, token]);
    return { page, ctx, identity, close: () => ctx.close() };
  }

  it('canPublish false stops publishing, and canSubscribe false stops receiving', async () => {
    const room = nextRoom();
    const publisher = await connect('alice', room);
    const noPublish = await connectWithGrants('viewer', room, { canPublish: false });
    const noSubscribe = await connectWithGrants('sender', room, { canSubscribe: false });
    try {
      const failure = await noPublish.page.evaluate(() =>
        window.harness.room.localParticipant.setCameraEnabled(true).then(() => 'published', (e) => String(e.message ?? e)),
      );
      assert.notEqual(failure, 'published');
      assert.equal(await noPublish.page.evaluate(() => window.harness.room.localParticipant.isCameraEnabled), false);

      await waitForSubscribed(noPublish, 'alice', ['camera']);
      await assertVideoFlows(noPublish, 'alice');

      await until(noSubscribe, () => !!window.harness.publication('alice', 'camera'), undefined, 15000, 'publication to appear');
      await sleep(1500);
      assert.deepEqual(await noSubscribe.page.evaluate(() => window.harness.subscribedSources('alice')), []);
    } finally {
      await publisher.close();
      await noPublish.close();
      await noSubscribe.close();
    }
  });

  it('canPublishSources limits which sources can be published', async () => {
    const room = nextRoom();
    const c = await connectWithGrants('alice', room, { canPublishSources: ['microphone'] });
    try {
      await c.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      assert.equal(await c.page.evaluate(() => window.harness.room.localParticipant.isMicrophoneEnabled), true);
      const failure = await c.page.evaluate(() =>
        window.harness.room.localParticipant.setCameraEnabled(true).then(() => 'published', (e) => String(e.message ?? e)),
      );
      assert.notEqual(failure, 'published');
    } finally {
      await c.close();
    }
  });

  it('a client updates its own metadata only with the grant', async () => {
    const room = nextRoom();
    const allowed = await connectWithGrants('alice', room, { canUpdateOwnMetadata: true });
    const denied = await connectWithGrants('bob', room, {});
    const watcher = await connect('watcher', room, { publish: false });
    try {
      await until(watcher, () => window.harness.remoteIdentities().length === 2, undefined, 15000, 'participants');
      await allowed.page.evaluate(() => window.harness.room.localParticipant.setMetadata('hello'));
      await allowed.page.evaluate(() => window.harness.room.localParticipant.setAttributes({ team: 'blue' }));
      await until(
        watcher,
        () => {
          const p = window.harness.room.remoteParticipants.get('alice');
          return p.metadata === 'hello' && p.attributes.team === 'blue';
        },
        undefined,
        10000,
        'metadata to arrive',
      );
      const refused = await denied.page.evaluate(() =>
        window.harness.room.localParticipant.setMetadata('nope').then(() => 'updated', (e) => String(e.message ?? e)),
      );
      assert.notEqual(refused, 'updated');
      await sleep(500);
      assert.notEqual(await watcher.page.evaluate(() => window.harness.room.remoteParticipants.get('bob').metadata), 'nope');
    } finally {
      await allowed.close();
      await denied.close();
      await watcher.close();
    }
  });
});

describe('adaptive stream and dynacast', () => {
  it('an element shown small receives a low layer, and the publisher stops what nobody watches', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { dynacast: true });
    const ctx = await browser.newContext({ permissions: ['camera', 'microphone'], viewport: { width: 1280, height: 720 } });
    const page = await ctx.newPage();
    await page.goto(`http://127.0.0.1:${fixture.port}/?${new URLSearchParams({ ws, identity: 'bob', room, mode, publish: '0', adaptive: '1' })}`);
    await page.waitForSelector('#state[data-state="connected"]', { state: 'attached', timeout: 30000 });
    const b = { page, ctx, identity: 'bob', close: () => ctx.close() };
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      const sid = await page.evaluate(() => window.harness.publication('alice', 'camera').trackSid);
      await page.evaluate((id) => {
        const v = document.querySelector(`video[data-sid="${id}"]`);
        v.style.cssText = 'width:160px;height:90px;position:fixed;top:0;left:0';
      }, sid);
      await until(b, (id) => (window.harness.frameHash(id)?.width ?? 9999) <= 400, sid, 25000, 'a low layer for a small element');
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });

      // With the only viewer on the low layer, the publisher should stop sending the high one.
      await until(
        a,
        async () => {
          const pub = window.harness.room.localParticipant.getTrackPublication('camera');
          const report = await pub.track.getSenderStats();
          const high = report.find((s) => s.rid === 'f');
          return !!high && (high.active === false || (window.lastBytes !== undefined && high.bytesSent === window.lastBytes) || ((window.lastBytes = high.bytesSent), false));
        },
        undefined,
        25000,
        'the publisher to stop the high layer',
      );
      await page.evaluate((id) => {
        const v = document.querySelector(`video[data-sid="${id}"]`);
        v.style.cssText = 'width:1280px;height:720px;position:fixed;top:0;left:0';
      }, sid);
      await until(b, (id) => (window.harness.frameHash(id)?.width ?? 0) >= 500, sid, 25000, 'a higher layer for a large element');
      await assertVideoFlows(b, 'alice', 'camera', { minFrames: 10 });
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('recovery', () => {
  it('a resume after the session is gone leads to a full rejoin', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      // The client asks to resume a session the server has never heard of.
      await a.page.evaluate(() => {
        window.harness.room.localParticipant.sid = 'PA_madeup';
        window.harness.room.engine.participantSid = 'PA_madeup';
        return window.harness.room.simulateScenario('signal-reconnect');
      });
      await until(a, () => window.harness.eventCount('reconnected') >= 1 && window.harness.connectionState() === 'connected', undefined, 30000, 'alice to recover');
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('reliable data sent during a resume is delivered exactly once', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(a, () => window.harness.remoteIdentities().includes('bob'), undefined, 10000, 'bob');
      await sleep(800);
      const count = 30;
      await a.page.evaluate(async (n) => {
        const lp = window.harness.room.localParticipant;
        const send = (i) => lp.publishData(new TextEncoder().encode(`m${i}`), { reliable: true });
        for (let i = 0; i < n / 2; i++) await send(i);
        const resuming = window.harness.room.simulateScenario('signal-reconnect');
        for (let i = n / 2; i < n; i++) await send(i);
        await resuming;
      }, count);
      await until(a, () => window.harness.eventCount('reconnected') >= 1, undefined, 25000, 'alice to reconnect');
      await until(b, (n) => window.harness.received.length >= n, count, 20000, 'every message');
      await sleep(1000);
      const texts = await b.page.evaluate(() => window.harness.received.map((m) => m.text));
      assert.equal(new Set(texts).size, count, `missing: ${texts.length} received`);
      assert.equal(texts.length, count, 'some messages arrived twice');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

describe('room isolation', () => {
  it('neither participants, media nor data cross rooms', async () => {
    const roomA = nextRoom();
    const roomB = nextRoom();
    const a = await connect('alice', roomA);
    const b = await connect('bob', roomB);
    // The same identity in another room is a different participant.
    const a2 = await connect('alice', roomB, { publish: false });
    try {
      await waitForSubscribed(a2, 'bob', ['camera']);
      await a.page.evaluate(() =>
        window.harness.room.localParticipant.publishData(new TextEncoder().encode('secret'), { reliable: true }),
      );
      // The reference batches announcements of participants who only subscribe, so this can take seconds.
      await until(b, () => window.harness.remoteIdentities().includes('alice'), undefined, 15000, 'alice in room B');
      assert.deepEqual(await a.page.evaluate(() => window.harness.remoteIdentities()), []);
      assert.equal((await b.page.evaluate(() => window.harness.received)).length, 0);
      assert.equal((await a2.page.evaluate(() => window.harness.received)).length, 0);
      assert.equal(await a.page.evaluate(() => window.harness.connectionState()), 'connected');
    } finally {
      await a.close();
      await b.close();
      await a2.close();
    }
  });
});

describe('room API', () => {
  const adminToken = (room, grants = {}) =>
    mintToken({
      apiKey,
      apiSecret,
      identity: 'api',
      room,
      grants: { roomJoin: false, roomAdmin: true, roomCreate: true, roomList: true, ...grants },
    });
  const api = async (method, room, body) => {
    const r = await fetch(`${http}/twirp/livekit.RoomService/${method}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: `Bearer ${adminToken(room)}` },
      body: JSON.stringify(body),
    });
    const text = await r.text();
    let json;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: r.status, text, json };
  };

  it('creates, lists and deletes rooms, and deleting ends the session', async () => {
    const room = nextRoom();
    const created = await api('CreateRoom', room, { name: room, metadata: 'hello', empty_timeout: 120 });
    assert.equal(created.status, 200, created.text);
    assert.equal(created.json.name, room);
    assert.equal(created.json.metadata, 'hello');
    assert.equal(created.json.empty_timeout, 120);
    assert.match(created.json.sid, /^RM_/);

    const a = await connect('alice', room, { publish: false });
    try {
      const listed = await api('ListRooms', room, { names: [room] });
      assert.equal(listed.json.rooms.length, 1);
      assert.equal(listed.json.rooms[0].sid, created.json.sid);

      assert.equal((await api('DeleteRoom', room, { room })).status, 200);
      await until(a, () => window.harness.connectionState() === 'disconnected', undefined, 10000, 'session to end');
      assert.equal(await a.page.evaluate(() => window.harness.disconnectReason()), 5 /* ROOM_DELETED */);
      assert.equal((await api('ListRooms', room, { names: [room] })).json.rooms.length, 0);
    } finally {
      await a.close();
    }
  });

  it('reports a missing room in the documented shape', async () => {
    const r = await api('DeleteRoom', 'no-such-room', { room: 'no-such-room' });
    assert.equal(r.status, 404);
    assert.deepEqual(r.json, { code: 'not_found', msg: 'requested room does not exist' });
  });

  it('lists participants with their tracks, and gets one', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    try {
      await sleep(1500);
      const listed = await api('ListParticipants', room, { room });
      assert.equal(listed.status, 200, listed.text);
      assert.equal(listed.json.participants.length, 1);
      const p = listed.json.participants[0];
      assert.equal(p.identity, 'alice');
      assert.equal(p.state, 'ACTIVE');
      assert.equal(p.tracks.length, 2);

      const one = await api('GetParticipant', room, { room, identity: 'alice' });
      assert.equal(one.json.sid, p.sid);
      const missing = await api('GetParticipant', room, { room, identity: 'nobody' });
      assert.equal(missing.status, 404);
      assert.equal(missing.json.msg, 'participant does not exist');
    } finally {
      await a.close();
    }
  });

  it('removes a participant, and others see them go', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(b, () => window.harness.remoteIdentities().includes('alice'), undefined, 10000, 'alice');
      assert.equal((await api('RemoveParticipant', room, { room, identity: 'alice' })).status, 200);
      await until(a, () => window.harness.connectionState() === 'disconnected', undefined, 10000, 'alice to be removed');
      assert.equal(await a.page.evaluate(() => window.harness.disconnectReason()), 4 /* PARTICIPANT_REMOVED */);
      await until(b, () => window.harness.remoteIdentities().length === 0, undefined, 10000, 'alice to leave');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('mutes a published track from the server', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      const sid = await b.page.evaluate(() => window.harness.publication('alice', 'microphone').trackSid);
      const r = await api('MutePublishedTrack', room, { room, identity: 'alice', track_sid: sid, muted: true });
      assert.equal(r.status, 200, r.text);
      assert.equal(r.json.track.muted, true);
      await until(b, () => window.harness.publication('alice', 'microphone').isMuted, undefined, 10000, 'remote mute');
      await until(
        a,
        () => window.harness.room.localParticipant.getTrackPublication('microphone').isMuted,
        undefined,
        10000,
        'local track to be muted',
      );
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('stops forwarding a muted publisher, even one that ignores being told and keeps sending', async () => {
    const room = nextRoom();
    // This client's microphone cannot be turned off from the page, so packets keep leaving it whatever it is told.
    const stubborn = () => {
      const enabled = Object.getOwnPropertyDescriptor(MediaStreamTrack.prototype, 'enabled');
      Object.defineProperty(MediaStreamTrack.prototype, 'enabled', { get: enabled.get, set() {} });
    };
    const a = await connect('alice', room, { init: stubborn });
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['microphone']);
      await assertAudioFlows(b, 'alice');
      const sid = await b.page.evaluate(() => window.harness.publication('alice', 'microphone').trackSid);
      const r = await api('MutePublishedTrack', room, { room, identity: 'alice', track_sid: sid, muted: true });
      assert.equal(r.status, 200, r.text);
      await until(b, () => window.harness.publication('alice', 'microphone').isMuted, undefined, 10000, 'remote mute');
      await sleep(1000);
      const first = forSource(await stats(b), 'alice', 'microphone');
      await sleep(3000);
      const second = forSource(await stats(b), 'alice', 'microphone');
      assert.ok(second.packetsReceived - first.packetsReceived < 5, `${second.packetsReceived - first.packetsReceived} packets arrived while muted`);

      await a.page.evaluate(() => window.harness.room.localParticipant.setMicrophoneEnabled(true));
      await until(b, () => !window.harness.publication('alice', 'microphone').isMuted, undefined, 10000, 'remote unmute');
      await assertAudioFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('updates participant metadata and permissions', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await until(b, () => window.harness.remoteIdentities().includes('alice'), undefined, 10000, 'alice');
      const r = await api('UpdateParticipant', room, {
        room,
        identity: 'alice',
        metadata: 'role=host',
        name: 'Alice A.',
        attributes: { team: 'blue' },
      });
      assert.equal(r.status, 200, r.text);
      assert.equal(r.json.metadata, 'role=host');
      await until(
        b,
        () => {
          const p = window.harness.room.remoteParticipants.get('alice');
          return p.metadata === 'role=host' && p.name === 'Alice A.' && p.attributes.team === 'blue';
        },
        undefined,
        10000,
        'metadata to reach bob',
      );
      await until(a, () => window.harness.room.localParticipant.metadata === 'role=host', undefined, 10000, 'metadata to reach alice');

      const revoked = await api('UpdateParticipant', room, {
        room,
        identity: 'alice',
        permission: { can_subscribe: true, can_publish: false, can_publish_data: true },
      });
      assert.equal(revoked.json.permission.can_publish, false);
      await until(a, () => window.harness.room.localParticipant.permissions?.canPublish === false, undefined, 10000, 'permission change');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('sends data from the server', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false });
    try {
      await sleep(1000);
      const r = await api('SendData', room, {
        room,
        data: Buffer.from('from-server').toString('base64'),
        kind: 'RELIABLE',
        topic: 'announcements',
        destination_identities: ['bob'],
      });
      assert.equal(r.status, 200, r.text);
      await until(b, () => window.harness.received.length >= 1, undefined, 10000, 'server message');
      const got = await b.page.evaluate(() => window.harness.received[0]);
      assert.equal(got.text, 'from-server');
      assert.equal(got.topic, 'announcements');
      await sleep(300);
      assert.equal((await a.page.evaluate(() => window.harness.received)).length, 0);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('updates room metadata for everyone in it', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, { publish: false });
    try {
      const r = await api('UpdateRoomMetadata', room, { room, metadata: 'topic=standup' });
      assert.equal(r.status, 200, r.text);
      assert.equal(r.json.metadata, 'topic=standup');
      await until(a, () => window.harness.room.metadata === 'topic=standup', undefined, 10000, 'room metadata');
    } finally {
      await a.close();
    }
  });

  it('changes a participant subscriptions from the server', async () => {
    const room = nextRoom();
    const a = await connect('alice', room);
    const b = await connect('bob', room, { publish: false });
    try {
      await waitForSubscribed(b, 'alice', ['camera']);
      const sid = await b.page.evaluate(() => window.harness.publication('alice', 'camera').trackSid);
      const r = await api('UpdateSubscriptions', room, { room, identity: 'bob', track_sids: [sid], subscribe: false });
      assert.equal(r.status, 200, r.text);
      await until(b, () => !window.harness.subscribedSources('alice').includes('camera'), undefined, 10000, 'unsubscribe');
      await api('UpdateSubscriptions', room, { room, identity: 'bob', track_sids: [sid], subscribe: true });
      await waitForSubscribed(b, 'alice', ['camera']);
      await assertVideoFlows(b, 'alice');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('enforces max participants', async () => {
    const room = nextRoom();
    await api('CreateRoom', room, { name: room, max_participants: 1 });
    const a = await connect('alice', room, { publish: false });
    const b = await connect('bob', room, { publish: false, waitConnected: false });
    try {
      await b.page.waitForSelector('#state[data-state="error"],#state[data-state="connected"]', { state: 'attached', timeout: 30000 });
      assert.equal(await b.page.$eval('#state', (e) => e.dataset.state), 'error');
      assert.deepEqual(await a.page.evaluate(() => window.harness.remoteIdentities()), []);
    } finally {
      await a.close();
      await b.close();
    }
  });
});

// Last, so that it covers every client the tests above opened.
describe('signal path', () => {
  it('every client connected on the path its mode asks for', () => {
    console.log(`signal connections in ${mode} mode: ${JSON.stringify(signalPaths)}`);
    assert.deepEqual(Object.keys(signalPaths), [mode === 'dual' ? '/rtc' : '/rtc/v1']);
  });
});

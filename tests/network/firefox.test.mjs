// Our SDK in Firefox, against both servers: Playwright's Firefox, real WebRTC and real decoders. Devices are Firefox's fake
// ones, and it never calls getDisplayMedia, so screen sharing from Firefox is not covered here. The servers run in a
// container on a Docker bridge, one machine. See lib.mjs.
//
// Safari is not here. It needs a Mac.
import assert from 'node:assert/strict';
import { after, describe, it } from 'node:test';
import { launchChrome, launchFirefox, measureFrames, nextRoom, onBothServers, openClient, publishCamera, sleep, stopFixture, viewerSummary, waitForSteadyFramesAfter, waitForWidth } from './lib.mjs';

after(stopFixture);

const STEADY_MS = 20000;
const PLAYING_FPS = 10;

/** Opens a Firefox client in the room, in its own browser. */
async function openFirefox(identity, room, server, query = {}) {
  return openClient(await launchFirefox(), { identity, room, ws: server.ws, query: { adaptive: '0', ...query } });
}

const hasMoved = (client, sid) =>
  client.page.evaluate(async (sid) => {
    const first = window.harness.frameHash(sid);
    await new Promise((r) => setTimeout(r, 1500));
    const second = window.harness.frameHash(sid);
    return !!first && !!second && first.hash !== second.hash;
  }, sid);

/** What a client receives of one remote track, from its stats. */
const receivedOf = async (client, identity, source) => {
  const rows = await client.page.evaluate(() => window.harness.subscribedStats());
  return rows.find((r) => r.identity === identity && r.source === source);
};

async function audioArrives(client, identity) {
  const before = await receivedOf(client, identity, 'microphone');
  await sleep(2000);
  const after = await receivedOf(client, identity, 'microphone');
  return before && after ? after.packetsReceived - before.packetsReceived : 0;
}

const waitForSubscribed = (client, identity, sources, timeout = 25000) =>
  client.page.waitForFunction(([who, want]) => want.every((s) => window.harness.subscribedSources(who).includes(s)), [identity, sources], { timeout, polling: 200 });

const sidOf = (client, identity, source) => client.page.evaluate(([who, s]) => window.harness.publication(who, s)?.trackSid, [identity, source]);

/** The errors a page reported, for a judge to read. */
const pageErrors = (...clients) => clients.flatMap((c) => c.errors);

describe('Firefox', () => {
  onBothServers(describe, it, 'Firefox publishes a camera and a microphone, Chrome receives', {
    link: 'no impairment',
    run: async (server) => {
      const room = nextRoom('fx');
      const viewer = await openClient(await launchChrome(), { identity: 'chrome', room, ws: server.ws, query: { adaptive: '0' } });
      const publisher = await openFirefox('firefox', room, server, { publish: '1' });
      try {
        await measureFrames(viewer);
        await waitForSubscribed(viewer, 'firefox', ['camera', 'microphone']);
        const t0 = Date.now();
        const steadyMs = await waitForSteadyFramesAfter(viewer, t0, 30000, { minFps: PLAYING_FPS });
        const sid = await sidOf(viewer, 'firefox', 'camera');
        const moved = await hasMoved(viewer, sid);
        const audioPackets = await audioArrives(viewer, 'firefox');

        // Whether Firefox's layers can really be told apart. It sends all of them all the time, and the viewer picks.
        const layers = await publisher.page.evaluate(() => window.harness.outboundStats('camera'));
        const wanted = {};
        for (const [name, quality] of [['low', 'LOW'], ['high', 'HIGH']]) {
          await viewer.page.evaluate(([sid, q]) => {
            const pub = [...window.harness.room.remoteParticipants.values()].flatMap((p) => [...p.trackPublications.values()]).find((x) => x.trackSid === sid);
            pub.setVideoQuality(window.harness.VideoQuality[q]);
          }, [sid, quality]);
          await sleep(6000);
          const row = await receivedOf(viewer, 'firefox', 'camera');
          wanted[name] = row?.width ?? null;
        }
        const summary = await viewerSummary(viewer, Date.now() - 5000);
        return { steadyMs, moved, audioPackets, layers, wanted, fps: summary.fps, errors: pageErrors(viewer, publisher) };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.steadyMs !== null && m.steadyMs <= STEADY_MS, `steady video within ${STEADY_MS} ms (got ${m.steadyMs})`);
      c.check(m.moved, 'the picture really changes');
      c.check(m.audioPackets > 50, `audio arrives (got ${m.audioPackets} packets in 2 s)`);
      c.check(m.fps >= PLAYING_FPS, `at least ${PLAYING_FPS} fps (got ${m.fps})`);
      c.check(m.layers.length >= 2, `Firefox sends layers (${JSON.stringify(m.layers)})`);
      c.check(m.wanted.low !== null && m.wanted.high !== null && m.wanted.low < m.wanted.high, `the viewer can choose a layer: low ${m.wanted.low}, high ${m.wanted.high}`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });

  onBothServers(describe, it, 'Chrome publishes, Firefox receives', {
    link: 'no impairment',
    run: async (server) => {
      const room = nextRoom('fx');
      const viewer = await openFirefox('firefox', room, server);
      const publisher = await openClient(await launchChrome(), { identity: 'chrome', room, ws: server.ws });
      try {
        await measureFrames(viewer);
        await publishCamera(publisher);
        await waitForSubscribed(viewer, 'chrome', ['camera', 'microphone']);
        const t0 = Date.now();
        const steadyMs = await waitForSteadyFramesAfter(viewer, t0, 30000, { minFps: PLAYING_FPS });
        const reachedTopMs = await waitForWidth(viewer, t0, 1280, 30000);
        const sid = await sidOf(viewer, 'chrome', 'camera');
        const moved = await hasMoved(viewer, sid);
        const audioPackets = await audioArrives(viewer, 'chrome');
        const summary = await viewerSummary(viewer, Date.now() - 5000);
        return { steadyMs, reachedTopMs, moved, audioPackets, fps: summary.fps, p50: summary.p50, errors: pageErrors(viewer, publisher) };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.steadyMs !== null && m.steadyMs <= STEADY_MS, `steady video within ${STEADY_MS} ms (got ${m.steadyMs})`);
      c.check(m.reachedTopMs !== null, `the top layer is reached (got ${m.reachedTopMs})`);
      c.check(m.moved, 'the picture really changes');
      c.check(m.audioPackets > 50, `audio arrives (got ${m.audioPackets} packets in 2 s)`);
      c.check(m.fps >= PLAYING_FPS, `at least ${PLAYING_FPS} fps (got ${m.fps})`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });

  onBothServers(describe, it, 'two Firefox browsers in a call, with data both ways', {
    link: 'no impairment',
    run: async (server) => {
      const room = nextRoom('fx');
      const a = await openFirefox('a', room, server, { publish: '1' });
      const b = await openFirefox('b', room, server, { publish: '1' });
      try {
        await measureFrames(a);
        await measureFrames(b);
        await waitForSubscribed(a, 'b', ['camera', 'microphone']);
        await waitForSubscribed(b, 'a', ['camera', 'microphone']);
        const t0 = Date.now();
        const aSteady = await waitForSteadyFramesAfter(a, t0, 30000, { minFps: PLAYING_FPS });
        const bSteady = await waitForSteadyFramesAfter(b, t0, 30000, { minFps: PLAYING_FPS });
        const sent = async (from, to, text) => {
          await from.page.evaluate((t) => window.harness.room.localParticipant.publishData(new TextEncoder().encode(t), { reliable: true }), text);
          await to.page.waitForFunction((t) => window.harness.received.some((m) => m.text === t), text, { timeout: 10000, polling: 100 });
          return true;
        };
        return { aSteady, bSteady, aToB: await sent(a, b, 'from a'), bToA: await sent(b, a, 'from b'), errors: pageErrors(a, b) };
      } finally {
        await a.close();
        await b.close();
      }
    },
    judge: (m, c) => {
      c.check(m.aSteady !== null && m.bSteady !== null, `steady video both ways (${m.aSteady}, ${m.bSteady})`);
      c.check(m.aToB && m.bToA, 'data arrives both ways');
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });

  onBothServers(describe, it, 'Firefox recovers: signalling dropped, then a full reconnect', {
    link: 'no impairment',
    timeout: 300000,
    run: async (server) => {
      const room = nextRoom('fx');
      const viewer = await openClient(await launchChrome(), { identity: 'chrome', room, ws: server.ws, query: { adaptive: '0' } });
      const publisher = await openFirefox('firefox', room, server, { publish: '1' });
      try {
        await measureFrames(viewer);
        await waitForSubscribed(viewer, 'firefox', ['camera', 'microphone']);
        assert.notEqual(await waitForSteadyFramesAfter(viewer, Date.now(), 30000, { minFps: PLAYING_FPS }), null, 'video did not start');

        const sid = () => publisher.page.evaluate(() => window.harness.room.localParticipant.sid);
        const firstSid = await sid();
        await publisher.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
        await publisher.page.waitForFunction(() => window.__events.some((e) => e.name === 'reconnected'), undefined, { timeout: 25000, polling: 200 });
        const afterSignal = await waitForSteadyFramesAfter(viewer, Date.now(), 15000, { minFps: PLAYING_FPS });
        const sameSession = (await sid()) === firstSid;

        const before = await publisher.page.evaluate(() => window.__events.filter((e) => e.name === 'reconnected').length);
        const at = Date.now();
        await publisher.page.evaluate(() => window.harness.room.simulateScenario('full-reconnect'));
        await publisher.page.waitForFunction((n) => window.__events.filter((e) => e.name === 'reconnected').length > n, before, { timeout: 45000, polling: 200 });
        const afterFull = await waitForSteadyFramesAfter(viewer, at + 500, 45000, { minFps: PLAYING_FPS });
        const newSession = (await sid()) !== firstSid;
        return { afterSignal, sameSession, afterFull, newSession, errors: pageErrors(viewer, publisher) };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.sameSession, 'a dropped signal keeps the session');
      c.check(m.afterSignal !== null, `video keeps going after the signal drop (${m.afterSignal})`);
      c.check(m.newSession, 'a full reconnect makes a new session');
      c.check(m.afterFull !== null && m.afterFull <= STEADY_MS, `video back within ${STEADY_MS} ms of a full reconnect (got ${m.afterFull})`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });

  onBothServers(describe, it, 'Firefox receives over a link with 5% loss', {
    link: "Firefox's ports only, server to browser: loss 5%",
    run: async (server) => {
      const room = nextRoom('fx');
      const viewer = await openFirefox('firefox', room, server);
      const ports = [...(await viewer.mediaPorts()), ...(await server.signalPorts())];
      const publisher = await openClient(await launchChrome(), { identity: 'chrome', room, ws: server.ws });
      try {
        await measureFrames(viewer);
        await server.impair({ down: 'loss 5%', ports });
        await publishCamera(publisher);
        const t0 = Date.now();
        await sleep(25000);
        const summary = await viewerSummary(viewer, t0 + 8000);
        return { ...summary, netem: await server.netemCounters(), errors: pageErrors(viewer, publisher) };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.fps >= PLAYING_FPS, `at least ${PLAYING_FPS} fps (got ${m.fps})`);
      c.check(m.longestGapMs <= 2000, `longest gap at most 2000 ms (got ${m.longestGapMs})`);
      c.check((m.netem.down?.dropped ?? 0) > 0, 'netem really dropped packets');
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });
});

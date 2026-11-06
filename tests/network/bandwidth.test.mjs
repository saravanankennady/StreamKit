// A link too thin for the top layer: from the start of a call, and arriving part way through one. Emulated with tc netem
// on one machine. See lib.mjs.
//
// Thresholds, and why:
//   First frame  People wait about three seconds for a share to appear before they ask if it worked. On 500 kbps there
//                is less to work with, so five. The reference does it in about one, which is what to aim at.
//   Playing      A thin link may lower quality, but it should not stop the picture: at least 5 fps of something readable,
//                and no stall past 3 s once it has adapted.
//   Recovery     When the link is good again, quality should come back by itself within half a minute.
//
// The queue on each thin link is about what a home router holds at that rate: 60 packets at 1 Mbps, 40 at 500 kbps.
import { after, describe, it } from 'node:test';
import { launchChrome, launchSharingChrome, measureFrames, nextRoom, onBothServers, openCall, openClient, publishCamera, publishScreen, senderSummary, sleep, stopFixture, viewerSummary, waitForFrameAfter, waitForSteadyFramesAfter, waitForWidth } from './lib.mjs';

after(stopFixture);

const LINKS = [
  { name: '1 Mbps', spec: 'delay 20ms rate 1mbit limit 60', firstFrameMs: 3000, kbps: 1000 },
  { name: '500 kbps', spec: 'delay 20ms rate 500kbit limit 40', firstFrameMs: 5000, kbps: 500 },
];
const SOURCES = [
  { name: 'screen share', track: 'screen_share', publish: (c) => publishScreen(c), top: 1920 },
  { name: 'camera', track: 'camera', publish: (c) => publishCamera(c), top: 1280 },
];
const PLAYING_FPS = 5;
const STALL_MS = 3000;
const RECOVERY_MS = 30000;

/** The widths a viewer saw over time, for reading a result by eye. */
async function widthTimeline(client, t0) {
  const out = [];
  for (const f of await client.frames()) {
    if (out.at(-1)?.width !== f.width) out.push({ s: Math.round((f.at - t0) / 100) / 10, width: f.width });
  }
  return out.slice(0, 16).map((w) => `${w.s}s:${w.width}`).join(' ');
}

/** A thin link from before anything is published. */
async function thinFromStart(server, source, link) {
  const call = await openCall(server);
  try {
    await server.impair({ down: link.spec, ports: call.viewerPorts });
    const t0 = Date.now();
    await source.publish(call.publisher);
    const firstFrameMs = await waitForFrameAfter(call.viewer, t0, 40000);
    await sleep(Math.max(0, t0 + 35000 - Date.now()));
    const limited = await viewerSummary(call.viewer, t0 + 15000);
    const netem = await server.netemCounters();

    await server.clear();
    const lifted = Date.now();
    const recoveryMs = await waitForWidth(call.viewer, lifted, source.top, 60000);
    const after = await viewerSummary(call.viewer, Date.now() - 5000);
    return {
      firstFrameMs,
      limited,
      netem,
      recoveryMs,
      afterFps: after.fps,
      afterWidth: after.usualWidth,
      timeline: await widthTimeline(call.viewer, t0),
      sender: senderSummary(call.publisher, t0 + 15000, lifted),
      errors: [...call.viewer.errors, ...call.publisher.errors],
    };
  } finally {
    await call.close();
  }
}

/** A viewer on a thin link joining a share that is already at full size, which is the hardest start: every layer is there to pick. */
async function joinsRunningShare(server, source, link) {
  const room = nextRoom('late');
  const publisher = await openClient(await launchSharingChrome(), { identity: 'sharer', room, ws: server.ws, viewport: { width: 1920, height: 1080 } });
  let viewer;
  try {
    await source.publish(publisher);
    await sleep(6000);
    const others = new Set(await server.signalPorts());
    viewer = await openClient(await launchChrome(), { identity: 'viewer', room, ws: server.ws, query: { adaptive: '0', autosub: '0' } });
    await measureFrames(viewer);
    const signal = (await server.signalPorts()).filter((p) => !others.has(p));
    await server.impair({ down: link.spec, ports: [...(await viewer.mediaPorts()), ...signal] });
    const t0 = Date.now();
    await viewer.page.evaluate((s) => window.harness.publication('sharer', s).setSubscribed(true), source.track);
    const firstFrameMs = await waitForFrameAfter(viewer, t0, 40000);
    await sleep(Math.max(0, t0 + 25000 - Date.now()));
    const limited = await viewerSummary(viewer, t0 + 10000);
    return { firstFrameMs, limited, timeline: await widthTimeline(viewer, t0), errors: [...viewer.errors, ...publisher.errors] };
  } finally {
    await viewer?.close();
    await publisher.close();
  }
}

/** A good link that gets thin during a call, then good again. */
async function stepDown(server, source, link) {
  const call = await openCall(server);
  try {
    const t0 = Date.now();
    await source.publish(call.publisher);
    const atTopMs = await waitForWidth(call.viewer, t0, source.top, 30000);
    const stepAt = Date.now();
    const clean = await viewerSummary(call.viewer, stepAt - 5000, stepAt);
    await server.impair({ down: link.spec, ports: call.viewerPorts });
    const adaptedMs = await waitForSteadyFramesAfter(call.viewer, stepAt, 30000, { minFps: PLAYING_FPS });
    await sleep(Math.max(0, stepAt + 30000 - Date.now()));
    const end = Date.now();
    const limited = await viewerSummary(call.viewer, end - 15000, end);
    const whole = await viewerSummary(call.viewer, stepAt, end);

    await server.clear();
    const lifted = Date.now();
    const recoveryMs = await waitForWidth(call.viewer, lifted, source.top, 60000);
    return {
      atTopMs,
      clean: { fps: clean.fps, width: clean.usualWidth, kbps: clean.kbps },
      adaptedMs,
      limited,
      longestStallMs: whole.longestGapMs,
      recoveryMs,
      timeline: await widthTimeline(call.viewer, t0),
      errors: [...call.viewer.errors, ...call.publisher.errors],
    };
  } finally {
    await call.close();
  }
}

for (const source of SOURCES) {
  describe(`${source.name} on a thin link from the start`, () => {
    for (const link of LINKS) {
      onBothServers(describe, it, `${source.name}: ${link.name} from the start, then lifted`, {
        link: `viewer's ports only, server to browser: ${link.spec}`,
        timeout: 300000,
        run: (server) => thinFromStart(server, source, link),
        judge: (m, c) => {
          c.check(m.firstFrameMs !== null && m.firstFrameMs <= link.firstFrameMs, `first frame within ${link.firstFrameMs} ms (got ${m.firstFrameMs})`);
          c.check(m.limited.fps >= PLAYING_FPS, `plays at ${PLAYING_FPS} fps or more on the thin link (got ${m.limited.fps})`);
          c.check(m.limited.longestGapMs <= STALL_MS, `no stall past ${STALL_MS} ms once settled (got ${m.limited.longestGapMs})`);
          c.check(m.limited.usualWidth !== null && m.limited.usualWidth < source.top, `a lower layer is chosen (got ${JSON.stringify(m.limited.widths)})`);
          c.check(m.limited.kbps <= link.kbps * 1.1, `what arrives fits the link (got ${m.limited.kbps} kbps)`);
          c.check(m.recoveryMs !== null && m.recoveryMs <= RECOVERY_MS, `back at ${source.top} px within ${RECOVERY_MS} ms of the link recovering (got ${m.recoveryMs})`);
          c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
        },
      });
    }
  });

  describe(`joining a running ${source.name} on a thin link`, () => {
    for (const link of LINKS) {
      onBothServers(describe, it, `${source.name}: joining on ${link.name}`, {
        link: `the joining viewer's ports only, server to browser: ${link.spec}`,
        run: (server) => joinsRunningShare(server, source, link),
        judge: (m, c) => {
          c.check(m.firstFrameMs !== null && m.firstFrameMs <= link.firstFrameMs, `first frame within ${link.firstFrameMs} ms (got ${m.firstFrameMs})`);
          c.check(m.limited.fps >= PLAYING_FPS, `plays at ${PLAYING_FPS} fps or more on the thin link (got ${m.limited.fps})`);
          c.check(m.limited.longestGapMs <= STALL_MS, `no stall past ${STALL_MS} ms once settled (got ${m.limited.longestGapMs})`);
          c.check(m.limited.usualWidth !== null && m.limited.usualWidth < source.top, `a lower layer is chosen (got ${JSON.stringify(m.limited.widths)})`);
          c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
        },
      });
    }
  });

  describe(`${source.name} when the link gets thin during a call`, () => {
    for (const link of LINKS) {
      onBothServers(describe, it, `${source.name}: step down to ${link.name}, then back`, {
        link: `viewer's ports only, server to browser: clean, then ${link.spec}, then clean`,
        timeout: 300000,
        run: (server) => stepDown(server, source, link),
        judge: (m, c) => {
          c.check(m.atTopMs !== null, `reaches ${source.top} px on the clean link first (got ${m.atTopMs})`);
          c.check(m.adaptedMs !== null && m.adaptedMs <= 15000, `steady video again within 15 s of the step (got ${m.adaptedMs})`);
          c.check(m.limited.fps >= PLAYING_FPS, `plays at ${PLAYING_FPS} fps or more on the thin link (got ${m.limited.fps})`);
          c.check(m.limited.usualWidth !== null && m.limited.usualWidth < source.top, `a lower layer is chosen (got ${JSON.stringify(m.limited.widths)})`);
          c.check(m.limited.longestGapMs <= STALL_MS, `no stall past ${STALL_MS} ms once settled (got ${m.limited.longestGapMs})`);
          c.check(m.recoveryMs !== null && m.recoveryMs <= RECOVERY_MS, `back at ${source.top} px within ${RECOVERY_MS} ms of the link recovering (got ${m.recoveryMs})`);
          c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
        },
      });
    }
  });
}

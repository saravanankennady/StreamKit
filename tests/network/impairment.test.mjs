// A steady call on a link that stays bad: packet loss, high round trips and jitter, on our server and the reference.
// Emulated with tc netem on one machine. See lib.mjs.
//
// Thresholds, and why:
//   SMOOTH_FPS   A 30 fps share below 20 fps looks visibly uneven when something moves.
//   USABLE_FPS   Below 10 fps a person starts calling it broken. On heavy loss the question is only whether it still plays.
//   LIVE_MS      Beyond about 400 ms, pointing at something on a shared screen while talking stops working (ITU G.114 puts
//                the limit for conversation at 400 ms one way). The path's own delay is added on top of it.
//   Longest gap  A stall of 1 s is seen and shrugged off. Past 2 s people start asking "has it frozen?".
import { after, describe, it } from 'node:test';
import { onBothServers, openCall, publishScreen, selectedPath, senderSummary, sleep, stopFixture, viewerSummary, waitForFrameAfter } from './lib.mjs';

after(stopFixture);

const SMOOTH_FPS = 20;
const USABLE_FPS = 10;
const LIVE_MS = 400;
const WARMUP_MS = 8000;
const WINDOW_MS = 30000;

/** A share running under a fixed impairment. whom says whose traffic: the viewer's, the sharer's, or the whole link. */
async function steadyShare(server, { down, up, whom = 'viewer', windowMs = WINDOW_MS, adaptive = false, publish, sdk, mode }) {
  const call = await openCall(server, { adaptive, sdk, mode });
  try {
    if (whom === 'viewer') await server.impair({ down, up, ports: call.viewerPorts });
    if (whom === 'all') await server.impair({ down, up });
    const t0 = Date.now();
    await publishScreen(call.publisher, publish ? { publish } : undefined);
    const firstFrameMs = await waitForFrameAfter(call.viewer, t0, 30000);
    if (whom === 'sharer') {
      // The sharing connection does not exist until it publishes, so this one is impaired from just after that.
      const others = new Set(call.viewerPorts);
      const signal = (await server.signalPorts()).filter((p) => !others.has(p));
      await server.impair({ down, up, ports: [...(await call.publisher.mediaPorts()), ...signal] });
    }
    await sleep(Math.max(0, t0 + WARMUP_MS + windowMs - Date.now()));
    const from = t0 + WARMUP_MS;
    const summary = await viewerSummary(call.viewer, from);
    const events = await call.viewer.events();
    const path = await selectedPath(call.viewer);
    return {
      firstFrameMs,
      ...summary,
      paused: events.filter((e) => e.name === 'trackStreamStateChanged' && e.detail.includes('paused')).length,
      reconnects: events.filter((e) => e.name === 'reconnecting').length,
      netem: await server.netemCounters(),
      viewerStayedOnImpairedPorts: whom !== 'viewer' || path.every((p) => call.viewerPorts.includes(p.localPort)),
      sender: senderSummary(call.publisher, from),
      errors: [...call.viewer.errors, ...call.publisher.errors],
    };
  } finally {
    await call.close();
  }
}

function plays(m, c, { fps, gapMs, latencyMs }) {
  c.check(m.firstFrameMs !== null, `a first frame arrived (${m.firstFrameMs} ms)`);
  c.check(m.fps >= fps, `at least ${fps} fps (got ${m.fps})`);
  c.check(m.longestGapMs <= gapMs, `longest gap at most ${gapMs} ms (got ${m.longestGapMs})`);
  if (latencyMs) c.check(m.p50 !== null && m.p50 <= latencyMs, `median latency at most ${latencyMs} ms (got ${m.p50})`);
  c.check(m.reconnects === 0, `no reconnect (got ${m.reconnects})`);
  c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
  c.check(m.viewerStayedOnImpairedPorts, 'media stayed on the impaired path');
}

// The conditions of the first hand measurements, for comparing with them directly: the whole link, one way, and adaptive
// stream on. With it on, the reference shows its 960 px layer here and ours its 1920 px one, so the simulcast rows do not
// compare like with like, and the single layer rows do.
describe('the hand-measured WiFi condition', () => {
  const judge = (m, c) => {
    plays(m, c, { fps: SMOOTH_FPS, gapMs: 1000, latencyMs: LIVE_MS });
    c.check(m.usualWidth === 1920, `stays at 1920 px (got ${JSON.stringify(m.widths)})`);
    c.check(m.paused === 0, 'video is not paused');
  };
  const spec = 'delay 10ms 8ms 25% loss 0.5%';
  onBothServers(describe, it, 'netem delay 10ms 8ms 25% loss 0.5%, whole link, server to browser, simulcast', {
    link: `server to both browsers: ${spec}`,
    run: (server) => steadyShare(server, { down: spec, whom: 'all', adaptive: true }),
    judge,
  });
  onBothServers(describe, it, 'netem delay 10ms 8ms 25% loss 0.5%, whole link, server to browser, single layer', {
    link: `server to both browsers: ${spec}`,
    run: (server) => steadyShare(server, { down: spec, whom: 'all', adaptive: true, publish: { simulcast: false, screenShareEncoding: { maxBitrate: 5_000_000, maxFramerate: 30 } } }),
    judge,
  });
});

describe('packet loss', () => {
  // NACKs reach the server and are answered if the lost packets come back. What stays lost in the end must be far less
  // than what the link dropped, and the browser must not have to fall back on asking for keyframes.
  const answersNacks = (m, c, lossPct) => {
    c.check(m.nacks > 0, `the browser sent NACKs (got ${m.nacks})`);
    const counted = m.retransmitted > 0 ? `${m.retransmitted} packets retransmitted` : `${m.residualLossPct}% stayed lost`;
    c.check(m.retransmitted > 0 || m.residualLossPct < lossPct / 2, `lost packets came back: ${counted} on a ${lossPct}% link`);
  };

  for (const [lossPct, fps, gapMs] of [
    [1, SMOOTH_FPS, 1000],
    [5, SMOOTH_FPS, 1000],
    [10, USABLE_FPS, 2000],
    [20, USABLE_FPS, 2000],
  ]) {
    onBothServers(describe, it, `${lossPct}% loss on the viewer's link, both ways`, {
      link: `viewer's ports only, both directions: loss ${lossPct}%`,
      run: (server) => steadyShare(server, { down: `loss ${lossPct}%`, up: `loss ${lossPct}%` }),
      judge: (m, c) => {
        plays(m, c, { fps, gapMs, latencyMs: lossPct <= 5 ? LIVE_MS : undefined });
        answersNacks(m, c, lossPct);
      },
    });
  }

  // The official client on one peer connection takes another route through the server, with RTX negotiated.
  onBothServers(describe, it, "5% loss on the viewer's link, official client on /rtc/v1", {
    link: "viewer's ports only, both directions: loss 5%",
    sdk: 'official client, one peer connection',
    run: (server) => steadyShare(server, { down: 'loss 5%', up: 'loss 5%', sdk: 'official', mode: 'v1' }),
    judge: (m, c) => {
      plays(m, c, { fps: SMOOTH_FPS, gapMs: 1000, latencyMs: LIVE_MS });
      answersNacks(m, c, 5);
    },
  });

  onBothServers(describe, it, "5% loss on the sharer's link, both ways", {
    link: "sharer's ports only, both directions: loss 5%",
    run: (server) => steadyShare(server, { down: 'loss 5%', up: 'loss 5%', whom: 'sharer' }),
    judge: (m, c) => plays(m, c, { fps: USABLE_FPS, gapMs: 2000 }),
  });
});

describe('a high round trip', () => {
  const judge = (m, c, extraOneWayMs) => {
    plays(m, c, { fps: SMOOTH_FPS, gapMs: 1000, latencyMs: LIVE_MS + extraOneWayMs });
    c.check(m.usualWidth === 1920, `stays at 1920 px (got ${JSON.stringify(m.widths)})`);
  };

  onBothServers(describe, it, "200 ms round trip on the viewer's link", {
    link: "viewer's ports only, both directions: delay 100ms",
    run: (server) => steadyShare(server, { down: 'delay 100ms', up: 'delay 100ms' }),
    judge: (m, c) => {
      judge(m, c, 100);
      c.check(m.rttMs >= 180 && m.rttMs <= 260, `the browser measured about 200 ms round trip (got ${m.rttMs})`);
    },
  });

  onBothServers(describe, it, "200 ms round trip on the sharer's link", {
    link: "sharer's ports only, both directions: delay 100ms",
    run: (server) => steadyShare(server, { down: 'delay 100ms', up: 'delay 100ms', whom: 'sharer' }),
    judge: (m, c) => judge(m, c, 100),
  });
});

describe('jitter on an otherwise clean link', () => {
  // The WiFi case. The link has room to spare, so nothing should pause and no layer should be given up.
  for (const spec of ['delay 10ms 8ms 25%', 'delay 30ms 20ms 25%']) {
    onBothServers(describe, it, `${spec} on the viewer's link, both ways`, {
      link: `viewer's ports only, both directions: ${spec}`,
      run: (server) => steadyShare(server, { down: spec, up: spec }),
      judge: (m, c) => {
        plays(m, c, { fps: SMOOTH_FPS, gapMs: 1000, latencyMs: LIVE_MS + 50 });
        c.check(m.paused === 0, 'video is not paused');
        c.check(Object.keys(m.widths).length === 1 && m.topWidth === 1920, `never leaves 1920 px (got ${JSON.stringify(m.widths)})`);
      },
    });
  }
});

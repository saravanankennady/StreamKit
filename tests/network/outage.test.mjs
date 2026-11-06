// The link goes away completely, and comes back. Emulated with tc netem at 100% loss both ways on one machine: packets
// vanish with no error to anyone, as they do when a route is lost. Both browsers are cut off, since both reach the server
// over the same link here. See lib.mjs.
//
// Thresholds, and why:
//   Short outage  Under the time anything gives up, so video should come back about as soon as the link does.
//   Long outage   Past our server's 20 s reconnect window, so the session is gone and the client has to make a new one.
//                 The client's own attempts run for about 40 s, so it must still be trying when the link returns.
import { after, describe, it } from 'node:test';
import { onBothServers, openCall, publishScreen, sleep, stopFixture, viewerSummary, waitForFrameAfter, waitForSteadyFramesAfter, waitForWidth } from './lib.mjs';

after(stopFixture);

const SHORT = { seconds: 5, firstFrameMs: 8000, steadyMs: 15000 };
const LONG = { seconds: 30, firstFrameMs: 30000, steadyMs: 40000 };

async function outage(server, { seconds }) {
  const call = await openCall(server);
  try {
    const t0 = Date.now();
    await publishScreen(call.publisher);
    await waitForWidth(call.viewer, t0, 1920, 30000);
    const before = await viewerSummary(call.viewer, Date.now() - 5000);

    await server.impair({ down: 'loss 100%', up: 'loss 100%' });
    const cut = Date.now();
    await sleep(seconds * 1000);
    await server.clear();
    const restored = Date.now();

    const firstFrameMs = await waitForFrameAfter(call.viewer, restored, 45000);
    const steadyMs = await waitForSteadyFramesAfter(call.viewer, restored, 60000, { minFps: 10 });
    await sleep(5000);
    const end = Date.now();
    const after = await viewerSummary(call.viewer, end - 5000, end);
    const viewerEvents = await call.viewer.events();
    const publisherEvents = await call.publisher.events();
    const count = (events, name) => events.filter((e) => e.name === name && e.at >= cut).length;
    return {
      beforeFps: before.fps,
      firstFrameMs,
      steadyMs,
      afterFps: after.fps,
      afterWidth: after.usualWidth,
      viewerState: await call.viewer.state(),
      publisherState: await call.publisher.state(),
      viewerReconnecting: count(viewerEvents, 'reconnecting') + count(viewerEvents, 'signalReconnecting'),
      viewerReconnected: count(viewerEvents, 'reconnected'),
      publisherReconnecting: count(publisherEvents, 'reconnecting') + count(publisherEvents, 'signalReconnecting'),
      publisherReconnected: count(publisherEvents, 'reconnected'),
      errors: [...call.viewer.errors, ...call.publisher.errors],
    };
  } finally {
    await call.close();
  }
}

function recovers(m, c, limits) {
  c.check(m.firstFrameMs !== null && m.firstFrameMs <= limits.firstFrameMs, `a frame within ${limits.firstFrameMs} ms of the link returning (got ${m.firstFrameMs})`);
  c.check(m.steadyMs !== null && m.steadyMs <= limits.steadyMs, `steady video within ${limits.steadyMs} ms (got ${m.steadyMs})`);
  c.check(m.afterFps >= 15, `playing properly afterwards (got ${m.afterFps} fps)`);
  c.check(m.viewerState === 'connected' && m.publisherState === 'connected', `both clients connected (${m.viewerState}, ${m.publisherState})`);
  c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
}

describe('a complete outage', () => {
  onBothServers(describe, it, `${SHORT.seconds} s outage: media resumes`, {
    link: `whole link, both directions: loss 100% for ${SHORT.seconds} s`,
    run: (server) => outage(server, SHORT),
    judge: (m, c) => recovers(m, c, SHORT),
  });

  onBothServers(describe, it, `${LONG.seconds} s outage, past the disconnect timeout: the clients come back`, {
    link: `whole link, both directions: loss 100% for ${LONG.seconds} s`,
    timeout: 300000,
    run: (server) => outage(server, LONG),
    judge: (m, c) => {
      recovers(m, c, LONG);
      c.check(m.viewerReconnected >= 1 && m.publisherReconnected >= 1, `both clients reconnected (${m.viewerReconnected}, ${m.publisherReconnected})`);
    },
  });
});

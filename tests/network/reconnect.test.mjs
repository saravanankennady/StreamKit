// The client's recovery paths, across a Docker bridge rather than loopback: the signalling socket cut at the server with
// media left alone, the same thing from the client's side, and a full reconnect. Emulated, one machine. See lib.mjs.
//
// Thresholds, and why:
//   Signal drop    Media did not stop, so nobody should see anything: no stall past a normal bad moment.
//   Full reconnect A new session has to be set up and everything published again. A person sees a blip, and video should
//                  be back in the time it takes to say "can you see me?".
import { after, describe, it } from 'node:test';
import { onBothServers, openCall, publishScreen, sleep, stopFixture, viewerSummary, waitForSteadyFramesAfter, waitForWidth } from './lib.mjs';

after(stopFixture);

const SIGNAL_STALL_MS = 1000;
const RECOVERY_MS = 10000;

/** A running share, with the moment it has reached full size. */
async function runningShare(server) {
  const call = await openCall(server);
  const t0 = Date.now();
  await publishScreen(call.publisher);
  const reached = await waitForWidth(call.viewer, t0, 1920, 30000);
  if (reached === null) throw new Error('the share never reached full size');
  return call;
}

const sids = (call) =>
  Promise.all(
    [call.viewer, call.publisher].map((c) =>
      c.page.evaluate(() => window.harness.room.localParticipant.sid),
    ),
  );

async function afterSignalDrop(server, cut) {
  const call = await runningShare(server);
  try {
    const before = await sids(call);
    const at = Date.now();
    await cut(call);
    await sleep(12000);
    const end = Date.now();
    const summary = await viewerSummary(call.viewer, at, end);
    const events = [...(await call.viewer.events()), ...(await call.publisher.events())].filter((e) => e.at >= at);
    return {
      fps: summary.fps,
      longestGapMs: summary.longestGapMs,
      freezes: summary.freezes,
      signalReconnecting: events.filter((e) => e.name === 'signalReconnecting').length,
      reconnected: events.filter((e) => e.name === 'reconnected').length,
      fullReconnects: events.filter((e) => e.name === 'reconnecting').length,
      sameSessions: JSON.stringify(before) === JSON.stringify(await sids(call)),
      states: [await call.viewer.state(), await call.publisher.state()],
      errors: [...call.viewer.errors, ...call.publisher.errors],
    };
  } finally {
    await call.close();
  }
}

function signalDropIsInvisible(m, c) {
  c.check(m.reconnected >= 1, `clients reported reconnecting and coming back (${m.reconnected})`);
  c.check(m.sameSessions, 'the sessions survived');
  c.check(m.longestGapMs <= SIGNAL_STALL_MS, `media kept going: longest gap at most ${SIGNAL_STALL_MS} ms (got ${m.longestGapMs})`);
  c.check(m.states.every((s) => s === 'connected'), `both connected (${m.states})`);
  c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
}

describe('signalling socket dropped', () => {
  onBothServers(describe, it, 'cut at the server with ss -K, media untouched', {
    link: 'no impairment. ss -K destroys the sockets on port 7880 inside the container',
    run: (server) => afterSignalDrop(server, () => server.killSignalSockets()),
    judge: signalDropIsInvisible,
  });

  onBothServers(describe, it, "the sharer's socket closed from the client: simulateScenario signal-reconnect", {
    link: 'no impairment',
    run: (server) => afterSignalDrop(server, (call) => call.publisher.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'))),
    judge: signalDropIsInvisible,
  });
});

describe('full reconnect', () => {
  const fullReconnect = (who) => async (server) => {
    const call = await runningShare(server);
    try {
      const before = await sids(call);
      const client = call[who];
      const at = Date.now();
      await client.page.evaluate(() => window.harness.room.simulateScenario('full-reconnect'));
      const steadyMs = await waitForSteadyFramesAfter(call.viewer, at + 500, 45000, { minFps: 10 });
      await sleep(5000);
      const end = Date.now();
      const after = await viewerSummary(call.viewer, end - 5000, end);
      const events = (await client.events()).filter((e) => e.at >= at);
      const now = await sids(call);
      return {
        steadyMs,
        afterFps: after.fps,
        afterWidth: after.usualWidth,
        reconnecting: events.filter((e) => e.name === 'reconnecting').length,
        reconnected: events.filter((e) => e.name === 'reconnected').length,
        newSession: before[who === 'viewer' ? 0 : 1] !== now[who === 'viewer' ? 0 : 1],
        states: [await call.viewer.state(), await call.publisher.state()],
        errors: [...call.viewer.errors, ...call.publisher.errors],
      };
    } finally {
      await call.close();
    }
  };
  const judge = (m, c) => {
    c.check(m.reconnecting >= 1 && m.reconnected >= 1, `reported going away and coming back (${m.reconnecting}, ${m.reconnected})`);
    c.check(m.newSession, 'a full reconnect is a new session');
    c.check(m.steadyMs !== null && m.steadyMs <= RECOVERY_MS, `steady video within ${RECOVERY_MS} ms (got ${m.steadyMs})`);
    c.check(m.afterFps >= 15, `playing properly afterwards (got ${m.afterFps} fps)`);
    c.check(m.states.every((s) => s === 'connected'), `both connected (${m.states})`);
    c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
  };

  onBothServers(describe, it, 'the viewer does a full reconnect', { link: 'no impairment', run: fullReconnect('viewer'), judge });
  onBothServers(describe, it, 'the sharer does a full reconnect, and publishes again', { link: 'no impairment', run: fullReconnect('publisher'), judge });
});

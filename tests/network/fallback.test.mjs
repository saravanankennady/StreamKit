// When UDP does not get through: ICE over TCP, and relaying through a real coturn. The blocking is iptables inside the
// containers, on one machine, so this shows the mechanism works. It says nothing about any real firewall or carrier.
// See lib.mjs.
//
// coturn here runs the image the production Compose file names, with the same credentials scheme. One difference: it
// is allowed to reach private addresses, because on a Docker bridge everything is one. Production refuses them.
import assert from 'node:assert/strict';
import { after, describe, it } from 'node:test';
import { Box, TURN_SECRET, createNetwork, ensureImage, machineLoad, openCall, publishScreen, recordResult, Checks, SERVERS, selectedPath, sleep, startServer, stopFixture, viewerSummary, waitForFrameAfter, waitForRoom, waitForSteadyFramesAfter, NET_IMAGE, onBothServers } from './lib.mjs';

after(stopFixture);

const PLAYING_FPS = 15;
const FIRST_FRAME_MS = 15000;

async function runShare(call, { windowMs = 15000 } = {}) {
  const t0 = Date.now();
  await publishScreen(call.publisher);
  const firstFrameMs = await waitForFrameAfter(call.viewer, t0, 45000);
  await sleep(windowMs);
  const summary = await viewerSummary(call.viewer, Date.now() - windowMs);
  return {
    firstFrameMs,
    ...summary,
    viewerPath: await selectedPath(call.viewer),
    publisherPath: await selectedPath(call.publisher),
    errors: [...call.viewer.errors, ...call.publisher.errors],
  };
}

const usesTcp = (path) => path.length > 0 && path.every((p) => p.localProtocol === 'tcp' || p.remoteProtocol === 'tcp');
const usesRelay = (path) => path.length > 0 && path.every((p) => p.localType === 'relay' || p.remoteType === 'relay');

describe('UDP blocked', () => {
  onBothServers(describe, it, 'UDP blocked from the start: ICE falls back to TCP', {
    link: 'iptables in the server container: all UDP dropped, both ways',
    run: async (server) => {
      await server.iptables(['-I INPUT -p udp -j DROP', '-I OUTPUT -p udp -j DROP']);
      const connectStart = Date.now();
      const call = await openCall(server);
      const connectMs = Date.now() - connectStart;
      try {
        return { connectMs, ...(await runShare(call)) };
      } finally {
        await call.close();
      }
    },
    judge: (m, c) => {
      c.check(m.firstFrameMs !== null && m.firstFrameMs <= FIRST_FRAME_MS, `a first frame within ${FIRST_FRAME_MS} ms (got ${m.firstFrameMs})`);
      c.check(m.fps >= PLAYING_FPS, `at least ${PLAYING_FPS} fps (got ${m.fps})`);
      c.check(usesTcp(m.viewerPath), `the viewer's media goes over TCP (${JSON.stringify(m.viewerPath)})`);
      c.check(usesTcp(m.publisherPath), `the sharer's media goes over TCP (${JSON.stringify(m.publisherPath)})`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });

  onBothServers(describe, it, 'UDP blocked during a call: clients move to TCP', {
    link: 'iptables in the server container: all UDP dropped from mid-call',
    timeout: 300000,
    run: async (server) => {
      const call = await openCall(server);
      try {
        const t0 = Date.now();
        await publishScreen(call.publisher);
        await waitForFrameAfter(call.viewer, t0, 30000);
        await sleep(5000);
        const blocked = Date.now();
        await server.iptables(['-I INPUT -p udp -j DROP', '-I OUTPUT -p udp -j DROP']);
        const steadyMs = await waitForSteadyFramesAfter(call.viewer, blocked + 2000, 90000, { minFps: 10 });
        await sleep(5000);
        const end = Date.now();
        const after = await viewerSummary(call.viewer, end - 5000, end);
        return {
          steadyMs,
          afterFps: after.fps,
          viewerPath: await selectedPath(call.viewer),
          publisherPath: await selectedPath(call.publisher),
          viewerState: await call.viewer.state(),
          publisherState: await call.publisher.state(),
          errors: [...call.viewer.errors, ...call.publisher.errors],
        };
      } finally {
        await call.close();
      }
    },
    judge: (m, c) => {
      c.check(m.steadyMs !== null && m.steadyMs <= 60000, `steady video again within a minute (got ${m.steadyMs})`);
      c.check(m.afterFps >= PLAYING_FPS, `at least ${PLAYING_FPS} fps afterwards (got ${m.afterFps})`);
      c.check(usesTcp(m.viewerPath) && usesTcp(m.publisherPath), `both on TCP (${JSON.stringify([m.viewerPath, m.publisherPath])})`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });
});

/** Starts coturn with the production command line, less TLS and less the refusal of private addresses. */
async function startCoturn(network) {
  const box = await Box.run({
    what: 'coturn',
    image: 'coturn/coturn:4.7-alpine',
    network,
    caps: [],
    memory: '512m',
    cmd: ['--no-cli', '--no-tls', '--no-dtls', '--fingerprint', '--lt-cred-mech', '--use-auth-secret', `--static-auth-secret=${TURN_SECRET}`, '--realm=network-suite.test', '--listening-port=3478', '--min-port=49160', '--max-port=49300', '--no-multicast-peers', '--verbose'],
  });
  return { box, ip: await box.ip() };
}

/** Runs iptables inside coturn's network, from a small container sharing it, since coturn's image does not have it. */
async function coturnIptables(coturn, rules) {
  await ensureImage();
  const side = await Box.run({ what: 'coturnfw', image: NET_IMAGE, extra: [`--network=container:${coturn.box.container}`] });
  try {
    await side.sh(rules.map((r) => `iptables ${r}`).join('\n'));
  } finally {
    await side.remove();
  }
}

function turnConfig(ip) {
  return {
    urls: [`turn:${ip}:3478?transport=udp`, `turn:${ip}:3478?transport=tcp`],
    servers: [
      { host: ip, port: 3478, protocol: 'udp' },
      { host: ip, port: 3478, protocol: 'tcp' },
    ],
  };
}

describe('TURN through a real coturn, with direct paths blocked', () => {
  for (const variant of ['udp', 'tcp']) {
    describe(`browser to coturn over ${variant}`, () => {
      let reference;
      for (const kind of SERVERS) {
        it(kind, { timeout: 480000 }, async (t) => {
          await waitForRoom();
          const started = machineLoad();
          const network = await createNetwork();
          const coturn = await startCoturn(network.name);
          const server = await startServer(kind, { network: network.name, turn: turnConfig(coturn.ip) });
          let metrics;
          let error;
          let allocations;
          try {
            // Signalling stays open. Nothing reaches media ports except from coturn, so the only way in is the relay.
            await server.iptables([`-I INPUT -p udp ! -s ${coturn.ip} -j DROP`, `-I INPUT -p tcp --dport 7881 ! -s ${coturn.ip} -j DROP`]);
            if (variant === 'tcp') await coturnIptables(coturn, ['-I INPUT -p udp --dport 3478 -j DROP']);
            const call = await openCall(server);
            try {
              metrics = await runShare(call);
            } finally {
              await call.close();
            }
            allocations = (await coturn.box.sh("grep -c 'ALLOCATE processed, success' /proc/1/fd/1 2>/dev/null; true", { allowFail: true })) || null;
          } catch (e) {
            error = e;
            t.diagnostic(`server log tail:\n${await server.logTail(15)}`);
          } finally {
            await server.stop();
            await coturn.box.remove();
            await network.remove();
          }
          const checks = new Checks();
          if (metrics) {
            checks.check(metrics.firstFrameMs !== null && metrics.firstFrameMs <= FIRST_FRAME_MS, `a first frame within ${FIRST_FRAME_MS} ms (got ${metrics.firstFrameMs})`);
            checks.check(metrics.fps >= PLAYING_FPS, `at least ${PLAYING_FPS} fps (got ${metrics.fps})`);
            checks.check(usesRelay(metrics.viewerPath), `the viewer's media is relayed (${JSON.stringify(metrics.viewerPath)})`);
            checks.check(usesRelay(metrics.publisherPath), `the sharer's media is relayed (${JSON.stringify(metrics.publisherPath)})`);
            for (const path of [metrics.viewerPath, metrics.publisherPath]) {
              checks.check(path.every((p) => p.relayProtocol === variant), `relayed over ${variant} (${path.map((p) => p.relayProtocol).join(',')})`);
            }
          }
          recordResult({
            scenario: `TURN, browser to coturn over ${variant}`,
            server: kind,
            sdk: process.env.NET_SDK ?? 'streamkit',
            link: `iptables: media ports refuse everything but coturn${variant === 'tcp' ? '; UDP to coturn dropped too' : ''}. coturn 4.7 in a container on the same bridge`,
            network: 'emulated: Docker bridge, one machine',
            load: { started, ended: machineLoad() },
            metrics,
            allocations,
            failed: checks.failed,
            passed: checks.passed,
            error: error ? String(error.message ?? error).split('\n')[0] : undefined,
          });
          t.diagnostic(`${kind}: ${JSON.stringify(metrics)} load=${started.load1}`);
          if (kind === 'reference') {
            reference = metrics;
            if (error) t.diagnostic(`reference failed: ${error.message}`);
            return;
          }
          if (error) throw error;
          assert.deepEqual(checks.failed, [], `TURN over ${variant} on ours, with the reference at ${JSON.stringify(reference?.viewerPath)}`);
        });
      }
    });
  }
});

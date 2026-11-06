// A client's address changes during a call, as it does walking out of WiFi coverage onto a phone network.
//
// One Chrome runs in a container of its own, so it can have network interfaces to lose. It starts on network A. It then
// gains network B and loses A, so its address really changes. A small router container joins A and B to the server's
// network, so the server stays at one address throughout, as a real server would, and sees the client arrive from
// somewhere new. Nothing is translated on the way.
//
// This is still one machine with Docker bridges. A real handover also has radio behaviour, carrier NAT and a change of
// path, and none of that is here. Chrome uses fake devices.
//
// Threshold: a person on a call moving between networks expects a short break, in the order of ten seconds, and the
// reference's time is logged beside ours.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { connect as tcpConnect, createServer as createTcpServer } from 'node:net';
import { after, describe, it } from 'node:test';
import { join } from 'node:path';
import { Box, Checks, SERVERS, chromium, createNetwork, ensureImage, fixture, here, launchChrome, machineLoad, measureFrames, nextRoom, openClient, publishCamera, recordResult, selectedPath, sleep, startServer, stopFixture, viewerSummary, waitForRoom, waitForSteadyFramesAfter } from './lib.mjs';

after(stopFixture);

const BROWSER_IMAGE = 'elnova-nettest-browser:1';
const RESUME_MS = 15000;

/** Forwards a TCP port on the host to a command run in a container, one process per connection, on no network at all. */
function controlTunnel(container, port) {
  const server = createTcpServer((socket) => {
    const child = spawn('docker', ['exec', '-i', container, 'socat', '-', `TCP:127.0.0.1:${port}`], { stdio: ['pipe', 'pipe', 'ignore'] });
    socket.pipe(child.stdin);
    child.stdout.pipe(socket);
    const end = () => {
      socket.destroy();
      child.kill();
    };
    socket.on('close', end);
    socket.on('error', end);
    child.on('close', end);
    child.stdin.on('error', () => {});
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ port: server.address().port, close: () => server.close() })));
}

/** Makes the test page reachable from a Docker network, for the one moment it loads. */
function pageTunnel(gateway, targetPort) {
  const server = createTcpServer((socket) => {
    const upstream = tcpConnect(targetPort, '127.0.0.1');
    socket.pipe(upstream);
    upstream.pipe(socket);
    socket.on('error', () => upstream.destroy());
    upstream.on('error', () => socket.destroy());
  });
  return new Promise((resolve) => server.listen(0, gateway, () => resolve({ port: server.address().port, close: () => server.close() })));
}

/** The whole arrangement: three networks, a router, a server, and a Chrome on network A. */
async function buildWorld(kind) {
  await ensureImage();
  await ensureImage(BROWSER_IMAGE, join(here, 'docker/browser.Dockerfile'));
  const S = await createNetwork('s');
  const A = await createNetwork('a');
  const B = await createNetwork('b');
  const parts = { networks: [S, A, B], boxes: [], tunnels: [] };
  const router = await Box.run({ what: 'router', network: S.name, sysctls: ['net.ipv4.ip_forward=1', 'net.ipv4.conf.all.rp_filter=0'], memory: '128m' });
  parts.boxes.push(router);
  await dockerNetwork('connect', A.name, router.container);
  await dockerNetwork('connect', B.name, router.container);
  const routerOn = { S: await router.ip(S.name), A: await router.ip(A.name), B: await router.ip(B.name) };

  const server = await startServer(kind, { network: S.name });
  parts.server = server;
  await server.box.sh(`ip route add ${A.subnet} via ${routerOn.S} && ip route add ${B.subnet} via ${routerOn.S}`);

  const { port: fixturePort } = await fixture();
  const pages = await pageTunnel(A.gateway, fixturePort);
  parts.tunnels.push(pages);
  const origin = `http://${A.gateway}:${pages.port}`;
  const chrome = await Box.run({
    what: 'roamer',
    image: BROWSER_IMAGE,
    network: A.name,
    memory: '3g',
    mounts: [['/opt/google/chrome', '/opt/google/chrome']],
    extra: ['--shm-size=512m'],
  });
  parts.boxes.push(chrome);
  await chrome.sh(`ip route add ${S.subnet} via ${routerOn.A}`);
  await chrome.background(
    '/opt/google/chrome/chrome --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage --remote-debugging-port=9222 --user-data-dir=/tmp/chrome ' +
      '--use-fake-device-for-media-stream --use-fake-ui-for-media-stream --autoplay-policy=no-user-gesture-required ' +
      `--unsafely-treat-insecure-origin-as-secure=${origin} about:blank > /tmp/chrome.log 2>&1`,
  );
  const control = await controlTunnel(chrome.container, 9222);
  parts.tunnels.push(control);
  let browser;
  for (let i = 0; i < 40 && !browser; i++) {
    try {
      browser = await chromium.connectOverCDP(`http://127.0.0.1:${control.port}`, { timeout: 2000 });
    } catch {
      await sleep(500);
    }
  }
  assert.ok(browser, `Chrome in the container did not start: ${await chrome.sh('tail -5 /tmp/chrome.log', { allowFail: true })}`);
  parts.browser = browser;
  return { ...parts, A, B, S, routerOn, server, chrome, origin, browser };
}

async function tearDown(world) {
  await world.browser?.close().catch(() => {});
  await world.server?.stop();
  for (const box of world.boxes) await box.remove();
  for (const tunnel of world.tunnels) tunnel.close();
  for (const net of world.networks) await net.remove();
}

const dockerNetwork = (verb, network, container) =>
  new Promise((resolve, reject) => execFile('docker', ['network', verb, network, container], (err, _o, stderr) => (err ? reject(new Error(stderr)) : resolve())));

describe('the client address changes during a call', () => {
  let reference;
  for (const kind of SERVERS) {
    it(kind, { timeout: 540000 }, async (t) => {
      await waitForRoom();
      const started = machineLoad();
      let world;
      let metrics;
      let error;
      let there;
      let roamer;
      try {
        world = await buildWorld(kind);
        const room = nextRoom('roam');
        there = await openClient(await launchChrome(), { identity: 'there', room, ws: world.server.ws });
        await measureFrames(there);
        roamer = await openClient(world.browser, { identity: 'roamer', room, ws: world.server.ws, ownsBrowser: false, origin: world.origin });
        await measureFrames(roamer);
        await roamer.page.evaluate(async () => {
          await window.harness.room.localParticipant.setCameraEnabled(true);
          await window.harness.room.localParticipant.setMicrophoneEnabled(true);
        });
        await publishCamera(there, { width: 640, height: 360, audio: false });

        const t0 = Date.now();
        const roamerSees = await waitForSteadyFramesAfter(roamer, t0, 30000, { minFps: 5 });
        const thereSees = await waitForSteadyFramesAfter(there, t0, 30000, { minFps: 5 });
        assert.ok(roamerSees !== null && thereSees !== null, `video did not start: roamer ${roamerSees}, other ${thereSees}`);
        // The browser hides its own address in its statistics, so the container is asked. With only one address left, a
        // browser that is receiving media can only be receiving it there.
        const addresses = () => world.chrome.sh("ip -4 -o addr show scope global | awk '{print $4}'");
        const addressBefore = await addresses();

        // The moment of the handover: the new network appears, traffic to the server moves to it, the old one goes.
        await dockerNetwork('connect', world.B.name, world.chrome.container);
        await world.chrome.sh(`ip route replace ${world.S.subnet} via ${world.routerOn.B}`);
        await dockerNetwork('disconnect', world.A.name, world.chrome.container);
        const moved = Date.now();

        const roamerResumes = await waitForSteadyFramesAfter(roamer, moved, 60000, { minFps: 5 });
        const thereResumes = await waitForSteadyFramesAfter(there, moved, 60000, { minFps: 5 });
        await sleep(5000);
        const end = Date.now();
        const roamerAfter = await viewerSummary(roamer, end - 5000, end);
        const thereAfter = await viewerSummary(there, end - 5000, end);
        const events = (await roamer.events()).filter((e) => e.at >= moved);
        metrics = {
          roamerResumes,
          thereResumes,
          roamerFpsAfter: roamerAfter.fps,
          thereFpsAfter: thereAfter.fps,
          addressBefore,
          addressAfter: await addresses(),
          paths: await selectedPath(roamer),
          subnetA: world.A.subnet,
          subnetB: world.B.subnet,
          reconnecting: events.filter((e) => e.name === 'reconnecting').length,
          reconnected: events.filter((e) => e.name === 'reconnected').length,
          state: await roamer.state(),
          errors: [...roamer.errors, ...there.errors],
        };
      } catch (e) {
        error = e;
        if (world?.server) t.diagnostic(`server log tail:\n${await world.server.logTail(15)}`);
      } finally {
        await roamer?.close();
        await there?.close();
        if (world) await tearDown(world);
      }
      const checks = new Checks();
      if (metrics) {
        const prefix = (subnet) => subnet.split('.').slice(0, 2).join('.');
        checks.check(metrics.addressBefore.startsWith(prefix(metrics.subnetA)) && metrics.addressAfter.startsWith(prefix(metrics.subnetB)) && !metrics.addressAfter.includes(prefix(metrics.subnetA)), `the client's address changed (${metrics.addressBefore} to ${metrics.addressAfter})`);
        checks.check(metrics.roamerResumes !== null && metrics.roamerResumes <= RESUME_MS, `the client that moved sees video again within ${RESUME_MS} ms (got ${metrics.roamerResumes})`);
        checks.check(metrics.thereResumes !== null && metrics.thereResumes <= RESUME_MS, `the other client sees it again within ${RESUME_MS} ms (got ${metrics.thereResumes})`);
        checks.check(metrics.roamerFpsAfter >= 10 && metrics.thereFpsAfter >= 10, `both play properly afterwards (${metrics.roamerFpsAfter}, ${metrics.thereFpsAfter} fps)`);
        checks.check(metrics.state === 'connected', `connected (${metrics.state})`);
        checks.check(metrics.errors.length === 0, `no errors in the page (${metrics.errors[0] ?? 'none'})`);
      }
      recordResult({
        scenario: 'network switch: the client address changes during a call',
        server: kind,
        sdk: process.env.NET_SDK ?? 'streamkit',
        link: 'Chrome in a container moves between two Docker networks, routed to the server by a third container. Fake devices',
        network: 'emulated: Docker bridges and a router container, one machine',
        load: { started, ended: machineLoad() },
        metrics,
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
      assert.deepEqual(checks.failed, [], `network switch on ours, with the reference at ${JSON.stringify(reference)}`);
    });
  }
});

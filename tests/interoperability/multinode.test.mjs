// Two real server processes sharing Redis, with real Chrome clients. Needs Redis, from
// `docker compose up -d redis`, at REDIS_URL. Run: node --test multinode.test.mjs
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';
import { mintToken, startFixtureServer } from './oracle/fixture-server.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const binary = process.env.STREAMKIT_SERVER ?? join(here, '../../bin/streamkit-server-cluster');
const redisURL = process.env.REDIS_URL ?? 'redis://127.0.0.1:6380/1';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function startNode(index) {
  const port = 27880 + index * 1000;
  const dir = mkdtempSync(join(tmpdir(), 'streamkit-node-'));
  const config = join(dir, 'config.yaml');
  writeFileSync(config, `port: ${port}
log_level: warn
rtc:
  udp_port: ${port + 2}
  tcp_port: ${port + 1}
redis:
  url: ${redisURL}
  node_url: http://127.0.0.1:${port}
`);
  const child = spawn(binary, ['--dev', '--config', config], { stdio: ['ignore', 'ignore', 'inherit'] });
  return { port, child, http: `http://127.0.0.1:${port}`, ws: `ws://127.0.0.1:${port}` };
}

const nodes = [];
let fixture;
let browser;
let counter = 0;
const nextRoom = () => `mn-${process.pid}-${++counter}`;

async function waitReady(node) {
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(`${node.http}/healthz`)).ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error('node did not start');
}

before(async () => {
  nodes.push(startNode(0), startNode(1));
  await Promise.all(nodes.map(waitReady));
  // Registration happens once a few seconds; both nodes must know each other before the first room.
  await sleep(3500);
  fixture = await startFixtureServer();
  browser = await chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
  });
});

after(async () => {
  await browser?.close();
  fixture?.server.close();
  for (const n of nodes) n.child.kill('SIGKILL');
});

async function connect(identity, room, node) {
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'] });
  const page = await ctx.newPage();
  const q = new URLSearchParams({ ws: node.ws, identity, room, mode: 'v1', publish: '1' });
  await page.goto(`http://127.0.0.1:${fixture.port}/?${q}`);
  await page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
  assert.equal(await page.$eval('#state', (e) => e.dataset.state), 'connected', `${identity} on port ${node.port}`);
  return { page, ctx, identity, close: () => ctx.close() };
}

const waitForSubscribed = (c, identity, timeout = 25000) =>
  c.page.waitForFunction((who) => window.harness.subscribedSources(who).length >= 2, identity, { timeout, polling: 200 });

async function assertVideoFlows(c, identity, minFrames = 15) {
  const frames = () =>
    c.page.evaluate(async (who) => (await window.harness.subscribedStats()).find((s) => s.identity === who && s.kind === 'video')?.framesDecoded ?? 0, identity);
  const a = await frames();
  await sleep(2500);
  const b = await frames();
  assert.ok(b - a >= minFrames, `${c.identity} decoded ${b - a} frames of ${identity}`);
}

const twirp = async (node, method, room, body) => {
  const token = mintToken({ apiKey: 'devkey', apiSecret: 'secret', identity: 'api', room, grants: { roomAdmin: true, roomCreate: true, roomList: true, roomJoin: false } });
  const r = await fetch(`${node.http}/twirp/livekit.RoomService/${method}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify(body),
  });
  return { status: r.status, json: await r.json().catch(() => null) };
};

async function hostOf(room) {
  // Only the hosting node counts the room in its own gauge, but both answer the API. Ask each node's metrics.
  const counts = await Promise.all(
    nodes.map(async (n) => {
      if (n.dead) return 0;
      const text = await (await fetch(`${n.http}/metrics`)).text();
      return Number(/^streamkit_rooms (\d+)/m.exec(text)?.[1] ?? 0);
    }),
  );
  return counts;
}

describe('cluster', () => {
  it('clients on different nodes share one room, on one media server', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, nodes[0]);
    const b = await connect('bob', room, nodes[1]);
    try {
      await waitForSubscribed(a, 'bob');
      await waitForSubscribed(b, 'alice');
      await assertVideoFlows(a, 'bob');
      await assertVideoFlows(b, 'alice');
      const counts = await hostOf(room);
      assert.equal(counts[0] + counts[1], 1, `one room in total, got ${counts}`);
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('the room API works through either node, and lists are complete on both', async () => {
    const room = nextRoom();
    const created = await twirp(nodes[0], 'CreateRoom', room, { name: room, metadata: 'x' });
    assert.equal(created.status, 200);
    const again = await twirp(nodes[1], 'CreateRoom', room, { name: room });
    assert.equal(again.json.sid, created.json.sid, 'both nodes must see the same room');

    const a = await connect('alice', room, nodes[1]);
    try {
      await sleep(2500);
      for (const node of nodes) {
        const participants = await twirp(node, 'ListParticipants', room, { room });
        assert.equal(participants.status, 200);
        assert.deepEqual(participants.json.participants.map((p) => p.identity), ['alice'], `through port ${node.port}`);
        const listed = await twirp(node, 'ListRooms', room, { names: [room] });
        assert.equal(listed.json.rooms.length, 1, `ListRooms through port ${node.port}: ${JSON.stringify(listed.json)}`);
        assert.equal(listed.json.rooms[0].sid, created.json.sid);
      }
      const updated = await twirp(nodes[0], 'UpdateRoomMetadata', room, { room, metadata: 'via-node-0' });
      assert.equal(updated.status, 200);
      await a.page.waitForFunction(() => window.harness.room.metadata === 'via-node-0', undefined, { timeout: 10000 });
      assert.equal((await twirp(nodes[1], 'DeleteRoom', room, { room })).status, 200);
      await a.page.waitForFunction(() => window.harness.connectionState() === 'disconnected', undefined, { timeout: 10000 });
    } finally {
      await a.close();
    }
  });

  it('a signalling resume through a proxying node keeps the session', async () => {
    const room = nextRoom();
    const a = await connect('alice', room, nodes[0]);
    const b = await connect('bob', room, nodes[1]);
    try {
      await waitForSubscribed(b, 'alice');
      await a.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
      await b.page.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
      await a.page.waitForFunction(() => window.harness.eventCount('reconnected') >= 1, undefined, { timeout: 25000 });
      await b.page.waitForFunction(() => window.harness.eventCount('reconnected') >= 1, undefined, { timeout: 25000 });
      await waitForSubscribed(a, 'bob');
      await waitForSubscribed(b, 'alice');
      await assertVideoFlows(b, 'alice');
      await assertVideoFlows(a, 'bob');
    } finally {
      await a.close();
      await b.close();
    }
  });

  it('when the hosting node dies, clients recover on another node', async () => {
    const room = nextRoom();
    // Connect through node 0, then find which node really hosts the room.
    const before = await hostOf(room);
    const created = await twirp(nodes[0], 'CreateRoom', room, { name: room });
    assert.equal(created.status, 200);
    await sleep(100);
    const afterCreate = await hostOf(room);
    const hostIndex = afterCreate[0] > before[0] ? 0 : 1;
    const host = nodes[hostIndex];
    const survivor = nodes[1 - hostIndex];

    // Both clients use the surviving node's address, as they would behind a load balancer.
    const a = await connect('alice', room, survivor);
    const b = await connect('bob', room, survivor);
    try {
      await waitForSubscribed(a, 'bob');
      await waitForSubscribed(b, 'alice');
      await assertVideoFlows(b, 'alice');

      host.child.kill('SIGKILL');
      host.dead = true;

      // The client resumes, learns the session is gone, and rejoins. The room restarts on the survivor.
      await a.page.waitForFunction(() => window.harness.connectionState() === 'connected' && window.harness.eventCount('reconnected') >= 1, undefined, { timeout: 45000, polling: 200 });
      await b.page.waitForFunction(() => window.harness.connectionState() === 'connected' && window.harness.eventCount('reconnected') >= 1, undefined, { timeout: 45000, polling: 200 });
      await waitForSubscribed(a, 'bob', 30000);
      await waitForSubscribed(b, 'alice', 30000);
      await assertVideoFlows(b, 'alice');
      await assertVideoFlows(a, 'bob');
      const moved = await twirp(survivor, 'ListRooms', room, { names: [room] });
      assert.equal(moved.json.rooms.length, 1);
      assert.notEqual(moved.json.rooms[0].sid, created.json.sid, 'the room restarted, so it has a new session ID');
    } finally {
      await a.close();
      await b.close();
    }
  });
});

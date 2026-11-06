// Runs two real Chrome clients against a server and writes every signalling frame, decoded, to JSONL.
// usage: node oracle/trace.mjs --ws ws://127.0.0.1:7880 --label oracle-dual --mode dual
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { SignalRequest, SignalResponse } from '@livekit/protocol';
import { chromium } from 'playwright-core';
import { startFixtureServer } from './fixture-server.mjs';

const arg = (k, d) => {
  const i = process.argv.indexOf(`--${k}`);
  return i > 0 ? process.argv[i + 1] : d;
};
const ws = arg('ws', 'ws://127.0.0.1:7880');
const label = arg('label', 'trace');
const mode = arg('mode', 'dual');
const seconds = Number(arg('seconds', '12'));
const here = dirname(fileURLToPath(import.meta.url));

const { server, port } = await startFixtureServer({ apiKey: arg('key', 'devkey'), apiSecret: arg('secret', 'secret') });
const browser = await chromium.launch({
  executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
  headless: true,
  args: [
    '--use-fake-device-for-media-stream',
    '--use-fake-ui-for-media-stream',
    '--autoplay-policy=no-user-gesture-required',
    '--disable-features=WebRtcHideLocalIpsWithMdns',
  ],
});

const scrub = (o) => {
  // tokens are replaced so traces can be stored; SDP is kept because it is the point of the trace.
  if (o?.message?.case === 'refreshToken') o.message.value = '<token>';
  return o;
};

async function openClient(identity, publish) {
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'] });
  const page = await ctx.newPage();
  const frames = [];
  const t0 = Date.now();
  page.on('websocket', (sock) => {
    frames.push({ t: Date.now() - t0, dir: 'open', url: sock.url().replace(/access_token=[^&]+/, 'access_token=<token>').replace(/join_request=[^&]+/, 'join_request=<omitted>') });
    sock.on('framesent', ({ payload }) => {
      const msg = SignalRequest.fromBinary(Buffer.from(payload));
      frames.push({ t: Date.now() - t0, dir: 'c2s', case: msg.message.case, value: scrub(msg).toJson() });
    });
    sock.on('framereceived', ({ payload }) => {
      const msg = SignalResponse.fromBinary(Buffer.from(payload));
      frames.push({ t: Date.now() - t0, dir: 's2c', case: msg.message.case, value: scrub(msg).toJson() });
    });
    sock.on('close', () => frames.push({ t: Date.now() - t0, dir: 'close' }));
  });
  page.on('pageerror', (e) => console.error(`[${identity}] pageerror`, e.message));
  await page.goto(`http://127.0.0.1:${port}/?ws=${encodeURIComponent(ws)}&identity=${identity}&room=trace-room&mode=${mode}&publish=${publish ? 1 : 0}`);
  return { page, frames, identity };
}

const a = await openClient('alice', true);
await a.page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
const b = await openClient('bob', true);
await b.page.waitForSelector('#state[data-state="connected"],#state[data-state="error"]', { state: 'attached', timeout: 30000 });
await new Promise((r) => setTimeout(r, seconds * 1000));

const report = {};
for (const c of [a, b]) {
  report[c.identity] = await c.page.evaluate(async () => ({
    state: document.getElementById('state').dataset.state,
    connectionState: window.harness.connectionState(),
    remote: window.harness.remoteIdentities(),
    stats: await window.harness.subscribedStats(),
    frame: window.harness.frameHash(),
    roomEvents: window.harness.events.map((e) => e.name),
  }));
}

const outDir = join(here, 'out');
await mkdir(outDir, { recursive: true });
for (const c of [a, b]) {
  await writeFile(join(outDir, `${label}-${c.identity}.jsonl`), c.frames.map((f) => JSON.stringify(f)).join('\n') + '\n');
}
await writeFile(join(outDir, `${label}-report.json`), JSON.stringify(report, null, 2));
console.log(JSON.stringify(Object.fromEntries(Object.entries(report).map(([k, v]) => [k, { ...v, roomEvents: undefined, eventCount: v.roomEvents.length }])), null, 1));
await browser.close();
server.close();

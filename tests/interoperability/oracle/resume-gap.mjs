// Measures how long media stalls around a signalling resume, sampled from the receiving decoder.
import { chromium } from 'playwright-core';
import { startFixtureServer } from './fixture-server.mjs';

const ws = process.argv[2] ?? 'ws://127.0.0.1:7880';
const { server, port } = await startFixtureServer();
const browser = await chromium.launch({
  executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
  headless: true,
  args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
});
const room = `gap-${Date.now()}`;
async function open(identity) {
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'] });
  const page = await ctx.newPage();
  await page.goto(`http://127.0.0.1:${port}/?ws=${encodeURIComponent(ws)}&identity=${identity}&room=${room}&mode=v1&publish=1`);
  await page.waitForSelector('#state[data-state="connected"]', { state: 'attached', timeout: 30000 });
  return page;
}
const a = await open('alice');
const b = await open('bob');
await a.waitForFunction(() => window.harness.subscribedSources('bob').length >= 2, undefined, { timeout: 20000 });
await b.waitForFunction(() => window.harness.subscribedSources('alice').length >= 2, undefined, { timeout: 20000 });
await new Promise((r) => setTimeout(r, 4000));

const sample = async (page, who) => {
  const rows = await page.evaluate(() => window.harness.subscribedStats());
  return rows.find((r) => r.identity === who && r.kind === 'video')?.framesDecoded ?? 0;
};
const samples = [];
const t0 = Date.now();
const timer = setInterval(async () => {
  samples.push({ t: Date.now() - t0, bob: await sample(b, 'alice'), alice: await sample(a, 'bob') });
}, 250);
await new Promise((r) => setTimeout(r, 1500));
const trigger = Date.now() - t0;
await a.evaluate(() => window.harness.room.simulateScenario('signal-reconnect'));
await a.waitForFunction(() => window.harness.eventCount('reconnected') >= 1, undefined, { timeout: 25000 });
const reconnected = Date.now() - t0;
await new Promise((r) => setTimeout(r, 6000));
clearInterval(timer);

const longestGap = (key) => {
  let best = 0, last = samples[0];
  for (const s of samples) {
    if (s[key] !== last[key]) { best = Math.max(best, s.t - last.t); last = s; }
  }
  return best;
};
const fps = (key, from, to) => {
  const w = samples.filter((s) => s.t >= from && s.t <= to);
  return w.length < 2 ? 0 : ((w.at(-1)[key] - w[0][key]) / ((w.at(-1).t - w[0].t) / 1000)).toFixed(1);
};
console.log(JSON.stringify({
  ws, trigger, reconnected, resumeMs: reconnected - trigger,
  stallMs: { 'alice->bob': longestGap('bob'), 'bob->alice': longestGap('alice') },
  fpsBefore: { 'alice->bob': fps('bob', 0, trigger), 'bob->alice': fps('alice', 0, trigger) },
  fps0to2sAfter: { 'alice->bob': fps('bob', reconnected, reconnected + 2000), 'bob->alice': fps('alice', reconnected, reconnected + 2000) },
  fps2to6sAfter: { 'alice->bob': fps('bob', reconnected + 2000, reconnected + 6000), 'bob->alice': fps('alice', reconnected + 2000, reconnected + 6000) },
}));
await browser.close();
server.close();

// Measures how often a new subscriber's video stalls right after starting, for a low-bitrate track with no
// simulcast. A stall shows as a decoder stuck on its first frame, one packet lost, NACKs nobody answers,
// and recovery only when a PLI brings the next keyframe about three seconds later.
//
//   node sdk-harness/repro-startup-stall.mjs ws://127.0.0.1:7880 [attempts] [streamkit|official]
//
// The last argument is which client publishes. The subscriber is always ours.
import { chromium } from 'playwright-core';
import { buildHarness } from './build.mjs';
import { startFixtureServer } from './fixture.mjs';

const ws = process.argv[2] ?? 'ws://127.0.0.1:7880';
const attempts = Number(process.argv[3] ?? 10);
const publisher = process.argv[4] ?? 'streamkit';

await buildHarness();
const fixture = await startFixtureServer();
const browser = await chromium.launch({
  executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
  headless: true,
  args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--autoplay-policy=no-user-gesture-required'],
});

async function open(identity, room, page) {
  const ctx = await browser.newContext({ permissions: ['camera', 'microphone'] });
  const p = await ctx.newPage();
  await p.goto(`http://127.0.0.1:${fixture.port}/${page}.html?${new URLSearchParams({ ws, identity, room, publish: '0', mode: 'dual' })}`);
  await p.waitForSelector('#state[data-state="connected"]', { state: 'attached', timeout: 30000 });
  return p;
}

let stalled = 0;
for (let i = 0; i < attempts; i++) {
  const room = `stall-${Date.now()}`;
  const a = await open('alice', room, publisher);
  const b = await open('bob', room, 'streamkit');
  await a.evaluate(async () => {
    const canvas = document.createElement('canvas');
    canvas.width = 320;
    canvas.height = 240;
    const g = canvas.getContext('2d');
    let n = 0;
    setInterval(() => {
      n++;
      g.fillStyle = 'rgb(200,40,40)';
      g.fillRect(0, 0, 320, 240);
      g.fillStyle = 'white';
      g.fillRect(n % 300, 100, 20, 20);
    }, 33);
    await window.harness.room.localParticipant.publishTrack(canvas.captureStream(30).getVideoTracks()[0], { source: 'camera', simulcast: false, name: 'camera' });
  });
  await b.waitForFunction(() => window.harness.subscribedSources('alice').includes('camera'), undefined, { timeout: 15000 });
  await b.waitForFunction(() => window.harness.frameHash(window.harness.publication('alice', 'camera').trackSid) !== null, undefined, { timeout: 15000 });
  const sample = () =>
    b.evaluate(async () => {
      const report = await window.harness.publication('alice', 'camera').track.getRTCStatsReport();
      let r = {};
      report.forEach((s) => {
        if (s.type === 'inbound-rtp') r = s;
      });
      return { decoded: r.framesDecoded, packets: r.packetsReceived, lost: r.packetsLost, nacks: r.nackCount, plis: r.pliCount };
    });
  const first = await sample();
  await new Promise((r) => setTimeout(r, 2000));
  const second = await sample();
  const stall = second.decoded <= first.decoded + 2;
  if (stall) stalled++;
  console.log(`${i}: ${stall ? 'STALL' : 'ok'} ${JSON.stringify(first)} -> ${JSON.stringify(second)}`);
  await a.context().close();
  await b.context().close();
}
console.log(`stalled ${stalled} of ${attempts}`);
await browser.close();
fixture.server.close();

// Measures how long a shared screen takes to reach a viewer, and how sharp it arrives. The publisher's tab shows a
// barcode of the clock beside small text, and is shared with real tab capture through the same call the meeting page
// makes. The viewer reads the barcode from each frame it presents, so the figure runs from drawing to presenting.
// Both sides are on this machine, so their clocks agree, and no real network is involved.
//
//   node sdk-harness/screen-latency.mjs ws://127.0.0.1:7880 [streamkit|official] [seconds] [static|scroll] [publish JSON] [capture JSON]
//
// Run it against the reference server too, on the same load. Compare rows; the absolute numbers move with the machine.
//
// This must never capture the desktop. --use-fake-ui-for-media-stream answers getDisplayMedia with the real screen, so
// it is not used here. The capture is checked to be this browser tab before anything is published, and stopped if not.
import { chromium } from 'playwright-core';
import { buildHarness } from './build.mjs';
import { startFixtureServer } from './fixture.mjs';

const ws = process.argv[2] ?? 'ws://127.0.0.1:7880';
const sdk = process.argv[3] ?? 'streamkit';
const seconds = Number(process.argv[4] ?? 30);
const motion = process.argv[5] ?? 'static';
const publishOptions = process.argv[6] ? JSON.parse(process.argv[6]) : undefined;
const captureOptions = process.argv[7] ? JSON.parse(process.argv[7]) : undefined;

const SOURCE = { width: 1920, height: 1080 };
const BITS = 32;
const BLOCK = 56;
const WARMUP_MS = 8000;

const launch = (extra) =>
  chromium.launch({
    executablePath: process.env.CHROME ?? '/usr/bin/google-chrome',
    headless: true,
    args: ['--autoplay-policy=no-user-gesture-required', '--disable-background-timer-throttling', '--disable-renderer-backgrounding', ...extra],
  });

// Keeps every peer connection reachable for getStats, whichever SDK made it.
const watchConnections = () => {
  const Native = window.RTCPeerConnection;
  window.__pcs = [];
  window.RTCPeerConnection = function (...args) {
    const pc = new Native(...args);
    window.__pcs.push(pc);
    return pc;
  };
  window.RTCPeerConnection.prototype = Native.prototype;
  window.__video = async () => {
    const out = [];
    for (const pc of window.__pcs) {
      const report = await pc.getStats();
      report.forEach((s) => {
        if ((s.type === 'inbound-rtp' || s.type === 'outbound-rtp') && s.kind === 'video') out.push(s);
      });
    }
    return out;
  };
};

function delta(now, before, field) {
  return (now[field] ?? 0) - (before?.[field] ?? 0);
}

function percentile(sorted, p) {
  return sorted.length === 0 ? null : sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))];
}

const avg = (list) => (list.length === 0 ? null : Math.round((list.reduce((a, b) => a + b, 0) / list.length) * 10) / 10);

await buildHarness();
const fixture = await startFixtureServer();
const publisherBrowser = await launch(['--auto-select-tab-capture-source-by-title=LatencySource']);
const viewerBrowser = await launch([]);
const room = `latency-${Date.now()}`;

async function open(browser, identity, viewport, query = {}) {
  const ctx = await browser.newContext({ viewport });
  await ctx.addInitScript(watchConnections);
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error(`${identity}: ${e.message}`));
  await page.goto(`http://127.0.0.1:${fixture.port}/${sdk}.html?${new URLSearchParams({ ws, identity, room, publish: '0', mode: 'dual', ...query })}`);
  await page.waitForSelector('#state[data-state="connected"]', { state: 'attached', timeout: 30000 });
  return page;
}

try {
  const viewer = await open(viewerBrowser, 'viewer', { width: 1280, height: 720 }, { adaptive: '1' });
  const publisher = await open(publisherBrowser, 'sharer', SOURCE);

  await publisher.evaluate(
    ({ BITS, BLOCK, motion }) => {
      document.title = 'LatencySource';
      document.body.style.cssText = 'margin:0;background:#fff;color:#111;overflow:hidden;font:12px/16px monospace';
      const bar = document.createElement('canvas');
      bar.width = BITS * BLOCK;
      bar.height = BLOCK;
      bar.style.cssText = 'position:fixed;left:0;top:0;z-index:2';
      document.body.appendChild(bar);
      const text = document.createElement('div');
      text.style.cssText = `position:absolute;left:0;top:${BLOCK}px;width:1920px;white-space:pre-wrap`;
      const line = 'const screen = shared.withSmallText(everyCharacter, mustStayReadable); // the quick brown fox jumps over the lazy dog 0123456789 ';
      text.textContent = Array.from({ length: 400 }, (_, i) => `${String(i).padStart(4, '0')} ${line}${line}`).join('\n');
      document.body.appendChild(text);
      const g = bar.getContext('2d');
      let top = 0;
      const draw = () => {
        const value = Date.now() & 0x0fffffff;
        let ones = 0;
        for (let i = 0; i < 28; i++) ones += (value >>> i) & 1;
        const word = (value | (ones << 28)) >>> 0;
        for (let i = 0; i < BITS; i++) {
          g.fillStyle = (word >>> i) & 1 ? '#fff' : '#000';
          g.fillRect(i * BLOCK, 0, BLOCK, BLOCK);
        }
        if (motion === 'scroll') {
          top = (top + 2) % 2000;
          text.style.top = `${BLOCK - top}px`;
        }
      };
      draw();
      setInterval(draw, 16);
    },
    { BITS, BLOCK, motion },
  );

  const startedAt = Date.now();
  await publisher.evaluate(
    async ({ capture, publish }) => {
      const stream = await navigator.mediaDevices.getDisplayMedia({ video: capture?.video ?? true, audio: false, preferCurrentTab: true, selfBrowserSurface: 'include' });
      const [track] = stream.getVideoTracks();
      const surface = track.getSettings().displaySurface;
      if (surface !== 'browser') {
        track.stop();
        throw new Error(`refusing to share a ${surface}: only this tab may be captured`);
      }
      if (capture?.contentHint) track.contentHint = capture.contentHint;
      await window.harness.room.localParticipant.publishTrack(track, { source: 'screen_share', name: 'screen', ...publish });
    },
    { capture: captureOptions, publish: publishOptions },
  );
  await viewer.waitForSelector('video[data-source="screen_share"]', { state: 'attached', timeout: 30000 });

  await viewer.evaluate(
    ({ BITS, BLOCK, SOURCE }) => {
      const video = document.querySelector('video[data-source="screen_share"]');
      video.style.cssText = 'width:1000px;height:562px;display:block';
      const W = SOURCE.width / 3;
      const H = SOURCE.height / 3;
      const canvas = document.createElement('canvas');
      canvas.width = W;
      canvas.height = H;
      const g = canvas.getContext('2d', { willReadFrequently: true });
      const crisp = document.createElement('canvas');
      crisp.width = 800;
      crisp.height = 300;
      const c = crisp.getContext('2d', { willReadFrequently: true });
      window.__frames = [];
      window.__sharp = [];
      let lastSharp = 0;
      const onFrame = (_, meta) => {
        const at = Date.now();
        g.drawImage(video, 0, 0, W, H);
        const row = g.getImageData(0, Math.round(BLOCK / 6), W, 1).data;
        let word = 0;
        for (let i = 0; i < BITS; i++) {
          const x = Math.round(((i + 0.5) * BLOCK) / 3);
          if (row[x * 4] > 128) word += 2 ** i;
        }
        const value = word % 2 ** 28;
        let ones = 0;
        for (let i = 0; i < 28; i++) ones += (value >>> i) & 1;
        const good = Math.floor(word / 2 ** 28) === ones % 16;
        window.__frames.push({ at, latency: good ? ((at & 0x0fffffff) - value + 2 ** 28) % 2 ** 28 : null, width: meta.width });
        if (at - lastSharp > 1000) {
          lastSharp = at;
          // How much fine detail survives in a fixed part of the small text, drawn at a fixed size.
          const s = meta.width / SOURCE.width;
          c.drawImage(video, 300 * s, 300 * s, 800 * s, 300 * s, 0, 0, 800, 300);
          const data = c.getImageData(0, 0, 800, 300).data;
          let edge = 0;
          for (let i = 4; i < data.length; i += 4) edge += Math.abs(data[i] - data[i - 4]);
          window.__sharp.push({ at, edge: edge / (data.length / 4) });
        }
        video.requestVideoFrameCallback(onFrame);
      };
      video.requestVideoFrameCallback(onFrame);
    },
    { BITS, BLOCK, SOURCE },
  );

  const inbound = { jitter: [], decode: [], fps: [], kbps: [], freezes: 0, freezeMs: 0, nacks: 0, plis: 0 };
  const outbound = {};
  let prevIn = {};
  let prevOut = {};
  for (let t = 0; t < seconds; t++) {
    await new Promise((r) => setTimeout(r, 1000));
    for (const s of await viewer.evaluate(() => window.__video())) {
      const before = prevIn[s.id];
      prevIn[s.id] = s;
      if (Date.now() - startedAt < WARMUP_MS || !before) continue;
      const emitted = delta(s, before, 'jitterBufferEmittedCount');
      if (emitted > 0) inbound.jitter.push((delta(s, before, 'jitterBufferDelay') / emitted) * 1000);
      const decoded = delta(s, before, 'framesDecoded');
      if (decoded > 0) inbound.decode.push((delta(s, before, 'totalDecodeTime') / decoded) * 1000);
      inbound.fps.push(decoded);
      inbound.kbps.push((delta(s, before, 'bytesReceived') * 8) / 1000);
      inbound.freezes += delta(s, before, 'freezeCount');
      inbound.freezeMs += delta(s, before, 'totalFreezesDuration') * 1000;
      inbound.nacks += delta(s, before, 'nackCount');
      inbound.plis += delta(s, before, 'pliCount');
      inbound.codec = s.decoderImplementation;
    }
    for (const s of await publisher.evaluate(() => window.__video())) {
      const before = prevOut[s.id];
      prevOut[s.id] = s;
      if (Date.now() - startedAt < WARMUP_MS || !before) continue;
      const o = (outbound[s.rid ?? 'only'] ??= { fps: [], width: 0, kbps: [], encode: [], pacer: [], limits: {}, target: 0, impl: '' });
      const encoded = delta(s, before, 'framesEncoded');
      o.fps.push(encoded);
      o.width = s.frameWidth ?? o.width;
      o.target = s.targetBitrate ?? o.target;
      o.impl = s.encoderImplementation ?? o.impl;
      o.kbps.push((delta(s, before, 'bytesSent') * 8) / 1000);
      if (encoded > 0) o.encode.push((delta(s, before, 'totalEncodeTime') / encoded) * 1000);
      const sent = delta(s, before, 'packetsSent');
      if (sent > 0) o.pacer.push((delta(s, before, 'totalPacketSendDelay') / sent) * 1000);
      const limit = s.qualityLimitationReason ?? 'none';
      o.limits[limit] = (o.limits[limit] ?? 0) + 1;
    }
  }

  const { frames, sharp } = await viewer.evaluate(() => ({ frames: window.__frames, sharp: window.__sharp }));
  const settled = frames.filter((f) => f.at - startedAt >= WARMUP_MS);
  const latencies = settled.map((f) => f.latency).filter((l) => l !== null).sort((a, b) => a - b);
  const gaps = settled.slice(1).map((f, i) => f.at - settled[i].at);
  const widths = [];
  for (const f of frames) {
    if (widths.at(-1)?.width !== f.width) widths.push({ s: Math.round((f.at - startedAt) / 100) / 10, width: f.width });
  }
  const span = settled.length > 1 ? (settled.at(-1).at - settled[0].at) / 1000 : 1;

  console.log(`sdk=${sdk} motion=${motion} server=${ws} publish=${JSON.stringify(publishOptions ?? {})} capture=${JSON.stringify(captureOptions ?? {})}`);
  console.log(`first frame ${frames.length ? frames[0].at - startedAt : '-'} ms. Widths over time: ${widths.slice(0, 8).map((w) => `${w.s}s:${w.width}`).join(' ')}`);
  console.log(`latency ms: p50=${percentile(latencies, 0.5)} p90=${percentile(latencies, 0.9)} p99=${percentile(latencies, 0.99)} max=${latencies.at(-1)} readable=${latencies.length}/${settled.length}`);
  console.log(`presented: ${Math.round((settled.length / span) * 10) / 10} fps. Gaps over 250 ms: ${gaps.filter((g) => g > 250).length}, longest ${gaps.length ? Math.max(...gaps) : 0} ms. Sharpness ${avg(sharp.filter((s) => s.at - startedAt >= WARMUP_MS).map((s) => s.edge))}`);
  console.log(`viewer: jitter buffer ${avg(inbound.jitter)} ms, decode ${avg(inbound.decode)} ms, ${avg(inbound.fps)} fps, ${Math.round(avg(inbound.kbps) ?? 0)} kbps, freezes ${inbound.freezes} (${Math.round(inbound.freezeMs)} ms), NACKs ${inbound.nacks}, PLIs ${inbound.plis}, ${inbound.codec ?? ''}`);
  const events = await viewer.evaluate(() => window.harness.events.filter((e) => /stream|mute|unsubscribed/i.test(e.name) && !/data/i.test(e.name)).map((e) => `${e.t}ms ${e.name} ${JSON.stringify(e.detail ?? '')}`));
  if (events.length > 0) console.log(`viewer events: ${events.slice(-8).join('; ')}`);
  for (const [rid, o] of Object.entries(outbound)) {
    console.log(`sender ${rid}: ${o.width}px ${avg(o.fps)} fps ${Math.round(avg(o.kbps) ?? 0)} kbps (target ${Math.round(o.target / 1000)}), encode ${avg(o.encode)} ms, pacer ${avg(o.pacer)} ms, limited by ${JSON.stringify(o.limits)}, ${o.impl}`);
  }
} finally {
  await publisherBrowser.close();
  await viewerBrowser.close();
  fixture.server.close();
}

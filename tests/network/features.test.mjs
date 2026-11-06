// Two features that had code but no browser test: H264 simulcast and screen share audio. Real Chrome, real encoders and
// decoders, fake devices. The servers run in a container on a Docker bridge, one machine. See lib.mjs.
import { after, describe, it } from 'node:test';
import { launchChrome, launchFakeDeviceChrome, measureFrames, nextRoom, onBothServers, openClient, publishCamera, sleep, stopFixture, viewerSummary, waitForSteadyFramesAfter, waitForWidth } from './lib.mjs';

after(stopFixture);

const PLAYING_FPS = 10;

const setQuality = (viewer, identity, source, quality) =>
  viewer.page.evaluate(
    ([who, s, q]) => {
      window.harness.publication(who, s).setVideoQuality(window.harness.VideoQuality[q]);
    },
    [identity, source, quality],
  );

describe('H264 simulcast', () => {
  onBothServers(describe, it, 'H264 in three layers: all are sent, and a viewer moves between them', {
    link: 'no impairment',
    run: async (server) => {
      const room = nextRoom('h264');
      const viewer = await openClient(await launchChrome(), { identity: 'viewer', room, ws: server.ws, query: { adaptive: '0' } });
      const publisher = await openClient(await launchChrome(), { identity: 'publisher', room, ws: server.ws });
      try {
        await measureFrames(viewer);
        await publishCamera(publisher, { publish: { videoCodec: 'h264', simulcast: true } });
        const t0 = Date.now();
        const firstSteady = await waitForSteadyFramesAfter(viewer, t0, 30000, { minFps: PLAYING_FPS });
        await sleep(6000);
        const layers = await publisher.page.evaluate(() => window.harness.outboundStats('camera'));
        const inbound = (await viewer.page.evaluate(() => window.__snapshot())).rows.filter((r) => r.type === 'inbound-rtp' && r.kind === 'video');
        const moves = {};
        for (const [name, quality, width] of [['low', 'LOW', 320], ['medium', 'MEDIUM', 640], ['high', 'HIGH', 1280]]) {
          const at = Date.now();
          await setQuality(viewer, 'publisher', 'camera', quality);
          // A switch only happens on a keyframe of the new layer, so this is the H264 keyframe parsing at work.
          await sleep(6000);
          const summary = await viewerSummary(viewer, at + 3000);
          moves[name] = { want: width, width: summary.usualWidth, fps: summary.fps, latency: summary.p50 };
        }
        return {
          firstSteady,
          layers,
          decoderMime: inbound.map((r) => r.mime),
          decoder: inbound.map((r) => r.decoder),
          moves,
          errors: [...viewer.errors, ...publisher.errors],
        };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.firstSteady !== null, `video starts (got ${m.firstSteady})`);
      c.check(m.layers.length === 3 && m.layers.every((l) => /h264/i.test(l.mimeType)), `three H264 layers are sent (${JSON.stringify(m.layers)})`);
      c.check(m.decoderMime.some((x) => /h264/i.test(x)), `the viewer decodes H264 (${m.decoderMime})`);
      for (const [name, move] of Object.entries(m.moves)) {
        c.check(move.width === move.want, `${name} shows ${move.want} px (got ${move.width})`);
        c.check(move.fps >= PLAYING_FPS, `${name} plays at ${PLAYING_FPS} fps or more (got ${move.fps})`);
      }
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });

  onBothServers(describe, it, 'H264 on a 1 Mbps link: a lower layer plays, and the top comes back', {
    link: "viewer's ports only, server to browser: delay 20ms rate 1mbit limit 60",
    run: async (server) => {
      const room = nextRoom('h264');
      const viewer = await openClient(await launchChrome(), { identity: 'viewer', room, ws: server.ws, query: { adaptive: '0' } });
      const ports = [...(await viewer.mediaPorts()), ...(await server.signalPorts())];
      const publisher = await openClient(await launchChrome(), { identity: 'publisher', room, ws: server.ws });
      try {
        await measureFrames(viewer);
        await server.impair({ down: 'delay 20ms rate 1mbit limit 60', ports });
        const t0 = Date.now();
        // Busier than the other tests' source. Chrome's H264 encoder gets far more out of few changes than its VP8 one does,
        // and at that bitrate the top layer fits a 1 Mbps link and there is nothing to test.
        await publishCamera(publisher, { publish: { videoCodec: 'h264', simulcast: true }, tiles: 80 });
        const firstSteadyMs = await waitForSteadyFramesAfter(viewer, t0, 30000, { minFps: 5 });
        await sleep(Math.max(0, t0 + 30000 - Date.now()));
        const limited = await viewerSummary(viewer, Date.now() - 15000);
        await server.clear();
        const recoveryMs = await waitForWidth(viewer, Date.now(), 1280, 60000);
        return { firstSteadyMs, limited, recoveryMs, errors: [...viewer.errors, ...publisher.errors] };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.firstSteadyMs !== null && m.firstSteadyMs <= 10000, `steady video within 10 s (got ${m.firstSteadyMs})`);
      c.check(m.limited.usualWidth !== null && m.limited.usualWidth < 1280, `a lower layer is chosen (${JSON.stringify(m.limited.widths)})`);
      c.check(m.limited.fps >= 5, `plays at 5 fps or more (got ${m.limited.fps})`);
      c.check(m.recoveryMs !== null && m.recoveryMs <= 30000, `back at 1280 px within 30 s (got ${m.recoveryMs})`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });
});

describe('screen share audio', () => {
  onBothServers(describe, it, 'a screen share with its audio: the track arrives and decodes', {
    link: 'no impairment',
    run: async (server) => {
      const room = nextRoom('screenaudio');
      const viewer = await openClient(await launchChrome(), { identity: 'viewer', room, ws: server.ws, query: { adaptive: '0' } });
      // Fake devices and the fake UI together, so getDisplayMedia returns Chrome's fake screen and fake sound, and no real capture.
      const publisher = await openClient(await launchFakeDeviceChrome(), { identity: 'sharer', room, ws: server.ws });
      try {
        await measureFrames(viewer);
        const local = await publisher.page.evaluate(async () => {
          const pub = await window.harness.room.localParticipant.setScreenShareEnabled(true, { audio: true });
          const lp = window.harness.room.localParticipant;
          const screen = pub?.track?.mediaStreamTrack?.getSettings?.();
          const audio = lp.getTrackPublication('screen_share_audio');
          return {
            surface: screen?.displaySurface,
            deviceId: screen?.deviceId,
            audioPublished: !!audio,
            audioLabel: audio?.track?.mediaStreamTrack?.label,
          };
        });
        await viewer.page.waitForFunction(() => window.harness.subscribedSources('sharer').includes('screen_share_audio'), undefined, { timeout: 20000, polling: 200 });
        const read = async () => (await viewer.page.evaluate(() => window.harness.subscribedStats())).find((r) => r.source === 'screen_share_audio');
        await sleep(3000);
        const before = await read();
        const energyBefore = (await viewer.page.evaluate(() => window.__snapshot())).rows.filter((r) => r.type === 'inbound-rtp' && r.kind === 'audio').reduce((sum, r) => sum + (r.audioEnergy ?? 0), 0);
        await sleep(4000);
        const after = await read();
        const energyAfter = (await viewer.page.evaluate(() => window.__snapshot())).rows.filter((r) => r.type === 'inbound-rtp' && r.kind === 'audio').reduce((sum, r) => sum + (r.audioEnergy ?? 0), 0);
        const screenFrames = await waitForSteadyFramesAfter(viewer, Date.now() - 5000, 15000, { minFps: 5 });
        return {
          local,
          audioPackets: after.packetsReceived - before.packetsReceived,
          samples: after.totalSamplesReceived - before.totalSamplesReceived,
          energy: energyAfter - energyBefore,
          screenFrames,
          errors: [...viewer.errors, ...publisher.errors],
        };
      } finally {
        await viewer.close();
        await publisher.close();
      }
    },
    judge: (m, c) => {
      c.check(m.local.audioPublished, `the sharer published screen audio (${JSON.stringify(m.local)})`);
      c.check(m.audioPackets > 150, `audio packets arrive (got ${m.audioPackets} in 4 s)`);
      c.check(m.samples > 100000, `the viewer's decoder produces samples (got ${m.samples} in 4 s)`);
      // Audio energy is recorded but not judged. Chrome's fake screen audio is silent, so it reads zero on both servers: this
      // shows the track's packets arrive and decode, not that a person would hear anything.
      c.check(m.screenFrames !== null, `the screen itself plays too (${m.screenFrames})`);
      c.check(m.errors.length === 0, `no errors in the page (${m.errors[0] ?? 'none'})`);
    },
  });
});

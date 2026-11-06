// SVC layer selection (GAP-020 to GAP-030), on both servers: a real Chrome publisher, with a subscriber that logs every
// packet's layers from what it receives, and a real Chrome viewer that must decode what it asks for.
//
// One RTP stream holds every layer, and a subscriber's choice is made frame by frame from the dependency descriptor. With
// the client's default L3T3_KEY, a subscriber on one spatial layer gets only that layer's frames, so any spatial change
// waits for a key frame.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { dirname, join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { openBrowser, openClient, reportNumbers, selectedTargets, sleep, startServer, until } from '../lib/targets.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const probeBinary = process.env.PROBE ?? join(here, '../../../bin/transport-probe');
const run = promisify(execFile);
const holdSeconds = Number(process.env.SVC_HOLD_SECONDS ?? 6);

async function watch(server, room, steps) {
  const { stdout } = await run(probeBinary, ['watch', '-url', server.ws, '-room', room, '-steps', steps], { timeout: 180000, maxBuffer: 64 << 20 });
  return JSON.parse(stdout);
}

const hold = `${holdSeconds}s`;
// A request that names no frame rate leaves an earlier limit in place, on both servers, so the last step names one.
const schedule = `LOW:${hold},MEDIUM:${hold},HIGH:${hold},HIGH@5:${hold},HIGH@30:${hold}`;

// Which spatial layers a window's frames came from, ignoring a stray frame at a switch boundary or a key picture, which
// always carries the layers below the top one.
function layersIn(step) {
  const perLayer = {};
  for (const [name, count] of Object.entries(step.frames ?? {})) perLayer[name[1]] = (perLayer[name[1]] ?? 0) + count;
  const total = Object.values(perLayer).reduce((a, b) => a + b, 0);
  return Object.keys(perLayer).filter((k) => perLayer[k] / total > 0.1).sort().join('');
}

const summaryOf = (r) =>
  r.steps.map((s) => ({
    step: s.step,
    layers: layersIn(s),
    kbps: s.kbps,
    masks: Object.keys(s.masks ?? {}).join('/'),
    ts_per_s: Number(s.timestamps_per_second.toFixed(1)),
    gaps: s.sequence_gaps,
    padding: s.padding_packets,
    no_marker: s.timestamps_without_marker,
    extra_markers: s.timestamps_with_several_markers,
    marker_off_top: s.markers_not_on_top_layer,
    dd_errors: s.dd_errors,
    max_ext: s.max_extension_size,
    frames: s.frames,
    first_ms: s.first_ms,
  }));

const assertCleanWire = (r) => {
  for (const s of r.steps) {
    assert.equal(s.dd_errors, 0, `${s.step}: a subscriber could not read ${s.dd_errors} descriptors`);
    assert.equal(s.sequence_gaps, 0, `${s.step}: ${s.sequence_gaps} sequence gaps`);
    assert.equal(s.non_monotonic_frame_numbers, 0, `${s.step}: frame numbers went backwards`);
    assert.ok(s.max_ts_delta_ms < 400, `${s.step}: timestamps jumped by ${s.max_ts_delta_ms} ms`);
  }
};

for (const target of selectedTargets()) {
  describe(`SVC on ${target.name}`, () => {
    let server;
    let env;
    let counter = 0;
    const nextRoom = () => `svc-${process.pid}-${++counter}`;

    before(async () => {
      server = await startServer(target);
      env = await openBrowser();
    });
    after(async () => {
      await env?.close();
      await server?.stop();
    });

    // A publisher sending for long enough that every layer is really there.
    async function publish(room, params) {
      const publisher = await openClient(env, server, 'pub', room, { publish: '1', autosub: '0', backupCodec: '0', ...params });
      await until(publisher, () => window.harness.room.localParticipant.trackPublications.size >= 2, undefined, 20000, 'tracks to publish');
      await sleep(3000);
      return publisher;
    }

    for (const [codec, mode] of [['vp9', 'L3T3_KEY'], ['av1', 'L3T3_KEY']]) {
      it(`${codec} ${mode}: each request gets only its own layer, with the masks the reference sends`, async () => {
        const room = nextRoom();
        const publisher = await publish(room, { videoCodec: codec });
        try {
          const r = await watch(server, room, schedule);
          reportNumbers(`${target.name} ${codec} ${mode}`, summaryOf(r));
          reportNumbers(`${target.name} ${codec} ${mode} descriptors`, { key: r.key_descriptor_hex, plain: r.plain_descriptor_hex });
          assert.equal(r.codec.toLowerCase(), `video/${codec}`);
          assertCleanWire(r);
          const [low, medium, high, slow, highAgain] = r.steps;
          assert.equal(layersIn(low), '0');
          assert.equal(layersIn(medium), '1');
          assert.equal(layersIn(high), '2');
          assert.equal(layersIn(slow), '2');
          assert.deepEqual(Object.keys(low.masks), ['7']);
          assert.deepEqual(Object.keys(medium.masks), ['63']);
          assert.deepEqual(Object.keys(high.masks), ['511']);
          assert.deepEqual(Object.keys(slow.masks), ['73']);
          assert.ok(low.kbps < medium.kbps && medium.kbps < high.kbps, `rates should rise with the layer: ${low.kbps}, ${medium.kbps}, ${high.kbps}`);
          assert.ok(slow.kbps < high.kbps, `the lowest frame rate should cost less: ${slow.kbps} against ${high.kbps}`);
          assert.ok(slow.timestamps_per_second < 8, `asked for 5 fps, got ${slow.timestamps_per_second}`);
          assert.ok(high.timestamps_per_second > 15, `asked for all of it, got ${high.timestamps_per_second} fps`);
          assert.ok(highAgain.timestamps_per_second > 15, `back to all of it, got ${highAgain.timestamps_per_second} fps`);
          for (const s of [low, medium, high, slow]) {
            assert.ok(s.timestamps_without_marker <= 1, `${s.step}: ${s.timestamps_without_marker} timestamps with no marker`);
            assert.ok(s.timestamps_with_several_markers <= 1, `${s.step}: ${s.timestamps_with_several_markers} timestamps with several markers`);
            assert.ok(s.markers_not_on_top_layer <= 1, `${s.step}: ${s.markers_not_on_top_layer} markers below the top layer`);
          }
        } finally {
          await publisher.close();
        }
      });

      it(`${codec} ${mode}: a Chrome viewer decodes what it asks for at each size`, async () => {
        const room = nextRoom();
        const publisher = await publish(room, { videoCodec: codec });
        const viewer = await openClient(env, server, 'view', room);
        try {
          await until(viewer, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the camera subscription');
          const row = () => viewer.page.evaluate(async () => (await window.harness.subscribedStats()).find((r) => r.kind === 'video'));
          const sid = (await row()).sid;
          const setQuality = (q) => viewer.page.evaluate((q) => { const h = window.harness; h.publication('pub', 'camera').setVideoQuality(h.VideoQuality[q]); }, q);
          const steps = [];
          const heights = { LOW: 180, MEDIUM: 360, HIGH: 720 };
          await viewer.page.waitForFunction((id) => window.harness.frameHash(id)?.height > 0, sid, { timeout: 20000, polling: 200 });
          let last = await row();
          for (const q of ['LOW', 'MEDIUM', 'HIGH', 'LOW', 'HIGH']) {
            const asked = Date.now();
            const startFrames = last.framesDecoded;
            await setQuality(q);
            await viewer.page.waitForFunction(([id, height]) => window.harness.frameHash(id)?.height === height, [sid, heights[q]], { timeout: 15000, polling: 50 });
            const tookMs = Date.now() - asked;
            await sleep(2000);
            const now = await row();
            const hash1 = await viewer.page.evaluate((id) => window.harness.frameHash(id), sid);
            await sleep(500);
            const hash2 = await viewer.page.evaluate((id) => window.harness.frameHash(id), sid);
            const sent = await publisher.page.evaluate(() => window.harness.publishedVideo());
            steps.push({ q, tookMs, height: hash2.height, decoded: now.framesDecoded - startFrames, dropped: now.framesDropped - last.framesDropped, freezes: now.freezeCount - last.freezeCount, keyFrames: now.keyFramesDecoded - last.keyFramesDecoded, plis: now.pliCount - last.pliCount, encoderHeight: sent.frameHeight, limitedBy: sent.limitedBy });
            assert.equal(hash2.height, heights[q], `${q}: decoded height ${hash2.height}`);
            assert.ok(now.framesDecoded - startFrames > 15, `${q}: decoded only ${now.framesDecoded - startFrames} frames`);
            assert.notEqual(hash1.hash, hash2.hash, `${q}: the pixels did not change`);
            last = await row();
          }
          reportNumbers(`${target.name} ${codec} ${mode} viewer`, steps);
          const dropped = steps.reduce((n, s) => n + s.dropped, 0);
          const freezes = steps.reduce((n, s) => n + s.freezes, 0);
          assert.ok(dropped <= 5, `${dropped} frames dropped`);
          assert.ok(freezes <= 1, `${freezes} freezes`);
          assert.deepEqual(viewer.errors, []);
        } finally {
          await viewer.close();
          await publisher.close();
        }
      });
    }

    it('vp9 L3T3: lower layers come along, because the upper ones depend on them', async () => {
      const room = nextRoom();
      const publisher = await publish(room, { videoCodec: 'vp9', scalabilityMode: 'L3T3' });
      try {
        const r = await watch(server, room, schedule);
        reportNumbers(`${target.name} vp9 L3T3`, summaryOf(r));
        assertCleanWire(r);
        const [low, medium, high, slow] = r.steps;
        assert.equal(layersIn(low), '0');
        assert.equal(layersIn(medium), '01');
        assert.equal(layersIn(high), '012');
        assert.equal(layersIn(slow), '012');
        assert.deepEqual(Object.keys(low.masks), ['7']);
        assert.deepEqual(Object.keys(medium.masks), ['63']);
        assert.deepEqual(Object.keys(high.masks), ['511']);
        assert.deepEqual(Object.keys(slow.masks), ['73']);
        assert.ok(low.kbps < medium.kbps && medium.kbps < high.kbps, `${low.kbps}, ${medium.kbps}, ${high.kbps}`);
        assert.ok(slow.timestamps_per_second < 8, `asked for 5 fps, got ${slow.timestamps_per_second}`);
        for (const s of [low, medium, high, slow]) {
          assert.ok(s.markers_not_on_top_layer <= 1, `${s.step}: ${s.markers_not_on_top_layer} markers below the top layer`);
        }
      } finally {
        await publisher.close();
      }
    });

    it('vp9 with no dependency descriptor falls back to its own payload descriptor', async () => {
      // Official clients force the extension onto their offers, so it has to come out of the answer they get.
      const stripFromAnswers = `
        (() => {
          const Native = window.RTCPeerConnection;
          window.RTCPeerConnection = class extends Native {
            setRemoteDescription(description) {
              if (description && description.type === 'answer') {
                const sdp = description.sdp.split('\\r\\n').filter((l) => !l.includes('dependency-descriptor')).join('\\r\\n');
                return super.setRemoteDescription({ type: 'answer', sdp });
              }
              return super.setRemoteDescription(description);
            }
          };
        })();
      `;
      const room = nextRoom();
      const publisher = await publish(room, { videoCodec: 'vp9', initScript: stripFromAnswers });
      try {
        const r = await watch(server, room, `LOW:${hold},MEDIUM:${hold},HIGH:${hold}`);
        reportNumbers(`${target.name} vp9 no descriptor`, summaryOf(r));
        assert.equal(r.steps[0].max_extension_size, 0, 'the subscriber should see no descriptor');
        const [low, medium, high] = r.steps;
        assert.equal(layersIn(low), '0');
        assert.equal(layersIn(medium), '01');
        assert.equal(layersIn(high), '012');
        assert.ok(low.kbps < medium.kbps && medium.kbps < high.kbps, `${low.kbps}, ${medium.kbps}, ${high.kbps}`);
        for (const s of r.steps) assert.equal(s.sequence_gaps, 0, `${s.step}: ${s.sequence_gaps} sequence gaps`);
      } finally {
        await publisher.close();
      }
    });

    // Nothing but the publisher's active mask says a layer stopped: one stream goes on either way (GAP-029).
    it('a publisher dropping its top layer moves a subscriber down to what is left', async () => {
      const room = nextRoom();
      const publisher = await publish(room, { videoCodec: 'vp9' });
      try {
        const watching = watch(server, room, `HIGH:8s,HIGH:10s`);
        await sleep(9000);
        await publisher.page.evaluate(async () => {
          const pub = [...window.harness.room.localParticipant.trackPublications.values()].find((p) => p.kind === 'video');
          const sender = pub.track.sender;
          const params = sender.getParameters();
          params.encodings[0].maxBitrate = 150000;
          await sender.setParameters(params);
        });
        const r = await watching;
        reportNumbers(`${target.name} publisher drops a layer`, summaryOf(r));
        const [before, limited] = r.steps;
        assert.equal(layersIn(before), '2', 'all three layers were on, and the top one is what was asked for');
        assert.ok(limited.packets > 0, 'video should keep flowing');
        const masks = Object.keys(limited.masks ?? {}).map(Number);
        assert.ok(masks.some((m) => m !== 511), `the active mask should change: ${masks}`);
        const top = (limited.frames.S2T0 ?? 0) + (limited.frames.S2T1 ?? 0) + (limited.frames.S2T2 ?? 0);
        const rest = Object.values(limited.frames).reduce((a, b) => a + b, 0) - top;
        assert.ok(rest > top, `${top} frames of the dropped layer against ${rest} of the rest`);
      } finally {
        await publisher.close();
      }
    });

    it('a late subscriber, joining between key frames, starts at the next one', async () => {
      const room = nextRoom();
      const publisher = await publish(room, { videoCodec: 'vp9' });
      const first = await openClient(env, server, 'first', room);
      try {
        await until(first, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the first viewer');
        await sleep(3000);
        const late = await openClient(env, server, 'late', room);
        try {
          await until(late, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the late viewer');
          const sid = await late.page.evaluate(() => window.harness.publication('pub', 'camera').trackSid);
          const asked = Date.now();
          await late.page.waitForFunction((id) => window.harness.frameHash(id)?.height > 0, sid, { timeout: 15000, polling: 50 });
          reportNumbers(`${target.name} late viewer`, { firstFrameMs: Date.now() - asked });
          const a = await late.page.evaluate(async () => (await window.harness.subscribedStats()).find((r) => r.kind === 'video'));
          await sleep(2000);
          const b = await late.page.evaluate(async () => (await window.harness.subscribedStats()).find((r) => r.kind === 'video'));
          assert.ok(b.framesDecoded - a.framesDecoded > 15, `decoded only ${b.framesDecoded - a.framesDecoded} frames`);
        } finally {
          await late.close();
        }
      } finally {
        await first.close();
        await publisher.close();
      }
    });
  });
}

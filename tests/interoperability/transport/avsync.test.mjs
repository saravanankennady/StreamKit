// Publisher-clock A/V sync (GAP-031 to GAP-039), on both servers. Two halves have to be right, and neither helps alone:
// sender reports that keep the publisher's clock mapping, and audio and video sharing one MediaStream ID.
//
// The first half is checked exactly, with a Pion publisher whose timestamps and reports are known. The result a viewer
// sees is measured in Chrome, with a flash and a beep, because a browser's statistics report what it believes, and it
// believes wrong reports.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { dirname, join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { apiKey, apiSecret, framesOf, openBrowser, openClient, reportNumbers, selectedTargets, sleep, startServer, until } from '../lib/targets.mjs';
import { mintToken } from '../oracle/fixture-server.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const probeBinary = process.env.PROBE ?? join(here, '../../../bin/transport-probe');
const run = promisify(execFile);
const measureSeconds = Number(process.env.AV_SECONDS ?? 30);

async function probe(args, timeout = 90000) {
  const { stdout } = await run(probeBinary, args, { timeout, maxBuffer: 16 << 20 });
  return JSON.parse(stdout);
}

async function createRoom(server, name, body = {}) {
  const token = mintToken({ apiKey, apiSecret, identity: 'api', room: name, grants: { roomJoin: false, roomAdmin: true, roomCreate: true, roomList: true } });
  const r = await fetch(`${server.http}/twirp/livekit.RoomService/CreateRoom`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify({ name, ...body }),
  });
  assert.equal(r.status, 200, `CreateRoom: ${await r.clone().text()}`);
  return r.json();
}

const median = (values) => {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted.length === 0 ? null : sorted[Math.floor(sorted.length / 2)];
};

// Pairs each flash with the nearest tone. The delay being measured is well under the 1.5 s between flashes.
function pairEvents({ flashes, tones }) {
  const diffs = [];
  for (const f of flashes) {
    let best = null;
    for (const t of tones) if (best === null || Math.abs(f - t) < Math.abs(f - best)) best = t;
    if (best !== null && Math.abs(f - best) < 750) diffs.push(f - best);
  }
  return diffs;
}

for (const target of selectedTargets()) {
  describe(`A/V sync on ${target.name}`, () => {
    let server;
    let env;
    let counter = 0;
    const nextRoom = () => `av-${process.pid}-${++counter}`;
    const measured = {};

    before(async () => {
      server = await startServer(target);
      env = await openBrowser();
    });
    after(async () => {
      await env?.close();
      await server?.stop();
    });

    describe('sender reports', () => {
      it('keep the mapping of a publisher whose video reaches the server 150 ms after capture', async () => {
        const r = await probe(['av', '-url', server.ws, '-room', nextRoom(), '-for', '12s', '-video-delay', '150ms']);
        reportNumbers(`${target.name} sender report error`, {
          audioMs: r.audio.max_abs_error_ms, videoMs: r.video.max_abs_error_ms, audioReports: r.audio.reports, videoReports: r.video.reports,
          intervalMs: r.audio.median_interval_ms, firstOffset: [r.audio.first_ts_offset, r.video.first_ts_offset], cname: r.audio.cnames, mids: r.video.sdes_mids,
        });
        for (const [kind, s] of [['audio', r.audio], ['video', r.video]]) {
          assert.ok(s.packets > 50, `${kind}: only ${s.packets} packets`);
          assert.ok(s.reports >= 2, `${kind}: only ${s.reports} reports`);
          assert.ok(s.max_abs_error_ms < 5, `${kind}: reports are ${s.max_abs_error_ms} ms from the publisher's clock`);
          assert.equal(s.first_ts_offset, 0, `${kind}: the first packet keeps its timestamp`);
          assert.ok(s.median_interval_ms > 2500 && s.median_interval_ms < 3500, `${kind}: interval ${s.median_interval_ms} ms`);
          assert.equal(s.cnames.length, 1, `${kind}: SDES CNAMEs ${s.cnames}`);
          assert.equal(s.cnames[0], s.msid.split(' ')[0], `${kind}: the CNAME is the stream ID`);
          assert.equal(s.sdes_mids.length, 1, `${kind}: SDES mids ${s.sdes_mids}`);
        }
      });

      it('are held back until the publisher has sent one, while media flows', async () => {
        const r = await probe(['av', '-url', server.ws, '-room', nextRoom(), '-for', '11s', '-withhold-sr', '6s']);
        reportNumbers(`${target.name} withheld`, {
          before: [r.audio.reports_before_publisher, r.video.reports_before_publisher],
          firstAfterMs: [r.audio.first_report_after_publisher_ms, r.video.first_report_after_publisher_ms],
          firstPacketMs: [r.audio.first_packet_ms, r.video.first_packet_ms],
        });
        assert.equal(r.audio.reports_before_publisher, 0);
        assert.equal(r.video.reports_before_publisher, 0);
        assert.ok(r.audio.first_packet_ms < 5000, 'audio should flow with no report');
        assert.ok(r.video.first_packet_ms < 5000, 'video should flow with no report');
        assert.ok(r.audio.reports >= 1 && r.video.reports >= 1, 'reports should follow the first one');
      });

      it("follow the publisher's clock even when it is an hour ahead of this machine's", async () => {
        const r = await probe(['av', '-url', server.ws, '-room', nextRoom(), '-for', '9s', '-ntp-offset', '1h']);
        reportNumbers(`${target.name} NTP offset`, { audioS: r.audio.ntp_minus_local_seconds, videoS: r.video.ntp_minus_local_seconds, errorMs: [r.audio.max_abs_error_ms, r.video.max_abs_error_ms] });
        for (const s of [r.audio, r.video]) {
          assert.ok(Math.abs(s.ntp_minus_local_seconds - 3600) < 1, `downstream NTP is ${s.ntp_minus_local_seconds} s from local`);
          assert.ok(s.max_abs_error_ms < 5, `reports are ${s.max_abs_error_ms} ms off`);
        }
      });
    });

    describe('in Chrome', () => {
      // Publishes with the given delay on the given room, and watches it for a while.
      async function measure(label, roomOptions, videoDelayMs) {
        const room = nextRoom();
        await createRoom(server, room, roomOptions);
        const publisher = await openClient(env, server, 'pub', room, { autosub: '0', videoDelayMs: String(videoDelayMs), avInit: '1' });
        const viewer = await openClient(env, server, 'view', room, { avInit: '1' });
        try {
          await publisher.page.evaluate(() => window.avPublish());
          await viewer.page.evaluate((who) => window.avWatch(who), 'pub');
          await until(viewer, () => window.avResult && window.avResult().flashes.length > 0, undefined, 30000, 'the first flash');
          // The browser's correction moves over time, so the last events are what count.
          await sleep(measureSeconds * 1000);
          const events = await viewer.page.evaluate(() => window.avResult());
          const stats = await viewer.page.evaluate(() => window.avStats());
          const diffs = pairEvents(events);
          const last = diffs.slice(-12);
          const offer = framesOf(viewer, 's2c', 'offer').at(-1);
          const msids = offer.sdp.split(/\r?\n/).filter((l) => l.startsWith('a=msid:')).map((l) => l.slice('a=msid:'.length).split(' ')[0]);
          const result = {
            label,
            events: diffs.length,
            medianMs: Math.round(median(diffs) ?? NaN),
            lastMedianMs: Math.round(median(last) ?? NaN),
            lastMinMs: Math.round(Math.min(...last)),
            lastMaxMs: Math.round(Math.max(...last)),
            audioJitterBufferMeanMs: Math.round((1000 * stats.audio.jitterBufferDelay) / stats.audio.jitterBufferEmittedCount),
            audioTargetMeanMs: Math.round((1000 * stats.audio.jitterBufferTargetDelay) / stats.audio.jitterBufferEmittedCount),
            audioMinimumMeanMs: Math.round((1000 * stats.audio.jitterBufferMinimumDelay) / stats.audio.jitterBufferEmittedCount),
            playoutEstimatePresent: [stats.audio.playout !== null && stats.audio.playout !== undefined, stats.video.playout !== null && stats.video.playout !== undefined],
            playoutAudioMinusVideoMs: stats.audio.playout && stats.video.playout ? Math.round(stats.audio.playout - stats.video.playout) : null,
            reportsSeen: [stats.audio.reportsSent ?? 0, stats.video.reportsSent ?? 0],
            distinctStreamIds: new Set(msids).size,
            framesDropped: stats.video.framesDropped,
            freezeCount: stats.video.freezeCount,
          };
          reportNumbers(`${target.name} ${label}`, result);
          assert.ok(diffs.length >= 12, `only ${diffs.length} flash and tone pairs`);
          return result;
        } finally {
          await publisher.close();
          await viewer.close();
        }
      }

      it('with no added delay, as a baseline for the measurement', async () => {
        measured.baseline = await measure('shared, no added delay', { syncStreams: true }, 0);
      });

      it('separate streams leave a 300 ms encoder delay in place', async () => {
        measured.separate = await measure('separate, 300 ms', {}, 300);
        assert.equal(measured.separate.distinctStreamIds, 2, 'each track has its own stream by default');
        assert.deepEqual(measured.separate.playoutEstimatePresent, [false, false], 'Chrome has no playout estimate for separate streams');
      });

      it('a shared stream makes Chrome correct the same delay, by delaying audio', async () => {
        measured.shared = await measure('shared, 300 ms', { syncStreams: true }, 300);
        assert.equal(measured.shared.distinctStreamIds, 1, 'sync_streams should give both tracks one stream');
        assert.deepEqual(measured.shared.playoutEstimatePresent, [true, true], 'Chrome should estimate playout on both');
        assert.ok(measured.shared.audioJitterBufferMeanMs > 100, `audio jitter buffer mean ${measured.shared.audioJitterBufferMeanMs} ms: sync is not acting`);
        const reduction = measured.separate.lastMedianMs - measured.shared.lastMedianMs;
        reportNumbers(`${target.name} reduction`, { ms: reduction });
        assert.ok(reduction >= 100, `sharing a stream took only ${reduction} ms off the offset`);
      });
    });

    describe('across simulcast layer switches', () => {
      it('sender reports stay on one line, with no sequence gaps', async () => {
        const room = nextRoom();
        const publisher = await openClient(env, server, 'pub', room, { publish: '1', autosub: '0' });
        try {
          await until(publisher, () => window.harness.room.localParticipant.trackPublications.size >= 2, undefined, 20000, 'tracks to publish');
          const r = await probe(['watch', '-url', server.ws, '-room', room, '-steps', 'HIGH:8s,LOW:8s,HIGH:8s,MEDIUM:8s']);
          reportNumbers(`${target.name} simulcast switches`, {
            videoSpreadMs: r.video_mapping_spread_ms,
            audioSpreadMs: r.audio_mapping_spread_ms,
            reports: r.video_reports.length,
            gaps: r.steps.map((s) => s.sequence_gaps),
            kbps: r.steps.map((s) => s.kbps),
            maxTsDeltaMs: r.steps.map((s) => s.max_ts_delta_ms),
          });
          assert.ok(r.video_reports.length >= 6, `only ${r.video_reports.length} video reports`);
          assert.ok(r.video_mapping_spread_ms < 10, `the video mapping moved by ${r.video_mapping_spread_ms} ms across switches`);
          // Audio does not switch. Chrome's own audio mapping moves by more than this on its own: 15 ms on the reference.
          assert.ok(r.audio_mapping_spread_ms < 50, `the audio mapping moved by ${r.audio_mapping_spread_ms} ms`);
          for (const s of r.steps) assert.equal(s.sequence_gaps, 0, `${s.step}: ${s.sequence_gaps} sequence gaps`);
          assert.ok(r.steps[1].kbps < r.steps[0].kbps, 'LOW should cost less than HIGH');
        } finally {
          await publisher.close();
        }
      });
    });
  });
}

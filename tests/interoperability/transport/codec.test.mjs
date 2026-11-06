// Codec negotiation (GAP-021): the official client in real Chrome, asking for each codec, on both servers. A browser
// sends whichever codec the answer lists first, so asking for a codec only means something if the server puts it there.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import { framesOf, openBrowser, openClient, reportNumbers, selectedTargets, sleep, startServer, until, videoCodecsOf } from '../lib/targets.mjs';

const SVC = new Set(['vp9', 'av1']);

// Proves the viewer decodes it: the decoder advances, and the pixels on screen change.
async function assertDecodes(viewer, publisher, mime) {
  const row = () =>
    viewer.page.evaluate(async (who) => (await window.harness.subscribedStats()).find((r) => r.identity === who && r.kind === 'video'), publisher);
  await until(viewer, (who) => window.harness.subscribedSources(who).includes('camera'), publisher, 20000, 'the camera subscription');
  const sid = (await row()).sid;
  await until(viewer, (id) => window.harness.frameHash(id) !== null, sid, 20000, 'a first decoded frame');
  await viewer.page.waitForFunction(
    async (who) => ((await window.harness.subscribedStats()).find((r) => r.identity === who && r.kind === 'video')?.framesDecoded ?? 0) > 0,
    publisher,
    { timeout: 20000, polling: 200 },
  );
  const a = await row();
  const hash1 = await viewer.page.evaluate((id) => window.harness.frameHash(id), sid);
  await sleep(2500);
  const b = await row();
  const hash2 = await viewer.page.evaluate((id) => window.harness.frameHash(id), sid);
  assert.equal(b.mimeType?.toLowerCase(), mime, `the viewer's decoder is ${b.mimeType}`);
  assert.ok(b.framesDecoded - a.framesDecoded >= 15, `decoded only ${b.framesDecoded - a.framesDecoded} frames in 2.5 s`);
  assert.ok(hash2.width > 0 && hash2.height > 0, 'decoded frame has no size');
  assert.notEqual(hash1.hash, hash2.hash, 'the pixels did not change');
  return { decodedPerSecond: Math.round((b.framesDecoded - a.framesDecoded) / 2.5), width: hash2.width, height: hash2.height, framesDropped: b.framesDropped };
}

for (const target of selectedTargets()) {
  describe(`codec negotiation on ${target.name}`, () => {
    let env;
    let server;
    let counter = 0;
    const nextRoom = () => `codec-${process.pid}-${++counter}`;

    before(async () => {
      server = await startServer(target);
      env = await openBrowser();
    });
    after(async () => {
      await env?.close();
      await server?.stop();
    });

    for (const codec of ['vp8', 'vp9', 'h264', 'av1']) {
      it(`a client asking for ${codec} publishes ${codec}, and a viewer decodes it`, async () => {
        const mime = `video/${codec}`;
        const room = nextRoom();
        const params = { publish: '1', videoCodec: codec, backupCodec: '0' };
        const publisher = await openClient(env, server, 'pub', room, params);
        const viewer = await openClient(env, server, 'view', room);
        try {
          await viewer.page.waitForFunction(() => window.harness.remoteIdentities().includes('pub'), undefined, { timeout: 20000 });
          // The camera's own section: the last answer lists every video section, and the camera is the only one.
          const answers = framesOf(publisher, 's2c', 'answer');
          assert.ok(answers.length > 0, 'the publisher got no answer');
          const answerCodecs = videoCodecsOf(answers.at(-1).sdp);
          assert.equal(answerCodecs[0]?.toLowerCase(), codec, `the answer lists ${answerCodecs.join(', ')}`);

          await until(publisher, () => window.harness.room.localParticipant.trackPublications.size >= 2, undefined, 20000, 'tracks to publish');
          let sent;
          for (let i = 0; i < 100; i++) {
            sent = await publisher.page.evaluate(() => window.harness.publishedVideo());
            if (sent?.mimeType && sent.framesEncoded > 0) break;
            await sleep(100);
          }
          assert.equal(sent?.mimeType?.toLowerCase(), mime, `Chrome sends ${sent?.mimeType}`);
          if (SVC.has(codec)) {
            assert.deepEqual(sent.scalabilityModes, ['L3T3_KEY'], 'the client keeps its SVC mode');
            assert.deepEqual(sent.rids, [null], 'SVC has one encoding with no RID');
          }

          const decoded = await assertDecodes(viewer, 'pub', mime);
          reportNumbers(`${target.name} ${codec}`, { answer: answerCodecs.slice(0, 3), sent: sent.mimeType, ...decoded });
          assert.deepEqual(publisher.errors, []);
          assert.deepEqual(viewer.errors, []);
        } finally {
          await publisher.close();
          await viewer.close();
        }
      });
    }

    it('a client that names a backup codec, as it does by default, still publishes the one it asked for', async () => {
      const room = nextRoom();
      const publisher = await openClient(env, server, 'pub', room, { publish: '1', videoCodec: 'vp9' });
      const viewer = await openClient(env, server, 'view', room);
      try {
        const add = framesOf(publisher, 'c2s', 'addTrack').find((a) => a.type === 1 /* VIDEO */);
        assert.ok(add.simulcastCodecs.length >= 2, `the client names ${add.simulcastCodecs.map((c) => c.codec)}`);
        const decoded = await assertDecodes(viewer, 'pub', 'video/vp9');
        const sent = await publisher.page.evaluate(() => window.harness.publishedVideo());
        assert.equal(sent.mimeType?.toLowerCase(), 'video/vp9');
        reportNumbers(`${target.name} vp9 with backup`, { named: add.simulcastCodecs.map((c) => c.codec), ...decoded });
      } finally {
        await publisher.close();
        await viewer.close();
      }
    });

    it('with no codec asked for, the client publishes vp8 simulcast', async () => {
      const room = nextRoom();
      const publisher = await openClient(env, server, 'pub', room, { publish: '1' });
      const viewer = await openClient(env, server, 'view', room);
      try {
        const decoded = await assertDecodes(viewer, 'pub', 'video/vp8');
        const sent = await publisher.page.evaluate(() => window.harness.publishedVideo());
        assert.equal(sent.mimeType?.toLowerCase(), 'video/vp8');
        reportNumbers(`${target.name} default`, { sent: sent.mimeType, rids: sent.rids, ...decoded });
      } finally {
        await publisher.close();
        await viewer.close();
      }
    });

    it('track_published describes an SVC track as the reference does', async () => {
      const room = nextRoom();
      const publisher = await openClient(env, server, 'pub', room, { publish: '1', videoCodec: 'vp9', backupCodec: '0' });
      try {
        await until(publisher, () => window.harness.room.localParticipant.trackPublications.size >= 2, undefined, 20000, 'tracks to publish');
        const published = framesOf(publisher, 's2c', 'trackPublished').find((t) => t.track.type === 1 /* VIDEO */);
        assert.ok(published, 'no track_published for the camera');
        const info = published.track;
        assert.equal(info.simulcast, false, 'an SVC track is not simulcast');
        assert.equal(info.codecs[0].videoLayerMode, 2 /* MULTIPLE_SPATIAL_LAYERS_PER_STREAM */);
        assert.equal(info.codecs[0].mimeType.toLowerCase(), 'video/vp9');
        assert.deepEqual(info.layers.map((l) => l.rid), ['', '', '']);
        assert.deepEqual(info.layers.map((l) => l.height), [180, 360, 720], 'layers in ascending order');
      } finally {
        await publisher.close();
      }
    });
  });
}

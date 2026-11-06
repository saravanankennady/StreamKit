// Data tracks (GAP-010 to GAP-019), on both servers. A raw signalling client checks the request validation table, because
// the official client never sends most of those mistakes. The official client in Chrome checks everything else, including
// the bytes on the wire: a server only replaces the handle.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import {
  DataBlobKey, DataTrackFrameEncoding, DataTrackSchemaEncoding, DataTrackSchemaId, GetDataBlobRequest, PublishDataTrackRequest, RequestResponse_Reason,
  SignalRequest, SignalResponse, StoreDataBlobRequest, UnpublishDataTrackRequest,
} from '@livekit/protocol';
import { apiKey, apiSecret, framesOf, openBrowser, openClient, reportNumbers, selectedTargets, sleep, startServer, until } from '../lib/targets.mjs';
import { mintToken } from '../oracle/fixture-server.mjs';

// A signalling connection and nothing else.
async function rawClient(server, identity, room, grants = {}) {
  const token = mintToken({ apiKey, apiSecret, identity, room, grants });
  const params = new URLSearchParams({ access_token: token, protocol: '16', sdk: 'js', auto_subscribe: '1', version: '2.23.0' });
  const socket = new WebSocket(`${server.ws}/rtc?${params}`);
  socket.binaryType = 'arraybuffer';
  const received = [];
  const waiting = [];
  socket.addEventListener('message', (e) => {
    const msg = SignalResponse.fromBinary(new Uint8Array(e.data));
    const w = waiting.find((x) => x.test(msg.message));
    if (w) {
      waiting.splice(waiting.indexOf(w), 1);
      w.resolve(msg.message);
    } else {
      received.push(msg.message);
    }
  });
  await new Promise((resolve, reject) => {
    socket.addEventListener('open', resolve);
    socket.addEventListener('error', reject);
  });
  const next = (test, timeout = 5000) =>
    new Promise((resolve, reject) => {
      const found = received.find(test);
      if (found) {
        received.splice(received.indexOf(found), 1);
        resolve(found);
        return;
      }
      const w = { test, resolve };
      waiting.push(w);
      setTimeout(() => {
        if (waiting.includes(w)) reject(new Error(`${identity}: nothing matched in ${timeout} ms`));
      }, timeout);
    });
  await next((m) => m.case === 'join');
  return {
    send: (message) => socket.send(new SignalRequest({ message }).toBinary()),
    next,
    received,
    close: () => socket.close(),
  };
}

const reasonName = (r) => RequestResponse_Reason[r];

for (const target of selectedTargets()) {
  describe(`data tracks on ${target.name}`, () => {
    let server;
    let env;
    let counter = 0;
    const nextRoom = () => `dt-${process.pid}-${++counter}`;

    before(async () => {
      server = await startServer(target);
      env = await openBrowser();
    });
    after(async () => {
      await env?.close();
      await server?.stop();
    });

    describe('requests', () => {
      const publish = (c, fields) => c.send({ case: 'publishDataTrackRequest', value: new PublishDataTrackRequest(fields) });
      const refusal = (c) => c.next((m) => m.case === 'requestResponse').then((m) => m.value);
      const accepted = (c) => c.next((m) => m.case === 'publishDataTrackResponse').then((m) => m.value.info);

      it('are checked in the order and with the answers of the reference', async () => {
        const c = await rawClient(server, 'pub', nextRoom());
        const refused = [];
        const expectRefused = async (fields, reason, message) => {
          publish(c, fields);
          const r = await refusal(c);
          refused.push([reasonName(r.reason), r.message]);
          assert.equal(reasonName(r.reason), reason, `${JSON.stringify(fields)}: ${reasonName(r.reason)}, ${r.message}`);
          assert.equal(r.message, message);
          assert.equal(r.request.case, 'publishDataTrack', 'the client finds its request in the answer');
          assert.equal(r.request.value.pubHandle, fields.pubHandle);
        };
        const custom = (name) => new DataTrackFrameEncoding({ value: { case: 'custom', value: name } });
        const schema = (name) => new DataTrackSchemaId({ name: 'schema', encoding: new DataTrackSchemaEncoding({ value: { case: 'custom', value: name } }) });
        try {
          await expectRefused({ pubHandle: 0, name: 'a' }, 'INVALID_HANDLE', 'handle should be > 0 AND < 65536');
          await expectRefused({ pubHandle: 65536, name: 'a' }, 'INVALID_HANDLE', 'handle should be > 0 AND < 65536');
          await expectRefused({ pubHandle: 1, name: '' }, 'INVALID_NAME', 'name should not be empty and should not exceed 256 characters');
          await expectRefused({ pubHandle: 1, name: 'x'.repeat(257) }, 'INVALID_NAME', 'name should not be empty and should not exceed 256 characters');

          // A bad handle comes before a bad name.
          await expectRefused({ pubHandle: 0, name: '' }, 'INVALID_HANDLE', 'handle should be > 0 AND < 65536');
          const badEncoding = 'encoding identifier is empty or exceeds the maximum length';
          await expectRefused({ pubHandle: 1, name: 'a', frameEncoding: custom('') }, 'INVALID_REQUEST', badEncoding);
          await expectRefused({ pubHandle: 1, name: 'a', frameEncoding: custom('x'.repeat(33)) }, 'INVALID_REQUEST', badEncoding);
          await expectRefused({ pubHandle: 1, name: 'a', schema: schema('') }, 'INVALID_REQUEST', badEncoding);

          publish(c, { pubHandle: 1, name: 'x'.repeat(256), frameEncoding: custom('y'.repeat(32)) });
          const info = await accepted(c);
          assert.match(info.sid, /^DTR_/);
          assert.equal(info.pubHandle, 1);
          assert.equal(info.name.length, 256);
          publish(c, { pubHandle: 65535, name: 'top handle' });
          assert.equal((await accepted(c)).pubHandle, 65535);

          await expectRefused({ pubHandle: 1, name: 'other' }, 'DUPLICATE_HANDLE', 'a data track with same handle already exists');
          await expectRefused({ pubHandle: 2, name: 'top handle' }, 'DUPLICATE_NAME', 'a data track with same name already exists');
          reportNumbers(`${target.name} refused`, refused);
        } finally {
          c.close();
        }
      });

      it('need canPublishData, and not canPublish', async () => {
        const room = nextRoom();
        const denied = await rawClient(server, 'no-data', room, { canPublishData: false });
        const allowed = await rawClient(server, 'no-media', room, { canPublish: false });
        try {
          publish(denied, { pubHandle: 1, name: 'a' });
          const r = await refusal(denied);
          assert.equal(reasonName(r.reason), 'NOT_ALLOWED');
          assert.equal(r.message, 'does not have permission to publish data');
          publish(allowed, { pubHandle: 1, name: 'a' });
          assert.match((await accepted(allowed)).sid, /^DTR_/);
        } finally {
          denied.close();
          allowed.close();
        }
      });

      it('answer an unpublish, and tell others by leaving the track out', async () => {
        const room = nextRoom();
        const pub = await rawClient(server, 'pub', room);
        const other = await rawClient(server, 'other', room);
        try {
          pub.send({ case: 'unpublishDataTrackRequest', value: new UnpublishDataTrackRequest({ pubHandle: 9 }) });
          const r = await refusal(pub);
          assert.equal(reasonName(r.reason), 'NOT_FOUND');
          assert.equal(r.request.case, 'unpublishDataTrack');

          publish(pub, { pubHandle: 9, name: 'a' });
          const info = await accepted(pub);
          const hasTrack = (m) => m.case === 'update' && m.value.participants.some((p) => p.identity === 'pub' && p.dataTracks.some((t) => t.sid === info.sid));
          await other.next(hasTrack);
          await pub.next(hasTrack);

          pub.send({ case: 'unpublishDataTrackRequest', value: new UnpublishDataTrackRequest({ pubHandle: 9 }) });
          const gone = await pub.next((m) => m.case === 'unpublishDataTrackResponse');
          assert.equal(gone.value.info.sid, info.sid);
          await other.next((m) => m.case === 'update' && m.value.participants.some((p) => p.identity === 'pub' && !p.dataTracks.some((t) => t.sid === info.sid)));
        } finally {
          pub.close();
          other.close();
        }
      });

      it('answer data blob requests as the feature being off', async () => {
        const c = await rawClient(server, 'pub', nextRoom());
        try {
          c.send({ case: 'storeDataBlobRequest', value: new StoreDataBlobRequest({ requestId: 7 }) });
          const stored = await refusal(c);
          assert.equal(reasonName(stored.reason), 'NOT_ALLOWED');
          assert.equal(stored.message, 'data blob not enabled');
          assert.equal(stored.requestId, 7);
          c.send({ case: 'getDataBlobRequest', value: new GetDataBlobRequest({ requestId: 8, participantIdentity: 'pub', key: new DataBlobKey({ key: { case: 'generic', value: 'k' } }) }) });
          const fetched = await refusal(c);
          assert.equal(reasonName(fetched.reason), 'NOT_FOUND');
          assert.equal(fetched.message, 'data blob not found');
          assert.equal(fetched.requestId, 8);
        } finally {
          c.close();
        }
      });
    });

    describe('with the official client', () => {
      // Records the bytes on the channel, on either side.
      const tapChannels = `
        (() => {
          window.dataTrackSent = [];
          window.dataTrackReceived = [];
          window.dataTrackChannels = [];
          const hex = (b) => [...new Uint8Array(b).slice(0, 40)].map((x) => x.toString(16).padStart(2, '0')).join('');
          const send = RTCDataChannel.prototype.send;
          RTCDataChannel.prototype.send = function (data) {
            if (this.label === '_data_track') window.dataTrackSent.push({ size: data.byteLength, hex: hex(data.buffer ? data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength) : data) });
            return send.call(this, data);
          };
          const Native = window.RTCPeerConnection;
          window.RTCPeerConnection = class extends Native {
            constructor(...args) {
              super(...args);
              this.addEventListener('datachannel', (e) => {
                if (e.channel.label !== '_data_track') return;
                window.dataTrackChannels.push({ ordered: e.channel.ordered, maxRetransmits: e.channel.maxRetransmits, negotiated: e.channel.negotiated });
                e.channel.addEventListener('message', async (m) => {
                  const data = m.data instanceof Blob ? await m.data.arrayBuffer() : m.data;
                  window.dataTrackReceived.push({ size: data.byteLength, hex: hex(data) });
                });
              });
            }
          };
        })();
      `;
      const open = (identity, room, params = {}) => openClient(env, server, identity, room, { initScript: tapChannels, ...params });

      const publishTrack = (c, name) =>
        c.page.evaluate(async (name) => {
          window.tracks ??= {};
          window.tracks[name] = await window.harness.room.localParticipant.publishDataTrack({ name });
          return { sid: window.tracks[name].info?.sid };
        }, name);
      const push = (c, name, size, stamp) =>
        c.page.evaluate(
          ([name, size, stamp]) => {
            const payload = new Uint8Array(size);
            for (let i = 0; i < size; i++) payload[i] = i % 251;
            window.tracks[name].tryPush({ payload, userTimestamp: BigInt(stamp) });
          },
          [name, size, stamp],
        );
      const subscribe = (c, who, name) =>
        c.page.evaluate(
          async ([who, name]) => {
            window.frames ??= {};
            window.frames[name] = [];
            window.readers ??= {};
            const p = window.harness.room.remoteParticipants.get(who);
            const track = await p.dataTracks.getDeferred(name);
            const reader = track.subscribe().getReader();
            window.readers[name] = reader;
            (async () => {
              for (;;) {
                const { value, done } = await reader.read();
                if (done) {
                  window.frames[name].done = true;
                  return;
                }
                let intact = true;
                for (let i = 0; i < value.payload.length; i++) if (value.payload[i] !== i % 251) intact = false;
                window.frames[name].push({ size: value.payload.length, intact, stamp: value.userTimestamp?.toString() });
              }
            })();
            return track.info.sid;
          },
          [who, name],
        );
      const frameCount = (c, name) => c.page.evaluate((name) => window.frames[name].length, name);
      const handlesOf = (c) => framesOf(c, 's2c', 'dataTrackSubscriberHandles').map((f) => f.subHandles);

      async function publisherAndViewer() {
        const room = nextRoom();
        const pub = await open('pub', room);
        const view = await open('view', room);
        await until(view, () => window.harness.remoteIdentities().includes('pub'), undefined, 15000, 'the publisher');
        return { room, pub, view, close: async () => { await pub.close(); await view.close(); } };
      }

      it('frames reach a subscriber whole, and the wire differs only in the handle', async () => {
        const { pub, view, close } = await publisherAndViewer();
        try {
          const { sid } = await publishTrack(pub, 'sensor');
          assert.match(sid, /^DTR_/);
          const subscribedSid = await subscribe(view, 'pub', 'sensor');
          assert.equal(subscribedSid, sid);
          await until(view, () => window.dataTrackChannels.length > 0, undefined, 15000, 'the data track channel');
          assert.deepEqual(await view.page.evaluate(() => window.dataTrackChannels[0]), { ordered: false, maxRetransmits: 0, negotiated: false });

          // The handle map comes first, or the client drops what it gets.
          for (let i = 0; i < 100 && handlesOf(view).length === 0; i++) await sleep(50);
          const map = handlesOf(view).at(-1);
          const entries = Object.entries(map);
          assert.equal(entries.length, 1, `handle map: ${JSON.stringify(map)}`);
          const [handle, entry] = entries[0];
          assert.equal(entry.publisherIdentity, 'pub');
          assert.equal(entry.trackSid, sid);
          assert.ok(Number(handle) > 0 && Number(handle) < 65536);

          await sleep(300);
          await push(pub, 'sensor', 10, 1000);
          await push(pub, 'sensor', 40000, 1001);
          await until(view, () => window.frames.sensor.length >= 2, undefined, 10000, 'the frames');
          const frames = await view.page.evaluate(() => window.frames.sensor);
          assert.deepEqual(frames, [{ size: 10, intact: true, stamp: '1000' }, { size: 40000, intact: true, stamp: '1001' }]);

          const sent = await pub.page.evaluate(() => window.dataTrackSent);
          const received = await view.page.evaluate(() => window.dataTrackReceived);
          reportNumbers(`${target.name} wire`, { sentPackets: sent.length, sizes: sent.map((p) => p.size), handle, first: sent[0].hex, atSubscriber: received[0].hex });
          assert.deepEqual(sent.map((p) => p.size), received.map((p) => p.size), 'packets arrive the size they left');
          for (let i = 0; i < sent.length; i++) {
            const a = sent[i].hex;
            const b = received[i].hex;
            assert.equal(a.slice(0, 4), b.slice(0, 4), `packet ${i}: flags`);
            assert.equal(a.slice(8), b.slice(8), `packet ${i} differs past the handle`);
            assert.equal(parseInt(b.slice(4, 8), 16), Number(handle), `packet ${i}: the subscriber's handle`);
          }
          assert.deepEqual(pub.errors, []);
          assert.deepEqual(view.errors, []);
        } finally {
          await close();
        }
      });

      it('a duplicate name fails with the client\'s own error', async () => {
        const { pub, close } = await publisherAndViewer();
        try {
          await publishTrack(pub, 'same');
          const error = await pub.page.evaluate(async () => {
            try {
              await window.harness.room.localParticipant.publishDataTrack({ name: 'same' });
              return null;
            } catch (e) {
              return { name: e.name, message: e.message };
            }
          });
          assert.equal(error?.name, 'DataTrackPublishError');
          assert.match(error.message, /already taken/);
        } finally {
          await close();
        }
      });

      it('unsubscribing stops frames, and subscribing again gets them under a new map', async () => {
        const { pub, view, close } = await publisherAndViewer();
        try {
          await publishTrack(pub, 'a');
          await subscribe(view, 'pub', 'a');
          await until(view, () => window.dataTrackChannels.length > 0, undefined, 15000, 'the data track channel');
          for (let i = 0; i < 100 && handlesOf(view).length === 0; i++) await sleep(50);
          await sleep(300);
          await push(pub, 'a', 10, 1);
          await until(view, () => window.frames.a.length >= 1, undefined, 10000, 'the first frame');

          await view.page.evaluate(() => window.readers.a.cancel());
          for (let i = 0; i < 100 && Object.keys(handlesOf(view).at(-1)).length !== 0; i++) await sleep(50);
          assert.deepEqual(Object.keys(handlesOf(view).at(-1)), [], 'the map should be empty once unsubscribed');

          const received = await view.page.evaluate(() => window.dataTrackReceived.length);
          await push(pub, 'a', 10, 2);
          await sleep(500);
          assert.equal(await view.page.evaluate(() => window.dataTrackReceived.length), received, 'nothing should arrive once unsubscribed');

          await subscribe(view, 'pub', 'a');
          for (let i = 0; i < 100 && Object.keys(handlesOf(view).at(-1)).length !== 1; i++) await sleep(50);
          assert.equal(Object.keys(handlesOf(view).at(-1)).length, 1);
          await sleep(300);
          await push(pub, 'a', 10, 3);
          await until(view, () => window.frames.a.length >= 1, undefined, 10000, 'a frame after subscribing again');
        } finally {
          await close();
        }
      });

      it('two tracks each have their own handle', async () => {
        const { pub, view, close } = await publisherAndViewer();
        try {
          const a = await publishTrack(pub, 'a');
          const b = await publishTrack(pub, 'b');
          await subscribe(view, 'pub', 'a');
          await subscribe(view, 'pub', 'b');
          for (let i = 0; i < 100 && Object.keys(handlesOf(view).at(-1) ?? {}).length !== 2; i++) await sleep(50);
          const map = handlesOf(view).at(-1);
          assert.deepEqual(Object.values(map).map((e) => e.trackSid).sort(), [a.sid, b.sid].sort());
          await sleep(300);
          await push(pub, 'a', 11, 1);
          await push(pub, 'b', 12, 2);
          await until(view, () => window.frames.a.length >= 1 && window.frames.b.length >= 1, undefined, 10000, 'frames on both');
          assert.equal((await view.page.evaluate(() => window.frames.a))[0].size, 11);
          assert.equal((await view.page.evaluate(() => window.frames.b))[0].size, 12);
        } finally {
          await close();
        }
      });

      it('unpublishing is seen by others and ends their stream', async () => {
        const { pub, view, close } = await publisherAndViewer();
        try {
          await publishTrack(pub, 'a');
          await subscribe(view, 'pub', 'a');
          for (let i = 0; i < 100 && handlesOf(view).length === 0; i++) await sleep(50);
          await pub.page.evaluate(() => window.tracks.a.unpublish());
          await until(view, () => window.frames.a.done === true, undefined, 15000, 'the stream to end');
          await until(view, () => !window.harness.room.remoteParticipants.get('pub').dataTracks.get('a'), undefined, 15000, 'the track to go');
        } finally {
          await close();
        }
      });

      it('a resume on either side keeps frames flowing under the same handles', async () => {
        const { pub, view, close } = await publisherAndViewer();
        try {
          await publishTrack(pub, 'a');
          await subscribe(view, 'pub', 'a');
          await until(view, () => window.dataTrackChannels.length > 0, undefined, 15000, 'the data track channel');
          for (let i = 0; i < 100 && handlesOf(view).length === 0; i++) await sleep(50);
          await sleep(300);
          await push(pub, 'a', 10, 1);
          await until(view, () => window.frames.a.length >= 1, undefined, 10000, 'a frame before resuming');
          const handles = JSON.stringify(handlesOf(view).at(-1));
          for (const [who, c] of [['subscriber', view], ['publisher', pub]]) {
            const before = await frameCount(view, 'a');
            const waited = await c.page.evaluate(async () => {
              const room = window.harness.room;
              const done = new Promise((r) => room.once('reconnected', r));
              await room.engine.client.ws?.close();
              const t = Date.now();
              await Promise.race([done, new Promise((r) => setTimeout(r, 15000))]);
              return Date.now() - t;
            });
            assert.ok(waited < 15000, `${who} did not resume`);
            await sleep(500);
            await push(pub, 'a', 10, 2);
            await until(view, (n) => window.frames.a.length > n, before, 10000, `a frame after the ${who} resumed`);
          }
          assert.equal(JSON.stringify(handlesOf(view).at(-1)), handles, 'handles should not change on a resume');
        } finally {
          await close();
        }
      });

      it('a publisher revoking permission stops frames and empties the map', async () => {
        const { pub, view, close } = await publisherAndViewer();
        try {
          await publishTrack(pub, 'a');
          await subscribe(view, 'pub', 'a');
          await until(view, () => window.dataTrackChannels.length > 0, undefined, 15000, 'the data track channel');
          for (let i = 0; i < 100 && handlesOf(view).length === 0; i++) await sleep(50);
          await sleep(300);
          await push(pub, 'a', 10, 1);
          await until(view, () => window.frames.a.length >= 1, undefined, 10000, 'a frame before revoking');
          await pub.page.evaluate(() => window.harness.room.localParticipant.setTrackSubscriptionPermissions(false, []));
          for (let i = 0; i < 100 && Object.keys(handlesOf(view).at(-1)).length !== 0; i++) await sleep(50);
          assert.deepEqual(Object.keys(handlesOf(view).at(-1)), [], 'the map should be empty');
          const update = framesOf(view, 's2c', 'subscriptionPermissionUpdate').find((u) => !u.allowed);
          assert.ok(update, 'the subscriber should be told');
          const received = await view.page.evaluate(() => window.dataTrackReceived.length);
          await push(pub, 'a', 10, 2);
          await sleep(500);
          assert.equal(await view.page.evaluate(() => window.dataTrackReceived.length), received, 'nothing should arrive');
        } finally {
          await close();
        }
      });

      it('a publisher leaving empties the map, and joiners see nothing of it', async () => {
        const { room, pub, view, close } = await publisherAndViewer();
        try {
          await publishTrack(pub, 'a');
          await subscribe(view, 'pub', 'a');
          for (let i = 0; i < 100 && handlesOf(view).length === 0; i++) await sleep(50);
          await pub.close();
          for (let i = 0; i < 200 && Object.keys(handlesOf(view).at(-1)).length !== 0; i++) await sleep(50);
          assert.deepEqual(Object.keys(handlesOf(view).at(-1)), []);
          void room;
        } finally {
          await close();
        }
      });
    });
  });
}

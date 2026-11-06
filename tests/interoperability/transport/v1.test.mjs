// The single peer connection protocol, /rtc/v1 (GAP-001 to GAP-009), on both servers. One connection carries media both
// ways, and the client makes every offer: a server attaches a subscribed track to a section the client already offered, and
// asks it to offer again.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import { apiKey, apiSecret, framesOf, openBrowser, openClient, reportNumbers, selectedTargets, sleep, startServer, until } from '../lib/targets.mjs';
import { mintToken } from '../oracle/fixture-server.mjs';

const singlePC = { ours: { rtc: { single_peer_connection: true } } };

// Counts connections and watches for the browser being handed a channel it did not make.
const watchConnections = `
  (() => {
    window.pcs = [];
    window.remoteChannels = [];
    const Native = window.RTCPeerConnection;
    window.RTCPeerConnection = class extends Native {
      constructor(...args) {
        super(...args);
        window.pcs.push(this);
        this.addEventListener('datachannel', (e) => window.remoteChannels.push(e.channel.label));
      }
    };
  })();
`;

for (const target of selectedTargets()) {
  describe(`/rtc/v1 on ${target.name}`, () => {
    let server;
    let env;
    let counter = 0;
    const nextRoom = () => `v1-${process.pid}-${++counter}`;
    const open = (identity, room, params = {}) => openClient(env, server, identity, room, { mode: 'v1', initScript: watchConnections, ...params });

    before(async () => {
      server = await startServer(target, singlePC);
      env = await openBrowser();
    });
    after(async () => {
      await env?.close();
      await server?.stop();
    });

    const decoding = async (viewer, sid, what) => {
      const row = () => viewer.page.evaluate(async (id) => (await window.harness.subscribedStats()).find((r) => r.sid === id), sid);
      await viewer.page.waitForFunction((id) => window.harness.frameHash(id)?.width > 0, sid, { timeout: 20000, polling: 100 }).catch((e) => {
        throw new Error(`${what}: no decoded frame: ${e.message}`);
      });
      const a = await row();
      const hash1 = await viewer.page.evaluate((id) => window.harness.frameHash(id), sid);
      await sleep(2000);
      const b = await row();
      const hash2 = await viewer.page.evaluate((id) => window.harness.frameHash(id), sid);
      assert.ok(b.framesDecoded - a.framesDecoded >= 10, `${what}: decoded only ${b.framesDecoded - a.framesDecoded} frames`);
      assert.notEqual(hash1.hash, hash2.hash, `${what}: the pixels did not change`);
    };

    describe('validate', () => {
      const token = (room) => mintToken({ apiKey, apiSecret, identity: 'v', room });
      const url = (room, params = {}) => `${server.http}/rtc/v1/validate?${new URLSearchParams({ access_token: token(room), ...params })}`;

      it('needs a join request, and rejects a bad one', async () => {
        const room = nextRoom();
        const missing = await fetch(url(room));
        assert.equal(missing.status, 400);
        assert.match(await missing.text(), /join_request is required/);
        assert.equal((await fetch(url(room, { join_request: '!!!' }))).status, 400);
        const empty = await fetch(url(room, { join_request: Buffer.from([]).toString('base64url') + '' }));
        assert.equal(empty.status, 400, 'an empty parameter is a missing one');
      });

      it('accepts a real one, and a bad token is still a bad token', async () => {
        const room = nextRoom();
        // An empty WrappedJoinRequest with no compression, and an empty JoinRequest: 0x08 0x00 says compression NONE.
        const joinRequest = Buffer.from([0x08, 0x00]).toString('base64');
        const good = await fetch(url(room, { join_request: joinRequest }));
        assert.equal(good.status, 200);
        const bad = await fetch(`${server.http}/rtc/v1/validate?${new URLSearchParams({ access_token: 'nonsense', join_request: joinRequest })}`);
        assert.equal(bad.status, 401);
      });
    });

    it('a join is one connection, with the first offer inside it', async () => {
      const room = nextRoom();
      const pub = await open('pub', room, { publish: '1', autosub: '0' });
      try {
        await until(pub, () => window.harness.room.localParticipant.trackPublications.size >= 2, undefined, 20000, 'tracks to publish');
        assert.equal(await pub.page.evaluate(() => window.pcs.length), 1, 'one connection');
        assert.deepEqual(await pub.page.evaluate(() => window.remoteChannels), [], 'no channels from the server');

        const url = pub.frames.find((f) => f.dir === 'open').url;
        assert.match(url, /\/rtc\/v1\?/, 'the client stayed on /rtc/v1');
        const order = pub.frames.filter((f) => f.dir === 's2c' || f.dir === 'c2s').map((f) => `${f.dir} ${f.case}`);
        assert.equal(order[0], 's2c join');
        const join = framesOf(pub, 's2c', 'join')[0];
        assert.equal(join.subscriberPrimary, false, 'nothing about this connection is subscriber first');
        const answers = framesOf(pub, 's2c', 'answer');
        assert.ok(answers.length >= 1, 'no answer');
        assert.equal(answers[0].id, 1, 'the first answer is to the offer in the join');
        assert.equal(framesOf(pub, 's2c', 'offer').length, 0, 'a server makes no offers here');
        assert.equal(framesOf(pub, 'c2s', 'answer').length, 0, 'a client makes no answers here');
        for (const t of framesOf(pub, 's2c', 'trickle')) assert.equal(t.target, 0 /* PUBLISHER */);
        reportNumbers(`${target.name} join`, order.slice(0, 12));

        const offers = framesOf(pub, 'c2s', 'offer');
        assert.ok(offers.length >= 1, 'the client should offer again as tracks publish');
        assert.ok(offers[0].id >= 2, `the next offer is id ${offers[0].id}`);
        for (const offer of offers) {
          const answer = answers.find((a) => a.id === offer.id);
          assert.ok(answer, `no answer to offer ${offer.id}`);
        }
      } finally {
        await pub.close();
      }
    });

    it('a fourth video needs a new section: requirements ask for it, and all of them decode', async () => {
      const room = nextRoom();
      const pub = await open('pub', room, { autosub: '0' });
      const view = await open('view', room);
      try {
        await until(view, () => window.harness.remoteIdentities().includes('pub'), undefined, 15000, 'the publisher');
        const sids = await pub.page.evaluate(async () => {
          const { room, Track } = window.harness;
          const stream = await navigator.mediaDevices.getUserMedia({ video: { width: 320, height: 180 } });
          const out = [];
          for (let i = 0; i < 4; i++) {
            const publication = await room.localParticipant.publishTrack(stream.getVideoTracks()[0].clone(), { name: `v${i}`, source: Track.Source.Unknown, simulcast: false, backupCodec: false, stream: `s${i}` });
            out.push(publication.trackSid);
          }
          return out;
        });
        await until(view, (n) => [...window.harness.room.remoteParticipants.get('pub').trackPublications.values()].filter((p) => p.track).length >= n, 4, 20000, 'all four tracks');
        for (const sid of sids) await decoding(view, sid, `track ${sid}`);

        const requirements = framesOf(view, 's2c', 'mediaSectionsRequirement');
        const asked = requirements.reduce((n, r) => n + r.numVideos, 0);
        assert.equal(asked, 1, `asked for ${asked} new video sections`);
        assert.equal(requirements.reduce((n, r) => n + r.numAudios, 0), 0);
        assert.ok(requirements.length >= 2, `only ${requirements.length} requirements`);

        const last = framesOf(view, 's2c', 'answer').at(-1);
        const map = last.midToTrackId;
        assert.deepEqual(Object.values(map).sort(), [...sids].sort(), 'the answer maps every track');
        assert.deepEqual(Object.keys(map).sort(), ['3', '4', '5', '7'], 'the preallocated video sections, then a new one past the data section');
        reportNumbers(`${target.name} requirements`, { values: requirements.map((r) => [r.numAudios, r.numVideos]), map });
        assert.equal(await view.page.evaluate(() => window.pcs.length), 1);
      } finally {
        await pub.close();
        await view.close();
      }
    });

    // Payload types on this connection are the browser's, on both directions, and they are not the numbers a server registers.
    for (const codec of ['vp9', 'av1', 'h264']) {
      it(`a ${codec} track publishes and decodes, each way on one connection`, async () => {
        const room = nextRoom();
        const pub = await open('pub', room, { publish: '1', videoCodec: codec, backupCodec: '0' });
        const view = await open('view', room, { publish: '1', videoCodec: codec, backupCodec: '0' });
        try {
          for (const [c, other] of [[pub, 'view'], [view, 'pub']]) {
            await until(c, (who) => window.harness.subscribedSources(who).includes('camera'), other, 20000, 'the camera');
            const sid = await c.page.evaluate((who) => window.harness.publication(who, 'camera').trackSid, other);
            await decoding(c, sid, `${c.identity} watching ${other}`);
            const row = await c.page.evaluate(async (id) => (await window.harness.subscribedStats()).find((r) => r.sid === id), sid);
            assert.equal(row.mimeType?.toLowerCase(), `video/${codec}`);
            const sent = await c.page.evaluate(() => window.harness.publishedVideo());
            assert.equal(sent.mimeType?.toLowerCase(), `video/${codec}`);
          }
          // The first answer is before anything has been said about what each section is for, and the last has the lot.
          const answer = framesOf(pub, 's2c', 'answer').at(-1);
          assert.ok(Object.keys(answer.midToTrackId).length >= 2, 'audio and video from the other client');
        } finally {
          await pub.close();
          await view.close();
        }
      });
    }

    it('unsubscribing and subscribing again reuse the section', async () => {
      const room = nextRoom();
      const pub = await open('pub', room, { publish: '1', autosub: '0' });
      const view = await open('view', room);
      try {
        await until(view, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the camera');
        const sid = await view.page.evaluate(() => window.harness.publication('pub', 'camera').trackSid);
        await decoding(view, sid, 'before unsubscribing');
        const midOf = () => {
          const map = framesOf(view, 's2c', 'answer').at(-1).midToTrackId;
          return Object.entries(map).find(([, t]) => t === sid)?.[0];
        };
        const first = midOf();
        assert.ok(first, 'the answer should map the track');

        await view.page.evaluate(() => window.harness.publication('pub', 'camera').setSubscribed(false));
        for (let i = 0; i < 100 && midOf() !== undefined; i++) await sleep(100);
        assert.equal(midOf(), undefined, 'the map should drop the track');
        const answerSdp = framesOf(view, 's2c', 'answer').at(-1).sdp;
        const section = answerSdp.split(/\r?\n(?=m=)/).find((s) => s.includes(`a=mid:${first}\r\n`));
        assert.match(section, /a=inactive/, 'the section it left goes inactive');

        await view.page.evaluate(() => window.harness.publication('pub', 'camera').setSubscribed(true));
        for (let i = 0; i < 100 && midOf() === undefined; i++) await sleep(100);
        assert.equal(midOf(), first, 'the same section should come back');
        await decoding(view, sid, 'after subscribing again');
      } finally {
        await pub.close();
        await view.close();
      }
    });

    it('a resume is the client offering, with no offer from the server', async () => {
      const room = nextRoom();
      const pub = await open('pub', room, { publish: '1', autosub: '0' });
      const view = await open('view', room);
      try {
        await until(view, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the camera');
        const sid = await view.page.evaluate(() => window.harness.publication('pub', 'camera').trackSid);
        await decoding(view, sid, 'before the resume');
        const mark = view.frames.length;
        await view.page.evaluate(() => {
          const room = window.harness.room;
          window.resumed = new Promise((r) => room.once('reconnected', r));
          return room.simulateScenario('signal-reconnect');
        });
        await view.page.evaluate(() => Promise.race([window.resumed, new Promise((_, no) => setTimeout(() => no(new Error('no resume')), 20000))]));
        await sleep(1000);
        const after = view.frames.slice(mark).filter((f) => f.dir === 's2c' || f.dir === 'c2s').map((f) => `${f.dir} ${f.case}`);
        reportNumbers(`${target.name} resume`, after.slice(0, 12));
        assert.ok(after.includes('s2c reconnect'), 'the server answers with reconnect');
        assert.ok(after.includes('c2s syncState'), 'the client sends its state');
        assert.ok(!after.includes('s2c offer'), 'no offer from the server');
        assert.ok(after.includes('c2s offer'), 'the client restarts ICE by offering');
        assert.ok(after.includes('s2c answer'), 'and gets an answer');
        const restart = view.frames.slice(mark).find((f) => f.dir === 's2c' && f.case === 'answer').value;
        assert.ok(Object.values(restart.midToTrackId).includes(sid), 'the answer has the whole map');
        await decoding(view, sid, 'after the resume');
        assert.equal(await view.page.evaluate(() => window.pcs.length), 1);
      } finally {
        await pub.close();
        await view.close();
      }
    });

    it('a full reconnect starts again on a new connection, and media comes back', async () => {
      const room = nextRoom();
      const pub = await open('pub', room, { publish: '1', autosub: '0' });
      const view = await open('view', room);
      try {
        await until(view, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the camera');
        const mark = view.frames.length;
        await view.page.evaluate(() => {
          const room = window.harness.room;
          window.reconnected = new Promise((r) => room.once('reconnected', r));
          return room.simulateScenario('full-reconnect');
        });
        await view.page.evaluate(() => Promise.race([window.reconnected, new Promise((_, no) => setTimeout(() => no(new Error('no full reconnect')), 30000))]));
        await until(view, () => window.harness.subscribedSources('pub').includes('camera'), undefined, 20000, 'the camera again');
        const after = view.frames.slice(mark);
        const firstAnswer = after.find((f) => f.dir === 's2c' && f.case === 'answer');
        assert.ok(firstAnswer, 'no answer after the reconnect');
        assert.equal(firstAnswer.value.id, 1, 'offer IDs start again');
        const sid = await view.page.evaluate(() => window.harness.publication('pub', 'camera').trackSid);
        await decoding(view, sid, 'after the reconnect');
        assert.equal(await view.page.evaluate(() => window.harness.connectionState()), 'connected');
      } finally {
        await pub.close();
        await view.close();
      }
    });

    it('reliable and lossy data go both ways on the channels the client made', async () => {
      const room = nextRoom();
      const a = await open('a', room);
      const b = await open('b', room);
      try {
        await until(a, () => window.harness.remoteIdentities().includes('b'), undefined, 15000, 'the other client');
        await until(b, () => window.harness.remoteIdentities().includes('a'), undefined, 15000, 'the other client');
        await sleep(500);
        const send = (c, text, reliable) => c.page.evaluate(([text, reliable]) => window.harness.room.localParticipant.publishData(new TextEncoder().encode(text), { reliable }), [text, reliable]);
        await send(a, 'from a, reliable', true);
        await send(a, 'from a, lossy', false);
        await send(b, 'from b, reliable', true);
        await send(b, 'from b, lossy', false);
        const got = (c, text) => until(c, (t) => window.harness.received.some((r) => r.text === t), text, 10000, `to get "${text}"`);
        await got(b, 'from a, reliable');
        await got(b, 'from a, lossy');
        await got(a, 'from b, reliable');
        await got(a, 'from b, lossy');
        assert.deepEqual(await a.page.evaluate(() => window.remoteChannels), []);
        assert.equal(await a.page.evaluate(() => window.pcs.length), 1);
      } finally {
        await a.close();
        await b.close();
      }
    });

    it('a data track carries frames on the client\'s own channel', async () => {
      const room = nextRoom();
      const pub = await open('pub', room);
      const view = await open('view', room);
      try {
        await until(view, () => window.harness.remoteIdentities().includes('pub'), undefined, 15000, 'the publisher');
        await pub.page.evaluate(async () => {
          window.track = await window.harness.room.localParticipant.publishDataTrack({ name: 'd' });
        });
        await view.page.evaluate(async () => {
          window.frames = [];
          const track = await window.harness.room.remoteParticipants.get('pub').dataTracks.getDeferred('d');
          const reader = track.subscribe().getReader();
          (async () => {
            for (;;) {
              const { value, done } = await reader.read();
              if (done) return;
              window.frames.push(value.payload.length);
            }
          })();
        });
        for (let i = 0; i < 100 && framesOf(view, 's2c', 'dataTrackSubscriberHandles').length === 0; i++) await sleep(50);
        await sleep(300);
        await pub.page.evaluate(() => window.track.tryPush({ payload: new Uint8Array(40000) }));
        await until(view, () => window.frames.includes(40000), undefined, 10000, 'the frame');
      } finally {
        await pub.close();
        await view.close();
      }
    });

    if (target.name === 'ours') {
      it('with the setting off, the routes are absent and a default client falls back to /rtc', async () => {
        await server.stop();
        const off = await startServer(target, { ours: { rtc: { single_peer_connection: false } } });
        try {
          const token = mintToken({ apiKey, apiSecret, identity: 'v', room: 'off' });
          const r = await fetch(`${off.http}/rtc/v1/validate?${new URLSearchParams({ access_token: token, join_request: Buffer.from([0x08, 0x00]).toString('base64') })}`);
          assert.equal(r.status, 404);
          const room = nextRoom();
          const c = await openClient(env, off, 'c', room, { mode: 'v1', initScript: watchConnections, publish: '1' });
          try {
            const urls = c.frames.filter((f) => f.dir === 'open').map((f) => new URL(f.url).pathname);
            assert.deepEqual(urls.slice(-1), ['/rtc'], `connected to ${urls}`);
            // The one it made for /rtc/v1 is closed by then, and its two for /rtc are in use.
            assert.equal(await c.page.evaluate(() => window.pcs.filter((pc) => pc.connectionState !== 'closed').length), 2);
          } finally {
            await c.close();
          }
        } finally {
          await off.stop();
          server = await startServer(target, singlePC);
        }
      });
    }
  });
}

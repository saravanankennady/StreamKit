// Phones talking to each other through a room: one pjsua plays a tone, another records what it hears, with the worker's G.711, resampling, Opus
// and the SFU in between, in both directions. No browser is involved, so this is the quick way to check a call's whole path. The same
// scenarios run against the reference. Everything is on loopback.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import { SipClient } from 'livekit-server-sdk';
import {
  Account, HOST, OURS, Phone, RawPeer, SDP, assertToneInWav, buildBinaries, invite, makeCertificate, parseSip, prepareDir, referenceAvailable, respondRaw, sipUri, sleep, startOurs, startReference, stopAll, stopProcess, stopReference, toneFile, waitFor, writeConfig,
} from './lib.mjs';

const LOW = 500;
const HIGH = 1500;
let toneLow;
let toneHigh;
let counter = 0;
const nextNumber = () => `+1888${String(100000 + (++counter)).slice(1)}`;
const nextRoom = (tag) => `${tag}-${process.pid}-${++counter}`;
const LOOPBACK = ['127.0.0.1/32'];

class Scene {
  constructor(stack) {
    Object.assign(this, { stack, phones: [], trunks: [], rules: [] });
  }

  phone(name, opts) {
    const p = new Phone(`${name}-${++counter}`, opts);
    this.phones.push(p);
    return p;
  }

  async inboundTrunk(numbers, opts = {}) {
    const t = await this.stack.sip.createSipInboundTrunk('test trunk', numbers, { allowedAddresses: LOOPBACK, ...opts });
    this.trunks.push(t.sipTrunkId);
    return t;
  }

  async outboundTrunk(address, numbers, opts = {}) {
    const t = await this.stack.sip.createSipOutboundTrunk('test trunk', address, numbers, opts);
    this.trunks.push(t.sipTrunkId);
    return t;
  }

  async directRule(room, trunkIds, opts = {}) {
    const r = await this.stack.sip.createSipDispatchRule({ type: 'direct', roomName: room, pin: opts.pin }, { trunkIds, ...opts });
    this.rules.push(r.sipDispatchRuleId);
    return r;
  }

  async done() {
    await Promise.all(this.phones.map((p) => p.stop()));
    await Promise.all(this.trunks.map((id) => this.stack.sip.deleteSipTrunk(id).catch(() => {})));
    await Promise.all(this.rules.map((id) => this.stack.sip.deleteSipDispatchRule(id).catch(() => {})));
  }
}

async function scene(stack, fn) {
  const s = new Scene(stack);
  try {
    await fn(s);
  } finally {
    await s.done();
  }
}

const listSip = async (stack, room) => (await stack.rooms.listParticipants(room).catch(() => [])).filter((p) => p.identity.startsWith('sip_') || p.identity.startsWith('phone-'));

function shared(getStack) {
  it('two callers in one room hear each other, and neither waits for the other', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('two');
      const trunk = await s.inboundTrunk([number]);
      const rule = await s.directRule(room, [trunk.sipTrunkId], { attributes: { from_rule: 'yes' }, metadata: 'rule metadata' });
      const a = s.phone('a', { play: toneLow, record: `two-a-${stack.name}.wav`, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, record: `two-b-${stack.name}.wav`, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      await a.waitForLog(/state changed to EARLY/);
      await waitFor('the first caller to be in the room', async () => (await listSip(stack, room)).length === 1);
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      const people = await listSip(stack, room);
      assert.equal(people.length, 2);
      const p = people.find((x) => x.identity === 'sip_+15550101');
      assert.ok(p, JSON.stringify(people.map((x) => x.identity)));
      assert.equal(p.name, 'Phone +15550101');
      assert.equal(p.kind, 3, 'kind should be SIP');
      assert.equal(p.metadata, 'rule metadata');
      assert.equal(p.attributes['sip.phoneNumber'], '+15550101');
      assert.equal(p.attributes['sip.trunkPhoneNumber'], number);
      assert.equal(p.attributes['sip.trunkID'], trunk.sipTrunkId);
      assert.equal(p.attributes['sip.ruleID'], rule.sipDispatchRuleId);
      assert.match(p.attributes['sip.callID'], /^SCL_/);
      assert.equal(p.attributes.from_rule, 'yes');
      await waitFor('sip.callStatus to be active', async () => (await listSip(stack, room)).every((x) => x.attributes['sip.callStatus'] === 'active'), { timeout: 10000 });
      await waitFor('its audio track', async () => (await listSip(stack, room)).find((x) => x.identity === 'sip_+15550101')?.tracks.filter((t) => t.type === 0).length === 1, { timeout: 10000 });
      await sleep(5000);
      a.hangup();
      await a.disconnected();
      await waitFor('the first caller to leave', async () => (await listSip(stack, room)).length === 1);
      b.hangup();
      await b.disconnected();
      await a.stop();
      await b.stop();
      assertToneInWav(a.recordPath, HIGH, [LOW]);
      assertToneInWav(b.recordPath, LOW, [HIGH]);
    });
  });

  it('a caller to an empty room hears ringing, and is answered when someone can hear them', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('empty');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const callee = s.phone('callee', { play: toneHigh, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const out = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      const caller = s.phone('caller', { play: toneLow });
      await caller.ready();
      caller.call(sipUri(stack, number));
      await caller.waitForLog(/state changed to EARLY/);
      await sleep(3000);
      assert.ok(!/state changed to CONFIRMED/.test(caller.log), 'the call was answered with nobody there');
      assert.equal((await listSip(stack, room)).length, 1, 'a ringing caller is in the room already');
      assert.equal((await listSip(stack, room))[0].attributes['sip.callStatus'], 'ringing');
      await stack.sip.createSipParticipant(out.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
      await caller.confirmed();
      caller.hangup();
      await caller.disconnected();
      await stack.rooms.removeParticipant(room, 'phone-out');
      await callee.disconnected();
    });
  });

  it('a caller nobody answers gets the status the trunk asks for, when its time is up', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('unanswered');
      const made = await stack.twirp('CreateSIPInboundTrunk', { trunk: { numbers: [number], allowed_addresses: ['127.0.0.1/32'], ringing_timeout: '2s', ringing_timeout_status: 'SIP_STATUS_TEMPORARILY_UNAVAILABLE' } });
      assert.equal(made.status, 200, made.text);
      s.trunks.push(made.body.sip_trunk_id);
      await s.directRule(room, [made.body.sip_trunk_id]);
      const caller = s.phone('caller', { play: toneLow });
      await caller.ready();
      const started = Date.now();
      caller.call(sipUri(stack, number));
      const m = await caller.disconnected();
      assert.equal(m[1], '480');
      assert.ok(Date.now() - started >= 1800, 'it gave up too soon');
      await waitFor('the participant to leave', async () => (await listSip(stack, room)).length === 0, { timeout: 10000 });
    });
  });

  it('a caller giving up before an answer is cancelled cleanly', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('cancel');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const caller = s.phone('caller', { play: toneLow });
      await caller.ready();
      caller.call(sipUri(stack, number));
      await caller.waitForLog(/state changed to EARLY/);
      await waitFor('the caller to be in the room', async () => (await listSip(stack, room)).length === 1);
      caller.hangup();
      const m = await caller.disconnected();
      assert.equal(m[1], '487');
      await waitFor('the participant to leave', async () => (await listSip(stack, room)).length === 0, { timeout: 15000 });
    });
  });

  it('a digit from one phone reaches the other through the room', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('digits');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      await waitFor('sip.callStatus to be active', async () => (await listSip(stack, room)).every((x) => x.attributes['sip.callStatus'] === 'active') && (await listSip(stack, room)).length === 2, { timeout: 10000 });
      await sleep(1500);
      b.mark();
      a.dtmf('1');
      await waitFor('the first digit', async () => (await b.digitsHeard()) === '1', { timeout: 10000 });
      a.dtmf('2');
      await waitFor('the second digit', async () => (await b.digitsHeard()) === '12', { timeout: 10000 });
      // A press of the same digit again is a second press.
      await sleep(400);
      a.dtmf('2');
      await waitFor('the third digit', async () => (await b.digitsHeard()) === '122', { timeout: 10000 });
      a.dtmf('#');
      await waitFor('the last digit', async () => (await b.digitsHeard()) === '122#', { timeout: 10000 });
      a.hangup();
      b.hangup();
      await a.disconnected();
      await b.disconnected();
    });
  });

  it('removing a participant hangs up its phone, and only its phone', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('remove');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      await stack.rooms.removeParticipant(room, 'sip_+15550101');
      const m = await a.disconnected();
      assert.equal(m[1], '200', 'the phone should have been sent a BYE');
      await sleep(1000);
      assert.ok(!/DISCONNECTED/.test(b.log), 'the other phone was hung up too');
      b.hangup();
      await b.disconnected();
    });
  });

  it('an outbound call joins a caller already in the room, and they hear each other', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('outbound');
      const callee = s.phone('callee', { play: toneHigh, record: `out-callee-${stack.name}.wav`, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      const info = await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', participantName: 'Out', dtmf: '4w#', waitUntilAnswered: true });
      assert.equal(info.participantIdentity, 'phone-out');
      assert.equal(info.roomName, room);
      assert.match(info.sipCallId, /^SCL_/);
      assert.ok(info.participantId, 'it should say the participant ID');
      await callee.confirmed();
      await waitFor('the phone to hear the digits', async () => (await callee.digitsHeard()) === '4#', { timeout: 10000 });
      const number = nextNumber();
      const inbound = await s.inboundTrunk([number]);
      await s.directRule(room, [inbound.sipTrunkId]);
      const caller = s.phone('caller', { play: toneLow, record: `out-caller-${stack.name}.wav`, user: '+15550101' });
      await caller.ready();
      caller.call(sipUri(stack, number));
      await caller.confirmed();
      const people = await listSip(stack, room);
      const out = people.find((x) => x.identity === 'phone-out');
      assert.ok(out, JSON.stringify(people.map((x) => x.identity)));
      assert.equal(out.name, 'Out');
      assert.equal(out.kind, 3);
      assert.equal(out.attributes['sip.callID'], info.sipCallId);
      assert.equal(out.attributes['sip.trunkID'], trunk.sipTrunkId);
      assert.equal(out.attributes['sip.phoneNumber'], '+15550177');
      assert.equal(out.attributes['sip.trunkPhoneNumber'], '+15550142');
      await waitFor('sip.callStatus to be active', async () => (await listSip(stack, room)).find((x) => x.identity === 'phone-out')?.attributes['sip.callStatus'] === 'active', { timeout: 10000 });
      await sleep(5000);
      callee.hangup();
      await callee.disconnected();
      await waitFor('the participant to leave', async () => !(await listSip(stack, room)).some((x) => x.identity === 'phone-out'), { timeout: 15000 });
      caller.hangup();
      await caller.disconnected();
      await callee.stop();
      await caller.stop();
      assertToneInWav(callee.recordPath, LOW, [HIGH]);
      assertToneInWav(caller.recordPath, HIGH, [LOW]);
    });
  });

  it('the room ending an outbound call sends a BYE', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('outbound-end');
      const callee = s.phone('callee', { play: toneHigh, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
      await callee.confirmed();
      await stack.rooms.removeParticipant(room, 'phone-out');
      const m = await callee.disconnected();
      assert.equal(m[1], '200');
    });
  });

  it('a call is transferred with REFER, and the phone reaches the target', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('transfer');
      const callee = s.phone('callee', { play: toneHigh, autoAnswer: 200, user: '+15550177' });
      const target = s.phone('target', { play: toneLow, autoAnswer: 200, user: 'target' });
      await callee.ready();
      await target.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
      await callee.confirmed();
      const res = await stack.twirp('TransferSIPParticipant', { participant_identity: 'phone-out', room_name: room, transfer_to: `sip:target@${HOST}:${target.sipPort}` }, { video: { roomAdmin: true, room } });
      assert.equal(res.status, 200, res.text);
      await target.confirmed();
      await waitFor('the first leg to end', async () => !(await listSip(stack, room)).some((x) => x.identity === 'phone-out'), { timeout: 15000 });
    });
  });

  it('an outbound call says what it was asked to on the wire, follows sip.callStatus, and passes headers both ways', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('wire');
      const peer = await new RawPeer().open();
      const seen = [];
      let answer;
      peer.socket.on('message', (m) => {
        const msg = parseSip(m.toString());
        seen.push(msg);
        if (msg.first.startsWith('INVITE')) {
          respondRaw(peer, msg, 180, 'Ringing');
          answer = () => respondRaw(peer, msg, 200, 'OK', `Contact: <sip:callee@${HOST}:${peer.port}>\r\nX-Carrier: from-the-carrier\r\nX-Other: other\r\n`, SDP(40000));
        }
        if (msg.first.startsWith('BYE')) respondRaw(peer, msg, 200, 'OK');
      });
      try {
        // Through the raw API, because the official client has no option for some of these.
        const made = await stack.twirp('CreateSIPOutboundTrunk', { trunk: {
          name: 'wire', address: `${HOST}:${peer.port}`, numbers: ['+15550142'], from_host: 'calls.example.com', include_headers: 'SIP_X_HEADERS',
          headers: { 'X-Team': 'blue' }, headers_to_attributes: { 'X-Carrier': 'carrier' }, attributes_to_headers: { custom_attr: 'X-Sent-On-Bye' },
        } });
        assert.equal(made.status, 200, made.text);
        s.trunks.push(made.body.sip_trunk_id);
        const info = await stack.sip.createSipParticipant(made.body.sip_trunk_id, '+15550177', room, {
          participantIdentity: 'phone-out', participantAttributes: { custom_attr: 'the-value' }, displayName: 'Front Desk', headers: { 'X-Call': 'one' },
        });
        const status = async () => (await listSip(stack, room)).find((x) => x.identity === 'phone-out')?.attributes['sip.callStatus'];
        assert.ok(info.participantId, 'it should return once the participant has joined');
        await waitFor('the phone to ring', () => answer);
        await waitFor('sip.callStatus to be ringing', async () => (await status()) === 'ringing');
        answer();
        await waitFor('sip.callStatus to be active', async () => (await status()) === 'active');
        const inv = seen.find((m) => m.first.startsWith('INVITE'));
        assert.match(inv.first, /^INVITE sip:\+15550177@/);
        assert.match(inv.header('from'), /"?Front Desk"?\s*<sip:\+15550142@calls\.example\.com/);
        assert.match(inv.header('to'), /\+15550177@/);
        assert.equal(inv.header('x-team'), 'blue');
        assert.equal(inv.header('x-call'), 'one');
        assert.match(inv.header('content-type'), /application\/sdp/);
        await waitFor('the ACK', () => seen.some((m) => m.first.startsWith('ACK')));
        const out = (await listSip(stack, room)).find((x) => x.identity === 'phone-out');
        assert.equal(out.attributes.carrier, 'from-the-carrier', JSON.stringify(out.attributes));
        assert.equal(out.attributes['sip.h.x-other'], 'other', JSON.stringify(out.attributes));
        await stack.rooms.removeParticipant(room, 'phone-out');
        const bye = await waitFor('the BYE', () => seen.find((m) => m.first.startsWith('BYE')));
        assert.equal(bye.header('x-sent-on-bye'), 'the-value');
      } finally {
        peer.close();
      }
    });
  });

  it('an inbound call passes its headers on as attributes, and a call to a hidden number hides it', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('inbound-headers');
      const trunk = await s.inboundTrunk([number], { headersToAttributes: { 'X-Customer': 'customer' }, includeHeaders: 1 });
      await s.directRule(room, [trunk.sipTrunkId], { hidePhoneNumber: true });
      const peer = await new RawPeer().open();
      try {
        const callId = `headers-${Date.now()}@tests`;
        const text = invite(stack.sipPort, { to: number, callId, tag: 'caller', localPort: peer.port, headers: 'X-Customer: acme\r\nX-Other: seen\r\nProxy-Authorization: Digest secret\r\n' });
        await peer.send(stack.sipPort, text);
        const p = await waitFor('the caller to be in the room', async () => (await stack.rooms.listParticipants(room).catch(() => []))[0], { timeout: 15000 });
        assert.equal(p.attributes.customer, 'acme', JSON.stringify(p.attributes));
        assert.equal(p.attributes['sip.h.x-other'], 'seen', JSON.stringify(p.attributes));
        assert.equal(p.attributes['sip.callStatus'], 'ringing');
        assert.equal(p.attributes['sip.phoneNumber'], undefined);
        assert.equal(p.attributes['sip.trunkPhoneNumber'], undefined);
        assert.ok(!p.identity.includes('5550100'), p.identity);
        assert.equal(p.name, 'Phone 0100');
        await peer.send(stack.sipPort, invite(stack.sipPort, { method: 'CANCEL', to: number, callId, tag: 'caller', localPort: peer.port, sdp: null, branch: /branch=z9hG4bK(\w+)/.exec(text)[1] }));
        const final = await waitFor('the 487', () => peer.messages.find((m) => m.status === 487), { timeout: 10000 });
        assert.ok(final);
      } finally {
        peer.close();
      }
    });
  });

  it('SIP over TCP works in both directions', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('tcp');
      const inbound = await s.inboundTrunk([number]);
      await s.directRule(room, [inbound.sipTrunkId]);
      const callee = s.phone('callee', { play: toneHigh, record: `tcp-callee-${stack.name}.wav`, autoAnswer: 200, transport: 'tcp', user: '+15550177' });
      await callee.ready();
      const out = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142'], { transport: 2 });
      await stack.sip.createSipParticipant(out.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
      await callee.confirmed();
      const caller = s.phone('caller', { play: toneLow, record: `tcp-caller-${stack.name}.wav`, transport: 'tcp', user: '+15550101' });
      await caller.ready();
      caller.call(sipUri(stack, number, 'tcp'));
      await caller.confirmed();
      await sleep(5000);
      caller.hangup();
      await caller.disconnected();
      await stack.rooms.removeParticipant(room, 'phone-out');
      await callee.disconnected();
      await caller.stop();
      await callee.stop();
      assertToneInWav(caller.recordPath, HIGH, [LOW]);
      assertToneInWav(callee.recordPath, LOW, [HIGH]);
    });
  });

  it('SRTP over SDES carries audio both ways, and a phone without it is refused when it is required', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('srtp');
      const trunk = await s.inboundTrunk([number], { mediaEncryption: 2 });
      await s.directRule(room, [trunk.sipTrunkId]);
      const plain = s.phone('plain', { play: toneLow });
      await plain.ready();
      plain.call(sipUri(stack, number));
      const refused = await plain.disconnected();
      assert.equal(refused[1], '488');
      const a = s.phone('a', { play: toneLow, srtp: 2, record: `srtp-a-${stack.name}.wav`, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, srtp: 2, record: `srtp-b-${stack.name}.wav`, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      await sleep(5000);
      a.hangup();
      b.hangup();
      await a.disconnected();
      await b.disconnected();
      await a.stop();
      await b.stop();
      assertToneInWav(a.recordPath, HIGH, [LOW]);
      assertToneInWav(b.recordPath, LOW, [HIGH]);
    });
  });

  it('an outbound call can require SRTP, or use it when the phone can', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('srtp-out');
      const required = s.phone('required', { play: toneHigh, srtp: 2, record: `srtp-out-${stack.name}.wav`, autoAnswer: 200, user: '+15550177' });
      const optional = s.phone('optional', { play: toneLow, srtp: 1, record: `srtp-opt-${stack.name}.wav`, autoAnswer: 200, user: '+15550178' });
      const plain = s.phone('plain', { autoAnswer: 200, user: '+15550179' });
      await required.ready();
      await optional.ready();
      await plain.ready();
      const requires = await s.outboundTrunk(`${HOST}:${required.sipPort}`, ['+15550142'], { mediaEncryption: 2 });
      const allows = await s.outboundTrunk(`${HOST}:${optional.sipPort}`, ['+15550142'], { mediaEncryption: 1 });
      const refused = await s.outboundTrunk(`${HOST}:${plain.sipPort}`, ['+15550142'], { mediaEncryption: 2 });
      await stack.sip.createSipParticipant(requires.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-required', waitUntilAnswered: true });
      await stack.sip.createSipParticipant(allows.sipTrunkId, '+15550178', room, { participantIdentity: 'phone-optional', waitUntilAnswered: true });
      await required.waitForLog(/SRTP started[\s\S]*Audio updated, stream #0: \S+ \(sendrecv\)/);
      await sleep(5000);
      await stack.rooms.removeParticipant(room, 'phone-required');
      await stack.rooms.removeParticipant(room, 'phone-optional');
      await required.disconnected();
      await optional.disconnected();
      await required.stop();
      await optional.stop();
      assertToneInWav(required.recordPath, LOW, [HIGH]);
      assertToneInWav(optional.recordPath, HIGH, [LOW]);
      // Asking a phone with no encryption for SRTP does not end with a call in the clear.
      await assert.rejects(stack.sip.createSipParticipant(refused.sipTrunkId, '+15550179', room, { participantIdentity: 'phone-plain', waitUntilAnswered: true }));
      assert.ok(!/state changed to CONFIRMED/.test(plain.log), 'a call went ahead in the clear');
    });
  });

  it('A-law is used when that is all a phone offers', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('alaw');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, record: `alaw-a-${stack.name}.wav`, codecs: ['pcma'], user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, record: `alaw-b-${stack.name}.wav`, codecs: ['pcmu'], user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      await sleep(5000);
      a.hangup();
      b.hangup();
      await a.disconnected();
      await b.disconnected();
      await a.stop();
      await b.stop();
      assertToneInWav(a.recordPath, HIGH, [LOW]);
      assertToneInWav(b.recordPath, LOW, [HIGH]);
    });
  });

  it('a phone behind a password gets in with it, and not without it', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('digest');
      const trunk = await s.inboundTrunk([number], { allowedAddresses: undefined, authUsername: 'carrier-user', authPassword: 'carrier-password' });
      await s.directRule(room, [trunk.sipTrunkId]);
      const right = s.phone('right', { play: toneLow, username: 'carrier-user', password: 'carrier-password', user: '+15550101' });
      await right.ready();
      right.call(sipUri(stack, number));
      await waitFor('the caller to be in the room', async () => (await listSip(stack, room)).length === 1, { timeout: 15000 });
      const wrong = s.phone('wrong', { username: 'carrier-user', password: 'not-the-password', user: '+15550102' });
      await wrong.ready();
      wrong.call(sipUri(stack, number));
      const m = await wrong.disconnected();
      assert.ok(['401', '403', '407'].includes(m[1]), `refused with ${m[1]}`);
      assert.equal((await listSip(stack, room)).length, 1, 'someone got in with a wrong password');
    });
  });

  it('a rule with a PIN asks for it, and only a right one gets in', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('pin');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId], { pin: '4242' });
      const phone = s.phone('caller', { play: toneLow });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      await sleep(1500);
      assert.equal((await listSip(stack, room)).length, 0, 'in the room before entering a PIN');
      // A wrong PIN ends the call.
      phone.dtmf('9999#');
      await phone.disconnected();
      assert.equal((await listSip(stack, room)).length, 0, 'a wrong PIN got in');
      const right = s.phone('right', { play: toneLow, user: '+15550103' });
      await right.ready();
      right.call(sipUri(stack, number));
      await right.confirmed();
      await sleep(1000);
      right.dtmf('4242#');
      await waitFor('the caller to be in the room', async () => (await listSip(stack, room)).length === 1, { timeout: 15000 });
      right.hangup();
      await right.disconnected();
    });
  });

  it('a rule makes a room for each caller, or from the number called', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const trunk = await s.inboundTrunk([number]);
      const prefix = `each${counter}`;
      const each = await stack.sip.createSipDispatchRule({ type: 'individual', roomPrefix: prefix }, { trunkIds: [trunk.sipTrunkId] });
      s.rules.push(each.sipDispatchRuleId);
      const phone = s.phone('caller', { play: toneLow });
      await phone.ready();
      phone.call(sipUri(stack, number));
      const found = await waitFor('a room for this caller', async () => (await stack.rooms.listRooms()).find((r) => r.name.startsWith(`${prefix}_+15550100_`)));
      assert.ok(found);
      phone.hangup();
      await phone.disconnected();
      await stack.sip.deleteSipDispatchRule(each.sipDispatchRuleId);
      s.rules.pop();
      const res = await stack.twirp('CreateSIPDispatchRule', { dispatch_rule: { name: 'callee', trunk_ids: [trunk.sipTrunkId], rule: { dispatch_rule_callee: { room_prefix: 'line' } } } });
      assert.equal(res.status, 200, res.text);
      s.rules.push(res.body.sip_dispatch_rule_id);
      phone.mark();
      phone.call(sipUri(stack, number));
      await waitFor('a room for the number', async () => (await stack.rooms.listRooms()).find((r) => r.name === `line_${number}`));
      phone.hangup();
      await phone.disconnected();
    });
  });
}

before(() => {
  prepareDir();
  toneLow = toneFile('tone-low.wav', LOW);
  toneHigh = toneFile('tone-high.wav', HIGH);
});

describe('this service', { skip: Boolean(process.env.SIP_ONLY_REFERENCE) && 'SIP_ONLY_REFERENCE is set' }, () => {
  let stack;
  before(async () => {
    await buildBinaries();
    stack = await startOurs();
  }, { timeout: 240000 });
  after(async () => {
    await Promise.all((stack?.workers ?? []).map((w) => stopProcess(w)));
    if (stack) await stopProcess(stack.server);
  });
  shared(() => stack);

  it('a call has a record: who, when, and how it ended', { timeout: 60000 }, async () => {
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('record');
      const trunk = await s.inboundTrunk([number]);
      const rule = await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      const p = (await listSip(stack, room)).find((x) => x.identity === 'sip_+15550101');
      const id = p.attributes['sip.callID'];
      await waitFor('the record to say active', async () => (await stack.account.ok('GET', `/sip/calls/${id}`)).status === 'active');
      a.hangup();
      await a.disconnected();
      const call = await waitFor('the record to say ended', async () => {
        const c = await stack.account.ok('GET', `/sip/calls/${id}`);
        return c.status === 'ended' ? c : null;
      });
      assert.equal(call.direction, 'inbound');
      assert.equal(call.trunk_id, trunk.sipTrunkId);
      assert.equal(call.dispatch_rule_id, rule.sipDispatchRuleId);
      assert.equal(call.room_name, room);
      assert.equal(call.participant_identity, 'sip_+15550101');
      assert.equal(call.from_user, '+15550101');
      assert.equal(call.to_user, number);
      assert.equal(call.audio_codec, 'PCMU');
      assert.equal(call.disconnect_reason, 'CLIENT_INITIATED');
      assert.match(call.sip_call_id, /./);
      assert.ok(call.answered_at && call.ended_at, JSON.stringify(call));
      b.hangup();
      await b.disconnected();
    });
  });

  it('going on hold keeps the call going, and it comes back', { timeout: 90000 }, async () => {
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('hold');
      // With a short media timeout, so a call on hold would be ended by it if that were not allowed for.
      const made = await stack.twirp('CreateSIPInboundTrunk', { trunk: { numbers: [number], allowed_addresses: ['127.0.0.1/32'], media: { media_timeout: '2s' } } });
      assert.equal(made.status, 200, made.text);
      s.trunks.push(made.body.sip_trunk_id);
      await s.directRule(room, [made.body.sip_trunk_id]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, record: 'hold-b.wav', user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      await sleep(1500);
      a.mark();
      a.hold();
      await a.waitForLog(/status is Local hold/, 10000);
      await sleep(5000);
      assert.equal((await listSip(stack, room)).length, 2, 'going on hold ended a call');
      a.mark();
      a.reinvite();
      await a.waitForLog(/Audio updated, stream #0: PCMU \(sendrecv\)/, 10000);
      await sleep(4000);
      b.hangup();
      a.hangup();
      await a.disconnected();
      await b.disconnected();
      await b.stop();
      assertToneInWav(b.recordPath, LOW, [HIGH]);
    });
  });

  it('a digit sent as SIP INFO reaches the room, and the other phone', { timeout: 60000 }, async () => {
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('info');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      await sleep(1500);
      b.mark();
      a.sipInfoDtmf('8');
      await waitFor('the digit', async () => (await b.digitsHeard()) === '8', { timeout: 10000 });
      a.hangup();
      b.hangup();
      await a.disconnected();
      await b.disconnected();
    });
  });

  it('SIP over TLS works in both directions, checking the certificate going out', { timeout: 120000 }, async () => {
    const cert = makeCertificate();
    await Promise.all(stack.workers.map((w) => stopProcess(w)));
    stack.workers.length = 0;
    await stack.startWorker('sip-worker-tls', writeConfig('config-tls.yaml', { tls: cert }));
    try {
      await scene(stack, async (s) => {
        const number = nextNumber();
        const room = nextRoom('tls');
        const trunk = await s.inboundTrunk([number]);
        await s.directRule(room, [trunk.sipTrunkId]);
        const callee = s.phone('callee', { play: toneHigh, record: 'tls-callee.wav', autoAnswer: 200, transport: 'tls', tlsCert: cert, user: '+15550177' });
        const caller = s.phone('caller', { play: toneLow, record: 'tls-caller.wav', transport: 'tls', tlsCert: cert, user: '+15550101' });
        await callee.ready();
        await caller.ready();
        // pjsua listens for TLS one port above the one it was given.
        const out = await s.outboundTrunk(`${HOST}:${callee.sipPort + 1}`, ['+15550142'], { transport: 3 });
        await stack.sip.createSipParticipant(out.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
        await callee.confirmed();
        caller.call(`sip:${number}@${HOST}:${OURS.tls};transport=tls`);
        await caller.confirmed();
        await sleep(5000);
        caller.hangup();
        await caller.disconnected();
        await stack.rooms.removeParticipant(room, 'phone-out');
        await callee.disconnected();
        await caller.stop();
        await callee.stop();
        assertToneInWav(caller.recordPath, HIGH, [LOW]);
        assertToneInWav(callee.recordPath, LOW, [HIGH]);
        // A certificate that does not match is refused, going out.
        const stranger = s.phone('stranger', { autoAnswer: 200, transport: 'tls', tlsCert: makeCertificate('wrong', '192.0.2.1'), user: '+15550178' });
        await stranger.ready();
        const bad = await s.outboundTrunk(`${HOST}:${stranger.sipPort + 1}`, ['+15550142'], { transport: 3 });
        await assert.rejects(stack.sip.createSipParticipant(bad.sipTrunkId, '+15550178', room, { participantIdentity: 'phone-bad', waitUntilAnswered: true }));
        assert.ok(!/state changed to CONFIRMED/.test(stranger.log), 'a call went out to a certificate that did not match');
      });
    } finally {
      await Promise.all(stack.workers.map((w) => stopProcess(w)));
      stack.workers.length = 0;
      await stack.startWorker();
    }
  });

  it('a worker that dies leaves calls that are marked failed, and the next one carries on', { timeout: 90000 }, async () => {
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('crash');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      const ids = (await listSip(stack, room)).map((x) => x.attributes['sip.callID']);
      const dead = stack.workers.pop();
      dead.kill('SIGKILL');
      await dead.exited;
      // Their room connections dropped with it, and the server takes the participants out.
      await waitFor('the participants to leave', async () => (await listSip(stack, room)).length === 0, { timeout: 30000 });
      for (const id of ids) {
        const call = await waitFor('the lease to run out', async () => {
          const c = await stack.account.ok('GET', `/sip/calls/${id}`);
          return c.status === 'failed' ? c : null;
        }, { timeout: 30000, every: 300 });
        assert.match(call.error, /stopped responding/);
      }
      a.hangup();
      b.hangup();
      await stack.startWorker();
      const c = s.phone('c', { play: toneLow, user: '+15550103' });
      const d = s.phone('d', { play: toneHigh, user: '+15550104' });
      await c.ready();
      await d.ready();
      c.call(sipUri(stack, number));
      d.call(sipUri(stack, number));
      await c.confirmed();
      await d.confirmed();
      c.hangup();
      d.hangup();
      await c.disconnected();
      await d.disconnected();
    });
  });

  it('a stopping worker turns new calls away, and hangs up what is left when its time is up', { timeout: 90000 }, async () => {
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('drain');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const a = s.phone('a', { play: toneLow, user: '+15550101' });
      const b = s.phone('b', { play: toneHigh, user: '+15550102' });
      await a.ready();
      await b.ready();
      a.call(sipUri(stack, number));
      b.call(sipUri(stack, number));
      await a.confirmed();
      await b.confirmed();
      const worker = stack.workers.pop();
      worker.kill('SIGTERM');
      await sleep(300);
      const peer = await new RawPeer().open();
      try {
        await peer.send(stack.sipPort, invite(stack.sipPort, { to: number, sdp: SDP(4000), localPort: peer.port }));
        const res = await peer.final();
        assert.equal(res.status, 503);
      } finally {
        peer.close();
      }
      assert.equal((await listSip(stack, room)).length, 2, 'calls should carry on while draining');
      const m = await a.disconnected(15000);
      assert.equal(m[1], '200', 'the worker should have sent a BYE as it stopped');
      const exit = await worker.exited;
      assert.equal(exit.code, 0, 'a clean stop');
      await stack.startWorker();
    });
  });

  it('two workers share the calls, and phones on each hear each other', { timeout: 120000 }, async () => {
    const w2 = await stack.startWorker('sip-worker-2', writeConfig('config-second.yaml', { port: OURS.second, rtpMin: OURS.rtpMax + 100, rtpMax: OURS.rtpMax + 199 }));
    try {
      await scene(stack, async (s) => {
        const number = nextNumber();
        const room = nextRoom('scale');
        const trunk = await s.inboundTrunk([number]);
        await s.directRule(room, [trunk.sipTrunkId]);
        const a = s.phone('a', { play: toneLow, record: 'scale-a.wav', user: '+15550101' });
        const b = s.phone('b', { play: toneHigh, record: 'scale-b.wav', user: '+15550102' });
        await a.ready();
        await b.ready();
        a.call(sipUri(stack, number));
        b.call(`sip:${number}@${HOST}:${OURS.second}`);
        await a.confirmed();
        await b.confirmed();
        const servers = await stack.account.ok('GET', '/servers');
        assert.ok(servers.items.filter((x) => x.stats.kind === 'sip_worker' && Date.now() - Date.parse(x.last_seen_at) < 15000).length >= 2, JSON.stringify(servers.items.map((x) => x.stats.kind)));
        await sleep(5000);
        a.hangup();
        b.hangup();
        await a.disconnected();
        await b.disconnected();
        await a.stop();
        await b.stop();
        assertToneInWav(a.recordPath, HIGH, [LOW]);
        assertToneInWav(b.recordPath, LOW, [HIGH]);
      });
    } finally {
      stack.workers.splice(stack.workers.indexOf(w2), 1);
      await stopProcess(w2);
    }
  });

  it('a worker an operator drains turns new calls away, and takes them when told again', { timeout: 60000 }, async () => {
    await scene(stack, async (s) => {
      const servers = await stack.account.ok('GET', '/servers');
      const workers = servers.items.filter((x) => x.stats.kind === 'sip_worker' && Date.now() - Date.parse(x.last_seen_at) < 15000);
      assert.ok(workers.length >= 1);
      const trunk = await s.outboundTrunk(`${HOST}:9`, ['+15550142']);
      // A number with no trunk: 403 from a worker taking calls, and 503 before it even looks from one that is not.
      const probe = async () => {
        const peer = await new RawPeer().open();
        try {
          await peer.send(stack.sipPort, invite(stack.sipPort, { to: '+19990000000', sdp: SDP(4000), localPort: peer.port }));
          return (await peer.final()).status;
        } finally {
          peer.close();
        }
      };
      assert.equal(await probe(), 403);
      const setDraining = (draining) => Promise.all(workers.map((w) => stack.account.ok('POST', `/servers/${w.node_id}/drain`, { draining })));
      try {
        await setDraining(true);
        await waitFor('the worker to turn calls away', async () => (await probe()) === 503, { timeout: 15000, every: 200 });
        await assert.rejects(stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', nextRoom('drained')), /no SIP worker/);
      } finally {
        await setDraining(false);
      }
      await waitFor('the worker to take calls again', async () => (await probe()) === 403, { timeout: 15000, every: 200 });
    });
  });

  it('the REST routes place a call, show it, and hang it up', { timeout: 60000 }, async () => {
    await scene(stack, async (s) => {
      const room = nextRoom('rest');
      const callee = s.phone('callee', { play: toneHigh, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      const call = await stack.account.ok('POST', '/sip/calls', { sip_trunk_id: trunk.sipTrunkId, sip_call_to: '+15550177', room_name: room, participant_identity: 'phone-out', wait_until_answered: true }, [201]);
      assert.match(call.id, /^SCL_/);
      assert.equal(call.direction, 'outbound');
      assert.equal(call.status, 'active');
      assert.equal(call.to_user, '+15550177');
      assert.equal(call.from_user, '+15550142');
      assert.equal(call.audio_codec, 'PCMU');
      assert.ok(call.answered_at);
      await callee.confirmed();
      const list = await stack.account.ok('GET', '/sip/calls?limit=1');
      assert.equal(list.items[0].id, call.id);
      assert.ok(list.next_cursor, 'there should be a cursor when the page is full');
      await stack.account.ok('POST', `/sip/calls/${call.id}/hangup`, {});
      const m = await callee.disconnected();
      assert.equal(m[1], '200', 'the phone should have been sent a BYE');
      const ended = await waitFor('the record to say so', async () => {
        const c = await stack.account.ok('GET', `/sip/calls/${call.id}`);
        return c.status === 'ended' ? c : null;
      });
      assert.equal(ended.disconnect_reason, 'CLIENT_INITIATED');
      assert.ok(ended.ended_at);
      const again = await stack.account.req('POST', `/sip/calls/${call.id}/hangup`, {});
      assert.equal(again.status, 409, again.text);
      assert.equal(again.body.error.code, 'sip_call_ended');
    });
  });

  it('an outbound call whose request hides the number keeps it out of the room and the record', { timeout: 60000 }, async () => {
    await scene(stack, async (s) => {
      const room = nextRoom('hidden-out');
      const callee = s.phone('callee', { play: toneHigh, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      const info = await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { hidePhoneNumber: true, waitUntilAnswered: true });
      assert.ok(!info.participantIdentity.includes('5550177'), info.participantIdentity);
      const p = (await stack.rooms.listParticipants(room))[0];
      assert.equal(p.attributes['sip.phoneNumber'], undefined);
      assert.equal(p.attributes['sip.trunkPhoneNumber'], undefined);
      const c = await stack.account.ok('GET', `/sip/calls/${info.sipCallId}`);
      assert.equal(c.to_user, '');
      assert.equal(c.from_user, '');
      await stack.rooms.removeParticipant(room, info.participantIdentity);
      await callee.disconnected();
    });
  });

  it('a project can only place calls through its own trunks', { timeout: 30000 }, async () => {
    const other = await Account.signUp(stack.base, 'other');
    await scene(stack, async (s) => {
      const trunk = await s.outboundTrunk('203.0.113.5:5060', ['+15550142'], { authUsername: 'u', authPassword: 'p' });
      const theirs = new SipClient(stack.base, other.apiKey, other.apiSecret);
      await assert.rejects(theirs.createSipParticipant(trunk.sipTrunkId, '+15550177', 'room'), /not exist/);
    });
  });
});

describe('the reference, with the same scenarios', { skip: !referenceAvailable() && 'needs the reference server and the elnova-sip-oracle:dev image' }, () => {
  let stack;
  before(async () => {
    stack = await startReference();
  }, { timeout: 120000 });
  after(async () => {
    if (stack) await stopReference(stack);
  });
  shared(() => stack);
});

after(stopAll);

// Real calls: pjsua, an independent SIP stack and media engine, calling through SIP sockets and RTP sockets into rooms, with someone in the
// room using the official client library in real Chrome. Audio is checked by content, by the tone that comes out of a real decoder at
// the far end. The same scenarios run against the reference server and its SIP bridge, so they are about the protocol and not about
// our reading of it. Everything is on loopback: this says nothing about real carriers, NAT or the internet.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import {
  HOST, Phone, RawPeer, assertHears, assertToneInWav, buildBinaries, findParticipant, invite, parseSip, person, prepareDir, referenceAvailable, respondRaw,
  sipUri, sleep, startOurs, startReference, stopAll, stopBrowser, stopProcess, stopReference, toneFile, waitFor,
} from './lib.mjs';

const TONE_PHONE = 1000;
const TONE_ROOM = 440;
let counter = 0;
let tonePhone;
const nextNumber = () => `+1555${String(100000 + (++counter)).slice(1)}`;
const nextRoom = (tag) => `${tag}-${process.pid}-${++counter}`;

const LOOPBACK = ['127.0.0.1/32'];

// Everything a scenario makes, so that it can be taken away even when the scenario fails.
class Scene {
  constructor(stack) {
    this.stack = stack;
    this.phones = [];
    this.people = [];
    this.trunks = [];
    this.rules = [];
  }

  phone(name, opts) {
    const p = new Phone(`${name}-${++counter}`, opts);
    this.phones.push(p);
    return p;
  }

  async person(identity, room, opts) {
    const p = await person(this.stack, identity, room, opts);
    this.people.push(p);
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
    const r = await this.stack.sip.createSipDispatchRule({ type: 'direct', roomName: room, pin: opts.pin }, { trunkIds, name: 'test rule', ...opts });
    this.rules.push(r.sipDispatchRuleId);
    return r;
  }

  async done() {
    await Promise.all(this.phones.map((p) => p.stop()));
    await Promise.all(this.people.map((p) => p.close()));
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

// Scenarios that have to hold on both this service and the reference.
function sharedScenarios(getStack) {
  it('an inbound call carries tone both ways, with the right participant, and ends when the phone hangs up', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('inbound');
      const trunk = await s.inboundTrunk([number]);
      const rule = await s.directRule(room, [trunk.sipTrunkId], { attributes: { from_rule: 'yes' }, metadata: 'rule metadata' });
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone, record: `inbound-${stack.name}.wav` });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      assert.equal(sip.identity, 'sip_+15550100');
      assert.equal(sip.name, 'Phone +15550100');
      assert.equal(sip.metadata, 'rule metadata');
      assert.equal(sip.kind, 3, 'the participant should be of kind SIP');
      assert.equal(sip.attributes['sip.phoneNumber'], '+15550100');
      assert.equal(sip.attributes['sip.trunkID'], trunk.sipTrunkId);
      assert.equal(sip.attributes['sip.ruleID'], rule.sipDispatchRuleId);
      assert.equal(sip.attributes['sip.trunkPhoneNumber'], number);
      assert.match(sip.attributes['sip.callID'], /^SCL_/);
      assert.equal(sip.attributes.from_rule, 'yes');
      await waitFor('sip.callStatus to be active', async () => (await alice.participants()).find((p) => p.identity === sip.identity)?.attributes['sip.callStatus'] === 'active');
      await assertHears(alice, sip.identity, TONE_PHONE, TONE_ROOM);
      await sleep(3000);
      phone.hangup();
      await phone.disconnected();
      await waitFor('the participant to leave', async () => (await alice.left()).includes(sip.identity));
      await phone.stop();
      assertToneInWav(phone.recordPath, TONE_ROOM, [TONE_PHONE]);
    });
  });

  it('DTMF goes both ways', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('dtmf');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      await waitFor('sip.callStatus to be active', async () => (await alice.participants()).find((p) => p.identity === sip.identity)?.attributes['sip.callStatus'] === 'active');
      // From the phone, as RFC 4733 events. Each digit should arrive exactly once. Digits only go once the phone can be heard: a person would wait for that too, and the
      // reference has lost a digit sent half a second after answering.
      await assertHears(alice, sip.identity, TONE_PHONE, TONE_ROOM);
      phone.dtmf('12#');
      await waitFor('the digits to reach the room', async () => (await alice.digits()).length >= 3, { timeout: 15000 });
      await sleep(1000);
      const digits = await alice.digits();
      assert.equal(digits.map((d) => d.digit).join(''), '12#');
      assert.deepEqual(digits.map((d) => d.code), [1, 2, 11]);
      assert.equal(digits[0].from, sip.identity);
      // Pressing a digit twice is two presses.
      phone.dtmf('5');
      await sleep(600);
      phone.dtmf('5');
      await waitFor('both presses', async () => (await alice.digits()).length >= 5, { timeout: 10000 });
      // From the room.
      phone.mark();
      await alice.sendDigit(7, '7');
      await sleep(400);
      await alice.sendDigit(11, '#');
      await waitFor('the phone to hear the digits', async () => (await phone.digitsHeard()) === '7#', { timeout: 10000 });
      phone.hangup();
      await phone.disconnected();
    });
  });

  it('removing the participant from the room hangs up the phone', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('remove');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      await stack.rooms.removeParticipant(room, sip.identity);
      const m = await phone.disconnected();
      assert.equal(m[1], '200', 'the phone should have been sent a BYE');
    });
  });

  it('the lk.sip.EndCall RPC hangs up the phone', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('rpc');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      await waitFor('sip.callStatus to be active', async () => (await alice.participants()).find((p) => p.identity === sip.identity)?.attributes['sip.callStatus'] === 'active');
      const headers = JSON.parse(await alice.rpc(sip.identity, 'lk.sip.GetRemoteHeaders', '{}'));
      assert.ok(headers.headers['Call-ID'] || headers.headers['call-id'], `headers: ${JSON.stringify(headers)}`);
      await alice.rpc(sip.identity, 'lk.sip.EndCall', '{}');
      await phone.disconnected();
    });
  });

  it('a caller to an empty room hears ringing, and is answered when someone arrives', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('empty');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.waitForLog(/state changed to EARLY/);
      await sleep(2500);
      assert.ok(!/state changed to CONFIRMED/.test(phone.log), 'the call was answered with nobody there');
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      await phone.confirmed();
      await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      phone.hangup();
      await phone.disconnected();
    });
  });

  it('a caller giving up before an answer is cancelled cleanly', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('cancel');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.waitForLog(/state changed to EARLY/);
      phone.hangup();
      const m = await phone.disconnected();
      assert.equal(m[1], '487');
      await waitFor('the participant to leave', async () => (await stack.rooms.listParticipants(room).catch(() => [])).length === 0, { timeout: 15000 });
    });
  });

  it('a call in with a password is challenged, and a right password gets in', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('digest');
      const trunk = await s.inboundTrunk([number], { allowedAddresses: undefined, authUsername: 'carrier-user', authPassword: 'carrier-password' });
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone, username: 'carrier-user', password: 'carrier-password' });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      phone.hangup();
      await phone.disconnected();
      // Leaving the room comes after the BYE is answered, and it takes a moment on the reference.
      await waitFor('the first caller to leave', async () => (await alice.participants()).filter((p) => p.identity.startsWith('sip_')).length === 0);

      const wrong = s.phone('wrong', { username: 'carrier-user', password: 'not-the-password' });
      await wrong.ready();
      wrong.call(sipUri(stack, number));
      const m = await wrong.disconnected();
      assert.ok(['401', '403', '407'].includes(m[1]), `refused with ${m[1]}`);
      assert.equal((await alice.participants()).filter((p) => p.identity.startsWith('sip_')).length, 0, 'someone got in with a wrong password');
    });
  });

  it('an outbound call carries tone both ways, and sends its digits', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('outbound');
      const callee = s.phone('callee', { play: tonePhone, record: `outbound-${stack.name}.wav`, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const info = await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, {
        participantIdentity: 'phone-out', participantName: 'Out', dtmf: '4w#', waitUntilAnswered: true,
      });
      assert.equal(info.participantIdentity, 'phone-out');
      assert.equal(info.roomName, room);
      assert.match(info.sipCallId, /^SCL_/);
      await callee.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity === 'phone-out');
      assert.equal(sip.kind, 3);
      assert.equal(sip.attributes['sip.callID'], info.sipCallId);
      assert.equal(sip.attributes['sip.trunkID'], trunk.sipTrunkId);
      assert.equal(sip.attributes['sip.phoneNumber'], '+15550177');
      assert.equal(sip.attributes['sip.trunkPhoneNumber'], '+15550142');
      await assertHears(alice, 'phone-out', TONE_PHONE, TONE_ROOM);
      await waitFor('the phone to hear the digits', async () => (await callee.digitsHeard()) === '4#', { timeout: 10000 });
      await waitFor('sip.callStatus to be active', async () => (await alice.participants()).find((p) => p.identity === 'phone-out')?.attributes['sip.callStatus'] === 'active');
      await sleep(3000);
      callee.hangup();
      await callee.disconnected();
      await waitFor('the participant to leave', async () => (await alice.left()).includes('phone-out'));
      await callee.stop();
      assertToneInWav(callee.recordPath, TONE_ROOM, [TONE_PHONE]);
    });
  });

  it('an outbound call the room ends sends a BYE', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('outbound-end');
      const callee = s.phone('callee', { play: tonePhone, autoAnswer: 200, user: '+15550177' });
      await callee.ready();
      const trunk = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142']);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
      await callee.confirmed();
      await findParticipant(alice, (p) => p.identity === 'phone-out');
      await stack.rooms.removeParticipant(room, 'phone-out');
      const m = await callee.disconnected();
      assert.equal(m[1], '200');
    });
  });

  it('a busy phone comes back as an error with its SIP status', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('busy');
      const peer = await new RawPeer().open();
      const seen = [];
      peer.socket.on('message', (m) => {
        const msg = parseSip(m.toString());
        seen.push(msg);
        if (msg.first.startsWith('INVITE')) {
          respondRaw(peer, msg, 486, 'Busy Here');
        }
      });
      try {
        const trunk = await s.outboundTrunk(`${HOST}:${peer.port}`, ['+15550142']);
        await assert.rejects(
          stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true }),
          (e) => {
            assert.equal(String(e.metadata?.sip_status_code ?? ''), '486', `error: ${JSON.stringify(e)}`);
            return true;
          },
        );
        const inv = seen.find((m) => m.first.startsWith('INVITE'));
        assert.match(inv.first, /^INVITE sip:\+15550177@/);
        assert.match(inv.header('from'), /\+15550142/);
        assert.match(inv.header('to'), /\+15550177/);
        assert.match(inv.body, /m=audio \d+ RTP\/AVP .*\b0\b/);
        assert.match(inv.body, /telephone-event\/8000/);
      } finally {
        peer.close();
      }
    });
  });

  it('an outbound call answers a digest challenge from the trunk', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const room = nextRoom('out-digest');
      const peer = await new RawPeer().open();
      const seen = [];
      const answers = [];
      peer.socket.on('message', (m) => {
        const msg = parseSip(m.toString());
        seen.push(msg);
        if (!msg.first.startsWith('INVITE')) return;
        const auth = msg.header('proxy-authorization');
        if (!auth) {
          respondRaw(peer, msg, 407, 'Proxy Authentication Required', 'Proxy-Authenticate: Digest realm="carrier", nonce="abc123", qop="auth", algorithm=MD5\r\n');
          return;
        }
        answers.push(auth);
        respondRaw(peer, msg, 486, 'Busy Here');
      });
      try {
        const trunk = await s.outboundTrunk(`${HOST}:${peer.port}`, ['+15550142'], { authUsername: 'out-user', authPassword: 'out-password' });
        await assert.rejects(stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true }));
        assert.equal(answers.length, 1, 'expected one answer to the challenge');
        assert.match(answers[0], /username="out-user"/);
        assert.match(answers[0], /realm="carrier"/);
        assert.match(answers[0], /nonce="abc123"/);
        assert.ok(!answers[0].includes('out-password'), 'the password went over the wire');
      } finally {
        peer.close();
      }
    });
  });

  it('a call is transferred with REFER, and the phone reaches the target', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('transfer');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const target = s.phone('target', { autoAnswer: 200, user: 'target', play: tonePhone });
      await target.ready();
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      await waitFor('sip.callStatus to be active', async () => (await alice.participants()).find((p) => p.identity === sip.identity)?.attributes['sip.callStatus'] === 'active');
      const at = await stack.twirp('TransferSIPParticipant', { participant_identity: sip.identity, room_name: room, transfer_to: `sip:target@${HOST}:${target.sipPort}` }, { video: { roomAdmin: true, room } });
      assert.equal(at.status, 200, at.text);
      await target.confirmed();
      await waitFor('the first leg to end', async () => (await alice.left()).includes(sip.identity), { timeout: 15000 });
    });
  });

  it('a transfer the target refuses comes back as an error, and the call goes on', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('transfer-no');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const peer = await new RawPeer().open();
      peer.socket.on('message', (m) => {
        const msg = parseSip(m.toString());
        if (msg.first.startsWith('INVITE')) respondRaw(peer, msg, 486, 'Busy Here');
      });
      try {
        const phone = s.phone('caller', { play: tonePhone });
        await phone.ready();
        phone.call(sipUri(stack, number));
        await phone.confirmed();
        const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
        await waitFor('sip.callStatus to be active', async () => (await alice.participants()).find((p) => p.identity === sip.identity)?.attributes['sip.callStatus'] === 'active');
        const at = await stack.twirp('TransferSIPParticipant', { participant_identity: sip.identity, room_name: room, transfer_to: `sip:target@${HOST}:${peer.port}`, ringing_timeout: '15s' }, { video: { roomAdmin: true, room } });
        assert.notEqual(at.status, 200, at.text);
        await sleep(1500);
        assert.ok(!(await alice.left()).includes(sip.identity), 'the call ended although the transfer failed');
        phone.hangup();
        await phone.disconnected();
      } finally {
        peer.close();
      }
    });
  });

  it('a transfer needs the room admin grant as well as the call grant', { timeout: 30000 }, async () => {
    const stack = getStack();
    const at = await stack.twirp('TransferSIPParticipant', { participant_identity: 'x', room_name: 'y', transfer_to: 'sip:a@b' }, { sip: { call: true } });
    assert.equal(at.status, 401, at.text);
    assert.equal(at.body.code, 'unauthenticated');
  });

  it('a dispatch rule gives each caller their own room', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const trunk = await s.inboundTrunk([number]);
      const rule = await stack.sip.createSipDispatchRule({ type: 'individual', roomPrefix: 'each' }, { trunkIds: [trunk.sipTrunkId] });
      s.rules.push(rule.sipDispatchRuleId);
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      const found = await waitFor('the room to exist', async () => {
        const rooms = await stack.rooms.listRooms();
        return rooms.find((r) => r.name.startsWith('each_+15550100_'));
      });
      const alice = await s.person('alice', found.name, { tone: TONE_ROOM });
      await phone.confirmed();
      await findParticipant(alice, (p) => p.identity === 'sip_+15550100');
      phone.hangup();
      await phone.disconnected();
    });
  });

  it('a dispatch rule can make the room from the number called', { timeout: 60000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const trunk = await s.inboundTrunk([number]);
      const res = await stack.twirp('CreateSIPDispatchRule', { dispatch_rule: { name: 'callee', trunk_ids: [trunk.sipTrunkId], rule: { dispatch_rule_callee: { room_prefix: 'line' } } } });
      assert.equal(res.status, 200, res.text);
      s.rules.push(res.body.sip_dispatch_rule_id);
      const alice = await s.person('alice', `line_${number}`, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      await findParticipant(alice, (p) => p.identity === 'sip_+15550100');
      phone.hangup();
      await phone.disconnected();
    });
  });

  it('a rule with a PIN asks for it, and lets a right one in', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('pin');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId], { pin: '4242' });
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      await sleep(1500);
      assert.equal((await alice.participants()).filter((p) => p.identity.startsWith('sip_')).length, 0, 'in the room before entering a PIN');
      phone.dtmf('9999#');
      await phone.disconnected();
      assert.equal((await alice.participants()).filter((p) => p.identity.startsWith('sip_')).length, 0, 'a wrong PIN got in');
      const right = s.phone('right', { play: tonePhone, user: '+15550103' });
      await right.ready();
      right.call(sipUri(stack, number));
      await right.confirmed();
      await sleep(1000);
      right.dtmf('4242#');
      await findParticipant(alice, (p) => p.identity === 'sip_+15550103', 20000);
      await assertHears(alice, 'sip_+15550103', TONE_PHONE, TONE_ROOM);
      right.hangup();
      await right.disconnected();
    });
  });

  it('SIP over TCP works in both directions', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('tcp');
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const phone = s.phone('caller', { play: tonePhone, transport: 'tcp' });
      await phone.ready();
      phone.call(sipUri(stack, number, 'tcp'));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      await assertHears(alice, sip.identity, TONE_PHONE, TONE_ROOM);
      phone.hangup();
      await phone.disconnected();

      const callee = s.phone('callee', { play: tonePhone, autoAnswer: 200, transport: 'tcp', user: '+15550177' });
      await callee.ready();
      const out = await s.outboundTrunk(`${HOST}:${callee.sipPort}`, ['+15550142'], { transport: 2 });
      await stack.sip.createSipParticipant(out.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
      await callee.confirmed();
      await assertHears(alice, 'phone-out', TONE_PHONE, TONE_ROOM);
      await stack.rooms.removeParticipant(room, 'phone-out');
      await callee.disconnected();
    });
  });

  it('SRTP over SDES carries audio, and a phone with no encryption is refused when it is required', { timeout: 90000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const room = nextRoom('srtp');
      const trunk = await s.inboundTrunk([number], { mediaEncryption: 2 });
      await s.directRule(room, [trunk.sipTrunkId]);
      const alice = await s.person('alice', room, { tone: TONE_ROOM });
      const plain = s.phone('plain', { play: tonePhone });
      await plain.ready();
      plain.call(sipUri(stack, number));
      const refused = await plain.disconnected();
      assert.equal(refused[1], '488');
      const phone = s.phone('caller', { play: tonePhone, srtp: 2, record: `srtp-${stack.name}.wav` });
      await phone.ready();
      phone.call(sipUri(stack, number));
      await phone.confirmed();
      const sip = await findParticipant(alice, (p) => p.identity.startsWith('sip_'));
      await assertHears(alice, sip.identity, TONE_PHONE, TONE_ROOM);
      await sleep(3000);
      phone.hangup();
      await phone.disconnected();
      await phone.stop();
      assertToneInWav(phone.recordPath, TONE_ROOM, [TONE_PHONE]);
    });
  });

  it('an offer with no codec in common gets a 488', { timeout: 30000 }, async () => {
    const stack = getStack();
    await scene(stack, async (s) => {
      const number = nextNumber();
      const trunk = await s.inboundTrunk([number]);
      await s.directRule(nextRoom('codec'), [trunk.sipTrunkId]);
      const peer = await new RawPeer().open();
      try {
        const sdp = `v=0\r\no=raw 1 1 IN IP4 ${HOST}\r\ns=raw\r\nc=IN IP4 ${HOST}\r\nt=0 0\r\nm=audio 4000 RTP/AVP 96\r\na=rtpmap:96 SPEEX/8000\r\n`;
        await peer.send(stack.sipPort, invite(stack.sipPort, { to: number, sdp, localPort: peer.port }));
        const res = await peer.final();
        assert.equal(res.status, 488);
      } finally {
        peer.close();
      }
    });
  });

  it('REGISTER is not something this does', { timeout: 30000 }, async () => {
    const stack = getStack();
    const peer = await new RawPeer().open();
    try {
      const id = `${Date.now()}`;
      await peer.send(stack.sipPort, `REGISTER sip:${HOST} SIP/2.0\r\nVia: SIP/2.0/UDP ${HOST}:${peer.port};branch=z9hG4bK${id}\r\nFrom: <sip:a@${HOST}>;tag=${id}\r\nTo: <sip:a@${HOST}>\r\nCall-ID: ${id}\r\nCSeq: 1 REGISTER\r\nMax-Forwards: 70\r\nContact: <sip:a@${HOST}:${peer.port}>\r\nContent-Length: 0\r\n\r\n`);
      const res = await peer.final();
      assert.equal(res.status, 405);
    } finally {
      peer.close();
    }
  });
}

describe('this service', { skip: Boolean(process.env.SIP_ONLY_REFERENCE) && 'SIP_ONLY_REFERENCE is set' }, () => {
  let stack;
  before(async () => {
    prepareDir();
    await buildBinaries();
    tonePhone = toneFile('tone-phone.wav', TONE_PHONE);
    stack = await startOurs();
  }, { timeout: 240000 });
  after(async () => {
    await Promise.all((stack?.workers ?? []).map((w) => stopProcess(w)));
    if (stack) await stopProcess(stack.server);
  });

  sharedScenarios(() => stack);
});

describe('the reference, running the same scenarios', { skip: !referenceAvailable() && 'needs the reference server and the elnova-sip-oracle:dev image' }, () => {
  let stack;
  before(async () => {
    tonePhone ??= toneFile('tone-phone.wav', TONE_PHONE);
    stack = await startReference();
  }, { timeout: 120000 });
  after(async () => {
    if (stack) await stopReference(stack);
  });
  sharedScenarios(() => stack);
});

after(async () => {
  await stopBrowser();
  await stopAll();
});

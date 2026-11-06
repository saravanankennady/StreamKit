// What a phone-facing port meets on its first day: scanners, password guessing, floods, malformed messages, and people trying to use it
// to reach something else. Everything here talks to the worker over real sockets on loopback. Nothing has been tried from the internet.
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync } from 'node:fs';
import { connect } from 'node:net';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { SipClient } from 'livekit-server-sdk';
import {
  Account, HOST, OURS, Phone, RawPeer, SDP, SPARE, buildBinaries, connectDb, digestHeader, invite, logOf, options, parseSip, prepareDir, respondRaw, sipUri, sleep, startOurs, startProcess, stopAll, stopProcess, tmp, toneFile, waitFor, writeConfig,
} from './lib.mjs';

let stack;
let counter = 0;
const nextNumber = () => `+1777${String(100000 + (++counter)).slice(1)}`;
const LOOPBACK = ['127.0.0.1/32'];

async function exchange(text, { wait = 400, port = OURS.sip } = {}) {
  const peer = await new RawPeer().open();
  try {
    await peer.send(port, text.replaceAll(`${HOST}:9;`, `${HOST}:${peer.port};`));
    await sleep(wait);
    return peer.messages;
  } finally {
    peer.close();
  }
}

// Workers come and go in this file, each with the limits that test is about.
async function withWorker(sip, fn) {
  await Promise.all(stack.workers.map((w) => stopProcess(w)));
  stack.workers.length = 0;
  const config = writeConfig('config-abuse.yaml', { sip });
  const w = await stack.startWorker('sip-worker-abuse', config);
  try {
    await fn(w);
  } finally {
    await Promise.all(stack.workers.map((x) => stopProcess(x)));
    stack.workers.length = 0;
  }
}

before(async () => {
  prepareDir();
  await buildBinaries();
  stack = await startOurs({ config: writeConfig('config.yaml'), worker: false });
}, { timeout: 240000 });

after(stopAll);

describe('who gets in', () => {
  it('a call that matches no trunk is refused, with rules or without', { timeout: 30000 }, async () => {
    await withWorker({}, async () => {
      // A rule that would take anyone, if that were all it took. The reference lets this through to the rules: this does not.
      const rule = await stack.sip.createSipDispatchRule({ type: 'direct', roomName: 'open-door' });
      try {
        const res = await exchange(invite(OURS.sip, { to: nextNumber() }));
        assert.equal(res.find((m) => m.status >= 200)?.status, 403, JSON.stringify(res.map((m) => m.first)));
      } finally {
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
      }
      const rooms = await stack.rooms.listRooms();
      assert.ok(!rooms.some((r) => r.name === 'open-door'), 'a stranger got a room made');
    });
  });

  it('a trunk\'s allow-list keeps out everyone else', { timeout: 30000 }, async () => {
    await withWorker({}, async () => {
      const number = nextNumber();
      const trunk = await stack.sip.createSipInboundTrunk('elsewhere', [number], { allowedAddresses: ['203.0.113.0/24'] });
      const rule = await stack.sip.createSipDispatchRule({ type: 'direct', roomName: 'r' }, { trunkIds: [trunk.sipTrunkId] });
      try {
        const res = await exchange(invite(OURS.sip, { to: number }));
        assert.equal(res.find((m) => m.status >= 200)?.status, 403);
      } finally {
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('an old trunk with no checks stops working when checks are required', { timeout: 30000 }, async () => {
    // Only possible by turning the setting off, making it, and turning it back on. The worker has to hold the line by itself.
    const db = await connectDb();
    await withWorker({}, async () => {
      const number = nextNumber();
      const trunk = await stack.sip.createSipInboundTrunk('was open', [number], { allowedAddresses: LOOPBACK });
      await db.query(`UPDATE sip_trunks SET info = info - 'allowed_addresses' WHERE id = $1`, [trunk.sipTrunkId]);
      const rule = await stack.sip.createSipDispatchRule({ type: 'direct', roomName: 'r2' }, { trunkIds: [trunk.sipTrunkId] });
      try {
        const res = await exchange(invite(OURS.sip, { to: number }));
        assert.equal(res.find((m) => m.status >= 200)?.status, 403);
      } finally {
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
    await db.end();
  });

  it('an operator can allow a trunk with no checks, when something else does the checking', { timeout: 30000 }, async () => {
    const db = await connectDb();
    await withWorker({ allow_unauthenticated_trunks: true }, async () => {
      const number = nextNumber();
      const trunk = await stack.sip.createSipInboundTrunk('behind a proxy', [number], { allowedAddresses: LOOPBACK });
      await db.query(`UPDATE sip_trunks SET info = info - 'allowed_addresses' WHERE id = $1`, [trunk.sipTrunkId]);
      const rule = await stack.sip.createSipDispatchRule({ type: 'direct', roomName: `proxied-${process.pid}` }, { trunkIds: [trunk.sipTrunkId] });
      try {
        const res = await exchange(invite(OURS.sip, { to: number }));
        assert.ok(res.some((m) => m.status === 180), JSON.stringify(res.map((m) => m.first)));
      } finally {
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
    await db.end();
  });

  it('digest is checked properly: the challenge, a wrong answer, a replay, an answer to something else', { timeout: 60000 }, async () => {
    await withWorker({}, async () => {
      const number = nextNumber();
      const trunk = await stack.sip.createSipInboundTrunk('digest', [number], { authUsername: 'carrier', authPassword: 'correct horse' });
      const rule = await stack.sip.createSipDispatchRule({ type: 'individual', roomPrefix: 'digest' }, { trunkIds: [trunk.sipTrunkId] });
      const uri = `sip:${number}@${HOST}:${OURS.sip}`;
      try {
        const callId = `digest-${Date.now()}@tests`;
        const tag = 'abc';
        const first = await exchange(invite(OURS.sip, { to: number, callId, tag }));
        const challenged = first.find((m) => m.status === 407);
        assert.ok(challenged, 'expected a 407');
        const challenge = challenged.header('proxy-authenticate');
        assert.match(challenge, /^Digest /);
        assert.match(challenge, /algorithm=MD5/);
        assert.match(challenge, /qop=/);
        // A wrong password is refused, with no second chance on that request.
        const wrong = await exchange(invite(OURS.sip, { to: number, callId, tag, cseq: 2, headers: `Proxy-Authorization: ${digestHeader(challenge, { username: 'carrier', password: 'wrong', uri })}\r\n` }));
        assert.equal(wrong.find((m) => m.status >= 200)?.status, 403);
        // A wrong user name, likewise.
        const badUser = await exchange(invite(OURS.sip, { to: number, callId, tag, cseq: 3, headers: `Proxy-Authorization: ${digestHeader(challenge, { username: 'nobody', password: 'correct horse', uri })}\r\n` }));
        assert.equal(badUser.find((m) => m.status >= 200)?.status, 403);
        // An answer to this challenge is no use for another call.
        const elsewhere = await exchange(invite(OURS.sip, { to: number, callId: 'other-call@tests', tag, cseq: 1, headers: `Proxy-Authorization: ${digestHeader(challenge, { username: 'carrier', password: 'correct horse', uri })}\r\n` }));
        assert.equal(elsewhere.find((m) => m.status >= 200)?.status, 403);
        // A nonce made up from nothing.
        const madeUp = await exchange(invite(OURS.sip, { to: number, callId, tag, cseq: 4, headers: `Proxy-Authorization: ${digestHeader('realm="StreamKit", nonce="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"', { username: 'carrier', password: 'correct horse', uri })}\r\n` }));
        assert.equal(madeUp.find((m) => m.status >= 200)?.status, 403);
        // The right answer is let through, and the same answer sent again is not.
        const answer = digestHeader(challenge, { username: 'carrier', password: 'correct horse', uri });
        const right = await exchange(invite(OURS.sip, { to: number, callId, tag, cseq: 5, headers: `Proxy-Authorization: ${answer}\r\n` }));
        assert.ok(right.some((m) => m.status === 180), `no ringing: ${JSON.stringify(right.map((m) => m.first))}`);
        const replay = await exchange(invite(OURS.sip, { to: number, callId, tag, cseq: 6, headers: `Proxy-Authorization: ${answer}\r\n` }));
        assert.equal(replay.find((m) => m.status >= 200)?.status, 407, `replay: ${JSON.stringify(replay.map((m) => m.first))}`);
      } finally {
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('no password or digest answer reaches a log', { timeout: 40000 }, async () => {
    await withWorker({}, async () => {
      const number = nextNumber();
      const secret = 'secret-sip-password-never-logged';
      const trunk = await stack.sip.createSipInboundTrunk('quiet', [number], { authUsername: 'carrier', authPassword: secret });
      try {
        const first = await exchange(invite(OURS.sip, { to: number, callId: 'quiet@tests' }));
        const challenge = first.find((m) => m.status === 407).header('proxy-authenticate');
        const uri = `sip:${number}@${HOST}:${OURS.sip}`;
        const header = digestHeader(challenge, { username: 'carrier', password: secret, uri });
        await exchange(invite(OURS.sip, { to: number, callId: 'quiet@tests', cseq: 2, headers: `Proxy-Authorization: ${header}\r\n` }));
        await sleep(300);
      } finally {
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
      for (const name of ['server', 'sip-worker-abuse']) {
        const log = logOf(name);
        assert.ok(!log.includes(secret), `the password is in the ${name} log`);
        assert.ok(!/Proxy-Authorization|response="/.test(log), `a digest answer is in the ${name} log`);
      }
    });
  });
});

describe('scanners and floods', () => {
  it('known scanning tools get no answer at all', { timeout: 30000 }, async () => {
    await withWorker({}, async () => {
      for (const userAgent of ['friendly-scanner', 'sipvicious', 'sipcli/v1.8']) {
        assert.equal((await exchange(invite(OURS.sip, { to: nextNumber(), userAgent }))).length, 0, `${userAgent} got an answer to an INVITE`);
        assert.equal((await exchange(options(OURS.sip, { userAgent }).replace(`${HOST}:9;`, `${HOST}:9;`))).length, 0, `${userAgent} got an answer to an OPTIONS`);
      }
    });
  });

  it('with silent rejects, a stranger learns nothing, and a carrier on an allow-list still gets answers', { timeout: 40000 }, async () => {
    await withWorker({ silent_rejects: true }, async () => {
      const number = nextNumber();
      assert.equal((await exchange(invite(OURS.sip, { to: number }))).length, 0, 'a stranger got an answer to an INVITE');
      assert.equal((await exchange(options(OURS.sip))).length, 0, 'a stranger got an answer to an OPTIONS');
      const id = `r${Date.now()}`;
      assert.equal((await exchange(`REGISTER sip:${HOST} SIP/2.0\r\nVia: SIP/2.0/UDP ${HOST}:9;branch=z9hG4bK${id}\r\nFrom: <sip:a@${HOST}>;tag=${id}\r\nTo: <sip:a@${HOST}>\r\nCall-ID: ${id}\r\nCSeq: 1 REGISTER\r\nMax-Forwards: 70\r\nContent-Length: 0\r\n\r\n`)).length, 0, 'a stranger got an answer to a REGISTER');
    });
    const number = nextNumber();
    const trunk = await stack.sip.createSipInboundTrunk('carrier', [number], { allowedAddresses: LOOPBACK });
    try {
      await withWorker({ silent_rejects: true }, async () => {
        await sleep(300);
        const res = await exchange(options(OURS.sip));
        assert.equal(res[0]?.status, 200, 'a carrier\'s health check got no answer');
        assert.match(res[0].header('allow'), /INVITE/);
      });
    } finally {
      await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
    }
  });

  it('a flood from one address is cut off', { timeout: 40000 }, async () => {
    await withWorker({ invites_per_second: 5, invite_burst: 10 }, async () => {
      const peer = await new RawPeer().open();
      try {
        const started = Date.now();
        for (let i = 0; i < 300; i++) await peer.send(OURS.sip, invite(OURS.sip, { to: '+19990000', localPort: peer.port }));
        const seconds = (Date.now() - started) / 1000;
        await sleep(500);
        // The bucket holds 10 and fills at 5 a second, for however long this took to send and for the wait after it.
        const allowed = 10 + 5 * (seconds + 0.5);
        const answered = peer.messages.filter((m) => m.status >= 200).length;
        assert.ok(answered <= allowed, `${answered} of 300 got answers in ${seconds}s, when ${allowed} should have`);
        assert.ok(answered >= 5, `only ${answered} got answers`);
      } finally {
        peer.close();
      }
    });
  });

  it('a source that keeps getting refused is banned', { timeout: 40000 }, async () => {
    await withWorker({ ban_after: 5, ban_seconds: 3 }, async () => {
      for (let i = 0; i < 6; i++) await exchange(invite(OURS.sip, { to: '+19990001' }), { wait: 60 });
      assert.equal((await exchange(options(OURS.sip))).length, 0, 'still being answered after being banned');
      await sleep(3200);
      assert.equal((await exchange(options(OURS.sip)))[0]?.status, 200, 'the ban did not end');
    });
  });

  it('password guessing gets a source banned too', { timeout: 40000 }, async () => {
    await withWorker({ ban_after: 4, ban_seconds: 5 }, async () => {
      const number = nextNumber();
      const trunk = await stack.sip.createSipInboundTrunk('guess', [number], { authUsername: 'carrier', authPassword: 'hunter2hunter2' });
      try {
        const uri = `sip:${number}@${HOST}:${OURS.sip}`;
        const first = await exchange(invite(OURS.sip, { to: number, callId: 'guess@tests' }));
        const challenge = first.find((m) => m.status === 407).header('proxy-authenticate');
        for (let i = 0; i < 4; i++) {
          await exchange(invite(OURS.sip, { to: number, callId: 'guess@tests', cseq: 2 + i, headers: `Proxy-Authorization: ${digestHeader(challenge, { username: 'carrier', password: `guess${i}`, uri })}\r\n` }), { wait: 60 });
        }
        assert.equal((await exchange(options(OURS.sip))).length, 0, 'guessing did not get this source banned');
      } finally {
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('malformed messages get no crash and no call', { timeout: 40000 }, async () => {
    await withWorker({}, async (worker) => {
      const peer = await new RawPeer().open();
      const bad = [
        'not sip at all\r\n\r\n',
        'INVITE\r\n\r\n',
        'INVITE sip:x SIP/2.0\r\n\r\n',
        `INVITE sip:a@${HOST} SIP/2.0\r\nVia: SIP/2.0/UDP ${HOST}:${peer.port}\r\n\r\n`,
        `INVITE sip:a@${HOST} SIP/2.0\r\nVia: nonsense\r\nFrom: x\r\nTo: y\r\nCall-ID: z\r\nCSeq: a b\r\nContent-Length: 99999\r\n\r\n`,
        `INVITE sip:a@${HOST} SIP/2.0\r\nVia: SIP/2.0/UDP ${HOST}:${peer.port};branch=z9hG4bK1\r\nFrom: <sip:a@b>;tag=1\r\nTo: <sip:c@d>\r\nCall-ID: 1\r\nCSeq: 1 INVITE\r\nContent-Length: -5\r\n\r\n`,
        `${'A'.repeat(60000)}\r\n\r\n`,
        Buffer.alloc(1400).fill(0xff).toString('latin1'),
        invite(OURS.sip, { to: 'x'.repeat(5000), localPort: peer.port }),
        invite(OURS.sip, { to: nextNumber(), from: '<script>alert(1)</script>', localPort: peer.port }),
        invite(OURS.sip, { to: nextNumber(), sdp: 'this is not sdp', localPort: peer.port }),
      ];
      for (const text of bad) await peer.send(OURS.sip, text);
      await sleep(600);
      peer.close();
      assert.equal(worker.exitCode, null, 'the worker died');
      assert.equal((await exchange(options(OURS.sip)))[0]?.status, 200, 'the worker stopped answering');
      assert.ok(!/panic:|goroutine \d+ \[/.test(logOf('sip-worker-abuse')), 'a panic in the log');
    });
  });

  it('half-open TCP connections cannot use up the service', { timeout: 40000 }, async () => {
    await withWorker({}, async () => {
      const sockets = [];
      for (let i = 0; i < 100; i++) {
        sockets.push(await new Promise((resolve) => {
          const s = connect(OURS.sip, HOST, () => resolve(s));
          s.on('error', () => resolve(s));
          s.resume();
        }));
      }
      await sleep(300);
      // Only some of these were let in. The port still answers for others, on UDP and TCP.
      assert.equal((await exchange(options(OURS.sip)))[0]?.status, 200);
      // And a connection that says nothing does not stay.
      await waitFor('every silent connection to be closed', () => sockets.every((s) => s.destroyed || s.readableEnded || s.closed), { timeout: 15000, every: 200 });
      for (const s of sockets) s.destroy();
    });
  });

  it('stray in-dialog requests get nowhere', { timeout: 30000 }, async () => {
    await withWorker({}, async () => {
      for (const method of ['BYE', 'NOTIFY', 'UPDATE', 'INFO']) {
        const res = await exchange(invite(OURS.sip, { method, to: '+1', toTag: 'none', sdp: null, cseq: 2 }));
        assert.equal(res.find((m) => m.status >= 200)?.status, 481, method);
      }
      const reinvite = await exchange(invite(OURS.sip, { to: '+1', toTag: 'none' }));
      assert.equal(reinvite.find((m) => m.status >= 200)?.status, 481);
      const cancel = await exchange(invite(OURS.sip, { method: 'CANCEL', to: '+1', sdp: null }));
      assert.equal(cancel.find((m) => m.status >= 200)?.status, 481);
    });
  });
});

describe('no open relay', () => {
  it('an INVITE for somewhere else is never passed on', { timeout: 40000 }, async () => {
    await withWorker({}, async () => {
      // Something listening for what would be a relayed call.
      const target = await new RawPeer().open();
      const number = nextNumber();
      const trunk = await stack.sip.createSipInboundTrunk('real', [number], { allowedAddresses: LOOPBACK });
      const rule = await stack.sip.createSipDispatchRule({ type: 'individual', roomPrefix: 'relay' }, { trunkIds: [trunk.sipTrunkId] });
      try {
        // To a number that is not ours, at an address that is not ours, and to an allowed number with a Route header pointing out.
        await exchange(invite(OURS.sip, { to: '+19005550100', host: HOST, uri: `sip:+19005550100@${HOST}:${target.port}` }));
        await exchange(invite(OURS.sip, { to: number, uri: `sip:${number}@${HOST}:${target.port}`, headers: `Route: <sip:${HOST}:${target.port};lr>\r\n` }));
        await sleep(800);
        assert.equal(target.messages.length, 0, 'something was sent on');
      } finally {
        target.close();
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('a phone in a call cannot have it transferred: REFER from outside is refused', { timeout: 60000 }, async () => {
    await withWorker({}, async () => {
      const tone = toneFile('abuse-tone.wav', 1000);
      const target = await new RawPeer().open();
      const phone = new Phone('referrer', { play: tone, autoAnswer: 200, user: '+15550177' });
      const room = `refer-${process.pid}`;
      const trunk = await stack.sip.createSipOutboundTrunk('real', `${HOST}:${phone.sipPort}`, ['+15550142']);
      try {
        await phone.ready();
        await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'phone-out', waitUntilAnswered: true });
        await phone.confirmed();
        phone.mark();
        // pjsua's own transfer command sends a REFER inside the dialog.
        phone.send('x');
        phone.send(`sip:relayed@${HOST}:${target.port}`);
        await phone.waitForLog(/405|Method Not Allowed/, 8000);
        await sleep(500);
        assert.equal(target.messages.length, 0, 'the REFER made this service call out');
        assert.equal((await stack.rooms.listParticipants(room)).length, 1, 'the call was dropped');
        await stack.rooms.removeParticipant(room, 'phone-out');
        await phone.disconnected();
      } finally {
        await phone.stop();
        target.close();
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });
});

describe('limits', () => {
  it('a project cannot have more calls than its limit, and others are not affected', { timeout: 90000 }, async () => {
    await withWorker({ max_calls_per_project: 2 }, async () => {
      const tone = toneFile('abuse-tone.wav', 1000);
      const number = nextNumber();
      const room = `limit-${process.pid}`;
      const trunk = await stack.sip.createSipInboundTrunk('limited', [number], { allowedAddresses: LOOPBACK });
      const rule = await stack.sip.createSipDispatchRule({ type: 'direct', roomName: room }, { trunkIds: [trunk.sipTrunkId] });
      const other = await Account.signUp(stack.base, 'unaffected');
      const otherSip = new SipClient(stack.base, other.apiKey, other.apiSecret);
      const otherNumber = nextNumber();
      const otherTrunk = await otherSip.createSipInboundTrunk('theirs', [otherNumber], { allowedAddresses: LOOPBACK });
      const otherRule = await otherSip.createSipDispatchRule({ type: 'direct', roomName: 'theirs' }, { trunkIds: [otherTrunk.sipTrunkId] });
      const phones = [];
      try {
        // Two phones in a room answer each other, so nobody else is needed.
        for (const user of ['+15551001', '+15551002']) {
          const p = new Phone(`limit-${user}`, { play: tone, user });
          phones.push(p);
          await p.ready();
          p.call(sipUri(stack, number));
        }
        await phones[0].confirmed();
        await phones[1].confirmed();
        const res = await exchange(invite(OURS.sip, { to: number, from: '+15551003' }));
        const final = res.find((m) => m.status >= 200);
        assert.equal(final?.status, 503, JSON.stringify(res.map((m) => m.first)));
        assert.ok(final.header('retry-after'), 'a 503 should say when to try again');
        // Another project's call is not held up by this one being full.
        const theirs = await exchange(invite(OURS.sip, { to: otherNumber, from: '+15551004' }));
        assert.ok(theirs.some((m) => m.status === 180), JSON.stringify(theirs.map((m) => m.first)));
        // One leaves, and there is room again.
        phones[0].hangup();
        await phones[0].disconnected();
        await sleep(500);
        const again = new Phone('limit-again', { play: tone, user: '+15551003' });
        phones.push(again);
        await again.ready();
        again.call(sipUri(stack, number));
        await again.confirmed();
      } finally {
        await Promise.all(phones.map((p) => p.stop()));
        await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
        await otherSip.deleteSipDispatchRule(otherRule.sipDispatchRuleId);
        await otherSip.deleteSipTrunk(otherTrunk.sipTrunkId);
      }
    });
  });

  it('the call limit also holds for outbound calls, through the API, and so does the rate limit', { timeout: 60000 }, async () => {
    await withWorker({}, async () => {
      // A second server on the same database, with limits of its own, since they are the API's to enforce.
      const config = writeConfig('config-limits.yaml', { sip: { max_calls_per_project: 1, max_outbound_per_minute: 3 } });
      writeFileSync(config, readFileSync(config, 'utf8').replace(`port: ${OURS.http}`, `port: ${SPARE.http}`).replace(`udp_port: ${OURS.udp}`, `udp_port: ${SPARE.udp}`).replace(`tcp_port: ${OURS.tcp}`, `tcp_port: ${SPARE.tcp}`));
      const server = startProcess('server-limits', join(tmp, 'bin/streamkit-server'), ['--dev', '--config', config]);
      const tone = toneFile('abuse-tone.wav', 1000);
      const callee = new Phone('limit-callee', { play: tone, autoAnswer: 200, user: '+15550177' });
      const room = `limit-out-${process.pid}`;
      const trunk = await stack.sip.createSipOutboundTrunk('limited', `${HOST}:${callee.sipPort}`, ['+15550142']);
      try {
        const base = `http://${HOST}:${SPARE.http}`;
        await waitFor('the second server', async () => (await fetch(`${base}/`)).ok);
        const limited = new SipClient(base, stack.apiKey, stack.apiSecret);
        await callee.ready();
        await limited.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'first', waitUntilAnswered: true });
        await callee.confirmed();
        await assert.rejects(limited.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'second' }), /as many calls/);
        await stack.rooms.removeParticipant(room, 'first');
        await callee.disconnected();
        await waitFor('the first call to end', async () => {
          const list = await stack.account.ok('GET', '/sip/calls');
          return list.items.every((c) => !['queued', 'dialing', 'ringing', 'active'].includes(c.status));
        });
        // Two used so far, of three a minute. The third goes through and the fourth does not.
        await limited.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'third' });
        await stack.rooms.removeParticipant(room, 'third').catch(() => {});
        await assert.rejects(limited.createSipParticipant(trunk.sipTrunkId, '+15550177', room, { participantIdentity: 'fourth' }), /too many calls/);
      } finally {
        await stopProcess(server);
        await callee.stop();
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('an outbound call with no answer gives up after its ringing timeout, with a CANCEL', { timeout: 60000 }, async () => {
    await withWorker({}, async () => {
      const peer = await new RawPeer().open();
      const seen = [];
      peer.socket.on('message', (m) => {
        const text = m.toString();
        seen.push(text.split('\r\n')[0]);
        if (text.startsWith('INVITE') || text.startsWith('CANCEL')) {
          const via = /^Via: (.*)$/m.exec(text)[1].trim();
          const header = (n) => new RegExp(`^${n}: (.*)$`, 'm').exec(text)[1].trim();
          const port = /rport=(\d+)/.exec(via)?.[1] ?? /:(\d+)/.exec(via)[1];
          const status = text.startsWith('INVITE') ? '180 Ringing' : '200 OK';
          peer.send(Number(port), `SIP/2.0 ${status}\r\nVia: ${via}\r\nFrom: ${header('From')}\r\nTo: ${header('To')};tag=ring\r\nCall-ID: ${header('Call-ID')}\r\nCSeq: ${header('CSeq')}\r\nContent-Length: 0\r\n\r\n`);
        }
      });
      const trunk = await stack.sip.createSipOutboundTrunk('quiet', `${HOST}:${peer.port}`, ['+15550142']);
      try {
        const started = Date.now();
        const res = await stack.twirp('CreateSIPParticipant', { sip_trunk_id: trunk.sipTrunkId, sip_call_to: '+15550177', room_name: `ring-${process.pid}`, wait_until_answered: true, ringing_timeout: '2s' });
        assert.notEqual(res.status, 200, res.text);
        assert.ok(Date.now() - started >= 1800, 'gave up too soon');
        assert.ok(Date.now() - started < 12000, 'took far too long to give up');
        await waitFor('a CANCEL', () => seen.some((l) => l.startsWith('CANCEL')), { timeout: 5000 });
      } finally {
        peer.close();
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('a phone that sends no media is hung up', { timeout: 70000 }, async () => {
    await withWorker({ media_timeout_seconds: 2 }, async () => {
      const peer = await new RawPeer().open();
      const seen = [];
      // Something that answers a call and then sends no RTP.
      peer.socket.on('message', (m) => {
        const msg = parseSip(m.toString());
        seen.push(msg);
        if (msg.first.startsWith('INVITE')) respondRaw(peer, msg, 200, 'OK', `Contact: <sip:callee@${HOST}:${peer.port}>\r\n`, SDP(40000));
        if (msg.first.startsWith('BYE')) respondRaw(peer, msg, 200, 'OK');
      });
      const trunk = await stack.sip.createSipOutboundTrunk('silent', `${HOST}:${peer.port}`, ['+15550142']);
      try {
        const info = await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', `silent-${process.pid}`, { participantIdentity: 'phone-out', waitUntilAnswered: true });
        const call = await waitFor('the call to be ended for lack of media', async () => {
          const c = await stack.account.ok('GET', `/sip/calls/${info.sipCallId}`);
          return c.status === 'failed' ? c : null;
        }, { timeout: 45000, every: 500 });
        assert.equal(call.disconnect_reason, 'MEDIA_FAILURE');
        await waitFor('a BYE', () => seen.some((m) => m.first.startsWith('BYE')), { timeout: 10000 });
      } finally {
        peer.close();
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });

  it('a call has a time limit', { timeout: 60000 }, async () => {
    await withWorker({ max_call_seconds: 3 }, async () => {
      const tone = toneFile('abuse-tone.wav', 1000);
      const phone = new Phone('short', { play: tone, autoAnswer: 200, user: '+15550177' });
      const trunk = await stack.sip.createSipOutboundTrunk('short', `${HOST}:${phone.sipPort}`, ['+15550142']);
      try {
        await phone.ready();
        await stack.sip.createSipParticipant(trunk.sipTrunkId, '+15550177', `short-${process.pid}`, { participantIdentity: 'phone-out', waitUntilAnswered: true });
        await phone.confirmed();
        const m = await phone.disconnected(10000);
        assert.equal(m[1], '200');
      } finally {
        await phone.stop();
        await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      }
    });
  });
});

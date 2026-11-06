// The API that SDKs and CLIs use for SIP, through the official clients, against both this server and the reference. A test in the first
// group only counts if the reference passes it too. The second group is where this service is stricter on purpose.
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { existsSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, before, describe, it } from 'node:test';
import { promisify } from 'node:util';
import { AccessToken, SipClient } from 'livekit-server-sdk';
import {
  Account, HOST, OURS, SPARE, buildBinaries, findUp, connectDb, logOf, prepareDir, referenceAvailable, startOurs, startProcess, startReference, stopAll, stopProcess, stopReference, tmp, waitFor, writeConfig,
} from './lib.mjs';

const run = promisify(execFile);
// An interpreter with the official livekit-api package, and a build of the official CLI. Each test says so if it has to skip.
const PYTHON = process.env.SIP_TEST_PYTHON ?? findUp('.toolchain/venvs/server-python-3.11/bin/python');
const LK = process.env.LK ?? findUp('.toolchain/bin/lk');

let counter = 0;
const nextNumber = () => `+1666${String(100000 + (++counter)).slice(1)}`;
const CARRIER = '203.0.113.5:5060';

function shared(getStack) {
  it('inbound trunks: create, read, list, change and delete through the official client', async () => {
    const { sip } = getStack();
    const number = nextNumber();
    const made = await sip.createSipInboundTrunk('front desk', [number], { allowedAddresses: ['203.0.113.0/24'], allowedNumbers: ['+15550100'], metadata: 'm' });
    assert.match(made.sipTrunkId, /^ST_/);
    assert.equal(made.name, 'front desk');
    assert.deepEqual(made.numbers, [number]);
    assert.deepEqual(made.allowedAddresses, ['203.0.113.0/24']);
    assert.deepEqual(made.allowedNumbers, ['+15550100']);
    assert.equal(made.metadata, 'm');
    const list = await sip.listSipInboundTrunk();
    assert.ok(list.some((t) => t.sipTrunkId === made.sipTrunkId));
    const changed = await sip.updateSipInboundTrunkFields(made.sipTrunkId, { name: 'reception', allowedAddresses: undefined });
    assert.equal(changed.name, 'reception');
    assert.deepEqual(changed.numbers, [number], 'an update changes only what it names');
    const deleted = await sip.deleteSipTrunk(made.sipTrunkId);
    assert.equal(deleted.sipTrunkId, made.sipTrunkId);
    assert.ok(!(await sip.listSipInboundTrunk()).some((t) => t.sipTrunkId === made.sipTrunkId));
  });

  it('outbound trunks: create, list, change and delete through the official client', async () => {
    const { sip } = getStack();
    const made = await sip.createSipOutboundTrunk('carrier', CARRIER, ['+15550142'], { transport: 2, destinationCountry: 'US', headers: { 'X-Team': 'blue' } });
    assert.match(made.sipTrunkId, /^ST_/);
    assert.equal(made.address, CARRIER);
    assert.equal(made.transport, 2);
    assert.equal(made.destinationCountry, 'US');
    assert.equal(made.headers['X-Team'], 'blue');
    const changed = await sip.updateSipOutboundTrunkFields(made.sipTrunkId, { name: 'renamed' });
    assert.equal(changed.name, 'renamed');
    assert.equal(changed.address, CARRIER);
    assert.ok((await sip.listSipOutboundTrunk()).some((t) => t.sipTrunkId === made.sipTrunkId));
    await sip.deleteSipTrunk(made.sipTrunkId);
    assert.ok(!(await sip.listSipOutboundTrunk()).some((t) => t.sipTrunkId === made.sipTrunkId));
  });

  it('dispatch rules: all three kinds, through the official client and the raw API', async () => {
    const stack = getStack();
    const { sip } = stack;
    const direct = await sip.createSipDispatchRule({ type: 'direct', roomName: 'lobby' }, { name: 'direct', trunkIds: ['ST_one'], attributes: { a: 'b' }, metadata: 'mm' });
    assert.match(direct.sipDispatchRuleId, /^SDR_/);
    assert.equal(direct.rule.rule.case, 'dispatchRuleDirect');
    assert.equal(direct.rule.rule.value.roomName, 'lobby');
    assert.equal(direct.attributes.a, 'b');
    const each = await sip.createSipDispatchRule({ type: 'individual', roomPrefix: 'call' }, { name: 'each', trunkIds: ['ST_two'] });
    assert.equal(each.rule.rule.case, 'dispatchRuleIndividual');
    const callee = await stack.twirp('CreateSIPDispatchRule', { dispatch_rule: { name: 'line', trunk_ids: ['ST_three'], rule: { dispatch_rule_callee: { room_prefix: 'line', randomize: true } } } });
    assert.equal(callee.status, 200, callee.text);
    assert.equal(callee.body.rule.dispatch_rule_callee.room_prefix, 'line');
    const changed = await sip.updateSipDispatchRuleFields(direct.sipDispatchRuleId, { name: 'renamed' });
    assert.equal(changed.name, 'renamed');
    assert.equal(changed.rule.rule.value.roomName, 'lobby');
    const list = await sip.listSipDispatchRule();
    for (const id of [direct.sipDispatchRuleId, each.sipDispatchRuleId, callee.body.sip_dispatch_rule_id]) {
      assert.ok(list.some((r) => r.sipDispatchRuleId === id), `${id} is missing`);
      await sip.deleteSipDispatchRule(id);
    }
    assert.equal((await sip.listSipDispatchRule()).filter((r) => r.sipDispatchRuleId === direct.sipDispatchRuleId).length, 0);
  });

  it('the old request shape for a dispatch rule still works', async () => {
    const stack = getStack();
    const res = await stack.twirp('CreateSIPDispatchRule', { name: 'legacy', trunk_ids: ['ST_four'], rule: { dispatch_rule_direct: { room_name: 'old' } } });
    assert.equal(res.status, 200, res.text);
    assert.equal(res.body.rule.dispatch_rule_direct.room_name, 'old');
    await stack.sip.deleteSipDispatchRule(res.body.sip_dispatch_rule_id);
  });

  it('every method needs its grant, and says so the same way', async () => {
    const stack = getStack();
    for (const [method, body] of [
      ['CreateSIPInboundTrunk', { trunk: { numbers: ['+1'], allowed_addresses: ['203.0.113.1'] } }],
      ['ListSIPInboundTrunk', {}],
      ['ListSIPOutboundTrunk', {}],
      ['ListSIPDispatchRule', {}],
      ['DeleteSIPTrunk', { sip_trunk_id: 'ST_x' }],
      ['DeleteSIPDispatchRule', { sip_dispatch_rule_id: 'SDR_x' }],
      ['CreateSIPParticipant', { sip_trunk_id: 'ST_x', sip_call_to: '+1', room_name: 'r' }],
    ]) {
      const res = await stack.twirp(method, body, { sip: {} });
      assert.equal(res.status, 401, `${method}: ${res.text}`);
      assert.equal(res.body.code, 'unauthenticated', method);
    }
    // Admin does not give the right to place calls, nor the other way around.
    const call = await stack.twirp('CreateSIPParticipant', { sip_trunk_id: 'ST_x', sip_call_to: '+1', room_name: 'r' }, { sip: { admin: true } });
    assert.equal(call.status, 401, call.text);
    const admin = await stack.twirp('ListSIPInboundTrunk', {}, { sip: { call: true } });
    assert.equal(admin.status, 401, admin.text);
  });

  it('a request with no valid token is refused', async () => {
    const stack = getStack();
    const res = await fetch(`${stack.base}/twirp/livekit.SIP/ListSIPInboundTrunk`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    assert.equal(res.status, 401);
    assert.equal((await res.json()).code, 'unauthenticated');
  });

  it('asks for the things a request has to have', async () => {
    const stack = getStack();
    const cases = [
      ['CreateSIPInboundTrunk', { trunk: { sip_trunk_id: 'ST_set', numbers: ['+1'], allowed_addresses: ['203.0.113.1'] } }, 'invalid_argument'],
      ['CreateSIPInboundTrunk', {}, 'invalid_argument'],
      ['CreateSIPOutboundTrunk', { trunk: { address: CARRIER } }, 'invalid_argument'],
      ['CreateSIPOutboundTrunk', { trunk: { numbers: ['+1'] } }, 'invalid_argument'],
      ['CreateSIPOutboundTrunk', { trunk: { numbers: ['+1'], address: 'sip:carrier.example' } }, 'invalid_argument'],
      ['CreateSIPOutboundTrunk', { trunk: { numbers: ['+1'], address: 'carrier.example;transport=tcp' } }, 'invalid_argument'],
      ['CreateSIPOutboundTrunk', { trunk: { numbers: ['+1'], address: CARRIER, headers: { Contact: 'x' } } }, 'invalid_argument'],
      ['CreateSIPDispatchRule', { dispatch_rule: { name: 'nothing' } }, 'invalid_argument'],
      ['CreateSIPParticipant', { sip_call_to: '+1', room_name: 'r' }, 'invalid_argument'],
      ['CreateSIPParticipant', { sip_trunk_id: 'ST_x', room_name: 'r' }, 'invalid_argument'],
      ['CreateSIPParticipant', { sip_trunk_id: 'ST_x', sip_call_to: 'a@b', room_name: 'r' }, 'invalid_argument'],
      ['GetSIPInboundTrunk', {}, 'invalid_argument'],
      ['DeleteSIPTrunk', {}, 'invalid_argument'],
      ['DeleteSIPDispatchRule', {}, 'invalid_argument'],
      ['UpdateSIPInboundTrunk', { sip_trunk_id: '', update: {} }, 'invalid_argument'],
    ];
    for (const [method, body, code] of cases) {
      const res = await stack.twirp(method, body);
      assert.equal(res.body?.code, code, `${method} ${JSON.stringify(body)}: HTTP ${res.status}: ${res.text}`);
    }
  });

  it('says when something is not there', async () => {
    const stack = getStack();
    for (const [method, body] of [
      ['GetSIPInboundTrunk', { sip_trunk_id: 'ST_nothing' }],
      ['GetSIPOutboundTrunk', { sip_trunk_id: 'ST_nothing' }],
      ['UpdateSIPInboundTrunk', { sip_trunk_id: 'ST_nothing', update: { name: 'x' } }],
      ['UpdateSIPOutboundTrunk', { sip_trunk_id: 'ST_nothing', update: { name: 'x' } }],
      ['UpdateSIPDispatchRule', { sip_dispatch_rule_id: 'SDR_nothing', update: { name: 'x' } }],
      ['DeleteSIPDispatchRule', { sip_dispatch_rule_id: 'SDR_nothing' }],
      ['CreateSIPParticipant', { sip_trunk_id: 'ST_nothing', sip_call_to: '+1', room_name: 'r' }],
    ]) {
      const res = await stack.twirp(method, body);
      assert.equal(res.status, 404, `${method}: ${res.text}`);
      assert.equal(res.body.code, 'not_found', method);
    }
    // Deleting a trunk that is not there is not an error.
    const gone = await stack.twirp('DeleteSIPTrunk', { sip_trunk_id: 'ST_nothing' });
    assert.equal(gone.status, 200, gone.text);
    assert.equal(gone.body.sip_trunk_id, 'ST_nothing');
  });

  it('refuses two trunks that could claim the same call', async () => {
    const { sip } = getStack();
    const number = nextNumber();
    const a = await sip.createSipInboundTrunk('a', [number], { allowedAddresses: ['203.0.113.0/24'] });
    try {
      await assert.rejects(sip.createSipInboundTrunk('b', [number], { allowedAddresses: ['203.0.113.0/24'] }), /onflicting/);
      // A different number is fine, and so is the same number from different callers.
      const c = await sip.createSipInboundTrunk('c', [nextNumber()], { allowedAddresses: ['203.0.113.0/24'] });
      await sip.deleteSipTrunk(c.sipTrunkId);
    } finally {
      await sip.deleteSipTrunk(a.sipTrunkId);
    }
  });

  it('refuses two rules for the same thing', async () => {
    const { sip } = getStack();
    const a = await sip.createSipDispatchRule({ type: 'direct', roomName: 'one' }, { trunkIds: ['ST_five'] });
    try {
      await assert.rejects(sip.createSipDispatchRule({ type: 'direct', roomName: 'two' }, { trunkIds: ['ST_five'] }), /same trunk/);
    } finally {
      await sip.deleteSipDispatchRule(a.sipDispatchRuleId);
    }
  });

  it('a list can ask for particular trunks and rules, in order', async () => {
    const stack = getStack();
    const a = await stack.sip.createSipOutboundTrunk('a', CARRIER, ['+15550142']);
    const b = await stack.sip.createSipOutboundTrunk('b', CARRIER, ['+15550143']);
    try {
      const res = await stack.twirp('ListSIPOutboundTrunk', { trunk_ids: [b.sipTrunkId, a.sipTrunkId] });
      assert.equal(res.status, 200, res.text);
      assert.deepEqual(res.body.items.map((t) => t.sip_trunk_id), [b.sipTrunkId, a.sipTrunkId]);
      const numbers = await stack.twirp('ListSIPOutboundTrunk', { numbers: ['+15550143'] });
      assert.ok(numbers.body.items.some((t) => t.sip_trunk_id === b.sipTrunkId));
      assert.ok(!numbers.body.items.some((t) => t.sip_trunk_id === a.sipTrunkId));
    } finally {
      await stack.sip.deleteSipTrunk(a.sipTrunkId);
      await stack.sip.deleteSipTrunk(b.sipTrunkId);
    }
  });

  it('protobuf bodies work as well as JSON', async () => {
    const stack = getStack();
    const at = new AccessToken(stack.apiKey, stack.apiSecret, { identity: 'tester' });
    at.addSIPGrant({ admin: true });
    // An empty message, in protobuf, is zero bytes. The official clients use JSON, and some others do not.
    const res = await fetch(`${stack.base}/twirp/livekit.SIP/ListSIPInboundTrunk`, { method: 'POST', headers: { 'Content-Type': 'application/protobuf', Authorization: `Bearer ${await at.toJwt()}` }, body: new Uint8Array() });
    assert.equal(res.status, 200);
    assert.match(res.headers.get('content-type'), /application\/protobuf/);
  });

  it('works through the official Python client', { skip: !(PYTHON && existsSync(PYTHON)) && 'no Python environment with livekit-api' }, async () => {
    const stack = getStack();
    const script = join(tmp, 'official_client.py');
    writeFileSync(script, `
import asyncio, os, sys
from livekit import api

async def main():
    lk = api.LiveKitAPI(os.environ['URL'], os.environ['KEY'], os.environ['SECRET'])
    made = await lk.sip.create_inbound_trunk(api.CreateSIPInboundTrunkRequest(trunk=api.SIPInboundTrunkInfo(name='py', numbers=[os.environ['NUMBER']], allowed_addresses=['203.0.113.0/24'])))
    listed = await lk.sip.list_inbound_trunk(api.ListSIPInboundTrunkRequest())
    assert any(t.sip_trunk_id == made.sip_trunk_id for t in listed.items)
    rule = await lk.sip.create_dispatch_rule(api.CreateSIPDispatchRuleRequest(dispatch_rule=api.SIPDispatchRuleInfo(name='py', trunk_ids=[made.sip_trunk_id], rule=api.SIPDispatchRule(dispatch_rule_direct=api.SIPDispatchRuleDirect(room_name='py')))))
    assert rule.sip_dispatch_rule_id.startswith('SDR_')
    out = await lk.sip.create_outbound_trunk(api.CreateSIPOutboundTrunkRequest(trunk=api.SIPOutboundTrunkInfo(name='py', address='203.0.113.5', numbers=['+15550142'])))
    assert out.address == '203.0.113.5'
    await lk.sip.delete_trunk(api.DeleteSIPTrunkRequest(sip_trunk_id=out.sip_trunk_id))
    await lk.sip.delete_dispatch_rule(api.DeleteSIPDispatchRuleRequest(sip_dispatch_rule_id=rule.sip_dispatch_rule_id))
    await lk.sip.delete_trunk(api.DeleteSIPTrunkRequest(sip_trunk_id=made.sip_trunk_id))
    await lk.aclose()
    print('ok')

asyncio.run(main())
`);
    const { stdout } = await run(PYTHON, ['-I', script], { env: { ...process.env, URL: stack.base, KEY: stack.apiKey, SECRET: stack.apiSecret, NUMBER: nextNumber() } });
    assert.match(stdout, /ok/);
  });

  it('works through the official CLI', { skip: !(LK && existsSync(LK)) && 'the official CLI has not been built' }, async () => {
    const stack = getStack();
    const lk = async (...args) => {
      const { stdout } = await run(LK, ['--url', stack.base, '--api-key', stack.apiKey, '--api-secret', stack.apiSecret, ...args], { cwd: tmp });
      return stdout;
    };
    const number = nextNumber();
    const trunkFile = join(tmp, `inbound-${stack.name}.json`);
    writeFileSync(trunkFile, JSON.stringify({ trunk: { name: 'cli', numbers: [number], allowed_addresses: ['203.0.113.0/24'] } }));
    const made = await lk('sip', 'inbound', 'create', trunkFile);
    const id = /ST_[A-Za-z0-9]+/.exec(made)?.[0];
    assert.ok(id, `no trunk ID in: ${made}`);
    assert.ok((await lk('sip', 'inbound', 'list')).includes(id));
    const ruleFile = join(tmp, `rule-${stack.name}.json`);
    writeFileSync(ruleFile, JSON.stringify({ dispatch_rule: { name: 'cli', trunk_ids: [id], rule: { dispatch_rule_direct: { room_name: 'cli' } } } }));
    const rule = /SDR_[A-Za-z0-9]+/.exec(await lk('sip', 'dispatch', 'create', ruleFile))?.[0];
    assert.ok(rule);
    assert.ok((await lk('sip', 'dispatch', 'list')).includes(rule));
    await lk('sip', 'dispatch', 'delete', rule);
    await lk('sip', 'inbound', 'delete', id);
    assert.ok(!(await lk('sip', 'inbound', 'list')).includes(id));
  });
}

const sealedPassword = 'carrier-password-that-must-stay-sealed';

describe('this server', () => {
  let stack;
  before(async () => {
    prepareDir();
    await buildBinaries();
    stack = await startOurs({ config: writeConfig('config-api.yaml'), worker: false });
  }, { timeout: 240000 });
  after(async () => {
    if (stack) await stopProcess(stack.server);
  });

  shared(() => stack);

  it('refuses headers that could corrupt a dialog or spoof a route, including ones the reference still allows', async () => {
    for (const name of ['Via', 'To', 'Route', 'Proxy-Authorization']) {
      const res = await stack.twirp('CreateSIPOutboundTrunk', { trunk: { numbers: ['+1'], address: CARRIER, headers: { [name]: 'x' } } });
      assert.equal(res.body?.code, 'invalid_argument', `${name}: ${res.text}`);
    }
    const res = await stack.twirp('CreateSIPOutboundTrunk', { trunk: { numbers: ['+1'], address: CARRIER, headers: { 'X-Good': 'a\r\nX-Evil: 1' } } });
    assert.equal(res.body?.code, 'invalid_argument', res.text);
  });

  it('never returns trunk credentials, by any route, and keeps them sealed', async () => {
    const { sip } = stack;
    const number = nextNumber();
    const inbound = await sip.createSipInboundTrunk('secret', [number], { authUsername: 'carrier-user-name', authPassword: sealedPassword });
    const outbound = await sip.createSipOutboundTrunk('secret', CARRIER, ['+15550142'], { authUsername: 'carrier-user-name', authPassword: sealedPassword });
    const seen = [];
    seen.push(JSON.stringify(inbound), JSON.stringify(outbound));
    for (const [method, body] of [
      ['ListSIPInboundTrunk', {}], ['ListSIPOutboundTrunk', {}], ['ListSIPTrunk', {}],
      ['GetSIPInboundTrunk', { sip_trunk_id: inbound.sipTrunkId }], ['GetSIPOutboundTrunk', { sip_trunk_id: outbound.sipTrunkId }],
      ['UpdateSIPInboundTrunk', { sip_trunk_id: inbound.sipTrunkId, update: { name: 'changed' } }],
      ['UpdateSIPOutboundTrunk', { sip_trunk_id: outbound.sipTrunkId, update: { name: 'changed' } }],
    ]) {
      const res = await stack.twirp(method, body);
      assert.equal(res.status, 200, `${method}: ${res.text}`);
      seen.push(res.text);
    }
    for (const path of ['/sip/trunks/inbound', '/sip/trunks/outbound', `/sip/trunks/inbound/${inbound.sipTrunkId}`, `/sip/trunks/outbound/${outbound.sipTrunkId}`]) {
      const res = await stack.account.req('GET', path);
      assert.equal(res.status, 200, res.text);
      seen.push(res.text);
    }
    for (const text of seen) {
      assert.ok(!text.includes(sealedPassword), `a password came back: ${text}`);
      assert.ok(!text.includes('carrier-user-name'), `a user name came back: ${text}`);
    }
    const rest = await stack.account.ok('GET', `/sip/trunks/inbound/${inbound.sipTrunkId}`);
    assert.equal(rest.has_credentials, true);
    // The database holds them only sealed.
    const db = await connectDb();
    try {
      const rows = await db.query(`SELECT info::text, encode(auth_sealed, 'escape') AS sealed FROM sip_trunks WHERE id = ANY($1)`, [[inbound.sipTrunkId, outbound.sipTrunkId]]);
      assert.equal(rows.rows.length, 2);
      for (const row of rows.rows) {
        assert.ok(!row.info.includes(sealedPassword));
        assert.ok(!row.info.includes('carrier-user-name'));
        assert.ok(row.sealed.length > sealedPassword.length, 'nothing is sealed');
        assert.ok(!row.sealed.includes(sealedPassword));
      }
    } finally {
      await db.end();
    }
    assert.ok(!logOf('server').includes(sealedPassword), 'a password reached the server log');
    await sip.deleteSipTrunk(inbound.sipTrunkId);
    await sip.deleteSipTrunk(outbound.sipTrunkId);
  });

  it('a trunk sent back without its credentials keeps them, and removing them is explicit', async () => {
    const { sip } = stack;
    const number = nextNumber();
    const made = await sip.createSipInboundTrunk('keep', [number], { authUsername: 'u1', authPassword: 'p1', allowedAddresses: ['203.0.113.0/24'] });
    const has = async () => (await stack.account.ok('GET', `/sip/trunks/inbound/${made.sipTrunkId}`)).has_credentials;
    const read = (await stack.twirp('GetSIPInboundTrunk', { sip_trunk_id: made.sipTrunkId })).body.trunk;
    const replaced = await stack.twirp('UpdateSIPInboundTrunk', { sip_trunk_id: made.sipTrunkId, replace: { ...read, name: 'replaced' } });
    assert.equal(replaced.status, 200, replaced.text);
    assert.equal(await has(), true, 'reading a trunk and sending it back lost its credentials');
    const cleared = await stack.twirp('UpdateSIPInboundTrunk', { sip_trunk_id: made.sipTrunkId, update: { auth_username: '', auth_password: '' } });
    assert.equal(cleared.status, 200, cleared.text);
    assert.equal(await has(), false);
    await sip.deleteSipTrunk(made.sipTrunkId);
  });

  it('will not create an inbound trunk that anyone on the internet could call', async () => {
    const { sip } = stack;
    await assert.rejects(sip.createSipInboundTrunk('open', [nextNumber()]), /allowed_addresses/, 'numbers are not a check');
    await assert.rejects(sip.createSipInboundTrunk('open', [nextNumber()], { allowedAddresses: ['0.0.0.0/0'] }), /allowed_addresses/, 'neither is the whole internet');
    await assert.rejects(sip.createSipInboundTrunk('open', [nextNumber()], { allowedAddresses: ['sip.example.com'] }), /allowed_addresses/, 'a From host can say anything');
    await assert.rejects(sip.createSipInboundTrunk('half', [nextNumber()], { authUsername: 'only-a-name' }), /together/);
    const made = await sip.createSipInboundTrunk('good', [nextNumber()], { allowedAddresses: ['203.0.113.9'] });
    await assert.rejects(stack.sip.updateSipInboundTrunkFields(made.sipTrunkId, { allowedAddresses: { clear: true } }));
    await sip.deleteSipTrunk(made.sipTrunkId);
  });

  it('keeps PINs sealed, shows that there is one, and keeps it when a rule comes back', async () => {
    const { sip } = stack;
    const made = await sip.createSipDispatchRule({ type: 'direct', roomName: 'vault', pin: '424242' }, { trunkIds: ['ST_six'] });
    assert.equal(made.rule.rule.value.pin, '****');
    const raw = await stack.twirp('ListSIPDispatchRule', {});
    assert.ok(!raw.text.includes('424242'), raw.text);
    const rule = raw.body.items.find((r) => r.sip_dispatch_rule_id === made.sipDispatchRuleId);
    const back = await stack.twirp('UpdateSIPDispatchRule', { sip_dispatch_rule_id: made.sipDispatchRuleId, replace: { ...rule, name: 'renamed' } });
    assert.equal(back.status, 200, back.text);
    const db = await connectDb();
    try {
      const row = await db.query(`SELECT info::text, pin_sealed IS NOT NULL AS sealed, encode(pin_sealed, 'escape') AS bytes FROM sip_dispatch_rules WHERE id = $1`, [made.sipDispatchRuleId]);
      assert.equal(row.rows[0].sealed, true, 'sending a rule back lost its PIN');
      assert.ok(!row.rows[0].info.includes('424242'));
      assert.ok(!row.rows[0].bytes.includes('424242'));
    } finally {
      await db.end();
    }
    await assert.rejects(sip.createSipDispatchRule({ type: 'direct', roomName: 'x', pin: '12a' }, { trunkIds: ['ST_seven'] }), /digits/);
    await sip.deleteSipDispatchRule(made.sipDispatchRuleId);
  });

  it('one project cannot see, change or use another project\'s trunks and rules', async () => {
    const other = await Account.signUp(stack.base, 'other');
    const theirs = new SipClient(stack.base, other.apiKey, other.apiSecret);
    const number = nextNumber();
    const trunk = await stack.sip.createSipInboundTrunk('mine', [number], { allowedAddresses: ['203.0.113.0/24'] });
    const out = await stack.sip.createSipOutboundTrunk('mine', CARRIER, ['+15550142'], { authUsername: 'u', authPassword: 'p' });
    const rule = await stack.sip.createSipDispatchRule({ type: 'direct', roomName: 'private' }, { trunkIds: [trunk.sipTrunkId] });
    try {
      assert.ok(!(await theirs.listSipInboundTrunk()).some((t) => t.sipTrunkId === trunk.sipTrunkId));
      assert.ok(!(await theirs.listSipOutboundTrunk()).some((t) => t.sipTrunkId === out.sipTrunkId));
      assert.ok(!(await theirs.listSipDispatchRule()).some((r) => r.sipDispatchRuleId === rule.sipDispatchRuleId));
      await assert.rejects(theirs.updateSipInboundTrunkFields(trunk.sipTrunkId, { name: 'stolen' }), /not exist/);
      await assert.rejects(theirs.updateSipOutboundTrunkFields(out.sipTrunkId, { name: 'stolen' }), /not exist/);
      await assert.rejects(theirs.updateSipDispatchRuleFields(rule.sipDispatchRuleId, { name: 'stolen' }), /not exist/);
      await assert.rejects(theirs.deleteSipDispatchRule(rule.sipDispatchRuleId), /not exist/);
      await theirs.deleteSipTrunk(trunk.sipTrunkId);
      assert.ok((await stack.sip.listSipInboundTrunk()).some((t) => t.sipTrunkId === trunk.sipTrunkId), 'another project deleted this trunk');
      await assert.rejects(theirs.createSipParticipant(out.sipTrunkId, '+15550177', 'room'), /not exist/, 'another project dialled through this trunk, at this project\'s cost');
      other.project = stack.account.project;
      const forbidden = await other.req('GET', '/sip/trunks/inbound');
      assert.equal(forbidden.status, 404, 'a person from another organization could read this project');
      // A phone number is a single thing, so two projects cannot both claim it.
      await assert.rejects(theirs.createSipInboundTrunk('squat', [number], { allowedAddresses: ['203.0.113.0/24'] }), /onflicting/);
      // Not when they name different carriers: then their calls cannot be mistaken for each other.
      const fine = await theirs.createSipInboundTrunk('elsewhere', [number], { allowedAddresses: ['198.51.100.0/24'] });
      await theirs.deleteSipTrunk(fine.sipTrunkId);
    } finally {
      await stack.sip.deleteSipDispatchRule(rule.sipDispatchRuleId);
      await stack.sip.deleteSipTrunk(trunk.sipTrunkId);
      await stack.sip.deleteSipTrunk(out.sipTrunkId);
    }
  });

  it('placing a call with no worker says so at once', async () => {
    const out = await stack.sip.createSipOutboundTrunk('none', CARRIER, ['+15550142']);
    try {
      const started = Date.now();
      await assert.rejects(stack.sip.createSipParticipant(out.sipTrunkId, '+15550177', 'room'), /no SIP worker/);
      assert.ok(Date.now() - started < 3000);
    } finally {
      await stack.sip.deleteSipTrunk(out.sipTrunkId);
    }
  });

  it('will not call a private address unless the installation allows that', async () => {
    const stricter = writeConfig('config-strict.yaml');
    const text = (await import('node:fs')).readFileSync(stricter, 'utf8').replace('allow_private_addresses: true', 'allow_private_addresses: false').replace(`port: ${OURS.http}`, `port: ${SPARE.http}`).replace(`udp_port: ${OURS.udp}`, `udp_port: ${SPARE.udp}`).replace(`tcp_port: ${OURS.tcp}`, `tcp_port: ${SPARE.tcp}`);
    writeFileSync(stricter, text);
    const server = startProcess('server-strict', join(tmp, 'bin/streamkit-server'), ['--dev', '--config', stricter]);
    try {
      const base = `http://${HOST}:${SPARE.http}`;
      await waitFor('the second server', async () => (await fetch(`${base}/`)).ok);
      const account = await Account.signUp(base, 'strict');
      const sip = new SipClient(base, account.apiKey, account.apiSecret);
      for (const address of ['10.0.0.5', '127.0.0.1:5060', '192.168.1.1', '169.254.169.254', 'localhost']) {
        await assert.rejects(sip.createSipOutboundTrunk('inside', address, ['+15550142']), /private network/, address);
      }
      await assert.rejects(sip.createSipParticipant('', '+15550177', 'room', { participantIdentity: 'x' }, { hostname: '127.0.0.1' }), /private network|no numbers/);
      const fine = await sip.createSipOutboundTrunk('outside', CARRIER, ['+15550142']);
      await sip.deleteSipTrunk(fine.sipTrunkId);
    } finally {
      await stopProcess(server);
    }
  });

  it('the REST routes do what the dashboard needs, and need the right role', async () => {
    const { account } = stack;
    const number = nextNumber();
    const trunk = await account.ok('POST', '/sip/trunks/inbound', { name: 'rest', numbers: [number], allowed_addresses: ['203.0.113.0/24'], auth_username: 'u', auth_password: 'p' }, [201]);
    assert.match(trunk.sip_trunk_id, /^ST_/);
    assert.equal(trunk.has_credentials, true);
    assert.equal(trunk.auth_password, '');
    const changed = await account.ok('PATCH', `/sip/trunks/inbound/${trunk.sip_trunk_id}`, { name: 'changed' });
    assert.equal(changed.name, 'changed');
    assert.equal(changed.has_credentials, true);
    const rule = await account.ok('POST', '/sip/dispatch-rules', { name: 'rest', trunk_ids: [trunk.sip_trunk_id], rule: { dispatch_rule_direct: { room_name: 'r', pin: '11' } } }, [201]);
    assert.equal(rule.rule.dispatch_rule_direct.pin, '****');
    const rules = await account.ok('GET', '/sip/dispatch-rules');
    assert.ok(rules.items.some((r) => r.sip_dispatch_rule_id === rule.sip_dispatch_rule_id));
    const out = await account.ok('POST', '/sip/trunks/outbound', { name: 'rest', address: CARRIER, numbers: ['+15550142'] }, [201]);
    assert.equal((await account.ok('GET', '/sip/trunks/outbound')).items.filter((t) => t.sip_trunk_id === out.sip_trunk_id).length, 1);
    const bad = await account.req('POST', '/sip/trunks/inbound', { name: 'open', numbers: [nextNumber()] });
    assert.equal(bad.status, 400, bad.text);
    assert.equal(bad.body.error.code, 'invalid_argument');
    assert.equal((await account.req('POST', '/sip/trunks/inbound', { nonsense: 1 })).status, 400);
    // A token can read with the admin grant, and writing is for people.
    const read = await fetch(`${stack.base}/api/v1/sip/trunks/inbound`, { headers: { Authorization: `Bearer ${await new AccessToken(stack.apiKey, stack.apiSecret, { identity: 't' }).toJwt()}` } });
    assert.equal(read.status, 403);
    const at = new AccessToken(stack.apiKey, stack.apiSecret, { identity: 't' });
    at.addSIPGrant({ admin: true });
    const token = await at.toJwt();
    const allowed = await fetch(`${stack.base}/api/v1/sip/trunks/inbound`, { headers: { Authorization: `Bearer ${token}` } });
    assert.equal(allowed.status, 200);
    const write = await fetch(`${stack.base}/api/v1/sip/trunks/inbound`, { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: '{}' });
    assert.equal(write.status, 401);
    await account.ok('DELETE', `/sip/dispatch-rules/${rule.sip_dispatch_rule_id}`);
    await account.ok('DELETE', `/sip/trunks/inbound/${trunk.sip_trunk_id}`);
    await account.ok('DELETE', `/sip/trunks/outbound/${out.sip_trunk_id}`);
    assert.equal((await account.req('DELETE', `/sip/trunks/outbound/${out.sip_trunk_id}`)).status, 404);
    const spec = await (await fetch(`${stack.base}/api/v1/openapi.yaml`)).text();
    assert.match(spec, /\/sip\/trunks\/inbound:/);
  });

  it('with no database, every method says SIP is not connected', async () => {
    const path = join(tmp, 'config-plain.yaml');
    writeFileSync(path, `port: ${SPARE.http}\nrtc:\n  udp_port: ${SPARE.udp}\n  tcp_port: ${SPARE.tcp}\n`);
    const plain = startProcess('server-plain', join(tmp, 'bin/streamkit-server'), ['--dev', '--config', path]);
    try {
      const base = `http://${HOST}:${SPARE.http}`;
      await waitFor('the server with no database', async () => (await fetch(`${base}/`)).ok);
      const sip = new SipClient(base, 'devkey', 'secret');
      await assert.rejects(sip.listSipInboundTrunk(), /sip not connected/);
    } finally {
      await stopProcess(plain);
    }
  });
});

describe('the reference, with the same tests', { skip: !referenceAvailable() && 'needs the reference server' }, () => {
  let stack;
  before(async () => {
    stack = await startReference({ bridge: false });
  }, { timeout: 60000 });
  after(async () => {
    if (stack) await stopReference(stack);
  });
  shared(() => stack);
});

after(stopAll);

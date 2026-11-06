// Tokens and webhooks with no server, including both directions against the official Node SDK, as a test-only comparison.
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { create } from '@bufbuild/protobuf';
import * as jose from 'jose';
import * as official from 'livekit-server-sdk';
import { describe, expect, it } from 'vitest';
import { AccessToken, RoomConfigurationSchema, StreamKitAPI, TokenVerifier, WebhookReceiver, signWebhook } from '../src/index.js';

const KEY = 'testkey';
const SECRET = 'testsecret-that-is-long-enough-for-tests';
const encoded = new TextEncoder().encode(SECRET);

const everyGrant = {
  video: {
    roomCreate: true, roomList: true, roomRecord: true, roomAdmin: true, roomJoin: true, room: 'r', canPublish: false, canSubscribe: true,
    canPublishData: false, canPublishSources: ['camera', 'screen_share_audio'] as ('camera' | 'screen_share_audio')[], canUpdateOwnMetadata: true,
    ingressAdmin: true, hidden: true, recorder: true, agent: true, canSubscribeMetrics: true, canManageAgentSession: true, destinationRoom: 'd',
  },
};

function fullToken(): AccessToken {
  const t = new AccessToken(KEY, SECRET, { identity: 'alice', name: 'Alice', metadata: 'm', attributes: { a: '1' } });
  t.addGrant(everyGrant.video);
  t.addSIPGrant({ admin: true, call: true });
  t.addAgentGrant({ admin: true, simulationAdmin: true, databaseAdmin: true, dispatchAdmin: true });
  t.addInferenceGrant({ perform: true });
  t.addObservabilityGrant({ write: true });
  t.addAgentEndpointGrant({ call: true, agentName: 'n', deployment: 'prod' });
  t.addAgentThreadGrant({ all: true, scope: 's', threadIds: ['t'], list: true, create: true, write: true, read: true, delete: true });
  t.kind = 'agent';
  t.kindDetails = ['cloud_agent'];
  t.roomConfig = create(RoomConfigurationSchema, { name: 'r', emptyTimeout: 11, departureTimeout: 12, maxParticipants: 13, metadata: 'cfg' });
  t.roomPreset = 'preset';
  t.sha256 = 'abc';
  return t;
}

const sign = (claims: jose.JWTPayload, alg = 'HS256') => new jose.SignJWT(claims).setProtectedHeader({ alg }).sign(encoded);

describe('AccessToken', () => {
  it('writes every grant under its wire name', async () => {
    const claims = jose.decodeJwt(await fullToken().toJwt()) as Record<string, any>;
    for (const name of ['sub', 'iss', 'exp', 'nbf', 'iat', 'name', 'kind', 'kindDetails', 'metadata', 'attributes', 'video', 'sip', 'agent', 'inference',
      'observability', 'agentEndpoint', 'agentThread', 'roomConfig', 'roomPreset', 'sha256']) {
      expect(claims, name).toHaveProperty(name);
    }
    expect(claims.sub).toBe('alice');
    expect(claims.iss).toBe(KEY);
    expect(claims.video).toEqual(everyGrant.video);
    expect(claims.roomConfig).toMatchObject({ emptyTimeout: 11, departureTimeout: 12, maxParticipants: 13, metadata: 'cfg' });
    expect(jose.decodeProtectedHeader(await fullToken().toJwt()).alg).toBe('HS256');
  });

  it('lasts as long as it is told to', async () => {
    const seconds = jose.decodeJwt(await new AccessToken(KEY, SECRET, { identity: 'a', ttl: 60 }).toJwt());
    expect(seconds.exp! - seconds.nbf!).toBe(60);
    const span = jose.decodeJwt(await new AccessToken(KEY, SECRET, { identity: 'a', ttl: '10m' }).toJwt());
    expect(span.exp! - span.nbf!).toBe(600);
    const standard = jose.decodeJwt(await new AccessToken(KEY, SECRET, { identity: 'a' }).toJwt());
    expect(standard.exp! - standard.nbf!).toBe(6 * 3600);
  });

  it('refuses what cannot work', async () => {
    expect(() => new AccessToken('', SECRET)).toThrow();
    expect(() => new AccessToken(KEY, '')).toThrow();
    const noIdentity = new AccessToken(KEY, SECRET);
    noIdentity.addGrant({ roomJoin: true, room: 'r' });
    await expect(noIdentity.toJwt()).rejects.toThrow(/identity/);
    const noRoom = new AccessToken(KEY, SECRET, { identity: 'a' });
    noRoom.addGrant({ roomJoin: true });
    await expect(noRoom.toJwt()).rejects.toThrow(/room/);
  });

  it('refuses to run in a page', () => {
    const page = globalThis as { document?: unknown };
    page.document = {};
    try {
      expect(() => new AccessToken(KEY, SECRET)).toThrow(/browser/);
    } finally {
      delete page.document;
    }
  });
});

describe('downloads', () => {
  it('outlast the timeout for API calls, which everything else keeps', async () => {
    const server = createServer(async (_req, res) => {
      for (let i = 0; i < 5; i++) {
        res.write('part');
        await new Promise((r) => setTimeout(r, 100));
      }
      res.end();
    });
    await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
    try {
      const { port } = server.address() as AddressInfo;
      const { egress } = new StreamKitAPI(`http://127.0.0.1:${port}`, KEY, SECRET, { requestTimeout: 0.2 });
      const res = await egress.downloadRecording('EG_1');
      expect(await res.text()).toBe('part'.repeat(5));
      await expect(egress.getEgress('EG_1')).rejects.toThrow();
    } finally {
      server.closeAllConnections();
      server.close();
    }
  });
});

describe('the environment', () => {
  it('supplies the server and credentials', async () => {
    const saved = { url: process.env.STREAMKIT_URL, key: process.env.STREAMKIT_API_KEY, secret: process.env.STREAMKIT_API_SECRET };
    try {
      delete process.env.STREAMKIT_URL;
      expect(() => new StreamKitAPI()).toThrow(/server address/);
      process.env.STREAMKIT_URL = 'wss://example.invalid';
      process.env.STREAMKIT_API_KEY = KEY;
      process.env.STREAMKIT_API_SECRET = SECRET;
      expect(new StreamKitAPI()).toBeTruthy();
      const token = await new AccessToken(undefined, undefined, { identity: 'a' }).toJwt();
      expect((await new TokenVerifier(KEY, SECRET).verify(token)).identity).toBe('a');
    } finally {
      for (const [name, value] of [['STREAMKIT_URL', saved.url], ['STREAMKIT_API_KEY', saved.key], ['STREAMKIT_API_SECRET', saved.secret]] as const) {
        if (value === undefined) delete process.env[name];
        else process.env[name] = value;
      }
    }
  });
});

describe('TokenVerifier', () => {
  const verifier = new TokenVerifier(KEY, SECRET);
  const now = () => Math.floor(Date.now() / 1000);
  const base = () => ({ iss: KEY, sub: 'a', exp: now() + 3600 });

  it('returns what a token says', async () => {
    const claims = await verifier.verify(await fullToken().toJwt());
    expect(claims.identity).toBe('alice');
    expect(claims.name).toBe('Alice');
    expect(claims.video).toEqual(everyGrant.video);
    expect(claims.sip).toEqual({ admin: true, call: true });
    expect(claims.agent?.dispatchAdmin).toBe(true);
    expect(claims.inference?.perform).toBe(true);
    expect(claims.observability?.write).toBe(true);
    expect(claims.agentEndpoint?.agentName).toBe('n');
    expect(claims.agentThread?.threadIds).toEqual(['t']);
    expect(claims.roomConfig?.maxParticipants).toBe(13);
    expect(claims.roomPreset).toBe('preset');
  });

  it('applies the same rules as a server', async () => {
    await expect(verifier.verify(await sign(base()))).resolves.toBeTruthy();
    await expect(verifier.verify(await sign({ ...base(), exp: now() - 50 }))).resolves.toBeTruthy();
    await expect(verifier.verify(await sign({ ...base(), exp: now() - 70 }))).rejects.toThrow();
    await expect(verifier.verify(await sign({ ...base(), nbf: now() + 50 }))).resolves.toBeTruthy();
    await expect(verifier.verify(await sign({ ...base(), nbf: now() + 70 }))).rejects.toThrow();
    const { exp: _exp, ...noExpiry } = base();
    await expect(verifier.verify(await sign(noExpiry))).rejects.toThrow();
    await expect(verifier.verify(await sign({ ...base(), iss: 'other' }))).rejects.toThrow();
    await expect(verifier.verify(await sign(base(), 'HS384'))).rejects.toThrow();
    await expect(new TokenVerifier(KEY, 'another secret').verify(await sign(base()))).rejects.toThrow();
    await expect(verifier.verify('not.a.token')).rejects.toThrow();
    // No signature at all.
    const none = `${btoa(JSON.stringify({ alg: 'none' }))}.${btoa(JSON.stringify(base()))}.`.replace(/=/g, '');
    await expect(verifier.verify(none)).rejects.toThrow();
  });

  it('takes identity from jti when there is no sub, and ignores claims it does not know', async () => {
    const claims = await verifier.verify(await sign({ iss: KEY, jti: 'from-jti', exp: now() + 60, somethingNew: { x: 1 }, roomConfig: { empty_timeout: 5, maxParticipants: 6, fieldFromTheFuture: true } }));
    expect(claims.identity).toBe('from-jti');
    expect(claims.roomConfig?.emptyTimeout).toBe(5);
    expect(claims.roomConfig?.maxParticipants).toBe(6);
  });
});

describe('against the official Node SDK', () => {
  it('this SDK verifies its tokens, with every grant', async () => {
    const theirs = new official.AccessToken(KEY, SECRET, { identity: 'bob', name: 'Bob', metadata: 'm', attributes: { a: '1' }, ttl: '1h' });
    theirs.addGrant({ roomJoin: true, room: 'r', canPublish: false, roomAdmin: true });
    theirs.addSIPGrant({ admin: true, call: true });
    theirs.addInferenceGrant({ perform: true });
    theirs.addObservabilityGrant({ write: true });
    theirs.kind = 'agent';
    theirs.roomPreset = 'p';
    const claims = await new TokenVerifier(KEY, SECRET).verify(await theirs.toJwt());
    expect(claims).toMatchObject({ identity: 'bob', name: 'Bob', metadata: 'm', kind: 'agent', roomPreset: 'p', attributes: { a: '1' } });
    expect(claims.video).toMatchObject({ roomJoin: true, room: 'r', canPublish: false, roomAdmin: true });
    expect(claims.sip).toEqual({ admin: true, call: true });
  });

  it('its verifier accepts tokens from this SDK', async () => {
    const claims = await new official.TokenVerifier(KEY, SECRET).verify(await fullToken().toJwt());
    expect(claims.sub).toBe('alice');
    expect(claims.video?.roomAdmin).toBe(true);
    expect(claims.sip?.call).toBe(true);
    expect(claims.roomPreset).toBe('preset');
  });

  it('its webhook receiver accepts what this SDK signs', async () => {
    const body = JSON.stringify({ event: 'room_started', id: 'EV_1', room: { name: 'r', sid: 'RM_1' }, createdAt: '1700000000' });
    const event = await new official.WebhookReceiver(KEY, SECRET).receive(body, await signWebhook(body, KEY, SECRET));
    expect(event.event).toBe('room_started');
  });

  it('this SDK accepts a webhook signed by its tools', async () => {
    const body = JSON.stringify({ event: 'room_finished', id: 'EV_2', room: { name: 'r', sid: 'RM_1' }, createdAt: '1700000000' });
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(body)));
    const theirs = new official.AccessToken(KEY, SECRET);
    theirs.sha256 = btoa(String.fromCharCode(...digest));
    const event = await new WebhookReceiver(KEY, SECRET).receive(body, await theirs.toJwt());
    expect(event.event).toBe('room_finished');
    expect(event.room?.name).toBe('r');
  });
});

describe('WebhookReceiver', () => {
  const body = JSON.stringify({ event: 'room_started', id: 'EV_1', room: { name: 'r' }, createdAt: '1700000000', somethingNew: { x: 1 } });
  const receiver = new WebhookReceiver(KEY, SECRET);

  it('passes a body that matches, as text or bytes, and ignores fields it does not know', async () => {
    const signature = await signWebhook(body, KEY, SECRET);
    expect((await receiver.receive(body, signature)).room?.name).toBe('r');
    expect((await receiver.receive(new TextEncoder().encode(body), signature)).createdAt).toBe(1700000000n);
  });

  it('rejects anything else', async () => {
    const signature = await signWebhook(body, KEY, SECRET);
    await expect(receiver.receive(`${body} `, signature)).rejects.toThrow(/does not match/);
    await expect(receiver.receive(body, undefined)).rejects.toThrow(/no Authorization/);
    await expect(receiver.receive(body, `Bearer ${signature}`)).rejects.toThrow();
    await expect(new WebhookReceiver(KEY, 'not the secret').receive(body, signature)).rejects.toThrow();
    await expect(new WebhookReceiver('other', SECRET).receive(body, signature)).rejects.toThrow();
    const noHash = await new AccessToken(KEY, SECRET).toJwt();
    await expect(receiver.receive(body, noHash)).rejects.toThrow(/does not match/);
  });
});

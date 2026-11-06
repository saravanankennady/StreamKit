// The SDK against real running servers: this server, and the reference wherever a call exists there. Participants are
// real: the streamkit CLI joining with a real WebRTC session and real media.
import { spawn } from 'node:child_process';
import { create } from '@bufbuild/protobuf';
import * as official from 'livekit-server-sdk';
import { describe, expect, it } from 'vitest';
import {
  AccessToken,
  DataPacket_Kind,
  NotSupportedError,
  ParticipantInfo_Kind,
  ParticipantInfo_State,
  RoomConfigurationSchema,
  RoomServiceClient,
  ServerError,
  StreamKitAPI,
  TokenVerifier,
  WebhookReceiver,
  egressEnded,
} from '../src/index.js';
import { capturedWebhooks, dataEvent, eachServer, fixture, roomName, sleep, type Target } from './helpers.js';

const code = (e: unknown) => (e as ServerError).code;

async function failure(p: Promise<unknown>): Promise<ServerError> {
  try {
    await p;
  } catch (e) {
    expect(e).toBeInstanceOf(ServerError);
    return e as ServerError;
  }
  throw new Error('it should have failed');
}

function clientWithGrant(t: Target, grant: Parameters<AccessToken['addGrant']>[0]) {
  return async () => {
    const token = new AccessToken(t.server.api_key, t.server.api_secret, { identity: 'caller' });
    token.addGrant(grant);
    return new RoomServiceClient(t.server.url, undefined, undefined, { token: await token.toJwt() });
  };
}

eachServer('Rooms', (target) => {
  it('goes through a room\'s whole life', async () => {
    const { room } = target().api;
    const name = roomName('life');
    const created = await room.createRoom({ name, emptyTimeout: 120, departureTimeout: 30, maxParticipants: 7, metadata: 'hello' });
    expect(created).toMatchObject({ name, emptyTimeout: 120, departureTimeout: 30, maxParticipants: 7, metadata: 'hello' });
    expect(created.sid).toMatch(/^RM_/);
    expect(created.creationTime).toBeGreaterThan(0n);

    expect((await room.createRoom({ name })).sid).toBe(created.sid);

    const named = await room.listRooms([name, `${name}-not-open`]);
    expect(named.map((r) => r.sid)).toEqual([created.sid]);
    expect((await room.listRooms()).map((r) => r.name)).toContain(name);

    expect((await room.updateRoomMetadata(name, 'changed')).metadata).toBe('changed');
    expect((await room.updateRoomMetadata(name, '')).metadata).toBe('');

    await room.deleteRoom(name);
    expect(await room.listRooms([name])).toEqual([]);
  });

  it('reports what does not exist', async () => {
    const { room } = target().api;
    const missing = roomName('missing');
    expect((await failure(room.deleteRoom(missing))).code).toBe('not_found');
    await expect(room.updateRoomMetadata(missing, 'x')).rejects.toThrow();
    expect(await room.listParticipants(missing)).toEqual([]);
    expect((await failure(room.getParticipant(missing, 'nobody'))).code).toBe('not_found');
    expect((await failure(room.removeParticipant(missing, 'nobody'))).code).toBe('not_found');
  });

  it('enforces grants', async () => {
    const t = target();
    const name = roomName('grants');
    await t.api.room.createRoom({ name });
    const denied = (e: ServerError) => {
      expect(e.code).toBe('unauthenticated');
      expect(e.status).toBe(401);
      expect(e.message).toBe('permissions denied');
    };
    const joinOnly = await clientWithGrant(t, { roomJoin: true, room: name })();
    denied(await failure(joinOnly.createRoom({ name })));
    denied(await failure(joinOnly.listRooms()));
    denied(await failure(joinOnly.deleteRoom(name)));
    denied(await failure(joinOnly.listParticipants(name)));
    const otherRoom = await clientWithGrant(t, { roomAdmin: true, room: `${name}-other` })();
    denied(await failure(otherRoom.listParticipants(name)));
    denied(await failure(otherRoom.sendData(name, new Uint8Array([1]))));
    // The right grant works, so those denials are about the grant.
    await (await clientWithGrant(t, { roomList: true })()).listRooms();
    await (await clientWithGrant(t, { roomAdmin: true, room: name })()).listParticipants(name);

    // Bad credentials are answered in plain text, outside the Twirp error shape.
    expect((await failure(new RoomServiceClient(t.server.url, t.server.api_key, 'not-the-secret').listRooms())).code).toBe('unauthenticated');
    const unknown = await failure(new RoomServiceClient(t.server.url, 'not-a-key', t.server.api_secret).listRooms());
    expect(unknown.code).toBe('unauthenticated');
    expect(unknown.message).toBe('invalid API key');
    await t.api.room.deleteRoom(name);
  });

  it('agrees with the official Node SDK, call for call', async () => {
    const t = target();
    const theirs = new official.RoomServiceClient(t.server.http, t.server.api_key, t.server.api_secret);
    const name = roomName('official');
    const created = await theirs.createRoom({ name, metadata: 'from them' });
    const read = await t.api.room.listRooms([name]);
    expect(read[0]).toMatchObject({ sid: created.sid, metadata: 'from them' });
    await t.api.room.updateRoomMetadata(name, 'from us');
    const back = await theirs.listRooms([name]);
    expect(back[0].metadata).toBe('from us');
    await theirs.deleteRoom(name);
  });
});

eachServer('Tokens', (target) => {
  it('tokens from this SDK join, and carry what they say', async () => {
    const t = target();
    const name = roomName('token');
    const token = new AccessToken(t.server.api_key, t.server.api_secret, { identity: 'holder', name: 'The Holder', metadata: 'token metadata', attributes: { color: 'green' } });
    token.addGrant({ roomJoin: true, room: name });
    const peer = t.join(name, 'holder', { token: await token.toJwt() });
    const joined = await peer.wait('joining', (e) => e.event === 'joined');
    expect(joined).toMatchObject({ identity: 'holder', name: 'The Holder', room: name, attributes: { color: 'green' } });
    expect(await t.api.room.getParticipant(name, 'holder')).toMatchObject({ metadata: 'token metadata', name: 'The Holder', attributes: { color: 'green' } });
  });

  it('tokens from the official Node SDK join too', async () => {
    const t = target();
    const name = roomName('official-token');
    const token = new official.AccessToken(t.server.api_key, t.server.api_secret, { identity: 'theirs', metadata: 'm' });
    token.addGrant({ roomJoin: true, room: name, roomAdmin: true });
    const peer = t.join(name, 'theirs', { token: await token.toJwt() });
    await peer.connected();
    expect((await t.api.room.getParticipant(name, 'theirs')).metadata).toBe('m');
  });

  it('a token with every grant joins and works', async () => {
    const t = target();
    const name = roomName('everything');
    const token = new AccessToken(t.server.api_key, t.server.api_secret, { identity: 'everything' });
    token.addGrant({
      roomJoin: true, room: name, roomCreate: true, roomList: true, roomRecord: true, roomAdmin: true, ingressAdmin: true, canPublish: true, canSubscribe: true,
      canPublishData: true, canUpdateOwnMetadata: true, canSubscribeMetrics: true, canManageAgentSession: true, canPublishSources: ['camera', 'microphone'], destinationRoom: `${name}-dest`,
    });
    token.addSIPGrant({ admin: true, call: true });
    token.addAgentGrant({ admin: true, dispatchAdmin: true });
    token.addInferenceGrant({ perform: true });
    token.addObservabilityGrant({ write: true });
    token.addAgentEndpointGrant({ call: true });
    token.addAgentThreadGrant({ all: true });
    const jwt = await token.toJwt();
    const peer = t.join(name, 'everything', { token: jwt });
    await peer.connected();
    const caller = new RoomServiceClient(t.server.url, undefined, undefined, { token: jwt });
    const [info] = await caller.listParticipants(name);
    expect(info.permission).toMatchObject({ canPublish: true, canUpdateMetadata: true });
    expect(info.permission?.canPublishSources.length).toBe(2);
  });

  it('applies what a token grants to its holder', async () => {
    const t = target();
    const name = roomName('limited');
    const token = new AccessToken(t.server.api_key, t.server.api_secret, { identity: 'limited' });
    token.kind = 'agent';
    token.addGrant({ roomJoin: true, room: name, canPublish: false, canPublishData: false });
    const peer = t.join(name, 'limited', { token: await token.toJwt() });
    await peer.connected();
    const info = await t.api.room.getParticipant(name, 'limited');
    expect(info.permission).toMatchObject({ canPublish: false, canPublishData: false, canSubscribe: true });
    expect(info.kind).toBe(ParticipantInfo_Kind.AGENT);
  });

  it('a join token\'s room configuration applies when the join creates the room', async () => {
    const t = target();
    const name = roomName('configured');
    const token = new AccessToken(t.server.api_key, t.server.api_secret, { identity: 'configured' });
    token.addGrant({ roomJoin: true, room: name });
    token.roomConfig = create(RoomConfigurationSchema, { emptyTimeout: 77, departureTimeout: 66, maxParticipants: 55 });
    const peer = t.join(name, 'configured', { token: await token.toJwt() });
    await peer.connected();
    const [room] = await t.api.room.listRooms([name]);
    expect(room).toMatchObject({ emptyTimeout: 77, departureTimeout: 66, maxParticipants: 55 });
  });

  it.skipIf(!fixture()?.reference_binary)('tokens from the reference server\'s own tool join, and verify here', async () => {
    const t = target();
    const name = roomName('tool');
    const info = fixture()!;
    const out = await new Promise<string>((resolve, reject) => {
      const child = spawn(info.reference_binary, ['--keys', `${t.server.api_key}: ${t.server.api_secret}`, 'create-join-token', '--room', name, '--identity', 'from-tool'], { stdio: ['ignore', 'pipe', 'pipe'] });
      let text = '';
      child.stdout.on('data', (d) => (text += d));
      child.on('exit', (status) => (status === 0 ? resolve(text) : reject(new Error(`the tool exited with ${status}`))));
    });
    const jwt = out.match(/eyJ[\w-]+\.[\w-]+\.[\w-]+/)?.[0];
    expect(jwt).toBeTruthy();
    const claims = await new TokenVerifier(t.server.api_key, t.server.api_secret).verify(jwt!);
    expect(claims).toMatchObject({ identity: 'from-tool', video: { room: name, roomJoin: true } });
    await t.join(name, 'from-tool', { token: jwt }).connected();
  });
});

eachServer('Participants', (target) => {
  it('sees real participants and their real tracks', async () => {
    const t = target();
    const name = roomName('media');
    const publisher = t.join(name, 'publisher', { publish: true });
    const tracks = await publisher.published();
    const subscriber = t.join(name, 'subscriber');
    for (const kind of ['video', 'audio']) {
      await subscriber.wait(`real ${kind}`, (e) => e.event === 'media' && e.kind === kind && (e.packets ?? 0) > 10);
    }
    const list = await t.api.room.listParticipants(name);
    expect(list.map((p) => p.identity).sort()).toEqual(['publisher', 'subscriber']);
    const info = await t.api.room.getParticipant(name, 'publisher');
    expect(info.state).toBe(ParticipantInfo_State.ACTIVE);
    expect(info.sid).toMatch(/^PA_/);
    expect(info.tracks.map((x) => x.sid).sort()).toEqual(tracks.map((x) => x.sid).sort());
    const missing = await failure(t.api.room.getParticipant(name, 'nobody'));
    expect(missing.code).toBe('not_found');
    expect(missing.message).toBe('participant does not exist');
  });

  it('mutes a real track', async () => {
    const t = target();
    const name = roomName('mute');
    const publisher = t.join(name, 'publisher', { publish: true });
    const tracks = await publisher.published();
    const audio = tracks.find((x) => x.kind === 'audio')!.sid!;
    const track = await t.api.room.mutePublishedTrack(name, 'publisher', audio, true);
    expect(track).toMatchObject({ sid: audio, muted: true });
    await publisher.wait('being told it was muted', (e) => e.event === 'muted' && e.sid === audio && e.muted === true);
    // A server lists a track once media has arrived for it, which can be a moment after it accepted the track.
    const deadline = Date.now() + 30000;
    for (;;) {
      const listed = (await t.api.room.getParticipant(name, 'publisher')).tracks.find((x) => x.sid === audio);
      if (listed) {
        expect(listed.muted).toBe(true);
        break;
      }
      if (Date.now() > deadline) throw new Error('the muted track was never listed');
      await sleep(100);
    }
    // Unmuting from afar is off unless the server turned it on.
    expect((await failure(t.api.room.mutePublishedTrack(name, 'publisher', audio, false))).code).toBe('failed_precondition');
  });

  it('changes a participant, and the room hears about it', async () => {
    const t = target();
    const name = roomName('update');
    const subject = t.join(name, 'subject');
    await subject.connected();
    const observer = t.join(name, 'observer');
    await observer.connected();
    const res = await t.api.room.updateParticipant(name, 'subject', { metadata: 'new metadata', name: 'New Name', attributes: { a: '1' } });
    expect(res).toMatchObject({ metadata: 'new metadata', name: 'New Name', attributes: { a: '1' } });
    for (const peer of [subject, observer]) {
      await peer.wait('the update', (e) => e.event === 'participant_update' && e.identity === 'subject' && e.metadata === 'new metadata' && e.name === 'New Name' && e.attributes?.a === '1');
    }
    // A permission replaces the whole set.
    const limited = await t.api.room.updateParticipant(name, 'subject', { permission: { canSubscribe: true } });
    expect(limited.permission).toMatchObject({ canPublish: false, canSubscribe: true });
    expect(limited.metadata).toBe('new metadata');
    await subject.wait('the permission update', (e) => e.event === 'participant_update' && e.identity === 'subject' && e.can_publish === false);
    expect((await failure(t.api.room.updateParticipant(name, 'nobody', { metadata: 'x' }))).code).toBe('not_found');
  });

  it('subscribes a participant to tracks', async () => {
    const t = target();
    const name = roomName('subs');
    const publisher = t.join(name, 'publisher', { publish: true });
    const tracks = await publisher.published();
    const video = tracks.find((x) => x.kind === 'video')!.sid!;
    const viewer = t.join(name, 'viewer', { noAutoSubscribe: true });
    await viewer.wait('joining', (e) => e.event === 'joined');
    await t.api.room.updateSubscriptions(name, 'viewer', [video], true);
    await viewer.wait('video after subscribing', (e) => e.event === 'media' && e.kind === 'video' && (e.packets ?? 0) > 10);
    expect(viewer.events.some((e) => e.event === 'media' && e.kind === 'audio')).toBe(false);
  });

  it('sends data to real data channels', async () => {
    const t = target();
    const name = roomName('data');
    const alice = t.join(name, 'alice');
    await alice.dataChannels();
    const bob = t.join(name, 'bob');
    await bob.dataChannels();
    const bytes = (s: string) => new TextEncoder().encode(s);

    await t.api.room.sendData(name, bytes('to everyone'), DataPacket_Kind.RELIABLE, { topic: 'news' });
    for (const peer of [alice, bob]) {
      expect(await peer.wait('the broadcast', dataEvent('to everyone'))).toMatchObject({ topic: 'news', channel: '_reliable' });
    }

    await t.api.room.sendData(name, bytes('for bob'), DataPacket_Kind.RELIABLE, { destinationIdentities: ['bob'] });
    await bob.wait('its own message', dataEvent('for bob'));
    // Reliable data arrives in order, so once alice has this, she would have had the other if it was coming.
    await t.api.room.sendData(name, bytes('after'));
    await alice.wait('the later message', dataEvent('after'));
    expect(alice.events.some(dataEvent('for bob'))).toBe(false);

    await t.api.room.sendData(name, bytes('lossy'), DataPacket_Kind.LOSSY);
    expect((await bob.wait('the lossy message', dataEvent('lossy'))).channel).toBe('_lossy');
  });

  it('removes a participant', async () => {
    const t = target();
    const name = roomName('remove');
    const peer = t.join(name, 'leaving');
    await peer.connected();
    await t.api.room.removeParticipant(name, 'leaving');
    expect((await peer.wait('being removed', (e) => e.event === 'left')).reason).toBe('PARTICIPANT_REMOVED');
    expect((await failure(t.api.room.getParticipant(name, 'leaving'))).code).toBe('not_found');
  });

  it('deleting a room disconnects everyone', async () => {
    const t = target();
    const name = roomName('delete');
    const peer = t.join(name, 'inside');
    await peer.connected();
    await t.api.room.deleteRoom(name);
    expect((await peer.wait('the room closing', (e) => e.event === 'left')).reason).toBe('ROOM_DELETED');
  });

  it('room metadata reaches those inside', async () => {
    const t = target();
    const name = roomName('meta');
    const peer = t.join(name, 'inside');
    await peer.connected();
    await t.api.room.updateRoomMetadata(name, 'for those inside');
    await peer.wait('the room update', (e) => e.event === 'room_update' && e.metadata === 'for those inside');
  });

  it('fails clearly where a call is not implemented', async () => {
    const t = target();
    const name = roomName('nope');
    await expect(t.api.room.forwardParticipant(name, 'x', `${name}-2`)).rejects.toThrow();
    await expect(t.api.room.moveParticipant(name, 'x', `${name}-2`)).rejects.toThrow();
    const rpc = await failure(t.api.room.performRpc(name, '', 'm', ''));
    expect(rpc.code).toBe(t.name === 'reference' ? 'invalid_argument' : 'unimplemented');
  });

  it('agent dispatch works on the reference, and is clearly refused here', async () => {
    const t = target();
    const name = roomName('agents');
    const { agentDispatch } = t.api;
    if (t.name === 'ours') {
      await expect(agentDispatch.createDispatch(name, 'helper')).rejects.toThrow(NotSupportedError);
      await expect(agentDispatch.listDispatch(name)).rejects.toThrow(/no AgentDispatchService service/);
      return;
    }
    const created = await agentDispatch.createDispatch(name, 'helper', { metadata: 'm' });
    expect(created.agentName).toBe('helper');
    expect(created.id).toMatch(/^AD_/);
    // The reference also keeps a dispatch of its own in every room, for agents that take any job.
    expect((await agentDispatch.listDispatch(name)).map((d) => d.id)).toContain(created.id);
    await agentDispatch.deleteDispatch(created.id, name);
  });
});

eachServer('Webhooks', (target) => {
  it('real deliveries pass this receiver and the official one, byte for byte', async () => {
    const t = target();
    const name = roomName('webhooks');
    const ours = new WebhookReceiver(t.server.api_key, t.server.api_secret);
    const theirs = new official.WebhookReceiver(t.server.api_key, t.server.api_secret);
    // Checks every delivery for this room so far, until the events named have arrived, and returns the last one.
    const waitFor = async (...want: string[]) => {
      const deadline = Date.now() + 45000;
      for (;;) {
        const seen = new Set<string>();
        let last = undefined as Awaited<ReturnType<typeof capturedWebhooks>>[number] | undefined;
        for (const c of await capturedWebhooks(t.name)) {
          const body = Buffer.from(c.body, 'base64').toString();
          const event = await ours.receive(body, c.authorization);
          if (event.room?.name !== name) continue;
          expect((await theirs.receive(body, c.authorization)).event).toBe(event.event);
          expect(c.content_type).toBe('application/webhook+json');
          expect(event.id).toBeTruthy();
          seen.add(event.event);
          last = c;
        }
        if (want.every((e) => seen.has(e))) return last!;
        if (Date.now() > deadline) throw new Error(`never saw all of ${want}. Saw ${[...seen]}`);
        await sleep(250);
      }
    };

    const peer = t.join(name, 'publisher', { publish: true });
    await peer.published();
    // A participant counts as joined once its media is connected, and a track as published once media arrives.
    await waitFor('room_started', 'participant_joined', 'track_published');
    await t.api.room.deleteRoom(name);
    const last = await waitFor('room_finished', 'participant_left');

    const body = Buffer.from(last.body, 'base64').toString();
    await expect(ours.receive(body.replace(name, `${name}x`), last.authorization)).rejects.toThrow(/does not match/);
  });
});

describe('Egress and ingress', () => {
  const info = fixture();
  const ready = info?.ours;

  describe.skipIf(!ready)('need this server\'s API', () => {
    for (const which of ['reference', 'plain'] as const) {
      it(`${which} says so`, async () => {
        const server = info![which];
        if (!server) return;
        const api = new StreamKitAPI(server.url, server.api_key, server.api_secret);
        await expect(api.egress.listEgress()).rejects.toThrow(NotSupportedError);
        await expect(api.ingress.listIngress()).rejects.toThrow(/no management API/);
      });
    }
  });

  it('kinds this server does not do are refused without calling anything', async () => {
    const api = new StreamKitAPI('http://127.0.0.1:1', 'k', 's');
    for (const call of [api.egress.startWebEgress(), api.egress.startTrackCompositeEgress(), api.egress.startTrackEgress(), api.egress.updateLayout(), api.egress.updateStream()]) {
      await expect(call).rejects.toThrow(NotSupportedError);
    }
  });

  describe.skipIf(!ready)('ingress configuration', () => {
    it('manages an ingress', async () => {
      const { ingress } = new StreamKitAPI(info!.ours.url, info!.ours.api_key, info!.ours.api_secret);
      const name = roomName('ingress');
      const created = await ingress.createIngress('rtmp', { name: 'stage', room: name, participantIdentity: 'stage', participantName: 'Stage' });
      expect(created.id).toMatch(/^IN_/);
      expect(created.streamKey).toBeTruthy();
      expect(created.publishUrl).toMatch(/^rtmp:\/\//);
      const got = await ingress.getIngress(created.id);
      expect(got.streamKey).toBeUndefined();
      expect(got).toMatchObject({ roomName: name, participantIdentity: 'stage', participantName: 'Stage', name: 'stage', enabled: true });
      expect((await ingress.listIngress()).map((x) => x.id)).toContain(created.id);
      expect(await ingress.updateIngress(created.id, { name: 'renamed', enabled: false })).toMatchObject({ name: 'renamed', enabled: false });
      const rotated = await ingress.rotateIngressKey(created.id);
      expect(rotated.streamKey).toBeTruthy();
      expect(rotated.streamKey).not.toBe(created.streamKey);
      await ingress.ingressLogs(created.id, 10);
      await ingress.deleteIngress(created.id);
      expect((await failure(ingress.getIngress(created.id))).code).toBe('not_found');
      const invalid = await failure(ingress.createIngress('carrier-pigeon' as 'rtmp', { room: name, participantIdentity: 'x' }));
      expect(invalid.code).toBe('validation_failed');
      expect(invalid.fields?.input_type).toBeTruthy();
    });
  });

  describe.skipIf(!info?.workers)('with real workers', () => {
    const peers: { close: () => Promise<void> }[] = [];
    const ours = () => new StreamKitAPI(info!.ours.url, info!.ours.api_key, info!.ours.api_secret);

    it('records a real room to a real file', async () => {
      const { Peer } = await import('./helpers.js');
      const { egress } = ours();
      const name = roomName('record');
      const peer = new Peer(info!.cli, info!.ours, name, 'on-camera', { publish: true });
      peers.push(peer);
      await peer.published();

      const job = await egress.startRoomCompositeEgress(name, { format: 'mp4', layout: 'grid' });
      expect(job.id).toMatch(/^EG_/);
      expect(job).toMatchObject({ roomName: name, type: 'room_composite', format: 'mp4' });
      const waitFor = async (check: (j: typeof job) => boolean) => {
        const deadline = Date.now() + 90000;
        for (;;) {
          const current = await egress.getEgress(job.id);
          if (check(current)) return current;
          if (Date.now() > deadline) throw new Error(`job stayed ${current.status}: ${current.error}`);
          await sleep(500);
        }
      };
      const active = await waitFor((j) => j.status === 'active' || egressEnded(j));
      expect(active.status).toBe('active');
      expect((await egress.listEgress({ limit: 10 })).items.map((j) => j.id)).toContain(job.id);
      expect((await failure(egress.retryRecording(job.id))).code).toBe('egress_running');

      // Let it record something worth checking.
      await sleep(5000);
      await egress.stopEgress(job.id);
      const done = await waitFor(egressEnded);
      expect(done.status).toBe('complete');
      expect(done.fileSize).toBeGreaterThan(0);
      expect((await failure(egress.stopEgress(job.id))).code).toBe('egress_not_running');
      expect((await egress.listRecordings()).items.map((j) => j.id)).toContain(job.id);

      const file = new Uint8Array(await (await egress.downloadRecording(job.id)).arrayBuffer());
      expect(file.length).toBe(done.fileSize);
      expect(new TextDecoder().decode(file.subarray(4, 8))).toBe('ftyp');

      await egress.deleteRecording(job.id);
      expect((await failure(egress.getEgress(job.id))).code).toBe('not_found');
      await peer.close();
    });

    it('brings a real RTMP stream into a room', async () => {
      const { Peer } = await import('./helpers.js');
      const { ingress } = ours();
      const name = roomName('rtmp');
      const created = await ingress.createIngress('rtmp', { room: name, participantIdentity: 'stage' });
      const pusher = spawn('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-re', '-f', 'lavfi', '-i', 'testsrc2=s=640x360:r=30', '-f', 'lavfi', '-i', 'sine=frequency=440:r=48000',
        '-c:v', 'libx264', '-preset', 'veryfast', '-tune', 'zerolatency', '-g', '60', '-c:a', 'aac', '-f', 'flv', `rtmp://${info!.rtmp}/live/${created.streamKey}`], { stdio: 'ignore' });
      try {
        const deadline = Date.now() + 60000;
        for (;;) {
          const current = await ingress.getIngress(created.id);
          if (current.status === 'publishing') break;
          if (Date.now() > deadline) throw new Error(`it stayed ${current.status}: ${current.error}`);
          await sleep(250);
        }
        const viewer = new Peer(info!.cli, info!.ours, name, 'viewer');
        peers.push(viewer);
        await viewer.wait('the stream', (e) => e.event === 'media' && e.kind === 'video' && (e.packets ?? 0) > 10);
        await ingress.stopIngress(created.id);
        await viewer.close();
      } finally {
        pusher.kill('SIGTERM');
        await ingress.deleteIngress(created.id);
      }
    });
  });
});

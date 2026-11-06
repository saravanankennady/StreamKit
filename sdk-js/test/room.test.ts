import { create } from '@bufbuild/protobuf';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConnectionQuality, ConnectionState, DisconnectReason, PermissionStatus, Room, RoomEvent, SubscriptionStatus, TrackSource, VideoQuality } from '../src/index.js';
import {
  type ParticipantInfo,
  ParticipantInfoSchema,
  ParticipantInfo_State,
  ParticipantPermissionSchema,
  SpeakerInfoSchema,
  TrackInfoSchema,
  TrackSource as ProtoSource,
  TrackType,
} from '../src/proto/livekit_models_pb.js';

function trackInfo(sid: string, over = {}) {
  return create(TrackInfoSchema, { sid, type: TrackType.VIDEO, source: ProtoSource.CAMERA, name: 'camera', ...over });
}

function info(identity: string, sid: string, version: number, over: Partial<ParticipantInfo> = {}) {
  return create(ParticipantInfoSchema, { identity, sid, version, state: ParticipantInfo_State.ACTIVE, ...over } as never);
}

class FakeStream extends EventTarget {
  constructor(public id: string) {
    super();
  }
}

function fakeTrack(id = 'browser-id', kind = 'video') {
  return { id, kind, readyState: 'live' } as unknown as MediaStreamTrack;
}

function connectedRoom() {
  const room = new Room();
  const r = room as any;
  r.state = ConnectionState.Connected;
  const sent: Array<{ kind: string; value: unknown }> = [];
  const send = (kind: string) => (value: unknown) => sent.push({ kind, value });
  r.engine = {
    isReconnecting: false,
    midToTrackId: {},
    subscriber: { pc: { getTransceivers: () => [] } },
    signal: {
      sendUpdateSubscription: send('subscription'),
      sendUpdateTrackSettings: send('settings'),
      sendMuteTrack: (sid: string, muted: boolean) => sent.push({ kind: 'mute', value: { sid, muted } }),
    },
  };
  return { room, r, sent };
}

function eventsOf(room: Room) {
  const events: string[] = [];
  for (const name of Object.values(RoomEvent)) room.on(name as never, (() => events.push(name)) as never);
  return events;
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('turning a source on and off while it is still publishing', () => {
  function publishing() {
    const { room } = connectedRoom();
    const lp = room.localParticipant as any;
    const calls: string[] = [];
    const pub = {
      track: {},
      mute: async () => void calls.push('mute'),
      unmute: async () => void calls.push('unmute'),
    };
    lp.getTrackPublication = () => pub;
    let finish = (_: unknown) => {};
    lp.pendingPublishing.set('microphone', new Promise((resolve) => (finish = resolve)));
    return { lp, calls, finish: () => finish(pub) };
  }

  it('the last request wins when it was off and then on', async () => {
    const { lp, calls, finish } = publishing();
    const requests = [lp.setMicrophoneEnabled(false), lp.setMicrophoneEnabled(true)];
    finish();
    await Promise.all(requests);
    expect(calls).toEqual(['mute', 'unmute']);
  });

  it('the last request wins when it was on and then off', async () => {
    const { lp, calls, finish } = publishing();
    const requests = [lp.setMicrophoneEnabled(true), lp.setMicrophoneEnabled(false)];
    finish();
    await Promise.all(requests);
    expect(calls.at(-1)).toBe('mute');
  });
});

describe('what stays with the room', () => {
  it('someone arriving plays through the speaker this client chose', () => {
    const { room, r } = connectedRoom();
    r.activeDevices.set('audiooutput', 'speaker-2');
    r.handleParticipantUpdates([info('alice', 'PA_1', 1)]);
    expect((room.remoteParticipants.get('alice') as any).sinkId).toBe('speaker-2');
  });

  it('gives the latest token the server issued, and keeps it after leaving', async () => {
    const { room, r } = connectedRoom();
    expect(room.accessToken).toBeUndefined();
    r.engine.token = 'refreshed';
    r.engine.close = async () => {};
    expect(room.accessToken).toBe('refreshed');
    await room.disconnect();
    expect(room.accessToken).toBe('refreshed');
  });
});

describe('participant updates', () => {
  it('a new participant arrives before its tracks do', () => {
    const { room, r } = connectedRoom();
    const events = eventsOf(room);
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    expect(events.filter((e) => e === 'participantConnected' || e === 'trackPublished')).toEqual(['participantConnected', 'trackPublished']);
    expect(room.remoteParticipants.get('alice')?.getTrackPublications().map((p) => p.trackSid)).toEqual(['TR_1']);
  });

  it('the track list is complete: one missing from it is gone', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1'), trackInfo('TR_2')] })]);
    const events = eventsOf(room);
    r.handleParticipantUpdates([info('alice', 'PA_1', 2, { tracks: [trackInfo('TR_2')] })]);
    expect(events).toContain('trackUnpublished');
    expect(room.remoteParticipants.get('alice')?.getTrackPublications().map((p) => p.trackSid)).toEqual(['TR_2']);
    r.handleParticipantUpdates([info('alice', 'PA_1', 3)]);
    expect(room.remoteParticipants.get('alice')?.getTrackPublications()).toEqual([]);
  });

  it('an update with a lower version is dropped, and an equal one applied', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 5, { metadata: 'five' })]);
    r.handleParticipantUpdates([info('alice', 'PA_1', 4, { metadata: 'four', tracks: [trackInfo('TR_1')] })]);
    const alice = room.remoteParticipants.get('alice')!;
    expect(alice.metadata).toBe('five');
    expect(alice.getTrackPublications()).toEqual([]);
    r.handleParticipantUpdates([info('alice', 'PA_1', 5, { metadata: 'five again' })]);
    expect(alice.metadata).toBe('five again');
  });

  it('a new session starts its version count again', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 100)]);
    r.handleParticipantUpdates([info('alice', 'PA_2', 1, { metadata: 'new session' })]);
    const alice = room.remoteParticipants.get('alice')!;
    expect(alice.sid).toBe('PA_2');
    expect(alice.metadata).toBe('new session');
  });

  it('a late disconnect from a replaced session does not remove its replacement', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_2', 1)]);
    r.handleParticipantUpdates([info('alice', 'PA_1', 9, { state: ParticipantInfo_State.DISCONNECTED })]);
    expect(room.remoteParticipants.has('alice')).toBe(true);
  });

  it('a disconnect removes the participant, with the reason', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    let seen: unknown[] = [];
    room.on(RoomEvent.ParticipantDisconnected, (p, reason) => {
      seen = [p.identity, reason];
    });
    r.handleParticipantUpdates([info('alice', 'PA_1', 2, { state: ParticipantInfo_State.DISCONNECTED, disconnectReason: DisconnectReason.DUPLICATE_IDENTITY as never })]);
    expect(seen).toEqual(['alice', DisconnectReason.DUPLICATE_IDENTITY]);
    expect(room.remoteParticipants.size).toBe(0);
  });

  it('an update with no identity is matched by session', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1)]);
    r.handleParticipantUpdates([info('', 'PA_1', 2, { metadata: 'by sid' })]);
    expect(room.remoteParticipants.get('alice')?.metadata).toBe('by sid');
  });

  it('this client is not treated as a remote participant, and its tracks are not removed by updates', () => {
    const { room, r } = connectedRoom();
    r.localParticipant.identity = 'me';
    r.localParticipant.sid = 'PA_me';
    r.localParticipant.addPublication({ trackSid: 'TR_mine', kind: 'video', isMuted: false, source: TrackSource.Camera });
    r.handleParticipantUpdates([info('me', 'PA_me', 1, { permission: create(ParticipantPermissionSchema, { canPublish: true }) })]);
    expect(room.remoteParticipants.size).toBe(0);
    expect(room.localParticipant.permissions?.canPublish).toBe(true);
    expect(room.localParticipant.getTrackPublications()).toHaveLength(1);
  });

  it('corrects the server if it has a different idea of a local mute', () => {
    const { r, sent } = connectedRoom();
    r.localParticipant.setEngine(Object.assign(new (class extends EventTarget { on() { return this; } })(), { signal: r.engine.signal }));
    r.localParticipant.identity = 'me';
    r.localParticipant.addPublication({ trackSid: 'TR_mine', kind: 'audio', isMuted: true, source: TrackSource.Microphone, updateInfo: () => {} });
    r.handleParticipantUpdates([info('me', 'PA_me', 1, { tracks: [trackInfo('TR_mine', { muted: false })] })]);
    expect(sent).toContainEqual({ kind: 'mute', value: { sid: 'TR_mine', muted: true } });
  });
});

describe('when events are delivered', () => {
  it('people already in the room at join are not announced', () => {
    const { room, r } = connectedRoom();
    r.state = ConnectionState.Connecting;
    const events = eventsOf(room);
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    expect(events).toEqual([]);
    expect(room.remoteParticipants.has('alice')).toBe(true);
  });

  it('changes during a resume wait until the application hears it is back', () => {
    const { room, r } = connectedRoom();
    r.state = ConnectionState.SignalReconnecting;
    const events = eventsOf(room);
    r.handleParticipantUpdates([info('alice', 'PA_1', 1)]);
    expect(events).toEqual([]);
    r.onResumed();
    const at = events.indexOf('reconnected');
    expect(at).toBeGreaterThanOrEqual(0);
    expect(events.indexOf('participantConnected')).toBeGreaterThan(at);
  });
});

describe('a full reconnect', () => {
  it('tells of the old session ending before the same track arrives in the new one', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    r.onMediaTrackAdded(fakeTrack('TR_1'), new FakeStream('PA_1|TR_1'), {});
    const seen: string[] = [];
    room.on(RoomEvent.TrackUnsubscribed, (_t, pub) => seen.push(`unsubscribed ${pub.trackSid}`));
    room.on(RoomEvent.TrackSubscribed, (_t, pub) => seen.push(`subscribed ${pub.trackSid}`));
    room.on(RoomEvent.Reconnected, () => seen.push('reconnected'));

    r.onRestarting();
    expect(room.state).toBe(ConnectionState.Reconnecting);
    expect(room.remoteParticipants.size).toBe(0);
    r.handleParticipantUpdates([info('alice', 'PA_2', 1, { tracks: [trackInfo('TR_1')] })]);
    r.onMediaTrackAdded(fakeTrack('TR_1'), new FakeStream('PA_2|TR_1'), {});
    r.finishReconnect();

    expect(seen).toEqual(['reconnected', 'unsubscribed TR_1', 'subscribed TR_1']);
    expect(room.remoteParticipants.get('alice')?.getTrackPublication(TrackSource.Camera)?.track).toBeDefined();
  });
});

describe('media and metadata in either order', () => {
  const media = (r: any, participant: string, trackSid: string) => {
    const stream = new FakeStream(`${participant}|${trackSid}`);
    r.onMediaTrackAdded(fakeTrack(trackSid), stream, {});
    return stream;
  };

  it('media after metadata is delivered at once', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    const events = eventsOf(room);
    media(r, 'PA_1', 'TR_1');
    expect(events).toContain('trackSubscribed');
    expect(room.remoteParticipants.get('alice')?.getTrackPublication(TrackSource.Camera)?.track).toBeDefined();
  });

  it('media before its participant is delivered when the participant appears', () => {
    const { room, r } = connectedRoom();
    const events = eventsOf(room);
    media(r, 'PA_1', 'TR_1');
    expect(events).toEqual([]);
    vi.advanceTimersByTime(1000);
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    expect(events).toContain('trackSubscribed');
  });

  it('media before its publication is delivered when the track is announced', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1)]);
    const events = eventsOf(room);
    media(r, 'PA_1', 'TR_1');
    r.handleParticipantUpdates([info('alice', 'PA_1', 2, { tracks: [trackInfo('TR_1')] })]);
    expect(events).toContain('trackSubscribed');
  });

  it('after a while the application hears, and a late update can still deliver it', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1)]);
    const events = eventsOf(room);
    media(r, 'PA_1', 'TR_1');
    vi.advanceTimersByTime(2999);
    expect(events).not.toContain('trackSubscriptionFailed');
    vi.advanceTimersByTime(1);
    expect(events).toContain('trackSubscriptionFailed');
    r.handleParticipantUpdates([info('alice', 'PA_1', 2, { tracks: [trackInfo('TR_1')] })]);
    expect(events).toContain('trackSubscribed');
  });

  it('nothing waits for media that has already been taken away', () => {
    const { r } = connectedRoom();
    const track = fakeTrack('TR_1');
    const stream = new FakeStream('PA_1|TR_1');
    r.onMediaTrackAdded(track, stream, {});
    expect(r.pendingMedia).toHaveLength(1);
    stream.dispatchEvent(Object.assign(new Event('removetrack'), { track }));
    expect(r.pendingMedia).toHaveLength(0);
  });

  it('media waits while connecting, and is delivered once connected', () => {
    const { room, r } = connectedRoom();
    r.state = ConnectionState.Connecting;
    r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1')] })]);
    media(r, 'PA_1', 'TR_1');
    expect(room.remoteParticipants.get('alice')?.getTrackPublication(TrackSource.Camera)?.track).toBeUndefined();
    r.state = ConnectionState.Connected;
    r.deliverPendingMedia();
    expect(room.remoteParticipants.get('alice')?.getTrackPublication(TrackSource.Camera)?.track).toBeDefined();
  });

  it('ignores this client seeing its own track come back', () => {
    const { r } = connectedRoom();
    r.localParticipant.sid = 'PA_me';
    media(r, 'PA_me', 'TR_1');
    expect(r.pendingMedia).toHaveLength(0);
  });

  it('a subscription the server says failed stops waiting and reports the reason', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('alice', 'PA_1', 1)]);
    let seen: unknown[] = [];
    room.on(RoomEvent.TrackSubscriptionFailed, (sid, p, reason) => {
      seen = [sid, p?.identity, reason];
    });
    media(r, 'PA_1', 'TR_1');
    r.onSubscriptionError('TR_1', 2);
    expect(r.pendingMedia).toHaveLength(0);
    expect(seen[0]).toBe('TR_1');
    expect(seen[2]).toBe(2);
  });
});

describe('publications', () => {
  function withPublication(autoSubscribe = true) {
    const ctx = connectedRoom();
    ctx.r.connectOptions.autoSubscribe = autoSubscribe;
    ctx.r.handleParticipantUpdates([info('alice', 'PA_1', 1, { tracks: [trackInfo('TR_1', { layers: [] })] })]);
    return { ...ctx, pub: ctx.room.remoteParticipants.get('alice')!.getTrackPublication(TrackSource.Camera)! };
  }

  it('follow the subscription through its states', () => {
    const { r, pub, sent } = withPublication();
    expect(pub.subscriptionStatus).toBe(SubscriptionStatus.Desired);
    r.onMediaTrackAdded(fakeTrack('TR_1'), new FakeStream('PA_1|TR_1'), {});
    expect(pub.subscriptionStatus).toBe(SubscriptionStatus.Subscribed);
    expect(pub.isSubscribed).toBe(true);
    pub.setSubscribed(false);
    expect(pub.subscriptionStatus).toBe(SubscriptionStatus.Unsubscribed);
    expect(pub.isSubscribed).toBe(false);
    const update = sent.find((s) => s.kind === 'subscription')?.value as any;
    expect(update.subscribe).toBe(false);
    expect(update.trackSids).toEqual(['TR_1']);
    expect(update.participantTracks[0].participantSid).toBe('PA_1');
  });

  it('with auto-subscribe off, start out unsubscribed', () => {
    const { pub } = withPublication(false);
    expect(pub.subscriptionStatus).toBe(SubscriptionStatus.Unsubscribed);
    expect(pub.isDesired).toBe(false);
  });

  it('a removed stream track ends the subscription', () => {
    const { room, r, pub } = withPublication();
    const track = fakeTrack('TR_1');
    const stream = new FakeStream('PA_1|TR_1');
    r.onMediaTrackAdded(track, stream, {});
    const events = eventsOf(room);
    stream.dispatchEvent(Object.assign(new Event('removetrack'), { track }));
    expect(pub.track).toBeUndefined();
    expect(events).toContain('trackUnsubscribed');
  });

  it('only the removal of its own track ends a subscription, in a stream shared with another', () => {
    const { r, pub } = withPublication();
    const stream = new FakeStream('PA_1|camera');
    const video = fakeTrack('TR_1');
    r.onMediaTrackAdded(video, stream, {});
    stream.dispatchEvent(Object.assign(new Event('removetrack'), { track: fakeTrack('TR_2') }));
    expect(pub.track).toBeDefined();
  });

  it('a track that is not allowed reports so, and subscribing clears that until the server says otherwise', () => {
    const { pub } = withPublication();
    (pub as any).setAllowed(false);
    expect(pub.permissionStatus).toBe(PermissionStatus.NotAllowed);
    pub.setSubscribed(true);
    expect(pub.permissionStatus).toBe(PermissionStatus.Allowed);
  });

  it('a mute from the server shows on the publication and fires the event', () => {
    const { room, r } = withPublication();
    const events = eventsOf(room);
    r.handleParticipantUpdates([info('alice', 'PA_1', 2, { tracks: [trackInfo('TR_1', { muted: true })] })]);
    expect(events).toContain('trackMuted');
    expect(room.remoteParticipants.get('alice')!.getTrackPublication(TrackSource.Camera)!.isMuted).toBe(true);
  });

  it('settings go out for each change', () => {
    const { pub, sent } = withPublication();
    pub.setVideoQuality(VideoQuality.LOW);
    pub.setEnabled(false);
    const settings = sent.filter((s) => s.kind === 'settings').map((s) => s.value as any);
    expect(settings[0]).toMatchObject({ trackSids: ['TR_1'], disabled: false, quality: 0 });
    expect(settings[1]).toMatchObject({ disabled: true });
  });
});

describe('speakers', () => {
  const speaker = (sid: string, level: number, active: boolean) => create(SpeakerInfoSchema, { sid, level, active });

  it('over signalling are changes, sorted loudest first', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('a', 'PA_a', 1), info('b', 'PA_b', 1)]);
    r.handleSpeakerDeltas([speaker('PA_a', 0.2, true)]);
    r.handleSpeakerDeltas([speaker('PA_b', 0.8, true)]);
    expect(room.activeSpeakers.map((p) => p.identity)).toEqual(['b', 'a']);
    r.handleSpeakerDeltas([speaker('PA_b', 0, false)]);
    expect(room.activeSpeakers.map((p) => p.identity)).toEqual(['a']);
    expect(room.remoteParticipants.get('b')!.isSpeaking).toBe(false);
  });

  it('over a data channel are the whole list', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('a', 'PA_a', 1), info('b', 'PA_b', 1)]);
    r.handleSpeakerList([speaker('PA_a', 0.5, true), speaker('PA_b', 0.5, true)]);
    r.handleSpeakerList([speaker('PA_b', 0.5, true)]);
    expect(room.activeSpeakers.map((p) => p.identity)).toEqual(['b']);
    expect(room.remoteParticipants.get('a')!.isSpeaking).toBe(false);
    expect(room.remoteParticipants.get('a')!.audioLevel).toBe(0);
  });

  it('ignore people nobody knows of', () => {
    const { room, r } = connectedRoom();
    r.handleSpeakerDeltas([speaker('PA_ghost', 1, true)]);
    expect(room.activeSpeakers).toEqual([]);
  });
});

describe('connection quality', () => {
  it('fires per participant, only on change', () => {
    const { room, r } = connectedRoom();
    r.handleParticipantUpdates([info('a', 'PA_a', 1)]);
    const a = room.remoteParticipants.get('a')!;
    const seen: ConnectionQuality[] = [];
    a.on('connectionQualityChanged', (q) => seen.push(q));
    a.setConnectionQuality(ConnectionQuality.Good);
    a.setConnectionQuality(ConnectionQuality.Good);
    a.setConnectionQuality(ConnectionQuality.Poor);
    expect(seen).toEqual([ConnectionQuality.Good, ConnectionQuality.Poor]);
  });
});

describe('sync state', () => {
  it('says what differs from the subscribe default, and what is disabled and published', () => {
    const { r, room } = connectedRoom();
    r.handleParticipantUpdates([info('a', 'PA_a', 1, { tracks: [trackInfo('TR_1'), trackInfo('TR_2', { source: ProtoSource.SCREEN_SHARE })] })]);
    const p = room.remoteParticipants.get('a')!;
    p.getTrackPublication(TrackSource.Camera)!.setSubscribed(false);
    p.getTrackPublication(TrackSource.ScreenShare)!.setEnabled(false);
    const s = r.syncState();
    expect(s.subscription.trackSids).toEqual(['TR_1']);
    expect(s.subscription.subscribe).toBe(false);
    expect(s.trackSidsDisabled).toEqual(['TR_2']);
    expect(s.publishTracks).toEqual([]);
  });

  it('with auto-subscribe off, names what this client does want', () => {
    const { r, room } = connectedRoom();
    r.connectOptions.autoSubscribe = false;
    r.handleParticipantUpdates([info('a', 'PA_a', 1, { tracks: [trackInfo('TR_1'), trackInfo('TR_2')] })]);
    room.remoteParticipants.get('a')!.trackPublications.get('TR_2');
    (room.remoteParticipants.get('a')!.trackPublications.get('TR_2') as any).setSubscribed(true);
    const s = r.syncState();
    expect(s.subscription.trackSids).toEqual(['TR_2']);
    expect(s.subscription.subscribe).toBe(true);
  });
});

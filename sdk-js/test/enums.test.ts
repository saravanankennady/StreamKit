import { describe, expect, it } from 'vitest';
import {
  ConnectionQuality,
  ConnectionState,
  DataPacketKind,
  DisconnectReason,
  ParticipantEvent,
  ParticipantKind,
  PermissionStatus,
  RoomEvent,
  StreamState,
  SubscriptionError,
  SubscriptionStatus,
  TrackKind,
  TrackSource,
  VideoQuality,
} from '../src/index.js';
import {
  DataPacket_Kind,
  DisconnectReason as ProtoDisconnectReason,
  ParticipantInfo_Kind,
  SubscriptionError as ProtoSubscriptionError,
  VideoQuality as ProtoVideoQuality,
} from '../src/proto/livekit_models_pb.js';

// Applications compare against these and log them, so a change here breaks people's code silently.
describe('public string values', () => {
  it('ConnectionState', () => {
    expect(Object.values(ConnectionState).sort()).toEqual(['connected', 'connecting', 'disconnected', 'reconnecting', 'signalReconnecting']);
  });
  it('TrackKind, TrackSource and StreamState', () => {
    expect(TrackKind).toMatchObject({ Audio: 'audio', Video: 'video', Unknown: 'unknown' });
    expect(TrackSource).toMatchObject({ Camera: 'camera', Microphone: 'microphone', ScreenShare: 'screen_share', ScreenShareAudio: 'screen_share_audio', Unknown: 'unknown' });
    expect(StreamState).toMatchObject({ Active: 'active', Paused: 'paused', Unknown: 'unknown' });
  });
  it('subscription and permission status', () => {
    expect(SubscriptionStatus).toMatchObject({ Desired: 'desired', Subscribed: 'subscribed', Unsubscribed: 'unsubscribed' });
    expect(PermissionStatus).toMatchObject({ Allowed: 'allowed', NotAllowed: 'not_allowed' });
  });
  it('ConnectionQuality', () => {
    expect(ConnectionQuality).toMatchObject({ Excellent: 'excellent', Good: 'good', Poor: 'poor', Lost: 'lost', Unknown: 'unknown' });
  });
  it('RoomEvent', () => {
    expect(RoomEvent).toMatchObject({
      Connected: 'connected',
      Reconnecting: 'reconnecting',
      SignalReconnecting: 'signalReconnecting',
      Reconnected: 'reconnected',
      Disconnected: 'disconnected',
      ConnectionStateChanged: 'connectionStateChanged',
      ParticipantConnected: 'participantConnected',
      ParticipantDisconnected: 'participantDisconnected',
      TrackPublished: 'trackPublished',
      TrackSubscribed: 'trackSubscribed',
      TrackSubscriptionFailed: 'trackSubscriptionFailed',
      TrackUnpublished: 'trackUnpublished',
      TrackUnsubscribed: 'trackUnsubscribed',
      TrackMuted: 'trackMuted',
      TrackUnmuted: 'trackUnmuted',
      LocalTrackPublished: 'localTrackPublished',
      LocalTrackUnpublished: 'localTrackUnpublished',
      ActiveSpeakersChanged: 'activeSpeakersChanged',
      ParticipantMetadataChanged: 'participantMetadataChanged',
      DataReceived: 'dataReceived',
      ConnectionQualityChanged: 'connectionQualityChanged',
      // Name and value differ here, deliberately.
      AudioPlaybackStatusChanged: 'audioPlaybackChanged',
      VideoPlaybackStatusChanged: 'videoPlaybackChanged',
      SignalConnected: 'signalConnected',
      ActiveDeviceChanged: 'activeDeviceChanged',
    });
  });
  it('ParticipantEvent', () => {
    expect(ParticipantEvent).toMatchObject({
      TrackPublished: 'trackPublished',
      TrackSubscribed: 'trackSubscribed',
      IsSpeakingChanged: 'isSpeakingChanged',
      ConnectionQualityChanged: 'connectionQualityChanged',
      AttributesChanged: 'attributesChanged',
      Active: 'active',
    });
  });
});

// Ours are declared by hand, to keep the generated names out of the public API. They must stay the wire values.
describe('wire values', () => {
  const sameMembers = (ours: Record<string, unknown>, theirs: Record<string, unknown>) => {
    for (const [k, v] of Object.entries(ours)) {
      if (typeof v === 'number') expect(theirs[k], k).toBe(v);
    }
  };
  it('match the generated enums', () => {
    sameMembers(VideoQuality, ProtoVideoQuality);
    sameMembers(DisconnectReason, ProtoDisconnectReason);
    sameMembers(SubscriptionError, ProtoSubscriptionError);
    sameMembers(DataPacketKind, DataPacket_Kind);
    sameMembers(ParticipantKind, ParticipantInfo_Kind);
  });
});

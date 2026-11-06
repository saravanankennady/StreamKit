import { type LocalParticipant, ParticipantEvent, type Participant, type RemoteParticipant, type Room, RoomEvent, TrackSource } from '@elnova-streamkit/client';
import { useMemo } from 'react';
import { useEnsureParticipant, useEnsureRoom } from '../context.js';
import { useEventVersion } from '../useEvents.js';

const PARTICIPANT_EVENTS = [
  RoomEvent.ParticipantConnected,
  RoomEvent.ParticipantDisconnected,
  RoomEvent.ConnectionStateChanged,
  RoomEvent.ParticipantMetadataChanged,
  RoomEvent.ParticipantNameChanged,
  RoomEvent.ParticipantAttributesChanged,
  RoomEvent.ParticipantPermissionsChanged,
  RoomEvent.TrackPublished,
  RoomEvent.TrackUnpublished,
  RoomEvent.TrackSubscribed,
  RoomEvent.TrackUnsubscribed,
  RoomEvent.TrackMuted,
  RoomEvent.TrackUnmuted,
  RoomEvent.LocalTrackPublished,
  RoomEvent.LocalTrackUnpublished,
  RoomEvent.ConnectionQualityChanged,
  RoomEvent.ActiveSpeakersChanged,
];

export interface UseParticipantsOptions {
  /** Update only on these room events, rather than on anything to do with a participant. */
  updateOnlyOn?: RoomEvent[];
  room?: Room;
}

export function useRemoteParticipants(options: UseParticipantsOptions = {}): RemoteParticipant[] {
  const room = useEnsureRoom(options.room);
  const version = useEventVersion(room, options.updateOnlyOn ?? PARTICIPANT_EVENTS);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => [...room.remoteParticipants.values()], [room, version]);
}

/** Everyone in the room, starting with this client. */
export function useParticipants(options: UseParticipantsOptions = {}): Participant[] {
  const room = useEnsureRoom(options.room);
  const remote = useRemoteParticipants(options);
  return useMemo(() => [room.localParticipant, ...remote], [room, remote]);
}

const LOCAL_EVENTS = [
  ParticipantEvent.TrackMuted,
  ParticipantEvent.TrackUnmuted,
  ParticipantEvent.LocalTrackPublished,
  ParticipantEvent.LocalTrackUnpublished,
  ParticipantEvent.ParticipantPermissionsChanged,
  ParticipantEvent.ParticipantMetadataChanged,
  ParticipantEvent.ParticipantNameChanged,
  ParticipantEvent.AttributesChanged,
  ParticipantEvent.MediaDevicesError,
];

export interface UseLocalParticipantResult {
  localParticipant: LocalParticipant;
  isMicrophoneEnabled: boolean;
  isCameraEnabled: boolean;
  isScreenShareEnabled: boolean;
  microphoneTrack?: ReturnType<LocalParticipant['getTrackPublication']>;
  cameraTrack?: ReturnType<LocalParticipant['getTrackPublication']>;
  lastMicrophoneError?: Error;
  lastCameraError?: Error;
}

export function useLocalParticipant(options: { room?: Room } = {}): UseLocalParticipantResult {
  const room = useEnsureRoom(options.room);
  const lp = room.localParticipant;
  useEventVersion(lp, LOCAL_EVENTS);
  return {
    localParticipant: lp,
    isMicrophoneEnabled: lp.isMicrophoneEnabled,
    isCameraEnabled: lp.isCameraEnabled,
    isScreenShareEnabled: lp.isScreenShareEnabled,
    microphoneTrack: lp.getTrackPublication(TrackSource.Microphone),
    cameraTrack: lp.getTrackPublication(TrackSource.Camera),
    lastMicrophoneError: lp.lastMicrophoneError,
    lastCameraError: lp.lastCameraError,
  };
}

const SPEAKING_EVENTS = [ParticipantEvent.IsSpeakingChanged];

export function useIsSpeaking(participant?: Participant): boolean {
  const p = useEnsureParticipant(participant);
  useEventVersion(p, SPEAKING_EVENTS);
  return p.isSpeaking;
}

const QUALITY_EVENTS = [ParticipantEvent.ConnectionQualityChanged];

/** The connection quality the server reports for a participant, by default the one from context. */
export function useConnectionQuality(participant?: Participant) {
  const p = useEnsureParticipant(participant);
  useEventVersion(p, QUALITY_EVENTS);
  return p.connectionQuality;
}

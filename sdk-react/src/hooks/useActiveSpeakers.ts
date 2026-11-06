import { type Participant, type Room, RoomEvent } from '@elnova-streamkit/client';
import { useEnsureRoom } from '../context.js';
import { useEventVersion } from '../useEvents.js';

const EVENTS = [RoomEvent.ActiveSpeakersChanged, RoomEvent.ConnectionStateChanged, RoomEvent.ParticipantDisconnected];

/** Who is speaking now, loudest first. */
export function useActiveSpeakers(options: { room?: Room } = {}): Participant[] {
  const room = useEnsureRoom(options.room);
  useEventVersion(room, EVENTS);
  return room.activeSpeakers;
}

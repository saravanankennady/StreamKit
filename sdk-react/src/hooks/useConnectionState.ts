import { type ConnectionState, type Room, RoomEvent } from '@elnova-streamkit/client';
import { useEnsureRoom } from '../context.js';
import { useEventVersion } from '../useEvents.js';

const EVENTS = [RoomEvent.ConnectionStateChanged];

export function useConnectionState(room?: Room): ConnectionState {
  const r = useEnsureRoom(room);
  useEventVersion(r, EVENTS);
  return r.state;
}

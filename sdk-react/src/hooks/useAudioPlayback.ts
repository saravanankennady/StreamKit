import { type Room, RoomEvent } from '@elnova-streamkit/client';
import { useCallback } from 'react';
import { useEnsureRoom } from '../context.js';
import { useEventVersion } from '../useEvents.js';

const EVENTS = [RoomEvent.AudioPlaybackStatusChanged];

/** Browsers can hold audio back until a user gesture. This says if they have, and starts it from a click. */
export function useAudioPlayback(room?: Room) {
  const r = useEnsureRoom(room);
  useEventVersion(r, EVENTS);
  const startAudio = useCallback(() => r.startAudio(), [r]);
  return { canPlayAudio: r.canPlaybackAudio, startAudio };
}

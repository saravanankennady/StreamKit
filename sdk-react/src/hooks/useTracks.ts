import { type Participant, type Room, RoomEvent, TrackSource } from '@elnova-streamkit/client';
import { useMemo } from 'react';
import { useEnsureRoom } from '../context.js';
import type { TrackReference, TrackReferenceOrPlaceholder } from '../trackReference.js';
import { useEventVersion } from '../useEvents.js';

const EVENTS = [
  RoomEvent.ParticipantConnected,
  RoomEvent.ParticipantDisconnected,
  RoomEvent.LocalTrackPublished,
  RoomEvent.LocalTrackUnpublished,
  RoomEvent.TrackPublished,
  RoomEvent.TrackUnpublished,
  RoomEvent.TrackSubscribed,
  RoomEvent.TrackUnsubscribed,
  RoomEvent.TrackSubscriptionStatusChanged,
  RoomEvent.TrackMuted,
  RoomEvent.TrackUnmuted,
  RoomEvent.ConnectionStateChanged,
];

const ALL_SOURCES: TrackSource[] = [TrackSource.Camera, TrackSource.Microphone, TrackSource.ScreenShare, TrackSource.ScreenShareAudio, TrackSource.Unknown];

export interface SourceWithPlaceholder {
  source: TrackSource;
  withPlaceholder: boolean;
}

export interface UseTracksOptions {
  /** Leave out tracks not being received. Defaults to true. */
  onlySubscribed?: boolean;
  /** More room events to update on, such as active speakers for a layout that follows them. */
  updateOnlyOn?: RoomEvent[];
  room?: Room;
}

function publicationsFor(participant: Participant, source: TrackSource, onlySubscribed: boolean): TrackReference[] {
  return participant
    .getTrackPublications()
    .filter((pub) => pub.source === source && (!onlySubscribed || pub.track !== undefined))
    .map((publication) => ({ participant, publication, source }));
}

/**
 * Tracks in the room, in participant order. A source given with withPlaceholder yields a placeholder for any
 * participant without that track, so a layout can reserve the space.
 */
export function useTracks(
  sources: Array<TrackSource | SourceWithPlaceholder> = ALL_SOURCES,
  options: UseTracksOptions = {},
): TrackReferenceOrPlaceholder[] {
  const room = useEnsureRoom(options.room);
  const onlySubscribed = options.onlySubscribed ?? true;
  const events = useMemo(() => [...EVENTS, ...(options.updateOnlyOn ?? [])], [options.updateOnlyOn?.join(' ')]);
  const version = useEventVersion(room, events);
  const key = JSON.stringify(sources);
  return useMemo(() => {
    const out: TrackReferenceOrPlaceholder[] = [];
    const parsed = sources.map((s) => (typeof s === 'string' ? { source: s, withPlaceholder: false } : s));
    for (const participant of [room.localParticipant, ...room.remoteParticipants.values()]) {
      for (const { source, withPlaceholder } of parsed) {
        const found = publicationsFor(participant, source, onlySubscribed);
        if (found.length > 0) out.push(...found);
        else if (withPlaceholder) out.push({ participant, source });
      }
    }
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [room, key, onlySubscribed, version]);
}

import { type Participant, ParticipantEvent, type RemoteTrack, type Track, TrackSource } from '@elnova-streamkit/client';
import { useEventVersion } from '../useEvents.js';

const EVENTS = [
  ParticipantEvent.TrackPublished,
  ParticipantEvent.TrackUnpublished,
  ParticipantEvent.TrackSubscribed,
  ParticipantEvent.TrackUnsubscribed,
  ParticipantEvent.LocalTrackPublished,
  ParticipantEvent.LocalTrackUnpublished,
  ParticipantEvent.TrackMuted,
  ParticipantEvent.TrackUnmuted,
];

/**
 * A participant's current publication of a source, its media, and whether that is muted. This follows the
 * participant rather than a publication captured earlier, because unpublishing and publishing again makes a
 * new publication.
 */
export function useParticipantTrack(participant: Participant | undefined, source: TrackSource, trackSid?: string) {
  useEventVersion(participant, EVENTS);
  const publication = trackSid ? participant?.trackPublications.get(trackSid) : participant?.getTrackPublication(source);
  return {
    publication,
    track: publication?.track as Track | RemoteTrack | undefined,
    isMuted: publication?.isMuted ?? true,
    isSubscribed: publication?.track !== undefined,
  };
}

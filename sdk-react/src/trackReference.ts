import type { Participant, TrackPublication, TrackSource } from '@elnova-streamkit/client';

/** A published track, which may or may not be subscribed: its media is at `publication.track`. */
export interface TrackReference {
  participant: Participant;
  publication: TrackPublication;
  source: TrackSource;
}

/** A place for a track that does not exist yet, so a layout can hold a tile for someone with their camera off. */
export interface TrackReferencePlaceholder {
  participant: Participant;
  publication?: undefined;
  source: TrackSource;
}

export type TrackReferenceOrPlaceholder = TrackReference | TrackReferencePlaceholder;

export function isTrackReference(ref: TrackReferenceOrPlaceholder | undefined): ref is TrackReference {
  return ref?.publication !== undefined;
}

export function isTrackReferencePlaceholder(ref: TrackReferenceOrPlaceholder | undefined): ref is TrackReferencePlaceholder {
  return ref !== undefined && ref.publication === undefined;
}

/** A stable ID, for React keys. */
export function getTrackReferenceId(ref: TrackReferenceOrPlaceholder): string {
  const { identity } = ref.participant;
  return `${identity}_${ref.source}_${ref.publication ? ref.publication.trackSid : 'placeholder'}`;
}

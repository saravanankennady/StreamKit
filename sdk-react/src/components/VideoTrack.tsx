import { TrackKind } from '@elnova-streamkit/client';
import { type VideoHTMLAttributes, useEffect, useRef } from 'react';
import { useEnsureTrackRef } from '../context.js';
import { useParticipantTrack } from '../hooks/useParticipantTrack.js';
import type { TrackReferenceOrPlaceholder } from '../trackReference.js';

export interface VideoTrackProps extends VideoHTMLAttributes<HTMLVideoElement> {
  /** The track to show. Defaults to the one in context. */
  trackRef?: TrackReferenceOrPlaceholder;
}

/**
 * Plays a video track. Attaching is what starts adaptive stream watching the element, so a track shown here
 * gets the layer that fits it.
 */
export function VideoTrack({ trackRef, ...props }: VideoTrackProps) {
  const ref = useEnsureTrackRef(trackRef);
  const { track, isMuted } = useParticipantTrack(ref.participant, ref.source, ref.publication?.trackSid);
  const element = useRef<HTMLVideoElement>(null);

  useEffect(() => {
    const el = element.current;
    if (!el || !track || track.kind !== TrackKind.Video) return;
    track.attach(el);
    return () => {
      track.detach(el);
    };
  }, [track]);

  return (
    <video
      ref={element}
      muted
      playsInline
      autoPlay
      data-identity={ref.participant.identity}
      data-source={ref.source}
      data-sid={ref.publication?.trackSid}
      data-muted={isMuted || undefined}
      {...props}
    />
  );
}

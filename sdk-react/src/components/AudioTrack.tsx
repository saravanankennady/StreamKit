import { RemoteAudioTrack, TrackKind } from '@elnova-streamkit/client';
import { type AudioHTMLAttributes, useEffect, useRef } from 'react';
import { useEnsureTrackRef } from '../context.js';
import { useParticipantTrack } from '../hooks/useParticipantTrack.js';
import type { TrackReferenceOrPlaceholder } from '../trackReference.js';

export interface AudioTrackProps extends AudioHTMLAttributes<HTMLAudioElement> {
  /** The track to play. Defaults to the one in context. */
  trackRef?: TrackReferenceOrPlaceholder;
  /** Between 0 and 1. */
  volume?: number;
  /** Stops the server sending this track. Unlike muting the element, this saves the bandwidth too. */
  muted?: boolean;
}

/** Plays an audio track. A local track never plays here, or people would hear themselves. */
export function AudioTrack({ trackRef, volume, muted, ...props }: AudioTrackProps) {
  const ref = useEnsureTrackRef(trackRef);
  const { track, publication } = useParticipantTrack(ref.participant, ref.source, ref.publication?.trackSid);
  const element = useRef<HTMLAudioElement>(null);

  useEffect(() => {
    const el = element.current;
    if (!el || !track || track.kind !== TrackKind.Audio || track.isLocal) return;
    track.attach(el);
    return () => {
      track.detach(el);
    };
  }, [track]);

  useEffect(() => {
    if (volume !== undefined && track instanceof RemoteAudioTrack) track.setVolume(volume);
  }, [track, volume]);

  useEffect(() => {
    if (muted === undefined || !publication || publication.isLocal) return;
    (publication as { setEnabled?: (enabled: boolean) => void }).setEnabled?.(!muted);
  }, [publication, muted]);

  return <audio ref={element} autoPlay data-identity={ref.participant.identity} data-source={ref.source} data-sid={ref.publication?.trackSid} {...props} />;
}

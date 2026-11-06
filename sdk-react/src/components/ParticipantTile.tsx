import { type Participant, TrackSource } from '@elnova-streamkit/client';
import type { HTMLAttributes, ReactNode } from 'react';
import { ParticipantContext, TrackRefContext, useMaybeParticipantContext, useMaybeTrackRefContext } from '../context.js';
import { useIsSpeaking, useConnectionQuality } from '../hooks/useParticipants.js';
import { useParticipantTrack } from '../hooks/useParticipantTrack.js';
import type { TrackReferenceOrPlaceholder } from '../trackReference.js';
import { AudioTrack } from './AudioTrack.js';
import { VideoTrack } from './VideoTrack.js';

export interface ParticipantTileProps extends HTMLAttributes<HTMLDivElement> {
  /** The participant to show. Defaults to the one in context. */
  participant?: Participant;
  /** The track to show. By default, this participant's camera. */
  trackRef?: TrackReferenceOrPlaceholder;
  /** Replaces the default layout. Everything inside has this participant and track in context. */
  children?: ReactNode;
}

/**
 * One person's video, or a placeholder when there is none, with their name and state. The state is on data
 * attributes for styling: data-speaking, data-video-muted, data-audio-muted and data-quality.
 */
export function ParticipantTile({ participant, trackRef, children, ...props }: ParticipantTileProps) {
  const contextParticipant = useMaybeParticipantContext();
  const contextTrack = useMaybeTrackRefContext();
  const ref = trackRef ?? contextTrack;
  const p = participant ?? ref?.participant ?? contextParticipant;
  if (!p) throw new Error('no participant: pass one, or use this inside a ParticipantContext');
  const source = ref?.source ?? TrackSource.Camera;

  const speaking = useIsSpeaking(p);
  const quality = useConnectionQuality(p);
  const video = useParticipantTrack(p, source, ref?.publication?.trackSid);
  const audio = useParticipantTrack(p, TrackSource.Microphone);

  const finalRef: TrackReferenceOrPlaceholder = video.publication ? { participant: p, publication: video.publication, source } : { participant: p, source };

  return (
    <div
      data-streamkit-participant-tile
      data-identity={p.identity}
      data-source={source}
      data-speaking={speaking}
      data-video-muted={video.isMuted}
      data-audio-muted={audio.isMuted}
      data-quality={quality}
      {...props}
    >
      <ParticipantContext.Provider value={p}>
        <TrackRefContext.Provider value={finalRef}>
          {children ?? (
            <>
              {video.isSubscribed ? <VideoTrack trackRef={finalRef} /> : <div data-streamkit-placeholder />}
              {!p.isLocal && audio.publication && audio.isSubscribed ? (
                <AudioTrack trackRef={{ participant: p, publication: audio.publication, source: TrackSource.Microphone }} />
              ) : null}
              <div data-streamkit-name>{p.name || p.identity}</div>
            </>
          )}
        </TrackRefContext.Provider>
      </ParticipantContext.Provider>
    </div>
  );
}

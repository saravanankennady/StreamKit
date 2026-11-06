import { type Room, TrackKind, TrackSource } from '@elnova-streamkit/client';
import { useTracks } from '../hooks/useTracks.js';
import { getTrackReferenceId } from '../trackReference.js';
import { AudioTrack } from './AudioTrack.js';

const SOURCES = [TrackSource.Microphone, TrackSource.ScreenShareAudio, TrackSource.Unknown];

export interface RoomAudioRendererProps {
  /** Between 0 and 1, for every track. */
  volume?: number;
  /** Stops the server sending audio at all. */
  muted?: boolean;
  room?: Room;
}

/** Plays everyone else's audio. A room needs one of these for anyone to be heard. */
export function RoomAudioRenderer({ volume, muted, room }: RoomAudioRendererProps) {
  const tracks = useTracks(SOURCES, { onlySubscribed: true, room }).filter((ref) => !ref.participant.isLocal && ref.publication?.kind === TrackKind.Audio);
  return (
    <div style={{ display: 'none' }} data-streamkit-room-audio>
      {tracks.map((ref) => (
        <AudioTrack key={getTrackReferenceId(ref)} trackRef={ref} volume={volume} muted={muted} />
      ))}
    </div>
  );
}

// The interface for avatars. No vendor is implemented yet: Anam, Tavus and a self-hosted engine are later work. See the Python
// framework's avatar.py for the contract, which is the same here.
import type { AudioFrame } from './rtc/audio.ts';
import { mintToken } from './token.ts';

export const ATTR_PUBLISH_ON_BEHALF = 'lk.publish_on_behalf';
export const AUDIO_STREAM_TOPIC = 'lk.audio_stream';
export const RPC_CLEAR_BUFFER = 'lk.clear_buffer';
export const RPC_PLAYBACK_STARTED = 'lk.playback_started';
export const RPC_PLAYBACK_FINISHED = 'lk.playback_finished';

export interface AvatarJoin {
  url: string;
  token: string;
  identity: string;
}

/** Signs the token for an avatar worker. It can join and publish in one room, and nothing else. */
export function avatarJoin(url: string, apiKey: string, apiSecret: string, options: { room: string; identity: string; onBehalfOf: string; name?: string; ttlSeconds?: number }): AvatarJoin {
  const token = mintToken(apiKey, apiSecret, {
    identity: options.identity,
    name: options.name ?? options.identity,
    kind: 'agent',
    video: { roomJoin: true, room: options.room, canPublish: true, canSubscribe: true, canPublishData: true },
    attributes: { [ATTR_PUBLISH_ON_BEHALF]: options.onBehalfOf },
    ttlSeconds: options.ttlSeconds,
  });
  return { url, token, identity: options.identity };
}

/** A vendor's service that renders an avatar and joins the room itself. */
export interface AvatarProvider {
  readonly name: string;
  start(join: AvatarJoin): Promise<void>;
  stop(): Promise<void>;
}

export interface VideoFrame {
  width: number;
  height: number;
  data: Uint8Array;
  timestamp: number;
}

/** A self-hosted engine: audio goes in, and audio and video come out together, which is what keeps lips in sync. */
export interface VideoGenerator {
  pushAudio(frame: AudioFrame): Promise<void>;
  clearBuffer(): Promise<void>;
  [Symbol.asyncIterator](): AsyncIterator<AudioFrame | VideoFrame>;
}

import type { ReconnectPolicy } from './engine/reconnectPolicy.js';
import type { AudioCaptureOptions, TrackPublishOptions, VideoCaptureOptions } from './track/options.js';
import type { AdaptiveStreamSettings } from './track/RemoteVideoTrack.js';

export interface RoomOptions {
  /**
   * Receive each video only as large as it is shown, and not at all while it cannot be seen. This depends
   * on tracks being attached to elements: a video attached to nothing receives nothing.
   */
  adaptiveStream?: boolean | AdaptiveStreamSettings;
  /** Stop encoding video layers nobody is watching. On by default. */
  dynacast?: boolean;
  audioCaptureDefaults?: AudioCaptureOptions;
  videoCaptureDefaults?: VideoCaptureOptions;
  publishDefaults?: TrackPublishOptions;
  /** Stop capture when a track is unpublished. On by default. */
  stopLocalTrackOnUnpublish?: boolean;
  /** Leave the room when the page goes away, so others see that straight away. On by default. */
  disconnectOnPageLeave?: boolean;
  reconnectPolicy?: ReconnectPolicy;
  audioOutput?: { deviceId?: string };
}

export interface RoomConnectOptions {
  /** Receive every track without asking. On by default. */
  autoSubscribe?: boolean;
  /** How long to wait for the WebSocket upgrade. */
  websocketTimeout?: number;
  /** How long to wait for a peer connection to come up, or for a negotiation to be answered. */
  peerConnectionTimeout?: number;
  /** Connection attempts when the server cannot be reached. */
  maxRetries?: number;
  /** Merged with what the server provides. ICE servers set here replace the server's. */
  rtcConfig?: RTCConfiguration;
}

export const roomOptionDefaults = {
  dynacast: true,
  stopLocalTrackOnUnpublish: true,
  disconnectOnPageLeave: true,
};

export const connectOptionDefaults = {
  autoSubscribe: true,
  websocketTimeout: 15_000,
  peerConnectionTimeout: 15_000,
  maxRetries: 1,
};

import { TrackSource } from '../enums.js';

export type VideoCodec = 'vp8' | 'h264' | 'vp9' | 'av1' | 'h265';
export type TrackPriority = RTCPriorityType;

export interface VideoEncoding {
  maxBitrate: number;
  maxFramerate?: number;
  priority?: TrackPriority;
}

export interface VideoResolution {
  width: number;
  height: number;
  frameRate?: number;
  aspectRatio?: number;
}

export interface VideoPreset {
  width: number;
  height: number;
  encoding: VideoEncoding;
}

export interface VideoCaptureOptions {
  deviceId?: ConstrainDOMString;
  facingMode?: 'user' | 'environment' | 'left' | 'right';
  resolution?: VideoResolution;
}

export interface AudioCaptureOptions {
  deviceId?: ConstrainDOMString;
  autoGainControl?: ConstrainBoolean;
  echoCancellation?: ConstrainBoolean;
  noiseSuppression?: ConstrainBoolean;
  channelCount?: ConstrainULong;
  sampleRate?: ConstrainULong;
  /** Stop the microphone while muted, turning off the browser's indicator. Unmuting takes a moment longer. */
  stopMicTrackOnMute?: boolean;
}

export interface ScreenShareCaptureOptions {
  audio?: boolean | AudioCaptureOptions;
  video?: true | { displaySurface?: 'window' | 'browser' | 'monitor' };
  resolution?: VideoResolution;
  contentHint?: 'detail' | 'text' | 'motion';
  preferCurrentTab?: boolean;
}

export interface CreateLocalTracksOptions {
  audio?: boolean | AudioCaptureOptions;
  video?: boolean | VideoCaptureOptions;
}

export interface TrackPublishOptions {
  name?: string;
  source?: TrackSource;
  /** Groups tracks that should play in sync. By default camera goes with microphone, screen with its audio. */
  stream?: string;
  simulcast?: boolean;
  videoCodec?: VideoCodec;
  videoEncoding?: VideoEncoding;
  /** Lower layers to send under the main one. */
  videoSimulcastLayers?: VideoPreset[];
  screenShareEncoding?: VideoEncoding;
  screenShareSimulcastLayers?: VideoPreset[];
  scalabilityMode?: string;
  /** Audio bitrate in bps. */
  audioBitrate?: number;
  dtx?: boolean;
  red?: boolean;
  forceStereo?: boolean;
  degradationPreference?: 'balanced' | 'maintain-framerate' | 'maintain-resolution';
}

const preset = (width: number, height: number, maxBitrate: number, maxFramerate: number): VideoPreset => ({
  width,
  height,
  encoding: { maxBitrate, maxFramerate },
});

export const VideoPresets = {
  h90: preset(160, 90, 90_000, 20),
  h180: preset(320, 180, 160_000, 20),
  h216: preset(384, 216, 180_000, 20),
  h360: preset(640, 360, 450_000, 20),
  h540: preset(960, 540, 800_000, 25),
  h720: preset(1280, 720, 1_700_000, 30),
  h1080: preset(1920, 1080, 3_000_000, 30),
  h1440: preset(2560, 1440, 5_000_000, 30),
  h2160: preset(3840, 2160, 8_000_000, 30),
} as const;

export const VideoPresets43 = {
  h120: preset(160, 120, 70_000, 20),
  h180: preset(240, 180, 125_000, 20),
  h240: preset(320, 240, 140_000, 20),
  h360: preset(480, 360, 330_000, 20),
  h480: preset(640, 480, 500_000, 20),
  h540: preset(720, 540, 600_000, 25),
  h720: preset(960, 720, 1_300_000, 30),
  h1080: preset(1440, 1080, 2_300_000, 30),
  h1440: preset(1920, 1440, 3_800_000, 30),
} as const;

export const ScreenSharePresets = {
  h360fps3: preset(640, 360, 200_000, 3),
  h360fps15: preset(640, 360, 400_000, 15),
  h720fps5: preset(1280, 720, 800_000, 5),
  h720fps15: preset(1280, 720, 1_500_000, 15),
  h720fps30: preset(1280, 720, 2_000_000, 30),
  h1080fps15: preset(1920, 1080, 2_500_000, 15),
  h1080fps30: preset(1920, 1080, 5_000_000, 30),
  original: preset(0, 0, 7_000_000, 30),
} as const;

export const AudioPresets = {
  telephone: { maxBitrate: 12_000 },
  speech: { maxBitrate: 24_000 },
  music: { maxBitrate: 48_000 },
  musicStereo: { maxBitrate: 64_000 },
  musicHighQuality: { maxBitrate: 96_000 },
  musicHighQualityStereo: { maxBitrate: 128_000 },
} as const;

export const publishDefaults = {
  audioBitrate: AudioPresets.music.maxBitrate,
  dtx: true,
  red: true,
  forceStereo: false,
  simulcast: true,
  videoCodec: 'vp8' as VideoCodec,
  screenShareEncoding: ScreenSharePresets.h1080fps15.encoding,
};

export const audioCaptureDefaults: AudioCaptureOptions = {
  deviceId: { ideal: 'default' },
  autoGainControl: true,
  echoCancellation: true,
  noiseSuppression: true,
};

export const videoCaptureDefaults: VideoCaptureOptions = {
  deviceId: { ideal: 'default' },
  resolution: { width: 1280, height: 720, frameRate: 30 },
};

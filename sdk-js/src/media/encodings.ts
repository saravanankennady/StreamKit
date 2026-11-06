import { VideoQuality } from '../enums.js';
import {
  type VideoCodec,
  type VideoEncoding,
  type VideoPreset,
  ScreenSharePresets,
  VideoPresets,
  VideoPresets43,
  publishDefaults,
} from '../track/options.js';

/** Layer names on the wire, lowest first. A layer's name fixes its quality, however many there are. */
export const SIMULCAST_RIDS = ['q', 'h', 'f'] as const;

export function qualityForRid(rid: string | undefined): VideoQuality {
  switch (rid) {
    case 'q':
      return VideoQuality.LOW;
    case 'h':
      return VideoQuality.MEDIUM;
    default:
      // Includes no rid at all: the only layer of a track is its top layer.
      return VideoQuality.HIGH;
  }
}

export function isSvcCodec(codec: string | undefined) {
  return codec === 'vp9' || codec === 'av1';
}

function presetsForAspect(width: number, height: number, isScreenShare: boolean): VideoPreset[] {
  if (isScreenShare) return Object.values(ScreenSharePresets);
  const longSide = Math.max(width, height);
  const shortSide = Math.min(width, height);
  const aspect = longSide / shortSide;
  return Math.abs(aspect - 16 / 9) < Math.abs(aspect - 4 / 3) ? Object.values(VideoPresets) : Object.values(VideoPresets43);
}

/** The bitrate for a capture size, from the first preset at least that large. */
export function encodingForSize(width: number, height: number, isScreenShare: boolean, codec?: VideoCodec): VideoEncoding {
  const presets = presetsForAspect(width, height, isScreenShare);
  const longSide = Math.max(width, height);
  let encoding = presets[0].encoding;
  for (const p of presets) {
    encoding = p.encoding;
    if (p.width >= longSide) break;
  }
  if (codec === 'av1' || codec === 'h265') return { ...encoding, maxBitrate: encoding.maxBitrate * 0.7 };
  if (codec === 'vp9') return { ...encoding, maxBitrate: encoding.maxBitrate * 0.85 };
  return encoding;
}

/** Lower layers by default. Screen share has one, at half size: legibility is what the bitrate is for. */
export function defaultLowerLayers(width: number, height: number, isScreenShare: boolean, top: VideoEncoding): VideoPreset[] {
  if (isScreenShare) {
    return [
      {
        width: Math.round(width / 2),
        height: Math.round(height / 2),
        encoding: { maxBitrate: Math.max(150_000, Math.floor(top.maxBitrate / 4)), maxFramerate: top.maxFramerate },
      },
    ];
  }
  const longSide = Math.max(width, height);
  const shortSide = Math.min(width, height);
  const aspect = longSide / shortSide;
  return Math.abs(aspect - 16 / 9) < Math.abs(aspect - 4 / 3)
    ? [VideoPresets.h180, VideoPresets.h360]
    : [VideoPresets43.h180, VideoPresets43.h360];
}

export interface EncodingOptions {
  simulcast?: boolean;
  videoCodec?: VideoCodec;
  videoEncoding?: VideoEncoding;
  videoSimulcastLayers?: VideoPreset[];
  screenShareEncoding?: VideoEncoding;
  screenShareSimulcastLayers?: VideoPreset[];
  scalabilityMode?: string;
}

/**
 * Chooses how to encode a video track. Simulcast sizes depend on the capture: a small capture has no room
 * for layers below it, and sending them anyway costs bandwidth for no gain.
 *
 * A capture with room for only one layer is sent without simulcast. Its layer then reports HIGH and its
 * encoding has no rid, and those two have to agree for dynacast updates to mean the same thing to both ends.
 */
export function computeVideoEncodings(
  isScreenShare: boolean,
  captureWidth: number | undefined,
  captureHeight: number | undefined,
  options: EncodingOptions = {},
): RTCRtpEncodingParameters[] {
  const width = captureWidth || 1280;
  const height = captureHeight || 720;
  const codec = options.videoCodec ?? publishDefaults.videoCodec;
  const useSimulcast = options.simulcast ?? publishDefaults.simulcast;

  const explicit = isScreenShare ? (options.screenShareEncoding ?? publishDefaults.screenShareEncoding) : options.videoEncoding;
  const top = explicit ?? encodingForSize(width, height, isScreenShare, codec);

  if (options.scalabilityMode && isSvcCodec(codec)) {
    return [{ ...encodingParams(top), scalabilityMode: options.scalabilityMode } as RTCRtpEncodingParameters];
  }
  if (isSvcCodec(codec)) {
    return [{ ...encodingParams(top), scalabilityMode: isScreenShare ? 'L1T3' : 'L3T3_KEY' } as RTCRtpEncodingParameters];
  }
  if (!useSimulcast) return [encodingParams(top)];

  const custom = isScreenShare ? options.screenShareSimulcastLayers : options.videoSimulcastLayers;
  const lower = custom ?? defaultLowerLayers(width, height, isScreenShare, top);
  const longSide = Math.max(width, height);
  const shortSide = Math.min(width, height);

  let layers: VideoPreset[];
  if (custom) {
    layers = custom.slice(0, 2);
  } else if (longSide >= 960 && lower.length >= 2) {
    layers = lower.slice(0, 2);
  } else if (longSide >= 480) {
    layers = lower.slice(0, 1);
  } else {
    layers = [];
  }
  if (layers.length === 0) return [encodingParams(top)];

  const out: RTCRtpEncodingParameters[] = layers.map((p, i) => ({
    rid: SIMULCAST_RIDS[i],
    scaleResolutionDownBy: Math.max(1, shortSide / Math.min(p.width, p.height)),
    ...encodingParams(p.encoding),
  }));
  out.push({ rid: SIMULCAST_RIDS[layers.length], scaleResolutionDownBy: 1, ...encodingParams(top) });
  return out;
}

function encodingParams(e: VideoEncoding): RTCRtpEncodingParameters {
  const out: RTCRtpEncodingParameters = { maxBitrate: e.maxBitrate };
  if (e.maxFramerate) out.maxFramerate = e.maxFramerate;
  if (e.priority) {
    out.priority = e.priority;
    out.networkPriority = e.priority;
  }
  return out;
}

export interface LayerDescription {
  quality: VideoQuality;
  rid: string;
  width: number;
  height: number;
  bitrate: number;
  spatialLayer: number;
}

const SVC_SPATIAL_LAYERS = /^[LS](\d)/;

/** What AddTrackRequest says about a track's layers. SSRCs are not known yet, so they are left at zero. */
export function describeLayers(width: number, height: number, encodings: RTCRtpEncodingParameters[]): LayerDescription[] {
  const first = encodings[0] as RTCRtpEncodingParameters & { scalabilityMode?: string };
  const svc = first?.scalabilityMode?.match(SVC_SPATIAL_LAYERS);
  if (svc) {
    const spatial = Number(svc[1]);
    const out: LayerDescription[] = [];
    for (let i = 0; i < spatial; i++) {
      const scale = 2 ** (spatial - 1 - i);
      const quality = spatial === 1 ? VideoQuality.HIGH : ([VideoQuality.LOW, VideoQuality.MEDIUM, VideoQuality.HIGH][i] ?? VideoQuality.HIGH);
      out.push({
        quality,
        rid: '',
        width: Math.ceil(width / scale),
        height: Math.ceil(height / scale),
        bitrate: first.maxBitrate ? Math.ceil(first.maxBitrate / 3 ** (spatial - 1 - i)) : 0,
        spatialLayer: i,
      });
    }
    return out;
  }
  return encodings.map((e, i) => {
    const scale = e.scaleResolutionDownBy ?? 1;
    return {
      quality: qualityForRid(e.rid),
      rid: e.rid ?? '',
      width: Math.ceil(width / scale),
      height: Math.ceil(height / scale),
      bitrate: e.maxBitrate ?? 0,
      spatialLayer: i,
    };
  });
}

/**
 * The start bitrate worth asking the encoder for. Asking for too little does nothing useful, and too much
 * on a slow link costs more than a slower start would. Screen share goes higher because it is usually a
 * single sharp frame, which cannot be built up gradually.
 */
export function startBitrateKbps(encodings: RTCRtpEncodingParameters[], isScreenShare: boolean): number | undefined {
  const svc = (encodings[0] as { scalabilityMode?: string })?.scalabilityMode;
  const totalBps = svc
    ? (encodings[0].maxBitrate ?? 0)
    : encodings.reduce((sum, e) => sum + (e.maxBitrate ?? 0), 0);
  const kbps = (totalBps / 1000) * 0.9;
  if (kbps < 300) return undefined;
  return isScreenShare ? kbps : Math.min(kbps, 1000);
}

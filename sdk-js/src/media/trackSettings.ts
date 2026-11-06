import { VideoQuality } from '../enums.js';

export interface SubscriberSettingsInput {
  isVideo: boolean;
  /** Set when the application called setEnabled. It overrides what adaptive stream decided. */
  requestedDisabled?: boolean;
  /** Whether adaptive stream currently sees this track, when it is watching. */
  adaptiveVisible?: boolean;
  requestedQuality?: VideoQuality;
  requestedDimensions?: { width: number; height: number };
  adaptiveDimensions?: { width: number; height: number };
  fps?: number;
  /** Published layers, to compare a requested quality with the space the track is shown in. */
  layers?: Array<{ quality: VideoQuality; width: number; height: number }>;
}

export interface SubscriberSettings {
  disabled: boolean;
  quality?: VideoQuality;
  width?: number;
  height?: number;
  fps?: number;
}

/**
 * Turns everything a subscriber has asked for into the one settings message the server understands. Width
 * and height pick a layer by size, and quality picks it by name. They are alternatives, with size used
 * whenever it asks for less.
 */
export function computeSubscriberSettings(input: SubscriberSettingsInput): SubscriberSettings {
  const disabled = input.requestedDisabled ?? (input.isVideo && input.adaptiveVisible !== undefined ? !input.adaptiveVisible : false);
  if (!input.isVideo) return { disabled };

  const out: SubscriberSettings = { disabled };
  if (input.fps) out.fps = input.fps;
  const adaptive = input.adaptiveDimensions;

  if (input.requestedDimensions) {
    const requested = input.requestedDimensions;
    const useAdaptive = adaptive && adaptive.width * adaptive.height < requested.width * requested.height;
    const size = useAdaptive ? adaptive : requested;
    out.width = Math.ceil(size.width);
    out.height = Math.ceil(size.height);
    return out;
  }
  if (input.requestedQuality !== undefined) {
    const layer = input.layers?.find((l) => l.quality === input.requestedQuality);
    if (adaptive && layer && adaptive.width < layer.width && adaptive.height < layer.height) {
      out.width = Math.ceil(adaptive.width);
      out.height = Math.ceil(adaptive.height);
    } else {
      out.quality = input.requestedQuality;
    }
    return out;
  }
  if (adaptive) {
    out.width = Math.ceil(adaptive.width);
    out.height = Math.ceil(adaptive.height);
    return out;
  }
  out.quality = VideoQuality.HIGH;
  return out;
}

import { VideoQuality } from '../enums.js';
import { qualityForRid } from './encodings.js';

export interface QualitySetting {
  quality: VideoQuality;
  enabled: boolean;
}

/**
 * Applies a subscribed quality update to a sender's encodings, in place, and reports whether anything
 * changed. Qualities missing from the update are left alone.
 *
 * Without simulcast there is one encoding, and any subscribed quality is served by it. A server enabling
 * only LOW means this track is wanted, however little of it.
 */
export function applyQualitiesToEncodings(encodings: RTCRtpEncodingParameters[], qualities: QualitySetting[], isSvc: boolean): boolean {
  const anyEnabled = qualities.some((q) => q.enabled);
  const single = encodings.length === 1 && !encodings[0].rid;
  let changed = false;
  for (const encoding of encodings) {
    let enabled: boolean | undefined;
    if (isSvc || single) {
      enabled = anyEnabled;
    } else {
      enabled = qualities.find((q) => q.quality === qualityForRid(encoding.rid))?.enabled;
    }
    if (enabled === undefined) continue;
    if (encoding.active !== enabled) {
      encoding.active = enabled;
      changed = true;
    }
  }
  return changed;
}

/** Codec names in updates are short and lowercase, but a server sending a mime type means the same thing. */
export function normalizeCodecName(codec: string): string {
  return codec.toLowerCase().replace(/^video\//, '');
}

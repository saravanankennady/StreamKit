const SEPARATOR = '|';
const TRACK_PREFIX = 'TR';

/** Splits a stream ID into participant and the rest. Only the first separator counts. */
export function unpackStreamId(packed: string): [participantSid: string, rest: string] {
  const at = packed.indexOf(SEPARATOR);
  return at < 0 ? [packed, ''] : [packed.slice(0, at), packed.slice(at + 1)];
}

/**
 * Finds a media track's server ID. Servers put it in one of several places: a stream ID's second part
 * unless that part is a sync group like "camera", or the map sent with each offer. The browser's own track
 * ID comes last, because when a server reuses a transceiver that ID still names the first track on it.
 */
export function resolveTrackSid(streamPart: string, mediaTrackId: string, midMapValue: string | undefined): string | undefined {
  if (streamPart.startsWith(TRACK_PREFIX)) return streamPart;
  if (midMapValue) return midMapValue;
  if (mediaTrackId.startsWith(TRACK_PREFIX)) return mediaTrackId;
  return undefined;
}

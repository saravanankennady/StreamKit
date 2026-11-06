import { describe, expect, it } from 'vitest';
import { resolveTrackSid, unpackStreamId } from '../src/room/streamId.js';

describe('unpackStreamId', () => {
  it('splits on the first separator only', () => {
    expect(unpackStreamId('PA_x|TR_y')).toEqual(['PA_x', 'TR_y']);
    expect(unpackStreamId('PA_x|a|b')).toEqual(['PA_x', 'a|b']);
    expect(unpackStreamId('PA_x')).toEqual(['PA_x', '']);
  });
});

describe('resolveTrackSid', () => {
  it('prefers a track ID in the stream ID', () => {
    expect(resolveTrackSid('TR_stream', 'TR_track', 'TR_mid')).toBe('TR_stream');
  });
  it('with a sync group in the stream ID, prefers the per-offer map over the browser track ID', () => {
    // A reused transceiver keeps its first track ID, so the browser's value can name the wrong track.
    expect(resolveTrackSid('camera', 'TR_old', 'TR_current')).toBe('TR_current');
  });
  it('falls back to the browser track ID', () => {
    expect(resolveTrackSid('camera', 'TR_track', undefined)).toBe('TR_track');
    expect(resolveTrackSid('', 'TR_track', undefined)).toBe('TR_track');
  });
  it('finds nothing where nothing names a track', () => {
    expect(resolveTrackSid('camera', 'abcd', undefined)).toBeUndefined();
  });
});

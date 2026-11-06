import { describe, expect, it } from 'vitest';
import { VideoQuality } from '../src/enums.js';
import { applyQualitiesToEncodings, normalizeCodecName } from '../src/media/dynacast.js';

const three = () => [{ rid: 'q', active: true }, { rid: 'h', active: true }, { rid: 'f', active: true }] as RTCRtpEncodingParameters[];
const { LOW, MEDIUM, HIGH } = VideoQuality;

describe('applyQualitiesToEncodings', () => {
  it('turns off what nobody watches', () => {
    const e = three();
    const changed = applyQualitiesToEncodings(e, [{ quality: LOW, enabled: true }, { quality: MEDIUM, enabled: false }, { quality: HIGH, enabled: false }], false);
    expect(changed).toBe(true);
    expect(e.map((x) => x.active)).toEqual([true, false, false]);
  });
  it('reports no change when there is none, so the browser is not asked for nothing', () => {
    expect(applyQualitiesToEncodings(three(), [{ quality: LOW, enabled: true }], false)).toBe(false);
  });
  it('leaves alone a quality the update does not mention', () => {
    const e = three();
    applyQualitiesToEncodings(e, [{ quality: HIGH, enabled: false }], false);
    expect(e.map((x) => x.active)).toEqual([true, true, false]);
  });
  it('maps a two-layer track by rid: its top layer is MEDIUM, not HIGH', () => {
    const e = [{ rid: 'q', active: true }, { rid: 'h', active: true }] as RTCRtpEncodingParameters[];
    applyQualitiesToEncodings(e, [{ quality: LOW, enabled: true }, { quality: MEDIUM, enabled: false }, { quality: HIGH, enabled: true }], false);
    expect(e.map((x) => x.active)).toEqual([true, false]);
  });
  it('keeps a track without simulcast on when any quality is wanted', () => {
    // Its one encoding serves every quality. Reading it as HIGH or LOW alone would switch it off.
    const lowOnly = [{ active: true }] as RTCRtpEncodingParameters[];
    applyQualitiesToEncodings(lowOnly, [{ quality: LOW, enabled: true }, { quality: MEDIUM, enabled: false }, { quality: HIGH, enabled: false }], false);
    expect(lowOnly[0].active).toBe(true);
    const highOnly = [{ active: true }] as RTCRtpEncodingParameters[];
    applyQualitiesToEncodings(highOnly, [{ quality: LOW, enabled: false }, { quality: HIGH, enabled: true }], false);
    expect(highOnly[0].active).toBe(true);
  });
  it('turns off a track without simulcast when nothing is wanted', () => {
    const e = [{ active: true }] as RTCRtpEncodingParameters[];
    applyQualitiesToEncodings(e, [{ quality: LOW, enabled: false }, { quality: HIGH, enabled: false }], false);
    expect(e[0].active).toBe(false);
  });
  it('SVC is one stream: any quality keeps all of it', () => {
    const e = [{ active: false, scalabilityMode: 'L3T3' }] as unknown as RTCRtpEncodingParameters[];
    applyQualitiesToEncodings(e, [{ quality: LOW, enabled: true }, { quality: HIGH, enabled: false }], true);
    expect(e[0].active).toBe(true);
  });
});

describe('normalizeCodecName', () => {
  it('accepts a mime type as well as a short name', () => {
    expect(normalizeCodecName('vp8')).toBe('vp8');
    expect(normalizeCodecName('VP8')).toBe('vp8');
    expect(normalizeCodecName('video/VP8')).toBe('vp8');
  });
});

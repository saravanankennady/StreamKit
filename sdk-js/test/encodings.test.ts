import { describe, expect, it } from 'vitest';
import { VideoQuality } from '../src/enums.js';
import { computeVideoEncodings, describeLayers, encodingForSize, qualityForRid, startBitrateKbps } from '../src/media/encodings.js';

const summary = (e: RTCRtpEncodingParameters[]) => e.map((x) => [x.rid ?? '', x.scaleResolutionDownBy ?? 1, x.maxBitrate, x.maxFramerate]);

describe('computeVideoEncodings', () => {
  it('1280x720 camera: three layers', () => {
    expect(summary(computeVideoEncodings(false, 1280, 720))).toEqual([
      ['q', 4, 160000, 20],
      ['h', 2, 450000, 20],
      ['f', 1, 1700000, 30],
    ]);
  });

  it('640x480: two layers, from the 4:3 set, topping out at h', () => {
    const e = computeVideoEncodings(false, 640, 480);
    expect(e.map((x) => x.rid)).toEqual(['q', 'h']);
    expect(e[0].scaleResolutionDownBy).toBeCloseTo(480 / 180);
    expect(e[0].maxBitrate).toBe(125000);
    expect(e[1].scaleResolutionDownBy).toBe(1);
    expect(e[1].maxBitrate).toBe(500000);
  });

  it('480x270: two layers', () => {
    const e = computeVideoEncodings(false, 480, 270);
    expect(e.map((x) => x.rid)).toEqual(['q', 'h']);
    expect(e[0].scaleResolutionDownBy).toBeCloseTo(270 / 180);
    expect(e[1].maxBitrate).toBe(450000);
  });

  it('320x240: no room for layers, so no simulcast and no rid', () => {
    const e = computeVideoEncodings(false, 320, 240);
    expect(e).toHaveLength(1);
    expect(e[0].rid).toBeUndefined();
    expect(e[0].maxBitrate).toBe(140000);
  });

  it('1920x1080 screen share: two layers at half size and the full size', () => {
    expect(summary(computeVideoEncodings(true, 1920, 1080))).toEqual([
      ['q', 2, 625000, 15],
      ['h', 1, 2500000, 15],
    ]);
  });

  it('honours simulcast being turned off and an explicit encoding', () => {
    expect(computeVideoEncodings(false, 1280, 720, { simulcast: false })).toEqual([{ maxBitrate: 1700000, maxFramerate: 30 }]);
    const e = computeVideoEncodings(false, 1280, 720, { videoEncoding: { maxBitrate: 1000000, maxFramerate: 15 } });
    expect(e[2].maxBitrate).toBe(1000000);
    expect(e[2].maxFramerate).toBe(15);
  });

  it('uses fewer bits for codecs that need fewer', () => {
    expect(encodingForSize(1280, 720, false, 'vp9').maxBitrate).toBeCloseTo(1445000);
    expect(encodingForSize(1280, 720, false, 'av1').maxBitrate).toBeCloseTo(1190000);
    expect(encodingForSize(1280, 720, false, 'h265').maxBitrate).toBeCloseTo(1190000);
    expect(encodingForSize(1280, 720, false, 'h264').maxBitrate).toBe(1700000);
  });

  it('sends SVC codecs as one encoding with a scalability mode', () => {
    expect(computeVideoEncodings(false, 1280, 720, { videoCodec: 'vp9' })).toEqual([{ maxBitrate: 1445000, maxFramerate: 30, scalabilityMode: 'L3T3_KEY' }]);
    expect((computeVideoEncodings(true, 1920, 1080, { videoCodec: 'av1' })[0] as any).scalabilityMode).toBe('L1T3');
  });

  it('assumes 720p if the capture size is not known', () => {
    expect(computeVideoEncodings(false, undefined, undefined).map((e) => e.rid)).toEqual(['q', 'h', 'f']);
  });
});

describe('describeLayers', () => {
  it('fixes quality by rid and leaves SSRCs for the server to learn', () => {
    expect(describeLayers(1280, 720, computeVideoEncodings(false, 1280, 720))).toEqual([
      { quality: VideoQuality.LOW, rid: 'q', width: 320, height: 180, bitrate: 160000, spatialLayer: 0 },
      { quality: VideoQuality.MEDIUM, rid: 'h', width: 640, height: 360, bitrate: 450000, spatialLayer: 1 },
      { quality: VideoQuality.HIGH, rid: 'f', width: 1280, height: 720, bitrate: 1700000, spatialLayer: 2 },
    ]);
  });

  it('a track without rids reports its one layer as HIGH', () => {
    const [layer] = describeLayers(320, 240, computeVideoEncodings(false, 320, 240));
    expect(layer.quality).toBe(VideoQuality.HIGH);
    expect(layer.rid).toBe('');
  });

  it('describes the spatial layers of an SVC stream', () => {
    const layers = describeLayers(1280, 720, [{ maxBitrate: 1000000, scalabilityMode: 'L3T3_KEY' } as any]);
    expect(layers.map((l) => [l.quality, l.width, l.height])).toEqual([
      [VideoQuality.LOW, 320, 180],
      [VideoQuality.MEDIUM, 640, 360],
      [VideoQuality.HIGH, 1280, 720],
    ]);
  });
});

describe('qualityForRid', () => {
  it('is fixed, whatever the number of layers', () => {
    expect(qualityForRid('q')).toBe(VideoQuality.LOW);
    expect(qualityForRid('h')).toBe(VideoQuality.MEDIUM);
    expect(qualityForRid('f')).toBe(VideoQuality.HIGH);
    expect(qualityForRid('')).toBe(VideoQuality.HIGH);
    expect(qualityForRid(undefined)).toBe(VideoQuality.HIGH);
  });
});

describe('startBitrateKbps', () => {
  it('is 90% of the total, capped for cameras', () => {
    expect(startBitrateKbps(computeVideoEncodings(false, 1280, 720), false)).toBe(1000);
    expect(startBitrateKbps([{ maxBitrate: 500000 }], false)).toBe(450);
  });
  it('is not capped for screen share', () => {
    expect(startBitrateKbps(computeVideoEncodings(true, 1920, 1080), true)).toBeCloseTo(2812.5);
  });
  it('is skipped when too low to help', () => {
    expect(startBitrateKbps([{ maxBitrate: 200000 }], false)).toBeUndefined();
  });
  it('counts one bitrate for SVC, since its layers share it', () => {
    expect(startBitrateKbps([{ maxBitrate: 800000, scalabilityMode: 'L3T3_KEY' } as any], false)).toBe(720);
  });
});

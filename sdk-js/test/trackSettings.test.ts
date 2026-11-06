import { describe, expect, it } from 'vitest';
import { VideoQuality } from '../src/enums.js';
import { computeSubscriberSettings } from '../src/media/trackSettings.js';

const layers = [
  { quality: VideoQuality.LOW, width: 320, height: 180 },
  { quality: VideoQuality.MEDIUM, width: 640, height: 360 },
  { quality: VideoQuality.HIGH, width: 1280, height: 720 },
];

describe('computeSubscriberSettings', () => {
  it('with nothing requested, asks for the best', () => {
    expect(computeSubscriberSettings({ isVideo: true })).toEqual({ disabled: false, quality: VideoQuality.HIGH });
  });
  it('audio only has enabled and disabled', () => {
    expect(computeSubscriberSettings({ isVideo: false, requestedDisabled: true })).toEqual({ disabled: true });
    expect(computeSubscriberSettings({ isVideo: false })).toEqual({ disabled: false });
  });
  it('a requested quality goes by name', () => {
    expect(computeSubscriberSettings({ isVideo: true, requestedQuality: VideoQuality.LOW, layers })).toEqual({ disabled: false, quality: VideoQuality.LOW });
  });
  it('a requested quality larger than the space it is shown in goes by size', () => {
    expect(
      computeSubscriberSettings({ isVideo: true, requestedQuality: VideoQuality.HIGH, layers, adaptiveDimensions: { width: 300, height: 150 } }),
    ).toEqual({ disabled: false, width: 300, height: 150 });
  });
  it('a requested quality no larger than its space still goes by name', () => {
    expect(
      computeSubscriberSettings({ isVideo: true, requestedQuality: VideoQuality.LOW, layers, adaptiveDimensions: { width: 1000, height: 600 } }),
    ).toEqual({ disabled: false, quality: VideoQuality.LOW });
  });
  it('adaptive dimensions alone go by size, rounded up', () => {
    expect(computeSubscriberSettings({ isVideo: true, adaptiveDimensions: { width: 300.2, height: 150.5 } })).toEqual({ disabled: false, width: 301, height: 151 });
  });
  it('requested dimensions give way to a smaller adaptive size', () => {
    expect(
      computeSubscriberSettings({ isVideo: true, requestedDimensions: { width: 1280, height: 720 }, adaptiveDimensions: { width: 320, height: 180 } }),
    ).toEqual({ disabled: false, width: 320, height: 180 });
    expect(
      computeSubscriberSettings({ isVideo: true, requestedDimensions: { width: 320, height: 180 }, adaptiveDimensions: { width: 1280, height: 720 } }),
    ).toEqual({ disabled: false, width: 320, height: 180 });
  });
  it('an explicit enable overrides adaptive visibility, in both directions', () => {
    expect(computeSubscriberSettings({ isVideo: true, requestedDisabled: false, adaptiveVisible: false }).disabled).toBe(false);
    expect(computeSubscriberSettings({ isVideo: true, requestedDisabled: true, adaptiveVisible: true }).disabled).toBe(true);
  });
  it('without an explicit choice, follows adaptive visibility', () => {
    expect(computeSubscriberSettings({ isVideo: true, adaptiveVisible: false }).disabled).toBe(true);
    expect(computeSubscriberSettings({ isVideo: true, adaptiveVisible: true }).disabled).toBe(false);
  });
  it('carries a frame rate along with whatever else', () => {
    expect(computeSubscriberSettings({ isVideo: true, fps: 15 })).toEqual({ disabled: false, quality: VideoQuality.HIGH, fps: 15 });
  });
});

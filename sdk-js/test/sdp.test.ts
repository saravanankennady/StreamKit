import { describe, expect, it } from 'vitest';
import { addDependencyDescriptor, applyAudioFeaturesToAnswer, enableOpusStereo, iceUfrag, readOfferedAudioFeatures, setStartBitrate } from '../src/engine/sdp.js';

const OFFER = [
  'v=0',
  'o=- 1 2 IN IP4 127.0.0.1',
  's=-',
  't=0 0',
  'a=group:BUNDLE 0 1',
  'm=audio 9 UDP/TLS/RTP/SAVPF 111 63',
  'a=ice-ufrag:abcd',
  'a=mid:0',
  'a=msid:s1 audio-track',
  'a=rtpmap:111 opus/48000/2',
  'a=rtcp-fb:111 transport-cc',
  'a=fmtp:111 minptime=10;useinbandfec=1',
  'a=rtpmap:63 red/48000/2',
  'm=video 9 UDP/TLS/RTP/SAVPF 96 97 98',
  'a=mid:1',
  'a=msid:s2 video-track',
  'a=rtpmap:96 VP8/90000',
  'a=rtcp-fb:96 nack',
  'a=rtpmap:97 rtx/90000',
  'a=fmtp:97 apt=96',
  'a=rtpmap:98 H264/90000',
  'a=fmtp:98 profile-level-id=42e01f',
  '',
].join('\r\n');

describe('addDependencyDescriptor', () => {
  const uri = 'https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension';
  const withExtensions = (video: string[], audio: string[] = []) =>
    [
      'v=0',
      'a=group:BUNDLE 0 1',
      'm=audio 9 UDP/TLS/RTP/SAVPF 111',
      'a=mid:0',
      ...audio,
      'a=msid:s1 audio-track',
      'a=rtpmap:111 opus/48000/2',
      'm=video 9 UDP/TLS/RTP/SAVPF 96',
      'a=mid:1',
      ...video,
      'a=msid:s2 video-track',
      'a=rtpmap:96 VP9/90000',
      '',
    ].join('\r\n');

  it('adds it to the track\'s section only, above every ID in use', () => {
    const out = addDependencyDescriptor(withExtensions(['a=extmap:3 urn:x', 'a=extmap:9 urn:y'], ['a=extmap:1 urn:z']), 'video-track');
    expect(out).toContain(`a=extmap:9 urn:y\r\na=extmap:10 ${uri}`);
    expect(out.match(/dependency-descriptor/g)).toHaveLength(1);
  });

  it('does nothing if it is already there, or for another track', () => {
    const already = withExtensions([`a=extmap:4 ${uri}`]);
    expect(addDependencyDescriptor(already, 'video-track')).toBe(already);
    const other = withExtensions([]);
    expect(addDependencyDescriptor(other, 'missing-track')).toBe(other);
  });

  it('uses the ID the bundle has for it already', () => {
    const offer = withExtensions(['a=extmap:9 urn:y'], [`a=extmap:6 ${uri}`]);
    expect(addDependencyDescriptor(offer, 'video-track')).toContain(`a=extmap:9 urn:y\r\na=extmap:6 ${uri}`);
  });

  it('steps over the ID RFC 8285 reserves', () => {
    expect(addDependencyDescriptor(withExtensions(['a=extmap:14 urn:y']), 'video-track')).toContain(`a=extmap:16 ${uri}`);
  });

  it('finds a place with no extensions in the section', () => {
    expect(addDependencyDescriptor(withExtensions([]), 'video-track')).toContain(`a=mid:1\r\na=extmap:1 ${uri}`);
  });
});

describe('enableOpusStereo', () => {
  it('adds stereo to Opus only', () => {
    const out = enableOpusStereo(OFFER);
    expect(out).toContain('a=fmtp:111 minptime=10;useinbandfec=1;stereo=1');
    expect(out).toContain('a=fmtp:97 apt=96');
  });
  it('does not add it twice', () => {
    const out = enableOpusStereo(enableOpusStereo(OFFER));
    expect(out.match(/stereo=1/g)).toHaveLength(1);
  });
  it('adds a line for a codec that has none, after its feedback lines', () => {
    const sdp = 'v=0\r\nm=audio 9 RTP 111\r\na=rtpmap:111 opus/48000/2\r\na=rtcp-fb:111 nack\r\na=mid:0\r\n';
    const lines = enableOpusStereo(sdp).split('\r\n');
    expect(lines.indexOf('a=fmtp:111 stereo=1')).toBe(lines.indexOf('a=rtcp-fb:111 nack') + 1);
  });
  it('keeps line endings as CRLF', () => {
    expect(enableOpusStereo(OFFER).replace(/\r\n/g, '')).not.toContain('\n');
  });
});

describe('setStartBitrate', () => {
  it('applies to the track and codec named', () => {
    const out = setStartBitrate(OFFER, 'video-track', 'vp8', 1000);
    expect(out).toContain('a=fmtp:96 x-google-start-bitrate=1000');
    expect(out).not.toContain('a=fmtp:98 profile-level-id=42e01f;x-google');
  });
  it('joins an existing fmtp line', () => {
    expect(setStartBitrate(OFFER, 'video-track', 'h264', 500)).toContain('a=fmtp:98 profile-level-id=42e01f;x-google-start-bitrate=500');
  });
  it('rounds the rate, and leaves the description alone if the track is not there', () => {
    expect(setStartBitrate(OFFER, 'video-track', 'vp8', 2812.5)).toContain('x-google-start-bitrate=2813');
    expect(setStartBitrate(OFFER, 'no-such-track', 'vp8', 1000)).toBe(OFFER);
  });
});

describe('answers', () => {
  const offer = [
    'v=0',
    'm=audio 9 RTP 111',
    'a=mid:0',
    'a=rtpmap:111 opus/48000/2',
    'a=rtcp-fb:111 nack',
    'a=fmtp:111 sprop-stereo=1',
    'm=audio 9 RTP 111',
    'a=mid:1',
    'a=rtpmap:111 opus/48000/2',
    '',
  ].join('\r\n');
  const answer = ['v=0', 'm=audio 9 RTP 111', 'a=mid:0', 'a=rtpmap:111 opus/48000/2', 'm=audio 9 RTP 111', 'a=mid:1', 'a=rtpmap:111 opus/48000/2', ''].join('\r\n');

  it('repeat stereo and NACK only for the sections that offered them', () => {
    const features = readOfferedAudioFeatures(offer);
    expect([...features.stereoMids]).toEqual(['0']);
    expect([...features.nackMids]).toEqual(['0']);
    const out = applyAudioFeaturesToAnswer(answer, features);
    const [, first, second] = out.split('m=audio');
    expect(first).toContain('a=fmtp:111 stereo=1');
    expect(first).toContain('a=rtcp-fb:111 nack');
    expect(second).not.toContain('stereo=1');
    expect(second).not.toContain('nack');
  });
});

describe('iceUfrag', () => {
  it('reads the first username', () => {
    expect(iceUfrag(OFFER)).toBe('abcd');
    expect(iceUfrag(undefined)).toBeUndefined();
    expect(iceUfrag('v=0\r\n')).toBeUndefined();
  });
});

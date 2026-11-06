import { TrackSource } from '../enums.js';
import { TrackInvalidError } from '../errors.js';
import { LocalAudioTrack } from './LocalAudioTrack.js';
import type { LocalTrack } from './LocalTrack.js';
import { LocalVideoTrack } from './LocalVideoTrack.js';
import type { AudioCaptureOptions, CreateLocalTracksOptions, ScreenShareCaptureOptions, VideoCaptureOptions } from './options.js';
import { audioCaptureDefaults, videoCaptureDefaults } from './options.js';

function videoConstraints(options: VideoCaptureOptions): MediaTrackConstraints {
  const out: MediaTrackConstraints = {};
  if (options.deviceId !== undefined) out.deviceId = options.deviceId;
  if (options.facingMode) out.facingMode = options.facingMode;
  const r = options.resolution;
  if (r) {
    out.width = r.width;
    out.height = r.height;
    if (r.frameRate) out.frameRate = r.frameRate;
    if (r.aspectRatio) out.aspectRatio = r.aspectRatio;
  }
  return out;
}

function audioConstraints(options: AudioCaptureOptions): MediaTrackConstraints {
  const { stopMicTrackOnMute: _ignored, ...rest } = options;
  return rest;
}

/** Captures from the camera and microphone. */
export async function createLocalTracks(options: CreateLocalTracksOptions = { audio: true, video: true }): Promise<LocalTrack[]> {
  const audio = options.audio === true ? audioCaptureDefaults : options.audio || undefined;
  const video = options.video === true ? videoCaptureDefaults : options.video || undefined;
  if (!audio && !video) return [];
  const constraints: MediaStreamConstraints = {};
  if (audio) constraints.audio = audioConstraints(audio);
  if (video) constraints.video = videoConstraints(video);
  const stream = await navigator.mediaDevices.getUserMedia(constraints);
  return stream.getTracks().map((t) => {
    if (t.kind === 'audio') {
      const track = new LocalAudioTrack(t, constraints.audio as MediaTrackConstraints, false, !!audio?.stopMicTrackOnMute);
      track.source = TrackSource.Microphone;
      return track;
    }
    const track = new LocalVideoTrack(t, constraints.video as MediaTrackConstraints);
    track.source = TrackSource.Camera;
    return track;
  });
}

export async function createLocalVideoTrack(options: VideoCaptureOptions = {}): Promise<LocalVideoTrack> {
  const [track] = await createLocalTracks({ video: { ...videoCaptureDefaults, ...options } });
  return track as LocalVideoTrack;
}

export async function createLocalAudioTrack(options: AudioCaptureOptions = {}): Promise<LocalAudioTrack> {
  const [track] = await createLocalTracks({ audio: { ...audioCaptureDefaults, ...options } });
  return track as LocalAudioTrack;
}

/** Asks the browser to share a screen, window or tab, with its audio if that was asked for and is available. */
export async function createLocalScreenTracks(options: ScreenShareCaptureOptions = {}): Promise<LocalTrack[]> {
  const md = navigator.mediaDevices as MediaDevices;
  if (!md?.getDisplayMedia) throw new TrackInvalidError('this browser cannot share its screen');
  const video: MediaTrackConstraints & Record<string, unknown> = {};
  if (options.resolution) {
    video.width = options.resolution.width;
    video.height = options.resolution.height;
    if (options.resolution.frameRate) video.frameRate = options.resolution.frameRate;
  }
  if (typeof options.video === 'object') Object.assign(video, options.video);
  const stream = await md.getDisplayMedia({
    audio: options.audio ?? false,
    video: Object.keys(video).length > 0 ? video : true,
    ...(options.preferCurrentTab ? { preferCurrentTab: true } : {}),
  } as DisplayMediaStreamOptions);
  const [videoTrack] = stream.getVideoTracks();
  if (!videoTrack) throw new TrackInvalidError('no video from the screen share');
  if (options.contentHint) videoTrack.contentHint = options.contentHint;
  // A display capture cannot be restarted without a new prompt, so it counts as the application's own.
  const screen = new LocalVideoTrack(videoTrack, undefined, true);
  screen.source = TrackSource.ScreenShare;
  const out: LocalTrack[] = [screen];
  const [audioTrack] = stream.getAudioTracks();
  if (audioTrack) {
    const audio = new LocalAudioTrack(audioTrack, undefined, true);
    audio.source = TrackSource.ScreenShareAudio;
    out.push(audio);
  }
  return out;
}

export function mediaTrackToLocalTrack(track: MediaStreamTrack): LocalTrack {
  if (track.kind === 'audio') return new LocalAudioTrack(track, undefined, true);
  if (track.kind === 'video') return new LocalVideoTrack(track, undefined, true);
  throw new TrackInvalidError(`unsupported track kind ${track.kind}`);
}

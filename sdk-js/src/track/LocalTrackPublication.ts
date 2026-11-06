import { TrackEvent } from '../events.js';
import type { TrackInfo } from '../proto/livekit_models_pb.js';
import type { LocalTrack } from './LocalTrack.js';
import type { TrackPublishOptions } from './options.js';
import { TrackPublication } from './TrackPublication.js';

export class LocalTrackPublication extends TrackPublication {
  declare track?: LocalTrack;
  options?: TrackPublishOptions;

  constructor(track: LocalTrack, info: TrackInfo, options?: TrackPublishOptions) {
    super(track.kind, info.sid, info.name);
    this.options = options;
    this.updateInfo(info);
    this.setTrack(track);
    this._isMuted = track.isMuted;
    track.on(TrackEvent.Ended, this.onEnded);
  }

  private onEnded = (track: unknown) => {
    this.emit(TrackEvent.Ended, track as LocalTrack);
  };

  /** @internal */
  detachFromTrack() {
    this.track?.off(TrackEvent.Ended, this.onEnded);
    this.setTrack(undefined);
  }

  override get isLocal() {
    return true;
  }

  override get isSubscribed() {
    return false;
  }

  get isUpstreamPaused() {
    return this.track?.isUpstreamPaused ?? false;
  }

  mute(): Promise<void> {
    return this.track?.mute() ?? Promise.resolve();
  }

  unmute(): Promise<void> {
    return this.track?.unmute() ?? Promise.resolve();
  }

  pauseUpstream(): Promise<void> {
    return this.track?.pauseUpstream() ?? Promise.resolve();
  }

  resumeUpstream(): Promise<void> {
    return this.track?.resumeUpstream() ?? Promise.resolve();
  }
}

import { PermissionStatus, SubscriptionStatus, TrackKind, TrackSource } from '../enums.js';
import { TrackEvent } from '../events.js';
import { type TrackInfo, VideoLayer_Mode } from '../proto/livekit_models_pb.js';
import { TypedEmitter } from '../util/emitter.js';
import { sourceFromProto } from './protoConvert.js';
import type { Track } from './Track.js';

export type PublicationEvents = {
  muted: () => void;
  unmuted: () => void;
  ended: (track: Track) => void;
  subscribed: (track: Track) => void;
  unsubscribed: (track: Track) => void;
  updateSettings: (publication: TrackPublication) => void;
  updateSubscription: (subscribed: boolean) => void;
  subscriptionStatusChanged: (status: SubscriptionStatus, previous: SubscriptionStatus) => void;
  subscriptionPermissionChanged: (status: PermissionStatus, previous: PermissionStatus) => void;
  streamStateChanged: (state: string) => void;
};

/** The record of a track being in a room. It exists whether or not this client receives the track. */
export class TrackPublication extends TypedEmitter<PublicationEvents> {
  static readonly SubscriptionStatus = SubscriptionStatus;
  static readonly PermissionStatus = PermissionStatus;

  kind: TrackKind;
  trackName: string;
  trackSid: string;
  track?: Track;
  source: TrackSource = TrackSource.Unknown;
  mimeType?: string;
  dimensions?: { width: number; height: number };
  simulcasted?: boolean;
  /** @internal */
  trackInfo?: TrackInfo;

  protected _isMuted = false;

  constructor(kind: TrackKind, trackSid: string, trackName: string) {
    super();
    this.kind = kind;
    this.trackSid = trackSid;
    this.trackName = trackName;
  }

  get isMuted() {
    return this._isMuted;
  }

  get isEnabled() {
    return true;
  }

  get isSubscribed() {
    return this.track !== undefined;
  }

  get isLocal() {
    return false;
  }

  get audioTrack() {
    return this.kind === TrackKind.Audio ? this.track : undefined;
  }

  get videoTrack() {
    return this.kind === TrackKind.Video ? this.track : undefined;
  }

  /** @internal */
  setTrack(track?: Track) {
    this.track?.off(TrackEvent.Muted, this.onTrackMuted);
    this.track?.off(TrackEvent.Unmuted, this.onTrackUnmuted);
    this.track = track;
    if (track) {
      track.sid = this.trackSid;
      track.on(TrackEvent.Muted, this.onTrackMuted);
      track.on(TrackEvent.Unmuted, this.onTrackUnmuted);
    }
  }

  private onTrackMuted = () => {
    this._isMuted = true;
    this.emit(TrackEvent.Muted);
  };

  private onTrackUnmuted = () => {
    this._isMuted = false;
    this.emit(TrackEvent.Unmuted);
  };

  /** @internal */
  updateInfo(info: TrackInfo) {
    this.trackInfo = info;
    this.trackSid = info.sid;
    this.trackName = info.name;
    this.source = sourceFromProto(info.source);
    this.mimeType = info.mimeType;
    this.simulcasted = info.simulcast || info.codecs.some((c) => c.videoLayerMode === VideoLayer_Mode.ONE_SPATIAL_LAYER_PER_STREAM && c.layers.length > 1);
    if (this.kind === TrackKind.Video && info.width > 0) {
      this.dimensions = { width: info.width, height: info.height };
    }
  }
}

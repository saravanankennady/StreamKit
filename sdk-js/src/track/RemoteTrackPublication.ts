import { PermissionStatus, StreamState, SubscriptionStatus, TrackKind, VideoQuality } from '../enums.js';
import { TrackEvent } from '../events.js';
import { computeSubscriberSettings, type SubscriberSettings } from '../media/trackSettings.js';
import { qualityForRid } from '../media/encodings.js';
import type { TrackInfo } from '../proto/livekit_models_pb.js';
import type { RemoteTrack } from './RemoteTrack.js';
import { RemoteVideoTrack } from './RemoteVideoTrack.js';
import { TrackPublication } from './TrackPublication.js';

export class RemoteTrackPublication extends TrackPublication {
  declare track?: RemoteTrack;

  /** Undefined until the application or the room's default has an opinion. */
  private subscribed?: boolean;
  private allowed = true;
  private requestedDisabled?: boolean;
  private adaptiveVisible?: boolean;
  private requestedQuality?: VideoQuality;
  private requestedDimensions?: { width: number; height: number };
  private adaptiveDimensions?: { width: number; height: number };
  private fps?: number;

  constructor(kind: TrackKind, info: TrackInfo, autoSubscribe: boolean) {
    super(kind, info.sid, info.name);
    this.subscribed = autoSubscribe;
    this.updateInfo(info);
  }

  override get isSubscribed() {
    if (this.subscribed === false) return false;
    return super.isSubscribed;
  }

  override get isEnabled() {
    if (this.requestedDisabled !== undefined) return !this.requestedDisabled;
    return this.adaptiveVisible ?? true;
  }

  get isDesired() {
    return this.subscribed !== false;
  }

  get subscriptionStatus(): SubscriptionStatus {
    if (this.subscribed === false) return SubscriptionStatus.Unsubscribed;
    return super.isSubscribed ? SubscriptionStatus.Subscribed : SubscriptionStatus.Desired;
  }

  get permissionStatus(): PermissionStatus {
    return this.allowed ? PermissionStatus.Allowed : PermissionStatus.NotAllowed;
  }

  get isAllowed() {
    return this.allowed;
  }

  get videoQuality() {
    return this.requestedQuality;
  }

  /** Asks the server to start or stop sending this track. */
  setSubscribed(subscribed: boolean) {
    const previous = this.subscriptionStatus;
    const previousPermission = this.permissionStatus;
    this.subscribed = subscribed;
    // The server will say so again if the publisher has not allowed this.
    if (subscribed) this.allowed = true;
    this.emit(TrackEvent.UpdateSubscription, subscribed);
    this.emitStatusIfChanged(previous);
    this.emitPermissionIfChanged(previousPermission);
  }

  /** Pauses delivery without giving up the subscription. Resuming is quicker than subscribing again. */
  setEnabled(enabled: boolean) {
    if (this.requestedDisabled === !enabled) return;
    this.requestedDisabled = !enabled;
    this.emitSettings();
  }

  /** Asks for a given simulcast layer. It holds until the application asks for another. */
  setVideoQuality(quality: VideoQuality) {
    if (this.requestedQuality === quality) return;
    this.requestedQuality = quality;
    this.requestedDimensions = undefined;
    this.emitSettings();
  }

  setVideoDimensions(dimensions: { width: number; height: number }) {
    if (this.requestedDimensions?.width === dimensions.width && this.requestedDimensions?.height === dimensions.height) return;
    this.requestedDimensions = dimensions;
    this.requestedQuality = undefined;
    this.emitSettings();
  }

  setVideoFPS(fps: number) {
    if (this.fps === fps) return;
    this.fps = fps;
    this.emitSettings();
  }

  /** @internal */
  override setTrack(track?: RemoteTrack) {
    const previous = this.subscriptionStatus;
    const previousPermission = this.permissionStatus;
    const old = this.track;
    if (old === track) return;
    if (old) {
      old.off(TrackEvent.VisibilityChanged, this.onVisibilityChanged);
      old.off(TrackEvent.VideoDimensionsChanged, this.onDimensionsChanged);
      old.off(TrackEvent.StreamStateChanged, this.onStreamState);
      old.off(TrackEvent.Ended, this.onEnded);
    }
    super.setTrack(track);
    if (track) {
      track.on(TrackEvent.VisibilityChanged, this.onVisibilityChanged);
      track.on(TrackEvent.VideoDimensionsChanged, this.onDimensionsChanged);
      track.on(TrackEvent.StreamStateChanged, this.onStreamState);
      track.on(TrackEvent.Ended, this.onEnded);
      // Having the media proves the publisher allows it, whatever was last heard.
      this.allowed = true;
      // The server forgets what was asked for once a subscription ends, or ignored it before there was one.
      if (this.hasRequestedSettings) this.emitSettings();
      this.emit(TrackEvent.Subscribed, track);
    } else if (old) {
      old.stop();
      this.adaptiveVisible = undefined;
      this.adaptiveDimensions = undefined;
      this.emit(TrackEvent.Unsubscribed, old);
    }
    this.emitStatusIfChanged(previous);
    this.emitPermissionIfChanged(previousPermission);
  }

  private get hasRequestedSettings() {
    return this.requestedDisabled !== undefined || this.requestedQuality !== undefined || this.requestedDimensions !== undefined || this.fps !== undefined;
  }

  private onEnded = (track: unknown) => {
    this.emit(TrackEvent.Ended, track as RemoteTrack);
  };

  private onStreamState = (state: StreamState) => {
    this.emit(TrackEvent.StreamStateChanged, state);
  };

  private onVisibilityChanged = (visible: boolean) => {
    this.adaptiveVisible = visible;
    this.emitSettings();
  };

  private onDimensionsChanged = (dimensions: { width: number; height: number }) => {
    this.adaptiveDimensions = dimensions;
    this.emitSettings();
  };

  /** @internal */
  setAllowed(allowed: boolean) {
    const previous = this.subscriptionStatus;
    const previousPermission = this.permissionStatus;
    this.allowed = allowed;
    this.emitPermissionIfChanged(previousPermission);
    this.emitStatusIfChanged(previous);
  }

  /** @internal */
  setMutedFromServer(muted: boolean) {
    if (this._isMuted === muted) return;
    this._isMuted = muted;
    this.track?.setMuted(muted);
    if (!this.track) this.emit(muted ? TrackEvent.Muted : TrackEvent.Unmuted);
  }

  /** @internal */
  override updateInfo(info: TrackInfo) {
    super.updateInfo(info);
    this.setMutedFromServer(info.muted);
  }

  private emitStatusIfChanged(previous: SubscriptionStatus) {
    const current = this.subscriptionStatus;
    if (current !== previous) this.emit(TrackEvent.SubscriptionStatusChanged, current, previous);
  }

  private emitPermissionIfChanged(previous: PermissionStatus) {
    const current = this.permissionStatus;
    if (current !== previous) this.emit(TrackEvent.SubscriptionPermissionChanged, current, previous);
  }

  private emitSettings() {
    this.emit(TrackEvent.UpdateSettings, this);
  }

  /** What to tell the server this subscriber wants. @internal */
  currentSettings(): SubscriberSettings {
    const layers = (this.trackInfo?.layers ?? []).map((l) => ({ quality: (l.rid ? qualityForRid(l.rid) : (l.quality as number)) as VideoQuality, width: l.width, height: l.height }));
    return computeSubscriberSettings({
      isVideo: this.kind === TrackKind.Video,
      requestedDisabled: this.requestedDisabled,
      adaptiveVisible: this.track instanceof RemoteVideoTrack && this.track.isAdaptiveStream ? this.adaptiveVisible : undefined,
      requestedQuality: this.requestedQuality,
      requestedDimensions: this.requestedDimensions,
      adaptiveDimensions: this.adaptiveDimensions,
      fps: this.fps,
      layers,
    });
  }

  /** Whether to tell a server about this publication after a resume. @internal */
  get hasSettingsToSend() {
    return this.kind === TrackKind.Video && this.isSubscribed;
  }
}

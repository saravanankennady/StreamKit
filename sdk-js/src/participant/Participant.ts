import { ConnectionQuality, ParticipantKind, TrackKind, TrackSource } from '../enums.js';
import { ParticipantEvent } from '../events.js';
import { type ParticipantInfo, ParticipantInfo_State, type ParticipantPermission } from '../proto/livekit_models_pb.js';
import type { LocalTrackPublication } from '../track/LocalTrackPublication.js';
import type { RemoteTrack } from '../track/RemoteTrack.js';
import type { RemoteTrackPublication } from '../track/RemoteTrackPublication.js';
import type { TrackPublication } from '../track/TrackPublication.js';
import { TypedEmitter } from '../util/emitter.js';

export type ParticipantEventCallbacks = {
  trackPublished: (publication: RemoteTrackPublication) => void;
  trackSubscribed: (track: RemoteTrack, publication: RemoteTrackPublication) => void;
  trackSubscriptionFailed: (trackSid: string, reason?: number) => void;
  trackUnpublished: (publication: RemoteTrackPublication) => void;
  trackUnsubscribed: (track: RemoteTrack, publication: RemoteTrackPublication) => void;
  trackMuted: (publication: TrackPublication) => void;
  trackUnmuted: (publication: TrackPublication) => void;
  localTrackPublished: (publication: LocalTrackPublication) => void;
  localTrackUnpublished: (publication: LocalTrackPublication) => void;
  participantMetadataChanged: (previous: string | undefined) => void;
  participantNameChanged: (name: string) => void;
  dataReceived: (payload: Uint8Array, kind: number) => void;
  isSpeakingChanged: (speaking: boolean) => void;
  connectionQualityChanged: (quality: ConnectionQuality) => void;
  trackStreamStateChanged: (publication: RemoteTrackPublication, state: string) => void;
  trackSubscriptionPermissionChanged: (publication: RemoteTrackPublication, status: string) => void;
  trackSubscriptionStatusChanged: (publication: RemoteTrackPublication, status: string) => void;
  mediaDevicesError: (error: Error, kind?: MediaDeviceKind) => void;
  participantPermissionsChanged: (previous?: ParticipantPermission) => void;
  attributesChanged: (changed: Record<string, string>) => void;
  localTrackSubscribed: (publication: LocalTrackPublication) => void;
  active: () => void;
};

/** One person or service in a room. */
export class Participant extends TypedEmitter<ParticipantEventCallbacks> {
  sid: string;
  identity: string;
  name?: string;
  metadata?: string;
  attributes: Record<string, string> = {};
  audioLevel = 0;
  isSpeaking = false;
  lastSpokeAt?: Date;
  permissions?: ParticipantPermission;
  kind: ParticipantKind = ParticipantKind.STANDARD;
  joinedAt?: Date;

  trackPublications = new Map<string, TrackPublication>();
  audioTrackPublications = new Map<string, TrackPublication>();
  videoTrackPublications = new Map<string, TrackPublication>();

  /** @internal */
  participantInfo?: ParticipantInfo;

  private _connectionQuality = ConnectionQuality.Unknown;
  private version?: number;

  constructor(sid: string, identity: string, name?: string, metadata?: string) {
    super();
    this.sid = sid;
    this.identity = identity;
    this.name = name;
    this.metadata = metadata;
  }

  get isLocal() {
    return false;
  }

  get isAgent() {
    return this.kind === ParticipantKind.AGENT;
  }

  /** Whether this participant's media connection is up. A participant can be in a room before that. */
  get isActive() {
    return this.participantInfo?.state === ParticipantInfo_State.ACTIVE;
  }

  get connectionQuality() {
    return this._connectionQuality;
  }

  get isCameraEnabled() {
    return this.isSourceOn(TrackSource.Camera);
  }

  get isMicrophoneEnabled() {
    return this.isSourceOn(TrackSource.Microphone);
  }

  get isScreenShareEnabled() {
    return this.getTrackPublication(TrackSource.ScreenShare) !== undefined;
  }

  private isSourceOn(source: TrackSource) {
    const pub = this.getTrackPublication(source);
    return pub !== undefined && !pub.isMuted;
  }

  getTrackPublications(): TrackPublication[] {
    return [...this.trackPublications.values()];
  }

  getTrackPublication(source: TrackSource): TrackPublication | undefined {
    for (const pub of this.trackPublications.values()) {
      if (pub.source === source) return pub;
    }
    return undefined;
  }

  getTrackPublicationByName(name: string): TrackPublication | undefined {
    for (const pub of this.trackPublications.values()) {
      if (pub.trackName === name) return pub;
    }
    return undefined;
  }

  /** @internal */
  addPublication(pub: TrackPublication) {
    this.trackPublications.set(pub.trackSid, pub);
    (pub.kind === TrackKind.Audio ? this.audioTrackPublications : this.videoTrackPublications).set(pub.trackSid, pub);
  }

  /** @internal */
  removePublication(sid: string) {
    this.trackPublications.delete(sid);
    this.audioTrackPublications.delete(sid);
    this.videoTrackPublications.delete(sid);
  }

  /**
   * Applies new information from the server. Updates can arrive out of order around a reconnect, so one
   * older than what is held for this session is dropped, and it reports false. An update for a newer session
   * under this identity starts counting again.
   * @internal
   */
  updateInfo(info: ParticipantInfo): boolean {
    if (this.sid === info.sid && this.version !== undefined && info.version < this.version) return false;
    // A session replaced by a newer one can still have an update on the way, and it no longer counts.
    const known = this.participantInfo?.joinedAtMs;
    if (this.sid !== info.sid && known && info.joinedAtMs && info.joinedAtMs < known) return false;
    const wasActive = this.isActive;
    this.version = info.version;
    this.sid = info.sid;
    if (info.identity) this.identity = info.identity;
    this.kind = info.kind as number as ParticipantKind;
    this.participantInfo = info;
    this.applyName(info.name);
    this.applyMetadata(info.metadata);
    this.applyAttributes(info.attributes);
    if (info.permission) this.applyPermissions(info.permission);
    if (info.joinedAtMs) this.joinedAt = new Date(Number(info.joinedAtMs));
    else if (info.joinedAt) this.joinedAt = new Date(Number(info.joinedAt) * 1000);
    if (this.isActive && !wasActive) this.emit(ParticipantEvent.Active);
    return true;
  }

  protected applyName(name: string) {
    if (this.name === name) return;
    this.name = name;
    this.emit(ParticipantEvent.ParticipantNameChanged, name);
  }

  protected applyMetadata(metadata: string) {
    const previous = this.metadata;
    if (previous === metadata) return;
    // A first value is not a change anyone has to hear about.
    this.metadata = metadata;
    if (previous !== undefined || metadata !== '') this.emit(ParticipantEvent.ParticipantMetadataChanged, previous);
  }

  protected applyAttributes(attributes: Record<string, string>) {
    const changed: Record<string, string> = {};
    for (const [k, v] of Object.entries(attributes)) {
      if (this.attributes[k] !== v) changed[k] = v;
    }
    for (const k of Object.keys(this.attributes)) {
      if (!(k in attributes)) changed[k] = '';
    }
    if (Object.keys(changed).length === 0) return;
    this.attributes = { ...attributes };
    this.emit(ParticipantEvent.AttributesChanged, changed);
  }

  protected applyPermissions(permissions: ParticipantPermission): boolean {
    const previous = this.permissions;
    const changed =
      !previous ||
      previous.canPublish !== permissions.canPublish ||
      previous.canSubscribe !== permissions.canSubscribe ||
      previous.canPublishData !== permissions.canPublishData ||
      previous.hidden !== permissions.hidden ||
      previous.canUpdateMetadata !== permissions.canUpdateMetadata ||
      previous.canPublishSources.join() !== permissions.canPublishSources.join();
    this.permissions = permissions;
    if (changed) this.emit(ParticipantEvent.ParticipantPermissionsChanged, previous);
    return changed;
  }

  /** @internal */
  setIsSpeaking(speaking: boolean) {
    if (speaking === this.isSpeaking) return;
    this.isSpeaking = speaking;
    if (speaking) this.lastSpokeAt = new Date();
    this.emit(ParticipantEvent.IsSpeakingChanged, speaking);
  }

  /** @internal */
  setConnectionQuality(quality: ConnectionQuality) {
    if (quality === this._connectionQuality) return;
    this._connectionQuality = quality;
    this.emit(ParticipantEvent.ConnectionQualityChanged, quality);
  }
}

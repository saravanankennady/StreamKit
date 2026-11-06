import { create } from '@bufbuild/protobuf';
import { TrackKind, TrackSource } from '../enums.js';
import { ParticipantEvent, TrackEvent } from '../events.js';
import { log } from '../logger.js';
import { ParticipantTracksSchema, VideoQuality as ProtoQuality, type ParticipantInfo } from '../proto/livekit_models_pb.js';
import { UpdateSubscriptionSchema, UpdateTrackSettingsSchema } from '../proto/livekit_rtc_pb.js';
import type { SignalClient } from '../signal/SignalClient.js';
import { kindFromProto } from '../track/protoConvert.js';
import { RemoteAudioTrack } from '../track/RemoteAudioTrack.js';
import type { RemoteTrack } from '../track/RemoteTrack.js';
import { RemoteTrackPublication } from '../track/RemoteTrackPublication.js';
import { type AdaptiveStreamSettings, RemoteVideoTrack } from '../track/RemoteVideoTrack.js';
import { Participant } from './Participant.js';

/** Someone else in the room. */
export class RemoteParticipant extends Participant {
  declare audioTrackPublications: Map<string, RemoteTrackPublication>;
  declare videoTrackPublications: Map<string, RemoteTrackPublication>;

  private volumes = new Map<TrackSource, number>();
  private sinkId?: string;

  constructor(
    private readonly signal: () => SignalClient | undefined,
    private readonly autoSubscribe: () => boolean,
    sid: string,
    identity: string,
  ) {
    super(sid, identity);
  }

  override getTrackPublication(source: TrackSource): RemoteTrackPublication | undefined {
    return super.getTrackPublication(source) as RemoteTrackPublication | undefined;
  }

  override getTrackPublications(): RemoteTrackPublication[] {
    return super.getTrackPublications() as RemoteTrackPublication[];
  }

  setVolume(volume: number, source: TrackSource.Microphone | TrackSource.ScreenShareAudio = TrackSource.Microphone) {
    this.volumes.set(source, volume);
    const track = this.getTrackPublication(source)?.track;
    if (track instanceof RemoteAudioTrack) track.setVolume(volume);
  }

  getVolume(source: TrackSource.Microphone | TrackSource.ScreenShareAudio = TrackSource.Microphone): number | undefined {
    const track = this.getTrackPublication(source)?.track;
    return track instanceof RemoteAudioTrack ? track.getVolume() : this.volumes.get(source);
  }

  /** @internal */
  setAudioOutput(deviceId: string) {
    this.sinkId = deviceId;
    return Promise.all(
      [...this.audioTrackPublications.values()].map((pub) => (pub.track instanceof RemoteAudioTrack ? pub.track.setSinkId(deviceId) : undefined)),
    );
  }

  /**
   * The list of tracks in an update is complete, so a publication missing from it has been unpublished.
   * Tracks are not added to the record of who has what without an event, and that event comes after the
   * state has settled, so listeners see a consistent participant.
   * @internal
   */
  override updateInfo(info: ParticipantInfo): boolean {
    if (!super.updateInfo(info)) return false;
    const valid = new Set<string>();
    const added: RemoteTrackPublication[] = [];
    for (const ti of info.tracks) {
      valid.add(ti.sid);
      const existing = this.trackPublications.get(ti.sid) as RemoteTrackPublication | undefined;
      if (existing) {
        existing.updateInfo(ti);
        continue;
      }
      const kind = kindFromProto(ti.type);
      if (!kind) {
        log.debug('ignoring published track of unknown type', ti.sid);
        continue;
      }
      const pub = new RemoteTrackPublication(kind, ti, this.autoSubscribe());
      this.wirePublication(pub);
      this.addPublication(pub);
      added.push(pub);
    }
    for (const pub of this.getTrackPublications()) {
      if (!valid.has(pub.trackSid)) this.unpublishTrack(pub.trackSid);
    }
    for (const pub of added) this.emit(ParticipantEvent.TrackPublished, pub);
    return true;
  }

  private wirePublication(pub: RemoteTrackPublication) {
    pub.on(TrackEvent.Muted, () => this.emit(ParticipantEvent.TrackMuted, pub));
    pub.on(TrackEvent.Unmuted, () => this.emit(ParticipantEvent.TrackUnmuted, pub));
    pub.on(TrackEvent.Subscribed, (track) => this.emit(ParticipantEvent.TrackSubscribed, track as RemoteTrack, pub));
    pub.on(TrackEvent.Unsubscribed, (track) => this.emit(ParticipantEvent.TrackUnsubscribed, track as RemoteTrack, pub));
    pub.on(TrackEvent.Ended, () => pub.setTrack(undefined));
    pub.on(TrackEvent.StreamStateChanged, (state) => this.emit(ParticipantEvent.TrackStreamStateChanged, pub, state));
    pub.on(TrackEvent.SubscriptionStatusChanged, (status) => this.emit(ParticipantEvent.TrackSubscriptionStatusChanged, pub, status));
    pub.on(TrackEvent.SubscriptionPermissionChanged, (status) => this.emit(ParticipantEvent.TrackSubscriptionPermissionChanged, pub, status));
    pub.on(TrackEvent.UpdateSubscription, (subscribed) => {
      this.signal()?.sendUpdateSubscription(
        create(UpdateSubscriptionSchema, {
          trackSids: [pub.trackSid],
          subscribe: subscribed,
          participantTracks: [create(ParticipantTracksSchema, { participantSid: this.sid, trackSids: [pub.trackSid] })],
        }),
      );
    });
    pub.on(TrackEvent.UpdateSettings, () => this.sendTrackSettings(pub));
  }

  /** @internal */
  sendTrackSettings(pub: RemoteTrackPublication) {
    const s = pub.currentSettings();
    this.signal()?.sendUpdateTrackSettings(
      create(UpdateTrackSettingsSchema, {
        trackSids: [pub.trackSid],
        disabled: s.disabled,
        quality: (s.quality ?? ProtoQuality.LOW) as number as ProtoQuality,
        width: s.width ?? 0,
        height: s.height ?? 0,
        fps: s.fps ?? 0,
      }),
    );
  }

  /**
   * Delivers received media to its publication. Reports false if the publication does not exist yet, so
   * the room can try again when more is known.
   * @internal
   */
  addSubscribedMediaTrack(
    mediaTrack: MediaStreamTrack,
    trackSid: string,
    stream: MediaStream,
    receiver: RTCRtpReceiver,
    adaptive: AdaptiveStreamSettings | undefined,
  ): boolean {
    const pub = this.trackPublications.get(trackSid) as RemoteTrackPublication | undefined;
    if (!pub) return false;
    if (pub.track?.mediaStreamTrack === mediaTrack) return true;
    const track: RemoteTrack =
      pub.kind === TrackKind.Video
        ? new RemoteVideoTrack(mediaTrack, trackSid, receiver, stream, adaptive)
        : new RemoteAudioTrack(mediaTrack, trackSid, receiver, stream, this.sinkId);
    track.source = pub.source;
    track.isMuted = pub.isMuted;
    const volume = this.volumes.get(pub.source);
    if (track instanceof RemoteAudioTrack && volume !== undefined) track.setVolume(volume);
    if (pub.track) pub.setTrack(undefined);
    pub.setTrack(track);
    return true;
  }

  /** @internal */
  unpublishTrack(sid: string) {
    const pub = this.trackPublications.get(sid) as RemoteTrackPublication | undefined;
    if (!pub) return;
    this.removePublication(sid);
    pub.setTrack(undefined);
    this.emit(ParticipantEvent.TrackUnpublished, pub);
  }
}

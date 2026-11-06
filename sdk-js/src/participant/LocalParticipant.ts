import { create } from '@bufbuild/protobuf';
import { DataPacketKind, TrackKind, TrackSource } from '../enums.js';
import { NegotiationError, PublishDataError, PublishTrackError, SignalRequestError, TrackInvalidError } from '../errors.js';
import { ParticipantEvent, TrackEvent } from '../events.js';
import type { RTCEngine } from '../engine/RTCEngine.js';
import { addDependencyDescriptor, setStartBitrate, enableOpusStereo } from '../engine/sdp.js';
import { isSafari } from '../util/browser.js';
import { log } from '../logger.js';
import { computeVideoEncodings, describeLayers, isSvcCodec, startBitrateKbps } from '../media/encodings.js';
import {
  AudioTrackFeature,
  type DataPacket,
  DataPacketSchema,
  DataPacket_Kind,
  type ParticipantInfo,
  type TrackInfo,
  VideoLayerSchema,
  VideoLayer_Mode,
  VideoQuality as ProtoQuality,
  UserPacketSchema,
} from '../proto/livekit_models_pb.js';
import {
  type AddTrackRequest,
  AddTrackRequestSchema,
  RequestResponse_Reason,
  SimulcastCodecSchema,
  SubscriptionPermissionSchema,
  type TrackPermission,
  TrackPermissionSchema,
  UpdateParticipantMetadataSchema,
} from '../proto/livekit_rtc_pb.js';
import { createLocalScreenTracks, createLocalTracks, mediaTrackToLocalTrack } from '../track/create.js';
import type { LocalTrack } from '../track/LocalTrack.js';
import { LocalTrackPublication } from '../track/LocalTrackPublication.js';
import { LocalVideoTrack } from '../track/LocalVideoTrack.js';
import type {
  AudioCaptureOptions,
  CreateLocalTracksOptions,
  ScreenShareCaptureOptions,
  TrackPublishOptions,
  VideoCaptureOptions,
  VideoCodec,
} from '../track/options.js';
import { publishDefaults } from '../track/options.js';
import { kindToProto, sourceToProto } from '../track/protoConvert.js';
import { Participant } from './Participant.js';

const SIGNAL_READY_TIMEOUT_MS = 15_000;
const METADATA_TIMEOUT_MS = 5000;

export interface DataPublishOptions {
  /** Retransmit until delivered, and in order. Without this, late data is dropped. Defaults to true. */
  reliable?: boolean;
  /** Who should receive it. Everyone in the room if unset. */
  destinationIdentities?: string[];
  topic?: string;
}

export interface ParticipantTrackPermission {
  participantIdentity?: string;
  participantSid?: string;
  allowAll?: boolean;
  allowedTrackSids?: string[];
}

interface LocalParticipantOptions {
  publishDefaults: TrackPublishOptions;
  audioCaptureDefaults: AudioCaptureOptions;
  videoCaptureDefaults: VideoCaptureOptions;
  stopLocalTrackOnUnpublish: boolean;
  dynacast: boolean;
}

interface PendingMetadata {
  resolve: () => void;
  reject: (e: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
  check: () => boolean;
}

/** This client, in the room. */
export class LocalParticipant extends Participant {
  declare audioTrackPublications: Map<string, LocalTrackPublication>;
  declare videoTrackPublications: Map<string, LocalTrackPublication>;

  lastCameraError?: Error;
  lastMicrophoneError?: Error;

  private engine?: RTCEngine;
  private pendingPublishing = new Map<TrackSource, Promise<LocalTrackPublication | undefined>>();
  private allParticipantsAllowed = true;
  private trackPermissions: ParticipantTrackPermission[] = [];
  private enabledVideoCodecs: string[] = [];
  private nextRequestId = 1;
  private pendingMetadata = new Map<number, PendingMetadata>();
  private startBitrateSent = false;
  private startBitrates = new Map<string, { cid: string; codec: string; kbps: number }>();
  /** Tracks of a codec with layers. Each offer has to ask for what lets a server choose among them, so this is not one-shot. */
  private layeredTracks = new Set<string>();

  constructor(
    sid: string,
    identity: string,
    private readonly options: LocalParticipantOptions,
  ) {
    super(sid, identity);
  }

  override get isLocal() {
    return true;
  }

  override getTrackPublication(source: TrackSource): LocalTrackPublication | undefined {
    return super.getTrackPublication(source) as LocalTrackPublication | undefined;
  }

  /** @internal */
  setEngine(engine: RTCEngine) {
    this.engine = engine;
    this.startBitrateSent = false;
    this.startBitrates.clear();
    engine.on('trackUnpublished', (sid) => this.onServerUnpublished(sid));
    engine.on('muteTrack', (req) => {
      const pub = this.trackPublications.get(req.sid) as LocalTrackPublication | undefined;
      if (!pub) return;
      void (req.muted ? pub.mute() : pub.unmute()).catch((e) => log.warn('could not apply mute from server', e));
    });
    engine.on('subscribedQualityUpdate', (update) => {
      if (!this.options.dynacast) return;
      const pub = this.trackPublications.get(update.trackSid) as LocalTrackPublication | undefined;
      if (!(pub?.track instanceof LocalVideoTrack)) return;
      const toSetting = (q: { quality: ProtoQuality; enabled: boolean }) => ({ quality: q.quality as number, enabled: q.enabled });
      if (update.subscribedCodecs.length > 0) {
        void pub.track.setPublishingCodecs(update.subscribedCodecs.map((c) => ({ codec: c.codec, qualities: c.qualities.map(toSetting) })));
      } else {
        // Only an older server sends this form. It describes the one codec there would be.
        void pub.track.setPublishingLayers(update.subscribedQualities.map(toSetting));
      }
    });
    engine.on('publisherAnswered', () => {
      for (const pub of this.videoTrackPublications.values()) {
        if (pub.track instanceof LocalVideoTrack) void pub.track.reapplyPublishingLayers();
      }
    });
    engine.on('requestResponse', (res) => {
      const pending = this.pendingMetadata.get(res.requestId);
      if (!pending || res.request.case !== 'updateMetadata') return;
      if (res.reason === RequestResponse_Reason.OK) this.finishMetadata(res.requestId);
      else this.finishMetadata(res.requestId, new SignalRequestError(res.message || 'request refused', RequestResponse_Reason[res.reason]));
    });
    engine.on('trackSubscribed', (sid) => {
      const pub = this.trackPublications.get(sid) as LocalTrackPublication | undefined;
      if (pub) this.emit(ParticipantEvent.LocalTrackSubscribed, pub);
    });
    engine.on('signalResumed', () => this.sendSubscriptionPermissions());
    engine.on('signalRestarted', () => {
      this.startBitrateSent = false;
      this.startBitrates.clear();
      this.sendSubscriptionPermissions();
    });
  }

  /** Later captures come from this device. @internal */
  setDefaultDevice(kind: 'audioinput' | 'videoinput', deviceId: ConstrainDOMString) {
    if (kind === 'audioinput') this.options.audioCaptureDefaults.deviceId = deviceId;
    else this.options.videoCaptureDefaults.deviceId = deviceId;
  }

  /** @internal */
  setEnabledCodecs(codecs: string[]) {
    this.enabledVideoCodecs = codecs;
  }

  /**
   * Applied to each publisher offer. The first video on a connection sets where the encoder starts, and after
   * that it has its own measurements to go on.
   */
  private mungeOffer = (sdp: string): string => {
    let out = enableOpusStereo(sdp);
    if (this.startBitrates.size > 0) {
      for (const { cid, codec, kbps } of this.startBitrates.values()) out = setStartBitrate(out, cid, codec, kbps);
      this.startBitrateSent = true;
      this.startBitrates.clear();
    }
    // Safari takes SVC with RIDs its own way, and this would not help it.
    if (!isSafari()) {
      for (const cid of this.layeredTracks) out = addDependencyDescriptor(out, cid);
    }
    return out;
  };

  /** @internal */
  override updateInfo(info: ParticipantInfo): boolean {
    if (!super.updateInfo(info)) return false;
    // The local tracks list is not authoritative: a track being published is not in it yet. But if the
    // server has a different idea of whether a track is muted, it needs telling.
    for (const ti of info.tracks) {
      const pub = this.trackPublications.get(ti.sid) as LocalTrackPublication | undefined;
      if (!pub) continue;
      const wire = pub.isMuted || (pub.track?.isUpstreamPaused ?? false);
      if (wire !== ti.muted) this.engine?.signal.sendMuteTrack(ti.sid, wire);
      pub.updateInfo(ti);
    }
    for (const [id, pending] of this.pendingMetadata) {
      if (pending.check()) this.finishMetadata(id);
    }
    return true;
  }

  private onServerUnpublished(sid: string) {
    const pub = this.trackPublications.get(sid) as LocalTrackPublication | undefined;
    if (pub?.track) void this.unpublishTrack(pub.track);
  }

  // Sources, and turning them on and off.

  setCameraEnabled(enabled: boolean, capture?: VideoCaptureOptions, publish?: TrackPublishOptions) {
    return this.setTrackEnabled(TrackSource.Camera, enabled, capture, publish);
  }

  setMicrophoneEnabled(enabled: boolean, capture?: AudioCaptureOptions, publish?: TrackPublishOptions) {
    return this.setTrackEnabled(TrackSource.Microphone, enabled, capture, publish);
  }

  setScreenShareEnabled(enabled: boolean, capture?: ScreenShareCaptureOptions, publish?: TrackPublishOptions) {
    return this.setTrackEnabled(TrackSource.ScreenShare, enabled, capture, publish);
  }

  async enableCameraAndMicrophone() {
    await Promise.all([this.setMicrophoneEnabled(true), this.setCameraEnabled(true)]);
  }

  private async setTrackEnabled(
    source: TrackSource,
    enabled: boolean,
    capture?: VideoCaptureOptions | AudioCaptureOptions | ScreenShareCaptureOptions,
    publish?: TrackPublishOptions,
  ): Promise<LocalTrackPublication | undefined> {
    // One request at a time per source, so double clicks do not publish two cameras.
    const inFlight = this.pendingPublishing.get(source);
    if (inFlight) {
      const pub = await inFlight.catch(() => undefined);
      if (enabled) {
        // A request to turn it off may have been waiting on the same publish, and it goes first. This one is later, so it wins.
        await pub?.unmute();
        return pub;
      }
    }
    let pub = this.getTrackPublication(source);
    if (!enabled) {
      if (!pub?.track) return pub;
      if (source === TrackSource.ScreenShare) {
        // There is nothing to resume a screen share from, so it ends, along with its audio.
        await this.unpublishTrack(pub.track);
        const audio = this.getTrackPublication(TrackSource.ScreenShareAudio);
        if (audio?.track) await this.unpublishTrack(audio.track);
      } else {
        await pub.mute();
      }
      return pub;
    }
    if (pub) {
      await pub.unmute();
      return pub;
    }
    const run = this.acquireAndPublish(source, capture, publish);
    this.pendingPublishing.set(source, run);
    try {
      pub = await run;
    } catch (e) {
      if (e instanceof Error) {
        if (source === TrackSource.Camera) this.lastCameraError = e;
        if (source === TrackSource.Microphone) this.lastMicrophoneError = e;
        const kind: MediaDeviceKind | undefined = source === TrackSource.Microphone ? 'audioinput' : source === TrackSource.Camera ? 'videoinput' : undefined;
        if (e.name === 'NotAllowedError' || e.name === 'NotFoundError' || e.name === 'NotReadableError' || e.name === 'OverconstrainedError') {
          this.emit(ParticipantEvent.MediaDevicesError, e, kind);
        }
      }
      throw e;
    } finally {
      this.pendingPublishing.delete(source);
    }
    return pub;
  }

  private async acquireAndPublish(
    source: TrackSource,
    capture: VideoCaptureOptions | AudioCaptureOptions | ScreenShareCaptureOptions | undefined,
    publish: TrackPublishOptions | undefined,
  ): Promise<LocalTrackPublication | undefined> {
    let tracks: LocalTrack[];
    if (source === TrackSource.Camera) {
      tracks = await this.createTracks({ video: (capture as VideoCaptureOptions) ?? true });
    } else if (source === TrackSource.Microphone) {
      tracks = await this.createTracks({ audio: (capture as AudioCaptureOptions) ?? true });
    } else if (source === TrackSource.ScreenShare) {
      tracks = await createLocalScreenTracks(capture as ScreenShareCaptureOptions);
    } else {
      throw new TrackInvalidError(`cannot capture source ${source}`);
    }
    try {
      const pubs = await Promise.all(tracks.map((t) => this.publishTrack(t, publish)));
      return pubs.find((p) => p.source === source) ?? pubs[0];
    } catch (e) {
      for (const t of tracks) t.stop();
      throw e;
    }
  }

  createTracks(options: CreateLocalTracksOptions): Promise<LocalTrack[]> {
    const merge = <T>(value: boolean | T | undefined, defaults: T): T | false =>
      value === undefined || value === false ? false : { ...defaults, ...(value === true ? {} : value) };
    return createLocalTracks({
      audio: merge(options.audio, this.options.audioCaptureDefaults),
      video: merge(options.video, this.options.videoCaptureDefaults),
    });
  }

  // Publishing.

  /**
   * Publishes a track. It resolves when the server has accepted the track and answered the offer, which is
   * before any media has arrived there.
   */
  async publishTrack(input: LocalTrack | MediaStreamTrack, options?: TrackPublishOptions): Promise<LocalTrackPublication> {
    const engine = this.engine;
    if (!engine || engine.isClosed) throw new PublishTrackError('not connected to a room', 408);
    const track = input instanceof MediaStreamTrack ? mediaTrackToLocalTrack(input) : input;
    for (const pub of this.trackPublications.values()) {
      if (pub.track === track || (pub.track as LocalTrack | undefined)?.mediaStreamTrack === track.mediaStreamTrack) {
        return pub as LocalTrackPublication;
      }
    }

    const opts: TrackPublishOptions = { ...publishDefaults, ...this.options.publishDefaults, ...options };
    if (options?.source) track.source = options.source;
    if (track.source === TrackSource.Unknown && track.kind === TrackKind.Audio) track.source = TrackSource.Microphone;
    if (track.source === TrackSource.Unknown && track.kind === TrackKind.Video) track.source = TrackSource.Camera;

    await engine.waitUntilReady(SIGNAL_READY_TIMEOUT_MS);
    this.assertCanPublish(track.source);

    try {
      return await this.publishPrepared(engine, track, opts);
    } catch (e) {
      // A negotiation that failed takes the connection with it, and restarting that clears everything in
      // flight. This publish goes through once the new session is up.
      if (!(e instanceof NegotiationError)) throw e;
      await this.engine?.waitUntilReady(SIGNAL_READY_TIMEOUT_MS);
      return this.publishPrepared(this.engine!, track, opts);
    }
  }

  private assertCanPublish(source: TrackSource) {
    const perm = this.permissions;
    const allowed = perm?.canPublish && (perm.canPublishSources.length === 0 || perm.canPublishSources.includes(sourceToProto(source)));
    if (!allowed) throw new PublishTrackError('this participant does not have permission to publish that source', 403);
  }

  private chooseCodec(wanted: VideoCodec | undefined): VideoCodec {
    const codec = wanted ?? publishDefaults.videoCodec;
    if (this.enabledVideoCodecs.length === 0 || this.enabledVideoCodecs.includes(codec)) return codec;
    return this.enabledVideoCodecs[0] as VideoCodec;
  }

  private async publishPrepared(engine: RTCEngine, track: LocalTrack, opts: TrackPublishOptions): Promise<LocalTrackPublication> {
    // The offer identifies the track by its media ID, so that is what this request has to name.
    track.cid = track.mediaStreamTrack.id;
    const req = create(AddTrackRequestSchema, {
      cid: track.cid,
      name: opts.name ?? track.source,
      type: kindToProto(track.kind),
      muted: track.isMuted,
      source: sourceToProto(track.source),
    });
    if (opts.stream) req.stream = opts.stream;

    let encodings: RTCRtpEncodingParameters[];
    if (track instanceof LocalVideoTrack) {
      encodings = await this.prepareVideo(track, opts, req);
    } else {
      req.disableDtx = !(opts.dtx ?? true);
      req.disableRed = !(opts.red ?? true);
      req.stereo = opts.forceStereo ?? false;
      if (req.disableDtx) req.audioFeatures.push(AudioTrackFeature.TF_NO_DTX);
      if (req.stereo) req.audioFeatures.push(AudioTrackFeature.TF_STEREO);
      encodings = [{ maxBitrate: opts.audioBitrate ?? publishDefaults.audioBitrate, priority: 'high', networkPriority: 'high' }];
    }

    const publisher = engine.publisher;
    if (!publisher) throw new PublishTrackError('no publisher connection', 408);
    publisher.offerMunger ??= this.mungeOffer;

    // A server listing codecs at join has made its choice, so the offer need not wait for it to say so.
    // Otherwise it can overrule the codec in its response, and the offer must not get ahead of that.
    const canOfferNow = this.enabledVideoCodecs.length > 0;
    const accepted = engine.addTrack(req);

    const sender = this.createSender(publisher.pc, track, encodings, opts);
    if (track instanceof LocalVideoTrack && !this.startBitrateSent) {
      const kbps = startBitrateKbps(encodings, track.source === TrackSource.ScreenShare);
      if (kbps) this.startBitrates.set(track.cid, { cid: track.cid, codec: track.codec ?? 'vp8', kbps });
    }

    let info: TrackInfo;
    try {
      if (canOfferNow) {
        const [ti] = await Promise.all([accepted, engine.negotiate()]);
        info = ti;
      } else {
        info = await accepted;
        await engine.negotiate();
      }
    } catch (e) {
      engine.cancelPendingTrack(req.cid);
      accepted.catch(() => undefined);
      this.removeSender(publisher.pc, sender);
      track.sender = undefined;
      if (!(e instanceof NegotiationError)) void engine.negotiate().catch(() => undefined);
      throw e;
    }

    const pub = new LocalTrackPublication(track, info, opts);
    this.wireLocalPublication(pub);
    this.addPublication(pub);
    this.emit(ParticipantEvent.LocalTrackPublished, pub);
    return pub;
  }

  private async prepareVideo(track: LocalVideoTrack, opts: TrackPublishOptions, req: AddTrackRequest) {
    const codec = this.chooseCodec(opts.videoCodec);
    track.codec = codec;
    const size = await track.waitForDimensions();
    const width = size?.width ?? 1280;
    const height = size?.height ?? 720;
    const encodings = computeVideoEncodings(track.source === TrackSource.ScreenShare, width, height, { ...opts, videoCodec: codec });
    track.encodings = encodings;
    req.width = width;
    req.height = height;
    req.layers = describeLayers(width, height, encodings).map((l) => create(VideoLayerSchema, { ...l, quality: l.quality as number as ProtoQuality }));
    const mode = isSvcCodec(codec) ? VideoLayer_Mode.MULTIPLE_SPATIAL_LAYERS_PER_STREAM : VideoLayer_Mode.ONE_SPATIAL_LAYER_PER_STREAM;
    if (isSvcCodec(codec)) this.layeredTracks.add(track.cid);
    req.simulcastCodecs = [create(SimulcastCodecSchema, { codec, cid: track.cid, videoLayerMode: mode })];
    return encodings;
  }

  private createSender(pc: RTCPeerConnection, track: LocalTrack, encodings: RTCRtpEncodingParameters[], opts: TrackPublishOptions): RTCRtpSender {
    const transceiver = pc.addTransceiver(track.mediaStreamTrack, {
      direction: 'sendonly',
      streams: track.mediaStream ? [track.mediaStream] : undefined,
      sendEncodings: encodings,
    });
    this.applyCodecPreference(transceiver, track, opts);
    track.sender = transceiver.sender;
    if (opts.degradationPreference) {
      const params = transceiver.sender.getParameters() as RTCRtpSendParameters & { degradationPreference?: string };
      params.degradationPreference = opts.degradationPreference;
      void transceiver.sender.setParameters(params).catch((e) => log.debug('could not set degradation preference', e));
    }
    return transceiver.sender;
  }

  /**
   * Browsers offer every codec they have, and the server answers from that list in its own order. Putting
   * the wanted one first is what makes a codec choice stick. For audio that is RED, where the browser has
   * it and the track wants it: the redundancy it carries repairs lost packets without a round trip.
   */
  private applyCodecPreference(transceiver: RTCRtpTransceiver, track: LocalTrack, opts: TrackPublishOptions) {
    const caps = typeof RTCRtpSender.getCapabilities === 'function' ? RTCRtpSender.getCapabilities(track.kind) : null;
    if (!caps || typeof transceiver.setCodecPreferences !== 'function') return;
    const wantRed = opts.red ?? true;
    const preferred = track instanceof LocalVideoTrack ? `video/${track.codec}` : wantRed ? 'audio/red' : 'audio/opus';
    const first = caps.codecs.filter((c) => c.mimeType.toLowerCase() === preferred);
    if (first.length === 0) return;
    // Left in the list, a server could still pick RED for a track that asked not to use it.
    const rest = caps.codecs.filter((c) => c.mimeType.toLowerCase() !== preferred && (wantRed || c.mimeType.toLowerCase() !== 'audio/red'));
    try {
      transceiver.setCodecPreferences([...first, ...rest]);
    } catch (e) {
      log.debug('could not set codec preference', e);
    }
  }

  private removeSender(pc: RTCPeerConnection, sender: RTCRtpSender) {
    if (pc.connectionState === 'closed') return;
    const transceiver = pc.getTransceivers().find((t) => t.sender === sender);
    try {
      // Servers learn of this from the section's direction in the next offer. There is no message for it.
      if (transceiver) transceiver.direction = 'inactive';
      pc.removeTrack(sender);
    } catch (e) {
      log.debug('could not remove sender', e);
    }
  }

  private wireLocalPublication(pub: LocalTrackPublication) {
    pub.on(TrackEvent.Muted, () => {
      this.engine?.signal.sendMuteTrack(pub.trackSid, true);
      this.emit(ParticipantEvent.TrackMuted, pub);
    });
    pub.on(TrackEvent.Unmuted, () => {
      this.engine?.signal.sendMuteTrack(pub.trackSid, false);
      this.emit(ParticipantEvent.TrackUnmuted, pub);
    });
    pub.on(TrackEvent.Ended, () => {
      // The browser ended the capture: a shared window closed, a device was unplugged, permission revoked.
      if (pub.track) void this.unpublishTrack(pub.track).catch((e) => log.warn('could not unpublish ended track', e));
    });
  }

  /** Stops publishing a track. By default this also stops capture. */
  async unpublishTrack(input: LocalTrack | MediaStreamTrack, stopOnUnpublish?: boolean): Promise<LocalTrackPublication | undefined> {
    const pub = this.findPublication(input);
    if (!pub) return undefined;
    const track = pub.track!;
    const stop = stopOnUnpublish ?? this.options.stopLocalTrackOnUnpublish;

    this.removePublication(pub.trackSid);
    pub.removeAllListeners();
    pub.detachFromTrack();

    const publisher = this.engine?.publisher;
    if (track.sender && publisher) this.removeSender(publisher.pc, track.sender);
    track.sender = undefined;
    if (stop) track.stop();

    this.emit(ParticipantEvent.LocalTrackUnpublished, pub);
    if (publisher) await this.engine?.negotiate().catch((e) => log.warn('negotiation after unpublish failed', e));
    return pub;
  }

  async unpublishTracks(tracks: Array<LocalTrack | MediaStreamTrack>) {
    return Promise.all(tracks.map((t) => this.unpublishTrack(t)));
  }

  private findPublication(input: LocalTrack | MediaStreamTrack): LocalTrackPublication | undefined {
    for (const pub of this.trackPublications.values()) {
      const t = pub.track as LocalTrack | undefined;
      if (t === input || t?.mediaStreamTrack === input) return pub as LocalTrackPublication;
    }
    return undefined;
  }

  /**
   * Publishes everything again in a new session. The tracks and their capture carry over: applications hold
   * on to them, and restarting a camera to reconnect would be visible to anyone looking at it.
   * @internal
   */
  async republishAllTracks() {
    const previous = [...this.trackPublications.values()] as LocalTrackPublication[];
    for (const pub of previous) {
      const track = pub.track;
      const options = pub.options;
      this.removePublication(pub.trackSid);
      pub.removeAllListeners();
      pub.detachFromTrack();
      if (!track) continue;
      track.sender = undefined;
      this.emit(ParticipantEvent.LocalTrackUnpublished, pub);
      try {
        await this.publishTrack(track, options);
      } catch (e) {
        log.warn('could not publish a track again after reconnecting', track.source, e);
      }
    }
  }

  /** The track publications as a session ending leaves them: released, unsent. @internal */
  clearPublications(stopTracks: boolean) {
    for (const pub of [...this.trackPublications.values()] as LocalTrackPublication[]) {
      const track = pub.track;
      this.removePublication(pub.trackSid);
      pub.removeAllListeners();
      pub.detachFromTrack();
      if (track) {
        track.sender = undefined;
        if (stopTracks) track.stop();
      }
      this.emit(ParticipantEvent.LocalTrackUnpublished, pub);
    }
  }

  // Data.

  /** Sends data to others in the room. A reliable send during an outage goes out when the session returns. */
  async publishData(data: Uint8Array, options: DataPublishOptions = {}): Promise<void> {
    if (!this.engine || this.engine.isClosed) throw new PublishDataError('not connected to a room');
    const kind = (options.reliable ?? true) ? DataPacketKind.RELIABLE : DataPacketKind.LOSSY;
    const destinations = options.destinationIdentities ?? [];
    const packet: DataPacket = create(DataPacketSchema, {
      kind: kind as number as DataPacket_Kind,
      destinationIdentities: destinations,
      value: {
        case: 'user',
        value: create(UserPacketSchema, {
          participantIdentity: this.identity,
          payload: data,
          topic: options.topic,
          destinationIdentities: destinations,
        }),
      },
    });
    await this.engine.sendDataPacket(packet, kind);
  }

  // Who may see this participant's tracks.

  /** Controls who may subscribe to this participant's tracks. */
  setTrackSubscriptionPermissions(allParticipantsAllowed: boolean, permissions: ParticipantTrackPermission[] = []) {
    this.allParticipantsAllowed = allParticipantsAllowed;
    this.trackPermissions = permissions;
    this.sendSubscriptionPermissions();
  }

  /** Sent on every connect too. A server can have lost or never had this, and it costs little to say again. */
  private sendSubscriptionPermissions() {
    const trackPermissions: TrackPermission[] = this.trackPermissions.map((p) =>
      create(TrackPermissionSchema, {
        participantIdentity: p.participantIdentity ?? '',
        participantSid: p.participantSid ?? '',
        allTracks: p.allowAll ?? false,
        trackSids: p.allowedTrackSids ?? [],
      }),
    );
    this.engine?.signal.sendSubscriptionPermission(
      create(SubscriptionPermissionSchema, { allParticipants: this.allParticipantsAllowed, trackPermissions }),
    );
  }

  /** @internal */
  sendInitialPermissions() {
    this.sendSubscriptionPermissions();
  }

  // Own metadata. These go through the server, which has the final say, and take effect when it replies.

  setMetadata(metadata: string): Promise<void> {
    return this.requestMetadataUpdate({ metadata });
  }

  setName(name: string): Promise<void> {
    return this.requestMetadataUpdate({ name });
  }

  setAttributes(attributes: Record<string, string>): Promise<void> {
    return this.requestMetadataUpdate({ attributes });
  }

  private requestMetadataUpdate(update: { metadata?: string; name?: string; attributes?: Record<string, string> }): Promise<void> {
    const engine = this.engine;
    if (!engine) return Promise.reject(new SignalRequestError('not connected to a room', 'NOT_CONNECTED'));
    const requestId = this.nextRequestId++;
    return new Promise<void>((resolve, reject) => {
      // Some servers confirm with a response, and others only show the change in the next update.
      const check = () =>
        (update.metadata === undefined || this.metadata === update.metadata) &&
        (update.name === undefined || this.name === update.name) &&
        (update.attributes === undefined || Object.entries(update.attributes).every(([k, v]) => (v === '' ? !(k in this.attributes) : this.attributes[k] === v)));
      const timer = setTimeout(() => {
        this.finishMetadata(requestId, new SignalRequestError('the server did not confirm the update', 'TIMEOUT'));
      }, METADATA_TIMEOUT_MS);
      this.pendingMetadata.set(requestId, { resolve, reject, timer, check });
      engine.signal.sendUpdateMetadata(
        create(UpdateParticipantMetadataSchema, {
          requestId,
          metadata: update.metadata ?? '',
          name: update.name ?? '',
          attributes: update.attributes ?? {},
        }),
      );
    });
  }

  private finishMetadata(id: number, error?: unknown) {
    const pending = this.pendingMetadata.get(id);
    if (!pending) return;
    clearTimeout(pending.timer);
    this.pendingMetadata.delete(id);
    if (error) pending.reject(error);
    else pending.resolve();
  }
}

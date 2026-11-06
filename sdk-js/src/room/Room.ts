import { create } from '@bufbuild/protobuf';
import { RTCEngine } from '../engine/RTCEngine.js';
import { ConnectionQuality, ConnectionState, DataPacketKind, DisconnectReason, SubscriptionError, TrackKind, TrackSource } from '../enums.js';
import { ConnectionError, ConnectionErrorReason } from '../errors.js';
import { ParticipantEvent, RoomEvent, TrackEvent } from '../events.js';
import { log } from '../logger.js';
import { type RoomConnectOptions, type RoomOptions, connectOptionDefaults, roomOptionDefaults } from '../options.js';
import { LocalParticipant } from '../participant/LocalParticipant.js';
import type { ParticipantEventCallbacks } from '../participant/Participant.js';
import { Participant } from '../participant/Participant.js';
import { RemoteParticipant } from '../participant/RemoteParticipant.js';
import {
  type DataPacket,
  ParticipantInfo_State,
  type ParticipantInfo,
  type Room as ProtoRoom,
  type ServerInfo,
  type SpeakerInfo,
} from '../proto/livekit_models_pb.js';
import {
  type JoinResponse,
  SimulateScenarioSchema,
  type SyncState,
  TrackPublishedResponseSchema,
  UpdateSubscriptionSchema,
} from '../proto/livekit_rtc_pb.js';
import { streamStateFromProto } from './protoState.js';
import { resolveTrackSid, unpackStreamId } from './streamId.js';
import { audioCaptureDefaults, publishDefaults, videoCaptureDefaults } from '../track/options.js';
import { qualityFromProto } from '../track/protoConvert.js';
import { RemoteAudioTrack } from '../track/RemoteAudioTrack.js';
import type { RemoteTrack } from '../track/RemoteTrack.js';
import type { RemoteTrackPublication } from '../track/RemoteTrackPublication.js';
import type { AdaptiveStreamSettings } from '../track/RemoteVideoTrack.js';
import type { LocalTrackPublication } from '../track/LocalTrackPublication.js';
import type { TrackPublication } from '../track/TrackPublication.js';
import { deferred } from '../util/async.js';
import { TypedEmitter } from '../util/emitter.js';

// How long media can wait for the metadata that identifies it before the application hears about it.
const MEDIA_WITHOUT_METADATA_MS = 3000;

export type RoomEventCallbacks = {
  connected: () => void;
  reconnecting: () => void;
  signalReconnecting: () => void;
  reconnected: () => void;
  disconnected: (reason?: DisconnectReason) => void;
  connectionStateChanged: (state: ConnectionState) => void;
  mediaDevicesChanged: () => void;
  participantConnected: (participant: RemoteParticipant) => void;
  participantDisconnected: (participant: RemoteParticipant, reason?: DisconnectReason) => void;
  trackPublished: (publication: RemoteTrackPublication, participant: RemoteParticipant) => void;
  trackSubscribed: (track: RemoteTrack, publication: RemoteTrackPublication, participant: RemoteParticipant) => void;
  trackSubscriptionFailed: (trackSid: string, participant?: RemoteParticipant, reason?: SubscriptionError) => void;
  trackUnpublished: (publication: RemoteTrackPublication, participant: RemoteParticipant) => void;
  trackUnsubscribed: (track: RemoteTrack, publication: RemoteTrackPublication, participant: RemoteParticipant) => void;
  trackMuted: (publication: TrackPublication, participant: Participant) => void;
  trackUnmuted: (publication: TrackPublication, participant: Participant) => void;
  localTrackPublished: (publication: LocalTrackPublication, participant: LocalParticipant) => void;
  localTrackUnpublished: (publication: LocalTrackPublication, participant: LocalParticipant) => void;
  activeSpeakersChanged: (speakers: Participant[]) => void;
  participantMetadataChanged: (metadata: string | undefined, participant: Participant) => void;
  participantNameChanged: (name: string, participant: Participant) => void;
  participantAttributesChanged: (changed: Record<string, string>, participant: Participant) => void;
  participantActive: (participant: Participant) => void;
  roomMetadataChanged: (metadata: string) => void;
  dataReceived: (payload: Uint8Array, participant: RemoteParticipant | undefined, kind: DataPacketKind, topic: string | undefined, encryptionType: undefined, participantIdentity: string | undefined) => void;
  connectionQualityChanged: (quality: ConnectionQuality, participant: Participant) => void;
  trackStreamStateChanged: (publication: RemoteTrackPublication, state: string, participant: RemoteParticipant) => void;
  trackSubscriptionPermissionChanged: (publication: RemoteTrackPublication, status: string, participant: RemoteParticipant) => void;
  trackSubscriptionStatusChanged: (publication: RemoteTrackPublication, status: string, participant: RemoteParticipant) => void;
  audioPlaybackChanged: (playing: boolean) => void;
  videoPlaybackChanged: (playing: boolean) => void;
  mediaDevicesError: (error: Error, kind?: MediaDeviceKind) => void;
  participantPermissionsChanged: (previous: unknown, participant: Participant) => void;
  signalConnected: () => void;
  recordingStatusChanged: (recording: boolean) => void;
  dcBufferStatusChanged: (isLow: boolean, kind: DataPacketKind) => void;
  activeDeviceChanged: (kind: MediaDeviceKind, deviceId: string) => void;
  localTrackSubscribed: (publication: LocalTrackPublication, participant: LocalParticipant) => void;
};

interface PendingMedia {
  mediaTrack: MediaStreamTrack;
  stream: MediaStream;
  receiver: RTCRtpReceiver;
  participantSid: string;
  trackSid?: string;
  timer?: ReturnType<typeof setTimeout>;
  reported: boolean;
  onRemoved: (ev: MediaStreamTrackEvent) => void;
}

/** A conversation, and this client's part in it. This is where applications start. */
export class Room extends TypedEmitter<RoomEventCallbacks> {
  state = ConnectionState.Disconnected;
  remoteParticipants = new Map<string, RemoteParticipant>();
  activeSpeakers: Participant[] = [];
  readonly localParticipant: LocalParticipant;
  readonly options: RoomOptions;
  serverInfo?: Partial<ServerInfo>;
  /** The session underneath. Exposed for diagnostics and tests, not as an API. */
  engine?: RTCEngine;

  private roomInfo?: ProtoRoom;
  private connectOptions = { ...connectOptionDefaults } as Required<Pick<RoomConnectOptions, 'autoSubscribe' | 'websocketTimeout' | 'peerConnectionTimeout' | 'maxRetries'>> & RoomConnectOptions;
  private connectFuture?: Promise<void>;
  private connectAbort?: AbortController;
  private bufferedEvents: Array<() => void> = [];
  private pendingMedia: PendingMedia[] = [];
  private sidWaiters = deferred<string>();
  private audioEnabled = true;
  private videoEnabled = true;
  private activeDevices = new Map<MediaDeviceKind, string>();
  private restartGeneration = 0;
  private lastToken?: string;

  constructor(options: RoomOptions = {}) {
    super();
    this.options = { ...roomOptionDefaults, ...options };
    this.localParticipant = new LocalParticipant('', '', {
      publishDefaults: { ...publishDefaults, ...options.publishDefaults },
      audioCaptureDefaults: { ...audioCaptureDefaults, ...options.audioCaptureDefaults },
      videoCaptureDefaults: { ...videoCaptureDefaults, ...options.videoCaptureDefaults },
      stopLocalTrackOnUnpublish: this.options.stopLocalTrackOnUnpublish ?? true,
      dynacast: this.options.dynacast ?? true,
    });
    this.forwardParticipantEvents(this.localParticipant);
    this.localParticipant.on(ParticipantEvent.LocalTrackPublished, (pub) => this.emit(RoomEvent.LocalTrackPublished, pub, this.localParticipant));
    this.localParticipant.on(ParticipantEvent.LocalTrackUnpublished, (pub) => this.emit(RoomEvent.LocalTrackUnpublished, pub, this.localParticipant));
    this.localParticipant.on(ParticipantEvent.MediaDevicesError, (e, kind) => this.emit(RoomEvent.MediaDevicesError, e, kind));
    this.localParticipant.on(ParticipantEvent.LocalTrackSubscribed, (pub) => this.emitWhenConnected(RoomEvent.LocalTrackSubscribed, pub, this.localParticipant));
    if (options.audioOutput?.deviceId) this.activeDevices.set('audiooutput', options.audioOutput.deviceId);
  }

  /** Lists capture and playback devices. Device names stay hidden until a permission prompt has been answered. */
  static async getLocalDevices(kind?: MediaDeviceKind, requestPermissions = true): Promise<MediaDeviceInfo[]> {
    let devices = await navigator.mediaDevices.enumerateDevices();
    const named = (d: MediaDeviceInfo) => !kind || d.kind === kind;
    if (requestPermissions && kind !== 'audiooutput' && devices.filter(named).some((d) => !d.label)) {
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: kind !== 'videoinput',
        video: kind !== 'audioinput',
      });
      for (const t of stream.getTracks()) t.stop();
      devices = await navigator.mediaDevices.enumerateDevices();
    }
    return devices.filter(named);
  }

  get name() {
    return this.roomInfo?.name ?? '';
  }

  get metadata() {
    return this.roomInfo?.metadata;
  }

  get numParticipants() {
    return this.roomInfo?.numParticipants ?? 0;
  }

  get numPublishers() {
    return this.roomInfo?.numPublishers ?? 0;
  }

  get isRecording() {
    return this.roomInfo?.activeRecording ?? false;
  }

  /**
   * The latest access token the server issued for this session, for an application's own calls to its server. The
   * one passed to connect may have expired by now. It stays available after leaving, until the next connect.
   */
  get accessToken(): string | undefined {
    return this.engine?.token ?? this.lastToken;
  }

  get canPlaybackAudio() {
    return this.audioEnabled;
  }

  get canPlaybackVideo() {
    return this.videoEnabled;
  }

  /** The server's ID for this room. Some servers assign it after the join, so this can take a moment. */
  getSid(): Promise<string> {
    return this.sidWaiters.promise;
  }

  getParticipantByIdentity(identity: string): Participant | undefined {
    return identity === this.localParticipant.identity ? this.localParticipant : this.remoteParticipants.get(identity);
  }

  /**
   * Joins the room. It resolves once the primary connection is up, which is when this client can hear and
   * be heard. A failure says why: the URL, the token, the server and the network each fail differently.
   */
  connect(url: string, token: string, opts: RoomConnectOptions = {}): Promise<void> {
    if (this.state === ConnectionState.Connected) return Promise.resolve();
    if (this.connectFuture) return this.connectFuture;
    this.connectOptions = { ...connectOptionDefaults, ...opts };
    this.setState(ConnectionState.Connecting);
    const abort = new AbortController();
    this.connectAbort = abort;
    const future: Promise<void> = this.runConnect(url, token, abort.signal).finally(() => {
      if (this.connectFuture === future) this.connectFuture = undefined;
    });
    this.connectFuture = future;
    return future;
  }

  private async runConnect(url: string, token: string, abort: AbortSignal) {
    let attempts = 0;
    for (;;) {
      attempts++;
      let engine: RTCEngine | undefined;
      try {
        engine = this.createEngine();
        await this.attemptConnection(engine, url, token, abort);
        return;
      } catch (e) {
        // A disconnect while connecting has already cleaned up, and a new connect may be well under way.
        if (abort.aborted) throw new ConnectionError('connection attempt cancelled', ConnectionErrorReason.Cancelled);
        await engine?.close(false);
        const retry = e instanceof ConnectionError && e.reason === ConnectionErrorReason.ServerUnreachable && attempts < this.connectOptions.maxRetries;
        if (retry) {
          log.info('server unreachable, trying again');
          continue;
        }
        const error = e instanceof ConnectionError ? e : new ConnectionError(`could not connect: ${String(e)}`, ConnectionErrorReason.InternalError);
        this.handleDisconnect(true, error.disconnectReason);
        throw error;
      }
    }
  }

  private async attemptConnection(engine: RTCEngine, url: string, token: string, abort: AbortSignal) {
    const aborted = new Promise<never>((_, reject) => {
      abort.addEventListener('abort', () => reject(new ConnectionError('connection attempt cancelled', ConnectionErrorReason.Cancelled)));
    });
    aborted.catch(() => undefined);
    const join = await engine.join(
      url,
      token,
      {
        signal: {
          autoSubscribe: this.connectOptions.autoSubscribe,
          adaptiveStream: !!this.options.adaptiveStream,
        },
        peerConnectionTimeout: this.connectOptions.peerConnectionTimeout,
        websocketTimeout: this.connectOptions.websocketTimeout,
        rtcConfig: this.connectOptions.rtcConfig,
        reconnectPolicy: this.options.reconnectPolicy,
      },
      abort,
    );
    if (abort.aborted) throw new ConnectionError('connection attempt cancelled', ConnectionErrorReason.Cancelled);
    this.applyJoinResponse(join);
    // Publishing is allowed from here: a server takes a track before its connections are up.
    this.emit(RoomEvent.SignalConnected);

    await Promise.race([engine.waitForPrimaryConnected(), aborted]);
    this.localParticipant.sendInitialPermissions();
    this.setState(ConnectionState.Connected);
    this.emit(RoomEvent.Connected);
    this.deliverPendingMedia();
  }

  private createEngine(): RTCEngine {
    const engine = new RTCEngine();
    this.engine = engine;
    this.localParticipant.setEngine(engine);
    engine.syncStateProvider = () => this.syncState();
    engine.on('disconnected', (reason) => this.handleDisconnect(true, reason));
    engine.on('resuming', () => {
      // Each retry passes through here, and the application has already heard.
      if (this.state === ConnectionState.SignalReconnecting) return;
      this.setState(ConnectionState.SignalReconnecting);
      this.emit(RoomEvent.SignalReconnecting);
    });
    engine.on('resumed', () => this.onResumed());
    engine.on('restarting', () => this.onRestarting());
    engine.on('signalRestarted', (join) => void this.onSignalRestarted(engine, join));
    engine.on('mediaTrackAdded', (track, stream, receiver) => this.onMediaTrackAdded(track, stream, receiver));
    engine.on('participantUpdate', (u) => this.handleParticipantUpdates(u.participants));
    engine.on('speakersChanged', (u) => this.handleSpeakerDeltas(u.speakers));
    engine.on('roomUpdate', (room) => this.setRoomInfo(room));
    engine.on('connectionQuality', (u) => {
      for (const q of u.updates) {
        const quality = qualityFromProto(q.quality);
        if (q.participantSid === this.localParticipant.sid) this.localParticipant.setConnectionQuality(quality);
        else this.findRemoteBySid(q.participantSid)?.setConnectionQuality(quality);
      }
    });
    engine.on('streamStateUpdate', (u) => {
      for (const info of u.streamStates) {
        const pub = this.findRemoteBySid(info.participantSid)?.trackPublications.get(info.trackSid) as RemoteTrackPublication | undefined;
        pub?.track?.setStreamState(streamStateFromProto(info.state));
      }
    });
    engine.on('subscriptionPermissionUpdate', (u) => {
      const pub = this.findRemoteBySid(u.participantSid)?.trackPublications.get(u.trackSid) as RemoteTrackPublication | undefined;
      pub?.setAllowed(u.allowed);
    });
    engine.on('subscriptionError', (res) => this.onSubscriptionError(res.trackSid, res.err as number as SubscriptionError));
    engine.on('dataPacket', (packet, kind) => this.handleDataPacket(packet, kind));
    engine.on('bufferStatus', (low, kind) => this.emit(RoomEvent.DCBufferStatusChanged, low, kind));
    this.listenForPageLeave();
    return engine;
  }

  private listenForPageLeave() {
    if (typeof navigator !== 'undefined') navigator.mediaDevices?.addEventListener?.('devicechange', this.onDeviceChange);
    if (!this.options.disconnectOnPageLeave || typeof window === 'undefined') return;
    // Not beforeunload: the page can still decide to stay after that, and would find itself out of the room.
    window.addEventListener('pagehide', this.onPageLeave);
  }

  private stopListeningForDevices() {
    if (typeof navigator !== 'undefined') navigator.mediaDevices?.removeEventListener?.('devicechange', this.onDeviceChange);
  }

  private onDeviceChange = () => {
    this.emit(RoomEvent.MediaDevicesChanged);
  };

  private stopListeningForPageLeave() {
    if (typeof window === 'undefined') return;
    window.removeEventListener('pagehide', this.onPageLeave);
  }

  private onPageLeave = () => {
    void this.disconnect();
  };

  /**
   * Leaves the room, telling the server, so others see the departure at once. The room is disconnected as
   * soon as this returns, so calling connect straight after is safe. The promise covers the goodbye.
   */
  async disconnect(stopTracks = true): Promise<void> {
    this.connectAbort?.abort();
    this.connectFuture = undefined;
    const engine = this.engine;
    this.lastToken = engine?.token ?? this.lastToken;
    this.engine = undefined;
    const closing = engine?.close(true);
    this.handleDisconnect(stopTracks, DisconnectReason.CLIENT_INITIATED);
    await closing;
  }

  private handleDisconnect(stopTracks: boolean, reason?: DisconnectReason) {
    this.restartGeneration++;
    this.stopListeningForPageLeave();
    this.stopListeningForDevices();
    if (this.engine) {
      this.lastToken = this.engine.token ?? this.lastToken;
      void this.engine.close(false);
      this.engine = undefined;
    }
    this.clearPendingMedia();
    this.bufferedEvents = [];
    this.connectAbort = undefined;
    if (this.state === ConnectionState.Disconnected) return;
    for (const p of [...this.remoteParticipants.values()]) {
      for (const pub of p.getTrackPublications()) p.unpublishTrack(pub.trackSid);
    }
    this.remoteParticipants.clear();
    this.activeSpeakers = [];
    this.localParticipant.clearPublications(stopTracks);
    this.roomInfo = undefined;
    this.sidWaiters = deferred<string>();
    this.setState(ConnectionState.Disconnected);
    this.emit(RoomEvent.Disconnected, reason === DisconnectReason.UNKNOWN_REASON ? undefined : reason);
  }

  private setState(state: ConnectionState) {
    if (this.state === state) return;
    this.state = state;
    this.emit(RoomEvent.ConnectionStateChanged, state);
  }

  /**
   * Events about others go through here. While connecting, they would describe people already in the room
   * as if they had just arrived, so they are dropped. While reconnecting, the application is told after it
   * has heard the connection is back, so it never sees the room change while it thinks the room is gone.
   */
  private emitWhenConnected<K extends keyof RoomEventCallbacks>(event: K, ...args: Parameters<RoomEventCallbacks[K]>) {
    if (this.state === ConnectionState.Connected) {
      this.emit(event, ...args);
      return;
    }
    if (this.state === ConnectionState.Connecting || this.state === ConnectionState.Disconnected) return;
    this.bufferedEvents.push(() => this.emit(event, ...args));
  }

  private flushBufferedEvents() {
    const events = this.bufferedEvents;
    this.bufferedEvents = [];
    for (const e of events) e();
  }

  private applyJoinResponse(join: JoinResponse) {
    const pi = join.participant;
    if (!pi) throw new ConnectionError('the server did not describe this participant', ConnectionErrorReason.InternalError);
    this.serverInfo = join.serverInfo ?? { version: join.serverVersion, region: join.serverRegion };
    this.localParticipant.updateInfo(pi);
    this.localParticipant.setEnabledCodecs(
      join.enabledPublishCodecs
        .map((c) => c.mime.toLowerCase())
        .filter((m) => m.startsWith('video/'))
        .map((m) => m.slice('video/'.length))
        .filter((c) => c === 'vp8' || c === 'h264' || c === 'vp9' || c === 'av1' || c === 'h265'),
    );
    if (join.room) this.setRoomInfo(join.room);
    this.handleParticipantUpdates(join.otherParticipants);
  }

  private setRoomInfo(room: ProtoRoom) {
    const previous = this.roomInfo;
    this.roomInfo = room;
    if (room.sid) this.sidWaiters.resolve(room.sid);
    if (previous && previous.metadata !== room.metadata) this.emitWhenConnected(RoomEvent.RoomMetadataChanged, room.metadata);
    if (previous && previous.activeRecording !== room.activeRecording) this.emitWhenConnected(RoomEvent.RecordingStatusChanged, room.activeRecording);
  }

  private findRemoteBySid(sid: string): RemoteParticipant | undefined {
    for (const p of this.remoteParticipants.values()) {
      if (p.sid === sid) return p;
    }
    return undefined;
  }

  private handleParticipantUpdates(infos: ParticipantInfo[]) {
    for (const info of infos) {
      if ((info.identity && info.identity === this.localParticipant.identity) || (info.sid && info.sid === this.localParticipant.sid)) {
        this.localParticipant.updateInfo(info);
        continue;
      }
      const identity = info.identity || this.findRemoteBySid(info.sid)?.identity;
      if (!identity) continue;
      const existing = this.remoteParticipants.get(identity);
      if (info.state === ParticipantInfo_State.DISCONNECTED) {
        // Clients match this on identity alone at their peril: a session replaced by a newer one under the
        // same identity still reports its own end, and that must not remove the newer one.
        if (existing && existing.sid === info.sid) this.removeParticipant(existing, info.disconnectReason as number as DisconnectReason);
        continue;
      }
      if (existing) {
        const previousSid = existing.sid;
        const applied = existing.updateInfo(info);
        if (applied && previousSid !== info.sid) log.debug('participant has a new session', identity);
      } else {
        const created = this.createRemoteParticipant(info);
        this.emitWhenConnected(RoomEvent.ParticipantConnected, created);
        created.updateInfo(info);
      }
    }
    this.deliverPendingMedia();
  }

  private createRemoteParticipant(info: ParticipantInfo): RemoteParticipant {
    const p = new RemoteParticipant(() => this.engine?.signal, () => this.connectOptions.autoSubscribe, info.sid, info.identity);
    this.remoteParticipants.set(info.identity, p);
    // Whoever arrives plays through the speaker this client chose, not only those who were here when it did.
    const output = this.activeDevices.get('audiooutput');
    if (output) void p.setAudioOutput(output);
    this.forwardParticipantEvents(p);
    p.on(ParticipantEvent.TrackPublished, (pub) => this.emitWhenConnected(RoomEvent.TrackPublished, pub, p));
    p.on(ParticipantEvent.TrackUnpublished, (pub) => this.emitWhenConnected(RoomEvent.TrackUnpublished, pub, p));
    p.on(ParticipantEvent.TrackSubscribed, (track, pub) => {
      this.watchPlayback(track);
      this.emitWhenConnected(RoomEvent.TrackSubscribed, track, pub, p);
    });
    p.on(ParticipantEvent.TrackUnsubscribed, (track, pub) => this.emitWhenConnected(RoomEvent.TrackUnsubscribed, track, pub, p));
    p.on(ParticipantEvent.TrackStreamStateChanged, (pub, state) => this.emitWhenConnected(RoomEvent.TrackStreamStateChanged, pub, state, p));
    p.on(ParticipantEvent.TrackSubscriptionPermissionChanged, (pub, status) => this.emitWhenConnected(RoomEvent.TrackSubscriptionPermissionChanged, pub, status, p));
    p.on(ParticipantEvent.TrackSubscriptionStatusChanged, (pub, status) => this.emitWhenConnected(RoomEvent.TrackSubscriptionStatusChanged, pub, status, p));
    return p;
  }

  /** Events every participant shares. Those of others wait for the room to be connected. */
  private forwardParticipantEvents(p: Participant) {
    const send: typeof this.emitWhenConnected = p instanceof RemoteParticipant ? (e, ...a) => this.emitWhenConnected(e, ...a) : (e, ...a) => void this.emit(e, ...a);
    const on = <K extends keyof ParticipantEventCallbacks>(event: K, fn: ParticipantEventCallbacks[K]) => p.on(event, fn);
    on(ParticipantEvent.TrackMuted, (pub) => send(RoomEvent.TrackMuted, pub, p));
    on(ParticipantEvent.TrackUnmuted, (pub) => send(RoomEvent.TrackUnmuted, pub, p));
    on(ParticipantEvent.ParticipantMetadataChanged, (metadata) => send(RoomEvent.ParticipantMetadataChanged, metadata, p));
    on(ParticipantEvent.ParticipantNameChanged, (name) => send(RoomEvent.ParticipantNameChanged, name, p));
    on(ParticipantEvent.AttributesChanged, (changed) => send(RoomEvent.ParticipantAttributesChanged, changed, p));
    on(ParticipantEvent.ConnectionQualityChanged, (quality) => send(RoomEvent.ConnectionQualityChanged, quality, p));
    on(ParticipantEvent.ParticipantPermissionsChanged, (previous) => send(RoomEvent.ParticipantPermissionsChanged, previous, p));
    on(ParticipantEvent.Active, () => send(RoomEvent.ParticipantActive, p));
  }

  private watchPlayback(track: RemoteTrack) {
    if (track instanceof RemoteAudioTrack) {
      track.on(TrackEvent.AudioPlaybackStarted, () => this.setAudioPlayback(true));
      track.on(TrackEvent.AudioPlaybackFailed, () => this.setAudioPlayback(false));
    } else {
      track.on(TrackEvent.VideoPlaybackStarted, () => this.setVideoPlayback(true));
      track.on(TrackEvent.VideoPlaybackFailed, () => this.setVideoPlayback(false));
    }
  }

  private setAudioPlayback(playing: boolean) {
    if (this.audioEnabled === playing) return;
    this.audioEnabled = playing;
    this.emit(RoomEvent.AudioPlaybackStatusChanged, playing);
  }

  private setVideoPlayback(playing: boolean) {
    if (this.videoEnabled === playing) return;
    this.videoEnabled = playing;
    this.emit(RoomEvent.VideoPlaybackStatusChanged, playing);
  }

  /** Starts audio the browser held back, waiting for a user gesture. Call it from a click handler. */
  async startAudio(): Promise<void> {
    await Promise.all(this.attachedElements(TrackKind.Audio).map((el) => el.play()));
    this.setAudioPlayback(true);
  }

  async startVideo(): Promise<void> {
    await Promise.all(this.attachedElements(TrackKind.Video).map((el) => el.play()));
    this.setVideoPlayback(true);
  }

  private attachedElements(kind: TrackKind): HTMLMediaElement[] {
    const out: HTMLMediaElement[] = [];
    for (const p of this.remoteParticipants.values()) {
      for (const pub of p.getTrackPublications()) {
        if (pub.kind === kind && pub.track) out.push(...pub.track.attachedElements);
      }
    }
    return out;
  }

  private removeParticipant(p: RemoteParticipant, reason?: DisconnectReason) {
    this.remoteParticipants.delete(p.identity);
    for (const pub of p.getTrackPublications()) p.unpublishTrack(pub.trackSid);
    this.activeSpeakers = this.activeSpeakers.filter((s) => s !== p);
    this.emitWhenConnected(RoomEvent.ParticipantDisconnected, p, reason === DisconnectReason.UNKNOWN_REASON ? undefined : reason);
  }

  private onMediaTrackAdded(mediaTrack: MediaStreamTrack, stream: MediaStream, receiver: RTCRtpReceiver) {
    if (this.state === ConnectionState.Disconnected || mediaTrack.readyState === 'ended') return;
    const [participantSid, rest] = unpackStreamId(stream.id);
    if (participantSid === this.localParticipant.sid) {
      log.warn('server sent this client its own track');
      return;
    }
    const mid = this.engine?.subscriber?.pc.getTransceivers().find((t) => t.receiver === receiver)?.mid;
    const trackSid = resolveTrackSid(rest, mediaTrack.id, mid ? this.engine?.midToTrackId[mid] : undefined);
    const entry: PendingMedia = {
      mediaTrack,
      stream,
      receiver,
      participantSid,
      trackSid,
      reported: false,
      onRemoved: (ev) => {
        if (ev.track === mediaTrack) this.dropPendingMedia(entry);
      },
    };
    stream.addEventListener('removetrack', entry.onRemoved);
    this.pendingMedia.push(entry);
    this.deliverPendingMedia();
  }

  /**
   * Hands media to its publication, if that is known yet. Media and the updates describing it travel on
   * separate paths, and either can arrive first. Whatever is waiting gets another try each time an update
   * arrives. If nothing has identified it for a while the application hears, but a late update can still
   * deliver it: the media is real, and there is no reason to lose it.
   */
  private deliverPendingMedia() {
    if (this.state === ConnectionState.Connecting || this.state === ConnectionState.Reconnecting || this.state === ConnectionState.Disconnected) return;
    const adaptive = this.adaptiveSettings();
    for (const entry of [...this.pendingMedia]) {
      const participant = this.findRemoteBySid(entry.participantSid);
      if (participant && entry.trackSid && participant.addSubscribedMediaTrack(entry.mediaTrack, entry.trackSid, entry.stream, entry.receiver, adaptive)) {
        this.dropPendingMedia(entry);
      } else if (!entry.timer && !entry.reported) {
        entry.timer = setTimeout(() => {
          entry.timer = undefined;
          entry.reported = true;
          log.warn('received media that nothing identifies', entry.participantSid, entry.trackSid);
          const p = this.findRemoteBySid(entry.participantSid);
          this.emitWhenConnected(RoomEvent.TrackSubscriptionFailed, entry.trackSid ?? entry.mediaTrack.id, p, undefined);
          if (entry.trackSid) p?.emit(ParticipantEvent.TrackSubscriptionFailed, entry.trackSid);
        }, MEDIA_WITHOUT_METADATA_MS);
      }
    }
  }

  private dropPendingMedia(entry: PendingMedia) {
    clearTimeout(entry.timer);
    entry.stream.removeEventListener('removetrack', entry.onRemoved);
    this.pendingMedia = this.pendingMedia.filter((e) => e !== entry);
  }

  private clearPendingMedia() {
    for (const entry of [...this.pendingMedia]) this.dropPendingMedia(entry);
  }

  private adaptiveSettings(): AdaptiveStreamSettings | undefined {
    const a = this.options.adaptiveStream;
    if (!a) return undefined;
    return a === true ? {} : a;
  }

  private onSubscriptionError(trackSid: string, reason: SubscriptionError) {
    for (const entry of this.pendingMedia.filter((e) => e.trackSid === trackSid)) this.dropPendingMedia(entry);
    let owner: RemoteParticipant | undefined;
    for (const p of this.remoteParticipants.values()) {
      if (p.trackPublications.has(trackSid)) owner = p;
    }
    this.emitWhenConnected(RoomEvent.TrackSubscriptionFailed, trackSid, owner, reason);
    owner?.emit(ParticipantEvent.TrackSubscriptionFailed, trackSid, reason);
  }

  /** Speaker updates over signalling are changes only: each names who changed, and everyone else stays. */
  private handleSpeakerDeltas(speakers: SpeakerInfo[]) {
    const current = new Map<string, Participant>(this.activeSpeakers.map((p) => [p.sid, p]));
    for (const s of speakers) {
      const p = s.sid === this.localParticipant.sid ? this.localParticipant : this.findRemoteBySid(s.sid);
      if (!p) continue;
      p.audioLevel = s.level;
      p.setIsSpeaking(s.active);
      if (s.active) current.set(s.sid, p);
      else current.delete(s.sid);
    }
    this.activeSpeakers = [...current.values()].sort((a, b) => b.audioLevel - a.audioLevel);
    this.emitWhenConnected(RoomEvent.ActiveSpeakersChanged, this.activeSpeakers);
  }

  /** Speaker updates over a data channel are the whole list: whoever it leaves out has stopped. */
  private handleSpeakerList(speakers: SpeakerInfo[]) {
    const active: Participant[] = [];
    const named = new Set<string>();
    for (const s of speakers) {
      const p = s.sid === this.localParticipant.sid ? this.localParticipant : this.findRemoteBySid(s.sid);
      if (!p) continue;
      p.audioLevel = s.level;
      p.setIsSpeaking(true);
      named.add(s.sid);
      active.push(p);
    }
    for (const p of [this.localParticipant, ...this.remoteParticipants.values()]) {
      if (!named.has(p.sid)) {
        p.audioLevel = 0;
        p.setIsSpeaking(false);
      }
    }
    this.activeSpeakers = active;
    this.emitWhenConnected(RoomEvent.ActiveSpeakersChanged, active);
  }

  private handleDataPacket(packet: DataPacket, kind: DataPacketKind) {
    switch (packet.value.case) {
      case 'user': {
        const user = packet.value.value;
        const participant = this.remoteParticipants.get(packet.participantIdentity);
        this.emit(RoomEvent.DataReceived, user.payload, participant, kind, user.topic, undefined, packet.participantIdentity || undefined);
        participant?.emit(ParticipantEvent.DataReceived, user.payload, kind);
        break;
      }
      case 'speaker':
        this.handleSpeakerList(packet.value.value.speakers);
        break;
      default:
        log.debug('unhandled data packet', packet.value.case);
    }
  }

  private syncState(): Pick<SyncState, 'subscription' | 'publishTracks' | 'trackSidsDisabled'> {
    const auto = this.connectOptions.autoSubscribe;
    const named: string[] = [];
    const disabled: string[] = [];
    for (const p of this.remoteParticipants.values()) {
      for (const pub of p.getTrackPublications()) {
        // With auto-subscribe on, what needs saying is what this client does not want, and vice versa.
        if (pub.isDesired !== auto) named.push(pub.trackSid);
        if (!pub.isEnabled) disabled.push(pub.trackSid);
      }
    }
    return {
      subscription: create(UpdateSubscriptionSchema, { trackSids: named, subscribe: !auto }),
      publishTracks: [...this.localParticipant.trackPublications.values()]
        .filter((pub) => pub.track && pub.trackInfo)
        .map((pub) => create(TrackPublishedResponseSchema, { cid: (pub as LocalTrackPublication).track!.cid, track: pub.trackInfo })),
      trackSidsDisabled: disabled,
    };
  }

  private onResumed() {
    // Whatever was asked of the server while the connection was down may not have reached it.
    for (const p of this.remoteParticipants.values()) {
      for (const pub of p.getTrackPublications()) {
        if (pub.hasSettingsToSend) p.sendTrackSettings(pub);
      }
    }
    this.finishReconnect();
  }

  /**
   * The application hears the connection is back, then what happened meanwhile, then any media that was
   * waiting. In that order, because after a full reconnect the same tracks return: if their new subscriptions
   * came first, the old ones ending would look like the new ones going away.
   */
  private finishReconnect() {
    this.setState(ConnectionState.Connected);
    this.emit(RoomEvent.Reconnected);
    this.flushBufferedEvents();
    this.deliverPendingMedia();
  }

  private onRestarting() {
    if (this.state !== ConnectionState.Reconnecting) {
      this.setState(ConnectionState.Reconnecting);
      this.emit(RoomEvent.Reconnecting);
    }
    this.clearPendingMedia();
    for (const p of [...this.remoteParticipants.values()]) this.removeParticipant(p);
    this.activeSpeakers = [];
  }

  private async onSignalRestarted(engine: RTCEngine, join: JoinResponse) {
    const generation = ++this.restartGeneration;
    const restarted = new Promise<void>((resolve) => engine.once('restarted', () => resolve()));
    try {
      this.applyJoinResponse(join);
    } catch (e) {
      log.error('could not apply the new session', e);
      return;
    }
    await this.localParticipant.republishAllTracks();
    await restarted;
    // A later failure may have started all this again by the time it finishes.
    if (generation !== this.restartGeneration) return;
    this.finishReconnect();
  }

  getActiveDevice(kind: MediaDeviceKind): string | undefined {
    return this.activeDevices.get(kind);
  }

  /**
   * Switches a capture or playback device. A published track moves to the device without a renegotiation,
   * and anything published later uses it too. With exact, failure to get that device is an error rather than
   * a silent choice of another.
   */
  async switchActiveDevice(kind: MediaDeviceKind, deviceId: string, exact = true): Promise<boolean> {
    const constraint: ConstrainDOMString = exact && deviceId !== 'default' ? { exact: deviceId } : deviceId;
    let ok = true;
    if (kind === 'audioinput' || kind === 'videoinput') {
      const pubs = kind === 'audioinput' ? this.localParticipant.audioTrackPublications : this.localParticipant.videoTrackPublications;
      const source = kind === 'audioinput' ? TrackSource.Microphone : TrackSource.Camera;
      for (const pub of pubs.values()) {
        if (pub.source === source && pub.track) ok = (await pub.track.setDeviceId(constraint)) && ok;
      }
      this.localParticipant.setDefaultDevice(kind, constraint);
    } else {
      await Promise.all([...this.remoteParticipants.values()].map((p) => p.setAudioOutput(deviceId)));
    }
    this.activeDevices.set(kind, deviceId);
    this.emit(RoomEvent.ActiveDeviceChanged, kind, deviceId);
    return ok;
  }

  /** Forces a recovery path, to test what an application does during one. */
  simulateScenario(scenario: 'signal-reconnect' | 'full-reconnect' | 'leave-full-reconnect' | 'server-leave' | 'node-failure') {
    const engine = this.engine;
    if (!engine) return;
    const send = (value: Parameters<typeof create<typeof SimulateScenarioSchema>>[1]) => engine.signal.sendSimulate(create(SimulateScenarioSchema, value));
    switch (scenario) {
      case 'signal-reconnect':
        engine.simulateSignalLoss();
        break;
      case 'full-reconnect':
        engine.forceFullReconnect();
        break;
      case 'leave-full-reconnect':
        send({ scenario: { case: 'leaveRequestFullReconnect', value: true } });
        break;
      case 'server-leave':
        send({ scenario: { case: 'serverLeave', value: true } });
        break;
      case 'node-failure':
        send({ scenario: { case: 'nodeFailure', value: true } });
        break;
    }
  }
}

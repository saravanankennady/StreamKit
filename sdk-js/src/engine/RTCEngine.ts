import { create } from '@bufbuild/protobuf';
import { DataPacketKind } from '../enums.js';
import {
  ConnectionError,
  ConnectionErrorReason,
  NegotiationError,
  PublishTrackError,
  SignalReconnectError,
  TrackInvalidError,
  UnexpectedConnectionState,
} from '../errors.js';
import { log } from '../logger.js';
import {
  ClientConfigSetting,
  ConnectionQuality as ProtoConnectionQuality,
  type DataPacket,
  type Room as ProtoRoom,
  ReconnectReason,
  type TrackInfo,
} from '../proto/livekit_models_pb.js';
import {
  type AddTrackRequest,
  type ConnectionQualityUpdate,
  type ICEServer,
  type JoinResponse,
  type LeaveRequest,
  LeaveRequest_Action,
  type MuteTrackRequest,
  type ParticipantUpdate,
  type ReconnectResponse,
  type RequestResponse,
  RequestResponse_Reason,
  type SessionDescription,
  SessionDescriptionSchema,
  SignalTarget,
  type SpeakersChanged,
  type StreamStateUpdate,
  type SubscribedQualityUpdate,
  type SubscriptionPermissionUpdate,
  type SubscriptionResponse,
  type SyncState,
  SyncStateSchema,
  type TrackPublishedResponse,
} from '../proto/livekit_rtc_pb.js';
import { SignalClient, SignalState } from '../signal/SignalClient.js';
import type { SignalOptions } from '../signal/url.js';
import { sleep } from '../util/async.js';
import { TypedEmitter } from '../util/emitter.js';
import { DataChannels } from './DataChannels.js';
import { PCTransport } from './PCTransport.js';
import { DefaultReconnectPolicy, type ReconnectPolicy } from './reconnectPolicy.js';

const ADD_TRACK_TIMEOUT_MS = 10_000;
const LOST_QUALITY_RECONNECT_MS = 10_000;
const STALLED_CONNECTION_MS = 3000;
// Connection state can read connected for a moment after the path has gone, so a check too soon proves
// nothing about whether the connection survived.
const MIN_RECONNECT_WAIT_MS = 2000;
const LEAVE_RECONNECT = 'leave-reconnect';

export interface EngineOptions {
  signal: SignalOptions;
  peerConnectionTimeout: number;
  websocketTimeout: number;
  rtcConfig?: RTCConfiguration;
  reconnectPolicy?: ReconnectPolicy;
}

type EngineEvents = {
  /** There is a live session to send requests to: after a join, a resume or a restart's new join. */
  signalReady: () => void;
  /** The session is over, for whatever reason. Nothing waiting for it will ever finish. */
  closing: () => void;
  disconnected: (reason?: number) => void;
  /** A resume is starting: the socket is lost, and media may well still be flowing. */
  resuming: () => void;
  signalResumed: (res: ReconnectResponse) => void;
  resumed: () => void;
  /** A full reconnect is starting. The session is gone, and everything local about it is out of date. */
  restarting: () => void;
  signalRestarted: (join: JoinResponse) => void;
  restarted: () => void;
  mediaTrackAdded: (track: MediaStreamTrack, stream: MediaStream, receiver: RTCRtpReceiver) => void;
  dataPacket: (packet: DataPacket, kind: DataPacketKind) => void;
  bufferStatus: (isLow: boolean, kind: DataPacketKind) => void;
  participantUpdate: (update: ParticipantUpdate) => void;
  speakersChanged: (update: SpeakersChanged) => void;
  roomUpdate: (room: ProtoRoom) => void;
  connectionQuality: (update: ConnectionQualityUpdate) => void;
  streamStateUpdate: (update: StreamStateUpdate) => void;
  subscribedQualityUpdate: (update: SubscribedQualityUpdate) => void;
  subscriptionPermissionUpdate: (update: SubscriptionPermissionUpdate) => void;
  subscriptionError: (res: SubscriptionResponse) => void;
  requestResponse: (res: RequestResponse) => void;
  trackPublished: (res: TrackPublishedResponse) => void;
  trackUnpublished: (trackSid: string) => void;
  trackSubscribed: (trackSid: string) => void;
  muteTrack: (req: MuteTrackRequest) => void;
  /** The publisher answered an offer. Browsers reset encoder state when that happens. */
  publisherAnswered: () => void;
};

interface PendingTrack {
  resolve: (info: TrackInfo) => void;
  reject: (reason: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
}

/**
 * The session underneath a room: one signal connection and two peer connections, and the logic that
 * brings them back when they fail. Everything above it deals in participants and tracks.
 */
export class RTCEngine extends TypedEmitter<EngineEvents> {
  readonly signal: SignalClient;
  readonly data: DataChannels;
  publisher?: PCTransport;
  subscriber?: PCTransport;
  subscriberPrimary = false;
  /** The last token from the server. It outlives the one the application passed. */
  token?: string;
  url?: string;
  joinResponse?: JoinResponse;
  participantSid?: string;
  midToTrackId: Record<string, string> = {};

  private options!: EngineOptions;
  private policy: ReconnectPolicy = new DefaultReconnectPolicy();
  private pendingTracks = new Map<string, PendingTrack>();
  private closed = true;
  private established = false;
  private sessionGone = false;
  private dataReleased = false;
  private publisherInUse = false;
  private resumeAllowed = true;
  private fullReconnectOnNext = false;
  private reconnectAttempts = 0;
  private reconnectStart = 0;
  private reconnectTimer?: ReturnType<typeof setTimeout>;
  private attemptingReconnect = false;
  private negotiationWaiters = new Set<(error: unknown) => void>();
  private lostQualityTimer?: ReturnType<typeof setTimeout>;
  private stalledTimers = new Map<PCTransport, ReturnType<typeof setTimeout>>();
  private latestSubscriberOffer?: SessionDescription;
  private latestSubscriberAnswer?: SessionDescription;

  /** Filled by the room: what the room knows goes in SyncState along with what the engine knows. */
  syncStateProvider?: () => Pick<SyncState, 'subscription' | 'publishTracks' | 'trackSidsDisabled'>;

  constructor() {
    super();
    this.signal = new SignalClient(() => this.options?.websocketTimeout ?? 15000);
    this.signal.handlers = {
      onClose: (reason) => this.handleDisconnect(`signal: ${reason}`, ReconnectReason.RR_SIGNAL_DISCONNECTED),
      onAnswer: (sd) => this.handleAnswer(sd),
      onOffer: (sd) => void this.handleOffer(sd),
      onTrickle: (c, target) => {
        const t = target === SignalTarget.PUBLISHER ? this.publisher : this.subscriber;
        void t?.addIceCandidate(c);
      },
      onParticipantUpdate: (u) => this.emit('participantUpdate', u),
      onTrackPublished: (res) => this.handleTrackPublished(res),
      onTrackUnpublished: (res) => this.emit('trackUnpublished', res.trackSid),
      onSpeakersChanged: (u) => this.emit('speakersChanged', u),
      onLeave: (leave) => this.handleLeave(leave),
      onMuteTrack: (req) => this.emit('muteTrack', req),
      onRoomUpdate: (room) => this.emit('roomUpdate', room),
      onConnectionQuality: (u) => this.handleConnectionQuality(u),
      onStreamStateUpdate: (u) => this.emit('streamStateUpdate', u),
      onSubscribedQualityUpdate: (u) => this.emit('subscribedQualityUpdate', u),
      onSubscriptionPermissionUpdate: (u) => this.emit('subscriptionPermissionUpdate', u),
      onSubscriptionError: (res) => this.emit('subscriptionError', res),
      onRefreshToken: (token) => {
        this.token = token;
      },
      onRequestResponse: (res) => this.handleRequestResponse(res),
      onTrackSubscribed: (res) => this.emit('trackSubscribed', res.trackSid),
    };
    this.data = new DataChannels({
      ensureReady: (kind) => this.ensureDataReady(kind),
      isReconnecting: () => this.holdingData,
      maxMessageSize: () => this.publisher?.pc.sctp?.maxMessageSize,
    });
    this.data.on('packet', (packet, kind) => this.emit('dataPacket', packet, kind));
    this.data.on('bufferStatus', (low, kind) => this.emit('bufferStatus', low, kind));
  }

  get isClosed() {
    return this.closed;
  }

  /** True while a reconnect is running, or about to. */
  get isReconnecting() {
    return this.attemptingReconnect || this.reconnectTimer !== undefined;
  }

  // Data goes back to the normal path just before what was held is sent. It lines up behind that. Anything
  // held after this point would have nothing coming along to send it.
  private get holdingData() {
    return this.isReconnecting && !this.dataReleased;
  }

  /** Connects the signal connection and starts the session. The peer connections are still coming up. */
  async join(url: string, token: string, options: EngineOptions, abort?: AbortSignal): Promise<JoinResponse> {
    this.url = url;
    this.token = token;
    this.options = options;
    this.policy = options.reconnectPolicy ?? this.policy;
    this.closed = false;
    try {
      const join = await this.connectSignal(abort);
      this.emit('signalReady');
      return join;
    } catch (e) {
      this.closed = true;
      throw e;
    }
  }

  private connectSignal(abort?: AbortSignal): Promise<JoinResponse> {
    return this.signal.join(this.url!, this.token!, this.options.signal, (join) => this.configure(join), abort);
  }

  private configure(join: JoinResponse) {
    this.joinResponse = join;
    this.subscriberPrimary = join.subscriberPrimary;
    this.participantSid = join.participant?.sid;
    this.resumeAllowed = join.clientConfiguration?.resumeConnection !== ClientConfigSetting.DISABLED;

    const config = this.rtcConfiguration(join.iceServers, join.clientConfiguration?.forceRelay === ClientConfigSetting.ENABLED);
    const publisher = new PCTransport(config);
    const subscriber = new PCTransport(config);
    this.publisher = publisher;
    this.subscriber = subscriber;

    publisher.on('offer', (offer, id) => this.signal.sendOffer(offer, id));
    publisher.on('iceCandidate', (c) => this.signal.sendIceCandidate(c, SignalTarget.PUBLISHER));
    publisher.on('connectionStateChange', (s) => this.onTransportState(publisher, s));
    publisher.on('offerAnswered', () => this.emit('publisherAnswered'));
    publisher.on('negotiationFailed', (e) => this.onNegotiationFailed(e));
    publisher.on('dataChannel', (dc) => this.data.addReceiving(dc));

    subscriber.on('iceCandidate', (c) => this.signal.sendIceCandidate(c, SignalTarget.SUBSCRIBER));
    subscriber.on('connectionStateChange', (s) => this.onTransportState(subscriber, s));
    subscriber.on('dataChannel', (dc) => this.data.addReceiving(dc));
    subscriber.on('track', (ev) => {
      // Without a stream the track cannot be tied to a participant, so it could never be delivered.
      if (ev.streams.length === 0) {
        log.warn('ignoring a track that has no stream', ev.track.id);
        return;
      }
      this.emit('mediaTrackAdded', ev.track, ev.streams[0], ev.receiver);
    });

    // With a publisher primary nothing starts until this client offers. With fast publish the server
    // expects the connection early, so that the first publish does not have to wait for it.
    if (!join.subscriberPrimary || join.fastPublish) {
      void this.negotiate().catch((e) => log.warn('first publisher negotiation failed', e));
    }
  }

  private rtcConfiguration(iceServers: ICEServer[], forceRelay: boolean): RTCConfiguration {
    const fromServer = iceServers.map((s) => ({ urls: s.urls, username: s.username || undefined, credential: s.credential || undefined }));
    const config: RTCConfiguration = {
      bundlePolicy: 'max-bundle',
      rtcpMuxPolicy: 'require',
      ...this.options.rtcConfig,
      // Servers the application configures take priority over what the server suggests.
      iceServers: this.options.rtcConfig?.iceServers?.length ? this.options.rtcConfig.iceServers : fromServer,
    };
    if (forceRelay) config.iceTransportPolicy = 'relay';
    return config;
  }

  private get primary() {
    return this.subscriberPrimary ? this.subscriber : this.publisher;
  }

  /** Resolves when the connection the session depends on is up. This is when a room counts as connected. */
  async waitForPrimaryConnected(): Promise<void> {
    const primary = this.primary;
    if (!primary) throw new ConnectionError('no peer connection', ConnectionErrorReason.InternalError);
    try {
      await primary.waitForConnected(this.options.peerConnectionTimeout, 'peer connection');
      this.established = true;
    } catch (e) {
      throw new ConnectionError(
        `could not establish pc connection: ${e instanceof Error ? e.message : String(e)}`,
        ConnectionErrorReason.InternalError,
      );
    }
  }

  private onTransportState(t: PCTransport, state: RTCPeerConnectionState) {
    log.debug(t === this.publisher ? 'publisher' : 'subscriber', state);
    clearTimeout(this.stalledTimers.get(t));
    this.stalledTimers.delete(t);
    // Before the session is up, a failure is the concern of whoever is waiting for it to come up.
    if ((state !== 'failed' && state !== 'disconnected') || this.closed || !this.established) return;
    const relied = t === this.primary || (t === this.publisher && this.publisherInUse);
    if (!relied) return;
    const lost = () =>
      this.handleDisconnect(
        `${t === this.publisher ? 'publisher' : 'subscriber'} connection ${state}`,
        t === this.publisher ? ReconnectReason.RR_PUBLISHER_FAILED : ReconnectReason.RR_SUBSCRIBER_FAILED,
      );
    if (state === 'failed') {
      lost();
      return;
    }
    // Browsers take half a minute to call a connection failed, and a socket on a route that has gone takes 15 seconds to
    // notice. A device that has moved to another network will not come back by itself, so this does not wait that long.
    this.stalledTimers.set(t, setTimeout(lost, STALLED_CONNECTION_MS));
  }

  private handleAnswer(sd: SessionDescription) {
    this.publisher?.handleAnswer(sd.sdp, sd.id).catch((e) => this.onNegotiationFailed(e));
  }

  private async handleOffer(sd: SessionDescription) {
    const subscriber = this.subscriber;
    if (!subscriber) return;
    if (Object.keys(sd.midToTrackId).length > 0) this.midToTrackId = { ...sd.midToTrackId };
    try {
      const answer = await subscriber.handleOffer(sd.sdp);
      if (!answer) return;
      this.latestSubscriberOffer = sd;
      this.latestSubscriberAnswer = create(SessionDescriptionSchema, { type: 'answer', sdp: answer.sdp ?? '', id: sd.id });
      this.signal.sendAnswer(answer, sd.id);
    } catch (e) {
      // The server is left waiting for an answer that will never come, so only a new session can recover.
      log.error('could not apply offer from server', e);
      this.onNegotiationFailed(e);
    }
  }

  private onNegotiationFailed(e: unknown) {
    log.warn('negotiation failed', e);
    if (this.closed) return;
    this.fullReconnectOnNext = true;
    this.handleDisconnect('negotiation failed', ReconnectReason.RR_UNKNOWN);
  }

  /**
   * Starts a publisher negotiation, and resolves once the server has answered an offer that contains the
   * state as of this call. An answer to an earlier offer does not count, however close in time.
   */
  negotiate(): Promise<void> {
    const publisher = this.publisher;
    if (!publisher) return Promise.reject(new NegotiationError('no publisher connection'));
    this.publisherInUse = true;
    // Opening these along with the first media section saves a second negotiation at the first data send.
    this.data.ensureChannels(publisher.pc);
    return new Promise<void>((resolve, reject) => {
      const checkpoint = publisher.latestOfferId;
      const finish = (error?: unknown) => {
        clearTimeout(timer);
        publisher.off('offerAnswered', onAnswered);
        this.negotiationWaiters.delete(finish);
        if (error) reject(error);
        else resolve();
      };
      const onAnswered = (id: number) => {
        if (id > checkpoint) finish();
      };
      const timer = setTimeout(() => {
        finish(new NegotiationError('negotiation timed out'));
        this.onNegotiationFailed(new NegotiationError('negotiation timed out'));
      }, this.options.peerConnectionTimeout);
      this.negotiationWaiters.add(finish);
      publisher.on('offerAnswered', onAnswered);
      publisher.negotiate();
    });
  }

  addTrack(req: AddTrackRequest): Promise<TrackInfo> {
    if (this.pendingTracks.has(req.cid)) {
      return Promise.reject(new TrackInvalidError('a track with this ID is already being published'));
    }
    return new Promise<TrackInfo>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingTracks.delete(req.cid);
        reject(new TrackInvalidError('publishing timed out: no response from the server'));
      }, ADD_TRACK_TIMEOUT_MS);
      this.pendingTracks.set(req.cid, { resolve, reject, timer });
      this.signal.sendAddTrack(req);
    });
  }

  cancelPendingTrack(cid: string) {
    const pending = this.pendingTracks.get(cid);
    if (!pending) return;
    clearTimeout(pending.timer);
    this.pendingTracks.delete(cid);
  }

  private handleTrackPublished(res: TrackPublishedResponse) {
    const pending = this.pendingTracks.get(res.cid);
    if (!pending) {
      log.debug('track published response for an unknown track', res.cid);
    } else if (!res.track) {
      clearTimeout(pending.timer);
      this.pendingTracks.delete(res.cid);
      pending.reject(new TrackInvalidError('the server accepted the track but did not describe it'));
    } else {
      clearTimeout(pending.timer);
      this.pendingTracks.delete(res.cid);
      pending.resolve(res.track);
    }
    this.emit('trackPublished', res);
  }

  private handleRequestResponse(res: RequestResponse) {
    if (res.request.case === 'addTrack' && res.reason !== RequestResponse_Reason.OK) {
      const cid = res.request.value.cid;
      const pending = this.pendingTracks.get(cid);
      if (pending) {
        clearTimeout(pending.timer);
        this.pendingTracks.delete(cid);
        const status = res.reason === RequestResponse_Reason.NOT_ALLOWED ? 403 : 400;
        pending.reject(new PublishTrackError(res.message || 'the server refused the track', status));
      }
    }
    this.emit('requestResponse', res);
  }

  private handleLeave(leave: LeaveRequest) {
    // The action decides this. can_reconnect is obsolete: a server setting only that means disconnect.
    switch (leave.action) {
      case LeaveRequest_Action.DISCONNECT:
        log.info('server ended the session', leave.reason);
        void this.close(false).then(() => this.emit('disconnected', leave.reason));
        break;
      case LeaveRequest_Action.RESUME:
        this.handleDisconnect(LEAVE_RECONNECT, ReconnectReason.RR_UNKNOWN);
        break;
      case LeaveRequest_Action.RECONNECT:
        this.fullReconnectOnNext = true;
        this.handleDisconnect(LEAVE_RECONNECT, ReconnectReason.RR_UNKNOWN);
        break;
    }
  }

  private handleConnectionQuality(update: ConnectionQualityUpdate) {
    const mine = update.updates.find((u) => u.participantSid === this.participantSid);
    if (mine) {
      // Lost from the server's side means it has stopped hearing from this publisher, whatever the
      // connection state reads here. Waiting a while keeps a short bad patch from costing a new session.
      if (mine.quality === ProtoConnectionQuality.LOST) {
        if (!this.lostQualityTimer) {
          this.lostQualityTimer = setTimeout(() => {
            this.lostQualityTimer = undefined;
            const sending = this.publisher?.pc.getSenders().some((s) => s.track);
            if (sending && this.primary?.isConnected && !this.isReconnecting) {
              log.warn('server has heard nothing from this publisher, reconnecting');
              this.fullReconnectOnNext = true;
              this.handleDisconnect('connection quality lost', ReconnectReason.RR_UNKNOWN);
            }
          }, LOST_QUALITY_RECONNECT_MS);
        }
      } else {
        clearTimeout(this.lostQualityTimer);
        this.lostQualityTimer = undefined;
      }
    }
    this.emit('connectionQuality', update);
  }

  /** Publisher connected and the channel open, opening what is missing. Used before a data send. */
  private async ensureDataReady(kind: DataPacketKind): Promise<RTCDataChannel> {
    const publisher = this.publisher;
    if (!publisher) throw new ConnectionError('no publisher connection', ConnectionErrorReason.InternalError);
    let dc = this.data.channel(kind);
    if (dc?.readyState === 'open' && publisher.isConnected) return dc;

    const timeout = this.options.peerConnectionTimeout;
    if (!dc || dc.readyState === 'closed' || dc.readyState === 'closing' || !publisher.hasLocalDescription) {
      this.data.ensureChannels(publisher.pc);
      dc = this.data.channel(kind);
      void this.negotiate().catch((e) => log.warn('negotiation for a data channel failed', e));
    }
    const channel = dc!;
    const started = Date.now();
    await publisher.waitForConnected(timeout, 'publisher connection').catch((e) => {
      throw new ConnectionError(`could not send data: ${e.message}`, ConnectionErrorReason.InternalError);
    });
    if (channel.readyState !== 'open') {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(
          () => reject(new ConnectionError(`could not send data: channel ${channel.label} did not open`, ConnectionErrorReason.InternalError)),
          Math.max(0, timeout - (Date.now() - started)),
        );
        channel.addEventListener('open', () => {
          clearTimeout(timer);
          resolve();
        }, { once: true });
        channel.addEventListener('close', () => {
          clearTimeout(timer);
          reject(new ConnectionError(`could not send data: channel ${channel.label} closed`, ConnectionErrorReason.InternalError));
        }, { once: true });
      });
    }
    return channel;
  }

  sendDataPacket(packet: DataPacket, kind: DataPacketKind): Promise<void> {
    return this.data.send(packet, kind);
  }

  /**
   * The one way into reconnecting, whatever went wrong. Only the first report of an outage schedules
   * anything: a lost network typically closes the socket and fails both connections at about the same time.
   */
  private handleDisconnect(connection: string, reason: ReconnectReason) {
    if (this.closed || this.attemptingReconnect || this.reconnectTimer) return;
    log.info('connection lost', connection);
    if (!this.reconnectStart) this.reconnectStart = Date.now();
    const delay = this.policy.nextRetryDelayInMs({
      retryCount: connection === LEAVE_RECONNECT ? 0 : this.reconnectAttempts,
      elapsedMs: Date.now() - this.reconnectStart,
    });
    if (delay === null) {
      log.warn('giving up on reconnecting');
      void this.close(false).then(() => this.emit('disconnected', undefined));
      return;
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = undefined;
      void this.attemptReconnect(reason);
    }, delay);
  }

  private async attemptReconnect(reason: ReconnectReason) {
    if (this.closed) return;
    this.attemptingReconnect = true;
    this.dataReleased = false;
    try {
      const canResume = this.resumeAllowed && !this.fullReconnectOnNext && !!this.publisher && !!this.subscriber;
      if (canResume) await this.resumeConnection(reason);
      else await this.restartConnection();
      this.reconnectAttempts = 0;
      this.reconnectStart = 0;
      this.fullReconnectOnNext = false;
      this.attemptingReconnect = false;
    } catch (e) {
      this.attemptingReconnect = false;
      if (this.closed) return;
      this.reconnectAttempts++;
      log.warn('reconnect attempt failed', e);
      if (e instanceof UnexpectedConnectionState) {
        await this.close(false);
        this.emit('disconnected', undefined);
        return;
      }
      // Only a failure of the socket alone leaves the session worth resuming.
      if (!(e instanceof SignalReconnectError)) this.fullReconnectOnNext = true;
      this.handleDisconnect('retrying', ReconnectReason.RR_UNKNOWN);
    }
  }

  private async resumeConnection(reason: ReconnectReason) {
    if (!this.url || !this.token) throw new UnexpectedConnectionState('no session to resume');
    this.emit('resuming');
    let res: ReconnectResponse;
    try {
      res = await this.signal.reconnect(this.url, this.token, this.participantSid, reason, this.options.signal);
    } catch (e) {
      if (e instanceof ConnectionError) {
        if (e.reason === ConnectionErrorReason.NotAllowed) throw new UnexpectedConnectionState(e.message);
        if (e.reason === ConnectionErrorReason.LeaveRequest) throw e;
      }
      throw new SignalReconnectError(e instanceof Error ? e.message : String(e));
    }
    if (this.closed) return;

    this.sendSyncState();
    this.emit('signalResumed', res);

    if (res.iceServers.length > 0 || res.clientConfiguration) {
      const config = this.rtcConfiguration(res.iceServers.length ? res.iceServers : (this.joinResponse?.iceServers ?? []), res.clientConfiguration?.forceRelay === ClientConfigSetting.ENABLED);
      this.publisher?.pc.setConfiguration(config);
      this.subscriber?.pc.setConfiguration(config);
    }
    // The subscriber is not restarted from here: its offers come from the server, which does that itself. A publisher still
    // connected has only lost its signalling. Restarting ICE would stop its media for a second and drop its bandwidth estimate.
    if (this.publisherInUse && this.publisher) {
      await this.publisher.createAndSendOffer({ iceRestart: this.publisher.pc.connectionState !== 'connected' });
    }

    await this.waitForTransports();
    if (this.signal.state === SignalState.Disconnected) throw new SignalReconnectError('signal connection lost during resume');
    this.signal.setReconnected();
    this.emit('signalReady');
    this.dataReleased = true;
    await this.data.resendAfterResume(res.lastMessageSeq);
    this.emit('resumed');
  }

  private sendSyncState() {
    const state = create(SyncStateSchema, {
      ...this.syncStateProvider?.(),
      dataChannels: this.data.channelInfos(),
      datachannelReceiveStates: this.data.receiveStates(),
    });
    // Sent without IDs: this describes what the connection holds, not a step in an exchange.
    if (this.latestSubscriberOffer && this.subscriber?.pc.remoteDescription) {
      state.offer = create(SessionDescriptionSchema, { type: 'offer', sdp: this.subscriber.pc.remoteDescription.sdp });
    }
    if (this.latestSubscriberAnswer && this.subscriber?.pc.localDescription) {
      state.answer = create(SessionDescriptionSchema, { type: 'answer', sdp: this.subscriber.pc.localDescription.sdp });
    }
    this.signal.sendSyncState(state);
  }

  private async waitForTransports() {
    await sleep(MIN_RECONNECT_WAIT_MS);
    const timeout = this.options.peerConnectionTimeout;
    const waits: Promise<void>[] = [];
    if (this.primary) waits.push(this.primary.waitForConnected(timeout, 'primary connection', { failFast: false }));
    if (this.publisherInUse && this.publisher && this.publisher !== this.primary) {
      waits.push(this.publisher.waitForConnected(timeout, 'publisher connection', { failFast: false }));
    }
    await Promise.all(waits);
  }

  private async restartConnection() {
    if (!this.url || !this.token) throw new UnexpectedConnectionState('no session to restart');
    this.emit('restarting');
    this.sessionGone = true;
    if (this.signal.isConnected) this.signal.sendLeave();
    await this.teardownSession();
    this.resumeAllowed = true;

    let join: JoinResponse;
    try {
      join = await this.connectSignal();
    } catch (e) {
      if (e instanceof ConnectionError && e.reason === ConnectionErrorReason.NotAllowed) {
        throw new UnexpectedConnectionState(e.message);
      }
      throw e;
    }
    if (this.closed) return;
    this.sessionGone = false;
    this.emit('signalReady');
    this.emit('signalRestarted', join);
    await this.waitForTransports();
    this.signal.setReconnected();
    this.emit('restarted');
    this.dataReleased = true;
    await this.data.sendCarriedOver();
  }

  /** Forgets the session on the wire, leaving what the application asked for ready to carry over. */
  private async teardownSession() {
    this.failNegotiationWaiters(new NegotiationError('connection restarting'));
    // Whoever is publishing tries again in the new session, as with any negotiation that did not finish.
    for (const [cid, pending] of this.pendingTracks) {
      clearTimeout(pending.timer);
      pending.reject(new NegotiationError('publishing interrupted by a reconnect'));
      this.pendingTracks.delete(cid);
    }
    this.data.resetForNewSession();
    this.closeTransports();
    this.latestSubscriberOffer = undefined;
    this.latestSubscriberAnswer = undefined;
    this.midToTrackId = {};
    this.publisherInUse = false;
    await this.signal.close();
  }

  private failNegotiationWaiters(error: unknown) {
    for (const fail of [...this.negotiationWaiters]) fail(error);
  }

  private closeTransports() {
    this.publisher?.close();
    this.subscriber?.close();
    this.publisher = undefined;
    this.subscriber = undefined;
  }

  /**
   * Waits until requests can go straight to a live session. A publish during an outage would otherwise
   * start its own timers against a server it cannot reach.
   */
  waitUntilReady(timeoutMs: number): Promise<void> {
    if (this.closed) return Promise.reject(new PublishTrackError('the connection has closed', 408));
    if (this.signal.isConnected && !this.reconnectTimer && !this.sessionGone) return Promise.resolve();
    return new Promise<void>((resolve, reject) => {
      const finish = (done: () => void) => {
        clearTimeout(timer);
        this.off('signalReady', onReady);
        this.off('closing', onClosed);
        done();
      };
      const onReady = () => finish(resolve);
      const onClosed = () => finish(() => reject(new PublishTrackError('the connection closed while waiting to publish', 408)));
      const timer = setTimeout(() => finish(() => reject(new PublishTrackError('the connection was not ready in time to publish', 408))), timeoutMs);
      this.on('signalReady', onReady);
      this.on('closing', onClosed);
    });
  }

  /** Forces the path a given failure would take. Only for testing recovery. */
  simulateSignalLoss() {
    this.signal.simulateSocketLoss();
  }

  forceFullReconnect() {
    this.fullReconnectOnNext = true;
    this.handleDisconnect('requested', ReconnectReason.RR_UNKNOWN);
  }

  /** Ends the session for good. With sendLeave, the server hears straight away rather than after a timeout. */
  async close(sendLeave = true) {
    if (this.closed) return;
    this.closed = true;
    this.established = false;
    this.sessionGone = false;
    this.emit('closing');
    clearTimeout(this.reconnectTimer);
    this.reconnectTimer = undefined;
    clearTimeout(this.lostQualityTimer);
    this.lostQualityTimer = undefined;
    for (const timer of this.stalledTimers.values()) clearTimeout(timer);
    this.stalledTimers.clear();
    this.reconnectAttempts = 0;
    this.reconnectStart = 0;
    this.fullReconnectOnNext = false;
    this.failNegotiationWaiters(new NegotiationError('connection closed'));
    for (const [, pending] of this.pendingTracks) {
      clearTimeout(pending.timer);
      pending.reject(new TrackInvalidError('connection closed'));
    }
    this.pendingTracks.clear();
    if (sendLeave) this.signal.sendLeave();
    this.data.close();
    this.closeTransports();
    await this.signal.close();
  }
}

import { create, fromBinary, fromJsonString, toBinary } from '@bufbuild/protobuf';
import { ConnectionError, ConnectionErrorReason } from '../errors.js';
import { log } from '../logger.js';
import {
  type AddTrackRequest,
  type ConnectionQualityUpdate,
  type JoinResponse,
  type LeaveRequest,
  LeaveRequest_Action,
  LeaveRequestSchema,
  type MuteTrackRequest,
  MuteTrackRequestSchema,
  type ParticipantUpdate,
  PingSchema,
  type ReconnectResponse,
  type RequestResponse,
  SessionDescriptionSchema,
  type SessionDescription,
  SignalRequestSchema,
  type SignalRequest,
  SignalResponseSchema,
  type SignalResponse,
  SignalTarget,
  type SimulateScenario,
  type SpeakersChanged,
  type StreamStateUpdate,
  type SubscribedQualityUpdate,
  type SubscriptionPermission,
  type SubscriptionPermissionUpdate,
  type SubscriptionResponse,
  type SyncState,
  type TrackPublishedResponse,
  type TrackSubscribed,
  type TrackUnpublishedResponse,
  TrickleRequestSchema,
  type UpdateParticipantMetadata,
  type UpdateSubscription,
  type UpdateTrackSettings,
} from '../proto/livekit_rtc_pb.js';
import { DisconnectReason as ProtoDisconnectReason, type Room as ProtoRoom } from '../proto/livekit_models_pb.js';
import type { SignalOptions } from './url.js';
import { signalUrl, validateUrl } from './url.js';
import { explainFailedUpgrade } from './validate.js';

/** Time allowed between the upgrade and the first message. The join path on a server is meant to be quick. */
const FIRST_MESSAGE_TIMEOUT_MS = 5000;
const CLOSE_WAIT_MS = 250;

export enum SignalState {
  Disconnected,
  Connecting,
  /** The socket may be open, but the session is not caught up yet: most requests wait. */
  Reconnecting,
  Connected,
}

export interface SignalHandlers {
  onClose(reason: string): void;
  onAnswer(sd: SessionDescription): void;
  onOffer(sd: SessionDescription): void;
  onTrickle(candidate: RTCIceCandidateInit, target: SignalTarget): void;
  onParticipantUpdate(update: ParticipantUpdate): void;
  onTrackPublished(res: TrackPublishedResponse): void;
  onTrackUnpublished(res: TrackUnpublishedResponse): void;
  onSpeakersChanged(update: SpeakersChanged): void;
  onLeave(leave: LeaveRequest): void;
  onMuteTrack(req: MuteTrackRequest): void;
  onRoomUpdate(room: ProtoRoom): void;
  onConnectionQuality(update: ConnectionQualityUpdate): void;
  onStreamStateUpdate(update: StreamStateUpdate): void;
  onSubscribedQualityUpdate(update: SubscribedQualityUpdate): void;
  onSubscriptionPermissionUpdate(update: SubscriptionPermissionUpdate): void;
  onSubscriptionError(res: SubscriptionResponse): void;
  onRefreshToken(token: string): void;
  onRequestResponse(res: RequestResponse): void;
  onTrackSubscribed(res: TrackSubscribed): void;
}

type RequestMessage = SignalRequest['message'];
type RequestCase = NonNullable<RequestMessage['case']>;

/**
 * These overtake requests held during a resume. The first group re-establishes the session, so anything
 * held back behind them would wait forever. Leave has to get through whatever the state.
 */
const PASS_THROUGH: ReadonlySet<RequestCase> = new Set<RequestCase>([
  'syncState',
  'trickle',
  'offer',
  'answer',
  'simulate',
  'leave',
]);

interface Pending<T> {
  resolve: (value: T) => void;
  reject: (reason: unknown) => void;
}

/**
 * The WebSocket half of a session. It owns the rules on the first message, keeps the connection alive
 * with pings, and holds requests back while a session is resuming.
 *
 * Exactly one socket is current at a time. Every socket callback checks that it still is, because a
 * replaced socket's events can arrive long after a new one took over.
 */
export class SignalClient {
  state = SignalState.Disconnected;
  /** Latest round trip measured from ping responses, in milliseconds. */
  rtt = 0;
  handlers: Partial<SignalHandlers> = {};

  private ws?: WebSocket;
  private queued: RequestMessage[] = [];
  private pingIntervalSeconds = 0;
  private pingTimeoutSeconds = 0;
  private pingTimer?: ReturnType<typeof setInterval>;
  private pingDeadline?: ReturnType<typeof setTimeout>;

  constructor(private readonly websocketTimeout: () => number = () => 15000) {}

  get isConnected() {
    return this.state === SignalState.Connected;
  }

  /**
   * Starts a new session. onJoin runs synchronously with the join message, so the peer connections exist
   * before the very next message, which is normally an offer, can be read.
   */
  join(url: string, token: string, opts: SignalOptions, onJoin: (join: JoinResponse) => void, abort?: AbortSignal): Promise<JoinResponse> {
    this.queued = [];
    this.state = SignalState.Connecting;
    return new Promise<JoinResponse>((resolve, reject) => {
      this.open(url, token, opts, abort, reject, (msg, settle) => {
        const m = msg.message;
        if (m.case === 'join') {
          this.pingTimeoutSeconds = m.value.pingTimeout;
          this.pingIntervalSeconds = m.value.pingInterval;
          onJoin(m.value);
          this.state = SignalState.Connected;
          this.startPing();
          settle();
          resolve(m.value);
          return;
        }
        settle();
        reject(firstMessageError(msg, 'did not receive join response'));
      });
    });
  }

  /** Reattaches a signalling connection to a session the server may still hold. */
  reconnect(url: string, token: string, sid: string | undefined, reason: number, opts: SignalOptions): Promise<ReconnectResponse> {
    this.state = SignalState.Reconnecting;
    const resumeOpts: SignalOptions = { ...opts, reconnect: true, sid, reconnectReason: reason };
    return new Promise<ReconnectResponse>((resolve, reject) => {
      this.open(url, token, resumeOpts, undefined, reject, (msg, settle) => {
        const m = msg.message;
        if (m.case === 'reconnect') {
          this.startPing();
          settle();
          resolve(m.value);
          return;
        }
        if (m.case === 'leave') {
          settle();
          reject(firstMessageError(msg, 'resume refused'));
          return;
        }
        // A resume accepts nothing else first, so the deadline keeps running.
        log.warn('dropping message ahead of the reconnect response', m.case);
      });
    });
  }

  /** Requests held during a resume go out once the peer connections are back. */
  setReconnected() {
    this.state = SignalState.Connected;
    const held = this.queued;
    this.queued = [];
    for (const m of held) this.write(m);
  }

  private open(
    url: string,
    token: string,
    opts: SignalOptions,
    abort: AbortSignal | undefined,
    reject: (e: unknown) => void,
    onFirstMessage: (msg: SignalResponse, settle: () => void) => void,
  ) {
    if (abort?.aborted) {
      reject(new ConnectionError('connection attempt cancelled', ConnectionErrorReason.Cancelled));
      return;
    }
    this.closeSocket();
    const ws = new WebSocket(signalUrl(url, token, opts));
    ws.binaryType = 'arraybuffer';
    this.ws = ws;

    let upgraded = false;
    let settled = false;
    let firstMessageTimer: ReturnType<typeof setTimeout> | undefined;

    const cleanup = () => {
      clearTimeout(upgradeTimer);
      clearTimeout(firstMessageTimer);
      abort?.removeEventListener('abort', onAbort);
    };
    const settle = () => {
      settled = true;
      cleanup();
    };
    const fail = (e: unknown) => {
      if (settled) return;
      settle();
      if (this.ws === ws) {
        this.ws = undefined;
        this.state = SignalState.Disconnected;
      }
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      ws.close();
      reject(e);
    };
    const onAbort = () => fail(new ConnectionError('connection attempt cancelled', ConnectionErrorReason.Cancelled));
    abort?.addEventListener('abort', onAbort);

    const upgradeTimer = setTimeout(() => {
      fail(new ConnectionError('could not establish signal connection: timed out', ConnectionErrorReason.Timeout));
    }, this.websocketTimeout());

    ws.onopen = () => {
      if (this.ws !== ws) return;
      upgraded = true;
      clearTimeout(upgradeTimer);
      firstMessageTimer = setTimeout(() => {
        fail(new ConnectionError('timed out waiting for the first message from the server', ConnectionErrorReason.Timeout));
      }, FIRST_MESSAGE_TIMEOUT_MS);
    };

    ws.onerror = () => {
      // Details never appear here for security reasons. The close event that follows does the work.
      log.debug('signal socket error');
    };

    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      if (!settled && !upgraded) {
        clearTimeout(upgradeTimer);
        void explainFailedUpgrade(validateUrl(url, token, opts)).then(fail);
        return;
      }
      if (!settled) {
        fail(new ConnectionError(`signal connection closed before the session started (code ${ev.code})`, ConnectionErrorReason.InternalError));
        return;
      }
      this.handleClosed(`socket closed (code ${ev.code})`);
    };

    ws.onmessage = (ev) => {
      if (this.ws !== ws) return;
      let msg: SignalResponse;
      try {
        msg = parseResponse(ev.data);
      } catch (e) {
        log.warn('could not read signal message', e);
        if (settled) this.handleClosed('unreadable message');
        else fail(new ConnectionError('could not read the response from the server', ConnectionErrorReason.InternalError));
        return;
      }
      if (!settled) {
        onFirstMessage(msg, settle);
        return;
      }
      this.resetPingDeadline();
      this.dispatch(msg);
    };
  }

  private dispatch(msg: SignalResponse) {
    const m = msg.message;
    const h = this.handlers;
    switch (m.case) {
      case 'answer':
        h.onAnswer?.(m.value);
        break;
      case 'offer':
        h.onOffer?.(m.value);
        break;
      case 'trickle': {
        let candidate: RTCIceCandidateInit;
        try {
          candidate = JSON.parse(m.value.candidateInit);
        } catch {
          // One bad candidate costs at most one network path. It is not worth losing the session for.
          log.warn('ignoring malformed ICE candidate');
          break;
        }
        h.onTrickle?.(candidate, m.value.target);
        break;
      }
      case 'update':
        h.onParticipantUpdate?.(m.value);
        break;
      case 'trackPublished':
        h.onTrackPublished?.(m.value);
        break;
      case 'trackUnpublished':
        h.onTrackUnpublished?.(m.value);
        break;
      case 'speakersChanged':
        h.onSpeakersChanged?.(m.value);
        break;
      case 'leave':
        h.onLeave?.(m.value);
        break;
      case 'mute':
        h.onMuteTrack?.(m.value);
        break;
      case 'roomUpdate':
        if (m.value.room) h.onRoomUpdate?.(m.value.room);
        break;
      case 'connectionQuality':
        h.onConnectionQuality?.(m.value);
        break;
      case 'streamStateUpdate':
        h.onStreamStateUpdate?.(m.value);
        break;
      case 'subscribedQualityUpdate':
        h.onSubscribedQualityUpdate?.(m.value);
        break;
      case 'subscriptionPermissionUpdate':
        h.onSubscriptionPermissionUpdate?.(m.value);
        break;
      case 'subscriptionResponse':
        h.onSubscriptionError?.(m.value);
        break;
      case 'refreshToken':
        h.onRefreshToken?.(m.value);
        break;
      case 'requestResponse':
        h.onRequestResponse?.(m.value);
        break;
      case 'trackSubscribed':
        h.onTrackSubscribed?.(m.value);
        break;
      case 'pongResp':
        this.rtt = Date.now() - Number(m.value.lastPingTimestamp);
        break;
      case 'pong':
        break;
      default:
        log.debug('unhandled signal message', m.case);
    }
  }

  private handleClosed(reason: string) {
    if (this.state === SignalState.Disconnected) return;
    this.closeSocket();
    this.state = SignalState.Disconnected;
    this.handlers.onClose?.(reason);
  }

  /** Forgets the current socket without reporting a loss. The session is unaffected. */
  private closeSocket() {
    this.stopPing();
    const ws = this.ws;
    this.ws = undefined;
    if (ws) {
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) ws.close();
    }
  }

  /** Closes for good. A queued leave message still reaches the server, since close flushes first. */
  async close() {
    this.stopPing();
    this.queued = [];
    this.state = SignalState.Disconnected;
    const ws = this.ws;
    this.ws = undefined;
    if (!ws) return;
    await new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, CLOSE_WAIT_MS);
      ws.onopen = ws.onmessage = ws.onerror = null;
      ws.onclose = () => {
        clearTimeout(timer);
        resolve();
      };
      ws.close();
    });
  }

  /** Drops the socket as a failed network would, without telling the server anything. For tests. */
  simulateSocketLoss() {
    this.ws?.close();
  }

  private startPing() {
    this.stopPing();
    if (this.pingTimeoutSeconds === 0 || this.pingIntervalSeconds === 0) {
      log.warn('server did not provide a ping schedule, so a dead connection will go unnoticed');
      return;
    }
    this.pingTimer = setInterval(() => this.sendPing(), this.pingIntervalSeconds * 1000);
    this.resetPingDeadline();
  }

  private stopPing() {
    clearInterval(this.pingTimer);
    clearTimeout(this.pingDeadline);
    this.pingTimer = undefined;
    this.pingDeadline = undefined;
  }

  // Any message proves the connection is alive, so all of them reset this, not only pongs.
  private resetPingDeadline() {
    if (!this.pingTimer) return;
    clearTimeout(this.pingDeadline);
    this.pingDeadline = setTimeout(() => {
      log.warn('signal connection stopped responding to pings');
      this.handleClosed('ping timeout');
    }, this.pingTimeoutSeconds * 1000);
  }

  private sendPing() {
    const now = BigInt(Date.now());
    // Servers answer each form. Older ones only know the first.
    this.write({ case: 'ping', value: now });
    this.write({ case: 'pingReq', value: create(PingSchema, { timestamp: now, rtt: BigInt(this.rtt) }) });
  }

  private write(message: RequestMessage) {
    const ws = this.ws;
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      log.debug('signal socket not open, dropping', message.case);
      return;
    }
    ws.send(toBinary(SignalRequestSchema, create(SignalRequestSchema, { message })));
  }

  private send(message: RequestMessage) {
    if (message.case && !PASS_THROUGH.has(message.case) && this.state !== SignalState.Connected) {
      this.queued.push(message);
      return;
    }
    this.write(message);
  }

  sendOffer(offer: RTCSessionDescriptionInit, id: number) {
    this.send({ case: 'offer', value: sessionDescription(offer, id) });
  }

  sendAnswer(answer: RTCSessionDescriptionInit, id: number) {
    this.send({ case: 'answer', value: sessionDescription(answer, id) });
  }

  sendIceCandidate(candidate: RTCIceCandidateInit, target: SignalTarget) {
    this.send({
      case: 'trickle',
      value: create(TrickleRequestSchema, { candidateInit: JSON.stringify(candidate), target }),
    });
  }

  sendAddTrack(req: AddTrackRequest) {
    this.send({ case: 'addTrack', value: req });
  }

  sendMuteTrack(sid: string, muted: boolean) {
    this.send({ case: 'mute', value: create(MuteTrackRequestSchema, { sid, muted }) });
  }

  sendUpdateSubscription(sub: UpdateSubscription) {
    this.send({ case: 'subscription', value: sub });
  }

  sendUpdateTrackSettings(settings: UpdateTrackSettings) {
    this.send({ case: 'trackSetting', value: settings });
  }

  sendSyncState(state: SyncState) {
    this.send({ case: 'syncState', value: state });
  }

  sendSubscriptionPermission(perm: SubscriptionPermission) {
    this.send({ case: 'subscriptionPermission', value: perm });
  }

  sendUpdateMetadata(update: UpdateParticipantMetadata) {
    this.send({ case: 'updateMetadata', value: update });
  }

  sendSimulate(scenario: SimulateScenario) {
    this.send({ case: 'simulate', value: scenario });
  }

  sendLeave() {
    this.send({
      case: 'leave',
      value: create(LeaveRequestSchema, {
        canReconnect: false,
        reason: ProtoDisconnectReason.CLIENT_INITIATED,
        action: LeaveRequest_Action.DISCONNECT,
      }),
    });
  }
}

function parseResponse(data: unknown): SignalResponse {
  if (typeof data === 'string') return fromJsonString(SignalResponseSchema, data, { ignoreUnknownFields: true });
  if (data instanceof ArrayBuffer) return fromBinary(SignalResponseSchema, new Uint8Array(data));
  throw new Error('unexpected message type');
}

function sessionDescription(sd: RTCSessionDescriptionInit, id: number): SessionDescription {
  return create(SessionDescriptionSchema, { type: sd.type, sdp: sd.sdp ?? '', id });
}

function firstMessageError(msg: SignalResponse, fallback: string): ConnectionError {
  if (msg.message.case === 'leave') {
    const reason = msg.message.value.reason as number;
    return new ConnectionError(
      `the server refused the connection: ${ProtoDisconnectReason[reason] ?? reason}`,
      ConnectionErrorReason.LeaveRequest,
      { disconnectReason: reason },
    );
  }
  return new ConnectionError(`${fallback}, got ${msg.message.case}`, ConnectionErrorReason.InternalError);
}

// A room participant, on werift. It speaks the two peer connection form of the protocol: the client publishes on one
// connection, and the server sends media and data to it on another.
import { randomUUID } from 'node:crypto';
import { EventEmitter } from 'node:events';
import os from 'node:os';
import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { WebSocket } from 'ws';
import { type MediaStreamTrack, RTCPeerConnection, RTCRtpCodecParameters } from 'werift';
import {
  ClientInfo_SDK,
  DataPacketSchema,
  DataPacket_Kind,
  DataStream_OperationType,
  DataStream_HeaderSchema,
  DataStream_ChunkSchema,
  DataStream_TextHeaderSchema,
  DataStream_TrailerSchema,
  DisconnectReason,
  ParticipantInfo_Kind,
  RpcAckSchema,
  RpcErrorSchema,
  RpcRequestSchema,
  RpcResponseSchema,
  ParticipantInfo_State,
  TrackSource,
  TrackType,
  type DataPacket,
  type ParticipantInfo,
  type TrackInfo,
} from '../proto/livekit_models_pb.js';
import {
  AddTrackRequestSchema,
  LeaveRequestSchema,
  LeaveRequest_Action,
  PingSchema,
  SessionDescriptionSchema,
  SignalRequestSchema,
  SignalResponseSchema,
  SignalTarget,
  UpdateParticipantMetadataSchema,
  type SessionDescription,
  type SignalRequest,
  type SignalResponse,
} from '../proto/livekit_rtc_pb.js';
import type { AudioSource } from './audio.ts';

const STREAM_CHUNK_BYTES = 15000;
const PROTOCOL_VERSION = 16;
export const SDK_VERSION = '0.1.0';

export interface TrackPublication {
  sid: string;
  name: string;
  kind: 'audio' | 'video' | 'data';
  source: string;
  participantSid: string;
  muted: boolean;
  track?: MediaStreamTrack;
}

export interface Participant {
  sid: string;
  identity: string;
  name: string;
  metadata: string;
  kind: string;
  isAgent: boolean;
  attributes: Record<string, string>;
  tracks: Map<string, TrackPublication>;
}

function publicationFrom(info: TrackInfo, participantSid: string): TrackPublication {
  return {
    sid: info.sid,
    name: info.name,
    kind: info.type === TrackType.AUDIO ? 'audio' : info.type === TrackType.VIDEO ? 'video' : 'data',
    source: TrackSource[info.source].toLowerCase(),
    participantSid,
    muted: info.muted,
  };
}

function participantFrom(info: ParticipantInfo): Participant {
  const kind = ParticipantInfo_Kind[info.kind].toLowerCase();
  const p: Participant = {
    sid: info.sid,
    identity: info.identity,
    name: info.name,
    metadata: info.metadata,
    kind,
    isAgent: info.kind === ParticipantInfo_Kind.AGENT,
    attributes: { ...info.attributes },
    tracks: new Map(),
  };
  return p;
}

export interface TextStreamInfo {
  streamId: string;
  topic: string;
  senderIdentity: string;
  attributes: Record<string, string>;
  generated: boolean;
}

export class RpcError extends Error {
  static readonly APPLICATION_ERROR = 1500;
  static readonly UNSUPPORTED_METHOD = 1400;
  readonly code: number;

  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

export class TextStreamReader {
  readonly info: TextStreamInfo;
  finalAttributes: Record<string, string> = {};
  private chunks: string[] = [];
  private done = false;
  private wake: (() => void) | null = null;

  constructor(info: TextStreamInfo) {
    this.info = info;
  }

  feed(text: string | null): void {
    if (text === null) this.done = true;
    else this.chunks.push(text);
    this.wake?.();
    this.wake = null;
  }

  async readAll(): Promise<string> {
    while (!this.done) await new Promise<void>((resolve) => (this.wake = resolve));
    return this.chunks.join('');
  }
}

export class TextStreamWriter {
  readonly streamId = randomUUID().replaceAll('-', '');
  private readonly room: Room;
  private readonly topic: string;
  private readonly sender: string;
  private readonly attributes: Record<string, string>;
  private readonly destinations: string[];
  private index = 0;
  private opened = false;
  private closed = false;

  constructor(room: Room, topic: string, options: { senderIdentity?: string; attributes?: Record<string, string>; destinations?: string[] }) {
    this.room = room;
    this.topic = topic;
    this.sender = options.senderIdentity || room.localIdentity;
    this.attributes = options.attributes ?? {};
    this.destinations = options.destinations ?? [];
  }

  private send(value: DataPacket['value']): void {
    this.room.sendPacket(
      create(DataPacketSchema, { kind: DataPacket_Kind.RELIABLE, participantIdentity: this.sender, destinationIdentities: this.destinations, value }),
      true,
    );
  }

  private open(): void {
    if (this.opened) return;
    this.opened = true;
    const header = create(DataStream_HeaderSchema, {
      streamId: this.streamId,
      timestamp: BigInt(Date.now()),
      topic: this.topic,
      mimeType: 'text/plain',
      attributes: this.attributes,
      contentHeader: { case: 'textHeader', value: create(DataStream_TextHeaderSchema, { operationType: DataStream_OperationType.CREATE }) },
    });
    this.send({ case: 'streamHeader', value: header });
  }

  write(text: string): void {
    this.open();
    const data = Buffer.from(text);
    for (let start = 0; start < data.length; start += STREAM_CHUNK_BYTES) {
      const chunk = create(DataStream_ChunkSchema, { streamId: this.streamId, chunkIndex: BigInt(this.index++), content: data.subarray(start, start + STREAM_CHUNK_BYTES) });
      this.send({ case: 'streamChunk', value: chunk });
    }
  }

  close(attributes: Record<string, string> = {}): void {
    if (this.closed) return;
    this.closed = true;
    this.open();
    this.send({ case: 'streamTrailer', value: create(DataStream_TrailerSchema, { streamId: this.streamId, attributes }) });
  }
}

/**
 * Events: participantConnected, participantDisconnected, participantAttributesChanged, trackPublished, trackSubscribed,
 * trackUnsubscribed, localTrackSubscribed, dataReceived, disconnected.
 */
export class Room extends EventEmitter {
  name = '';
  sid = '';
  metadata = '';
  local: Participant | null = null;
  readonly remoteParticipants = new Map<string, Participant>();
  private ws: WebSocket | null = null;
  private pub: RTCPeerConnection | null = null;
  private sub: RTCPeerConnection | null = null;
  private reliable: ReturnType<RTCPeerConnection['createDataChannel']> | null = null;
  private lossy: ReturnType<RTCPeerConnection['createDataChannel']> | null = null;
  private pendingAnswer: { resolve: (sd: SessionDescription) => void; reject: (e: Error) => void } | null = null;
  private readonly trackPublished = new Map<string, (info: TrackInfo) => void>();
  private readonly subscribedLocal = new Set<string>();
  private readonly subscribedWaiters = new Map<string, Array<() => void>>();
  private readonly pendingTracks = new Map<string, MediaStreamTrack>();
  private readonly pendingCandidates: Record<'pub' | 'sub', string[]> = { pub: [], sub: [] };
  private readonly textHandlers = new Map<string, (reader: TextStreamReader) => unknown>();
  private readonly textStreams = new Map<string, TextStreamReader>();
  private readonly rpcHandlers = new Map<string, (caller: string, payload: string) => Promise<string> | string>();
  private readonly rpcCalls = new Map<string, { resolve: (payload: string) => void; reject: (e: Error) => void }>();
  private sequence = 0;
  private closed = false;
  private pingTimer: NodeJS.Timeout | null = null;
  private subChain: Promise<void> = Promise.resolve();
  private pubChain: Promise<void> = Promise.resolve();
  connected = false;
  disconnectReason: DisconnectReason = DisconnectReason.UNKNOWN_REASON;

  get localIdentity(): string {
    return this.local?.identity ?? '';
  }

  async connect(url: string, token: string, options: { autoSubscribe?: boolean; timeoutMs?: number } = {}): Promise<void> {
    const timeoutMs = options.timeoutMs ?? 20000;
    const base = url.replace(/\/$/, '').replace(/^http/, 'ws');
    const query = new URLSearchParams({
      access_token: token,
      auto_subscribe: options.autoSubscribe === false ? '0' : '1',
      sdk: ClientInfo_SDK[ClientInfo_SDK.NODE] ?? 'node',
      version: SDK_VERSION,
      protocol: String(PROTOCOL_VERSION),
      os: os.platform(),
      os_version: os.release(),
      adaptive_stream: '0',
    });
    const ws = new WebSocket(`${base}/rtc?${query}`, { maxPayload: 16 << 20 });
    ws.binaryType = 'nodebuffer';
    this.ws = ws;
    const join = await new Promise<Extract<SignalResponse['message'], { case: 'join' }>['value']>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('timed out joining')), timeoutMs);
      ws.once('unexpected-response', (_req, res) => {
        clearTimeout(timer);
        reject(new Error(`server refused the connection: ${res.statusCode}`));
      });
      ws.once('error', (e) => {
        clearTimeout(timer);
        reject(e);
      });
      ws.once('message', (raw: Buffer) => {
        clearTimeout(timer);
        const first = fromBinary(SignalResponseSchema, raw);
        if (first.message.case !== 'join') reject(new Error(`expected a join response, got ${first.message.case}`));
        else resolve(first.message.value);
      });
    });
    this.name = join.room?.name ?? '';
    this.sid = join.room?.sid ?? '';
    this.metadata = join.room?.metadata ?? '';
    this.local = participantFrom(join.participant!);
    for (const info of join.otherParticipants) this.applyParticipant(info);

    const config = {
      iceServers: join.iceServers.flatMap((s) => s.urls.map((u) => ({ urls: u, username: s.username || undefined, credential: s.credential || undefined }))),
      codecs: { audio: [new RTCRtpCodecParameters({ mimeType: 'audio/opus', clockRate: 48000, channels: 2 })], video: [] },
    };
    this.pub = new RTCPeerConnection(config);
    this.sub = new RTCPeerConnection(config);
    // The track's own id is werift's. The server's track SID is the id in the SDP.
    this.sub.ontrack = (event: { track: MediaStreamTrack; transceiver: { receiver: { remoteTrackId?: string } } }) => {
      this.onRemoteTrack(event.track, event.transceiver.receiver.remoteTrackId ?? event.track.id ?? '');
    };
    this.sub.onDataChannel.subscribe((channel) => channel.onMessage.subscribe((data) => this.onDataMessage(data as Buffer)));
    this.reliable = this.pub.createDataChannel('_reliable', { ordered: true });
    this.lossy = this.pub.createDataChannel('_lossy', { ordered: false, maxRetransmits: 0 });
    const opened = new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('the publisher connection did not come up')), timeoutMs);
      this.reliable!.stateChanged.subscribe((state) => {
        if (state === 'open') {
          clearTimeout(timer);
          resolve();
        }
      });
    });

    ws.on('message', (raw: Buffer) => {
      this.onSignal(fromBinary(SignalResponseSchema, raw)).catch((e) => console.error('error handling signal message', e));
    });
    ws.on('close', () => void this.disconnect());
    this.pingTimer = setInterval(() => {
      this.sendSignal({ case: 'pingReq', value: create(PingSchema, { timestamp: BigInt(Date.now()) }) });
    }, (join.pingInterval || 5) * 1000);

    await this.negotiatePublisher();
    await opened;
    this.connected = true;
  }

  private sendSignal(message: SignalRequest['message']): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(toBinary(SignalRequestSchema, create(SignalRequestSchema, { message })));
    }
  }

  async disconnect(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.connected = false;
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.sendSignal({ case: 'leave', value: create(LeaveRequestSchema, { reason: DisconnectReason.CLIENT_INITIATED, action: LeaveRequest_Action.DISCONNECT }) });
    this.ws?.removeAllListeners('close');
    this.ws?.close();
    await this.pub?.close();
    await this.sub?.close();
    this.emit('disconnected', this.disconnectReason);
  }

  private negotiatePublisher(): Promise<void> {
    const run = async () => {
      const pc = this.pub!;
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitForGathering(pc);
      const answered = new Promise<SessionDescription>((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('no answer from the server')), 15000);
        this.pendingAnswer = {
          resolve: (sd) => {
            clearTimeout(timer);
            resolve(sd);
          },
          reject,
        };
      });
      this.sendSignal({ case: 'offer', value: create(SessionDescriptionSchema, { type: 'offer', sdp: pc.localDescription!.sdp }) });
      const answer = await answered;
      await pc.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
      await this.flushCandidates('pub', pc);
    };
    const next = this.pubChain.then(run, run);
    this.pubChain = next.catch(() => {});
    return next;
  }

  private async onSignal(message: SignalResponse): Promise<void> {
    const m = message.message;
    switch (m.case) {
      case 'answer':
        this.pendingAnswer?.resolve(m.value);
        this.pendingAnswer = null;
        break;
      case 'offer':
        await this.onOffer(m.value);
        break;
      case 'trickle': {
        const target = m.value.target === SignalTarget.SUBSCRIBER ? 'sub' : 'pub';
        const pc = target === 'sub' ? this.sub : this.pub;
        if (!pc?.remoteDescription) this.pendingCandidates[target].push(m.value.candidateInit);
        else await this.addCandidate(pc, m.value.candidateInit);
        break;
      }
      case 'update':
        for (const info of m.value.participants) this.applyParticipant(info);
        break;
      case 'trackPublished': {
        const resolve = this.trackPublished.get(m.value.cid);
        if (resolve && m.value.track) {
          this.trackPublished.delete(m.value.cid);
          resolve(m.value.track);
        }
        break;
      }
      case 'trackSubscribed': {
        this.subscribedLocal.add(m.value.trackSid);
        for (const fn of this.subscribedWaiters.get(m.value.trackSid) ?? []) fn();
        this.subscribedWaiters.delete(m.value.trackSid);
        this.emit('localTrackSubscribed', m.value.trackSid);
        break;
      }
      case 'leave':
        this.disconnectReason = m.value.reason;
        await this.disconnect();
        break;
      default:
        break;
    }
  }

  private onOffer(offer: SessionDescription): Promise<void> {
    const run = async () => {
      const pc = this.sub!;
      await pc.setRemoteDescription({ type: 'offer', sdp: offer.sdp });
      await this.flushCandidates('sub', pc);
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      await waitForGathering(pc);
      this.sendSignal({ case: 'answer', value: create(SessionDescriptionSchema, { type: 'answer', sdp: pc.localDescription!.sdp, id: offer.id }) });
    };
    const next = this.subChain.then(run, run);
    this.subChain = next.catch(() => {});
    return next;
  }

  private async flushCandidates(target: 'pub' | 'sub', pc: RTCPeerConnection): Promise<void> {
    const pending = this.pendingCandidates[target];
    this.pendingCandidates[target] = [];
    for (const raw of pending) await this.addCandidate(pc, raw);
  }

  private async addCandidate(pc: RTCPeerConnection, raw: string): Promise<void> {
    const init = raw ? JSON.parse(raw) : {};
    if (!init.candidate) return;
    await pc.addIceCandidate(init).catch(() => {});
  }

  private applyParticipant(info: ParticipantInfo): void {
    if (this.local && info.identity === this.local.identity) {
      this.local.attributes = { ...info.attributes };
      this.local.metadata = info.metadata;
      return;
    }
    let current = this.remoteParticipants.get(info.identity);
    if (info.state === ParticipantInfo_State.DISCONNECTED) {
      if (current && current.sid === info.sid) {
        this.remoteParticipants.delete(info.identity);
        this.emit('participantDisconnected', current);
      }
      return;
    }
    if (!current || current.sid !== info.sid) {
      if (current) this.emit('participantDisconnected', current);
      current = participantFrom(info);
      this.remoteParticipants.set(info.identity, current);
      this.emit('participantConnected', current);
    } else {
      current.attributes = { ...info.attributes };
      current.metadata = info.metadata;
    }
    const seen = new Set<string>();
    for (const t of info.tracks) {
      seen.add(t.sid);
      if (!current.tracks.has(t.sid)) {
        const pub = publicationFrom(t, info.sid);
        current.tracks.set(t.sid, pub);
        this.emit('trackPublished', pub, current);
        const pending = this.pendingTracks.get(t.sid);
        if (pending) {
          this.pendingTracks.delete(t.sid);
          pub.track = pending;
          this.emit('trackSubscribed', pending, pub, current);
        }
      }
    }
    for (const [sid, pub] of current.tracks) {
      if (!seen.has(sid)) {
        current.tracks.delete(sid);
        if (pub.track) this.emit('trackUnsubscribed', pub.track, pub, current);
      }
    }
  }

  private onRemoteTrack(track: MediaStreamTrack, id: string): void {
    for (const participant of this.remoteParticipants.values()) {
      const pub = participant.tracks.get(id);
      if (pub) {
        pub.track = track;
        this.emit('trackSubscribed', track, pub, participant);
        return;
      }
    }
    this.pendingTracks.set(id, track);
  }

  private onDataMessage(raw: Buffer): void {
    if (typeof raw === 'string') return;
    let dp: DataPacket;
    try {
      dp = fromBinary(DataPacketSchema, raw);
    } catch {
      return;
    }
    const v = dp.value;
    switch (v.case) {
      case 'streamHeader': {
        const header = v.value;
        if (header.contentHeader.case !== 'textHeader') return;
        const handler = this.textHandlers.get(header.topic);
        if (!handler) return;
        const reader = new TextStreamReader({
          streamId: header.streamId,
          topic: header.topic,
          senderIdentity: dp.participantIdentity,
          attributes: { ...header.attributes },
          generated: header.contentHeader.value.generated,
        });
        this.textStreams.set(header.streamId, reader);
        void handler(reader);
        break;
      }
      case 'streamChunk':
        this.textStreams.get(v.value.streamId)?.feed(Buffer.from(v.value.content).toString());
        break;
      case 'streamTrailer': {
        const reader = this.textStreams.get(v.value.streamId);
        if (reader) {
          this.textStreams.delete(v.value.streamId);
          reader.finalAttributes = { ...v.value.attributes };
          reader.feed(null);
        }
        break;
      }
      case 'rpcRequest':
        void this.onRpcRequest(dp.participantIdentity, v.value);
        break;
      case 'rpcAck':
        break;
      case 'rpcResponse': {
        const call = this.rpcCalls.get(v.value.requestId);
        if (call) {
          this.rpcCalls.delete(v.value.requestId);
          if (v.value.value.case === 'error') call.reject(new RpcError(v.value.value.value.code, v.value.value.value.message));
          else if (v.value.value.case === 'payload') call.resolve(v.value.value.value);
          // A compressed payload is never sent in answer to a plain request, so this does not read one.
          else call.reject(new RpcError(RpcError.APPLICATION_ERROR, 'the response had no plain payload'));
        }
        break;
      }
      default:
        this.emit('dataReceived', dp);
    }
  }

  private async onRpcRequest(caller: string, request: { id: string; method: string; payload: string }): Promise<void> {
    const send = (value: DataPacket['value']) => this.sendPacket(create(DataPacketSchema, { kind: DataPacket_Kind.RELIABLE, destinationIdentities: [caller], value }), true);
    send({ case: 'rpcAck', value: create(RpcAckSchema, { requestId: request.id }) });
    const fail = (code: number, message: string) =>
      send({ case: 'rpcResponse', value: create(RpcResponseSchema, { requestId: request.id, value: { case: 'error', value: create(RpcErrorSchema, { code, message }) } }) });
    const handler = this.rpcHandlers.get(request.method);
    if (!handler) {
      fail(RpcError.UNSUPPORTED_METHOD, 'Method not supported at destination');
      return;
    }
    try {
      const payload = await handler(caller, request.payload);
      send({ case: 'rpcResponse', value: create(RpcResponseSchema, { requestId: request.id, value: { case: 'payload', value: payload } }) });
    } catch (e) {
      if (e instanceof RpcError) fail(e.code, e.message);
      else fail(RpcError.APPLICATION_ERROR, 'Application error in method handler');
    }
  }

  /** The handler gets the caller's identity and the payload, and returns a payload. */
  registerRpcMethod(name: string, handler: (caller: string, payload: string) => Promise<string> | string): void {
    this.rpcHandlers.set(name, handler);
  }

  performRpc(destination: string, method: string, payload: string, timeoutMs = 10000): Promise<string> {
    const id = randomUUID();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.rpcCalls.delete(id);
        reject(new Error(`rpc ${method} timed out`));
      }, timeoutMs);
      this.rpcCalls.set(id, {
        resolve: (value) => {
          clearTimeout(timer);
          resolve(value);
        },
        reject: (e) => {
          clearTimeout(timer);
          reject(e);
        },
      });
      const value = create(RpcRequestSchema, { id, method, payload, responseTimeoutMs: timeoutMs, version: 1 });
      this.sendPacket(create(DataPacketSchema, { kind: DataPacket_Kind.RELIABLE, destinationIdentities: [destination], value: { case: 'rpcRequest', value } }), true);
    });
  }

  registerTextStreamHandler(topic: string, handler: (reader: TextStreamReader) => unknown): void {
    this.textHandlers.set(topic, handler);
  }

  streamText(topic: string, options: { senderIdentity?: string; attributes?: Record<string, string>; destinations?: string[] } = {}): TextStreamWriter {
    return new TextStreamWriter(this, topic, options);
  }

  sendPacket(dp: DataPacket, reliable: boolean): void {
    const channel = reliable ? this.reliable : this.lossy;
    if (!channel || channel.readyState !== 'open') return;
    if (reliable) dp.sequence = ++this.sequence;
    if (this.local) dp.participantSid = this.local.sid;
    channel.send(Buffer.from(toBinary(DataPacketSchema, dp)));
  }

  async publishTrack(source: AudioSource, options: { name: string; trackSource?: TrackSource; timeoutMs?: number }): Promise<LocalTrackPublication> {
    source.track.streamId = `${this.localIdentity}-stream`;
    // The server matches the track in the SDP by this ID, and werift's sender chooses it.
    const transceiver = this.pub!.addTransceiver(source.track, { direction: 'sendonly' });
    const cid = transceiver.sender.trackId;
    const published = new Promise<TrackInfo>((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('the server did not accept the track')), options.timeoutMs ?? 15000);
      this.trackPublished.set(cid, (info) => {
        clearTimeout(timer);
        resolve(info);
      });
    });
    this.sendSignal({
      case: 'addTrack',
      value: create(AddTrackRequestSchema, { cid, name: options.name, type: TrackType.AUDIO, source: options.trackSource ?? TrackSource.MICROPHONE }),
    });
    const info = await published;
    await this.negotiatePublisher();
    source.start();
    return new LocalTrackPublication(this, info.sid, source);
  }

  waitForLocalSubscription(sid: string, timeoutMs?: number): Promise<boolean> {
    if (this.subscribedLocal.has(sid)) return Promise.resolve(true);
    return new Promise((resolve) => {
      const timer = timeoutMs ? setTimeout(() => resolve(false), timeoutMs) : null;
      const list = this.subscribedWaiters.get(sid) ?? [];
      list.push(() => {
        if (timer) clearTimeout(timer);
        resolve(true);
      });
      this.subscribedWaiters.set(sid, list);
    });
  }

  setAttributes(attributes: Record<string, string>): void {
    if (!this.closed) this.sendSignal({ case: 'updateMetadata', value: create(UpdateParticipantMetadataSchema, { attributes }) });
  }
}

export class LocalTrackPublication {
  readonly room: Room;
  readonly sid: string;
  readonly source: AudioSource;

  constructor(room: Room, sid: string, source: AudioSource) {
    this.room = room;
    this.sid = sid;
    this.source = source;
  }

  waitForSubscription(timeoutMs?: number): Promise<boolean> {
    return this.room.waitForLocalSubscription(this.sid, timeoutMs);
  }
}

async function waitForGathering(pc: RTCPeerConnection): Promise<void> {
  if (pc.iceGatheringState === 'complete') return;
  await new Promise<void>((resolve) => {
    const { unSubscribe } = pc.iceGatheringStateChange.subscribe((state) => {
      if (state === 'complete') {
        unSubscribe();
        resolve();
      }
    });
  });
}

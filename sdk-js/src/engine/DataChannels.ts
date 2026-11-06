import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { DataPacketKind } from '../enums.js';
import { PublishDataError } from '../errors.js';
import { log } from '../logger.js';
import { type DataPacket, DataPacketSchema } from '../proto/livekit_models_pb.js';
import { type DataChannelInfo, DataChannelInfoSchema, type DataChannelReceiveState, DataChannelReceiveStateSchema, SignalTarget } from '../proto/livekit_rtc_pb.js';
import { Mutex } from '../util/async.js';
import { TypedEmitter } from '../util/emitter.js';
import { ReliableBuffer, SequenceTracker } from './reliableBuffer.js';

export const RELIABLE_LABEL = '_reliable';
export const LOSSY_LABEL = '_lossy';

// A bigger message is not guaranteed to survive SCTP between implementations.
const MAX_MESSAGE_BYTES = 64000;

const RELIABLE_LOW_WATERMARK = 64 << 10;
const RELIABLE_HIGH_WATERMARK = 1 << 20;
const LOSSY_LOW_WATERMARK = 8 << 10;
const LOSSY_HIGH_WATERMARK = 256 << 10;

type Events = {
  packet: (packet: DataPacket, kind: DataPacketKind) => void;
  bufferStatus: (isLow: boolean, kind: DataPacketKind) => void;
};

export interface DataTransport {
  /** Gets the publisher connection and channels ready, starting a negotiation if that is what they need. */
  ensureReady(kind: DataPacketKind): Promise<RTCDataChannel>;
  /** True while sends should wait for the connection to return, rather than trying to use it. */
  isReconnecting(): boolean;
  /** The size the remote peer accepts, once it is known. */
  maxMessageSize(): number | undefined;
}

/**
 * The data channels of a session: the two this client opens on the publisher connection to send, and the
 * two the server opens on the subscriber connection to deliver.
 *
 * Reliable packets carry sequence numbers, so that a resume can resend what the server missed and receivers
 * can ignore what they already have.
 */
export class DataChannels extends TypedEmitter<Events> {
  private reliable?: RTCDataChannel;
  private lossy?: RTCDataChannel;
  private receiving = new Map<string, RTCDataChannel>();
  private nextSequence = 1;
  private buffer = new ReliableBuffer<DataPacket>();
  private received = new SequenceTracker();
  private sendLock = new Mutex();
  private reliableLowWaiters: Array<() => void> = [];
  private bufferLow = new Map<DataPacketKind, boolean>();
  private carriedOver: DataPacket[] = [];

  constructor(private readonly transport: DataTransport) {
    super();
  }

  /** Opens the sending channels on a publisher connection, if this session has none. */
  ensureChannels(pc: RTCPeerConnection) {
    if (!this.reliable || this.reliable.readyState === 'closed' || this.reliable.readyState === 'closing') {
      this.reliable = pc.createDataChannel(RELIABLE_LABEL, { ordered: true });
      this.setupSending(this.reliable, DataPacketKind.RELIABLE);
    }
    if (!this.lossy || this.lossy.readyState === 'closed' || this.lossy.readyState === 'closing') {
      this.lossy = pc.createDataChannel(LOSSY_LABEL, { ordered: false, maxRetransmits: 0 });
      this.setupSending(this.lossy, DataPacketKind.LOSSY);
    }
  }

  channel(kind: DataPacketKind): RTCDataChannel | undefined {
    return kind === DataPacketKind.RELIABLE ? this.reliable : this.lossy;
  }

  private setupSending(dc: RTCDataChannel, kind: DataPacketKind) {
    dc.binaryType = 'arraybuffer';
    dc.bufferedAmountLowThreshold = kind === DataPacketKind.RELIABLE ? RELIABLE_LOW_WATERMARK : LOSSY_LOW_WATERMARK;
    dc.onbufferedamountlow = () => {
      this.setBufferLow(kind, true);
      if (kind === DataPacketKind.RELIABLE) {
        const waiters = this.reliableLowWaiters;
        this.reliableLowWaiters = [];
        for (const w of waiters) w();
      }
    };
    // Servers can deliver on these too.
    dc.onmessage = (ev) => this.handleMessage(ev.data, kind);
    dc.onclose = () => log.debug('publisher data channel closed', dc.label);
    dc.onerror = (ev) => log.warn('publisher data channel error', dc.label, ev);
  }

  /** Takes a channel the server opened. Other labels are features this client does not handle. */
  addReceiving(dc: RTCDataChannel) {
    let kind: DataPacketKind;
    if (dc.label === RELIABLE_LABEL) kind = DataPacketKind.RELIABLE;
    else if (dc.label === LOSSY_LABEL) kind = DataPacketKind.LOSSY;
    else {
      log.debug('ignoring data channel', dc.label);
      return;
    }
    dc.binaryType = 'arraybuffer';
    dc.onmessage = (ev) => this.handleMessage(ev.data, kind);
    this.receiving.set(dc.label, dc);
  }

  private handleMessage(data: unknown, kind: DataPacketKind) {
    if (!(data instanceof ArrayBuffer)) {
      log.debug('dropping data message that is not binary');
      return;
    }
    let packet: DataPacket;
    try {
      packet = fromBinary(DataPacketSchema, new Uint8Array(data));
    } catch (e) {
      log.warn('could not read data packet', e);
      return;
    }
    if (packet.sequence > 0 && packet.participantSid !== '' && !this.received.accept(packet.participantSid, packet.sequence)) {
      log.debug('dropping repeated data packet', packet.participantSid, packet.sequence);
      return;
    }
    // Servers and older clients fill in different levels of the packet. Nothing above this has to care.
    if (packet.value.case === 'user') {
      const user = packet.value.value;
      if (!packet.participantIdentity) packet.participantIdentity = user.participantIdentity;
      else if (!user.participantIdentity) user.participantIdentity = packet.participantIdentity;
      if (packet.destinationIdentities.length === 0) packet.destinationIdentities = user.destinationIdentities;
      else if (user.destinationIdentities.length === 0) user.destinationIdentities = packet.destinationIdentities;
    }
    this.emit('packet', packet, kind);
  }

  async send(packet: DataPacket, kind: DataPacketKind): Promise<void> {
    if (kind === DataPacketKind.LOSSY) return this.sendLossy(packet);
    return this.sendReliable(packet);
  }

  private assertSize(bytes: Uint8Array) {
    const limit = Math.min(this.transport.maxMessageSize() ?? MAX_MESSAGE_BYTES, MAX_MESSAGE_BYTES);
    if (bytes.length > limit) {
      throw new PublishDataError(`data packet of ${bytes.length} bytes is larger than the limit of ${limit}`);
    }
  }

  private async sendLossy(packet: DataPacket) {
    const bytes = toBinary(DataPacketSchema, packet);
    this.assertSize(bytes);
    // Lossy means current. Anything sent during an outage would be stale by the time it arrived.
    if (this.transport.isReconnecting()) return;
    const dc = await this.transport.ensureReady(DataPacketKind.LOSSY);
    if (dc.bufferedAmount > LOSSY_HIGH_WATERMARK) {
      this.setBufferLow(DataPacketKind.LOSSY, false);
      return;
    }
    dc.send(new Uint8Array(bytes));
    this.noteBuffer(dc, DataPacketKind.LOSSY);
  }

  private async sendReliable(packet: DataPacket) {
    packet.sequence = this.nextSequence;
    const bytes = toBinary(DataPacketSchema, packet);
    this.assertSize(bytes);
    this.nextSequence++;
    const entry = this.buffer.push(packet.sequence, bytes, packet);
    // During a reconnect the send counts as done: the packet goes out once the connection returns.
    if (this.transport.isReconnecting()) return;
    return this.sendLock.run(async () => {
      if (entry.sent) return;
      try {
        await this.writeReliable(entry.bytes);
        entry.sent = true;
      } catch (e) {
        if (this.transport.isReconnecting()) return;
        this.buffer.remove(entry);
        throw e;
      }
    });
  }

  private async writeReliable(bytes: Uint8Array) {
    const dc = await this.transport.ensureReady(DataPacketKind.RELIABLE);
    if (dc.bufferedAmount > RELIABLE_HIGH_WATERMARK) {
      this.setBufferLow(DataPacketKind.RELIABLE, false);
      await new Promise<void>((resolve) => this.reliableLowWaiters.push(resolve));
    }
    dc.send(new Uint8Array(bytes));
    this.noteBuffer(dc, DataPacketKind.RELIABLE);
  }

  private noteBuffer(dc: RTCDataChannel, kind: DataPacketKind) {
    if (dc.bufferedAmount > dc.bufferedAmountLowThreshold) this.setBufferLow(kind, false);
  }

  private setBufferLow(kind: DataPacketKind, low: boolean) {
    if (this.bufferLow.get(kind) === low) return;
    this.bufferLow.set(kind, low);
    this.emit('bufferStatus', low, kind);
  }

  /**
   * Resends after a resume what the server has not confirmed. A server that has received nothing reports 0,
   * and the packets held during the outage still have to go, so 0 does not mean nothing to send.
   */
  resendAfterResume(lastSequence: number): Promise<void> {
    return this.sendLock.run(async () => {
      this.buffer.dropThrough(lastSequence);
      for (const entry of [...this.buffer.all()]) {
        await this.writeReliable(entry.bytes);
        entry.sent = true;
      }
    });
  }

  /**
   * A full reconnect is a new session: sequence numbers restart under a new participant ID, and the server
   * has no memory of this client. Packets the application was told would go out still have to.
   */
  resetForNewSession() {
    this.carriedOver = this.buffer.takeUnsent();
    this.nextSequence = 1;
    this.reliable = undefined;
    this.lossy = undefined;
    this.receiving.clear();
    this.received.clear();
    this.reliableLowWaiters = [];
    this.bufferLow.clear();
  }

  /** Sends what was held across a full reconnect, with this session's sequence numbers. */
  async sendCarriedOver() {
    const packets = this.carriedOver;
    this.carriedOver = [];
    for (const packet of packets) await this.sendReliable(packet);
  }

  /** What SyncState reports, so a server can avoid resending what this client already has. */
  receiveStates(): DataChannelReceiveState[] {
    return this.received.states().map((s) =>
      create(DataChannelReceiveStateSchema, { publisherSid: s.sender, lastSeq: s.sequence }),
    );
  }

  channelInfos(): DataChannelInfo[] {
    const out: DataChannelInfo[] = [];
    for (const dc of [this.reliable, this.lossy]) {
      if (dc && dc.id !== null) out.push(create(DataChannelInfoSchema, { label: dc.label, id: dc.id, target: SignalTarget.PUBLISHER }));
    }
    for (const dc of this.receiving.values()) {
      if (dc.id !== null) out.push(create(DataChannelInfoSchema, { label: dc.label, id: dc.id, target: SignalTarget.SUBSCRIBER }));
    }
    return out;
  }

  close() {
    for (const dc of [this.reliable, this.lossy, ...this.receiving.values()]) {
      if (!dc) continue;
      dc.onmessage = dc.onclose = dc.onerror = dc.onbufferedamountlow = null;
      dc.close();
    }
    this.reliable = undefined;
    this.lossy = undefined;
    this.receiving.clear();
    const waiters = this.reliableLowWaiters;
    this.reliableLowWaiters = [];
    for (const w of waiters) w();
  }
}

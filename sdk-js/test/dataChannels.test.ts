import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { describe, expect, it, vi } from 'vitest';
import { DataChannels, LOSSY_LABEL, RELIABLE_LABEL } from '../src/engine/DataChannels.js';
import { DataPacketKind } from '../src/enums.js';
import { PublishDataError } from '../src/errors.js';
import { type DataPacket, DataPacketSchema, UserPacketSchema } from '../src/proto/livekit_models_pb.js';

class FakeChannel {
  readyState: RTCDataChannelState = 'open';
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;
  binaryType = 'blob';
  id: number | null = 1;
  sent: DataPacket[] = [];
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onbufferedamountlow: (() => void) | null = null;
  onclose: unknown = null;
  onerror: unknown = null;
  constructor(public label: string) {}
  send(bytes: Uint8Array) {
    this.sent.push(fromBinary(DataPacketSchema, bytes));
  }
  close() {
    this.readyState = 'closed';
  }
}

const packet = (text: string) =>
  create(DataPacketSchema, { value: { case: 'user', value: create(UserPacketSchema, { payload: new TextEncoder().encode(text) }) } });
const textOf = (p: DataPacket) => (p.value.case === 'user' ? new TextDecoder().decode(p.value.value.payload) : '');
const wire = (p: Partial<Parameters<typeof create<typeof DataPacketSchema>>[1]>) => {
  const bytes = toBinary(DataPacketSchema, create(DataPacketSchema, p as never));
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
};

function make() {
  const channels = new Map<string, FakeChannel>();
  const pc = {
    createDataChannel(label: string, init: RTCDataChannelInit) {
      const c = new FakeChannel(label);
      channels.set(label, c);
      return Object.assign(c, { init });
    },
  };
  const state = { reconnecting: false, maxSize: undefined as number | undefined };
  const data: DataChannels = new DataChannels({
    ensureReady: async (kind): Promise<RTCDataChannel> => {
      data.ensureChannels(pc as unknown as RTCPeerConnection);
      return data.channel(kind)!;
    },
    isReconnecting: () => state.reconnecting,
    maxMessageSize: () => state.maxSize,
  });
  data.ensureChannels(pc as unknown as RTCPeerConnection);
  const packets: Array<[DataPacket, DataPacketKind]> = [];
  data.on('packet', (p, k) => packets.push([p, k]));
  return { data, channels, state, packets, reliable: channels.get(RELIABLE_LABEL)!, lossy: channels.get(LOSSY_LABEL)! };
}

describe('sending channels', () => {
  it('have the labels and delivery a server expects', () => {
    const { reliable, lossy } = make();
    expect((reliable as any).init).toEqual({ ordered: true });
    expect((lossy as any).init).toEqual({ ordered: false, maxRetransmits: 0 });
  });
});

describe('reliable sends', () => {
  it('number packets from one, and send them in order', async () => {
    const { data, reliable } = make();
    await Promise.all([data.send(packet('a'), DataPacketKind.RELIABLE), data.send(packet('b'), DataPacketKind.RELIABLE)]);
    expect(reliable.sent.map((p) => [p.sequence, textOf(p)])).toEqual([[1, 'a'], [2, 'b']]);
  });

  it('are refused if too large, without using up a number', async () => {
    const { data, reliable } = make();
    const big = create(DataPacketSchema, { value: { case: 'user', value: create(UserPacketSchema, { payload: new Uint8Array(70000) }) } });
    await expect(data.send(big, DataPacketKind.RELIABLE)).rejects.toBeInstanceOf(PublishDataError);
    await data.send(packet('next'), DataPacketKind.RELIABLE);
    expect(reliable.sent[0].sequence).toBe(1);
  });

  it('respect a smaller limit from the other side', async () => {
    const { data, state } = make();
    state.maxSize = 10;
    await expect(data.send(packet('more than ten bytes'), DataPacketKind.LOSSY)).rejects.toBeInstanceOf(PublishDataError);
  });

  it('wait when the channel is full, and carry on when it drains', async () => {
    const { data, reliable } = make();
    reliable.bufferedAmount = (1 << 20) + 1;
    const done = data.send(packet('a'), DataPacketKind.RELIABLE);
    await vi.waitFor(() => expect(data.channel(DataPacketKind.RELIABLE)).toBeDefined());
    await new Promise((r) => setTimeout(r));
    expect(reliable.sent).toEqual([]);
    reliable.bufferedAmount = 0;
    reliable.onbufferedamountlow?.();
    await done;
    expect(reliable.sent).toHaveLength(1);
  });

  it('during a reconnect settle at once, and go out afterwards', async () => {
    const { data, reliable, state } = make();
    await data.send(packet('before'), DataPacketKind.RELIABLE);
    state.reconnecting = true;
    await data.send(packet('during'), DataPacketKind.RELIABLE);
    expect(reliable.sent).toHaveLength(1);
    state.reconnecting = false;
    await data.resendAfterResume(1);
    expect(reliable.sent.map((p) => [p.sequence, textOf(p)])).toEqual([[1, 'before'], [2, 'during']]);
  });
});

describe('resume', () => {
  it('resends exactly what the server has not confirmed, with the original numbers', async () => {
    const { data, reliable } = make();
    for (const t of ['a', 'b', 'c', 'd']) await data.send(packet(t), DataPacketKind.RELIABLE);
    reliable.sent = [];
    await data.resendAfterResume(2);
    expect(reliable.sent.map((p) => [p.sequence, textOf(p)])).toEqual([[3, 'c'], [4, 'd']]);
  });

  it('a server that has received nothing still gets what was held for it', async () => {
    const { data, reliable, state } = make();
    state.reconnecting = true;
    await data.send(packet('held'), DataPacketKind.RELIABLE);
    state.reconnecting = false;
    await data.resendAfterResume(0);
    expect(reliable.sent.map(textOf)).toEqual(['held']);
  });

  it('sends keep their place behind a resend', async () => {
    const { data, reliable, state } = make();
    state.reconnecting = true;
    await data.send(packet('held'), DataPacketKind.RELIABLE);
    state.reconnecting = false;
    const resend = data.resendAfterResume(0);
    const next = data.send(packet('next'), DataPacketKind.RELIABLE);
    await Promise.all([resend, next]);
    expect(reliable.sent.map(textOf)).toEqual(['held', 'next']);
  });
});

describe('full reconnect', () => {
  it('restarts numbering, and sends what never went out under the new numbers', async () => {
    const { data, channels, state } = make();
    await data.send(packet('sent'), DataPacketKind.RELIABLE);
    state.reconnecting = true;
    await data.send(packet('held'), DataPacketKind.RELIABLE);
    data.resetForNewSession();
    state.reconnecting = false;
    await data.sendCarriedOver();
    const fresh = channels.get(RELIABLE_LABEL)!;
    expect(fresh.sent.map((p) => [p.sequence, textOf(p)])).toEqual([[1, 'held']]);
    await data.send(packet('new'), DataPacketKind.RELIABLE);
    expect(fresh.sent.map((p) => p.sequence)).toEqual([1, 2]);
  });
});

describe('lossy sends', () => {
  it('have no number and nothing to resend', async () => {
    const { data, lossy, reliable } = make();
    await data.send(packet('a'), DataPacketKind.LOSSY);
    expect(lossy.sent[0].sequence).toBe(0);
    await data.resendAfterResume(0);
    expect(reliable.sent).toEqual([]);
  });
  it('are dropped during a reconnect, and when the channel is backed up', async () => {
    const { data, lossy, state } = make();
    state.reconnecting = true;
    await data.send(packet('a'), DataPacketKind.LOSSY);
    state.reconnecting = false;
    lossy.bufferedAmount = (256 << 10) + 1;
    await data.send(packet('b'), DataPacketKind.LOSSY);
    expect(lossy.sent).toEqual([]);
  });
});

describe('receiving', () => {
  const userFrom = (sid: string, identity: string, sequence: number) =>
    wire({ sequence, participantSid: sid, participantIdentity: identity, value: { case: 'user', value: create(UserPacketSchema, { payload: new Uint8Array([1]) }) } });

  it('takes the two known labels and ignores the rest', () => {
    const { data, packets } = make();
    const other = new FakeChannel('_data_track');
    data.addReceiving(other as unknown as RTCDataChannel);
    expect(other.onmessage).toBeNull();
    const dc = new FakeChannel(LOSSY_LABEL);
    data.addReceiving(dc as unknown as RTCDataChannel);
    dc.onmessage?.({ data: userFrom('PA_1', 'x', 0) });
    expect(packets[0][1]).toBe(DataPacketKind.LOSSY);
  });

  it('drops repeats from a sender, by sequence', () => {
    const { data, packets } = make();
    const dc = new FakeChannel(RELIABLE_LABEL);
    data.addReceiving(dc as unknown as RTCDataChannel);
    dc.onmessage?.({ data: userFrom('PA_1', 'a', 1) });
    dc.onmessage?.({ data: userFrom('PA_1', 'a', 1) });
    dc.onmessage?.({ data: userFrom('PA_1', 'a', 2) });
    dc.onmessage?.({ data: userFrom('PA_2', 'b', 1) });
    expect(packets).toHaveLength(3);
  });

  it('a packet with no sequence or sender is never a repeat', () => {
    const { data, packets } = make();
    const dc = new FakeChannel(RELIABLE_LABEL);
    data.addReceiving(dc as unknown as RTCDataChannel);
    dc.onmessage?.({ data: userFrom('', 'server', 0) });
    dc.onmessage?.({ data: userFrom('', 'server', 0) });
    expect(packets).toHaveLength(2);
  });

  it('fills in identity and destinations from whichever level of the packet has them', () => {
    const { data, packets } = make();
    const dc = new FakeChannel(RELIABLE_LABEL);
    data.addReceiving(dc as unknown as RTCDataChannel);
    dc.onmessage?.({ data: wire({ value: { case: 'user', value: create(UserPacketSchema, { participantIdentity: 'old', destinationIdentities: ['d'] }) } }) });
    const [p] = packets[0];
    expect(p.participantIdentity).toBe('old');
    expect(p.destinationIdentities).toEqual(['d']);
  });

  it('drops text, and anything unreadable, without trouble', () => {
    const { data, packets } = make();
    const dc = new FakeChannel(RELIABLE_LABEL);
    data.addReceiving(dc as unknown as RTCDataChannel);
    dc.onmessage?.({ data: 'text' });
    dc.onmessage?.({ data: new Uint8Array([0xff, 0xff]).buffer });
    expect(packets).toEqual([]);
  });

  it('reports what it has seen, for a server to avoid repeating itself', () => {
    const { data } = make();
    const dc = new FakeChannel(RELIABLE_LABEL);
    data.addReceiving(dc as unknown as RTCDataChannel);
    dc.onmessage?.({ data: userFrom('PA_1', 'a', 5) });
    expect(data.receiveStates().map((s) => [s.publisherSid, s.lastSeq])).toEqual([['PA_1', 5]]);
  });
});

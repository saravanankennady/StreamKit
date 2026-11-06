import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { type EngineOptions, RTCEngine } from '../src/engine/RTCEngine.js';
import { DataPacketKind } from '../src/enums.js';
import { ConnectionErrorReason } from '../src/errors.js';
import {
  ClientConfigSetting,
  ClientConfigurationSchema,
  DataPacketSchema,
  DisconnectReason,
  ParticipantInfoSchema,
  TrackInfoSchema,
  UserPacketSchema,
} from '../src/proto/livekit_models_pb.js';
import {
  AddTrackRequestSchema,
  JoinResponseSchema,
  LeaveRequestSchema,
  LeaveRequest_Action,
  ReconnectResponseSchema,
  RequestResponseSchema,
  RequestResponse_Reason,
  SessionDescriptionSchema,
  type SignalRequest,
  SignalRequestSchema,
  SignalResponseSchema,
  type SignalResponse,
  TrackPublishedResponseSchema,
} from '../src/proto/livekit_rtc_pb.js';

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  static OPEN = 1;
  static CONNECTING = 0;
  readyState = 0;
  binaryType = '';
  sent: SignalRequest[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
  receive(message: SignalResponse['message']) {
    const b = toBinary(SignalResponseSchema, create(SignalResponseSchema, { message }));
    this.onmessage?.({ data: b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength) });
  }
  serverClose() {
    this.readyState = 3;
    this.onclose?.({ code: 1006 });
  }
  send(data: Uint8Array) {
    this.sent.push(fromBinary(SignalRequestSchema, data));
  }
  close() {
    this.readyState = 3;
    this.onclose?.({ code: 1000 });
  }
  get isResume() {
    return new URL(this.url).searchParams.get('reconnect') === '1';
  }
}

class FakeChannel extends EventTarget {
  readyState: RTCDataChannelState = 'open';
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;
  binaryType = 'blob';
  id = 1;
  sent: Uint8Array[] = [];
  onmessage: unknown = null;
  onbufferedamountlow: unknown = null;
  onclose: unknown = null;
  onerror: unknown = null;
  constructor(public label: string) {
    super();
  }
  send(b: Uint8Array) {
    this.sent.push(b);
  }
  close() {
    this.readyState = 'closed';
  }
}

class FakePC {
  static instances: FakePC[] = [];
  signalingState: RTCSignalingState = 'stable';
  connectionState: RTCPeerConnectionState = 'connected';
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  onicecandidate: unknown = null;
  onconnectionstatechange: (() => void) | null = null;
  ontrack: unknown = null;
  ondatachannel: unknown = null;
  offers: boolean[] = [];
  channels: FakeChannel[] = [];
  sctp = null;
  constructor(public config: RTCConfiguration) {
    FakePC.instances.push(this);
  }
  async createOffer(o?: { iceRestart?: boolean }) {
    this.offers.push(!!o?.iceRestart);
    return { type: 'offer', sdp: 'v=0\r\n' } as RTCSessionDescriptionInit;
  }
  async createAnswer() {
    return { type: 'answer', sdp: 'v=0\r\n' } as RTCSessionDescriptionInit;
  }
  async setLocalDescription(d: RTCSessionDescriptionInit) {
    if (d.type === 'rollback') {
      this.signalingState = 'stable';
      return;
    }
    this.localDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-local-offer' : 'stable';
  }
  async setRemoteDescription(d: RTCSessionDescriptionInit) {
    this.remoteDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-remote-offer' : 'stable';
  }
  async addIceCandidate() {}
  setConfiguration() {}
  getSenders() {
    return [];
  }
  getTransceivers() {
    return [];
  }
  createDataChannel(label: string) {
    const c = new FakeChannel(label);
    this.channels.push(c);
    return c;
  }
  close() {
    this.connectionState = 'closed';
  }
  fail() {
    this.setConnectionState('failed');
  }
  setConnectionState(s: RTCPeerConnectionState) {
    this.connectionState = s;
    this.onconnectionstatechange?.();
  }
}

const socket = () => FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
const OPTIONS: EngineOptions = { signal: { autoSubscribe: true }, peerConnectionTimeout: 1000, websocketTimeout: 1000, reconnectPolicy: { nextRetryDelayInMs: () => 10 } };

const joinMessage = (over = {}): SignalResponse['message'] => ({
  case: 'join',
  value: create(JoinResponseSchema, {
    pingInterval: 5,
    pingTimeout: 15,
    subscriberPrimary: true,
    participant: create(ParticipantInfoSchema, { sid: 'PA_1', identity: 'me' }),
    ...over,
  }),
});

async function joined(over = {}, options = OPTIONS) {
  const engine = new RTCEngine();
  const p = engine.join('ws://h', 'first-token', options);
  socket().open();
  socket().receive(joinMessage(over));
  await p;
  await engine.waitForPrimaryConnected();
  return engine;
}

const flush = () => vi.advanceTimersByTimeAsync(0);
const events = (engine: RTCEngine) => {
  const seen: string[] = [];
  for (const e of ['resuming', 'signalResumed', 'resumed', 'restarting', 'signalRestarted', 'restarted', 'disconnected'] as const) {
    engine.on(e, () => seen.push(e));
  }
  return seen;
};

/** Takes a resume attempt through to finished, as a server that accepts it would. */
async function acceptResume(lastMessageSeq = 0) {
  socket().open();
  socket().receive({ case: 'reconnect', value: create(ReconnectResponseSchema, { lastMessageSeq }) });
  await vi.advanceTimersByTimeAsync(2000);
}

beforeEach(() => {
  vi.useFakeTimers();
  FakeWebSocket.instances = [];
  FakePC.instances = [];
  vi.stubGlobal('WebSocket', FakeWebSocket);
  vi.stubGlobal('RTCPeerConnection', FakePC);
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('failed to fetch')));
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('joining', () => {
  it('makes both connections, and offers only if the server wants the publisher up early', async () => {
    await joined({ fastPublish: false });
    expect(FakePC.instances).toHaveLength(2);
    await flush();
    await vi.advanceTimersByTimeAsync(25);
    expect(FakePC.instances[0].offers).toEqual([]);
  });

  it('with fast publish, offers straight away with data channels ready', async () => {
    const engine = await joined({ fastPublish: true });
    await vi.advanceTimersByTimeAsync(25);
    expect(socket().sent.filter((m) => m.message.case === 'offer')).toHaveLength(1);
    expect(FakePC.instances[0].channels.map((c) => c.label)).toEqual(['_reliable', '_lossy']);
    await engine.close();
  });

  it('with a publisher as primary, offers even without fast publish', async () => {
    const engine = await joined({ subscriberPrimary: false, fastPublish: false });
    await vi.advanceTimersByTimeAsync(25);
    expect(socket().sent.filter((m) => m.message.case === 'offer')).toHaveLength(1);
    await engine.close();
  });

  it('answers a subscriber offer with its ID', async () => {
    await joined();
    socket().receive({ case: 'offer', value: create(SessionDescriptionSchema, { sdp: 'v=0\r\n', id: 41 }) });
    await flush();
    const answer = socket().sent.find((m) => m.message.case === 'answer');
    expect(answer?.message.case === 'answer' && answer.message.value.id).toBe(41);
  });

  it('keeps the token the server refreshes', async () => {
    const engine = await joined();
    socket().receive({ case: 'refreshToken', value: 'newer' });
    expect(engine.token).toBe('newer');
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    expect(new URL(socket().url).searchParams.get('access_token')).toBe('newer');
  });
});

describe('deciding between resume and full reconnect', () => {
  it('a lost socket resumes, sending the session and reason', async () => {
    const engine = await joined();
    const seen = events(engine);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    expect(socket().isResume).toBe(true);
    const params = new URL(socket().url).searchParams;
    expect(params.get('sid')).toBe('PA_1');
    expect(params.get('reconnect_reason')).toBe('1');
    await acceptResume();
    expect(seen).toEqual(['resuming', 'signalResumed', 'resumed']);
    expect(socket().sent.map((m) => m.message.case)).toContain('syncState');
  });

  it('a resume with the publisher in use restarts ICE on it if it has lost its connection, and leaves the subscriber to the server', async () => {
    await joined({ fastPublish: true });
    await vi.advanceTimersByTimeAsync(25);
    socket().receive({ case: 'answer', value: create(SessionDescriptionSchema, { sdp: 'v=0\r\n', id: 1 }) });
    await flush();
    const [publisher, subscriber] = FakePC.instances;
    publisher.connectionState = 'disconnected';
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    await acceptResume();
    expect(publisher.offers).toEqual([false, true]);
    expect(subscriber.offers).toEqual([]);
    expect(socket().sent.map((m) => m.message.case).indexOf('syncState')).toBeLessThan(socket().sent.map((m) => m.message.case).indexOf('offer'));
  });

  it('a publisher still connected after losing its socket keeps its ICE session', async () => {
    await joined({ fastPublish: true });
    await vi.advanceTimersByTimeAsync(25);
    socket().receive({ case: 'answer', value: create(SessionDescriptionSchema, { sdp: 'v=0\r\n', id: 1 }) });
    await flush();
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    await acceptResume();
    expect(FakePC.instances[0].offers).toEqual([false, false]);
  });

  it('a failed primary connection resumes too, and says why', async () => {
    await joined();
    FakePC.instances[1].fail();
    await vi.advanceTimersByTimeAsync(10);
    expect(socket().isResume).toBe(true);
    expect(new URL(socket().url).searchParams.get('reconnect_reason')).toBe('3');
  });

  it('a primary connection that stays disconnected resumes without waiting for it to fail', async () => {
    await joined();
    const before = FakeWebSocket.instances.length;
    FakePC.instances[1].setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(2500);
    expect(FakeWebSocket.instances.length).toBe(before);
    await vi.advanceTimersByTimeAsync(1500);
    expect(socket().isResume).toBe(true);
  });

  it('a connection that comes back by itself is left alone', async () => {
    await joined();
    const before = FakeWebSocket.instances.length;
    FakePC.instances[1].setConnectionState('disconnected');
    await vi.advanceTimersByTimeAsync(1000);
    FakePC.instances[1].setConnectionState('connected');
    await vi.advanceTimersByTimeAsync(5000);
    expect(FakeWebSocket.instances.length).toBe(before);
  });

  it('a socket failure during a resume tries the resume again', async () => {
    const engine = await joined();
    const seen = events(engine);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(4000);
    expect(socket().isResume).toBe(true);
    expect(seen).not.toContain('restarting');
  });

  it('a resume the server refuses becomes a full reconnect', async () => {
    const engine = await joined();
    const seen = events(engine);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    socket().open();
    socket().receive({ case: 'leave', value: create(LeaveRequestSchema, { reason: DisconnectReason.STATE_MISMATCH, action: LeaveRequest_Action.RECONNECT }) });
    await vi.advanceTimersByTimeAsync(20);
    expect(seen).toContain('restarting');
    expect(socket().isResume).toBe(false);
    socket().open();
    socket().receive(joinMessage());
    await vi.advanceTimersByTimeAsync(2000);
    expect(seen).toContain('signalRestarted');
    expect(seen).toContain('restarted');
  });

  it('a token refused during a resume ends the session', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('token expired', { status: 401 })));
    const engine = await joined();
    const seen = events(engine);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    expect(seen).toContain('disconnected');
    expect(engine.isClosed).toBe(true);
  });

  it('a server that disables resume always gets a full reconnect', async () => {
    const engine = await joined({ clientConfiguration: create(ClientConfigurationSchema, { resumeConnection: ClientConfigSetting.DISABLED }) });
    const seen = events(engine);
    socket().serverClose();
    await vi.advanceTimersByTimeAsync(10);
    expect(seen).toEqual(['restarting']);
    expect(socket().isResume).toBe(false);
  });

  it('gives up when the policy says so', async () => {
    const engine = await joined({}, { ...OPTIONS, reconnectPolicy: { nextRetryDelayInMs: () => null } });
    const seen = events(engine);
    socket().serverClose();
    await flush();
    expect(seen).toEqual(['disconnected']);
    expect(engine.isClosed).toBe(true);
  });

  it('a negotiation that times out goes to a full reconnect', async () => {
    const engine = await joined({ fastPublish: true });
    const seen = events(engine);
    await vi.advanceTimersByTimeAsync(1000);
    await vi.advanceTimersByTimeAsync(20);
    expect(seen).toContain('restarting');
  });

  it('several reports of one outage make one attempt', async () => {
    await joined();
    const before = FakeWebSocket.instances.length;
    socket().serverClose();
    FakePC.instances[1].fail();
    FakePC.instances[0].fail();
    await vi.advanceTimersByTimeAsync(10);
    expect(FakeWebSocket.instances.length).toBe(before + 1);
  });
});

describe('leave from the server', () => {
  const leave = (over: object): SignalResponse['message'] => ({ case: 'leave', value: create(LeaveRequestSchema, over) });

  it('disconnect ends the session, with the reason', async () => {
    const engine = await joined();
    let reason: number | undefined;
    engine.on('disconnected', (r) => {
      reason = r;
    });
    socket().receive(leave({ action: LeaveRequest_Action.DISCONNECT, reason: DisconnectReason.ROOM_DELETED }));
    await flush();
    expect(reason).toBe(DisconnectReason.ROOM_DELETED);
  });

  it('the action decides, not the old can_reconnect flag', async () => {
    const engine = await joined();
    const seen = events(engine);
    socket().receive(leave({ canReconnect: true }));
    await flush();
    expect(seen).toEqual(['disconnected']);
  });

  it('resume resumes', async () => {
    const engine = await joined();
    const seen = events(engine);
    socket().receive(leave({ action: LeaveRequest_Action.RESUME }));
    await vi.advanceTimersByTimeAsync(10);
    expect(seen).toEqual(['resuming']);
    expect(socket().isResume).toBe(true);
  });

  it('reconnect goes straight to a full reconnect', async () => {
    const engine = await joined();
    const seen = events(engine);
    socket().receive(leave({ action: LeaveRequest_Action.RECONNECT }));
    await vi.advanceTimersByTimeAsync(10);
    expect(seen).toEqual(['restarting']);
    expect(socket().isResume).toBe(false);
  });
});

describe('publishing', () => {
  const add = () => create(AddTrackRequestSchema, { cid: 'c1' });

  it('resolves with what the server says, by client ID', async () => {
    const engine = await joined();
    const p = engine.addTrack(add());
    expect(socket().sent.at(-1)?.message.case).toBe('addTrack');
    socket().receive({ case: 'trackPublished', value: create(TrackPublishedResponseSchema, { cid: 'c1', track: create(TrackInfoSchema, { sid: 'TR_1' }) }) });
    await expect(p).resolves.toMatchObject({ sid: 'TR_1' });
  });

  it('refuses a duplicate while one is pending', async () => {
    const engine = await joined();
    void engine.addTrack(add()).catch(() => {});
    await expect(engine.addTrack(add())).rejects.toThrow(/already/);
  });

  it('times out with a clear error if the server never replies', async () => {
    const engine = await joined();
    const p = engine.addTrack(add());
    const assertion = expect(p).rejects.toThrow(/no response from the server/);
    await vi.advanceTimersByTimeAsync(10_000);
    await assertion;
  });

  it('a refusal from the server fails at once, with a status', async () => {
    const engine = await joined();
    const req = add();
    const p = engine.addTrack(req);
    socket().receive({
      case: 'requestResponse',
      value: create(RequestResponseSchema, { reason: RequestResponse_Reason.NOT_ALLOWED, message: 'no', request: { case: 'addTrack', value: req } }),
    });
    await expect(p).rejects.toMatchObject({ name: 'PublishTrackError', status: 403 });
  });

  it('waits for a live session, and fails if there will not be one', async () => {
    const engine = await joined();
    socket().serverClose();
    let ready = false;
    void engine.waitUntilReady(10_000).then(() => {
      ready = true;
    });
    await vi.advanceTimersByTimeAsync(10);
    expect(ready).toBe(false);
    await acceptResume();
    expect(ready).toBe(true);

    socket().serverClose();
    const waiting = engine.waitUntilReady(50);
    const assertion = expect(waiting).rejects.toMatchObject({ status: 408 });
    await vi.advanceTimersByTimeAsync(50);
    await assertion;
  });
});

describe('data around a resume', () => {
  const packet = () => create(DataPacketSchema, { value: { case: 'user', value: create(UserPacketSchema, { payload: new Uint8Array([1]) }) } });

  it('what is sent while reconnecting goes out with the resume', async () => {
    const engine = await joined({ fastPublish: true });
    await vi.advanceTimersByTimeAsync(25);
    socket().receive({ case: 'answer', value: create(SessionDescriptionSchema, { sdp: 'v=0\r\n', id: 1 }) });
    await flush();
    const channel = FakePC.instances[0].channels[0];
    await engine.sendDataPacket(packet(), DataPacketKind.RELIABLE);
    expect(channel.sent).toHaveLength(1);
    socket().serverClose();
    await flush();
    await engine.sendDataPacket(packet(), DataPacketKind.RELIABLE);
    expect(channel.sent).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(10);
    await acceptResume(1);
    await flush();
    expect(channel.sent).toHaveLength(2);
  });
});

describe('errors', () => {
  it('reports an unreachable server from the validate request', async () => {
    const engine = new RTCEngine();
    const p = engine.join('ws://h', 't', OPTIONS);
    socket().serverClose();
    await expect(p).rejects.toMatchObject({ reason: ConnectionErrorReason.ServerUnreachable });
  });

  it('a primary connection that fails ends the wait early, and clearly', async () => {
    const engine = new RTCEngine();
    const p = engine.join('ws://h', 't', OPTIONS);
    socket().open();
    socket().receive(joinMessage());
    await p;
    FakePC.instances[1].connectionState = 'connecting';
    const waiting = engine.waitForPrimaryConnected();
    FakePC.instances[1].fail();
    await expect(waiting).rejects.toThrow(/could not establish pc connection/);
  });
});

import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConnectionError, ConnectionErrorReason } from '../src/errors.js';
import { DisconnectReason } from '../src/proto/livekit_models_pb.js';
import {
  JoinResponseSchema,
  LeaveRequestSchema,
  LeaveRequest_Action,
  ReconnectResponseSchema,
  SessionDescriptionSchema,
  type SignalRequest,
  SignalRequestSchema,
  SignalResponseSchema,
  SignalTarget,
  type SignalResponse,
  TrickleRequestSchema,
  UpdateSubscriptionSchema,
} from '../src/proto/livekit_rtc_pb.js';
import { SignalClient, SignalState } from '../src/signal/SignalClient.js';

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];
  static OPEN = 1;
  static CONNECTING = 0;
  readyState = 0;
  binaryType = '';
  sent: SignalRequest[] = [];
  onopen: ((ev?: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code: number }) => void) | null = null;
  onerror: ((ev?: unknown) => void) | null = null;
  closed = false;

  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
  receive(message: SignalResponse['message']) {
    const bytes = toBinary(SignalResponseSchema, create(SignalResponseSchema, { message }));
    this.onmessage?.({ data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) });
  }
  serverClose(code = 1006) {
    this.readyState = 3;
    this.onclose?.({ code });
  }
  send(data: Uint8Array) {
    this.sent.push(fromBinary(SignalRequestSchema, data));
  }
  close() {
    this.closed = true;
    this.readyState = 3;
    this.onclose?.({ code: 1000 });
  }
}

const socket = () => FakeWebSocket.instances[FakeWebSocket.instances.length - 1];
const joinMessage = (over = {}): SignalResponse['message'] => ({
  case: 'join',
  value: create(JoinResponseSchema, { pingInterval: 5, pingTimeout: 15, ...over }),
});
const reconnectMessage = (lastMessageSeq = 0): SignalResponse['message'] => ({
  case: 'reconnect',
  value: create(ReconnectResponseSchema, { lastMessageSeq }),
});
const opts = { autoSubscribe: true };

async function joined(client: SignalClient) {
  const p = client.join('ws://h', 't', opts, () => {});
  socket().open();
  socket().receive(joinMessage());
  await p;
  return socket();
}

beforeEach(() => {
  vi.useFakeTimers();
  FakeWebSocket.instances = [];
  vi.stubGlobal('WebSocket', FakeWebSocket);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('first message on a join', () => {
  it('join resolves, and its handler runs before the next message can be read', async () => {
    const client = new SignalClient();
    const order: string[] = [];
    client.handlers.onOffer = () => order.push('offer');
    const p = client.join('ws://h', 't', opts, () => order.push('joined'));
    socket().open();
    socket().receive(joinMessage());
    // No await between these: the offer that follows a join must find the session already set up.
    socket().receive({ case: 'offer', value: create(SessionDescriptionSchema, { sdp: 'x', id: 1 }) });
    expect(order).toEqual(['joined', 'offer']);
    await p;
    expect(client.state).toBe(SignalState.Connected);
  });

  it('a leave is a refusal, and carries the reason out', async () => {
    const client = new SignalClient();
    const p = client.join('ws://h', 't', opts, () => {});
    socket().open();
    socket().receive({ case: 'leave', value: create(LeaveRequestSchema, { reason: DisconnectReason.JOIN_FAILURE, action: LeaveRequest_Action.DISCONNECT }) });
    const e = await p.catch((x) => x);
    expect(e).toBeInstanceOf(ConnectionError);
    expect(e.reason).toBe(ConnectionErrorReason.LeaveRequest);
    expect(e.disconnectReason).toBe(DisconnectReason.JOIN_FAILURE);
  });

  it('anything else fails the join', async () => {
    const client = new SignalClient();
    const p = client.join('ws://h', 't', opts, () => {});
    socket().open();
    socket().receive({ case: 'refreshToken', value: 'x' });
    const e = await p.catch((x) => x);
    expect(e.message).toMatch(/did not receive join response/);
  });

  it('times out if nothing comes after the upgrade', async () => {
    const client = new SignalClient();
    const p = client.join('ws://h', 't', opts, () => {});
    socket().open();
    const assertion = expect(p).rejects.toMatchObject({ reason: ConnectionErrorReason.Timeout });
    await vi.advanceTimersByTimeAsync(5000);
    await assertion;
    expect(socket().closed).toBe(true);
  });

  it('times out if the upgrade never finishes', async () => {
    const client = new SignalClient(() => 1000);
    const p = client.join('ws://h', 't', opts, () => {});
    const assertion = expect(p).rejects.toMatchObject({ reason: ConnectionErrorReason.Timeout });
    await vi.advanceTimersByTimeAsync(1000);
    await assertion;
  });

  it('a closed socket before the session starts is an error, not a loss', async () => {
    const client = new SignalClient();
    const lost = vi.fn();
    client.handlers.onClose = lost;
    const p = client.join('ws://h', 't', opts, () => {});
    socket().open();
    socket().serverClose();
    await expect(p).rejects.toBeInstanceOf(ConnectionError);
    expect(lost).not.toHaveBeenCalled();
  });

  it('a failed upgrade asks the validate endpoint why', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response('token is invalid', { status: 401 }));
    vi.stubGlobal('fetch', fetchMock);
    const client = new SignalClient();
    const p = client.join('ws://h', 'tok', opts, () => {});
    socket().serverClose();
    const e = await p.catch((x) => x);
    expect(e.reason).toBe(ConnectionErrorReason.NotAllowed);
    expect(e.message).toBe('token is invalid');
    expect(fetchMock.mock.calls[0][0]).toMatch(/^http:\/\/h\/rtc\/validate\?/);
  });

  it('aborting cancels', async () => {
    const client = new SignalClient();
    const abort = new AbortController();
    const p = client.join('ws://h', 't', opts, () => {}, abort.signal);
    abort.abort();
    await expect(p).rejects.toMatchObject({ reason: ConnectionErrorReason.Cancelled });
  });
});

describe('first message on a resume', () => {
  it('reconnect resolves, and carries the sequence for data replay', async () => {
    const client = new SignalClient();
    await joined(client);
    const p = client.reconnect('ws://h', 't', 'PA_1', 1, opts);
    expect(new URL(socket().url).searchParams.get('reconnect')).toBe('1');
    expect(new URL(socket().url).searchParams.get('sid')).toBe('PA_1');
    socket().open();
    socket().receive(reconnectMessage(7));
    expect((await p).lastMessageSeq).toBe(7);
  });

  it('anything but reconnect or leave is dropped, and the deadline keeps running', async () => {
    const client = new SignalClient();
    await joined(client);
    const offer = vi.fn();
    client.handlers.onOffer = offer;
    const p = client.reconnect('ws://h', 't', 'PA_1', 1, opts);
    socket().open();
    socket().receive(joinMessage());
    socket().receive({ case: 'offer', value: create(SessionDescriptionSchema, { sdp: 'x' }) });
    expect(offer).not.toHaveBeenCalled();
    const assertion = expect(p).rejects.toMatchObject({ reason: ConnectionErrorReason.Timeout });
    await vi.advanceTimersByTimeAsync(5000);
    await assertion;
  });

  it('a leave refuses it', async () => {
    const client = new SignalClient();
    await joined(client);
    const p = client.reconnect('ws://h', 't', 'PA_1', 1, opts);
    socket().open();
    socket().receive({ case: 'leave', value: create(LeaveRequestSchema, { reason: DisconnectReason.STATE_MISMATCH, action: LeaveRequest_Action.RECONNECT }) });
    await expect(p).rejects.toMatchObject({ reason: ConnectionErrorReason.LeaveRequest });
  });

  it('a replaced socket can no longer report anything', async () => {
    const client = new SignalClient();
    const old = await joined(client);
    const lost = vi.fn();
    client.handlers.onClose = lost;
    const p = client.reconnect('ws://h', 't', 'PA_1', 1, opts);
    old.serverClose();
    expect(lost).not.toHaveBeenCalled();
    socket().open();
    socket().receive(reconnectMessage());
    await p;
  });
});

describe('holding requests during a resume', () => {
  const subscription = create(UpdateSubscriptionSchema, { trackSids: ['TR_1'], subscribe: true });

  it('ordinary requests wait, session-restoring ones overtake, in order', async () => {
    const client = new SignalClient();
    await joined(client);
    const p = client.reconnect('ws://h', 't', 'PA_1', 1, opts);
    const ws = socket();
    ws.open();
    ws.receive(reconnectMessage());
    await p;

    client.sendMuteTrack('TR_1', true);
    client.sendUpdateSubscription(subscription);
    client.sendOffer({ type: 'offer', sdp: 'o' }, 5);
    client.sendAnswer({ type: 'answer', sdp: 'a' }, 6);
    client.sendIceCandidate({ candidate: 'c' }, SignalTarget.PUBLISHER);
    expect(ws.sent.map((m) => m.message.case)).toEqual(['offer', 'answer', 'trickle']);

    client.setReconnected();
    expect(ws.sent.map((m) => m.message.case)).toEqual(['offer', 'answer', 'trickle', 'mute', 'subscription']);
    expect(client.state).toBe(SignalState.Connected);
  });

  it('a new join starts with nothing held from the old session', async () => {
    const client = new SignalClient();
    await joined(client);
    const p1 = client.reconnect('ws://h', 't', 'PA_1', 1, opts);
    socket().open();
    socket().receive(reconnectMessage());
    await p1;
    client.sendMuteTrack('TR_old', true);

    const p2 = client.join('ws://h', 't', opts, () => {});
    socket().open();
    socket().receive(joinMessage());
    await p2;
    client.setReconnected();
    expect(socket().sent.map((m) => m.message.case)).toEqual([]);
  });
});

describe('messages', () => {
  it('an answer echoes the ID it is given', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    client.sendAnswer({ type: 'answer', sdp: 'a' }, 41);
    const m = ws.sent[0].message;
    expect(m.case).toBe('answer');
    expect(m.case === 'answer' && m.value.id).toBe(41);
  });

  it('a malformed candidate is dropped without costing the connection', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    const lost = vi.fn();
    const trickle = vi.fn();
    client.handlers.onClose = lost;
    client.handlers.onTrickle = trickle;
    ws.receive({ case: 'trickle', value: create(TrickleRequestSchema, { candidateInit: '{not json' }) });
    expect(trickle).not.toHaveBeenCalled();
    expect(lost).not.toHaveBeenCalled();
    ws.receive({ case: 'trickle', value: create(TrickleRequestSchema, { candidateInit: '{"candidate":"c"}', target: SignalTarget.SUBSCRIBER }) });
    expect(trickle).toHaveBeenCalledWith({ candidate: 'c' }, SignalTarget.SUBSCRIBER);
  });

  it('accepts JSON frames, ignoring fields it does not know', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    const token = vi.fn();
    client.handlers.onRefreshToken = token;
    ws.onmessage?.({ data: JSON.stringify({ refreshToken: 'abc', somethingNew: 1 }) });
    expect(token).toHaveBeenCalledWith('abc');
  });

  it('an unreadable frame counts as losing the connection', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    const lost = vi.fn();
    client.handlers.onClose = lost;
    ws.onmessage?.({ data: new Uint8Array([0xff, 0xff, 0xff]).buffer });
    expect(lost).toHaveBeenCalled();
  });

  it('a socket that closes reports the loss, whatever its close code', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    const lost = vi.fn();
    client.handlers.onClose = lost;
    ws.serverClose(1000);
    expect(lost).toHaveBeenCalledTimes(1);
    expect(client.state).toBe(SignalState.Disconnected);
  });

  it('closing on purpose reports nothing, and the leave goes out first', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    const lost = vi.fn();
    client.handlers.onClose = lost;
    client.sendLeave();
    await client.close();
    expect(ws.sent.map((m) => m.message.case)).toEqual(['leave']);
    expect(ws.closed).toBe(true);
    expect(lost).not.toHaveBeenCalled();
  });
});

describe('ping', () => {
  it('sends both forms on schedule', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    await vi.advanceTimersByTimeAsync(5000);
    expect(ws.sent.map((m) => m.message.case)).toEqual(['ping', 'pingReq']);
    await vi.advanceTimersByTimeAsync(5000);
    expect(ws.sent).toHaveLength(4);
  });

  it('measures the round trip from a pong', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    await vi.advanceTimersByTimeAsync(5000);
    const sent = ws.sent[1].message;
    const stamp = sent.case === 'pingReq' ? sent.value.timestamp : 0n;
    await vi.advanceTimersByTimeAsync(40);
    ws.receive({ case: 'pongResp', value: { $typeName: 'livekit.Pong', lastPingTimestamp: stamp, timestamp: 0n } });
    expect(client.rtt).toBe(40);
  });

  it('silence for the timeout counts as losing the connection', async () => {
    const client = new SignalClient();
    await joined(client);
    const lost = vi.fn();
    client.handlers.onClose = lost;
    await vi.advanceTimersByTimeAsync(14_999);
    expect(lost).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(lost).toHaveBeenCalledWith('ping timeout');
  });

  it('any message at all proves the connection is alive', async () => {
    const client = new SignalClient();
    const ws = await joined(client);
    const lost = vi.fn();
    client.handlers.onClose = lost;
    await vi.advanceTimersByTimeAsync(14_000);
    ws.receive({ case: 'refreshToken', value: 'x' });
    await vi.advanceTimersByTimeAsync(14_000);
    expect(lost).not.toHaveBeenCalled();
  });

  it('with no schedule from the server, sends nothing', async () => {
    const client = new SignalClient();
    const p = client.join('ws://h', 't', opts, () => {});
    socket().open();
    socket().receive(joinMessage({ pingInterval: 0, pingTimeout: 0 }));
    await p;
    await vi.advanceTimersByTimeAsync(60_000);
    expect(socket().sent).toEqual([]);
  });
});

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { PCTransport } from '../src/engine/PCTransport.js';

/** Just enough of a peer connection to follow the signalling state machine. */
class FakePC {
  signalingState: RTCSignalingState = 'stable';
  connectionState: RTCPeerConnectionState = 'new';
  localDescription: RTCSessionDescriptionInit | null = null;
  remoteDescription: RTCSessionDescriptionInit | null = null;
  added: RTCIceCandidateInit[] = [];
  offers = 0;
  rejectEdited = false;
  onicecandidate: unknown = null;
  onconnectionstatechange: (() => void) | null = null;
  ontrack: unknown = null;
  ondatachannel: unknown = null;

  async createOffer(opts?: { iceRestart?: boolean }) {
    this.offers++;
    return { type: 'offer', sdp: `offer-${this.offers}${opts?.iceRestart ? '-restart' : ''}\r\n` } as RTCSessionDescriptionInit;
  }
  async createAnswer() {
    return { type: 'answer', sdp: 'v=0\r\n' } as RTCSessionDescriptionInit;
  }
  async setLocalDescription(d: RTCSessionDescriptionInit) {
    if (d.type === 'rollback') {
      this.signalingState = 'stable';
      return;
    }
    if (this.rejectEdited && d.sdp?.includes('edited')) throw new Error('rejected');
    this.localDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-local-offer' : 'stable';
  }
  async setRemoteDescription(d: RTCSessionDescriptionInit) {
    this.remoteDescription = d;
    this.signalingState = d.type === 'offer' ? 'have-remote-offer' : 'stable';
  }
  async addIceCandidate(c: RTCIceCandidateInit) {
    this.added.push(c);
  }
  close() {
    this.connectionState = 'closed';
  }
  setState(s: RTCPeerConnectionState) {
    this.connectionState = s;
    this.onconnectionstatechange?.();
  }
}

function make() {
  const pc = new FakePC();
  const t = new PCTransport({}, () => pc as unknown as RTCPeerConnection);
  const offers: Array<{ sdp?: string; id: number }> = [];
  const answered: number[] = [];
  t.on('offer', (o, id) => offers.push({ sdp: o.sdp, id }));
  t.on('offerAnswered', (id) => answered.push(id));
  return { pc, t, offers, answered };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe('publisher offers', () => {
  it('count from one, and calls close together become one offer', async () => {
    const { t, offers } = make();
    t.negotiate();
    t.negotiate();
    t.negotiate();
    await vi.advanceTimersByTimeAsync(20);
    expect(offers.map((o) => o.id)).toEqual([1]);
  });

  it('wait for the answer to the last one, then go out again', async () => {
    const { pc, t, offers, answered } = make();
    await t.createAndSendOffer();
    await t.createAndSendOffer();
    expect(offers).toHaveLength(1);
    await t.handleAnswer('answer', 1);
    await vi.advanceTimersByTimeAsync(0);
    expect(answered).toEqual([1]);
    expect(offers.map((o) => o.id)).toEqual([1, 2]);
    expect(pc.signalingState).toBe('have-local-offer');
  });

  it('fall back to the original if the browser refuses an edit', async () => {
    const { pc, t, offers } = make();
    pc.rejectEdited = true;
    t.offerMunger = (sdp) => `edited-${sdp}`;
    await t.createAndSendOffer();
    expect(offers[0].sdp).toBe('offer-1\r\n');
    expect(pc.localDescription?.sdp).toBe('offer-1\r\n');
  });

  it('an ICE restart replaces an offer that was never answered', async () => {
    const { pc, t, offers } = make();
    await t.createAndSendOffer();
    await t.createAndSendOffer({ iceRestart: true });
    expect(offers.map((o) => o.id)).toEqual([1, 2]);
    expect(offers[1].sdp).toContain('restart');
    expect(pc.signalingState).toBe('have-local-offer');
  });
});

describe('answers', () => {
  it('an answer with the offer ID applies and counts as answering it', async () => {
    const { pc, t, answered } = make();
    await t.createAndSendOffer();
    await t.handleAnswer('a1', 1);
    expect(pc.remoteDescription?.sdp).toBe('a1');
    expect(answered).toEqual([1]);
  });

  it('an answer to an older offer is ignored', async () => {
    const { pc, t, answered } = make();
    await t.createAndSendOffer();
    await t.handleAnswer('a1', 1);
    await t.createAndSendOffer();
    await t.handleAnswer('stale', 1);
    expect(pc.remoteDescription?.sdp).toBe('a1');
    expect(pc.signalingState).toBe('have-local-offer');
    expect(answered).toEqual([1]);
    await t.handleAnswer('a2', 2);
    expect(answered).toEqual([1, 2]);
  });

  it('an answer with no ID counts as answering what is outstanding', async () => {
    // A server that does not track IDs would otherwise leave every publish waiting for ever.
    const { pc, t, answered } = make();
    await t.createAndSendOffer();
    await t.handleAnswer('a', 0);
    expect(pc.signalingState).toBe('stable');
    expect(answered).toEqual([1]);
  });

  it('an answer with nothing outstanding is ignored', async () => {
    const { pc, t, answered } = make();
    await t.createAndSendOffer();
    await t.handleAnswer('a1', 1);
    await t.handleAnswer('again', 1);
    await t.handleAnswer('zero', 0);
    expect(pc.remoteDescription?.sdp).toBe('a1');
    expect(answered).toEqual([1]);
  });

  it('an answer to a replaced offer does not satisfy the restart', async () => {
    const { pc, t, answered } = make();
    await t.createAndSendOffer();
    await t.createAndSendOffer({ iceRestart: true });
    await t.handleAnswer('late', 1);
    expect(pc.signalingState).toBe('have-local-offer');
    expect(answered).toEqual([]);
    await t.handleAnswer('good', 2);
    expect(answered).toEqual([2]);
  });
});

describe('candidates', () => {
  it('wait for a remote description', async () => {
    const { pc, t } = make();
    await t.addIceCandidate({ candidate: 'a' });
    expect(pc.added).toEqual([]);
    await t.handleOffer('v=0\r\na=ice-ufrag:u1\r\n');
    expect(pc.added).toEqual([{ candidate: 'a' }]);
    await t.addIceCandidate({ candidate: 'b' });
    expect(pc.added).toEqual([{ candidate: 'a' }, { candidate: 'b' }]);
  });

  it('on the side that offers, wait out an ICE restart', async () => {
    const { pc, t } = make();
    await t.createAndSendOffer();
    await t.handleAnswer('a1', 1);
    await t.createAndSendOffer({ iceRestart: true });
    await t.addIceCandidate({ candidate: 'new' });
    expect(pc.added).toEqual([]);
    await t.handleAnswer('a2', 2);
    expect(pc.added).toEqual([{ candidate: 'new' }]);
  });

  it('sent just ahead of a restart offer from the server are applied again after it', async () => {
    const { pc, t } = make();
    await t.handleOffer('v=0\r\na=ice-ufrag:u1\r\n');
    await t.addIceCandidate({ candidate: 'late-for-old' });
    await t.handleOffer('v=0\r\na=ice-ufrag:u2\r\n');
    expect(pc.added.filter((c) => c.candidate === 'late-for-old')).toHaveLength(2);
  });

  it('are not applied again for an offer that did not restart', async () => {
    const { pc, t } = make();
    await t.handleOffer('v=0\r\na=ice-ufrag:u1\r\n');
    await t.addIceCandidate({ candidate: 'c' });
    await t.handleOffer('v=0\r\na=ice-ufrag:u1\r\n');
    expect(pc.added).toHaveLength(1);
  });
});

describe('waitForConnected', () => {
  it('resolves on connected', async () => {
    const { pc, t } = make();
    const p = t.waitForConnected(1000, 'x');
    pc.setState('connected');
    await expect(p).resolves.toBeUndefined();
  });
  it('rejects on timeout', async () => {
    const { t } = make();
    const p = t.waitForConnected(1000, 'x');
    const assertion = expect(p).rejects.toThrow(/timed out/);
    await vi.advanceTimersByTimeAsync(1000);
    await assertion;
  });
  it('fails early when nothing can change the outcome', async () => {
    const { pc, t } = make();
    const p = t.waitForConnected(1000, 'x');
    pc.setState('failed');
    await expect(p).rejects.toThrow(/failed/);
  });
  it('during a resume, a failed connection can still come back', async () => {
    const { pc, t } = make();
    pc.setState('failed');
    const p = t.waitForConnected(1000, 'x', { failFast: false });
    pc.setState('connecting');
    pc.setState('connected');
    await expect(p).resolves.toBeUndefined();
  });
});

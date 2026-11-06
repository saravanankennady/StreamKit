import { log } from '../logger.js';
import { Mutex } from '../util/async.js';
import { TypedEmitter } from '../util/emitter.js';
import { applyAudioFeaturesToAnswer, iceUfrag, readOfferedAudioFeatures } from './sdp.js';

const OFFER_DEBOUNCE_MS = 20;
// Enough for the host candidates of an ICE generation, which is all a re-add has to recover.
const MAX_RECENT_CANDIDATES = 64;

type Events = {
  /** A local offer is ready to send, with the ID its answer must carry. */
  offer: (offer: RTCSessionDescriptionInit, id: number) => void;
  offerAnswered: (id: number) => void;
  iceCandidate: (candidate: RTCIceCandidateInit) => void;
  connectionStateChange: (state: RTCPeerConnectionState) => void;
  negotiationFailed: (error: unknown) => void;
  track: (ev: RTCTrackEvent) => void;
  dataChannel: (channel: RTCDataChannel) => void;
};

/** Edits an offer before it is applied. It can inspect the description's sections and has to be fast. */
export type OfferMunger = (sdp: string) => string;

/**
 * One RTCPeerConnection, and the negotiation rules on top of it. The publisher connection offers and the
 * subscriber connection answers; a transport only ever does one of those.
 */
export class PCTransport extends TypedEmitter<Events> {
  readonly pc: RTCPeerConnection;
  /** Applied to each outgoing offer. Failure to apply falls back to the unedited offer. */
  offerMunger?: OfferMunger;
  latestOfferId = 0;

  private lock = new Mutex();
  private pendingCandidates: RTCIceCandidateInit[] = [];
  private recentCandidates: RTCIceCandidateInit[] = [];
  private restartingIce = false;
  private renegotiate = false;
  private answeredOfferId = 0;
  private negotiateTimer?: ReturnType<typeof setTimeout>;
  private closed = false;

  constructor(config: RTCConfiguration, createPC: (c: RTCConfiguration) => RTCPeerConnection = (c) => new RTCPeerConnection(c)) {
    super();
    this.pc = createPC(config);
    this.pc.onicecandidate = (ev) => {
      // A null candidate means gathering finished. Servers neither expect nor need to hear that.
      if (ev.candidate) this.emit('iceCandidate', ev.candidate.toJSON());
    };
    this.pc.onconnectionstatechange = () => this.emit('connectionStateChange', this.pc.connectionState);
    this.pc.ontrack = (ev) => this.emit('track', ev);
    this.pc.ondatachannel = (ev) => this.emit('dataChannel', ev.channel);
  }

  get connectionState() {
    return this.pc.connectionState;
  }

  get isConnected() {
    return this.pc.connectionState === 'connected';
  }

  get hasLocalDescription() {
    return !!this.pc.localDescription;
  }

  /**
   * Waits for the connection to come up. A failed connection ends the wait early when nothing could change
   * that. During a resume an ICE restart can still revive it, so only the deadline counts then.
   */
  waitForConnected(timeoutMs: number, what: string, opts: { failFast?: boolean } = {}): Promise<void> {
    const failFast = opts.failFast ?? true;
    return new Promise<void>((resolve, reject) => {
      const check = () => {
        const state = this.pc.connectionState;
        if (state === 'connected') finish(resolve);
        else if (state === 'closed' || (failFast && state === 'failed')) finish(() => reject(new Error(`${what} ${state}`)));
      };
      const finish = (done: () => void) => {
        clearTimeout(timer);
        this.off('connectionStateChange', check);
        done();
      };
      const timer = setTimeout(() => finish(() => reject(new Error(`${what} timed out`))), timeoutMs);
      this.on('connectionStateChange', check);
      check();
    });
  }

  /** Schedules an offer. Calls close together become one, since each change needs no offer of its own. */
  negotiate() {
    if (this.negotiateTimer || this.closed) return;
    this.negotiateTimer = setTimeout(() => {
      this.negotiateTimer = undefined;
      this.createAndSendOffer().catch((e) => this.emit('negotiationFailed', e));
    }, OFFER_DEBOUNCE_MS);
  }

  createAndSendOffer(opts: { iceRestart?: boolean } = {}): Promise<void> {
    return this.lock.run(async () => {
      if (this.closed) return;
      if (opts.iceRestart) this.restartingIce = true;

      if (this.pc.signalingState === 'have-local-offer') {
        if (!opts.iceRestart) {
          // Only one offer can be outstanding. This one goes out when the answer arrives.
          this.renegotiate = true;
          return;
        }
        // The connection the offer was sent on is gone, and its answer may never come. Nothing else can
        // proceed from this state, so the exchange is abandoned.
        await this.pc.setLocalDescription({ type: 'rollback' });
      }

      const offer = await this.pc.createOffer({ iceRestart: opts.iceRestart });
      const id = ++this.latestOfferId;
      const applied = await this.applyLocal(offer);
      this.emit('offer', applied, id);
    });
  }

  private async applyLocal(desc: RTCSessionDescriptionInit): Promise<RTCSessionDescriptionInit> {
    const munged = desc.type === 'offer' && this.offerMunger ? this.offerMunger(desc.sdp ?? '') : desc.sdp;
    if (munged === desc.sdp) {
      await this.pc.setLocalDescription(desc);
      return desc;
    }
    try {
      const edited = { type: desc.type, sdp: munged };
      await this.pc.setLocalDescription(edited);
      return edited;
    } catch (e) {
      log.warn('browser rejected an edited description, using the original', e);
      await this.pc.setLocalDescription(desc);
      return desc;
    }
  }

  /**
   * Applies the server's answer to a publisher offer.
   *
   * An answer with an ID matches an offer: anything but the newest offer is stale, because a newer answer
   * is on its way. An answer with ID 0 comes from a server that does not track IDs, so it can only be
   * taken as answering whichever offer is outstanding.
   */
  handleAnswer(sdp: string, id: number): Promise<void> {
    return this.lock.run(async () => {
      if (this.closed) return;
      if (id !== 0 && (id !== this.latestOfferId || id === this.answeredOfferId)) {
        log.debug('ignoring answer to an old offer', id, this.latestOfferId);
        return;
      }
      if (this.pc.signalingState !== 'have-local-offer') {
        log.debug('ignoring answer with no offer outstanding', id);
        return;
      }
      await this.pc.setRemoteDescription({ type: 'answer', sdp });
      const answered = this.latestOfferId;
      this.answeredOfferId = answered;
      this.restartingIce = false;
      await this.flushCandidates();
      this.emit('offerAnswered', answered);
      if (this.renegotiate) {
        this.renegotiate = false;
        void this.createAndSendOffer().catch((e) => this.emit('negotiationFailed', e));
      }
    });
  }

  /** Applies a server offer and returns the answer, for the subscriber connection. */
  handleOffer(sdp: string): Promise<RTCSessionDescriptionInit | undefined> {
    return this.lock.run(async () => {
      if (this.closed) return undefined;
      const before = iceUfrag(this.pc.remoteDescription?.sdp);
      await this.pc.setRemoteDescription({ type: 'offer', sdp });
      const restarted = before !== undefined && iceUfrag(sdp) !== before;
      await this.flushCandidates(restarted);

      const answer = await this.pc.createAnswer();
      // The browser ignores what the offer said about stereo and audio NACK unless the answer repeats it.
      const edited = { type: answer.type, sdp: applyAudioFeaturesToAnswer(answer.sdp ?? '', readOfferedAudioFeatures(sdp)) };
      try {
        await this.pc.setLocalDescription(edited);
        return edited;
      } catch (e) {
        log.warn('browser rejected an edited answer, using the original', e);
        await this.pc.setLocalDescription(answer);
        return answer;
      }
    });
  }

  /**
   * Candidates can arrive before the description they belong to, in either direction: the server trickles
   * while its offer is on the way, and a restart makes earlier candidates stale.
   */
  async addIceCandidate(candidate: RTCIceCandidateInit) {
    if (!this.pc.remoteDescription || this.restartingIce) {
      this.pendingCandidates.push(candidate);
      return;
    }
    this.rememberCandidate(candidate);
    await this.applyCandidate(candidate);
  }

  private rememberCandidate(candidate: RTCIceCandidateInit) {
    this.recentCandidates.push(candidate);
    if (this.recentCandidates.length > MAX_RECENT_CANDIDATES) this.recentCandidates.shift();
  }

  private async applyCandidate(candidate: RTCIceCandidateInit) {
    try {
      await this.pc.addIceCandidate(candidate);
    } catch (e) {
      log.debug('could not add ICE candidate', e);
    }
  }

  /**
   * After a server-side restart, candidates sent just ahead of its offer went to the old generation and
   * are lost to the new one, and a resume can leave the new offer with no others to rely on. Adding them
   * again is harmless if they were not needed.
   */
  private async flushCandidates(readdRecent = false) {
    const pending = this.pendingCandidates;
    this.pendingCandidates = [];
    if (readdRecent) {
      for (const c of this.recentCandidates) await this.applyCandidate(c);
    }
    for (const c of pending) {
      this.rememberCandidate(c);
      await this.applyCandidate(c);
    }
  }

  close() {
    this.closed = true;
    clearTimeout(this.negotiateTimer);
    this.negotiateTimer = undefined;
    this.pendingCandidates = [];
    this.recentCandidates = [];
    this.pc.onicecandidate = null;
    this.pc.onconnectionstatechange = null;
    this.pc.ontrack = null;
    this.pc.ondatachannel = null;
    this.pc.close();
    // Anyone still waiting on this connection should hear now, not when their timer runs out.
    this.emit('connectionStateChange', 'closed');
    this.removeAllListeners();
  }
}

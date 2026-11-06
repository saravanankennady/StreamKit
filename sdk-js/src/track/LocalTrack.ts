import { TrackKind } from '../enums.js';
import { TrackInvalidError } from '../errors.js';
import { TrackEvent } from '../events.js';
import { Mutex, sleep } from '../util/async.js';
import { Track } from './Track.js';

const DIMENSIONS_TIMEOUT_MS = 1000;

/** A track captured here, for publishing. */
export abstract class LocalTrack extends Track {
  /** The RTP sender while published. @internal */
  sender?: RTCRtpSender;
  isUpstreamPaused = false;
  /** The ID the server knows this track by, from when it was published. A restart does not change it. */
  cid: string;

  protected constraints: MediaTrackConstraints;
  protected readonly isUserProvided: boolean;
  protected muteLock = new Mutex();
  /** Restarts briefly end the underlying track. Those are not the ended event an application should see. */
  private ownedStop = false;

  protected constructor(mediaTrack: MediaStreamTrack, kind: TrackKind, constraints?: MediaTrackConstraints, userProvided = false) {
    super(mediaTrack, kind);
    this.cid = mediaTrack.id;
    this.constraints = constraints ?? mediaTrack.getConstraints();
    this.isUserProvided = userProvided;
    this._mediaStream = new MediaStream([mediaTrack]);
    this.listenForEnd(mediaTrack);
  }

  get isLocal() {
    return true;
  }

  private listenForEnd(track: MediaStreamTrack) {
    track.addEventListener('ended', () => {
      // Late events from a track already replaced mean nothing.
      if (track === this._mediaStreamTrack && !this.ownedStop) this.emit(TrackEvent.Ended, this);
    });
  }

  /** The capture size, once the browser knows it. */
  get dimensions(): { width: number; height: number } | undefined {
    if (this.kind !== TrackKind.Video) return undefined;
    const { width, height } = this._mediaStreamTrack.getSettings();
    return width && height ? { width, height } : undefined;
  }

  /** Capture size takes a moment to appear for some sources, and encodings depend on it. */
  async waitForDimensions(timeoutMs = DIMENSIONS_TIMEOUT_MS): Promise<{ width: number; height: number } | undefined> {
    if (this.kind !== TrackKind.Video) return undefined;
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const d = this.dimensions;
      if (d) return d;
      await sleep(50);
    }
    return this.dimensions;
  }

  /** Stops sending media. The browser sends silence or black until unmuted, and the room hears of it. */
  mute(): Promise<void> {
    return this.muteLock.run(async () => {
      if (!this.isMuted) await this.applyMute();
    });
  }

  unmute(): Promise<void> {
    // Unmuting a camera opens it again, so asking for what is already the case must not do that.
    return this.muteLock.run(async () => {
      if (this.isMuted) await this.applyUnmute();
    });
  }

  protected async applyMute() {
    this._mediaStreamTrack.enabled = false;
    this.setMuted(true);
  }

  protected async applyUnmute() {
    this._mediaStreamTrack.enabled = true;
    this.setMuted(false);
  }

  getDeviceId(): string | undefined {
    return this._mediaStreamTrack.getSettings().deviceId;
  }

  /** Switches to another capture device. Returns whether the track is on it now. */
  async setDeviceId(deviceId: ConstrainDOMString): Promise<boolean> {
    if (this.constraints.deviceId === deviceId && this._mediaStreamTrack.readyState === 'live') return true;
    const previous = this.constraints;
    this.constraints = { ...previous, deviceId };
    if (this.isMuted && this.kind === TrackKind.Video) return true;
    try {
      await this.restart();
    } catch (e) {
      // Restarting stops the old capture first, so a failure here would leave nothing being sent.
      this.constraints = previous;
      await this.restart();
      throw e;
    }
    const want = typeof deviceId === 'string' ? deviceId : Array.isArray(deviceId) ? deviceId[0] : (deviceId as ConstrainDOMStringParameters).exact ?? (deviceId as ConstrainDOMStringParameters).ideal;
    const wanted = Array.isArray(want) ? want[0] : want;
    return wanted === undefined || wanted === 'default' || this.getDeviceId() === wanted;
  }

  /** Captures again, and swaps the new media in without renegotiating. */
  async restart(constraints?: MediaTrackConstraints): Promise<void> {
    if (this.isUserProvided) throw new TrackInvalidError('cannot restart a track the application captured itself');
    const wanted = constraints ?? this.constraints;
    // Stopping first matters on phones, where a camera cannot be opened twice at once.
    this.ownedStop = true;
    this._mediaStreamTrack.stop();
    try {
      const stream = await navigator.mediaDevices.getUserMedia(this.kind === TrackKind.Audio ? { audio: wanted } : { video: wanted });
      const [track] = stream.getTracks();
      this.constraints = wanted;
      await this.setMediaStreamTrack(track);
    } finally {
      this.ownedStop = false;
    }
    this.emit(TrackEvent.Restarted, this);
  }

  /** Publishes different media under this track, such as a processed version. */
  async replaceTrack(track: MediaStreamTrack): Promise<void> {
    await this.setMediaStreamTrack(track);
    this.emit(TrackEvent.Restarted, this);
  }

  protected async setMediaStreamTrack(track: MediaStreamTrack) {
    track.enabled = !this.isMuted;
    this._mediaStreamTrack = track;
    this._mediaStream = new MediaStream([track]);
    this.listenForEnd(track);
    this.reattach();
    await this.sender?.replaceTrack(this.isUpstreamPaused ? null : track);
  }

  /** Stops sending without touching capture, so a local preview keeps running. */
  async pauseUpstream() {
    if (this.isUpstreamPaused) return;
    this.isUpstreamPaused = true;
    await this.sender?.replaceTrack(null);
  }

  async resumeUpstream() {
    if (!this.isUpstreamPaused) return;
    this.isUpstreamPaused = false;
    await this.sender?.replaceTrack(this._mediaStreamTrack);
  }

  /** Stops capture. The track cannot be used again without a restart. */
  stop() {
    this.ownedStop = true;
    this._mediaStreamTrack.stop();
  }

  getRTCStatsReport(): Promise<RTCStatsReport | undefined> {
    return this.sender ? this.sender.getStats() : Promise.resolve(undefined);
  }
}

import { TrackKind } from '../enums.js';
import { TrackEvent } from '../events.js';
import { Track } from './Track.js';

/** A track received from another participant. */
export abstract class RemoteTrack extends Track {
  /** @internal */
  receiver?: RTCRtpReceiver;

  private stream: MediaStream;
  private onRemoveTrack: (ev: MediaStreamTrackEvent) => void;

  protected constructor(mediaTrack: MediaStreamTrack, sid: string, kind: TrackKind, receiver: RTCRtpReceiver, stream: MediaStream) {
    super(mediaTrack, kind);
    this.sid = sid;
    this.receiver = receiver;
    this.stream = stream;
    this._mediaStream = stream;
    // This is how a server ends a subscription: it renegotiates the section away, the browser takes the
    // track out of the stream. A stream can carry other tracks, so the event has to be for this one.
    this.onRemoveTrack = (ev) => {
      if (ev.track === this._mediaStreamTrack) this.emit(TrackEvent.Ended, this);
    };
    stream.addEventListener('removetrack', this.onRemoveTrack);
  }

  get isLocal() {
    return false;
  }

  /**
   * Stops playback here. The media track itself keeps running, on purpose: the browser owns it, and a
   * server can reuse its transceiver for another track later, which a stopped one could never receive.
   * @internal
   */
  stop() {
    this.stream.removeEventListener('removetrack', this.onRemoveTrack);
    this.detach();
  }

  getRTCStatsReport(): Promise<RTCStatsReport | undefined> {
    return this.receiver ? this.receiver.getStats() : Promise.resolve(undefined);
  }
}

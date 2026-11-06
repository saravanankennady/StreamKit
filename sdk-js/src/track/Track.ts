import { StreamState, TrackKind, TrackSource } from '../enums.js';
import { TrackEvent } from '../events.js';
import { log } from '../logger.js';
import { TypedEmitter } from '../util/emitter.js';

export type TrackEvents = {
  muted: (track: Track) => void;
  unmuted: (track: Track) => void;
  restarted: (track: Track) => void;
  ended: (track: Track) => void;
  audioPlaybackStarted: () => void;
  audioPlaybackFailed: (error: unknown) => void;
  videoPlaybackStarted: () => void;
  videoPlaybackFailed: (error: unknown) => void;
  elementAttached: (element: HTMLMediaElement) => void;
  elementDetached: (element: HTMLMediaElement) => void;
  visibilityChanged: (visible: boolean, track: Track) => void;
  videoDimensionsChanged: (dimensions: { width: number; height: number }, track: Track) => void;
  streamStateChanged: (state: StreamState) => void;
};

/** A media track, and the elements playing it. */
export abstract class Track extends TypedEmitter<TrackEvents> {
  static readonly Kind = TrackKind;
  static readonly Source = TrackSource;
  static readonly StreamState = StreamState;

  readonly kind: TrackKind;
  attachedElements: HTMLMediaElement[] = [];
  isMuted = false;
  source: TrackSource = TrackSource.Unknown;
  /** The server's ID for this track, once published or subscribed. */
  sid?: string;
  streamState: StreamState = StreamState.Active;

  protected _mediaStreamTrack: MediaStreamTrack;
  protected _mediaStream?: MediaStream;

  protected constructor(mediaTrack: MediaStreamTrack, kind: TrackKind) {
    super();
    this._mediaStreamTrack = mediaTrack;
    this.kind = kind;
  }

  abstract get isLocal(): boolean;

  get mediaStreamTrack() {
    return this._mediaStreamTrack;
  }

  get mediaStream() {
    return this._mediaStream;
  }

  /** Plays this track in an element, creating one if none is given. Returns the element. */
  attach(): HTMLMediaElement;
  attach(element: HTMLMediaElement): HTMLMediaElement;
  attach(element?: HTMLMediaElement): HTMLMediaElement {
    const el = element ?? (document.createElement(this.kind === TrackKind.Video ? 'video' : 'audio') as HTMLMediaElement);
    if (!this.attachedElements.includes(el)) this.attachedElements.push(el);
    attachTrackToElement(this._mediaStreamTrack, el);
    this.playElement(el);
    this.emit(TrackEvent.ElementAttached, el);
    return el;
  }

  protected playElement(el: HTMLMediaElement) {
    const failed = this.kind === TrackKind.Audio ? TrackEvent.AudioPlaybackFailed : TrackEvent.VideoPlaybackFailed;
    const started = this.kind === TrackKind.Audio ? TrackEvent.AudioPlaybackStarted : TrackEvent.VideoPlaybackStarted;
    el.play()
      .then(() => this.emit(started))
      .catch((e) => {
        // An abort only means the element was given something else to do before it could start.
        if (e?.name === 'NotAllowedError') this.emit(failed, e);
        else log.debug('element did not start playing', e);
      });
  }

  /** Stops playing in one element, or in every element if none is given. */
  detach(): HTMLMediaElement[];
  detach(element: HTMLMediaElement): HTMLMediaElement;
  detach(element?: HTMLMediaElement): HTMLMediaElement | HTMLMediaElement[] {
    if (element) {
      detachTrackFromElement(this._mediaStreamTrack, element);
      this.attachedElements = this.attachedElements.filter((e) => e !== element);
      this.emit(TrackEvent.ElementDetached, element);
      return element;
    }
    const all = this.attachedElements;
    this.attachedElements = [];
    for (const el of all) {
      detachTrackFromElement(this._mediaStreamTrack, el);
      this.emit(TrackEvent.ElementDetached, el);
    }
    return all;
  }

  /** @internal */
  setStreamState(state: StreamState) {
    if (this.streamState === state) return;
    this.streamState = state;
    this.emit(TrackEvent.StreamStateChanged, state);
  }

  /** Moves playing elements to a new underlying track, so applications keep the elements they hold. */
  protected reattach() {
    for (const el of this.attachedElements) {
      attachTrackToElement(this._mediaStreamTrack, el);
      this.playElement(el);
    }
  }

  /** @internal */
  setMuted(muted: boolean) {
    if (this.isMuted === muted) return;
    this.isMuted = muted;
    this.emit(muted ? TrackEvent.Muted : TrackEvent.Unmuted, this);
  }
}

function attachTrackToElement(track: MediaStreamTrack, el: HTMLMediaElement) {
  // An element can hold a camera and a microphone together, and each attach replaces only its own kind.
  let stream = el.srcObject instanceof MediaStream ? el.srcObject : undefined;
  if (!stream) stream = new MediaStream();
  const existing = track.kind === 'audio' ? stream.getAudioTracks() : stream.getVideoTracks();
  if (!existing.includes(track)) {
    for (const t of existing) stream.removeTrack(t);
    stream.addTrack(track);
  }
  if (el.srcObject !== stream) el.srcObject = stream;
  el.autoplay = true;
  if (el instanceof HTMLVideoElement) {
    el.playsInline = true;
    // A silent video can start on its own. With sound it would need a user gesture.
    if (stream.getAudioTracks().length === 0) el.muted = true;
  }
}

function detachTrackFromElement(track: MediaStreamTrack, el: HTMLMediaElement) {
  const stream = el.srcObject;
  if (stream instanceof MediaStream) {
    stream.removeTrack(track);
    if (stream.getTracks().length === 0) el.srcObject = null;
  }
}

export function kindFromMediaTrack(track: MediaStreamTrack): TrackKind {
  return track.kind === 'audio' ? TrackKind.Audio : track.kind === 'video' ? TrackKind.Video : TrackKind.Unknown;
}

import { TrackKind } from '../enums.js';
import { LocalTrack } from './LocalTrack.js';

export class LocalAudioTrack extends LocalTrack {
  /** Stop the microphone while muted, turning off the browser's indicator. */
  stopOnMute = false;

  constructor(mediaTrack: MediaStreamTrack, constraints?: MediaTrackConstraints, userProvided = false, stopOnMute = false) {
    super(mediaTrack, TrackKind.Audio, constraints, userProvided);
    this.stopOnMute = stopOnMute;
  }

  protected override async applyMute() {
    if (this.stopOnMute && !this.isUserProvided) this.stop();
    await super.applyMute();
  }

  protected override async applyUnmute() {
    if (this.stopOnMute && !this.isUserProvided && this._mediaStreamTrack.readyState === 'ended') {
      await this.restart();
    }
    await super.applyUnmute();
  }
}

import { TrackKind, TrackSource } from '../enums.js';
import { log } from '../logger.js';
import { applyQualitiesToEncodings, normalizeCodecName, type QualitySetting } from '../media/dynacast.js';
import { isSvcCodec } from '../media/encodings.js';
import { Mutex } from '../util/async.js';
import { isFirefox } from '../util/browser.js';
import { LocalTrack } from './LocalTrack.js';
import type { VideoCodec } from './options.js';

export interface SubscribedCodecQualities {
  codec: string;
  qualities: QualitySetting[];
}

export class LocalVideoTrack extends LocalTrack {
  /** @internal */
  codec?: VideoCodec;
  /** @internal */
  encodings?: RTCRtpEncodingParameters[];

  private latestQualities?: QualitySetting[];
  // Two overlapping parameter changes make the browser refuse the second.
  private layerLock = new Mutex();

  constructor(mediaTrack: MediaStreamTrack, constraints?: MediaTrackConstraints, userProvided = false) {
    super(mediaTrack, TrackKind.Video, constraints, userProvided);
  }

  // A stopped camera turns its indicator off, which is what people expect of muting a camera.
  private get stopsOnMute() {
    return this.source === TrackSource.Camera && !this.isUserProvided;
  }

  protected override async applyMute() {
    if (this.stopsOnMute) this.stop();
    await super.applyMute();
  }

  protected override async applyUnmute() {
    if (this.stopsOnMute) {
      await this.restart();
    }
    await super.applyUnmute();
  }

  /**
   * Stops and starts encoding layers according to what subscribers watch. An update names a codec, and only
   * the one this track is sending applies.
   */
  async setPublishingCodecs(codecs: SubscribedCodecQualities[]) {
    const mine = codecs.find((c) => normalizeCodecName(c.codec) === this.codec);
    if (!mine) {
      log.debug('subscribed quality update does not name this codec', this.codec, codecs.map((c) => c.codec));
      return;
    }
    await this.setPublishingLayers(mine.qualities);
  }

  async setPublishingLayers(qualities: QualitySetting[]) {
    this.latestQualities = qualities;
    await this.applyPublishingLayers(qualities);
  }

  /** Applying an answer can turn encodings back on. Whatever was last asked for has to be put back. */
  async reapplyPublishingLayers() {
    if (this.latestQualities) await this.applyPublishingLayers(this.latestQualities);
  }

  private applyPublishingLayers(qualities: QualitySetting[]) {
    return this.layerLock.run(async () => {
      const sender = this.sender;
      // Firefox cannot switch off an individual encoding, so it keeps sending all of them.
      if (!sender || isFirefox()) return;
      const params = sender.getParameters();
      if (!params.encodings || params.encodings.length === 0) return;
      const changed = applyQualitiesToEncodings(params.encodings, qualities, isSvcCodec(this.codec));
      if (!changed) return;
      try {
        await sender.setParameters(params);
      } catch (e) {
        log.warn('could not change published layers', e);
      }
    });
  }
}

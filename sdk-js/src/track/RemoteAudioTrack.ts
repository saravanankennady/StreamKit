import { TrackKind } from '../enums.js';
import { RemoteTrack } from './RemoteTrack.js';

interface SinkElement extends HTMLMediaElement {
  setSinkId(id: string): Promise<void>;
}

export class RemoteAudioTrack extends RemoteTrack {
  private volume?: number;
  private sinkId?: string;

  constructor(mediaTrack: MediaStreamTrack, sid: string, receiver: RTCRtpReceiver, stream: MediaStream, sinkId?: string) {
    super(mediaTrack, sid, TrackKind.Audio, receiver, stream);
    this.sinkId = sinkId;
  }

  override attach(): HTMLMediaElement;
  override attach(element: HTMLMediaElement): HTMLMediaElement;
  override attach(element?: HTMLMediaElement): HTMLMediaElement {
    const el = element ? super.attach(element) : super.attach();
    if (this.volume !== undefined) el.volume = this.volume;
    if (this.sinkId !== undefined && 'setSinkId' in el) void (el as SinkElement).setSinkId(this.sinkId).catch(() => {});
    return el;
  }

  setVolume(volume: number) {
    this.volume = volume;
    for (const el of this.attachedElements) el.volume = volume;
  }

  getVolume(): number {
    return this.volume ?? this.attachedElements[0]?.volume ?? 1;
  }

  /** Plays through a given output device, on browsers that can choose. */
  async setSinkId(deviceId: string) {
    this.sinkId = deviceId;
    await Promise.all(
      this.attachedElements.filter((el) => 'setSinkId' in el).map((el) => (el as SinkElement).setSinkId(deviceId)),
    );
  }
}

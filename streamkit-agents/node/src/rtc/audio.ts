// Audio inside the framework is 16-bit PCM. Opus is at the WebRTC edge.
import { Decoder, Encoder } from '@evan/opus';
import { MediaStreamTrack, RtpHeader, RtpPacket } from 'werift';

export const OUTPUT_RATE = 48000;
const FRAME_MS = 20;
const FRAME_SAMPLES = (OUTPUT_RATE * FRAME_MS) / 1000;
const OPUS_PAYLOAD_TYPE = 111;
const MAX_LOOP_STALL_MS = 200;

export class AudioFrame {
  readonly data: Int16Array;
  readonly sampleRate: number;
  readonly numChannels: number;

  constructor(data: Int16Array, sampleRate: number, numChannels = 1) {
    this.data = data;
    this.sampleRate = sampleRate;
    this.numChannels = numChannels;
  }

  get samplesPerChannel(): number {
    return this.data.length / this.numChannels;
  }

  get duration(): number {
    return this.samplesPerChannel / this.sampleRate;
  }

  static fromBuffer(buffer: Buffer, sampleRate: number, numChannels = 1): AudioFrame {
    const copy = new Int16Array(Math.floor(buffer.length / 2));
    for (let i = 0; i < copy.length; i++) copy[i] = buffer.readInt16LE(i * 2);
    return new AudioFrame(copy, sampleRate, numChannels);
  }

  toBuffer(): Buffer {
    const out = Buffer.alloc(this.data.length * 2);
    for (let i = 0; i < this.data.length; i++) out.writeInt16LE(this.data[i], i * 2);
    return out;
  }

  static concat(frames: AudioFrame[]): AudioFrame {
    const total = frames.reduce((sum, f) => sum + f.data.length, 0);
    const data = new Int16Array(total);
    let at = 0;
    for (const f of frames) {
      data.set(f.data, at);
      at += f.data.length;
    }
    return new AudioFrame(data, frames[0].sampleRate, frames[0].numChannels);
  }
}

export function rmsLevel(data: Int16Array): number {
  if (data.length === 0) return 0;
  let sum = 0;
  for (let i = 0; i < data.length; i++) {
    const s = data[i] / 32768;
    sum += s * s;
  }
  return Math.sqrt(sum / data.length);
}

/**
 * Converts a stream of mono frames to one rate. It low-pass filters with a windowed sinc, which is what going down in rate
 * needs, and keeps its place between calls, so one instance goes with one stream.
 */
export class Resampler {
  readonly outRate: number;
  private inRate = 0;
  private position = 0;
  private history = new Float64Array(0);
  private static readonly TAPS = 16;

  constructor(outRate: number) {
    this.outRate = outRate;
  }

  push(frame: AudioFrame): AudioFrame {
    const mono = toMono(frame);
    if (frame.sampleRate === this.outRate) return new AudioFrame(mono, this.outRate);
    if (frame.sampleRate !== this.inRate) {
      this.inRate = frame.sampleRate;
      this.position = 0;
      this.history = new Float64Array(Resampler.TAPS * 2);
    }
    const ratio = this.inRate / this.outRate;
    const cutoff = Math.min(1, 1 / ratio);
    const halfWidth = Math.ceil(Resampler.TAPS / cutoff);
    if (this.history.length !== halfWidth * 2) this.history = new Float64Array(halfWidth * 2);
    const combined = new Float64Array(this.history.length + mono.length);
    combined.set(this.history);
    combined.set(mono, this.history.length);
    const out: number[] = [];
    // position counts from the start of the new samples. The history in front gives the filter its left side.
    let at = this.position;
    while (at + halfWidth < mono.length) {
      const centre = at + this.history.length;
      const base = Math.floor(centre);
      let sum = 0;
      let weight = 0;
      for (let i = base - halfWidth + 1; i <= base + halfWidth; i++) {
        const x = centre - i;
        const w = sinc(x * cutoff) * blackman(x / halfWidth);
        sum += combined[i] * w;
        weight += w;
      }
      out.push(Math.max(-32768, Math.min(32767, Math.round(sum / weight))));
      at += ratio;
    }
    this.position = at - mono.length;
    this.history = combined.slice(combined.length - this.history.length);
    return new AudioFrame(Int16Array.from(out), this.outRate);
  }
}

function sinc(x: number): number {
  if (x === 0) return 1;
  return Math.sin(Math.PI * x) / (Math.PI * x);
}

function blackman(x: number): number {
  const a = Math.abs(x);
  if (a >= 1) return 0;
  return 0.42 + 0.5 * Math.cos(Math.PI * a) + 0.08 * Math.cos(2 * Math.PI * a);
}

function toMono(frame: AudioFrame): Int16Array {
  if (frame.numChannels === 1) return frame.data;
  const out = new Int16Array(frame.samplesPerChannel);
  for (let i = 0; i < out.length; i++) {
    let sum = 0;
    for (let c = 0; c < frame.numChannels; c++) sum += frame.data[i * frame.numChannels + c];
    out[i] = Math.round(sum / frame.numChannels);
  }
  return out;
}

/**
 * A local audio track. Frames pushed in go out in real time as Opus over RTP, with silence when there is nothing to send. This
 * is where the framework knows what was really played, and where audio can be held back or dropped within one frame, which is
 * what interruptions need.
 */
export class AudioSource {
  readonly track = new MediaStreamTrack({ kind: 'audio' });
  private readonly encoder = new Encoder({ channels: 1, sample_rate: OUTPUT_RATE, application: 'voip' });
  private readonly resampler = new Resampler(OUTPUT_RATE);
  private queue: Int16Array[] = [];
  private queued = 0;
  private readonly queueLimit: number;
  private waiters: Array<() => void> = [];
  private drainWaiters: Array<() => void> = [];
  private playedSamples = 0;
  private sequence = Math.floor(Math.random() * 65535);
  private timestamp = Math.floor(Math.random() * 2 ** 31);
  private timer: NodeJS.Timeout | null = null;
  private nextAt = 0;
  private firstAudioCallback: ((at: number) => void) | null = null;
  paused = false;

  constructor(queueSeconds = 0.2) {
    this.queueLimit = queueSeconds * OUTPUT_RATE;
  }

  start(): void {
    if (this.timer) return;
    this.nextAt = Date.now();
    this.tick();
  }

  private tick = (): void => {
    const now = Date.now();
    // Catch up after a short stall, and start again from now after a long one, rather than send a burst.
    if (now - this.nextAt > MAX_LOOP_STALL_MS) this.nextAt = now;
    while (this.nextAt <= now) {
      this.sendFrame();
      this.nextAt += FRAME_MS;
    }
    this.timer = setTimeout(this.tick, Math.max(1, this.nextAt - Date.now()));
  };

  private sendFrame(): void {
    const pcm = this.takeFrame();
    const packet = Buffer.from(this.encoder.encode(pcm));
    const header = new RtpHeader({
      version: 2,
      payloadType: OPUS_PAYLOAD_TYPE,
      sequenceNumber: this.sequence,
      timestamp: this.timestamp,
      marker: false,
    });
    this.sequence = (this.sequence + 1) & 0xffff;
    this.timestamp = (this.timestamp + FRAME_SAMPLES) >>> 0;
    this.track.writeRtp(new RtpPacket(header, packet));
  }

  private takeFrame(): Int16Array {
    const out = new Int16Array(FRAME_SAMPLES);
    if (this.paused || this.queued === 0) return out;
    let filled = 0;
    while (filled < FRAME_SAMPLES && this.queue.length > 0) {
      const head = this.queue[0];
      const take = Math.min(head.length, FRAME_SAMPLES - filled);
      out.set(head.subarray(0, take), filled);
      filled += take;
      if (take === head.length) this.queue.shift();
      else this.queue[0] = head.subarray(take);
    }
    this.queued -= filled;
    this.playedSamples += filled;
    if (this.firstAudioCallback) {
      const callback = this.firstAudioCallback;
      this.firstAudioCallback = null;
      callback(Date.now() / 1000);
    }
    if (this.queued <= this.queueLimit) this.wake();
    if (this.queued === 0) {
      const waiting = this.drainWaiters;
      this.drainWaiters = [];
      for (const fn of waiting) fn();
    }
    return out;
  }

  private wake(): void {
    const waiting = this.waiters;
    this.waiters = [];
    for (const fn of waiting) fn();
  }

  get queuedSeconds(): number {
    return this.queued / OUTPUT_RATE;
  }

  get playedSeconds(): number {
    return this.playedSamples / OUTPUT_RATE;
  }

  onNextAudio(callback: ((at: number) => void) | null): void {
    this.firstAudioCallback = callback;
  }

  /** Queues audio, waiting while the queue is full, so a producer runs only slightly ahead of real time. */
  async captureFrame(frame: AudioFrame): Promise<void> {
    const converted = this.resampler.push(frame);
    if (converted.data.length === 0) return;
    this.queue.push(converted.data);
    this.queued += converted.data.length;
    if (this.queued > this.queueLimit) {
      await new Promise<void>((resolve) => this.waiters.push(resolve));
    }
  }

  /** Drops queued audio at once, and returns how many seconds were dropped. */
  clearQueue(): number {
    const dropped = this.queuedSeconds;
    this.queue = [];
    this.queued = 0;
    this.wake();
    const waiting = this.drainWaiters;
    this.drainWaiters = [];
    for (const fn of waiting) fn();
    return dropped;
  }

  pause(): void {
    this.paused = true;
  }

  resume(): void {
    this.paused = false;
  }

  waitForPlayout(): Promise<void> {
    if (this.queued === 0) return Promise.resolve();
    return new Promise((resolve) => this.drainWaiters.push(resolve));
  }

  stop(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = null;
  }
}

/** Decodes a subscribed track's RTP. Packets arrive as they arrive: this does no jitter buffering beyond reordering by order of receipt. */
export class AudioStream {
  private readonly decoder = new Decoder({ channels: 1, sample_rate: OUTPUT_RATE });
  private readonly resampler: Resampler;
  private readonly unsubscribe: () => void;

  constructor(track: MediaStreamTrack, sampleRate: number, onFrame: (frame: AudioFrame) => void) {
    this.resampler = new Resampler(sampleRate);
    const { unSubscribe } = track.onReceiveRtp.subscribe((rtp) => {
      if (rtp.payload.length === 0) return;
      const decoded = this.decoder.decode(rtp.payload);
      const frame = AudioFrame.fromBuffer(Buffer.from(decoded.buffer, decoded.byteOffset, decoded.byteLength), OUTPUT_RATE);
      const out = this.resampler.push(frame);
      if (out.data.length > 0) onFrame(out);
    });
    this.unsubscribe = unSubscribe;
  }

  close(): void {
    this.unsubscribe();
  }
}

// The speech interfaces, sentence splitting, and voice activity detection.
import type { Usage } from './llm.ts';
import { AudioFrame, Resampler, rmsLevel } from './rtc/audio.ts';

export interface SpeechEvent {
  text: string;
  usage?: Usage;
}

export type StreamingSpeechEvent =
  | { type: 'start_of_speech' }
  | { type: 'interim'; text: string }
  | { type: 'final'; text: string }
  // The provider decided the user finished speaking, for providers that detect this.
  | { type: 'end_of_speech' }
  | { type: 'usage'; usage: Usage };

/** Audio goes in with pushFrame, and events come out through onEvent. */
export interface RecognizeStream {
  onEvent: (event: StreamingSpeechEvent) => void;
  pushFrame(frame: AudioFrame): void;
  /** Asks the provider to finish what it has heard so far. */
  flush(): void;
  endInput(): void;
  close(): void;
}

export abstract class STT {
  provider = 'unknown';
  readonly model: string;
  // A streaming STT takes all audio as it comes. Any other gets one utterance at a time, cut out by the VAD.
  readonly streaming: boolean = false;
  sampleRate = 16000;

  constructor(model: string) {
    this.model = model;
  }

  get label(): string {
    return `${this.provider}/${this.model}`;
  }

  abstract recognize(frame: AudioFrame): Promise<SpeechEvent>;

  stream(): RecognizeStream {
    throw new Error(`${this.label} does not stream`);
  }
}

export interface SynthesizedAudio {
  frame: AudioFrame;
  // Set on the first audio of each piece of text, so played audio can be traced back to the words it holds.
  segmentText?: string;
}

export abstract class TTS {
  provider = 'unknown';
  readonly model: string;
  readonly voice: string | undefined;
  sampleRate = 24000;
  lastUsage: Usage | null = null;
  segmentStartedAt: number | null = null;

  constructor(model: string, voice?: string) {
    this.model = model;
    this.voice = voice;
  }

  get label(): string {
    return `${this.provider}/${this.model}`;
  }

  abstract synthesize(text: string, signal?: AbortSignal): AsyncGenerator<AudioFrame>;

  /** Speaks text as it arrives, one sentence at a time, so the first words are heard long before the LLM has finished. */
  async *stream(text: AsyncIterable<string>, signal?: AbortSignal): AsyncGenerator<SynthesizedAudio> {
    const tokenizer = new SentenceTokenizer();
    const pending: Array<string | null> = [];
    let wake: (() => void) | null = null;
    const put = (item: string | null) => {
      pending.push(item);
      wake?.();
      wake = null;
    };
    const splitting = (async () => {
      try {
        for await (const delta of text) for (const sentence of tokenizer.push(delta)) put(sentence);
        const tail = tokenizer.flush();
        if (tail) put(tail);
      } finally {
        put(null);
      }
    })();
    while (true) {
      while (pending.length === 0) await new Promise<void>((resolve) => (wake = resolve));
      const sentence = pending.shift()!;
      if (sentence === null) break;
      let first = true;
      this.segmentStartedAt = Date.now() / 1000;
      for await (const frame of this.synthesize(sentence, signal)) {
        yield { frame, segmentText: first ? sentence : undefined };
        first = false;
      }
    }
    await splitting;
  }
}

// Words whose full stop does not end a sentence.
const ABBREVIATIONS = new Set(['mr', 'mrs', 'ms', 'dr', 'prof', 'sr', 'jr', 'st', 'vs', 'etc', 'e.g', 'i.e', 'no', 'inc', 'ltd', 'fig']);

/**
 * Splits streaming text into sentences. Very short fragments wait for more, because speech engines sound poor on them, and very
 * long sentences break at a clause, so they do not hold up the first audio for long.
 */
export class SentenceTokenizer {
  private buffer = '';
  private readonly minChars: number;
  private readonly maxChars: number;

  constructor(options: { minChars?: number; maxChars?: number } = {}) {
    this.minChars = options.minChars ?? 16;
    this.maxChars = options.maxChars ?? 180;
  }

  push(text: string): string[] {
    this.buffer += text;
    const out: string[] = [];
    const end = /[.!?]+["')\]]*(\s+|$)/g;
    let from = 0;
    while (true) {
      end.lastIndex = from;
      const match = end.exec(this.buffer);
      if (!match) break;
      const stop = match.index + match[0].length;
      // A match at the very end could still grow: "3." may become "3.5".
      if (stop === this.buffer.length && !match[1]) break;
      const sentence = this.buffer.slice(0, stop).trim();
      if (sentence.length < this.minChars || isAbbreviation(sentence)) {
        from = stop;
        continue;
      }
      out.push(sentence);
      this.buffer = this.buffer.slice(stop);
      from = 0;
    }
    while (this.buffer.length > this.maxChars) {
      const head = this.buffer.slice(0, this.maxChars);
      let cut = -1;
      for (const m of head.matchAll(/[,;:]\s+/g)) cut = m.index + m[0].length;
      if (cut <= 0) cut = head.lastIndexOf(' ') + 1;
      if (cut <= 0) break;
      out.push(this.buffer.slice(0, cut).trim());
      this.buffer = this.buffer.slice(cut);
    }
    return out;
  }

  flush(): string {
    const tail = this.buffer.trim();
    this.buffer = '';
    return tail;
  }
}

function isAbbreviation(sentence: string): boolean {
  if (/[!?]["')\]]*$/.test(sentence)) return false;
  const last = sentence.replace(/[.!?"')\]]+$/, '').split(/\s+/).at(-1)?.toLowerCase() ?? '';
  return ABBREVIATIONS.has(last) || (last.length === 1 && /[a-z]/.test(last));
}

/** Cuts a byte stream of 16-bit PCM into frames. Network chunks can end in the middle of a sample. */
export class PCMFramer {
  private readonly sampleRate: number;
  private readonly frameBytes: number;
  private buffer = Buffer.alloc(0);

  constructor(sampleRate: number, frameMs = 50) {
    this.sampleRate = sampleRate;
    this.frameBytes = Math.floor((sampleRate * frameMs) / 1000) * 2;
  }

  push(data: Buffer): AudioFrame[] {
    this.buffer = Buffer.concat([this.buffer, data]);
    const out: AudioFrame[] = [];
    while (this.buffer.length >= this.frameBytes) {
      out.push(AudioFrame.fromBuffer(this.buffer.subarray(0, this.frameBytes), this.sampleRate));
      this.buffer = this.buffer.subarray(this.frameBytes);
    }
    return out;
  }

  flush(): AudioFrame | null {
    const usable = this.buffer.length - (this.buffer.length % 2);
    if (usable === 0) return null;
    const frame = AudioFrame.fromBuffer(this.buffer.subarray(0, usable), this.sampleRate);
    this.buffer = Buffer.alloc(0);
    return frame;
  }
}

export type VADEvent =
  | { type: 'start_of_speech'; speechDuration: number }
  | { type: 'inference_done'; speaking: boolean; speechDuration: number; silenceDuration: number }
  | { type: 'end_of_speech'; speechDuration: number; silenceDuration: number; audio: AudioFrame };

/** Turns per-window speech decisions into speech start and end events. */
export abstract class VAD {
  readonly sampleRate = 16000;
  protected readonly window: number;
  protected readonly activation: number;
  protected readonly deactivation: number;
  private readonly minSpeech: number;
  private readonly minSilence: number;
  private readonly padding: number;
  private readonly resampler = new Resampler(16000);
  private pending: number[] = [];
  private ring: number[] = [];
  private utterance: number[] = [];
  private speaking = false;
  private speech = 0;
  private silence = 0;
  private candidate = 0;
  onEvent: (event: VADEvent) => void = () => {};

  protected constructor(window: number, options: { activation: number; deactivation: number; minSpeech: number; minSilence: number; padding: number }) {
    this.window = window;
    this.activation = options.activation;
    this.deactivation = options.deactivation;
    this.minSpeech = options.minSpeech;
    this.minSilence = options.minSilence;
    this.padding = options.padding;
  }

  protected abstract probability(window: number[]): number | Promise<number>;

  private chain: Promise<void> = Promise.resolve();

  pushFrame(frame: AudioFrame): void {
    const converted = this.resampler.push(frame);
    for (const s of converted.data) this.pending.push(s);
    while (this.pending.length >= this.window) {
      const window = this.pending.splice(0, this.window);
      const result = this.probability(window);
      if (typeof result === 'number') {
        this.process(window, result);
      } else {
        // An asynchronous model must still see, and answer for, windows in order.
        this.chain = this.chain.then(() => result).then((p) => this.process(window, p));
      }
    }
  }

  private process(window: number[], probability: number): void {
    const step = window.length / this.sampleRate;
    if (this.speaking) {
      this.utterance.push(...window);
      if (probability >= this.deactivation) {
        this.speech += step;
        this.silence = 0;
      } else {
        this.silence += step;
        if (this.silence >= this.minSilence) {
          this.speaking = false;
          const audio = new AudioFrame(Int16Array.from(this.utterance), this.sampleRate);
          this.utterance = [];
          this.onEvent({ type: 'end_of_speech', speechDuration: this.speech, silenceDuration: this.silence, audio });
          this.speech = 0;
          this.candidate = 0;
        }
      }
    } else {
      this.ring.push(...window);
      const keep = Math.floor(this.padding * this.sampleRate) + window.length;
      if (this.ring.length > keep) this.ring.splice(0, this.ring.length - keep);
      if (probability >= this.activation) {
        this.candidate += step;
        if (this.candidate >= this.minSpeech) {
          this.speaking = true;
          this.speech = this.candidate;
          this.silence = 0;
          this.utterance = this.ring;
          this.ring = [];
          this.onEvent({ type: 'start_of_speech', speechDuration: this.speech });
        }
      } else {
        this.candidate = 0;
        this.silence += step;
      }
    }
    this.onEvent({ type: 'inference_done', speaking: this.speaking, speechDuration: this.speech, silenceDuration: this.silence });
  }
}

/** Compares loudness to a threshold. It cannot tell speech from other noise, so it suits clean audio and tests. */
export class EnergyVAD extends VAD {
  private readonly threshold: number;

  constructor(options: { threshold?: number; minSpeech?: number; minSilence?: number; padding?: number } = {}) {
    super(320, { activation: 0.5, deactivation: 0.5, minSpeech: options.minSpeech ?? 0.1, minSilence: options.minSilence ?? 0.3, padding: options.padding ?? 0.3 });
    this.threshold = options.threshold ?? 0.015;
  }

  protected probability(window: number[]): number {
    return rmsLevel(Int16Array.from(window)) >= this.threshold ? 1 : 0;
  }
}

const SILERO_CONTEXT = 64;

/**
 * Silero VAD, which is MIT licensed, on ONNX Runtime. It takes windows of 512 samples at 16 kHz. Install onnxruntime-node to use it.
 * modelPath defaults to the SILERO_VAD_MODEL environment variable.
 */
export class SileroVAD extends VAD {
  // These are onnxruntime-node's types, which stay out of this package's required dependencies.
  private readonly session: any;
  private readonly ort: any;
  private state: any;
  private context = new Float32Array(SILERO_CONTEXT);
  private running: Promise<unknown> = Promise.resolve();

  private constructor(ort: any, session: any, options: { activation: number; deactivation: number; minSpeech: number; minSilence: number; padding: number }) {
    super(512, options);
    this.ort = ort;
    this.session = session;
    this.state = new ort.Tensor('float32', new Float32Array(2 * 128), [2, 1, 128]);
  }

  static async create(options: { modelPath?: string; activation?: number; deactivation?: number; minSpeech?: number; minSilence?: number; padding?: number } = {}): Promise<SileroVAD> {
    const path = options.modelPath ?? process.env.SILERO_VAD_MODEL;
    if (!path) throw new Error('no Silero model: pass modelPath or set SILERO_VAD_MODEL');
    const name = 'onnxruntime-node';
    const ort = await import(name);
    const session = await ort.InferenceSession.create(path, { intraOpNumThreads: 1, interOpNumThreads: 1 });
    return new SileroVAD(ort, session, {
      activation: options.activation ?? 0.5,
      deactivation: options.deactivation ?? 0.35,
      minSpeech: options.minSpeech ?? 0.1,
      minSilence: options.minSilence ?? 0.25,
      padding: options.padding ?? 0.3,
    });
  }

  protected probability(window: number[]): Promise<number> {
    // The model keeps state between windows, so they run one at a time.
    const result = this.running.then(() => this.infer(window));
    this.running = result.catch(() => {});
    return result;
  }

  private async infer(window: number[]): Promise<number> {
    const input = new Float32Array(SILERO_CONTEXT + window.length);
    input.set(this.context);
    for (let i = 0; i < window.length; i++) input[SILERO_CONTEXT + i] = window[i] / 32768;
    const { Tensor } = this.ort;
    const output = await this.session.run({
      input: new Tensor('float32', input, [1, input.length]),
      state: this.state,
      sr: new Tensor('int64', BigInt64Array.from([16000n]), []),
    });
    this.state = output.stateN;
    // The model expects the tail of the previous window in front of each new one.
    this.context = input.slice(input.length - SILERO_CONTEXT);
    return output.output.data[0] as number;
  }
}

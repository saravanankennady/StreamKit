// Streaming speech to text over WebSockets. Written from the documentation named in docs/VOICE_AI_PROVIDER_MATRIX.md, and tested
// against fixtures only.
import { WebSocket } from 'ws';
import { emptyUsage } from './llm.ts';
import { AudioFrame, Resampler } from './rtc/audio.ts';
import { type RecognizeStream, type SpeechEvent, STT, type StreamingSpeechEvent } from './speech.ts';

abstract class WebSocketStream implements RecognizeStream {
  onEvent: (event: StreamingSpeechEvent) => void = () => {};
  protected readonly ws: WebSocket;
  protected readonly resampler: Resampler;
  private readonly outgoing: Array<Buffer | string> = [];

  protected constructor(url: string, headers: Record<string, string>, rate: number) {
    this.resampler = new Resampler(rate);
    this.ws = new WebSocket(url, { headers });
    this.ws.on('open', () => {
      for (const item of this.outgoing.splice(0)) this.ws.send(item);
    });
    this.ws.on('message', (raw: Buffer, isBinary: boolean) => {
      if (!isBinary) this.onMessage(JSON.parse(raw.toString()));
    });
    this.ws.on('error', (e) => console.warn(`speech stream failed: ${e.name}`));
  }

  protected send(item: Buffer | string): void {
    if (this.ws.readyState === WebSocket.OPEN) this.ws.send(item);
    else if (this.ws.readyState === WebSocket.CONNECTING) this.outgoing.push(item);
  }

  protected abstract onMessage(message: Record<string, any>): void;
  abstract pushFrame(frame: AudioFrame): void;
  abstract flush(): void;
  abstract endInput(): void;

  close(): void {
    this.ws.close();
  }
}

export class DeepgramSTT extends STT {
  override readonly streaming = true;
  private readonly apiKey: string;
  private readonly wsUrl: string;
  private readonly language: string;
  private readonly endpointingMs: number;

  constructor(model: string, options: { apiKey: string; language?: string; baseUrl?: string; endpointingMs?: number }) {
    super(model);
    this.provider = 'deepgram';
    this.apiKey = options.apiKey;
    this.wsUrl = `${(options.baseUrl ?? 'wss://api.deepgram.com').replace(/\/$/, '')}/v1/listen`;
    this.language = options.language ?? 'en';
    this.endpointingMs = options.endpointingMs ?? 300;
  }

  async recognize(): Promise<SpeechEvent> {
    throw new Error('deepgram is used through its streaming API here');
  }

  override stream(): RecognizeStream {
    const query = new URLSearchParams({
      model: this.model,
      language: this.language,
      encoding: 'linear16',
      sample_rate: String(this.sampleRate),
      channels: '1',
      interim_results: 'true',
      vad_events: 'true',
      smart_format: 'true',
      endpointing: String(this.endpointingMs),
    });
    return new DeepgramStream(`${this.wsUrl}?${query}`, this.apiKey, this.sampleRate);
  }
}

class DeepgramStream extends WebSocketStream {
  private audioSeconds = 0;

  constructor(url: string, key: string, rate: number) {
    super(url, { authorization: `Token ${key}` }, rate);
  }

  pushFrame(frame: AudioFrame): void {
    const converted = this.resampler.push(frame);
    if (converted.data.length === 0) return;
    this.audioSeconds += converted.duration;
    this.send(converted.toBuffer());
  }

  flush(): void {
    this.send(JSON.stringify({ type: 'Finalize' }));
  }

  endInput(): void {
    this.send(JSON.stringify({ type: 'CloseStream' }));
  }

  protected onMessage(message: Record<string, any>): void {
    switch (message.type) {
      case 'SpeechStarted':
        this.onEvent({ type: 'start_of_speech' });
        break;
      case 'Results': {
        const text = message.channel?.alternatives?.[0]?.transcript ?? '';
        if (text) this.onEvent({ type: message.is_final ? 'final' : 'interim', text });
        if (message.speech_final) this.onEvent({ type: 'end_of_speech' });
        break;
      }
      case 'UtteranceEnd':
        this.onEvent({ type: 'end_of_speech' });
        break;
      case 'Metadata':
        this.onEvent({ type: 'usage', usage: { ...emptyUsage(), audioSeconds: message.duration ?? this.audioSeconds } });
        break;
      default:
        break;
    }
  }
}

// The API wants each message to hold 50 ms to 1000 ms of audio.
const ASSEMBLYAI_MIN_SECONDS = 0.05;

export class AssemblyAISTT extends STT {
  override readonly streaming = true;
  private readonly apiKey: string;
  private readonly wsUrl: string;

  constructor(model: string, options: { apiKey: string; baseUrl?: string }) {
    super(model);
    this.provider = 'assemblyai';
    this.apiKey = options.apiKey;
    this.wsUrl = `${(options.baseUrl ?? 'wss://streaming.assemblyai.com').replace(/\/$/, '')}/v3/ws`;
  }

  async recognize(): Promise<SpeechEvent> {
    throw new Error('assemblyai is used through its streaming API here');
  }

  override stream(): RecognizeStream {
    const query = new URLSearchParams({ speech_model: this.model, sample_rate: String(this.sampleRate), encoding: 'pcm_s16le' });
    return new AssemblyAIStream(`${this.wsUrl}?${query}`, this.apiKey, this.sampleRate);
  }
}

class AssemblyAIStream extends WebSocketStream {
  private pending: Buffer[] = [];
  private pendingBytes = 0;
  private readonly minBytes: number;

  constructor(url: string, key: string, rate: number) {
    // The key goes in the Authorization header, with no Bearer prefix.
    super(url, { authorization: key }, rate);
    this.minBytes = Math.floor(rate * ASSEMBLYAI_MIN_SECONDS) * 2;
  }

  pushFrame(frame: AudioFrame): void {
    const converted = this.resampler.push(frame);
    if (converted.data.length === 0) return;
    const data = converted.toBuffer();
    this.pending.push(data);
    this.pendingBytes += data.length;
    if (this.pendingBytes >= this.minBytes) {
      this.send(Buffer.concat(this.pending));
      this.pending = [];
      this.pendingBytes = 0;
    }
  }

  flush(): void {
    this.send(JSON.stringify({ type: 'ForceEndpoint' }));
  }

  endInput(): void {
    this.send(JSON.stringify({ type: 'Terminate' }));
  }

  protected onMessage(message: Record<string, any>): void {
    switch (message.type) {
      case 'SpeechStarted':
        this.onEvent({ type: 'start_of_speech' });
        break;
      case 'Turn': {
        const ended = Boolean(message.end_of_turn);
        const text = ended && message.utterance ? message.utterance : (message.transcript ?? '');
        if (text) this.onEvent({ type: ended ? 'final' : 'interim', text });
        if (ended) this.onEvent({ type: 'end_of_speech' });
        break;
      }
      case 'Termination':
        this.onEvent({ type: 'usage', usage: { ...emptyUsage(), audioSeconds: message.audio_duration_seconds ?? 0 } });
        break;
      default:
        break;
    }
  }
}

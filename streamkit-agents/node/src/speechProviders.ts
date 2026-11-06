// Speech providers that return audio over plain HTTP, one sentence at a time. Written from the documentation named in
// docs/VOICE_AI_PROVIDER_MATRIX.md, and tested against fixtures only.
import { emptyUsage } from './llm.ts';
import { failIfBad, post } from './providers.ts';
import type { AudioFrame } from './rtc/audio.ts';
import { PCMFramer, TTS } from './speech.ts';

/** Skips a WAV header at the start of a byte stream. A stream that does not start with one passes unchanged. */
class WavHeaderSkipper {
  private buffer = Buffer.alloc(0);
  private done = false;

  push(data: Buffer): Buffer {
    if (this.done) return data;
    this.buffer = Buffer.concat([this.buffer, data]);
    if (this.buffer.length < 12) return Buffer.alloc(0);
    if (this.buffer.subarray(0, 4).toString('latin1') !== 'RIFF') {
      this.done = true;
      return this.buffer;
    }
    let position = 12;
    while (position + 8 <= this.buffer.length) {
      const size = this.buffer.readUInt32LE(position + 4);
      if (this.buffer.subarray(position, position + 4).toString('latin1') === 'data') {
        this.done = true;
        return this.buffer.subarray(position + 8);
      }
      position += 8 + size + (size % 2);
    }
    return Buffer.alloc(0);
  }
}

abstract class HttpTTS extends TTS {
  protected abstract request(text: string): { url: string; headers: Record<string, string>; body: unknown; hasWavHeader?: boolean };

  async *synthesize(text: string, signal?: AbortSignal): AsyncGenerator<AudioFrame> {
    const { url, headers, body, hasWavHeader } = this.request(text);
    const response = await post(this.provider, url, { method: 'POST', headers: { 'content-type': 'application/json', ...headers }, body: JSON.stringify(body), signal });
    await failIfBad(this.provider, response);
    const framer = new PCMFramer(this.sampleRate);
    const skipper = hasWavHeader ? new WavHeaderSkipper() : null;
    for await (const chunk of response.body as unknown as AsyncIterable<Uint8Array>) {
      const data = Buffer.from(chunk);
      for (const frame of framer.push(skipper ? skipper.push(data) : data)) yield frame;
    }
    const tail = framer.flush();
    if (tail) yield tail;
    this.lastUsage = { ...emptyUsage(), characters: text.length };
  }
}

export class CartesiaTTS extends HttpTTS {
  private readonly apiKey: string;
  private readonly url: string;

  constructor(model: string, options: { voice: string; apiKey: string; baseUrl?: string }) {
    super(model, options.voice);
    this.provider = 'cartesia';
    this.apiKey = options.apiKey;
    this.url = `${(options.baseUrl ?? 'https://api.cartesia.ai').replace(/\/$/, '')}/tts/bytes`;
  }

  protected request(text: string) {
    return {
      url: this.url,
      headers: { authorization: `Bearer ${this.apiKey}`, 'cartesia-version': '2026-08-14' },
      body: { model_id: this.model, transcript: text, voice: { id: this.voice }, output_format: { container: 'raw', encoding: 'pcm_s16le', sample_rate: this.sampleRate } },
    };
  }
}

export class ElevenLabsTTS extends HttpTTS {
  private readonly apiKey: string;
  private readonly base: string;

  constructor(model: string, options: { voice: string; apiKey: string; baseUrl?: string }) {
    super(model, options.voice);
    this.provider = 'elevenlabs';
    this.apiKey = options.apiKey;
    this.base = (options.baseUrl ?? 'https://api.elevenlabs.io').replace(/\/$/, '');
  }

  protected request(text: string) {
    return {
      url: `${this.base}/v1/text-to-speech/${this.voice}/stream?output_format=pcm_${this.sampleRate}`,
      headers: { 'xi-api-key': this.apiKey },
      body: { text, model_id: this.model },
    };
  }
}

export class DeepgramTTS extends HttpTTS {
  private readonly apiKey: string;
  private readonly base: string;

  constructor(model: string, options: { apiKey: string; baseUrl?: string }) {
    super(model);
    this.provider = 'deepgram';
    this.apiKey = options.apiKey;
    this.base = (options.baseUrl ?? 'https://api.deepgram.com').replace(/\/$/, '');
  }

  protected request(text: string) {
    return {
      url: `${this.base}/v1/speak?model=${encodeURIComponent(this.model)}&encoding=linear16&sample_rate=${this.sampleRate}`,
      headers: { authorization: `Token ${this.apiKey}` },
      body: { text },
      // The documented container for linear16 is WAV.
      hasWavHeader: true,
    };
  }
}

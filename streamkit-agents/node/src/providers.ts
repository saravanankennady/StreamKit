// Provider adapters, written from the same documentation as the Python ones. See docs/VOICE_AI_PROVIDER_MATRIX.md.
// Streaming STT vendors and realtime models are Python only so far.
import { APIError, type ChatChunk, type ChatContext, type ChatOptions, errorForStatus, type FunctionCall, LLM, newId, sseEvents, emptyUsage } from './llm.ts';
import { AudioFrame } from './rtc/audio.ts';
import { PCMFramer, STT, TTS, type SpeechEvent } from './speech.ts';

export async function failIfBad(provider: string, response: Response): Promise<void> {
  if (!response.ok) throw errorForStatus(provider, response.status, await response.text());
}

export async function post(provider: string, url: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init);
  } catch (e) {
    if ((e as Error).name === 'AbortError') throw e;
    // fetch's error text can hold the URL, and a URL can hold a key.
    throw new APIError(`${provider} could not be reached: ${(e as Error).name}`);
  }
}

export interface OpenAICompatibleOptions {
  baseUrl: string;
  apiKey?: string;
  provider?: string;
  extraBody?: Record<string, unknown>;
  // Off for providers whose documentation does not show stream_options.
  streamUsage?: boolean;
}

export class OpenAICompatibleLLM extends LLM {
  private readonly url: string;
  private readonly headers: Record<string, string>;
  private readonly extraBody: Record<string, unknown>;
  private readonly streamUsage: boolean;

  constructor(model: string, options: OpenAICompatibleOptions) {
    super(model);
    this.provider = options.provider ?? 'openai-compatible';
    this.url = `${options.baseUrl.replace(/\/$/, '')}/chat/completions`;
    this.headers = { 'content-type': 'application/json', ...(options.apiKey ? { authorization: `Bearer ${options.apiKey}` } : {}) };
    this.extraBody = options.extraBody ?? {};
    this.streamUsage = options.streamUsage ?? true;
  }

  protected async *stream(ctx: ChatContext, options: ChatOptions): AsyncGenerator<ChatChunk> {
    const body: Record<string, unknown> = { model: this.model, messages: ctx.toOpenAI(), stream: true, ...this.extraBody };
    if (this.streamUsage) body.stream_options = { include_usage: true };
    if (options.tools?.length) {
      body.tools = options.tools.map((t) => ({ type: 'function', function: { name: t.name, description: t.description, parameters: t.parameters } }));
      if (options.toolChoice) body.tool_choice = options.toolChoice;
    }
    if (options.temperature !== undefined) body.temperature = options.temperature;
    if (options.maxTokens !== undefined) body.max_tokens = options.maxTokens;
    const response = await post(this.provider, this.url, { method: 'POST', headers: this.headers, body: JSON.stringify(body), signal: options.signal });
    await failIfBad(this.provider, response);
    // Tool call fragments, by index, until they are complete.
    const partial = new Map<number, { id: string; name: string; arguments: string }>();
    for await (const { data } of sseEvents(response.body!)) {
      if (data.trim() === '[DONE]') break;
      const chunk = JSON.parse(data);
      if (chunk.error) throw new APIError(`${this.provider} reported an error: ${JSON.stringify(chunk.error).slice(0, 300)}`, { retryable: false });
      for (const choice of chunk.choices ?? []) {
        const delta = choice.delta ?? {};
        for (const call of delta.tool_calls ?? []) {
          const slot = partial.get(call.index ?? 0) ?? { id: '', name: '', arguments: '' };
          slot.id = call.id || slot.id;
          slot.name += call.function?.name ?? '';
          slot.arguments += call.function?.arguments ?? '';
          partial.set(call.index ?? 0, slot);
        }
        if (delta.content) yield { text: delta.content };
        if (choice.finish_reason) yield { finishReason: choice.finish_reason };
      }
      if (chunk.usage) {
        yield {
          usage: {
            ...emptyUsage(),
            promptTokens: chunk.usage.prompt_tokens ?? 0,
            completionTokens: chunk.usage.completion_tokens ?? 0,
            cachedTokens: chunk.usage.prompt_tokens_details?.cached_tokens ?? 0,
            reportedCost: chunk.usage.cost ?? null,
          },
        };
      }
    }
    if (partial.size > 0) {
      const calls: FunctionCall[] = [...partial.entries()]
        .sort(([a], [b]) => a - b)
        .map(([i, s]) => ({ type: 'function_call', id: newId(), callId: s.id || `call_${i}`, name: s.name, arguments: s.arguments || '{}' }));
      yield { toolCalls: calls };
    }
  }
}

const ANTHROPIC_VERSION = '2026-10-21';

export class AnthropicLLM extends LLM {
  private readonly url: string;
  private readonly headers: Record<string, string>;

  constructor(model: string, options: { apiKey: string; baseUrl?: string }) {
    super(model);
    this.provider = 'anthropic';
    this.url = `${(options.baseUrl ?? 'https://api.anthropic.com').replace(/\/$/, '')}/v1/messages`;
    this.headers = { 'content-type': 'application/json', 'x-api-key': options.apiKey, 'anthropic-version': ANTHROPIC_VERSION };
  }

  protected async *stream(ctx: ChatContext, options: ChatOptions): AsyncGenerator<ChatChunk> {
    const { system, messages } = ctx.toAnthropic();
    const body: Record<string, unknown> = { model: this.model, messages, max_tokens: options.maxTokens ?? 1024, stream: true };
    if (system) body.system = system;
    if (options.temperature !== undefined) body.temperature = options.temperature;
    if (options.tools?.length) {
      body.tools = options.tools.map((t) => ({ name: t.name, description: t.description, input_schema: t.parameters }));
      if (options.toolChoice === 'required') body.tool_choice = { type: 'any' };
      else if (options.toolChoice === 'none') body.tool_choice = { type: 'none' };
      else if (options.toolChoice && options.toolChoice !== 'auto') body.tool_choice = { type: 'tool', name: options.toolChoice };
    }
    const response = await post(this.provider, this.url, { method: 'POST', headers: this.headers, body: JSON.stringify(body), signal: options.signal });
    await failIfBad(this.provider, response);
    const blocks = new Map<number, { id: string; name: string; json: string }>();
    const calls: FunctionCall[] = [];
    let input = 0;
    let cached = 0;
    let output = 0;
    for await (const { event, data } of sseEvents(response.body!)) {
      if (event === 'ping') continue;
      const payload = JSON.parse(data);
      switch (payload.type ?? event) {
        case 'message_start':
          input = payload.message.usage?.input_tokens ?? 0;
          cached = payload.message.usage?.cache_read_input_tokens ?? 0;
          output = payload.message.usage?.output_tokens ?? 0;
          break;
        case 'content_block_start':
          if (payload.content_block?.type === 'tool_use') blocks.set(payload.index, { id: payload.content_block.id, name: payload.content_block.name, json: '' });
          break;
        case 'content_block_delta':
          if (payload.delta?.type === 'text_delta') yield { text: payload.delta.text };
          else if (payload.delta?.type === 'input_json_delta') blocks.get(payload.index)!.json += payload.delta.partial_json ?? '';
          break;
        case 'content_block_stop': {
          const block = blocks.get(payload.index);
          if (block) {
            blocks.delete(payload.index);
            calls.push({ type: 'function_call', id: newId(), callId: block.id, name: block.name, arguments: block.json || '{}' });
          }
          break;
        }
        case 'message_delta':
          // These counts are cumulative, so the last one is the total.
          output = payload.usage?.output_tokens ?? output;
          if (payload.delta?.stop_reason) yield { finishReason: payload.delta.stop_reason };
          break;
        case 'error': {
          const type = payload.error?.type;
          throw new APIError(`anthropic reported ${type}: ${String(payload.error?.message).slice(0, 300)}`, {
            retryable: ['overloaded_error', 'rate_limit_error', 'api_error', 'timeout_error'].includes(type),
          });
        }
        default:
          break;
      }
    }
    if (calls.length) yield { toolCalls: calls };
    yield { usage: { ...emptyUsage(), promptTokens: input, cachedTokens: cached, completionTokens: output } };
  }
}

export class GoogleLLM extends LLM {
  private readonly url: string;
  private readonly headers: Record<string, string>;

  constructor(model: string, options: { apiKey: string; baseUrl?: string }) {
    super(model);
    this.provider = 'google';
    this.url = `${(options.baseUrl ?? 'https://generativelanguage.googleapis.com').replace(/\/$/, '')}/v1beta/models/${model}:streamGenerateContent?alt=sse`;
    // The key goes in a header, not the query string, so it cannot end up in a logged URL.
    this.headers = { 'content-type': 'application/json', 'x-goog-api-key': options.apiKey };
  }

  protected async *stream(ctx: ChatContext, options: ChatOptions): AsyncGenerator<ChatChunk> {
    const { system, contents } = ctx.toGoogle();
    const body: Record<string, unknown> = { contents };
    if (system) body.systemInstruction = { parts: [{ text: system }] };
    const generation: Record<string, unknown> = {};
    if (options.temperature !== undefined) generation.temperature = options.temperature;
    if (options.maxTokens !== undefined) generation.maxOutputTokens = options.maxTokens;
    if (Object.keys(generation).length) body.generationConfig = generation;
    if (options.tools?.length) {
      body.tools = [{ functionDeclarations: options.tools.map((t) => ({ name: t.name, description: t.description, parametersJsonSchema: t.parameters })) }];
    }
    const response = await post(this.provider, this.url, { method: 'POST', headers: this.headers, body: JSON.stringify(body), signal: options.signal });
    await failIfBad(this.provider, response);
    const calls: FunctionCall[] = [];
    let usage: ChatChunk['usage'];
    for await (const { data } of sseEvents(response.body!)) {
      const chunk = JSON.parse(data);
      if (chunk.error) throw new APIError(`google reported an error: ${JSON.stringify(chunk.error).slice(0, 300)}`, { retryable: false });
      if (chunk.usageMetadata) {
        usage = { ...emptyUsage(), promptTokens: chunk.usageMetadata.promptTokenCount ?? 0, completionTokens: chunk.usageMetadata.candidatesTokenCount ?? 0 };
      }
      for (const candidate of chunk.candidates ?? []) {
        for (const part of candidate.content?.parts ?? []) {
          if (part.text && !part.thought) yield { text: part.text };
          if (part.functionCall) {
            calls.push({
              type: 'function_call',
              id: newId(),
              callId: part.functionCall.id ?? `call_${part.functionCall.name}_${calls.length}`,
              name: part.functionCall.name,
              arguments: JSON.stringify(part.functionCall.args ?? {}),
            });
          }
        }
        if (candidate.finishReason) yield { finishReason: candidate.finishReason };
      }
    }
    if (calls.length) yield { toolCalls: calls };
    if (usage) yield { usage };
  }
}

export class OpenAICompatibleSTT extends STT {
  private readonly url: string;
  private readonly headers: Record<string, string>;
  private readonly language: string | undefined;

  constructor(model: string, options: OpenAICompatibleOptions & { language?: string }) {
    super(model);
    this.provider = options.provider ?? 'openai-compatible';
    this.url = `${options.baseUrl.replace(/\/$/, '')}/audio/transcriptions`;
    this.headers = options.apiKey ? { authorization: `Bearer ${options.apiKey}` } : {};
    this.language = options.language;
  }

  async recognize(frame: AudioFrame): Promise<SpeechEvent> {
    const form = new FormData();
    form.append('model', this.model);
    form.append('response_format', 'json');
    if (this.language) form.append('language', this.language);
    form.append('file', new Blob([new Uint8Array(wav(frame))], { type: 'audio/wav' }), 'speech.wav');
    const response = await post(this.provider, this.url, { method: 'POST', headers: this.headers, body: form });
    await failIfBad(this.provider, response);
    const body = await response.json();
    return { text: String(body.text ?? '').trim(), usage: { ...emptyUsage(), audioSeconds: frame.duration } };
  }
}

function wav(frame: AudioFrame): Buffer {
  const data = frame.toBuffer();
  const h = Buffer.alloc(44);
  h.write('RIFF', 0);
  h.writeUInt32LE(36 + data.length, 4);
  h.write('WAVEfmt ', 8);
  h.writeUInt32LE(16, 16);
  h.writeUInt16LE(1, 20);
  h.writeUInt16LE(frame.numChannels, 22);
  h.writeUInt32LE(frame.sampleRate, 24);
  h.writeUInt32LE(frame.sampleRate * frame.numChannels * 2, 28);
  h.writeUInt16LE(frame.numChannels * 2, 32);
  h.writeUInt16LE(16, 34);
  h.write('data', 36);
  h.writeUInt32LE(data.length, 40);
  return Buffer.concat([h, data]);
}

export class OpenAICompatibleTTS extends TTS {
  private readonly url: string;
  private readonly headers: Record<string, string>;

  constructor(model: string, options: OpenAICompatibleOptions & { voice?: string; sampleRate?: number }) {
    super(model, options.voice ?? 'alloy');
    this.provider = options.provider ?? 'openai-compatible';
    this.sampleRate = options.sampleRate ?? 24000;
    this.url = `${options.baseUrl.replace(/\/$/, '')}/audio/speech`;
    this.headers = { 'content-type': 'application/json', ...(options.apiKey ? { authorization: `Bearer ${options.apiKey}` } : {}) };
  }

  async *synthesize(text: string, signal?: AbortSignal): AsyncGenerator<AudioFrame> {
    const body = { model: this.model, input: text, voice: this.voice, response_format: 'pcm' };
    const response = await post(this.provider, this.url, { method: 'POST', headers: this.headers, body: JSON.stringify(body), signal });
    await failIfBad(this.provider, response);
    const rate = Number(response.headers.get('x-sample-rate')) || this.sampleRate;
    const framer = new PCMFramer(rate);
    for await (const chunk of response.body as unknown as AsyncIterable<Uint8Array>) {
      for (const frame of framer.push(Buffer.from(chunk))) yield frame;
    }
    const tail = framer.flush();
    if (tail) yield tail;
    this.lastUsage = { ...emptyUsage(), characters: text.length };
  }
}

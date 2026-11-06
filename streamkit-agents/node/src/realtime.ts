// Models that take speech in and give speech out over one connection, in the same shape as the Python framework. They run full
// duplex, where the provider decides when turns start and end, or turn based, where the framework's own VAD decides.
//
// No real realtime model has been run against this code: see docs/VOICE_AI_PROVIDER_MATRIX.md.
import { EventEmitter } from 'node:events';
import { randomUUID } from 'node:crypto';
import { WebSocket } from 'ws';
import { emptyUsage, type FunctionCall, type FunctionTool, newId, type Usage } from './llm.ts';
import { AudioFrame, Resampler } from './rtc/audio.ts';

export interface RealtimeCapabilities {
  // Whether the provider can be told how much of a reply was heard.
  messageTruncation: boolean;
  // Whether the provider starts a reply by itself once given a tool result.
  autoToolReply: boolean;
}

/** One reply. Audio and text arrive at once, and either can finish first. */
export class Generation {
  readonly responseId: string;
  itemId = '';
  text = '';
  audioSeconds = 0;
  audio: AudioFrame[] = [];
  functionCalls: FunctionCall[] = [];
  usage: Usage | null = null;
  interrupted = false;
  done = false;
  private wake: (() => void) | null = null;

  constructor(responseId: string) {
    this.responseId = responseId;
  }

  pushAudio(frame: AudioFrame): void {
    this.audioSeconds += frame.duration;
    this.audio.push(frame);
    this.notify();
  }

  finish(): void {
    this.done = true;
    this.notify();
  }

  notify(): void {
    this.wake?.();
    this.wake = null;
  }

  /** Waits until there is more to read, or the reply is over. */
  async changed(): Promise<void> {
    await new Promise<void>((resolve) => (this.wake = resolve));
  }
}

/** Events: generation(Generation), speechStarted(), speechStopped(), inputTranscribed(text, final), error(Error), closed(). */
export abstract class RealtimeSession extends EventEmitter {
  abstract readonly capabilities: RealtimeCapabilities;
  abstract start(): Promise<void>;
  abstract pushAudio(frame: AudioFrame): void;
  abstract commitAudio(): void;
  abstract clearAudio(): void;
  abstract generateReply(instructions?: string): void;
  abstract interrupt(): void;
  abstract truncate(itemId: string, audioEndMs: number): void;
  abstract sendToolResult(call: FunctionCall, output: string, isError: boolean): void;
  abstract close(): Promise<void>;
}

export abstract class RealtimeModel {
  provider = 'unknown';
  readonly model: string;
  inputSampleRate = 24000;

  constructor(model: string) {
    this.model = model;
  }

  get label(): string {
    return `${this.provider}/${this.model}`;
  }

  /** serverTurnDetection selects full duplex when true, and turn based when false. */
  abstract session(options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean }): RealtimeSession;
}

const OPENAI_RATE = 24000;

export class OpenAIRealtimeModel extends RealtimeModel {
  private readonly apiKey: string;
  private readonly voice: string;
  private readonly baseUrl: string;

  constructor(model = 'gpt-realtime-2.1', options: { apiKey: string; voice?: string; baseUrl?: string }) {
    super(model);
    this.provider = 'openai';
    this.apiKey = options.apiKey;
    this.voice = options.voice ?? 'marin';
    this.baseUrl = options.baseUrl ?? 'wss://api.openai.com/v1/realtime';
  }

  session(options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean }): RealtimeSession {
    return new OpenAIRealtimeSession(`${this.baseUrl}?model=${encodeURIComponent(this.model)}`, this.apiKey, this.model, this.voice, options);
  }
}

class OpenAIRealtimeSession extends RealtimeSession {
  readonly capabilities = { messageTruncation: true, autoToolReply: false };
  private readonly url: string;
  private readonly apiKey: string;
  private readonly model: string;
  private readonly voice: string;
  private readonly options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean };
  private ws: WebSocket | null = null;
  private readonly resampler = new Resampler(OPENAI_RATE);
  private readonly generations = new Map<string, Generation>();
  private active: Generation | null = null;

  constructor(url: string, apiKey: string, model: string, voice: string, options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean }) {
    super();
    this.url = url;
    this.apiKey = apiKey;
    this.model = model;
    this.voice = voice;
    this.options = options;
  }

  private send(event: Record<string, unknown>): void {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(event));
  }

  start(): Promise<void> {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(this.url, { headers: { authorization: `Bearer ${this.apiKey}` } });
      this.ws = ws;
      let ready = false;
      ws.on('open', () => {
        this.send({
          type: 'session.update',
          session: {
            type: 'realtime',
            model: this.model,
            instructions: this.options.instructions,
            output_modalities: ['audio'],
            audio: {
              input: { format: { type: 'audio/pcm', rate: OPENAI_RATE }, turn_detection: this.options.serverTurnDetection ? { type: 'semantic_vad' } : null },
              output: { format: { type: 'audio/pcm', rate: OPENAI_RATE }, voice: this.voice },
            },
            tools: this.options.tools.map((t) => ({ type: 'function', name: t.name, description: t.description, parameters: t.parameters })),
            tool_choice: 'auto',
          },
        });
      });
      ws.on('message', (raw: Buffer) => {
        const event = JSON.parse(raw.toString());
        if (event.type === 'session.updated' && !ready) {
          ready = true;
          resolve();
        }
        this.handle(event);
      });
      ws.on('error', (e) => {
        if (!ready) reject(new Error(`openai realtime connection failed: ${e.name}`));
      });
      ws.on('close', () => {
        this.active?.finish();
        this.emit('closed');
      });
    });
  }

  pushAudio(frame: AudioFrame): void {
    const converted = this.resampler.push(frame);
    if (converted.data.length > 0) this.send({ type: 'input_audio_buffer.append', audio: converted.toBuffer().toString('base64') });
  }

  commitAudio(): void {
    this.send({ type: 'input_audio_buffer.commit' });
  }

  clearAudio(): void {
    this.send({ type: 'input_audio_buffer.clear' });
  }

  generateReply(instructions?: string): void {
    this.send(instructions ? { type: 'response.create', response: { instructions } } : { type: 'response.create' });
  }

  interrupt(): void {
    if (this.active) {
      this.active.interrupted = true;
      this.send({ type: 'response.cancel' });
    }
  }

  truncate(itemId: string, audioEndMs: number): void {
    this.send({ type: 'conversation.item.truncate', item_id: itemId, content_index: 0, audio_end_ms: audioEndMs });
  }

  sendToolResult(call: FunctionCall, output: string): void {
    this.send({ type: 'conversation.item.create', item: { type: 'function_call_output', call_id: call.callId, output } });
  }

  private handle(event: Record<string, any>): void {
    switch (event.type) {
      case 'input_audio_buffer.speech_started':
        this.emit('speechStarted');
        break;
      case 'input_audio_buffer.speech_stopped':
        this.emit('speechStopped');
        break;
      case 'conversation.item.input_audio_transcription.completed':
        this.emit('inputTranscribed', event.transcript ?? '', true);
        break;
      case 'response.created': {
        const generation = new Generation(event.response.id);
        this.generations.set(generation.responseId, generation);
        this.active = generation;
        this.emit('generation', generation);
        break;
      }
      case 'response.output_item.added':
        if (event.item?.type === 'message') {
          const generation = this.generations.get(event.response_id);
          if (generation) generation.itemId = event.item.id ?? '';
        }
        break;
      case 'response.output_audio.delta': {
        const generation = this.generations.get(event.response_id);
        if (generation) {
          generation.itemId ||= event.item_id ?? '';
          generation.pushAudio(AudioFrame.fromBuffer(Buffer.from(event.delta, 'base64'), OPENAI_RATE));
        }
        break;
      }
      case 'response.output_audio_transcript.delta': {
        const generation = this.generations.get(event.response_id);
        if (generation) {
          generation.text += event.delta ?? '';
          generation.notify();
        }
        break;
      }
      case 'response.function_call_arguments.done':
        this.generations.get(event.response_id)?.functionCalls.push({ type: 'function_call', id: newId(), callId: event.call_id ?? '', name: event.name ?? '', arguments: event.arguments ?? '{}' });
        break;
      case 'response.done': {
        const generation = this.generations.get(event.response?.id);
        if (!generation) break;
        this.generations.delete(generation.responseId);
        const usage = event.response.usage ?? {};
        generation.usage = { ...emptyUsage(), promptTokens: usage.input_tokens ?? 0, completionTokens: usage.output_tokens ?? 0, cachedTokens: usage.input_token_details?.cached_tokens ?? 0 };
        if (event.response.status === 'cancelled') generation.interrupted = true;
        generation.finish();
        if (this.active === generation) this.active = null;
        break;
      }
      case 'error':
        // Cancelling when nothing is running is a harmless race with the reply finishing.
        if (event.error?.code !== 'response_cancel_not_active') {
          this.emit('error', new Error(`openai realtime: ${event.error?.code}: ${String(event.error?.message).slice(0, 300)}`));
        }
        break;
      default:
        break;
    }
  }

  async close(): Promise<void> {
    this.ws?.close();
  }
}

const GEMINI_INPUT_RATE = 16000;
const GEMINI_OUTPUT_RATE = 24000;
const GEMINI_PATH = '/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent';

export class GeminiLiveModel extends RealtimeModel {
  private readonly apiKey: string;
  private readonly voice: string;
  private readonly baseUrl: string;

  constructor(model = 'gemini-3.8-live', options: { apiKey: string; voice?: string; baseUrl?: string }) {
    super(model);
    this.provider = 'google';
    this.inputSampleRate = GEMINI_INPUT_RATE;
    this.apiKey = options.apiKey;
    this.voice = options.voice ?? 'Kore';
    this.baseUrl = (options.baseUrl ?? 'wss://generativelanguage.googleapis.com').replace(/\/$/, '');
  }

  session(options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean }): RealtimeSession {
    return new GeminiLiveSession(`${this.baseUrl}${GEMINI_PATH}?key=${encodeURIComponent(this.apiKey)}`, this.model, this.voice, options);
  }
}

class GeminiLiveSession extends RealtimeSession {
  // Gemini hears nothing of what was played, and starts its own replies after a tool result.
  readonly capabilities = { messageTruncation: false, autoToolReply: true };
  private readonly url: string;
  private readonly model: string;
  private readonly voice: string;
  private readonly options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean };
  private ws: WebSocket | null = null;
  private readonly resampler = new Resampler(GEMINI_INPUT_RATE);
  private active: Generation | null = null;
  private activityOpen = false;
  private usage: Usage | null = null;

  constructor(url: string, model: string, voice: string, options: { instructions: string; tools: FunctionTool[]; serverTurnDetection: boolean }) {
    super();
    this.url = url;
    this.model = model;
    this.voice = voice;
    this.options = options;
  }

  private send(message: Record<string, unknown>): void {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(message));
  }

  start(): Promise<void> {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(this.url);
      this.ws = ws;
      let ready = false;
      ws.on('open', () => {
        const setup: Record<string, unknown> = {
          model: `models/${this.model}`,
          generationConfig: { responseModalities: ['AUDIO'], speechConfig: { voiceConfig: { prebuiltVoiceConfig: { voiceName: this.voice } } } },
          inputAudioTranscription: {},
          outputAudioTranscription: {},
        };
        if (this.options.instructions) setup.systemInstruction = { parts: [{ text: this.options.instructions }] };
        if (this.options.tools.length) {
          setup.tools = [{ functionDeclarations: this.options.tools.map((t) => ({ name: t.name, description: t.description, parametersJsonSchema: t.parameters })) }];
        }
        if (!this.options.serverTurnDetection) setup.realtimeInputConfig = { automaticActivityDetection: { disabled: true } };
        this.send({ setup });
      });
      ws.on('message', (raw: Buffer) => {
        const message = JSON.parse(raw.toString());
        if (message.setupComplete !== undefined && !ready) {
          ready = true;
          resolve();
          return;
        }
        this.handle(message);
      });
      // The URL holds the key, so the error must not include it.
      ws.on('error', (e) => {
        if (!ready) reject(new Error(`gemini live connection failed: ${e.name}`));
      });
      ws.on('close', () => {
        this.active?.finish();
        this.emit('closed');
      });
    });
  }

  pushAudio(frame: AudioFrame): void {
    const converted = this.resampler.push(frame);
    if (converted.data.length === 0) return;
    if (!this.options.serverTurnDetection && !this.activityOpen) {
      this.activityOpen = true;
      this.send({ realtimeInput: { activityStart: {} } });
    }
    this.send({ realtimeInput: { audio: { mimeType: `audio/pcm;rate=${GEMINI_INPUT_RATE}`, data: converted.toBuffer().toString('base64') } } });
  }

  commitAudio(): void {
    if (this.activityOpen) {
      this.activityOpen = false;
      this.send({ realtimeInput: { activityEnd: {} } });
    }
  }

  clearAudio(): void {
    // Gemini has no way to take back audio it has received. Nothing further is sent until the next turn.
    this.activityOpen = false;
  }

  generateReply(instructions?: string): void {
    // Gemini replies on its own at the end of activity. A reply with nothing new comes from a client turn.
    if (instructions) this.send({ clientContent: { turns: [{ role: 'user', parts: [{ text: instructions }] }], turnComplete: true } });
  }

  interrupt(): void {
    // Sending client content interrupts the model. An empty turn keeps the context as it was.
    if (this.active) {
      this.active.interrupted = true;
      this.send({ clientContent: { turnComplete: false } });
    }
  }

  truncate(): void {}

  sendToolResult(call: FunctionCall, output: string, isError: boolean): void {
    this.send({ toolResponse: { functionResponses: [{ id: call.callId, name: call.name, response: { [isError ? 'error' : 'result']: output } }] } });
  }

  private generation(): Generation {
    if (!this.active) {
      this.active = new Generation(randomUUID());
      this.emit('generation', this.active);
    }
    return this.active;
  }

  private finishActive(): void {
    const generation = this.active;
    if (!generation) return;
    this.active = null;
    generation.usage = this.usage;
    generation.finish();
  }

  private handle(message: Record<string, any>): void {
    if (message.usageMetadata) {
      this.usage = { ...emptyUsage(), promptTokens: message.usageMetadata.promptTokenCount ?? 0, completionTokens: message.usageMetadata.responseTokenCount ?? message.usageMetadata.candidatesTokenCount ?? 0 };
    }
    if (message.toolCall) {
      const generation = this.generation();
      for (const call of message.toolCall.functionCalls ?? []) {
        generation.functionCalls.push({ type: 'function_call', id: newId(), callId: call.id ?? randomUUID(), name: call.name, arguments: JSON.stringify(call.args ?? {}) });
      }
      // Nothing further comes for this reply until the tool results are sent.
      this.finishActive();
      return;
    }
    const content = message.serverContent;
    if (!content) return;
    if (content.interrupted) {
      this.emit('speechStarted');
      if (this.active) this.active.interrupted = true;
      this.finishActive();
      return;
    }
    if (content.inputTranscription?.text) this.emit('inputTranscribed', content.inputTranscription.text, Boolean(content.inputTranscription.finished));
    if (content.outputTranscription?.text) {
      const generation = this.generation();
      generation.text += content.outputTranscription.text;
      generation.notify();
    }
    for (const part of content.modelTurn?.parts ?? []) {
      if (part.inlineData?.data) this.generation().pushAudio(AudioFrame.fromBuffer(Buffer.from(part.inlineData.data, 'base64'), GEMINI_OUTPUT_RATE));
    }
    if (content.turnComplete) this.finishActive();
  }

  async close(): Promise<void> {
    this.ws?.close();
  }
}

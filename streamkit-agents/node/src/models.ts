// Choosing models by name, where keys come from, and where session records go, in the same shape as the Python framework.
import { appendFileSync } from 'node:fs';
import type { LLM, Usage } from './llm.ts';
import { AnthropicLLM, GoogleLLM, OpenAICompatibleLLM, OpenAICompatibleSTT, OpenAICompatibleTTS } from './providers.ts';
import { GeminiLiveModel, OpenAIRealtimeModel, type RealtimeModel } from './realtime.ts';
import { CartesiaTTS, DeepgramTTS, ElevenLabsTTS } from './speechProviders.ts';
import { AssemblyAISTT, DeepgramSTT } from './streamingStt.ts';
import type { AgentSession, TurnMetrics, UsageRecord } from './session.ts';
import type { STT, TTS } from './speech.ts';

export class CredentialError extends Error {}

const ENV_KEYS: Record<string, string> = {
  openai: 'OPENAI_API_KEY',
  anthropic: 'ANTHROPIC_API_KEY',
  google: 'GOOGLE_API_KEY',
  groq: 'GROQ_API_KEY',
  openrouter: 'OPENROUTER_API_KEY',
  deepseek: 'DEEPSEEK_API_KEY',
  together: 'TOGETHER_API_KEY',
  mistral: 'MISTRAL_API_KEY',
  deepgram: 'DEEPGRAM_API_KEY',
  assemblyai: 'ASSEMBLYAI_API_KEY',
  cartesia: 'CARTESIA_API_KEY',
  elevenlabs: 'ELEVENLABS_API_KEY',
};

const PRESETS: Record<string, { baseUrl: string; envKey: string; streamUsage: boolean }> = {
  openai: { baseUrl: 'https://api.openai.com/v1', envKey: 'OPENAI_API_KEY', streamUsage: true },
  groq: { baseUrl: 'https://api.groq.com/openai/v1', envKey: 'GROQ_API_KEY', streamUsage: true },
  openrouter: { baseUrl: 'https://openrouter.ai/api/v1', envKey: 'OPENROUTER_API_KEY', streamUsage: true },
  deepseek: { baseUrl: 'https://api.deepseek.com', envKey: 'DEEPSEEK_API_KEY', streamUsage: true },
  together: { baseUrl: 'https://api.together.ai/v1', envKey: 'TOGETHER_API_KEY', streamUsage: true },
  mistral: { baseUrl: 'https://api.mistral.ai/v1', envKey: 'MISTRAL_API_KEY', streamUsage: false },
  ollama: { baseUrl: 'http://localhost:11434/v1', envKey: '', streamUsage: true },
};

export interface CredentialSource {
  get(provider: string, name?: string): Promise<string | null>;
}

export class EnvironmentCredentials implements CredentialSource {
  async get(provider: string, name = 'default'): Promise<string | null> {
    const variable = ENV_KEYS[provider];
    return variable && name === 'default' ? (process.env[variable] ?? null) : null;
  }
}

/** Reads a key over the server-side API, with a token holding the agent grant. Browsers are refused there. */
export class ControlPlaneCredentials implements CredentialSource {
  private readonly url: string;
  private readonly token: string;
  private readonly cache = new Map<string, string>();

  constructor(apiUrl: string, token: string) {
    this.url = `${apiUrl.replace(/\/$/, '')}/api/v1/agent-credentials/resolve`;
    this.token = token;
  }

  async get(provider: string, name = 'default'): Promise<string | null> {
    const key = `${provider}/${name}`;
    const cached = this.cache.get(key);
    if (cached) return cached;
    const response = await fetch(this.url, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: `Bearer ${this.token}` },
      body: JSON.stringify({ provider, name }),
    });
    if (response.status === 404) return null;
    if (!response.ok) throw new CredentialError(`the control plane refused the credential lookup: ${response.status}`);
    const secret = (await response.json()).secret as string | undefined;
    if (secret) this.cache.set(key, secret);
    return secret ?? null;
  }
}

export class Credentials {
  private readonly sources: CredentialSource[];

  constructor(sources: CredentialSource[] = [new EnvironmentCredentials()]) {
    this.sources = sources;
  }

  async require(provider: string, name = 'default'): Promise<string> {
    for (const source of this.sources) {
      const value = await source.get(provider, name);
      if (value) return value;
    }
    const variable = ENV_KEYS[provider];
    throw new CredentialError(`no credential for ${provider}: ${variable ? `set ${variable}, or ` : ''}store one in the control plane`);
  }
}

function split(spec: string): [string, string] {
  const at = spec.indexOf('/');
  if (at < 1 || at === spec.length - 1) throw new Error(`a model is named "provider/model", not ${spec}`);
  return [spec.slice(0, at), spec.slice(at + 1)];
}

/** A model is named by a string, so a choice can come from code, configuration or a job's metadata. */
export class ModelSelector {
  private readonly credentials: Credentials;

  constructor(credentials: Credentials = new Credentials()) {
    this.credentials = credentials;
  }

  async llm(spec: string, options: { baseUrl?: string } = {}): Promise<LLM> {
    const [provider, model] = split(spec);
    if (provider === 'anthropic') return new AnthropicLLM(model, { apiKey: await this.credentials.require('anthropic') });
    if (provider === 'google') return new GoogleLLM(model, { apiKey: await this.credentials.require('google') });
    if (provider === 'local') return new OpenAICompatibleLLM(model, { baseUrl: options.baseUrl ?? process.env.STREAMKIT_LOCAL_LLM_URL ?? '', provider: 'local' });
    const preset = PRESETS[provider];
    if (!preset) throw new Error(`no LLM provider called ${provider}`);
    const apiKey = preset.envKey ? await this.credentials.require(provider) : undefined;
    return new OpenAICompatibleLLM(model, { baseUrl: options.baseUrl ?? preset.baseUrl, apiKey, provider, streamUsage: preset.streamUsage });
  }

  async stt(spec: string, options: { baseUrl?: string; language?: string } = {}): Promise<STT> {
    const [provider, model] = split(spec);
    if (provider === 'local') return new OpenAICompatibleSTT(model, { baseUrl: options.baseUrl ?? process.env.STREAMKIT_LOCAL_STT_URL ?? '', provider: 'local', language: options.language });
    if (provider === 'openai' || provider === 'groq') {
      return new OpenAICompatibleSTT(model, { baseUrl: options.baseUrl ?? PRESETS[provider].baseUrl, apiKey: await this.credentials.require(provider), provider, language: options.language });
    }
    if (provider === 'deepgram') return new DeepgramSTT(model, { apiKey: await this.credentials.require('deepgram'), language: options.language });
    if (provider === 'assemblyai') return new AssemblyAISTT(model, { apiKey: await this.credentials.require('assemblyai') });
    throw new Error(`no STT provider called ${provider}`);
  }

  async tts(spec: string, options: { baseUrl?: string; sampleRate?: number } = {}): Promise<TTS> {
    const [provider, rest] = split(spec);
    const [model, voice] = rest.split(':');
    if (provider === 'local') {
      return new OpenAICompatibleTTS(model, { baseUrl: options.baseUrl ?? process.env.STREAMKIT_LOCAL_TTS_URL ?? '', provider: 'local', voice: voice || 'default', sampleRate: options.sampleRate ?? 22050 });
    }
    if (provider === 'openai') {
      return new OpenAICompatibleTTS(model, { baseUrl: options.baseUrl ?? PRESETS.openai.baseUrl, apiKey: await this.credentials.require('openai'), provider: 'openai', voice: voice || 'marin' });
    }
    if (provider === 'deepgram') return new DeepgramTTS(model, { apiKey: await this.credentials.require('deepgram'), baseUrl: options.baseUrl });
    if (provider === 'cartesia' || provider === 'elevenlabs') {
      if (!voice) throw new Error(`${provider} needs a voice: "${provider}/<model>:<voice id>"`);
      const Class = provider === 'cartesia' ? CartesiaTTS : ElevenLabsTTS;
      return new Class(model, { voice, apiKey: await this.credentials.require(provider), baseUrl: options.baseUrl });
    }
    throw new Error(`no TTS provider called ${provider}`);
  }
}

export async function selectRealtime(selector: Credentials, spec: string, options: { voice?: string } = {}): Promise<RealtimeModel> {
  const [provider, model] = split(spec);
  if (provider === 'openai') return new OpenAIRealtimeModel(model, { apiKey: await selector.require('openai'), voice: options.voice });
  if (provider === 'google') return new GeminiLiveModel(model, { apiKey: await selector.require('google'), voice: options.voice });
  throw new Error(`no realtime provider called ${provider}`);
}

export class ModelChoice {
  stt?: string;
  llm?: string;
  tts?: string;

  constructor(values: { stt?: string; llm?: string; tts?: string } = {}) {
    this.stt = values.stt;
    this.llm = values.llm;
    this.tts = values.tts;
  }

  overlay(other: ModelChoice): ModelChoice {
    return new ModelChoice({ stt: other.stt ?? this.stt, llm: other.llm ?? this.llm, tts: other.tts ?? this.tts });
  }

  /** Reads {"models": {"llm": "..."}} from a dispatch's metadata. Metadata that is not JSON chooses nothing. */
  static fromJobMetadata(metadata: string): ModelChoice {
    try {
      const models = JSON.parse(metadata || '{}')?.models;
      return models && typeof models === 'object' ? new ModelChoice(models) : new ModelChoice();
    } catch {
      return new ModelChoice();
    }
  }
}

/** Your prices, in US dollars. The framework ships none. */
export interface Prices {
  llmInputPerMtok?: number;
  llmOutputPerMtok?: number;
  sttPerMinute?: number;
  ttsPerMchar?: number;
}

function costOf(record: UsageRecord, prices?: Prices): number | null {
  const u: Usage = record.usage;
  if (u.reportedCost !== null) return u.reportedCost;
  if (!prices) return null;
  if (record.kind === 'stt' && prices.sttPerMinute !== undefined) return (u.audioSeconds / 60) * prices.sttPerMinute;
  if (record.kind === 'tts' && prices.ttsPerMchar !== undefined) return (u.characters / 1e6) * prices.ttsPerMchar;
  if (record.kind === 'llm' && prices.llmInputPerMtok !== undefined && prices.llmOutputPerMtok !== undefined) {
    return (u.promptTokens / 1e6) * prices.llmInputPerMtok + (u.completionTokens / 1e6) * prices.llmOutputPerMtok;
  }
  return null;
}

/** A turn in the JSON shape the control plane stores. A partial total would look complete, so one unknown cost makes it unknown. */
export function turnReport(turn: TurnMetrics, prices?: Prices): Record<string, unknown> {
  const latency = turn.latency;
  const costs = turn.usage.map((r) => costOf(r, prices));
  return {
    turn_id: turn.turnId,
    pipeline: turn.pipeline,
    user_text: turn.userText,
    reply_text: turn.replyText,
    played_text: turn.playedText,
    interrupted: turn.interrupted,
    tool_calls: turn.toolCalls,
    started_at: turn.speechStartedAt ?? turn.llmStartedAt,
    latency: {
      transcription: latency.transcription,
      end_of_turn: latency.endOfTurn,
      llm_first_token: latency.llmFirstToken,
      tts_first_audio: latency.ttsFirstAudio,
      end_to_end: latency.endToEnd,
    },
    usage: turn.usage.map((r, i) => ({ kind: r.kind, model: r.model, ...r.usage, cost_usd: costs[i] })),
    cost_usd: costs.length > 0 && costs.every((c) => c !== null) ? costs.reduce<number>((a, c) => a + (c as number), 0) : null,
  };
}

export class SessionReport {
  readonly sessionId = crypto.randomUUID().replaceAll('-', '');
  readonly startedAt = Date.now() / 1000;
  room = '';
  jobId = '';
  agentName = '';
  pipeline = 'cascaded';
  prices?: Prices;

  toDict(session: AgentSession): Record<string, unknown> {
    const turns = session.turns.map((t) => turnReport(t, this.prices));
    const costs = turns.map((t) => t.cost_usd as number | null);
    return {
      session_id: this.sessionId,
      room: this.room,
      job_id: this.jobId,
      agent_name: this.agentName,
      pipeline: this.pipeline,
      models: session.models,
      started_at: this.startedAt,
      ended_at: Date.now() / 1000,
      turns,
      errors: [],
      cost_usd: costs.length > 0 && costs.every((c) => c !== null) ? costs.reduce<number>((a, c) => a + (c as number), 0) : null,
    };
  }
}

export interface MetricsSink {
  onSessionEnd(report: Record<string, unknown>): Promise<void> | void;
}

/** Appends a JSON line per session. Transcripts are personal data, so choose the file's place with care. */
export class JsonlSink implements MetricsSink {
  private readonly path: string;

  constructor(path: string) {
    this.path = path;
  }

  onSessionEnd(report: Record<string, unknown>): void {
    appendFileSync(this.path, `${JSON.stringify({ type: 'session', ...report })}\n`);
  }
}

/** Sends sessions to the control plane. includeText=false leaves out what people said. */
export class ControlPlaneSink implements MetricsSink {
  private readonly url: string;
  private readonly token: string;
  private readonly includeText: boolean;

  constructor(apiUrl: string, token: string, options: { includeText?: boolean } = {}) {
    this.url = `${apiUrl.replace(/\/$/, '')}/api/v1/agent-sessions`;
    this.token = token;
    this.includeText = options.includeText ?? true;
  }

  async onSessionEnd(report: Record<string, unknown>): Promise<void> {
    if (!this.includeText) {
      for (const t of report.turns as Array<Record<string, unknown>>) {
        t.user_text = '';
        t.reply_text = '';
        t.played_text = '';
      }
    }
    const response = await fetch(this.url, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: `Bearer ${this.token}` },
      body: JSON.stringify(report),
    });
    if (!response.ok) console.warn(`the control plane refused the session: ${response.status}`);
  }
}

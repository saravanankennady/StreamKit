// An AgentSession runs a conversation with one user. This is the cascaded pipeline, the same as the Python framework's: the VAD
// and STT listen, the LLM answers and the TTS speaks. Realtime pipelines are not ported to Node yet.
import { randomUUID } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { APIError, ChatContext, type ChatChunk, type ChatItem, type ChatMessage, type FunctionCall, type FunctionCallOutput, type FunctionTool, type LLM, newId, StopResponse, ToolError, type Usage } from './llm.ts';
import { AudioFrame, AudioSource, AudioStream } from './rtc/audio.ts';
import { type MetricsSink, type Prices, SessionReport } from './models.ts';
import type { Generation, RealtimeModel, RealtimeSession } from './realtime.ts';
import type { LocalTrackPublication, Participant, Room, TextStreamReader, TextStreamWriter, TrackPublication } from './rtc/room.ts';
import type { RecognizeStream, STT, StreamingSpeechEvent, SynthesizedAudio, TTS, VAD, VADEvent } from './speech.ts';

const STATE_ATTRIBUTE = 'lk.agent.state';
const TRANSCRIPTION_TOPIC = 'lk.transcription';
const CHAT_TOPIC = 'lk.chat';
const ATTR_SEGMENT_ID = 'lk.segment_id';
const ATTR_FINAL = 'lk.transcription_final';
const ATTR_TRACK_ID = 'lk.transcribed_track_id';
const TRANSCRIPT_TICK_MS = 100;

export type AgentState = 'initializing' | 'idle' | 'listening' | 'thinking' | 'speaking';

export interface AgentOptions {
  instructions: string;
  tools?: FunctionTool[];
  chatCtx?: ChatContext;
  name?: string;
  stt?: STT;
  llm?: LLM;
  tts?: TTS;
  vad?: VAD;
}

export class Agent {
  instructions: string;
  tools: FunctionTool[];
  chatCtx: ChatContext;
  name: string;
  stt?: STT;
  llm?: LLM;
  tts?: TTS;
  vad?: VAD;
  session: AgentSession | null = null;

  constructor(options: AgentOptions) {
    this.instructions = options.instructions;
    this.tools = options.tools ?? [];
    this.chatCtx = options.chatCtx?.copy() ?? new ChatContext();
    this.name = options.name ?? this.constructor.name;
    this.stt = options.stt;
    this.llm = options.llm;
    this.tts = options.tts;
    this.vad = options.vad;
  }

  async onEnter(): Promise<void> {}
  async onExit(): Promise<void> {}
  async onUserTurnCompleted(_turnCtx: ChatContext, _newMessage: ChatMessage): Promise<void> {}

  llmNode(ctx: ChatContext, tools: FunctionTool[], llm: LLM, signal: AbortSignal): AsyncGenerator<ChatChunk> {
    return llm.chat(ctx, { tools, signal });
  }

  ttsNode(text: AsyncIterable<string>, tts: TTS, signal: AbortSignal): AsyncGenerator<SynthesizedAudio> {
    return tts.stream(text, signal);
  }
}

export class SpeechHandle {
  readonly id = randomUUID().slice(0, 12);
  readonly allowInterruptions: boolean;
  interrupted = false;
  private finished = false;
  private doneResolve!: () => void;
  private readonly donePromise: Promise<void>;
  readonly abort = new AbortController();

  constructor(allowInterruptions = true) {
    this.allowInterruptions = allowInterruptions;
    this.donePromise = new Promise((resolve) => (this.doneResolve = resolve));
  }

  interrupt(): void {
    if (this.allowInterruptions && !this.finished) {
      this.interrupted = true;
      this.abort.abort();
    }
  }

  finish(): void {
    this.finished = true;
    this.doneResolve();
  }

  done(): boolean {
    return this.finished;
  }

  waitForPlayout(): Promise<void> {
    return this.donePromise;
  }
}

/** Works out the words already heard, given how much audio has played. Inside a sentence, words are spread evenly: an estimate. */
export class PlayoutTranscript {
  // While a sentence's audio is still arriving its length is not known, so a typical speaking rate limits the words too.
  private static readonly WORDS_PER_SECOND = 2.6;
  private readonly segments: Array<{ text: string; start: number; end: number | null }> = [];
  pushedSeconds = 0;

  beginSegment(text: string): void {
    const last = this.segments.at(-1);
    if (last) last.end = this.pushedSeconds;
    this.segments.push({ text, start: this.pushedSeconds, end: null });
  }

  addAudio(seconds: number): void {
    this.pushedSeconds += seconds;
  }

  finish(): void {
    const last = this.segments.at(-1);
    if (last) last.end = this.pushedSeconds;
  }

  get fullText(): string {
    return this.segments.map((s) => s.text.trim()).filter(Boolean).join(' ');
  }

  textAt(position: number): string {
    const out: string[] = [];
    for (const segment of this.segments) {
      if (position <= segment.start) break;
      const end = segment.end ?? this.pushedSeconds;
      if (position >= end && segment.end !== null) {
        out.push(segment.text.trim());
        continue;
      }
      const words = segment.text.split(/\s+/).filter(Boolean);
      let count = Math.floor(Math.min(1, (position - segment.start) / Math.max(end - segment.start, 1e-6)) * words.length);
      if (segment.end === null) count = Math.min(count, Math.floor((position - segment.start) * PlayoutTranscript.WORDS_PER_SECOND));
      if (count > 0) out.push(words.slice(0, count).join(' '));
      break;
    }
    return out.join(' ');
  }
}

export interface UsageRecord {
  kind: 'llm' | 'stt' | 'tts' | 'realtime';
  model: string;
  usage: Usage;
}

export class TurnMetrics {
  turnId = randomUUID().slice(0, 12);
  pipeline = 'cascaded';
  speechStartedAt: number | null = null;
  speechEndedAt: number | null = null;
  transcriptAt: number | null = null;
  turnCommittedAt: number | null = null;
  llmStartedAt: number | null = null;
  llmFirstTokenAt: number | null = null;
  ttsStartedAt: number | null = null;
  ttsFirstAudioAt: number | null = null;
  playoutStartedAt: number | null = null;
  playoutFinishedAt: number | null = null;
  userText = '';
  replyText = '';
  playedText = '';
  interrupted = false;
  usage: UsageRecord[] = [];
  toolCalls: string[] = [];

  get latency() {
    const since = (end: number | null, start: number | null) => (end === null || start === null ? null : end - start);
    return {
      transcription: since(this.transcriptAt, this.speechEndedAt),
      endOfTurn: since(this.turnCommittedAt, this.speechEndedAt),
      llmFirstToken: since(this.llmFirstTokenAt, this.llmStartedAt),
      ttsFirstAudio: since(this.ttsFirstAudioAt, this.ttsStartedAt),
      endToEnd: since(this.playoutStartedAt, this.speechEndedAt),
    };
  }
}

export interface SessionOptions {
  stt?: STT;
  llm?: LLM;
  tts?: TTS;
  vad?: VAD;
  minEndpointingDelay?: number;
  allowInterruptions?: boolean;
  minInterruptionDuration?: number;
  falseInterruptionTimeout?: number;
  maxToolSteps?: number;
  userdata?: unknown;
  metricsSinks?: MetricsSink[];
  prices?: Prices;
  // A realtime model replaces the STT, LLM and TTS. duplex selects full duplex, where the provider finds turns. Otherwise the VAD does.
  realtime?: RealtimeModel;
  duplex?: boolean;
  // What decides the user has finished: the VAD, or a streaming STT that detects it. Defaults to the VAD if there is one.
  turnDetection?: 'vad' | 'stt';
  maxEndpointingDelay?: number;
}

/**
 * Events: agentStateChanged(old, new), userInputTranscribed(text, final), conversationItemAdded(item), agentFalseInterruption(),
 * metricsCollected(turn), error(source, error), close().
 */
export class AgentSession extends EventEmitter {
  readonly history = new ChatContext();
  readonly turns: TurnMetrics[] = [];
  readonly models: Record<string, string> = {};
  readonly userdata: unknown;
  readonly report = new SessionReport();
  private readonly metricsSinks: MetricsSink[];
  private readonly realtime?: RealtimeModel;
  private readonly duplex: boolean;
  // How many frames of the user's audio have arrived. A count above zero is what shows the agent can hear them.
  framesReceived = 0;
  private rt: RealtimeSession | null = null;
  private sttStream: RecognizeStream | null = null;
  private readonly turnDetectionOption?: 'vad' | 'stt';
  private readonly maxEndpointingDelay: number;
  private transcriptWaiters: Array<() => void> = [];
  private rtSending = false;
  state: AgentState = 'initializing';
  agent: Agent | null = null;
  room: Room | null = null;
  output = new AudioSource();
  private stt?: STT;
  private llm?: LLM;
  private tts?: TTS;
  private vad?: VAD;
  private readonly minEndpointingDelay: number;
  private readonly allowInterruptions: boolean;
  private readonly minInterruptionDuration: number;
  private readonly falseInterruptionTimeout: number;
  private readonly maxToolSteps: number;
  private publication: LocalTrackPublication | null = null;
  private linked: Participant | null = null;
  private audioStream: AudioStream | null = null;
  private trackSid = '';
  private queue: Array<{ handle: SpeechHandle; run: () => Promise<void>; metrics: TurnMetrics }> = [];
  private running = false;
  private closed = false;
  currentSpeech: SpeechHandle | null = null;
  private pausedByUser = false;
  private falseTimer: NodeJS.Timeout | null = null;
  private turn: TurnMetrics | null = null;
  private transcript: string[] = [];
  private commitTimer: NodeJS.Timeout | null = null;
  private recognizing = 0;
  private recognized: Array<() => void> = [];
  private position = 0;

  constructor(options: SessionOptions = {}) {
    super();
    this.stt = options.stt;
    this.llm = options.llm;
    this.tts = options.tts;
    this.vad = options.vad;
    this.minEndpointingDelay = options.minEndpointingDelay ?? 0.5;
    this.allowInterruptions = options.allowInterruptions ?? true;
    this.minInterruptionDuration = options.minInterruptionDuration ?? 0.5;
    this.falseInterruptionTimeout = options.falseInterruptionTimeout ?? 2;
    this.maxToolSteps = options.maxToolSteps ?? 3;
    this.userdata = options.userdata;
    this.metricsSinks = options.metricsSinks ?? [];
    this.realtime = options.realtime;
    this.turnDetectionOption = options.turnDetection;
    this.maxEndpointingDelay = options.maxEndpointingDelay ?? 3;
    this.duplex = options.duplex ?? true;
    this.report.prices = options.prices;
  }

  async start(agent: Agent, options: { room: Room; participantIdentity?: string; jobId?: string; agentName?: string }): Promise<void> {
    this.agent = agent;
    agent.session = this;
    this.room = options.room;
    this.report.room = options.room.name;
    this.report.jobId = options.jobId ?? '';
    this.report.agentName = options.agentName ?? '';
    this.stt = agent.stt ?? this.stt;
    this.vad = agent.vad ?? this.vad;
    if (this.realtime) {
      if (!this.duplex && !this.vad) throw new Error('turn based realtime needs a VAD to find where turns end');
      this.models.realtime = this.realtime.label;
      this.report.pipeline = this.duplex ? 'realtime-duplex' : 'realtime-turn';
      await this.startRealtime(agent);
    } else if (!this.vad && !this.stt?.streaming) {
      throw new Error('an STT that takes one utterance at a time needs a VAD to cut them out');
    }
    if (this.stt && !this.realtime) {
      this.models.stt = this.stt.label;
      if (this.stt.streaming) {
        this.sttStream = this.stt.stream();
        this.sttStream.onEvent = (event) => this.onSttEvent(event);
      }
    }
    if (this.vad) this.vad.onEvent = (event) => (this.realtime ? this.onRealtimeVad(event) : this.onVad(event));

    const room = options.room;
    const accepts = (p: Participant) => (options.participantIdentity ? p.identity === options.participantIdentity : !p.isAgent);
    const link = (p: Participant) => {
      if (!this.linked && accepts(p)) this.linked = p;
    };
    const subscribe = (track: TrackPublication['track'], pub: TrackPublication, p: Participant) => {
      link(p);
      if (!this.linked || p.sid !== this.linked.sid || pub.kind !== 'audio' || pub.source !== 'microphone' || !track) return;
      this.audioStream?.close();
      this.trackSid = pub.sid;
      this.audioStream = new AudioStream(track, this.realtime?.inputSampleRate ?? 16000, (frame) => this.pushAudio(frame));
    };
    room.on('participantConnected', link);
    room.on('participantDisconnected', (p: Participant) => {
      if (this.linked && p.sid === this.linked.sid) {
        this.linked = null;
        this.audioStream?.close();
        this.audioStream = null;
        void this.close();
      }
    });
    room.on('trackSubscribed', subscribe);
    room.registerTextStreamHandler(CHAT_TOPIC, async (reader: TextStreamReader) => {
      const text = await reader.readAll();
      if (text.trim()) {
        this.interrupt();
        this.generateReply({ userInput: text });
      }
    });
    this.publication = await room.publishTrack(this.output, { name: 'agent_audio' });
    for (const p of room.remoteParticipants.values()) {
      link(p);
      for (const pub of p.tracks.values()) if (pub.track) subscribe(pub.track, pub, p);
    }
    this.setState('listening');
    await agent.onEnter();
  }

  private pushAudio(frame: AudioFrame): void {
    if (this.closed) return;
    this.framesReceived += 1;
    if (this.rt) {
      if (this.duplex) {
        this.rt.pushAudio(frame);
        return;
      }
      if (this.rtSending) this.rt.pushAudio(frame);
    }
    this.vad?.pushFrame(frame);
    if (this.sttStream) {
      // Speech that cannot be interrupted must not end up in the next turn's transcript.
      const silent = this.currentSpeech !== null && !this.currentSpeech.allowInterruptions;
      this.sttStream.pushFrame(silent ? new AudioFrame(new Int16Array(frame.data.length), frame.sampleRate, frame.numChannels) : frame);
    }
  }

  private get turnDetection(): 'vad' | 'stt' {
    return this.turnDetectionOption ?? (this.vad ? 'vad' : 'stt');
  }

  private onSttEvent(event: StreamingSpeechEvent): void {
    switch (event.type) {
      case 'interim':
        this.turn ??= new TurnMetrics();
        // With no VAD, words arriving are the first sign the user is talking over the agent.
        if (!this.vad) this.pauseForInterruption();
        this.emit('userInputTranscribed', event.text, false);
        break;
      case 'final':
        if (event.text.trim()) this.addFinalTranscript(event.text.trim());
        break;
      case 'end_of_speech':
        this.turn ??= new TurnMetrics();
        this.turn.speechEndedAt ??= Date.now() / 1000;
        if (this.turnDetection === 'stt') {
          this.cancelCommit();
          this.commitTimer = setTimeout(() => void this.commit(), 0);
        }
        break;
      case 'usage':
        (this.turn ?? this.turns.at(-1))?.usage.push({ kind: 'stt', model: this.stt!.label, usage: event.usage });
        break;
      default:
        break;
    }
  }

  private addFinalTranscript(text: string): void {
    this.turn ??= new TurnMetrics();
    this.turn.transcriptAt = Date.now() / 1000;
    this.transcript.push(text);
    for (const fn of this.transcriptWaiters.splice(0)) fn();
    this.emit('userInputTranscribed', text, true);
    this.publishUserTranscript(text);
    this.confirmInterruption();
  }

  private async startRealtime(agent: Agent): Promise<void> {
    const rt = this.realtime!.session({ instructions: agent.instructions, tools: agent.tools, serverTurnDetection: this.duplex });
    this.rt = rt;
    rt.on('generation', (generation: Generation) => this.onGeneration(generation));
    rt.on('speechStarted', () => {
      this.turn ??= new TurnMetrics();
      this.turn.speechStartedAt ??= Date.now() / 1000;
      // The provider has already stopped generating, and what is queued here must stop too.
      this.confirmInterruption();
    });
    rt.on('speechStopped', () => {
      this.turn ??= new TurnMetrics();
      this.turn.speechEndedAt = Date.now() / 1000;
      this.turn.turnCommittedAt = this.turn.speechEndedAt;
    });
    rt.on('inputTranscribed', (text: string, final: boolean) => {
      const target = this.turn ?? this.turns.at(-1);
      if (target) target.userText = text;
      if (this.turn) this.turn.transcriptAt = Date.now() / 1000;
      this.emit('userInputTranscribed', text, final);
      if (final && text.trim()) {
        const message: ChatMessage = { type: 'message', id: newId(), role: 'user', content: text.trim(), interrupted: false };
        agent.chatCtx.add(message);
        this.record(message);
        this.publishUserTranscript(text.trim());
      }
    });
    rt.on('error', (e: Error) => this.emit('error', 'realtime', e));
    await rt.start();
  }

  private onRealtimeVad(event: VADEvent): void {
    if (event.type === 'start_of_speech') {
      this.turn ??= new TurnMetrics();
      this.turn.speechStartedAt ??= Date.now() / 1000 - event.speechDuration;
      this.cancelCommit();
      this.rtSending = true;
    } else if (event.type === 'inference_done') {
      if (event.speaking && event.speechDuration >= this.minInterruptionDuration && this.currentSpeech) this.confirmInterruption();
    } else if (event.type === 'end_of_speech') {
      if (this.turn) this.turn.speechEndedAt = Date.now() / 1000 - event.silenceDuration;
      this.cancelCommit();
      this.commitTimer = setTimeout(() => {
        this.commitTimer = null;
        this.rtSending = false;
        if (this.turn) this.turn.turnCommittedAt = Date.now() / 1000;
        this.rt?.commitAudio();
        this.rt?.generateReply();
      }, this.minEndpointingDelay * 1000);
    }
  }

  private onGeneration(generation: Generation): void {
    const metrics = this.turn ?? new TurnMetrics();
    this.turn = null;
    metrics.pipeline = this.report.pipeline;
    metrics.llmStartedAt ??= Date.now() / 1000;
    const handle = new SpeechHandle(this.allowInterruptions);
    this.enqueue(handle, () => this.forwardGeneration(handle, generation, metrics), metrics);
  }

  /** Plays one realtime reply. Words are spread over the audio once it has all arrived, and limited by a speaking rate before that. */
  private async forwardGeneration(handle: SpeechHandle, generation: Generation, metrics: TurnMetrics): Promise<void> {
    const output = this.output;
    const start = output.playedSeconds;
    this.setState('thinking');
    const textAt = (position: number): string => {
      if (generation.audioSeconds <= 0 || position <= 0) return '';
      const share = Math.min(1, position / generation.audioSeconds);
      if (generation.done && share >= 1) return generation.text.trim();
      const words = generation.text.split(/\s+/).filter(Boolean);
      let count = Math.floor(share * words.length);
      if (!generation.done) count = Math.min(count, Math.floor(position * 2.6));
      return words.slice(0, count).join(' ');
    };
    const writer: TextStreamWriter | null = this.room
      ? this.room.streamText(TRANSCRIPTION_TOPIC, { attributes: { [ATTR_SEGMENT_ID]: newId(), [ATTR_TRACK_ID]: this.publication?.sid ?? '', [ATTR_FINAL]: 'false' } })
      : null;
    let heardSent = 0;
    output.onNextAudio((at) => {
      metrics.playoutStartedAt ??= at;
      this.setState('speaking');
    });
    const release = setInterval(() => {
      const heard = textAt(output.playedSeconds - start);
      if (writer && heard.length > heardSent) {
        writer.write(heard.slice(heardSent));
        heardSent = heard.length;
      }
    }, TRANSCRIPT_TICK_MS);
    const interrupted = new Promise<void>((resolve) => handle.abort.signal.addEventListener('abort', () => resolve(), { once: true }));
    const pump = (async () => {
      while (!handle.interrupted) {
        const frame = generation.audio.shift();
        if (frame) {
          if (metrics.ttsFirstAudioAt === null) {
            metrics.ttsFirstAudioAt = Date.now() / 1000;
            metrics.ttsStartedAt = metrics.llmStartedAt;
            metrics.llmFirstTokenAt = metrics.ttsFirstAudioAt;
          }
          await output.captureFrame(frame);
        } else if (generation.done) {
          return;
        } else {
          await generation.changed();
        }
      }
    })();
    try {
      await Promise.race([pump.then(() => output.waitForPlayout()), interrupted]);
    } finally {
      clearInterval(release);
      output.onNextAudio(null);
    }
    const position = output.playedSeconds - start;
    if (handle.interrupted) {
      output.clearQueue();
      output.resume();
      generation.notify();
      this.rt?.interrupt();
      if (this.rt?.capabilities.messageTruncation && generation.itemId) this.rt.truncate(generation.itemId, Math.floor(position * 1000));
    } else {
      metrics.playoutFinishedAt = Date.now() / 1000;
    }
    const heard = handle.interrupted ? textAt(position) : generation.text.trim();
    metrics.replyText = generation.text.trim();
    metrics.playedText = heard;
    if (writer) {
      if (heard.length > heardSent) writer.write(heard.slice(heardSent));
      writer.close({ [ATTR_FINAL]: 'true' });
    }
    const agent = this.agent!;
    if (heard) {
      const message: ChatMessage = { type: 'message', id: newId(), role: 'assistant', content: heard, interrupted: handle.interrupted };
      agent.chatCtx.add(message);
      this.record(message);
    }
    if (handle.interrupted) return;
    while (!generation.done) await generation.changed();
    if (generation.usage) metrics.usage.push({ kind: 'realtime', model: this.realtime!.label, usage: generation.usage });
    const calls = generation.functionCalls;
    if (calls.length === 0) return;
    for (const call of calls) {
      agent.chatCtx.add(call);
      this.record(call);
    }
    const { outputs, target, stop } = await this.runTools(calls, agent.tools, handle, metrics);
    outputs.forEach((o, i) => {
      agent.chatCtx.add(o);
      this.record(o);
      this.rt?.sendToolResult(calls[i], o.output, o.isError);
    });
    if (target) {
      await this.updateAgent(target);
      return;
    }
    if (!stop && !this.rt?.capabilities.autoToolReply) this.rt?.generateReply();
  }

  private setState(state: AgentState): void {
    if (state === this.state) return;
    const old = this.state;
    this.state = state;
    this.emit('agentStateChanged', old, state);
    if (this.room?.connected) this.room.setAttributes({ [STATE_ATTRIBUTE]: state });
  }

  private record(item: ChatItem): void {
    this.history.add(item);
    this.emit('conversationItemAdded', item);
  }

  say(text: string, options: { allowInterruptions?: boolean } = {}): SpeechHandle {
    const handle = new SpeechHandle(options.allowInterruptions ?? this.allowInterruptions);
    const metrics = new TurnMetrics();
    this.enqueue(handle, () => this.runSay(handle, text, metrics), metrics);
    return handle;
  }

  generateReply(options: { userInput?: string; instructions?: string; allowInterruptions?: boolean; metrics?: TurnMetrics } = {}): SpeechHandle {
    const handle = new SpeechHandle(options.allowInterruptions ?? this.allowInterruptions);
    const metrics = options.metrics ?? new TurnMetrics();
    this.enqueue(handle, () => this.runReply(handle, options.userInput, options.instructions, metrics), metrics);
    return handle;
  }

  private enqueue(handle: SpeechHandle, run: () => Promise<void>, metrics: TurnMetrics): void {
    this.queue.push({ handle, run, metrics });
    if (!this.running) void this.speechLoop();
  }

  private async speechLoop(): Promise<void> {
    this.running = true;
    while (this.queue.length > 0 && !this.closed) {
      const { handle, run, metrics } = this.queue.shift()!;
      if (handle.interrupted) {
        handle.finish();
        continue;
      }
      this.currentSpeech = handle;
      try {
        await run();
      } catch (e) {
        if (!(e instanceof StopResponse) && (e as Error).name !== 'AbortError') {
          console.error('a reply failed', e);
          this.emit('error', 'reply', e);
        }
      }
      handle.finish();
      this.currentSpeech = null;
      this.cancelFalseTimer();
      this.pausedByUser = false;
      metrics.interrupted = handle.interrupted;
      if (metrics.llmStartedAt || metrics.ttsStartedAt || metrics.playoutStartedAt) {
        this.turns.push(metrics);
        this.emit('metricsCollected', metrics);
      }
    }
    this.running = false;
    if (!this.closed) this.setState('listening');
  }

  interrupt(): void {
    this.currentSpeech?.interrupt();
    const keep = [];
    for (const job of this.queue) {
      if (job.handle.allowInterruptions) {
        job.handle.interrupt();
        job.handle.finish();
      } else keep.push(job);
    }
    this.queue = keep;
  }

  private onVad(event: VADEvent): void {
    if (event.type === 'start_of_speech') {
      this.turn ??= new TurnMetrics();
      this.turn.speechStartedAt ??= Date.now() / 1000 - event.speechDuration;
      this.cancelCommit();
    } else if (event.type === 'inference_done') {
      if (event.speaking && event.speechDuration >= this.minInterruptionDuration) this.pauseForInterruption();
    } else if (event.type === 'end_of_speech') {
      this.turn ??= new TurnMetrics();
      this.turn.speechEndedAt = Date.now() / 1000 - event.silenceDuration;
      if (this.pausedByUser && this.falseInterruptionTimeout > 0) {
        this.cancelFalseTimer();
        this.falseTimer = setTimeout(() => this.resumeAfterFalseInterruption(), this.falseInterruptionTimeout * 1000);
      }
      if (this.stt && !this.stt.streaming && !(this.currentSpeech && !this.currentSpeech.allowInterruptions)) void this.recognize(event.audio);
      if (this.turnDetection === 'vad') this.startCommit();
    }
  }

  private pauseForInterruption(): void {
    const speech = this.currentSpeech;
    if (!speech || !speech.allowInterruptions || this.pausedByUser) return;
    this.pausedByUser = true;
    this.cancelFalseTimer();
    this.output.pause();
    this.setState('listening');
  }

  private resumeAfterFalseInterruption(): void {
    this.falseTimer = null;
    if (!this.pausedByUser) return;
    this.pausedByUser = false;
    this.output.resume();
    if (this.currentSpeech) this.setState('speaking');
    this.emit('agentFalseInterruption');
  }

  private cancelFalseTimer(): void {
    if (this.falseTimer) clearTimeout(this.falseTimer);
    this.falseTimer = null;
  }

  private confirmInterruption(): void {
    this.cancelFalseTimer();
    this.pausedByUser = false;
    this.interrupt();
  }

  private async recognize(audio: AudioFrame): Promise<void> {
    this.recognizing += 1;
    try {
      const event = await this.stt!.recognize(audio);
      if (event.usage && this.turn) this.turn.usage.push({ kind: 'stt', model: this.stt!.label, usage: event.usage });
      if (event.text) {
        this.addFinalTranscript(event.text);
      } else if (this.pausedByUser) {
        this.resumeAfterFalseInterruption();
      }
    } catch (e) {
      this.emit('error', 'stt', e);
    } finally {
      this.recognizing -= 1;
      if (this.recognizing === 0) {
        const waiting = this.recognized;
        this.recognized = [];
        for (const fn of waiting) fn();
      }
    }
  }

  private publishUserTranscript(text: string): void {
    if (!this.room) return;
    const writer = this.room.streamText(TRANSCRIPTION_TOPIC, {
      senderIdentity: this.linked?.identity,
      attributes: { [ATTR_SEGMENT_ID]: newId(), [ATTR_TRACK_ID]: this.trackSid, [ATTR_FINAL]: 'true' },
    });
    writer.write(text);
    writer.close({ [ATTR_FINAL]: 'true' });
  }

  private cancelCommit(): void {
    if (this.commitTimer) clearTimeout(this.commitTimer);
    this.commitTimer = null;
  }

  private startCommit(): void {
    this.cancelCommit();
    const ended = this.turn?.speechEndedAt ?? Date.now() / 1000;
    const wait = Math.max(0, this.minEndpointingDelay - (Date.now() / 1000 - ended));
    this.commitTimer = setTimeout(() => void this.commit(), wait * 1000);
  }

  private async commit(): Promise<void> {
    this.commitTimer = null;
    if (this.recognizing > 0) await new Promise<void>((resolve) => this.recognized.push(resolve));
    if (this.sttStream && this.transcript.length === 0 && this.turn) {
      // A streaming STT has been told to finish, and its transcript is on the way. If nothing comes it heard no words.
      this.sttStream.flush();
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, this.maxEndpointingDelay * 1000);
        this.transcriptWaiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
    if (this.commitTimer) return;
    const text = this.transcript.join(' ').trim();
    const turn = this.turn;
    this.turn = null;
    this.transcript = [];
    if (!text || !turn) return;
    turn.turnCommittedAt = Date.now() / 1000;
    turn.userText = text;
    this.confirmInterruption();
    this.generateReply({ userInput: text, metrics: turn });
  }

  private async runReply(handle: SpeechHandle, userInput: string | undefined, instructions: string | undefined, metrics: TurnMetrics): Promise<void> {
    const agent = this.agent!;
    if (this.rt) {
      // The reply arrives as its own generation. This only asks for it.
      if (userInput) {
        const message: ChatMessage = { type: 'message', id: newId(), role: 'user', content: userInput, interrupted: false };
        agent.chatCtx.add(message);
        this.record(message);
      }
      this.rt.generateReply(userInput ? `${instructions ? `${instructions}\n\n` : ''}The user said: ${userInput}` : instructions);
      return;
    }
    if (userInput) {
      const message: ChatMessage = { type: 'message', id: newId(), role: 'user', content: userInput, interrupted: false };
      metrics.userText ||= userInput;
      await agent.onUserTurnCompleted(agent.chatCtx.copy(), message);
      agent.chatCtx.add(message);
      this.record(message);
    }
    this.setState('thinking');
    await this.publication?.waitForSubscription(10000);
    let oneOff = instructions;
    for (let step = 0; step <= this.maxToolSteps; step++) {
      const ctx = agent.chatCtx.copy();
      ctx.setInstructions(agent.instructions);
      if (oneOff) {
        ctx.addMessage('system', oneOff);
        oneOff = undefined;
      }
      const tools = step < this.maxToolSteps ? agent.tools : [];
      const calls = await this.speakLLMStep(handle, ctx, tools, metrics);
      if (handle.interrupted || calls.length === 0) return;
      for (const call of calls) {
        agent.chatCtx.add(call);
        this.record(call);
      }
      const { outputs, target, stop } = await this.runTools(calls, agent.tools, handle, metrics);
      for (const output of outputs) {
        agent.chatCtx.add(output);
        this.record(output);
      }
      if (handle.interrupted) return;
      if (target) {
        await this.updateAgent(target);
        return;
      }
      if (stop) return;
    }
  }

  private async runSay(handle: SpeechHandle, text: string, metrics: TurnMetrics): Promise<void> {
    if (this.rt && !(this.agent!.tts ?? this.tts)) {
      this.rt.generateReply(`Say exactly this, and nothing else: ${text}`);
      return;
    }
    await this.publication?.waitForSubscription(10000);
    async function* once() {
      yield text;
    }
    const transcript = await this.play(handle, once(), metrics);
    this.storeReply(handle, transcript, metrics);
  }

  private storeReply(handle: SpeechHandle, transcript: PlayoutTranscript, metrics: TurnMetrics): void {
    metrics.replyText = transcript.fullText;
    const heard = handle.interrupted ? transcript.textAt(this.position) : transcript.fullText;
    metrics.playedText = heard;
    if (heard) {
      const message: ChatMessage = { type: 'message', id: newId(), role: 'assistant', content: heard, interrupted: handle.interrupted };
      this.agent!.chatCtx.add(message);
      this.record(message);
    }
  }

  private async speakLLMStep(handle: SpeechHandle, ctx: ChatContext, tools: FunctionTool[], metrics: TurnMetrics): Promise<FunctionCall[]> {
    const llm = this.agent!.llm ?? this.llm;
    if (!llm) throw new Error('no LLM is configured for this agent');
    this.models.llm = llm.label;
    const calls: FunctionCall[] = [];
    const queue: Array<string | null> = [];
    let wake: (() => void) | null = null;
    const put = (item: string | null) => {
      queue.push(item);
      wake?.();
      wake = null;
    };
    const runLLM = (async () => {
      metrics.llmStartedAt ??= Date.now() / 1000;
      try {
        for await (const chunk of this.agent!.llmNode(ctx, tools, llm, handle.abort.signal)) {
          if (chunk.text) {
            metrics.llmFirstTokenAt ??= Date.now() / 1000;
            put(chunk.text);
          }
          if (chunk.toolCalls) calls.push(...chunk.toolCalls);
          if (chunk.usage) metrics.usage.push({ kind: 'llm', model: llm.label, usage: chunk.usage });
        }
      } finally {
        put(null);
      }
    })();
    async function* textStream() {
      while (true) {
        while (queue.length === 0) await new Promise<void>((resolve) => (wake = resolve));
        const item = queue.shift()!;
        if (item === null) return;
        yield item;
      }
    }
    try {
      const transcript = await this.play(handle, textStream(), metrics);
      this.storeReply(handle, transcript, metrics);
      if (handle.interrupted) {
        await runLLM.catch(() => {});
        return [];
      }
      await runLLM;
    } catch (e) {
      if (e instanceof APIError) {
        this.emit('error', 'reply', e);
        return [];
      }
      throw e;
    }
    return calls;
  }

  private async play(handle: SpeechHandle, text: AsyncIterable<string>, metrics: TurnMetrics): Promise<PlayoutTranscript> {
    const tts = this.agent!.tts ?? this.tts;
    if (!tts) throw new Error('no TTS is configured for this agent');
    this.models.tts = tts.label;
    const output = this.output;
    const transcript = new PlayoutTranscript();
    const start = output.playedSeconds;
    this.position = 0;
    const writer: TextStreamWriter | null = this.room
      ? this.room.streamText(TRANSCRIPTION_TOPIC, { attributes: { [ATTR_SEGMENT_ID]: newId(), [ATTR_TRACK_ID]: this.publication?.sid ?? '', [ATTR_FINAL]: 'false' } })
      : null;
    let heardSent = 0;
    let spoke = false;
    output.onNextAudio((at) => {
      metrics.playoutStartedAt ??= at;
      spoke = true;
      if (!this.pausedByUser) this.setState('speaking');
    });
    const release = setInterval(() => {
      if (!writer) return;
      const heard = transcript.textAt(output.playedSeconds - start);
      if (heard.length > heardSent) {
        writer.write(heard.slice(heardSent));
        heardSent = heard.length;
      }
    }, TRANSCRIPT_TICK_MS);
    // The TTS runs ahead of playout, into this queue. A TTS is usually faster than speech, so what each sentence's audio adds
    // up to is known well before it has played, which is what maps played audio back to words.
    const audioQueue: Array<AudioFrame | null> = [];
    let audioWake: (() => void) | null = null;
    const putAudio = (frame: AudioFrame | null) => {
      audioQueue.push(frame);
      audioWake?.();
      audioWake = null;
    };
    const synth = (async () => {
      try {
        for await (const audio of this.agent!.ttsNode(text, tts, handle.abort.signal)) {
          if (handle.interrupted) return;
          if (metrics.ttsFirstAudioAt === null) {
            metrics.ttsFirstAudioAt = Date.now() / 1000;
            metrics.ttsStartedAt = tts.segmentStartedAt ?? metrics.ttsFirstAudioAt;
          }
          if (audio.segmentText !== undefined) transcript.beginSegment(audio.segmentText);
          transcript.addAudio(audio.frame.duration);
          putAudio(audio.frame);
        }
        transcript.finish();
        if (tts.lastUsage) metrics.usage.push({ kind: 'tts', model: tts.label, usage: tts.lastUsage });
      } finally {
        putAudio(null);
      }
    })();
    const feed = (async () => {
      while (true) {
        while (audioQueue.length === 0) await new Promise<void>((resolve) => (audioWake = resolve));
        const frame = audioQueue.shift()!;
        if (frame === null || handle.interrupted) return;
        await output.captureFrame(frame);
      }
    })();
    const interrupted = new Promise<void>((resolve) => handle.abort.signal.addEventListener('abort', () => resolve(), { once: true }));
    try {
      await Promise.race([feed.then(() => synth).then(() => output.waitForPlayout()), interrupted]);
    } finally {
      clearInterval(release);
      output.onNextAudio(null);
    }
    this.position = output.playedSeconds - start;
    if (handle.interrupted) {
      output.clearQueue();
      output.resume();
      await synth.catch(() => {});
    } else {
      metrics.playoutFinishedAt = Date.now() / 1000;
      await synth;
    }
    if (writer && spoke) {
      const heard = handle.interrupted ? transcript.textAt(this.position) : transcript.fullText;
      if (heard.length > heardSent) writer.write(heard.slice(heardSent));
      writer.close({ [ATTR_FINAL]: 'true' });
    }
    return transcript;
  }

  private async runTools(calls: FunctionCall[], tools: FunctionTool[], handle: SpeechHandle, metrics: TurnMetrics): Promise<{ outputs: FunctionCallOutput[]; target: Agent | null; stop: boolean }> {
    const byName = new Map(tools.map((t) => [t.name, t]));
    const handoffs = new Map<string, Agent>();
    let stop = false;
    const output = (call: FunctionCall, text: string, isError = false): FunctionCallOutput => ({ type: 'function_call_output', id: newId(), callId: call.callId, name: call.name, output: text, isError });
    const outputs = await Promise.all(
      calls.map(async (call) => {
        metrics.toolCalls.push(call.name);
        const tool = byName.get(call.name);
        if (!tool) return output(call, `there is no tool called ${call.name}`, true);
        try {
          const args = JSON.parse(call.arguments || '{}');
          const result = await tool.execute(args, { session: this, speechHandle: handle, functionCall: call, userdata: this.userdata });
          if (result instanceof Agent) {
            handoffs.set(call.callId, result);
            return output(call, `transferred to ${result.name}`);
          }
          return output(call, result === undefined || result === null ? '' : String(result));
        } catch (e) {
          if (e instanceof StopResponse) {
            stop = true;
            return output(call, '');
          }
          if (e instanceof ToolError) return output(call, e.message, true);
          console.error(`tool ${call.name} failed`, e);
          // The details stay in the log. They may hold things the model should not repeat aloud.
          return output(call, 'the tool failed', true);
        }
      }),
    );
    let target: Agent | null = null;
    if (handoffs.size > 1) {
      for (const o of outputs) {
        if (handoffs.has(o.callId)) {
          o.output = 'only one handoff can happen at a time, so this one did not';
          o.isError = true;
        }
      }
    } else if (handoffs.size === 1) {
      target = [...handoffs.values()][0];
    }
    return { outputs, target, stop };
  }

  /** Hands the conversation to another agent. Its first reply comes from its onEnter, or the user's next turn. */
  async updateAgent(agent: Agent): Promise<void> {
    const old = this.agent;
    if (old) {
      await old.onExit();
      this.record({ type: 'agent_handoff', id: newId(), oldAgent: old.name, newAgent: agent.name });
    }
    this.agent = agent;
    agent.session = this;
    if (this.rt) {
      const previous = this.rt;
      await this.startRealtime(agent);
      await previous.close();
    }
    await agent.onEnter();
  }

  /** Changes models from the next reply on, without ending the session. */
  updateModels(models: { llm?: LLM; stt?: STT; tts?: TTS }): void {
    if (models.llm) {
      this.llm = models.llm;
      if (this.agent) this.agent.llm = undefined;
    }
    if (models.stt) this.stt = models.stt;
    if (models.tts) {
      this.tts = models.tts;
      if (this.agent) this.agent.tts = undefined;
    }
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.interrupt();
    this.cancelCommit();
    this.cancelFalseTimer();
    this.audioStream?.close();
    this.output.stop();
    await this.rt?.close();
    this.sttStream?.endInput();
    this.sttStream?.close();
    await this.agent?.onExit();
    for (const sink of this.metricsSinks) {
      try {
        await sink.onSessionEnd(this.report.toDict(this));
      } catch (e) {
        console.error('a metrics sink failed', e);
      }
    }
    this.emit('close');
  }
}

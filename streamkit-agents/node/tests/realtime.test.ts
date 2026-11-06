// Realtime pipelines over real WebRTC, with a local fixture in place of the model. THE MODEL IS A FIXTURE: the room, the media and
// the session plumbing are real, and the "model" replays the documented protocol and speaks a tone. No real realtime model has
// been run against this code.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import { type WebSocket, WebSocketServer } from 'ws';
import { Agent, AgentSession, AssemblyAISTT, CartesiaTTS, DeepgramSTT, EnergyVAD, functionTool, GeminiLiveModel, type JobContext, type LLM, OpenAICompatibleLLM, OpenAIRealtimeModel, type RealtimeModel, type STT, type TTS, Worker } from '../src/index.ts';
import { createServer } from 'node:http';
import { adminToken, agentIsListening, API_KEY, API_SECRET, eventually, type RunningServer, sleep, startServer, TestUser, tone, twirp, uniqueRoom, wantedServers } from './harness.ts';

const KEY = 'sk-fixture-secret-key';

function energy(b: Buffer): number {
  let sum = 0;
  const n = Math.floor(b.length / 2);
  for (let i = 0; i < n; i++) {
    const s = b.readInt16LE(i * 2) / 32768;
    sum += s * s;
  }
  return n ? Math.sqrt(sum / n) : 0;
}

function pcmTone(seconds: number, rate: number): Buffer {
  const out = Buffer.alloc(Math.floor(seconds * rate) * 2);
  for (let i = 0; i < out.length / 2; i++) out.writeInt16LE(Math.round(Math.sin((2 * Math.PI * 220 * i) / rate) * 0.25 * 32767), i * 2);
  return out;
}

/** Speaks OpenAI's documented Realtime events, with a loudness check for its turn detection. */
class OpenAIFixture {
  readonly events: Array<Record<string, any>> = [];
  readonly truncated: Array<Record<string, any>> = [];
  readonly toolOutputs: Array<Record<string, any>> = [];
  session: Record<string, any> = {};
  headers: Record<string, unknown> = {};
  responses = 0;
  url = '';
  private server!: WebSocketServer;
  private stop = false;
  private speaking = false;
  private quiet = 0;
  private readonly voice: Buffer;
  private readonly transcript: string;
  private readonly toolCall?: { call_id: string; name: string; arguments: string };

  constructor(options: { replySeconds: number; transcript?: string; toolCall?: { call_id: string; name: string; arguments: string } }) {
    this.voice = pcmTone(options.replySeconds, 24000);
    this.transcript = options.transcript ?? 'hello from the fixture model';
    this.toolCall = options.toolCall;
  }

  async start(): Promise<void> {
    this.server = new WebSocketServer({ port: 0, host: '127.0.0.1' });
    await new Promise((resolve) => this.server.once('listening', resolve));
    this.url = `ws://127.0.0.1:${(this.server.address() as { port: number }).port}/v1/realtime`;
    this.server.on('connection', (ws, req) => {
      this.headers = req.headers;
      ws.on('message', (raw: Buffer) => this.handle(ws, JSON.parse(raw.toString())));
    });
  }

  close(): void {
    for (const c of this.server.clients) c.terminate();
    this.server.close();
  }

  private send(ws: WebSocket, event: unknown): void {
    if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(event));
  }

  private handle(ws: WebSocket, event: Record<string, any>): void {
    this.events.push(event.type === 'input_audio_buffer.append' ? { type: event.type } : event);
    switch (event.type) {
      case 'session.update':
        Object.assign(this.session, event.session);
        this.send(ws, { type: 'session.updated', session: this.session });
        break;
      case 'input_audio_buffer.append': {
        const data = Buffer.from(event.audio, 'base64');
        if (this.session.audio?.input?.turn_detection) this.detect(ws, data);
        break;
      }
      case 'input_audio_buffer.commit':
        this.send(ws, { type: 'conversation.item.input_audio_transcription.completed', item_id: 'u', content_index: 0, transcript: 'what the user said' });
        break;
      case 'response.create':
        void this.respond(ws);
        break;
      case 'response.cancel':
        this.stop = true;
        break;
      case 'conversation.item.truncate':
        this.truncated.push(event);
        break;
      case 'conversation.item.create':
        if (event.item?.type === 'function_call_output') this.toolOutputs.push(event.item);
        break;
      default:
        break;
    }
  }

  private detect(ws: WebSocket, data: Buffer): void {
    const loud = energy(data) > 0.02;
    if (loud && !this.speaking) {
      this.speaking = true;
      this.quiet = 0;
      this.stop = true;
      this.send(ws, { type: 'input_audio_buffer.speech_started', audio_start_ms: 0, item_id: 'u' });
    } else if (this.speaking && !loud) {
      this.quiet += data.length / 2 / 24000;
      if (this.quiet > 0.3) {
        this.speaking = false;
        this.send(ws, { type: 'input_audio_buffer.speech_stopped', audio_end_ms: 0, item_id: 'u' });
        this.send(ws, { type: 'conversation.item.input_audio_transcription.completed', item_id: 'u', content_index: 0, transcript: 'what the user said' });
        void this.respond(ws);
      }
    } else if (this.speaking && loud) {
      this.quiet = 0;
    }
  }

  private async respond(ws: WebSocket): Promise<void> {
    this.responses += 1;
    const id = `resp_${this.responses}`;
    this.stop = false;
    this.send(ws, { type: 'response.created', response: { id, status: 'in_progress', output: [] } });
    if (this.toolCall && this.toolOutputs.length === 0) {
      this.send(ws, { type: 'response.function_call_arguments.done', response_id: id, ...this.toolCall });
      this.send(ws, { type: 'response.done', response: { id, status: 'completed', usage: { input_tokens: 10, output_tokens: 2 } } });
      return;
    }
    const itemId = `item_${id}`;
    this.send(ws, { type: 'response.output_item.added', response_id: id, item: { id: itemId, type: 'message' } });
    for (const word of this.transcript.split(' ')) this.send(ws, { type: 'response.output_audio_transcript.delta', response_id: id, item_id: itemId, delta: `${word} ` });
    const chunk = 4800;
    let status = 'completed';
    for (let at = 0; at < this.voice.length; at += chunk) {
      if (this.stop) {
        status = 'cancelled';
        break;
      }
      this.send(ws, { type: 'response.output_audio.delta', response_id: id, item_id: itemId, delta: this.voice.subarray(at, at + chunk).toString('base64') });
      await sleep(90);
    }
    this.send(ws, { type: 'response.done', response: { id, status, usage: { input_tokens: 100, output_tokens: 50, input_token_details: { cached_tokens: 5 } } } });
  }
}

/** Speaks Gemini's documented Live messages, with the same limits. */
class GeminiFixture {
  setup: Record<string, any> = {};
  readonly messages: Array<Record<string, any>> = [];
  readonly toolResponses: Array<Record<string, any>> = [];
  path = '';
  url = '';
  private server!: WebSocketServer;
  private stop = false;
  private speaking = false;
  private quiet = 0;
  private responding = false;
  private readonly voice: Buffer;
  private readonly toolCall?: Record<string, unknown>;

  constructor(options: { replySeconds: number; toolCall?: Record<string, unknown> }) {
    this.voice = pcmTone(options.replySeconds, 24000);
    this.toolCall = options.toolCall;
  }

  async start(): Promise<void> {
    this.server = new WebSocketServer({ port: 0, host: '127.0.0.1' });
    await new Promise((resolve) => this.server.once('listening', resolve));
    this.url = `ws://127.0.0.1:${(this.server.address() as { port: number }).port}`;
    this.server.on('connection', (ws, req) => {
      this.path = req.url ?? '';
      ws.on('message', (raw: Buffer) => this.handle(ws, JSON.parse(raw.toString())));
    });
  }

  close(): void {
    for (const c of this.server.clients) c.terminate();
    this.server.close();
  }

  private send(ws: WebSocket, message: unknown): void {
    if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(message));
  }

  private get manual(): boolean {
    return Boolean(this.setup.realtimeInputConfig?.automaticActivityDetection?.disabled);
  }

  private handle(ws: WebSocket, message: Record<string, any>): void {
    if (message.setup) {
      this.setup = message.setup;
      this.send(ws, { setupComplete: {} });
      return;
    }
    if (message.realtimeInput?.audio) {
      this.messages.push({ realtimeInput: { audio: { mimeType: message.realtimeInput.audio.mimeType } } });
      if (!this.manual) this.detect(ws, Buffer.from(message.realtimeInput.audio.data, 'base64'));
      return;
    }
    this.messages.push(message);
    if (message.realtimeInput?.activityEnd) this.turnDone(ws);
    if (message.toolResponse) {
      this.toolResponses.push(...message.toolResponse.functionResponses);
      void this.respond(ws);
    }
  }

  private detect(ws: WebSocket, data: Buffer): void {
    const loud = energy(data) > 0.02;
    if (loud && !this.speaking) {
      this.speaking = true;
      this.quiet = 0;
      if (this.responding) {
        this.stop = true;
        this.send(ws, { serverContent: { interrupted: true } });
      }
    } else if (this.speaking && !loud) {
      this.quiet += data.length / 2 / 16000;
      if (this.quiet > 0.3) {
        this.speaking = false;
        this.turnDone(ws);
      }
    } else if (this.speaking && loud) {
      this.quiet = 0;
    }
  }

  private turnDone(ws: WebSocket): void {
    this.send(ws, { serverContent: { inputTranscription: { text: 'what the user said', finished: true } } });
    void this.respond(ws);
  }

  private async respond(ws: WebSocket): Promise<void> {
    this.stop = false;
    if (this.toolCall && this.toolResponses.length === 0) {
      this.send(ws, { toolCall: { functionCalls: [this.toolCall] } });
      return;
    }
    this.responding = true;
    this.send(ws, { serverContent: { outputTranscription: { text: 'hello from the fixture model' } } });
    const chunk = 4800;
    for (let at = 0; at < this.voice.length; at += chunk) {
      if (this.stop) {
        this.responding = false;
        return;
      }
      this.send(ws, { serverContent: { modelTurn: { role: 'model', parts: [{ inlineData: { mimeType: 'audio/pcm;rate=24000', data: this.voice.subarray(at, at + chunk).toString('base64') } }] } } });
      await sleep(90);
    }
    this.responding = false;
    this.send(ws, { usageMetadata: { promptTokenCount: 30, responseTokenCount: 40 } });
    this.send(ws, { serverContent: { generationComplete: true, turnComplete: true } });
  }
}

/** Answers with a transcript once it has heard loud audio then quiet, in Deepgram's documented messages or AssemblyAI's. */
async function sttFixture(kind: 'deepgram' | 'assemblyai', transcript: string) {
  const server = new WebSocketServer({ port: 0, host: '127.0.0.1' });
  await new Promise((resolve) => server.once('listening', resolve));
  const seen = { headers: {} as Record<string, unknown>, path: '', controls: [] as string[] };
  server.on('connection', (ws, req) => {
    seen.headers = req.headers;
    seen.path = req.url ?? '';
    if (kind === 'assemblyai') ws.send(JSON.stringify({ type: 'Begin', id: 's', expires_at: 1 }));
    let spoke = false;
    let quiet = 0;
    ws.on('message', (raw: Buffer, isBinary: boolean) => {
      if (!isBinary) {
        seen.controls.push(JSON.parse(raw.toString()).type);
        return;
      }
      if (energy(raw) > 0.02) {
        spoke = true;
        quiet = 0;
      } else if (spoke) {
        quiet += raw.length;
        if (quiet > 16000) {
          spoke = false;
          if (kind === 'deepgram') ws.send(JSON.stringify({ type: 'Results', is_final: true, speech_final: true, channel: { alternatives: [{ transcript }] } }));
          else ws.send(JSON.stringify({ type: 'Turn', end_of_turn: true, transcript, utterance: transcript, words: [] }));
        }
      }
    });
  });
  return { url: `ws://127.0.0.1:${(server.address() as { port: number }).port}`, seen, close: () => { for (const c of server.clients) c.terminate(); server.close(); } };
}

/** One HTTP server that answers as an OpenAI-shaped LLM and as a speech provider. */
async function httpFixture(reply: string) {
  const pcm = pcmTone(1, 24000);
  const server = createServer((req, res) => {
    req.resume();
    req.on('end', () => {
      if (req.url?.includes('chat/completions')) {
        res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: reply } }] })}\n\n`);
        res.end('data: [DONE]\n\n');
      } else {
        res.end(pcm);
      }
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  return { url: `http://127.0.0.1:${(server.address() as { port: number }).port}`, close: () => { server.closeAllConnections(); server.close(); } };
}

for (const serverName of wantedServers) {
  describe(`streaming speech to text on ${serverName}, with fixtures for the vendors`, () => {
    let server: RunningServer;
    before(async () => {
      server = await startServer(serverName);
    });
    after(async () => {
      await server.stop();
    });

    async function runPipeline(stt: STT, llm: LLM, tts: TTS, options: { vad?: EnergyVAD; turnDetection?: 'vad' | 'stt' }): Promise<void> {
      let session: AgentSession | null = null;
      const name = `node-stt-${Math.random().toString(36).slice(2, 10)}`;
      const worker = new Worker({
        wsUrl: server.wsUrl,
        apiKey: API_KEY,
        apiSecret: API_SECRET,
        agentName: name,
        loadFn: () => 0,
        entrypoint: async (ctx: JobContext) => {
          await ctx.connect();
          const s = new AgentSession({ stt, llm, tts, vad: options.vad, turnDetection: options.turnDetection });
          ctx.addShutdownCallback(() => s.close());
          await s.start(new Agent({ instructions: 'Be brief.' }), { room: ctx.room });
          session = s;
        },
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('node-stt');
      const user = new TestUser(server, room);
      try {
        await user.join();
        await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: name, room }, adminToken(room));
        await eventually(() => session && [...user.room.remoteParticipants.values()].some((p) => p.isAgent), 'the agent to appear', 20000);
        const identity = [...user.room.remoteParticipants.values()].find((p) => p.isAgent)!.identity;
        await agentIsListening(() => session);
        await user.say(tone(0.8));
        await eventually(() => session!.turns.length > 0, 'the turn to finish', 20000);
        assert.deepEqual(session!.history.messages().map((m) => [m.role, m.content]), [
          ['user', 'hello from the user'],
          ['assistant', 'A reply from the model that is long enough.'],
        ]);
        await eventually(() => user.loudFrames(identity, 0.05) > 10, 'the reply to be heard');
        await eventually(() => user.transcripts.some((t) => t.identity === 'user'), "the user's transcript");
      } finally {
        await user.leave();
        await worker.close();
      }
    }

    it('Deepgram with its own turn detection', async () => {
      const stt = await sttFixture('deepgram', 'hello from the user');
      const http = await httpFixture('A reply from the model that is long enough.');
      try {
        await runPipeline(new DeepgramSTT('nova-3', { apiKey: KEY, baseUrl: stt.url }), new OpenAICompatibleLLM('m', { baseUrl: http.url, apiKey: KEY }), new CartesiaTTS('sonic-3.6', { voice: 'v', apiKey: KEY, baseUrl: http.url }), { turnDetection: 'stt' });
        assert.equal(stt.seen.headers.authorization, `Token ${KEY}`);
        for (const part of ['/v1/listen?', 'model=nova-3', 'encoding=linear16', 'sample_rate=16000', 'interim_results=true']) assert.ok(stt.seen.path.includes(part), stt.seen.path);
      } finally {
        stt.close();
        http.close();
      }
    });

    it('a streaming STT with the VAD deciding the turn', async () => {
      const stt = await sttFixture('deepgram', 'hello from the user');
      const http = await httpFixture('A reply from the model that is long enough.');
      try {
        await runPipeline(new DeepgramSTT('nova-3', { apiKey: KEY, baseUrl: stt.url }), new OpenAICompatibleLLM('m', { baseUrl: http.url, apiKey: KEY }), new CartesiaTTS('sonic-3.6', { voice: 'v', apiKey: KEY, baseUrl: http.url }), { vad: new EnergyVAD({ minSilence: 0.15 }), turnDetection: 'vad' });
      } finally {
        stt.close();
        http.close();
      }
    });

    it('AssemblyAI', async () => {
      const stt = await sttFixture('assemblyai', 'hello from the user');
      const http = await httpFixture('A reply from the model that is long enough.');
      try {
        await runPipeline(new AssemblyAISTT('universal-3-6-pro', { apiKey: KEY, baseUrl: stt.url }), new OpenAICompatibleLLM('m', { baseUrl: http.url, apiKey: KEY }), new CartesiaTTS('sonic-3.6', { voice: 'v', apiKey: KEY, baseUrl: http.url }), { turnDetection: 'stt' });
        assert.equal(stt.seen.headers.authorization, KEY, 'this API takes the key with no Bearer prefix');
        for (const part of ['/v3/ws?', 'speech_model=universal-3-6-pro', 'sample_rate=16000', 'encoding=pcm_s16le']) assert.ok(stt.seen.path.includes(part), stt.seen.path);
      } finally {
        stt.close();
        http.close();
      }
    });
  });

  describe(`realtime on ${serverName}, with a fixture for the model`, () => {
    let server: RunningServer;
    before(async () => {
      server = await startServer(serverName);
    });
    after(async () => {
      await server.stop();
    });

    async function withSession(model: RealtimeModel, options: { duplex: boolean; agent?: Agent; vad?: EnergyVAD; minEndpointingDelay?: number; minInterruptionDuration?: number }, run: (h: { session: AgentSession; user: TestUser; identity: string }) => Promise<void>) {
      let session: AgentSession | null = null;
      const name = `node-rt-${Math.random().toString(36).slice(2, 10)}`;
      const worker = new Worker({
        wsUrl: server.wsUrl,
        apiKey: API_KEY,
        apiSecret: API_SECRET,
        agentName: name,
        loadFn: () => 0,
        entrypoint: async (ctx: JobContext) => {
          await ctx.connect();
          const s = new AgentSession({ realtime: model, duplex: options.duplex, vad: options.vad, minEndpointingDelay: options.minEndpointingDelay, minInterruptionDuration: options.minInterruptionDuration });
          ctx.addShutdownCallback(() => s.close());
          await s.start(options.agent ?? new Agent({ instructions: 'You are a test agent.' }), { room: ctx.room });
          session = s;
        },
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('node-rt');
      const user = new TestUser(server, room);
      try {
        await user.join();
        const { status } = await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: name, room }, adminToken(room));
        assert.equal(status, 200);
        await eventually(() => session && [...user.room.remoteParticipants.values()].some((p) => p.isAgent), 'the agent to appear', 20000);
        const identity = [...user.room.remoteParticipants.values()].find((p) => p.isAgent)!.identity;
        await agentIsListening(() => session);
        await run({ session: session!, user, identity });
      } finally {
        await user.leave();
        await worker.close();
      }
    }

    it('OpenAI full duplex: the reply reaches the user', async () => {
      const fixture = new OpenAIFixture({ replySeconds: 1.5 });
      await fixture.start();
      try {
        await withSession(new OpenAIRealtimeModel('gpt-realtime-2.1', { apiKey: KEY, baseUrl: fixture.url }), { duplex: true }, async ({ session, user, identity }) => {
          await user.say(tone(0.6));
          await eventually(() => user.loudFrames(identity, 0.05) > 25, 'the voice to reach the user');
          await eventually(() => session.turns.length > 0, 'the turn to finish');
          assert.equal(fixture.headers.authorization, `Bearer ${KEY}`);
          assert.equal(fixture.session.type, 'realtime');
          assert.deepEqual(fixture.session.audio.input.turn_detection, { type: 'semantic_vad' });
          assert.deepEqual(fixture.session.audio.input.format, { type: 'audio/pcm', rate: 24000 });
          const messages = session.history.messages().map((m) => [m.role, m.content]);
          assert.deepEqual(messages, [
            ['user', 'what the user said'],
            ['assistant', 'hello from the fixture model'],
          ]);
          const usage = session.turns[0].usage.find((u) => u.kind === 'realtime')!.usage;
          assert.deepEqual([usage.promptTokens, usage.completionTokens, usage.cachedTokens], [100, 50, 5]);
        });
      } finally {
        fixture.close();
      }
    });

    it('OpenAI full duplex: an interruption truncates at what played', async () => {
      const fixture = new OpenAIFixture({ replySeconds: 10, transcript: Array.from({ length: 40 }, (_, i) => `word${i}`).join(' ') });
      await fixture.start();
      try {
        await withSession(new OpenAIRealtimeModel('gpt-realtime-2.1', { apiKey: KEY, baseUrl: fixture.url }), { duplex: true }, async ({ session, user }) => {
          await user.say(tone(0.6));
          await eventually(() => session.state === 'speaking', 'the model to speak');
          await sleep(1500);
          const before = session.output.playedSeconds;
          await user.say(tone(0.5));
          await eventually(() => fixture.truncated.length > 0, 'the truncate event');
          const t = fixture.truncated[0];
          // Rounded down to a millisecond, and on loopback the user can be heard within one 20 ms frame.
          assert.ok(t.audio_end_ms >= Math.floor(before * 1000) && t.audio_end_ms <= (before + 1.5) * 1000, JSON.stringify({ ...t, before }));
          assert.ok(fixture.events.some((e) => e.type === 'response.cancel'));
          assert.equal(session.output.queuedSeconds, 0);
          await eventually(() => session.history.messages().some((m) => m.interrupted), 'the cut reply to be stored');
          const cut = session.history.messages().find((m) => m.interrupted)!;
          assert.ok(cut.content.split(' ').length < 30, cut.content);
        });
      } finally {
        fixture.close();
      }
    });

    it('OpenAI turn based: the local VAD commits the turn', async () => {
      const fixture = new OpenAIFixture({ replySeconds: 1 });
      await fixture.start();
      try {
        await withSession(new OpenAIRealtimeModel('gpt-realtime-2.1', { apiKey: KEY, baseUrl: fixture.url }), { duplex: false, vad: new EnergyVAD({ minSilence: 0.2 }), minEndpointingDelay: 0.3 }, async ({ session, user, identity }) => {
          assert.equal(fixture.session.audio.input.turn_detection, null);
          await user.say(tone(0.6));
          await eventually(() => user.loudFrames(identity, 0.05) > 15, 'the reply');
          const names = fixture.events.map((e) => e.type);
          assert.ok(names.indexOf('input_audio_buffer.commit') < names.indexOf('response.create'));
          await eventually(() => session.turns.length > 0, 'the turn record');
          assert.ok(session.turns[0].latency.endOfTurn !== null);
        });
      } finally {
        fixture.close();
      }
    });

    it('OpenAI: a tool call, and the framework asks for the reply', async () => {
      const seen: string[] = [];
      const lookup = functionTool({
        name: 'get_weather',
        description: 'Gets the weather.',
        parameters: { type: 'object', properties: { city: { type: 'string' } }, required: ['city'] },
        execute: ({ city }) => {
          seen.push(String(city));
          return 'sunny';
        },
      });
      const fixture = new OpenAIFixture({ replySeconds: 0.5, toolCall: { call_id: 'call_1', name: 'get_weather', arguments: '{"city":"Paris"}' } });
      await fixture.start();
      try {
        await withSession(new OpenAIRealtimeModel('gpt-realtime-2.1', { apiKey: KEY, baseUrl: fixture.url }), { duplex: true, agent: new Agent({ instructions: 'Use tools.', tools: [lookup] }) }, async ({ user, identity }) => {
          await user.say(tone(0.6));
          await eventually(() => seen[0] === 'Paris', 'the tool to run');
          await eventually(() => fixture.toolOutputs.length > 0, 'the tool result');
          assert.deepEqual(fixture.toolOutputs[0], { type: 'function_call_output', call_id: 'call_1', output: 'sunny' });
          await eventually(() => fixture.responses >= 2, 'a second response');
          await eventually(() => user.loudFrames(identity, 0.05) > 5, 'the reply after the tool');
        });
      } finally {
        fixture.close();
      }
    });

    it('Gemini full duplex, and its interruption', async () => {
      const fixture = new GeminiFixture({ replySeconds: 8 });
      await fixture.start();
      try {
        await withSession(new GeminiLiveModel('gemini-3.8-live', { apiKey: KEY, baseUrl: fixture.url }), { duplex: true }, async ({ session, user }) => {
          await user.say(tone(0.6));
          await eventually(() => session.state === 'speaking', 'the model to speak');
          assert.ok(fixture.path.startsWith('/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?key='));
          assert.equal(fixture.setup.model, 'models/gemini-3.8-live');
          assert.deepEqual(fixture.setup.generationConfig.responseModalities, ['AUDIO']);
          assert.equal(fixture.messages[0].realtimeInput.audio.mimeType, 'audio/pcm;rate=16000');
          await sleep(1000);
          await user.say(tone(0.4));
          await eventually(() => session.turns.some((t) => t.interrupted), 'the reply to be cut off');
          assert.equal(session.output.queuedSeconds, 0);
        });
      } finally {
        fixture.close();
      }
    });

    it('Gemini turn based, and a tool it replies to by itself', async () => {
      const seen: string[] = [];
      const lookup = functionTool({
        name: 'lookup',
        description: 'Looks something up.',
        parameters: { type: 'object', properties: { term: { type: 'string' } }, required: ['term'] },
        execute: ({ term }) => {
          seen.push(String(term));
          return 'found it';
        },
      });
      const fixture = new GeminiFixture({ replySeconds: 0.5, toolCall: { id: 'c1', name: 'lookup', args: { term: 'x' } } });
      await fixture.start();
      try {
        await withSession(new GeminiLiveModel('gemini-3.8-live', { apiKey: KEY, baseUrl: fixture.url }), { duplex: false, vad: new EnergyVAD({ minSilence: 0.2 }), minEndpointingDelay: 0.3, agent: new Agent({ instructions: 'Use tools.', tools: [lookup] }) }, async ({ user, identity }) => {
          assert.equal(fixture.setup.realtimeInputConfig.automaticActivityDetection.disabled, true);
          await user.say(tone(0.6));
          await eventually(() => seen[0] === 'x', 'the tool to run');
          await eventually(() => fixture.toolResponses.length > 0, 'the tool result');
          assert.deepEqual(fixture.toolResponses[0], { id: 'c1', name: 'lookup', response: { result: 'found it' } });
          const shapes = fixture.messages.filter((m) => m.realtimeInput).map((m) => Object.keys(m.realtimeInput)[0]);
          assert.equal(shapes[0], 'activityStart');
          assert.ok(shapes.includes('activityEnd'));
          await eventually(() => user.loudFrames(identity, 0.05) > 5, 'the reply after the tool');
        });
      } finally {
        fixture.close();
      }
    });
  });
}

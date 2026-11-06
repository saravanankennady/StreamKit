// The Node.js framework over real WebRTC, on both servers. The last tests use real speech, through local engines.
import assert from 'node:assert/strict';
import { after, before, describe, it } from 'node:test';
import { Agent, AgentSession, AudioFrame, EnergyVAD, functionTool, JobContext, OpenAICompatibleLLM, OpenAICompatibleSTT, OpenAICompatibleTTS, SileroVAD, Worker } from '../src/index.ts';
import { JobType } from '../src/proto/livekit_agent_pb.js';
import { adminToken, agentIsListening, API_KEY, API_SECRET, enginesPresent, eventually, type Engines, type RunningServer, sleep, startEngines, startServer, TestUser, tone, twirp, uniqueRoom, wantedServers } from './harness.ts';

const agentName = () => `node-agent-${Math.random().toString(36).slice(2, 10)}`;

function makeWorker(server: RunningServer, entrypoint: (ctx: JobContext) => Promise<void>, name = agentName(), jobType?: JobType) {
  return new Worker({ entrypoint, wsUrl: server.wsUrl, apiKey: API_KEY, apiSecret: API_SECRET, agentName: name, loadFn: () => 0, jobType });
}

for (const serverName of wantedServers) {
  describe(`agents on ${serverName}`, () => {
    let server: RunningServer;
    before(async () => {
      server = await startServer(serverName);
    });
    after(async () => {
      await server.stop();
    });

    it('a worker gets an explicit dispatch and joins as an agent', async () => {
      let joined: JobContext | null = null;
      const worker = makeWorker(server, async (ctx) => {
        await ctx.connect();
        joined = ctx;
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('dispatch');
      const user = new TestUser(server, room);
      try {
        await user.join(false);
        const { status, body } = await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: worker.agentName, room, metadata: 'hello' }, adminToken(room));
        assert.equal(status, 200, JSON.stringify(body));
        await eventually(() => joined, 'the job to start');
        assert.equal(joined!.metadata, 'hello');
        await eventually(() => user.room.remoteParticipants.size === 1, 'the agent to appear');
        const participant = [...user.room.remoteParticipants.values()][0];
        assert.ok(participant.isAgent);
        assert.equal(participant.attributes['lk.agent_name'], worker.agentName);
        assert.equal(participant.attributes['lk.agent.name'], worker.agentName);

        const deleted = await twirp(server, 'AgentDispatchService', 'DeleteDispatch', { dispatch_id: body.id, room }, adminToken(room));
        assert.equal(deleted.status, 200);
        await eventually(() => worker.jobs.size === 0, 'the job to end');
      } finally {
        await user.leave();
        await worker.close();
      }
    });

    it('a declined job goes to the next worker', async () => {
      const name = agentName();
      let ran = 0;
      let declined = 0;
      const entrypoint = async (ctx: JobContext) => {
        await ctx.connect();
        ran += 1;
      };
      const decliner = new Worker({
        entrypoint,
        wsUrl: server.wsUrl,
        apiKey: API_KEY,
        apiSecret: API_SECRET,
        agentName: name,
        loadFn: () => 0,
        requestFn: (request) => {
          declined += 1;
          request.reject();
        },
      });
      const taker = makeWorker(server, entrypoint, name);
      decliner.start();
      taker.start();
      await decliner.waitRegistered();
      await taker.waitRegistered();
      try {
        for (let i = 0; i < 6; i++) {
          const room = uniqueRoom('decline');
          await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: name, room }, adminToken(room));
        }
        await eventually(() => ran === 6, 'all six jobs to run', 30000);
        assert.ok(declined > 0);
      } finally {
        await decliner.close();
        await taker.close();
      }
    });

    it('agents do not use up the room limit or keep the room open', async () => {
      const name = agentName();
      let joined = false;
      const worker = makeWorker(server, async (ctx) => {
        await ctx.connect();
        joined = true;
      }, name);
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('dependent');
      const admin = adminToken(room);
      const user = new TestUser(server, room);
      try {
        const created = await twirp(server, 'RoomService', 'CreateRoom', { name: room, max_participants: 1, empty_timeout: 60, departure_timeout: 1, agents: [{ agent_name: name }] }, admin);
        assert.equal(created.status, 200, JSON.stringify(created.body));
        await eventually(() => joined, 'the agent to join');
        await user.join(false);
        await user.leave();
        const deadline = Date.now() + 30000;
        while (true) {
          const { body } = await twirp(server, 'RoomService', 'ListRooms', { names: [room] }, admin);
          if (!body.rooms?.length) break;
          assert.ok(Date.now() < deadline, 'the agent kept the room open');
          await sleep(250);
        }
        await eventually(() => worker.jobs.size === 0, 'the job to end with the room');
      } finally {
        await worker.close();
      }
    });

    it('a publisher job starts once for each publisher', async () => {
      const jobs: JobContext[] = [];
      const name = agentName();
      const worker = makeWorker(server, async (ctx) => {
        jobs.push(ctx);
        await ctx.connect();
      }, name, JobType.JT_PUBLISHER);
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('publisher');
      const user = new TestUser(server, room);
      try {
        await twirp(server, 'RoomService', 'CreateRoom', { name: room, empty_timeout: 60, agents: [{ agent_name: name }] }, adminToken(room));
        await user.join();
        const speaking = user.say(tone(3));
        await eventually(() => jobs.length > 0, 'the publisher job');
        assert.equal(jobs[0].job.type, JobType.JT_PUBLISHER);
        assert.equal(jobs[0].job.participant?.identity, 'user');
        await speaking;
        assert.equal(jobs.length, 1);
      } finally {
        await user.leave();
        await worker.close();
      }
    });

    it('RPC between two participants', async () => {
      const room = uniqueRoom('rpc');
      const a = new TestUser(server, room, 'caller');
      const b = new TestUser(server, room, 'callee');
      try {
        await a.join(false);
        await b.join(false);
        b.room.registerRpcMethod('double', (caller, payload) => {
          assert.equal(caller, 'caller');
          return payload + payload;
        });
        await eventually(() => a.room.remoteParticipants.has('callee'), 'the callee');
        assert.equal(await a.room.performRpc('callee', 'double', 'ab'), 'abab');
        await assert.rejects(a.room.performRpc('callee', 'missing', ''), /not supported/);
      } finally {
        await a.leave();
        await b.leave();
      }
    });

    it('audio goes both ways through a job', async () => {
      const heardByAgent: number[] = [];
      let replied = false;
      let bothWays: AgentSession | null = null;
      const worker = makeWorker(server, async (ctx) => {
        await ctx.connect();
        const session = new AgentSession({
          vad: Object.assign(new EnergyVAD({ minSilence: 0.2 }), {}),
          tts: new (class extends OpenAICompatibleTTS {
            async *synthesize(): AsyncGenerator<AudioFrame> {
              yield tone(1);
            }
          })('tone', { baseUrl: 'http://unused.invalid' }),
          stt: new (class extends OpenAICompatibleSTT {
            async recognize(frame: AudioFrame) {
              heardByAgent.push(frame.duration);
              return { text: 'something' };
            }
          })('none', { baseUrl: 'http://unused.invalid' }),
          llm: new (class extends OpenAICompatibleLLM {
            protected async *stream() {
              replied = true;
              yield { text: 'A sentence for the speech engine to say.' };
            }
          })('none', { baseUrl: 'http://unused.invalid' }),
        });
        ctx.addShutdownCallback(() => session.close());
        await session.start(new Agent({ instructions: 'test' }), { room: ctx.room });
        bothWays = session;
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('both-ways');
      const user = new TestUser(server, room);
      try {
        await user.join();
        await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: worker.agentName, room }, adminToken(room));
        await eventually(() => [...user.room.remoteParticipants.values()].some((p) => p.isAgent), 'the agent to appear');
        const identity = [...user.room.remoteParticipants.values()].find((p) => p.isAgent)!.identity;
        await agentIsListening(() => bothWays);
        await user.say(tone(0.8));
        await eventually(() => heardByAgent.length > 0, 'the agent to hear an utterance');
        assert.ok(heardByAgent[0] > 0.5, `the utterance was ${heardByAgent[0]} s`);
        await eventually(() => replied, 'the reply to start');
        await eventually(() => user.loudFrames(identity, 0.05) > 30, 'the agent to be heard');
      } finally {
        await user.leave();
        await worker.close();
      }
    });
  });
}

describe('real speech through local engines', { skip: !enginesPresent() }, () => {
  let engines: Engines;
  const servers: Record<string, RunningServer> = {};
  before(async () => {
    engines = await startEngines();
    for (const name of wantedServers) servers[name] = await startServer(name);
  });
  after(async () => {
    await engines.stop();
    for (const server of Object.values(servers)) await server.stop();
  });

  async function speak(text: string): Promise<AudioFrame> {
    const response = await fetch(`${engines.ttsUrl}/audio/speech`, { method: 'POST', body: JSON.stringify({ input: text, model: engines.ttsModel, voice: 'default' }) });
    return AudioFrame.fromBuffer(Buffer.from(await response.arrayBuffer()), engines.ttsRate);
  }

  const stack = async () => ({
    stt: new OpenAICompatibleSTT(engines.sttModel, { baseUrl: engines.sttUrl, provider: 'local' }),
    llm: new OpenAICompatibleLLM(engines.llmModel, { baseUrl: engines.llmUrl, provider: 'local', extraBody: { temperature: 0, seed: 7 } }),
    tts: new OpenAICompatibleTTS(engines.ttsModel, { baseUrl: engines.ttsUrl, provider: 'local', sampleRate: engines.ttsRate }),
    vad: await SileroVAD.create({ modelPath: `${process.env.STREAMKIT_DEV_ROOT ?? '/run/media/nsroot/develop/streamin_engine'}/.toolchain/models/silero/silero_vad.onnx` }),
  });

  for (const serverName of wantedServers) {
    it(`real question, real answer, real tool on ${serverName}`, async () => {
      const server = servers[serverName];
      let session: AgentSession | null = null;
      const toolCalls: string[] = [];
      const getWeather = functionTool({
        name: 'get_weather',
        description: 'Gets the current weather in a city.',
        parameters: { type: 'object', properties: { city: { type: 'string', description: 'The name of the city.' } }, required: ['city'] },
        execute: ({ city }) => {
          toolCalls.push(String(city));
          return 'It is sunny and 21 degrees.';
        },
      });
      const worker = makeWorker(server, async (ctx) => {
        await ctx.connect();
        session = new AgentSession(await stack());
        ctx.addShutdownCallback(() => session!.close());
        await session.start(
          new Agent({
            instructions: 'You are a voice assistant. You must use the get_weather tool for any question about the weather. Answer in one short sentence, and say the temperature.',
            tools: [getWeather],
          }),
          { room: ctx.room },
        );
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('real');
      const user = new TestUser(server, room);
      try {
        await user.join();
        await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: worker.agentName, room }, adminToken(room));
        await eventually(() => session && [...user.room.remoteParticipants.values()].some((p) => p.isAgent), 'the agent to appear', 20000);
        const identity = [...user.room.remoteParticipants.values()].find((p) => p.isAgent)!.identity;
        await agentIsListening(() => session);
        await user.say(await speak('What is the weather in Paris?'));
        await eventually(() => session!.turns.length > 0, 'the turn to finish', 60000);

        assert.deepEqual(toolCalls.map((c) => c.toLowerCase()), ['paris']);
        const messages = session!.history.messages();
        assert.equal(messages[0].role, 'user');
        assert.match(messages[0].content.toLowerCase(), /weather in paris/);
        const reply = messages.at(-1)!;
        assert.equal(reply.role, 'assistant');
        assert.match(reply.content, /21|twenty/i);

        // What the user received, decoded from RTP, is the same words: Whisper can read it back.
        const heard = user.audioFrom(identity);
        assert.ok(heard, 'no audio reached the user');
        const check = new OpenAICompatibleSTT(engines.sttModel, { baseUrl: engines.sttUrl });
        const spoken = await check.recognize(heard);
        assert.match(spoken.text, /21|twenty/i, spoken.text);

        const turn = session!.turns[0];
        for (const [name, value] of Object.entries(turn.latency)) {
          assert.ok(value !== null && value >= 0 && value < 60, `${name} = ${value}`);
        }
        console.log(`node cascaded on ${serverName}:`, turn.latency);
        assert.ok(turn.usage.some((u) => u.kind === 'llm' && u.usage.promptTokens > 0));
      } finally {
        await user.leave();
        await worker.close();
      }
    });

    it(`a handoff to another agent, with a real LLM, on ${serverName}`, async () => {
      const server = servers[serverName];
      let session: AgentSession | null = null;
      class Billing extends Agent {
        async onEnter() {
          this.session!.say('Hello, this is the billing department.');
        }
      }
      const transfer = functionTool({
        name: 'transfer_to_billing',
        description: 'Transfers the caller to the billing department. Call this whenever the caller mentions a bill.',
        parameters: { type: 'object', properties: {} },
        execute: () => new Billing({ instructions: 'You are the billing agent.', name: 'billing' }),
      });
      const worker = makeWorker(server, async (ctx) => {
        await ctx.connect();
        session = new AgentSession(await stack());
        ctx.addShutdownCallback(() => session!.close());
        await session.start(new Agent({ instructions: 'You are a receptionist. If the caller mentions a bill, you must call the transfer_to_billing tool at once, and say nothing else.', tools: [transfer], name: 'reception' }), { room: ctx.room });
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('handoff');
      const user = new TestUser(server, room);
      try {
        await user.join();
        await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: worker.agentName, room }, adminToken(room));
        await eventually(() => session && [...user.room.remoteParticipants.values()].some((p) => p.isAgent), 'the agent to appear', 20000);
        const agents = [...user.room.remoteParticipants.values()].filter((p) => p.isAgent);
        await agentIsListening(() => session);
        await user.say(await speak('I have a question about my bill.'));
        await eventually(() => session!.agent?.name === 'billing', 'the handoff', 30000);
        await eventually(() => session!.history.messages().some((m) => /billing department/.test(m.content)), 'the new agent to speak', 20000);
        assert.ok(session!.history.items.some((i) => i.type === 'agent_handoff'));
        // The same participant carried on: there is one agent in the room.
        assert.deepEqual([...user.room.remoteParticipants.values()].filter((p) => p.isAgent).map((p) => p.identity), agents.map((p) => p.identity));
        const check = new OpenAICompatibleSTT(engines.sttModel, { baseUrl: engines.sttUrl });
        assert.match((await check.recognize(user.audioFrom(agents[0].identity)!)).text, /billing/i);
      } finally {
        await user.leave();
        await worker.close();
      }
    });

    it(`the user interrupts with real speech on ${serverName}`, async () => {
      const server = servers[serverName];
      let session: AgentSession | null = null;
      const story =
        'Once upon a time there was a small town by the sea. Every morning the fishing boats went out before dawn. ' +
        'The children would run down to the harbour to watch them go. In the evening the whole town came to see what they had caught. ' +
        'There was a lighthouse at the end of the long stone pier, and its keeper knew every boat by name.';
      class Teller extends Agent {
        async onEnter() {
          this.session!.say(story);
        }
      }
      const worker = makeWorker(server, async (ctx) => {
        await ctx.connect();
        session = new AgentSession(await stack());
        ctx.addShutdownCallback(() => session!.close());
        await session.start(new Teller({ instructions: 'You are a voice assistant. Answer in one short sentence.' }), { room: ctx.room });
      });
      worker.start();
      await worker.waitRegistered();
      const room = uniqueRoom('interrupt');
      const user = new TestUser(server, room);
      try {
        await user.join();
        await twirp(server, 'AgentDispatchService', 'CreateDispatch', { agent_name: worker.agentName, room }, adminToken(room));
        await eventually(() => session && session.state === 'speaking', 'the story to start', 20000);
        await sleep(3000);
        await user.say(await speak('Stop please. What is the capital of Italy?'));
        await eventually(() => session!.history.messages().some((m) => m.interrupted), 'the story to be cut off', 20000);
        const cut = session!.history.messages().find((m) => m.interrupted)!;
        assert.equal(cut.role, 'assistant');
        assert.ok(story.startsWith(cut.content.slice(0, 20)));
        assert.ok(cut.content.split(' ').length < story.split(' ').length * 0.7, cut.content);
        await eventually(() => session!.history.messages().some((m) => m.role === 'assistant' && /rome/i.test(m.content)), 'the new answer', 30000);
      } finally {
        await user.leave();
        await worker.close();
      }
    });
  }
});

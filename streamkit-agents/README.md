# streamkit-agents

Voice AI agents for Elnova StreamKit, in Python and Node.js. An agent is a participant in a room. It hears users, thinks, and speaks.

- Architecture and decisions: [docs/architecture/agents.md](../docs/architecture/agents.md)
- Which providers are tested how far: [docs/VOICE_AI_PROVIDER_MATRIX.md](../docs/VOICE_AI_PROVIDER_MATRIX.md)

## Python

```python
from streamkit_agents import Agent, AgentSession, JobContext, ModelSelector, RunContext, SileroVAD, Worker, function_tool

@function_tool
async def get_weather(ctx: RunContext, city: str) -> str:
    """Gets the current weather in a city.

    Args:
        city: The name of the city.
    """
    return "It is sunny and 21 degrees."

async def entrypoint(ctx: JobContext) -> None:
    await ctx.connect()
    models = ModelSelector()
    session = AgentSession(
        stt=await models.stt("deepgram/nova-3"),
        llm=await models.llm("anthropic/claude-haiku-5-5"),
        tts=await models.tts("cartesia/sonic-3.6:<voice id>"),
        vad=SileroVAD(),
    )
    ctx.add_shutdown_callback(session.aclose)
    # A session ends when its user leaves. This ends the job with it, rather than waiting for the room to close.
    session.on("close", lambda: ctx.shutdown("the session ended"))
    await session.start(Agent(instructions="You are a helpful voice assistant.", tools=[get_weather]), room=ctx.room)

worker = Worker(entrypoint, ws_url="wss://...", api_key="...", api_secret="...", agent_name="helper")
```

A realtime model replaces the VAD, STT, LLM and TTS:

```python
session = AgentSession(realtime=await models.realtime("openai/gpt-realtime-2.1"), duplex=True)
```

With `duplex=False` and a `vad`, the framework's own VAD decides where turns end, which is turn based realtime.

`examples/local_voice_agent.py` runs on local engines with no keys. `local-engines/` has how to run those.

### Where things are

| Path | What it holds |
| --- | --- |
| `python/src/streamkit_agents/worker.py` | The worker: `/agent`, jobs, load |
| `python/src/streamkit_agents/rtc/` | Our room client, on aiortc |
| `python/src/streamkit_agents/voice/` | The session and the three pipelines |
| `python/src/streamkit_agents/providers/` | Adapters |
| `python/src/streamkit_agents/models.py` | Choosing models by name |
| `python/src/streamkit_agents/credentials.py` | Where keys come from |
| `python/src/streamkit_agents/observability.py` | Turn metrics, usage and cost |
| `python/src/streamkit_agents/avatar.py` | The avatar interface. No vendors yet |

### Tests

```sh
cd python && python -m pytest                   # both servers, and the local engines if installed
STREAMKIT_TEST_SERVERS=ours python -m pytest    # one server
```

Nothing in the media path is simulated, with one labelled exception: `test_realtime_fixture.py` runs real WebRTC against a fixture in
place of the realtime model, because no local engine does that job. `test_providers_fixture.py` replays each provider's documented
wire format, which proves the protocol code only.

## Node.js

The same API shape, in TypeScript, run directly by Node's type stripping. It needs Node 23.6 or later. Tools take a written-out JSON
schema, because Node has no type hints to read at run time.

```ts
import { Agent, AgentSession, ModelSelector, SileroVAD, Worker } from '@elnova-streamkit/agents';

const models = new ModelSelector();
const worker = new Worker({
  agentName: 'helper',
  entrypoint: async (ctx) => {
    await ctx.connect();
    const session = new AgentSession({
      stt: await models.stt('openai/gpt-transcribe'),
      llm: await models.llm('anthropic/claude-haiku-5-5'),
      tts: await models.tts('openai/gpt-4o-mini-tts:marin'),
      vad: await SileroVAD.create(),
    });
    ctx.addShutdownCallback(() => session.close());
    await session.start(new Agent({ instructions: 'You are a helpful voice assistant.' }), { room: ctx.room });
  },
});
worker.start();
```

## What each language has

| | Python | Node.js |
| --- | --- | --- |
| Worker, `/agent`, job types, reconnect | Yes | Yes |
| Room client: audio both ways, data, text streams | Yes, on aiortc | Yes, on werift |
| RPC | v1 | v1 |
| Cascaded pipeline, interruptions, tools, handoffs | Yes | Yes |
| Turn based and full duplex realtime | Yes, fixture tested | Yes, fixture tested |
| VAD | Silero and energy | Silero and energy |
| LLM protocols: OpenAI-compatible, Anthropic, Google | Yes | Yes |
| OpenAI-shaped STT and TTS | Yes | Yes |
| Cartesia, ElevenLabs, Deepgram speech | Yes | Yes |
| Streaming STT: Deepgram, AssemblyAI | Yes | Yes |
| Choosing models by name, credentials from the control plane | Yes | Yes |
| Metrics, sinks and cost | Yes | Yes, reported when the session closes |

Each language runs its jobs in the worker's own process. Running each job in a process of its own is not implemented in either.

## Licenses

See [local-engines/README.md](local-engines/README.md) for the engines used to test. The packages themselves depend on:

| Dependency | License | Used for |
| --- | --- | --- |
| aiortc, aioice, pylibsrtp | BSD-3-Clause | Python WebRTC |
| PyAV | BSD-3-Clause, wrapping FFmpeg (LGPL in its wheels) | Python resampling |
| websockets, httpx, protobuf, numpy | BSD-3-Clause | Python |
| onnxruntime (optional) | MIT | Silero VAD |
| werift | MIT | Node WebRTC |
| @evan/opus | MIT, wrapping libopus (BSD-3-Clause) | Node Opus. Its package carries native builds and a WebAssembly fallback. opusscript, tried first, broke once WebAssembly memory grew, so it fails with several streams in one process |
| ws, @bufbuild/protobuf | MIT, Apache-2.0 | Node |

Have someone check this list before distributing. The wire protocol is LiveKit's Apache-2.0 definitions, vendored in `proto/vendor/`.
No code was copied from `reference/agents`. Attribute and topic names such as `lk.agent.state` are part of the wire format.

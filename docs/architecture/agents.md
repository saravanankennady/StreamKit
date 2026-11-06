# Voice AI agents

An agent is an ordinary room participant that runs somewhere else. The server only brokers jobs: a worker holds a WebSocket open on
`/agent`, the server offers it a job when a room needs an agent, and an accepted job gets a token that joins the room. From then on
the agent talks to the room like any client. Everything about voice and models is in the framework, `streamkit-agents/`.

Which providers are tested how far is in [VOICE_AI_PROVIDER_MATRIX.md](../VOICE_AI_PROVIDER_MATRIX.md).

```text
 browser ── mic ──► SFU ──► agent's room client ── PCM ──► VAD ─┬─► STT ──► end of turn ──► LLM ──► tools
    ▲                                                           │                            │
    │                                                           └── pause, interrupt         ▼
    └── speaker ◄── SFU ◄── Opus ◄── AudioSource ◄── PCM ◄───────────────────────────────── TTS
```

## Server

`server/agent` holds the broker: worker registration, offers, load-weighted selection, job tokens and the job lifecycle. It follows the
reference's behaviour, checked by running the same tests on both servers, including with the official Python worker.

- **Dispatch** is automatic (a room with no configured agents gets one dispatch, for unnamed workers), from a token's `roomConfig` or
  `CreateRoom`'s `agents`, or through the Twirp `AgentDispatchService`.
- **Agents are dependent participants.** With recorders, they do not count toward `max_participants` or `num_participants`, do not
  keep a room open, and do not attract agents of their own.
- **`TrackSubscribed` fires when a subscriber's downtrack binds**, not on the first forwarded packet, once per track, and not for
  hidden subscribers. Agents hold their audio until they hear it.
- **An agent's text stream or chat message can carry a user's identity**, marked `generated`, because that is how a user's transcript
  reaches clients as theirs. Anyone else's identity is overwritten as before. `Transcription` packets are forwarded only from agents.

### Where our server differs from the reference, on purpose

| Behaviour | Reference | Ours | Why |
| --- | --- | --- | --- |
| A dispatch naming an agent, with no worker registered | The job is lost | It waits, as `JS_PENDING` with `JSR_NO_CAPACITY`, and starts when a worker registers | Someone asked for that agent in that room. A worker being restarted should not silently lose it |
| The automatic dispatch, with no worker | Lost | Lost | Otherwise starting an unnamed worker would put an agent into every room already running |
| `migrate_job` | No-op | The reconnected worker takes its jobs back | Otherwise those jobs can no longer be terminated |
| `ListDispatch` after a job ends | Keeps showing `JS_RUNNING` | Shows the job's real state | A listing should say what happened |
| Chunk and trailer identity on a stream sent for someone else | Overwritten with the agent's | Same as the stream's header | A stream should say one thing about who it is from. The official client was not seen to mind either way |
| Token `roomPreset` | Looks up a preset from config | Ignored | We have no presets |

### Limits

- A broker sees only its own node. On a cluster, a worker must connect to each node.
- The WebTransport data plane for agent HTTP endpoints, `/agents/...`, is not implemented.
- `PerformRpc`, server to agent, still returns `unimplemented`. Agents and clients can already call each other.
- `JT_PARTICIPANT` jobs exist on the server, though neither framework has a use for them yet.

## Framework

Python and Node.js have the same API shape. A table of what each has is in [streamkit-agents/README.md](../../streamkit-agents/README.md).

### Why our own room client

Python's room client is ours, on [aiortc](https://github.com/aiortc/aiortc), and Node's is on
[werift](https://github.com/shinyoshiaki/werift-webrtc). The obvious alternative, the official rtc SDK, is a closed binary from our
point of view, and the licensing rule is that the official client packages are test-only. So they stay as test oracles, where they
have already earned their keep: `test_official_worker_oracle.py` runs the official worker and SDK against our server.

What this costs: neither library has libwebrtc's audio processing, and neither has been run under packet loss in these tests. Audio
is Opus with NACK where the library does it. For a telephone-quality voice this has been fine on a clean network. **It has not been
measured on a bad one.** ICE here is UDP only, so a network that blocks UDP needs a TURN server.

### Three pipelines

| Pipeline | Who finds turns | How a reply starts | How an interruption works |
| --- | --- | --- | --- |
| Cascaded | Local VAD, or the STT if it detects them | LLM streaming into a TTS, a sentence at a time | Pause on speech, then stop for good on a transcript, or resume if none comes |
| Turn based realtime | Local VAD | Commit the turn's audio and ask for a reply | Stop on speech, and tell the provider how much was heard |
| Full duplex realtime | The provider | The provider decides | The provider says so. Stop, and tell it how much was heard, if it can use that |

They share the session, the speech queue, the room I/O and the metrics. A handoff can change pipeline.

### Interruptions

Speech over the agent first **pauses** playback, which is cheap to undo. A transcript, or the end of the user's turn, makes it final:
what was queued is dropped within one 20 ms frame. If no words come within `false_interruption_timeout`, it was noise, and playback
resumes where it stopped.

The history keeps **what was played, not what was generated**. The source's position is what has gone to the network. A TTS speaks a
sentence at a time, so sentences are known exactly, and inside a sentence the words are spread evenly, which is an estimate. Network and
client buffers are not counted. So a stored reply can run a few hundred milliseconds past what the user really heard.

### Turn detection

VAD plus a delay. `min_endpointing_delay` counts from the end of speech. The VAD is Silero, which is MIT licensed.

`EndOfTurnModel` is an interface for a model that predicts whether the user has finished. **No model is bundled. The official framework's
turn detector must not be plugged in**: its model license forbids using it with any other framework. See [licensing](licensing.md). If you want a smart
detector, it has to come from somewhere whose license allows it, and someone should read that license first.

### Credentials

A provider key is someone else's paid credential, so it is handled like an API secret.

- A worker reads keys from its own environment first, then, if configured, from the control plane.
- `PUT /api/v1/provider-credentials` stores a key sealed with AES-256-GCM and the master key, the same as API secrets. It is
  write-only: no response to a signed-in person ever contains one, nor does the audit log.
- `POST /api/v1/agent-credentials/resolve` is the only way back out. It takes an access token with the agent grant and no room join,
  which only someone holding the API secret can sign. Session cookies are refused, and so is any request with an `Origin` header,
  so a page holding such a token gets nothing.
- Keys never travel in job metadata or attributes, because other participants can read those.

### Choosing models at run time

A model is named `provider/model`, with `:voice` for a TTS. A choice can come from code, from configuration, or per room from dispatch
metadata: `{"models": {"llm": "anthropic/claude-haiku-5-5"}}`. `session.update_models` changes models in the middle of a session, and
an agent can carry its own.

### Observability

Each turn records wall clock times, from which come the latency breakdown: transcription, end of turn, LLM first token, TTS first
audio and end to end. Everything is measured or reported. A number that was not measured stays empty.

**Cost is never guessed.** It comes from the provider's own report, or from prices you supply in `Prices`. The framework ships no price
list, because prices change and differ by contract. One unknown part makes a turn's total unknown, so a partial sum cannot pass for
the whole cost.

Sessions go to any `MetricsSink`: a JSONL file, or the control plane, where `GET /api/v1/agent-sessions` reads them. Turns hold what
people said, so `ControlPlaneSink(include_text=False)` leaves that out. There is no dashboard page yet, only the API.

### Avatars

Not implemented. `streamkit_agents/avatar.py` holds the interface: an avatar worker is a participant of its own, with kind `agent` and
`lk.publish_on_behalf` set, which publishes audio and video. `AvatarProvider` is for a vendor, and `VideoGenerator` is for a self-hosted
engine. Anam, Tavus and the owner's own engine would each plug in there. The contract is described in that file.

## Measured

**This is one machine, and these are a few samples, not a benchmark.** An i7-14700HX with 28 threads, 31 GB and no GPU, shared with other
work, on 2026-11-05. The server, worker, engines and client all ran on that host, so WebRTC never crossed a real network. The engines are
llama.cpp with Qwen2.5 1.5B, faster-whisper `base.en` and Piper, all on the CPU.

End to end is from the end of the user's speech to the first audio of the reply, in seconds, by the agent's clock. The browser line is
from the last loud sample Chrome sent to the first sound it decoded.

| Run | Server | Transcription | LLM first token | TTS first audio | End to end | Load, 1 min |
| --- | --- | --- | --- | --- | --- | --- |
| Python, first question | ours | 0.657 | 0.386 | 0.054 | 1.323 | 7.2 |
| Python, first question | reference | 0.699 | 0.069 | 0.053 | 1.032 | 6.9 |
| Python, after an interruption | ours | 0.842 | 0.375 | 0.068 | 1.530 | 8.2 |
| Python, after an interruption | reference | 0.703 | 0.038 | 0.059 | 1.035 | 8.1 |
| Node, with a tool | ours | 0.848 | 2.361 | 0.137 | 3.736 | not recorded |
| Chrome, agent's clock | ours | 1.079 | 1.545 | 0.089 | 3.055 | 10.5 |
| Chrome, Chrome's clock | ours | | | | 3.338 | 10.5 |
| Chrome, agent's clock | reference | 0.673 | 0.148 | 0.061 | 1.065 | 17.0 |
| Chrome, Chrome's clock | reference | | | | 1.309 | 17.0 |

**Do not read this as one server against the other.** The whole gap is in the LLM's first token, and that is llama.cpp's prompt cache:
the reference runs second, with a prompt that differs from the last one by a word or two. Without the LLM, the first two rows come to
0.94 and 0.96 s. Transcription counts from the end of speech, so it includes the silence the VAD waits for.

No turn has been measured under packet loss, on a mobile network or across the internet.

## Limits found by testing

- **aiortc starts a track's receiver only if DTLS has finished when it applies that offer**, so a track offered during the handshake stays
  silent for good. Our room client holds further offers until the connection is up. How often this bites depends on the network, which
  is why it showed up only when the machine changed networks. `test_track_offered_during_the_handshake_is_heard` covers it.
- **The room clients do not resume.** When the machine moved from one Wi-Fi network to another during a test, on 2026-11-04 at 21:49:31,
  that test's session ended and stored nothing. The timing points to the address change, though that was not reproduced on purpose, and
  the same test passed 12 of 12 times on a steady network.
- **Whisper cannot always hear what Piper says.** With no WebRTC involved, it got "Tokyo" 17 of 30 times, and "Rome" and "Madrid" 30 of
  30. The tests that read speech back use words it can hear, so a miss there says something about the audio.

## What we learned from the official worker

Running the official worker and SDK against our server answered our first questions about it.

- It connects to `/rtc`, registers on `/agent`, and joins with our job token.
- Its wait for `TrackSubscribed` ends on our server, and its audio reaches a subscriber.
- Its transcripts in a user's name arrive whole, marked generated, on both servers, when read by our client.
- The official browser client, 2.23.0, shows a user's transcript as that user's on both servers. The analysis predicted it would drop one
  on the reference, whose chunks carry the agent's identity. For the short streams in `tests/agents/browser.test.mjs`, it does not.

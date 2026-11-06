# Voice AI provider matrix

What each provider adapter in `streamkit-agents` has been tested against, and which documentation it was written from.

Every entry is in one of three states, and only these three.

- **Real**: tested against the real service or engine, with real requests. The test is named.
- **Fixture**: written against the provider's documentation and tested against a local fixture replaying that documented wire
  format. This proves our protocol code only. It says nothing about the service, and it needs a key to go further.
- **Not supported**: there is no adapter, for the reason given.

No vendor API keys exist on this machine, so nearly everything is in the second state. That is the correct state for it.
Documentation was read on **2026-11-04**. Providers change their APIs: re-read before relying on an entry that is months old.

Fixture tests: `streamkit-agents/python/tests/test_providers_fixture.py` and `test_realtime_fixture.py`.

## LLM

About three protocols reach most LLMs.

| Provider | State | Adapter | Node.js | Documentation read | Notes |
| --- | --- | --- | --- | --- | --- |
| llama.cpp `llama-server` (local) | **Real** | `OpenAICompatibleLLM` | Yes | https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md | Build b12102, Qwen2.5 1.5B q4_k_m. Streaming, usage and tool calls, in `test_cascaded_real.py` and Node's `agents.test.ts` |
| OpenAI chat completions | **Fixture** | `OpenAICompatibleLLM` | Yes | https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create | That page shows no streaming tool call chunk. Its shape, `delta.tool_calls[]` by `index`, comes from the tool call fields on the same page and what llama.cpp produced for real. Check first with a key |
| Anthropic Messages | **Fixture** | `AnthropicLLM` | Yes | https://platform.claude.com/docs/en/build-with-claude/streaming | `anthropic-version: 2026-10-21`, as in the examples on that page |
| Google Gemini | **Fixture** | `GoogleLLM` | Yes | https://aistudio.google.com/api-reference/generate-content | `alt=sse` and the `x-goog-api-key` header are not on that page. They come from Google's other documentation and need confirming with a key |
| OpenRouter | **Fixture** | `OpenAICompatibleLLM` | Yes | https://openrouter.ai/docs/api/reference/overview | Documents streaming and tools. Its `usage.cost` is kept as the reported cost |
| Groq | **Fixture** | `OpenAICompatibleLLM` | Yes | https://console.groq.com/docs/openai, https://console.groq.com/docs/tool-use | Tools are documented in the OpenAI shape. Streaming with tools is not covered by the pages read |
| Together AI | **Fixture** | `OpenAICompatibleLLM` | Yes | https://docs.together.ai/docs/openai-api-compatibility | Documents chat completions with streaming and tools |
| DeepSeek | **Fixture** | `OpenAICompatibleLLM` | Yes | https://api-docs.deepseek.com/ | Documents streaming. The pages read do not confirm tools in this shape |
| Mistral | **Fixture** | `OpenAICompatibleLLM` | Yes | https://docs.mistral.ai/api/ | Streaming and tools are documented. `stream_options` is not, so the preset does not send it, and streamed usage is not expected |
| Ollama | **Fixture** | `OpenAICompatibleLLM` | Yes | https://docs.ollama.com/api/openai-compatibility | Needs no key. It is not run here because the installed models are 5 GB and the daemon is shared. `tool_choice` is documented as not supported |
| xAI | **Not supported** | | | https://docs.x.ai/docs/api-reference | The page read documents the Responses API, not chat completions. No Responses adapter exists yet |
| Azure OpenAI, AWS Bedrock, Fireworks, Cerebras | **Not supported** | | | not read | No adapter. Their documentation has not been read, so nothing is claimed |
| Hosted inference gateway | **Not supported** | | | | A cloud service, and not needed |

## Speech to text

| Provider | State | Adapter | Node.js | Documentation read | Notes |
| --- | --- | --- | --- | --- | --- |
| faster-whisper (local) | **Real** | `OpenAICompatibleSTT` | Yes | https://github.com/SYSTRAN/faster-whisper | Behind our small OpenAI-shaped server, `local-engines/stt_server.py`. `base.en`. In `test_cascaded_real.py` and Node's `agents.test.ts` |
| OpenAI `/audio/transcriptions` | **Fixture** | `OpenAICompatibleSTT` | Yes | https://developers.openai.com/api/docs/guides/speech-to-text | One utterance at a time, cut out by the VAD. `whisper-1` and the `gpt-4o-*-transcribe` models are documented as shutting down on 2027-02-26. `gpt-transcribe` is the current model. It takes `languages`, and we send `language`, so set no language with it until that is tested |
| Groq transcriptions | **Fixture** | `OpenAICompatibleSTT` | Yes | https://console.groq.com/docs/openai | Documented as supported, except `vtt` and `srt` |
| Deepgram live | **Fixture** | `DeepgramSTT` | Yes | https://developers.deepgram.com/reference/speech-to-text/listen-streaming | Streaming. The page lists `Finalize` and `CloseStream` by type name, without showing their JSON. `Authorization: Token` comes from Deepgram's speech reference |
| AssemblyAI streaming v3 | **Fixture** | `AssemblyAISTT` | Yes | https://www.assemblyai.com/docs/api-reference/streaming-api/streaming-api | Streaming. The key goes in `Authorization` with no prefix, and each message holds at least 50 ms |
| All others | **Not supported** | | | not read | Each speech vendor has its own protocol and needs its own client. None is claimed |

## Text to speech

| Provider | State | Adapter | Node.js | Documentation read | Notes |
| --- | --- | --- | --- | --- | --- |
| Piper (local) | **Real** | `OpenAICompatibleTTS` | Yes | https://github.com/OHF-Voice/piper1-gpl | Behind `local-engines/tts_server.py`, in its own process because it is GPL. Voice `en_US-ljspeech-medium` |
| OpenAI `/audio/speech` | **Fixture** | `OpenAICompatibleTTS` | Yes | https://developers.openai.com/api/docs/guides/text-to-speech | Raw PCM at 24 kHz. `tts-1`, `tts-1-hd` and `gpt-4o-mini-tts` are documented as shutting down on 2027-01-06, and the suggested replacement is on the Realtime API, not this endpoint |
| Cartesia | **Fixture** | `CartesiaTTS` | Yes | https://docs.cartesia.ai/api-reference/tts/bytes | `/tts/bytes`, so one sentence at a time. `Cartesia-Version: 2026-08-14`. The WebSocket API is not used |
| ElevenLabs | **Fixture** | `ElevenLabsTTS` | Yes | https://elevenlabs.io/docs/api-reference/text-to-speech/stream | HTTP streaming, one sentence at a time. Raw `pcm_24000` |
| Deepgram Aura | **Fixture** | `DeepgramTTS` | Yes | https://developers.deepgram.com/reference/text-to-speech/speak-request | `linear16` is documented with a WAV container, so the header is skipped |
| All others | **Not supported** | | | not read | As for STT |

## Realtime models

**No real realtime model has been run against this code.** The session plumbing runs over real WebRTC in
`test_realtime_fixture.py`, with a fixture in place of the model. No local engine that does full duplex fits on this machine. The
candidate looked at was [Kyutai's Moshi](https://github.com/kyutai-labs/moshi): its README asks for a GPU with 24 GB for the PyTorch
version and gives no way to run it on a CPU, and this machine has no working GPU driver. It was not tried.

| Provider | State | Adapter | Node.js | Documentation read | Notes |
| --- | --- | --- | --- | --- | --- |
| OpenAI Realtime, full duplex | **Fixture** | `OpenAIRealtimeModel`, `duplex=True` | Yes | https://developers.openai.com/api/docs/guides/realtime-conversations, https://developers.openai.com/api/docs/guides/voice-websockets?api=realtime, https://developers.openai.com/api/reference/resources/realtime/server-events | The GA shape, with `semantic_vad`. Truncates at the measured playout position |
| OpenAI Realtime, turn based | **Fixture** | `OpenAIRealtimeModel`, `duplex=False` | Yes | as above | `turn_detection: null`. Commit, then `response.create` |
| Gemini Live, full duplex | **Fixture** | `GeminiLiveModel`, `duplex=True` | Yes | https://aistudio.google.com/api-reference/live, https://aistudio.google.com/docs/live-api/capabilities, https://aistudio.google.com/docs/live-api/tools, https://ai.google.dev/gemini-api/docs/live-api/get-started-websocket | The API key goes in the URL, as documented, so it never appears in our logs or errors. The field naming the activity inside `voiceActivity` is not shown on the pages read, so `speech_stopped` from Gemini is unconfirmed |
| Gemini Live, turn based | **Fixture** | `GeminiLiveModel`, `duplex=False` | Yes | as above | `activityStart` and `activityEnd`, with automatic detection off |
| All others | **Not supported** | | | not read | AWS, Azure, Nvidia, Phonic, Thunderphone, Ultravox and xAI have realtime APIs. None has an adapter |

## Voice activity and turn detection

| Component | State | License | Notes |
| --- | --- | --- | --- |
| Silero VAD through ONNX Runtime | **Real** | MIT, checked in the repository's `LICENSE` on 2026-11-04 | Python and Node. https://github.com/snakers4/silero-vad |
| Energy VAD | **Real** | ours | Compares loudness. It cannot tell speech from noise |
| VAD plus delay | **Real** | ours | The default turn detection. `min_endpointing_delay` after the end of speech |
| `EndOfTurnModel` interface | Interface only | | No model is bundled |
| The official framework's turn detector | **Not supported** | Its model license forbids using it with any other framework | It must not be plugged into the interface above |

## Keys that would move an adapter to Real

Nobody needs to do anything now. This is the list for when someone wants to.

| Key | Moves to Real |
| --- | --- |
| `OPENAI_API_KEY` | OpenAI chat, transcription, speech, and both Realtime modes |
| `ANTHROPIC_API_KEY` | Anthropic |
| `GOOGLE_API_KEY` | Gemini, and both Gemini Live modes |
| `DEEPGRAM_API_KEY` | Deepgram STT and TTS |
| `ASSEMBLYAI_API_KEY` | AssemblyAI |
| `CARTESIA_API_KEY`, and a voice ID | Cartesia |
| `ELEVENLABS_API_KEY`, and a voice ID | ElevenLabs |
| `GROQ_API_KEY` | Groq chat and transcription |
| `OPENROUTER_API_KEY` | OpenRouter |
| `DEEPSEEK_API_KEY` | DeepSeek |
| `TOGETHER_API_KEY` | Together AI |
| `MISTRAL_API_KEY` | Mistral |

With a key in the environment, the same pipeline tests can run against the real service: pick the model with `ModelSelector` in
place of the local stack in `test_cascaded_real.py`.

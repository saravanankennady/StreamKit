"""Provider adapters against local fixtures that replay each provider's documented wire format.

PROTOCOL ONLY. These show the adapters speak what the documentation says. They say nothing about the real services, and no
provider counts as supported because of them. See docs/VOICE_AI_PROVIDER_MATRIX.md.
"""

from __future__ import annotations

import asyncio
import json

import pytest

from fixtures import HTTPFixture, Reply, WSFixture, collect, named_sse, pcm_tone, sse
from streamkit_agents.credentials import ControlPlaneCredentials, CredentialError, Credentials, EnvironmentCredentials
from streamkit_agents.llm import APIError, ChatContext, FallbackLLM, FunctionCall, FunctionCallOutput, RetryPolicy, function_tool
from streamkit_agents.models import ModelChoice, ModelSelector, UnknownProvider
from streamkit_agents.observability import Prices, TurnMetrics, UsageRecord
from streamkit_agents.llm.base import Usage
from streamkit_agents.providers.anthropic import API_VERSION, AnthropicLLM
from streamkit_agents.providers.assemblyai import AssemblyAISTT
from streamkit_agents.providers.deepgram import DeepgramSTT, DeepgramTTS
from streamkit_agents.providers.google import GoogleLLM
from streamkit_agents.providers.openai_compatible import OpenAICompatibleLLM, OpenAICompatibleSTT, OpenAICompatibleTTS
from streamkit_agents.providers.speech_http import CartesiaTTS, ElevenLabsTTS
from streamkit_agents.rtc import AudioFrame
from streamkit_agents.stt import SpeechEventType
from streamkit_agents.tts import SentenceTokenizer

KEY = "sk-fixture-secret-key"
FAST = RetryPolicy(max_retries=2, interval=0.01)


@function_tool
async def get_weather(city: str, unit: str = "c") -> str:
    """Gets the weather.

    Args:
        city: The city.
        unit: Celsius or fahrenheit.
    """
    return "fine"


def context() -> ChatContext:
    ctx = ChatContext()
    ctx.add_message("system", "Be brief.")
    ctx.add_message("user", "Weather in Paris?")
    ctx.add(FunctionCall(call_id="c1", name="get_weather", arguments='{"city": "Paris"}'))
    ctx.add(FunctionCall(call_id="c2", name="get_weather", arguments='{"city": "Rome"}'))
    ctx.add(FunctionCallOutput(call_id="c1", name="get_weather", output="sunny"))
    ctx.add(FunctionCallOutput(call_id="c2", name="get_weather", output="rain"))
    ctx.add_message("assistant", "Sunny in Paris.")
    ctx.add_message("user", "Thanks.")
    return ctx


async def test_openai_chat_streaming_and_tool_calls():
    def handler(request):
        return Reply(chunks=sse(
            {"id": "x", "object": "chat.completion.chunk", "choices": [{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": None}], "usage": None},
            {"choices": [{"index": 0, "delta": {"content": "Hel"}}]},
            {"choices": [{"index": 0, "delta": {"content": "lo"}}]},
            {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "id": "call_a", "type": "function", "function": {"name": "get_", "arguments": ""}}]}}]},
            {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"name": "weather", "arguments": '{"ci'}}]}}]},
            {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 1, "id": "call_b", "function": {"name": "other", "arguments": "{}"}}]}}]},
            {"choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"arguments": 'ty":"Paris"}'}}]}}]},
            {"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}]},
            {"choices": [], "usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18, "prompt_tokens_details": {"cached_tokens": 3}}},
            "[DONE]",
        ))

    async with HTTPFixture(handler) as f:
        llm = OpenAICompatibleLLM("gpt-x", base_url=f.url + "/v1", api_key=KEY, provider="openai")
        chunks = await collect(llm.chat(context(), tools=[get_weather], tool_choice="auto", temperature=0.5, max_tokens=99))
        await llm.aclose()

    request = f.requests[0]
    assert request.method == "POST" and request.path == "/v1/chat/completions"
    assert request.headers["authorization"] == f"Bearer {KEY}"
    body = request.json()
    assert body["model"] == "gpt-x" and body["stream"] is True and body["stream_options"] == {"include_usage": True}
    assert body["temperature"] == 0.5 and body["max_tokens"] == 99 and body["tool_choice"] == "auto"
    tool = body["tools"][0]
    assert tool["type"] == "function" and tool["function"]["name"] == "get_weather"
    assert tool["function"]["parameters"]["required"] == ["city"]
    assert tool["function"]["parameters"]["properties"]["city"] == {"type": "string", "description": "The city."}
    messages = body["messages"]
    assert [m["role"] for m in messages] == ["system", "user", "assistant", "tool", "tool", "assistant", "user"]
    assert [c["id"] for c in messages[2]["tool_calls"]] == ["c1", "c2"], "parallel calls belong to one message"
    assert messages[3] == {"role": "tool", "tool_call_id": "c1", "content": "sunny"}

    assert "".join(c.text for c in chunks) == "Hello"
    calls = [t for c in chunks for t in c.tool_calls]
    assert [(t.call_id, t.name, t.arguments) for t in calls] == [("call_a", "get_weather", '{"city":"Paris"}'), ("call_b", "other", "{}")]
    usage = next(c.usage for c in chunks if c.usage)
    assert (usage.prompt_tokens, usage.completion_tokens, usage.cached_tokens) == (11, 7, 3)
    assert usage.reported_cost is None


async def test_openrouter_reported_cost_is_kept():
    def handler(request):
        return Reply(chunks=sse(
            {"choices": [{"delta": {"content": "x"}}]},
            {"choices": [], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "cost": 0.00042}},
            "[DONE]",
        ))

    async with HTTPFixture(handler) as f:
        llm = OpenAICompatibleLLM("m", base_url=f.url, api_key=KEY, provider="openrouter")
        chunks = await collect(llm.chat(context()))
        await llm.aclose()
    usage = next(c.usage for c in chunks if c.usage)
    assert usage.reported_cost == 0.00042
    record = UsageRecord("llm", "openrouter/m", usage)
    # The provider's own figure wins over any prices we were given.
    assert record.cost(Prices(llm_input_per_mtok=100, llm_output_per_mtok=100)) == 0.00042


async def test_retry_before_output_but_never_after():
    attempts = []

    def flaky(request):
        attempts.append(1)
        if len(attempts) == 1:
            return Reply(status=503, chunks=[b"busy"])
        return Reply(chunks=sse({"choices": [{"delta": {"content": "ok"}}]}, "[DONE]"))

    async with HTTPFixture(flaky) as f:
        llm = OpenAICompatibleLLM("m", base_url=f.url, api_key=KEY, retry=FAST)
        chunks = await collect(llm.chat(context()))
        await llm.aclose()
    assert "".join(c.text for c in chunks) == "ok" and len(attempts) == 2

    # A client error will not get better.
    attempts.clear()

    def refused(request):
        attempts.append(1)
        return Reply(status=401, chunks=[b'{"error":"bad key"}'])

    async with HTTPFixture(refused) as f:
        llm = OpenAICompatibleLLM("m", base_url=f.url, api_key=KEY, retry=FAST)
        with pytest.raises(APIError) as e:
            await collect(llm.chat(context()))
        await llm.aclose()
    assert len(attempts) == 1 and e.value.status == 401
    assert KEY not in str(e.value)

    # Failing after words went out must not make the agent say them twice.
    attempts.clear()

    def dies(request):
        attempts.append(1)
        return Reply(chunks=[*sse({"choices": [{"delta": {"content": "half a sen"}}]}), b'data: {"error": {"message": "boom"}}\n\n'])

    async with HTTPFixture(dies) as f:
        llm = OpenAICompatibleLLM("m", base_url=f.url, api_key=KEY, retry=FAST)
        got = []
        with pytest.raises(APIError):
            async for c in llm.chat(context()):
                got.append(c.text)
        await llm.aclose()
    assert len(attempts) == 1 and got == ["half a sen"]


async def test_fallback_moves_on_before_output():
    def broken(request):
        return Reply(status=500, chunks=[b"down"])

    def working(request):
        return Reply(chunks=sse({"choices": [{"delta": {"content": "from the second"}}]}, "[DONE]"))

    async with HTTPFixture(broken) as a, HTTPFixture(working) as b:
        first = OpenAICompatibleLLM("m", base_url=a.url, retry=RetryPolicy(max_retries=0))
        second = OpenAICompatibleLLM("m", base_url=b.url, retry=RetryPolicy(max_retries=0))
        fallback = FallbackLLM([first, second])
        chunks = await collect(fallback.chat(context()))
        await fallback.aclose()
    assert "".join(c.text for c in chunks) == "from the second"


async def test_anthropic_messages_streaming():
    def handler(request):
        return Reply(chunks=named_sse(
            {"type": "message_start", "message": {"id": "msg_1", "type": "message", "role": "assistant", "content": [], "usage": {"input_tokens": 25, "output_tokens": 1, "cache_read_input_tokens": 4}}},
            {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
            {"type": "ping"},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Hello"}},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": " there"}},
            {"type": "content_block_stop", "index": 0},
            {"type": "content_block_start", "index": 1, "content_block": {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {}}},
            {"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": '{"city": "Pa'}},
            {"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": 'ris"}'}},
            {"type": "content_block_stop", "index": 1},
            {"type": "message_delta", "delta": {"stop_reason": "tool_use", "stop_sequence": None}, "usage": {"output_tokens": 15}},
            {"type": "message_stop"},
        ))

    async with HTTPFixture(handler) as f:
        llm = AnthropicLLM("claude-x", api_key=KEY, base_url=f.url)
        chunks = await collect(llm.chat(context(), tools=[get_weather], tool_choice="required"))
        await llm.aclose()

    request = f.requests[0]
    assert request.path == "/v1/messages"
    assert request.headers["x-api-key"] == KEY and request.headers["anthropic-version"] == API_VERSION
    body = request.json()
    assert body["stream"] is True and body["max_tokens"] > 0
    assert body["system"] == "Be brief."
    assert body["tool_choice"] == {"type": "any"}
    assert body["tools"][0]["input_schema"]["required"] == ["city"]
    roles = [m["role"] for m in body["messages"]]
    assert roles == ["user", "assistant", "user", "assistant", "user"], "roles must alternate"
    assert [b["type"] for b in body["messages"][1]["content"]] == ["tool_use", "tool_use"]
    assert [b["type"] for b in body["messages"][2]["content"]] == ["tool_result", "tool_result"]
    assert body["messages"][1]["content"][0]["input"] == {"city": "Paris"}

    assert "".join(c.text for c in chunks) == "Hello there"
    calls = [t for c in chunks for t in c.tool_calls]
    assert [(t.call_id, t.name, json.loads(t.arguments)) for t in calls] == [("toolu_1", "get_weather", {"city": "Paris"})]
    usage = next(c.usage for c in chunks if c.usage)
    assert (usage.prompt_tokens, usage.completion_tokens, usage.cached_tokens) == (25, 15, 4), "output counts are cumulative"


async def test_anthropic_overloaded_error_in_stream_is_retried():
    attempts = []

    def handler(request):
        attempts.append(1)
        if len(attempts) == 1:
            return Reply(chunks=named_sse({"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}))
        return Reply(chunks=named_sse(
            {"type": "message_start", "message": {"usage": {"input_tokens": 1, "output_tokens": 1}}},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "ok"}},
            {"type": "message_stop"},
        ))

    async with HTTPFixture(handler) as f:
        llm = AnthropicLLM("m", api_key=KEY, base_url=f.url, retry=FAST)
        chunks = await collect(llm.chat(context()))
        await llm.aclose()
    assert "".join(c.text for c in chunks) == "ok" and len(attempts) == 2


async def test_google_stream_generate_content():
    def handler(request):
        return Reply(chunks=sse(
            {"candidates": [{"content": {"role": "model", "parts": [{"text": "Hel"}]}}]},
            {"candidates": [{"content": {"parts": [{"text": "lo"}, {"functionCall": {"id": "fc1", "name": "get_weather", "args": {"city": "Paris"}}}]}, "finishReason": "STOP"}],
             "usageMetadata": {"promptTokenCount": 9, "candidatesTokenCount": 4, "totalTokenCount": 13}},
        ))

    async with HTTPFixture(handler) as f:
        llm = GoogleLLM("gemini-x", api_key=KEY, base_url=f.url)
        chunks = await collect(llm.chat(context(), tools=[get_weather]))
        await llm.aclose()

    request = f.requests[0]
    assert request.path == "/v1beta/models/gemini-x:streamGenerateContent"
    assert request.query["alt"] == ["sse"]
    assert request.headers["x-goog-api-key"] == KEY
    assert KEY not in str(request.query), "the key must not be in the URL"
    body = request.json()
    assert body["systemInstruction"] == {"parts": [{"text": "Be brief."}]}
    assert [c["role"] for c in body["contents"]] == ["user", "model", "user", "model", "user"]
    assert body["contents"][1]["parts"][0]["functionCall"]["args"] == {"city": "Paris"}
    assert body["contents"][2]["parts"][0]["functionResponse"]["name"] == "get_weather"
    assert body["tools"][0]["functionDeclarations"][0]["parametersJsonSchema"]["required"] == ["city"]

    assert "".join(c.text for c in chunks) == "Hello"
    calls = [t for c in chunks for t in c.tool_calls]
    assert [(t.call_id, t.name, json.loads(t.arguments)) for t in calls] == [("fc1", "get_weather", {"city": "Paris"})]
    usage = next(c.usage for c in chunks if c.usage)
    assert (usage.prompt_tokens, usage.completion_tokens) == (9, 4)


async def test_openai_transcription_upload():
    def handler(request):
        return Reply(chunks=[json.dumps({"text": "  hello there "}).encode()])

    async with HTTPFixture(handler) as f:
        stt = OpenAICompatibleSTT("gpt-transcribe", base_url=f.url + "/v1", api_key=KEY, language="en", provider="openai")
        event = await stt.recognize(AudioFrame(pcm_tone(0.5, 16000), 16000))
        await stt.aclose()
    request = f.requests[0]
    assert request.path == "/v1/audio/transcriptions" and request.headers["authorization"] == f"Bearer {KEY}"
    assert request.headers["content-type"].startswith("multipart/form-data")
    assert b'name="model"' in request.body and b"gpt-transcribe" in request.body and b"RIFF" in request.body
    assert event.text == "hello there" and event.type == SpeechEventType.FINAL
    assert abs(event.usage.audio_seconds - 0.5) < 0.01


async def test_deepgram_live_transcription():
    received = []

    async def handler(ws, fixture):
        await ws.send(json.dumps({"type": "Metadata", "request_id": "r"}))
        async for message in ws:
            received.append(message)
            if isinstance(message, bytes) and len([m for m in received if isinstance(m, bytes)]) == 2:
                await ws.send(json.dumps({"type": "SpeechStarted", "channel": [0], "timestamp": 0.1}))
                await ws.send(json.dumps({"type": "Results", "is_final": False, "speech_final": False, "channel": {"alternatives": [{"transcript": "hel", "confidence": 0.5}]}}))
                await ws.send(json.dumps({"type": "Results", "is_final": True, "speech_final": True, "channel": {"alternatives": [{"transcript": "hello", "confidence": 0.9}]}}))
            if isinstance(message, str) and json.loads(message)["type"] == "CloseStream":
                await ws.send(json.dumps({"type": "Metadata", "request_id": "r", "duration": 0.2}))
                return

    async with WSFixture(handler) as f:
        stt = DeepgramSTT("nova-3", api_key=KEY, base_url=f.url)
        stream = stt.stream()
        # 48 kHz input, to check it arrives at the rate declared in the URL.
        stream.push_frame(AudioFrame(pcm_tone(0.1, 48000), 48000))
        stream.push_frame(AudioFrame(pcm_tone(0.1, 48000), 48000))
        events = []

        async def read():
            async for e in stream:
                events.append(e)

        reader = asyncio.create_task(read())
        await asyncio.sleep(0.2)
        stream.flush()
        stream.end_input()
        await asyncio.wait_for(reader, 5)
        await stream.aclose()

    connection = f.connections[0]
    assert connection["headers"]["Authorization"] == f"Token {KEY}"
    for part in ("/v1/listen?", "model=nova-3", "encoding=linear16", "sample_rate=16000", "interim_results=true", "vad_events=true"):
        assert part in connection["path"]
    audio = b"".join(m for m in received if isinstance(m, bytes))
    assert 5000 < len(audio) < 7000, "0.2 s at 16 kHz is 6400 bytes"
    controls = [json.loads(m)["type"] for m in received if isinstance(m, str)]
    assert controls == ["Finalize", "CloseStream"]
    kinds = [e.type for e in events]
    assert SpeechEventType.START_OF_SPEECH in kinds and SpeechEventType.END_OF_SPEECH in kinds
    assert [e.text for e in events if e.type == SpeechEventType.INTERIM] == ["hel"]
    assert [e.text for e in events if e.type == SpeechEventType.FINAL] == ["hello"]
    assert any(e.type == SpeechEventType.USAGE and e.usage.audio_seconds for e in events)


async def test_assemblyai_streaming_turns():
    received = []

    async def handler(ws, fixture):
        await ws.send(json.dumps({"type": "Begin", "id": "s", "expires_at": 1}))
        async for message in ws:
            received.append(message)
            if isinstance(message, bytes) and len(received) == 1:
                await ws.send(json.dumps({"type": "SpeechStarted", "timestamp": 1, "confidence": 0.9}))
                await ws.send(json.dumps({"type": "Turn", "turn_order": 0, "end_of_turn": False, "transcript": "hello", "words": []}))
                await ws.send(json.dumps({"type": "Turn", "turn_order": 0, "end_of_turn": True, "transcript": "hello there", "utterance": "Hello there.", "words": []}))
            if isinstance(message, str) and json.loads(message)["type"] == "Terminate":
                await ws.send(json.dumps({"type": "Termination", "audio_duration_seconds": 1.5, "session_duration_seconds": 2}))
                return

    async with WSFixture(handler) as f:
        stt = AssemblyAISTT("universal-3-6-pro", api_key=KEY, base_url=f.url)
        stream = stt.stream()
        # Under 50 ms is held back, because the API wants at least that in each message.
        stream.push_frame(AudioFrame(pcm_tone(0.02, 16000), 16000))
        await asyncio.sleep(0.05)
        assert not received
        stream.push_frame(AudioFrame(pcm_tone(0.04, 16000), 16000))
        events = []

        async def read():
            async for e in stream:
                events.append(e)

        reader = asyncio.create_task(read())
        await asyncio.sleep(0.2)
        stream.end_input()
        await asyncio.wait_for(reader, 5)
        await stream.aclose()

    connection = f.connections[0]
    assert connection["headers"]["Authorization"] == KEY, "this API takes the key with no Bearer prefix"
    for part in ("/v3/ws?", "speech_model=universal-3-6-pro", "sample_rate=16000", "encoding=pcm_s16le"):
        assert part in connection["path"]
    assert len(received[0]) >= 1600, "50 ms at 16 kHz"
    assert [e.text for e in events if e.type == SpeechEventType.INTERIM] == ["hello"]
    assert [e.text for e in events if e.type == SpeechEventType.FINAL] == ["Hello there."]
    assert any(e.type == SpeechEventType.END_OF_SPEECH for e in events)
    assert next(e for e in events if e.type == SpeechEventType.USAGE).usage.audio_seconds == 1.5


async def test_openai_speech_handles_chunks_cut_mid_sample():
    pcm = pcm_tone(0.2, 24000)

    def handler(request):
        # Network chunks fall anywhere, including inside a sample.
        return Reply(chunks=[pcm[:1001], pcm[1001:5003], pcm[5003:]])

    async with HTTPFixture(handler) as f:
        tts = OpenAICompatibleTTS("gpt-4o-mini-tts", base_url=f.url + "/v1", api_key=KEY, voice="marin", provider="openai")
        frames = await collect(tts.synthesize("Hello"))
        await tts.aclose()
    request = f.requests[0]
    assert request.path == "/v1/audio/speech"
    assert request.json() == {"model": "gpt-4o-mini-tts", "input": "Hello", "voice": "marin", "response_format": "pcm"}
    assert b"".join(fr.data for fr in frames) == pcm
    assert all(fr.sample_rate == 24000 for fr in frames)
    assert tts.last_usage.characters == 5


async def test_cartesia_bytes():
    pcm = pcm_tone(0.1, 24000)

    async with HTTPFixture(lambda r: Reply(headers={"Content-Type": "audio/pcm"}, chunks=[pcm])) as f:
        tts = CartesiaTTS("sonic-3.6", voice="voice-id", api_key=KEY, base_url=f.url)
        frames = await collect(tts.synthesize("Hello"))
        await tts.aclose()
    request = f.requests[0]
    assert request.path == "/tts/bytes"
    assert request.headers["authorization"] == f"Bearer {KEY}" and request.headers["cartesia-version"] == "2026-08-14"
    body = request.json()
    assert body["model_id"] == "sonic-3.6" and body["transcript"] == "Hello" and body["voice"] == {"id": "voice-id"}
    assert body["output_format"] == {"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000}
    assert b"".join(fr.data for fr in frames) == pcm


async def test_elevenlabs_stream():
    pcm = pcm_tone(0.1, 24000)

    async with HTTPFixture(lambda r: Reply(chunks=[pcm])) as f:
        tts = ElevenLabsTTS("eleven_multilingual_v2", voice="abc", api_key=KEY, base_url=f.url)
        frames = await collect(tts.synthesize("Hello"))
        await tts.aclose()
    request = f.requests[0]
    assert request.path == "/v1/text-to-speech/abc/stream" and request.query["output_format"] == ["pcm_24000"]
    assert request.headers["xi-api-key"] == KEY
    assert request.json() == {"text": "Hello", "model_id": "eleven_multilingual_v2"}
    assert b"".join(fr.data for fr in frames) == pcm


async def test_deepgram_speech_skips_the_wav_header():
    pcm = pcm_tone(0.1, 24000)
    header = b"RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00" + bytes(16) + b"LIST\x04\x00\x00\x00abcd" + b"data\x00\x00\x00\x00"

    async with HTTPFixture(lambda r: Reply(chunks=[header[:10], header[10:] + pcm[:100], pcm[100:]])) as f:
        tts = DeepgramTTS("aura-2-thalia-en", api_key=KEY, base_url=f.url)
        frames = await collect(tts.synthesize("Hello"))
        await tts.aclose()
    request = f.requests[0]
    assert request.path == "/v1/speak"
    assert request.query["model"] == ["aura-2-thalia-en"] and request.query["encoding"] == ["linear16"] and request.query["sample_rate"] == ["24000"]
    assert request.headers["authorization"] == f"Token {KEY}"
    assert request.json() == {"text": "Hello"}
    assert b"".join(fr.data for fr in frames) == pcm


def test_sentence_tokenizer():
    t = SentenceTokenizer()
    assert t.push("Hello there, this is a test. Another ") == ["Hello there, this is a test."]
    assert t.push("one is coming! Short. ") == ["Another one is coming!"]
    assert t.push("Dr. Smith paid 3.5 dollars today. ") == ["Short. Dr. Smith paid 3.5 dollars today."]
    assert t.flush() == ""
    long = "word " * 80
    pieces = SentenceTokenizer(max_chars=100).push(long)
    assert pieces and all(len(p) <= 100 for p in pieces)


def test_function_tool_schema():
    assert get_weather.parameters == {
        "type": "object",
        "properties": {
            "city": {"type": "string", "description": "The city."},
            "unit": {"type": "string", "description": "Celsius or fahrenheit."},
        },
        "required": ["city"],
    }
    assert get_weather.description == "Gets the weather."


def test_cost_is_never_guessed():
    usage = Usage(prompt_tokens=1000, completion_tokens=500)
    record = UsageRecord("llm", "x/y", usage)
    assert record.cost(None) is None, "no prices and no report means unknown"
    assert record.cost(Prices(llm_input_per_mtok=1.0)) is None, "half the prices is still unknown"
    assert record.cost(Prices(llm_input_per_mtok=1.0, llm_output_per_mtok=2.0)) == pytest.approx(0.002)
    turn = TurnMetrics(usage=[record, UsageRecord("tts", "x/z", Usage(characters=10))])
    # One unknown part makes the total unknown, so a partial sum cannot pass for the whole cost.
    assert turn.to_dict(Prices(llm_input_per_mtok=1.0, llm_output_per_mtok=2.0))["cost_usd"] is None


async def test_control_plane_credentials():
    def handler(request):
        if request.json()["provider"] == "missing":
            return Reply(status=404, chunks=[b"{}"])
        if request.json()["provider"] == "refused":
            return Reply(status=403, chunks=[b"{}"])
        return Reply(chunks=[json.dumps({"secret": KEY}).encode()])

    async with HTTPFixture(handler) as f:
        source = ControlPlaneCredentials(f.url, "worker-token")
        creds = Credentials([EnvironmentCredentials(), source])
        assert await creds.require("deepgram") == KEY
        assert await source.get("deepgram") == KEY
        assert len(f.requests) == 1, "the key is cached, not fetched per call"
        assert f.requests[0].path == "/api/v1/agent-credentials/resolve"
        assert f.requests[0].headers["authorization"] == "Bearer worker-token"
        with pytest.raises(CredentialError):
            await creds.require("missing")
        with pytest.raises(CredentialError) as e:
            await creds.get("refused")
        assert "worker-token" not in str(e.value)


async def test_model_selection(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", KEY)
    monkeypatch.setenv("DEEPGRAM_API_KEY", KEY)
    monkeypatch.setenv("CARTESIA_API_KEY", KEY)
    monkeypatch.setenv("OPENAI_API_KEY", KEY)
    monkeypatch.setenv("GOOGLE_API_KEY", KEY)
    selector = ModelSelector()
    assert (await selector.llm("anthropic/claude-haiku-5-5")).label == "anthropic/claude-haiku-5-5"
    assert (await selector.stt("deepgram/nova-3")).label == "deepgram/nova-3"
    tts = await selector.tts("cartesia/sonic-3.6:voice-1")
    assert tts.label == "cartesia/sonic-3.6" and tts.voice == "voice-1"
    assert (await selector.realtime("openai/gpt-realtime-2.1")).label == "openai/gpt-realtime-2.1"
    assert (await selector.realtime("google/gemini-3.8-live")).label == "google/gemini-3.8-live"
    with pytest.raises(UnknownProvider):
        await selector.llm("nonsense/model")
    with pytest.raises(ValueError):
        await selector.llm("no-slash")
    with pytest.raises(ValueError):
        await selector.tts("cartesia/sonic-3.6")
    monkeypatch.delenv("ANTHROPIC_API_KEY")
    with pytest.raises(CredentialError):
        await selector.llm("anthropic/claude-haiku-5-5")


def test_model_choice_from_job_metadata():
    assert ModelChoice.from_job_metadata("") == ModelChoice()
    assert ModelChoice.from_job_metadata("not json") == ModelChoice()
    assert ModelChoice.from_job_metadata('{"other": 1}') == ModelChoice()
    chosen = ModelChoice.from_job_metadata('{"models": {"llm": "anthropic/x", "tts": "openai/y"}}')
    assert chosen.llm == "anthropic/x" and chosen.tts == "openai/y" and chosen.stt is None
    base = ModelChoice(stt="deepgram/nova-3", llm="openai/z")
    merged = base.overlay(chosen)
    assert merged.stt == "deepgram/nova-3" and merged.llm == "anthropic/x" and merged.tts == "openai/y"

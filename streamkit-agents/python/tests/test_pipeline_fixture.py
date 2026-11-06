"""The cascaded pipeline with vendor adapters, over real WebRTC, against fixtures replaying each vendor's documented wire format.

PROTOCOL ONLY for the vendors. What this adds to test_providers_fixture.py is that the adapters work as a pipeline: a streaming STT
with its own turn detection, an LLM streaming into a TTS, usage from each, and the same behaviour on both servers.
"""

from __future__ import annotations

import asyncio
import json
import uuid

import pytest

from fixtures import HTTPFixture, Reply, WSFixture, energy, named_sse, pcm_tone, sse
from helpers import TestUser, admin_token, agent_is_listening, eventually, tone, twirp, unique_room
from streamkit_agents import Agent, AgentSession, EnergyVAD, Prices, Worker
from streamkit_agents.providers.anthropic import AnthropicLLM
from streamkit_agents.providers.assemblyai import AssemblyAISTT
from streamkit_agents.providers.deepgram import DeepgramSTT, DeepgramTTS
from streamkit_agents.providers.google import GoogleLLM
from streamkit_agents.providers.openai_compatible import OpenAICompatibleLLM
from streamkit_agents.providers.speech_http import CartesiaTTS, ElevenLabsTTS

pytestmark = pytest.mark.webrtc

KEY = "sk-fixture-secret-key"


def deepgram_fixture(transcript: str):
    """Answers with a transcript once it has heard loud audio followed by quiet."""

    async def handler(ws, fixture):
        spoke = False
        quiet = 0
        async for message in ws:
            if isinstance(message, str):
                kind = json.loads(message)["type"]
                if kind == "CloseStream":
                    return
                continue
            if energy(message) > 0.02:
                if not spoke:
                    await ws.send(json.dumps({"type": "SpeechStarted", "channel": [0], "timestamp": 0.0}))
                spoke = True
                quiet = 0
            elif spoke:
                quiet += len(message)
                if quiet > 16000:
                    await ws.send(json.dumps({"type": "Results", "is_final": True, "speech_final": True, "channel": {"alternatives": [{"transcript": transcript, "confidence": 0.9}]}}))
                    spoke = False

    return WSFixture(handler)


def assemblyai_fixture(transcript: str):
    async def handler(ws, fixture):
        await ws.send(json.dumps({"type": "Begin", "id": "s", "expires_at": 1}))
        spoke = False
        quiet = 0
        async for message in ws:
            if isinstance(message, str):
                if json.loads(message)["type"] == "Terminate":
                    await ws.send(json.dumps({"type": "Termination", "audio_duration_seconds": 1, "session_duration_seconds": 1}))
                    return
                continue
            if energy(message) > 0.02:
                spoke = True
                quiet = 0
            elif spoke:
                quiet += len(message)
                if quiet > 16000:
                    await ws.send(json.dumps({"type": "Turn", "turn_order": 0, "end_of_turn": True, "transcript": transcript, "utterance": transcript, "words": []}))
                    spoke = False

    return WSFixture(handler)


def openai_llm_fixture(reply: str):
    words = reply.split(" ")

    def handler(request):
        return Reply(chunks=sse(
            *[{"choices": [{"index": 0, "delta": {"content": w + (" " if i < len(words) - 1 else "")}}]} for i, w in enumerate(words)],
            {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
            {"choices": [], "usage": {"prompt_tokens": 40, "completion_tokens": 8, "cost": 0.0001}},
            "[DONE]",
        ))

    return HTTPFixture(handler)


def anthropic_llm_fixture(reply: str):
    def handler(request):
        return Reply(chunks=named_sse(
            {"type": "message_start", "message": {"usage": {"input_tokens": 40, "output_tokens": 1}}},
            {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": reply}},
            {"type": "content_block_stop", "index": 0},
            {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 8}},
            {"type": "message_stop"},
        ))

    return HTTPFixture(handler)


def google_llm_fixture(reply: str):
    def handler(request):
        return Reply(chunks=sse({"candidates": [{"content": {"role": "model", "parts": [{"text": reply}]}, "finishReason": "STOP"}], "usageMetadata": {"promptTokenCount": 40, "candidatesTokenCount": 8}}))

    return HTTPFixture(handler)


def speech_fixture(rate: int = 24000, seconds: float = 1.0):
    return HTTPFixture(lambda request: Reply(chunks=[pcm_tone(seconds, rate)]))


async def run_pipeline(server, stt, llm, tts, *, vad=None, turn_detection=None) -> AgentSession:
    """Speaks, waits for the reply, and returns the session."""
    holder = {}
    started = asyncio.Event()

    async def entry(ctx):
        await ctx.connect()
        session = AgentSession(stt=stt, llm=llm, tts=tts, vad=vad, turn_detection=turn_detection, prices=Prices(tts_per_mchar=10.0, stt_per_minute=1.0))
        ctx.add_shutdown_callback(session.aclose)
        await session.start(Agent(instructions="Be brief."), room=ctx.room)
        holder["session"] = session
        holder["identity"] = ctx.room.local_identity
        started.set()
        await ctx.wait_for_shutdown()

    name = f"agent-{uuid.uuid4().hex[:8]}"
    worker = Worker(entry, ws_url=server.ws_url, api_key=server.api_key, api_secret=server.api_secret, agent_name=name, load_fn=lambda: 0.0)
    worker.start()
    await worker.wait_registered()
    room = unique_room("pipeline")
    user = TestUser(server, room)
    try:
        await user.join()
        status, body = await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": name, "room": room}, admin_token(server, room))
        assert status == 200, body
        await asyncio.wait_for(started.wait(), 20)
        await eventually(lambda: any(p.is_agent for p in user.room.remote_participants.values()), message="the agent to appear")
        await agent_is_listening(holder["session"])
        await user.say(tone(0.8))
        await asyncio.sleep(0.8)
        session = holder["session"]
        await eventually(lambda: session.metrics.turns, timeout=20, message="the turn to finish")

        heard = user.heard.get(holder["identity"])
        assert heard is not None and sum(end - start for start, end in heard.speech_periods(0.05)) > 0.5, "the reply was not heard"
        messages = [(m.role, m.content) for m in session.history.messages()]
        assert messages[0] == ("user", "hello from the user")
        assert messages[1] == ("assistant", "A reply from the model that is long enough.")
        await eventually(lambda: any(t["identity"] == "user" for t in user.transcripts), message="the user's transcript")
        return session
    finally:
        await user.leave()
        await worker.aclose()


async def test_deepgram_with_its_own_turn_detection_openai_and_deepgram_speech(server):
    async with deepgram_fixture("hello from the user") as stt_fixture, openai_llm_fixture("A reply from the model that is long enough.") as llm_fixture, speech_fixture() as tts_fixture:
        session = await run_pipeline(
            server,
            DeepgramSTT("nova-3", api_key=KEY, base_url=stt_fixture.url),
            OpenAICompatibleLLM("gpt-x", base_url=llm_fixture.url, api_key=KEY, provider="openai"),
            DeepgramTTS("aura-2-thalia-en", api_key=KEY, base_url=tts_fixture.url),
            turn_detection="stt",
        )
        turn = session.metrics.turns[0]
        assert turn.end_to_end is not None
        llm = next(r for r in turn.usage if r.kind == "llm")
        assert llm.usage.reported_cost == 0.0001
        assert session.metrics.models == {"stt": "deepgram/nova-3", "llm": "openai/gpt-x", "tts": "deepgram/aura-2-thalia-en"}


async def test_streaming_stt_with_the_vad_deciding_the_turn(server):
    """The VAD can finish before the STT does. The turn must wait for the words, not be lost."""
    async with deepgram_fixture("hello from the user") as stt_fixture, openai_llm_fixture("A reply from the model that is long enough.") as llm_fixture, speech_fixture() as tts_fixture:
        session = await run_pipeline(
            server,
            DeepgramSTT("nova-3", api_key=KEY, base_url=stt_fixture.url),
            OpenAICompatibleLLM("gpt-x", base_url=llm_fixture.url, api_key=KEY),
            CartesiaTTS("sonic-3.6", voice="v", api_key=KEY, base_url=tts_fixture.url),
            vad=EnergyVAD(min_silence=0.15),
            turn_detection="vad",
        )
        assert session.metrics.turns[0].end_of_turn_delay is not None


async def test_assemblyai_anthropic_elevenlabs(server):
    async with assemblyai_fixture("hello from the user") as stt_fixture, anthropic_llm_fixture("A reply from the model that is long enough.") as llm_fixture, speech_fixture() as tts_fixture:
        session = await run_pipeline(
            server,
            AssemblyAISTT("universal-3-6-pro", api_key=KEY, base_url=stt_fixture.url),
            AnthropicLLM("claude-x", api_key=KEY, base_url=llm_fixture.url),
            ElevenLabsTTS("eleven_multilingual_v2", voice="v", api_key=KEY, base_url=tts_fixture.url),
            turn_detection="stt",
        )
        usage = next(r.usage for r in session.metrics.turns[0].usage if r.kind == "llm")
        assert (usage.prompt_tokens, usage.completion_tokens) == (40, 8)


async def test_google_llm_in_a_pipeline(server):
    async with deepgram_fixture("hello from the user") as stt_fixture, google_llm_fixture("A reply from the model that is long enough.") as llm_fixture, speech_fixture() as tts_fixture:
        session = await run_pipeline(
            server,
            DeepgramSTT("nova-3", api_key=KEY, base_url=stt_fixture.url),
            GoogleLLM("gemini-x", api_key=KEY, base_url=llm_fixture.url),
            CartesiaTTS("sonic-3.6", voice="v", api_key=KEY, base_url=tts_fixture.url),
            turn_detection="stt",
        )
        assert session.metrics.models["llm"] == "google/gemini-x"

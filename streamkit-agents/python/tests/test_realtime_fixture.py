"""Realtime pipelines over real WebRTC, with a local fixture in place of the model.

THE MODEL IS A FIXTURE. The room, the media and the session plumbing are real: audio goes through the server both ways and is
decoded at the far end. The "model" only replays the documented protocol, with a loudness check for its turn detection, and
speaks a tone. No real realtime model has been run against this code.
"""

from __future__ import annotations

import asyncio
import json
import uuid

import pytest

from fixtures import GeminiLiveFixture, OpenAIRealtimeFixture
from helpers import TestUser, admin_token, agent_is_listening, eventually, tone, twirp, unique_room
from streamkit_agents import Agent, AgentSession, EnergyVAD, RunContext, Worker, function_tool
from streamkit_agents.providers.gemini_live import GeminiLiveModel
from streamkit_agents.providers.openai_realtime import OpenAIRealtimeModel

pytestmark = pytest.mark.webrtc

KEY = "sk-fixture-secret-key"


class RealtimeHarness:
    def __init__(self, server, model, *, duplex: bool, agent: Agent | None = None, vad=None, **session_kwargs):
        self.server = server
        self.model = model
        self.duplex = duplex
        self.agent = agent or Agent(instructions="You are a test agent.")
        self.vad = vad
        self.session_kwargs = session_kwargs
        self.session: AgentSession | None = None
        self.ctx = None
        self.started = asyncio.Event()
        self.name = f"agent-{uuid.uuid4().hex[:8]}"
        self.room = unique_room("rt")
        self.worker = Worker(self.entry, ws_url=server.ws_url, api_key=server.api_key, api_secret=server.api_secret, agent_name=self.name, load_fn=lambda: 0.0)
        self.user = TestUser(server, self.room)

    async def entry(self, ctx) -> None:
        self.ctx = ctx
        await ctx.connect()
        self.session = AgentSession(realtime=self.model, duplex=self.duplex, vad=self.vad, **self.session_kwargs)
        ctx.add_shutdown_callback(self.session.aclose)
        await self.session.start(self.agent, room=ctx.room)
        self.started.set()
        await ctx.wait_for_shutdown()

    async def __aenter__(self):
        self.worker.start()
        await self.worker.wait_registered()
        await self.user.join()
        status, body = await twirp(self.server, "AgentDispatchService", "CreateDispatch", {"agent_name": self.name, "room": self.room}, admin_token(self.server, self.room))
        assert status == 200, body
        await asyncio.wait_for(self.started.wait(), 20)
        await eventually(lambda: any(p.is_agent for p in self.user.room.remote_participants.values()), message="the agent to appear")
        await agent_is_listening(self.session)
        return self

    async def __aexit__(self, *exc):
        await self.user.leave()
        await self.worker.aclose()

    @property
    def heard(self):
        return self.user.heard.get(self.ctx.room.local_identity)

    def heard_seconds(self, threshold: float = 0.05) -> float:
        heard = self.heard
        if heard is None:
            return 0.0
        return sum(end - start for start, end in heard.speech_periods(threshold))


async def speak_then_silence(user: TestUser, seconds: float = 0.6) -> None:
    await user.say(tone(seconds))
    # Silence afterwards is what lets a turn detector decide it has ended.
    await asyncio.sleep(0.6)


async def test_openai_duplex_reply_reaches_the_user(server):
    async with OpenAIRealtimeFixture(reply_seconds=1.5, transcript="hello from the fixture model") as fixture:
        model = OpenAIRealtimeModel("gpt-realtime-2.1", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=True) as h:
            await speak_then_silence(h.user)
            await eventually(lambda: h.heard_seconds() > 1.0, timeout=15, message="the model's voice to reach the user")
            await eventually(lambda: h.session.current_speech is None and h.session.metrics.turns, timeout=15, message="the turn to finish")

            # What the adapter sent is what the documentation shows.
            connection = fixture.fixture.connections[0]
            assert connection["headers"]["Authorization"] == f"Bearer {KEY}"
            assert connection["path"] == "/v1/realtime?model=gpt-realtime-2.1"
            config = fixture.session
            assert config["type"] == "realtime" and config["instructions"] == "You are a test agent."
            assert config["audio"]["input"]["format"] == {"type": "audio/pcm", "rate": 24000}
            assert config["audio"]["output"]["voice"] == "marin"
            assert config["audio"]["input"]["turn_detection"] == {"type": "semantic_vad"}
            assert len(fixture.appended) > 24000, "the user's audio reached the model"

            messages = [(m.role, m.content) for m in h.session.history.messages()]
            assert ("user", "what the user said") in messages
            assert ("assistant", "hello from the fixture model") in messages

            turn = h.session.metrics.turns[-1]
            assert turn.pipeline == "realtime-duplex" and turn.interrupted is False
            assert turn.end_to_end is not None and turn.end_to_end < 10
            usage = next(r.usage for r in turn.usage if r.kind == "realtime")
            assert (usage.prompt_tokens, usage.completion_tokens, usage.audio_input_tokens, usage.audio_output_tokens, usage.cached_tokens) == (100, 50, 80, 45, 5)


async def test_openai_duplex_interruption_truncates_at_what_played(server):
    async with OpenAIRealtimeFixture(reply_seconds=10, transcript=" ".join(f"word{i}" for i in range(40))) as fixture:
        model = OpenAIRealtimeModel("gpt-realtime-2.1", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=True) as h:
            await speak_then_silence(h.user)
            await eventually(lambda: h.session.state == "speaking", timeout=15, message="the model to speak")
            await asyncio.sleep(1.5)
            played_before = h.session.output.played_seconds
            await h.user.say(tone(0.5))
            await eventually(lambda: fixture.truncated, timeout=10, message="the truncate event")
            truncate = fixture.truncated[0]
            assert truncate["content_index"] == 0 and truncate["item_id"].startswith("item_resp_")
            # Measured from playout, so it reflects what the user could have heard.
            assert int(played_before * 1000) <= truncate["audio_end_ms"] <= (played_before + 1.5) * 1000, truncate
            assert any(e["type"] == "response.cancel" for e in fixture.events)

            assert h.session.output.queued_seconds == 0, "queued audio must be dropped at once"
            cut = next(m for m in h.session.history.messages() if m.role == "assistant")
            assert cut.interrupted and 0 < len(cut.content.split()) < 30
            turn = h.session.metrics.turns[0]
            assert turn.interrupted and turn.played_text == cut.content


async def test_openai_turn_based_commits_when_the_local_vad_says(server):
    async with OpenAIRealtimeFixture(reply_seconds=1.0) as fixture:
        model = OpenAIRealtimeModel("gpt-realtime-2.1", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=False, vad=EnergyVAD(min_silence=0.2), min_endpointing_delay=0.3) as h:
            assert fixture.session["audio"]["input"]["turn_detection"] is None, "turn based mode turns the provider's detection off"
            before = len(fixture.appended)
            await asyncio.sleep(0.5)
            assert len(fixture.appended) == before, "no audio goes to the model between turns"

            await speak_then_silence(h.user)
            await eventually(lambda: h.heard_seconds() > 0.7, timeout=15, message="the reply")
            names = [e["type"] for e in fixture.events]
            assert names.index("input_audio_buffer.commit") < names.index("response.create")
            assert 0.3 * 48000 < len(fixture.appended) < 3 * 48000, f"{len(fixture.appended)} bytes of audio"
            await eventually(lambda: h.session.metrics.turns, timeout=10, message="the turn's record")
            turn = h.session.metrics.turns[-1]
            assert turn.pipeline == "realtime-turn"
            assert turn.end_of_turn_delay is not None and turn.end_to_end is not None


async def test_openai_turn_based_interruption_by_local_vad(server):
    async with OpenAIRealtimeFixture(reply_seconds=10) as fixture:
        model = OpenAIRealtimeModel("gpt-realtime-2.1", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=False, vad=EnergyVAD(min_silence=0.2), min_endpointing_delay=0.2, min_interruption_duration=0.2) as h:
            await speak_then_silence(h.user)
            await eventually(lambda: h.session.state == "speaking", timeout=15, message="the model to speak")
            await asyncio.sleep(1.0)
            await h.user.say(tone(0.6))
            await eventually(lambda: fixture.truncated, timeout=10, message="the truncate event")
            assert any(e["type"] == "response.cancel" for e in fixture.events)
            assert h.session.metrics.turns[0].interrupted


async def test_openai_tool_call(server):
    seen = []

    @function_tool
    async def get_weather(ctx: RunContext, city: str) -> str:
        """Gets the weather.

        Args:
            city: The city.
        """
        seen.append(city)
        return "sunny"

    call = {"call_id": "call_1", "name": "get_weather", "arguments": json.dumps({"city": "Paris"})}
    async with OpenAIRealtimeFixture(reply_seconds=0.5, tool_call=call) as fixture:
        model = OpenAIRealtimeModel("gpt-realtime-2.1", api_key=KEY, base_url=fixture.url)
        agent = Agent(instructions="Use tools.", tools=[get_weather])
        async with RealtimeHarness(server, model, duplex=True, agent=agent) as h:
            assert fixture.session["tools"][0]["name"] == "get_weather"
            await speak_then_silence(h.user)
            await eventually(lambda: seen == ["Paris"], timeout=15, message="the tool to run")
            await eventually(lambda: fixture.tool_outputs, timeout=10, message="the tool result")
            assert fixture.tool_outputs[0] == {"type": "function_call_output", "call_id": "call_1", "output": "sunny"}
            # This provider does not reply to a tool result by itself, so the framework asks.
            await eventually(lambda: fixture.responses >= 2, timeout=10, message="a second response")
            await eventually(lambda: h.heard_seconds() > 0.2, timeout=15, message="the reply after the tool")


async def test_openai_handoff_opens_a_session_with_new_instructions(server):
    class Second(Agent):
        def __init__(self):
            super().__init__(instructions="You are the second agent.", name="second")

    @function_tool
    async def transfer(ctx: RunContext) -> Agent:
        """Transfers the call."""
        return Second()

    call = {"call_id": "call_1", "name": "transfer", "arguments": "{}"}
    async with OpenAIRealtimeFixture(reply_seconds=0.3, tool_call=call) as fixture:
        model = OpenAIRealtimeModel("gpt-realtime-2.1", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=True, agent=Agent(instructions="First.", tools=[transfer], name="first")) as h:
            identity = h.ctx.room.local_identity
            await speak_then_silence(h.user)
            await eventually(lambda: h.session.agent.name == "second", timeout=15, message="the handoff")
            await eventually(lambda: len(fixture.fixture.connections) == 2, timeout=10, message="the new connection")
            await eventually(lambda: fixture.session.get("instructions") == "You are the second agent.", timeout=5, message="the new instructions")
            assert h.ctx.room.local_identity == identity


async def test_gemini_duplex(server):
    async with GeminiLiveFixture(reply_seconds=1.5) as fixture:
        model = GeminiLiveModel("gemini-3.8-live", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=True) as h:
            await speak_then_silence(h.user)
            await eventually(lambda: h.heard_seconds() > 1.0, timeout=15, message="the model's voice to reach the user")
            await eventually(lambda: h.session.metrics.turns, timeout=15, message="the turn's record")

            connection = fixture.fixture.connections[0]
            assert connection["path"].startswith("/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?key=")
            setup = fixture.setup
            assert setup["model"] == "models/gemini-3.8-live"
            assert setup["generationConfig"]["responseModalities"] == ["AUDIO"]
            assert setup["generationConfig"]["speechConfig"]["voiceConfig"]["prebuiltVoiceConfig"]["voiceName"] == "Kore"
            assert setup["systemInstruction"] == {"parts": [{"text": "You are a test agent."}]}
            assert "realtimeInputConfig" not in setup, "automatic detection is the default"
            assert fixture.messages[0]["realtimeInput"]["audio"]["mimeType"] == "audio/pcm;rate=16000"
            assert len(fixture.audio_received) > 16000
            messages = [(m.role, m.content) for m in h.session.history.messages()]
            assert ("user", "what the user said") in messages
            assert ("assistant", "hello from the fixture model") in messages
            turn = h.session.metrics.turns[-1]
            usage = next(r.usage for r in turn.usage if r.kind == "realtime")
            assert (usage.prompt_tokens, usage.completion_tokens) == (30, 40)
            # A key in a WebSocket URL must not leak through an error.
            assert not [e for e in h.session.metrics.errors if KEY in e["message"]]


async def test_gemini_interrupted_by_the_provider(server):
    async with GeminiLiveFixture(reply_seconds=10) as fixture:
        model = GeminiLiveModel("gemini-3.8-live", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=True) as h:
            await speak_then_silence(h.user)
            await eventually(lambda: h.session.state == "speaking", timeout=15, message="the model to speak")
            await asyncio.sleep(1.0)
            await h.user.say(tone(0.4))
            await eventually(lambda: h.session.metrics.turns and h.session.metrics.turns[0].interrupted, timeout=10, message="the reply to be cut off")
            assert h.session.output.queued_seconds == 0
            # Gemini cannot be told how much was heard, so the framework must not try.
            assert not h.session.metrics.errors


async def test_gemini_turn_based(server):
    async with GeminiLiveFixture(reply_seconds=1.0) as fixture:
        model = GeminiLiveModel("gemini-3.8-live", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=False, vad=EnergyVAD(min_silence=0.2), min_endpointing_delay=0.3) as h:
            assert fixture.setup["realtimeInputConfig"]["automaticActivityDetection"]["disabled"] is True
            await speak_then_silence(h.user)
            await eventually(lambda: h.heard_seconds() > 0.7, timeout=15, message="the reply")
            shapes = [next(iter(m["realtimeInput"])) for m in fixture.messages if "realtimeInput" in m]
            assert shapes[0] == "activityStart" and shapes[-1] == "activityEnd" and "audio" in shapes


async def test_gemini_tool_call_replies_by_itself(server):
    seen = []

    @function_tool
    async def lookup(ctx: RunContext, term: str) -> str:
        """Looks something up.

        Args:
            term: What to look up.
        """
        seen.append(term)
        return "found it"

    async with GeminiLiveFixture(reply_seconds=0.5, tool_call={"id": "c1", "name": "lookup", "args": {"term": "x"}}) as fixture:
        model = GeminiLiveModel("gemini-3.8-live", api_key=KEY, base_url=fixture.url)
        async with RealtimeHarness(server, model, duplex=True, agent=Agent(instructions="Use tools.", tools=[lookup])) as h:
            assert fixture.setup["tools"][0]["functionDeclarations"][0]["name"] == "lookup"
            await speak_then_silence(h.user)
            await eventually(lambda: seen == ["x"], timeout=15, message="the tool to run")
            await eventually(lambda: fixture.tool_responses, timeout=10, message="the tool result")
            assert fixture.tool_responses[0] == {"id": "c1", "name": "lookup", "response": {"result": "found it"}}
            await eventually(lambda: h.heard_seconds() > 0.2, timeout=15, message="the reply after the tool")

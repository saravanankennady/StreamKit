"""The real thing: a worker joins a room over WebRTC, hears real speech, and answers with real speech, through local STT, LLM
and TTS engines. Nothing in the media path is simulated. What the user receives is decoded from RTP, and transcribed by Whisper
to check that the agent said it.

A second STT instance doing the checking is the only reason these tests need Whisper twice.
"""

from __future__ import annotations

import asyncio
import json
import os
import pathlib
import time
import uuid

import httpx
import pytest

from helpers import TestUser, admin_token, agent_is_listening, eventually, twirp, unique_room
from streamkit_agents import Agent, AgentSession, RunContext, SileroVAD, Worker, function_tool
from streamkit_agents.providers.openai_compatible import OpenAICompatibleLLM, OpenAICompatibleSTT, OpenAICompatibleTTS
from streamkit_agents.rtc import AudioFrame
from streamkit_agents.stt import STT, SpeechEvent, SpeechEventType

pytestmark = [pytest.mark.webrtc, pytest.mark.engines]

RESULTS = pathlib.Path(os.environ.get("STREAMKIT_AGENT_RESULTS", "/run/media/nsroot/develop/streamin_engine/.toolchain/tmp/agents-latency.jsonl"))


async def speak(engines, text: str) -> AudioFrame:
    """Makes real speech for the user to say, with the TTS engine."""
    async with httpx.AsyncClient(timeout=30) as client:
        response = await client.post(f"{engines.tts_url}/audio/speech", json={"input": text, "model": engines.tts_model, "voice": "default"})
    response.raise_for_status()
    return AudioFrame(response.content, engines.tts_rate)


async def transcribe(engines, pcm: bytes, rate: int = 16000) -> str:
    stt = OpenAICompatibleSTT(engines.stt_model, base_url=engines.stt_url)
    try:
        return (await stt.recognize(AudioFrame(pcm, rate))).text
    finally:
        await stt.aclose()


def local_llm(engines, *, provider: str = "local") -> OpenAICompatibleLLM:
    return OpenAICompatibleLLM(engines.llm_model, base_url=engines.llm_url, provider=provider, extra_body={"temperature": 0, "seed": 7})


def local_stack(engines) -> dict:
    return {
        "stt": OpenAICompatibleSTT(engines.stt_model, base_url=engines.stt_url, provider="local"),
        "llm": local_llm(engines),
        "tts": OpenAICompatibleTTS(engines.tts_model, base_url=engines.tts_url, provider="local", sample_rate=engines.tts_rate),
        "vad": SileroVAD(),
    }


class Harness:
    """A worker running one kind of agent, with a user in the room."""

    def __init__(self, server, engines, make_agent, **session_kwargs):
        self.server = server
        self.engines = engines
        self.make_agent = make_agent
        self.session_kwargs = session_kwargs
        self.session: AgentSession | None = None
        self.ctx = None
        self.started = asyncio.Event()
        self.name = f"agent-{uuid.uuid4().hex[:8]}"
        self.room = unique_room("voice")
        self.worker = Worker(self.entry, ws_url=server.ws_url, api_key=server.api_key, api_secret=server.api_secret, agent_name=self.name, load_fn=lambda: 0.0)
        self.user = TestUser(server, self.room)

    async def entry(self, ctx) -> None:
        self.ctx = ctx
        await ctx.connect()
        stack = {**local_stack(self.engines), **self.session_kwargs}
        self.session = AgentSession(**stack)
        ctx.add_shutdown_callback(self.session.aclose)
        await self.session.start(self.make_agent(), room=ctx.room, job_id=ctx.job_id, agent_name=ctx.agent_name)
        self.started.set()
        await ctx.wait_for_shutdown()

    async def __aenter__(self) -> "Harness":
        self.worker.start()
        await self.worker.wait_registered()
        await self.user.join()
        status, body = await twirp(self.server, "AgentDispatchService", "CreateDispatch", {"agent_name": self.name, "room": self.room}, admin_token(self.server, self.room))
        assert status == 200, body
        await asyncio.wait_for(self.started.wait(), 20)
        await eventually(lambda: any(p.is_agent for p in self.user.room.remote_participants.values()), message="the agent to appear")
        await agent_is_listening(self.session)
        return self

    async def __aexit__(self, *exc) -> None:
        await self.user.leave()
        await self.worker.aclose()

    @property
    def heard(self):
        identity = self.ctx.room.local_identity
        return self.user.heard.get(identity)

    async def wait_for_reply_audio(self, *, timeout: float = 30, since: float | None = None) -> None:
        def has_sound() -> bool:
            heard = self.heard
            if heard is None:
                return False
            return any(end >= (since or 0) for _, end in heard.speech_periods())

        await eventually(has_sound, timeout=timeout, message="the agent's audio to reach the user")

    async def wait_for_quiet(self, *, timeout: float = 45, quiet_for: float = 1.0) -> None:
        loop = asyncio.get_running_loop()

        def quiet() -> bool:
            periods = self.heard.speech_periods() if self.heard else []
            return bool(periods) and loop.time() - periods[-1][1] > quiet_for and self.session.current_speech is None

        await eventually(quiet, timeout=timeout, message="the agent to finish speaking")


def record_latency(server, turn, label: str) -> dict:
    latency = turn.to_dict()["latency"]
    load = os.getloadavg()
    record = {"test": label, "server": server.name, "at": time.time(), "latency": latency, "load_1m": load[0], "cpus": os.cpu_count()}
    RESULTS.parent.mkdir(parents=True, exist_ok=True)
    with RESULTS.open("a") as f:
        f.write(json.dumps(record) + "\n")
    print(f"\n{label} on {server.name}: {latency}, load {load[0]:.1f} on {os.cpu_count()} CPUs")
    return record


async def test_real_speech_in_real_speech_out(server, engines):
    # A different question on each server. llama.cpp remembers the last prompt, and answers the same one again in a tenth of the time,
    # which would make whichever server came second look faster. These two capitals are ones Whisper can hear from Piper: 30 of 30
    # with no WebRTC in the way. "Tokyo" managed 17 of 30, as "Tokuvau" and the like, and "Paris" 29 of 30.
    country, capital = ("Italy", "rome") if not server.is_reference else ("Spain", "madrid")
    agent = lambda: Agent(instructions="You are a voice assistant. Answer in one short sentence, without any lists or symbols.")
    async with Harness(server, engines, agent) as h:
        question = await speak(engines, f"What is the capital of {country}?")
        await h.user.say(question)
        await h.wait_for_reply_audio()
        await h.wait_for_quiet()

        heard_text = await transcribe(engines, h.heard.pcm)
        assert capital in heard_text.lower(), heard_text

        history = [(m.role, m.content) for m in h.session.history.messages()]
        assert history[0][0] == "user" and f"capital of {country.lower()}" in history[0][1].lower(), history
        assert history[1][0] == "assistant" and capital in history[1][1].lower(), history

        # The latency breakdown holds real numbers for every stage.
        assert len(h.session.metrics.turns) == 1
        turn = h.session.metrics.turns[0]
        assert turn.interrupted is False
        for name in ("transcription_delay", "end_of_turn_delay", "llm_time_to_first_token", "tts_time_to_first_audio", "end_to_end"):
            value = getattr(turn, name)
            assert value is not None and 0 <= value < 30, f"{name} = {value}"
        record_latency(server, turn, "cascaded")

        # llama.cpp reports token counts, and the speech engines' usage is counted by the adapters.
        kinds = {r.kind for r in turn.usage}
        assert {"llm", "stt", "tts"} <= kinds, turn.usage
        llm_usage = next(r for r in turn.usage if r.kind == "llm").usage
        assert llm_usage.prompt_tokens > 0 and llm_usage.completion_tokens > 0
        # Nobody reported a cost and no prices were given, so the cost is unknown, not zero.
        assert turn.to_dict()["cost_usd"] is None

        # Clients get both sides of the transcript, each attributed correctly.
        await eventually(lambda: any(t["identity"] == "user" for t in h.user.transcripts), message="the user's transcript")
        mine = next(t for t in h.user.transcripts if t["identity"] == "user")
        assert mine["generated"] is True and country.lower() in mine["text"].lower()
        await eventually(lambda: h.user.agent_transcripts(), message="the agent's transcript")
        spoken = h.user.agent_transcripts()[-1]
        assert capital in spoken["text"].lower() and spoken["final_attributes"].get("lk.transcription_final") == "true"

        # And the agent's state, which clients show.
        agent_view = next(p for p in h.user.room.remote_participants.values() if p.is_agent)
        await eventually(lambda: agent_view.attributes.get("lk.agent.state") == "listening", message="the state attribute")


async def test_user_interrupts_with_real_speech(server, engines):
    story = (
        "Once upon a time there was a small town by the sea. Every morning the fishing boats went out before dawn. "
        "The children would run down to the harbour to watch them go. In the evening the whole town came to see what they had caught. "
        "There was a lighthouse at the end of the long stone pier. Its keeper was an old woman who knew every boat by name. "
        "She told stories about the storms she had seen, and nobody ever tired of hearing them."
    )

    class Teller(Agent):
        async def on_enter(self):
            self.session.say(story)

    async with Harness(server, engines, lambda: Teller(instructions="You are a voice assistant. Answer in one short sentence.")) as h:
        await h.wait_for_reply_audio(timeout=20)
        loop = asyncio.get_running_loop()
        await asyncio.sleep(4)
        assert h.session.state == "speaking"

        said_at = loop.time()
        await h.user.say(await speak(engines, "Stop please. What is the capital of Italy?"))

        # The story stopped: what was stored is what was played, well short of all of it.
        await eventually(lambda: any(getattr(m, "interrupted", False) for m in h.session.history.messages()), timeout=15, message="the reply to be cut off")
        cut = next(m for m in h.session.history.messages() if m.interrupted)
        assert cut.role == "assistant"
        assert story.startswith(cut.content[:20])
        assert 5 < len(cut.content.split()) < len(story.split()) * 0.6, cut.content

        # Then it answers the new question.
        await eventually(lambda: any(m.role == "assistant" and "rome" in m.content.lower() for m in h.session.history.messages()), timeout=30, message="the new answer")
        await h.wait_for_quiet()

        heard_after = b"".join(f for at, f in h.heard.frames if at >= said_at)
        assert "rome" in (await transcribe(engines, heard_after)).lower()
        turns = h.session.metrics.turns
        assert turns[0].interrupted and turns[0].played_text == cut.content
        record_latency(server, turns[-1], "cascaded-after-interruption")


class _SilentSTT(STT):
    """Real audio goes in, and it reports hearing no words, which is how to hold a transcript back on purpose. This test is
    about what the session does then: the VAD and audio here are real."""

    provider = "test"
    streaming = False

    def __init__(self):
        super().__init__("nothing")

    async def _recognize(self, frame):
        return SpeechEvent(SpeechEventType.FINAL, text="")


async def test_false_interruption_resumes_playback(server, engines):
    story = "This sentence is here to keep the agent talking for a good while. " * 6
    resumed = asyncio.Event()

    class Teller(Agent):
        async def on_enter(self):
            self.session.say(story)
            self.session.on("agent_false_interruption", lambda: resumed.set())

    async with Harness(server, engines, lambda: Teller(instructions="Be brief."), stt=_SilentSTT()) as h:
        await h.wait_for_reply_audio(timeout=20)
        await asyncio.sleep(2)
        await h.user.say(await speak(engines, "Hold on, one moment please."))
        await asyncio.wait_for(resumed.wait(), 15)
        # A pause inside what the user said can make two short interruptions of it, so give the last one time to resume.
        await eventually(lambda: h.session.state == "speaking", timeout=10, message="playback to carry on")
        played = h.session.output.played_seconds
        await asyncio.sleep(1)
        assert h.session.output.played_seconds > played, "playback did not carry on"
        # It carries on and finishes, with the whole reply stored and nothing marked as interrupted.
        await eventually(lambda: h.session.current_speech is None, timeout=60, message="the story to end")
        # A closing session also ends the speech, and stores nothing. If the machine changes networks mid test, that is what happens.
        assert not h.session._closed, f"the session closed before the story ended: {h.ctx.shutdown_reason!r}"
        message = h.session.history.messages()[-1]
        assert not message.interrupted and len(message.content) > len(story) * 0.9


async def test_tool_call_with_a_real_llm(server, engines):
    calls = []

    @function_tool
    async def get_weather(ctx: RunContext, city: str) -> str:
        """Gets the current weather in a city.

        Args:
            city: The name of the city.
        """
        calls.append(city)
        return "It is sunny and 21 degrees."

    agent = lambda: Agent(
        instructions="You are a voice assistant. You must use the get_weather tool for any question about the weather. Then answer in one short sentence, and say the temperature.",
        tools=[get_weather],
    )
    async with Harness(server, engines, agent) as h:
        await h.user.say(await speak(engines, "What is the weather in Paris?"))
        await h.wait_for_reply_audio(timeout=40)
        await h.wait_for_quiet()
        assert [c.lower() for c in calls] == ["paris"], calls
        kinds = [type(i).__name__ for i in h.session.history]
        assert kinds.index("FunctionCall") < kinds.index("FunctionCallOutput") < len(kinds) - 1
        reply = h.session.history.messages()[-1].content
        assert "21" in reply or "twenty" in reply.lower(), reply
        assert h.session.metrics.turns[-1].tool_calls == ["get_weather"]


async def test_handoff_to_another_agent(server, engines):
    class Billing(Agent):
        def __init__(self):
            super().__init__(instructions="You are the billing agent.", name="billing")

        async def on_enter(self):
            self.session.say("Hello, this is the billing department.")

    @function_tool
    async def transfer_to_billing(ctx: RunContext) -> Agent:
        """Transfers the caller to the billing department. Call this whenever the caller mentions a bill."""
        return Billing()

    agent = lambda: Agent(
        instructions="You are a receptionist. If the caller mentions a bill, you must call the transfer_to_billing tool at once, and say nothing else.",
        tools=[transfer_to_billing],
        name="reception",
    )
    async with Harness(server, engines, agent) as h:
        identity = h.ctx.room.local_identity
        await h.user.say(await speak(engines, "I have a question about my bill."))
        await eventually(lambda: h.session.agent.name == "billing", timeout=30, message="the handoff")
        await h.wait_for_reply_audio(timeout=20)
        await h.wait_for_quiet()
        assert "billing" in (await transcribe(engines, h.heard.pcm)).lower()
        # The same participant carried on: there is one agent in the room.
        agents = [p for p in h.user.room.remote_participants.values() if p.is_agent]
        assert [p.identity for p in agents] == [identity]
        assert [(x["from"], x["to"]) for x in h.session.metrics.handoffs] == [("reception", "billing")]
        kinds = [type(i).__name__ for i in h.session.history]
        assert "AgentHandoff" in kinds


async def test_models_can_change_during_a_session(server, engines):
    agent = lambda: Agent(instructions="You are a voice assistant. Answer in one short sentence.")
    async with Harness(server, engines, agent) as h:
        await h.user.say(await speak(engines, "What is the capital of France?"))
        await h.wait_for_reply_audio()
        await h.wait_for_quiet()
        assert h.session.metrics.models["llm"] == f"local/{engines.llm_model}"

        h.session.update_models(llm=local_llm(engines, provider="swapped"))
        since = asyncio.get_running_loop().time()
        await h.user.say(await speak(engines, "What is the capital of Spain?"))
        await h.wait_for_reply_audio(since=since)
        await h.wait_for_quiet()
        assert h.session.metrics.models["llm"] == f"swapped/{engines.llm_model}"
        assert "madrid" in h.session.history.messages()[-1].content.lower()
        usage_models = [r.model for t in h.session.metrics.turns for r in t.usage if r.kind == "llm"]
        assert usage_models == [f"local/{engines.llm_model}", f"swapped/{engines.llm_model}"]


async def test_instructions_change_during_a_session(server, engines):
    seen: list[str] = []

    class Recording(Agent):
        def llm_node(self, ctx, tools, llm):
            # What reaches the LLM is what this test is about. A model this small follows instructions only sometimes.
            seen.append(next(m.content for m in ctx.messages() if m.role == "system"))
            return super().llm_node(ctx, tools, llm)

    async with Harness(server, engines, lambda: Recording(instructions="Answer in one short sentence.")) as h:
        await h.user.say(await speak(engines, "How are you today?"))
        await h.wait_for_reply_audio()
        await h.wait_for_quiet()
        await h.session.agent.update_instructions("Answer in one word.")
        since = asyncio.get_running_loop().time()
        await h.user.say(await speak(engines, "What is the capital of Spain?"))
        await h.wait_for_reply_audio(since=since)
        await h.wait_for_quiet()
        assert seen == ["Answer in one short sentence.", "Answer in one word."]


async def test_typed_chat_gets_a_spoken_reply(server, engines):
    agent = lambda: Agent(instructions="You are a voice assistant. Answer in one short sentence.")
    async with Harness(server, engines, agent) as h:
        writer = h.user.room.stream_text("lk.chat")
        await writer.write("What is the capital of Italy?")
        await writer.close()
        await h.wait_for_reply_audio()
        await h.wait_for_quiet()
        history = [(m.role, m.content) for m in h.session.history.messages()]
        assert history[0] == ("user", "What is the capital of Italy?")
        assert "rome" in history[1][1].lower()
        assert "rome" in (await transcribe(engines, h.heard.pcm)).lower()

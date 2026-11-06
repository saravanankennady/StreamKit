"""Realtime pipelines: one provider connection takes speech in and gives speech out.

Full duplex streams audio all the time, and the provider decides when turns start and end. Turn based keeps the framework's
VAD in charge: audio goes to the provider only during the user's turn, and a reply is requested when it ends.
"""

from __future__ import annotations

import asyncio
import logging
import time

from ..llm.base import APIError
from ..llm.chat import ChatMessage
from ..observability import TurnMetrics, UsageRecord
from ..realtime import Generation, RealtimeModel
from ..rtc.audio import AudioFrame
from ..tts import TTS
from ..vad import VAD, VADEvent, VADEventType
from .agent import Agent
from .session import AgentSession, Pipeline
from .speech import GrowingTranscript, SpeechHandle

logger = logging.getLogger("streamkit_agents.voice")

TRANSCRIPT_TICK = 0.1


class RealtimePipeline(Pipeline):
    def __init__(self, session: AgentSession, agent: Agent, model: RealtimeModel, *, duplex: bool):
        super().__init__(session, agent)
        self.model = model
        self.duplex = duplex
        self.name = "realtime-duplex" if duplex else "realtime-turn"
        self.input_sample_rate = model.input_sample_rate
        self.vad: VAD | None = agent.vad or session.vad
        if not duplex and self.vad is None:
            raise ValueError("turn based realtime needs a VAD to find where turns end")
        self._rt = None
        self._vad_stream = None
        self._tasks: list[asyncio.Task] = []
        self._sending = False
        self._commit_task: asyncio.Task | None = None
        self._turn: TurnMetrics | None = None
        self._generations: list[Generation] = []
        self._closed = False

    async def start(self) -> None:
        self._rt = self.model.session(instructions=self.agent.instructions, tools=self.agent.tools, server_turn_detection=self.duplex)
        self._rt.on("generation", self._on_generation)
        self._rt.on("speech_started", self._on_provider_speech_started)
        self._rt.on("speech_stopped", self._on_provider_speech_stopped)
        self._rt.on("input_transcribed", self._on_input_transcribed)
        self._rt.on("error", self._on_error)
        await self._rt.start()
        if not self.duplex:
            self._vad_stream = self.vad.stream()
            self._tasks.append(asyncio.create_task(self._vad_loop()))

    def push_audio(self, frame: AudioFrame) -> None:
        if self.duplex:
            self._rt.push_audio(frame)
            return
        self._vad_stream.push_frame(frame)
        if self._sending:
            self._rt.push_audio(frame)

    def commit_user_turn(self) -> None:
        if not self.duplex:
            self._start_commit(0.0)

    async def _vad_loop(self) -> None:
        async for event in self._vad_stream:
            try:
                self._on_vad(event)
            except Exception:
                logger.exception("handling a VAD event failed")

    def _on_vad(self, event: VADEvent) -> None:
        if event.type == VADEventType.START_OF_SPEECH:
            self._turn = self._turn or TurnMetrics(pipeline=self.name, speech_started_at=time.time() - event.speech_duration)
            if self._commit_task is not None:
                self._commit_task.cancel()
                self._commit_task = None
            self._sending = True
            if event.audio is not None and event.audio.data:
                # The audio just before the VAD was sure, so the first syllable is kept.
                self._rt.push_audio(event.audio)
        elif event.type == VADEventType.INFERENCE_DONE:
            if event.speaking and event.speech_duration >= self.session.min_interruption_duration and self.session.current_speech is not None:
                self.session._confirm_interruption()
        elif event.type == VADEventType.END_OF_SPEECH:
            if self._turn is not None:
                self._turn.speech_ended_at = time.time() - event.silence_duration
            self._start_commit(self.session.min_endpointing_delay)

    def _start_commit(self, delay: float) -> None:
        if self._commit_task is not None:
            self._commit_task.cancel()
        self._commit_task = asyncio.create_task(self._commit_after(delay))

    async def _commit_after(self, delay: float) -> None:
        await asyncio.sleep(delay)
        self._commit_task = None
        self._sending = False
        if self._turn is not None:
            self._turn.turn_committed_at = time.time()
        await self._rt.commit_audio()
        await self._rt.generate_reply()

    def _on_provider_speech_started(self) -> None:
        self._turn = self._turn or TurnMetrics(pipeline=self.name, speech_started_at=time.time())
        # The provider has already stopped generating, and what is queued here must stop too.
        self.session._confirm_interruption()

    def _on_provider_speech_stopped(self) -> None:
        if self._turn is None:
            self._turn = TurnMetrics(pipeline=self.name)
        self._turn.speech_ended_at = time.time()
        self._turn.turn_committed_at = self._turn.speech_ended_at

    def _on_input_transcribed(self, text: str, final: bool) -> None:
        if self._turn is not None:
            self._turn.transcript_at = time.time()
            self._turn.user_text = text
        elif self.session.metrics.turns:
            # Providers can report a transcript after the reply has started.
            self.session.metrics.turns[-1].user_text = text
        self.session.emit("user_input_transcribed", text, final)
        if final and text.strip():
            message = ChatMessage(role="user", content=text.strip())
            self.agent.chat_ctx.add(message)
            self.session._record(message)
            if self.session.io is not None:
                asyncio.ensure_future(self.session.io.publish_user_transcript(text.strip(), True, message.id))

    def _on_error(self, error: Exception) -> None:
        self.session.metrics.record_error("realtime", error)
        self.session.emit("error", "realtime", error)

    def _on_generation(self, generation: Generation) -> None:
        turn, self._turn = self._turn, None
        metrics = turn or TurnMetrics(pipeline=self.name)
        metrics.pipeline = self.name
        metrics.llm_started_at = metrics.llm_started_at or time.time()
        handle = self.session._new_handle(None)

        async def run() -> None:
            await self._forward(handle, generation, metrics)

        self.session._enqueue(handle, run, metrics)

    async def _forward(self, handle: SpeechHandle, generation: Generation, metrics: TurnMetrics) -> None:
        output = self.session.output
        self.session._set_state("thinking")
        start = output.played_seconds
        transcript = GrowingTranscript()
        writer = self.session.io.agent_transcript_writer() if self.session.io is not None else None
        heard_sent = 0

        def on_first_audio(at: float) -> None:
            metrics.playout_started_at = metrics.playout_started_at or at
            self.session._set_state("speaking")

        output.on_next_audio(on_first_audio)

        async def pump_audio() -> None:
            while True:
                frame = await generation.audio.get()
                if frame is None:
                    return
                if metrics.tts_first_audio_at is None:
                    metrics.tts_first_audio_at = time.time()
                    metrics.llm_first_token_at = metrics.tts_first_audio_at
                await output.capture_frame(frame)

        async def pump_text() -> None:
            while True:
                delta = await generation.text.get()
                if delta is None:
                    return
                transcript.add_text(delta)

        async def release_text() -> None:
            nonlocal heard_sent
            while writer is not None:
                await asyncio.sleep(TRANSCRIPT_TICK)
                transcript.set_audio(generation.audio_seconds, complete=generation.done.is_set())
                heard = transcript.text_at(output.played_seconds - start)
                if len(heard) > heard_sent:
                    await writer.write(heard[heard_sent:])
                    heard_sent = len(heard)

        audio_task = asyncio.create_task(pump_audio())
        words_task = asyncio.create_task(pump_text())

        async def both() -> None:
            await audio_task
            await words_task

        streams = asyncio.create_task(both())
        text_task = asyncio.create_task(release_text())
        stop_task = asyncio.create_task(handle._wait_interrupted())
        try:
            await asyncio.wait({streams, stop_task}, return_when=asyncio.FIRST_COMPLETED)
            if not handle.interrupted:
                playout = asyncio.create_task(output.wait_for_playout())
                await asyncio.wait({playout, stop_task}, return_when=asyncio.FIRST_COMPLETED)
                playout.cancel()
        finally:
            for task in (streams, audio_task, words_task, stop_task, text_task):
                task.cancel()
            output.on_next_audio(None)

        position = output.played_seconds - start
        transcript.set_audio(generation.audio_seconds, complete=generation.done.is_set())
        if handle.interrupted:
            output.clear_queue()
            output.resume()
            try:
                await self._rt.interrupt()
                if self._rt.capabilities.message_truncation and generation.item_id:
                    await self._rt.truncate(generation.item_id, int(position * 1000))
            except APIError as e:
                self.session.metrics.record_error("realtime", e)
        else:
            metrics.playout_finished_at = time.time()

        heard = transcript.text_at(position) if handle.interrupted else transcript.full_text
        metrics.reply_text = transcript.full_text
        metrics.played_text = heard
        if writer is not None:
            if len(heard) > heard_sent:
                await writer.write(heard[heard_sent:])
            await writer.close({"lk.transcription_final": "true"})
        if heard:
            message = ChatMessage(role="assistant", content=heard, interrupted=handle.interrupted)
            self.agent.chat_ctx.add(message)
            self.session._record(message)

        if not handle.interrupted:
            await generation.done.wait()
        if generation.usage is not None:
            metrics.usage.append(UsageRecord("realtime", self.model.label, generation.usage))
        if handle.interrupted or not generation.function_calls:
            return
        await self._run_tools(handle, generation, metrics)

    async def _run_tools(self, handle: SpeechHandle, generation: Generation, metrics: TurnMetrics) -> None:
        calls = generation.function_calls
        for call in calls:
            self.agent.chat_ctx.add(call)
            self.session._record(call)
        outputs, target, stop = await self.session.run_tools(calls, self.agent.tools, handle, agent=self.agent, metrics=metrics)
        for output, call in zip(outputs, calls):
            self.agent.chat_ctx.add(output)
            self.session._record(output)
            await self._rt.send_tool_result(call, output.output, is_error=output.is_error)
        if target is not None:
            await self.session.update_agent(target)
            return
        if not stop and not self._rt.capabilities.auto_tool_reply:
            await self._rt.generate_reply()

    async def run_reply(self, handle: SpeechHandle, *, user_input: str | None, instructions: str | None, metrics: TurnMetrics) -> None:
        # A generation from this request arrives as its own speech. This handle only starts it.
        text = instructions
        if user_input:
            message = ChatMessage(role="user", content=user_input)
            self.agent.chat_ctx.add(message)
            self.session._record(message)
            text = f"{instructions}\n\nThe user said: {user_input}" if instructions else f"The user said: {user_input}"
        await self._rt.generate_reply(text)

    async def run_say(self, handle: SpeechHandle, text: str, metrics: TurnMetrics) -> None:
        tts: TTS | None = self.agent.tts or self.session.tts
        if tts is not None:
            output = self.session.output
            async for frame in tts.synthesize(text):
                if handle.interrupted:
                    output.clear_queue()
                    return
                await output.capture_frame(frame)
            await output.wait_for_playout()
            return
        await self._rt.generate_reply(f"Say exactly this, and nothing else: {text}")

    async def update_instructions(self, instructions: str) -> None:
        await self._rt.update_instructions(instructions)

    async def update_tools(self, tools) -> None:
        if not self._rt.capabilities.mutable_tools:
            raise APIError(f"{self.model.label} fixes its tools when the session starts", retryable=False)
        await self._rt.update_tools(tools)

    async def aclose(self) -> None:
        self._closed = True
        if self._commit_task is not None:
            self._commit_task.cancel()
        if self._vad_stream is not None:
            self._vad_stream.end_input()
        for task in self._tasks:
            task.cancel()
        if self._rt is not None:
            await self._rt.aclose()

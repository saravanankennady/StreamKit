"""The cascaded pipeline: VAD and STT listen, the LLM answers, and the TTS speaks. Each is a separate provider."""

from __future__ import annotations

import asyncio
import logging
import time
import uuid
from typing import AsyncIterator

from ..llm.base import LLM, APIError
from ..llm.chat import ChatMessage
from ..observability import TurnMetrics, UsageRecord
from ..rtc.audio import AudioFrame, resample
from ..stt import STT, SpeechEvent, SpeechEventType
from ..tts import TTS, SynthesizedAudio
from ..vad import VAD, VADEvent, VADEventType
from .session import AgentSession, Pipeline
from .agent import Agent
from .speech import PlayoutTranscript, SpeechHandle

logger = logging.getLogger("streamkit_agents.voice")

TRANSCRIPT_TICK = 0.1


class CascadedPipeline(Pipeline):
    name = "cascaded"
    input_sample_rate = 16000

    def __init__(self, session: AgentSession, agent: Agent):
        super().__init__(session, agent)
        self.stt: STT | None = agent.stt or session.stt
        self.vad: VAD | None = agent.vad or session.vad
        if self.stt is not None and not self.stt.streaming and self.vad is None:
            raise ValueError(f"{self.stt.label} takes one utterance at a time, so it needs a VAD to cut them out")
        self._vad_stream = None
        self._stt_stream = None
        self._tasks: list[asyncio.Task] = []
        self._turn: TurnMetrics | None = None
        self._transcript: list[str] = []
        self._segment_id = uuid.uuid4().hex[:12]
        self._commit_task: asyncio.Task | None = None
        self._recognizing = 0
        self._recognized = asyncio.Event()
        self._recognized.set()
        self._transcript_arrived = asyncio.Event()
        self._closed = False

    @property
    def turn_detection(self) -> str:
        chosen = self.session.turn_detection
        if chosen:
            return chosen
        if self.vad is not None:
            return "vad"
        if self.stt is not None and self.stt.streaming:
            return "stt"
        return "manual"

    def _llm(self) -> LLM:
        llm = self.agent.llm or self.session.llm
        if llm is None:
            raise ValueError("no LLM is configured for this agent")
        self.session.metrics.models["llm"] = llm.label
        return llm

    def _tts(self) -> TTS:
        tts = self.agent.tts or self.session.tts
        if tts is None:
            raise ValueError("no TTS is configured for this agent")
        self.session.metrics.models["tts"] = tts.label
        return tts

    async def start(self) -> None:
        if self.stt is not None:
            self.session.metrics.models["stt"] = self.stt.label
        if self.vad is not None:
            self._vad_stream = self.vad.stream()
            self._tasks.append(asyncio.create_task(self._vad_loop()))
        if self.stt is not None and self.stt.streaming:
            self._stt_stream = self.stt.stream()
            self._tasks.append(asyncio.create_task(self._stt_loop()))

    def push_audio(self, frame: AudioFrame) -> None:
        if self._vad_stream is not None:
            self._vad_stream.push_frame(frame)
        if self._stt_stream is not None:
            speech = self.session.current_speech
            if speech is not None and not speech.allow_interruptions:
                # Speech that cannot be interrupted must not end up in the next turn's transcript.
                frame = AudioFrame(bytes(len(frame.data)), frame.sample_rate, frame.num_channels)
            self._stt_stream.push_frame(frame)

    def commit_user_turn(self) -> None:
        if self._stt_stream is not None:
            self._stt_stream.flush()
        self._start_commit(delay=0.0)

    def _new_turn(self) -> TurnMetrics:
        if self._turn is None:
            self._turn = TurnMetrics(pipeline=self.name)
            self._transcript = []
            self._segment_id = uuid.uuid4().hex[:12]
        return self._turn

    async def _vad_loop(self) -> None:
        async for event in self._vad_stream:
            try:
                self._on_vad(event)
            except Exception:
                logger.exception("handling a VAD event failed")

    def _on_vad(self, event: VADEvent) -> None:
        if event.type == VADEventType.START_OF_SPEECH:
            turn = self._new_turn()
            if turn.speech_started_at is None:
                turn.speech_started_at = time.time() - event.speech_duration
            self.session.emit("user_started_speaking")
            self._cancel_commit()
        elif event.type == VADEventType.INFERENCE_DONE:
            if event.speaking and event.speech_duration >= self.session.min_interruption_duration:
                self.session._pause_for_interruption()
        elif event.type == VADEventType.END_OF_SPEECH:
            turn = self._new_turn()
            turn.speech_ended_at = time.time() - event.silence_duration
            self.session.emit("user_stopped_speaking")
            self.session._user_stopped_speaking()
            speech = self.session.current_speech
            uninterruptible = speech is not None and not speech.allow_interruptions
            if self.stt is not None and not self.stt.streaming and not uninterruptible:
                self._recognizing += 1
                self._recognized.clear()
                task = asyncio.create_task(self._recognize(event.audio))
                self._tasks.append(task)
                task.add_done_callback(lambda t: self._tasks.remove(t) if t in self._tasks else None)
            if self.turn_detection == "vad":
                self._start_commit()

    async def _recognize(self, audio: AudioFrame | None) -> None:
        try:
            if audio is None or not audio.data:
                return
            frame = audio if audio.sample_rate == self.stt.sample_rate else resample(audio, self.stt.sample_rate)
            event = await self.stt.recognize(frame)
            if event.usage is not None and self._turn is not None:
                self._turn.usage.append(UsageRecord("stt", self.stt.label, event.usage))
            if event.text.strip():
                self._on_final_transcript(event.text.strip())
            elif self.session._paused_by_user:
                # Heard something and it held no words, so playback can come back right away.
                self.session._resume_after_false_interruption()
        except APIError as e:
            self.session.metrics.record_error("stt", e)
            self.session.emit("error", "stt", e)
        finally:
            self._recognizing -= 1
            if self._recognizing == 0:
                self._recognized.set()

    async def _stt_loop(self) -> None:
        async for event in self._stt_stream:
            try:
                self._on_stt(event)
            except Exception:
                logger.exception("handling an STT event failed")

    def _on_stt(self, event: SpeechEvent) -> None:
        if event.type == SpeechEventType.INTERIM:
            self._new_turn()
            if self.vad is None:
                # With no VAD, words arriving are the first sign the user is talking over the agent.
                self.session._pause_for_interruption()
            self.session.emit("user_input_transcribed", event.text, False)
            if self.session.io is not None:
                asyncio.ensure_future(self.session.io.publish_user_transcript(event.text, False, self._segment_id))
        elif event.type == SpeechEventType.FINAL:
            if event.text.strip():
                self._new_turn()
                self._on_final_transcript(event.text.strip())
        elif event.type == SpeechEventType.END_OF_SPEECH:
            turn = self._new_turn()
            if turn.speech_ended_at is None:
                turn.speech_ended_at = time.time()
            if self.turn_detection == "stt":
                self._start_commit(delay=0.0)
        elif event.type == SpeechEventType.USAGE and event.usage is not None:
            target = self._turn or (self.session.metrics.turns[-1] if self.session.metrics.turns else None)
            if target is not None:
                target.usage.append(UsageRecord("stt", self.stt.label, event.usage))

    def _on_final_transcript(self, text: str) -> None:
        turn = self._new_turn()
        turn.transcript_at = time.time()
        self._transcript.append(text)
        self._transcript_arrived.set()
        self.session.emit("user_input_transcribed", text, True)
        if self.session.io is not None:
            asyncio.ensure_future(self.session.io.publish_user_transcript(text, True, self._segment_id))
        words = sum(len(part.split()) for part in self._transcript)
        if words >= self.session.min_interruption_words:
            self.session._confirm_interruption()

    def _cancel_commit(self) -> None:
        if self._commit_task is not None:
            self._commit_task.cancel()
            self._commit_task = None

    def _start_commit(self, delay: float | None = None) -> None:
        self._cancel_commit()
        self._commit_task = asyncio.create_task(self._commit_after(delay))

    async def _commit_after(self, delay: float | None) -> None:
        wait = delay
        if wait is None:
            # The delay counts from the end of speech, and the VAD has already used some of it to be sure.
            ended = self._turn.speech_ended_at if self._turn is not None else None
            elapsed = time.time() - ended if ended is not None else 0.0
            wait = max(0.0, self.session.min_endpointing_delay - elapsed)
        await asyncio.sleep(wait)
        # A batch STT may still be working on the last utterance, so the words are not all in yet.
        await self._recognized.wait()
        if self._stt_stream is not None and not self._transcript and self._turn is not None:
            # A streaming STT has been told to finish, and its transcript is on the way. If nothing comes it heard no words.
            self._transcript_arrived.clear()
            self._stt_stream.flush()
            try:
                await asyncio.wait_for(self._transcript_arrived.wait(), self.session.max_endpointing_delay)
            except asyncio.TimeoutError:
                pass
        model = self.session.end_of_turn_model
        transcript = " ".join(self._transcript).strip()
        if model is not None and transcript and delay is None:
            probability = await model.probability(self.agent.chat_ctx, transcript)
            if probability < self.session.end_of_turn_threshold:
                await asyncio.sleep(max(0.0, self.session.max_endpointing_delay - wait))
                transcript = " ".join(self._transcript).strip()
        self._commit_task = None
        turn, self._turn = self._turn, None
        self._transcript = []
        if not transcript or turn is None:
            return
        turn.turn_committed_at = time.time()
        turn.user_text = transcript
        self.session._confirm_interruption()
        self.session.generate_reply(user_input=transcript, metrics=turn)

    async def run_reply(self, handle: SpeechHandle, *, user_input: str | None, instructions: str | None, metrics: TurnMetrics) -> None:
        agent = self.agent
        metrics.pipeline = self.name
        if user_input:
            message = ChatMessage(role="user", content=user_input)
            metrics.user_text = metrics.user_text or user_input
            turn_ctx = agent.chat_ctx.copy()
            await agent.on_user_turn_completed(turn_ctx, message)
            agent.chat_ctx.add(message)
            self.session._record(message)
        self.session._set_state("thinking")
        if self.session.io is not None:
            await self.session.io.wait_for_subscriber()
        one_off = instructions
        for step in range(self.session.max_tool_steps + 1):
            ctx = agent.chat_ctx.copy()
            ctx.set_instructions(agent.instructions)
            if one_off:
                ctx.add_message("system", one_off)
                one_off = None
            tools = agent.tools if step < self.session.max_tool_steps else []
            calls = await self._speak_llm_step(handle, ctx, tools, metrics)
            if handle.interrupted or not calls:
                return
            for call in calls:
                agent.chat_ctx.add(call)
                self.session._record(call)
            outputs, target, stop = await self.session.run_tools(calls, agent.tools, handle, agent=agent, metrics=metrics)
            for output in outputs:
                agent.chat_ctx.add(output)
                self.session._record(output)
            if handle.interrupted:
                return
            if target is not None:
                await self.session.update_agent(target)
                return
            if stop:
                return

    async def run_say(self, handle: SpeechHandle, text: str, metrics: TurnMetrics) -> None:
        metrics.pipeline = self.name
        if self.session.io is not None:
            await self.session.io.wait_for_subscriber()
        if text:
            transcript = await self._play(handle, _once(text), metrics)
            self._store_reply(handle, transcript, metrics)

    def _store_reply(self, handle: SpeechHandle, transcript: PlayoutTranscript, metrics: TurnMetrics) -> None:
        metrics.reply_text = transcript.full_text
        position = self._position
        heard = transcript.text_at(position) if handle.interrupted else transcript.full_text
        metrics.played_text = heard
        if heard:
            message = ChatMessage(role="assistant", content=heard, interrupted=handle.interrupted)
            self.agent.chat_ctx.add(message)
            self.session._record(message)

    async def _speak_llm_step(self, handle: SpeechHandle, ctx, tools, metrics: TurnMetrics) -> list:
        """One LLM call and the speech it produces. Returns its tool calls, if any."""
        llm = self._llm()
        text: asyncio.Queue[str | None] = asyncio.Queue()
        calls: list = []

        async def run_llm() -> None:
            metrics.llm_started_at = metrics.llm_started_at or time.time()
            try:
                async for chunk in self.agent.llm_node(ctx, tools, llm):
                    if chunk.text:
                        if metrics.llm_first_token_at is None:
                            metrics.llm_first_token_at = time.time()
                        text.put_nowait(chunk.text)
                    calls.extend(chunk.tool_calls)
                    if chunk.usage is not None:
                        metrics.usage.append(UsageRecord("llm", llm.label, chunk.usage))
                metrics.llm_finished_at = time.time()
            finally:
                text.put_nowait(None)

        async def text_stream() -> AsyncIterator[str]:
            while True:
                delta = await text.get()
                if delta is None:
                    return
                yield delta

        llm_task = asyncio.create_task(run_llm())
        try:
            transcript = await self._play(handle, text_stream(), metrics)
            self._store_reply(handle, transcript, metrics)
            if handle.interrupted:
                return []
            await llm_task
        except APIError as e:
            self.session.metrics.record_error("reply", e)
            self.session.emit("error", "reply", e)
            return []
        finally:
            llm_task.cancel()
        return calls

    async def _play(self, handle: SpeechHandle, text, metrics: TurnMetrics) -> PlayoutTranscript:
        """Speaks a stream of text, and returns once it has played or been interrupted."""
        output = self.session.output
        tts = self._tts()
        transcript = PlayoutTranscript()
        start = output.played_seconds
        self._position = 0.0
        writer = self.session.io.agent_transcript_writer() if self.session.io is not None else None
        heard_sent = 0
        spoke = asyncio.Event()

        def on_first_audio(at: float) -> None:
            metrics.playout_started_at = metrics.playout_started_at or at
            spoke.set()
            if not self.session._paused_by_user:
                self.session._set_state("speaking")

        output.on_next_audio(on_first_audio)

        # The TTS runs ahead of playout, into this queue. A TTS is usually faster than speech, so what each sentence's audio
        # adds up to is known well before it has played, which is what maps played audio back to words.
        audio_queue: asyncio.Queue[AudioFrame | None] = asyncio.Queue()

        async def synthesize() -> None:
            try:
                async for audio in self.agent.tts_node(text, tts):
                    audio: SynthesizedAudio
                    if metrics.tts_first_audio_at is None:
                        metrics.tts_first_audio_at = time.time()
                        # Measured from the first sentence, not from the start of the reply, so the LLM's time is not in it.
                        metrics.tts_started_at = tts.segment_started_at or metrics.tts_first_audio_at
                    if audio.segment_text is not None:
                        transcript.begin_segment(audio.segment_text)
                    transcript.add_audio(audio.frame.duration)
                    audio_queue.put_nowait(audio.frame)
                transcript.finish()
                if tts.last_usage is not None:
                    metrics.usage.append(UsageRecord("tts", tts.label, tts.last_usage))
            finally:
                audio_queue.put_nowait(None)

        async def feed() -> None:
            while True:
                frame = await audio_queue.get()
                if frame is None:
                    return
                await output.capture_frame(frame)

        async def release_text() -> None:
            nonlocal heard_sent
            while writer is not None:
                await asyncio.sleep(TRANSCRIPT_TICK)
                heard = transcript.text_at(output.played_seconds - start)
                if len(heard) > heard_sent:
                    await writer.write(heard[heard_sent:])
                    heard_sent = len(heard)

        synth_task = asyncio.create_task(synthesize())
        feed_task = asyncio.create_task(feed())
        text_task = asyncio.create_task(release_text())
        stop_task = asyncio.create_task(handle._wait_interrupted())
        try:
            done, _ = await asyncio.wait({feed_task, stop_task}, return_when=asyncio.FIRST_COMPLETED)
            if feed_task in done:
                # The feed ends when the TTS does, so this is where a failed TTS shows.
                synth_task.result()
                playout = asyncio.create_task(output.wait_for_playout())
                await asyncio.wait({playout, stop_task}, return_when=asyncio.FIRST_COMPLETED)
                playout.cancel()
        finally:
            for task in (synth_task, feed_task, stop_task, text_task):
                task.cancel()
            output.on_next_audio(None)

        self._position = output.played_seconds - start
        if handle.interrupted:
            output.clear_queue()
            output.resume()
        else:
            metrics.playout_finished_at = time.time()
        if writer is not None and spoke.is_set():
            heard = transcript.text_at(self._position) if handle.interrupted else transcript.full_text
            if len(heard) > heard_sent:
                await writer.write(heard[heard_sent:])
            await writer.close({"lk.transcription_final": "true"})
        return transcript

    async def aclose(self) -> None:
        self._closed = True
        self._cancel_commit()
        if self._vad_stream is not None:
            self._vad_stream.end_input()
        if self._stt_stream is not None:
            await self._stt_stream.aclose()
        for task in list(self._tasks):
            task.cancel()


async def _once(text: str) -> AsyncIterator[str]:
    yield text

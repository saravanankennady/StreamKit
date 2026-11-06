"""An AgentSession runs a conversation with one user: it holds the current agent, the queue of things to say, and the
interruption state. A Pipeline does the part that differs between cascaded and realtime models."""

from __future__ import annotations

import asyncio
import logging
import time
from abc import ABC, abstractmethod
from typing import Any, Literal

from ..llm.base import LLM
from ..llm.chat import AgentHandoff, ChatContext, ChatItem, FunctionCall, FunctionCallOutput
from ..llm.tools import FunctionTool, RunContext, StopResponse, ToolError
from ..observability import MetricsSink, Prices, SessionMetrics, TurnMetrics
from ..realtime import RealtimeModel
from ..rtc.audio import AudioFrame, AudioSource
from ..rtc.room import Emitter, Room
from ..stt import STT
from ..tts import TTS
from ..vad import VAD
from .agent import Agent
from .room_io import RoomIO
from .speech import SpeechHandle

logger = logging.getLogger("streamkit_agents.voice")

AgentState = Literal["initializing", "idle", "listening", "thinking", "speaking"]
# What decides that the user has finished. A realtime model in full duplex decides by itself.
TurnDetection = Literal["vad", "stt", "manual", "realtime"]


class EndOfTurnModel(ABC):
    """A model that predicts, from what was said, whether the user has finished. The framework ships none, and the delays
    below are enough for most uses. Note that LiveKit's turn detector is licensed for use only with its own framework, so
    it must not be plugged in here. See docs/architecture/agents.md."""

    @abstractmethod
    async def probability(self, ctx: ChatContext, transcript: str) -> float: ...


class Pipeline(ABC):
    name = ""
    input_sample_rate = 16000

    def __init__(self, session: "AgentSession", agent: Agent):
        self.session = session
        self.agent = agent

    @abstractmethod
    async def start(self) -> None: ...

    @abstractmethod
    def push_audio(self, frame: AudioFrame) -> None: ...

    @abstractmethod
    async def run_reply(self, handle: SpeechHandle, *, user_input: str | None, instructions: str | None, metrics: TurnMetrics) -> None: ...

    @abstractmethod
    async def run_say(self, handle: SpeechHandle, text: str, metrics: TurnMetrics) -> None: ...

    def commit_user_turn(self) -> None:
        """Ends the user's turn now, for manual turn detection."""

    async def update_instructions(self, instructions: str) -> None:
        return None

    async def update_tools(self, tools: list[FunctionTool]) -> None:
        return None

    @abstractmethod
    async def aclose(self) -> None: ...


class AgentSession(Emitter):
    """Events: agent_state_changed(old, new), user_input_transcribed(text, final), conversation_item_added(item),
    agent_false_interruption(), speech_created(handle), metrics_collected(turn), error(source, exception), close()."""

    def __init__(
        self,
        *,
        stt: STT | None = None,
        llm: LLM | None = None,
        tts: TTS | None = None,
        vad: VAD | None = None,
        realtime: RealtimeModel | None = None,
        duplex: bool = True,
        turn_detection: TurnDetection | None = None,
        end_of_turn_model: EndOfTurnModel | None = None,
        min_endpointing_delay: float = 0.5,
        max_endpointing_delay: float = 3.0,
        end_of_turn_threshold: float = 0.5,
        allow_interruptions: bool = True,
        min_interruption_duration: float = 0.5,
        min_interruption_words: int = 0,
        false_interruption_timeout: float = 2.0,
        resume_false_interruption: bool = True,
        max_tool_steps: int = 3,
        userdata: Any = None,
        metrics_sinks: list[MetricsSink] | None = None,
        prices: Prices | None = None,
    ):
        super().__init__()
        self.stt = stt
        self.llm = llm
        self.tts = tts
        self.vad = vad
        self.realtime = realtime
        self.duplex = duplex
        self.turn_detection = turn_detection
        self.end_of_turn_model = end_of_turn_model
        self.min_endpointing_delay = min_endpointing_delay
        self.max_endpointing_delay = max_endpointing_delay
        self.end_of_turn_threshold = end_of_turn_threshold
        self.allow_interruptions = allow_interruptions
        self.min_interruption_duration = min_interruption_duration
        self.min_interruption_words = min_interruption_words
        self.false_interruption_timeout = false_interruption_timeout
        self.resume_false_interruption = resume_false_interruption
        self.max_tool_steps = max_tool_steps
        self.userdata = userdata
        self.history = ChatContext()
        self.state: AgentState = "initializing"
        self.metrics = SessionMetrics(sinks=metrics_sinks, prices=prices)
        self.agent: Agent | None = None
        self.io: RoomIO | None = None
        self.output: AudioSource | None = None
        self._pipeline: Pipeline | None = None
        self._queue: asyncio.Queue[tuple[SpeechHandle, Any]] = asyncio.Queue()
        self._speech_task: asyncio.Task | None = None
        self.current_speech: SpeechHandle | None = None
        self._paused_by_user = False
        self._false_interruption_timer: asyncio.Task | None = None
        self._closed = False
        self._closed_event = asyncio.Event()
        self._state_tasks: set[asyncio.Task] = set()

    @property
    def input_sample_rate(self) -> int:
        return self._pipeline.input_sample_rate if self._pipeline else 16000

    async def start(self, agent: Agent, *, room: Room | None = None, participant_identity: str | None = None, output: AudioSource | None = None, close_on_disconnect: bool = True, job_id: str = "", agent_name: str = "") -> None:
        """Starts the session in a room, or against a given output for use without a room."""
        self.agent = agent
        agent.session = self
        if room is not None:
            self.io = RoomIO(self, room, participant_identity=participant_identity, close_on_disconnect=close_on_disconnect)
            self.output = self.io.source
            self.metrics.room = room.name
        else:
            self.output = output or AudioSource()
        self.metrics.job_id = job_id
        self.metrics.agent_name = agent_name
        self._pipeline = self._make_pipeline(agent)
        self.metrics.pipeline = self._pipeline.name
        await self._pipeline.start()
        if self.io is not None:
            await self.io.start()
        self._speech_task = asyncio.create_task(self._speech_loop())
        self._set_state("listening")
        await agent.on_enter()

    def _make_pipeline(self, agent: Agent) -> Pipeline:
        from .cascaded import CascadedPipeline
        from .realtime_pipeline import RealtimePipeline

        realtime = agent.realtime or self.realtime
        if realtime is not None:
            self.metrics.models["realtime"] = realtime.label
            return RealtimePipeline(self, agent, realtime, duplex=self.duplex)
        return CascadedPipeline(self, agent)

    def _push_audio(self, frame: AudioFrame) -> None:
        if self._pipeline is not None and not self._closed:
            self._pipeline.push_audio(frame)

    def _set_state(self, state: AgentState) -> None:
        if state == self.state:
            return
        old, self.state = self.state, state
        self.emit("agent_state_changed", old, state)
        if self.io is not None:
            task = asyncio.ensure_future(self.io.set_state(state))
            self._state_tasks.add(task)
            task.add_done_callback(self._state_tasks.discard)

    def _record(self, item: ChatItem) -> None:
        """Adds to the session's full record. Each agent keeps its own context on top of this."""
        self.history.add(item)
        self.emit("conversation_item_added", item)

    def say(self, text: str, *, allow_interruptions: bool | None = None, add_to_chat_ctx: bool = True) -> SpeechHandle:
        handle = self._new_handle(allow_interruptions)
        metrics = TurnMetrics(pipeline=self.metrics.pipeline)

        async def run() -> None:
            await self._pipeline.run_say(handle, text, metrics)

        self._enqueue(handle, run, metrics)
        return handle

    def generate_reply(self, *, user_input: str | None = None, instructions: str | None = None, allow_interruptions: bool | None = None, metrics: TurnMetrics | None = None) -> SpeechHandle:
        handle = self._new_handle(allow_interruptions)
        turn = metrics or TurnMetrics(pipeline=self.metrics.pipeline)

        async def run() -> None:
            await self._pipeline.run_reply(handle, user_input=user_input, instructions=instructions, metrics=turn)

        self._enqueue(handle, run, turn)
        return handle

    def _new_handle(self, allow_interruptions: bool | None) -> SpeechHandle:
        allowed = self.allow_interruptions if allow_interruptions is None else allow_interruptions
        handle = SpeechHandle(allow_interruptions=allowed)
        self.emit("speech_created", handle)
        return handle

    def _enqueue(self, handle: SpeechHandle, run: Any, metrics: TurnMetrics) -> None:
        self._queue.put_nowait((handle, (run, metrics)))

    async def _speech_loop(self) -> None:
        while not self._closed:
            handle, (run, metrics) = await self._queue.get()
            if handle.interrupted:
                handle._finish()
                continue
            self.current_speech = handle
            try:
                await run()
            except asyncio.CancelledError:
                raise
            except StopResponse:
                pass
            except Exception as e:
                logger.exception("a reply failed")
                self.metrics.record_error("reply", e)
                self.emit("error", "reply", e)
            finally:
                handle._finish()
                self.current_speech = None
                self._cancel_false_interruption()
                self._paused_by_user = False
            metrics.interrupted = handle.interrupted
            # A realtime reply is requested by one item and spoken by another. The request has nothing to report.
            if metrics.llm_started_at or metrics.tts_started_at or metrics.playout_started_at:
                await self.metrics.turn_finished(metrics)
                self.emit("metrics_collected", metrics)
            if self._queue.empty() and not self._closed:
                self._set_state("listening")

    def interrupt(self) -> None:
        """Stops whatever the agent is saying."""
        if self.current_speech is not None:
            self.current_speech.interrupt()
        # Anything still queued goes too: it was a reply to the conversation as it stood before.
        pending = []
        while not self._queue.empty():
            pending.append(self._queue.get_nowait())
        for handle, job in pending:
            if handle.allow_interruptions:
                handle.interrupt()
                handle._finish()
            else:
                self._queue.put_nowait((handle, job))

    def commit_user_turn(self) -> None:
        if self._pipeline is not None:
            self._pipeline.commit_user_turn()

    # Interruptions come in two steps. Speech over the agent first pauses playback, which is cheap to undo. A transcript
    # or a committed turn then confirms it. If neither comes, it was noise, and playback resumes where it stopped.

    def _pause_for_interruption(self) -> None:
        speech = self.current_speech
        if speech is None or not speech.allow_interruptions or self._paused_by_user:
            return
        self._paused_by_user = True
        self._cancel_false_interruption()
        if self.output is not None:
            self.output.pause()
        self._set_state("listening")

    def _user_stopped_speaking(self) -> None:
        if self._paused_by_user and self.resume_false_interruption and self.false_interruption_timeout > 0:
            self._cancel_false_interruption()
            self._false_interruption_timer = asyncio.create_task(self._false_interruption_wait())

    async def _false_interruption_wait(self) -> None:
        await asyncio.sleep(self.false_interruption_timeout)
        self._resume_after_false_interruption()

    def _resume_after_false_interruption(self) -> None:
        self._false_interruption_timer = None
        if not self._paused_by_user:
            return
        self._paused_by_user = False
        if self.output is not None:
            self.output.resume()
        if self.current_speech is not None:
            self._set_state("speaking")
        self.emit("agent_false_interruption")

    def _cancel_false_interruption(self) -> None:
        timer = self._false_interruption_timer
        self._false_interruption_timer = None
        if timer is not None and timer is not asyncio.current_task():
            timer.cancel()

    def _confirm_interruption(self) -> None:
        """Something the user really said has arrived, so what the agent is saying stops for good."""
        self._cancel_false_interruption()
        self._paused_by_user = False
        self.interrupt()

    async def run_tools(self, calls: list[FunctionCall], tools: list[FunctionTool], handle: SpeechHandle, *, agent: Agent, metrics: TurnMetrics) -> tuple[list[FunctionCallOutput], Agent | None, bool]:
        """Runs a step's tool calls at once. Returns their outputs, the agent to hand off to if any, and whether a tool
        asked for no spoken reply."""
        by_name = {t.name: t for t in tools}
        handoff: dict[str, Agent] = {}
        stop = False

        async def run_one(call: FunctionCall) -> FunctionCallOutput:
            nonlocal stop
            metrics.tool_calls.append(call.name)
            tool = by_name.get(call.name)
            if tool is None:
                return FunctionCallOutput(call.call_id, call.name, f"there is no tool called {call.name}", is_error=True)
            try:
                result = await tool.call(RunContext(session=self, speech_handle=handle, function_call=call), call.arguments)
            except ToolError as e:
                return FunctionCallOutput(call.call_id, call.name, str(e), is_error=True)
            except StopResponse:
                stop = True
                return FunctionCallOutput(call.call_id, call.name, "", is_error=False)
            except Exception as e:
                logger.exception("tool %s failed", call.name)
                self.metrics.record_error(f"tool:{call.name}", e)
                # The details stay in the log. They may hold things the model should not repeat aloud.
                return FunctionCallOutput(call.call_id, call.name, "the tool failed", is_error=True)
            if isinstance(result, Agent):
                handoff[call.call_id] = result
                return FunctionCallOutput(call.call_id, call.name, f"transferred to {result.name}")
            if isinstance(result, tuple) and len(result) == 2 and isinstance(result[0], Agent):
                handoff[call.call_id] = result[0]
                return FunctionCallOutput(call.call_id, call.name, str(result[1]))
            return FunctionCallOutput(call.call_id, call.name, "" if result is None else str(result))

        outputs = list(await asyncio.gather(*(run_one(c) for c in calls)))
        target: Agent | None = None
        if len(handoff) > 1:
            # Two agents cannot both take over, and picking one would be a guess.
            for output in outputs:
                if output.call_id in handoff:
                    output.output = "only one handoff can happen at a time, so this one did not"
                    output.is_error = True
        elif handoff:
            target = next(iter(handoff.values()))
        return outputs, target, stop

    async def update_agent(self, agent: Agent) -> None:
        """Hands the conversation to another agent. Its first reply comes from its on_enter, or the user's next turn."""
        old = self.agent
        if old is not None:
            await old.on_exit()
            record = AgentHandoff(old_agent=old.name, new_agent=agent.name)
            self._record(record)
            self.metrics.handoffs.append({"at": time.time(), "from": old.name, "to": agent.name})
        previous = self._pipeline
        self.agent = agent
        agent.session = self
        self._pipeline = self._make_pipeline(agent)
        self.metrics.pipeline = self._pipeline.name
        await self._pipeline.start()
        if previous is not None:
            await previous.aclose()
        await agent.on_enter()

    async def _pipeline_update_instructions(self, instructions: str) -> None:
        if self._pipeline is not None:
            await self._pipeline.update_instructions(instructions)

    async def _pipeline_update_tools(self, tools: list[FunctionTool]) -> None:
        if self._pipeline is not None:
            await self._pipeline.update_tools(tools)

    def update_models(self, *, llm: LLM | None = None, stt: STT | None = None, tts: TTS | None = None) -> None:
        """Changes models from the next reply on, without ending the session."""
        if llm is not None:
            self.llm = llm
            if self.agent is not None:
                self.agent.llm = None
        if stt is not None:
            self.stt = stt
            if self.agent is not None:
                self.agent.stt = None
        if tts is not None:
            self.tts = tts
            if self.agent is not None:
                self.agent.tts = None

    async def aclose(self) -> None:
        if self._closed:
            return
        self._closed = True
        self.interrupt()
        self._cancel_false_interruption()
        if self._speech_task is not None:
            self._speech_task.cancel()
        if self.agent is not None:
            try:
                await self.agent.on_exit()
            except Exception:
                logger.exception("on_exit failed")
        if self._pipeline is not None:
            await self._pipeline.aclose()
        await self.metrics.session_ended()
        self.emit("close")
        self._closed_event.set()

    async def wait_closed(self) -> None:
        await self._closed_event.wait()


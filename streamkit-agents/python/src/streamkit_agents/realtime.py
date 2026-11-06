"""The interface for models that take speech in and give speech out over one connection.

A realtime model replaces the VAD, STT, LLM and TTS. It runs in one of two modes:

- Full duplex: audio flows continuously, and the provider decides when turns start and end.
- Turn based: the framework's own VAD decides. Audio is committed at the end of a turn and a reply is requested.
"""

from __future__ import annotations

import asyncio
from abc import ABC, abstractmethod
from dataclasses import dataclass

from .llm.base import Usage
from .llm.chat import ChatContext, FunctionCall
from .llm.tools import FunctionTool
from .rtc.audio import AudioFrame
from .rtc.room import Emitter


@dataclass
class RealtimeCapabilities:
    # Whether the provider can be told how much of a reply was heard.
    message_truncation: bool = False
    # Whether the provider starts a reply by itself once given a tool result.
    auto_tool_reply: bool = False
    user_transcription: bool = True
    # Whether tools can change once the session has started.
    mutable_tools: bool = True


class Generation:
    """One reply. Audio and text arrive at once, and either stream can finish first."""

    def __init__(self, response_id: str):
        self.response_id = response_id
        self.item_id = ""
        self.audio: asyncio.Queue[AudioFrame | None] = asyncio.Queue()
        self.text: asyncio.Queue[str | None] = asyncio.Queue()
        self.function_calls: list[FunctionCall] = []
        self.usage: Usage | None = None
        self.done = asyncio.Event()
        self.interrupted = False
        self.audio_seconds = 0.0

    def push_audio(self, frame: AudioFrame) -> None:
        """Adapters call this as audio arrives. It usually arrives faster than it plays."""
        self.audio_seconds += frame.duration
        self.audio.put_nowait(frame)

    def _finish(self) -> None:
        self.audio.put_nowait(None)
        self.text.put_nowait(None)
        self.done.set()


class RealtimeSession(Emitter, ABC):
    """Events: session_ready, speech_started, speech_stopped, input_transcribed(text, final), generation(Generation),
    error(Exception), closed."""

    def __init__(self, capabilities: RealtimeCapabilities):
        super().__init__()
        self.capabilities = capabilities

    @abstractmethod
    async def start(self) -> None:
        """Connects and configures the session."""

    @abstractmethod
    def push_audio(self, frame: AudioFrame) -> None: ...

    @abstractmethod
    async def commit_audio(self) -> None:
        """Ends the user's turn, in turn based mode."""

    @abstractmethod
    async def clear_audio(self) -> None: ...

    @abstractmethod
    async def generate_reply(self, instructions: str | None = None) -> None: ...

    @abstractmethod
    async def interrupt(self) -> None:
        """Stops the reply in progress."""

    async def truncate(self, item_id: str, audio_end_ms: int) -> None:
        """Says how much of a reply was heard, when the provider supports it."""

    @abstractmethod
    async def send_tool_result(self, call: FunctionCall, output: str, *, is_error: bool = False) -> None: ...

    @abstractmethod
    async def update_instructions(self, instructions: str) -> None: ...

    async def update_tools(self, tools: list[FunctionTool]) -> None:
        raise NotImplementedError

    async def seed_history(self, ctx: ChatContext) -> None:
        """Gives the provider earlier conversation, such as after a handoff."""

    @abstractmethod
    async def aclose(self) -> None: ...


class RealtimeModel(ABC):
    provider = "unknown"
    input_sample_rate = 24000
    output_sample_rate = 24000

    def __init__(self, model: str):
        self.model = model

    @property
    def label(self) -> str:
        return f"{self.provider}/{self.model}"

    @abstractmethod
    def session(self, *, instructions: str, tools: list[FunctionTool], server_turn_detection: bool) -> RealtimeSession:
        """server_turn_detection selects full duplex when true, and turn based when false."""


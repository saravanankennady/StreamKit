"""An Agent is a role in the conversation: its instructions, tools and models. A handoff moves to another Agent, in the same
session, in the same room, with the same participant identity."""

from __future__ import annotations

from typing import TYPE_CHECKING, AsyncIterable, AsyncIterator

from ..llm.base import LLM, ChatChunk
from ..llm.chat import ChatContext, ChatMessage
from ..llm.tools import FunctionTool
from ..realtime import RealtimeModel
from ..stt import STT
from ..tts import TTS, SynthesizedAudio
from ..vad import VAD

if TYPE_CHECKING:
    from .session import AgentSession


class Agent:
    def __init__(
        self,
        *,
        instructions: str,
        tools: list[FunctionTool] | None = None,
        chat_ctx: ChatContext | None = None,
        name: str | None = None,
        stt: STT | None = None,
        llm: LLM | None = None,
        tts: TTS | None = None,
        vad: VAD | None = None,
        realtime: RealtimeModel | None = None,
    ):
        self.instructions = instructions
        self.tools = list(tools or [])
        self.chat_ctx = chat_ctx.copy() if chat_ctx else ChatContext()
        self.name = name or type(self).__name__
        self.stt = stt
        self.llm = llm
        self.tts = tts
        self.vad = vad
        self.realtime = realtime
        self.session: AgentSession | None = None

    async def on_enter(self) -> None:
        """Runs when this agent takes over the conversation."""

    async def on_exit(self) -> None:
        """Runs when this agent hands the conversation on, or the session ends."""

    async def on_user_turn_completed(self, turn_ctx: ChatContext, new_message: ChatMessage) -> None:
        """Runs before the LLM answers a turn. It can edit a copy of the context, change the message, or raise StopResponse
        to end the turn with no reply."""

    def llm_node(self, ctx: ChatContext, tools: list[FunctionTool], llm: LLM) -> AsyncIterator[ChatChunk]:
        """Override to change what reaches or comes from the LLM."""
        return llm.chat(ctx, tools=tools)

    def tts_node(self, text: AsyncIterable[str], tts: TTS) -> AsyncIterator[SynthesizedAudio]:
        """Override to change what reaches the TTS or the audio it makes."""
        return tts.stream(text)

    async def update_instructions(self, instructions: str) -> None:
        """Takes effect from the next reply, or at once on a realtime model."""
        self.instructions = instructions
        if self.session is not None:
            await self.session._pipeline_update_instructions(instructions)

    async def update_tools(self, tools: list[FunctionTool]) -> None:
        self.tools = list(tools)
        if self.session is not None:
            await self.session._pipeline_update_tools(self.tools)

    def __repr__(self) -> str:
        return f"<Agent {self.name}>"


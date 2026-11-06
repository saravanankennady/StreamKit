"""The LLM interface, and what all adapters share: errors and when a failure may be retried."""

from __future__ import annotations

import asyncio
import logging
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Literal

from .chat import ChatContext, FunctionCall
from .tools import FunctionTool

logger = logging.getLogger("streamkit_agents.llm")


class APIError(Exception):
    """A provider call failed. The message must never contain a credential."""

    def __init__(self, message: str, *, status: int | None = None, retryable: bool = True):
        super().__init__(message)
        self.status = status
        self.retryable = retryable


def error_for_status(provider: str, status: int, body: str = "") -> APIError:
    # 408 and 409 can succeed on a retry, 429 and 5xx usually do, and other client errors never will.
    retryable = status in (408, 409, 429) or status >= 500
    detail = body[:300].replace("\n", " ")
    return APIError(f"{provider} returned {status}: {detail}", status=status, retryable=retryable)


@dataclass
class RetryPolicy:
    max_retries: int = 2
    interval: float = 0.5
    connect_timeout: float = 10.0
    read_timeout: float = 60.0


@dataclass
class Usage:
    """What a provider reported for one call. Fields the provider does not report stay at zero or None."""

    prompt_tokens: int = 0
    completion_tokens: int = 0
    cached_tokens: int = 0
    audio_input_tokens: int = 0
    audio_output_tokens: int = 0
    audio_seconds: float = 0.0
    characters: int = 0
    # Only set when the provider itself reported a cost.
    reported_cost: float | None = None
    raw: dict[str, Any] = field(default_factory=dict)


@dataclass
class ChatChunk:
    text: str = ""
    tool_calls: list[FunctionCall] = field(default_factory=list)
    usage: Usage | None = None
    finish_reason: str | None = None


ToolChoice = Literal["auto", "none", "required"] | str | None


class LLM(ABC):
    provider = "unknown"

    def __init__(self, model: str, *, retry: RetryPolicy | None = None):
        self.model = model
        self.retry = retry or RetryPolicy()

    def chat(
        self,
        ctx: ChatContext,
        *,
        tools: list[FunctionTool] | None = None,
        tool_choice: ToolChoice = None,
        temperature: float | None = None,
        max_tokens: int | None = None,
    ) -> AsyncIterator[ChatChunk]:
        return self._with_retries(ctx, tools or [], tool_choice, temperature, max_tokens)

    async def _with_retries(self, ctx, tools, tool_choice, temperature, max_tokens) -> AsyncIterator[ChatChunk]:
        attempt = 0
        while True:
            sent_output = False
            try:
                async for chunk in self._stream(ctx, tools, tool_choice, temperature, max_tokens):
                    sent_output = sent_output or bool(chunk.text or chunk.tool_calls)
                    yield chunk
                return
            except APIError as e:
                # Once words have gone out, a retry would make the agent say them twice.
                if sent_output or not e.retryable or attempt >= self.retry.max_retries:
                    raise
                attempt += 1
                logger.warning("%s failed, retrying (%d): %s", self.provider, attempt, e)
                await asyncio.sleep(self.retry.interval * attempt)

    @abstractmethod
    def _stream(self, ctx, tools, tool_choice, temperature, max_tokens) -> AsyncIterator[ChatChunk]: ...

    async def aclose(self) -> None:
        return None

    @property
    def label(self) -> str:
        return f"{self.provider}/{self.model}"


class FallbackLLM(LLM):
    """Tries each LLM in turn until one starts answering. Failing after output is an error: nothing is said twice."""

    provider = "fallback"

    def __init__(self, llms: list[LLM]):
        if not llms:
            raise ValueError("no LLMs given")
        super().__init__(",".join(x.label for x in llms), retry=RetryPolicy(max_retries=0))
        self.llms = llms

    async def _stream(self, ctx, tools, tool_choice, temperature, max_tokens) -> AsyncIterator[ChatChunk]:
        last: APIError | None = None
        for llm in self.llms:
            sent = False
            try:
                async for chunk in llm.chat(ctx, tools=tools, tool_choice=tool_choice, temperature=temperature, max_tokens=max_tokens):
                    sent = sent or bool(chunk.text or chunk.tool_calls)
                    yield chunk
                return
            except APIError as e:
                if sent:
                    raise
                logger.warning("%s failed, trying the next LLM: %s", llm.label, e)
                last = e
        raise APIError(f"all LLMs failed, the last with: {last}", retryable=False)

    async def aclose(self) -> None:
        for llm in self.llms:
            await llm.aclose()

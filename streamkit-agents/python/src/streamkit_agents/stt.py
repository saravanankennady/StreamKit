"""The speech-to-text interface."""

from __future__ import annotations

import asyncio
import enum
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import AsyncIterator

from .llm.base import APIError, RetryPolicy, Usage
from .rtc.audio import AudioFrame


class SpeechEventType(str, enum.Enum):
    START_OF_SPEECH = "start_of_speech"
    INTERIM = "interim"
    FINAL = "final"
    # The provider decided the user finished speaking, for providers that detect this.
    END_OF_SPEECH = "end_of_speech"
    USAGE = "usage"


@dataclass
class SpeechEvent:
    type: SpeechEventType
    text: str = ""
    language: str = ""
    confidence: float | None = None
    usage: Usage | None = None


class STT(ABC):
    provider = "unknown"
    # A streaming STT takes all audio as it comes. Any other gets one utterance at a time, cut out by the VAD.
    streaming = False
    sample_rate = 16000

    def __init__(self, model: str, *, language: str | None = None, retry: RetryPolicy | None = None):
        self.model = model
        self.language = language
        self.retry = retry or RetryPolicy()

    @property
    def label(self) -> str:
        return f"{self.provider}/{self.model}"

    async def recognize(self, frame: AudioFrame) -> SpeechEvent:
        """Transcribes one utterance, already at this STT's sample rate."""
        attempt = 0
        while True:
            try:
                return await self._recognize(frame)
            except APIError as e:
                if not e.retryable or attempt >= self.retry.max_retries:
                    raise
                attempt += 1
                await asyncio.sleep(self.retry.interval * attempt)

    @abstractmethod
    async def _recognize(self, frame: AudioFrame) -> SpeechEvent: ...

    def stream(self) -> "RecognizeStream":
        raise NotImplementedError(f"{self.label} does not stream")

    async def aclose(self) -> None:
        return None


class RecognizeStream(ABC):
    """Audio goes in with push_frame, and events come out by iterating."""

    def __init__(self) -> None:
        self._events: asyncio.Queue[SpeechEvent | None] = asyncio.Queue()

    @abstractmethod
    def push_frame(self, frame: AudioFrame) -> None: ...

    def flush(self) -> None:
        """Asks the provider to finish what it has heard so far."""

    def end_input(self) -> None:
        """No more audio will come."""

    @abstractmethod
    async def aclose(self) -> None: ...

    def __aiter__(self) -> AsyncIterator[SpeechEvent]:
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[SpeechEvent]:
        while True:
            event = await self._events.get()
            if event is None:
                return
            yield event

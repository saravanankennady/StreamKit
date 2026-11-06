"""The text-to-speech interface, and turning a stream of LLM text into a stream of audio."""

from __future__ import annotations

import asyncio
import re
import time
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import AsyncIterable, AsyncIterator

from .llm.base import RetryPolicy, Usage
from .rtc.audio import AudioFrame


@dataclass
class SynthesizedAudio:
    frame: AudioFrame
    # Set on the first audio of each piece of text, so played audio can be traced back to the words it holds.
    segment_text: str | None = None


class TTS(ABC):
    provider = "unknown"
    sample_rate = 24000

    def __init__(self, model: str, *, voice: str | None = None, retry: RetryPolicy | None = None):
        self.model = model
        self.voice = voice
        self.retry = retry or RetryPolicy()
        self.last_usage: Usage | None = None
        # When the sentence now being spoken went to the provider, which is what its speed is measured from.
        self.segment_started_at: float | None = None

    @property
    def label(self) -> str:
        return f"{self.provider}/{self.model}"

    @abstractmethod
    def synthesize(self, text: str) -> AsyncIterator[AudioFrame]:
        """Speaks one piece of text, yielding audio as it is produced."""

    async def stream(self, text: AsyncIterable[str]) -> AsyncIterator[SynthesizedAudio]:
        """Speaks text as it arrives, one sentence at a time. The next sentence starts as soon as it is complete, so
        the first words are heard long before the LLM has finished."""
        tokenizer = SentenceTokenizer()
        pending: asyncio.Queue[str | None] = asyncio.Queue()

        async def split() -> None:
            try:
                async for delta in text:
                    for sentence in tokenizer.push(delta):
                        pending.put_nowait(sentence)
                tail = tokenizer.flush()
                if tail:
                    pending.put_nowait(tail)
            finally:
                pending.put_nowait(None)

        splitter = asyncio.create_task(split())
        try:
            while True:
                sentence = await pending.get()
                if sentence is None:
                    break
                first = True
                self.segment_started_at = time.time()
                async for frame in self.synthesize(sentence):
                    yield SynthesizedAudio(frame, sentence if first else None)
                    first = False
            await splitter
        finally:
            splitter.cancel()

    async def aclose(self) -> None:
        return None


# Words whose full stop does not end a sentence.
_ABBREVIATIONS = {"mr", "mrs", "ms", "dr", "prof", "sr", "jr", "st", "vs", "etc", "e.g", "i.e", "no", "inc", "ltd", "fig"}
_SENTENCE_END = re.compile(r"""[.!?]+["')\]]*(\s+|$)""")
_CLAUSE_END = re.compile(r"[,;:]\s+")


class SentenceTokenizer:
    """Splits streaming text into sentences. Very short fragments wait for more, because speech engines sound poor on
    them, and very long sentences break at a clause, so they do not hold up the first audio for long."""

    def __init__(self, *, min_chars: int = 16, max_chars: int = 180):
        self._buffer = ""
        self._min = min_chars
        self._max = max_chars

    def push(self, text: str) -> list[str]:
        self._buffer += text
        out: list[str] = []
        search_from = 0
        while True:
            match = _SENTENCE_END.search(self._buffer, search_from)
            if match is None:
                break
            # A match at the very end could still grow: "3." may become "3.5".
            if match.end() == len(self._buffer) and not match.group(1):
                break
            sentence = self._buffer[: match.end()].strip()
            if len(sentence) < self._min or self._is_abbreviation(sentence):
                search_from = match.end()
                continue
            out.append(sentence)
            self._buffer = self._buffer[match.end():]
            search_from = 0
        while len(self._buffer) > self._max:
            clause = None
            for m in _CLAUSE_END.finditer(self._buffer, 0, self._max):
                clause = m
            cut = clause.end() if clause else self._buffer.rfind(" ", 0, self._max) + 1
            if cut <= 0:
                break
            out.append(self._buffer[:cut].strip())
            self._buffer = self._buffer[cut:]
        return out

    def flush(self) -> str:
        tail, self._buffer = self._buffer.strip(), ""
        return tail

    @staticmethod
    def _is_abbreviation(sentence: str) -> bool:
        last = sentence.rstrip(".!?\"')]").rsplit(None, 1)[-1].lower() if sentence.strip() else ""
        if sentence.rstrip("\"')]").endswith(("!", "?")):
            return False
        return last in _ABBREVIATIONS or (len(last) == 1 and last.isalpha())


class PCMFramer:
    """Cuts a byte stream of 16-bit PCM into frames. Network chunks can end in the middle of a sample."""

    def __init__(self, sample_rate: int, *, frame_ms: int = 50, num_channels: int = 1):
        self.sample_rate = sample_rate
        self.num_channels = num_channels
        self._size = sample_rate * frame_ms // 1000 * 2 * num_channels
        self._buffer = bytearray()

    def push(self, data: bytes) -> list[AudioFrame]:
        self._buffer += data
        out = []
        while len(self._buffer) >= self._size:
            out.append(AudioFrame(bytes(self._buffer[: self._size]), self.sample_rate, self.num_channels))
            del self._buffer[: self._size]
        return out

    def flush(self) -> AudioFrame | None:
        usable = len(self._buffer) - len(self._buffer) % (2 * self.num_channels)
        if usable == 0:
            return None
        frame = AudioFrame(bytes(self._buffer[:usable]), self.sample_rate, self.num_channels)
        self._buffer.clear()
        return frame

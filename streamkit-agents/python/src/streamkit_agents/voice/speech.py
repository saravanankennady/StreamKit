"""A SpeechHandle is one thing the agent is going to say. A PlayoutTranscript maps the audio already played back to words."""

from __future__ import annotations

import asyncio
import uuid
from dataclasses import dataclass


class SpeechHandle:
    def __init__(self, *, allow_interruptions: bool = True):
        self.id = uuid.uuid4().hex[:12]
        self.allow_interruptions = allow_interruptions
        self.interrupted = False
        self._interrupt = asyncio.Event()
        self._done = asyncio.Event()

    def interrupt(self) -> None:
        """Stops the speech, unless it was marked as not interruptible."""
        if self.allow_interruptions and not self._done.is_set():
            self.interrupted = True
            self._interrupt.set()

    def done(self) -> bool:
        return self._done.is_set()

    async def wait_for_playout(self) -> None:
        await self._done.wait()

    async def _wait_interrupted(self) -> None:
        await self._interrupt.wait()

    def _finish(self) -> None:
        self._done.set()


@dataclass
class _Segment:
    text: str
    start: float
    end: float | None = None


class PlayoutTranscript:
    """Works out the words already heard, given how much audio has played.

    A TTS speaks a sentence at a time, so the audio of each sentence is known exactly. Inside a sentence the words are
    spread evenly over its audio, which is an estimate. A reply cut off mid sentence is stored up to the estimated word.
    While a sentence's audio is still arriving its length is not known yet, so a typical speaking rate limits the words too.
    """

    WORDS_PER_SECOND = 2.6

    def __init__(self) -> None:
        self._segments: list[_Segment] = []
        self.pushed_seconds = 0.0
        self._closed = False

    def begin_segment(self, text: str) -> None:
        if self._segments:
            self._segments[-1].end = self.pushed_seconds
        self._segments.append(_Segment(text, self.pushed_seconds))

    def add_audio(self, seconds: float) -> None:
        self.pushed_seconds += seconds

    def finish(self) -> None:
        self._closed = True
        if self._segments:
            self._segments[-1].end = self.pushed_seconds

    @property
    def full_text(self) -> str:
        return " ".join(s.text.strip() for s in self._segments if s.text.strip())

    def text_at(self, position: float) -> str:
        out: list[str] = []
        for segment in self._segments:
            if position <= segment.start:
                break
            # A segment still being synthesized ends, as far as we know, where its audio ends so far.
            end = segment.end if segment.end is not None else self.pushed_seconds
            if position >= end and segment.end is not None:
                out.append(segment.text.strip())
                continue
            span = max(end - segment.start, 1e-6)
            words = segment.text.split()
            count = int(min(1.0, (position - segment.start) / span) * len(words))
            if segment.end is None:
                count = min(count, int((position - segment.start) * self.WORDS_PER_SECOND))
            if count:
                out.append(" ".join(words[:count]))
            break
        return " ".join(out)


class GrowingTranscript:
    """The same, for a realtime model, whose text arrives alongside its audio with no sentence boundaries to go by.

    Once all the audio has arrived, words are spread evenly over it. Until then the total is unknown, and text usually runs
    ahead of audio, so the words released are also limited by a typical speaking rate. That can understate what was heard
    by a little, which beats storing words that were never spoken.
    """

    WORDS_PER_SECOND = 2.6

    def __init__(self) -> None:
        self._text = ""
        self._audio_seconds = 0.0
        self._complete = False

    def add_text(self, delta: str) -> None:
        self._text += delta

    def set_audio(self, seconds: float, *, complete: bool) -> None:
        self._audio_seconds = seconds
        self._complete = complete

    @property
    def full_text(self) -> str:
        return self._text.strip()

    def text_at(self, position: float) -> str:
        if self._audio_seconds <= 0 or position <= 0:
            return ""
        words = self._text.split()
        share = min(1.0, position / self._audio_seconds)
        count = int(share * len(words))
        if self._complete:
            if share >= 1.0:
                return self.full_text
        else:
            count = min(count, int(position * self.WORDS_PER_SECOND))
        return " ".join(words[:count])

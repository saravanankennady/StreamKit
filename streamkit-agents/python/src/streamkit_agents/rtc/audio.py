"""Audio inside the framework is 16-bit PCM. Opus lives at the WebRTC edge, in aiortc."""

from __future__ import annotations

import asyncio
import fractions
import time
from dataclasses import dataclass
from typing import AsyncIterator, Callable

import av
import numpy as np
from aiortc import MediaStreamTrack
from aiortc.mediastreams import MediaStreamError

# What aiortc's Opus encoder works in, and the frame size it sends.
OUTPUT_RATE = 48000
OUTPUT_FRAME_MS = 20
OUTPUT_FRAME_SAMPLES = OUTPUT_RATE * OUTPUT_FRAME_MS // 1000
SAMPLE_BYTES = 2


@dataclass
class AudioFrame:
    """A run of 16-bit little-endian PCM, interleaved if it has several channels."""

    data: bytes
    sample_rate: int
    num_channels: int = 1

    @property
    def samples_per_channel(self) -> int:
        return len(self.data) // (SAMPLE_BYTES * self.num_channels)

    @property
    def duration(self) -> float:
        return self.samples_per_channel / self.sample_rate

    def to_numpy(self) -> np.ndarray:
        return np.frombuffer(self.data, dtype=np.int16)

    @staticmethod
    def silence(duration: float, sample_rate: int, num_channels: int = 1) -> "AudioFrame":
        samples = int(duration * sample_rate) * num_channels
        return AudioFrame(bytes(samples * SAMPLE_BYTES), sample_rate, num_channels)

    @staticmethod
    def concat(frames: list["AudioFrame"]) -> "AudioFrame":
        if not frames:
            raise ValueError("no frames")
        return AudioFrame(b"".join(f.data for f in frames), frames[0].sample_rate, frames[0].num_channels)


class Resampler:
    """Converts a stream of frames to one rate and to mono. It keeps filter state between calls, so one instance goes
    with one stream."""

    def __init__(self, out_rate: int):
        self.out_rate = out_rate
        self._resampler: av.AudioResampler | None = None
        self._in_rate = 0
        self._in_channels = 0

    def push(self, frame: AudioFrame) -> AudioFrame | None:
        if frame.sample_rate == self.out_rate and frame.num_channels == 1:
            return frame
        if self._resampler is None or frame.sample_rate != self._in_rate or frame.num_channels != self._in_channels:
            self._resampler = av.AudioResampler(format="s16", layout="mono", rate=self.out_rate)
            self._in_rate = frame.sample_rate
            self._in_channels = frame.num_channels
        layout = "mono" if frame.num_channels == 1 else "stereo"
        packed = np.frombuffer(frame.data, dtype=np.int16).reshape(1, -1)
        av_frame = av.AudioFrame.from_ndarray(packed, format="s16", layout=layout)
        av_frame.sample_rate = frame.sample_rate
        out = [f.to_ndarray().tobytes() for f in self._resampler.resample(av_frame)]
        data = b"".join(out)
        return AudioFrame(data, self.out_rate, 1) if data else None


def resample(frame: AudioFrame, out_rate: int) -> AudioFrame:
    """Resamples one self-contained piece of audio, such as a whole utterance."""
    r = Resampler(out_rate)
    out = r.push(frame)
    tail = b""
    if r._resampler is not None:
        tail = b"".join(f.to_ndarray().tobytes() for f in r._resampler.resample(None))
    return AudioFrame((out.data if out else b"") + tail, out_rate, 1)


def rms_level(data: bytes) -> float:
    """Returns the loudness of s16 audio, from 0 to 1."""
    if not data:
        return 0.0
    samples = np.frombuffer(data, dtype=np.int16).astype(np.float64) / 32768.0
    return float(np.sqrt(np.mean(samples * samples)))


class AudioSource(MediaStreamTrack):
    """A local audio track. Frames pushed in come out in real time, as RTP, with silence when there is nothing to send.

    This is where the framework knows what was really played: the audio that has left for the network. Audio can be held
    back or dropped within one frame, which is what interruptions need.
    """

    kind = "audio"

    def __init__(self, sample_rate: int = OUTPUT_RATE, queue_seconds: float = 0.2):
        super().__init__()
        self._in_rate = sample_rate
        self._queue_limit = int(queue_seconds * OUTPUT_RATE) * SAMPLE_BYTES
        self._resampler = Resampler(OUTPUT_RATE)
        self._buffer = bytearray()
        self._space = asyncio.Event()
        self._space.set()
        self._drained = asyncio.Event()
        self._drained.set()
        self._next_at = 0.0
        self._pts = 0
        self._paused = False
        self._played_samples = 0
        self._on_first_audio: Callable[[float], None] | None = None

    @property
    def paused(self) -> bool:
        return self._paused

    @property
    def queued_seconds(self) -> float:
        return len(self._buffer) / SAMPLE_BYTES / OUTPUT_RATE

    @property
    def played_seconds(self) -> float:
        """How much pushed audio has gone out, since the source was created. Silence does not count."""
        return self._played_samples / OUTPUT_RATE

    def on_next_audio(self, callback: Callable[[float], None] | None) -> None:
        """Calls back once, with the wall clock time, when the next pushed audio leaves."""
        self._on_first_audio = callback

    async def capture_frame(self, frame: AudioFrame) -> None:
        """Queues audio, waiting while the queue is full. So a producer runs only slightly ahead of real time, and a
        reply cut off early has not spent much on audio nobody will hear."""
        converted = self._resampler.push(frame)
        if converted is None:
            return
        self._buffer += converted.data
        self._drained.clear()
        if len(self._buffer) > self._queue_limit:
            self._space.clear()
        await self._space.wait()

    def clear_queue(self) -> float:
        """Drops queued audio at once, and returns how many seconds were dropped."""
        dropped = self.queued_seconds
        self._buffer.clear()
        self._space.set()
        self._drained.set()
        return dropped

    def pause(self) -> None:
        """Holds queued audio back, sending silence, until resume or clear_queue."""
        self._paused = True

    def resume(self) -> None:
        self._paused = False

    async def wait_for_playout(self) -> None:
        await self._drained.wait()

    async def recv(self) -> av.AudioFrame:
        if self.readyState != "live":
            raise MediaStreamError
        now = time.monotonic()
        if self._next_at == 0.0:
            self._next_at = now
        delay = self._next_at - now
        if delay > 0:
            await asyncio.sleep(delay)
        elif delay < -0.2:
            # The event loop stalled. Carrying on from now avoids a burst the receiver would have to catch up on.
            self._next_at = now
        self._next_at += OUTPUT_FRAME_MS / 1000

        need = OUTPUT_FRAME_SAMPLES * SAMPLE_BYTES
        if self._buffer and not self._paused:
            chunk = bytes(self._buffer[:need])
            del self._buffer[: len(chunk)]
            self._played_samples += len(chunk) // SAMPLE_BYTES
            if len(chunk) < need:
                chunk += bytes(need - len(chunk))
            if self._on_first_audio is not None:
                callback, self._on_first_audio = self._on_first_audio, None
                callback(time.time())
            if len(self._buffer) <= self._queue_limit:
                self._space.set()
            if not self._buffer:
                self._drained.set()
        else:
            chunk = bytes(need)

        samples = np.frombuffer(chunk, dtype=np.int16).reshape(1, -1)
        frame = av.AudioFrame.from_ndarray(samples, format="s16", layout="mono")
        frame.sample_rate = OUTPUT_RATE
        frame.pts = self._pts
        frame.time_base = fractions.Fraction(1, OUTPUT_RATE)
        self._pts += OUTPUT_FRAME_SAMPLES
        return frame


class AudioStream:
    """Reads a subscribed track as PCM at a chosen rate, one frame per decoded packet."""

    def __init__(self, track: MediaStreamTrack, sample_rate: int = 16000):
        self._track = track
        self._rate = sample_rate
        self._resampler = av.AudioResampler(format="s16", layout="mono", rate=sample_rate)

    def __aiter__(self) -> AsyncIterator[AudioFrame]:
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[AudioFrame]:
        while True:
            try:
                frame = await self._track.recv()
            except MediaStreamError:
                return
            for out in self._resampler.resample(frame):
                yield AudioFrame(out.to_ndarray().tobytes(), self._rate, 1)

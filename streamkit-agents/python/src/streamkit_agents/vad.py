"""Voice activity detection. Silero is the real detector. The energy detector needs nothing installed, and suits clean audio
and tests of everything around it.

Silero VAD is MIT licensed (https://github.com/snakers4/silero-vad). We load its ONNX file and run it with ONNX Runtime,
with our own windowing. No LiveKit model or code is involved.
"""

from __future__ import annotations

import asyncio
import enum
import os
from dataclasses import dataclass
from typing import AsyncIterator

import numpy as np

from .rtc.audio import AudioFrame, Resampler, rms_level


class VADEventType(str, enum.Enum):
    START_OF_SPEECH = "start_of_speech"
    # Sent for every analysed window. Interruption reads how much speech has built up from this.
    INFERENCE_DONE = "inference_done"
    END_OF_SPEECH = "end_of_speech"


@dataclass
class VADEvent:
    type: VADEventType
    speaking: bool = False
    probability: float = 0.0
    speech_duration: float = 0.0
    silence_duration: float = 0.0
    # On START_OF_SPEECH, the audio just before, so the first syllable is not lost. On END_OF_SPEECH, the whole utterance.
    audio: AudioFrame | None = None


class VAD:
    sample_rate = 16000

    def stream(self) -> "VADStream":
        raise NotImplementedError


class VADStream:
    """Turns per-window speech probabilities into speech start and end events."""

    def __init__(
        self,
        *,
        window_samples: int,
        sample_rate: int,
        activation: float,
        deactivation: float,
        min_speech: float,
        min_silence: float,
        padding: float,
    ):
        self._rate = sample_rate
        self._window = window_samples
        self._activation = activation
        self._deactivation = deactivation
        self._min_speech = min_speech
        self._min_silence = min_silence
        self._padding_bytes = int(padding * sample_rate) * 2
        self._resampler = Resampler(sample_rate)
        self._pending = bytearray()
        self._ring = bytearray()
        self._utterance = bytearray()
        self._speaking = False
        self._speech = 0.0
        self._silence = 0.0
        self._candidate = 0.0
        self._events: asyncio.Queue[VADEvent | None] = asyncio.Queue()

    def push_frame(self, frame: AudioFrame) -> None:
        converted = self._resampler.push(frame)
        if converted is None:
            return
        self._pending += converted.data
        need = self._window * 2
        while len(self._pending) >= need:
            window = bytes(self._pending[:need])
            del self._pending[:need]
            self._process(window)

    def end_input(self) -> None:
        self._events.put_nowait(None)

    def _probability(self, window: bytes) -> float:
        raise NotImplementedError

    def _process(self, window: bytes) -> None:
        probability = self._probability(window)
        step = self._window / self._rate
        if self._speaking:
            self._utterance += window
            if probability >= self._deactivation:
                self._speech += step
                self._silence = 0.0
            else:
                self._silence += step
                if self._silence >= self._min_silence:
                    self._speaking = False
                    audio = AudioFrame(bytes(self._utterance), self._rate)
                    self._utterance.clear()
                    self._events.put_nowait(VADEvent(
                        VADEventType.END_OF_SPEECH, speaking=False, probability=probability,
                        speech_duration=self._speech, silence_duration=self._silence, audio=audio,
                    ))
                    self._speech = 0.0
                    self._candidate = 0.0
        else:
            self._ring += window
            if len(self._ring) > self._padding_bytes + len(window):
                del self._ring[: len(self._ring) - self._padding_bytes - len(window)]
            if probability >= self._activation:
                self._candidate += step
                if self._candidate >= self._min_speech:
                    self._speaking = True
                    self._speech = self._candidate
                    self._silence = 0.0
                    self._utterance = bytearray(self._ring)
                    self._ring.clear()
                    self._events.put_nowait(VADEvent(
                        VADEventType.START_OF_SPEECH, speaking=True, probability=probability,
                        speech_duration=self._speech, audio=AudioFrame(bytes(self._utterance), self._rate),
                    ))
            else:
                self._candidate = 0.0
                self._silence += step
        self._events.put_nowait(VADEvent(
            VADEventType.INFERENCE_DONE, speaking=self._speaking, probability=probability,
            speech_duration=self._speech, silence_duration=self._silence,
        ))

    def __aiter__(self) -> AsyncIterator[VADEvent]:
        return self._iterate()

    async def _iterate(self) -> AsyncIterator[VADEvent]:
        while True:
            event = await self._events.get()
            if event is None:
                return
            yield event


class EnergyVAD(VAD):
    """Compares loudness to a threshold. It cannot tell speech from other noise."""

    def __init__(self, *, threshold: float = 0.015, min_speech: float = 0.1, min_silence: float = 0.3, padding: float = 0.3):
        self.threshold = threshold
        self.min_speech = min_speech
        self.min_silence = min_silence
        self.padding = padding

    def stream(self) -> VADStream:
        threshold = self.threshold

        class _Stream(VADStream):
            def _probability(self, window: bytes) -> float:
                return 1.0 if rms_level(window) >= threshold else 0.0

        return _Stream(
            window_samples=320, sample_rate=16000, activation=0.5, deactivation=0.5,
            min_speech=self.min_speech, min_silence=self.min_silence, padding=self.padding,
        )


class SileroVAD(VAD):
    """Silero VAD through ONNX Runtime, on 16 kHz audio in windows of 512 samples.

    model_path: where silero_vad.onnx is. Defaults to the SILERO_VAD_MODEL environment variable.
    """

    def __init__(
        self,
        model_path: str | None = None,
        *,
        activation: float = 0.5,
        deactivation: float = 0.35,
        min_speech: float = 0.1,
        min_silence: float = 0.25,
        padding: float = 0.3,
    ):
        import onnxruntime

        path = model_path or os.environ.get("SILERO_VAD_MODEL")
        if not path:
            raise ValueError("no Silero model: pass model_path or set SILERO_VAD_MODEL")
        options = onnxruntime.SessionOptions()
        # One thread is plenty for a 32 ms window, and a voice agent is not the only thing on the machine.
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        self._session = onnxruntime.InferenceSession(path, options, providers=["CPUExecutionProvider"])
        self.activation = activation
        self.deactivation = deactivation
        self.min_speech = min_speech
        self.min_silence = min_silence
        self.padding = padding

    def stream(self) -> VADStream:
        session = self._session

        class _Stream(VADStream):
            def __init__(self, **kwargs):
                super().__init__(**kwargs)
                self._state = np.zeros((2, 1, 128), dtype=np.float32)
                self._context = np.zeros((1, 64), dtype=np.float32)

            def _probability(self, window: bytes) -> float:
                samples = np.frombuffer(window, dtype=np.int16).astype(np.float32).reshape(1, -1) / 32768.0
                # The model expects the tail of the previous window in front of each new one.
                model_input = np.concatenate([self._context, samples], axis=1)
                output, self._state = session.run(None, {
                    "input": model_input, "state": self._state, "sr": np.array(16000, dtype=np.int64),
                })
                self._context = samples[:, -64:]
                return float(output[0][0])

        return _Stream(
            window_samples=512, sample_rate=16000, activation=self.activation, deactivation=self.deactivation,
            min_speech=self.min_speech, min_silence=self.min_silence, padding=self.padding,
        )

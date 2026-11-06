"""AssemblyAI streaming transcription. Written from https://www.assemblyai.com/docs/api-reference/streaming-api/streaming-api."""

from __future__ import annotations

import asyncio
import json
from urllib.parse import urlencode

import websockets
from websockets.asyncio.client import connect

from ..llm.base import APIError, RetryPolicy, Usage
from ..rtc.audio import AudioFrame, Resampler
from ..stt import STT, RecognizeStream, SpeechEvent, SpeechEventType

# The API wants each message to hold 50 ms to 1000 ms of audio.
MIN_CHUNK_SECONDS = 0.05


class AssemblyAISTT(STT):
    provider = "assemblyai"
    streaming = True
    sample_rate = 16000

    def __init__(self, model: str = "universal-3-6-pro", *, api_key: str, base_url: str = "wss://streaming.assemblyai.com", retry: RetryPolicy | None = None):
        super().__init__(model, retry=retry)
        self._key = api_key
        self._url = base_url.rstrip("/") + "/v3/ws"

    def stream(self) -> "AssemblyAIStream":
        query = urlencode({"speech_model": self.model, "sample_rate": self.sample_rate, "encoding": "pcm_s16le"})
        return AssemblyAIStream(f"{self._url}?{query}", self._key, self.sample_rate)

    async def _recognize(self, frame: AudioFrame) -> SpeechEvent:
        raise APIError("assemblyai is used through its streaming API here", retryable=False)


class AssemblyAIStream(RecognizeStream):
    def __init__(self, url: str, key: str, rate: int):
        super().__init__()
        self._rate = rate
        self._resampler = Resampler(rate)
        self._pending = bytearray()
        self._outgoing: asyncio.Queue[bytes | str | None] = asyncio.Queue()
        self._task = asyncio.create_task(self._run(url, key))

    def push_frame(self, frame: AudioFrame) -> None:
        converted = self._resampler.push(frame)
        if converted is None:
            return
        self._pending += converted.data
        minimum = int(self._rate * MIN_CHUNK_SECONDS) * 2
        if len(self._pending) >= minimum:
            self._outgoing.put_nowait(bytes(self._pending))
            self._pending.clear()

    def flush(self) -> None:
        self._outgoing.put_nowait(json.dumps({"type": "ForceEndpoint"}))

    def end_input(self) -> None:
        self._outgoing.put_nowait(json.dumps({"type": "Terminate"}))

    async def _run(self, url: str, key: str) -> None:
        try:
            # The key goes in the Authorization header, with no Bearer prefix.
            async with connect(url, additional_headers={"Authorization": key}) as ws:
                sender = asyncio.create_task(self._send(ws))
                try:
                    await self._receive(ws)
                finally:
                    sender.cancel()
        except (OSError, websockets.exceptions.WebSocketException) as e:
            raise APIError(f"assemblyai connection failed: {type(e).__name__}") from e
        finally:
            self._events.put_nowait(None)

    async def _send(self, ws) -> None:
        while True:
            item = await self._outgoing.get()
            if item is None:
                return
            await ws.send(item)

    async def _receive(self, ws) -> None:
        async for raw in ws:
            if isinstance(raw, bytes):
                continue
            message = json.loads(raw)
            kind = message.get("type")
            if kind == "SpeechStarted":
                self._events.put_nowait(SpeechEvent(SpeechEventType.START_OF_SPEECH))
            elif kind == "Turn":
                ended = message.get("end_of_turn")
                text = message.get("utterance") if ended and message.get("utterance") else message.get("transcript", "")
                if text:
                    self._events.put_nowait(SpeechEvent(SpeechEventType.FINAL if ended else SpeechEventType.INTERIM, text=text))
                if ended:
                    self._events.put_nowait(SpeechEvent(SpeechEventType.END_OF_SPEECH))
            elif kind == "Termination":
                self._events.put_nowait(SpeechEvent(SpeechEventType.USAGE, usage=Usage(audio_seconds=message.get("audio_duration_seconds") or 0.0, raw=message)))
                return

    async def aclose(self) -> None:
        self._task.cancel()
        try:
            await self._task
        except (asyncio.CancelledError, APIError):
            pass

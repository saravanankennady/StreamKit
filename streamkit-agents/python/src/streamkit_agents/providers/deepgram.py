"""Deepgram speech. Written from https://developers.deepgram.com/reference/speech-to-text/listen-streaming and
https://developers.deepgram.com/reference/text-to-speech/speak-request."""

from __future__ import annotations

import asyncio
import json
from typing import AsyncIterator
from urllib.parse import urlencode

import httpx
import websockets
from websockets.asyncio.client import connect

from ..llm.base import APIError, RetryPolicy, Usage, error_for_status
from ..rtc.audio import AudioFrame, Resampler
from ..stt import STT, RecognizeStream, SpeechEvent, SpeechEventType
from ..tts import TTS, PCMFramer
from ._http import WavHeaderSkipper, make_client, raise_for_status, transport_error


class DeepgramSTT(STT):
    provider = "deepgram"
    streaming = True
    sample_rate = 16000

    def __init__(self, model: str = "nova-3", *, api_key: str, language: str = "en", base_url: str = "wss://api.deepgram.com", http_url: str = "https://api.deepgram.com", endpointing_ms: int = 300, retry: RetryPolicy | None = None):
        super().__init__(model, language=language, retry=retry)
        self._key = api_key
        self._ws_url = base_url.rstrip("/") + "/v1/listen"
        self._http_url = http_url.rstrip("/") + "/v1/listen"
        self._endpointing = endpointing_ms

    def _params(self) -> dict[str, str]:
        return {
            "model": self.model,
            "language": self.language or "en",
            "encoding": "linear16",
            "sample_rate": str(self.sample_rate),
            "channels": "1",
            "interim_results": "true",
            "vad_events": "true",
            "smart_format": "true",
            "endpointing": str(self._endpointing),
        }

    def stream(self) -> "DeepgramStream":
        return DeepgramStream(self._ws_url + "?" + urlencode(self._params()), self._key, self.sample_rate)

    async def _recognize(self, frame: AudioFrame) -> SpeechEvent:
        params = {"model": self.model, "language": self.language or "en", "encoding": "linear16", "sample_rate": str(frame.sample_rate), "smart_format": "true"}
        async with make_client(self.retry, {"Authorization": f"Token {self._key}", "Content-Type": "audio/raw"}) as client:
            try:
                response = await client.post(self._http_url, params=params, content=frame.data)
            except httpx.HTTPError as e:
                raise transport_error(self.provider, e) from e
        if response.status_code >= 400:
            raise error_for_status(self.provider, response.status_code, response.text)
        channels = response.json().get("results", {}).get("channels", [])
        text = channels[0]["alternatives"][0]["transcript"] if channels else ""
        return SpeechEvent(SpeechEventType.FINAL, text=text, usage=Usage(audio_seconds=frame.duration))


class DeepgramStream(RecognizeStream):
    def __init__(self, url: str, key: str, rate: int):
        super().__init__()
        self._outgoing: asyncio.Queue[bytes | str | None] = asyncio.Queue()
        self._resampler = Resampler(rate)
        self._audio_seconds = 0.0
        self._task = asyncio.create_task(self._run(url, key))

    def push_frame(self, frame: AudioFrame) -> None:
        converted = self._resampler.push(frame)
        if converted is not None:
            self._audio_seconds += converted.duration
            self._outgoing.put_nowait(converted.data)

    def flush(self) -> None:
        self._outgoing.put_nowait(json.dumps({"type": "Finalize"}))

    def end_input(self) -> None:
        self._outgoing.put_nowait(json.dumps({"type": "CloseStream"}))
        self._outgoing.put_nowait(None)

    async def _run(self, url: str, key: str) -> None:
        try:
            async with connect(url, additional_headers={"Authorization": f"Token {key}"}) as ws:
                sender = asyncio.create_task(self._send(ws))
                try:
                    await self._receive(ws)
                finally:
                    sender.cancel()
        except (OSError, websockets.exceptions.WebSocketException) as e:
            self._events.put_nowait(SpeechEvent(SpeechEventType.USAGE, usage=Usage(audio_seconds=self._audio_seconds)))
            raise APIError(f"deepgram connection failed: {type(e).__name__}") from e
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
            elif kind == "Results":
                alternatives = (message.get("channel") or {}).get("alternatives") or [{}]
                text = alternatives[0].get("transcript") or ""
                confidence = alternatives[0].get("confidence")
                if text:
                    final = message.get("is_final")
                    self._events.put_nowait(SpeechEvent(SpeechEventType.FINAL if final else SpeechEventType.INTERIM, text=text, confidence=confidence))
                if message.get("speech_final"):
                    self._events.put_nowait(SpeechEvent(SpeechEventType.END_OF_SPEECH))
            elif kind == "UtteranceEnd":
                self._events.put_nowait(SpeechEvent(SpeechEventType.END_OF_SPEECH))
            elif kind == "Metadata":
                self._events.put_nowait(SpeechEvent(SpeechEventType.USAGE, usage=Usage(audio_seconds=message.get("duration") or self._audio_seconds, raw=message)))

    async def aclose(self) -> None:
        self._task.cancel()
        try:
            await self._task
        except (asyncio.CancelledError, APIError):
            pass


class DeepgramTTS(TTS):
    provider = "deepgram"
    sample_rate = 24000

    def __init__(self, model: str = "aura-2-thalia-en", *, api_key: str, base_url: str = "https://api.deepgram.com", retry: RetryPolicy | None = None):
        super().__init__(model, retry=retry)
        self._url = base_url.rstrip("/") + "/v1/speak"
        self._client = make_client(self.retry, {"Authorization": f"Token {api_key}"})

    async def synthesize(self, text: str) -> AsyncIterator[AudioFrame]:
        # The documented container for linear16 is WAV, so the audio starts with a header to skip.
        params = {"model": self.model, "encoding": "linear16", "sample_rate": str(self.sample_rate)}
        framer = PCMFramer(self.sample_rate)
        header = WavHeaderSkipper()
        try:
            async with self._client.stream("POST", self._url, params=params, json={"text": text}) as response:
                await raise_for_status(self.provider, response)
                async for chunk in response.aiter_bytes():
                    for frame in framer.push(header.push(chunk)):
                        yield frame
        except httpx.HTTPError as e:
            raise transport_error(self.provider, e) from e
        tail = framer.flush()
        if tail is not None:
            yield tail
        self.last_usage = Usage(characters=len(text))

    async def aclose(self) -> None:
        await self._client.aclose()

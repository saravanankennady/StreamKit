"""Speech providers that return audio over plain HTTP.

Cartesia: https://docs.cartesia.ai/api-reference/tts/bytes
ElevenLabs: https://elevenlabs.io/docs/api-reference/text-to-speech/stream
"""

from __future__ import annotations

from typing import Any, AsyncIterator

import httpx

from ..llm.base import RetryPolicy, Usage
from ..rtc.audio import AudioFrame
from ..tts import TTS, PCMFramer
from ._http import make_client, raise_for_status, transport_error


class _HTTPTTS(TTS):
    def _request(self, text: str) -> tuple[str, dict[str, str], dict[str, Any]]:
        raise NotImplementedError

    async def synthesize(self, text: str) -> AsyncIterator[AudioFrame]:
        url, params, body = self._request(text)
        framer = PCMFramer(self.sample_rate)
        try:
            async with self._client.stream("POST", url, params=params, json=body) as response:
                await raise_for_status(self.provider, response)
                async for chunk in response.aiter_bytes():
                    for frame in framer.push(chunk):
                        yield frame
        except httpx.HTTPError as e:
            raise transport_error(self.provider, e) from e
        tail = framer.flush()
        if tail is not None:
            yield tail
        self.last_usage = Usage(characters=len(text))

    async def aclose(self) -> None:
        await self._client.aclose()


class CartesiaTTS(_HTTPTTS):
    provider = "cartesia"
    sample_rate = 24000

    def __init__(self, model: str = "sonic-3.6", *, voice: str, api_key: str, language: str | None = None, base_url: str = "https://api.cartesia.ai", api_version: str = "2026-08-14", retry: RetryPolicy | None = None):
        super().__init__(model, voice=voice, retry=retry)
        self._url = base_url.rstrip("/") + "/tts/bytes"
        self._language = language
        self._client = make_client(self.retry, {"Authorization": f"Bearer {api_key}", "Cartesia-Version": api_version})

    def _request(self, text: str):
        body: dict[str, Any] = {
            "model_id": self.model,
            "transcript": text,
            "voice": {"id": self.voice},
            "output_format": {"container": "raw", "encoding": "pcm_s16le", "sample_rate": self.sample_rate},
        }
        if self._language:
            body["language"] = self._language
        return self._url, {}, body


class ElevenLabsTTS(_HTTPTTS):
    provider = "elevenlabs"
    sample_rate = 24000

    def __init__(self, model: str = "eleven_multilingual_v2", *, voice: str, api_key: str, base_url: str = "https://api.elevenlabs.io", retry: RetryPolicy | None = None):
        super().__init__(model, voice=voice, retry=retry)
        self._base = base_url.rstrip("/")
        self._client = make_client(self.retry, {"xi-api-key": api_key})

    def _request(self, text: str):
        return f"{self._base}/v1/text-to-speech/{self.voice}/stream", {"output_format": f"pcm_{self.sample_rate}"}, {"text": text, "model_id": self.model}

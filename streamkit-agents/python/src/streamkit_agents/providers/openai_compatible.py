"""OpenAI-shaped HTTP APIs: chat completions, audio transcriptions and audio speech.

Many services and local engines answer in these shapes. Each preset below names a base URL taken from that service's own
documentation. See docs/VOICE_AI_PROVIDER_MATRIX.md for what has been checked, and when.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, AsyncIterator

import httpx

from ..llm.base import LLM, APIError, ChatChunk, RetryPolicy, Usage, error_for_status
from ..llm.chat import ChatContext, FunctionCall
from ..rtc.audio import AudioFrame
from ..stt import STT, SpeechEvent, SpeechEventType
from ..tts import TTS, PCMFramer
from ._http import as_wav, make_client, raise_for_status, sse_events, transport_error


@dataclass(frozen=True)
class Preset:
    base_url: str
    env_key: str
    # Whether its documentation shows these working in the OpenAI shape. A preset is not offered without chat.
    supports_stream_usage: bool = True


PRESETS: dict[str, Preset] = {
    "openai": Preset("https://api.openai.com/v1", "OPENAI_API_KEY"),
    "groq": Preset("https://api.groq.com/openai/v1", "GROQ_API_KEY"),
    "openrouter": Preset("https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"),
    "deepseek": Preset("https://api.deepseek.com", "DEEPSEEK_API_KEY"),
    "together": Preset("https://api.together.ai/v1", "TOGETHER_API_KEY"),
    "mistral": Preset("https://api.mistral.ai/v1", "MISTRAL_API_KEY", supports_stream_usage=False),
    "ollama": Preset("http://localhost:11434/v1", ""),
}


class OpenAICompatibleLLM(LLM):
    """Chat completions with streaming."""

    provider = "openai-compatible"

    def __init__(
        self,
        model: str,
        *,
        base_url: str,
        api_key: str | None = None,
        provider: str | None = None,
        stream_usage: bool = True,
        retry: RetryPolicy | None = None,
        extra_body: dict[str, Any] | None = None,
    ):
        super().__init__(model, retry=retry)
        if provider:
            self.provider = provider
        self._url = base_url.rstrip("/") + "/chat/completions"
        self._stream_usage = stream_usage
        self._extra = extra_body or {}
        headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
        self._client = make_client(self.retry, headers)

    async def _stream(self, ctx: ChatContext, tools, tool_choice, temperature, max_tokens) -> AsyncIterator[ChatChunk]:
        body: dict[str, Any] = {"model": self.model, "messages": ctx.to_openai(), "stream": True, **self._extra}
        if self._stream_usage:
            body["stream_options"] = {"include_usage": True}
        if tools:
            body["tools"] = [t.to_openai() for t in tools]
            if tool_choice:
                body["tool_choice"] = tool_choice
        if temperature is not None:
            body["temperature"] = temperature
        if max_tokens is not None:
            body["max_tokens"] = max_tokens
        # Tool call fragments, by index, until they are complete.
        partial: dict[int, dict[str, str]] = {}
        try:
            async with self._client.stream("POST", self._url, json=body) as response:
                await raise_for_status(self.provider, response)
                async for _, data in sse_events(response):
                    if data.strip() == "[DONE]":
                        break
                    try:
                        chunk = json.loads(data)
                    except json.JSONDecodeError:
                        continue
                    if "error" in chunk:
                        raise APIError(f"{self.provider} reported an error: {str(chunk['error'])[:300]}", retryable=False)
                    usage = _usage(chunk.get("usage"))
                    for choice in chunk.get("choices") or []:
                        delta = choice.get("delta") or {}
                        for call in delta.get("tool_calls") or []:
                            slot = partial.setdefault(call.get("index", 0), {"id": "", "name": "", "arguments": ""})
                            slot["id"] = call.get("id") or slot["id"]
                            function = call.get("function") or {}
                            slot["name"] += function.get("name") or ""
                            slot["arguments"] += function.get("arguments") or ""
                        if delta.get("content"):
                            yield ChatChunk(text=delta["content"])
                        if choice.get("finish_reason"):
                            yield ChatChunk(finish_reason=choice["finish_reason"])
                    if usage is not None:
                        yield ChatChunk(usage=usage)
        except httpx.HTTPError as e:
            raise transport_error(self.provider, e) from e
        if partial:
            yield ChatChunk(tool_calls=[
                FunctionCall(call_id=s["id"] or f"call_{i}", name=s["name"], arguments=s["arguments"] or "{}")
                for i, s in sorted(partial.items())
            ])

    async def aclose(self) -> None:
        await self._client.aclose()


def _usage(raw: dict[str, Any] | None) -> Usage | None:
    if not raw:
        return None
    details = raw.get("prompt_tokens_details") or {}
    return Usage(
        prompt_tokens=raw.get("prompt_tokens") or 0,
        completion_tokens=raw.get("completion_tokens") or 0,
        cached_tokens=details.get("cached_tokens") or 0,
        # OpenRouter puts a cost here. OpenAI itself does not.
        reported_cost=raw.get("cost"),
        raw=raw,
    )


class OpenAICompatibleSTT(STT):
    """POST /audio/transcriptions, one utterance at a time."""

    provider = "openai-compatible"
    streaming = False
    sample_rate = 16000

    def __init__(self, model: str, *, base_url: str, api_key: str | None = None, language: str | None = None, provider: str | None = None, retry: RetryPolicy | None = None):
        super().__init__(model, language=language, retry=retry)
        if provider:
            self.provider = provider
        self._url = base_url.rstrip("/") + "/audio/transcriptions"
        self._client = make_client(self.retry, {"Authorization": f"Bearer {api_key}"} if api_key else {})

    async def _recognize(self, frame: AudioFrame) -> SpeechEvent:
        data = {"model": self.model, "response_format": "json"}
        if self.language:
            data["language"] = self.language
        try:
            response = await self._client.post(
                self._url, data=data, files={"file": ("speech.wav", as_wav(frame), "audio/wav")}
            )
        except httpx.HTTPError as e:
            raise transport_error(self.provider, e) from e
        if response.status_code >= 400:
            raise error_for_status(self.provider, response.status_code, response.text)
        body = response.json()
        usage = Usage(audio_seconds=frame.duration, raw=body.get("usage") or {})
        return SpeechEvent(SpeechEventType.FINAL, text=(body.get("text") or "").strip(), language=self.language or "", usage=usage)

    async def aclose(self) -> None:
        await self._client.aclose()


class OpenAICompatibleTTS(TTS):
    """POST /audio/speech, streamed as raw 24 kHz PCM."""

    provider = "openai-compatible"
    sample_rate = 24000

    def __init__(self, model: str, *, base_url: str, api_key: str | None = None, voice: str = "alloy", provider: str | None = None, sample_rate: int = 24000, retry: RetryPolicy | None = None):
        super().__init__(model, voice=voice, retry=retry)
        if provider:
            self.provider = provider
        self.sample_rate = sample_rate
        self._url = base_url.rstrip("/") + "/audio/speech"
        self._client = make_client(self.retry, {"Authorization": f"Bearer {api_key}"} if api_key else {})

    async def synthesize(self, text: str) -> AsyncIterator[AudioFrame]:
        body = {"model": self.model, "input": text, "voice": self.voice, "response_format": "pcm"}
        framer = PCMFramer(self.sample_rate)
        attempt = 0
        while True:
            sent = False
            try:
                async with self._client.stream("POST", self._url, json=body) as response:
                    await raise_for_status(self.provider, response)
                    # A server may say what rate it produces. Ours is the documented 24 kHz unless it does.
                    rate = response.headers.get("x-sample-rate")
                    if rate and int(rate) != framer.sample_rate:
                        framer = PCMFramer(int(rate))
                    async for chunk in response.aiter_bytes():
                        for frame in framer.push(chunk):
                            sent = True
                            yield frame
                tail = framer.flush()
                if tail is not None:
                    yield tail
                self.last_usage = Usage(characters=len(text))
                return
            except (httpx.HTTPError, APIError) as e:
                error = transport_error(self.provider, e) if isinstance(e, httpx.HTTPError) else e
                if sent or not error.retryable or attempt >= self.retry.max_retries:
                    raise error from e
                attempt += 1

    async def aclose(self) -> None:
        await self._client.aclose()

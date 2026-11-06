"""Choosing models at run time. A model is named by a string, "provider/model", optionally followed by ":voice" for TTS, so a
choice can come from code, configuration or a job's metadata.

    selector = ModelSelector()
    llm = await selector.llm("anthropic/claude-haiku-5-5")
    tts = await selector.tts("cartesia/sonic-3.6:<voice id>")

A provider is only here once its documentation has been read. docs/VOICE_AI_PROVIDER_MATRIX.md says how far each has been
tested.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any

from .credentials import Credentials
from .llm.base import LLM
from .providers.anthropic import AnthropicLLM
from .providers.assemblyai import AssemblyAISTT
from .providers.deepgram import DeepgramSTT, DeepgramTTS
from .providers.gemini_live import GeminiLiveModel
from .providers.google import GoogleLLM
from .providers.openai_compatible import PRESETS, OpenAICompatibleLLM, OpenAICompatibleSTT, OpenAICompatibleTTS
from .providers.openai_realtime import OpenAIRealtimeModel
from .providers.speech_http import CartesiaTTS, ElevenLabsTTS
from .realtime import RealtimeModel
from .stt import STT
from .tts import TTS


class UnknownProvider(ValueError):
    pass


def _split(spec: str) -> tuple[str, str]:
    provider, _, model = spec.partition("/")
    if not model:
        raise ValueError(f'a model is named "provider/model", not {spec!r}')
    return provider, model


class ModelSelector:
    def __init__(self, credentials: Credentials | None = None):
        self.credentials = credentials or Credentials()

    async def llm(self, spec: str, *, base_url: str | None = None) -> LLM:
        provider, model = _split(spec)
        if provider == "anthropic":
            return AnthropicLLM(model, api_key=await self.credentials.require("anthropic"))
        if provider == "google":
            return GoogleLLM(model, api_key=await self.credentials.require("google"))
        if provider == "local":
            return OpenAICompatibleLLM(model, base_url=base_url or os.environ["STREAMKIT_LOCAL_LLM_URL"], provider="local", api_key="local")
        preset = PRESETS.get(provider)
        if preset is None:
            raise UnknownProvider(f"no LLM provider called {provider}")
        key = await self.credentials.require(provider) if preset.env_key else None
        return OpenAICompatibleLLM(model, base_url=base_url or preset.base_url, api_key=key, provider=provider, stream_usage=preset.supports_stream_usage)

    async def stt(self, spec: str, *, language: str | None = None, base_url: str | None = None) -> STT:
        provider, model = _split(spec)
        if provider == "deepgram":
            return DeepgramSTT(model, api_key=await self.credentials.require("deepgram"), language=language or "en")
        if provider == "assemblyai":
            return AssemblyAISTT(model, api_key=await self.credentials.require("assemblyai"))
        if provider == "local":
            return OpenAICompatibleSTT(model, base_url=base_url or os.environ["STREAMKIT_LOCAL_STT_URL"], provider="local", language=language, api_key="local")
        if provider in ("openai", "groq"):
            return OpenAICompatibleSTT(model, base_url=base_url or PRESETS[provider].base_url, api_key=await self.credentials.require(provider), provider=provider, language=language)
        raise UnknownProvider(f"no STT provider called {provider}")

    async def tts(self, spec: str, *, base_url: str | None = None) -> TTS:
        provider, rest = _split(spec)
        model, _, voice = rest.partition(":")
        if provider == "deepgram":
            return DeepgramTTS(model, api_key=await self.credentials.require("deepgram"))
        if provider == "cartesia":
            if not voice:
                raise ValueError('Cartesia needs a voice: "cartesia/<model>:<voice id>"')
            return CartesiaTTS(model, voice=voice, api_key=await self.credentials.require("cartesia"))
        if provider == "elevenlabs":
            if not voice:
                raise ValueError('ElevenLabs needs a voice: "elevenlabs/<model>:<voice id>"')
            return ElevenLabsTTS(model, voice=voice, api_key=await self.credentials.require("elevenlabs"))
        if provider == "local":
            return OpenAICompatibleTTS(model, base_url=base_url or os.environ["STREAMKIT_LOCAL_TTS_URL"], voice=voice or "default", provider="local", api_key="local", sample_rate=int(os.environ.get("STREAMKIT_LOCAL_TTS_RATE", "22050")))
        if provider == "openai":
            return OpenAICompatibleTTS(model, base_url=base_url or PRESETS["openai"].base_url, voice=voice or "marin", api_key=await self.credentials.require("openai"), provider="openai")
        raise UnknownProvider(f"no TTS provider called {provider}")

    async def realtime(self, spec: str, *, voice: str | None = None) -> RealtimeModel:
        provider, model = _split(spec)
        if provider == "openai":
            kwargs: dict[str, Any] = {"voice": voice} if voice else {}
            return OpenAIRealtimeModel(model, api_key=await self.credentials.require("openai"), **kwargs)
        if provider == "google":
            kwargs = {"voice": voice} if voice else {}
            return GeminiLiveModel(model, api_key=await self.credentials.require("google"), **kwargs)
        raise UnknownProvider(f"no realtime provider called {provider}")


@dataclass
class ModelChoice:
    """Which models a session should use. Later sources win over earlier ones, field by field."""

    stt: str | None = None
    llm: str | None = None
    tts: str | None = None
    realtime: str | None = None

    def overlay(self, other: "ModelChoice") -> "ModelChoice":
        return ModelChoice(
            stt=other.stt or self.stt,
            llm=other.llm or self.llm,
            tts=other.tts or self.tts,
            realtime=other.realtime or self.realtime,
        )

    @staticmethod
    def from_job_metadata(metadata: str) -> "ModelChoice":
        """Reads {"models": {"llm": "...", ...}} from a dispatch's metadata, so the caller of the dispatch API can choose
        models per room. Metadata that is not JSON, or does not mention models, chooses nothing."""
        try:
            value = json.loads(metadata) if metadata else {}
        except json.JSONDecodeError:
            return ModelChoice()
        models = value.get("models") if isinstance(value, dict) else None
        if not isinstance(models, dict):
            return ModelChoice()
        return ModelChoice(stt=models.get("stt"), llm=models.get("llm"), tts=models.get("tts"), realtime=models.get("realtime"))

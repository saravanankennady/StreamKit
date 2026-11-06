"""Where a worker gets provider API keys: its own environment first, then, if configured, a server-side call to the control
plane, where they are stored sealed.

Keys stay in this process. Nothing here logs them, and they never go near a room: a job's metadata and participant
attributes reach other participants, so credentials must never travel that way.
"""

from __future__ import annotations

import logging
import os
from typing import Protocol

import httpx

logger = logging.getLogger("streamkit_agents.credentials")


class CredentialError(Exception):
    pass


# The environment variable each provider's key is read from.
ENV_KEYS = {
    "openai": "OPENAI_API_KEY",
    "anthropic": "ANTHROPIC_API_KEY",
    "google": "GOOGLE_API_KEY",
    "groq": "GROQ_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "together": "TOGETHER_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "deepgram": "DEEPGRAM_API_KEY",
    "assemblyai": "ASSEMBLYAI_API_KEY",
    "cartesia": "CARTESIA_API_KEY",
    "elevenlabs": "ELEVENLABS_API_KEY",
}


class CredentialSource(Protocol):
    async def get(self, provider: str, name: str = "default") -> str | None: ...


class EnvironmentCredentials:
    async def get(self, provider: str, name: str = "default") -> str | None:
        variable = ENV_KEYS.get(provider)
        return os.environ.get(variable) if variable and name == "default" else None


class ControlPlaneCredentials:
    """Fetches a key over the server-side API. It authenticates with an access token holding the agent grant, which only
    someone with the project's API secret can sign, and browsers are refused."""

    def __init__(self, api_url: str, token: str, *, timeout: float = 10.0):
        self._url = api_url.rstrip("/") + "/api/v1/agent-credentials/resolve"
        self._headers = {"Authorization": f"Bearer {token}"}
        self._timeout = timeout
        self._cache: dict[tuple[str, str], str] = {}

    async def get(self, provider: str, name: str = "default") -> str | None:
        key = (provider, name)
        if key in self._cache:
            return self._cache[key]
        async with httpx.AsyncClient(timeout=self._timeout) as client:
            response = await client.post(self._url, json={"provider": provider, "name": name}, headers=self._headers)
        if response.status_code == 404:
            return None
        if response.status_code >= 400:
            raise CredentialError(f"the control plane refused the credential lookup: {response.status_code}")
        value = response.json().get("secret") or None
        if value:
            self._cache[key] = value
        return value


class Credentials:
    """Looks through sources in order."""

    def __init__(self, sources: list[CredentialSource] | None = None):
        self.sources = sources if sources is not None else [EnvironmentCredentials()]

    async def get(self, provider: str, name: str = "default") -> str | None:
        for source in self.sources:
            value = await source.get(provider, name)
            if value:
                return value
        return None

    async def require(self, provider: str, name: str = "default") -> str:
        value = await self.get(provider, name)
        if not value:
            variable = ENV_KEYS.get(provider)
            hint = f"set {variable}, or " if variable else ""
            raise CredentialError(f"no credential for {provider}: {hint}store one in the control plane")
        return value

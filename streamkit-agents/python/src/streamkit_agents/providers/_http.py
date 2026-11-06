"""HTTP helpers shared by the adapters."""

from __future__ import annotations

import io
import json
import wave
from typing import Any, AsyncIterator

import httpx

from ..llm.base import APIError, RetryPolicy, error_for_status
from ..rtc.audio import AudioFrame


def make_client(retry: RetryPolicy, headers: dict[str, str] | None = None) -> httpx.AsyncClient:
    timeout = httpx.Timeout(connect=retry.connect_timeout, read=retry.read_timeout, write=30.0, pool=10.0)
    return httpx.AsyncClient(timeout=timeout, headers=headers or {})


async def sse_events(response: httpx.Response) -> AsyncIterator[tuple[str | None, str]]:
    """Reads Server-Sent Events, yielding (event name, data). An event's data may span several lines."""
    event: str | None = None
    data: list[str] = []
    async for line in response.aiter_lines():
        if not line:
            if data:
                yield event, "\n".join(data)
            event, data = None, []
        elif line.startswith(":"):
            continue
        elif line.startswith("event:"):
            event = line[6:].strip()
        elif line.startswith("data:"):
            value = line[5:]
            data.append(value[1:] if value.startswith(" ") else value)
    if data:
        yield event, "\n".join(data)


async def raise_for_status(provider: str, response: httpx.Response) -> None:
    if response.status_code >= 400:
        body = (await response.aread()).decode(errors="replace")
        raise error_for_status(provider, response.status_code, body)


def transport_error(provider: str, e: Exception) -> APIError:
    """Describes a network failure. httpx errors do not contain request headers, so no credential can leak this way."""
    return APIError(f"{provider} could not be reached: {type(e).__name__}", retryable=True)


def as_wav(frame: AudioFrame) -> bytes:
    """Wraps PCM in a WAV file, for APIs that take an uploaded file."""
    out = io.BytesIO()
    with wave.open(out, "wb") as w:
        w.setnchannels(frame.num_channels)
        w.setsampwidth(2)
        w.setframerate(frame.sample_rate)
        w.writeframes(frame.data)
    return out.getvalue()


def json_or_empty(raw: str) -> dict[str, Any]:
    try:
        value = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    return value if isinstance(value, dict) else {}


class WavHeaderSkipper:
    """Passes a byte stream through without its WAV header. A stream that does not start with one passes unchanged."""

    def __init__(self) -> None:
        self._buffer = bytearray()
        self._done = False

    def push(self, data: bytes) -> bytes:
        if self._done:
            return data
        self._buffer += data
        if len(self._buffer) < 12:
            return b""
        if bytes(self._buffer[:4]) != b"RIFF":
            self._done = True
            return bytes(self._buffer)
        position = 12
        while position + 8 <= len(self._buffer):
            size = int.from_bytes(self._buffer[position + 4 : position + 8], "little")
            if bytes(self._buffer[position : position + 4]) == b"data":
                self._done = True
                return bytes(self._buffer[position + 8 :])
            position += 8 + size + size % 2
        return b""

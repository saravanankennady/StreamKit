"""The signalling connection: protobuf over a WebSocket, on the /rtc path both servers serve."""

from __future__ import annotations

import asyncio
import logging
import platform
import time
from typing import Awaitable, Callable
from urllib.parse import urlencode

import websockets
from websockets.asyncio.client import connect

from .._proto import livekit_rtc_pb2 as rtc

logger = logging.getLogger("streamkit_agents.rtc")

PROTOCOL_VERSION = 16
SDK_VERSION = "0.1.0"

Handler = Callable[[rtc.SignalResponse], Awaitable[None]]


class SignalError(Exception):
    """The server refused the connection, or it failed."""


class SignalClient:
    def __init__(self, url: str, token: str, *, auto_subscribe: bool = True):
        self._url = _rtc_url(url, token, auto_subscribe)
        self._ws = None
        self._reader: asyncio.Task | None = None
        self._pinger: asyncio.Task | None = None
        self._handler: Handler | None = None
        self._send_lock = asyncio.Lock()
        self.on_close: Callable[[], None] | None = None

    async def connect(self, timeout: float = 15.0) -> rtc.JoinResponse:
        """Opens the socket and returns the join response, which is always the first message."""
        try:
            self._ws = await connect(self._url, open_timeout=timeout, max_size=16 << 20, ping_interval=None)
        except websockets.exceptions.InvalidStatus as e:
            body = (e.response.body or b"").decode(errors="replace").strip()
            raise SignalError(f"server refused the connection: {e.response.status_code} {body}") from e
        first = rtc.SignalResponse()
        first.ParseFromString(await asyncio.wait_for(self._ws.recv(), timeout))
        if first.WhichOneof("message") != "join":
            raise SignalError(f"expected a join response, got {first.WhichOneof('message')}")
        return first.join

    def start(self, handler: Handler, ping_interval: float) -> None:
        self._handler = handler
        self._reader = asyncio.create_task(self._read_loop())
        self._pinger = asyncio.create_task(self._ping_loop(ping_interval or 5))

    async def send(self, request: rtc.SignalRequest) -> None:
        if self._ws is None:
            raise SignalError("not connected")
        async with self._send_lock:
            await self._ws.send(request.SerializeToString())

    async def _read_loop(self) -> None:
        try:
            async for raw in self._ws:
                if isinstance(raw, str):
                    continue
                message = rtc.SignalResponse()
                message.ParseFromString(raw)
                try:
                    await self._handler(message)
                except Exception:
                    logger.exception("error handling signal message %s", message.WhichOneof("message"))
        except websockets.exceptions.ConnectionClosed:
            pass
        finally:
            if self.on_close is not None:
                self.on_close()

    async def _ping_loop(self, interval: float) -> None:
        while True:
            await asyncio.sleep(interval)
            try:
                now = int(time.time() * 1000)
                await self.send(rtc.SignalRequest(ping_req=rtc.Ping(timestamp=now)))
            except (SignalError, websockets.exceptions.ConnectionClosed):
                return

    async def close(self) -> None:
        for task in (self._pinger, self._reader):
            if task is not None and task is not asyncio.current_task():
                task.cancel()
        if self._ws is not None:
            await self._ws.close()
            self._ws = None


def _rtc_url(url: str, token: str, auto_subscribe: bool) -> str:
    base = url.rstrip("/")
    if base.startswith("http"):
        base = "ws" + base[len("http"):]
    query = urlencode(
        {
            "access_token": token,
            "auto_subscribe": "1" if auto_subscribe else "0",
            "sdk": "python",
            "version": SDK_VERSION,
            "protocol": PROTOCOL_VERSION,
            "os": platform.system().lower(),
            "os_version": platform.release(),
            "adaptive_stream": "0",
        }
    )
    return f"{base}/rtc?{query}"

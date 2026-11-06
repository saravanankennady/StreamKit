"""Local servers that replay each provider's documented wire format.

These prove the adapter speaks the protocol as written in the provider's documentation on the date in
docs/VOICE_AI_PROVIDER_MATRIX.md. They prove nothing about the provider itself, and a provider tested only this way is not
"supported": it is "written against the docs, tested against a fixture, needs a key".
"""

from __future__ import annotations

import asyncio
import base64
import json
from dataclasses import dataclass, field
from typing import Any, AsyncIterator, Awaitable, Callable
from urllib.parse import parse_qs, urlsplit

import numpy as np
from websockets.asyncio.server import serve


@dataclass
class Request:
    method: str
    path: str
    query: dict[str, list[str]]
    headers: dict[str, str]
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body)


@dataclass
class Reply:
    status: int = 200
    headers: dict[str, str] = field(default_factory=dict)
    chunks: list[bytes] = field(default_factory=list)


class HTTPFixture:
    """A small HTTP server on a real socket. The handler returns chunks, which go out as separate chunked writes, the way a
    streaming API sends them."""

    def __init__(self, handler: Callable[[Request], Awaitable[Reply] | Reply]):
        self.handler = handler
        self.requests: list[Request] = []
        self._server = None
        self.url = ""

    async def __aenter__(self) -> "HTTPFixture":
        self._server = await asyncio.start_server(self._serve, "127.0.0.1", 0)
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"http://127.0.0.1:{port}"
        return self

    async def __aexit__(self, *exc) -> None:
        self._server.close()

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        try:
            while True:
                line = await reader.readline()
                if not line:
                    return
                method, target, _ = line.decode().split(" ", 2)
                headers: dict[str, str] = {}
                while True:
                    h = await reader.readline()
                    if h in (b"\r\n", b""):
                        break
                    name, value = h.decode().split(":", 1)
                    headers[name.strip().lower()] = value.strip()
                body = await reader.readexactly(int(headers.get("content-length", 0)))
                parts = urlsplit(target)
                request = Request(method, parts.path, parse_qs(parts.query), headers, body)
                self.requests.append(request)
                result = self.handler(request)
                reply = await result if asyncio.iscoroutine(result) else result
                head = f"HTTP/1.1 {reply.status} X\r\nTransfer-Encoding: chunked\r\n"
                for k, v in reply.headers.items():
                    head += f"{k}: {v}\r\n"
                writer.write(head.encode() + b"\r\n")
                for chunk in reply.chunks:
                    writer.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
                    await writer.drain()
                    await asyncio.sleep(0)
                writer.write(b"0\r\n\r\n")
                await writer.drain()
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            writer.close()


def sse(*events: Any) -> list[bytes]:
    """Plain data-only events, with OpenAI's terminator added by the caller if wanted."""
    return [f"data: {json.dumps(e) if not isinstance(e, str) else e}\n\n".encode() for e in events]


def named_sse(*events: dict[str, Any]) -> list[bytes]:
    """Events with names, the Anthropic form, where the name repeats the type in the data."""
    return [f"event: {e['type']}\ndata: {json.dumps(e)}\n\n".encode() for e in events]


class WSFixture:
    """A WebSocket server on a real socket. Each connection runs the given coroutine."""

    def __init__(self, handler: Callable[[Any, "WSFixture"], Awaitable[None]]):
        self.handler = handler
        self.url = ""
        self.connections: list[dict[str, Any]] = []
        self._server = None

    async def __aenter__(self) -> "WSFixture":
        async def run(ws):
            request = ws.request
            info = {"path": request.path, "headers": dict(request.headers.raw_items()), "received": []}
            self.connections.append(info)
            await self.handler(ws, self)

        self._server = await serve(run, "127.0.0.1", 0, max_size=64 << 20)
        port = self._server.sockets[0].getsockname()[1]
        self.url = f"ws://127.0.0.1:{port}"
        return self

    async def __aexit__(self, *exc) -> None:
        self._server.close()
        await self._server.wait_closed()


def pcm_tone(seconds: float, rate: int, frequency: float = 220.0, amplitude: float = 0.25) -> bytes:
    t = np.arange(int(seconds * rate)) / rate
    return (np.sin(2 * np.pi * frequency * t) * amplitude * 32767).astype(np.int16).tobytes()


def energy(data: bytes) -> float:
    if not data:
        return 0.0
    samples = np.frombuffer(data, dtype=np.int16).astype(np.float64) / 32768.0
    return float(np.sqrt(np.mean(samples * samples)))


class OpenAIRealtimeFixture:
    """Speaks the Realtime API's documented events. Its server side turn detection is a loudness check, and its voice is
    whatever audio it is given, sent at about real time. It is a test of our session code, not a model.

    It follows https://developers.openai.com/api/docs/guides/realtime-conversations and the server events reference.
    """

    def __init__(self, *, reply_seconds: float = 3.0, voice: bytes | None = None, transcript: str = "hello from the fixture model", tool_call: dict[str, str] | None = None):
        self.rate = 24000
        self.voice = voice or pcm_tone(reply_seconds, self.rate)
        self.transcript = transcript
        self.tool_call = tool_call
        self.events: list[dict[str, Any]] = []
        self.session: dict[str, Any] = {}
        self.appended = bytearray()
        self.truncated: list[dict[str, Any]] = []
        self.tool_outputs: list[dict[str, Any]] = []
        self.responses = 0
        self.fixture: WSFixture | None = None
        self._stop_response: asyncio.Event | None = None
        self._vad_speaking = False
        self._vad_silence = 0.0
        self._response_task: asyncio.Task | None = None

    async def __aenter__(self) -> "OpenAIRealtimeFixture":
        self.fixture = WSFixture(self._handle)
        await self.fixture.__aenter__()
        return self

    async def __aexit__(self, *exc) -> None:
        if self._response_task is not None:
            self._response_task.cancel()
        await self.fixture.__aexit__(*exc)

    @property
    def url(self) -> str:
        return self.fixture.url + "/v1/realtime"

    @property
    def server_vad(self) -> bool:
        audio = self.session.get("audio") or {}
        return bool((audio.get("input") or {}).get("turn_detection"))

    async def _handle(self, ws, fixture: WSFixture) -> None:
        await ws.send(json.dumps({"type": "session.created", "event_id": "e0", "session": {"id": "sess_1", "object": "realtime.session"}}))
        async for raw in ws:
            event = json.loads(raw)
            self.events.append(event if event["type"] != "input_audio_buffer.append" else {"type": event["type"]})
            kind = event["type"]
            if kind == "session.update":
                self.session.update(event.get("session") or {})
                await ws.send(json.dumps({"type": "session.updated", "event_id": "e1", "session": self.session}))
            elif kind == "input_audio_buffer.append":
                data = base64.b64decode(event["audio"])
                self.appended += data
                if self.server_vad:
                    await self._server_vad(ws, data)
            elif kind == "input_audio_buffer.commit":
                await ws.send(json.dumps({"type": "input_audio_buffer.committed", "item_id": "item_user", "previous_item_id": None}))
                await ws.send(json.dumps({"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "content_index": 0, "transcript": "what the user said"}))
            elif kind == "response.create":
                self._start_response(ws, event.get("response") or {})
            elif kind == "response.cancel":
                if self._stop_response is not None:
                    self._stop_response.set()
            elif kind == "conversation.item.truncate":
                self.truncated.append(event)
            elif kind == "conversation.item.create":
                if (event.get("item") or {}).get("type") == "function_call_output":
                    self.tool_outputs.append(event["item"])

    async def _server_vad(self, ws, data: bytes) -> None:
        loud = energy(data) > 0.02
        step = len(data) / 2 / self.rate
        if loud and not self._vad_speaking:
            self._vad_speaking = True
            self._vad_silence = 0.0
            if self._stop_response is not None:
                self._stop_response.set()
            await ws.send(json.dumps({"type": "input_audio_buffer.speech_started", "audio_start_ms": 0, "item_id": "item_user"}))
        elif self._vad_speaking and not loud:
            self._vad_silence += step
            if self._vad_silence > 0.3:
                self._vad_speaking = False
                await ws.send(json.dumps({"type": "input_audio_buffer.speech_stopped", "audio_end_ms": 0, "item_id": "item_user"}))
                await ws.send(json.dumps({"type": "input_audio_buffer.committed", "item_id": "item_user", "previous_item_id": None}))
                await ws.send(json.dumps({"type": "conversation.item.input_audio_transcription.completed", "item_id": "item_user", "content_index": 0, "transcript": "what the user said"}))
                self._start_response(ws, {})
        elif self._vad_speaking and loud:
            self._vad_silence = 0.0

    def _start_response(self, ws, response: dict[str, Any]) -> None:
        self.responses += 1
        self._response_task = asyncio.create_task(self._respond(ws, f"resp_{self.responses}"))

    async def _respond(self, ws, response_id: str) -> None:
        self._stop_response = asyncio.Event()
        await ws.send(json.dumps({"type": "response.created", "response": {"id": response_id, "status": "in_progress", "output": []}}))
        if self.tool_call and not self.tool_outputs:
            call = self.tool_call
            await ws.send(json.dumps({"type": "response.function_call_arguments.done", "response_id": response_id, "item_id": "item_fn", "output_index": 0, "call_id": call["call_id"], "name": call["name"], "arguments": call["arguments"]}))
            await ws.send(json.dumps({"type": "response.done", "response": {"id": response_id, "status": "completed", "usage": {"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}}))
            return
        item_id = f"item_{response_id}"
        await ws.send(json.dumps({"type": "response.output_item.added", "response_id": response_id, "output_index": 0, "item": {"id": item_id, "type": "message", "role": "assistant"}}))
        for word in self.transcript.split():
            await ws.send(json.dumps({"type": "response.output_audio_transcript.delta", "response_id": response_id, "item_id": item_id, "output_index": 0, "content_index": 0, "delta": word + " "}))
        chunk = int(self.rate * 0.1) * 2
        status = "completed"
        for start in range(0, len(self.voice), chunk):
            if self._stop_response.is_set():
                status = "cancelled"
                break
            data = self.voice[start : start + chunk]
            await ws.send(json.dumps({"type": "response.output_audio.delta", "response_id": response_id, "item_id": item_id, "output_index": 0, "content_index": 0, "delta": base64.b64encode(data).decode()}))
            await asyncio.sleep(0.09)
        await ws.send(json.dumps({"type": "response.done", "response": {"id": response_id, "status": status, "usage": {
            "input_tokens": 100, "output_tokens": 50, "total_tokens": 150,
            "input_token_details": {"audio_tokens": 80, "text_tokens": 20, "cached_tokens": 5},
            "output_token_details": {"audio_tokens": 45, "text_tokens": 5},
        }}}))


class GeminiLiveFixture:
    """Speaks the Live API's documented messages, with the same limits as OpenAIRealtimeFixture. It follows
    https://aistudio.google.com/api-reference/live and https://aistudio.google.com/docs/live-api/capabilities."""

    def __init__(self, *, reply_seconds: float = 3.0, voice: bytes | None = None, tool_call: dict[str, Any] | None = None):
        self.out_rate = 24000
        self.voice = voice or pcm_tone(reply_seconds, self.out_rate)
        self.tool_call = tool_call
        self.setup: dict[str, Any] = {}
        self.messages: list[dict[str, Any]] = []
        self.audio_received = bytearray()
        self.tool_responses: list[dict[str, Any]] = []
        self.turns = 0
        self.fixture: WSFixture | None = None
        self._speaking = False
        self._silence = 0.0
        self._stop: asyncio.Event | None = None
        self._task: asyncio.Task | None = None

    async def __aenter__(self) -> "GeminiLiveFixture":
        self.fixture = WSFixture(self._handle)
        await self.fixture.__aenter__()
        return self

    async def __aexit__(self, *exc) -> None:
        if self._task is not None:
            self._task.cancel()
        await self.fixture.__aexit__(*exc)

    @property
    def url(self) -> str:
        return self.fixture.url

    @property
    def manual(self) -> bool:
        config = (self.setup.get("realtimeInputConfig") or {}).get("automaticActivityDetection") or {}
        return bool(config.get("disabled"))

    async def _handle(self, ws, fixture: WSFixture) -> None:
        async for raw in ws:
            message = json.loads(raw)
            if "setup" in message:
                self.setup = message["setup"]
                await ws.send(json.dumps({"setupComplete": {}}))
                continue
            realtime = message.get("realtimeInput")
            if realtime is not None:
                audio = realtime.get("audio")
                if audio:
                    data = base64.b64decode(audio["data"])
                    self.audio_received += data
                    self.messages.append({"realtimeInput": {"audio": {"mimeType": audio["mimeType"]}}})
                    if not self.manual:
                        await self._auto_vad(ws, data)
                else:
                    self.messages.append(message)
                    if "activityEnd" in realtime:
                        await self._user_turn_done(ws)
                continue
            self.messages.append(message)
            if "toolResponse" in message:
                self.tool_responses.extend(message["toolResponse"]["functionResponses"])
                self._task = asyncio.create_task(self._respond(ws))
            elif "clientContent" in message and message["clientContent"].get("turnComplete"):
                self._task = asyncio.create_task(self._respond(ws))
            elif "clientContent" in message and self._stop is not None:
                self._stop.set()

    async def _auto_vad(self, ws, data: bytes) -> None:
        loud = energy(data) > 0.02
        if loud and not self._speaking:
            self._speaking = True
            self._silence = 0.0
            if self._stop is not None and not self._stop.is_set():
                self._stop.set()
                await ws.send(json.dumps({"serverContent": {"interrupted": True}}))
        elif self._speaking and not loud:
            self._silence += len(data) / 2 / 16000
            if self._silence > 0.3:
                self._speaking = False
                await self._user_turn_done(ws)
        elif self._speaking and loud:
            self._silence = 0.0

    async def _user_turn_done(self, ws) -> None:
        await ws.send(json.dumps({"serverContent": {"inputTranscription": {"text": "what the user said", "finished": True}}}))
        self._task = asyncio.create_task(self._respond(ws))

    async def _respond(self, ws) -> None:
        self.turns += 1
        self._stop = asyncio.Event()
        if self.tool_call and not self.tool_responses:
            await ws.send(json.dumps({"toolCall": {"functionCalls": [self.tool_call]}}))
            return
        await ws.send(json.dumps({"serverContent": {"outputTranscription": {"text": "hello from the fixture model"}}}))
        chunk = int(self.out_rate * 0.1) * 2
        for start in range(0, len(self.voice), chunk):
            if self._stop.is_set():
                return
            data = self.voice[start : start + chunk]
            await ws.send(json.dumps({"serverContent": {"modelTurn": {"role": "model", "parts": [{"inlineData": {"mimeType": "audio/pcm;rate=24000", "data": base64.b64encode(data).decode()}}]}}}))
            await asyncio.sleep(0.09)
        await ws.send(json.dumps({"usageMetadata": {"promptTokenCount": 30, "responseTokenCount": 40, "totalTokenCount": 70}}))
        await ws.send(json.dumps({"serverContent": {"generationComplete": True, "turnComplete": True}}))


async def collect(stream: AsyncIterator[Any]) -> list[Any]:
    return [x async for x in stream]

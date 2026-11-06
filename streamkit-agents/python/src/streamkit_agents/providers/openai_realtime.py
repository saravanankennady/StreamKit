"""The OpenAI Realtime API over WebSocket, in its generally available shape. Written from:

- https://developers.openai.com/api/docs/guides/voice-websockets?api=realtime
- https://developers.openai.com/api/docs/guides/realtime-conversations
- https://developers.openai.com/api/reference/resources/realtime/server-events
"""

from __future__ import annotations

import asyncio
import base64
import json
import logging
from typing import Any
from urllib.parse import urlencode

import websockets
from websockets.asyncio.client import connect

from ..llm.base import APIError, Usage
from ..llm.chat import FunctionCall
from ..llm.tools import FunctionTool
from ..realtime import Generation, RealtimeCapabilities, RealtimeModel, RealtimeSession
from ..rtc.audio import AudioFrame, Resampler

logger = logging.getLogger("streamkit_agents.openai_realtime")

RATE = 24000


class OpenAIRealtimeModel(RealtimeModel):
    provider = "openai"
    input_sample_rate = RATE
    output_sample_rate = RATE

    def __init__(self, model: str = "gpt-realtime-2.1", *, api_key: str, voice: str = "marin", base_url: str = "wss://api.openai.com/v1/realtime", turn_detection: str = "semantic_vad"):
        super().__init__(model)
        self._key = api_key
        self._voice = voice
        self._url = base_url
        self._turn_detection = turn_detection

    def session(self, *, instructions: str, tools: list[FunctionTool], server_turn_detection: bool) -> "OpenAIRealtimeSession":
        return OpenAIRealtimeSession(
            url=f"{self._url}?{urlencode({'model': self.model})}",
            key=self._key,
            model=self.model,
            voice=self._voice,
            instructions=instructions,
            tools=tools,
            turn_detection={"type": self._turn_detection} if server_turn_detection else None,
        )


class OpenAIRealtimeSession(RealtimeSession):
    def __init__(self, *, url: str, key: str, model: str, voice: str, instructions: str, tools: list[FunctionTool], turn_detection: dict[str, Any] | None):
        super().__init__(RealtimeCapabilities(message_truncation=True, auto_tool_reply=False, user_transcription=True, mutable_tools=True))
        self._url = url
        self._key = key
        self._model = model
        self._voice = voice
        self._instructions = instructions
        self._tools = tools
        self._turn_detection = turn_detection
        self._ws = None
        self._reader: asyncio.Task | None = None
        self._sender: asyncio.Task | None = None
        self._outgoing: asyncio.Queue[dict[str, Any] | None] = asyncio.Queue()
        self._resampler = Resampler(RATE)
        self._generations: dict[str, Generation] = {}
        self._active: Generation | None = None
        self._ready = asyncio.Event()

    def _session_config(self) -> dict[str, Any]:
        return {
            "type": "realtime",
            "model": self._model,
            "instructions": self._instructions,
            "output_modalities": ["audio"],
            "audio": {
                "input": {
                    "format": {"type": "audio/pcm", "rate": RATE},
                    "turn_detection": self._turn_detection,
                },
                "output": {"format": {"type": "audio/pcm", "rate": RATE}, "voice": self._voice},
            },
            "tools": [t.to_openai_realtime() for t in self._tools],
            "tool_choice": "auto",
        }

    async def start(self) -> None:
        try:
            self._ws = await connect(self._url, additional_headers={"Authorization": f"Bearer {self._key}"}, max_size=64 << 20)
        except (OSError, websockets.exceptions.WebSocketException) as e:
            raise APIError(f"openai realtime connection failed: {type(e).__name__}") from e
        self._reader = asyncio.create_task(self._read_loop())
        self._sender = asyncio.create_task(self._send_loop())
        self._send({"type": "session.update", "session": self._session_config()})
        await asyncio.wait_for(self._ready.wait(), 15)
        self.emit("session_ready")

    def _send(self, event: dict[str, Any]) -> None:
        self._outgoing.put_nowait(event)

    async def _send_loop(self) -> None:
        try:
            while True:
                event = await self._outgoing.get()
                if event is None:
                    return
                await self._ws.send(json.dumps(event))
        except websockets.exceptions.ConnectionClosed:
            return

    def push_audio(self, frame: AudioFrame) -> None:
        converted = self._resampler.push(frame)
        if converted is not None and converted.data:
            self._send({"type": "input_audio_buffer.append", "audio": base64.b64encode(converted.data).decode()})

    async def commit_audio(self) -> None:
        self._send({"type": "input_audio_buffer.commit"})

    async def clear_audio(self) -> None:
        self._send({"type": "input_audio_buffer.clear"})

    async def generate_reply(self, instructions: str | None = None) -> None:
        event: dict[str, Any] = {"type": "response.create"}
        if instructions:
            event["response"] = {"instructions": instructions}
        self._send(event)

    async def interrupt(self) -> None:
        if self._active is not None:
            self._active.interrupted = True
            self._send({"type": "response.cancel"})

    async def truncate(self, item_id: str, audio_end_ms: int) -> None:
        self._send({"type": "conversation.item.truncate", "item_id": item_id, "content_index": 0, "audio_end_ms": audio_end_ms})

    async def send_tool_result(self, call: FunctionCall, output: str, *, is_error: bool = False) -> None:
        self._send({"type": "conversation.item.create", "item": {"type": "function_call_output", "call_id": call.call_id, "output": output}})

    async def update_instructions(self, instructions: str) -> None:
        self._instructions = instructions
        self._send({"type": "session.update", "session": {"type": "realtime", "instructions": instructions}})

    async def update_tools(self, tools: list[FunctionTool]) -> None:
        self._tools = tools
        self._send({"type": "session.update", "session": {"type": "realtime", "tools": [t.to_openai_realtime() for t in tools]}})

    async def _read_loop(self) -> None:
        try:
            async for raw in self._ws:
                self._handle(json.loads(raw))
        except websockets.exceptions.ConnectionClosed:
            pass
        finally:
            if self._active is not None:
                self._active._finish()
            self.emit("closed")

    def _handle(self, event: dict[str, Any]) -> None:
        kind = event.get("type", "")
        if kind == "session.updated":
            self._ready.set()
        elif kind == "input_audio_buffer.speech_started":
            self.emit("speech_started")
        elif kind == "input_audio_buffer.speech_stopped":
            self.emit("speech_stopped")
        elif kind == "conversation.item.input_audio_transcription.completed":
            self.emit("input_transcribed", event.get("transcript", ""), True)
        elif kind == "response.created":
            generation = Generation(event["response"]["id"])
            self._generations[generation.response_id] = generation
            self._active = generation
            self.emit("generation", generation)
        elif kind == "response.output_item.added":
            generation = self._generations.get(event.get("response_id", ""))
            item = event.get("item") or {}
            if generation is not None and item.get("type") == "message":
                generation.item_id = item.get("id", "")
        elif kind == "response.output_audio.delta":
            generation = self._generations.get(event.get("response_id", ""))
            if generation is not None:
                if not generation.item_id:
                    generation.item_id = event.get("item_id", "")
                generation.push_audio(AudioFrame(base64.b64decode(event["delta"]), RATE))
        elif kind == "response.output_audio_transcript.delta":
            generation = self._generations.get(event.get("response_id", ""))
            if generation is not None:
                generation.text.put_nowait(event.get("delta", ""))
        elif kind == "response.function_call_arguments.done":
            generation = self._generations.get(event.get("response_id", ""))
            if generation is not None:
                generation.function_calls.append(FunctionCall(call_id=event.get("call_id", ""), name=event.get("name", ""), arguments=event.get("arguments", "{}")))
        elif kind == "response.done":
            response = event.get("response") or {}
            generation = self._generations.pop(response.get("id", ""), None)
            if generation is not None:
                usage = response.get("usage") or {}
                input_details = usage.get("input_token_details") or {}
                output_details = usage.get("output_token_details") or {}
                generation.usage = Usage(
                    prompt_tokens=usage.get("input_tokens", 0),
                    completion_tokens=usage.get("output_tokens", 0),
                    cached_tokens=input_details.get("cached_tokens", 0),
                    audio_input_tokens=input_details.get("audio_tokens", 0),
                    audio_output_tokens=output_details.get("audio_tokens", 0),
                    raw=usage,
                )
                if response.get("status") == "cancelled":
                    generation.interrupted = True
                generation._finish()
                if self._active is generation:
                    self._active = None
        elif kind == "error":
            error = event.get("error") or {}
            # Cancelling when nothing is running is a harmless race with the reply finishing.
            if error.get("code") == "response_cancel_not_active":
                return
            self.emit("error", APIError(f"openai realtime: {error.get('code')}: {str(error.get('message'))[:300]}", retryable=False))

    async def aclose(self) -> None:
        self._outgoing.put_nowait(None)
        for task in (self._sender, self._reader):
            if task is not None:
                task.cancel()
        if self._ws is not None:
            await self._ws.close()

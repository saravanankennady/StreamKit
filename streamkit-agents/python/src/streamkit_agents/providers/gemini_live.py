"""The Gemini Live API over WebSocket. Written from:

- https://aistudio.google.com/api-reference/live
- https://aistudio.google.com/docs/live-api/capabilities
- https://aistudio.google.com/docs/live-api/tools
- https://ai.google.dev/gemini-api/docs/live-api/get-started-websocket
"""

from __future__ import annotations

import asyncio
import base64
import json
import uuid
from typing import Any
from urllib.parse import urlencode

import websockets
from websockets.asyncio.client import connect

from ..llm.base import APIError, Usage
from ..llm.chat import FunctionCall
from ..llm.tools import FunctionTool
from ..realtime import Generation, RealtimeCapabilities, RealtimeModel, RealtimeSession
from ..rtc.audio import AudioFrame, Resampler

INPUT_RATE = 16000
OUTPUT_RATE = 24000
PATH = "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"


class GeminiLiveModel(RealtimeModel):
    provider = "google"
    input_sample_rate = INPUT_RATE
    output_sample_rate = OUTPUT_RATE

    def __init__(self, model: str = "gemini-3.8-live", *, api_key: str, voice: str = "Kore", base_url: str = "wss://generativelanguage.googleapis.com"):
        super().__init__(model)
        self._key = api_key
        self._voice = voice
        self._base = base_url.rstrip("/")

    def session(self, *, instructions: str, tools: list[FunctionTool], server_turn_detection: bool) -> "GeminiLiveSession":
        return GeminiLiveSession(
            url=f"{self._base}{PATH}?{urlencode({'key': self._key})}",
            model=self.model,
            voice=self._voice,
            instructions=instructions,
            tools=tools,
            server_turn_detection=server_turn_detection,
        )


class GeminiLiveSession(RealtimeSession):
    def __init__(self, *, url: str, model: str, voice: str, instructions: str, tools: list[FunctionTool], server_turn_detection: bool):
        # Gemini hears nothing of what was played, starts its own replies after a tool result, and fixes tools at setup.
        super().__init__(RealtimeCapabilities(message_truncation=False, auto_tool_reply=True, user_transcription=True, mutable_tools=False))
        self._url = url
        self._model = model
        self._voice = voice
        self._instructions = instructions
        self._tools = tools
        self._auto_detect = server_turn_detection
        self._ws = None
        self._reader: asyncio.Task | None = None
        self._sender: asyncio.Task | None = None
        self._outgoing: asyncio.Queue[dict[str, Any] | None] = asyncio.Queue()
        self._resampler = Resampler(INPUT_RATE)
        self._active: Generation | None = None
        self._ready = asyncio.Event()
        self._activity_open = False
        self._usage: Usage | None = None

    def _setup(self) -> dict[str, Any]:
        setup: dict[str, Any] = {
            "model": f"models/{self._model}",
            "generationConfig": {
                "responseModalities": ["AUDIO"],
                "speechConfig": {"voiceConfig": {"prebuiltVoiceConfig": {"voiceName": self._voice}}},
            },
            "inputAudioTranscription": {},
            "outputAudioTranscription": {},
        }
        if self._instructions:
            setup["systemInstruction"] = {"parts": [{"text": self._instructions}]}
        if self._tools:
            setup["tools"] = [{"functionDeclarations": [t.to_google() for t in self._tools]}]
        if not self._auto_detect:
            setup["realtimeInputConfig"] = {"automaticActivityDetection": {"disabled": True}}
        return setup

    async def start(self) -> None:
        try:
            self._ws = await connect(self._url, max_size=64 << 20)
        except (OSError, websockets.exceptions.WebSocketException) as e:
            # The URL holds the key, so the message must not include it.
            raise APIError(f"gemini live connection failed: {type(e).__name__}") from None
        self._reader = asyncio.create_task(self._read_loop())
        self._sender = asyncio.create_task(self._send_loop())
        self._send({"setup": self._setup()})
        await asyncio.wait_for(self._ready.wait(), 15)
        self.emit("session_ready")

    def _send(self, message: dict[str, Any]) -> None:
        self._outgoing.put_nowait(message)

    async def _send_loop(self) -> None:
        try:
            while True:
                message = await self._outgoing.get()
                if message is None:
                    return
                await self._ws.send(json.dumps(message))
        except websockets.exceptions.ConnectionClosed:
            return

    def push_audio(self, frame: AudioFrame) -> None:
        converted = self._resampler.push(frame)
        if converted is None or not converted.data:
            return
        if not self._auto_detect and not self._activity_open:
            self._activity_open = True
            self._send({"realtimeInput": {"activityStart": {}}})
        self._send({"realtimeInput": {"audio": {"mimeType": f"audio/pcm;rate={INPUT_RATE}", "data": base64.b64encode(converted.data).decode()}}})

    async def commit_audio(self) -> None:
        if self._activity_open:
            self._activity_open = False
            self._send({"realtimeInput": {"activityEnd": {}}})

    async def clear_audio(self) -> None:
        # Gemini has no way to take back audio it has received. Nothing further is sent until the next turn.
        self._activity_open = False

    async def generate_reply(self, instructions: str | None = None) -> None:
        # Gemini replies on its own at the end of activity. A reply with nothing new comes from a client turn.
        if instructions:
            self._send({"clientContent": {"turns": [{"role": "user", "parts": [{"text": instructions}]}], "turnComplete": True}})

    async def interrupt(self) -> None:
        # Sending client content interrupts the model. An empty turn keeps the context as it was.
        if self._active is not None:
            self._active.interrupted = True
            self._send({"clientContent": {"turnComplete": False}})

    async def send_tool_result(self, call: FunctionCall, output: str, *, is_error: bool = False) -> None:
        key = "error" if is_error else "result"
        self._send({"toolResponse": {"functionResponses": [{"id": call.call_id, "name": call.name, "response": {key: output}}]}})

    async def update_instructions(self, instructions: str) -> None:
        self._instructions = instructions
        self.emit("error", APIError("gemini live fixes its instructions at setup, so this applies to the next session", retryable=False))

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

    def _generation(self) -> Generation:
        if self._active is None:
            self._active = Generation(uuid.uuid4().hex)
            self.emit("generation", self._active)
        return self._active

    def _handle(self, message: dict[str, Any]) -> None:
        if "setupComplete" in message:
            self._ready.set()
            return
        meta = message.get("usageMetadata")
        if meta:
            self._usage = Usage(
                prompt_tokens=meta.get("promptTokenCount", 0),
                completion_tokens=meta.get("responseTokenCount", meta.get("candidatesTokenCount", 0)),
                raw=meta,
            )
        activity = (message.get("voiceActivity") or {}).get("voiceActivityType") or (message.get("voiceActivity") or {}).get("type")
        if activity == "ACTIVITY_START":
            self.emit("speech_started")
        elif activity == "ACTIVITY_END":
            self.emit("speech_stopped")
        tool_call = message.get("toolCall")
        if tool_call:
            generation = self._generation()
            for call in tool_call.get("functionCalls") or []:
                generation.function_calls.append(FunctionCall(
                    call_id=call.get("id") or uuid.uuid4().hex,
                    name=call["name"],
                    arguments=json.dumps(call.get("args") or {}),
                ))
            # Nothing further comes for this reply until the tool results are sent.
            generation.usage = self._usage
            self._active = None
            generation._finish()
            return
        content = message.get("serverContent")
        if not content:
            return
        if content.get("interrupted"):
            self.emit("speech_started")
            if self._active is not None:
                self._active.interrupted = True
                generation, self._active = self._active, None
                generation.usage = self._usage
                generation._finish()
            return
        transcript = content.get("inputTranscription")
        if transcript and transcript.get("text"):
            self.emit("input_transcribed", transcript["text"], bool(transcript.get("finished")))
        output = content.get("outputTranscription")
        if output and output.get("text"):
            self._generation().text.put_nowait(output["text"])
        for part in (content.get("modelTurn") or {}).get("parts") or []:
            blob = part.get("inlineData")
            if blob and blob.get("data"):
                self._generation().push_audio(AudioFrame(base64.b64decode(blob["data"]), OUTPUT_RATE))
        if content.get("turnComplete") and self._active is not None:
            generation, self._active = self._active, None
            generation.usage = self._usage
            generation._finish()

    async def aclose(self) -> None:
        self._outgoing.put_nowait(None)
        for task in (self._sender, self._reader):
            if task is not None:
                task.cancel()
        if self._ws is not None:
            await self._ws.close()

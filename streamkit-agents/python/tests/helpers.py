"""Shared test helpers. Everything here works on real connections."""

from __future__ import annotations

import asyncio
import uuid
from dataclasses import dataclass, field
from typing import Any

import httpx
import numpy as np

from streamkit_agents.rtc import AudioFrame, AudioSource, AudioStream, Room, rms_level
from streamkit_agents.token import mint_token


def unique_room(prefix: str = "t") -> str:
    return f"{prefix}-{uuid.uuid4().hex[:10]}"


def user_token(server, room: str, identity: str = "user", **extra) -> str:
    return mint_token(server.api_key, server.api_secret, identity=identity, video={"roomJoin": True, "room": room}, **extra)


def admin_token(server, room: str) -> str:
    return mint_token(server.api_key, server.api_secret, identity="api", video={"roomAdmin": True, "room": room, "roomCreate": True, "roomList": True})


async def twirp(server, service: str, method: str, body: dict[str, Any], token: str) -> tuple[int, dict[str, Any]]:
    async with httpx.AsyncClient(timeout=10) as client:
        response = await client.post(
            f"{server.http_url}/twirp/livekit.{service}/{method}",
            json=body,
            headers={"Authorization": f"Bearer {token}"},
        )
    try:
        return response.status_code, response.json()
    except ValueError:
        return response.status_code, {"raw": response.text}


async def eventually(condition, *, timeout: float = 10.0, interval: float = 0.05, message: str = "condition") -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while True:
        if condition():
            return
        if asyncio.get_running_loop().time() > deadline:
            raise AssertionError(f"timed out waiting for {message}")
        await asyncio.sleep(interval)


@dataclass
class ReceivedAudio:
    """Audio decoded from a subscribed track, with when each frame arrived."""

    frames: list[tuple[float, bytes]] = field(default_factory=list)
    rate: int = 16000

    @property
    def pcm(self) -> bytes:
        return b"".join(f for _, f in self.frames)

    def speech_periods(self, threshold: float = 0.01, min_gap: float = 0.3) -> list[tuple[float, float]]:
        """Finds the times when the track held sound, merging short gaps."""
        periods: list[list[float]] = []
        for at, data in self.frames:
            if rms_level(data) < threshold:
                continue
            if periods and at - periods[-1][1] <= min_gap:
                periods[-1][1] = at
            else:
                periods.append([at, at])
        return [(a, b) for a, b in periods]


class TestUser:
    """A person in a room, with a real WebRTC connection. Their microphone plays what the test gives it."""

    __test__ = False

    def __init__(self, server, room: str, identity: str = "user", *, token: str | None = None):
        self.server = server
        self.room_name = room
        self.identity = identity
        self.token = token
        self.room = Room()
        self.mic = AudioSource()
        self.heard: dict[str, ReceivedAudio] = {}
        self.transcripts: list[dict[str, Any]] = []
        self._tasks: list[asyncio.Task] = []

    async def join(self, *, publish: bool = True) -> "TestUser":
        self.room.on("track_subscribed", self._on_track)
        self.room.register_text_stream_handler("lk.transcription", self._on_transcript)
        await self.room.connect(self.server.ws_url, self.token or user_token(self.server, self.room_name, self.identity))
        if publish:
            self.publication = await self.room.publish_track(self.mic, name="mic")
        return self

    def _on_track(self, track, publication, participant) -> None:
        received = self.heard.setdefault(participant.identity, ReceivedAudio())

        async def consume() -> None:
            loop = asyncio.get_running_loop()
            async for frame in AudioStream(track, received.rate):
                received.frames.append((loop.time(), frame.data))

        self._tasks.append(asyncio.create_task(consume()))

    async def _on_transcript(self, reader) -> None:
        text = await reader.read_all()
        self.transcripts.append({
            "identity": reader.info.sender_identity,
            "text": text,
            "generated": reader.info.generated,
            "attributes": dict(reader.info.attributes),
            "final_attributes": reader.final_attributes,
        })

    async def say(self, frame: AudioFrame) -> None:
        """Plays audio into the microphone, in real time."""
        step = int(frame.sample_rate * 0.05) * 2
        for start in range(0, len(frame.data), step):
            await self.mic.capture_frame(AudioFrame(frame.data[start : start + step], frame.sample_rate))
        await self.mic.wait_for_playout()

    def agent_transcripts(self) -> list[dict[str, Any]]:
        return [t for t in self.transcripts if t["identity"] != self.identity]

    async def leave(self) -> None:
        for task in self._tasks:
            task.cancel()
        await self.room.disconnect()


def tone(seconds: float, *, rate: int = 48000, frequency: float = 300.0, amplitude: float = 0.3) -> AudioFrame:
    t = np.arange(int(seconds * rate)) / rate
    return AudioFrame((np.sin(2 * np.pi * frequency * t) * amplitude * 32767).astype(np.int16).tobytes(), rate)


async def agent_is_listening(session, *, timeout: float = 20.0) -> None:
    """Waits until the user's audio is reaching the agent. A user's microphone sends silence all the time, so this does not need
    them to speak. What they say before then is lost, as it would be for any participant who has not finished joining."""
    await eventually(lambda: session.io is not None and session.io.frames_received > 0, timeout=timeout, message="the agent to be listening")

"""The interface for avatars. No vendor is implemented yet: Anam, Tavus and a self-hosted engine are later work.

An avatar worker is a participant of its own. It joins the room with a token whose kind is agent, and its attribute
lk.publish_on_behalf names the agent it speaks for. It publishes the avatar's audio and video. The agent stops publishing its
own audio and streams it to the avatar instead:

    AgentSession  -- PCM, byte stream on lk.audio_stream -->  avatar worker  -- A/V tracks -->  room

An interruption is an RPC, lk.clear_buffer, on the avatar worker. The worker answers by calling lk.playback_finished on the
agent, saying how far it got, which is how the agent knows what was really heard. A worker of ours and a vendor's would use
the same contract.

A vendor adapter implements AvatarProvider. A self-hosted engine implements VideoGenerator and runs under an avatar worker.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import AsyncIterator

from .rtc.audio import AudioFrame
from .token import mint_token

ATTR_PUBLISH_ON_BEHALF = "lk.publish_on_behalf"
AUDIO_STREAM_TOPIC = "lk.audio_stream"
RPC_CLEAR_BUFFER = "lk.clear_buffer"
RPC_PLAYBACK_STARTED = "lk.playback_started"
RPC_PLAYBACK_FINISHED = "lk.playback_finished"


@dataclass
class AvatarJoin:
    """What an avatar worker needs in order to join."""

    url: str
    token: str
    identity: str


def avatar_join(url: str, api_key: str, api_secret: str, *, room: str, identity: str, on_behalf_of: str, name: str = "", ttl: float = 3600) -> AvatarJoin:
    """Signs the token for an avatar worker. It can join and publish in one room, and nothing else."""
    token = mint_token(
        api_key,
        api_secret,
        identity=identity,
        name=name or identity,
        kind="agent",
        video={"roomJoin": True, "room": room, "canPublish": True, "canSubscribe": True, "canPublishData": True},
        attributes={ATTR_PUBLISH_ON_BEHALF: on_behalf_of},
        ttl=ttl,
    )
    return AvatarJoin(url=url, token=token, identity=identity)


class AvatarProvider(ABC):
    """A vendor's service that renders an avatar and joins the room itself."""

    name = "unknown"

    @abstractmethod
    async def start(self, join: AvatarJoin) -> None:
        """Asks the vendor to join the room with these details."""

    @abstractmethod
    async def stop(self) -> None: ...


@dataclass
class VideoFrame:
    """One frame in RGBA, for a VideoGenerator to produce."""

    width: int
    height: int
    data: bytes
    timestamp: float


class VideoGenerator(ABC):
    """A self-hosted engine: audio goes in, and audio and video come out together, which is what keeps lips in sync."""

    @abstractmethod
    async def push_audio(self, frame: AudioFrame) -> None: ...

    @abstractmethod
    async def clear_buffer(self) -> None:
        """Drops audio not yet rendered, because the user interrupted."""

    @abstractmethod
    def __aiter__(self) -> AsyncIterator[AudioFrame | VideoFrame]: ...

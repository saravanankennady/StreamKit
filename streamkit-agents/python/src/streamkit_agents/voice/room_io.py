"""Connects an AgentSession to a room: the user's microphone in, the agent's audio out, transcripts, typed chat and state.

Topic and attribute names here are part of the wire format clients read. They are not ours to rename.
"""

from __future__ import annotations

import asyncio
import logging
import uuid
from typing import TYPE_CHECKING

from .._proto import livekit_models_pb2 as models
from ..rtc.audio import AudioSource, AudioStream
from ..rtc.room import LocalTrackPublication, Participant, Room, TextStreamReader, TextStreamWriter, TrackPublication

if TYPE_CHECKING:
    from .session import AgentSession

logger = logging.getLogger("streamkit_agents.voice")

STATE_ATTRIBUTE = "lk.agent.state"
TRANSCRIPTION_TOPIC = "lk.transcription"
CHAT_TOPIC = "lk.chat"
ATTR_SEGMENT_ID = "lk.segment_id"
ATTR_FINAL = "lk.transcription_final"
ATTR_TRACK_ID = "lk.transcribed_track_id"


class RoomIO:
    def __init__(self, session: "AgentSession", room: Room, *, participant_identity: str | None = None, track_name: str = "agent_audio", close_on_disconnect: bool = True, subscriber_timeout: float = 10.0):
        self._session = session
        self.room = room
        self._wanted_identity = participant_identity
        self._track_name = track_name
        self._close_on_disconnect = close_on_disconnect
        self._subscriber_timeout = subscriber_timeout
        self.source = AudioSource()
        self.publication: LocalTrackPublication | None = None
        self.linked: Participant | None = None
        self._audio_task: asyncio.Task | None = None
        self._track_sid = ""
        # How many frames of the user's audio have arrived. A count above zero is what shows the agent can hear them.
        self.frames_received = 0

    async def start(self) -> None:
        self.room.on("track_subscribed", self._on_track_subscribed)
        self.room.on("participant_connected", self._on_participant_connected)
        self.room.on("participant_disconnected", self._on_participant_disconnected)
        self.room.on("track_unsubscribed", self._on_track_unsubscribed)
        self.room.register_text_stream_handler(CHAT_TOPIC, self._on_chat)
        self.publication = await self.room.publish_track(self.source, name=self._track_name, source=models.MICROPHONE)
        for participant in list(self.room.remote_participants.values()):
            self._on_participant_connected(participant)
            for publication in participant.tracks.values():
                if publication.track is not None:
                    self._on_track_subscribed(publication.track, publication, participant)

    def _accepts(self, participant: Participant) -> bool:
        if self._wanted_identity:
            return participant.identity == self._wanted_identity
        # Avatars and other agents are not who this agent is talking to.
        return not participant.is_agent

    def _on_participant_connected(self, participant: Participant) -> None:
        if self.linked is None and self._accepts(participant):
            self.linked = participant

    def _on_participant_disconnected(self, participant: Participant) -> None:
        if self.linked is not None and participant.sid == self.linked.sid:
            self.linked = None
            if self._audio_task is not None:
                self._audio_task.cancel()
                self._audio_task = None
            if self._close_on_disconnect:
                asyncio.ensure_future(self._session.aclose())

    def _on_track_subscribed(self, track, publication: TrackPublication, participant: Participant) -> None:
        self._on_participant_connected(participant)
        if self.linked is None or participant.sid != self.linked.sid:
            return
        if publication.kind != "audio" or publication.source != "microphone":
            return
        if self._audio_task is not None:
            self._audio_task.cancel()
        self._track_sid = publication.sid
        self._audio_task = asyncio.create_task(self._pump_audio(track))

    def _on_track_unsubscribed(self, track, publication: TrackPublication, participant: Participant) -> None:
        if publication.sid == self._track_sid and self._audio_task is not None:
            self._audio_task.cancel()
            self._audio_task = None

    async def _pump_audio(self, track) -> None:
        rate = self._session.input_sample_rate
        try:
            async for frame in AudioStream(track, rate):
                self.frames_received += 1
                self._session._push_audio(frame)
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception("reading the user's audio failed")

    async def _on_chat(self, reader: TextStreamReader) -> None:
        text = await reader.read_all()
        if text.strip():
            self._session.interrupt()
            self._session.generate_reply(user_input=text)

    async def set_state(self, state: str) -> None:
        if self.room.connected:
            await self.room.set_attributes({STATE_ATTRIBUTE: state})

    async def wait_for_subscriber(self) -> None:
        """A reply is not worth starting until someone can hear it. The server sends the signal this waits for when a
        subscriber's connection is ready."""
        if self.publication is not None:
            if not await self.publication.wait_for_subscription(self._subscriber_timeout):
                logger.warning("no subscriber to the agent's audio after %.0f s, speaking anyway", self._subscriber_timeout)

    def agent_transcript_writer(self) -> TextStreamWriter:
        return self.room.stream_text(TRANSCRIPTION_TOPIC, attributes={
            ATTR_SEGMENT_ID: uuid.uuid4().hex[:12],
            ATTR_TRACK_ID: self.publication.sid if self.publication else "",
            ATTR_FINAL: "false",
        })

    async def publish_user_transcript(self, text: str, final: bool, segment_id: str) -> None:
        """Sends what the user said, attributed to the user. The server keeps that identity for agents."""
        identity = self.linked.identity if self.linked else None
        writer = self.room.stream_text(TRANSCRIPTION_TOPIC, sender_identity=identity, attributes={
            ATTR_SEGMENT_ID: segment_id,
            ATTR_TRACK_ID: self._track_sid,
            ATTR_FINAL: "true" if final else "false",
        })
        await writer.write(text)
        await writer.close({ATTR_FINAL: "true" if final else "false"})

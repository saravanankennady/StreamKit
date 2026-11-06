"""A room participant, built on aiortc. It speaks the two peer connection form of the protocol: the client publishes on
one connection, and the server sends media and data to it on another."""

from __future__ import annotations

import asyncio
import inspect
import json
import logging
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable

from aiortc import (
    MediaStreamTrack,
    RTCConfiguration,
    RTCDataChannel,
    RTCIceServer,
    RTCPeerConnection,
    RTCRtpTransceiver,
    RTCSessionDescription,
)
from aiortc.sdp import candidate_from_sdp

from .._proto import livekit_models_pb2 as models
from .._proto import livekit_rtc_pb2 as rtc
from .audio import AudioSource
from .signal import SignalClient, SignalError

logger = logging.getLogger("streamkit_agents.rtc")

RELIABLE = "_reliable"
LOSSY = "_lossy"
# A data channel message over this size risks being dropped along the way. Streams split text to stay under it.
STREAM_CHUNK_BYTES = 15000


class Emitter:
    """Calls registered functions. A coroutine function runs as a task, so a slow handler holds nothing up."""

    def __init__(self) -> None:
        self._handlers: dict[str, list[Callable[..., Any]]] = {}
        self._tasks: set[asyncio.Task] = set()

    def on(self, event: str, fn: Callable[..., Any] | None = None) -> Any:
        if fn is None:
            def decorator(f: Callable[..., Any]) -> Callable[..., Any]:
                self.on(event, f)
                return f

            return decorator
        self._handlers.setdefault(event, []).append(fn)
        return fn

    def off(self, event: str, fn: Callable[..., Any]) -> None:
        if fn in self._handlers.get(event, []):
            self._handlers[event].remove(fn)

    def emit(self, event: str, *args: Any) -> None:
        for fn in list(self._handlers.get(event, [])):
            try:
                result = fn(*args)
            except Exception:
                logger.exception("error in %s handler", event)
                continue
            if inspect.isawaitable(result):
                task = asyncio.ensure_future(result)
                self._tasks.add(task)
                task.add_done_callback(self._finish)

    def _finish(self, task: asyncio.Task) -> None:
        self._tasks.discard(task)
        if not task.cancelled() and task.exception() is not None:
            logger.error("error in event handler", exc_info=task.exception())


@dataclass
class TrackPublication:
    sid: str
    name: str
    kind: str
    source: str
    participant_sid: str
    muted: bool = False
    track: MediaStreamTrack | None = None

    @staticmethod
    def from_info(info: models.TrackInfo, participant_sid: str) -> "TrackPublication":
        return TrackPublication(
            sid=info.sid,
            name=info.name,
            kind="audio" if info.type == models.AUDIO else "video" if info.type == models.VIDEO else "data",
            source=models.TrackSource.Name(info.source).lower(),
            participant_sid=participant_sid,
            muted=info.muted,
        )


@dataclass
class Participant:
    sid: str
    identity: str
    name: str = ""
    metadata: str = ""
    kind: str = "standard"
    attributes: dict[str, str] = field(default_factory=dict)
    tracks: dict[str, TrackPublication] = field(default_factory=dict)

    @property
    def is_agent(self) -> bool:
        return self.kind == "agent"


@dataclass
class TextStreamInfo:
    stream_id: str
    topic: str
    sender_identity: str
    attributes: dict[str, str]
    generated: bool = False


class TextStreamReader:
    """A text stream arriving from another participant."""

    def __init__(self, info: TextStreamInfo):
        self.info = info
        self._queue: asyncio.Queue[str | None] = asyncio.Queue()
        self.final_attributes: dict[str, str] = {}

    def _feed(self, text: str | None) -> None:
        self._queue.put_nowait(text)

    def __aiter__(self):
        return self._iterate()

    async def _iterate(self):
        while True:
            chunk = await self._queue.get()
            if chunk is None:
                return
            yield chunk

    async def read_all(self) -> str:
        return "".join([c async for c in self])


class TextStreamWriter:
    """Sends text in pieces as it becomes available. Closing it ends the stream for receivers."""

    def __init__(self, room: "Room", *, topic: str, sender_identity: str | None, attributes: dict[str, str], destinations: list[str] | None):
        self._room = room
        self.stream_id = uuid.uuid4().hex
        self._topic = topic
        self._sender = sender_identity or room.local_identity
        self._attributes = attributes
        self._destinations = destinations or []
        self._index = 0
        self._opened = False
        self._closed = False

    async def _packet(self, **value: Any) -> None:
        dp = models.DataPacket(
            kind=models.DataPacket.RELIABLE,
            participant_identity=self._sender,
            destination_identities=self._destinations,
            **value,
        )
        await self._room._send_packet(dp, reliable=True)

    async def open(self) -> None:
        if self._opened:
            return
        self._opened = True
        await self._packet(
            stream_header=models.DataStream.Header(
                stream_id=self.stream_id,
                timestamp=int(time.time() * 1000),
                topic=self._topic,
                mime_type="text/plain",
                attributes=self._attributes,
                text_header=models.DataStream.TextHeader(operation_type=models.DataStream.CREATE),
            )
        )

    async def write(self, text: str) -> None:
        await self.open()
        data = text.encode()
        for start in range(0, len(data), STREAM_CHUNK_BYTES):
            await self._packet(
                stream_chunk=models.DataStream.Chunk(
                    stream_id=self.stream_id,
                    chunk_index=self._index,
                    content=data[start : start + STREAM_CHUNK_BYTES],
                )
            )
            self._index += 1

    async def close(self, attributes: dict[str, str] | None = None) -> None:
        if self._closed:
            return
        self._closed = True
        await self.open()
        await self._packet(
            stream_trailer=models.DataStream.Trailer(stream_id=self.stream_id, attributes=attributes or {})
        )


class Room(Emitter):
    """Events: participant_connected, participant_disconnected, participant_attributes_changed, track_published,
    track_unpublished, track_subscribed, track_unsubscribed, local_track_subscribed, data_received, disconnected."""

    def __init__(self) -> None:
        super().__init__()
        self.name = ""
        self.sid = ""
        self.metadata = ""
        self.local: Participant | None = None
        self.remote_participants: dict[str, Participant] = {}  # by identity
        self.server_info: models.ServerInfo | None = None
        self._signal: SignalClient | None = None
        self._pub: RTCPeerConnection | None = None
        self._sub: RTCPeerConnection | None = None
        self._reliable: RTCDataChannel | None = None
        self._lossy: RTCDataChannel | None = None
        self._sub_channels: list[RTCDataChannel] = []
        self._answer: asyncio.Future | None = None
        self._pub_lock = asyncio.Lock()
        self._sub_lock = asyncio.Lock()
        self._offer_tasks: set[asyncio.Task] = set()
        self._track_published: dict[str, asyncio.Future] = {}
        self._subscribed_local: dict[str, asyncio.Event] = {}
        self._pending_tracks: dict[str, MediaStreamTrack] = {}
        self._pending_candidates: dict[str, list[str]] = {"pub": [], "sub": []}
        self._sequence = 0
        self._closed = False
        self._connected = asyncio.Event()
        self._data_ready = asyncio.Event()
        self._text_handlers: dict[str, Callable[[TextStreamReader], Any]] = {}
        self._text_streams: dict[str, TextStreamReader] = {}
        self._rpc_handlers: dict[str, Callable[[str, str], Awaitable[str]]] = {}
        self._rpc_calls: dict[str, asyncio.Future] = {}
        self.disconnect_reason: int = 0

    @property
    def local_identity(self) -> str:
        return self.local.identity if self.local else ""

    @property
    def connected(self) -> bool:
        return self._connected.is_set() and not self._closed

    async def connect(self, url: str, token: str, *, auto_subscribe: bool = True, timeout: float = 20.0) -> None:
        self._signal = SignalClient(url, token, auto_subscribe=auto_subscribe)
        self._signal.on_close = self._on_signal_closed
        join = await self._signal.connect(timeout)
        self.name = join.room.name
        self.sid = join.room.sid
        self.metadata = join.room.metadata
        self.server_info = join.server_info
        self.local = _participant_from_info(join.participant)
        for info in join.other_participants:
            self._apply_participant(info)

        # An empty configuration means no outside STUN server, which aiortc would otherwise add by default.
        config = RTCConfiguration(iceServers=[
            RTCIceServer(urls=list(s.urls), username=s.username or None, credential=s.credential or None)
            for s in join.ice_servers
        ])
        self._pub = RTCPeerConnection(config)
        self._sub = RTCPeerConnection(config)
        self._sub.on("track", self._on_remote_track)
        self._sub.on("datachannel", self._on_sub_channel)
        self._sub.on("connectionstatechange", self._on_sub_state)
        self._pub.on("connectionstatechange", self._on_pub_state)
        self._reliable = self._pub.createDataChannel(RELIABLE, ordered=True)
        self._lossy = self._pub.createDataChannel(LOSSY, ordered=False, maxRetransmits=0)
        self._reliable.on("open", self._data_ready.set)

        self._signal.start(self._on_signal, join.ping_interval)
        await self._negotiate_publisher()
        try:
            await asyncio.wait_for(self._data_ready.wait(), timeout)
        except asyncio.TimeoutError as e:
            await self.disconnect()
            raise SignalError("the publisher connection did not come up") from e
        self._connected.set()

    async def disconnect(self) -> None:
        if self._closed:
            return
        self._closed = True
        if self._signal is not None:
            try:
                await self._signal.send(rtc.SignalRequest(leave=rtc.LeaveRequest(
                    reason=models.CLIENT_INITIATED, action=rtc.LeaveRequest.DISCONNECT)))
            except Exception:
                pass
            self._signal.on_close = None
            await self._signal.close()
        for task in list(self._offer_tasks):
            task.cancel()
        for pc in (self._pub, self._sub):
            if pc is not None:
                await pc.close()
        self.emit("disconnected", self.disconnect_reason)

    def _on_signal_closed(self) -> None:
        if not self._closed:
            asyncio.ensure_future(self.disconnect())

    def _on_sub_state(self) -> None:
        logger.debug("subscriber connection %s", self._sub.connectionState)

    def _on_pub_state(self) -> None:
        logger.debug("publisher connection %s", self._pub.connectionState)
        if self._pub.connectionState == "failed" and not self._closed:
            asyncio.ensure_future(self.disconnect())

    async def _negotiate_publisher(self) -> None:
        async with self._pub_lock:
            offer = await self._pub.createOffer()
            await self._pub.setLocalDescription(offer)
            self._answer = asyncio.get_running_loop().create_future()
            await self._signal.send(rtc.SignalRequest(offer=rtc.SessionDescription(
                type="offer", sdp=self._pub.localDescription.sdp)))
            answer = await asyncio.wait_for(self._answer, 15)
            await self._pub.setRemoteDescription(RTCSessionDescription(sdp=answer.sdp, type="answer"))
            await self._flush_candidates("pub", self._pub)

    async def _on_signal(self, message: rtc.SignalResponse) -> None:
        kind = message.WhichOneof("message")
        if kind == "answer":
            if self._answer is not None and not self._answer.done():
                self._answer.set_result(message.answer)
        elif kind == "offer":
            # In its own task, because it waits for the connection, which needs the trickled candidates still to come.
            task = asyncio.create_task(self._on_offer(message.offer))
            self._offer_tasks.add(task)
            task.add_done_callback(self._offer_done)
        elif kind == "trickle":
            await self._on_trickle(message.trickle)
        elif kind == "update":
            for info in message.update.participants:
                self._apply_participant(info)
        elif kind == "track_published":
            future = self._track_published.pop(message.track_published.cid, None)
            if future is not None and not future.done():
                future.set_result(message.track_published.track)
        elif kind == "track_unpublished":
            pass
        elif kind == "track_subscribed":
            event = self._subscribed_local.setdefault(message.track_subscribed.track_sid, asyncio.Event())
            event.set()
            self.emit("local_track_subscribed", message.track_subscribed.track_sid)
        elif kind == "room_update":
            self.metadata = message.room_update.room.metadata
        elif kind == "leave":
            self.disconnect_reason = message.leave.reason
            await self.disconnect()

    async def _on_offer(self, offer: rtc.SessionDescription) -> None:
        async with self._sub_lock:
            await self._sub.setRemoteDescription(RTCSessionDescription(sdp=offer.sdp, type="offer"))
            await self._flush_candidates("sub", self._sub)
            answer = await self._sub.createAnswer()
            await self._sub.setLocalDescription(answer)
            await self._signal.send(rtc.SignalRequest(answer=rtc.SessionDescription(
                type="answer", sdp=self._sub.localDescription.sdp, id=offer.id)))
            # The next offer waits for the connection. aiortc starts a track's receiver only if DTLS has finished when it
            # applies that offer, and does not come back to it afterwards, so a track offered during the handshake would stay
            # silent for good. A server sends its next offer as soon as it has this answer, which makes that a real race.
            await self._until_connected(self._sub)

    @staticmethod
    async def _until_connected(pc: RTCPeerConnection, timeout: float = 15.0) -> None:
        deadline = asyncio.get_running_loop().time() + timeout
        while pc.connectionState not in ("connected", "failed", "closed"):
            if asyncio.get_running_loop().time() > deadline:
                logger.warning("the subscriber connection did not come up")
                return
            await asyncio.sleep(0.01)

    def _offer_done(self, task: asyncio.Task) -> None:
        self._offer_tasks.discard(task)
        if not task.cancelled() and task.exception() is not None:
            logger.error("handling an offer failed", exc_info=task.exception())

    async def _on_trickle(self, trickle: rtc.TrickleRequest) -> None:
        target = "sub" if trickle.target == rtc.SUBSCRIBER else "pub"
        pc = self._sub if target == "sub" else self._pub
        if pc is None or pc.remoteDescription is None:
            self._pending_candidates[target].append(trickle.candidateInit)
            return
        await self._add_candidate(pc, trickle.candidateInit)

    async def _flush_candidates(self, target: str, pc: RTCPeerConnection) -> None:
        pending, self._pending_candidates[target] = self._pending_candidates[target], []
        for init in pending:
            await self._add_candidate(pc, init)

    @staticmethod
    async def _add_candidate(pc: RTCPeerConnection, raw: str) -> None:
        init = json.loads(raw) if raw else {}
        text = init.get("candidate") or ""
        if not text:
            return
        candidate = candidate_from_sdp(text.split(":", 1)[1])
        candidate.sdpMid = init.get("sdpMid")
        candidate.sdpMLineIndex = init.get("sdpMLineIndex")
        try:
            await pc.addIceCandidate(candidate)
        except ValueError:
            logger.debug("ignoring candidate", exc_info=True)

    def _apply_participant(self, info: models.ParticipantInfo) -> None:
        if self.local is not None and info.identity == self.local.identity:
            self.local.attributes = dict(info.attributes)
            self.local.metadata = info.metadata
            return
        current = self.remote_participants.get(info.identity)
        if info.state == models.ParticipantInfo.DISCONNECTED:
            if current is not None and current.sid == info.sid:
                del self.remote_participants[info.identity]
                self.emit("participant_disconnected", current)
            return
        if current is None or current.sid != info.sid:
            if current is not None:
                self.emit("participant_disconnected", current)
            current = _participant_from_info(info)
            current.tracks = {}
            self.remote_participants[info.identity] = current
            self.emit("participant_connected", current)
        else:
            if dict(info.attributes) != current.attributes:
                current.attributes = dict(info.attributes)
                self.emit("participant_attributes_changed", current)
            current.metadata = info.metadata
            current.name = info.name
        seen = set()
        for t in info.tracks:
            seen.add(t.sid)
            if t.sid not in current.tracks:
                pub = TrackPublication.from_info(t, info.sid)
                current.tracks[t.sid] = pub
                self.emit("track_published", pub, current)
                pending = self._pending_tracks.pop(t.sid, None)
                if pending is not None:
                    pub.track = pending
                    self.emit("track_subscribed", pending, pub, current)
            else:
                current.tracks[t.sid].muted = t.muted
        for sid in list(current.tracks):
            if sid not in seen:
                pub = current.tracks.pop(sid)
                if pub.track is not None:
                    self.emit("track_unsubscribed", pub.track, pub, current)
                self.emit("track_unpublished", pub, current)

    def _on_remote_track(self, track: MediaStreamTrack) -> None:
        for participant in self.remote_participants.values():
            pub = participant.tracks.get(track.id)
            if pub is not None:
                pub.track = track
                self.emit("track_subscribed", track, pub, participant)
                return
        # The track can arrive before the update that says whose it is.
        self._pending_tracks[track.id] = track

    def _on_sub_channel(self, channel: RTCDataChannel) -> None:
        self._sub_channels.append(channel)
        channel.on("message", self._on_data_message)

    def _on_data_message(self, raw: bytes | str) -> None:
        if isinstance(raw, str):
            return
        dp = models.DataPacket()
        try:
            dp.ParseFromString(raw)
        except Exception:
            logger.debug("bad data packet")
            return
        value = dp.WhichOneof("value")
        if value == "stream_header":
            self._on_stream_header(dp)
        elif value == "stream_chunk":
            reader = self._text_streams.get(dp.stream_chunk.stream_id)
            if reader is not None:
                reader._feed(dp.stream_chunk.content.decode(errors="replace"))
        elif value == "stream_trailer":
            reader = self._text_streams.pop(dp.stream_trailer.stream_id, None)
            if reader is not None:
                reader.final_attributes = dict(dp.stream_trailer.attributes)
                reader._feed(None)
        elif value == "rpc_request":
            asyncio.ensure_future(self._on_rpc_request(dp))
        elif value == "rpc_ack":
            pass
        elif value == "rpc_response":
            call = self._rpc_calls.pop(dp.rpc_response.request_id, None)
            if call is not None and not call.done():
                if dp.rpc_response.WhichOneof("value") == "error":
                    call.set_exception(RpcError(dp.rpc_response.error.code, dp.rpc_response.error.message))
                else:
                    call.set_result(dp.rpc_response.payload)
        else:
            self.emit("data_received", dp)

    def _on_stream_header(self, dp: models.DataPacket) -> None:
        header = dp.stream_header
        if header.WhichOneof("content_header") != "text_header":
            return
        handler = self._text_handlers.get(header.topic)
        if handler is None:
            return
        reader = TextStreamReader(TextStreamInfo(
            stream_id=header.stream_id,
            topic=header.topic,
            sender_identity=dp.participant_identity,
            attributes=dict(header.attributes),
            generated=header.text_header.generated,
        ))
        self._text_streams[header.stream_id] = reader
        result = handler(reader)
        if inspect.isawaitable(result):
            asyncio.ensure_future(result)

    def register_text_stream_handler(self, topic: str, handler: Callable[[TextStreamReader], Any]) -> None:
        self._text_handlers[topic] = handler

    def stream_text(
        self,
        topic: str,
        *,
        sender_identity: str | None = None,
        attributes: dict[str, str] | None = None,
        destinations: list[str] | None = None,
    ) -> TextStreamWriter:
        return TextStreamWriter(self, topic=topic, sender_identity=sender_identity,
                                attributes=attributes or {}, destinations=destinations)

    async def _send_packet(self, dp: models.DataPacket, *, reliable: bool) -> None:
        channel = self._reliable if reliable else self._lossy
        if channel is None or channel.readyState != "open":
            return
        if reliable:
            self._sequence += 1
            dp.sequence = self._sequence
        if self.local is not None:
            dp.participant_sid = self.local.sid
        channel.send(dp.SerializeToString())

    async def publish_data(self, payload: bytes, *, reliable: bool = True, topic: str = "", destinations: list[str] | None = None) -> None:
        dp = models.DataPacket(
            kind=models.DataPacket.RELIABLE if reliable else models.DataPacket.LOSSY,
            participant_identity=self.local_identity,
            destination_identities=destinations or [],
            user=models.UserPacket(payload=payload, topic=topic, destination_identities=destinations or []),
        )
        await self._send_packet(dp, reliable=reliable)

    async def publish_transcription(self, participant_identity: str, track_sid: str, segments: list[models.TranscriptionSegment]) -> None:
        dp = models.DataPacket(
            kind=models.DataPacket.RELIABLE,
            participant_identity=self.local_identity,
            transcription=models.Transcription(
                transcribed_participant_identity=participant_identity,
                track_id=track_sid,
                segments=segments,
            ),
        )
        await self._send_packet(dp, reliable=True)

    def register_rpc_method(self, name: str, handler: Callable[[str, str], Awaitable[str]]) -> None:
        """The handler gets the caller's identity and the payload, and returns a payload."""
        self._rpc_handlers[name] = handler

    async def _on_rpc_request(self, dp: models.DataPacket) -> None:
        request = dp.rpc_request
        caller = dp.participant_identity
        await self._send_packet(models.DataPacket(
            kind=models.DataPacket.RELIABLE,
            destination_identities=[caller],
            rpc_ack=models.RpcAck(request_id=request.id),
        ), reliable=True)
        handler = self._rpc_handlers.get(request.method)
        response = models.RpcResponse(request_id=request.id)
        if handler is None:
            response.error.code = RpcError.UNSUPPORTED_METHOD
            response.error.message = "Method not supported at destination"
        else:
            try:
                response.payload = await handler(caller, request.payload)
            except RpcError as e:
                response.error.code = e.code
                response.error.message = e.message
            except Exception:
                logger.exception("rpc handler %s failed", request.method)
                response.error.code = RpcError.APPLICATION_ERROR
                response.error.message = "Application error in method handler"
        await self._send_packet(models.DataPacket(
            kind=models.DataPacket.RELIABLE,
            destination_identities=[caller],
            rpc_response=response,
        ), reliable=True)

    async def perform_rpc(self, destination: str, method: str, payload: str, *, timeout: float = 10.0) -> str:
        request_id = uuid.uuid4().hex
        future = asyncio.get_running_loop().create_future()
        self._rpc_calls[request_id] = future
        await self._send_packet(models.DataPacket(
            kind=models.DataPacket.RELIABLE,
            destination_identities=[destination],
            rpc_request=models.RpcRequest(
                id=request_id, method=method, payload=payload,
                response_timeout_ms=int(timeout * 1000), version=1,
            ),
        ), reliable=True)
        try:
            return await asyncio.wait_for(future, timeout)
        finally:
            self._rpc_calls.pop(request_id, None)

    async def publish_track(self, track: MediaStreamTrack, *, name: str, source: int = models.MICROPHONE, timeout: float = 15.0) -> "LocalTrackPublication":
        cid = track.id
        future = asyncio.get_running_loop().create_future()
        self._track_published[cid] = future
        await self._signal.send(rtc.SignalRequest(add_track=rtc.AddTrackRequest(
            cid=cid,
            name=name,
            type=models.AUDIO if track.kind == "audio" else models.VIDEO,
            source=source,
        )))
        info = await asyncio.wait_for(future, timeout)
        transceiver: RTCRtpTransceiver = self._pub.addTransceiver(track, direction="sendonly")
        await self._negotiate_publisher()
        return LocalTrackPublication(self, info.sid, track, transceiver)

    async def set_attributes(self, attributes: dict[str, str]) -> None:
        if self._signal is not None and not self._closed:
            await self._signal.send(rtc.SignalRequest(update_metadata=rtc.UpdateParticipantMetadata(attributes=attributes)))

    async def set_metadata(self, metadata: str) -> None:
        if self._signal is not None and not self._closed:
            await self._signal.send(rtc.SignalRequest(update_metadata=rtc.UpdateParticipantMetadata(metadata=metadata)))


@dataclass
class LocalTrackPublication:
    room: Room
    sid: str
    track: MediaStreamTrack
    transceiver: RTCRtpTransceiver

    async def wait_for_subscription(self, timeout: float | None = None) -> bool:
        """Waits until the server says someone can receive this track. Returns False on timeout."""
        event = self.room._subscribed_local.setdefault(self.sid, asyncio.Event())
        try:
            await asyncio.wait_for(event.wait(), timeout)
            return True
        except asyncio.TimeoutError:
            return False


class RpcError(Exception):
    APPLICATION_ERROR = 1500
    UNSUPPORTED_METHOD = 1400

    def __init__(self, code: int, message: str):
        super().__init__(message)
        self.code = code
        self.message = message


def _participant_from_info(info: models.ParticipantInfo) -> Participant:
    p = Participant(
        sid=info.sid,
        identity=info.identity,
        name=info.name,
        metadata=info.metadata,
        kind=models.ParticipantInfo.Kind.Name(info.kind).lower(),
        attributes=dict(info.attributes),
    )
    for t in info.tracks:
        p.tracks[t.sid] = TrackPublication.from_info(t, info.sid)
    return p


__all__ = [
    "AudioSource",
    "Emitter",
    "LocalTrackPublication",
    "Participant",
    "Room",
    "RpcError",
    "SignalError",
    "TextStreamInfo",
    "TextStreamReader",
    "TextStreamWriter",
    "TrackPublication",
]

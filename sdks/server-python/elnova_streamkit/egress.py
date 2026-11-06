"""Starts and manages recordings and stream outputs, over this server's REST API. The server needs a control plane and an
egress worker."""

import dataclasses
from dataclasses import dataclass, field
from typing import Any, BinaryIO, Optional
from urllib.parse import quote

from ._http import NotSupportedError, Transport, page_query
from .access_token import VideoGrants

ENDED_STATUSES = ("complete", "failed", "aborted")
_RECORD = VideoGrants(room_record=True)


@dataclass
class Egress:
    """A recording or streaming job. Times are RFC 3339."""

    id: str = ""
    project_id: str = ""
    room_name: str = ""
    room_sid: str = ""
    # room_composite, participant, audio_only or video_only.
    type: str = ""
    # starting, active, ending, complete, failed or aborted.
    status: str = ""
    format: str = ""
    output_url: str = ""
    stream_urls: list[str] = field(default_factory=list)
    file_size: int = 0
    duration_ms: int = 0
    error: str = ""
    created_at: str = ""
    started_at: Optional[str] = None
    ended_at: Optional[str] = None
    retry_of: Optional[str] = None

    @property
    def ended(self) -> bool:
        """Whether the job has finished, one way or another."""
        return self.status in ENDED_STATUSES


@dataclass
class EgressPage:
    items: list[Egress]
    # Fetches the next page. Empty means this is the last.
    next_cursor: str = ""


def _known(cls: type, value: dict) -> Any:
    names = {f.name for f in dataclasses.fields(cls)}
    return cls(**{k: v for k, v in value.items() if k in names})


class EgressService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    async def _job(self, verb: str, path: str, body: Any = None) -> Egress:
        return _known(Egress, await self._t.rest(verb, path, _RECORD, body))

    async def _start(
        self, kind: str, room: str, identity: Optional[str], format: Optional[str], stream_urls: Optional[list[str]], layout: Optional[str]
    ) -> Egress:
        body: dict[str, Any] = {"type": kind, "room": room}
        if identity:
            body["participant"] = identity
        if layout:
            body["layout"] = layout
        if format:
            body["output"] = {"kind": "file", "format": format}
        if stream_urls:
            body["stream_urls"] = stream_urls
        return await self._job("POST", "/egress", body)

    async def start_room_composite_egress(
        self, room: str, *, format: Optional[str] = None, stream_urls: Optional[list[str]] = None, layout: Optional[str] = None
    ) -> Egress:
        """Records or streams a whole room as one picture. Give a format (mp4 or webm) for a file, stream_urls (rtmp:// or
        rtmps://, at most five) to stream, or both. layout is grid, the default, or speaker."""
        return await self._start("room_composite", room, None, format, stream_urls, layout)

    async def start_participant_egress(
        self, room: str, identity: str, *, format: Optional[str] = None, stream_urls: Optional[list[str]] = None
    ) -> Egress:
        """Records or streams one participant."""
        return await self._start("participant", room, identity, format, stream_urls, None)

    async def start_audio_only_egress(
        self, room: str, *, identity: Optional[str] = None, format: Optional[str] = None, stream_urls: Optional[list[str]] = None
    ) -> Egress:
        """Records or streams a room's audio, or one participant's when identity is given."""
        return await self._start("audio_only", room, identity, format, stream_urls, None)

    async def start_video_only_egress(
        self, room: str, *, identity: Optional[str] = None, format: Optional[str] = None, stream_urls: Optional[list[str]] = None
    ) -> Egress:
        """Records or streams a room's video, or one participant's when identity is given."""
        return await self._start("video_only", room, identity, format, stream_urls, None)

    async def start_web_egress(self, *_: Any, **__: Any) -> Egress:
        """Not supported: this server only records rooms, not arbitrary pages."""
        raise NotSupportedError("web egress: this server does not do this")

    async def start_track_composite_egress(self, *_: Any, **__: Any) -> Egress:
        """Not supported. Use start_audio_only_egress or start_video_only_egress for one kind of media."""
        raise NotSupportedError("track composite egress: this server does not do this")

    async def start_track_egress(self, *_: Any, **__: Any) -> Egress:
        """Not supported."""
        raise NotSupportedError("track egress: this server does not do this")

    async def update_layout(self, *_: Any, **__: Any) -> Egress:
        """Not supported: a running job keeps the layout it started with."""
        raise NotSupportedError("changing a running job's layout: this server does not do this")

    async def update_stream(self, *_: Any, **__: Any) -> Egress:
        """Not supported: a running job keeps the stream outputs it started with."""
        raise NotSupportedError("changing a running job's stream outputs: this server does not do this")

    async def _page(self, path: str, limit: Optional[int], cursor: Optional[str]) -> EgressPage:
        body = await self._t.rest("GET", path + page_query(limit, cursor), _RECORD)
        return EgressPage([_known(Egress, item) for item in body["items"]], body.get("next_cursor") or "")

    async def list_egress(self, *, limit: Optional[int] = None, cursor: Optional[str] = None) -> EgressPage:
        """Lists jobs, newest first."""
        return await self._page("/egress", limit, cursor)

    async def get_egress(self, egress_id: str) -> Egress:
        """Returns one job."""
        return await self._job("GET", f"/egress/{quote(egress_id, safe='')}")

    async def stop_egress(self, egress_id: str) -> Egress:
        """Stops a running job. The file, if any, is complete once the job's status says so."""
        return await self._job("POST", f"/egress/{quote(egress_id, safe='')}/stop")

    async def list_recordings(self, *, limit: Optional[int] = None, cursor: Optional[str] = None) -> EgressPage:
        """Lists jobs with a file output."""
        return await self._page("/recordings", limit, cursor)

    async def get_recording(self, egress_id: str) -> Egress:
        """Returns one job with a file output."""
        return await self._job("GET", f"/recordings/{quote(egress_id, safe='')}")

    async def download_recording(self, egress_id: str, into: BinaryIO) -> int:
        """Writes a recording's file to an open binary file, and returns how many bytes it wrote."""
        return await self._t.rest_download(f"/recordings/{quote(egress_id, safe='')}/file", _RECORD, into)

    async def retry_recording(self, egress_id: str) -> Egress:
        """Starts a new job with the same request as a finished one."""
        return await self._job("POST", f"/recordings/{quote(egress_id, safe='')}/retry")

    async def delete_recording(self, egress_id: str) -> None:
        """Deletes a finished job and its file."""
        await self._t.rest("DELETE", f"/recordings/{quote(egress_id, safe='')}", _RECORD)

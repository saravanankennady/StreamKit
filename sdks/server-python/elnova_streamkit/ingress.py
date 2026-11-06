"""Manages ingresses, over this server's REST API. The server needs a control plane and, for a stream to actually arrive, an
ingress worker."""

import dataclasses
from dataclasses import dataclass, field
from typing import Any, Optional
from urllib.parse import quote

from ._http import Transport, page_query
from .access_token import VideoGrants

_ADMIN = VideoGrants(ingress_admin=True)


@dataclass
class IngressStats:
    """Describes a running stream."""

    bitrate_in: int = 0
    width: int = 0
    height: int = 0
    fps: float = 0
    audio_codec: str = ""
    video_codec: str = ""
    started_at: Optional[str] = None


@dataclass
class Ingress:
    """A way to bring outside media into a room. Times are RFC 3339."""

    id: str = ""
    project_id: str = ""
    name: str = ""
    # rtmp, whip or url.
    input_type: str = ""
    room_name: str = ""
    participant_identity: str = ""
    participant_name: str = ""
    source_url: str = ""
    enabled: bool = True
    # Where a publisher sends its stream.
    publish_url: str = ""
    # Only in the responses to create_ingress and rotate_ingress_key. Keep it secret.
    stream_key: str = ""
    # inactive, buffering, publishing or error.
    status: str = ""
    error: str = ""
    stats: IngressStats = field(default_factory=IngressStats)
    created_at: str = ""
    updated_at: str = ""


@dataclass
class IngressEvent:
    level: str = ""
    type: str = ""
    message: str = ""
    created_at: str = ""


def _known(cls: type, value: dict) -> Any:
    names = {f.name for f in dataclasses.fields(cls)}
    return cls(**{k: v for k, v in value.items() if k in names})


def _ingress(value: dict) -> Ingress:
    out = _known(Ingress, value)
    out.stats = _known(IngressStats, value.get("stats") or {})
    return out


class IngressService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    async def create_ingress(
        self,
        input_type: str,
        *,
        room: str,
        participant_identity: str,
        name: str = "",
        participant_name: str = "",
        source_url: str = "",
    ) -> Ingress:
        """Creates an ingress: rtmp, whip, or url, which pulls from source_url (http, https, rtmp or srt). The response holds
        the stream key, which is never shown again."""
        body = {"input_type": input_type, "room": room, "participant_identity": participant_identity}
        for key, value in (("name", name), ("participant_name", participant_name), ("source_url", source_url)):
            if value:
                body[key] = value
        return _ingress(await self._t.rest("POST", "/ingress", _ADMIN, body))

    async def list_ingress(self) -> list[Ingress]:
        """Lists ingresses."""
        return [_ingress(item) for item in (await self._t.rest("GET", "/ingress", _ADMIN))["items"]]

    async def get_ingress(self, ingress_id: str) -> Ingress:
        """Returns one ingress."""
        return _ingress(await self._t.rest("GET", f"/ingress/{quote(ingress_id, safe='')}", _ADMIN))

    async def update_ingress(
        self,
        ingress_id: str,
        *,
        name: Optional[str] = None,
        room: Optional[str] = None,
        participant_identity: Optional[str] = None,
        participant_name: Optional[str] = None,
        enabled: Optional[bool] = None,
    ) -> Ingress:
        """Changes an ingress. Fields left as None stay as they are. Changing the room or identity, or disabling it, ends a
        running stream."""
        body = {
            key: value
            for key, value in (
                ("name", name),
                ("room", room),
                ("participant_identity", participant_identity),
                ("participant_name", participant_name),
                ("enabled", enabled),
            )
            if value is not None
        }
        return _ingress(await self._t.rest("PATCH", f"/ingress/{quote(ingress_id, safe='')}", _ADMIN, body))

    async def delete_ingress(self, ingress_id: str) -> None:
        """Deletes an ingress and stops any running stream."""
        await self._t.rest("DELETE", f"/ingress/{quote(ingress_id, safe='')}", _ADMIN)

    async def stop_ingress(self, ingress_id: str) -> Ingress:
        """Stops the running stream. The ingress can be used again."""
        return _ingress(await self._t.rest("POST", f"/ingress/{quote(ingress_id, safe='')}/stop", _ADMIN))

    async def rotate_ingress_key(self, ingress_id: str) -> Ingress:
        """Replaces the stream key, and returns the new one once. The old one stops working and its stream ends."""
        return _ingress(await self._t.rest("POST", f"/ingress/{quote(ingress_id, safe='')}/rotate-key", _ADMIN))

    async def ingress_logs(self, ingress_id: str, *, limit: Optional[int] = None) -> list[IngressEvent]:
        """Returns an ingress's events, newest first."""
        body = await self._t.rest("GET", f"/ingress/{quote(ingress_id, safe='')}/logs{page_query(limit, None)}", _ADMIN)
        return [_known(IngressEvent, item) for item in body["items"]]

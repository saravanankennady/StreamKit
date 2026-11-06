"""The room API. Each call signs its own short-lived token, carrying only the grant that call needs."""

import secrets

from ._http import Transport
from .access_token import VideoGrants
from .proto.livekit_models_pb2 import ParticipantInfo, Room
from .proto.livekit_room_pb2 import (
    CreateRoomRequest,
    DeleteRoomRequest,
    DeleteRoomResponse,
    ForwardParticipantRequest,
    ForwardParticipantResponse,
    ListParticipantsRequest,
    ListParticipantsResponse,
    ListRoomsRequest,
    ListRoomsResponse,
    MoveParticipantRequest,
    MoveParticipantResponse,
    MuteRoomTrackRequest,
    MuteRoomTrackResponse,
    PerformRpcRequest,
    PerformRpcResponse,
    RemoveParticipantResponse,
    RoomParticipantIdentity,
    SendDataRequest,
    SendDataResponse,
    UpdateParticipantRequest,
    UpdateRoomMetadataRequest,
    UpdateSubscriptionsRequest,
    UpdateSubscriptionsResponse,
)

_SERVICE = "RoomService"


def _admin(room: str) -> VideoGrants:
    return VideoGrants(room_admin=True, room=room)


class RoomService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    async def create_room(self, request: CreateRoomRequest) -> Room:
        """Creates a room, or returns it unchanged if it is already open."""
        return await self._t.twirp(_SERVICE, "CreateRoom", VideoGrants(room_create=True), request, Room())

    async def list_rooms(self, request: ListRoomsRequest) -> ListRoomsResponse:
        """Lists open rooms, optionally only those named."""
        return await self._t.twirp(_SERVICE, "ListRooms", VideoGrants(room_list=True), request, ListRoomsResponse())

    async def delete_room(self, request: DeleteRoomRequest) -> DeleteRoomResponse:
        """Closes a room and disconnects everyone in it."""
        return await self._t.twirp(_SERVICE, "DeleteRoom", VideoGrants(room_create=True), request, DeleteRoomResponse())

    async def update_room_metadata(self, request: UpdateRoomMetadataRequest) -> Room:
        """Replaces a room's metadata. An empty value clears it."""
        return await self._t.twirp(_SERVICE, "UpdateRoomMetadata", _admin(request.room), request, Room())

    async def list_participants(self, request: ListParticipantsRequest) -> ListParticipantsResponse:
        """Lists a room's participants. An unknown room has none, and that is not an error."""
        return await self._t.twirp(_SERVICE, "ListParticipants", _admin(request.room), request, ListParticipantsResponse())

    async def get_participant(self, request: RoomParticipantIdentity) -> ParticipantInfo:
        """Returns one participant."""
        return await self._t.twirp(_SERVICE, "GetParticipant", _admin(request.room), request, ParticipantInfo())

    async def remove_participant(self, request: RoomParticipantIdentity) -> RemoveParticipantResponse:
        """Disconnects a participant."""
        return await self._t.twirp(_SERVICE, "RemoveParticipant", _admin(request.room), request, RemoveParticipantResponse())

    async def mute_published_track(self, request: MuteRoomTrackRequest) -> MuteRoomTrackResponse:
        """Mutes a participant's track. Unmuting needs room.enable_remote_unmute on the server."""
        return await self._t.twirp(_SERVICE, "MutePublishedTrack", _admin(request.room), request, MuteRoomTrackResponse())

    async def update_participant(self, request: UpdateParticipantRequest) -> ParticipantInfo:
        """Changes a participant's metadata, name, attributes or permissions. Empty metadata and name mean unchanged. A
        permission replaces the whole set."""
        return await self._t.twirp(_SERVICE, "UpdateParticipant", _admin(request.room), request, ParticipantInfo())

    async def update_subscriptions(self, request: UpdateSubscriptionsRequest) -> UpdateSubscriptionsResponse:
        """Subscribes a participant to tracks, or unsubscribes them."""
        return await self._t.twirp(_SERVICE, "UpdateSubscriptions", _admin(request.room), request, UpdateSubscriptionsResponse())

    async def send_data(self, request: SendDataRequest) -> SendDataResponse:
        """Sends a data packet to a room, or to the identities named."""
        if not request.nonce:
            # Lets a client spot a packet it has already seen, when a call is retried.
            request.nonce = secrets.token_bytes(16)
        return await self._t.twirp(_SERVICE, "SendData", _admin(request.room), request, SendDataResponse())

    async def forward_participant(self, request: ForwardParticipantRequest) -> ForwardParticipantResponse:
        """Not implemented by this server, or by the open-source reference server."""
        grant = VideoGrants(room_admin=True, room=request.room, destination_room=request.destination_room)
        return await self._t.twirp(_SERVICE, "ForwardParticipant", grant, request, ForwardParticipantResponse())

    async def move_participant(self, request: MoveParticipantRequest) -> MoveParticipantResponse:
        """Not implemented by this server, or by the open-source reference server."""
        grant = VideoGrants(room_admin=True, room=request.room, destination_room=request.destination_room)
        return await self._t.twirp(_SERVICE, "MoveParticipant", grant, request, MoveParticipantResponse())

    async def perform_rpc(self, request: PerformRpcRequest) -> PerformRpcResponse:
        """Asks a participant to run a method. Not implemented by this server yet: it fails with the code unimplemented."""
        return await self._t.twirp(_SERVICE, "PerformRpc", _admin(request.room), request, PerformRpcResponse())

"""Asks for agents to be sent to rooms, over the same Twirp service the reference server has. This server does not have that
service yet, so each call raises NotSupportedError here."""

from ._http import Transport
from .access_token import VideoGrants
from .proto.livekit_agent_dispatch_pb2 import (
    AgentDispatch,
    CreateAgentDispatchRequest,
    DeleteAgentDispatchRequest,
    ListAgentDispatchRequest,
    ListAgentDispatchResponse,
)

_SERVICE = "AgentDispatchService"


def _admin(room: str) -> VideoGrants:
    return VideoGrants(room_admin=True, room=room)


class AgentDispatchService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    async def create_dispatch(self, request: CreateAgentDispatchRequest) -> AgentDispatch:
        """Sends an agent to a room."""
        return await self._t.twirp(_SERVICE, "CreateDispatch", _admin(request.room), request, AgentDispatch())

    async def delete_dispatch(self, request: DeleteAgentDispatchRequest) -> AgentDispatch:
        """Cancels a dispatch."""
        return await self._t.twirp(_SERVICE, "DeleteDispatch", _admin(request.room), request, AgentDispatch())

    async def list_dispatch(self, request: ListAgentDispatchRequest) -> ListAgentDispatchResponse:
        """Lists a room's dispatches."""
        return await self._t.twirp(_SERVICE, "ListDispatch", _admin(request.room), request, ListAgentDispatchResponse())

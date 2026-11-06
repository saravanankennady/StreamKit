"""Python server SDK for Elnova StreamKit: access tokens, webhook checks, and clients for rooms, recording and ingest.

The wire types are in elnova_streamkit.proto, named as the protocol names them.
"""

import os
from types import TracebackType
from typing import Optional

import aiohttp

from ._http import NotSupportedError, ServerError, Transport
from .access_token import (
    AccessToken,
    AgentEndpointGrants,
    AgentGrants,
    AgentThreadGrants,
    Claims,
    InferenceGrants,
    ObservabilityGrants,
    SIPGrants,
    TokenVerifier,
    VideoGrants,
)
from .agent_dispatch import AgentDispatchService
from .egress import Egress, EgressPage, EgressService
from .ingress import Ingress, IngressEvent, IngressService, IngressStats
from .room import RoomService
from .webhook import WebhookError, WebhookReceiver, sign_webhook

__version__ = "0.1.0"

__all__ = [
    "AccessToken",
    "AgentDispatchService",
    "AgentEndpointGrants",
    "AgentGrants",
    "AgentThreadGrants",
    "Claims",
    "Egress",
    "EgressPage",
    "EgressService",
    "InferenceGrants",
    "Ingress",
    "IngressEvent",
    "IngressService",
    "IngressStats",
    "NotSupportedError",
    "ObservabilityGrants",
    "RoomService",
    "SIPGrants",
    "ServerError",
    "StreamKitAPI",
    "TokenVerifier",
    "VideoGrants",
    "WebhookError",
    "WebhookReceiver",
    "sign_webhook",
]


class StreamKitAPI:
    """Clients for all the services of one server. Use it as an async context manager, or call aclose when finished.

    url may be the ws:// address clients connect to. url, api_key and api_secret default to STREAMKIT_URL,
    STREAMKIT_API_KEY and STREAMKIT_API_SECRET. Pass token to send that token on every call, instead of signing one per
    call with only the grants that call needs. You are then responsible for its grants.
    """

    def __init__(
        self,
        url: Optional[str] = None,
        api_key: Optional[str] = None,
        api_secret: Optional[str] = None,
        *,
        token: Optional[str] = None,
        timeout: float = 30,
        session: Optional[aiohttp.ClientSession] = None,
    ) -> None:
        url = url or os.getenv("STREAMKIT_URL")
        if not url:
            raise ValueError("a server address is required")
        api_key = api_key or os.getenv("STREAMKIT_API_KEY")
        api_secret = api_secret or os.getenv("STREAMKIT_API_SECRET")
        if not token and (not api_key or not api_secret):
            raise ValueError("an API key and secret, or a token, are required")
        self._owns_session = session is None
        self._session = session or aiohttp.ClientSession()
        transport = Transport(self._session, url, api_key, api_secret, token, timeout)
        self.room = RoomService(transport)
        self.egress = EgressService(transport)
        self.ingress = IngressService(transport)
        self.agent_dispatch = AgentDispatchService(transport)

    async def aclose(self) -> None:
        if self._owns_session:
            await self._session.close()

    async def __aenter__(self) -> "StreamKitAPI":
        return self

    async def __aexit__(
        self,
        exc_type: Optional[type[BaseException]],
        exc: Optional[BaseException],
        tb: Optional[TracebackType],
    ) -> None:
        await self.aclose()

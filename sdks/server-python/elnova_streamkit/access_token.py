"""Access tokens: HS256 JWTs with the API key as their issuer."""

import dataclasses
import datetime
import os
import re
import time
from dataclasses import dataclass, field
from typing import Literal, Optional, TypeVar

import jwt
from google.protobuf.json_format import MessageToDict, ParseDict

from .proto.livekit_room_pb2 import RoomConfiguration

DEFAULT_TTL = datetime.timedelta(hours=6)
# Clock difference tolerated between whoever signed a token and whoever checks it. Servers allow the same.
DEFAULT_LEEWAY = datetime.timedelta(minutes=1)


@dataclass
class VideoGrants:
    """A token's room permissions. A field left as None is not written to the token."""

    room_create: Optional[bool] = None
    room_list: Optional[bool] = None
    room_record: Optional[bool] = None

    room_admin: Optional[bool] = None
    room_join: Optional[bool] = None
    room: Optional[str] = None

    # Granted when unset.
    can_publish: Optional[bool] = None
    can_subscribe: Optional[bool] = None
    # Follows can_publish when unset.
    can_publish_data: Optional[bool] = None
    # Limits publishing to these sources: camera, microphone, screen_share, screen_share_audio. Empty allows all of them.
    # It cannot allow what can_publish denies.
    can_publish_sources: Optional[list[str]] = None
    # Denied when unset.
    can_update_own_metadata: Optional[bool] = None

    ingress_admin: Optional[bool] = None

    hidden: Optional[bool] = None
    recorder: Optional[bool] = None
    agent: Optional[bool] = None

    can_subscribe_metrics: Optional[bool] = None
    can_manage_agent_session: Optional[bool] = None

    # The room this token may forward or move a participant to.
    destination_room: Optional[str] = None


@dataclass
class SIPGrants:
    """Permission to manage SIP resources and place calls. This server has no SIP service, so it does not act on this yet."""

    admin: Optional[bool] = None
    call: Optional[bool] = None


@dataclass
class AgentGrants:
    """Permission over hosted agents. This server does not act on this yet."""

    admin: Optional[bool] = None
    simulation_admin: Optional[bool] = None
    database_admin: Optional[bool] = None
    dispatch_admin: Optional[bool] = None


@dataclass
class InferenceGrants:
    """Permission to call hosted inference. This server does not act on this."""

    perform: Optional[bool] = None


@dataclass
class ObservabilityGrants:
    """Permission to publish observability data. This server does not act on this."""

    write: Optional[bool] = None


@dataclass
class AgentEndpointGrants:
    """Permission to call an agent's HTTP endpoints. This server does not act on this."""

    call: Optional[bool] = None
    agent_name: Optional[str] = None
    deployment: Optional[str] = None


@dataclass
class AgentThreadGrants:
    """Permission over agent threads. This server does not act on this."""

    all: Optional[bool] = None
    scope: Optional[str] = None
    thread_ids: Optional[list[str]] = None
    list: Optional[bool] = None
    create: Optional[bool] = None
    write: Optional[bool] = None
    read: Optional[bool] = None
    delete: Optional[bool] = None


@dataclass
class Claims:
    """Everything a token says about its holder, apart from the JWT's own fields."""

    identity: str = ""
    name: str = ""
    kind: str = ""
    kind_details: Optional[list[str]] = None
    metadata: str = ""
    attributes: dict[str, str] = field(default_factory=dict)
    video: Optional[VideoGrants] = None
    sip: Optional[SIPGrants] = None
    agent: Optional[AgentGrants] = None
    inference: Optional[InferenceGrants] = None
    observability: Optional[ObservabilityGrants] = None
    agent_endpoint: Optional[AgentEndpointGrants] = None
    agent_thread: Optional[AgentThreadGrants] = None
    # The configuration a room gets if this token's join creates it.
    room_config: Optional[RoomConfiguration] = None
    # Names a stored room configuration.
    room_preset: str = ""
    sha256: str = ""


def _lower_camel(name: str) -> str:
    first, *rest = name.split("_")
    return first + "".join(word.capitalize() for word in rest)


def _snake(name: str) -> str:
    return re.sub(r"(?<!^)(?=[A-Z])", "_", name).lower()


def _grant_to_json(grant: object) -> dict:
    return {_lower_camel(f.name): getattr(grant, f.name) for f in dataclasses.fields(grant) if getattr(grant, f.name) is not None}


_G = TypeVar("_G")


def _grant_from_json(cls: type[_G], value: Optional[dict]) -> Optional[_G]:
    if value is None:
        return None
    known = {f.name for f in dataclasses.fields(cls)}
    return cls(**{_snake(k): v for k, v in value.items() if _snake(k) in known})


class AccessToken:
    """Builds a signed token. API key and secret default to STREAMKIT_API_KEY and STREAMKIT_API_SECRET."""

    ParticipantKind = Literal["standard", "ingress", "egress", "sip", "agent", "connector", "bridge"]

    def __init__(self, api_key: Optional[str] = None, api_secret: Optional[str] = None) -> None:
        self.api_key = api_key or os.getenv("STREAMKIT_API_KEY")
        self.api_secret = api_secret or os.getenv("STREAMKIT_API_SECRET")
        if not self.api_key or not self.api_secret:
            raise ValueError("an API key and secret are required")
        self.claims = Claims()
        self.ttl = DEFAULT_TTL

    def with_identity(self, identity: str) -> "AccessToken":
        self.claims.identity = identity
        return self

    def with_ttl(self, ttl: datetime.timedelta) -> "AccessToken":
        self.ttl = ttl
        return self

    def with_grants(self, grants: VideoGrants) -> "AccessToken":
        self.claims.video = grants
        return self

    def with_sip_grants(self, grants: SIPGrants) -> "AccessToken":
        self.claims.sip = grants
        return self

    def with_agent_grants(self, grants: AgentGrants) -> "AccessToken":
        self.claims.agent = grants
        return self

    def with_inference_grants(self, grants: InferenceGrants) -> "AccessToken":
        self.claims.inference = grants
        return self

    def with_observability_grants(self, grants: ObservabilityGrants) -> "AccessToken":
        self.claims.observability = grants
        return self

    def with_agent_endpoint_grants(self, grants: AgentEndpointGrants) -> "AccessToken":
        self.claims.agent_endpoint = grants
        return self

    def with_agent_thread_grants(self, grants: AgentThreadGrants) -> "AccessToken":
        self.claims.agent_thread = grants
        return self

    def with_kind(self, kind: ParticipantKind) -> "AccessToken":
        self.claims.kind = kind
        return self

    def with_kind_details(self, details: list[str]) -> "AccessToken":
        self.claims.kind_details = details
        return self

    def with_name(self, name: str) -> "AccessToken":
        self.claims.name = name
        return self

    def with_metadata(self, metadata: str) -> "AccessToken":
        self.claims.metadata = metadata
        return self

    def with_attributes(self, attributes: dict[str, str]) -> "AccessToken":
        self.claims.attributes = attributes
        return self

    def with_sha256(self, sha256: str) -> "AccessToken":
        self.claims.sha256 = sha256
        return self

    def with_room_preset(self, preset: str) -> "AccessToken":
        self.claims.room_preset = preset
        return self

    def with_room_config(self, config: RoomConfiguration) -> "AccessToken":
        self.claims.room_config = config
        return self

    def to_jwt(self) -> str:
        """Signs the token."""
        c = self.claims
        if c.video and c.video.room_join and (not c.identity or not c.video.room):
            raise ValueError("a token that joins a room needs an identity and a room")
        now = int(time.time())
        payload: dict = {"iss": self.api_key, "nbf": now, "iat": now, "exp": now + int(self.ttl.total_seconds())}
        if c.identity:
            payload["sub"] = c.identity
        for name in ("name", "kind", "metadata", "sha256"):
            if value := getattr(c, name):
                payload[name] = value
        if c.kind_details:
            payload["kindDetails"] = c.kind_details
        if c.attributes:
            payload["attributes"] = c.attributes
        if c.room_preset:
            payload["roomPreset"] = c.room_preset
        for key, grant in (
            ("video", c.video),
            ("sip", c.sip),
            ("agent", c.agent),
            ("inference", c.inference),
            ("observability", c.observability),
            ("agentEndpoint", c.agent_endpoint),
            ("agentThread", c.agent_thread),
        ):
            if grant is not None:
                payload[key] = _grant_to_json(grant)
        if c.room_config is not None:
            payload["roomConfig"] = MessageToDict(c.room_config)
        return jwt.encode(payload, self.api_secret, algorithm="HS256")


class TokenVerifier:
    """Checks tokens signed with one key, the same checks a server makes."""

    def __init__(
        self,
        api_key: Optional[str] = None,
        api_secret: Optional[str] = None,
        *,
        leeway: datetime.timedelta = DEFAULT_LEEWAY,
    ) -> None:
        self.api_key = api_key or os.getenv("STREAMKIT_API_KEY")
        self.api_secret = api_secret or os.getenv("STREAMKIT_API_SECRET")
        if not self.api_key or not self.api_secret:
            raise ValueError("an API key and secret are required")
        self._leeway = leeway

    def verify(self, token: str) -> Claims:
        """Checks a token's signature, issuer and validity window, and returns its claims. It takes HS256 only, requires
        an expiry, and ignores claims it does not know. It raises jwt.InvalidTokenError if the token fails."""
        c = jwt.decode(
            token,
            key=self.api_secret,
            issuer=self.api_key,
            algorithms=["HS256"],
            leeway=self._leeway,
            options={"require": ["exp", "iss"], "verify_aud": False},
        )
        room_config = None
        if c.get("roomConfig"):
            room_config = ParseDict(c["roomConfig"], RoomConfiguration(), ignore_unknown_fields=True)
        return Claims(
            identity=c.get("sub") or c.get("jti") or "",
            name=c.get("name", ""),
            kind=c.get("kind", ""),
            kind_details=c.get("kindDetails"),
            metadata=c.get("metadata", ""),
            attributes=c.get("attributes") or {},
            video=_grant_from_json(VideoGrants, c.get("video")),
            sip=_grant_from_json(SIPGrants, c.get("sip")),
            agent=_grant_from_json(AgentGrants, c.get("agent")),
            inference=_grant_from_json(InferenceGrants, c.get("inference")),
            observability=_grant_from_json(ObservabilityGrants, c.get("observability")),
            agent_endpoint=_grant_from_json(AgentEndpointGrants, c.get("agentEndpoint")),
            agent_thread=_grant_from_json(AgentThreadGrants, c.get("agentThread")),
            room_config=room_config,
            room_preset=c.get("roomPreset", ""),
            sha256=c.get("sha256", ""),
        )

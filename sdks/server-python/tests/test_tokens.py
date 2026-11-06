"""Tokens and webhooks with no server, including both directions against the official Python SDK, as a test-only comparison.
Importing both in one process is also a test: this package keeps its wire types in its own descriptor pool for that."""

import asyncio
import base64
import datetime
import hashlib
import io
import json
import time

import jwt
import pytest
from aiohttp import web
from livekit import api as official

from elnova_streamkit import (
    AccessToken,
    AgentEndpointGrants,
    AgentGrants,
    AgentThreadGrants,
    InferenceGrants,
    ObservabilityGrants,
    SIPGrants,
    StreamKitAPI,
    TokenVerifier,
    VideoGrants,
    WebhookError,
    WebhookReceiver,
    sign_webhook,
)
from elnova_streamkit.proto.livekit_room_pb2 import RoomConfiguration

KEY = "testkey"
SECRET = "testsecret-that-is-long-enough-for-tests"

EVERY_VIDEO_GRANT = VideoGrants(
    room_create=True,
    room_list=True,
    room_record=True,
    room_admin=True,
    room_join=True,
    room="r",
    can_publish=False,
    can_subscribe=True,
    can_publish_data=False,
    can_publish_sources=["camera", "screen_share_audio"],
    can_update_own_metadata=True,
    ingress_admin=True,
    hidden=True,
    recorder=True,
    agent=True,
    can_subscribe_metrics=True,
    can_manage_agent_session=True,
    destination_room="d",
)


def full_token() -> AccessToken:
    return (
        AccessToken(KEY, SECRET)
        .with_identity("alice")
        .with_name("Alice")
        .with_metadata("m")
        .with_attributes({"a": "1"})
        .with_kind("agent")
        .with_kind_details(["cloud_agent"])
        .with_grants(EVERY_VIDEO_GRANT)
        .with_sip_grants(SIPGrants(admin=True, call=True))
        .with_agent_grants(AgentGrants(admin=True, simulation_admin=True, database_admin=True, dispatch_admin=True))
        .with_inference_grants(InferenceGrants(perform=True))
        .with_observability_grants(ObservabilityGrants(write=True))
        .with_agent_endpoint_grants(AgentEndpointGrants(call=True, agent_name="n", deployment="prod"))
        .with_agent_thread_grants(AgentThreadGrants(all=True, scope="s", thread_ids=["t"], list=True, create=True, write=True, read=True, delete=True))
        .with_room_config(RoomConfiguration(name="r", empty_timeout=11, departure_timeout=12, max_participants=13, metadata="cfg"))
        .with_room_preset("preset")
        .with_sha256("abc")
    )


def decoded(token: str) -> dict:
    return jwt.decode(token, options={"verify_signature": False})


def sign(claims: dict, algorithm: str = "HS256", secret: str = SECRET) -> str:
    return jwt.encode(claims, secret, algorithm=algorithm)


def future(seconds: int = 3600) -> int:
    return int(time.time()) + seconds


def test_every_grant_is_written_under_its_wire_name() -> None:
    token = full_token().to_jwt()
    assert jwt.get_unverified_header(token)["alg"] == "HS256"
    claims = decoded(token)
    for name in (
        "sub", "iss", "exp", "nbf", "iat", "name", "kind", "kindDetails", "metadata", "attributes", "video", "sip", "agent", "inference",
        "observability", "agentEndpoint", "agentThread", "roomConfig", "roomPreset", "sha256",
    ):  # fmt: skip
        assert name in claims, name
    assert claims["sub"] == "alice"
    assert claims["iss"] == KEY
    assert claims["video"] == {
        "roomCreate": True, "roomList": True, "roomRecord": True, "roomAdmin": True, "roomJoin": True, "room": "r", "canPublish": False,
        "canSubscribe": True, "canPublishData": False, "canPublishSources": ["camera", "screen_share_audio"], "canUpdateOwnMetadata": True,
        "ingressAdmin": True, "hidden": True, "recorder": True, "agent": True, "canSubscribeMetrics": True, "canManageAgentSession": True,
        "destinationRoom": "d",
    }  # fmt: skip
    assert claims["agentEndpoint"] == {"call": True, "agentName": "n", "deployment": "prod"}
    assert claims["agentThread"]["threadIds"] == ["t"]
    assert claims["roomConfig"]["emptyTimeout"] == 11
    assert claims["roomConfig"]["maxParticipants"] == 13


def test_lifetime() -> None:
    claims = decoded(AccessToken(KEY, SECRET).with_identity("a").with_ttl(datetime.timedelta(minutes=1)).to_jwt())
    assert claims["exp"] - claims["nbf"] == 60
    claims = decoded(AccessToken(KEY, SECRET).with_identity("a").to_jwt())
    assert claims["exp"] - claims["nbf"] == 6 * 3600


def test_refuses_what_cannot_work() -> None:
    with pytest.raises(ValueError):
        AccessToken("", SECRET)
    with pytest.raises(ValueError):
        AccessToken(KEY, "")
    with pytest.raises(ValueError):
        AccessToken(KEY, SECRET).with_grants(VideoGrants(room_join=True, room="r")).to_jwt()
    with pytest.raises(ValueError):
        AccessToken(KEY, SECRET).with_identity("a").with_grants(VideoGrants(room_join=True)).to_jwt()


def test_unset_permissions_are_left_out() -> None:
    # Unset and false mean different things on a server, so nothing may be written that the caller did not set.
    claims = decoded(AccessToken(KEY, SECRET).with_identity("a").with_grants(VideoGrants(room_join=True, room="r")).to_jwt())
    assert claims["video"] == {"roomJoin": True, "room": "r"}


def test_verify_returns_what_a_token_says() -> None:
    claims = TokenVerifier(KEY, SECRET).verify(full_token().to_jwt())
    assert claims.identity == "alice"
    assert claims.name == "Alice"
    assert claims.video == EVERY_VIDEO_GRANT
    assert claims.sip == SIPGrants(admin=True, call=True)
    assert claims.agent.dispatch_admin is True
    assert claims.inference.perform is True
    assert claims.observability.write is True
    assert claims.agent_endpoint.agent_name == "n"
    assert claims.agent_thread.thread_ids == ["t"]
    assert claims.room_config.max_participants == 13
    assert claims.room_preset == "preset"
    assert claims.attributes == {"a": "1"}


@pytest.mark.parametrize(
    "claims,good",
    [
        (lambda: {"iss": KEY, "sub": "a", "exp": future()}, True),
        (lambda: {"iss": KEY, "sub": "a", "exp": future(-50)}, True),
        (lambda: {"iss": KEY, "sub": "a", "exp": future(-70)}, False),
        (lambda: {"iss": KEY, "sub": "a", "exp": future(), "nbf": future(50)}, True),
        (lambda: {"iss": KEY, "sub": "a", "exp": future(), "nbf": future(70)}, False),
        (lambda: {"iss": KEY, "sub": "a"}, False),
        (lambda: {"iss": "other", "sub": "a", "exp": future()}, False),
        (lambda: {"sub": "a", "exp": future()}, False),
    ],
)
def test_verify_applies_the_same_rules_as_a_server(claims, good: bool) -> None:
    verifier = TokenVerifier(KEY, SECRET)
    token = sign(claims())
    if good:
        assert verifier.verify(token).identity == "a"
    else:
        with pytest.raises(jwt.InvalidTokenError):
            verifier.verify(token)


def test_verify_refuses_other_signatures() -> None:
    claims = {"iss": KEY, "sub": "a", "exp": future()}
    verifier = TokenVerifier(KEY, SECRET)
    for token in (
        sign(claims, secret="another secret that is long enough"),
        sign(claims, algorithm="HS384", secret=SECRET),
        jwt.encode(claims, key="", algorithm="none"),
        "not.a.token",
    ):
        with pytest.raises(jwt.InvalidTokenError):
            verifier.verify(token)


def test_verify_takes_identity_from_jti_and_ignores_unknown_claims() -> None:
    claims = TokenVerifier(KEY, SECRET).verify(
        sign({"iss": KEY, "jti": "from-jti", "exp": future(), "aud": ["a"], "somethingNew": {"x": 1}, "roomConfig": {"empty_timeout": 5, "maxParticipants": 6, "fieldFromTheFuture": True}})
    )
    assert claims.identity == "from-jti"
    assert claims.video is None
    assert claims.room_config.empty_timeout == 5
    assert claims.room_config.max_participants == 6


async def test_downloads_outlast_the_timeout_for_api_calls() -> None:
    async def handler(request: web.Request) -> web.StreamResponse:
        response = web.StreamResponse()
        await response.prepare(request)
        for _ in range(5):
            await response.write(b"part")
            await asyncio.sleep(0.1)
        await response.write_eof()
        return response

    app = web.Application()
    app.router.add_get("/{tail:.*}", handler)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    try:
        async with StreamKitAPI(f"http://127.0.0.1:{port}", KEY, SECRET, timeout=0.2) as api:
            into = io.BytesIO()
            assert await api.egress.download_recording("EG_1", into) == 20
            assert into.getvalue() == b"part" * 5
            with pytest.raises(asyncio.TimeoutError):
                await api.egress.get_egress("EG_1")
    finally:
        await runner.cleanup()


async def test_the_environment_supplies_the_server_and_credentials(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("STREAMKIT_URL", raising=False)
    monkeypatch.delenv("STREAMKIT_API_KEY", raising=False)
    monkeypatch.delenv("STREAMKIT_API_SECRET", raising=False)
    with pytest.raises(ValueError, match="server address"):
        StreamKitAPI()
    with pytest.raises(ValueError, match="API key"):
        StreamKitAPI("ws://example.invalid")
    monkeypatch.setenv("STREAMKIT_URL", "wss://example.invalid")
    monkeypatch.setenv("STREAMKIT_API_KEY", KEY)
    monkeypatch.setenv("STREAMKIT_API_SECRET", SECRET)
    async with StreamKitAPI() as api:
        assert api.room is not None
    assert TokenVerifier().verify(AccessToken().with_identity("a").to_jwt()).identity == "a"


def test_official_tokens_verify_here() -> None:
    theirs = (
        official.AccessToken(KEY, SECRET)
        .with_identity("bob")
        .with_name("Bob")
        .with_metadata("m")
        .with_attributes({"a": "1"})
        .with_kind("agent")
        .with_room_preset("p")
        .with_grants(official.VideoGrants(room_join=True, room="r", room_admin=True, can_publish=False))
        .with_sip_grants(official.SIPGrants(admin=True, call=True))
        .with_inference_grants(official.InferenceGrants(perform=True))
        .with_observability_grants(official.ObservabilityGrants(write=True))
    )
    claims = TokenVerifier(KEY, SECRET).verify(theirs.to_jwt())
    assert (claims.identity, claims.name, claims.metadata, claims.kind, claims.room_preset) == ("bob", "Bob", "m", "agent", "p")
    assert claims.attributes == {"a": "1"}
    assert claims.video.room_admin is True and claims.video.can_publish is False
    assert claims.sip.call is True


def test_tokens_from_here_verify_with_the_official_sdk() -> None:
    claims = official.TokenVerifier(KEY, SECRET).verify(full_token().to_jwt())
    assert claims.identity == "alice"
    assert claims.video.room_admin is True
    assert claims.video.destination_room == "d"
    assert claims.sip.call is True
    assert claims.room_preset == "preset"
    assert claims.room_config.max_participants == 13


def test_official_receiver_accepts_what_this_sdk_signs() -> None:
    body = json.dumps({"event": "room_started", "id": "EV_1", "room": {"name": "r"}, "createdAt": "1700000000"})
    event = official.WebhookReceiver(official.TokenVerifier(KEY, SECRET)).receive(body, sign_webhook(body, KEY, SECRET))
    assert event.event == "room_started"


def test_this_receiver_accepts_what_the_official_sdk_signs() -> None:
    body = json.dumps({"event": "room_finished", "id": "EV_2", "room": {"name": "r"}, "createdAt": "1700000000"})
    digest = base64.b64encode(hashlib.sha256(body.encode()).digest()).decode()
    token = official.AccessToken(KEY, SECRET).with_sha256(digest).to_jwt()
    event = WebhookReceiver(TokenVerifier(KEY, SECRET)).receive(body, token)
    assert event.event == "room_finished"
    assert event.room.name == "r"


def test_webhook_receiver() -> None:
    body = json.dumps({"event": "room_started", "id": "EV_1", "room": {"name": "r"}, "createdAt": "1700000000", "somethingNew": {"x": 1}})
    signature = sign_webhook(body, KEY, SECRET)
    receiver = WebhookReceiver(TokenVerifier(KEY, SECRET))

    event = receiver.receive(body, signature)
    assert (event.event, event.room.name, event.created_at) == ("room_started", "r", 1700000000)
    assert receiver.receive(body.encode(), signature).id == "EV_1"

    with pytest.raises(WebhookError):
        receiver.receive(body + " ", signature)
    with pytest.raises(WebhookError):
        receiver.receive(body, "")
    with pytest.raises(jwt.InvalidTokenError):
        receiver.receive(body, "Bearer " + signature)
    with pytest.raises(jwt.InvalidTokenError):
        WebhookReceiver(TokenVerifier(KEY, "not the secret")).receive(body, signature)
    with pytest.raises(jwt.InvalidTokenError):
        WebhookReceiver(TokenVerifier("other", SECRET)).receive(body, signature)
    # A valid token with no body hash covers nothing.
    with pytest.raises(WebhookError):
        receiver.receive(body, AccessToken(KEY, SECRET).to_jwt())

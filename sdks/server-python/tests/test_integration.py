"""The SDK against real running servers: this server, and the reference wherever a call exists there. Participants are real:
the streamkit CLI joining with a real WebRTC session and real media."""

import asyncio
import io
import re
import subprocess
import time

import jwt as pyjwt
import pytest
from conftest import captured_webhooks, data_event, room_name
from livekit import api as official

from elnova_streamkit import (
    AccessToken,
    AgentGrants,
    NotSupportedError,
    ServerError,
    SIPGrants,
    StreamKitAPI,
    TokenVerifier,
    VideoGrants,
    WebhookError,
    WebhookReceiver,
)
from elnova_streamkit.proto.livekit_agent_dispatch_pb2 import (
    CreateAgentDispatchRequest,
    DeleteAgentDispatchRequest,
    ListAgentDispatchRequest,
)
from elnova_streamkit.proto.livekit_models_pb2 import DataPacket, ParticipantInfo, ParticipantPermission
from elnova_streamkit.proto.livekit_room_pb2 import (
    CreateRoomRequest,
    DeleteRoomRequest,
    ForwardParticipantRequest,
    ListParticipantsRequest,
    ListRoomsRequest,
    MoveParticipantRequest,
    MuteRoomTrackRequest,
    PerformRpcRequest,
    RoomConfiguration,
    RoomParticipantIdentity,
    SendDataRequest,
    UpdateParticipantRequest,
    UpdateRoomMetadataRequest,
    UpdateSubscriptionsRequest,
)


async def failure(call) -> ServerError:
    with pytest.raises(ServerError) as caught:
        await call
    return caught.value


def api_with_token(target, grant: VideoGrants) -> StreamKitAPI:
    token = AccessToken(target.server["api_key"], target.server["api_secret"]).with_identity("caller").with_grants(grant).to_jwt()
    return StreamKitAPI(target.server["url"], token=token)


async def test_room_lifecycle(target) -> None:
    room = target.api.room
    name = room_name("life")
    created = await room.create_room(CreateRoomRequest(name=name, empty_timeout=120, departure_timeout=30, max_participants=7, metadata="hello"))
    assert (created.name, created.empty_timeout, created.departure_timeout, created.max_participants, created.metadata) == (name, 120, 30, 7, "hello")
    assert created.sid.startswith("RM_")
    assert created.creation_time > 0

    assert (await room.create_room(CreateRoomRequest(name=name))).sid == created.sid

    named = await room.list_rooms(ListRoomsRequest(names=[name, name + "-not-open"]))
    assert [r.sid for r in named.rooms] == [created.sid]
    assert name in [r.name for r in (await room.list_rooms(ListRoomsRequest())).rooms]

    assert (await room.update_room_metadata(UpdateRoomMetadataRequest(room=name, metadata="changed"))).metadata == "changed"
    assert (await room.update_room_metadata(UpdateRoomMetadataRequest(room=name, metadata=""))).metadata == ""

    await room.delete_room(DeleteRoomRequest(room=name))
    assert list((await room.list_rooms(ListRoomsRequest(names=[name]))).rooms) == []


async def test_what_does_not_exist(target) -> None:
    room = target.api.room
    missing = room_name("missing")
    assert (await failure(room.delete_room(DeleteRoomRequest(room=missing)))).code == "not_found"
    with pytest.raises(ServerError):
        await room.update_room_metadata(UpdateRoomMetadataRequest(room=missing, metadata="x"))
    assert list((await room.list_participants(ListParticipantsRequest(room=missing))).participants) == []
    assert (await failure(room.get_participant(RoomParticipantIdentity(room=missing, identity="nobody")))).code == "not_found"
    assert (await failure(room.remove_participant(RoomParticipantIdentity(room=missing, identity="nobody")))).code == "not_found"


async def test_grants_are_enforced(target) -> None:
    name = room_name("grants")
    await target.api.room.create_room(CreateRoomRequest(name=name))

    def denied(e: ServerError) -> None:
        assert (e.code, e.status, e.message) == ("unauthenticated", 401, "permissions denied")

    async with api_with_token(target, VideoGrants(room_join=True, room=name)) as join_only:
        denied(await failure(join_only.room.create_room(CreateRoomRequest(name=name))))
        denied(await failure(join_only.room.list_rooms(ListRoomsRequest())))
        denied(await failure(join_only.room.delete_room(DeleteRoomRequest(room=name))))
        denied(await failure(join_only.room.list_participants(ListParticipantsRequest(room=name))))
    async with api_with_token(target, VideoGrants(room_admin=True, room=name + "-other")) as other_room:
        denied(await failure(other_room.room.list_participants(ListParticipantsRequest(room=name))))
        denied(await failure(other_room.room.send_data(SendDataRequest(room=name, data=b"x"))))
    # The right grant works, so those denials are about the grant.
    async with api_with_token(target, VideoGrants(room_list=True)) as lister:
        await lister.room.list_rooms(ListRoomsRequest())
    async with api_with_token(target, VideoGrants(room_admin=True, room=name)) as admin:
        await admin.room.list_participants(ListParticipantsRequest(room=name))

    # Bad credentials are answered in plain text, outside the Twirp error shape.
    server = target.server
    async with StreamKitAPI(server["url"], server["api_key"], "not-the-secret") as bad:
        assert (await failure(bad.room.list_rooms(ListRoomsRequest()))).code == "unauthenticated"
    async with StreamKitAPI(server["url"], "not-a-key", server["api_secret"]) as bad:
        e = await failure(bad.room.list_rooms(ListRoomsRequest()))
        assert (e.code, e.message) == ("unauthenticated", "invalid API key")
    await target.api.room.delete_room(DeleteRoomRequest(room=name))


async def test_agrees_with_the_official_sdk(target) -> None:
    server = target.server
    async with official.LiveKitAPI(server["http"], server["api_key"], server["api_secret"]) as theirs:
        name = room_name("official")
        created = await theirs.room.create_room(official.CreateRoomRequest(name=name, metadata="from them"))
        read = await target.api.room.list_rooms(ListRoomsRequest(names=[name]))
        assert (read.rooms[0].sid, read.rooms[0].metadata) == (created.sid, "from them")
        await target.api.room.update_room_metadata(UpdateRoomMetadataRequest(room=name, metadata="from us"))
        back = await theirs.room.list_rooms(official.ListRoomsRequest(names=[name]))
        assert back.rooms[0].metadata == "from us"
        await theirs.room.delete_room(official.DeleteRoomRequest(room=name))


async def test_sdk_token_joins(target) -> None:
    name = room_name("token")
    token = (
        AccessToken(target.server["api_key"], target.server["api_secret"])
        .with_identity("holder")
        .with_name("The Holder")
        .with_metadata("token metadata")
        .with_attributes({"color": "green"})
        .with_grants(VideoGrants(room_join=True, room=name))
        .to_jwt()
    )
    peer = target.join(name, "holder", token=token)
    joined = await peer.wait("joining", lambda e: e["event"] == "joined")
    assert (joined["identity"], joined["name"], joined["room"], joined["attributes"]) == ("holder", "The Holder", name, {"color": "green"})
    info = await target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="holder"))
    assert (info.metadata, info.name, dict(info.attributes)) == ("token metadata", "The Holder", {"color": "green"})


async def test_official_token_joins(target) -> None:
    name = room_name("official-token")
    token = (
        official.AccessToken(target.server["api_key"], target.server["api_secret"])
        .with_identity("theirs")
        .with_metadata("m")
        .with_grants(official.VideoGrants(room_join=True, room=name))
        .to_jwt()
    )
    await target.join(name, "theirs", token=token).connected()
    assert (await target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="theirs"))).metadata == "m"


async def test_token_with_every_grant_joins(target) -> None:
    name = room_name("everything")
    token = (
        AccessToken(target.server["api_key"], target.server["api_secret"])
        .with_identity("everything")
        .with_grants(
            VideoGrants(
                room_join=True, room=name, room_create=True, room_list=True, room_record=True, room_admin=True, ingress_admin=True, can_publish=True,
                can_subscribe=True, can_publish_data=True, can_update_own_metadata=True, can_subscribe_metrics=True, can_manage_agent_session=True,
                can_publish_sources=["camera", "microphone"], destination_room=name + "-dest",
            )  # fmt: skip
        )
        .with_sip_grants(SIPGrants(admin=True, call=True))
        .with_agent_grants(AgentGrants(admin=True, dispatch_admin=True))
        .to_jwt()
    )
    await target.join(name, "everything", token=token).connected()
    async with StreamKitAPI(target.server["url"], token=token) as caller:
        listed = await caller.room.list_participants(ListParticipantsRequest(room=name))
    permission = listed.participants[0].permission
    assert permission.can_publish and permission.can_update_metadata
    assert len(permission.can_publish_sources) == 2


async def test_token_grants_apply(target) -> None:
    name = room_name("limited")
    token = (
        AccessToken(target.server["api_key"], target.server["api_secret"])
        .with_identity("limited")
        .with_kind("agent")
        .with_grants(VideoGrants(room_join=True, room=name, can_publish=False))
        .to_jwt()
    )
    await target.join(name, "limited", token=token).connected()
    info = await target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="limited"))
    assert not info.permission.can_publish
    # Data follows publishing, when a token does not say otherwise.
    assert not info.permission.can_publish_data
    assert info.permission.can_subscribe
    assert info.kind == ParticipantInfo.AGENT


async def test_token_room_config(target) -> None:
    name = room_name("configured")
    token = (
        AccessToken(target.server["api_key"], target.server["api_secret"])
        .with_identity("configured")
        .with_grants(VideoGrants(room_join=True, room=name))
        .with_room_config(RoomConfiguration(empty_timeout=77, departure_timeout=66, max_participants=55))
        .to_jwt()
    )
    await target.join(name, "configured", token=token).connected()
    room = (await target.api.room.list_rooms(ListRoomsRequest(names=[name]))).rooms[0]
    assert (room.empty_timeout, room.departure_timeout, room.max_participants) == (77, 66, 55)


async def test_reference_tool_token(target, fx) -> None:
    if not fx["reference_binary"]:
        pytest.skip("the reference server is not available")
    name = room_name("tool")
    keys = f"{target.server['api_key']}: {target.server['api_secret']}"
    out = subprocess.run([fx["reference_binary"], "--keys", keys, "create-join-token", "--room", name, "--identity", "from-tool"], capture_output=True, text=True, check=True).stdout
    token = re.search(r"eyJ[\w-]+\.[\w-]+\.[\w-]+", out).group(0)
    claims = TokenVerifier(target.server["api_key"], target.server["api_secret"]).verify(token)
    assert (claims.identity, claims.video.room, claims.video.room_join) == ("from-tool", name, True)
    await target.join(name, "from-tool", token=token).connected()


async def test_participants_with_real_media(target) -> None:
    name = room_name("media")
    publisher = target.join(name, "publisher", publish=True)
    tracks = await publisher.published()
    subscriber = target.join(name, "subscriber")
    for kind in ("video", "audio"):
        await subscriber.wait(f"real {kind}", lambda e, k=kind: e["event"] == "media" and e["kind"] == k and e["packets"] > 10)

    listed = await target.api.room.list_participants(ListParticipantsRequest(room=name))
    assert sorted(p.identity for p in listed.participants) == ["publisher", "subscriber"]
    info = await target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="publisher"))
    assert info.state == ParticipantInfo.ACTIVE
    assert info.sid.startswith("PA_")
    assert sorted(t.sid for t in info.tracks) == sorted(t["sid"] for t in tracks)
    missing = await failure(target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="nobody")))
    assert (missing.code, missing.message) == ("not_found", "participant does not exist")


async def test_mute_published_track(target) -> None:
    name = room_name("mute")
    publisher = target.join(name, "publisher", publish=True)
    tracks = await publisher.published()
    audio = next(t["sid"] for t in tracks if t["kind"] == "audio")
    res = await target.api.room.mute_published_track(MuteRoomTrackRequest(room=name, identity="publisher", track_sid=audio, muted=True))
    assert res.track.muted and res.track.sid == audio
    await publisher.wait("being told it was muted", lambda e: e["event"] == "muted" and e["sid"] == audio and e["muted"])
    # A server lists a track once media has arrived for it, which can be a moment after it accepted the track.
    deadline = time.monotonic() + 30
    while True:
        info = await target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="publisher"))
        listed = next((t for t in info.tracks if t.sid == audio), None)
        if listed is not None:
            assert listed.muted
            break
        if time.monotonic() > deadline:
            raise AssertionError("the muted track was never listed")
        await asyncio.sleep(0.1)
    # Unmuting from afar is off unless the server turned it on.
    e = await failure(target.api.room.mute_published_track(MuteRoomTrackRequest(room=name, identity="publisher", track_sid=audio, muted=False)))
    assert e.code == "failed_precondition"


async def test_update_participant(target) -> None:
    name = room_name("update")
    subject = target.join(name, "subject")
    await subject.connected()
    observer = target.join(name, "observer")
    await observer.connected()
    res = await target.api.room.update_participant(UpdateParticipantRequest(room=name, identity="subject", metadata="new metadata", name="New Name", attributes={"a": "1"}))
    assert (res.metadata, res.name, dict(res.attributes)) == ("new metadata", "New Name", {"a": "1"})

    def updated(e: dict) -> bool:
        return e["event"] == "participant_update" and e["identity"] == "subject" and e.get("metadata") == "new metadata" and e.get("name") == "New Name" and e.get("attributes", {}).get("a") == "1"

    for peer in (subject, observer):
        await peer.wait("the update", updated)
    # A permission replaces the whole set.
    limited = await target.api.room.update_participant(UpdateParticipantRequest(room=name, identity="subject", permission=ParticipantPermission(can_subscribe=True)))
    assert not limited.permission.can_publish and limited.permission.can_subscribe
    assert limited.metadata == "new metadata"
    await subject.wait("the permission update", lambda e: e["event"] == "participant_update" and e["identity"] == "subject" and e.get("can_publish") is False)
    e = await failure(target.api.room.update_participant(UpdateParticipantRequest(room=name, identity="nobody", metadata="x")))
    assert e.code == "not_found"


async def test_update_subscriptions(target) -> None:
    name = room_name("subs")
    publisher = target.join(name, "publisher", publish=True)
    tracks = await publisher.published()
    video = next(t["sid"] for t in tracks if t["kind"] == "video")
    viewer = target.join(name, "viewer", no_auto_subscribe=True)
    await viewer.wait("joining", lambda e: e["event"] == "joined")
    await target.api.room.update_subscriptions(UpdateSubscriptionsRequest(room=name, identity="viewer", track_sids=[video], subscribe=True))
    await viewer.wait("video after subscribing", lambda e: e["event"] == "media" and e["kind"] == "video" and e["packets"] > 10)
    assert not any(e["event"] == "media" and e["kind"] == "audio" for e in viewer.events)


async def test_send_data(target) -> None:
    name = room_name("data")
    alice = target.join(name, "alice")
    await alice.data_channels()
    bob = target.join(name, "bob")
    await bob.data_channels()
    send = target.api.room.send_data

    await send(SendDataRequest(room=name, data=b"to everyone", kind=DataPacket.RELIABLE, topic="news"))
    for peer in (alice, bob):
        e = await peer.wait("the broadcast", data_event("to everyone"))
        assert (e["topic"], e["channel"]) == ("news", "_reliable")

    await send(SendDataRequest(room=name, data=b"for bob", kind=DataPacket.RELIABLE, destination_identities=["bob"]))
    await bob.wait("its own message", data_event("for bob"))
    # Reliable data arrives in order, so once alice has this, she would have had the other if it was coming.
    await send(SendDataRequest(room=name, data=b"after", kind=DataPacket.RELIABLE))
    await alice.wait("the later message", data_event("after"))
    assert not any(data_event("for bob")(e) for e in alice.events)

    await send(SendDataRequest(room=name, data=b"lossy", kind=DataPacket.LOSSY))
    assert (await bob.wait("the lossy message", data_event("lossy")))["channel"] == "_lossy"

    e = await failure(send(SendDataRequest(room=name, data=b"x", nonce=b"short")))
    assert e.code == "invalid_argument"


async def test_remove_participant(target) -> None:
    name = room_name("remove")
    peer = target.join(name, "leaving")
    await peer.connected()
    await target.api.room.remove_participant(RoomParticipantIdentity(room=name, identity="leaving"))
    assert (await peer.wait("being removed", lambda e: e["event"] == "left"))["reason"] == "PARTICIPANT_REMOVED"
    assert (await failure(target.api.room.get_participant(RoomParticipantIdentity(room=name, identity="leaving")))).code == "not_found"


async def test_delete_room_disconnects(target) -> None:
    name = room_name("delete")
    peer = target.join(name, "inside")
    await peer.connected()
    await target.api.room.delete_room(DeleteRoomRequest(room=name))
    assert (await peer.wait("the room closing", lambda e: e["event"] == "left"))["reason"] == "ROOM_DELETED"


async def test_room_metadata_reaches_those_inside(target) -> None:
    name = room_name("meta")
    peer = target.join(name, "inside")
    await peer.connected()
    await target.api.room.update_room_metadata(UpdateRoomMetadataRequest(room=name, metadata="for those inside"))
    await peer.wait("the room update", lambda e: e["event"] == "room_update" and e.get("metadata") == "for those inside")


async def test_methods_that_are_not_implemented(target) -> None:
    name = room_name("nope")
    with pytest.raises(ServerError):
        await target.api.room.forward_participant(ForwardParticipantRequest(room=name, identity="x", destination_room=name + "-2"))
    with pytest.raises(ServerError):
        await target.api.room.move_participant(MoveParticipantRequest(room=name, identity="x", destination_room=name + "-2"))
    rpc = await failure(target.api.room.perform_rpc(PerformRpcRequest(room=name, method="m")))
    assert rpc.code == ("invalid_argument" if target.name == "reference" else "unimplemented")


async def test_agent_dispatch(target) -> None:
    name = room_name("agents")
    dispatch = target.api.agent_dispatch
    request = CreateAgentDispatchRequest(room=name, agent_name="helper", metadata="m")
    if target.name == "ours":
        with pytest.raises(NotSupportedError, match="no AgentDispatchService service"):
            await dispatch.create_dispatch(request)
        return
    created = await dispatch.create_dispatch(request)
    assert created.agent_name == "helper"
    assert created.id.startswith("AD_")
    # The reference also keeps a dispatch of its own in every room, for agents that take any job.
    assert created.id in [d.id for d in (await dispatch.list_dispatch(ListAgentDispatchRequest(room=name))).agent_dispatches]
    await dispatch.delete_dispatch(DeleteAgentDispatchRequest(dispatch_id=created.id, room=name))


async def test_webhooks_from_real_servers(target, fx) -> None:
    name = room_name("webhooks")
    server = target.server
    ours = WebhookReceiver(TokenVerifier(server["api_key"], server["api_secret"]))
    theirs = official.WebhookReceiver(official.TokenVerifier(server["api_key"], server["api_secret"]))

    async def wait_for(*names: str) -> dict:
        deadline = time.monotonic() + 45
        while True:
            seen = set()
            last = None
            for c in captured_webhooks(fx, target.name):
                event = ours.receive(c["body"], c["authorization"])
                if event.room.name != name:
                    continue
                assert theirs.receive(c["body"].decode(), c["authorization"]).event == event.event
                assert c["content_type"] == "application/webhook+json"
                assert event.id and event.created_at
                seen.add(event.event)
                last = c
            if set(names) <= seen:
                return last
            if time.monotonic() > deadline:
                raise AssertionError(f"never saw all of {names}. Saw {seen}")
            await asyncio.sleep(0.25)

    peer = target.join(name, "publisher", publish=True)
    await peer.published()
    # A participant counts as joined once its media is connected, and a track as published once media arrives.
    await wait_for("room_started", "participant_joined", "track_published")
    await target.api.room.delete_room(DeleteRoomRequest(room=name))
    last = await wait_for("room_finished", "participant_left")

    with pytest.raises(WebhookError):
        ours.receive(last["body"].replace(name.encode(), name.encode() + b"x", 1), last["authorization"])
    with pytest.raises(pyjwt.InvalidTokenError):
        WebhookReceiver(TokenVerifier(server["api_key"], "not the secret")).receive(last["body"], last["authorization"])


@pytest.mark.parametrize("which", ["reference", "plain"])
async def test_egress_and_ingress_need_this_servers_api(fx, which) -> None:
    server = fx[which]
    if server is None:
        pytest.skip("the reference server is not available")
    async with StreamKitAPI(server["url"], server["api_key"], server["api_secret"]) as api:
        with pytest.raises(NotSupportedError):
            await api.egress.list_egress()
        with pytest.raises(NotSupportedError, match="no management API"):
            await api.ingress.list_ingress()


async def test_unsupported_egress_kinds() -> None:
    async with StreamKitAPI("http://127.0.0.1:1", "k", "s") as api:
        for call in (api.egress.start_web_egress, api.egress.start_track_composite_egress, api.egress.start_track_egress, api.egress.update_layout, api.egress.update_stream):
            with pytest.raises(NotSupportedError):
                await call("anything")


async def test_egress_validation(ours, fx) -> None:
    missing = room_name("missing-room")
    e = await failure(ours.api.egress.start_room_composite_egress(missing, format="mp4"))
    assert e.code == ("room_not_found" if fx["workers"] else "egress_unavailable")
    e = await failure(ours.api.egress.get_egress("EG_not_a_job"))
    assert e.code == "not_found"


async def test_ingress_configuration(ours) -> None:
    ingress = ours.api.ingress
    name = room_name("ingress")
    created = await ingress.create_ingress("rtmp", name="stage", room=name, participant_identity="stage", participant_name="Stage")
    assert created.id.startswith("IN_")
    assert created.stream_key
    assert created.publish_url.startswith("rtmp://")
    got = await ingress.get_ingress(created.id)
    assert got.stream_key == ""
    assert (got.room_name, got.participant_identity, got.participant_name, got.name, got.enabled) == (name, "stage", "Stage", "stage", True)
    assert created.id in [x.id for x in await ingress.list_ingress()]
    updated = await ingress.update_ingress(created.id, name="renamed", enabled=False)
    assert (updated.name, updated.enabled) == ("renamed", False)
    rotated = await ingress.rotate_ingress_key(created.id)
    assert rotated.stream_key and rotated.stream_key != created.stream_key
    await ingress.ingress_logs(created.id, limit=10)
    await ingress.delete_ingress(created.id)
    assert (await failure(ingress.get_ingress(created.id))).code == "not_found"
    invalid = await failure(ingress.create_ingress("carrier-pigeon", room=name, participant_identity="x"))
    assert invalid.code == "validation_failed"
    assert invalid.fields["input_type"]


async def test_recording_end_to_end(ours, fx) -> None:
    if not fx["workers"]:
        pytest.skip("STREAMKIT_TEST_WORKERS is not set")
    egress = ours.api.egress
    name = room_name("record")
    peer = ours.join(name, "on-camera", publish=True)
    await peer.published()

    job = await egress.start_room_composite_egress(name, format="mp4", layout="grid")
    assert job.id.startswith("EG_")
    assert (job.room_name, job.type, job.format) == (name, "room_composite", "mp4")

    async def wait_for(check) -> object:
        deadline = time.monotonic() + 90
        while True:
            current = await egress.get_egress(job.id)
            if check(current):
                return current
            if time.monotonic() > deadline:
                raise AssertionError(f"job stayed {current.status}: {current.error}")
            await asyncio.sleep(0.5)

    active = await wait_for(lambda j: j.status == "active" or j.ended)
    assert active.status == "active"
    assert job.id in [j.id for j in (await egress.list_egress(limit=10)).items]
    assert (await failure(egress.retry_recording(job.id))).code == "egress_running"

    # Let it record something worth checking.
    await asyncio.sleep(5)
    await egress.stop_egress(job.id)
    done = await wait_for(lambda j: j.ended)
    assert done.status == "complete"
    assert done.file_size > 0
    assert (await failure(egress.stop_egress(job.id))).code == "egress_not_running"
    assert job.id in [j.id for j in (await egress.list_recordings()).items]

    file = io.BytesIO()
    assert await egress.download_recording(job.id, file) == done.file_size
    assert file.getvalue()[4:8] == b"ftyp"

    await egress.delete_recording(job.id)
    assert (await failure(egress.get_egress(job.id))).code == "not_found"


async def test_ingress_end_to_end(ours, fx) -> None:
    if not fx["workers"]:
        pytest.skip("STREAMKIT_TEST_WORKERS is not set")
    ingress = ours.api.ingress
    name = room_name("rtmp")
    created = await ingress.create_ingress("rtmp", room=name, participant_identity="stage")
    pusher = subprocess.Popen(
        ["ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "testsrc2=s=640x360:r=30", "-f", "lavfi", "-i", "sine=frequency=440:r=48000",
         "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-g", "60", "-c:a", "aac", "-f", "flv", f"rtmp://{fx['rtmp']}/live/{created.stream_key}"],
    )  # fmt: skip
    try:
        deadline = time.monotonic() + 60
        while True:
            current = await ingress.get_ingress(created.id)
            if current.status == "publishing":
                break
            if time.monotonic() > deadline:
                raise AssertionError(f"it stayed {current.status}: {current.error}")
            await asyncio.sleep(0.25)
        viewer = ours.join(name, "viewer")
        await viewer.wait("the stream", lambda e: e["event"] == "media" and e["kind"] == "video" and e["packets"] > 10)
        await ingress.stop_ingress(created.id)
    finally:
        pusher.terminate()
        pusher.wait(timeout=10)
        await ingress.delete_ingress(created.id)

"""The server side of agents, through the framework's own worker, on both servers.

Real WebSockets and real WebRTC. Where our server differs from the reference on purpose, the test says so.
"""

from __future__ import annotations

import asyncio
import uuid

import pytest
import websockets
from websockets.asyncio.client import connect

from helpers import TestUser, admin_token, eventually, tone, twirp, unique_room, user_token
from streamkit_agents import Worker, mint_token, worker_token
from streamkit_agents._proto import livekit_agent_pb2 as agent_pb
from streamkit_agents._proto import livekit_models_pb2 as models
from streamkit_agents.rtc import AudioSource, Room, rms_level

pytestmark = pytest.mark.webrtc


def make_worker(server, entrypoint, *, agent_name=None, **kwargs) -> Worker:
    # Tests share a server, so an unnamed worker would catch every room's automatic dispatch. Each gets its own name.
    return Worker(
        entrypoint,
        ws_url=server.ws_url,
        api_key=server.api_key,
        api_secret=server.api_secret,
        agent_name=agent_name if agent_name is not None else f"agent-{uuid.uuid4().hex[:8]}",
        load_fn=lambda: 0.0,
        **kwargs,
    )


class JobLog:
    def __init__(self):
        self.jobs = []
        self.joined = asyncio.Event()
        self.contexts = []

    async def entry(self, ctx):
        self.jobs.append(ctx.job)
        self.contexts.append(ctx)
        await ctx.connect()
        self.joined.set()
        await ctx.wait_for_shutdown()


async def test_explicit_dispatch_through_the_api(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("dispatch")
    admin = admin_token(server, room)
    try:
        status, room_info = await twirp(server, "RoomService", "CreateRoom", {"name": room, "empty_timeout": 60}, admin)
        assert status == 200, room_info
        status, dispatch = await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": worker.agent_name, "room": room, "metadata": "hello"}, admin)
        assert status == 200, dispatch
        await asyncio.wait_for(log.joined.wait(), 15)
        job = log.jobs[0]
        assert job.room.name == room and job.metadata == "hello" and job.agent_name == worker.agent_name
        assert job.type == agent_pb.JT_ROOM

        status, listed = await twirp(server, "AgentDispatchService", "ListDispatch", {"room": room}, admin)
        assert status == 200
        ours = [d for d in listed["agent_dispatches"] if d["id"] == dispatch["id"]]
        assert len(ours) == 1 and ours[0]["agent_name"] == worker.agent_name

        # Deleting ends the job, and the agent goes.
        status, _ = await twirp(server, "AgentDispatchService", "DeleteDispatch", {"dispatch_id": dispatch["id"], "room": room}, admin)
        assert status == 200
        await eventually(lambda: not worker.jobs, timeout=15, message="the job to end")
    finally:
        await worker.aclose()


async def test_room_api_agents_replace_automatic_dispatch(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("roomapi")
    try:
        status, info = await twirp(server, "RoomService", "CreateRoom", {
            "name": room, "empty_timeout": 60,
            "agents": [{"agent_name": worker.agent_name, "metadata": "from create room"}],
        }, admin_token(server, room))
        assert status == 200, info
        await asyncio.wait_for(log.joined.wait(), 15)
        assert log.jobs[0].metadata == "from create room"
    finally:
        await worker.aclose()


async def test_token_room_config_dispatches_an_agent(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("token")
    token = mint_token(server.api_key, server.api_secret, identity="user", video={"roomJoin": True, "room": room},
                       room_config={"agents": [{"agentName": worker.agent_name, "metadata": "from token"}], "maxParticipants": 5})
    user = TestUser(server, room, token=token)
    try:
        await user.join(publish=False)
        await asyncio.wait_for(log.joined.wait(), 15)
        assert log.jobs[0].metadata == "from token"
        await eventually(lambda: any(p.is_agent for p in user.room.remote_participants.values()), message="the agent in the user's view")
    finally:
        await user.leave()
        await worker.aclose()


async def test_job_token_makes_an_agent_participant(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("kind")
    admin = admin_token(server, room)
    user = TestUser(server, room)
    try:
        await user.join(publish=False)
        await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": worker.agent_name, "room": room}, admin)
        await asyncio.wait_for(log.joined.wait(), 15)
        await eventually(lambda: len(user.room.remote_participants) == 1, message="the agent to appear")
        participant = next(iter(user.room.remote_participants.values()))
        assert participant.is_agent
        assert participant.identity == f"agent-{log.jobs[0].id}"
        # Both spellings exist on the wire: the server's, then the framework's.
        assert participant.attributes.get("lk.agent_name") == worker.agent_name
        assert participant.attributes.get("lk.agent.name") == worker.agent_name

        status, info = await twirp(server, "RoomService", "ListParticipants", {"room": room}, admin)
        assert status == 200
        kinds = {p["identity"]: p.get("kind") for p in info["participants"]}
        assert kinds[participant.identity] == "AGENT"
    finally:
        await user.leave()
        await worker.aclose()


async def test_agents_do_not_count_toward_limits_or_hold_rooms_open(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("dependent")
    admin = admin_token(server, room)
    user = TestUser(server, room)
    try:
        status, _ = await twirp(server, "RoomService", "CreateRoom", {
            "name": room, "max_participants": 1, "empty_timeout": 60, "departure_timeout": 1,
            "agents": [{"agent_name": worker.agent_name}],
        }, admin)
        assert status == 200
        await asyncio.wait_for(log.joined.wait(), 15)

        # The agent is here, and the room's only place is still free.
        await user.join(publish=False)
        # The reference refreshes this number on a timer, so it can lag. The agent must never be in it.
        counts = []
        deadline = asyncio.get_running_loop().time() + 15
        while asyncio.get_running_loop().time() < deadline:
            status, rooms = await twirp(server, "RoomService", "ListRooms", {"names": [room]}, admin)
            assert status == 200
            counts.append(rooms["rooms"][0]["num_participants"])
            if counts[-1] == 1:
                break
            await asyncio.sleep(0.25)
        assert counts[-1] == 1 and max(counts) == 1, counts

        # A second person does not fit.
        second = Room()
        with pytest.raises(Exception):
            await second.connect(server.ws_url, user_token(server, room, "second"))

        # Everyone leaves, and the agent alone does not keep the room.
        await user.leave()

        async def room_gone():
            _, rooms = await twirp(server, "RoomService", "ListRooms", {"names": [room]}, admin)
            return not rooms.get("rooms")

        deadline = asyncio.get_running_loop().time() + 30
        while not await room_gone():
            assert asyncio.get_running_loop().time() < deadline, "the agent kept the room open"
            await asyncio.sleep(0.25)
        # The room closing ends the job.
        await eventually(lambda: not worker.jobs, timeout=15, message="the job to end with the room")
    finally:
        await worker.aclose()


async def test_automatic_dispatch_reaches_unnamed_workers(server):
    """The reference's rule: a room with no configured agents gets one dispatch, for workers with no name."""
    log = JobLog()
    worker = make_worker(server, log.entry, agent_name="")
    worker.start()
    await worker.wait_registered()
    room = unique_room("automatic")
    user = TestUser(server, room)
    try:
        await user.join(publish=False)
        await eventually(lambda: any(j.room.name == room for j in log.jobs), timeout=15, message="the automatic job")
    finally:
        await user.leave()
        await worker.aclose()


async def test_publisher_jobs_start_once_per_publisher(server):
    log = JobLog()
    worker = make_worker(server, log.entry, job_type=agent_pb.JT_PUBLISHER)
    worker.start()
    await worker.wait_registered()
    room = unique_room("publisher")
    admin = admin_token(server, room)
    user = TestUser(server, room)
    second_track = AudioSource()
    try:
        status, _ = await twirp(server, "RoomService", "CreateRoom", {"name": room, "empty_timeout": 60, "agents": [{"agent_name": worker.agent_name}]}, admin)
        assert status == 200
        await user.join()
        feeder = asyncio.create_task(user.say(tone(6)))
        await eventually(lambda: log.jobs, timeout=15, message="the publisher job")
        job = log.jobs[0]
        assert job.type == agent_pb.JT_PUBLISHER and job.participant.identity == "user"
        await user.room.publish_track(second_track, name="second", source=models.SCREEN_SHARE_AUDIO)
        await asyncio.sleep(1.5)
        assert len(log.jobs) == 1, "a second track must not start a second job"
        feeder.cancel()
    finally:
        await user.leave()
        await worker.aclose()


async def test_worker_declines_then_job_goes_to_another(server):
    declined = asyncio.Event()

    async def decline(request):
        declined.set()
        await request.reject()

    log = JobLog()
    name = f"agent-{uuid.uuid4().hex[:8]}"
    first = make_worker(server, log.entry, agent_name=name, request_fn=decline)
    second = make_worker(server, log.entry, agent_name=name)
    first.start()
    second.start()
    await first.wait_registered()
    await second.wait_registered()
    room = unique_room("decline")
    try:
        # Whichever is chosen first, the job must end up running. Several rooms make it likely the decliner gets asked.
        for i in range(6):
            r = f"{room}-{i}"
            await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": name, "room": r}, admin_token(server, r))
        await eventually(lambda: len(log.jobs) == 6, timeout=30, message="all six jobs to run")
        assert declined.is_set()
    finally:
        await first.aclose()
        await second.aclose()


async def test_worker_registration_is_authenticated(server):
    url = f"{server.ws_url}/agent"
    no_grant = mint_token(server.api_key, server.api_secret, identity="x", video={"roomJoin": True, "room": "r"})
    for headers in ({}, {"Authorization": f"Bearer {no_grant}"}):
        with pytest.raises(websockets.exceptions.InvalidStatus) as e:
            async with connect(url, additional_headers=headers):
                pass
        assert e.value.response.status_code == 401


async def test_first_worker_message_must_be_register(server):
    url = f"{server.ws_url}/agent"
    token = worker_token(server.api_key, server.api_secret)
    async with connect(url, additional_headers={"Authorization": f"Bearer {token}"}) as ws:
        await ws.send(agent_pb.WorkerMessage(update_worker=agent_pb.UpdateWorkerStatus(load=0.1)).SerializeToString())
        with pytest.raises(websockets.exceptions.ConnectionClosed):
            await asyncio.wait_for(ws.recv(), 12)


async def test_track_subscribed_fires_without_a_forwarded_packet(server):
    """A publisher that goes quiet still hears about its subscriber. The signal must not depend on a packet being forwarded
    after the subscriber binds. This is the wait every agent's audio goes through."""
    room = unique_room("bind")
    publisher = TestUser(server, room, "publisher")
    subscriber = TestUser(server, room, "subscriber")
    try:
        await publisher.join()
        # Audio flows briefly, so the server has a receiver. Then the track goes silent, in the RTP sense: aiortc's source
        # is stopped from producing anything further.
        await publisher.say(tone(0.5))
        await asyncio.sleep(0.2)
        publisher.mic._paused = True  # keeps sending silence packets, the state an idle agent is in
        await subscriber.join(publish=False)
        assert await publisher.publication.wait_for_subscription(10), "TrackSubscribed never arrived"
    finally:
        await publisher.leave()
        await subscriber.leave()


async def test_agent_text_stream_on_behalf_of_a_user(server):
    """An agent sends a transcript in a user's name. On both servers, receivers see the user's identity on the header, marked
    as generated. Our server keeps the identity on chunks too. The reference overwrites it on chunks, which the framework's
    own reader tolerates and strict clients may not: docs/architecture/agents.md has what we found."""
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("identity")
    admin = admin_token(server, room)
    user = TestUser(server, room)
    watcher = TestUser(server, room, "watcher")
    packets = []
    try:
        await user.join(publish=False)
        await watcher.join(publish=False)
        watcher.room.on("data_received", packets.append)
        await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": worker.agent_name, "room": room}, admin)
        await asyncio.wait_for(log.joined.wait(), 15)
        ctx = log.contexts[0]

        writer = ctx.room.stream_text("lk.transcription", sender_identity="user", attributes={"lk.segment_id": "s1", "lk.transcription_final": "true"})
        await writer.write("the words the user said")
        await writer.close()
        await eventually(lambda: watcher.transcripts, message="the transcript")
        got = watcher.transcripts[0]
        assert got["identity"] == "user"
        assert got["generated"] is True
        assert got["text"] == "the words the user said"

        # The agent's own stream stays its own, unmarked.
        own = ctx.room.stream_text("lk.transcription", attributes={"lk.segment_id": "s2"})
        await own.write("the agent speaking")
        await own.close()
        await eventually(lambda: len(watcher.transcripts) == 2, message="the second transcript")
        assert watcher.transcripts[1]["identity"] == ctx.room.local_identity
        assert watcher.transcripts[1]["generated"] is False
    finally:
        await user.leave()
        await watcher.leave()
        await worker.aclose()


async def test_person_cannot_send_transcriptions_or_speak_for_others(server):
    room = unique_room("spoof")
    mallory = TestUser(server, room, "mallory")
    victim = TestUser(server, room, "victim")
    watcher = TestUser(server, room, "watcher")
    got = []
    try:
        await mallory.join(publish=False)
        await victim.join(publish=False)
        await watcher.join(publish=False)
        watcher.room.on("data_received", got.append)

        writer = mallory.room.stream_text("lk.transcription", sender_identity="victim")
        await writer.write("words put in someone's mouth")
        await writer.close()
        await eventually(lambda: watcher.transcripts, message="the stream")
        assert watcher.transcripts[0]["identity"] == "mallory"
        assert watcher.transcripts[0]["generated"] is False

        await mallory.room.publish_transcription("victim", "TR_x", [models.TranscriptionSegment(id="1", text="forged", final=True)])
        await mallory.room.publish_data(b"marker")
        await eventually(lambda: any(p.WhichOneof("value") == "user" for p in got), message="the marker")
        assert not [p for p in got if p.WhichOneof("value") == "transcription"], "a person's transcription packet was forwarded"
    finally:
        await mallory.leave()
        await victim.leave()
        await watcher.leave()


async def test_rpc_between_agent_and_user(server):
    room = unique_room("rpc")
    a = TestUser(server, room, "caller")
    b = TestUser(server, room, "callee")
    try:
        await a.join(publish=False)
        await b.join(publish=False)

        async def double(caller, payload):
            assert caller == "caller"
            return payload * 2

        b.room.register_rpc_method("double", double)
        await eventually(lambda: "callee" in a.room.remote_participants, message="the callee")
        assert await a.room.perform_rpc("callee", "double", "ab") == "abab"
    finally:
        await a.leave()
        await b.leave()


async def test_named_dispatch_waits_for_a_worker_on_our_server(server):
    """Our server keeps a named dispatch until a worker comes. The reference loses it, which this checks too, so that the
    difference stays deliberate and known."""
    room = unique_room("wait")
    name = f"agent-{uuid.uuid4().hex[:8]}"
    admin = admin_token(server, room)
    status, _ = await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": name, "room": room}, admin)
    assert status == 200
    log = JobLog()
    worker = make_worker(server, log.entry, agent_name=name)
    worker.start()
    await worker.wait_registered()
    try:
        if server.is_reference:
            await asyncio.sleep(2)
            assert not log.jobs
        else:
            await asyncio.wait_for(log.joined.wait(), 15)
            assert log.jobs[0].room.name == room
    finally:
        await worker.aclose()


async def test_worker_reconnects_and_keeps_its_job(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("reconnect")
    admin = admin_token(server, room)
    user = TestUser(server, room)
    try:
        await user.join(publish=False)
        await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": worker.agent_name, "room": room}, admin)
        await asyncio.wait_for(log.joined.wait(), 15)
        first_id = worker.worker_id
        await worker._ws.close()
        await eventually(lambda: worker.worker_id != first_id and worker._registered.is_set(), timeout=15, message="the worker to re-register")
        # The job carried on through the reconnect.
        assert worker.jobs
        ctx = log.contexts[0]
        assert ctx.room.connected
    finally:
        await user.leave()
        await worker.aclose()


async def test_participant_jobs_start_once_per_person(server):
    log = JobLog()
    worker = make_worker(server, log.entry, job_type=agent_pb.JT_PARTICIPANT)
    worker.start()
    await worker.wait_registered()
    room = unique_room("participant")
    a = TestUser(server, room, "first")
    b = TestUser(server, room, "second")
    try:
        status, _ = await twirp(server, "RoomService", "CreateRoom", {"name": room, "empty_timeout": 60, "agents": [{"agent_name": worker.agent_name}]}, admin_token(server, room))
        assert status == 200
        await a.join(publish=False)
        await b.join(publish=False)
        await eventually(lambda: len(log.jobs) == 2, timeout=15, message="a job per person")
        await asyncio.sleep(1)
        # The agents that joined do not attract jobs of their own.
        assert sorted(j.participant.identity for j in log.jobs) == ["first", "second"]
        assert all(j.type == agent_pb.JT_PARTICIPANT for j in log.jobs)
    finally:
        await a.leave()
        await b.leave()
        await worker.aclose()


async def test_removing_an_agent_ends_its_job_as_failed(server):
    log = JobLog()
    worker = make_worker(server, log.entry)
    worker.start()
    await worker.wait_registered()
    room = unique_room("removed")
    admin = admin_token(server, room)
    user = TestUser(server, room)
    try:
        await user.join(publish=False)
        status, dispatch = await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": worker.agent_name, "room": room}, admin)
        assert status == 200
        await asyncio.wait_for(log.joined.wait(), 15)
        identity = log.contexts[0].room.local_identity

        status, _ = await twirp(server, "RoomService", "RemoveParticipant", {"room": room, "identity": identity}, admin)
        assert status == 200
        await eventually(lambda: not worker.jobs, timeout=15, message="the job to end")

        # The reference's listing keeps showing JS_RUNNING after this: it does not update the stored job. Ours shows what happened.
        if server.is_reference:
            return

        async def job_state():
            _, listed = await twirp(server, "AgentDispatchService", "ListDispatch", {"room": room, "dispatch_id": dispatch["id"]}, admin)
            jobs = listed["agent_dispatches"][0]["state"].get("jobs", [])
            return jobs[0].get("state", {}) if jobs else {}

        deadline = asyncio.get_running_loop().time() + 5
        state = await job_state()
        while state.get("status") != "JS_FAILED" and asyncio.get_running_loop().time() < deadline:
            await asyncio.sleep(0.2)
            state = await job_state()
        assert state.get("status") == "JS_FAILED", state
        assert state.get("error") == "agent worker left the room"
    finally:
        await user.leave()
        await worker.aclose()


async def test_track_offered_during_the_handshake_is_heard(server):
    """Someone joining a room with a track already in it gets that track offered straight after the first answer, while DTLS is
    still going. aiortc starts a receiver only if DTLS has finished when it applies an offer, so without care this track stays
    silent for good. It depended on how fast the network was, and showed up when this machine changed networks."""
    for attempt in range(3):
        room = unique_room(f"handshake{attempt}")
        publisher = TestUser(server, room, "publisher")
        late = TestUser(server, room, "late")
        try:
            await publisher.join()
            feeder = asyncio.create_task(publisher.say(tone(4)))
            await late.join(publish=False)

            def heard() -> bool:
                received = late.heard.get("publisher")
                return received is not None and any(rms_level(f) > 0.05 for _, f in received.frames)

            await eventually(heard, timeout=10, message="the late joiner to hear the track")
            feeder.cancel()
        finally:
            await publisher.leave()
            await late.leave()

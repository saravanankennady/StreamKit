"""The official Python worker and rtc SDK, unmodified, against both servers. This tests our server against someone else's client,
and answers the first questions about it: does that SDK connect at all, and does
the wait for TrackSubscribed end.

No LiveKit model is present or used. See tests/agents/oracle/stub for how the framework's import of its model package is met.
"""

from __future__ import annotations

import asyncio
import json
import os
import pathlib
import subprocess
import uuid

import pytest

from conftest import REPO, TOOLCHAIN
from helpers import TestUser, admin_token, eventually, twirp, unique_room

pytestmark = pytest.mark.webrtc

PYTHON = TOOLCHAIN / "venvs/agents-oracle/bin/python"
SCRIPT = REPO / "tests/agents/oracle/official_worker.py"
STUB = REPO / "tests/agents/oracle/stub"


class OfficialWorker:
    def __init__(self, server, agent_name: str):
        self.agent_name = agent_name
        self.events: list[dict] = []
        self.process = None
        self.server = server
        self.task = None

    async def start(self) -> None:
        env = {
            **os.environ,
            "LIVEKIT_URL": self.server.ws_url,
            "LIVEKIT_API_KEY": self.server.api_key,
            "LIVEKIT_API_SECRET": self.server.api_secret,
            "AGENT_NAME": self.agent_name,
            "PYTHONPATH": str(STUB),
        }
        self.process = await asyncio.create_subprocess_exec(
            str(PYTHON), str(SCRIPT), env=env, cwd=str(pathlib.Path(SCRIPT).parent),
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
        )
        self.task = asyncio.create_task(self._read())

    async def _read(self) -> None:
        async for line in self.process.stdout:
            try:
                self.events.append(json.loads(line))
            except ValueError:
                pass

    def named(self, name: str) -> dict | None:
        return next((e for e in self.events if e.get("event") == name), None)

    async def stop(self) -> None:
        if self.process.returncode is None:
            self.process.terminate()
            try:
                await asyncio.wait_for(self.process.wait(), 10)
            except asyncio.TimeoutError:
                self.process.kill()
        self.task.cancel()


@pytest.mark.skipif(not PYTHON.exists(), reason="the oracle virtualenv is not installed")
async def test_official_worker_end_to_end(server):
    worker = OfficialWorker(server, f"oracle-{uuid.uuid4().hex[:8]}")
    await worker.start()
    room = unique_room("oracle")
    user = TestUser(server, room)
    try:
        await eventually(lambda: worker.named("registered"), timeout=30, message="the official worker to register")
        await user.join(publish=False)
        status, body = await twirp(server, "AgentDispatchService", "CreateDispatch", {"agent_name": worker.agent_name, "room": room}, admin_token(server, room))
        assert status == 200, body

        await eventually(lambda: worker.named("joined"), timeout=30, message="the official worker to join with its job token")
        joined = worker.named("joined")
        assert joined["room"] == room and joined["identity"] == f"agent-{joined['job']}"
        assert joined["attributes"].get("lk.agent_name") == worker.agent_name

        await eventually(lambda: any(p.is_agent for p in user.room.remote_participants.values()), message="the agent to appear")
        agent = next(p for p in user.room.remote_participants.values() if p.is_agent)

        # TrackSubscribed reached the official SDK, or its wait would have timed out.
        await eventually(lambda: worker.named("subscribed"), timeout=25, message="the official SDK's wait for a subscriber to end")

        # Its audio got through, decoded at the far end.
        def loud_enough() -> bool:
            heard = user.heard.get(agent.identity)
            return heard is not None and sum(end - start for start, end in heard.speech_periods(0.05)) > 1.0

        await eventually(loud_enough, timeout=15, message="the official SDK's audio to be heard")

        # Its transcripts arrive in the user's name, marked as generated, whole.
        await eventually(lambda: worker.named("transcripts_sent"), timeout=15, message="the transcripts")
        await eventually(lambda: len(user.transcripts) >= 2, timeout=10, message="the transcripts to arrive")
        for t in user.transcripts:
            assert t["generated"] is True, t
            assert t["identity"] == "user", t
        assert any(t["text"] == "the words the user said" for t in user.transcripts)
        assert any(len(t["text"]) == 5 * 4000 for t in user.transcripts), [len(t["text"]) for t in user.transcripts]
    finally:
        await user.leave()
        await worker.stop()

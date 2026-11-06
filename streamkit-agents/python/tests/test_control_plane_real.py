"""The framework against a real control plane: our server with PostgreSQL, a project's own API key, stored provider keys and
session reports. There is no reference for this: it is our own product's API.

It needs the development database. STREAMKIT_TEST_DATABASE_URL, or its usual address, must reach a server where a database can be
created.
"""

from __future__ import annotations

import asyncio
import os
import subprocess
import time
import uuid

import httpx
import pytest

from conftest import _build_ours, _wait_http
from streamkit_agents import ControlPlaneCredentials, ControlPlaneSink, Credentials, Worker, mint_token, worker_token
from streamkit_agents.credentials import EnvironmentCredentials
from streamkit_agents.llm.base import Usage
from streamkit_agents.observability import Prices, SessionMetrics, TurnMetrics, UsageRecord

psycopg = pytest.importorskip("psycopg")

DB_URL = os.environ.get("STREAMKIT_TEST_DATABASE_URL", "postgres://streamkit:streamkit-dev-only@127.0.0.1:5433/streamkit?sslmode=disable")
MASTER_KEY = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
PORT = 58780
PROVIDER_KEY = "sk-provider-key-not-real"


@pytest.fixture(scope="module")
def control_plane(tmp_path_factory):
    try:
        admin = psycopg.connect(DB_URL.replace("?sslmode=disable", ""), autocommit=True, connect_timeout=3)
    except psycopg.OperationalError:
        pytest.skip("the development database is not running")
    name = f"streamkit_agents_{uuid.uuid4().hex[:10]}"
    admin.execute(f"CREATE DATABASE {name}")
    workdir = tmp_path_factory.mktemp("cp")
    binary = workdir / "streamkit-server"
    _build_ours(binary)
    config = workdir / "server.yaml"
    url = DB_URL.replace("/streamkit?", f"/{name}?")
    config.write_text(
        f"port: {PORT}\nrtc:\n  tcp_port: {PORT + 1}\n  udp_port: {PORT + 2}\n"
        f"control_plane:\n  database_url: {url}\n  master_key: {MASTER_KEY}\n  mail:\n    dir: {workdir}\n"
    )
    log = open(workdir / "server.log", "wb")
    proc = subprocess.Popen([str(binary), "--dev", "--config", str(config)], stdout=log, stderr=log)
    try:
        _wait_http(f"http://127.0.0.1:{PORT}/", proc)
        yield f"http://127.0.0.1:{PORT}", url
    finally:
        proc.terminate()
        proc.wait(10)
        admin.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")
        admin.close()


class Owner:
    def __init__(self, base: str):
        self.base = base
        self.client = httpx.AsyncClient(base_url=base + "/api/v1", timeout=10)
        self.project = ""

    async def setup(self) -> dict:
        email = f"owner-{uuid.uuid4().hex[:8]}@example.com"
        r = await self.client.post("/auth/register", json={"email": email, "password": "a-good-long-password", "name": "Test", "organization": email + " org"})
        assert r.status_code == 201, r.text
        r = await self.client.post("/projects", json={"name": "agents"})
        assert r.status_code == 201, r.text
        self.project = r.json()["id"]
        self.client.headers["X-Project-ID"] = self.project
        r = await self.client.post("/keys", json={"name": "worker"})
        assert r.status_code == 201, r.text
        return r.json()


async def test_worker_with_a_projects_key_reads_a_sealed_provider_key(control_plane):
    base, db_url = control_plane
    owner = Owner(base)
    key = await owner.setup()
    try:
        r = await owner.client.put("/provider-credentials", json={"provider": "openai", "secret": PROVIDER_KEY})
        assert r.status_code == 200 and PROVIDER_KEY not in r.text

        with psycopg.connect(db_url.replace("?sslmode=disable", "")) as db:
            sealed = db.execute("SELECT secret_sealed FROM provider_credentials").fetchone()[0]
        assert PROVIDER_KEY.encode() not in bytes(sealed), "the key is in the database in plain text"

        token = worker_token(key["api_key"], key["secret"])
        source = ControlPlaneCredentials(base, token)
        assert await source.get("openai") == PROVIDER_KEY
        assert await source.get("nobody") is None

        # The environment wins, so a worker can be pointed at its own key.
        os.environ["OPENAI_API_KEY"] = "from-the-environment"
        try:
            assert await Credentials([EnvironmentCredentials(), ControlPlaneCredentials(base, token)]).get("openai") == "from-the-environment"
        finally:
            del os.environ["OPENAI_API_KEY"]

        # A job's own token is for joining a room. It cannot read keys.
        job_like = mint_token(key["api_key"], key["secret"], identity="agent", video={"roomJoin": True, "room": "r", "agent": True})
        async with httpx.AsyncClient(timeout=10) as client:
            r = await client.post(f"{base}/api/v1/agent-credentials/resolve", json={"provider": "openai"}, headers={"Authorization": f"Bearer {job_like}"})
        assert r.status_code == 403 and PROVIDER_KEY not in r.text
    finally:
        await owner.client.aclose()


async def test_worker_registers_with_a_projects_key_and_stays_in_its_project(control_plane):
    base, _ = control_plane
    owner = Owner(base)
    other = Owner(base)
    key = await owner.setup()
    other_key = await other.setup()
    jobs = []

    async def entry(ctx):
        jobs.append(ctx.job)
        await ctx.connect()
        await ctx.wait_for_shutdown()

    def make(k):
        return Worker(entry, ws_url=base.replace("http", "ws"), api_key=k["api_key"], api_secret=k["secret"], agent_name="helper", load_fn=lambda: 0.0)

    worker = make(other_key)
    worker.start()
    await worker.wait_registered()
    mine = make(key)
    mine.start()
    await mine.wait_registered()
    try:
        room = f"cp-{uuid.uuid4().hex[:8]}"
        admin = mint_token(key["api_key"], key["secret"], identity="api", video={"roomAdmin": True, "room": room})
        async with httpx.AsyncClient(timeout=10) as client:
            r = await client.post(f"{base}/twirp/livekit.AgentDispatchService/CreateDispatch", json={"agent_name": "helper", "room": room}, headers={"Authorization": f"Bearer {admin}"})
        assert r.status_code == 200, r.text
        deadline = time.time() + 15
        while not jobs:
            assert time.time() < deadline, "the job did not arrive"
            await asyncio.sleep(0.05)
        assert len(mine.jobs) == 1 and not worker.jobs, "a job went to another project's worker"
    finally:
        await mine.aclose()
        await worker.aclose()
        await owner.client.aclose()
        await other.client.aclose()


async def test_session_reports_reach_the_dashboard_api(control_plane):
    base, _ = control_plane
    owner = Owner(base)
    key = await owner.setup()
    try:
        token = worker_token(key["api_key"], key["secret"])
        now = time.time()
        metrics = SessionMetrics(room="support", job_id="AJ_1", agent_name="helper", prices=Prices(llm_input_per_mtok=1.0, llm_output_per_mtok=2.0))
        metrics.pipeline = "cascaded"
        metrics.models = {"llm": "local/qwen"}
        metrics.sinks = [ControlPlaneSink(base, token)]
        turn = TurnMetrics(pipeline="cascaded", speech_ended_at=now, playout_started_at=now + 1.25, llm_started_at=now + 0.5, llm_first_token_at=now + 0.7, user_text="a private question", reply_text="a reply", played_text="a reply")
        turn.usage.append(UsageRecord("llm", "local/qwen", Usage(prompt_tokens=1000, completion_tokens=500)))
        await metrics.turn_finished(turn)
        await metrics.session_ended()

        r = await owner.client.get("/agent-sessions", params={"room": "support"})
        assert r.status_code == 200, r.text
        items = r.json()["items"]
        assert len(items) == 1
        item = items[0]
        assert item["agent_name"] == "helper" and item["pipeline"] == "cascaded" and item["turn_count"] == 1
        assert item["models"] == {"llm": "local/qwen"}
        assert item["avg_end_to_end"] == pytest.approx(1.25, abs=0.01)
        assert item["cost_usd"] == pytest.approx(0.002)
        assert "private question" not in r.text, "the list must not carry what people said"

        one = await owner.client.get(f"/agent-sessions/{item['id']}")
        assert one.status_code == 200
        body = one.json()["body"]
        assert body["turns"][0]["user_text"] == "a private question"
        assert body["turns"][0]["latency"]["llm_first_token"] == pytest.approx(0.2, abs=0.01)

        # A deployment that must not store what people said can leave it out.
        private = SessionMetrics(room="private", agent_name="helper")
        private.sinks = [ControlPlaneSink(base, token, include_text=False)]
        await private.turn_finished(TurnMetrics(user_text="secret words", reply_text="more", speech_ended_at=now, playout_started_at=now + 1))
        await private.session_ended()
        r = await owner.client.get("/agent-sessions", params={"room": "private"})
        one = await owner.client.get(f"/agent-sessions/{r.json()['items'][0]['id']}")
        assert "secret words" not in one.text
        # A turn with no prices and no reported cost has no cost, which is not the same as zero.
        assert r.json()["items"][0]["cost_usd"] is None
    finally:
        await owner.client.aclose()


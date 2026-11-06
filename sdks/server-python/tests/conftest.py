"""Starts the real servers once for the test run. Set STREAMKIT_TEST_DATABASE_URL to run the tests that need them, and
STREAMKIT_TEST_WORKERS=1 for the recording and ingest tests. See tests/sdk/fixture for what runs."""

import asyncio
import base64
import json
import os
import subprocess
import tempfile
import threading
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Optional

import pytest
import pytest_asyncio

from elnova_streamkit import StreamKitAPI

REPO = Path(__file__).resolve().parents[3]
_counter = 0


def room_name(tag: str) -> str:
    global _counter
    _counter += 1
    return f"sdk-python-{tag}-{os.getpid()}-{_counter}"


@pytest.fixture(scope="session")
def fx() -> Any:
    if not os.getenv("STREAMKIT_TEST_DATABASE_URL"):
        pytest.skip("STREAMKIT_TEST_DATABASE_URL is not set")
    binary = REPO / "bin" / "sdkfixture"
    subprocess.run(["go", "build", "-o", str(binary), "./tests/sdk/cmd/sdkfixture"], cwd=REPO, check=True)
    args = ["--workers"] if os.getenv("STREAMKIT_TEST_WORKERS") else []
    # Closing its standard input stops everything, so nothing outlives this run however it ends.
    child = subprocess.Popen([str(binary), *args], cwd=REPO, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    line = child.stdout.readline()
    if not line:
        raise RuntimeError(f"the fixture exited with {child.wait()}")
    yield json.loads(line)
    child.stdin.close()
    child.wait(timeout=60)


class Peer:
    """A real participant, run as the streamkit CLI: a real WebRTC session with real media."""

    def __init__(self, cli: str, server: dict, room: str, identity: str, *, publish: bool = False, no_auto_subscribe: bool = False, token: Optional[str] = None) -> None:
        self.identity = identity
        self.events: list[dict] = []
        args = [cli, "room", "join", room, "--json", "--url", server["url"], "--identity", identity]
        args += ["--token", token] if token else ["--api-key", server["api_key"], "--api-secret", server["api_secret"]]
        if publish:
            args.append("--publish-test-pattern")
        if no_auto_subscribe:
            args.append("--no-auto-subscribe")
        env = {**os.environ, "STREAMKIT_CONFIG_DIR": tempfile.gettempdir() + "/sdk-python-peers"}
        self._process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env)
        self._stderr = ""
        threading.Thread(target=self._read, daemon=True).start()
        threading.Thread(target=self._read_errors, daemon=True).start()

    def _read(self) -> None:
        for line in self._process.stdout:
            try:
                self.events.append(json.loads(line))
            except ValueError:
                pass

    def _read_errors(self) -> None:
        for line in self._process.stderr:
            self._stderr += line

    def _seen(self) -> str:
        return ",".join(e["event"] for e in self.events if e["event"] != "media")

    async def wait(self, what: str, check: Callable[[dict], bool], timeout: float = 30) -> dict:
        """The first event since the start of the session that check accepts."""
        deadline = time.monotonic() + timeout
        while True:
            for event in list(self.events):
                if check(event):
                    return event
            if self._process.poll() is not None:
                await asyncio.sleep(0.2)
                for event in list(self.events):
                    if check(event):
                        return event
                raise AssertionError(f"{self.identity} left without {what}. Saw {self._seen()}. Errors: {self._stderr}")
            if time.monotonic() > deadline:
                raise AssertionError(f"{self.identity}: timed out waiting for {what}. Saw {self._seen()}. Errors: {self._stderr}")
            await asyncio.sleep(0.05)

    async def connected(self) -> dict:
        return await self.wait("a connection", lambda e: e["event"] == "connected")

    async def data_channels(self) -> None:
        for label in ("_reliable", "_lossy"):
            await self.wait(f"data channel {label}", lambda e: e["event"] == "data_channel_open" and e["channel"] == label)

    async def published(self) -> list[dict]:
        """Waits until the server has accepted the test pattern's two tracks."""
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            tracks = [e for e in self.events if e["event"] == "track_published"]
            if len(tracks) >= 2:
                return tracks
            await asyncio.sleep(0.05)
        raise AssertionError(f"{self.identity}: the tracks were never published")

    def close(self) -> None:
        if self._process.poll() is None:
            self._process.terminate()
        try:
            self._process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self._process.kill()
            self._process.wait()


@dataclass
class Target:
    name: str
    server: dict
    api: StreamKitAPI
    cli: str
    peers: list

    def join(self, room: str, identity: str, **options: Any) -> Peer:
        peer = Peer(self.cli, self.server, room, identity, **options)
        self.peers.append(peer)
        return peer


@pytest_asyncio.fixture(params=["ours", "reference"])
async def target(request: pytest.FixtureRequest, fx: dict) -> Any:
    """This server and the reference, in turn. A test the reference fails tests the wrong thing."""
    server = fx[request.param]
    if server is None:
        pytest.skip("the reference server is not available")
    async with StreamKitAPI(server["url"], server["api_key"], server["api_secret"]) as api:
        t = Target(request.param, server, api, fx["cli"], [])
        yield t
        for peer in t.peers:
            peer.close()


@pytest_asyncio.fixture
async def ours(fx: dict) -> Any:
    server = fx["ours"]
    async with StreamKitAPI(server["url"], server["api_key"], server["api_secret"]) as api:
        t = Target("ours", server, api, fx["cli"], [])
        yield t
        for peer in t.peers:
            peer.close()


def data_event(payload: str) -> Callable[[dict], bool]:
    encoded = base64.b64encode(payload.encode()).decode()
    return lambda e: e["event"] == "data" and e.get("data_base64") == encoded


def captured_webhooks(fx: dict, which: str) -> list[dict]:
    """Webhooks as they arrived at the capture server."""
    with urllib.request.urlopen(f"{fx['webhooks']}/captured/{which}") as res:
        items = json.load(res)
    for item in items:
        item["body"] = base64.b64decode(item["body"])
    return items

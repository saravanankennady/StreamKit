"""Test fixtures: real servers and real engines, started for the tests and stopped by PID.

Each test that touches the protocol runs against both our server and the reference server. A test the reference fails is
testing the wrong thing. STREAMKIT_TEST_SERVERS=ours or =reference narrows a run to one.
"""

from __future__ import annotations

import os
import pathlib
import shutil
import socket
import subprocess
import tempfile
import time

import httpx
import pytest

HERE = pathlib.Path(__file__).resolve().parent
REPO = HERE.parents[2]
ROOT = pathlib.Path(os.environ.get("STREAMKIT_DEV_ROOT", "/run/media/nsroot/develop/streamin_engine"))
TOOLCHAIN = ROOT / ".toolchain"
REFERENCE_BIN = ROOT / "reference/oracle/bin/x/livekit-server"

API_KEY = "devkey"
API_SECRET = "secret"

PORTS = {"ours": 58880, "reference": 58980}
ENGINE_PORTS = {"llm": 57990, "stt": 57991, "tts": 57992}


def _wanted_servers() -> list[str]:
    value = os.environ.get("STREAMKIT_TEST_SERVERS", "ours,reference")
    return [s.strip() for s in value.split(",") if s.strip()]


def _wait_http(url: str, proc: subprocess.Popen, timeout: float = 90) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"{url}: the process exited with {proc.returncode}")
        try:
            if httpx.get(url, timeout=1).status_code == 200:
                return
        except httpx.HTTPError:
            pass
        time.sleep(0.1)
    raise RuntimeError(f"{url} did not come up")


def _build_ours(out: pathlib.Path) -> None:
    env = {
        **os.environ,
        "PATH": f"{TOOLCHAIN}/go/bin:{os.environ['PATH']}",
        "GOROOT": str(TOOLCHAIN / "go"),
        "GOPATH": str(TOOLCHAIN / "gopath"),
        "GOTMPDIR": str(TOOLCHAIN / "tmp"),
        "GOTOOLCHAIN": "local",
    }
    subprocess.run(["go", "build", "-o", str(out), "./server/cmd/streamkit-server"], cwd=REPO, env=env, check=True)


class RunningServer:
    def __init__(self, name: str, port: int, proc: subprocess.Popen, log: pathlib.Path):
        self.name = name
        self.port = port
        self.proc = proc
        self.log = log
        self.ws_url = f"ws://127.0.0.1:{port}"
        self.http_url = f"http://127.0.0.1:{port}"
        self.api_key = API_KEY
        self.api_secret = API_SECRET

    @property
    def is_reference(self) -> bool:
        return self.name == "reference"


@pytest.fixture(scope="session")
def _workdir():
    path = pathlib.Path(tempfile.mkdtemp(prefix="sk-agents-", dir=os.environ.get("TMPDIR")))
    yield path
    shutil.rmtree(path, ignore_errors=True)


@pytest.fixture(scope="session")
def _servers(_workdir):
    """Starts whichever servers are asked for, once per run."""
    running: dict[str, RunningServer] = {}
    for name in _wanted_servers():
        port = PORTS[name]
        log = _workdir / f"{name}.log"
        out = open(log, "wb")
        if name == "ours":
            binary = _workdir / "streamkit-server"
            _build_ours(binary)
            config = _workdir / "ours.yaml"
            config.write_text(f"port: {port}\nrtc:\n  tcp_port: {port + 1}\n  udp_port: {port + 2}\nroom:\n  departure_timeout: 20\n")
            proc = subprocess.Popen([str(binary), "--dev", "--config", str(config)], stdout=out, stderr=out)
        else:
            body = f"port: {port}\nrtc:\n  tcp_port: {port + 1}\n  udp_port: {port + 2}\n"
            proc = subprocess.Popen([str(REFERENCE_BIN), "--dev", "--bind", "127.0.0.1", "--config-body", body], stdout=out, stderr=out)
        server = RunningServer(name, port, proc, log)
        _wait_http(server.http_url + "/", proc)
        running[name] = server
    yield running
    for server in running.values():
        server.proc.terminate()
    for server in running.values():
        try:
            server.proc.wait(10)
        except subprocess.TimeoutExpired:
            server.proc.kill()


def pytest_generate_tests(metafunc):
    if "server" in metafunc.fixturenames:
        metafunc.parametrize("server", _wanted_servers(), indirect=True)


@pytest.fixture
def server(request, _servers) -> RunningServer:
    return _servers[request.param]


class Engines:
    """Local engines behind OpenAI-shaped APIs. See streamkit-agents/local-engines."""

    llm_url = f"http://127.0.0.1:{ENGINE_PORTS['llm']}/v1"
    stt_url = f"http://127.0.0.1:{ENGINE_PORTS['stt']}/v1"
    tts_url = f"http://127.0.0.1:{ENGINE_PORTS['tts']}/v1"
    llm_model = "qwen2.5-1.5b-instruct-q4_k_m"
    stt_model = "faster-whisper-base.en"
    tts_model = "piper-en_US-ljspeech-medium"
    tts_rate = 22050


def _engines_present() -> bool:
    return (TOOLCHAIN / "models/llm/qwen2.5-1.5b-instruct-q4_k_m.gguf").exists() and (TOOLCHAIN / "venvs/agents-engines").exists()


@pytest.fixture(scope="session")
def engines(_workdir):
    if not _engines_present():
        pytest.skip("the local engines are not installed under .toolchain")
    python = TOOLCHAIN / "venvs/agents-engines/bin/python"
    local = HERE.parents[1] / "local-engines"
    llama = TOOLCHAIN / "llama.cpp/llama-b12102"
    procs = []

    def start(name, args, env=None):
        log = open(_workdir / f"{name}.log", "wb")
        procs.append(subprocess.Popen(args, stdout=log, stderr=log, env={**os.environ, **(env or {})}))

    start("stt", [python, local / "stt_server.py", "--model", TOOLCHAIN / "models/whisper/base.en", "--port", str(ENGINE_PORTS["stt"]), "--threads", "4"])
    start("tts", [python, local / "tts_server.py", "--voice", TOOLCHAIN / "models/piper/en_US-ljspeech-medium.onnx", "--port", str(ENGINE_PORTS["tts"])])
    start(
        "llm",
        [llama / "llama-server", "-m", TOOLCHAIN / "models/llm/qwen2.5-1.5b-instruct-q4_k_m.gguf", "--host", "127.0.0.1", "--port", str(ENGINE_PORTS["llm"]), "-t", "8", "-c", "4096", "--jinja"],
        env={"LD_LIBRARY_PATH": str(llama)},
    )
    try:
        _wait_http(f"http://127.0.0.1:{ENGINE_PORTS['stt']}/", procs[0])
        _wait_http(f"http://127.0.0.1:{ENGINE_PORTS['tts']}/", procs[1])
        _wait_http(f"http://127.0.0.1:{ENGINE_PORTS['llm']}/health", procs[2])
        yield Engines()
    finally:
        for proc in procs:
            proc.terminate()
        for proc in procs:
            try:
                proc.wait(10)
            except subprocess.TimeoutExpired:
                proc.kill()


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]

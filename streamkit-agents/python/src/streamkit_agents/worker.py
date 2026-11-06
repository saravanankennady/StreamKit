"""A worker holds a WebSocket open to the server's /agent path, accepts jobs, and runs them.

Jobs run as tasks in the worker's process. One bad job can therefore hold up the others, or take them down, so run several
small workers rather than one with many jobs. Running each job in its own process is not implemented yet.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import os
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable

import websockets
from websockets.asyncio.client import connect

from ._proto import livekit_agent_pb2 as agent
from ._proto import livekit_models_pb2 as models
from .rtc.room import Participant, Room
from .token import worker_token

logger = logging.getLogger("streamkit_agents.worker")

ATTR_AGENT_NAME = "lk.agent.name"
STATUS_INTERVAL = 1.5
LOAD_SAMPLE_INTERVAL = 0.5
LOAD_WINDOW_SAMPLES = 5
MAX_RETRY_DELAY = 10
ASSIGNMENT_TIMEOUT = 7.5


class JobContext:
    """What an entrypoint gets. It connects when you say, so that you can get ready first."""

    def __init__(self, worker: "Worker", job: agent.Job, url: str, token: str):
        self._worker = worker
        self.job = job
        self.url = url
        self.token = token
        self.room = Room()
        self._shutdown = asyncio.Event()
        self.shutdown_reason = ""
        self._callbacks: list[Callable[[], Any]] = []

    @property
    def job_id(self) -> str:
        return self.job.id

    @property
    def metadata(self) -> str:
        return self.job.metadata

    @property
    def agent_name(self) -> str:
        return self.job.agent_name

    async def connect(self, *, auto_subscribe: bool = True) -> None:
        await self.room.connect(self.url, self.token, auto_subscribe=auto_subscribe)
        self.room.on("disconnected", lambda *_: self.shutdown("disconnected from the room"))
        await self._worker._job_running(self.job.id)

    def add_shutdown_callback(self, callback: Callable[[], Any]) -> None:
        self._callbacks.append(callback)

    def shutdown(self, reason: str = "") -> None:
        """Ends the job. The entrypoint is cancelled if it has not returned by then."""
        if not self._shutdown.is_set():
            self.shutdown_reason = reason
            self._shutdown.set()

    async def wait_for_shutdown(self) -> None:
        await self._shutdown.wait()

    async def wait_for_participant(self, *, identity: str | None = None, timeout: float | None = None) -> Participant:
        """Waits for someone other than an agent to be in the room."""

        def match(p: Participant) -> bool:
            return p.identity == identity if identity else not p.is_agent

        for p in self.room.remote_participants.values():
            if match(p):
                return p
        arrived: asyncio.Future[Participant] = asyncio.get_running_loop().create_future()

        def on_connected(p: Participant) -> None:
            if match(p) and not arrived.done():
                arrived.set_result(p)

        self.room.on("participant_connected", on_connected)
        try:
            return await asyncio.wait_for(arrived, timeout)
        finally:
            self.room.off("participant_connected", on_connected)


@dataclass
class JobRequest:
    """An offered job, for a worker's request function to decide on. If it does nothing, the job is declined."""

    job: agent.Job
    _answer: asyncio.Future = field(repr=False, default=None)

    async def accept(self, *, identity: str = "", name: str = "", metadata: str = "", attributes: dict[str, str] | None = None) -> None:
        if not self._answer.done():
            self._answer.set_result(agent.AvailabilityResponse(
                job_id=self.job.id,
                available=True,
                participant_identity=identity or f"agent-{self.job.id}",
                participant_name=name,
                participant_metadata=metadata,
                participant_attributes=attributes or {},
            ))

    async def reject(self, *, terminate: bool = False) -> None:
        if not self._answer.done():
            self._answer.set_result(agent.AvailabilityResponse(job_id=self.job.id, available=False, terminate=terminate))


Entrypoint = Callable[[JobContext], Awaitable[None]]
RequestFn = Callable[[JobRequest], Awaitable[None]]


async def _accept_all(request: JobRequest) -> None:
    await request.accept()


class CPULoad:
    """Average use of the machine's CPUs over the last couple of seconds, from /proc/stat."""

    def __init__(self) -> None:
        self._samples: list[float] = []
        self._last: tuple[int, int] | None = self._read()

    @staticmethod
    def _read() -> tuple[int, int] | None:
        try:
            with open("/proc/stat") as f:
                parts = f.readline().split()[1:]
        except OSError:
            return None
        values = [int(v) for v in parts]
        return values[3] + (values[4] if len(values) > 4 else 0), sum(values)

    def sample(self) -> None:
        current = self._read()
        if current is None or self._last is None:
            return
        idle, total = current[0] - self._last[0], current[1] - self._last[1]
        self._last = current
        if total > 0:
            self._samples.append(1 - idle / total)
            del self._samples[:-LOAD_WINDOW_SAMPLES]

    @property
    def value(self) -> float:
        return sum(self._samples) / len(self._samples) if self._samples else 0.0


class Worker:
    def __init__(
        self,
        entrypoint: Entrypoint,
        *,
        ws_url: str | None = None,
        api_key: str | None = None,
        api_secret: str | None = None,
        agent_name: str = "",
        job_type: int = agent.JT_ROOM,
        request_fn: RequestFn = _accept_all,
        load_fn: Callable[[], float] | None = None,
        load_threshold: float = 0.7,
        permissions: models.ParticipantPermission | None = None,
        deployment: str = "",
        version: str = "0.1.0",
    ):
        self.entrypoint = entrypoint
        self.ws_url = ws_url or os.environ.get("STREAMKIT_URL") or ""
        self.api_key = api_key or os.environ.get("STREAMKIT_API_KEY") or ""
        self.api_secret = api_secret or os.environ.get("STREAMKIT_API_SECRET") or ""
        if not (self.ws_url and self.api_key and self.api_secret):
            raise ValueError("a worker needs a URL, an API key and an API secret")
        self.agent_name = agent_name
        self.job_type = job_type
        self.request_fn = request_fn
        self.load_threshold = load_threshold
        self.permissions = permissions
        self.deployment = deployment
        self.version = version
        self.worker_id = ""
        self.server_info: models.ServerInfo | None = None
        self.jobs: dict[str, tuple[JobContext, asyncio.Task]] = {}
        self._cpu = CPULoad() if load_fn is None else None
        self._load_fn = load_fn
        self._ws = None
        self._send_lock = asyncio.Lock()
        self._closed = False
        self._draining = False
        self._reserved = 0
        self._assignments: dict[str, asyncio.Future] = {}
        self._registered = asyncio.Event()
        self._run_task: asyncio.Task | None = None
        self._background: set[asyncio.Task] = set()

    @property
    def load(self) -> float:
        return self._load_fn() if self._load_fn else self._cpu.value

    def _background_task(self, coro) -> asyncio.Task:
        task = asyncio.create_task(coro)
        self._background.add(task)
        task.add_done_callback(self._background.discard)
        return task

    def start(self) -> asyncio.Task:
        self._run_task = asyncio.create_task(self.run())
        return self._run_task

    async def wait_registered(self, timeout: float = 15) -> None:
        await asyncio.wait_for(self._registered.wait(), timeout)

    async def run(self) -> None:
        """Stays connected, reconnecting with a growing delay, until closed."""
        retries = 0
        while not self._closed:
            try:
                await self._connect_once()
                retries = 0
            except asyncio.CancelledError:
                raise
            except (OSError, websockets.exceptions.WebSocketException, asyncio.TimeoutError) as e:
                # A refusal is worth seeing, but the token that caused it is not.
                logger.warning("worker connection failed: %s", type(e).__name__)
            self._registered.clear()
            if self._closed:
                return
            retries += 1
            await asyncio.sleep(min(retries * 2, MAX_RETRY_DELAY))

    async def _connect_once(self) -> None:
        url = self.ws_url.rstrip("/")
        if url.startswith("http"):
            url = "ws" + url[len("http"):]
        token = worker_token(self.api_key, self.api_secret)
        async with connect(f"{url}/agent", additional_headers={"Authorization": f"Bearer {token}"}, max_size=16 << 20) as ws:
            self._ws = ws
            await self._send(agent.WorkerMessage(register=agent.RegisterWorkerRequest(
                type=self.job_type,
                agent_name=self.agent_name,
                version=self.version,
                allowed_permissions=self.permissions,
                deployment=self.deployment,
            )))
            first = agent.ServerMessage()
            first.ParseFromString(await asyncio.wait_for(ws.recv(), 10))
            if first.WhichOneof("message") != "register":
                raise websockets.exceptions.InvalidMessage("the server did not confirm the registration")
            self.worker_id = first.register.worker_id
            self.server_info = first.register.server_info
            self._registered.set()
            logger.info("worker registered as %s", self.worker_id)
            if self.jobs:
                # The server may have forgotten who holds these jobs.
                await self._send(agent.WorkerMessage(migrate_job=agent.MigrateJobRequest(job_ids=list(self.jobs))))
            status = self._background_task(self._status_loop())
            try:
                async for raw in ws:
                    if isinstance(raw, str):
                        continue
                    message = agent.ServerMessage()
                    message.ParseFromString(raw)
                    self._on_message(message)
            finally:
                status.cancel()
                self._ws = None

    async def _send(self, message: agent.WorkerMessage) -> None:
        ws = self._ws
        if ws is None:
            return
        async with self._send_lock:
            await ws.send(message.SerializeToString())

    def _on_message(self, message: agent.ServerMessage) -> None:
        kind = message.WhichOneof("message")
        if kind == "availability":
            self._background_task(self._answer_offer(message.availability.job))
        elif kind == "assignment":
            future = self._assignments.pop(message.assignment.job.id, None)
            if future is not None and not future.done():
                future.set_result(message.assignment)
            else:
                logger.warning("assignment for a job not waiting for one: %s", message.assignment.job.id)
        elif kind == "termination":
            entry = self.jobs.get(message.termination.job_id)
            if entry is not None:
                entry[0].shutdown("the server terminated the job")

    async def _answer_offer(self, job: agent.Job) -> None:
        answer: asyncio.Future[agent.AvailabilityResponse] = asyncio.get_running_loop().create_future()
        self._reserved += 1
        try:
            request = JobRequest(job=job, _answer=answer)
            try:
                await asyncio.wait_for(self.request_fn(request), 8)
            except asyncio.TimeoutError:
                logger.warning("the request function took too long on job %s", job.id)
            except Exception:
                logger.exception("the request function failed on job %s", job.id)
            if not answer.done():
                answer.set_result(agent.AvailabilityResponse(job_id=job.id, available=False))
            response = answer.result()
            if response.available:
                # Our name, for clients looking for an agent by attribute. The server adds its own similar key.
                response.participant_attributes[ATTR_AGENT_NAME] = self.agent_name
            assignment_future = asyncio.get_running_loop().create_future()
            if response.available:
                self._assignments[job.id] = assignment_future
            await self._send(agent.WorkerMessage(availability=response))
            if not response.available:
                return
            try:
                assignment = await asyncio.wait_for(assignment_future, ASSIGNMENT_TIMEOUT)
            except asyncio.TimeoutError:
                self._assignments.pop(job.id, None)
                logger.warning("no assignment arrived for job %s", job.id)
                return
            self._launch(assignment)
        finally:
            self._reserved -= 1

    def _launch(self, assignment: agent.JobAssignment) -> None:
        url = assignment.url or self.ws_url
        ctx = JobContext(self, assignment.job, url, assignment.token)
        task = self._background_task(self._run_job(ctx))
        self.jobs[assignment.job.id] = (ctx, task)

    async def _job_running(self, job_id: str) -> None:
        await self._send(agent.WorkerMessage(update_job=agent.UpdateJobStatus(job_id=job_id, status=agent.JS_RUNNING)))

    async def _run_job(self, ctx: JobContext) -> None:
        status = agent.JS_SUCCESS
        error = ""
        entry = asyncio.create_task(self.entrypoint(ctx))
        waiter = asyncio.create_task(ctx.wait_for_shutdown())
        try:
            await asyncio.wait({entry, waiter}, return_when=asyncio.FIRST_COMPLETED)
            if entry.done():
                exception = entry.exception() if not entry.cancelled() else None
                if exception is not None:
                    logger.error("job %s failed", ctx.job.id, exc_info=exception)
                    status = agent.JS_FAILED
                    # The type alone: exception text can hold things that should not leave the worker.
                    error = type(exception).__name__
                else:
                    # An entrypoint may return while its session carries on. The job lasts until it is shut down.
                    await waiter
            else:
                entry.cancel()
                try:
                    await entry
                except (asyncio.CancelledError, Exception):
                    pass
        finally:
            waiter.cancel()
            for callback in ctx._callbacks:
                try:
                    result = callback()
                    if inspect.isawaitable(result):
                        await result
                except Exception:
                    logger.exception("a shutdown callback failed")
            await ctx.room.disconnect()
            self.jobs.pop(ctx.job.id, None)
            try:
                await self._send(agent.WorkerMessage(update_job=agent.UpdateJobStatus(job_id=ctx.job.id, status=status, error=error)))
            except websockets.exceptions.WebSocketException:
                pass

    async def _status_loop(self) -> None:
        elapsed = 0.0
        while True:
            await asyncio.sleep(LOAD_SAMPLE_INTERVAL)
            if self._cpu is not None:
                self._cpu.sample()
            elapsed += LOAD_SAMPLE_INTERVAL
            if elapsed >= STATUS_INTERVAL:
                elapsed = 0.0
                await self._report_status()

    async def _report_status(self) -> None:
        load = self.load
        full = self._draining or load + self._reserved * 0.05 >= self.load_threshold
        try:
            await self._send(agent.WorkerMessage(update_worker=agent.UpdateWorkerStatus(
                status=agent.WS_FULL if full else agent.WS_AVAILABLE,
                load=load,
                job_count=len(self.jobs),
            )))
        except websockets.exceptions.WebSocketException:
            pass

    async def drain(self) -> None:
        """Takes no new jobs, and waits for the current ones to finish."""
        self._draining = True
        await self._report_status()
        while self.jobs:
            await asyncio.sleep(0.2)

    async def aclose(self) -> None:
        self._closed = True
        for ctx, _ in list(self.jobs.values()):
            ctx.shutdown("the worker is closing")
        for _, task in list(self.jobs.values()):
            try:
                await asyncio.wait_for(asyncio.shield(task), 5)
            except (asyncio.TimeoutError, asyncio.CancelledError):
                pass
        if self._ws is not None:
            await self._ws.close()
        if self._run_task is not None:
            self._run_task.cancel()
            try:
                await self._run_task
            except (asyncio.CancelledError, Exception):
                pass
        for task in list(self._background):
            task.cancel()


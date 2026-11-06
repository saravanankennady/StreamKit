"""What an agent session did and how fast: one record per turn, with its latency breakdown, usage and cost.

Everything here is measured or reported by a provider. A number that was not measured stays None, so an absent value can
never pass for a real one. Cost is never guessed: it comes from the provider's own report, or from prices you supply.
"""

from __future__ import annotations

import json
import logging
import time
import uuid
from dataclasses import asdict, dataclass, field
from typing import Any, Protocol

import httpx

from .llm.base import Usage

logger = logging.getLogger("streamkit_agents.observability")


def _since(end: float | None, start: float | None) -> float | None:
    if end is None or start is None:
        return None
    return round(end - start, 4)


@dataclass
class Prices:
    """Your prices, in US dollars. The framework ships none, because they change and differ by contract."""

    llm_input_per_mtok: float | None = None
    llm_cached_input_per_mtok: float | None = None
    llm_output_per_mtok: float | None = None
    stt_per_minute: float | None = None
    tts_per_mchar: float | None = None
    realtime_audio_input_per_mtok: float | None = None
    realtime_audio_output_per_mtok: float | None = None


@dataclass
class UsageRecord:
    """Usage from one provider call, with the model it came from."""

    kind: str  # llm, stt, tts or realtime
    model: str
    usage: Usage

    def cost(self, prices: Prices | None) -> float | None:
        if self.usage.reported_cost is not None:
            return self.usage.reported_cost
        if prices is None:
            return None
        u = self.usage
        if self.kind == "stt" and prices.stt_per_minute is not None:
            return u.audio_seconds / 60 * prices.stt_per_minute
        if self.kind == "tts" and prices.tts_per_mchar is not None:
            return u.characters / 1e6 * prices.tts_per_mchar
        if self.kind in ("llm", "realtime") and prices.llm_input_per_mtok is not None and prices.llm_output_per_mtok is not None:
            uncached = u.prompt_tokens - u.cached_tokens - u.audio_input_tokens
            total = uncached / 1e6 * prices.llm_input_per_mtok + (u.completion_tokens - u.audio_output_tokens) / 1e6 * prices.llm_output_per_mtok
            if u.cached_tokens:
                if prices.llm_cached_input_per_mtok is None:
                    return None
                total += u.cached_tokens / 1e6 * prices.llm_cached_input_per_mtok
            if u.audio_input_tokens:
                if prices.realtime_audio_input_per_mtok is None:
                    return None
                total += u.audio_input_tokens / 1e6 * prices.realtime_audio_input_per_mtok
            if u.audio_output_tokens:
                if prices.realtime_audio_output_per_mtok is None:
                    return None
                total += u.audio_output_tokens / 1e6 * prices.realtime_audio_output_per_mtok
            return total
        return None


@dataclass
class TurnMetrics:
    """Wall clock times of one turn, in seconds since the epoch. The latency properties are differences of these."""

    turn_id: str = field(default_factory=lambda: uuid.uuid4().hex[:12])
    speech_started_at: float | None = None
    speech_ended_at: float | None = None
    transcript_at: float | None = None
    turn_committed_at: float | None = None
    llm_started_at: float | None = None
    llm_first_token_at: float | None = None
    llm_finished_at: float | None = None
    tts_started_at: float | None = None
    tts_first_audio_at: float | None = None
    # When the first reply audio left for the network.
    playout_started_at: float | None = None
    playout_finished_at: float | None = None
    user_text: str = ""
    reply_text: str = ""
    played_text: str = ""
    interrupted: bool = False
    pipeline: str = ""
    usage: list[UsageRecord] = field(default_factory=list)
    tool_calls: list[str] = field(default_factory=list)

    @property
    def transcription_delay(self) -> float | None:
        return _since(self.transcript_at, self.speech_ended_at)

    @property
    def end_of_turn_delay(self) -> float | None:
        return _since(self.turn_committed_at, self.speech_ended_at)

    @property
    def llm_time_to_first_token(self) -> float | None:
        return _since(self.llm_first_token_at, self.llm_started_at)

    @property
    def tts_time_to_first_audio(self) -> float | None:
        return _since(self.tts_first_audio_at, self.tts_started_at)

    @property
    def end_to_end(self) -> float | None:
        """From the user finishing speaking to the first reply audio leaving. Network and playback are not included."""
        return _since(self.playout_started_at, self.speech_ended_at)

    def to_dict(self, prices: Prices | None = None) -> dict[str, Any]:
        costs = [r.cost(prices) for r in self.usage]
        return {
            "turn_id": self.turn_id,
            "pipeline": self.pipeline,
            "user_text": self.user_text,
            "reply_text": self.reply_text,
            "played_text": self.played_text,
            "interrupted": self.interrupted,
            "tool_calls": self.tool_calls,
            "started_at": self.speech_started_at or self.llm_started_at,
            "latency": {
                "transcription": self.transcription_delay,
                "end_of_turn": self.end_of_turn_delay,
                "llm_first_token": self.llm_time_to_first_token,
                "tts_first_audio": self.tts_time_to_first_audio,
                "end_to_end": self.end_to_end,
            },
            "usage": [{"kind": r.kind, "model": r.model, **_usage_dict(r.usage), "cost_usd": r.cost(prices)} for r in self.usage],
            # A partial total would look complete, so one unknown cost makes the turn's total unknown.
            "cost_usd": sum(c for c in costs) if costs and all(c is not None for c in costs) else None,
        }


def _usage_dict(u: Usage) -> dict[str, Any]:
    out = asdict(u)
    out.pop("raw", None)
    return out


class MetricsSink(Protocol):
    async def on_turn(self, session: "SessionMetrics", turn: TurnMetrics) -> None: ...

    async def on_session_end(self, session: "SessionMetrics") -> None: ...


class SessionMetrics:
    def __init__(self, *, room: str = "", job_id: str = "", agent_name: str = "", sinks: list[MetricsSink] | None = None, prices: Prices | None = None):
        self.session_id = uuid.uuid4().hex
        self.room = room
        self.job_id = job_id
        self.agent_name = agent_name
        self.models: dict[str, str] = {}
        self.pipeline = ""
        self.started_at = time.time()
        self.ended_at: float | None = None
        self.turns: list[TurnMetrics] = []
        self.errors: list[dict[str, Any]] = []
        self.handoffs: list[dict[str, Any]] = []
        self.sinks = sinks or []
        self.prices = prices

    def record_error(self, source: str, error: Exception) -> None:
        self.errors.append({"at": time.time(), "source": source, "message": str(error)[:500]})

    async def turn_finished(self, turn: TurnMetrics) -> None:
        self.turns.append(turn)
        for sink in self.sinks:
            try:
                await sink.on_turn(self, turn)
            except Exception:
                logger.exception("metrics sink failed")

    async def session_ended(self) -> None:
        self.ended_at = time.time()
        for sink in self.sinks:
            try:
                await sink.on_session_end(self)
            except Exception:
                logger.exception("metrics sink failed")

    def to_dict(self) -> dict[str, Any]:
        turns = [t.to_dict(self.prices) for t in self.turns]
        costs = [t["cost_usd"] for t in turns]
        return {
            "session_id": self.session_id,
            "room": self.room,
            "job_id": self.job_id,
            "agent_name": self.agent_name,
            "pipeline": self.pipeline,
            "models": self.models,
            "started_at": self.started_at,
            "ended_at": self.ended_at,
            "turns": turns,
            "errors": self.errors,
            "handoffs": self.handoffs,
            "cost_usd": sum(c for c in costs) if costs and all(c is not None for c in costs) else None,
        }


class JsonlSink:
    """Appends one JSON line per turn and session. Transcripts are personal data, so choose the file's place with care."""

    def __init__(self, path: str):
        self.path = path

    def _write(self, record: dict[str, Any]) -> None:
        with open(self.path, "a", encoding="utf-8") as f:
            f.write(json.dumps(record) + "\n")

    async def on_turn(self, session: SessionMetrics, turn: TurnMetrics) -> None:
        self._write({"type": "turn", "session_id": session.session_id, **turn.to_dict(session.prices)})

    async def on_session_end(self, session: SessionMetrics) -> None:
        self._write({"type": "session", **session.to_dict()})


class ControlPlaneSink:
    """Sends sessions to the control plane, where the dashboard API reads them. It authenticates with an access token
    holding the agent grant, signed with the project's API key, and so it can only write to that project.

    include_text=False leaves transcripts out, for deployments that must not store what people said.
    """

    def __init__(self, api_url: str, token: str, *, include_text: bool = True):
        self._url = api_url.rstrip("/") + "/api/v1/agent-sessions"
        self._headers = {"Authorization": f"Bearer {token}"}
        self._include_text = include_text

    async def on_turn(self, session: SessionMetrics, turn: TurnMetrics) -> None:
        return None

    async def on_session_end(self, session: SessionMetrics) -> None:
        payload = session.to_dict()
        if not self._include_text:
            for t in payload["turns"]:
                t["user_text"] = t["reply_text"] = t["played_text"] = ""
        async with httpx.AsyncClient(timeout=10) as client:
            response = await client.post(self._url, json=payload, headers=self._headers)
        if response.status_code >= 400:
            logger.warning("control plane refused the session: %d", response.status_code)

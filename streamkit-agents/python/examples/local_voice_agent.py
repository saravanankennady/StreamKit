#!/usr/bin/env python3
"""A voice agent that runs entirely on local engines: Whisper, a small GGUF model and Piper, with no vendor keys.

Start the engines as in streamkit-agents/local-engines/README.md, then:

    STREAMKIT_URL=ws://127.0.0.1:7880 STREAMKIT_API_KEY=devkey STREAMKIT_API_SECRET=secret \\
    STREAMKIT_LOCAL_LLM_URL=http://127.0.0.1:57990/v1 STREAMKIT_LOCAL_STT_URL=http://127.0.0.1:57991/v1 \\
    STREAMKIT_LOCAL_TTS_URL=http://127.0.0.1:57992/v1 SILERO_VAD_MODEL=/path/to/silero_vad.onnx \\
    python local_voice_agent.py

Any model can be chosen per room through dispatch metadata, such as {"models": {"llm": "anthropic/claude-haiku-5-5"}}, if
that provider's key is in this process's environment.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import signal

from streamkit_agents import Agent, AgentSession, JobContext, JsonlSink, ModelChoice, ModelSelector, RunContext, SileroVAD, Worker, function_tool


@function_tool
async def get_time(ctx: RunContext) -> str:
    """Tells the current time."""
    import time

    return time.strftime("%H:%M")


class Assistant(Agent):
    def __init__(self) -> None:
        super().__init__(
            instructions="You are a voice assistant. Your words are spoken aloud, so answer in one short sentence, with no lists or symbols.",
            tools=[get_time],
        )

    async def on_enter(self) -> None:
        self.session.say("Hello, how can I help?")


DEFAULTS = ModelChoice(
    stt="local/faster-whisper-base.en",
    llm="local/qwen2.5-1.5b-instruct-q4_k_m",
    tts="local/piper-en_US-ljspeech-medium",
)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--agent-name", default="")
    parser.add_argument("--metrics-file", default=os.environ.get("STREAMKIT_METRICS_FILE"))
    parser.add_argument("--no-greeting", action="store_true")
    parser.add_argument("--dev", action="store_true", help="take jobs however busy the machine is")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO)
    selector = ModelSelector()
    sinks = [JsonlSink(args.metrics_file)] if args.metrics_file else []

    async def entrypoint(ctx: JobContext) -> None:
        chosen = DEFAULTS.overlay(ModelChoice.from_job_metadata(ctx.metadata))
        await ctx.connect()
        session = AgentSession(
            stt=await selector.stt(chosen.stt, language="en"),
            llm=await selector.llm(chosen.llm),
            tts=await selector.tts(chosen.tts),
            vad=SileroVAD(),
            metrics_sinks=sinks,
        )
        ctx.add_shutdown_callback(session.aclose)
        session.on("close", lambda: ctx.shutdown("the session ended"))
        agent = Assistant()
        if args.no_greeting:
            agent.on_enter = _do_nothing  # type: ignore[method-assign]
        await session.start(agent, room=ctx.room, job_id=ctx.job_id, agent_name=ctx.agent_name)

    async def run() -> None:
        worker = Worker(entrypoint, agent_name=args.agent_name, load_threshold=float("inf") if args.dev else 0.7)
        stop = asyncio.Event()
        for sig in (signal.SIGINT, signal.SIGTERM):
            asyncio.get_running_loop().add_signal_handler(sig, stop.set)
        worker.start()
        await worker.wait_registered()
        print("worker registered", flush=True)
        await stop.wait()
        await worker.aclose()

    asyncio.run(run())


async def _do_nothing() -> None:
    return None


if __name__ == "__main__":
    main()

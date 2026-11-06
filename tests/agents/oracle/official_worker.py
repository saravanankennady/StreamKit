#!/usr/bin/env python3
"""The official Python worker and rtc SDK, unmodified, as an oracle for our server's side of the protocol.

It registers on /agent, takes a job, joins with the job's token, publishes a tone, and sends transcripts in a user's name. A test
watches the far end. If our server's /agent, job tokens or TrackSubscribed were wrong, this is where it would show.

Run with the agents-oracle virtualenv and tests/agents/oracle/stub on PYTHONPATH.
"""

import asyncio
import json
import logging
import os

import numpy as np
from livekit import rtc
from livekit.agents import AgentServer, JobContext, JobExecutorType


def report(event: dict) -> None:
    print(json.dumps(event), flush=True)


async def run_job(ctx: JobContext) -> None:
    await ctx.connect()
    local = ctx.room.local_participant
    report({"event": "joined", "room": ctx.room.name, "job": ctx.job.id, "identity": local.identity, "kind": int(local.kind), "attributes": dict(local.attributes)})

    source = rtc.AudioSource(48000, 1)
    track = rtc.LocalAudioTrack.create_audio_track("oracle_audio", source)
    publication = await local.publish_track(track, rtc.TrackPublishOptions(source=rtc.TrackSource.SOURCE_MICROPHONE))
    report({"event": "published", "sid": publication.sid})

    # The framework holds its audio until this resolves, so this is the wait the server has to end.
    await asyncio.wait_for(publication.wait_for_subscription(), 20)
    report({"event": "subscribed"})

    samples = 480
    tone = (np.sin(2 * np.pi * 330 * np.arange(samples) / 48000) * 0.3 * 32767).astype(np.int16)
    for _ in range(300):
        await source.capture_frame(rtc.AudioFrame(tone.tobytes(), 48000, 1, samples))
    report({"event": "spoke"})

    # Transcripts in the user's name: one long enough to go as chunks, and a short one.
    user = next(iter(ctx.room.remote_participants), "user")
    for label, text in (("long", "word " * 4000), ("short", "the words the user said")):
        writer = await local.stream_text(topic="lk.transcription", sender_identity=user, attributes={"which": label})
        await writer.write(text)
        await writer.aclose()
    report({"event": "transcripts_sent"})
    await asyncio.sleep(60)


def main() -> None:
    logging.basicConfig(level=logging.WARNING)
    server = AgentServer(
        ws_url=os.environ["LIVEKIT_URL"],
        api_key=os.environ["LIVEKIT_API_KEY"],
        api_secret=os.environ["LIVEKIT_API_SECRET"],
        job_executor_type=JobExecutorType.THREAD,
        num_idle_processes=0,
        port=0,
    )

    @server.rtc_session(agent_name=os.environ["AGENT_NAME"])
    async def entrypoint(ctx: JobContext) -> None:
        await run_job(ctx)

    @server.on("worker_registered")
    def registered(worker_id, server_info) -> None:
        report({"event": "registered", "worker_id": worker_id, "server_version": server_info.version})

    asyncio.run(server.run(devmode=True))


if __name__ == "__main__":
    main()

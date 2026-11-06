"""Small pieces with exact answers. No servers, and no engines."""

from __future__ import annotations

import asyncio
import time

import numpy as np

from fixtures import pcm_tone
from streamkit_agents.avatar import ATTR_PUBLISH_ON_BEHALF, avatar_join
from streamkit_agents.llm import ChatContext, FunctionCall, FunctionCallOutput
from streamkit_agents.providers._http import WavHeaderSkipper
from streamkit_agents.rtc import AudioFrame, AudioSource, resample, rms_level
from streamkit_agents.token import mint_token, worker_token
from streamkit_agents.tts import PCMFramer
from streamkit_agents.vad import EnergyVAD, SileroVAD, VADEventType
from streamkit_agents.voice import GrowingTranscript, PlayoutTranscript

import base64
import json
import os

import pytest


def decode_jwt(token: str) -> dict:
    payload = token.split(".")[1]
    return json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))


def test_playout_transcript_follows_the_audio():
    t = PlayoutTranscript()
    t.begin_segment("one two three four")
    t.add_audio(2.0)
    t.begin_segment("five six")
    t.add_audio(1.0)
    t.finish()
    assert t.text_at(0.0) == ""
    assert t.text_at(1.0) == "one two"
    assert t.text_at(2.0) == "one two three four"
    assert t.text_at(2.5) == "one two three four five"
    assert t.text_at(3.0) == "one two three four five six"
    assert t.full_text == "one two three four five six"


def test_growing_transcript_does_not_run_ahead_of_speech():
    t = GrowingTranscript()
    t.add_text(" ".join(f"w{i}" for i in range(100)))
    # All the words have arrived, and only a little audio. What has played cannot be all of them.
    t.set_audio(10.0, complete=False)
    assert len(t.text_at(1.0).split()) <= 3
    # Once the audio is complete, words are spread over it.
    t.set_audio(40.0, complete=True)
    assert len(t.text_at(10.0).split()) == 25
    assert len(t.text_at(40.0).split()) == 100


def test_chat_context_truncate_keeps_calls_with_their_results():
    ctx = ChatContext()
    ctx.add_message("system", "instructions")
    ctx.add_message("user", "one")
    ctx.add(FunctionCall(call_id="c", name="t", arguments="{}"))
    ctx.add(FunctionCallOutput(call_id="c", name="t", output="x"))
    ctx.add_message("assistant", "two")
    ctx.truncate(2)
    kinds = [type(i).__name__ for i in ctx]
    # The output's call was cut, so the output goes too. An orphan would be rejected by every provider.
    assert kinds == ["ChatMessage", "ChatMessage"]
    assert ctx.items[0].role == "system" and ctx.items[1].content == "two"


def test_set_instructions_replaces_rather_than_adds():
    ctx = ChatContext()
    ctx.add_message("user", "hello")
    ctx.set_instructions("first")
    ctx.set_instructions("second")
    assert [(m.role, m.content) for m in ctx.messages()] == [("system", "second"), ("user", "hello")]


def test_anthropic_format_starts_with_the_user():
    ctx = ChatContext()
    ctx.add_message("system", "s")
    ctx.add_message("assistant", "Hello, how can I help?")
    ctx.add_message("user", "hi")
    system, messages = ctx.to_anthropic()
    assert system == "s"
    assert [m["role"] for m in messages] == ["user", "assistant", "user"]


def test_wav_header_skipper():
    data = bytes(range(100))
    header = b"RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00" + bytes(16) + b"data\x00\x00\x00\x00"
    skipper = WavHeaderSkipper()
    out = b"".join(skipper.push(header[i:i + 7]) for i in range(0, len(header), 7)) + skipper.push(data)
    assert out == data
    plain = WavHeaderSkipper()
    assert plain.push(b"not a wav file at all") + plain.push(b"!") == b"not a wav file at all!"


def test_pcm_framer_handles_odd_lengths():
    framer = PCMFramer(16000, frame_ms=10)
    frames = framer.push(b"\x01" * 321)
    assert [len(f.data) for f in frames] == [320]
    assert framer.flush() is None
    framer.push(b"\x01" * 3)
    assert len(framer.flush().data) == 4


def test_resample_keeps_duration_and_pitch():
    rate = 48000
    frame = AudioFrame(pcm_tone(1.0, rate, frequency=440), rate)
    out = resample(frame, 16000)
    assert abs(out.duration - 1.0) < 0.02
    samples = np.frombuffer(out.data, dtype=np.int16).astype(float)
    spectrum = np.abs(np.fft.rfft(samples))
    peak = np.argmax(spectrum) * 16000 / len(samples)
    assert abs(peak - 440) < 5


async def test_audio_source_paces_pauses_and_drops():
    source = AudioSource()
    frames = []

    async def drain():
        while True:
            frames.append(await source.recv())

    drain_task = asyncio.create_task(drain())
    played = []
    source.on_next_audio(played.append)
    start = time.monotonic()
    producer = asyncio.create_task(source.capture_frame(AudioFrame(pcm_tone(0.5, 48000), 48000)))
    await asyncio.sleep(0)  # the producer has to have queued something for there to be anything to wait for
    await source.wait_for_playout()
    elapsed = time.monotonic() - start
    await producer
    # Half a second of audio takes half a second to leave.
    assert 0.4 < elapsed < 0.8, elapsed
    assert abs(source.played_seconds - 0.5) < 0.03
    assert played and abs(played[0] - time.time()) < 2

    # Paused, nothing plays, and what is queued stays queued.
    source.pause()
    await source.capture_frame(AudioFrame(pcm_tone(0.1, 48000), 48000))
    before = source.played_seconds
    await asyncio.sleep(0.1)
    assert source.played_seconds == before and source.queued_seconds > 0.09
    source.resume()
    await source.wait_for_playout()
    assert source.played_seconds > before

    # Dropping is immediate, and says how much went.
    source.pause()
    await source.capture_frame(AudioFrame(pcm_tone(0.1, 48000), 48000))
    assert abs(source.clear_queue() - 0.1) < 0.02
    assert source.queued_seconds == 0
    drain_task.cancel()
    source.stop()


async def test_backpressure_keeps_producers_near_real_time():
    source = AudioSource(queue_seconds=0.1)

    async def drain():
        while True:
            await source.recv()

    drain_task = asyncio.create_task(drain())
    start = time.monotonic()
    for _ in range(10):
        await source.capture_frame(AudioFrame(pcm_tone(0.1, 48000), 48000))
    elapsed = time.monotonic() - start
    # A second of audio cannot be pushed much faster than a second of playing.
    assert elapsed > 0.7, elapsed
    drain_task.cancel()
    source.stop()


def test_vad_finds_speech_in_a_tone():
    vad = EnergyVAD(min_speech=0.05, min_silence=0.1)
    stream = vad.stream()
    stream.push_frame(AudioFrame(bytes(16000), 16000))
    stream.push_frame(AudioFrame(pcm_tone(0.5, 16000), 16000))
    stream.push_frame(AudioFrame(bytes(16000), 16000))
    events = []
    while not stream._events.empty():
        events.append(stream._events.get_nowait())
    kinds = [e.type for e in events if e.type != VADEventType.INFERENCE_DONE]
    assert kinds == [VADEventType.START_OF_SPEECH, VADEventType.END_OF_SPEECH]
    utterance = next(e for e in events if e.type == VADEventType.END_OF_SPEECH).audio
    assert 0.5 <= utterance.duration < 1.1, "the utterance holds the speech, and the padding before it"
    assert rms_level(utterance.data) > 0.05


@pytest.mark.skipif(not os.environ.get("SILERO_VAD_MODEL"), reason="SILERO_VAD_MODEL is not set")
def test_silero_ignores_a_tone_and_silence():
    """Silero is what tells speech from noise, which the energy detector cannot do. The speech side of this is in
    test_cascaded_real.py, where it has real speech to work on."""
    stream = SileroVAD().stream()
    stream.push_frame(AudioFrame(bytes(32000), 16000))
    stream.push_frame(AudioFrame(pcm_tone(1.0, 16000, frequency=1000), 16000))
    events = []
    while not stream._events.empty():
        events.append(stream._events.get_nowait())
    assert events, "no windows were analysed"
    assert all(e.type == VADEventType.INFERENCE_DONE for e in events)


def test_tokens():
    claims = decode_jwt(worker_token("key", "secret"))
    assert claims["iss"] == "key" and claims["video"] == {"agent": True} and claims["exp"] > time.time()
    full = decode_jwt(mint_token("key", "secret", identity="u", kind="agent", attributes={"a": "b"}, room_config={"agents": []}, video={"roomJoin": True}))
    assert full["sub"] == "u" and full["kind"] == "agent" and full["attributes"] == {"a": "b"} and full["roomConfig"] == {"agents": []}


def test_avatar_join_token_can_only_join_one_room():
    join = avatar_join("wss://x", "key", "secret", room="r", identity="avatar-1", on_behalf_of="agent-1")
    claims = decode_jwt(join.token)
    assert claims["kind"] == "agent" and claims["sub"] == "avatar-1"
    assert claims["attributes"][ATTR_PUBLISH_ON_BEHALF] == "agent-1"
    assert claims["video"]["room"] == "r" and claims["video"]["roomJoin"] is True
    assert "agent" not in claims["video"], "an avatar's token must not be able to register workers or read keys"
    assert "roomAdmin" not in claims["video"]

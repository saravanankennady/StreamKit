#!/usr/bin/env python3
"""Local speech-to-text behind the OpenAI transcription API shape, using faster-whisper (MIT) on the CPU.

This is for proving the framework with real speech and no vendor keys. It is not part of the shipped package.

    python stt_server.py --model <directory> --port 57991
"""

import argparse
import email.parser
import email.policy
import io
import json
import time
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
from faster_whisper import WhisperModel


def parse_multipart(content_type: str, body: bytes) -> dict[str, bytes]:
    message = email.parser.BytesParser(policy=email.policy.HTTP).parsebytes(
        b"Content-Type: " + content_type.encode() + b"\r\n\r\n" + body
    )
    fields = {}
    for part in message.iter_parts():
        name = part.get_param("name", header="content-disposition")
        if name:
            fields[name] = part.get_payload(decode=True)
    return fields


def read_wav(data: bytes) -> np.ndarray:
    with wave.open(io.BytesIO(data)) as w:
        if w.getframerate() != 16000 or w.getnchannels() != 1 or w.getsampwidth() != 2:
            raise ValueError("this server takes 16 kHz mono 16-bit WAV")
        pcm = w.readframes(w.getnframes())
    return np.frombuffer(pcm, dtype=np.int16).astype(np.float32) / 32768.0


class Handler(BaseHTTPRequestHandler):
    model: WhisperModel
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _json(self, status: int, payload: dict):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self._json(200, {"status": "ok"})

    def do_POST(self):
        if not self.path.endswith("/audio/transcriptions"):
            self._json(404, {"error": {"message": "not found"}})
            return
        length = int(self.headers.get("Content-Length", 0))
        fields = parse_multipart(self.headers.get("Content-Type", ""), self.rfile.read(length))
        try:
            audio = read_wav(fields["file"])
        except (KeyError, ValueError, wave.Error) as e:
            self._json(400, {"error": {"message": str(e)}})
            return
        started = time.time()
        language = (fields.get("language") or b"").decode() or "en"
        segments, _ = self.model.transcribe(audio, language=language, beam_size=1, vad_filter=False, condition_on_previous_text=False)
        text = " ".join(s.text.strip() for s in segments)
        self._json(200, {"text": text, "duration": len(audio) / 16000, "processing_seconds": round(time.time() - started, 3)})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--port", type=int, default=57991)
    parser.add_argument("--threads", type=int, default=4)
    args = parser.parse_args()
    Handler.model = WhisperModel(args.model, device="cpu", compute_type="int8", cpu_threads=args.threads)
    # Run once, so the first real request does not pay for loading.
    list(Handler.model.transcribe(np.zeros(16000, dtype=np.float32), language="en", beam_size=1)[0])
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"stt ready on {args.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()

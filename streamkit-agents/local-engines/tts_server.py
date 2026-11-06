#!/usr/bin/env python3
"""Local text-to-speech behind the OpenAI speech API shape, using Piper on the CPU.

This is for proving the framework with real speech and no vendor keys. It is not part of the shipped package. The piper-tts
package is GPL-3.0, which is why it runs as a separate process here and nothing in streamkit_agents imports it.

    python tts_server.py --voice <file.onnx> --port 57992

It answers with raw 16-bit PCM at the voice's own rate, and says which rate in the x-sample-rate header.
"""

import argparse
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from piper import PiperVoice


class Handler(BaseHTTPRequestHandler):
    voice: PiperVoice
    lock = threading.Lock()
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
        self._json(200, {"status": "ok", "sample_rate": self.voice.config.sample_rate})

    def do_POST(self):
        if not self.path.endswith("/audio/speech"):
            self._json(404, {"error": {"message": "not found"}})
            return
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        text = body.get("input") or ""
        if not text.strip():
            self._json(400, {"error": {"message": "no input"}})
            return
        self.send_response(200)
        self.send_header("Content-Type", "audio/pcm")
        self.send_header("x-sample-rate", str(self.voice.config.sample_rate))
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        with self.lock:
            for chunk in self.voice.synthesize(text):
                data = chunk.audio_int16_bytes
                self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
                self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--voice", required=True)
    parser.add_argument("--port", type=int, default=57992)
    args = parser.parse_args()
    Handler.voice = PiperVoice.load(args.voice)
    list(Handler.voice.synthesize("Ready."))
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"tts ready on {args.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()

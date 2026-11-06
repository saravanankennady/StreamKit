#!/usr/bin/env python3
"""Generates the protobuf bindings from the vendored wire protocol.

    python scripts/gen_proto.py

Needs grpcio-tools. The output is committed, so installing the package needs no compiler.
"""

import pathlib
import re
import shutil
import subprocess
import sys

HERE = pathlib.Path(__file__).resolve().parent
OUT = HERE.parent / "src" / "streamkit_agents" / "_proto"
VENDOR = HERE.parents[2] / "proto" / "vendor" / "livekit"
FILES = [
    "livekit_agent.proto",
    "livekit_agent_dispatch.proto",
    "livekit_egress.proto",
    "livekit_ingress.proto",
    "livekit_metrics.proto",
    "livekit_models.proto",
    "livekit_room.proto",
    "livekit_rtc.proto",
    "logger/options.proto",
]


def main() -> None:
    (OUT / "logger").mkdir(parents=True, exist_ok=True)
    subprocess.run(
        [
            sys.executable,
            "-m",
            "grpc_tools.protoc",
            f"-I{VENDOR}",
            f"--python_out={OUT}",
            *FILES,
        ],
        check=True,
    )
    # protoc writes absolute imports, as if the output directory were on sys.path. Make them relative to this package.
    for path in OUT.rglob("*_pb2.py"):
        text = path.read_text()
        text = re.sub(r"^import (livekit_\w+_pb2)", r"from . import \1", text, flags=re.M)
        prefix = "from .logger" if path.parent == OUT else "from ..logger"
        text = re.sub(r"^from logger import", f"{prefix} import", text, flags=re.M)
        path.write_text(text)
    (OUT / "__init__.py").write_text('"""Generated from proto/vendor/livekit by scripts/gen_proto.py. Do not edit."""\n')
    (OUT / "logger" / "__init__.py").write_text("")
    shutil.rmtree(OUT / "__pycache__", ignore_errors=True)


if __name__ == "__main__":
    main()

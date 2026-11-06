"""Generates this SDK's wire types from the vendored .proto files. Needs grpcio-tools. Run from anywhere:

    python sdks/server-python/scripts/generate_proto.py

The generated code is changed in two ways. Its imports become relative, so that it lives inside this package. And it
registers in this package's own descriptor pool rather than the process-wide one, so that it can be imported beside
another package built from the same .proto files, such as the official room client. Protobuf refuses two definitions of
one name in the same pool.
"""

import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
SOURCE = ROOT / "proto" / "vendor" / "livekit"
OUT = ROOT / "sdks" / "server-python" / "elnova_streamkit" / "proto"
FILES = [
    "logger/options.proto",
    "livekit_metrics.proto",
    "livekit_models.proto",
    "livekit_egress.proto",
    "livekit_ingress.proto",
    "livekit_agent.proto",
    "livekit_agent_dispatch.proto",
    "livekit_room.proto",
    "livekit_webhook.proto",
]

POOL_MODULE = '''"""This package's own descriptor pool. See scripts/generate_proto.py for why it has one."""

from google.protobuf import descriptor_pb2, descriptor_pool, timestamp_pb2

POOL = descriptor_pool.DescriptorPool()
# The vendored files import these, and a new pool starts empty.
for _module in (descriptor_pb2, timestamp_pb2):
    POOL.AddSerializedFile(_module.DESCRIPTOR.serialized_pb)
'''


def rewrite(path: Path) -> None:
    text = path.read_text()
    # Sibling modules, whether flat or under logger/.
    text = re.sub(r"^import (livekit_\w+_pb2) as", r"from . import \1 as", text, flags=re.M)
    text = re.sub(r"^from logger import options_pb2 as", "from .logger import options_pb2 as", text, flags=re.M)
    depth = ".." if path.parent.name == "logger" else "."
    text = text.replace("_descriptor_pool.Default().AddSerializedFile", "_POOL.AddSerializedFile")
    text = text.replace(
        "from google.protobuf import descriptor_pool as _descriptor_pool\n",
        f"from {depth}_pool import POOL as _POOL\n",
    )
    path.write_text(text)


def main() -> int:
    if OUT.exists():
        shutil.rmtree(OUT)
    (OUT / "logger").mkdir(parents=True)
    subprocess.run(
        [sys.executable, "-m", "grpc_tools.protoc", f"-I{SOURCE}", f"--python_out={OUT}", f"--pyi_out={OUT}", *FILES],
        check=True,
    )
    for path in OUT.rglob("*_pb2.py*"):
        rewrite(path)
    (OUT / "_pool.py").write_text(POOL_MODULE)
    (OUT / "__init__.py").write_text('"""Wire types, generated from the vendored .proto files. Do not edit."""\n')
    (OUT / "logger" / "__init__.py").write_text("")
    return 0


if __name__ == "__main__":
    sys.exit(main())

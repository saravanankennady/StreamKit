"""Signs access tokens, which are HS256 JWTs. A worker needs one to register."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time
from typing import Any


def _b64(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def mint_token(
    api_key: str,
    api_secret: str,
    *,
    identity: str = "",
    video: dict[str, Any] | None = None,
    name: str = "",
    kind: str = "",
    metadata: str = "",
    attributes: dict[str, str] | None = None,
    room_config: dict[str, Any] | None = None,
    ttl: float = 3600,
) -> str:
    now = int(time.time())
    claims: dict[str, Any] = {"iss": api_key, "sub": identity, "nbf": now, "iat": now, "exp": int(now + ttl), "video": video or {}}
    if name:
        claims["name"] = name
    if kind:
        claims["kind"] = kind
    if metadata:
        claims["metadata"] = metadata
    if attributes:
        claims["attributes"] = attributes
    if room_config:
        claims["roomConfig"] = room_config
    header = _b64(json.dumps({"alg": "HS256", "typ": "JWT"}, separators=(",", ":")).encode())
    payload = _b64(json.dumps(claims, separators=(",", ":")).encode())
    signature = hmac.new(api_secret.encode(), f"{header}.{payload}".encode(), hashlib.sha256).digest()
    return f"{header}.{payload}.{_b64(signature)}"


def worker_token(api_key: str, api_secret: str, *, ttl: float = 3600) -> str:
    """A token that can do one thing: register a worker."""
    return mint_token(api_key, api_secret, video={"agent": True}, ttl=ttl)

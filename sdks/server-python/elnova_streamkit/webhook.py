"""Checks that webhooks really came from a server holding your key."""

import base64
import datetime
import hashlib
import hmac
from typing import Union

from google.protobuf.json_format import Parse

from .access_token import AccessToken, TokenVerifier
from .proto.livekit_webhook_pb2 import WebhookEvent

# How long a signature made by sign_webhook lasts: the same as a server's.
_SIGNATURE_TTL = datetime.timedelta(minutes=5)


class WebhookError(Exception):
    """A webhook failed its check."""


def _body_hash(body: bytes) -> str:
    return base64.b64encode(hashlib.sha256(body).digest()).decode()


class WebhookReceiver:
    def __init__(self, token_verifier: TokenVerifier) -> None:
        self._verifier = token_verifier

    def receive(self, body: Union[str, bytes], auth_token: str) -> WebhookEvent:
        """Checks a webhook and returns its event. It raises WebhookError if the body does not match, and
        jwt.InvalidTokenError if the signature is not good.

        Pass the body as it arrived. Parsing it first and serializing it again changes the bytes, and the signature
        covers the bytes. auth_token is the Authorization header, which is a token with no Bearer prefix. Fields this SDK
        does not know are ignored."""
        if not auth_token:
            raise WebhookError("the webhook has no Authorization header")
        raw = body.encode() if isinstance(body, str) else body
        claims = self._verifier.verify(auth_token)
        if not hmac.compare_digest(claims.sha256, _body_hash(raw)):
            raise WebhookError("the webhook body does not match its signature")
        return Parse(raw, WebhookEvent(), ignore_unknown_fields=True)


def sign_webhook(body: Union[str, bytes], api_key: str, api_secret: str) -> str:
    """Returns the Authorization header value for a webhook with this body. Servers do this themselves: it is for testing
    a receiver."""
    raw = body.encode() if isinstance(body, str) else body
    return AccessToken(api_key, api_secret).with_sha256(_body_hash(raw)).with_ttl(_SIGNATURE_TTL).to_jwt()

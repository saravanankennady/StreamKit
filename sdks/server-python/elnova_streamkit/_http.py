"""What every client shares: where the server is, how to sign for it, and how to read a failure."""

import datetime
import json
from typing import Any, Optional, TypeVar
from urllib.parse import quote

import aiohttp
from google.protobuf.message import Message

from .access_token import AccessToken, VideoGrants

_USER_AGENT = "elnova-streamkit-server-python/0.1.0"
# How long the token signed for each call lasts.
_CALL_TOKEN_TTL = datetime.timedelta(minutes=10)

M = TypeVar("M", bound=Message)


class ServerError(Exception):
    """A failure the server reported."""

    def __init__(
        self,
        code: str,
        message: str,
        status: int,
        metadata: Optional[dict[str, str]] = None,
        fields: Optional[dict[str, str]] = None,
    ) -> None:
        super().__init__(f"{code}: {message} (HTTP {status})")
        self.code = code
        self.message = message
        self.status = status
        self.metadata = metadata or {}
        self.fields = fields or {}


class NotSupportedError(Exception):
    """Something this server does not do."""


_CODE_FOR_STATUS = {
    400: "invalid_argument",
    401: "unauthenticated",
    403: "permission_denied",
    404: "not_found",
    429: "resource_exhausted",
    501: "unimplemented",
    502: "unavailable",
    503: "unavailable",
    504: "unavailable",
}


async def _server_error(res: aiohttp.ClientResponse) -> ServerError:
    """Twirp errors are {code,msg}, REST errors are {error:{code,message}}, and authentication failures and proxies answer
    in plain text."""
    text = await res.text()
    try:
        body = json.loads(text)
    except ValueError:
        body = None
    if isinstance(body, dict):
        error = body.get("error")
        if isinstance(error, dict) and error.get("code"):
            return ServerError(error["code"], error.get("message", ""), res.status, fields=error.get("fields"))
        if body.get("code"):
            return ServerError(body["code"], body.get("msg", ""), res.status, metadata=body.get("meta"))
    return ServerError(_CODE_FOR_STATUS.get(res.status, "unknown"), text.strip(), res.status)


def _is_json(res: aiohttp.ClientResponse) -> bool:
    return res.headers.get("Content-Type", "").startswith("application/json")


class Transport:
    def __init__(
        self,
        session: aiohttp.ClientSession,
        url: str,
        api_key: Optional[str],
        api_secret: Optional[str],
        token: Optional[str],
        timeout: float,
    ) -> None:
        url = url.rstrip("/")
        if url.startswith("ws"):
            url = "http" + url[2:]
        self.url = url
        self._session = session
        self._api_key = api_key
        self._api_secret = api_secret
        self._token = token
        self._timeout = aiohttp.ClientTimeout(total=timeout)

    def _authorization(self, grant: VideoGrants) -> str:
        if self._token:
            return f"Bearer {self._token}"
        token = AccessToken(self._api_key, self._api_secret).with_grants(grant).with_ttl(_CALL_TOKEN_TTL).to_jwt()
        return f"Bearer {token}"

    async def twirp(self, service: str, method: str, grant: VideoGrants, request: Message, response: M) -> M:
        """Calls one Twirp method in protobuf. A route that does not exist means the server does not have this service."""
        headers = {
            "Authorization": self._authorization(grant),
            "Content-Type": "application/protobuf",
            "Accept": "application/protobuf",
            "User-Agent": _USER_AGENT,
        }
        async with self._session.post(
            f"{self.url}/twirp/livekit.{service}/{method}",
            data=request.SerializeToString(),
            headers=headers,
            timeout=self._timeout,
        ) as res:
            if res.status != 200:
                if res.status == 404 and not _is_json(res):
                    raise NotSupportedError(f"{method}: this server has no {service} service")
                raise await _server_error(res)
            response.ParseFromString(await res.read())
            return response

    async def rest(self, verb: str, path: str, grant: VideoGrants, body: Any = None) -> Any:
        """Calls the management API, and returns the decoded response if there is one."""
        async with self._rest_request(verb, path, grant, body) as res:
            await self._check(res)
            if res.status == 204:
                return None
            return await res.json()

    async def rest_download(self, path: str, grant: VideoGrants, into: Any) -> int:
        """Writes a file from the management API to an open binary file, and returns how many bytes it wrote. Fetching a
        file can take far longer than an API call should, so the time limit here is on silence, not on the whole."""
        written = 0
        timeout = aiohttp.ClientTimeout(total=None, sock_read=self._timeout.total)
        async with self._rest_request("GET", path, grant, None, timeout) as res:
            await self._check(res)
            async for chunk in res.content.iter_chunked(1 << 16):
                into.write(chunk)
                written += len(chunk)
        return written

    def _rest_request(self, verb: str, path: str, grant: VideoGrants, body: Any, timeout: Optional[aiohttp.ClientTimeout] = None) -> Any:
        headers = {
            "Authorization": self._authorization(grant),
            "Content-Type": "application/json",
            "User-Agent": _USER_AGENT,
        }
        data = None if body is None else json.dumps(body)
        return self._session.request(verb, f"{self.url}/api/v1{path}", data=data, headers=headers, timeout=timeout or self._timeout)

    async def _check(self, res: aiohttp.ClientResponse) -> None:
        if res.status < 300:
            return
        if res.status == 404 and not _is_json(res):
            raise NotSupportedError(
                f"{self.url} has no management API at /api/v1. "
                "It needs an Elnova StreamKit server with a control plane (control_plane.database_url)"
            )
        raise await _server_error(res)


def page_query(limit: Optional[int], cursor: Optional[str]) -> str:
    parts = []
    if limit:
        parts.append(f"limit={int(limit)}")
    if cursor:
        parts.append(f"cursor={quote(cursor, safe='')}")
    return "?" + "&".join(parts) if parts else ""

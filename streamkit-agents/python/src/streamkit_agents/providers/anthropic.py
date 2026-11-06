"""The Anthropic Messages API, streaming. Written from https://platform.claude.com/docs/en/build-with-claude/streaming."""

from __future__ import annotations

import json
from typing import Any, AsyncIterator

import httpx

from ..llm.base import LLM, APIError, ChatChunk, RetryPolicy, Usage
from ..llm.chat import ChatContext, FunctionCall
from ._http import make_client, raise_for_status, sse_events, transport_error

API_VERSION = "2026-10-21"
DEFAULT_MAX_TOKENS = 1024


class AnthropicLLM(LLM):
    provider = "anthropic"

    def __init__(self, model: str, *, api_key: str, base_url: str = "https://api.anthropic.com", api_version: str = API_VERSION, retry: RetryPolicy | None = None):
        super().__init__(model, retry=retry)
        self._url = base_url.rstrip("/") + "/v1/messages"
        self._client = make_client(self.retry, {"x-api-key": api_key, "anthropic-version": api_version})

    async def _stream(self, ctx: ChatContext, tools, tool_choice, temperature, max_tokens) -> AsyncIterator[ChatChunk]:
        system, messages = ctx.to_anthropic()
        body: dict[str, Any] = {
            "model": self.model,
            "messages": messages,
            "max_tokens": max_tokens or DEFAULT_MAX_TOKENS,
            "stream": True,
        }
        if system:
            body["system"] = system
        if temperature is not None:
            body["temperature"] = temperature
        if tools:
            body["tools"] = [t.to_anthropic() for t in tools]
            if tool_choice == "required":
                body["tool_choice"] = {"type": "any"}
            elif tool_choice == "none":
                body["tool_choice"] = {"type": "none"}
            elif tool_choice and tool_choice != "auto":
                body["tool_choice"] = {"type": "tool", "name": tool_choice}

        # Tool input arrives as fragments of JSON, per content block.
        blocks: dict[int, dict[str, str]] = {}
        calls: list[FunctionCall] = []
        input_tokens = cached = output_tokens = 0
        try:
            async with self._client.stream("POST", self._url, json=body) as response:
                await raise_for_status(self.provider, response)
                async for event, data in sse_events(response):
                    if event == "ping":
                        continue
                    payload = json.loads(data)
                    kind = payload.get("type") or event
                    if kind == "message_start":
                        usage = payload["message"].get("usage") or {}
                        input_tokens = usage.get("input_tokens", 0)
                        cached = usage.get("cache_read_input_tokens", 0)
                        output_tokens = usage.get("output_tokens", 0)
                    elif kind == "content_block_start":
                        block = payload["content_block"]
                        if block.get("type") == "tool_use":
                            blocks[payload["index"]] = {"id": block["id"], "name": block["name"], "json": ""}
                    elif kind == "content_block_delta":
                        delta = payload["delta"]
                        if delta.get("type") == "text_delta":
                            yield ChatChunk(text=delta["text"])
                        elif delta.get("type") == "input_json_delta":
                            blocks[payload["index"]]["json"] += delta.get("partial_json", "")
                    elif kind == "content_block_stop":
                        block = blocks.pop(payload.get("index"), None)
                        if block is not None:
                            calls.append(FunctionCall(call_id=block["id"], name=block["name"], arguments=block["json"] or "{}"))
                    elif kind == "message_delta":
                        # These counts are cumulative, so the last one is the total.
                        usage = payload.get("usage") or {}
                        output_tokens = usage.get("output_tokens", output_tokens)
                        if (payload.get("delta") or {}).get("stop_reason"):
                            yield ChatChunk(finish_reason=payload["delta"]["stop_reason"])
                    elif kind == "message_stop":
                        break
                    elif kind == "error":
                        error = payload.get("error") or {}
                        retryable = error.get("type") in ("overloaded_error", "rate_limit_error", "api_error", "timeout_error")
                        raise APIError(f"anthropic reported {error.get('type')}: {error.get('message', '')[:300]}", retryable=retryable)
        except httpx.HTTPError as e:
            raise transport_error(self.provider, e) from e
        if calls:
            yield ChatChunk(tool_calls=calls)
        yield ChatChunk(usage=Usage(prompt_tokens=input_tokens, cached_tokens=cached, completion_tokens=output_tokens))

    async def aclose(self) -> None:
        await self._client.aclose()

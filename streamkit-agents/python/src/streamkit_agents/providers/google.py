"""Gemini through the Generative Language REST API. Written from https://aistudio.google.com/api-reference/generate-content."""

from __future__ import annotations

import json
from typing import Any, AsyncIterator

import httpx

from ..llm.base import LLM, APIError, ChatChunk, RetryPolicy, Usage
from ..llm.chat import ChatContext, FunctionCall
from ._http import make_client, raise_for_status, sse_events, transport_error


class GoogleLLM(LLM):
    provider = "google"

    def __init__(self, model: str, *, api_key: str, base_url: str = "https://generativelanguage.googleapis.com", retry: RetryPolicy | None = None):
        super().__init__(model, retry=retry)
        self._url = f"{base_url.rstrip('/')}/v1beta/models/{model}:streamGenerateContent"
        # The key goes in a header, not the query string, so it cannot end up in a logged URL.
        self._client = make_client(self.retry, {"x-goog-api-key": api_key})

    async def _stream(self, ctx: ChatContext, tools, tool_choice, temperature, max_tokens) -> AsyncIterator[ChatChunk]:
        system, contents = ctx.to_google()
        body: dict[str, Any] = {"contents": contents}
        if system:
            body["systemInstruction"] = {"parts": [{"text": system}]}
        generation: dict[str, Any] = {}
        if temperature is not None:
            generation["temperature"] = temperature
        if max_tokens is not None:
            generation["maxOutputTokens"] = max_tokens
        if generation:
            body["generationConfig"] = generation
        if tools:
            body["tools"] = [{"functionDeclarations": [t.to_google() for t in tools]}]
            mode = {"required": "ANY", "none": "NONE", "auto": "AUTO"}.get(tool_choice or "auto")
            if mode:
                body["toolConfig"] = {"functionCallingConfig": {"mode": mode}}
            else:
                body["toolConfig"] = {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": [tool_choice]}}

        usage: Usage | None = None
        calls: list[FunctionCall] = []
        try:
            async with self._client.stream("POST", self._url, params={"alt": "sse"}, json=body) as response:
                await raise_for_status(self.provider, response)
                async for _, data in sse_events(response):
                    chunk = json.loads(data)
                    if "error" in chunk:
                        raise APIError(f"google reported an error: {str(chunk['error'])[:300]}", retryable=False)
                    meta = chunk.get("usageMetadata")
                    if meta:
                        usage = Usage(
                            prompt_tokens=meta.get("promptTokenCount", 0),
                            completion_tokens=meta.get("candidatesTokenCount", 0),
                            cached_tokens=meta.get("cachedContentTokenCount", 0),
                            raw=meta,
                        )
                    for candidate in chunk.get("candidates") or []:
                        for part in (candidate.get("content") or {}).get("parts") or []:
                            if part.get("text") and not part.get("thought"):
                                yield ChatChunk(text=part["text"])
                            call = part.get("functionCall")
                            if call:
                                calls.append(FunctionCall(
                                    call_id=call.get("id") or f"call_{call['name']}_{len(calls)}",
                                    name=call["name"],
                                    arguments=json.dumps(call.get("args") or {}),
                                ))
                        if candidate.get("finishReason"):
                            yield ChatChunk(finish_reason=candidate["finishReason"])
        except httpx.HTTPError as e:
            raise transport_error(self.provider, e) from e
        if calls:
            yield ChatChunk(tool_calls=calls)
        if usage is not None:
            yield ChatChunk(usage=usage)

    async def aclose(self) -> None:
        await self._client.aclose()

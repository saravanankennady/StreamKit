"""Conversation history, and how it turns into each provider protocol's message format."""

from __future__ import annotations

import json
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Iterator, Literal, Union

Role = Literal["system", "user", "assistant"]


def _new_id() -> str:
    return uuid.uuid4().hex[:16]


@dataclass
class ChatMessage:
    role: Role
    content: str
    id: str = field(default_factory=_new_id)
    # Only part of this message was spoken, because the user cut in. It holds what was spoken.
    interrupted: bool = False
    created_at: float = field(default_factory=time.time)


@dataclass
class FunctionCall:
    """A complete tool call. Adapters assemble partial deltas, so nothing above them sees fragments."""

    call_id: str
    name: str
    arguments: str
    id: str = field(default_factory=_new_id)


@dataclass
class FunctionCallOutput:
    call_id: str
    name: str
    output: str
    is_error: bool = False
    id: str = field(default_factory=_new_id)


@dataclass
class AgentHandoff:
    """A record that the conversation moved between agents. Providers do not see it."""

    old_agent: str
    new_agent: str
    id: str = field(default_factory=_new_id)


ChatItem = Union[ChatMessage, FunctionCall, FunctionCallOutput, AgentHandoff]


class ChatContext:
    def __init__(self, items: list[ChatItem] | None = None):
        self.items: list[ChatItem] = list(items or [])

    def __iter__(self) -> Iterator[ChatItem]:
        return iter(self.items)

    def __len__(self) -> int:
        return len(self.items)

    def add_message(self, role: Role, content: str, *, interrupted: bool = False) -> ChatMessage:
        message = ChatMessage(role=role, content=content, interrupted=interrupted)
        self.items.append(message)
        return message

    def add(self, item: ChatItem) -> ChatItem:
        self.items.append(item)
        return item

    def copy(self) -> "ChatContext":
        return ChatContext(self.items)

    def messages(self) -> list[ChatMessage]:
        return [i for i in self.items if isinstance(i, ChatMessage)]

    def set_instructions(self, instructions: str) -> None:
        """Keeps one system message at the start, holding the agent's instructions."""
        for i, item in enumerate(self.items):
            if isinstance(item, ChatMessage) and item.role == "system":
                self.items[i] = ChatMessage(role="system", content=instructions, id=item.id)
                return
        self.items.insert(0, ChatMessage(role="system", content=instructions))

    def truncate(self, max_items: int) -> None:
        """Keeps the newest items and the system message. A tool result never stays without its call."""
        system = [i for i in self.items if isinstance(i, ChatMessage) and i.role == "system"]
        rest = [i for i in self.items if not (isinstance(i, ChatMessage) and i.role == "system")]
        rest = rest[-max_items:] if max_items > 0 else []
        while rest and isinstance(rest[0], FunctionCallOutput):
            rest.pop(0)
        self.items = system + rest

    def to_openai(self) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        for item in self.items:
            if isinstance(item, ChatMessage):
                out.append({"role": item.role, "content": item.content})
            elif isinstance(item, FunctionCall):
                call = {"id": item.call_id, "type": "function", "function": {"name": item.name, "arguments": item.arguments}}
                # Parallel calls belong to one assistant message.
                if out and out[-1]["role"] == "assistant" and "tool_calls" in out[-1]:
                    out[-1]["tool_calls"].append(call)
                elif out and out[-1]["role"] == "assistant" and out[-1].get("content") is not None and "tool_calls" not in out[-1]:
                    out[-1]["tool_calls"] = [call]
                else:
                    out.append({"role": "assistant", "content": None, "tool_calls": [call]})
            elif isinstance(item, FunctionCallOutput):
                out.append({"role": "tool", "tool_call_id": item.call_id, "content": item.output})
        return out

    def to_anthropic(self) -> tuple[str, list[dict[str, Any]]]:
        """Returns the system prompt and messages. Roles must alternate and start with the user, so neighbours merge."""
        system: list[str] = []
        out: list[dict[str, Any]] = []

        def add(role: str, block: dict[str, Any]) -> None:
            if out and out[-1]["role"] == role:
                out[-1]["content"].append(block)
            else:
                out.append({"role": role, "content": [block]})

        for item in self.items:
            if isinstance(item, ChatMessage):
                if item.role == "system":
                    system.append(item.content)
                elif item.content:
                    add(item.role, {"type": "text", "text": item.content})
            elif isinstance(item, FunctionCall):
                add("assistant", {"type": "tool_use", "id": item.call_id, "name": item.name, "input": _parse_args(item.arguments)})
            elif isinstance(item, FunctionCallOutput):
                block: dict[str, Any] = {"type": "tool_result", "tool_use_id": item.call_id, "content": item.output}
                if item.is_error:
                    block["is_error"] = True
                add("user", block)
        if out and out[0]["role"] != "user":
            out.insert(0, {"role": "user", "content": [{"type": "text", "text": "(the conversation continues)"}]})
        return "\n\n".join(system), out

    def to_google(self) -> tuple[str, list[dict[str, Any]]]:
        """Returns the system instruction and contents."""
        system: list[str] = []
        out: list[dict[str, Any]] = []

        def add(role: str, part: dict[str, Any]) -> None:
            if out and out[-1]["role"] == role:
                out[-1]["parts"].append(part)
            else:
                out.append({"role": role, "parts": [part]})

        for item in self.items:
            if isinstance(item, ChatMessage):
                if item.role == "system":
                    system.append(item.content)
                elif item.content:
                    add("model" if item.role == "assistant" else "user", {"text": item.content})
            elif isinstance(item, FunctionCall):
                add("model", {"functionCall": {"id": item.call_id, "name": item.name, "args": _parse_args(item.arguments)}})
            elif isinstance(item, FunctionCallOutput):
                key = "error" if item.is_error else "output"
                add("user", {"functionResponse": {"id": item.call_id, "name": item.name, "response": {key: item.output}}})
        return "\n\n".join(system), out


def _parse_args(arguments: str) -> dict[str, Any]:
    try:
        value = json.loads(arguments or "{}")
    except json.JSONDecodeError:
        return {}
    return value if isinstance(value, dict) else {}

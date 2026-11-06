"""Tools an LLM can call: plain functions, with a JSON schema worked out from their signature."""

from __future__ import annotations

import enum
import inspect
import json
import re
import types
import typing
from dataclasses import dataclass
from typing import Any, Awaitable, Callable, get_args, get_origin, get_type_hints


class ToolError(Exception):
    """Raised by a tool to tell the LLM it failed, in words the LLM can act on."""


class StopResponse(Exception):
    """Raised by a tool or hook to end the turn with no spoken reply."""


@dataclass
class RunContext:
    """What a tool can see of the conversation it runs in."""

    session: Any
    speech_handle: Any = None
    function_call: Any = None

    @property
    def userdata(self) -> Any:
        return self.session.userdata


@dataclass
class FunctionTool:
    name: str
    description: str
    parameters: dict[str, Any]
    fn: Callable[..., Awaitable[Any]]
    context_param: str | None = None

    async def call(self, ctx: RunContext, arguments: str) -> Any:
        try:
            kwargs = json.loads(arguments or "{}")
        except json.JSONDecodeError as e:
            raise ToolError(f"the arguments were not valid JSON: {e}") from e
        if not isinstance(kwargs, dict):
            raise ToolError("the arguments must be a JSON object")
        allowed = set(self.parameters.get("properties", {}))
        unknown = set(kwargs) - allowed
        if unknown:
            raise ToolError(f"unknown arguments: {sorted(unknown)}")
        missing = set(self.parameters.get("required", [])) - set(kwargs)
        if missing:
            raise ToolError(f"missing arguments: {sorted(missing)}")
        if self.context_param:
            kwargs[self.context_param] = ctx
        result = self.fn(**kwargs)
        return await result if inspect.isawaitable(result) else result

    def to_openai(self) -> dict[str, Any]:
        return {"type": "function", "function": {"name": self.name, "description": self.description, "parameters": self.parameters}}

    def to_anthropic(self) -> dict[str, Any]:
        return {"name": self.name, "description": self.description, "input_schema": self.parameters}

    def to_google(self) -> dict[str, Any]:
        return {"name": self.name, "description": self.description, "parametersJsonSchema": self.parameters}

    def to_openai_realtime(self) -> dict[str, Any]:
        return {"type": "function", "name": self.name, "description": self.description, "parameters": self.parameters}


def _schema_for(hint: Any) -> dict[str, Any]:
    origin = get_origin(hint)
    if hint is str:
        return {"type": "string"}
    if hint is bool:
        return {"type": "boolean"}
    if hint is int:
        return {"type": "integer"}
    if hint is float:
        return {"type": "number"}
    if hint is dict or origin is dict:
        return {"type": "object"}
    if hint is list or origin is list:
        args = get_args(hint)
        return {"type": "array", "items": _schema_for(args[0]) if args else {}}
    if origin is typing.Literal:
        values = list(get_args(hint))
        return {**_schema_for(type(values[0])), "enum": values}
    if inspect.isclass(hint) and issubclass(hint, enum.Enum):
        values = [m.value for m in hint]
        return {**_schema_for(type(values[0])), "enum": values}
    if origin in (typing.Union, types.UnionType):
        members = [a for a in get_args(hint) if a is not type(None)]
        if len(members) == 1:
            return _schema_for(members[0])
        return {"anyOf": [_schema_for(m) for m in members]}
    raise TypeError(f"a tool parameter cannot have the type {hint!r}")


_DOC_ARG = re.compile(r"^\s*(\w+)\s*(?:\([^)]*\))?:\s*(.+)$")


def _parse_doc(doc: str) -> tuple[str, dict[str, str]]:
    """Reads a Google-style docstring into a description and per-argument text."""
    description: list[str] = []
    args: dict[str, str] = {}
    in_args = False
    for line in inspect.cleandoc(doc or "").splitlines():
        stripped = line.strip()
        if stripped.lower() in ("args:", "arguments:", "parameters:"):
            in_args = True
            continue
        if stripped.lower() in ("returns:", "raises:"):
            in_args = False
            continue
        if in_args:
            match = _DOC_ARG.match(line)
            if match:
                args[match.group(1)] = match.group(2).strip()
        elif stripped:
            description.append(stripped)
    return " ".join(description), args


def function_tool(fn: Callable[..., Any] | None = None, *, name: str | None = None, description: str | None = None) -> Any:
    """Turns a function into a tool. A parameter annotated RunContext is filled in, and is not shown to the LLM.

        @function_tool
        async def lookup_weather(ctx: RunContext, city: str) -> str:
            '''Looks up the weather.

            Args:
                city: The city to look up.
            '''
    """

    def wrap(f: Callable[..., Any]) -> FunctionTool:
        doc_text, doc_args = _parse_doc(f.__doc__ or "")
        hints = get_type_hints(f)
        properties: dict[str, Any] = {}
        required: list[str] = []
        context_param = None
        for param in inspect.signature(f).parameters.values():
            if param.name == "self":
                continue
            hint = hints.get(param.name, str)
            if hint is RunContext:
                context_param = param.name
                continue
            schema = _schema_for(hint)
            if param.name in doc_args:
                schema["description"] = doc_args[param.name]
            properties[param.name] = schema
            if param.default is inspect.Parameter.empty:
                required.append(param.name)
        return FunctionTool(
            name=name or f.__name__,
            description=description if description is not None else doc_text,
            parameters={"type": "object", "properties": properties, "required": required},
            fn=f,
            context_param=context_param,
        )

    return wrap(fn) if fn is not None else wrap

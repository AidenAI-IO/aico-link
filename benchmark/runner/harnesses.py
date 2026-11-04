"""Agent harness kinds that experiments can compare.

Each kind declares its configuration rules, how to check its installed runtime,
and how to build its adapter, so experiment orchestration never branches on the
agent kind. Aiden is the native harness under test; every other kind is an
external harness that reaches the device only through the tool gateway.
"""
from __future__ import annotations

import dataclasses as dc
import os
from pathlib import Path
import re
import shutil
import subprocess
from typing import Any, Callable
import urllib.parse

from runner.harness import (ClaudeCodeAdapter, CodexAdapter, ExternalHarness, OpenClawAdapter, PiAdapter,
                            pi_environment)


SAFE_ID = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}\Z")
THINKING_LEVELS = {"off", "minimal", "low", "medium", "high", "xhigh", "max"}
CUSTOM_ENDPOINT_FIELDS = {"base_url", "api", "api_key_env", "context_window", "max_tokens"}
CUSTOM_ENDPOINT_APIS = ("openai-completions", "openai-responses", "anthropic-messages", "google-generative-ai")
DEFAULT_API_KEY_ENV = "AIDEN_BENCHMARK_PI_API_KEY"
OPENCLAW_MIN_NODE = (24, 22, 1)


@dc.dataclass(frozen=True)
class HarnessKind:
    kind: str
    label: str
    external: bool = True
    # Supported custom endpoint protocols; the first is the default.
    apis: tuple[str, ...] = CUSTOM_ENDPOINT_APIS
    requires_base_url: bool = True
    # Return identity fields (executable, version, ...) to record on the agent.
    runtime: Callable[[dict, Path], dict] | None = None
    build: Callable[[dict], ExternalHarness] | None = None


def _executable(variable: str, default: str, label: str) -> str:
    executable = shutil.which(os.environ.get(variable, default))
    if not executable:
        raise ValueError(f"{label} executable not found; install it or set {variable}")
    return executable


def _version(command: list[str], **kwargs) -> str:
    return subprocess.check_output(command, text=True, timeout=20, **kwargs).strip()


def _endpoint_fields(agent: dict) -> dict[str, Any]:
    return {key: agent[key] for key in CUSTOM_ENDPOINT_FIELDS if key in agent}


def pi_runtime(agent: dict, scratch: Path) -> dict:
    executable = _executable("AIDEN_BENCHMARK_PI_EXECUTABLE", "pi", "pi")
    env = pi_environment(executable, agent["provider"], scratch / "pi-probe")
    return {"executable": executable, "version": _version([executable, "--version"], env=env)}


def openclaw_runtime() -> tuple[str, str, str]:
    """Resolve the installed CLI and a compatible Node without changing host PATH."""
    executable = shutil.which(os.environ.get("AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE", "openclaw"))
    node = shutil.which(os.environ.get("AIDEN_BENCHMARK_OPENCLAW_NODE", "node"))
    if not executable or not node:
        raise ValueError("OpenClaw CLI and Node 24.22.1+ are required; set AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE and AIDEN_BENCHMARK_OPENCLAW_NODE")
    version = _version([node, "--version"])
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", version)
    version_tuple = tuple(map(int, match.groups())) if match else (0, 0, 0)
    if not ((version_tuple[0] == 24 and version_tuple >= OPENCLAW_MIN_NODE)
            or version_tuple >= (26, 12, 1)):
        raise ValueError(f"OpenClaw requires Node 24.22.1+ or 26.12.1+; found {version}")
    return executable, node, _version([node, executable, "--version"])


def _openclaw_runtime(agent: dict, scratch: Path) -> dict:
    executable, node, version = openclaw_runtime()
    return {"executable": executable, "node": node, "version": version,
            "node_version": _version([node, "--version"])}


def _claude_code_runtime(agent: dict, scratch: Path) -> dict:
    executable = _executable("AIDEN_BENCHMARK_CLAUDE_CODE_EXECUTABLE", "claude", "Claude Code")
    return {"executable": executable, "version": _version([executable, "--version"])}


def _codex_runtime(agent: dict, scratch: Path) -> dict:
    executable = _executable("AIDEN_BENCHMARK_CODEX_EXECUTABLE", "codex", "Codex")
    return {"executable": executable, "version": _version([executable, "--version"])}


HARNESSES = {kind.kind: kind for kind in (
    HarnessKind("aiden", "Aiden", external=False),
    HarnessKind("pi", "pi", requires_base_url=False, runtime=pi_runtime,
                build=lambda agent: PiAdapter(executable=agent["executable"], provider=agent["provider"],
                                              model=agent["model"], thinking=agent["thinking"],
                                              **_endpoint_fields(agent))),
    HarnessKind("openclaw", "OpenClaw", runtime=_openclaw_runtime,
                build=lambda agent: OpenClawAdapter(executable=agent["executable"], node=agent["node"],
                                                    provider=agent["provider"], model=agent["model"],
                                                    thinking=agent["thinking"], **_endpoint_fields(agent))),
    HarnessKind("claude-code", "Claude Code", apis=("anthropic-messages",), runtime=_claude_code_runtime,
                build=lambda agent: ClaudeCodeAdapter(executable=agent["executable"], model=agent["model"],
                                                      thinking=agent["thinking"], base_url=agent["base_url"],
                                                      api_key_env=agent["api_key_env"],
                                                      max_tokens=agent["max_tokens"])),
    HarnessKind("codex", "Codex", apis=("openai-responses",), runtime=_codex_runtime,
                build=lambda agent: CodexAdapter(executable=agent["executable"], model=agent["model"],
                                                 thinking=agent["thinking"], base_url=agent["base_url"],
                                                 api_key_env=agent["api_key_env"],
                                                 context_window=agent["context_window"])),
)}


def validate_agents(value: Any) -> list[dict[str, Any]]:
    from runner.operations import AIDEN_TOOL_LAYER, TOOL_LAYERS
    if not isinstance(value, list) or not 1 <= len(value) <= 8:
        raise ValueError("agents must contain between 1 and 8 configurations")
    result = []
    ids = set()
    for item in value:
        if not isinstance(item, dict) or set(item) - ({"id", "kind", "provider", "model", "thinking", "tool_layer"}
                                                     | CUSTOM_ENDPOINT_FIELDS):
            raise ValueError("Unknown agent configuration fields")
        agent = {key: str(val).strip() for key, val in item.items()}
        harness = HARNESSES.get(agent.get("kind", ""))
        if harness is None:
            raise ValueError(f"Supported agents: {', '.join(HARNESSES)}")
        agent.setdefault("id", harness.kind)
        if not SAFE_ID.fullmatch(agent["id"]) or agent["id"] in ids:
            raise ValueError("Agent IDs must be unique safe directory names")
        agent.setdefault("tool_layer", AIDEN_TOOL_LAYER)
        if agent["tool_layer"] not in TOOL_LAYERS:
            raise ValueError(f"tool_layer must be one of {', '.join(TOOL_LAYERS)}")
        if harness.external:
            _validate_external(harness, agent, item)
        elif agent["tool_layer"] != AIDEN_TOOL_LAYER:
            raise ValueError("Aiden uses its own tool layer; raw tools are available to external agents")
        elif set(agent) - {"id", "kind", "tool_layer"}:
            raise ValueError("Aiden uses the frozen Agent Config; set its model there")
        ids.add(agent["id"])
        result.append(agent)
    return result


def _validate_external(harness: HarnessKind, agent: dict, item: dict) -> None:
    label = harness.label
    if not agent.get("provider") or not agent.get("model"):
        raise ValueError(f"{label} requires an explicit provider and model")
    agent.setdefault("thinking", "medium")
    if agent["thinking"] not in THINKING_LEVELS:
        raise ValueError(f"Unsupported {label} thinking level")
    if not agent.get("base_url"):
        if set(item) & CUSTOM_ENDPOINT_FIELDS:
            raise ValueError(f"{label} custom endpoint fields require base_url")
        if harness.requires_base_url:
            raise ValueError(f"{label} comparison requires an explicit custom API base_url")
        return
    url = urllib.parse.urlsplit(agent["base_url"])
    try:
        port = url.port
    except ValueError:
        raise ValueError(f"{label} base_url has an invalid port") from None
    if (url.scheme not in {"http", "https"} or not url.hostname or url.username is not None
            or url.query or url.fragment or port == 0
            or any(char.isspace() for char in agent["base_url"])):
        raise ValueError(f"{label} base_url must be an HTTP(S) base URL without credentials, query or fragment")
    agent["base_url"] = agent["base_url"].rstrip("/")
    agent.setdefault("api", harness.apis[0])
    if agent["api"] not in harness.apis:
        raise ValueError(f"{label} supports API protocols: {', '.join(harness.apis)}")
    agent.setdefault("api_key_env", DEFAULT_API_KEY_ENV)
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", agent["api_key_env"]):
        raise ValueError("api_key_env must be an environment variable name, not a key or command")
    for name, default in (("context_window", 128000), ("max_tokens", 16384)):
        number = item.get(name, default)
        if type(number) is not int or number <= 0:
            raise ValueError(f"{label} {name} must be a positive integer")
        agent[name] = number
    if agent["max_tokens"] > agent["context_window"]:
        raise ValueError(f"{label} max_tokens must not exceed context_window")

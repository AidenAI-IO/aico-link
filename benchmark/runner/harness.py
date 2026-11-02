"""Native agent execution for portable, single-turn experiments.

Adapters translate evidence; they never retry tasks or add planning instructions.
Pi uses print/JSON mode so process completion includes its native retry loop.
"""
from __future__ import annotations

import dataclasses as dc
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any, Callable, Protocol
import uuid

from runner.agent_client import AgentClient, AgentTimeoutError
from runner.process import terminate_process_tree
from runner.tool_gateway import AUDIT_FILE, DaemonToolBackend, ToolGateway


PI_PROVIDER_ENV = {
    "anthropic": {"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN"},
    "openai": {"OPENAI_API_KEY"},
    "azure-openai": {"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_API_VERSION",
                     "AZURE_OPENAI_DEPLOYMENT_NAME_MAP"},
    "google": {"GEMINI_API_KEY"}, "openrouter": {"OPENROUTER_API_KEY"},
    "deepseek": {"DEEPSEEK_API_KEY"}, "groq": {"GROQ_API_KEY"},
    "mistral": {"MISTRAL_API_KEY"}, "xai": {"XAI_API_KEY"},
    "fireworks": {"FIREWORKS_API_KEY"}, "together": {"TOGETHER_API_KEY"},
    "cerebras": {"CEREBRAS_API_KEY"}, "minimax": {"MINIMAX_API_KEY"},
    "moonshot": {"MOONSHOT_API_KEY"}, "zai": {"ZAI_API_KEY", "ZAI_CODING_CN_API_KEY"},
    "kimi": {"KIMI_API_KEY"}, "opencode": {"OPENCODE_API_KEY"},
    "qwen": {"QWEN_TOKEN_PLAN_API_KEY", "QWEN_TOKEN_PLAN_CN_API_KEY"},
    "xiaomi": {"XIAOMI_API_KEY", "XIAOMI_TOKEN_PLAN_CN_API_KEY",
               "XIAOMI_TOKEN_PLAN_AMS_API_KEY", "XIAOMI_TOKEN_PLAN_SGP_API_KEY"},
    "cloudflare": {"CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"},
    "amazon-bedrock": {"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
                        "AWS_BEARER_TOKEN_BEDROCK", "AWS_REGION"},
}


# Network and TLS settings every host-side harness needs to reach its model.
PASSTHROUGH_ENV = ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
                   "SSL_CERT_FILE", "SSL_CERT_DIR")
CUSTOM_ENDPOINT_COST = {"cost_usd": None, "cost_source": None,
                        "cost_unavailable_reason": "Custom endpoint pricing is not configured"}
# OpenClaw's own turn timeout must not preempt the runner deadline, so that
# deadline overruns are recorded as timeouts for every harness.
OPENCLAW_TIMEOUT_GRACE_SEC = 30
# External harnesses run unsandboxed on the benchmark host, so they receive only
# the shared business tools: native shell, file, and device tools would act on
# the host or bypass the device interface.
OPENCLAW_TOOL_POLICY = {"profile": "full", "allow": ["benchmark__*"], "toolSearch": False}


def pi_environment(executable: str, provider: str, scratch: Path) -> dict[str, str]:
    """Give Pi an isolated home and only the selected provider's credentials."""
    node = shutil.which("node")
    paths = [str(Path(executable).resolve().parent)]
    if node:
        paths.insert(0, str(Path(node).resolve().parent))
    paths.append(os.defpath)
    env = {
        "PATH": os.pathsep.join(paths),
        "HOME": str(scratch / "home"),
        "TMPDIR": str(scratch),
        "PI_CODING_AGENT_DIR": str(scratch / "config"),
        "PI_CODING_AGENT_SESSION_DIR": str(scratch / "sessions"),
        "PI_OFFLINE": "1",
        "PI_TELEMETRY": "0",
    }
    if "LANG" in os.environ:
        env["LANG"] = os.environ["LANG"]
    for key in PI_PROVIDER_ENV.get(provider.lower(), set()):
        if key in os.environ:
            env[key] = os.environ[key]
    if provider.lower() == "amazon-bedrock":
        aws_dir = Path.home() / ".aws"
        for key, filename in (("AWS_SHARED_CREDENTIALS_FILE", "credentials"),
                              ("AWS_CONFIG_FILE", "config")):
            candidate = aws_dir / filename
            if key not in env and candidate.is_file():
                env[key] = str(candidate)
    for key in PASSTHROUGH_ENV:
        if key in os.environ:
            env[key] = os.environ[key]
    if os.name == "nt":
        for name in ("SystemRoot", "WINDIR", "USERPROFILE"):
            if name in os.environ:
                env[name] = os.environ[name]
    return env


@dc.dataclass
class Execution:
    history: list[dict[str, Any]] = dc.field(default_factory=list)
    episode: dict[str, Any] | None = None
    metrics: dict[str, Any] = dc.field(default_factory=dict)
    timed_out: bool = False
    canceled: bool = False
    pre_screenshot: Path | None = None
    post_screenshot: Path | None = None
    environment_state: dict[str, Any] | None = None
    environment_state_error: str = ""


class ExternalHarness(Protocol):
    """A native agent that reaches the device only through the tool gateway."""

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, tool_endpoint: str, tool_catalog: list[dict]) -> Execution: ...


def record_catalog(catalog: list[dict], artifacts: Path, tool_layer: str) -> dict[str, Any]:
    """Persist one attempt's business tool catalog and return its identity metrics."""
    serialized = json.dumps(catalog, sort_keys=True, ensure_ascii=False)
    (artifacts / "business-tools.json").write_text(serialized, encoding="utf-8")
    return {"business_tools_sha256": hashlib.sha256(serialized.encode()).hexdigest(),
            "business_tools": [tool["name"] for tool in catalog], "tool_layer": tool_layer}


def _run_with_deadline(command: list[str], *, cwd: Path, env: dict[str, str], prompt: str,
                       stdout_path: Path, stderr_path: Path, started: float, timeout: int,
                       stop: threading.Event) -> tuple[int | None, bool, bool]:
    """Feed the prompt on stdin and stop at the runner deadline or cancellation."""
    timed_out = canceled = False
    # stdin avoids option, @file, and shell expansion of task text.
    with stdout_path.open("wb") as output, stderr_path.open("wb") as errors:
        proc = subprocess.Popen(command, cwd=cwd, env=env, stdin=subprocess.PIPE,
                                stdout=output, stderr=errors, start_new_session=True)
        try:
            assert proc.stdin is not None
            proc.stdin.write(prompt.encode("utf-8"))
            proc.stdin.close()
            while proc.poll() is None:
                if stop.wait(0.1):
                    canceled = True
                    break
                if time.monotonic() - started >= timeout:
                    timed_out = True
                    break
        finally:
            terminate_process_tree(proc)
    return proc.returncode, timed_out, canceled


def _number(value: Any) -> bool:
    return type(value) in (int, float) and math.isfinite(value) and value >= 0


def normalize_pi_events(events: list[dict[str, Any]]) -> Execution:
    """Keep native tool names and pair results by call ID, including parallel calls."""
    result = Execution()
    calls: dict[str, dict[str, Any]] = {}
    evidence: dict[str, list[dict]] = {}
    costs: list[float] = []
    usage_complete = True
    token_usage_complete = True
    errors: list[str] = []
    api_retries = 0
    observed_models: set[str] = set()
    for event in events:
        kind = event.get("type")
        if kind == "tool_execution_start":
            call = {
                "type": "tool_call", "tool_call_id": event["toolCallId"],
                "tool_name": event["toolName"],
                "tool_input": json.dumps(event.get("args", {}), ensure_ascii=False),
            }
            calls[event["toolCallId"]] = call
            result.history.append(call)
        elif kind == "tool_execution_end":
            call_id = event["toolCallId"]
            call = calls.get(call_id)
            if call is None:
                raise ValueError(f"pi returned a result for unknown tool call {call_id}")
            # The existing scorer consumes adjacent call/result pairs. Preserve
            # actual interleaving in native-events.jsonl, pairing only this view.
            index = next(i for i, item in enumerate(result.history) if item is call)
            tool_output = event.get("result", {})
            details = tool_output.get("details", {}) if isinstance(tool_output, dict) else {}
            tool_evidence = []
            if details.get("source") == "aiden-tool-api":
                normalized = details.get("raw_input")
                if isinstance(normalized, str):
                    normalized = json.loads(normalized)
                if isinstance(normalized, dict):
                    tool_evidence.append({"type": "visual_coordinate_mapping", "metadata": {
                        "tool": event["toolName"], "normalized_input": normalized}})
            tool_evidence.append({"type": "tool_result", "tool_name": event["toolName"],
                                  "is_error": bool(event.get("isError")) or bool(details.get("is_error")),
                                  "tool_error": details.get("tool_error")})
            evidence[call_id] = tool_evidence
            if isinstance(tool_output, dict) and isinstance(tool_output.get("content"), list):
                # Keep full image bytes in native-events.jsonl. Normalized history
                # records the observation without duplicating megabytes of base64.
                images = [block for block in tool_output["content"] if block.get("type") == "image"]
                if images:
                    tool_output = {**tool_output, "observation": "screenshot observation",
                                   "content": [{k: v for k, v in block.items() if k != "data"}
                                               if block.get("type") == "image" else block
                                               for block in tool_output["content"]]}
            result.history.insert(index + 1, {
                "type": "tool_result", "tool_call_id": call_id,
                "tool_name": event["toolName"],
                "is_error": bool(event.get("isError")) or bool(details.get("is_error")),
                "content": json.dumps(tool_output, ensure_ascii=False),
            })
        elif kind == "message_end":
            message = event.get("message", {})
            if message.get("role") != "assistant":
                continue
            observed_models.add(f"{message.get('provider', '')}/{message.get('model', '')}")
            content = message.get("content", [])
            text = "\n".join(block.get("text", "") for block in content if block.get("type") == "text")
            item: dict[str, Any] = {"type": "assistant", "content": text}
            usage = message.get("usage")
            if isinstance(usage, dict):
                # Pi separates uncached input, cache reads and cache writes.
                input_parts = [usage.get(key) for key in ("input", "cacheRead", "cacheWrite")]
                item["usage"] = {
                    "input_tokens": sum(input_parts) if all(_number(v) for v in input_parts) else None,
                    "output_tokens": usage.get("output"),
                    "cached_input_tokens": usage.get("cacheRead"),
                }
                cost = (usage.get("cost") or {}).get("total")
                if _number(cost):
                    costs.append(cost)
                else:
                    usage_complete = False
            else:
                usage_complete = False
                token_usage_complete = False
            result.history.append(item)
            if message.get("stopReason") in {"error", "aborted", "length"}:
                errors.append(str(message.get("errorMessage") or message["stopReason"]))
                # pi retries failed model requests natively; each failure is one retry.
                api_retries += message.get("stopReason") == "error"
            elif message.get("stopReason") == "stop":
                # A native retry can successfully recover from an earlier error.
                errors.clear()
    result.episode = {"source": "pi-native-events-and-tool-api", "events": [
        item for call_id in calls for item in evidence.get(call_id, [])
    ]}
    result.metrics.update({
        "cost_usd": sum(costs) if costs and usage_complete else None,
        "cost_source": "pi_model_pricing_estimate" if costs and usage_complete else None,
        "observed_models": sorted(observed_models),
        "api_retries": api_retries,
    })
    if errors:
        result.metrics["agent_error"] = errors[-1]
    if not token_usage_complete:
        result.metrics.update(input_tokens=None, output_tokens=None, total_tokens=None,
                              cached_input_tokens=None)
    return result


class PiAdapter:
    def __init__(self, *, executable: str, provider: str, model: str, thinking: str,
                 base_url: str = "", api: str = "openai-completions",
                 api_key_env: str = "AIDEN_BENCHMARK_PI_API_KEY",
                 context_window: int = 128000, max_tokens: int = 16384):
        self.executable, self.provider, self.model, self.thinking = executable, provider, model, thinking
        self.base_url, self.api, self.api_key_env = base_url, api, api_key_env
        self.context_window, self.max_tokens = context_window, max_tokens

    def _configure_endpoint(self, env: dict[str, str], *, vision: bool) -> None:
        """Materialize only this experiment's endpoint; never copy personal config."""
        key = os.environ.get(self.api_key_env, "")
        if not key.strip():
            raise ValueError(f"Set {self.api_key_env} in the benchmark server environment before running pi")
        # A fixed private name avoids overwriting PATH/HOME when the user chooses
        # a credential variable. models.json contains only an environment reference.
        env["AIDEN_BENCHMARK_PI_CUSTOM_KEY"] = key
        directory = Path(env["PI_CODING_AGENT_DIR"])
        directory.mkdir(parents=True, exist_ok=True)
        settings = {"providers": {self.provider: {
            "baseUrl": self.base_url, "api": self.api,
            "apiKey": "$AIDEN_BENCHMARK_PI_CUSTOM_KEY",
            "models": [{"id": self.model, "input": ["text", "image"] if vision else ["text"],
                        "reasoning": self.thinking != "off", "contextWindow": self.context_window,
                        "maxTokens": self.max_tokens}],
        }}}
        (directory / "models.json").write_text(json.dumps(settings), encoding="utf-8")

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, tool_endpoint: str = "", tool_catalog: list[dict] | None = None) -> Execution:
        artifacts.mkdir(parents=True, exist_ok=True)
        if stop.is_set():
            return Execution(canceled=True)
        started = time.monotonic()
        with tempfile.TemporaryDirectory(prefix="aiden-pi-") as scratch:
            scratch_path = Path(scratch)
            workspace = scratch_path / "workspace"
            workspace.mkdir()
            env = pi_environment(self.executable, "" if self.base_url else self.provider, scratch_path)
            if self.base_url:
                self._configure_endpoint(env, vision=bool(tool_endpoint))
            command = [self.executable, "--mode", "json", "--print", "--no-session",
                       "--no-extensions", "--no-skills", "--no-prompt-templates",
                       "--no-context-files", "--no-themes", "--offline",
                       "--provider", self.provider, "--model", self.model,
                       "--thinking", self.thinking]
            # Built-in read/bash/edit/write act on the host; enable only the catalog.
            names = [tool["name"] for tool in tool_catalog or []]
            command.extend(["--tools", ",".join(names)] if names else ["--no-tools"])
            ready_file = scratch_path / "tools-ready.json"
            if tool_endpoint:
                config_file = scratch_path / "operation-tools.json"
                config_file.write_text(json.dumps({"endpoint": tool_endpoint, "tools": tool_catalog,
                                                   "ready_file": str(ready_file)}), encoding="utf-8")
                env["AIDEN_OPERATION_CONFIG"] = str(config_file)
                command.extend(["-e", str(Path(__file__).with_name("pi_operation_tools.mjs"))])
            returncode, timed_out, canceled = _run_with_deadline(
                command, cwd=workspace, env=env, prompt=prompt,
                stdout_path=artifacts / "native-events.jsonl", stderr_path=artifacts / "stderr.log",
                started=started, timeout=timeout, stop=stop)
            if tool_endpoint and not ready_file.exists() and not (timed_out or canceled):
                raise RuntimeError("pi did not load the operation tools extension; inspect stderr.log")
            events = []
            with (artifacts / "native-events.jsonl").open("rb") as stream:
                for line in stream:
                    if not line.strip():
                        continue
                    try:
                        event = json.loads(line)
                    except (ValueError, UnicodeDecodeError):
                        if timed_out or canceled:
                            break  # A killed process may leave one partial record.
                        raise ValueError("pi emitted malformed JSON; see native-events.jsonl")
                    if not isinstance(event, dict):
                        raise ValueError("pi emitted a non-object JSON event")
                    events.append(event)
            result = normalize_pi_events(events)
            if self.base_url:
                # Pi defaults custom models to zero prices. Unknown pricing must
                # not look like a free run or enter cost-per-success comparisons.
                result.metrics.update(CUSTOM_ENDPOINT_COST)
            result.timed_out, result.canceled = timed_out, canceled
            result.metrics["task_wall_ms"] = int((time.monotonic() - started) * 1000)
            result.metrics["exit_code"] = returncode
            if returncode and not (timed_out or canceled):
                result.metrics["agent_error"] = f"pi exited with code {returncode}; see stderr.log"
            if not events and not (timed_out or canceled):
                result.metrics["agent_error"] = "pi produced no events; see stderr.log"
            return result


MCP_BRIDGE = Path(__file__).with_name("mcp_operation_tools.py")
# Claude Code: bare mode skips hooks, plugins, and CLAUDE.md discovery; with no
# built-in tools and strict MCP config only the benchmark server's tools remain.
CLAUDE_CODE_EFFORT = {"off": "low", "minimal": "low", "low": "low", "medium": "medium",
                      "high": "high", "xhigh": "xhigh", "max": "max"}
# Codex: features and settings that would expose shell, web, image, agent, or
# desktop tools (`web_search="disabled"` covers web search). Verified against
# codex-cli 0.165.0; MCP resource helpers and request_user_input remain
# because they cannot act on the host. Benchmark MCP calls are pre-approved.
CODEX_DISABLED_FEATURES = (
    "shell_tool", "unified_exec", "goals", "view_image", "standalone_web_search", "multi_agent", "multi_agent_v2", "apps", "browser_use", "computer_use",
    "image_generation", "default_mode_request_user_input", "request_permissions_tool", "sleep_tool",
)
CODEX_EFFORT = {"off": "none", "minimal": "minimal", "low": "low", "medium": "medium",
                "high": "high", "xhigh": "xhigh", "max": "max"}
CODEX_NATIVE_ITEMS = {"command_execution", "file_change", "web_search"}


def _host_env(home: Path, *path_dirs: str | Path) -> dict[str, str]:
    """A minimal environment for a host CLI with a scratch home."""
    env = {key: value for key, value in os.environ.items() if key in {"LANG", "TMPDIR", *PASSTHROUGH_ENV}}
    env.update(HOME=str(home), PATH=os.pathsep.join([*(str(path) for path in path_dirs), os.defpath]))
    return env


def _api_key(api_key_env: str, harness: str) -> str:
    key = os.environ.get(api_key_env, "").strip()
    if not key:
        raise ValueError(f"Set {api_key_env} in the benchmark server environment before running {harness}")
    return key


def _write_bridge_config(root: Path, tool_endpoint: str, tool_catalog: list[dict]) -> Path:
    path = root / "operation-tools.json"
    path.write_text(json.dumps({"endpoint": tool_endpoint, "tools": tool_catalog}), encoding="utf-8")
    return path


def _gateway_history(artifacts: Path) -> tuple[list[dict], list[dict]]:
    """Rebuild scorer history and tool evidence from the tool gateway audit."""
    calls = []
    audit = artifacts / AUDIT_FILE
    if audit.exists():
        for line in audit.read_text(encoding="utf-8").splitlines():
            try:
                call = json.loads(line)
            except ValueError:
                # A call interrupted at the deadline can leave a partial final line.
                break
            if isinstance(call, dict):
                calls.append(call)
    history, evidence = [], []
    for call in calls:
        name, raw = call["tool_name"], call.get("raw_input") or "{}"
        history.append({"type": "tool_call", "tool_name": name, "tool_input": raw})
        history.append({"type": "tool_result", "tool_name": name,
                        "is_error": call["is_error"], "content": call["observation"]})
        evidence.append({"type": "visual_coordinate_mapping", "metadata": {"tool": name, "normalized_input": raw}})
        evidence.append({"type": "tool_result", "tool_name": name,
                         "is_error": call["is_error"], "tool_error": call.get("tool_error")})
    return history, evidence


def _final_answer(history: list[dict], text: Any, usage: dict[str, Any] | None) -> None:
    """Append the final response; usage totals feed the existing token metrics."""
    if not isinstance(text, str) or not text.strip():
        return
    item: dict[str, Any] = {"type": "assistant", "content": text}
    if usage:
        item["usage"] = usage
    history.append(item)


def _jsonl_events(path: Path, *, interrupted: bool) -> list[dict]:
    events = []
    if path.exists():
        for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                if interrupted:
                    break  # A killed process may leave one partial record.
                continue
            if isinstance(event, dict):
                events.append(event)
    return events


def _gateway_execution(artifacts: Path, source: str, *, started: float, returncode: int | None,
                       timed_out: bool, canceled: bool, model: str) -> Execution:
    history, evidence = _gateway_history(artifacts)
    result = Execution(history=history, episode={"source": source, "events": evidence},
                       timed_out=timed_out, canceled=canceled)
    result.metrics.update(task_wall_ms=int((time.monotonic() - started) * 1000), exit_code=returncode,
                          observed_models=[model], **CUSTOM_ENDPOINT_COST)
    return result


class OpenClawAdapter:
    """One embedded OpenClaw turn with an isolated config, state, and MCP tools."""

    def __init__(self, *, executable: str, node: str, provider: str, model: str,
                 thinking: str, base_url: str, api_key_env: str,
                 context_window: int = 128000, max_tokens: int = 16384,
                 api: str = "openai-completions"):
        self.executable, self.node = executable, node
        self.provider, self.model, self.thinking = provider, model, thinking
        self.base_url, self.api_key_env, self.api = base_url, api_key_env, api
        self.context_window, self.max_tokens = context_window, max_tokens

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, tool_endpoint: str, tool_catalog: list[dict]) -> Execution:
        artifacts.mkdir(parents=True, exist_ok=True)
        if stop.is_set():
            return Execution(canceled=True)
        key = _api_key(self.api_key_env, "OpenClaw")
        started = time.monotonic()
        with tempfile.TemporaryDirectory(prefix="aiden-openclaw-") as scratch:
            root = Path(scratch)
            workspace, state = root / "workspace", root / "state"
            for directory in (workspace, state, root / "home"):
                directory.mkdir()
            bridge_config = _write_bridge_config(root, tool_endpoint, tool_catalog)
            config = {"models": {"mode": "replace", "providers": {self.provider: {
                "baseUrl": self.base_url, "api": self.api,
                "apiKey": "${AIDEN_BENCHMARK_OPENCLAW_KEY}",
                "models": [{"id": self.model, "name": self.model, "reasoning": self.thinking != "off",
                            "input": ["text", "image"], "contextWindow": self.context_window,
                            "maxTokens": self.max_tokens}],
            }}}, "agents": {"defaults": {"contextInjection": "never"}},
                "tools": OPENCLAW_TOOL_POLICY,
                "mcp": {"servers": {"benchmark": {"command": sys.executable,
                                                  "args": [str(MCP_BRIDGE), str(bridge_config)]}}}}
            config_path = root / "openclaw.json"
            config_path.write_text(json.dumps(config), encoding="utf-8")
            env = _host_env(root / "home", Path(self.node).parent, Path(self.executable).parent)
            env.update(OPENCLAW_AGENT_DIR=str(state / "agent"), AIDEN_BENCHMARK_OPENCLAW_KEY=key)
            command = [self.node, self.executable, "agent", "exec", "--config", str(config_path),
                       "--state-dir", str(state), "--cwd", str(workspace),
                       "--message-file", "-", "--json", "--model", f"{self.provider}/{self.model}",
                       "--thinking", self.thinking, "--code-mode", "direct",
                       "--timeout", str(timeout + OPENCLAW_TIMEOUT_GRACE_SEC)]
            returncode, timed_out, canceled = _run_with_deadline(
                command, cwd=workspace, env=env, prompt=prompt,
                stdout_path=artifacts / "native-output.json", stderr_path=artifacts / "stderr.log",
                started=started, timeout=timeout, stop=stop)
            try:
                parsed = json.loads((artifacts / "native-output.json").read_text(encoding="utf-8"))
                envelope = parsed if isinstance(parsed, dict) else {}
            except (ValueError, UnicodeDecodeError):
                envelope = {}
            for transcript in sorted(state.rglob("*.jsonl")):
                if "sessions" in transcript.parts:
                    shutil.copyfile(transcript, artifacts / "native-transcript.jsonl")
                    break
        result = _gateway_execution(artifacts, "openclaw-tool-gateway-audit", started=started,
                                    returncode=returncode, timed_out=timed_out, canceled=canceled,
                                    model=f"{self.provider}/{self.model}")
        usage = envelope.get("usage") if isinstance(envelope.get("usage"), dict) else {}
        input_parts = [usage.get(key) for key in ("input", "cacheRead", "cacheWrite")]
        _final_answer(result.history, envelope.get("final"), {
            "input_tokens": sum(input_parts) if all(_number(v) for v in input_parts) else None,
            "output_tokens": usage.get("output"), "cached_input_tokens": usage.get("cacheRead"),
        } if usage else None)
        summary = envelope.get("toolSummary") if isinstance(envelope.get("toolSummary"), dict) else {}
        if isinstance(summary.get("calls"), int):
            # Native calls are absent from the gateway audit; OpenClaw reports them here.
            # The summary has only a total, so a call count is attributed to native
            # tools only when it names one; business calls can fail before the gateway.
            native = sorted(name for name in summary.get("tools") or [] if not str(name).startswith("benchmark__"))
            business_calls = sum(item["type"] == "tool_call" for item in result.history)
            result.metrics["native_tool_calls_reported"] = max(1, summary["calls"] - business_calls) if native else 0
            result.metrics["native_tools_reported"] = native
        if not (timed_out or canceled) and (returncode or envelope.get("ok") is not True
                                            or envelope.get("status") != "ok"):
            error = envelope.get("error")
            message = error.get("message") if isinstance(error, dict) else error
            result.metrics["agent_error"] = str(message or f"OpenClaw exited with code {returncode}; see stderr.log")
        return result


class ClaudeCodeAdapter:
    """One `claude -p` turn in bare mode whose only tools are the MCP business tools."""

    def __init__(self, *, executable: str, model: str, thinking: str, base_url: str,
                 api_key_env: str, max_tokens: int = 16384):
        self.executable, self.model, self.thinking = executable, model, thinking
        self.base_url, self.api_key_env, self.max_tokens = base_url, api_key_env, max_tokens

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, tool_endpoint: str, tool_catalog: list[dict]) -> Execution:
        artifacts.mkdir(parents=True, exist_ok=True)
        if stop.is_set():
            return Execution(canceled=True)
        key = _api_key(self.api_key_env, "Claude Code")
        started = time.monotonic()
        with tempfile.TemporaryDirectory(prefix="aiden-claude-code-") as scratch:
            root = Path(scratch)
            for directory in ("home", "config", "workspace"):
                (root / directory).mkdir()
            mcp_config = root / "mcp.json"
            mcp_config.write_text(json.dumps({"mcpServers": {"benchmark": {
                "command": sys.executable,
                "args": [str(MCP_BRIDGE), str(_write_bridge_config(root, tool_endpoint, tool_catalog))]}}}),
                encoding="utf-8")
            env = _host_env(root / "home", Path(self.executable).parent)
            # The Anthropic client appends /v1/messages to its base URL.
            base_url = self.base_url[:-3] if self.base_url.endswith("/v1") else self.base_url
            env.update(CLAUDE_CONFIG_DIR=str(root / "config"), ANTHROPIC_BASE_URL=base_url,
                       ANTHROPIC_API_KEY=key, ANTHROPIC_DEFAULT_HAIKU_MODEL=self.model,
                       CLAUDE_CODE_MAX_OUTPUT_TOKENS=str(self.max_tokens), MCP_TOOL_TIMEOUT="300000",
                       DISABLE_TELEMETRY="1", DISABLE_AUTOUPDATER="1", DISABLE_ERROR_REPORTING="1",
                       CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1")
            command = [self.executable, "-p", "--bare", "--output-format", "stream-json", "--verbose",
                       "--model", self.model, "--effort", CLAUDE_CODE_EFFORT[self.thinking],
                       "--tools", "", "--mcp-config", str(mcp_config), "--strict-mcp-config",
                       "--allowedTools", "mcp__benchmark", "--setting-sources", "",
                       "--no-session-persistence"]
            returncode, timed_out, canceled = _run_with_deadline(
                command, cwd=root / "workspace", env=env, prompt=prompt,
                stdout_path=artifacts / "native-events.jsonl", stderr_path=artifacts / "stderr.log",
                started=started, timeout=timeout, stop=stop)
        events = _jsonl_events(artifacts / "native-events.jsonl", interrupted=timed_out or canceled)
        result = _gateway_execution(artifacts, "claude-code-tool-gateway-audit", started=started,
                                    returncode=returncode, timed_out=timed_out, canceled=canceled, model=self.model)
        outcome = next((event for event in reversed(events) if event.get("type") == "result"), {})
        usage = outcome.get("usage") if isinstance(outcome.get("usage"), dict) else {}
        input_parts = [usage.get(key) for key in
                       ("input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")]
        _final_answer(result.history, outcome.get("result"), {
            "input_tokens": sum(v or 0 for v in input_parts) if _number(usage.get("input_tokens")) else None,
            "output_tokens": usage.get("output_tokens"),
            "cached_input_tokens": usage.get("cache_read_input_tokens"),
        } if usage else None)
        init = next((event for event in events if event.get("type") == "system" and event.get("tools") is not None), {})
        result.metrics["offered_tools"] = init.get("tools")
        native = [block.get("name") for event in events if event.get("type") == "assistant"
                  for block in (event.get("message") or {}).get("content") or []
                  if isinstance(block, dict) and block.get("type") == "tool_use"
                  and not str(block.get("name", "")).startswith("mcp__benchmark__")]
        result.metrics.update(native_tool_calls_reported=len(native), native_tools_reported=sorted(set(native)),
                              api_retries=sum(event.get("type") == "system" and event.get("subtype") == "api_retry"
                                              for event in events))
        if not (timed_out or canceled) and (returncode or outcome.get("subtype") != "success"
                                            or outcome.get("is_error")):
            result.metrics["agent_error"] = str(outcome.get("result") or outcome.get("subtype")
                                                or f"Claude Code exited with code {returncode}; see stderr.log")
        return result


class CodexAdapter:
    """One ephemeral `codex exec` turn whose only device tools are the MCP business tools."""

    def __init__(self, *, executable: str, model: str, thinking: str, base_url: str, api_key_env: str,
                 context_window: int = 128000):
        self.executable, self.model, self.thinking = executable, model, thinking
        self.base_url, self.api_key_env, self.context_window = base_url, api_key_env, context_window

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, tool_endpoint: str, tool_catalog: list[dict]) -> Execution:
        artifacts.mkdir(parents=True, exist_ok=True)
        if stop.is_set():
            return Execution(canceled=True)
        key = _api_key(self.api_key_env, "Codex")
        started = time.monotonic()
        toml = json.dumps  # JSON strings are valid TOML basic strings.
        with tempfile.TemporaryDirectory(prefix="aiden-codex-") as scratch:
            root = Path(scratch)
            for directory in ("home", "codex-home", "workspace"):
                (root / directory).mkdir()
            bridge_config = _write_bridge_config(root, tool_endpoint, tool_catalog)
            env = _host_env(root / "home", Path(self.executable).resolve().parent, Path(self.executable).parent)
            env.update(CODEX_HOME=str(root / "codex-home"), AIDEN_BENCHMARK_CODEX_KEY=key)
            command = [self.executable, "exec", "--json", "--ephemeral", "--skip-git-repo-check",
                       "--ignore-user-config", "--ignore-rules", "-C", str(root / "workspace"),
                       "-s", "read-only", "-m", self.model,
                       "-c", 'model_provider="benchmark"',
                       "-c", f"model_providers.benchmark={{name=\"benchmark\", base_url={toml(self.base_url)}, "
                             "env_key=\"AIDEN_BENCHMARK_CODEX_KEY\", wire_api=\"responses\"}",
                       "-c", f"mcp_servers.benchmark={{command={toml(sys.executable)}, "
                             f"args=[{toml(str(MCP_BRIDGE))}, {toml(str(bridge_config))}], tool_timeout_sec=300, "
                             'default_tools_approval_mode="approve"}',
                       "-c", f"model_reasoning_effort={toml(CODEX_EFFORT[self.thinking])}",
                       "-c", f"model_context_window={self.context_window}",
                       "-c", 'approval_policy="never"', "-c", 'web_search="disabled"',
                       *(arg for feature in CODEX_DISABLED_FEATURES for arg in ("--disable", feature)), "-"]
            returncode, timed_out, canceled = _run_with_deadline(
                command, cwd=root / "workspace", env=env, prompt=prompt,
                stdout_path=artifacts / "native-events.jsonl", stderr_path=artifacts / "stderr.log",
                started=started, timeout=timeout, stop=stop)
        events = _jsonl_events(artifacts / "native-events.jsonl", interrupted=timed_out or canceled)
        result = _gateway_execution(artifacts, "codex-tool-gateway-audit", started=started,
                                    returncode=returncode, timed_out=timed_out, canceled=canceled, model=self.model)
        items = [event.get("item") or {} for event in events if event.get("type") == "item.completed"]
        messages = [item.get("text") for item in items if item.get("type") == "agent_message"]
        totals: dict[str, int] = {}
        for event in events:
            if event.get("type") == "turn.completed" and isinstance(event.get("usage"), dict):
                for name, value in event["usage"].items():
                    if _number(value):
                        totals[name] = totals.get(name, 0) + value
        _final_answer(result.history, messages[-1] if messages else None, {
            "input_tokens": totals.get("input_tokens"), "output_tokens": totals.get("output_tokens"),
            "cached_input_tokens": totals.get("cached_input_tokens"),
        } if totals else None)
        native = [item.get("type") if item.get("type") in CODEX_NATIVE_ITEMS else f"mcp:{item.get('server')}"
                  for item in items if item.get("type") in CODEX_NATIVE_ITEMS
                  or (item.get("type") == "mcp_tool_call" and item.get("server") != "benchmark")]
        result.metrics.update(native_tool_calls_reported=len(native), native_tools_reported=sorted(set(native)),
                              # Top-level error events are Codex reconnects to the model endpoint.
                              api_retries=sum(event.get("type") == "error" for event in events))
        failure = next((event for event in reversed(events) if event.get("type") == "turn.failed"), None)
        if failure is None and not any(event.get("type") == "turn.completed" for event in events):
            failure = next((event for event in reversed(events) if event.get("type") == "error"), None)
        if not (timed_out or canceled) and (returncode or failure or not messages):
            error = (failure or {}).get("error") or (failure or {}).get("message")
            message = error.get("message") if isinstance(error, dict) else error
            result.metrics["agent_error"] = str(message or f"Codex exited with code {returncode}; see stderr.log")
        return result


def episode_step_screenshots(episode: dict[str, Any]) -> dict[int, str]:
    """Map each tool call's ordinal to the episode screenshot its result recorded."""
    steps: dict[int, str] = {}
    calls = 0
    for event in episode.get("events") or []:
        if event.get("type") == "tool_call":
            calls += 1
        elif event.get("type") == "tool_result" and calls and isinstance(event.get("screenshot_ref"), str):
            steps[calls] = event["screenshot_ref"]
    return steps


def export_episode_screenshots(container: str, episode: dict[str, Any], artifacts: Path) -> int:
    """Copy Aiden's per-step screenshots out of its daemon as `step_<n>` report images.

    External harnesses get these from the tool gateway; Aiden calls its tools
    directly, so they exist only inside the daemon until it is removed.
    """
    steps = episode_step_screenshots(episode)
    if not steps:
        return 0
    name = re.sub(r"[^A-Za-z0-9_.-]", "_", str(episode.get("id") or "")) or "unknown"
    found = subprocess.run(["docker", "exec", container, "find", "/", "-xdev", "-type", "d", "-name", name,
                            "-path", "*episodes*"], capture_output=True, text=True, timeout=30)
    episode_dir = next((line for line in found.stdout.splitlines() if line.strip()), "")
    if not episode_dir:
        raise RuntimeError(f"episode directory for {name} not found in the daemon")
    with tempfile.TemporaryDirectory(prefix="aiden-episode-") as scratch:
        subprocess.run(["docker", "cp", f"{container}:{episode_dir}/artifacts", scratch],
                       check=True, capture_output=True, timeout=60)
        exported = 0
        for step, ref in steps.items():
            source = Path(scratch) / "artifacts" / Path(ref).name
            if source.is_file():
                suffix = ".png" if source.suffix.lower() == ".png" else ".jpg"
                shutil.copyfile(source, artifacts / f"step_{step}{suffix}")
                exported += 1
    return exported


class AidenAdapter:
    """Each attempt owns a fresh Docker daemon and its persistent state."""

    def __init__(self, *, image: str, prepared_config: Path, ready_timeout: int = 90):
        self.image, self.prepared_config, self.ready_timeout = image, prepared_config, ready_timeout

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, environment_url: str = "", benchmark_task_id: str = "",
                external: ExternalHarness | None = None,
                select_tools: Callable[[list[dict]], list[dict]] | None = None) -> Execution:
        """Run Aiden natively, or serve its selected tools to an external harness."""
        # Import lazily: the existing WebUI owns the daemon provisioning helpers.
        from runner.webui import (Job, docker_published_port, prepare_run_config,
                                  start_daemon_compose, stop_daemon_compose, endpoint_for_docker)
        artifacts.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="aiden-experiment-") as scratch:
            config = Path(scratch) / "config"
            prepare_run_config(self.prepared_config, config)
            job = Job(id=f"experiment-{uuid.uuid4().hex[:16]}", endpoint="", docker_endpoint="", suites=[])
            client = None
            worker = None
            result = Execution()
            setup_started = time.monotonic()
            try:
                container = start_daemon_compose(
                    job, image=self.image, host_port=0, config_dir=config,
                    environment_bridge_endpoint=endpoint_for_docker(environment_url) if environment_url else "",
                    environment_bridge_mode=bool(environment_url),
                    benchmark_task_id=benchmark_task_id,
                    device_type="android" if environment_url else "",
                    log_path=artifacts / "daemon-setup.log", stop_requested=stop.is_set,
                )
                client = AgentClient(f"http://127.0.0.1:{docker_published_port(container, 8080)}")
                deadline = time.monotonic() + self.ready_timeout
                while not client.health():
                    if stop.wait(0.2):
                        return Execution(canceled=True)
                    if time.monotonic() >= deadline:
                        raise RuntimeError("Aiden daemon did not become ready")
                result.metrics["setup_ms"] = int((time.monotonic() - setup_started) * 1000)
                if select_tools is not None:
                    catalog = select_tools(client.list_tools())
                    result.metrics.update(record_catalog(catalog, artifacts, "aiden"))
                    if external is not None:
                        # The external harness sees the gateway, never the daemon URL.
                        with ToolGateway(DaemonToolBackend(client.base_url), catalog, artifacts) as gateway:
                            execution = external.execute(prompt, timeout, artifacts, stop,
                                                         tool_endpoint=gateway.url, tool_catalog=catalog)
                        execution.metrics.update(result.metrics)
                        return execution
                started = time.monotonic()
                done = threading.Event()
                response: list[Any] = []

                def chat() -> None:
                    try:
                        response.append(client.chat(prompt, timeout_sec=timeout))
                    except Exception as exc:
                        response.append(exc)
                    finally:
                        done.set()

                worker = threading.Thread(target=chat, daemon=True)
                worker.start()
                while not done.wait(0.1):
                    if stop.is_set():
                        result.canceled = True
                        return result  # finally removes the owned daemon.
                    if time.monotonic() - started >= timeout:
                        result.timed_out = True
                        result.metrics.update(task_wall_ms=int((time.monotonic() - started) * 1000),
                                              agent_error="Aiden exceeded the execution deadline")
                        return result
                result.metrics["task_wall_ms"] = int((time.monotonic() - started) * 1000)
                value = response[0]
                if isinstance(value, Exception):
                    result.timed_out = isinstance(value, AgentTimeoutError)
                    result.metrics["agent_error"] = str(value)
                    try:
                        result.history = client.get_history()
                    except Exception:
                        pass
                else:
                    result.history = value.history
                    if value.episode_id:
                        try:
                            result.episode = client.get_episode(value.episode_id)
                        except Exception as exc:
                            result.metrics["episode_error"] = str(exc)
                (artifacts / "native-history.json").write_text(json.dumps(result.history, ensure_ascii=False), encoding="utf-8")
                if result.episode is not None:
                    (artifacts / "episode.json").write_text(json.dumps(result.episode, ensure_ascii=False), encoding="utf-8")
                    try:
                        result.metrics["step_screenshots_exported"] = export_episode_screenshots(
                            container, result.episode, artifacts)
                    except (OSError, RuntimeError, subprocess.SubprocessError) as exc:
                        # Evidence for the report only; scoring already has pre/post images.
                        result.metrics["step_screenshot_error"] = str(exc)
                return result
            finally:
                # A runner deadline can fire before chat() returns its partial
                # history. Retain evidence before destroying the owned daemon,
                # just as pi retains its partial event stream when terminated.
                if client is not None and worker is not None and (result.timed_out or result.canceled):
                    if not result.history:
                        try:
                            result.history = client.get_history()
                        except Exception as exc:
                            result.metrics["history_error"] = str(exc)
                    (artifacts / "native-history.json").write_text(
                        json.dumps(result.history, ensure_ascii=False), encoding="utf-8")
                stop_daemon_compose(job)
                if client is not None:
                    client.close()
                if worker is not None:
                    worker.join(timeout=2)

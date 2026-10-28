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
import shutil
import subprocess
import tempfile
import threading
import time
from typing import Any, Protocol
import uuid

from runner.agent_client import AgentClient, AgentTimeoutError
from runner.process import terminate_process_tree


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
    for key in ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
                "SSL_CERT_FILE", "SSL_CERT_DIR"):
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


class HarnessAdapter(Protocol):
    def execute(self, prompt: str, timeout: int, artifacts: Path,
                stop: threading.Event) -> Execution: ...


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
        timed_out = canceled = False
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
            ready_file = scratch_path / "tools-ready.json"
            if tool_endpoint:
                config_file = scratch_path / "operation-tools.json"
                config_file.write_text(json.dumps({"endpoint": tool_endpoint, "tools": tool_catalog,
                                                   "ready_file": str(ready_file)}), encoding="utf-8")
                env["AIDEN_OPERATION_CONFIG"] = str(config_file)
                command.extend(["-e", str(Path(__file__).with_name("pi_operation_tools.mjs"))])
            # stdin avoids option, @file, and shell expansion of task text.
            with (artifacts / "native-events.jsonl").open("wb") as output, \
                    (artifacts / "stderr.log").open("wb") as errors:
                proc = subprocess.Popen(command, cwd=workspace, env=env,
                                        stdin=subprocess.PIPE, stdout=output, stderr=errors,
                                        start_new_session=True)
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
                result.metrics.update(cost_usd=None, cost_source=None,
                                      cost_unavailable_reason="Custom endpoint pricing is not configured")
            result.timed_out, result.canceled = timed_out, canceled
            result.metrics["task_wall_ms"] = int((time.monotonic() - started) * 1000)
            result.metrics["exit_code"] = proc.returncode
            if proc.returncode and not (timed_out or canceled):
                result.metrics["agent_error"] = f"pi exited with code {proc.returncode}; see stderr.log"
            if not events and not (timed_out or canceled):
                result.metrics["agent_error"] = "pi produced no events; see stderr.log"
            return result


class AidenAdapter:
    """Each attempt owns a fresh Docker daemon and its persistent state."""

    def __init__(self, *, image: str, prepared_config: Path, ready_timeout: int = 90):
        self.image, self.prepared_config, self.ready_timeout = image, prepared_config, ready_timeout

    def execute(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                *, environment_url: str = "", benchmark_task_id: str = "",
                pi: PiAdapter | None = None, tool_names: tuple[str, ...] = ()) -> Execution:
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
                if tool_names:
                    _, body = client._get("/api/tools")
                    available = {tool["name"]: tool for tool in json.loads(body)["tools"]}
                    missing = set(tool_names) - available.keys()
                    if missing:
                        raise RuntimeError(f"Shared business tools are missing: {sorted(missing)}")
                    catalog = [available[name] for name in tool_names]
                    serialized = json.dumps(catalog, sort_keys=True, ensure_ascii=False)
                    (artifacts / "business-tools.json").write_text(serialized, encoding="utf-8")
                    result.metrics["business_tools_sha256"] = hashlib.sha256(serialized.encode()).hexdigest()
                    if pi is not None:
                        execution = pi.execute(prompt, timeout, artifacts, stop,
                                               tool_endpoint=client.base_url, tool_catalog=catalog)
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

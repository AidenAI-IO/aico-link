#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import quote, unquote, urlparse
from urllib.request import Request, urlopen


SCRIPT_PATH = Path(__file__).resolve()
BENCHMARK_ROOT = SCRIPT_PATH.parents[2]
HOST = "127.0.0.1"
PORT = 4174
_USER_TEMP = Path(tempfile.gettempdir()) / f"mobilegym-{os.getuid()}"
_USER_TEMP.mkdir(mode=0o700, exist_ok=True)
_USER_TEMP.chmod(0o700)
LOG_PATH = _USER_TEMP / "mobilegym_run.log"
PID_PATH = _USER_TEMP / "mobilegym_runner.pid"
STATE_NAME = "state.json"
TAIL_BYTES = 64 * 1024
_SAFE_SUITE_SEGMENT = re.compile(r"^[A-Za-z0-9_.\-]+$")
_SAFE_RUN_ID = re.compile(r"^[A-Za-z0-9_\-]+$")
COMMON_CLI_PATHS = [
    "/usr/local/bin",
    "/opt/homebrew/bin",
    "/Applications/Docker.app/Contents/Resources/bin",
]


class LauncherError(RuntimeError):
    pass


class RunCommand:
    def __init__(self, argv: list[str], cwd: Path, env: dict[str, str]) -> None:
        self.argv = argv
        self.cwd = cwd
        self.env = env


_process: subprocess.Popen[bytes] | None = None
_process_lock = threading.Lock()


def valid_suite_name(suite: str) -> bool:
    if not suite or suite.startswith("/"):
        return False
    for part in suite.split("/"):
        if part in {"", ".", ".."} or not _SAFE_SUITE_SEGMENT.match(part):
            return False
    return True


def is_safe_run_id(run_id: str) -> bool:
    return bool(run_id and _SAFE_RUN_ID.match(run_id))


def selected_suites(payload: dict[str, Any]) -> list[str]:
    suites_value = payload.get("suites")
    if suites_value is not None:
        if not isinstance(suites_value, list):
            raise LauncherError("suites must be a JSON array")
        suites = [str(item).strip() for item in suites_value]
    else:
        suites = [str(payload.get("suite") or "").strip()]

    suites = [suite for suite in suites if suite]
    if not suites:
        raise LauncherError("missing suite field")
    for suite in suites:
        if not valid_suite_name(suite):
            raise LauncherError(f"invalid suite name: {suite!r}")
    return suites


def add_common_cli_paths(env: dict[str, str]) -> None:
    current = env.get("PATH") or "/usr/bin:/bin:/usr/sbin:/sbin"
    parts = [part for part in current.split(os.pathsep) if part]
    for path in reversed(COMMON_CLI_PATHS):
        if path not in parts:
            parts.insert(0, path)
    env["PATH"] = os.pathsep.join(parts)


def scan_suites(benchmark_root: str | Path = BENCHMARK_ROOT) -> list[dict[str, Any]]:
    root = Path(benchmark_root)
    items: list[dict[str, Any]] = []
    suites_dir = root / "suites"
    if suites_dir.exists():
        for suite_path in sorted(suites_dir.rglob("*.json")):
            if suite_path.name.startswith("._"):
                continue
            rel = suite_path.relative_to(suites_dir)
            items.append(
                {
                    "name": suite_path.stem,
                    "path": str(rel),
                    "custom": rel.parts[:1] == ("custom",),
                    "type": "aico",
                    "task_count": count_aico_tasks(suite_path),
                    "concurrent": True,
                }
            )

    return sorted(items, key=lambda item: (item["type"] != "aico", item["name"]))


def scan_mobilegym_builtins(root: Path) -> list[dict[str, Any]]:
    all_tasks = root / "mobilegym" / "suites" / "all_tasks.txt"
    try:
        lines = all_tasks.read_text(encoding="utf-8").splitlines()
    except OSError:
        return []

    counts: dict[str, int] = {}
    for line in lines:
        task = line.strip()
        if not task or "." not in task:
            continue
        suite, _ = task.split(".", 1)
        counts[suite] = counts.get(suite, 0) + 1

    return [
        {
            "name": name,
            "type": "mobilegym_builtin",
            "task_count": count,
            "concurrent": True,
        }
        for name, count in sorted(counts.items())
    ]


def count_aico_tasks(suite_path: Path) -> int:
    try:
        raw = json.loads(suite_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return 0
    tasks = raw.get("tasks")
    return len(tasks) if isinstance(tasks, list) else 0


def build_run_command(
    benchmark_root: str | Path = BENCHMARK_ROOT,
    payload: dict[str, Any] | None = None,
) -> RunCommand:
    payload = payload or {}
    mode = str(payload.get("mode") or "mobilegym")
    if mode not in {"mobilegym", ""}:
        raise LauncherError(f"unsupported local launcher mode: {mode!r}")

    return build_mobilegym_run_command(benchmark_root, payload)


def build_mobilegym_run_command(
    benchmark_root: str | Path = BENCHMARK_ROOT,
    payload: dict[str, Any] | None = None,
) -> RunCommand:
    raise LauncherError("Mac-local MobileGym runs have moved to the benchmark WebUI")


def start_run(benchmark_root: Path, payload: dict[str, Any]) -> dict[str, Any]:
    return start_mobilegym_run(benchmark_root, payload)


def start_mobilegym_run(benchmark_root: Path, payload: dict[str, Any]) -> dict[str, Any]:
    global _process
    with _process_lock:
        if _process is not None and _process.poll() is None:
            raise LauncherError("MobileGym benchmark is already running")

        command = build_run_command(benchmark_root, payload)
        suites = selected_suites(payload)
        validate_model_environment(command.env)
        LOG_PATH.write_text("Starting Mac-local MobileGym benchmark...\n", encoding="utf-8")
        log_file = LOG_PATH.open("ab")
        try:
            _process = subprocess.Popen(
                command.argv,
                cwd=command.cwd,
                env=command.env,
                stdout=log_file,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
        finally:
            log_file.close()

        PID_PATH.write_text(str(_process.pid), encoding="utf-8")
        write_state(
            benchmark_root,
            {
                "status": "running",
                "mode": "mobilegym",
                "run_id": command.env.get("MOBILEGYM_BATCH_ID", ""),
                "suite": ",".join(suites),
                "suites": suites,
                "suite_type": payload.get("suite_type") or "mobilegym_builtin",
                "parallel": int(payload.get("parallel") or 4),
                "total": int(payload.get("limit") or 0),
                "current": 1 if int(payload.get("limit") or 0) else 0,
                "completed": 0,
                "model": current_model_label(command.env),
            },
        )
        threading.Thread(target=watch_process, args=(benchmark_root, _process), daemon=True).start()
        return {"ok": True, "status": "running", "pid": _process.pid}


def watch_process(benchmark_root: Path, process: subprocess.Popen[bytes]) -> None:
    global _process
    rc = process.wait()
    try:
        PID_PATH.unlink()
    except OSError:
        pass
    write_state(benchmark_root, {"status": "idle", "exit_code": rc})
    with _process_lock:
        if _process is process:
            _process = None


def write_state(benchmark_root: Path, state: dict[str, Any]) -> None:
    try:
        (benchmark_root / STATE_NAME).write_text(
            json.dumps(state, separators=(",", ":")),
            encoding="utf-8",
        )
    except OSError:
        pass


def current_status(benchmark_root: Path) -> dict[str, Any]:
    with _process_lock:
        running = _process is not None and _process.poll() is None
    if running:
        try:
            raw = json.loads((benchmark_root / STATE_NAME).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            raw = {}
        raw["status"] = "running"
        raw.update(mobilegym_progress(benchmark_root, str(raw.get("run_id") or "")))
        return raw
    return {"status": "idle"}


def read_log() -> str:
    try:
        data = LOG_PATH.read_bytes()
    except OSError:
        return ""
    if len(data) > TAIL_BYTES:
        data = data[-TAIL_BYTES:]
    return data.decode("utf-8", errors="replace")


def list_runs(benchmark_root: Path) -> list[dict[str, Any]]:
    return sorted(
        list_mobilegym_runs(benchmark_root),
        key=lambda item: str(item.get("run_id") or ""),
        reverse=True,
    )[:20]


def list_mobilegym_runs(benchmark_root: Path) -> list[dict[str, Any]]:
    runs_dir = benchmark_root / "runs" / "mobilegym"
    state = read_json_file(benchmark_root / STATE_NAME)
    try:
        run_ids = sorted((p.name for p in runs_dir.iterdir() if p.is_dir()), reverse=True)
    except OSError:
        return []
    items = []
    for run_id in run_ids[:20]:
        summary = read_json_file(runs_dir / run_id / "summary.json")
        state_for_run = state if state.get("run_id") == run_id and state.get("status") == "running" else {}
        progress = mobilegym_progress(benchmark_root, run_id) if state_for_run else {}
        rows = summary_run_items(run_id, summary, state_for_run, progress, runs_dir / run_id)
        if rows:
            items.extend(rows)
            continue
        items.extend(state_run_items(run_id, state_for_run, progress))
    return items

def summary_run_items(
    run_id: str,
    summary: dict[str, Any],
    state_for_run: dict[str, Any],
    progress: dict[str, Any],
    run_dir: Path,
) -> list[dict[str, Any]]:
    suites = summary_suite_summaries(summary)
    if not suites:
        return []
    items = []
    status = str(state_for_run.get("status") or "done")
    fallback_model = str(summary.get("model") or state_for_run.get("model") or current_model_label())
    for suite_summary in suites:
        totals = summary_totals(suite_summary)
        total = totals["tasks"]
        done = int(progress.get("completed") or 0) if state_for_run else total
        suite = str(suite_summary.get("suite") or "mobilegym")
        item = {
            "run_id": run_id,
            "suite": suite,
            "status": status,
            "progress": f"{done}/{total}" if total else "",
            "model": str(suite_summary.get("model") or fallback_model),
            "totals": totals,
        }
        report_path = report_path_for(run_id, suite, run_dir)
        if report_path:
            item["report_path"] = report_path
        items.append(item)
    return items


def report_path_for(run_id: str, suite: str, run_dir: Path) -> str:
    if valid_suite_name(suite) and (run_dir / suite / "index.html").is_file():
        return "/benchmark/report/" + quote(run_id, safe="") + "/" + quote_suite_path(suite)
    if (run_dir / "index.html").is_file():
        return "/benchmark/report/" + quote(run_id, safe="")
    return ""


def quote_suite_path(suite: str) -> str:
    return "/".join(quote(part, safe="") for part in suite.split("/"))


def report_file_for(root: Path, report_id: str) -> Path | None:
    parts = [unquote(part) for part in report_id.split("/") if part]
    if not parts or not is_safe_run_id(parts[0]):
        return None
    run_dir = root / "runs" / "mobilegym" / parts[0]
    if len(parts) == 1:
        return run_dir / "index.html"
    artifact_names = {"index.html", "llm_analysis.md", "llm_analysis.json", "llm_analysis_error.txt"}
    if parts[-1] in artifact_names:
        if len(parts) == 2:
            return run_dir / parts[-1]
        suite = "/".join(parts[1:-1])
        if not valid_suite_name(suite):
            return None
        return run_dir / suite / parts[-1]
    suite = "/".join(parts[1:])
    if not valid_suite_name(suite):
        return None
    return run_dir / suite / "index.html"


def report_content_type(path: Path) -> str:
    return {
        ".html": "text/html; charset=utf-8",
        ".md": "text/markdown; charset=utf-8",
        ".json": "application/json; charset=utf-8",
        ".txt": "text/plain; charset=utf-8",
    }.get(path.suffix, "application/octet-stream")


def state_run_items(run_id: str, state_for_run: dict[str, Any], progress: dict[str, Any]) -> list[dict[str, Any]]:
    suites = state_suites(state_for_run) or ["mobilegym"]
    items = []
    totals = {
        "tasks": int(progress.get("total") or 0),
        "passed": 0,
        "failed": 0,
    }
    total = totals["tasks"]
    done = int(progress.get("completed") or 0) if state_for_run else total
    for suite in suites:
        items.append(
            {
                "run_id": run_id,
                "suite": suite,
                "status": str(state_for_run.get("status") or "done"),
                "progress": f"{done}/{total}" if total else "",
                "model": str(state_for_run.get("model") or current_model_label()),
                "totals": totals,
            }
        )
    return items


def summary_suite_summaries(summary: dict[str, Any]) -> list[dict[str, Any]]:
    suites = summary.get("suites")
    if isinstance(suites, list):
        rows = []
        for item in suites:
            if isinstance(item, dict) and item.get("suite"):
                row = dict(summary)
                row.update(item)
                rows.append(row)
        if rows:
            return rows
    suite = summary_suite(summary)
    if suite:
        row = dict(summary)
        row["suite"] = suite
        return [row]
    return []


def summary_totals(summary: dict[str, Any]) -> dict[str, int]:
    return {
        "tasks": int(summary.get("tasks") or 0),
        "passed": int(summary.get("passed") or 0),
        "failed": int(summary.get("failed") or 0)
        + int(summary.get("timeout") or 0)
        + int(summary.get("error") or 0)
        + int(summary.get("worker_failed") or 0)
        + int(summary.get("unknown") or 0),
    }


def state_suites(state: dict[str, Any]) -> list[str]:
    raw = state.get("suites")
    if isinstance(raw, list):
        return [str(item) for item in raw if str(item)]
    suite = str(state.get("suite") or "")
    return [part.strip() for part in suite.split(",") if part.strip()]


def mobilegym_progress(benchmark_root: Path, run_id: str) -> dict[str, Any]:
    if not is_safe_run_id(run_id):
        return {}
    run_dir = benchmark_root / "runs" / "mobilegym" / run_id
    if not run_dir.is_dir():
        return {}
    task_ids: list[str] = []
    completed_ids: set[str] = set()
    shard_dirs = sorted(path.parent for path in run_dir.glob("**/shard.json") if path.is_file())
    if not shard_dirs:
        return {}
    for shard_dir in shard_dirs:
        metadata = read_json_file(shard_dir / "shard.json")
        selected = [str(task_id) for task_id in metadata.get("selected_task_ids") or []]
        if selected:
            task_ids.extend(task_id for task_id in selected if task_id not in task_ids)
        elif int(metadata.get("selected_task_count") or 0) == 0:
            continue
        completed_ids.update(_completed_task_ids(shard_dir / "raw"))

    completed = sum(1 for task_id in task_ids if task_id in completed_ids)
    total = len(task_ids)
    payload: dict[str, Any] = {
        "total": total,
        "completed": completed,
        "current": min(total, completed + 1) if total and completed < total else completed,
        "progress": f"{completed}/{total}" if total else "",
    }
    current_task = _current_task_id(task_ids, completed_ids)
    if current_task:
        payload["current_task"] = current_task
    return payload


def _completed_task_ids(raw_dir: Path) -> set[str]:
    completed: set[str] = set()
    for name in ("results.jsonl", "errors.jsonl"):
        for path in raw_dir.glob(f"**/{name}"):
            for row in read_jsonl(path):
                task_id = row_task_id(row)
                if task_id:
                    completed.add(task_id)
    return completed


def _current_task_id(task_ids: list[str], completed_ids: set[str]) -> str:
    for task_id in task_ids:
        if task_id not in completed_ids:
            return task_id
    return ""


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return rows
    for line in lines:
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(row, dict):
            rows.append(row)
    return rows


def row_task_id(row: dict[str, Any]) -> str:
    for key in ("id", "task_id", "name"):
        if row.get(key):
            return str(row[key])
    return ""


def read_json_file(path: Path) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}
    return data if isinstance(data, dict) else {}


def summary_suite(summary: dict[str, Any]) -> str:
    suite = summary.get("suite")
    if isinstance(suite, str) and suite:
        return suite
    suites = summary.get("suites")
    if isinstance(suites, list):
        names = []
        for item in suites:
            if isinstance(item, dict) and item.get("suite"):
                names.append(str(item["suite"]))
        if names:
            return ", ".join(names)
    return ""


def current_model_label(env: dict[str, str] | None = None) -> str:
    env = env or os.environ
    return env.get("AICO_BENCHMARK_AGENT_MODEL") or "aico-go"


def model_config_from_payload(payload: dict[str, Any]) -> dict[str, str]:
    board_url = str(payload.get("board_url") or "").strip()
    if not board_url:
        return {}
    return fetch_board_model_config(board_url)


def benchmark_config_from_payload(payload: dict[str, Any]) -> dict[str, str]:
    board_url = str(payload.get("board_url") or "").strip()
    if not board_url:
        return {}
    return fetch_board_benchmark_config(board_url)


def apply_analysis_env_from_payload(env: dict[str, str], payload: dict[str, Any]) -> None:
    env["AICO_BENCHMARK_LLM_ANALYSIS"] = "1"
    for payload_key, env_key in {
        "analysis_model": "AICO_BENCHMARK_ANALYSIS_MODEL",
        "analysis_max_log_bytes": "AICO_BENCHMARK_ANALYSIS_MAX_LOG_BYTES",
        "analysis_max_code_bytes": "AICO_BENCHMARK_ANALYSIS_MAX_CODE_BYTES",
        "analysis_timeout_sec": "AICO_BENCHMARK_ANALYSIS_TIMEOUT_SEC",
    }.items():
        value = payload.get(payload_key)
        if value not in (None, ""):
            env[env_key] = str(value)
    if "AICO_BENCHMARK_ANALYSIS_MODEL" not in env and env.get("AICO_BENCHMARK_JUDGE_MODEL"):
        env["AICO_BENCHMARK_ANALYSIS_MODEL"] = env["AICO_BENCHMARK_JUDGE_MODEL"]


def fetch_board_model_config(board_url: str) -> dict[str, str]:
    model_values = parse_agent_model_values(fetch_board_toml_section(board_url, "model_settings.model"))
    provider_ref = model_values.get("provider", "")
    provider_values: dict[str, str] = {}
    if provider_ref:
        provider_values = parse_agent_model_provider_values(
            fetch_board_toml_section(board_url, f"model_settings.providers.{provider_ref}")
        )
    return legacy_model_environment(model_values, provider_values)


def fetch_board_benchmark_config(board_url: str) -> dict[str, str]:
    return parse_agent_benchmark_assignments(fetch_board_toml_section(board_url, "benchmark"))


def fetch_board_toml_section(board_url: str, section: str) -> str:
    base = board_url.rstrip("/")
    if not base.startswith(("http://", "https://")):
        raise LauncherError("invalid board_url")
    command = (
        "awk -v target=" + shlex.quote(section) + " "
        "'index($0, \"[\" target \"]\") == 1 {in_section=1; next} "
        "/^\\[/{in_section=0} in_section{print}' /userdata/agent/agent.toml"
    )
    body = json.dumps({"input": {"command": command, "timeout": 5}}).encode("utf-8")
    request = Request(
        base + "/api/tools/shell",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urlopen(request, timeout=8) as response:
            raw = response.read().decode("utf-8", errors="replace")
    except OSError as exc:
        raise LauncherError(f"failed to fetch board config: {exc}") from exc

    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        output = raw
    else:
        output = str(data.get("output") or data.get("Output") or raw) if isinstance(data, dict) else raw
    return output


def parse_agent_model_config(text: str) -> dict[str, str]:
    model_values = parse_agent_model_values(toml_section(text, "model_settings.model"))
    provider_ref = model_values.get("provider", "")
    provider_values = parse_agent_model_provider_values(
        toml_section(text, f"model_settings.providers.{provider_ref}")
    )
    return legacy_model_environment(model_values, provider_values)


def legacy_model_environment(
    model_values: dict[str, str], provider_values: dict[str, str]
) -> dict[str, str]:
    provider = provider_values.get("type") or provider_values.get("provider") or model_values.get("provider")
    api_key = model_values.get("api_key") or provider_values.get("api_key")
    env: dict[str, str] = {}
    if provider:
        env["MODEL_PROVIDER"] = provider
    if model := model_values.get("model"):
        env["MODEL_NAME"] = model
    if base_url := provider_values.get("base_url"):
        env["MODEL_BASE_URL"] = base_url
    if api_key:
        env["MODEL_API_KEY"] = api_key
    return env


def parse_agent_model_assignments(text: str) -> dict[str, str]:
    return agent_model_environment(parse_agent_model_values(text), {})


def parse_agent_model_values(text: str) -> dict[str, str]:
    return parse_toml_assignments(text, {"provider", "model", "api_key"})


def parse_agent_model_provider_values(text: str) -> dict[str, str]:
    return parse_toml_assignments(text, {"type", "provider", "base_url", "api_key"})


def agent_model_environment(
    model_values: dict[str, str], provider_values: dict[str, str]
) -> dict[str, str]:
    provider = provider_values.get("type") or provider_values.get("provider") or model_values.get("provider")
    api_key = model_values.get("api_key") or provider_values.get("api_key")
    env: dict[str, str] = {}
    if provider:
        env["AICO_BENCHMARK_AGENT_PROVIDER"] = provider
    if model := model_values.get("model"):
        env["AICO_BENCHMARK_AGENT_MODEL"] = model
    if base_url := provider_values.get("base_url"):
        env["AICO_BENCHMARK_AGENT_BASE_URL"] = base_url
    if api_key:
        env["AICO_BENCHMARK_AGENT_API_KEY"] = api_key
    return env


def parse_agent_benchmark_config(text: str) -> dict[str, str]:
    return parse_agent_benchmark_assignments(toml_section(text, "benchmark"))


def parse_agent_benchmark_assignments(text: str) -> dict[str, str]:
    values = parse_toml_assignments(text, {"api_key", "judge_model", "base_url"})
    env: dict[str, str] = {}
    if values.get("api_key"):
        env["AICO_BENCHMARK_JUDGE_API_KEY"] = values["api_key"]
    if values.get("judge_model"):
        env["AICO_BENCHMARK_JUDGE_MODEL"] = values["judge_model"]
    if values.get("base_url"):
        env["AICO_BENCHMARK_JUDGE_BASE_URL"] = values["base_url"]
    return env


def parse_toml_assignments(text: str, allowed_keys: set[str]) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().split(" #", 1)[0].strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "'"}:
            value = value[1:-1]
        if key in allowed_keys and value:
            values[key] = value
    return values


def toml_section(text: str, section: str) -> str:
    found = False
    in_section = False
    lines: list[str] = []
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if line.startswith("[") and line.endswith("]"):
            name = line.strip("[]").strip()
            if name == section:
                found = True
                in_section = True
                continue
            if in_section:
                break
            continue
        if in_section:
            lines.append(raw_line)
    return "\n".join(lines) if found else ""


def validate_model_environment(env: dict[str, str] | None = None) -> None:
    env = env or os.environ
    provider = env.get("AICO_BENCHMARK_AGENT_PROVIDER") or "openrouter"
    api_key = env.get("AICO_BENCHMARK_AGENT_API_KEY")
    if provider == "openrouter" and not api_key:
        if any(env.get(name) for name in ("MODEL_API_KEY", "OPENROUTER_API_KEY", "AICO_MODEL_API_KEY")):
            raise LauncherError(
                "MobileGym model config missing: set AICO_BENCHMARK_AGENT_API_KEY "
                "before starting the Mac MobileGym launcher"
            )
        raise LauncherError(
            "MobileGym model config missing: set MODEL_API_KEY or OPENROUTER_API_KEY "
            "(or AICO_BENCHMARK_AGENT_API_KEY) before starting the Mac MobileGym launcher"
        )


def make_handler(benchmark_root: str | Path = BENCHMARK_ROOT) -> type[BaseHTTPRequestHandler]:
    root = Path(benchmark_root)

    class LocalLauncherHandler(BaseHTTPRequestHandler):
        def do_OPTIONS(self) -> None:
            self.send_response(204)
            self.send_common_headers("text/plain; charset=utf-8")
            self.end_headers()

        def do_GET(self) -> None:
            path = urlparse(self.path).path
            if path == "/benchmark/suites":
                self.send_json(scan_suites(root))
            elif path == "/benchmark/status":
                self.send_json(current_status(root))
            elif path == "/benchmark/log":
                self.send_text(read_log())
            elif path == "/benchmark/runs":
                self.send_json(list_runs(root))
            elif path.startswith("/benchmark/report/"):
                self.send_report(path.removeprefix("/benchmark/report/"))
            else:
                self.send_error(404, "not found")

        def do_POST(self) -> None:
            path = urlparse(self.path).path
            if path != "/benchmark/run":
                self.send_error(404, "not found")
                return
            try:
                payload = self.read_json()
                self.send_json(start_run(root, payload))
            except LauncherError as exc:
                self.send_json({"error": str(exc)}, status=400)
            except Exception as exc:
                self.send_json({"error": str(exc)}, status=500)

        def read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length") or "0")
            if length == 0:
                return {}
            raw = self.rfile.read(length)
            data = json.loads(raw.decode("utf-8"))
            if not isinstance(data, dict):
                raise LauncherError("request body must be a JSON object")
            return data

        def send_json(self, payload: Any, status: int = 200) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_common_headers("application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def send_text(self, text: str, status: int = 200) -> None:
            body = text.encode("utf-8")
            self.send_response(status)
            self.send_common_headers("text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def send_report(self, report_id: str) -> None:
            report_path = report_file_for(root, report_id)
            if report_path is None:
                self.send_error(404, "not found")
                return
            try:
                body = report_path.read_bytes()
            except OSError:
                self.send_error(404, "not found")
                return
            self.send_response(200)
            self.send_common_headers(report_content_type(report_path))
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def send_common_headers(self, content_type: str) -> None:
            self.send_header("Content-Type", content_type)
            self.send_header("Access-Control-Allow-Origin", "*")
            self.send_header("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
            self.send_header("Access-Control-Allow-Headers", "Content-Type")
            self.send_header("Access-Control-Allow-Private-Network", "true")

        def log_message(self, fmt: str, *args: Any) -> None:
            print(f"{self.address_string()} - {fmt % args}", file=sys.stderr)

    return LocalLauncherHandler


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Mac-local MobileGym launcher for the board benchmark UI.")
    parser.add_argument("--benchmark-root", type=Path, default=BENCHMARK_ROOT)
    parser.add_argument("--host", default=HOST)
    parser.add_argument("--port", type=int, default=PORT)
    args = parser.parse_args(argv)

    server = ThreadingHTTPServer((args.host, args.port), make_handler(args.benchmark_root))
    print(f"MobileGym local launcher listening on http://{args.host}:{args.port}")
    print(f"Benchmark root: {args.benchmark_root}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nStopping MobileGym local launcher")
        return 130
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

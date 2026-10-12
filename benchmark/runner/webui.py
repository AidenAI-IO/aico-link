from __future__ import annotations

import argparse
import concurrent.futures
import dataclasses as dc
import hashlib
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.error
import urllib.request
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable, Mapping

from runner.agent_client import AgentClient
from runner.analysis import AnalysisResult, analyze_run, config_from_env
from runner.capture import DEFAULT_SCREENSHOT_TIMEOUT_SEC
from runner.environment_endpoint import EnvironmentEndpoint
from runner.html_report import generate_report_html
from runner.judge import DEFAULT_JUDGE_API_KEY_ENV, JudgeConfig
from runner.metrics import aggregate_rows
from runner.platform import (
    read_environment_health,
    resolve_environment_platform,
)
from runner.process import terminate_process_tree
from runner.preflight import (
    MOBILEGYM_PREFLIGHT_COMPLETE_ENV,
    preflight_mobilegym_environment,
)
from runner.reset import ResetError, call_environment_release
from runner.suite import load_suite

try:
    from benchmark.runner.environment import (
        EnvironmentManager,
        MobileGymEnvironment,
        ensure_mobilegym_image,
    )
    from benchmark.runner.adb_android_environment import (
        ADBAndroidEnvironment,
        ADBAndroidEnvironmentManager,
        DEFAULT_ADB_SERIAL,
    )
    from benchmark.runner.config import (
        AgentConfigManager,
        apply_benchmark_telemetry,
        apply_agent_toml_runtime_defaults,
        materialize_agent_config_credentials,
        missing_agent_config_error,
        render_agent_template,
        validate_agent_toml,
    )
except ImportError:
    from runner.environment import (
        EnvironmentManager,
        MobileGymEnvironment,
        ensure_mobilegym_image,
    )
    from runner.adb_android_environment import (
        ADBAndroidEnvironment,
        ADBAndroidEnvironmentManager,
        DEFAULT_ADB_SERIAL,
    )
    from runner.config import (
        AgentConfigManager,
        apply_benchmark_telemetry,
        apply_agent_toml_runtime_defaults,
        materialize_agent_config_credentials,
        missing_agent_config_error,
        render_agent_template,
        validate_agent_toml,
    )


REPO_ROOT = Path(__file__).resolve().parents[2]
BENCHMARK_ROOT = REPO_ROOT / "benchmark"
DEFAULT_SUITES_DIR = BENCHMARK_ROOT / "suites"
DEFAULT_RUNS_DIR = BENCHMARK_ROOT / "runs" / "webui"
DEFAULT_BASE_CONFIG_DIR = BENCHMARK_ROOT / "config"
DEFAULT_BUNDLED_SKILLS_DIR = REPO_ROOT / "src" / "agent" / "config" / "skills"
DEFAULT_DAEMON_IMAGE = "aiden-agent-daemon:local"
DEFAULT_MOBILEGYM_IMAGE = "aiden-mobilegym-simulator:py311"
BENCHMARK_DOCKER_DIR = BENCHMARK_ROOT / "docker"
MOBILEGYM_DOCKER_DIR = BENCHMARK_ROOT / "mobilegym" / "docker"
AGENT_DAEMON_COMPOSE_FILE = BENCHMARK_DOCKER_DIR / "docker-compose.agent-daemon.yml"
DEFAULT_DAEMON_READY_TIMEOUT_SEC = 90
DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC = 120
DEFAULT_MOBILEGYM_PARALLEL_ENVS = 5
DOCKER_DIAGNOSTIC_TIMEOUT_SEC = 5
DEFAULT_JUDGE_MODEL = JudgeConfig().model
DEFAULT_JUDGE_BASE_URL = JudgeConfig().base_url
WEBUI_SETTINGS_FILE = "webui-settings.json"
JOB_RECORD_FILE = "job.json"
LOG_TAIL_BYTES = 96 * 1024
TERMINAL_JOB_STATUSES = {"passed", "failed", "stopped", "canceled"}
TERMINAL_TASK_STATUSES = {
    "passed",
    "failed",
    "stopped",
    "canceled",
    "skipped",
    "timeout",
    "judge_error",
}
STOP_REQUESTED_JOB_STATUSES = {"stopping", "stopped", "canceled"}
JOB_REPORT_RUN_ID = "_job-report"
RUNTIME_CONFIG_DIR_NAMES = {"cache", "log", "memory", "sessions", "skill-state"}
MEMORY_CONFIG_FILE_NAMES = {"extraction.yaml"}


class JobStopped(RuntimeError):
    pass


class TaskStopped(RuntimeError):
    pass


@dc.dataclass(frozen=True)
class WebUIConfig:
    suites_dir: Path = DEFAULT_SUITES_DIR
    runs_dir: Path = DEFAULT_RUNS_DIR
    base_config_dir: Path = DEFAULT_BASE_CONFIG_DIR
    agent_config_path: Path | None = None
    daemon_image: str = DEFAULT_DAEMON_IMAGE
    mobilegym_image: str = DEFAULT_MOBILEGYM_IMAGE
    build_daemon_image: bool = True
    build_mobilegym_image: bool = True
    daemon_ready_timeout_sec: int = DEFAULT_DAEMON_READY_TIMEOUT_SEC
    mobilegym_ready_timeout_sec: int = DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC


@dc.dataclass
class TaskRecord:
    id: str
    suite: str
    task_id: str
    benchmark_task_id: str
    status: str = "queued"
    message: str = ""
    created_at: str = ""
    started_at: str = ""
    finished_at: str = ""
    agent_url: str = ""
    container_name: str = ""
    run_id: str = ""
    state_file: str = ""
    runner_log: str = ""
    daemon_log: str = ""
    screen_url: str = ""
    report_url: str = ""
    exit_code: int | None = None
    error: str = ""


@dc.dataclass
class Job:
    id: str
    endpoint: str
    docker_endpoint: str
    suites: list[str]
    environment_endpoint: str = ""
    environment_id: str = ""
    environment_name: str = ""
    environment_type: str = "device"
    environment_web_url: str = ""
    status: str = "queued"
    message: str = ""
    created_at: str = ""
    started_at: str = ""
    finished_at: str = ""
    agent_url: str = ""
    container_name: str = ""
    config_dir: str = ""
    raw_runs_dir: str = ""
    state_file: str = ""
    runner_log: str = ""
    daemon_log: str = ""
    report_url: str = ""
    suite_results: list[dict[str, Any]] = dc.field(default_factory=list)
    task_records: list[TaskRecord] = dc.field(default_factory=list)
    no_judge: bool = False
    judge_model: str = DEFAULT_JUDGE_MODEL
    judge_base_url: str = DEFAULT_JUDGE_BASE_URL
    judge_api_key_set: bool = False
    repeats: int | None = None
    parallel_tasks: int = 1
    target_platform: str = ""


class BenchmarkWebApp:
    def __init__(self, config: WebUIConfig):
        self.config = config
        self.config.runs_dir.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._jobs: dict[str, Job] = {}
        self._webui_judge_api_key = ""
        self._job_judge_api_keys: dict[str, str] = {}
        self._job_runner_procs: dict[str, Any] = {}
        self._job_daemon_jobs: dict[str, list[Job]] = {}
        self._task_daemon_jobs: dict[str, list[Job]] = {}
        self.env_manager = EnvironmentManager(
            runs_dir=config.runs_dir,
            mobilegym_image=config.mobilegym_image,
            build_mobilegym_image=config.build_mobilegym_image,
            ready_timeout_sec=config.mobilegym_ready_timeout_sec,
            repo_root=REPO_ROOT,
        )
        self.adb_env_manager = ADBAndroidEnvironmentManager(runs_dir=config.runs_dir)
        self.config_manager = AgentConfigManager(
            base_config_dir=config.base_config_dir,
            config_path=config.agent_config_path or (config.runs_dir / "agent.toml"),
        )
        self._migrate_persisted_webui_secret()
        self._load_persisted_jobs()

    def list_suites(self) -> list[dict[str, Any]]:
        return list_benchmark_suites(self.config.suites_dir)

    def get_suite_detail(self, suite_key: str) -> dict[str, Any] | None:
        try:
            suite_path = resolve_suite_path(self.config.suites_dir, suite_key)
            suite = load_suite(suite_path)
            tasks = []
            for task in suite.tasks:
                rubric = [{"id": item.id, "check": item.check} for item in task.rubric]
                hard_assertions = {
                    "min_tool_calls": task.hard_assertions.min_tool_calls,
                    "max_tool_calls": task.hard_assertions.max_tool_calls,
                    "must_complete_within_sec": task.hard_assertions.must_complete_within_sec,
                    "response_required": task.hard_assertions.response_required,
                    "required_tools": task.hard_assertions.required_tools,
                    "forbidden_tools": task.hard_assertions.forbidden_tools,
                    "prohibited_actions": task.hard_assertions.prohibited_actions,
                }
                tasks.append({
                    "id": task.id,
                    "category": task.category,
                    "description_for_judge": task.description_for_judge,
                    "prompt": task.prompt,
                    "rubric": rubric,
                    "hard_assertions": hard_assertions,
                    "setup": task.setup,
                    "repeats": task.repeats,
                    "input_screenshot": task.input_screenshot,
                    "expected_answer": task.expected_answer,
                    "answer_format": task.answer_format,
                    "expected_recalled_memory_ids": task.expected_recalled_memory_ids,
                })
            return {
                "name": suite.name,
                "prompt_prefix": suite.prompt_prefix,
                "tasks": tasks,
            }
        except Exception:
            return None

    def list_jobs(self) -> list[dict[str, Any]]:
        with self._lock:
            jobs = [self._job_payload(job) for job in self._jobs.values()]
        jobs.sort(key=lambda item: item.get("created_at", ""), reverse=True)
        return jobs

    def get_agent_config(self) -> dict[str, Any]:
        content, source = self.config_manager.get_config()
        return {
            "content": content,
            "path": str(self.config_manager.config_path),
            "source": source,
        }

    def save_agent_config(self, payload: dict[str, Any]) -> dict[str, Any]:
        content = str(payload.get("content") or "")
        content, source = self.config_manager.save_config(content)
        return {
            "content": content,
            "path": str(self.config_manager.config_path),
            "source": source,
        }

    def reset_agent_config(self) -> dict[str, Any]:
        content, source = self.config_manager.reset_config()
        return {
            "content": content,
            "path": str(self.config_manager.config_path),
            "source": source,
        }

    def get_webui_settings(self) -> dict[str, Any]:
        return sanitize_webui_settings(self._load_webui_settings(include_secrets=False))

    def save_webui_settings(self, payload: dict[str, Any]) -> dict[str, Any]:
        current = self._load_webui_settings(include_secrets=True)
        incoming_judge = payload.get("judge") if isinstance(payload.get("judge"), dict) else {}
        current_judge = current.setdefault("judge", {})
        if "enabled" in incoming_judge:
            current_judge["enabled"] = bool(incoming_judge.get("enabled"))
        if "model" in incoming_judge:
            model = str(incoming_judge.get("model") or "").strip() or DEFAULT_JUDGE_MODEL
            current_judge["model"] = model
        if "base_url" in incoming_judge:
            base_url = str(incoming_judge.get("base_url") or "").strip() or DEFAULT_JUDGE_BASE_URL
            current_judge["base_url"] = base_url
        if "api_key" in incoming_judge:
            api_key = str(incoming_judge.get("api_key") or "").strip()
            if api_key:
                with self._lock:
                    self._webui_judge_api_key = api_key
                current_judge["has_api_key"] = True

        # Global metrics_k setting
        if "metrics_k" in payload:
            try:
                k = int(payload["metrics_k"])
                k = max(1, min(k, 10))  # Clamp to [1, 10]
                current["metrics_k"] = k
            except (ValueError, TypeError):
                pass  # Keep existing value

        if "device_environments" in payload:
            current["device_environments"] = normalize_device_environments(payload.get("device_environments"))
        if "selected_environment_id" in payload:
            current["selected_environment_id"] = str(payload.get("selected_environment_id") or "")

        normalized = normalize_webui_settings(current, include_secrets=True)
        with self._lock:
            normalized["judge"]["api_key"] = self._webui_judge_api_key
        sanitized = self._settings_without_secrets(normalized)
        write_json_atomic(self._webui_settings_path(), sanitized)
        return sanitized

    def get_job(self, job_id: str) -> dict[str, Any] | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            return self._job_payload(job)

    def read_job_log(self, job_id: str) -> str | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            paths = [Path(job.runner_log), Path(job.daemon_log)]
        parts = []
        for title, path in (("runner", paths[0]), ("daemon", paths[1])):
            parts.append(f"== {title} ==")
            parts.append(tail_text(path, LOG_TAIL_BYTES))
        return "\n".join(parts)

    def read_task_log(self, job_id: str, task_record_id: str) -> str | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            record = next((item for item in job.task_records if item.id == task_record_id), None)
            if record is None:
                return None
            paths = [Path(record.runner_log), Path(record.daemon_log)]
        parts = []
        for title, path in (("runner", paths[0]), ("daemon", paths[1])):
            parts.append(f"== {title} ==")
            parts.append(tail_text(path, LOG_TAIL_BYTES))
        return "\n".join(parts)

    def task_screen_payload(self, job_id: str, task_record_id: str) -> dict[str, Any] | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            record = next((item for item in job.task_records if item.id == task_record_id), None)
            if record is None:
                return None
            endpoint = job.environment_endpoint
            task_id = record.benchmark_task_id
        if not endpoint:
            return {"ok": False, "error": {"code": "missing_environment", "message": "job has no environment endpoint"}}
        try:
            return read_environment_bridge_screen(endpoint, task_id)
        except Exception as exc:
            return {"ok": False, "error": {"code": "screen_request_failed", "message": str(exc)}}

    def list_mobilegym_environments(self) -> list[dict[str, Any]]:
        environments = [
            self._mobilegym_environment_payload(env)
            for env in self.env_manager.list_all()
        ]
        environments.sort(key=lambda item: item.get("created_at", ""), reverse=True)
        return environments

    def start_mobilegym_environment(self, payload: dict[str, Any]) -> dict[str, Any]:
        name = str(payload.get("name") or "").strip() or "MobileGym"
        parallel_envs = parse_positive_int(
            payload.get("parallel_envs"),
            default=DEFAULT_MOBILEGYM_PARALLEL_ENVS,
            field="parallel_envs",
        )
        env = self.env_manager.start_mobilegym(name=name, parallel_envs=parallel_envs)
        return self._mobilegym_environment_payload(env)

    def stop_mobilegym_environment(self, environment_id: str) -> dict[str, Any] | None:
        env = self.env_manager.stop(environment_id)
        if env is None:
            return None
        return self._mobilegym_environment_payload(env)

    def delete_mobilegym_environment(self, environment_id: str) -> dict[str, Any] | None:
        env = self.env_manager.delete(environment_id)
        if env is None:
            return None
        return self._mobilegym_environment_payload(env)

    def list_adb_android_environments(self) -> list[dict[str, Any]]:
        environments = [
            self._adb_android_environment_payload(env)
            for env in self.adb_env_manager.list_all()
        ]
        environments.sort(key=lambda item: item.get("created_at", ""), reverse=True)
        return environments

    def start_adb_android_environment(self, payload: dict[str, Any]) -> dict[str, Any]:
        serial = str(payload.get("serial") or "").strip() or DEFAULT_ADB_SERIAL
        name = str(payload.get("name") or "").strip() or f"ADB Android ({serial})"
        raw_port = payload.get("bridge_port")
        # 0 is the same "auto-pick a free port" sentinel the CLI uses
        # (--bridge-port 0); only positive values need validation.
        if raw_port in (None, "", 0, "0"):
            bridge_port = 0
        else:
            bridge_port = parse_positive_int(raw_port, default=0, field="bridge_port")
        env = self.adb_env_manager.start_adb_android(name=name, serial=serial, bridge_port=bridge_port)
        return self._adb_android_environment_payload(env)

    def stop_adb_android_environment(self, environment_id: str) -> dict[str, Any] | None:
        env = self.adb_env_manager.stop(environment_id)
        if env is None:
            return None
        return self._adb_android_environment_payload(env)

    def delete_adb_android_environment(self, environment_id: str) -> dict[str, Any] | None:
        env = self.adb_env_manager.delete(environment_id)
        if env is None:
            return None
        return self._adb_android_environment_payload(env)

    def shutdown(self) -> None:
        with self._lock:
            job_ids = list(self._jobs)
        environment_ids = [env.id for env in self.env_manager.list_all()]
        adb_environment_ids = [env.id for env in self.adb_env_manager.list_all()]
        for job_id in job_ids:
            self.stop_job(job_id)
        for environment_id in environment_ids:
            self.stop_mobilegym_environment(environment_id)
        for environment_id in adb_environment_ids:
            self.stop_adb_android_environment(environment_id)

    def start_job(self, payload: dict[str, Any]) -> dict[str, Any]:
        suite_keys = payload.get("suites") or []
        if not isinstance(suite_keys, list) or not suite_keys:
            raise ValueError("at least one suite is required")
        suite_keys = [str(item) for item in suite_keys]
        suite_paths = [
            resolve_suite_path(self.config.suites_dir, key) for key in suite_keys
        ]
        mock_suite_flags = [suite_uses_mock_environment(path) for path in suite_paths]
        if any(mock_suite_flags) and not all(mock_suite_flags):
            raise ValueError(
                "mock environment suites and external environment suites must run in separate jobs"
            )

        repeats = payload.get("repeats")
        repeats_value = None
        if repeats not in (None, ""):
            repeats_value = int(repeats)
            if repeats_value <= 0:
                raise ValueError("repeats must be positive")

        environment_payload = payload.get("environment") if isinstance(payload.get("environment"), dict) else {}
        requested_environment_type = str(
            payload.get("environment_type") or environment_payload.get("type") or ""
        ).strip()
        if all(mock_suite_flags):
            if requested_environment_type and requested_environment_type != "mock":
                raise ValueError("mock environment suites must use environment_type=mock")
            environment_type = "mock"
        else:
            environment_type = requested_environment_type or "device"
            if environment_type == "mock":
                raise ValueError("environment_type=mock requires mock environment suites")
        if environment_type not in {"device", "mobilegym", "adb_android", "mock"}:
            raise ValueError(
                "environment_type must be device, mobilegym, adb_android, or mock"
            )

        endpoint = str(payload.get("endpoint") or "").strip()
        normalized_requested_endpoint = ""
        if environment_type != "mock" and endpoint:
            try:
                normalized_requested_endpoint = EnvironmentEndpoint(endpoint).base
            except ValueError as exc:
                raise ValueError("endpoint must be an http(s) base URL") from exc

        settings = self._load_webui_settings(include_secrets=True)
        judge_settings = settings.get("judge") if isinstance(settings.get("judge"), dict) else {}

        # Use WebUI settings default metrics_k if repeats not specified
        if repeats_value is None:
            repeats_value = int(settings.get("metrics_k", 1))
            if repeats_value <= 0:
                repeats_value = 1

        no_judge = bool(payload.get("no_judge")) if "no_judge" in payload else not bool(judge_settings.get("enabled", True))
        judge_model = (
            str(payload.get("judge_model") or "").strip()
            or str(judge_settings.get("model") or "").strip()
            or DEFAULT_JUDGE_MODEL
        )
        judge_base_url = (
            str(payload.get("judge_base_url") or "").strip()
            or str(judge_settings.get("base_url") or "").strip()
            or DEFAULT_JUDGE_BASE_URL
        )
        judge_api_key = (
            str(payload.get("judge_api_key") or "").strip()
            or str(judge_settings.get("api_key") or "").strip()
        )

        environment_id = str(payload.get("environment_id") or environment_payload.get("id") or "")
        environment_name = str(payload.get("environment_name") or environment_payload.get("name") or "")
        environment_endpoint = str(payload.get("environment_endpoint") or "").strip()
        environment_web_url = str(payload.get("environment_web_url") or environment_payload.get("web_url") or "").strip()
        parallel_tasks = parse_positive_int(payload.get("parallel_tasks"), default=1, field="parallel_tasks")
        if environment_type == "mock":
            environment_id = "mock-aiden-app"
            environment_name = "Mock Aiden App environment"
            environment_endpoint = ""
            environment_web_url = ""
            parallel_tasks = 1
        if environment_type == "device" and not environment_endpoint:
            environment_endpoint = endpoint.rstrip("/")
        if environment_type == "mobilegym":
            mobilegym_env = self.env_manager.get(environment_id)
            if mobilegym_env is not None:
                if mobilegym_env.status != "running":
                    raise ValueError(
                        "selected MobileGym environment is not fresh; remove it "
                        "and start a fresh MobileGym environment"
                    )
                environment_endpoint = mobilegym_env.public_endpoint.rstrip("/")
                environment_web_url = mobilegym_env.web_url
                parallel_tasks = mobilegym_env.parallel_envs
            elif not environment_endpoint:
                public_endpoint = str(environment_payload.get("public_endpoint") or "").strip()
                if public_endpoint:
                    environment_endpoint = public_endpoint.rstrip("/")
            if mobilegym_env is None and isinstance(environment_payload, dict) and environment_payload.get("parallel_envs") not in (None, ""):
                parallel_tasks = parse_positive_int(
                    environment_payload.get("parallel_envs"),
                    default=DEFAULT_MOBILEGYM_PARALLEL_ENVS,
                    field="parallel_envs",
                )
            if environment_endpoint:
                bridge_concurrency = read_environment_bridge_concurrency(environment_endpoint)
                if bridge_concurrency is not None:
                    parallel_tasks = bridge_concurrency
        if environment_type == "adb_android":
            adb_env = self.adb_env_manager.get(environment_id)
            if adb_env is not None:
                environment_endpoint = adb_env.public_endpoint.rstrip("/")
            elif not environment_endpoint:
                public_endpoint = str(environment_payload.get("public_endpoint") or "").strip()
                if public_endpoint:
                    environment_endpoint = public_endpoint.rstrip("/")
            environment_web_url = ""
            # Single adb device: never run tasks in parallel.
            parallel_tasks = 1

        if environment_type == "mock":
            endpoint = ""
            docker_endpoint = ""
        else:
            if not environment_endpoint:
                raise ValueError("resolved environment endpoint is required")
            try:
                environment_endpoint = EnvironmentEndpoint(environment_endpoint).base
            except ValueError as exc:
                raise ValueError("resolved environment endpoint must be an http(s) base URL") from exc
            docker_endpoint = endpoint_for_docker(environment_endpoint)
            if normalized_requested_endpoint and normalized_requested_endpoint not in {
                environment_endpoint,
                docker_endpoint,
            }:
                raise ValueError("endpoint does not match resolved environment endpoint")
            endpoint = environment_endpoint

        job_id = new_job_id()
        job_dir = self.config.runs_dir / job_id
        raw_runs_dir = job_dir / "raw"
        job_dir.mkdir(parents=True, exist_ok=True)
        raw_runs_dir.mkdir(parents=True, exist_ok=True)
        port = reserve_free_port()
        now = now_iso()

        job = Job(
            id=job_id,
            endpoint=endpoint,
            docker_endpoint=docker_endpoint,
            suites=suite_keys,
            environment_endpoint=environment_endpoint,
            environment_id=environment_id,
            environment_name=environment_name,
            environment_type=environment_type,
            environment_web_url=environment_web_url,
            status="queued",
            created_at=now,
            agent_url="" if environment_type == "mock" else f"http://127.0.0.1:{port}",
            container_name=f"aiden-benchmark-agent-{job_id}",
            config_dir=str(job_dir / "config"),
            raw_runs_dir=str(raw_runs_dir),
            state_file=str(job_dir / "state.json"),
            runner_log=str(job_dir / "runner.log"),
            daemon_log=str(job_dir / "daemon.log"),
            no_judge=no_judge,
            judge_model=judge_model,
            judge_base_url=judge_base_url,
            judge_api_key_set=bool(judge_api_key) and not no_judge,
            repeats=repeats_value,
            parallel_tasks=parallel_tasks,
        )
        with self._lock:
            self._jobs[job.id] = job
            if judge_api_key and not no_judge:
                self._job_judge_api_keys[job.id] = judge_api_key
        self._persist_job(job)
        thread = threading.Thread(target=self._run_job, args=(job,), name=f"benchmark-{job.id}", daemon=True)
        thread.start()
        return self._job_payload(job)

    def cancel_job(self, job_id: str) -> dict[str, Any] | None:
        return self.stop_job(job_id)

    def stop_job(self, job_id: str) -> dict[str, Any] | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            if job.status in TERMINAL_JOB_STATUSES:
                return self._job_payload(job)
            job.status = "stopping"
            job.message = "stop requested"
            procs = runner_procs_for_stop(self._job_runner_procs.get(job.id))
            daemon_jobs = [job, *self._job_daemon_jobs.get(job.id, [])]
            runner_log = Path(job.runner_log) if job.runner_log else None
            state_file = Path(job.state_file) if job.state_file else None
        if runner_log is not None:
            append_log(runner_log, "STOP requested")
        if state_file is not None:
            update_state_status(state_file, "stopping", run_id=job.id)
        self._persist_job(job)
        for proc in procs:
            terminate_process_tree(proc)
        for daemon_job in daemon_jobs:
            stop_daemon_compose(daemon_job)
        return self._job_payload(job)

    def stop_task_worker(self, job_id: str, task_record_id: str) -> dict[str, Any] | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            record = next((item for item in job.task_records if item.id == task_record_id), None)
            if record is None:
                return None
            if record.status in TERMINAL_TASK_STATUSES:
                return self._job_payload(job)
            record.status = "stopping"
            record.message = "stop requested"
            task_key = task_worker_key(job.id, record.id)
            procs = runner_procs_for_stop(self._job_runner_procs.get(task_key))
            daemon_jobs = list(self._task_daemon_jobs.get(task_key, []))
            runner_log = Path(record.runner_log) if record.runner_log else None
            state_file = Path(record.state_file) if record.state_file else None
        if runner_log is not None:
            append_log(runner_log, "STOP requested")
        if state_file is not None:
            update_state_status(state_file, "stopping", run_id=record.run_id or job.id)
        self._persist_job(job)
        for proc in procs:
            terminate_process_tree(proc)
        for daemon_job in daemon_jobs:
            stop_daemon_compose(daemon_job)
        return self._job_payload(job)

    def _set_job(self, job: Job, **updates: Any) -> None:
        with self._lock:
            if job.status in STOP_REQUESTED_JOB_STATUSES and updates.get("status") not in STOP_REQUESTED_JOB_STATUSES:
                updates.pop("status", None)
                updates.pop("message", None)
            for key, value in updates.items():
                setattr(job, key, value)
        self._persist_job(job)

    def _job_stop_requested(self, job: Job) -> bool:
        with self._lock:
            return job.status in STOP_REQUESTED_JOB_STATUSES

    def _raise_if_job_stop_requested(self, job: Job) -> None:
        if self._job_stop_requested(job):
            raise JobStopped("job stop requested")

    def _task_stop_requested(self, job: Job, record_id: str) -> bool:
        with self._lock:
            record = next((item for item in job.task_records if item.id == record_id), None)
            return bool(record and record.status in STOP_REQUESTED_JOB_STATUSES)

    def _raise_if_task_worker_stop_requested(self, job: Job, record_id: str) -> None:
        self._raise_if_job_stop_requested(job)
        if self._task_stop_requested(job, record_id):
            raise TaskStopped("task stop requested")

    def _finish_stopped_job(self, job: Job) -> None:
        if job.state_file:
            update_state_status(Path(job.state_file), "stopped", run_id=job.id)
        self._set_job(job, status="stopped", finished_at=now_iso(), message="")

    def _agent_config_path(self) -> Path:
        return self.config.agent_config_path or (self.config.runs_dir / "agent.toml")

    def _webui_settings_path(self) -> Path:
        return self.config.runs_dir / WEBUI_SETTINGS_FILE

    def _settings_without_secrets(self, settings: dict[str, Any]) -> dict[str, Any]:
        sanitized = sanitize_webui_settings(settings)
        with self._lock:
            has_api_key = bool(self._webui_judge_api_key)
        if has_api_key:
            sanitized["judge"]["has_api_key"] = True
        return sanitized

    def _migrate_persisted_webui_secret(self) -> None:
        path = self._webui_settings_path()
        if not path.exists():
            return
        settings = load_webui_settings(path, include_secrets=True)
        judge = settings.get("judge") if isinstance(settings.get("judge"), dict) else {}
        api_key = str(judge.get("api_key") or "").strip()
        if not api_key:
            return
        self._webui_judge_api_key = api_key
        write_json_atomic(path, self._settings_without_secrets(settings))

    def _load_persisted_jobs(self) -> None:
        restored: dict[str, Job] = {}
        for path in sorted(self.config.runs_dir.glob(f"*/{JOB_RECORD_FILE}")):
            job = load_job_record(path)
            if job is None:
                continue
            if job.status not in TERMINAL_JOB_STATUSES:
                job.status = "stopped"
                job.message = "restored after WebUI restart"
                if not job.finished_at:
                    job.finished_at = now_iso()
                for record in job.task_records:
                    if record.status not in TERMINAL_TASK_STATUSES:
                        record.status = "stopped"
                        record.message = "restored after WebUI restart"
                        if not record.finished_at:
                            record.finished_at = job.finished_at
                if job.state_file:
                    update_state_status(Path(job.state_file), "stopped", run_id=job.id)
                self._persist_job(job)
            restored[job.id] = job
        with self._lock:
            self._jobs.update(restored)

    def _persist_job(self, job: Job) -> None:
        with self._lock:
            persist_job_record(job)

    def _load_webui_settings(self, include_secrets: bool = False) -> dict[str, Any]:
        settings = load_webui_settings(self._webui_settings_path(), include_secrets=False)
        with self._lock:
            api_key = self._webui_judge_api_key
        if include_secrets:
            settings = normalize_webui_settings(settings, include_secrets=True)
            settings["judge"]["api_key"] = api_key
        elif api_key:
            settings["judge"]["has_api_key"] = True
        return settings

    def _job_payload(self, job: Job) -> dict[str, Any]:
        with self._lock:
            payload = dc.asdict(job)
        payload["progress"] = read_json_file(Path(job.state_file))
        payload["suite_results"] = list(payload.get("suite_results") or [])
        payload["task_records"] = list(payload.get("task_records") or [])
        payload["totals"] = aggregate_totals(payload["suite_results"])
        return payload

    def _mobilegym_environment_payload(self, env: MobileGymEnvironment) -> dict[str, Any]:
        payload = dc.asdict(env)
        payload["log_tail"] = tail_text(Path(env.log_path), LOG_TAIL_BYTES)
        return payload

    def _adb_android_environment_payload(self, env: ADBAndroidEnvironment) -> dict[str, Any]:
        return self.adb_env_manager.environment_payload(env)

    def _new_task_record(self, job: Job, suite_key: str, task_id: str) -> TaskRecord:
        token = worker_token(suite_key, task_id)
        benchmark_task_id = f"{suite_key}:{task_id}"
        workers_dir = Path(job.raw_runs_dir).parent / "workers"
        workers_dir.mkdir(parents=True, exist_ok=True)
        return TaskRecord(
            id=token,
            suite=suite_key,
            task_id=task_id,
            benchmark_task_id=benchmark_task_id,
            status="queued",
            created_at=now_iso(),
            state_file=str(workers_dir / f"{token}.state.json"),
            runner_log=str(workers_dir / f"{token}.runner.log"),
            daemon_log=str(workers_dir / f"{token}.daemon.log"),
            screen_url=webui_task_screen_url(job.id, token),
        )

    def _ensure_task_record(self, job: Job, suite_key: str, task_id: str) -> TaskRecord:
        token = worker_token(suite_key, task_id)
        with self._lock:
            existing = next((item for item in job.task_records if item.id == token), None)
            if existing is not None:
                return existing
        record = self._new_task_record(job, suite_key, task_id)
        with self._lock:
            existing = next((item for item in job.task_records if item.id == token), None)
            if existing is not None:
                return existing
            job.task_records.append(record)
        self._persist_job(job)
        return record

    def _set_task_record(self, job: Job, record_id: str, **updates: Any) -> None:
        with self._lock:
            record = next((item for item in job.task_records if item.id == record_id), None)
            if record is None:
                return
            if record.status in STOP_REQUESTED_JOB_STATUSES and updates.get("status") not in STOP_REQUESTED_JOB_STATUSES:
                updates.pop("status", None)
                updates.pop("message", None)
            for key, value in updates.items():
                if hasattr(record, key):
                    setattr(record, key, value)
        self._persist_job(job)

    def _run_job(self, job: Job) -> None:
        host_port = int(urllib.parse.urlparse(job.agent_url).port or 0)
        try:
            self._raise_if_job_stop_requested(job)
            self._set_job(job, status="preparing", started_at=now_iso(), message="preparing config")
            agent_config_text = self.get_agent_config()["content"]
            if job.environment_type != "mock":
                health = read_environment_health(job.environment_endpoint or job.endpoint)
                resolution = resolve_environment_platform(health)
                preflight_mobilegym_environment(
                    job.environment_endpoint or job.endpoint,
                    health=health,
                )
                self._set_job(
                    job,
                    target_platform=resolution.value,
                )
            prepare_run_config(
                self.config.base_config_dir,
                Path(job.config_dir),
                agent_config_text=agent_config_text,
            )
            self._raise_if_job_stop_requested(job)
            if job.environment_type == "mock":
                self._set_job(
                    job,
                    status="running",
                    message="running mock environment suites",
                )
                for suite_key in job.suites:
                    self._raise_if_job_stop_requested(job)
                    self._run_mock_suite(job, suite_key)
                self._set_job(
                    job,
                    target_platform=_mock_job_target_platform(job.suite_results),
                )
                self._raise_if_job_stop_requested(job)
                self._refresh_job_report(job)
                final_status = (
                    "passed"
                    if job.suite_results
                    and all(item.get("exit_code") == 0 for item in job.suite_results)
                    else "failed"
                )
                self._set_job(
                    job,
                    status=final_status,
                    finished_at=now_iso(),
                    message="",
                )
                return
            self._set_job(job, status="starting_agent", message="starting docker agent")
            ensure_daemon_image(
                self.config.daemon_image,
                self.config.build_daemon_image,
                Path(job.runner_log),
                stop_requested=lambda: self._job_stop_requested(job),
            )
            self._raise_if_job_stop_requested(job)
            if self._uses_mobilegym_task_workers(job):
                self._set_job(job, status="running", message="running suites")
                for suite_key in job.suites:
                    self._raise_if_job_stop_requested(job)
                    self._run_mobilegym_suite_parallel(job, suite_key)
                self._raise_if_job_stop_requested(job)
                self._refresh_job_report(job)
                final_status = "passed" if job.suite_results and all(item.get("exit_code") == 0 for item in job.suite_results) else "failed"
                self._set_job(job, status=final_status, finished_at=now_iso(), message="")
                return
            container_id = start_daemon_compose(
                job,
                image=self.config.daemon_image,
                host_port=host_port,
                config_dir=Path(job.config_dir),
                environment_bridge_endpoint=job.docker_endpoint,
                benchmark_task_id=job_benchmark_task_id(job.id),
                device_type=job.target_platform,
                log_path=Path(job.runner_log),
                stop_requested=lambda: self._job_stop_requested(job),
            )
            append_log(Path(job.runner_log), f"container {container_id}")
            log_proc = start_daemon_logs(job, Path(job.daemon_log))
            try:
                self._wait_for_daemon(job)
                self._raise_if_job_stop_requested(job)
                self._set_job(job, status="running", message="running suites")
                for suite_key in job.suites:
                    self._raise_if_job_stop_requested(job)
                    self._run_suite(job, suite_key)
            finally:
                stop_daemon_compose(job)
                if log_proc is not None:
                    log_proc.terminate()
                self._release_job_environment(job)
            self._raise_if_job_stop_requested(job)
            self._refresh_job_report(job)
            final_status = "passed" if job.suite_results and all(item.get("exit_code") == 0 for item in job.suite_results) else "failed"
            self._set_job(job, status=final_status, finished_at=now_iso(), message="")
        except JobStopped:
            append_log(Path(job.runner_log), "STOPPED")
            stop_daemon_compose(job)
            self._finish_stopped_job(job)
        except Exception as exc:
            append_log(Path(job.runner_log), f"ERROR: {exc}")
            stop_daemon_compose(job)
            if self._job_stop_requested(job):
                self._finish_stopped_job(job)
            else:
                self._set_job(job, status="failed", finished_at=now_iso(), message=str(exc))
        finally:
            with self._lock:
                self._job_judge_api_keys.pop(job.id, None)
                self._job_daemon_jobs.pop(job.id, None)
                self._job_runner_procs.pop(job.id, None)
                for key in list(self._task_daemon_jobs):
                    if key.startswith(f"{job.id}:"):
                        self._task_daemon_jobs.pop(key, None)
                for key in list(self._job_runner_procs):
                    if key.startswith(f"{job.id}:"):
                        self._job_runner_procs.pop(key, None)

    def _release_job_environment(self, job: Job) -> None:
        """Drop the job's environment ownership after its daemon is gone.

        The runner releases each task's route itself, but a job that is stopped
        or crashes mid-task never gets there, and the job-level route id is
        never reused — so a leaked lease would answer every later job with
        429 until the bridge is restarted.
        """
        if not job.environment_endpoint:
            return
        try:
            call_environment_release(
                job.environment_endpoint,
                task_id=job_benchmark_task_id(job.id),
            )
        except Exception as exc:
            append_log(Path(job.runner_log), f"warning: failed to release environment: {exc}")

    def _refresh_job_report(self, job: Job) -> None:
        try:
            with self._lock:
                analysis_api_key = self._job_judge_api_keys.get(job.id, "") or self._webui_judge_api_key
            report_url = write_job_report(job, analysis_api_key=analysis_api_key)
        except Exception as exc:
            append_log(Path(job.runner_log), f"warning: failed to write job report: {exc}")
            return
        if not report_url:
            report_url = single_suite_report_url(job.suite_results)
        if report_url:
            self._set_job(job, report_url=report_url)

    def _wait_for_daemon(
        self,
        job: Job,
        *,
        stop_job: Job | None = None,
        stop_check: Callable[[], None] | None = None,
    ) -> None:
        client = AgentClient(job.agent_url)
        deadline = time.monotonic() + max(1, self.config.daemon_ready_timeout_sec)
        control_job = stop_job or job
        while time.monotonic() < deadline:
            if stop_check is not None:
                stop_check()
            else:
                self._raise_if_job_stop_requested(control_job)
            if client.health():
                return
            time.sleep(1)
        raise RuntimeError(f"agent daemon did not become ready at {job.agent_url}")

    def _uses_mobilegym_task_workers(self, job: Job) -> bool:
        return job.environment_type == "mobilegym" and job.parallel_tasks > 1 and bool(job.environment_endpoint)

    def _run_mobilegym_suite_parallel(self, job: Job, suite_key: str) -> None:
        self._raise_if_job_stop_requested(job)
        suite_path = resolve_suite_path(self.config.suites_dir, suite_key)

        suite = load_suite(suite_path)
        task_counts = {
            task.id: max(1, job.repeats if job.repeats is not None else task.repeats)
            for task in suite.tasks
        }
        total_runs = sum(task_counts.values())
        write_state(
            Path(job.state_file),
            {
                "status": "running",
                "suite": suite_key,
                "run_id": job.id,
                "total": total_runs,
                "completed": 0,
                "current": 0,
                "parallel": job.parallel_tasks,
            },
        )
        if not suite.tasks:
            with self._lock:
                job.suite_results.append({"suite": suite_key, "exit_code": 0, "run_id": ""})
            self._persist_job(job)
            update_state_status(Path(job.state_file), "done", run_id=job.id)
            return

        for task in suite.tasks:
            self._ensure_task_record(job, suite_key, task.id)

        completed = 0
        max_workers = min(max(1, job.parallel_tasks), len(suite.tasks))
        with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix=f"bench-{job.id}") as executor:
            future_to_task = {
                executor.submit(self._run_mobilegym_task_worker, job, suite_key, suite_path, task.id): task
                for task in suite.tasks
            }
            for future in concurrent.futures.as_completed(future_to_task):
                task = future_to_task[future]
                try:
                    result = future.result()
                except JobStopped:
                    result = {"suite": suite_key, "task_id": task.id, "exit_code": -15, "run_id": "", "stopped": True}
                    self._set_task_record(
                        job,
                        worker_token(suite_key, task.id),
                        status="stopped",
                        message="stop requested",
                        exit_code=-15,
                        finished_at=now_iso(),
                    )
                except TaskStopped:
                    result = {"suite": suite_key, "task_id": task.id, "exit_code": -15, "run_id": "", "stopped": True}
                    self._set_task_record(
                        job,
                        worker_token(suite_key, task.id),
                        status="stopped",
                        message="stop requested",
                        exit_code=-15,
                        finished_at=now_iso(),
                    )
                except Exception as exc:
                    append_log(Path(job.runner_log), f"ERROR: {suite_key} {task.id}: {exc}")
                    result = {"suite": suite_key, "task_id": task.id, "exit_code": 1, "run_id": "", "error": str(exc)}
                    self._set_task_record(
                        job,
                        worker_token(suite_key, task.id),
                        status="failed",
                        message=str(exc),
                        error=str(exc),
                        exit_code=1,
                        finished_at=now_iso(),
                    )
                completed += task_counts.get(task.id, 1)
                with self._lock:
                    job.suite_results.append(result)
                self._persist_job(job)
                write_state(
                    Path(job.state_file),
                    {
                        "status": "stopped" if self._job_stop_requested(job) else "running",
                        "suite": suite_key,
                        "run_id": job.id,
                        "total": total_runs,
                        "completed": min(completed, total_runs),
                        "current": min(completed, total_runs),
                        "current_task": task.id,
                        "last_result": "stopped" if result.get("stopped") else "passed" if result.get("exit_code") == 0 else "failed",
                        "parallel": job.parallel_tasks,
                    },
                )
        if self._job_stop_requested(job):
            update_state_status(Path(job.state_file), "stopped", run_id=job.id)
            raise JobStopped("job stop requested")
        update_state_status(Path(job.state_file), "done", run_id=job.id)

    def _run_mobilegym_task_worker(self, job: Job, suite_key: str, suite_path: Path, task_id: str) -> dict[str, Any]:
        self._raise_if_job_stop_requested(job)
        record = self._ensure_task_record(job, suite_key, task_id)
        token = record.id
        benchmark_task_id = record.benchmark_task_id
        self._raise_if_task_worker_stop_requested(job, token)
        host_port = 0
        worker_job = Job(
            id=f"{job.id}-{token}",
            endpoint=job.endpoint,
            docker_endpoint=job.docker_endpoint,
            suites=[suite_key],
            environment_endpoint=job.environment_endpoint,
            environment_id=job.environment_id,
            environment_name=job.environment_name,
            environment_type=job.environment_type,
            environment_web_url=job.environment_web_url,
            agent_url=f"http://127.0.0.1:{host_port}",
            container_name=f"{job.container_name}-{token}",
            config_dir=job.config_dir,
            raw_runs_dir=job.raw_runs_dir,
            state_file=record.state_file,
            runner_log=record.runner_log,
            daemon_log=record.daemon_log,
            no_judge=job.no_judge,
            judge_model=job.judge_model,
            judge_api_key_set=job.judge_api_key_set,
            repeats=job.repeats,
            parallel_tasks=1,
        )
        self._set_task_record(
            job,
            token,
            status="starting_agent",
            message="starting docker agent",
            started_at=now_iso(),
            agent_url=worker_job.agent_url,
            container_name=worker_job.container_name,
            state_file=worker_job.state_file,
            runner_log=worker_job.runner_log,
            daemon_log=worker_job.daemon_log,
        )
        self._register_daemon_job(job.id, worker_job)
        self._register_task_daemon_job(job.id, token, worker_job)
        log_proc: subprocess.Popen | None = None
        try:
            try:
                container_id = start_daemon_compose(
                    worker_job,
                    image=self.config.daemon_image,
                    host_port=host_port,
                    config_dir=Path(job.config_dir),
                    environment_bridge_endpoint=job.docker_endpoint,
                    benchmark_task_id=benchmark_task_id,
                    device_type=job.target_platform,
                    log_path=Path(worker_job.runner_log),
                    stop_requested=lambda: (
                        self._job_stop_requested(job)
                        or self._task_stop_requested(job, token)
                    ),
                )
                published_port = docker_published_port(container_id, 8080)
                worker_job.agent_url = f"http://127.0.0.1:{published_port}"
                self._set_task_record(job, token, agent_url=worker_job.agent_url)
            except JobStopped:
                if self._task_stop_requested(job, token) and not self._job_stop_requested(job):
                    raise TaskStopped("task stop requested")
                raise
            self._raise_if_task_worker_stop_requested(job, token)
            append_log(Path(job.runner_log), f"worker {task_id} container {container_id}")
            append_log(Path(worker_job.runner_log), f"container {container_id}")
            log_proc = start_daemon_logs(worker_job, Path(worker_job.daemon_log))
            self._wait_for_daemon(
                worker_job,
                stop_job=job,
                stop_check=lambda: self._raise_if_task_worker_stop_requested(job, token),
            )
            self._raise_if_task_worker_stop_requested(job, token)
            run_id = f"{job.id}-{token}"
            self._set_task_record(job, token, status="running", message="running task", run_id=run_id)
            cmd = [
                sys.executable,
                "-m",
                "runner.main",
                "run",
                "--suite",
                str(suite_path),
                "--task-id",
                task_id,
                "--run-id",
                run_id,
                "--benchmark-task-id",
                benchmark_task_id,
                "--agent-url",
                worker_job.agent_url,
                "--out",
                job.raw_runs_dir,
                "--state-file",
                worker_job.state_file,
                "--benchmark-token-file",
                str(Path(job.config_dir) / "control_token"),
                "--environment-url",
                job.environment_endpoint,
            ]
            if job.no_judge:
                cmd.append("--no-judge")
            else:
                cmd.extend(["--judge-model", job.judge_model or DEFAULT_JUDGE_MODEL])
                cmd.extend(["--judge-base-url", job.judge_base_url or DEFAULT_JUDGE_BASE_URL])
            if job.repeats:
                cmd.extend(["--repeats", str(job.repeats)])
            env = os.environ.copy()
            env[MOBILEGYM_PREFLIGHT_COMPLETE_ENV] = "1"
            if not job.no_judge:
                with self._lock:
                    judge_api_key = self._job_judge_api_keys.get(job.id, "")
                if judge_api_key:
                    env[DEFAULT_JUDGE_API_KEY_ENV] = judge_api_key
            exit_code = self._run_runner_process(
                worker_job,
                cmd,
                env,
                owner_job_id=job.id,
                extra_owner_ids=[task_worker_key(job.id, token)],
            )
            run_path = Path(job.raw_runs_dir) / run_id
            result: dict[str, Any] = {
                "suite": suite_key,
                "task_id": task_id,
                "exit_code": exit_code,
                "run_id": run_id if run_path.exists() else "",
            }
            if run_path.exists():
                manifest = read_json_file(run_path / "manifest.json") or {}
                result["manifest"] = manifest
                result["report_url"] = f"/reports/{job.id}/{run_id}/report.html"
            if self._job_stop_requested(job):
                result["stopped"] = True
            if self._task_stop_requested(job, token):
                result["stopped"] = True
            final_status = "stopped" if result.get("stopped") else "passed" if exit_code == 0 else "failed"
            self._set_task_record(
                job,
                token,
                status=final_status,
                message="stop requested" if final_status == "stopped" else "",
                finished_at=now_iso(),
                exit_code=exit_code,
                run_id=result.get("run_id") or run_id,
                report_url=str(result.get("report_url") or ""),
            )
            return result
        except TaskStopped:
            self._set_task_record(
                job,
                token,
                status="stopped",
                message="stop requested",
                finished_at=now_iso(),
                exit_code=-15,
            )
            raise
        except JobStopped:
            self._set_task_record(
                job,
                token,
                status="stopped",
                message="stop requested",
                finished_at=now_iso(),
                exit_code=-15,
            )
            raise
        except Exception as exc:
            self._set_task_record(
                job,
                token,
                status="failed",
                message=str(exc),
                error=str(exc),
                finished_at=now_iso(),
                exit_code=1,
            )
            raise
        finally:
            if job.environment_endpoint:
                try:
                    call_environment_release(job.environment_endpoint, timeout=2, task_id=benchmark_task_id)
                except ResetError as exc:
                    append_log(Path(job.runner_log), f"warning: failed to release {benchmark_task_id}: {exc}")
                    append_log(Path(worker_job.runner_log), f"warning: failed to release {benchmark_task_id}: {exc}")
            stop_daemon_compose(worker_job)
            if log_proc is not None:
                log_proc.terminate()
            self._unregister_daemon_job(job.id, worker_job)
            self._unregister_task_daemon_job(job.id, token, worker_job)

    def _run_runner_process(
        self,
        job: Job,
        cmd: list[str],
        env: dict[str, str],
        *,
        owner_job_id: str | None = None,
        extra_owner_ids: list[str] | None = None,
    ) -> int:
        append_log(Path(job.runner_log), "\n$ " + " ".join(cmd))
        popen_kwargs: dict[str, Any] = {}
        if os.name == "posix":
            popen_kwargs["start_new_session"] = True
        proc_job_id = owner_job_id or job.id
        with Path(job.runner_log).open("ab") as log:
            proc = subprocess.Popen(
                cmd,
                cwd=BENCHMARK_ROOT,
                stdout=log,
                stderr=subprocess.STDOUT,
                env=env,
                **popen_kwargs,
            )
            self._register_runner_proc(proc_job_id, proc)
            for extra_owner_id in extra_owner_ids or []:
                self._register_runner_proc(extra_owner_id, proc)
            try:
                return int(proc.wait())
            finally:
                self._unregister_runner_proc(proc_job_id, proc)
                for extra_owner_id in extra_owner_ids or []:
                    self._unregister_runner_proc(extra_owner_id, proc)

    def _register_runner_proc(self, job_id: str, proc: subprocess.Popen) -> None:
        with self._lock:
            current = self._job_runner_procs.get(job_id)
            if current is None:
                self._job_runner_procs[job_id] = {proc}
            elif isinstance(current, set):
                current.add(proc)
            else:
                self._job_runner_procs[job_id] = {current, proc}

    def _unregister_runner_proc(self, job_id: str, proc: subprocess.Popen) -> None:
        with self._lock:
            current = self._job_runner_procs.get(job_id)
            if isinstance(current, set):
                current.discard(proc)
                if not current:
                    self._job_runner_procs.pop(job_id, None)
            elif current is proc:
                self._job_runner_procs.pop(job_id, None)

    def _register_daemon_job(self, job_id: str, daemon_job: Job) -> None:
        with self._lock:
            self._job_daemon_jobs.setdefault(job_id, []).append(daemon_job)

    def _unregister_daemon_job(self, job_id: str, daemon_job: Job) -> None:
        with self._lock:
            jobs = self._job_daemon_jobs.get(job_id)
            if not jobs:
                return
            self._job_daemon_jobs[job_id] = [item for item in jobs if item is not daemon_job]
            if not self._job_daemon_jobs[job_id]:
                self._job_daemon_jobs.pop(job_id, None)

    def _register_task_daemon_job(self, job_id: str, task_record_id: str, daemon_job: Job) -> None:
        with self._lock:
            self._task_daemon_jobs.setdefault(task_worker_key(job_id, task_record_id), []).append(daemon_job)

    def _unregister_task_daemon_job(self, job_id: str, task_record_id: str, daemon_job: Job) -> None:
        task_key = task_worker_key(job_id, task_record_id)
        with self._lock:
            jobs = self._task_daemon_jobs.get(task_key)
            if not jobs:
                return
            self._task_daemon_jobs[task_key] = [item for item in jobs if item is not daemon_job]
            if not self._task_daemon_jobs[task_key]:
                self._task_daemon_jobs.pop(task_key, None)

    def _run_suite(self, job: Job, suite_key: str) -> None:
        self._raise_if_job_stop_requested(job)
        suite_path = resolve_suite_path(self.config.suites_dir, suite_key)
        write_state(
            Path(job.state_file),
            {
                "status": "running",
                "suite": suite_key,
                "run_id": job.id,
                "total": 0,
                "completed": 0,
                "current": 1,
            },
        )
        existing = {p.name for p in Path(job.raw_runs_dir).iterdir() if p.is_dir()}
        cmd = [
            sys.executable,
            "-m",
            "runner.main",
            "run",
            "--suite",
            str(suite_path),
            "--agent-url",
            job.agent_url,
            "--out",
            job.raw_runs_dir,
        ]
        cmd.extend(["--state-file", job.state_file])
        cmd.extend(["--benchmark-token-file", str(Path(job.config_dir) / "control_token")])
        if job.environment_endpoint:
            cmd.extend(["--environment-url", job.environment_endpoint])
            # Must match the id the shared daemon was started with, or the
            # bridge rejects the daemon's tool calls as another task's.
            cmd.extend(["--benchmark-task-id", job_benchmark_task_id(job.id)])
        if job.no_judge:
            cmd.append("--no-judge")
        else:
            cmd.extend(["--judge-model", job.judge_model or DEFAULT_JUDGE_MODEL])
            cmd.extend(["--judge-base-url", job.judge_base_url or DEFAULT_JUDGE_BASE_URL])
        if job.repeats:
            cmd.extend(["--repeats", str(job.repeats)])
        append_log(Path(job.runner_log), "\n$ " + " ".join(cmd))
        env = os.environ.copy()
        env[MOBILEGYM_PREFLIGHT_COMPLETE_ENV] = "1"
        if not job.no_judge:
            with self._lock:
                judge_api_key = self._job_judge_api_keys.get(job.id, "")
            if judge_api_key:
                env[DEFAULT_JUDGE_API_KEY_ENV] = judge_api_key
        self._raise_if_job_stop_requested(job)
        popen_kwargs: dict[str, Any] = {}
        if os.name == "posix":
            popen_kwargs["start_new_session"] = True
        with Path(job.runner_log).open("ab") as log:
            proc = subprocess.Popen(
                cmd,
                cwd=BENCHMARK_ROOT,
                stdout=log,
                stderr=subprocess.STDOUT,
                env=env,
                **popen_kwargs,
            )
            self._register_runner_proc(job.id, proc)
            try:
                exit_code = proc.wait()
            finally:
                self._unregister_runner_proc(job.id, proc)
        new_runs = sorted(
            (p for p in Path(job.raw_runs_dir).iterdir() if p.is_dir() and p.name not in existing),
            key=lambda p: p.stat().st_mtime,
        )
        result = {
            "suite": suite_key,
            "exit_code": exit_code,
            "run_id": new_runs[-1].name if new_runs else "",
        }
        if new_runs:
            manifest = read_json_file(new_runs[-1] / "manifest.json") or {}
            result["manifest"] = manifest
            result["report_url"] = f"/reports/{job.id}/{new_runs[-1].name}/report.html"
        if self._job_stop_requested(job):
            update_state_status(Path(job.state_file), "stopped", run_id=job.id)
            result["stopped"] = True
        with self._lock:
            job.suite_results.append(result)
        self._persist_job(job)
        if self._job_stop_requested(job):
            raise JobStopped("job stop requested")

    def _run_mock_suite(self, job: Job, suite_key: str) -> None:
        self._raise_if_job_stop_requested(job)
        suite_path = resolve_suite_path(self.config.suites_dir, suite_key)
        suite = load_suite(suite_path)
        for task in suite.tasks:
            record = self._ensure_task_record(job, suite_key, task.id)
            self._set_task_record(
                job,
                record.id,
                status="queued",
                message="waiting for mock environment worker",
                runner_log=job.runner_log,
                daemon_log="",
                screen_url="",
            )

        run_id = f"{job.id}-{worker_token(suite_key, 'mock')}"
        run_dir = Path(job.raw_runs_dir) / run_id
        cmd = [
            sys.executable,
            "-m",
            "runner.main",
            "run",
            "--suite",
            str(suite_path),
            "--auto-agent-setup",
            "--daemon-image",
            self.config.daemon_image,
            "--base-config-dir",
            job.config_dir,
            "--run-id",
            run_id,
            "--out",
            job.raw_runs_dir,
            "--state-file",
            job.state_file,
            "--verbose",
        ]
        if not self.config.build_daemon_image:
            cmd.append("--no-build-daemon-image")
        if job.no_judge:
            cmd.append("--no-judge")
        else:
            cmd.extend(["--judge-model", job.judge_model or DEFAULT_JUDGE_MODEL])
        if job.repeats:
            cmd.extend(["--repeats", str(job.repeats)])

        env = os.environ.copy()
        if not job.no_judge:
            with self._lock:
                judge_api_key = self._job_judge_api_keys.get(job.id, "")
            if judge_api_key:
                env[DEFAULT_JUDGE_API_KEY_ENV] = judge_api_key
        exit_code = self._run_runner_process(job, cmd, env)

        result = {
            "suite": suite_key,
            "exit_code": exit_code,
            "run_id": run_id if run_dir.exists() else "",
        }
        if run_dir.exists():
            manifest = read_json_file(run_dir / "manifest.json") or {}
            result["manifest"] = manifest
            result["report_url"] = f"/reports/{job.id}/{run_id}/report.html"
        self._update_mock_task_records(job, suite_key, run_id, run_dir)
        if self._job_stop_requested(job):
            update_state_status(Path(job.state_file), "stopped", run_id=job.id)
            result["stopped"] = True
        with self._lock:
            job.suite_results.append(result)
        self._persist_job(job)
        if self._job_stop_requested(job):
            raise JobStopped("job stop requested")

    def _update_mock_task_records(
        self,
        job: Job,
        suite_key: str,
        run_id: str,
        run_dir: Path,
    ) -> None:
        rows = read_results_jsonl(run_dir / "results.jsonl")
        report_url = (
            f"/reports/{job.id}/{run_id}/report.html" if run_dir.exists() else ""
        )
        updated_task_ids: set[str] = set()
        for row in rows:
            task_id = str(row.get("task_id") or "").strip()
            if not task_id:
                continue
            updated_task_ids.add(task_id)
            record = self._ensure_task_record(job, suite_key, task_id)
            status = str(row.get("status") or "failed").strip() or "failed"
            self._set_task_record(
                job,
                record.id,
                status=status,
                message="completed in mock environment",
                finished_at=now_iso(),
                run_id=run_id,
                report_url=report_url,
                exit_code=0 if status == "passed" else 1,
            )
        fallback_status = "stopped" if self._job_stop_requested(job) else "failed"
        with self._lock:
            missing_record_ids = [
                record.id
                for record in job.task_records
                if record.suite == suite_key
                and record.task_id not in updated_task_ids
                and record.status not in TERMINAL_TASK_STATUSES
            ]
        for record_id in missing_record_ids:
            self._set_task_record(
                job,
                record_id,
                status=fallback_status,
                message="mock environment run ended without a task result",
                finished_at=now_iso(),
                run_id=run_id,
                report_url=report_url,
                exit_code=1,
            )


def list_benchmark_suites(suites_dir: Path) -> list[dict[str, Any]]:
    suites = []
    if not suites_dir.exists():
        return suites
    for path in sorted(suites_dir.rglob("*.json")):
        rel = path.relative_to(suites_dir)
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except Exception as exc:
            item = {
                "key": rel.as_posix(),
                "name": path.stem,
                "kind": "invalid",
                "task_count": 0,
                "error": str(exc),
            }
        else:
            kind = "benchmark"
            entries = data.get("tasks")
            entries = entries if isinstance(entries, list) else []
            categories = sorted(
                {str(task.get("category")) for task in entries if isinstance(task, dict) and task.get("category")}
            )
            item = {
                "key": rel.as_posix(),
                "name": str(data.get("name") or path.stem),
                "kind": kind,
                "task_count": len(entries),
                "categories": categories,
                "suite_category": data.get("suite_category", "Other"),
                "mock_environment": suite_data_uses_mock_environment(data),
            }
        suites.append(item)
    return suites


def suite_data_uses_mock_environment(data: dict[str, Any]) -> bool:
    if data.get("mock_environment") is not None:
        return True
    tasks = data.get("tasks")
    if not isinstance(tasks, list):
        return False
    return any(
        isinstance(task, dict) and task.get("mock_environment") is not None
        for task in tasks
    )


def suite_uses_mock_environment(path: Path) -> bool:
    try:
        data = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return False
    return isinstance(data, dict) and suite_data_uses_mock_environment(data)


def _mock_job_target_platform(suite_results: list[dict[str, Any]]) -> str:
    platforms = {
        str((result.get("manifest") or {}).get("target_platform") or "").strip()
        for result in suite_results
    }
    platforms.discard("")
    if not platforms:
        return ""
    if len(platforms) == 1:
        return next(iter(platforms))
    return "mixed"


def resolve_suite_path(suites_dir: Path, key: str) -> Path:
    pure = Path(key)
    if pure.is_absolute() or ".." in pure.parts or not key.endswith(".json"):
        raise ValueError(f"invalid suite key: {key}")
    path = (suites_dir / pure).resolve()
    root = suites_dir.resolve()
    if root != path and root not in path.parents:
        raise ValueError(f"invalid suite key: {key}")
    if not path.exists() or not path.is_file():
        raise ValueError(f"suite not found: {key}")
    return path


def parse_positive_int(value: Any, *, default: int, field: str) -> int:
    if value in (None, ""):
        return default
    try:
        parsed = int(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{field} must be a positive integer") from exc
    if parsed < 1:
        raise ValueError(f"{field} must be a positive integer")
    return parsed


def worker_token(suite_key: str, task_id: str) -> str:
    raw = f"{suite_key}-{task_id}"
    digest = hashlib.sha1(raw.encode("utf-8")).hexdigest()[:8]
    slug = re.sub(r"[^a-z0-9_-]+", "-", raw.lower()).strip("-_")
    slug = slug[:42].strip("-_") or "task"
    return f"{slug}-{digest}"


def task_worker_key(job_id: str, task_record_id: str) -> str:
    return f"{job_id}:{task_record_id}"


def job_benchmark_task_id(job_id: str) -> str:
    """Route id shared by a job's single daemon and its runner processes.

    Jobs that run every task through one long-lived daemon (device, adb_android,
    and non-parallel mobilegym) cannot use the runner's default per-task route
    id: the daemon is started once, so it would send one fixed id — or none at
    all — while the runner claimed a different id per task, and an environment
    bridge that enforces single-environment ownership rejects the mismatch with
    `429 no_bridge_env_available`. One id per job keeps both sides in agreement;
    tasks run sequentially on that path, so job-level ownership is enough.
    """
    return f"webui:{job_id}"


def runner_procs_for_stop(value: Any) -> list[subprocess.Popen]:
    if value is None:
        return []
    if isinstance(value, (set, list, tuple)):
        return list(value)
    return [value]


def prepare_run_config(
    base_config_dir: Path,
    dest_dir: Path,
    agent_config_text: str | None = None,
) -> None:
    if agent_config_text is None and not any(
        path.is_file()
        for path in (
            base_config_dir / "agent.toml",
            base_config_dir / "agent.toml.template",
        )
    ):
        raise missing_agent_config_error(base_config_dir)

    if dest_dir.exists():
        shutil.rmtree(dest_dir)
    dest_dir.mkdir(parents=True, exist_ok=True)
    if base_config_dir.exists():
        for item in base_config_dir.iterdir():
            if item.name == "memory":
                memory_dir = dest_dir / item.name
                for name in MEMORY_CONFIG_FILE_NAMES:
                    source = item / name
                    if source.is_file():
                        memory_dir.mkdir(parents=True, exist_ok=True)
                        shutil.copy2(source, memory_dir / name)
                continue
            if item.name in RUNTIME_CONFIG_DIR_NAMES:
                continue
            target = dest_dir / item.name
            if item.is_dir():
                shutil.copytree(item, target)
            else:
                shutil.copy2(item, target)
    skills_dir = dest_dir / "skills"
    if DEFAULT_BUNDLED_SKILLS_DIR.is_dir():
        skills_dir.mkdir(parents=True, exist_ok=True)
        for skill in DEFAULT_BUNDLED_SKILLS_DIR.iterdir():
            target = skills_dir / skill.name
            if skill.is_dir() and not target.exists():
                shutil.copytree(skill, target)
    for name in ("log", "memory", "skill-state"):
        (dest_dir / name).mkdir(parents=True, exist_ok=True)
    token_path = dest_dir / "control_token"
    if not token_path.exists():
        token_path.write_text(os.urandom(24).hex(), encoding="utf-8")
    template = dest_dir / "agent.toml.template"
    config = dest_dir / "agent.toml"
    if agent_config_text is not None:
        config.write_text(agent_config_text, encoding="utf-8")
    else:
        if template.exists() and not config.exists():
            config.write_text(render_agent_template(template.read_text(encoding="utf-8")), encoding="utf-8")
        if not config.exists():
            raise missing_agent_config_error(base_config_dir)

    content = apply_agent_toml_runtime_defaults(config.read_text(encoding="utf-8"))
    content = apply_benchmark_telemetry(content)
    content = materialize_agent_config_credentials(content)
    validate_agent_toml(content)
    config.write_text(content, encoding="utf-8")


def default_webui_settings(include_secrets: bool = False) -> dict[str, Any]:
    judge: dict[str, Any] = {
        "enabled": True,
        "model": DEFAULT_JUDGE_MODEL,
        "base_url": DEFAULT_JUDGE_BASE_URL,
    }
    if include_secrets:
        judge["api_key"] = ""
    else:
        judge["has_api_key"] = False
    return {
        "judge": judge,
        "device_environments": [],
        "selected_environment_id": "",
    }


def normalize_device_environments(raw: Any) -> list[dict[str, str]]:
    if not isinstance(raw, list):
        return []
    out: list[dict[str, str]] = []
    seen: set[str] = set()
    for index, item in enumerate(raw):
        if not isinstance(item, dict):
            continue
        env_id = str(item.get("id") or "").strip() or f"device-{index + 1}"
        name = str(item.get("name") or "").strip()
        endpoint = str(item.get("endpoint") or "").strip()
        if not name or not endpoint or env_id in seen:
            continue
        seen.add(env_id)
        out.append({
            "id": env_id,
            "name": name,
            "endpoint": endpoint,
        })
    return out


def normalize_webui_settings(data: Any, include_secrets: bool = False) -> dict[str, Any]:
    if not isinstance(data, dict):
        data = {}
    raw_judge = data.get("judge") if isinstance(data.get("judge"), dict) else {}
    api_key = str(raw_judge.get("api_key") or "").strip()
    has_api_key = bool(api_key) or bool(raw_judge.get("has_api_key", False))
    judge: dict[str, Any] = {
        "enabled": bool(raw_judge.get("enabled", True)),
        "model": str(raw_judge.get("model") or DEFAULT_JUDGE_MODEL).strip() or DEFAULT_JUDGE_MODEL,
        "base_url": str(raw_judge.get("base_url") or DEFAULT_JUDGE_BASE_URL).strip() or DEFAULT_JUDGE_BASE_URL,
    }
    if include_secrets:
        judge["api_key"] = api_key
    else:
        judge["has_api_key"] = has_api_key

    result = {
        "judge": judge,
        "device_environments": normalize_device_environments(data.get("device_environments")),
        "selected_environment_id": str(data.get("selected_environment_id") or ""),
    }

    # Preserve metrics_k if present
    if "metrics_k" in data:
        try:
            k = int(data["metrics_k"])
            result["metrics_k"] = max(1, min(k, 10))
        except (TypeError, ValueError):
            pass

    return result


def sanitize_webui_settings(data: dict[str, Any]) -> dict[str, Any]:
    return normalize_webui_settings(data, include_secrets=False)


def load_webui_settings(path: Path, include_secrets: bool = False) -> dict[str, Any]:
    if not path.exists():
        return default_webui_settings(include_secrets=include_secrets)
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        data = {}
    return normalize_webui_settings(data, include_secrets=include_secrets)


def write_text_atomic(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(content, encoding="utf-8")
    tmp.replace(path)


def write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    write_text_atomic(path, json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n")


def endpoint_for_docker(endpoint: str) -> str:
    parsed = urllib.parse.urlparse(endpoint)
    if parsed.hostname not in {"localhost", "127.0.0.1", "::1"}:
        return endpoint.rstrip("/")
    netloc = "host.docker.internal"
    if parsed.port:
        netloc += f":{parsed.port}"
    if parsed.username:
        userinfo = parsed.username
        if parsed.password:
            userinfo += f":{parsed.password}"
        netloc = f"{userinfo}@{netloc}"
    return urllib.parse.urlunparse((parsed.scheme, netloc, parsed.path, parsed.params, parsed.query, parsed.fragment)).rstrip("/")


def webui_task_screen_url(job_id: str, task_record_id: str) -> str:
    return (
        "/screens/jobs/"
        + urllib.parse.quote(str(job_id), safe="")
        + "/tasks/"
        + urllib.parse.quote(str(task_record_id), safe="")
    )


def read_environment_bridge_screen(endpoint: str, benchmark_task_id: str, *, timeout: float = DEFAULT_SCREENSHOT_TIMEOUT_SEC) -> dict[str, Any]:
    headers = {"Content-Type": "application/json"}
    task_id = str(benchmark_task_id or "").strip()
    if task_id:
        headers["benchmark-task-id"] = task_id
    req = urllib.request.Request(
        EnvironmentEndpoint(endpoint).screen,
        data=b'{"format":"jpeg","quality":80}',
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            body = response.read()
    except urllib.error.HTTPError as exc:
        try:
            body = exc.read()
        except Exception:
            body = b""
        raise RuntimeError(f"screen request failed HTTP {exc.code}: {body[:200]!r}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"screen request failed: {exc}") from exc
    payload = json.loads(body.decode("utf-8")) if body else {}
    if not isinstance(payload, dict):
        raise RuntimeError(f"screen request returned unexpected payload: {payload!r}")
    return payload


def read_environment_bridge_concurrency(endpoint: str, *, timeout: float = 2.0) -> int | None:
    try:
        url = EnvironmentEndpoint(endpoint).concurrent
        with urllib.request.urlopen(url, timeout=timeout) as response:
            body = response.read()
        payload = json.loads(body.decode("utf-8")) if body else {}
    except Exception:
        return None
    if not isinstance(payload, dict) or payload.get("ok") is False:
        return None
    data = payload.get("data") if isinstance(payload.get("data"), dict) else payload
    try:
        concurrent_value = int(data.get("concurrent"))
    except (AttributeError, TypeError, ValueError):
        return None
    return concurrent_value if concurrent_value > 0 else None


def reserve_free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def docker_publish_arg(host_port: int, container_port: int, *, bind_host: str = "") -> str:
    host_port = int(host_port)
    if host_port == 0:
        return f"{bind_host}::{container_port}" if bind_host else str(container_port)
    return f"{bind_host}:{host_port}:{container_port}" if bind_host else f"{host_port}:{container_port}"


def docker_published_port(container_name: str, container_port: int) -> int:
    output = subprocess.check_output(
        ["docker", "port", container_name, f"{int(container_port)}/tcp"],
        text=True,
    ).strip()
    for line in output.splitlines():
        try:
            return int(line.rsplit(":", 1)[1])
        except (IndexError, ValueError):
            continue
    raise RuntimeError(f"could not determine published port for {container_name}:{container_port}")


def container_boot_failure_detail(container_name: str) -> str:
    """Best-effort summary of why a container stopped before publishing a port.

    The docker CLI answers ``docker port`` with "No public port ... published"
    for a container that has already exited, which hides the real startup
    failure. This collects the container state and the tail of its logs so the
    caller can surface the actual cause. Returns "" when nothing could be
    gathered.
    """
    detail: list[str] = []
    try:
        state = subprocess.run(
            [
                "docker",
                "inspect",
                "-f",
                "{{.State.Status}} (exit {{.State.ExitCode}})",
                container_name,
            ],
            capture_output=True,
            text=True,
            check=False,
            timeout=DOCKER_DIAGNOSTIC_TIMEOUT_SEC,
        ).stdout.strip()
        if state:
            detail.append(f"container state: {state}")
    except subprocess.TimeoutExpired:
        pass
    except Exception:
        pass
    try:
        logs = subprocess.run(
            ["docker", "logs", "--tail", "40", container_name],
            capture_output=True,
            text=True,
            check=False,
            timeout=DOCKER_DIAGNOSTIC_TIMEOUT_SEC,
        )
        tail_lines = (logs.stdout + logs.stderr).strip().splitlines()
        if tail_lines:
            detail.append("last container logs:\n" + "\n".join(tail_lines[-40:]))
    except subprocess.TimeoutExpired:
        pass
    except Exception:
        pass
    return "\n".join(detail)


def build_mobilegym_environment_command(
    *,
    image: str,
    container_name: str,
    host_web_port: int,
    host_bridge_port: int,
    benchmark_dir: Path,
    bridge_port: int = 9090,
    parallel_envs: int = DEFAULT_MOBILEGYM_PARALLEL_ENVS,
) -> list[str]:
    parallel_envs = max(1, int(parallel_envs))
    script = "\n".join(
        [
            "set -eu",
            'export PATH="/opt/venv/bin:$PATH"',
            "cd /mobilegym",
            "npm run preview -- --host 0.0.0.0 --port 4173 >/tmp/mobilegym-preview.log 2>&1 &",
            'preview_pid="$!"',
            'cleanup() { kill "$preview_pid" 2>/dev/null || true; }',
            "trap cleanup INT TERM EXIT",
            "ready=0",
            "for _ in $(seq 1 120); do",
            (
                "  if python3 -c \"import urllib.request; "
                "urllib.request.urlopen('http://127.0.0.1:4173', timeout=1).read(1)\" "
                ">/dev/null 2>&1; then"
            ),
            "    ready=1",
            "    break",
            "  fi",
            '  if ! kill -0 "$preview_pid" 2>/dev/null; then',
            "    cat /tmp/mobilegym-preview.log >&2",
            "    exit 1",
            "  fi",
            "  sleep 1",
            "done",
            'if [ "$ready" != "1" ]; then',
            "  cat /tmp/mobilegym-preview.log >&2",
            "  exit 1",
            "fi",
            (
                "exec python3 /app/benchmark/mobilegym/scripts/start_simulator.py "
                "--mobilegym-root /mobilegym "
                "--env-url http://127.0.0.1:4173 "
                "--bridge-host 0.0.0.0 "
                f"--bridge-port {bridge_port} "
                f"--parallel-envs {parallel_envs} "
                "--headless"
            ),
        ]
    )
    bridge_endpoint = f"http://host.docker.internal:{host_bridge_port}"
    return [
        "docker",
        "run",
        "--rm",
        "-d",
        "--name",
        container_name,
        "--add-host",
        "host.docker.internal:host-gateway",
        "-p",
        docker_publish_arg(host_web_port, 4173, bind_host="127.0.0.1"),
        "-p",
        docker_publish_arg(
            host_bridge_port,
            bridge_port,
            bind_host="127.0.0.1" if int(host_bridge_port) == 0 else "",
        ),
        "-v",
        f"{benchmark_dir.resolve()}:/app/benchmark:ro",
        "-e",
        "MOBILEGYM_ROOT=/mobilegym",
        "-e",
        f"NO_PROXY={docker_no_proxy(bridge_endpoint)}",
        "-e",
        f"no_proxy={docker_no_proxy(bridge_endpoint)}",
        "--entrypoint",
        "sh",
        image,
        "-c",
        script,
    ]


def docker_no_proxy(endpoint: str) -> str:
    base = ["localhost", "127.0.0.1", "host.docker.internal"]
    host = urllib.parse.urlparse(endpoint).hostname
    if host and host not in base:
        base.append(host)
    existing = os.getenv("NO_PROXY") or os.getenv("no_proxy") or ""
    for item in existing.split(","):
        item = item.strip()
        if item and item not in base:
            base.append(item)
    return ",".join(base)


def ensure_daemon_image(
    image: str,
    build_image: bool,
    log_path: Path,
    *,
    stop_requested: Callable[[], bool] | None = None,
) -> None:
    if not build_image:
        inspect = subprocess.run(
            ["docker", "image", "inspect", image],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if inspect.returncode != 0:
            raise RuntimeError(f"Docker image not found: {image}")
        return
    env = daemon_compose_env(image=image)
    run_logged_command(
        daemon_compose_command("build", "daemon"),
        log_path,
        cwd=BENCHMARK_DOCKER_DIR,
        env=env,
        stop_requested=stop_requested,
    )


def ensure_docker_image(
    image: str,
    build_missing: bool,
    log_path: Path,
    target: str,
    *,
    stop_requested: Callable[[], bool] | None = None,
) -> None:
    inspect = subprocess.run(["docker", "image", "inspect", image], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
    if inspect.returncode == 0:
        return
    if not build_missing:
        raise RuntimeError(f"Docker image not found: {image}")
    cmd = [
        "docker",
        "build",
        "-f",
        str(REPO_ROOT / "benchmark" / "mobilegym" / "docker" / "Dockerfile"),
        "--target",
        target,
        "-t",
        image,
        str(REPO_ROOT),
    ]
    append_log(log_path, "$ " + " ".join(cmd))
    popen_kwargs: dict[str, Any] = {}
    if os.name == "posix":
        popen_kwargs["start_new_session"] = True
    with log_path.open("ab") as log:
        proc = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, **popen_kwargs)
        while proc.poll() is None:
            if stop_requested is not None and stop_requested():
                terminate_process_tree(proc)
                raise JobStopped("job stop requested")
            time.sleep(0.25)
    if proc.returncode != 0:
        raise subprocess.CalledProcessError(proc.returncode, cmd)


def daemon_compose_project(job: Job) -> str:
    return job.container_name or f"aiden-benchmark-agent-{job.id}"


def daemon_compose_command(*args: str, project: str | None = None) -> list[str]:
    command = [
        "docker",
        "compose",
        "-f",
        str(AGENT_DAEMON_COMPOSE_FILE),
    ]
    if project:
        command.extend(["-p", project])
    command.extend(args)
    return command


def daemon_compose_env(
    *,
    image: str,
    host_port: int | None = None,
    config_dir: Path | None = None,
    environment_bridge_endpoint: str = "",
    benchmark_task_id: str = "",
    device_type: str = "",
    environment_bridge_mode: bool | None = None,
) -> dict[str, str]:
    env = dict(os.environ)
    # Strip general host proxies so builds and model/device traffic do not
    # inherit them. The explicitly configured telemetry-only proxy is retained.
    # NO_PROXY is a bypass list, not a proxy, and is set explicitly below.
    for key in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                "http_proxy", "https_proxy", "all_proxy"):
        env.pop(key, None)
    env["AIDEN_DAEMON_IMAGE"] = image
    bridge_enabled = bool(environment_bridge_endpoint) if environment_bridge_mode is None else bool(environment_bridge_mode)
    env["AIDEN_ENVIRONMENT_BRIDGE_MODE"] = "1" if bridge_enabled else "0"
    if host_port is not None:
        # Callers use 0 for auto-selection; start_daemon_compose resolves it
        # to a concrete free port before invoking Compose. Explicit positive
        # ports remain stable for local callers that need a predictable endpoint.
        env["AIDEN_DAEMON_HOST_PORT"] = str(host_port)
    if config_dir is not None:
        env["AIDEN_CONFIG_DIR"] = str(config_dir.resolve())
        env["AIDEN_BENCHMARK_TOKEN_FILE"] = "/config/control_token"
    if environment_bridge_endpoint:
        env["ENVIRONMENT_BRIDGE_ENDPOINT"] = environment_bridge_endpoint
        no_proxy = docker_no_proxy(environment_bridge_endpoint)
        env["NO_PROXY"] = no_proxy
        env["no_proxy"] = no_proxy
    if benchmark_task_id:
        env["AIDEN_BENCHMARK_TASK_ID"] = benchmark_task_id
    if device_type:
        env["AIDEN_DEVICE_TYPE"] = device_type
    return env


def start_daemon_compose(
    job: Job,
    *,
    image: str,
    host_port: int,
    config_dir: Path,
    environment_bridge_endpoint: str,
    log_path: Path,
    benchmark_task_id: str = "",
    device_type: str = "",
    environment_bridge_mode: bool | None = None,
    stop_requested: Callable[[], bool] | None = None,
) -> str:
    project = daemon_compose_project(job)
    # Port 0 is passed through to Docker so the bind and allocation are atomic.
    # Reserving a port with a short-lived socket leaves a race before Compose
    # starts the container, especially when benchmark workers start together.
    env = daemon_compose_env(
        image=image,
        host_port=host_port,
        config_dir=config_dir,
        environment_bridge_endpoint=environment_bridge_endpoint,
        benchmark_task_id=benchmark_task_id,
        device_type=device_type,
        environment_bridge_mode=environment_bridge_mode,
    )
    run_logged_command(
        daemon_compose_command(
            "up",
            "-d",
            "--force-recreate",
            "daemon",
            project=project,
        ),
        log_path,
        cwd=BENCHMARK_DOCKER_DIR,
        env=env,
        stop_requested=stop_requested,
    )
    ps_command = daemon_compose_command("ps", "-q", "daemon", project=project)
    append_log(log_path, "$ " + " ".join(ps_command))
    return subprocess.check_output(ps_command, cwd=BENCHMARK_DOCKER_DIR, env=env, text=True).strip()


def stop_daemon_compose(job: Job) -> None:
    subprocess.run(
        daemon_compose_command(
            "down",
            "--volumes",
            "--remove-orphans",
            project=daemon_compose_project(job),
        ),
        cwd=BENCHMARK_DOCKER_DIR,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )


def start_daemon_logs(job: Job, log_path: Path) -> subprocess.Popen | None:
    try:
        log_file = log_path.open("ab")
        try:
            return subprocess.Popen(
                daemon_compose_command(
                    "logs",
                    "-f",
                    "daemon",
                    project=daemon_compose_project(job),
                ),
                cwd=BENCHMARK_DOCKER_DIR,
                stdout=log_file,
                stderr=subprocess.STDOUT,
            )
        finally:
            log_file.close()
    except Exception:
        return None


def run_logged_command(
    cmd: list[str],
    log_path: Path,
    *,
    cwd: Path | None = None,
    env: Mapping[str, str] | None = None,
    stop_requested: Callable[[], bool] | None = None,
) -> None:
    append_log(log_path, "$ " + " ".join(cmd))
    popen_kwargs: dict[str, Any] = {}
    if os.name == "posix":
        popen_kwargs["start_new_session"] = True
    with log_path.open("ab") as log:
        proc = subprocess.Popen(
            cmd,
            cwd=cwd,
            env=dict(env) if env is not None else None,
            stdout=log,
            stderr=subprocess.STDOUT,
            **popen_kwargs,
        )
        while proc.poll() is None:
            if stop_requested is not None and stop_requested():
                terminate_process_tree(proc)
                raise JobStopped("job stop requested")
            time.sleep(0.25)
    if proc.returncode != 0:
        raise subprocess.CalledProcessError(proc.returncode, cmd)


def wait_for_http_health(url: str, timeout_sec: int) -> None:
    deadline = time.monotonic() + max(1, timeout_sec)
    last_error = ""
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if 200 <= response.status < 300:
                    return
        except Exception as exc:
            last_error = str(exc)
        time.sleep(1)
    suffix = f": {last_error}" if last_error else ""
    raise RuntimeError(f"service did not become ready at {url}{suffix}")


def start_docker_logs(container_name: str, log_path: Path) -> subprocess.Popen | None:
    try:
        log_file = log_path.open("ab")
        try:
            return subprocess.Popen(["docker", "logs", "-f", container_name], stdout=log_file, stderr=subprocess.STDOUT)
        finally:
            log_file.close()
    except Exception:
        return None


def aggregate_totals(results: list[dict[str, Any]]) -> dict[str, int]:
    totals = {"tasks": 0, "passed": 0, "failed": 0, "skipped": 0, "judge_error": 0, "timeout": 0}
    for result in results:
        manifest = result.get("manifest") or {}
        for key, value in (manifest.get("totals") or {}).items():
            if isinstance(value, int):
                totals[key] = totals.get(key, 0) + value
    return totals


def job_record_path(job: Job) -> Path | None:
    if job.raw_runs_dir:
        return Path(job.raw_runs_dir).parent / JOB_RECORD_FILE
    if job.state_file:
        return Path(job.state_file).parent / JOB_RECORD_FILE
    return None


def persist_job_record(job: Job) -> None:
    path = job_record_path(job)
    if path is None:
        return
    write_json_atomic(path, dc.asdict(job))


def load_job_record(path: Path) -> Job | None:
    data = read_json_file(path)
    if not isinstance(data, dict):
        return None
    task_records = []
    raw_task_records = data.get("task_records") if isinstance(data.get("task_records"), list) else []
    for raw in raw_task_records:
        if not isinstance(raw, dict):
            continue
        try:
            task_records.append(TaskRecord(**dataclass_kwargs(TaskRecord, raw)))
        except TypeError:
            continue
    data = dict(data)
    data["task_records"] = task_records
    try:
        job = Job(**dataclass_kwargs(Job, data))
    except TypeError:
        return None
    if not job.raw_runs_dir:
        job.raw_runs_dir = str(path.parent / "raw")
    if not job.state_file:
        job.state_file = str(path.parent / "state.json")
    if not job.runner_log:
        job.runner_log = str(path.parent / "runner.log")
    if not job.daemon_log:
        job.daemon_log = str(path.parent / "daemon.log")
    return job


def dataclass_kwargs(cls: type[Any], data: dict[str, Any]) -> dict[str, Any]:
    names = {field.name for field in dc.fields(cls)}
    return {key: value for key, value in data.items() if key in names}


def single_suite_report_url(results: list[dict[str, Any]]) -> str:
    urls = [str(result.get("report_url") or "") for result in results if result.get("report_url")]
    return urls[0] if len(urls) == 1 else ""


def write_job_report(job: Job, *, analysis_api_key: str = "") -> str:
    raw_runs_dir = Path(job.raw_runs_dir)
    if not raw_runs_dir.exists():
        return ""
    report_dir = raw_runs_dir / JOB_REPORT_RUN_ID
    rows = merged_job_report_rows(job)
    if not rows:
        return ""
    metrics_k = _job_report_metrics_k(job)
    metric_rows = _job_report_metric_rows(rows)
    aggregate = aggregate_rows(metric_rows, k=metrics_k)
    if report_dir.exists():
        shutil.rmtree(report_dir)
    (report_dir / "tasks").mkdir(parents=True, exist_ok=True)
    for row in rows:
        source_artifact = str(row.pop("_source_artifact_dir", "") or "")
        report_task_id = str(row.get("task_id") or "")
        if source_artifact and Path(source_artifact).exists() and report_task_id:
            source_artifact_dir = Path(source_artifact)
            shutil.copytree(source_artifact_dir, report_dir / "tasks" / report_task_id, dirs_exist_ok=True)
        row["artifact_dir"] = str(report_dir / "tasks" / report_task_id) if report_task_id else ""
    manifest = {
        "run_id": JOB_REPORT_RUN_ID,
        "job_id": job.id,
        "suite_path": ", ".join(job.suites),
        "selected_task_ids": [str(row.get("task_id") or "") for row in rows],
        "agent_url": job.agent_url,
        "environment_url": job.environment_endpoint or None,
        "judge_config": None if job.no_judge else {"provider": "openai-compatible", "model": job.judge_model or DEFAULT_JUDGE_MODEL, "base_url": job.judge_base_url or DEFAULT_JUDGE_BASE_URL},
        "started_at": job.started_at,
        "finished_at": job.finished_at or now_iso(),
        "totals": totals_from_report_rows(rows),
        "metrics_schema_version": "p0-v1",
        "metrics_k": metrics_k,
    }
    write_json_atomic(report_dir / "manifest.json", manifest)
    write_json_atomic(
        report_dir / "metrics.json",
        {
            "schema_version": "p0-v1",
            "suite": ", ".join(job.suites),
            "run_id": JOB_REPORT_RUN_ID,
            "metrics_k": metrics_k,
            "aggregate": aggregate,
        },
    )
    (report_dir / "summary.md").write_text(
        _job_report_summary(job, aggregate), encoding="utf-8"
    )
    with (report_dir / "results.jsonl").open("w", encoding="utf-8") as handle:
        for row in rows:
            handle.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")
    _run_job_analysis_if_enabled(report_dir, job, analysis_api_key=analysis_api_key)
    (report_dir / "report.html").write_text(generate_report_html(report_dir), encoding="utf-8")
    return f"/reports/{job.id}/{JOB_REPORT_RUN_ID}/report.html"


def _job_report_metrics_k(job: Job) -> int:
    """Use the configured repeat count, or the common source-run prefix."""
    if job.repeats is not None:
        return max(1, int(job.repeats))
    values: list[int] = []
    for result in job.suite_results:
        manifest = result.get("manifest") if isinstance(result, dict) else None
        if not isinstance(manifest, dict):
            run_id = str(result.get("run_id") or "") if isinstance(result, dict) else ""
            if run_id:
                manifest = read_json_file(Path(job.raw_runs_dir) / run_id / "manifest.json")
        value = manifest.get("metrics_k") if isinstance(manifest, dict) else None
        if isinstance(value, int) and value > 0:
            values.append(value)
    return min(values, default=1)


def _job_report_summary(job: Job, aggregate: dict[str, Any]) -> str:
    def rate(name: str) -> str:
        value = (aggregate.get(name) or {}).get("value")
        return "n/a" if value is None else f"{float(value) * 100:.1f}%"

    def coverage(name: str) -> str:
        metric = aggregate.get(name) or {}
        eligible = metric.get("eligible_tasks")
        total = metric.get("total_tasks")
        return f"{eligible}/{total}" if eligible is not None and total is not None else "n/a"

    best = (aggregate.get("oracle_best_score_at_k") or {}).get("value")
    best_text = "n/a" if best is None else f"{float(best):.2f}"
    lines = [
        f"# Benchmark Summary — {job.id}",
        "",
        f"Suites: {', '.join(job.suites)}",
        f"Attempts: {aggregate.get('attempts', 0)} across {aggregate.get('unique_tasks', 0)} tasks",
        "",
        "## Capability Metrics",
        "",
        f"- k={aggregate.get('metrics_k', 1)}",
        f"- pass@1: {rate('pass_at_1')} (coverage {coverage('pass_at_1')})",
        f"- pass@k: {rate('pass_at_k')} (coverage {coverage('pass_at_k')})",
        f"- pass^k: {rate('pass_pow_k')} (coverage {coverage('pass_pow_k')})",
        f"- oracle best score@k: {best_text} (diagnostic only)",
        "",
        "## Outcome Quality",
        "",
        f"- eligible attempts: {aggregate.get('agent_eligible_attempts', 0)}",
        f"- failure classes: {json.dumps(aggregate.get('failure_classes', {}), sort_keys=True)}",
        "",
    ]
    return "\n".join(lines)


def _job_report_metric_rows(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Restore source task grouping for fixed-k aggregation in merged reports."""
    metric_rows: list[dict[str, Any]] = []
    for row in rows:
        metrics = row.get("metrics") if isinstance(row.get("metrics"), dict) else {}
        source_suite = str(metrics.get("source_suite") or row.get("suite") or "")
        source_task_id = str(metrics.get("source_task_id") or row.get("task_id") or "")
        metric_row = dict(row)
        metric_row["task_id"] = f"{source_suite}:{source_task_id}" if source_suite else source_task_id
        metric_rows.append(metric_row)
    return metric_rows


def _run_job_analysis_if_enabled(
    report_dir: Path,
    job: Job,
    *,
    analysis_api_key: str = "",
) -> AnalysisResult | None:
    cfg = config_from_env()
    if not _analysis_env_is_set() and not job.no_judge and analysis_api_key:
        cfg.enabled = True
    if not cfg.enabled:
        return None
    if not os.environ.get("AIDEN_BENCHMARK_ANALYSIS_MODEL") and job.judge_model:
        cfg.model = job.judge_model or DEFAULT_JUDGE_MODEL
    if not os.environ.get("AIDEN_BENCHMARK_ANALYSIS_BASE_URL"):
        cfg.base_url = job.judge_base_url or DEFAULT_JUDGE_BASE_URL
    if analysis_api_key:
        cfg.api_key_value = analysis_api_key
    result = analyze_run(report_dir, REPO_ROOT, cfg)
    if not result.ok:
        append_log(Path(job.runner_log), f"warning: WebUI LLM analysis failed: {result.warning}")
    return result


def _analysis_env_is_set() -> bool:
    return bool(os.environ.get("AIDEN_BENCHMARK_LLM_ANALYSIS", "").strip())


def merged_job_report_rows(job: Job) -> list[dict[str, Any]]:
    raw_runs_dir = Path(job.raw_runs_dir)
    rows: list[dict[str, Any]] = []
    used_ids: set[str] = set()
    for index, suite_result in enumerate(job.suite_results, start=1):
        run_id = str(suite_result.get("run_id") or "")
        run_dir = raw_runs_dir / run_id if run_id else None
        result_rows = read_results_jsonl(run_dir / "results.jsonl") if run_dir is not None else []
        if not result_rows:
            fallback = fallback_report_row(suite_result, job.id, index, used_ids)
            if fallback is not None:
                rows.append(fallback)
            continue
        for row in result_rows:
            source_task_id = str(row.get("task_id") or suite_result.get("task_id") or f"task-{index}")
            source_suite = str(suite_result.get("suite") or row.get("suite") or "")
            attempt = int(row.get("attempt") or 1)
            report_task_id = unique_report_task_id(source_suite, source_task_id, attempt, used_ids)
            metrics = row.get("metrics") if isinstance(row.get("metrics"), dict) else {}
            metrics = dict(metrics)
            metrics.setdefault("source_task_id", source_task_id)
            metrics.setdefault("source_suite", source_suite)
            row = dict(row)
            row["task_id"] = report_task_id
            row["run_id"] = JOB_REPORT_RUN_ID
            row["suite"] = source_suite
            row["metrics"] = metrics
            if source_suite:
                category = str(row.get("category") or "")
                row["category"] = f"{source_suite} / {category}" if category else source_suite
            row["_source_artifact_dir"] = resolve_artifact_dir(row, run_dir, source_task_id)
            rows.append(row)
    return rows


def read_results_jsonl(path: Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    rows: list[dict[str, Any]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            item = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(item, dict):
            rows.append(item)
    return rows


def resolve_artifact_dir(row: dict[str, Any], run_dir: Path | None, source_task_id: str) -> str:
    raw = str(row.get("artifact_dir") or "").strip()
    if raw:
        path = Path(raw)
        if not path.is_absolute() and run_dir is not None:
            path = run_dir / path
        if path.exists():
            return str(path)
    if run_dir is None:
        return ""
    attempt = int(row.get("attempt") or 1)
    candidates = [run_dir / "tasks" / source_task_id]
    if attempt > 1:
        candidates.insert(0, run_dir / "tasks" / source_task_id / f"attempt_{attempt}")
    for path in candidates:
        if path.exists():
            return str(path)
    return ""


def unique_report_task_id(suite_key: str, task_id: str, attempt: int, used_ids: set[str]) -> str:
    attempt_suffix = f"-attempt-{attempt}" if attempt > 1 else ""
    raw = f"{suite_key}-{task_id}{attempt_suffix}"
    digest = hashlib.sha1(raw.encode("utf-8")).hexdigest()[:8]
    slug = re.sub(r"[^a-z0-9_.-]+", "-", raw.lower()).strip("-_.")
    slug = slug[:56].strip("-_.") or "task"
    candidate = f"{slug}-{digest}"
    suffix = 2
    while candidate in used_ids:
        candidate = f"{slug}-{digest}-{suffix}"
        suffix += 1
    used_ids.add(candidate)
    return candidate


def fallback_report_row(
    suite_result: dict[str, Any],
    job_id: str,
    index: int,
    used_ids: set[str],
) -> dict[str, Any] | None:
    task_id = str(suite_result.get("task_id") or "").strip()
    if not task_id:
        return None
    suite = str(suite_result.get("suite") or "")
    status = "skipped" if suite_result.get("stopped") else "passed" if suite_result.get("exit_code") == 0 else "failed"
    report_task_id = unique_report_task_id(suite, task_id or f"task-{index}", 1, used_ids)
    error = str(suite_result.get("error") or "")
    return {
        "suite": suite,
        "run_id": JOB_REPORT_RUN_ID,
        "task_id": report_task_id,
        "category": suite,
        "attempt": 1,
        "status": status,
        "rubric": [],
        "rubric_pass_count": 0,
        "rubric_total": 0,
        "metrics": {
            "source_task_id": task_id,
            "source_suite": suite,
            "source_job_id": job_id,
            "success": None,
            "agent_eligible": False,
            "failure_class": "environment",
            **({"error": error} if error else {}),
        },
        "artifact_dir": "",
        "_source_artifact_dir": "",
    }


def totals_from_report_rows(rows: list[dict[str, Any]]) -> dict[str, int]:
    totals = {"tasks": len(rows), "passed": 0, "failed": 0, "skipped": 0, "judge_error": 0, "timeout": 0}
    for row in rows:
        status = str(row.get("status") or "")
        if status in totals and status != "tasks":
            totals[status] += 1
        elif status:
            totals["failed"] += 1
    return totals


def read_json_file(path: Path) -> dict[str, Any] | None:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else None
    except Exception:
        return None


def write_state(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(payload, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
    tmp.replace(path)


def update_state_status(path: Path, status: str, *, run_id: str = "") -> None:
    payload = read_json_file(path) or {}
    payload["status"] = status
    if run_id and not payload.get("run_id"):
        payload["run_id"] = run_id
    write_state(path, payload)


def tail_text(path: Path, max_bytes: int) -> str:
    try:
        with path.open("rb") as handle:
            handle.seek(0, os.SEEK_END)
            size = handle.tell()
            handle.seek(max(0, size - max_bytes))
            return handle.read().decode("utf-8", errors="replace")
    except Exception:
        return ""


def append_log(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(text.rstrip() + "\n")


def new_job_id() -> str:
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    return f"{stamp}-{os.urandom(3).hex()}"


def new_environment_id() -> str:
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    return f"mg-{stamp}-{os.urandom(3).hex()}"


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


class WebHandler(BaseHTTPRequestHandler):
    server: "BenchmarkHTTPServer"

    def do_GET(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        if path == "/":
            self._send_html(INDEX_HTML)
            return
        if path == "/api/suites":
            self._send_json({"suites": self.server.app.list_suites()})
            return
        if path.startswith("/api/suites/"):
            self._handle_get_suite(path)
            return
        if path == "/api/agent-config":
            self._send_json({"config": self.server.app.get_agent_config()})
            return
        if path == "/api/webui-settings":
            self._send_json({"settings": self.server.app.get_webui_settings()})
            return
        if path == "/api/jobs":
            self._send_json({"jobs": self.server.app.list_jobs()})
            return
        if path == "/api/environments/mobilegym":
            self._send_json({"environments": self.server.app.list_mobilegym_environments()})
            return
        if path == "/api/environments/adb-android":
            self._send_json({"environments": self.server.app.list_adb_android_environments()})
            return
        if path.startswith("/screens/jobs/"):
            self._handle_screen_viewer(path)
            return
        if path.startswith("/api/jobs/"):
            self._handle_get_job(path)
            return
        if path.startswith("/reports/"):
            self._handle_report(path)
            return
        self.send_error(HTTPStatus.NOT_FOUND)

    def do_POST(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        if path == "/api/jobs":
            try:
                payload = self._read_json()
                job = self.server.app.start_job(payload)
            except Exception as exc:
                self._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
                return
            self._send_json({"job": job}, status=HTTPStatus.CREATED)
            return
        if path == "/api/environments/mobilegym":
            try:
                payload = self._read_json()
                environment = self.server.app.start_mobilegym_environment(payload)
            except Exception as exc:
                self._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
                return
            self._send_json({"environment": environment}, status=HTTPStatus.CREATED)
            return
        if path == "/api/environments/adb-android":
            try:
                payload = self._read_json()
                environment = self.server.app.start_adb_android_environment(payload)
            except Exception as exc:
                self._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
                return
            self._send_json({"environment": environment}, status=HTTPStatus.CREATED)
            return
        if path == "/api/agent-config":
            try:
                payload = self._read_json()
                config = self.server.app.save_agent_config(payload)
            except Exception as exc:
                self._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
                return
            self._send_json({"config": config})
            return
        if path == "/api/agent-config/reset":
            try:
                config = self.server.app.reset_agent_config()
            except Exception as exc:
                self._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
                return
            self._send_json({"config": config})
            return
        if path == "/api/webui-settings":
            try:
                payload = self._read_json()
                settings = self.server.app.save_webui_settings(payload)
            except Exception as exc:
                self._send_json({"error": str(exc)}, status=HTTPStatus.BAD_REQUEST)
                return
            self._send_json({"settings": settings})
            return
        if path.startswith("/api/environments/mobilegym/") and path.endswith("/stop"):
            parts = path.strip("/").split("/")
            if len(parts) != 5:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            environment = self.server.app.stop_mobilegym_environment(parts[3])
            if environment is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"environment": environment})
            return
        if path.startswith("/api/environments/adb-android/") and path.endswith("/stop"):
            parts = path.strip("/").split("/")
            if len(parts) != 5:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            environment = self.server.app.stop_adb_android_environment(parts[3])
            if environment is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"environment": environment})
            return
        if path.startswith("/api/jobs/"):
            parts = path.strip("/").split("/")
            if len(parts) == 6 and parts[3] == "tasks" and parts[5] == "stop":
                job = self.server.app.stop_task_worker(parts[2], parts[4])
                if job is None:
                    self.send_error(HTTPStatus.NOT_FOUND)
                    return
                self._send_json({"job": job})
                return
        if path.startswith("/api/jobs/") and path.endswith("/stop"):
            parts = path.strip("/").split("/")
            if len(parts) != 4:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            job_id = parts[2]
            job = self.server.app.stop_job(job_id)
            if job is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"job": job})
            return
        if path.startswith("/api/jobs/") and path.endswith("/cancel"):
            parts = path.strip("/").split("/")
            if len(parts) != 4:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            job_id = parts[2]
            job = self.server.app.cancel_job(job_id)
            if job is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"job": job})
            return
        self.send_error(HTTPStatus.NOT_FOUND)

    def do_DELETE(self) -> None:
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        if path.startswith("/api/environments/mobilegym/"):
            parts = path.strip("/").split("/")
            if len(parts) != 4:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            environment = self.server.app.delete_mobilegym_environment(parts[3])
            if environment is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"environment": environment})
            return
        if path.startswith("/api/environments/adb-android/"):
            parts = path.strip("/").split("/")
            if len(parts) != 4:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            environment = self.server.app.delete_adb_android_environment(parts[3])
            if environment is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"environment": environment})
            return
        self.send_error(HTTPStatus.NOT_FOUND)

    def log_message(self, fmt: str, *args: Any) -> None:
        return

    def _handle_get_job(self, path: str) -> None:
        parts = path.strip("/").split("/")
        if len(parts) == 3:
            job = self.server.app.get_job(parts[2])
            if job is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_json({"job": job})
            return
        if len(parts) == 4 and parts[3] == "log":
            text = self.server.app.read_job_log(parts[2])
            if text is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_text(text)
            return
        if len(parts) == 6 and parts[3] == "tasks" and parts[5] == "log":
            text = self.server.app.read_task_log(parts[2], parts[4])
            if text is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self._send_text(text)
            return
        if len(parts) == 6 and parts[3] == "tasks" and parts[5] == "screen":
            payload = self.server.app.task_screen_payload(parts[2], parts[4])
            if payload is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            status = HTTPStatus.OK if payload.get("ok") is not False else HTTPStatus.BAD_GATEWAY
            self._send_json(payload, status=status)
            return
        self.send_error(HTTPStatus.NOT_FOUND)

    def _handle_get_suite(self, path: str) -> None:
        parts = path.strip("/").split("/")
        if len(parts) < 3:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        # URL decode the suite key to handle encoded slashes and special characters
        suite_key = urllib.parse.unquote("/".join(parts[2:]))
        suite = self.server.app.get_suite_detail(suite_key)
        if suite is None:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self._send_json({"suite": suite})

    def _handle_screen_viewer(self, path: str) -> None:
        parts = path.strip("/").split("/")
        if len(parts) != 5 or parts[0] != "screens" or parts[1] != "jobs" or parts[3] != "tasks":
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        job = self.server.app.get_job(parts[2])
        if job is None:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        records = job.get("task_records") if isinstance(job.get("task_records"), list) else []
        if not any(isinstance(record, dict) and record.get("id") == parts[4] for record in records):
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self._send_html(TASK_SCREEN_HTML)

    def _handle_report(self, path: str) -> None:
        parts = path.strip("/").split("/")
        if len(parts) != 4 or parts[0] != "reports" or parts[3] != "report.html":
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        job_id, run_id = parts[1], parts[2]
        job = self.server.app.get_job(job_id)
        if job is None:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        raw_runs_dir = Path(job["raw_runs_dir"]).resolve()
        report = (raw_runs_dir / run_id / "report.html").resolve()
        if raw_runs_dir not in report.parents or not report.exists():
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self._send_html(report.read_text(encoding="utf-8"))

    def _read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or "0")
        if length <= 0:
            return {}
        data = json.loads(self.rfile.read(length).decode("utf-8"))
        if not isinstance(data, dict):
            raise ValueError("request body must be an object")
        return data

    def _send_json(self, payload: dict[str, Any], status: HTTPStatus = HTTPStatus.OK) -> None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_html(self, text: str) -> None:
        data = text.encode("utf-8")
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_text(self, text: str) -> None:
        data = text.encode("utf-8")
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class BenchmarkHTTPServer(ThreadingHTTPServer):
    def __init__(self, server_address: tuple[str, int], app: BenchmarkWebApp):
        super().__init__(server_address, WebHandler)
        self.app = app


def serve(config: WebUIConfig, host: str, port: int) -> None:
    app = BenchmarkWebApp(config)
    server = BenchmarkHTTPServer((host, port), app)
    bound_host, bound_port = server.server_address
    print(f"Benchmark Web UI: http://{bound_host}:{bound_port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        app.shutdown()
        server.server_close()


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="benchmark.runner webui")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8765)
    parser.add_argument("--suites-dir", default=str(DEFAULT_SUITES_DIR))
    parser.add_argument("--runs-dir", default=str(DEFAULT_RUNS_DIR))
    parser.add_argument("--base-config-dir", default=str(DEFAULT_BASE_CONFIG_DIR))
    parser.add_argument("--agent-config", default="")
    parser.add_argument("--daemon-image", default=DEFAULT_DAEMON_IMAGE)
    parser.add_argument("--mobilegym-image", default=DEFAULT_MOBILEGYM_IMAGE)
    parser.add_argument("--no-build-daemon-image", action="store_true")
    parser.add_argument("--no-build-mobilegym-image", action="store_true")
    args = parser.parse_args(argv)
    serve(
        WebUIConfig(
            suites_dir=Path(args.suites_dir),
            runs_dir=Path(args.runs_dir),
            base_config_dir=Path(args.base_config_dir),
            agent_config_path=Path(args.agent_config) if args.agent_config else None,
            daemon_image=args.daemon_image,
            mobilegym_image=args.mobilegym_image,
            build_daemon_image=not args.no_build_daemon_image,
            build_mobilegym_image=not args.no_build_mobilegym_image,
        ),
        args.host,
        args.port,
    )
    return 0


TASK_SCREEN_HTML = r"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Benchmark Task Screen</title>
  <style>
    :root { color-scheme: light dark; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 0; background: #111; color: #f6f7f9; }
    header { display: flex; align-items: center; gap: 16px; padding: 12px 16px; background: #1b1d22; border-bottom: 1px solid #333842; }
    header strong { font-size: 14px; }
    header span { color: #b8c0cc; font-size: 13px; }
    main { display: grid; grid-template-columns: minmax(0, 1fr) 280px; min-height: calc(100vh - 46px); }
    .screen { display: grid; place-items: center; padding: 16px; overflow: auto; }
    img { max-width: min(100%, 420px); width: auto; height: auto; background: #000; box-shadow: 0 12px 40px rgba(0, 0, 0, .45); }
    .placeholder { color: #9aa3af; border: 1px dashed #46515f; border-radius: 8px; padding: 24px; }
    aside { border-left: 1px solid #333842; background: #17191f; padding: 14px; overflow: auto; }
    h2 { margin: 0 0 10px; font-size: 13px; color: #dfe4ea; }
    dl { display: grid; grid-template-columns: 88px 1fr; gap: 6px 10px; margin: 0 0 18px; font-size: 12px; }
    dt { color: #8d98a7; }
    dd { margin: 0; color: #f3f5f8; overflow-wrap: anywhere; }
    .action { border-top: 1px solid #2c313a; padding: 8px 0; font-size: 12px; }
    .action strong { display: block; color: #f3f5f8; }
    .action span { color: #98a2b3; overflow-wrap: anywhere; }
    .error { color: #ffb4ab; }
    @media (max-width: 760px) {
      main { grid-template-columns: 1fr; }
      aside { border-left: 0; border-top: 1px solid #333842; }
    }
  </style>
</head>
<body>
  <header>
    <strong>Benchmark Task Screen</strong>
    <span id="status">connecting</span>
    <span id="taskId"></span>
    <span id="updated"></span>
  </header>
  <main>
    <section class="screen">
      <img id="shot" alt="Current task screenshot" hidden>
      <div id="placeholder" class="placeholder">Waiting for a screenshot.</div>
    </section>
    <aside>
      <h2>Task State</h2>
      <dl>
        <dt>Task</dt><dd id="taskState">unknown</dd>
        <dt>Backend</dt><dd id="backend">unknown</dd>
        <dt>Seq</dt><dd id="seq">none</dd>
        <dt>Size</dt><dd id="size">none</dd>
      </dl>
    </aside>
  </main>
  <script>
    const statusEl = document.getElementById('status');
    const taskIdEl = document.getElementById('taskId');
    const taskStateEl = document.getElementById('taskState');
    const updatedEl = document.getElementById('updated');
    const backendEl = document.getElementById('backend');
    const seqEl = document.getElementById('seq');
    const sizeEl = document.getElementById('size');
    const shotEl = document.getElementById('shot');
    const placeholderEl = document.getElementById('placeholder');
    const parts = window.location.pathname.split('/').filter(Boolean);
    const jobId = parts[2] || '';
    const taskRecordId = parts[4] || '';
    const screenApi = `/api/jobs/${encodeURIComponent(jobId)}/tasks/${encodeURIComponent(taskRecordId)}/screen`;
    taskIdEl.textContent = taskRecordId ? `task ${taskRecordId}` : '';
    taskStateEl.textContent = taskRecordId || 'unknown';

    async function refresh() {
      try {
        const res = await fetch(screenApi, {cache: 'no-store'});
        const body = await res.json();
        if (!res.ok || !body.ok) throw new Error(body.error?.message || res.statusText);
        const data = body.data || {};
        const meta = data.meta || {};
        const capture = data.capture_info || {};
        statusEl.textContent = 'ok';
        statusEl.className = '';
        backendEl.textContent = capture.capture_backend || 'unknown';
        seqEl.textContent = meta.seq == null ? 'none' : String(meta.seq);
        updatedEl.textContent = new Date().toLocaleTimeString();
        if (data.image) {
          const format = meta.pixel_format === 'png' ? 'png' : 'jpeg';
          shotEl.src = `data:image/${format};base64,${data.image}`;
          shotEl.hidden = false;
          placeholderEl.hidden = true;
          sizeEl.textContent = `${meta.width || '?'} x ${meta.height || '?'}`;
        } else {
          shotEl.hidden = true;
          placeholderEl.hidden = false;
          placeholderEl.textContent = 'Waiting for a screenshot.';
          sizeEl.textContent = 'none';
        }
      } catch (err) {
        statusEl.textContent = String(err.message || err);
        statusEl.className = 'error';
      }
    }

    refresh();
    setInterval(refresh, 1000);
  </script>
</body>
</html>
"""


INDEX_HTML = r"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Aiden Benchmark</title>
  <style>
    :root {
      --bg: #f4f4f4;
      --layer: #ffffff;
      --layer-alt: #f4f4f4;
      --field: #f4f4f4;
      --border: #e0e0e0;
      --border-strong: #8d8d8d;
      --text: #161616;
      --muted: #525252;
      --muted-2: #6f6f6f;
      --blue: #0f62fe;
      --blue-hover: #0353e9;
      --gray-button: #393939;
      --gray-button-hover: #4c4c4c;
      --green: #198038;
      --red: #da1e28;
      --orange: #ba4e00;
      --purple: #8a3ffc;
      --focus: #0f62fe;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: var(--bg);
      color: var(--text);
      font-family: "IBM Plex Sans", Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      font-size: 14px;
      line-height: 1.4;
      letter-spacing: 0;
    }
    .topbar {
      height: 48px;
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0 16px;
      background: #161616;
      color: #fff;
    }
    .brand {
      display: flex;
      align-items: center;
      gap: 12px;
      min-width: 0;
    }
    .brand-mark {
      width: 20px;
      height: 20px;
      background: var(--blue);
      display: grid;
      place-items: center;
      font-size: 12px;
      font-weight: 700;
    }
    .brand-title { font-size: 15px; font-weight: 600; }
    .header-meta {
      color: #c6c6c6;
      font-size: 12px;
      text-transform: uppercase;
      letter-spacing: 0.02em;
    }
    .layout {
      display: grid;
      grid-template-columns: 600px minmax(0, 1fr);
      min-height: calc(100vh - 48px);
      gap: 1px;
      background: var(--border);
    }
    .side,
    .workspace {
      background: var(--bg);
      min-width: 0;
    }
    .side {
      display: grid;
      align-content: start;
      gap: 1px;
    }
    .workspace {
      display: grid;
      grid-template-rows: auto auto auto auto minmax(360px, 1fr);
      gap: 1px;
    }
    .tile {
      background: var(--layer);
      border-radius: 0;
      padding: 16px;
      min-width: 0;
    }
    .tile-header,
    .toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 16px;
    }
    .tile-title {
      margin: 0;
      font-size: 16px;
      font-weight: 600;
      letter-spacing: 0;
    }
    .tile-kicker {
      margin-top: 2px;
      color: var(--muted);
      font-size: 12px;
    }
    .form-grid {
      display: grid;
      grid-template-columns: 1fr;
      gap: 12px;
      align-items: stretch;
    }
    .field {
      display: grid;
      gap: 6px;
      min-width: 0;
      background: var(--layer);
    }
    .form-grid button {
      justify-self: start;
      min-width: 120px;
    }
    .field label,
    .check-label {
      color: var(--muted);
      font-size: 12px;
      font-weight: 600;
    }
    input[type="text"],
    input:not([type]),
    input[type="url"],
    input[type="number"],
    input[type="password"],
    input[type="search"],
    select,
    textarea {
      width: 100%;
      border: 0;
      border-bottom: 1px solid var(--border-strong);
      border-radius: 0;
      color: var(--text);
      background: var(--field);
      font: inherit;
    }
    input[type="text"],
    input:not([type]),
    input[type="url"],
    input[type="number"],
    input[type="password"],
    input[type="search"],
    select {
      height: 40px;
      padding: 0 12px;
    }
    textarea {
      min-height: 220px;
      max-height: 360px;
      resize: vertical;
      padding: 12px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 12px;
      line-height: 1.45;
    }
    input:focus,
    textarea:focus,
    button:focus,
    a:focus {
      outline: 2px solid var(--focus);
      outline-offset: -2px;
    }
    button {
      height: 40px;
      border: 0;
      border-radius: 0;
      padding: 0 16px;
      background: var(--gray-button);
      color: #fff;
      font: inherit;
      font-weight: 600;
      cursor: pointer;
      white-space: nowrap;
    }
    button:hover { background: var(--gray-button-hover); }
    button.primary { background: var(--blue); color: #fff; }
    button.primary:hover { background: var(--blue-hover); }
    button.danger { background: transparent; color: var(--red); padding: 0 8px; }
    button.danger:hover { background: #fff1f1; }
    button:disabled { opacity: 0.45; cursor: not-allowed; }
    .ghost-button {
      background: transparent;
      color: var(--blue);
      padding: 0 8px;
    }
    .ghost-button:hover { background: #edf5ff; }
    .config-actions {
      display: flex;
      align-items: center;
      gap: 8px;
      margin-top: 12px;
    }
    .config-actions span {
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .segmented {
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      border: 1px solid var(--border-strong);
      margin-bottom: 16px;
    }
    .segmented button {
      height: 32px;
      min-width: 0;
      padding: 0 8px;
      background: var(--layer);
      color: var(--text);
      border-right: 1px solid var(--border-strong);
      overflow: hidden;
      text-overflow: ellipsis;
      font-weight: 500;
    }
    .segmented button:last-child { border-right: 0; }
    .segmented button.active {
      background: var(--gray-button);
      color: #fff;
    }
    .env-panel[hidden] { display: none; }
    .table-wrap {
      border-top: 1px solid var(--border);
      max-height: 430px;
      overflow: auto;
      background: var(--layer);
    }
    .suite-table-wrap { max-height: calc(100vh - 180px); min-height: 520px; }
    .job-table-wrap { max-height: 240px; }
    .task-table-wrap { max-height: 280px; min-height: 180px; }
    table {
      width: 100%;
      border-collapse: collapse;
      table-layout: fixed;
    }
    thead { background: #e0e0e0; }
    th, td {
      border-bottom: 1px solid var(--border);
      padding: 10px 12px;
      text-align: left;
      vertical-align: middle;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    th {
      height: 40px;
      color: var(--muted);
      font-size: 12px;
      font-weight: 600;
    }
    tbody tr { background: var(--layer); }
    tbody tr:hover { background: #f4f4f4; }
    tbody tr.selected-row { box-shadow: inset 3px 0 0 var(--blue); }
    td:first-child input[type="checkbox"],
    td:first-child input[type="radio"] {
      display: block;
      margin: 0 auto;
    }
    .muted { color: var(--muted); }
    .cell-main {
      display: grid;
      gap: 2px;
      min-width: 0;
    }
    .cell-main span,
    .cell-main small {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .cell-main small {
      color: var(--muted-2);
      font-size: 12px;
    }
    .status {
      display: inline-flex;
      align-items: center;
      min-height: 24px;
      padding: 0 8px;
      border: 0;
      background: #e0e0e0;
      color: #393939;
      font-size: 12px;
      font-weight: 600;
      text-transform: uppercase;
    }
    .status.passed { background: #defbe6; color: var(--green); }
    .status.failed { background: #fff1f1; color: var(--red); }
    .status.running, .status.queued, .status.starting, .status.starting_agent, .status.preparing, .status.building { background: #edf5ff; color: var(--blue); }
    .status.canceled, .status.stopping { background: #fff8e1; color: var(--orange); }
    .status.stopped, .status.device { background: #e0e0e0; color: #525252; }
    .status.unhealthy { background: #fff1f1; color: var(--orange); }
    .status.mobilegym { background: #e8daff; color: var(--purple); }
    .status-actions {
      display: flex;
      gap: 8px;
      align-items: center;
    }
    .inline-actions {
      display: flex;
      gap: 10px;
      align-items: center;
      min-width: 0;
    }
    .env-actions {
      justify-content: flex-end;
      gap: 8px;
      overflow: visible;
    }
    .env-actions button { flex: 0 0 auto; }
    .table-button {
      height: 28px;
      padding: 0 8px;
      min-width: 0;
      font-size: 12px;
    }
    .suite-category-group {
      border: 1px solid var(--border);
      margin-bottom: 12px;
      border-radius: 4px;
      overflow: hidden;
      background: var(--layer);
    }
    .suite-category-group:last-child {
      margin-bottom: 0;
    }
    .suite-category-header {
      position: sticky;
      top: 0;
      background: #f0f0f0;
      padding: 10px 12px;
      font-size: 13px;
      font-weight: 600;
      color: var(--text);
      cursor: pointer;
      user-select: none;
      display: flex;
      align-items: center;
      gap: 8px;
      z-index: 1;
      border-bottom: 1px solid var(--border);
    }
    .suite-category-header:hover {
      background: #e8e8e8;
    }
    .suite-category-header::before {
      content: '▼';
      font-size: 10px;
      transition: transform 0.2s;
      color: var(--muted);
    }
    .suite-category-header.collapsed::before {
      transform: rotate(-90deg);
    }
    .suite-category-header.collapsed {
      border-bottom: 0;
    }
    .suite-category-body {
      display: block;
    }
    .suite-category-body.collapsed {
      display: none;
    }
    .suite-category-group table {
      width: 100%;
      margin: 0;
    }
    .suite-category-group .cell-main {
      min-width: 0;
      display: block;
    }
    .suite-category-group .cell-main span {
      display: block;
      font-weight: 500;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .suite-category-group .cell-main small {
      display: block;
      color: var(--muted-2);
      font-size: 11px;
      margin-top: 2px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .suite-category-row {
      background: #f0f0f0;
      cursor: pointer;
      user-select: none;
    }
    .suite-category-row:hover {
      background: #e8e8e8;
    }
    .suite-category-header-cell {
      padding: 10px 12px !important;
      font-size: 13px;
      font-weight: 600;
      color: var(--text);
    }
    .category-arrow {
      display: inline-block;
      width: 16px;
      font-size: 10px;
      color: var(--muted);
      transition: transform 0.2s;
    }
    .suite-row {
      background: var(--layer);
    }
    .progress {
      height: 8px;
      background: #e0e0e0;
      overflow: hidden;
    }
    .progress > div { height: 100%; width: 0%; background: var(--blue); transition: width 160ms linear; }
    .summary-strip {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 16px;
      align-items: center;
    }
    .run-config-grid {
      display: grid;
      grid-template-columns: 1fr;
      gap: 16px;
      align-items: stretch;
    }
    .run-config-row {
      display: grid;
      grid-template-columns: minmax(220px, 1fr) minmax(360px, 1.2fr) auto;
      gap: 16px;
      align-items: end;
    }
    .run-meta {
      display: grid;
      gap: 4px;
      min-width: 0;
    }
    .run-meta strong {
      font-size: 20px;
      font-weight: 500;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .run-actions {
      display: flex;
      gap: 12px;
      align-items: center;
      justify-content: flex-start;
    }
    .judge-inline {
      display: grid;
      grid-template-columns: auto minmax(220px, 1fr) minmax(240px, 1fr) minmax(180px, 0.8fr);
      gap: 12px;
      align-items: end;
    }
    .check-label {
      display: flex;
      gap: 8px;
      align-items: center;
      min-height: 40px;
      white-space: nowrap;
    }
    .check-label input { width: 16px; height: 16px; margin: 0; }
    .metric-grid {
      display: grid;
      grid-template-columns: repeat(6, minmax(0, 1fr));
      gap: 1px;
      background: var(--border);
      margin-top: 16px;
    }
    .metric {
      background: var(--layer-alt);
      min-height: 88px;
      padding: 12px;
    }
    .metric span {
      display: block;
      color: var(--muted);
      font-size: 12px;
      margin-bottom: 8px;
    }
    .metric strong {
      display: block;
      font-size: 28px;
      line-height: 1;
      font-weight: 400;
    }
    .detail-grid {
      display: grid;
      grid-template-columns: minmax(360px, 0.95fr) minmax(420px, 1.05fr);
      gap: 1px;
      min-width: 0;
      min-height: 0;
      background: var(--border);
    }
    .detail-grid .tile {
      min-height: 0;
    }
    .detail-grid textarea {
      min-height: 300px;
      max-height: 520px;
    }
    .modal-backdrop {
      position: fixed;
      inset: 0;
      z-index: 20;
      display: grid;
      place-items: center;
      padding: 24px;
      background: rgba(22, 22, 22, 0.42);
    }
    .modal-backdrop[hidden] { display: none; }
    .modal {
      width: min(1120px, calc(100vw - 32px));
      max-height: calc(100vh - 48px);
      overflow: auto;
      background: var(--layer);
      border: 1px solid var(--border-strong);
      box-shadow: 0 16px 48px rgba(0, 0, 0, 0.22);
    }
    .modal-header,
    .modal-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      padding: 16px;
      border-bottom: 1px solid var(--border);
    }
    .modal-footer {
      justify-content: flex-end;
      border-top: 1px solid var(--border);
      border-bottom: 0;
    }
    .modal-body {
      padding: 16px;
    }
    .modal-env-grid {
      display: grid;
      grid-template-columns: minmax(360px, 0.5fr) minmax(420px, 1fr);
      gap: 16px;
      align-items: start;
    }
    .modal-env-table { max-height: 360px; border: 1px solid var(--border); }
    pre {
      margin: 0;
      min-height: 300px;
      height: 100%;
      max-height: 420px;
      overflow: auto;
      border: 0;
      background: #262626;
      color: #f4f4f4;
      padding: 16px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 12px;
      line-height: 1.45;
      white-space: pre-wrap;
    }
    a { color: var(--blue); text-decoration: none; }
    a:hover { text-decoration: underline; }
    .empty-row {
      color: var(--muted);
      height: 48px;
    }
    .task-detail {
      border: 1px solid var(--border);
      margin-bottom: 16px;
      background: var(--layer);
    }
    .task-detail-header {
      background: #f0f0f0;
      padding: 12px 16px;
      font-weight: 600;
      border-bottom: 1px solid var(--border);
      cursor: pointer;
      user-select: none;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .task-detail-header:hover {
      background: #e8e8e8;
    }
    .task-detail-header::before {
      content: '▼';
      font-size: 10px;
      transition: transform 0.2s;
      color: var(--muted);
    }
    .task-detail-header.collapsed::before {
      transform: rotate(-90deg);
    }
    .task-detail-body {
      padding: 16px;
      display: block;
    }
    .task-detail-body.collapsed {
      display: none;
    }
    .detail-section {
      margin-bottom: 16px;
    }
    .detail-section:last-child {
      margin-bottom: 0;
    }
    .detail-section h3 {
      margin: 0 0 8px;
      font-size: 13px;
      font-weight: 600;
      color: var(--muted);
      text-transform: uppercase;
    }
    .detail-section pre {
      margin: 0;
      min-height: auto;
      max-height: 300px;
      padding: 12px;
      font-size: 12px;
      line-height: 1.45;
    }
    .detail-list {
      display: grid;
      gap: 8px;
    }
    .detail-item {
      display: grid;
      grid-template-columns: 160px 1fr;
      gap: 12px;
      font-size: 13px;
    }
    .detail-item dt {
      color: var(--muted);
      font-weight: 600;
    }
    .detail-item dd {
      margin: 0;
      color: var(--text);
      word-break: break-word;
    }
    .rubric-list {
      list-style: none;
      padding: 0;
      margin: 0;
      display: grid;
      gap: 8px;
    }
    .rubric-item {
      background: var(--layer-alt);
      padding: 10px 12px;
      border-left: 3px solid var(--blue);
      font-size: 13px;
    }
    .rubric-item strong {
      display: block;
      margin-bottom: 4px;
      color: var(--text);
    }
    .rubric-item span {
      color: var(--muted-2);
    }
    @media (max-width: 980px) {
      .layout { grid-template-columns: 1fr; }
      .suite-table-wrap { max-height: 360px; }
      .metric-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); }
      .summary-strip { grid-template-columns: 1fr; }
      .run-config-row { grid-template-columns: 1fr; align-items: stretch; }
      .judge-inline { grid-template-columns: 1fr; align-items: stretch; }
      .detail-grid { grid-template-columns: 1fr; }
      .modal-env-grid { grid-template-columns: 1fr; }
    }
    @media (max-width: 1120px) {
      .modal-env-grid { grid-template-columns: 1fr; }
    }
  </style>
</head>
<body>
  <header class="topbar">
    <div class="brand">
      <div class="brand-mark">A</div>
      <div class="brand-title">Aiden Benchmark</div>
    </div>
    <div id="headerStatus" class="header-meta">Idle</div>
  </header>
  <main class="layout">
    <aside class="side">
      <section class="tile">
        <div class="toolbar">
          <div>
            <h2 class="tile-title">Suites</h2>
            <div class="tile-kicker">Select one or more suites</div>
          </div>
          <input id="suiteFilter" type="search" style="max-width:172px" placeholder="Filter">
        </div>
        <div class="table-wrap suite-table-wrap" id="suitesContainer"></div>
      </section>
    </aside>

    <section class="workspace">
      <section class="tile">
        <div class="run-config-grid">
          <div class="run-config-row">
            <div class="run-meta">
              <span class="tile-kicker">Run configuration</span>
              <strong><span id="selectedEnvLabel">No environment</span></strong>
              <span class="muted"><span id="selectedSuitesLabel">0 suites</span> selected - <span id="selectedJudgeLabel">judge enabled</span></span>
            </div>
            <div class="judge-inline">
              <label class="check-label"><input id="judgeEnabled" type="checkbox" checked> Enable judge</label>
              <div class="field"><label for="judgeModel">Judge model</label><input id="judgeModel" autocomplete="off" placeholder="anthropic/claude-sonnet-4-6"></div>
              <div class="field"><label for="judgeBaseUrl">Base URL</label><input id="judgeBaseUrl" autocomplete="off" placeholder="https://openrouter.ai/api/v1"></div>
              <div class="field"><label for="judgeApiKey">API key</label><input id="judgeApiKey" type="password" autocomplete="off" placeholder="AIDEN_BENCHMARK_JUDGE_API_KEY"></div>
            </div>
          </div>
          <div class="run-actions">
            <div class="field" style="max-width: 120px;">
              <label for="metricsK">Attempts (k)</label>
              <input id="metricsK" type="number" min="1" max="10" value="1" autocomplete="off" style="text-align: center;">
            </div>
            <button id="runBtn" class="primary">Run selected suites</button>
          </div>
        </div>
      </section>

      <section class="tile">
        <div class="tile-header">
          <div>
            <h2 class="tile-title">Progress</h2>
            <div id="activeJobLabel" class="tile-kicker">No active job</div>
          </div>
          <div class="status-actions">
            <span id="activeJobStatus" class="status">idle</span>
            <button id="activeStopJob" class="danger" type="button" hidden>Stop</button>
          </div>
        </div>
        <div class="progress"><div id="progressBar"></div></div>
        <div class="metric-grid">
          <div class="metric"><span class="muted">Tasks</span><strong id="mTasks">0</strong></div>
          <div class="metric"><span class="muted">Passed</span><strong id="mPassed">0</strong></div>
          <div class="metric"><span class="muted">Failed</span><strong id="mFailed">0</strong></div>
          <div class="metric"><span class="muted">Skipped</span><strong id="mSkipped">0</strong></div>
          <div class="metric"><span class="muted">Judge</span><strong id="mJudge">0</strong></div>
          <div class="metric"><span class="muted">Timeout</span><strong id="mTimeout">0</strong></div>
        </div>
      </section>

      <section class="tile">
        <div class="tile-header">
          <div>
            <h2 class="tile-title">Task workers</h2>
            <div class="tile-kicker">Isolated environment task records</div>
          </div>
        </div>
        <div class="table-wrap task-table-wrap">
          <table>
            <thead><tr><th>Task</th><th style="width:132px">Status</th><th>Agent</th><th style="width:220px">Screen / log</th></tr></thead>
            <tbody id="taskRows"></tbody>
          </table>
        </div>
      </section>

      <section class="tile">
        <div class="tile-header">
          <div>
            <h2 class="tile-title">Jobs</h2>
            <div class="tile-kicker">Recent benchmark runs</div>
          </div>
          <select id="jobCategoryFilter" style="max-width:200px; padding:4px 8px; border:1px solid var(--border); background:var(--field); border-radius:4px">
            <option value="">All categories</option>
          </select>
        </div>
        <div class="table-wrap job-table-wrap">
          <table>
            <thead><tr><th>Job</th><th style="width:220px">Suite</th><th>Environment</th><th style="width:120px">Status</th><th style="width:120px">Report</th><th style="width:96px"></th></tr></thead>
            <tbody id="jobRows"></tbody>
          </table>
        </div>
      </section>

      <div class="detail-grid">
        <section class="tile">
          <div class="tile-header">
            <div>
              <h2 class="tile-title">Agent config</h2>
              <div id="agentConfigPath" class="tile-kicker">agent.toml</div>
            </div>
          </div>
          <div class="field">
            <label for="agentConfigText">agent.toml</label>
            <textarea id="agentConfigText" spellcheck="false" readonly></textarea>
          </div>
          <div class="config-actions">
            <button id="editAgentConfig" class="primary" type="button">Edit</button>
            <button id="saveAgentConfig" class="primary" type="button">Save</button>
            <button id="resetAgentConfig" class="ghost-button" type="button">Reset</button>
            <span id="agentConfigStatus" class="muted"></span>
          </div>
        </section>

        <section class="tile">
          <div class="tile-header">
            <div>
              <h2 class="tile-title">Log</h2>
              <div id="logScopeLabel" class="tile-kicker">Runner and daemon output</div>
            </div>
            <button id="showJobLog" class="ghost-button table-button" type="button">Job log</button>
          </div>
          <pre id="logBox"></pre>
        </section>
      </div>
    </section>
  </main>
  <div id="suiteDetailDialog" class="modal-backdrop" hidden>
    <section class="modal" role="dialog" aria-modal="true" aria-labelledby="suiteDetailTitle">
      <div class="modal-header">
        <div>
          <h2 id="suiteDetailTitle" class="tile-title">Suite Details</h2>
          <div id="suiteDetailSubtitle" class="tile-kicker"></div>
        </div>
        <button id="closeSuiteDetail" class="ghost-button table-button" type="button">Close</button>
      </div>
      <div class="modal-body">
        <div id="suiteDetailContent"></div>
      </div>
      <div class="modal-footer">
        <button id="cancelSuiteDetail" class="ghost-button" type="button">Close</button>
      </div>
    </section>
  </div>
  <div id="runEnvDialog" class="modal-backdrop" hidden>
    <section class="modal" role="dialog" aria-modal="true" aria-labelledby="runEnvTitle">
      <div class="modal-header">
        <div>
          <h2 id="runEnvTitle" class="tile-title">Choose Environment</h2>
          <div class="tile-kicker">device / mobilegym / adb android</div>
        </div>
        <button id="closeRunEnv" class="ghost-button table-button" type="button">Close</button>
      </div>
      <div class="modal-body">
        <div class="modal-env-grid">
          <section>
            <div class="segmented" role="tablist" aria-label="Environment type">
              <button id="deviceTab" class="active" type="button">Device</button>
              <button id="mobilegymTab" type="button">MobileGym</button>
              <button id="adbAndroidTab" type="button">ADB Android</button>
            </div>
            <div id="devicePanel" class="env-panel">
              <div class="form-grid">
                <div class="field"><label for="envName">Name</label><input id="envName" autocomplete="off"></div>
                <div class="field"><label for="envEndpoint">Endpoint</label><input id="envEndpoint" placeholder="http://host:8080" autocomplete="off"></div>
                <button id="saveEnv" class="primary" type="button">Save device</button>
              </div>
            </div>
            <div id="mobilegymPanel" class="env-panel" hidden>
              <div class="form-grid">
                <div class="field"><label for="mobilegymName">Name</label><input id="mobilegymName" placeholder="MobileGym" autocomplete="off"></div>
                <div class="field"><label for="mobilegymParallelEnvs">Envs</label><input id="mobilegymParallelEnvs" type="number" min="1" step="1" value="5"></div>
                <button id="startMobileGym" class="primary" type="button">Start MobileGym</button>
              </div>
            </div>
            <div id="adbAndroidPanel" class="env-panel" hidden>
              <div class="form-grid">
                <div class="field"><label for="adbAndroidName">Name</label><input id="adbAndroidName" placeholder="ADB Android" autocomplete="off"></div>
                <div class="field"><label for="adbAndroidSerial">ADB Serial</label><input id="adbAndroidSerial" placeholder="127.0.0.1:6555" value="127.0.0.1:6555" autocomplete="off"></div>
                <div class="field"><label for="adbAndroidBridgePort">Bridge Port</label><input id="adbAndroidBridgePort" type="number" min="0" step="1" placeholder="auto"></div>
                <button id="startADBAndroid" class="primary" type="button">Start ADB Android</button>
              </div>
            </div>
          </section>
          <section>
            <div class="table-wrap modal-env-table">
              <table>
                <thead><tr><th style="width:40px"></th><th style="width:128px">Environment</th><th>Endpoint</th><th style="width:160px"></th></tr></thead>
                <tbody id="envRows"></tbody>
              </table>
            </div>
          </section>
        </div>
      </div>
      <div class="modal-footer">
        <button id="cancelRunEnv" class="ghost-button" type="button">Cancel</button>
        <button id="confirmRunBtn" class="primary" type="button">Run selected suites</button>
      </div>
    </section>
  </div>
  <script>
    const DEFAULT_JUDGE_MODEL = 'anthropic/claude-sonnet-4-6';
    const DEFAULT_JUDGE_BASE_URL = 'https://openrouter.ai/api/v1';
    let deviceEnvironments = [];
    let mobilegymEnvironments = [];
    let adbAndroidEnvironments = [];
    let selectedEnvironmentId = '';
    let editingDeviceEnvId = null;
    let suites = [];
    let selectedSuites = new Set();
    let jobs = [];
    let activeJobId = null;
    let activeTaskLogId = null;
    let agentConfigDirty = false;
    let agentConfigLoaded = false;
    let agentConfigEditing = false;

    function uid(){ return Date.now().toString(36) + Math.random().toString(36).slice(2, 8); }
    function normalizeDeviceEnv(env, index){
      return {
        id: String(env.id || uid()),
        name: String(env.name || `Device ${index + 1}`),
        endpoint: String(env.endpoint || ''),
        type: 'device',
        status: 'device'
      };
    }
    async function loadWebuiSettings(){
      try {
        const res = await fetch('/api/webui-settings');
        const body = await res.json();
        if(!res.ok) throw new Error(body.error || 'failed to load settings');
        applyWebuiSettings(body.settings || {});
      } catch (err) {
        document.getElementById('logBox').textContent = err.message || String(err);
        applyWebuiSettings({});
      }
    }
    function applyWebuiSettings(settings){
      deviceEnvironments = (Array.isArray(settings.device_environments) ? settings.device_environments : [])
        .map(normalizeDeviceEnv)
        .filter(env => env.name && env.endpoint);
      selectedEnvironmentId = String(settings.selected_environment_id || '');
      const judge = settings.judge || {};
      document.getElementById('judgeEnabled').checked = judge.enabled !== false;
      document.getElementById('judgeModel').value = String(judge.model || DEFAULT_JUDGE_MODEL);
      document.getElementById('judgeBaseUrl').value = String(judge.base_url || DEFAULT_JUDGE_BASE_URL);
      const keyInput = document.getElementById('judgeApiKey');
      keyInput.value = '';
      keyInput.placeholder = judge.has_api_key ? 'Saved; leave blank to keep' : 'AIDEN_BENCHMARK_JUDGE_API_KEY';
      const metricsK = parseInt(settings.metrics_k, 10) || 1;
      document.getElementById('metricsK').value = metricsK;
      syncJudgePanel();
      renderEnvs();
      syncRunState();
    }
    async function saveWebuiSettings(options = {}){
      const judge = currentJudgeSettings();
      const judgePayload = {enabled: judge.enabled, model: judge.model, base_url: judge.baseUrl};
      if(judge.apiKey) judgePayload.api_key = judge.apiKey;
      const metricsK = parseInt(document.getElementById('metricsK').value, 10) || 1;
      const payload = {
        judge: judgePayload,
        metrics_k: metricsK,
        device_environments: deviceEnvironments.map(env => ({id: env.id, name: env.name, endpoint: env.endpoint})),
        selected_environment_id: selectedEnvironmentId
      };
      try {
        const res = await fetch('/api/webui-settings', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(payload)
        });
        const body = await res.json();
        if(!res.ok) throw new Error(body.error || 'failed to save settings');
        if(!options.keepInputs) applyWebuiSettings(body.settings || {});
        return true;
      } catch (err) {
        document.getElementById('logBox').textContent = err.message || String(err);
        return false;
      }
    }
    function allEnvironments(){ return [...deviceEnvironments, ...mobilegymEnvironments, ...adbAndroidEnvironments]; }
    function envCanRun(env){ return !!env && !!env.endpoint && (env.type === 'device' || env.status === 'running'); }
    function selectedEnv(){
      const current = allEnvironments().find(env => env.id === selectedEnvironmentId);
      if(envCanRun(current)) return current;
      return allEnvironments().find(envCanRun) || null;
    }
    function setSelectedEnv(id){
      const env = allEnvironments().find(item => item.id === id);
      if(!envCanRun(env)) return;
      selectedEnvironmentId = id;
      renderEnvs();
      syncRunState();
      saveWebuiSettings({keepInputs: true});
    }
    function setEnvMode(mode){
      const isMobileGym = mode === 'mobilegym';
      const isADBAndroid = mode === 'adb_android';
      document.getElementById('deviceTab').classList.toggle('active', !isMobileGym && !isADBAndroid);
      document.getElementById('mobilegymTab').classList.toggle('active', isMobileGym);
      document.getElementById('adbAndroidTab').classList.toggle('active', isADBAndroid);
      document.getElementById('devicePanel').hidden = isMobileGym || isADBAndroid;
      document.getElementById('mobilegymPanel').hidden = !isMobileGym;
      document.getElementById('adbAndroidPanel').hidden = !isADBAndroid;
    }

    function currentJudgeSettings(){
      const enabled = document.getElementById('judgeEnabled').checked;
      const model = document.getElementById('judgeModel').value.trim() || DEFAULT_JUDGE_MODEL;
      const baseUrl = document.getElementById('judgeBaseUrl').value.trim() || DEFAULT_JUDGE_BASE_URL;
      const apiKey = document.getElementById('judgeApiKey').value.trim();
      return {enabled, model, baseUrl, apiKey};
    }

    function persistJudgeSettings(){
      syncJudgePanel();
      syncRunState();
      saveWebuiSettings({keepInputs: true});
    }

    function syncJudgePanel(){
      const enabled = document.getElementById('judgeEnabled').checked;
      document.getElementById('judgeModel').disabled = !enabled;
      document.getElementById('judgeBaseUrl').disabled = !enabled;
      document.getElementById('judgeApiKey').disabled = !enabled;
    }

    function setAgentConfigStatus(text, isError = false){
      const node = document.getElementById('agentConfigStatus');
      node.textContent = text || '';
      node.style.color = isError ? 'var(--red)' : '';
    }

    function setAgentConfigEditing(editing){
      agentConfigEditing = !!editing;
      const text = document.getElementById('agentConfigText');
      text.readOnly = !agentConfigEditing;
      document.getElementById('editAgentConfig').disabled = agentConfigEditing;
      document.getElementById('saveAgentConfig').disabled = !agentConfigEditing;
      document.getElementById('resetAgentConfig').disabled = !agentConfigEditing;
      if(agentConfigEditing){
        text.focus();
        setAgentConfigStatus(agentConfigDirty ? 'Modified' : 'Editing');
      }
    }

    function applyAgentConfig(config){
      document.getElementById('agentConfigText').value = config.content || '';
      document.getElementById('agentConfigPath').textContent = config.path || 'agent.toml';
      agentConfigDirty = false;
      agentConfigLoaded = true;
      setAgentConfigEditing(false);
      setAgentConfigStatus(config.source === 'generated' ? 'Generated' : 'Saved');
    }

    async function loadAgentConfig(){
      try {
        const res = await fetch('/api/agent-config');
        const body = await res.json();
        if(!res.ok) throw new Error(body.error || 'failed to load agent.toml');
        applyAgentConfig(body.config || {});
      } catch (err) {
        setAgentConfigStatus(err.message || String(err), true);
      }
    }

    async function saveAgentConfig(options = {}){
      const content = document.getElementById('agentConfigText').value;
      try {
        const res = await fetch('/api/agent-config', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({content})
        });
        const body = await res.json();
        if(!res.ok) throw new Error(body.error || 'failed to save agent.toml');
        applyAgentConfig(body.config || {content});
        setAgentConfigStatus(options.silent ? 'Saved for run' : 'Saved');
        return true;
      } catch (err) {
        const message = err.message || String(err);
        setAgentConfigStatus(message, true);
        document.getElementById('logBox').textContent = message;
        return false;
      }
    }

    async function resetAgentConfig(){
      const res = await fetch('/api/agent-config/reset', {method: 'POST'});
      const body = await res.json();
      if(!res.ok){
        const message = body.error || 'failed to reset agent.toml';
        setAgentConfigStatus(message, true);
        document.getElementById('logBox').textContent = message;
        return;
      }
      applyAgentConfig(body.config || {});
      setAgentConfigStatus('Reset');
    }

    function renderEnvs(){
      const tbody = document.getElementById('envRows');
      const current = selectedEnv();
      const envs = allEnvironments();
      tbody.innerHTML = '';
      if(!envs.length){
        tbody.innerHTML = '<tr><td class="empty-row" colspan="4">No environments saved</td></tr>';
      }
      envs.forEach(env => {
        const selectable = envCanRun(env);
        const isManaged = env.type === 'mobilegym' || env.type === 'adb_android';
        const displayEndpoint = isManaged ? (env.public_endpoint || env.endpoint) : env.endpoint;
        let endpointDetail = 'manual';
        if(env.type === 'mobilegym') endpointDetail = `${env.endpoint} · ${env.parallel_envs || 5} envs`;
        if(env.type === 'adb_android') endpointDetail = `adb ${env.serial || ''}`;
        const status = isManaged ? (env.status || env.type) : 'device';
        const actionHtml = env.type === 'device'
          ? `<button class="ghost-button" data-edit="${escapeHtml(env.id)}">Edit</button> <button class="danger" data-delete="${escapeHtml(env.id)}">Delete</button>`
          : managedEnvActionHtml(env);
        const tr = document.createElement('tr');
        tr.innerHTML = `<td><input type="radio" name="activeEnv" ${current && current.id === env.id ? 'checked' : ''} ${selectable ? '' : 'disabled'}></td>
          <td title="${escapeHtml(env.name)}"><div class="cell-main"><span>${escapeHtml(env.name)}</span><small>${escapeHtml(status)}</small></div></td>
          <td title="${escapeHtml(env.endpoint)}"><div class="cell-main"><span>${escapeHtml(displayEndpoint)}</span><small>${escapeHtml(endpointDetail)}</small></div></td>
          <td><div class="inline-actions env-actions">${actionHtml}</div></td>`;
        tr.querySelector('input').onchange = () => setSelectedEnv(env.id);
        const edit = tr.querySelector('[data-edit]');
        if(edit) edit.onclick = () => {
          editingDeviceEnvId = env.id;
          document.getElementById('envName').value = env.name;
          document.getElementById('envEndpoint').value = env.endpoint;
          setEnvMode('device');
        };
        const del = tr.querySelector('[data-delete]');
        if(del) del.onclick = () => {
          deviceEnvironments = deviceEnvironments.filter(e => e.id !== env.id);
          if(selectedEnvironmentId === env.id) selectedEnvironmentId = '';
          renderEnvs();
          syncRunState();
          saveWebuiSettings({keepInputs: true});
        };
        const stop = tr.querySelector('[data-stop]');
        if(stop) stop.onclick = () => env.type === 'adb_android' ? stopADBAndroid(env.id) : stopMobileGym(env.id);
        const remove = tr.querySelector('[data-remove]');
        if(remove) remove.onclick = () => env.type === 'adb_android' ? removeADBAndroid(env.id) : removeMobileGym(env.id);
        tbody.appendChild(tr);
      });
      syncRunState();
    }

    function managedEnvActionHtml(env){
      if(['building', 'starting', 'running', 'stopping'].includes(env.status)){
        return `<button class="danger" data-stop="${escapeHtml(env.id)}" ${env.status === 'stopping' ? 'disabled' : ''}>Stop</button>`;
      }
      return `<button class="danger" data-remove="${escapeHtml(env.id)}">Remove</button>`;
    }

    function saveEnvFromForm(){
      const name = document.getElementById('envName').value.trim();
      const endpoint = document.getElementById('envEndpoint').value.trim();
      if(!name || !endpoint) return;
      if(editingDeviceEnvId){
        const env = deviceEnvironments.find(e => e.id === editingDeviceEnvId);
        if(env){ env.name = name; env.endpoint = endpoint; }
      } else {
        const env = {id: uid(), name, endpoint, type: 'device', status: 'device'};
        deviceEnvironments.push(env);
        selectedEnvironmentId = env.id;
      }
      editingDeviceEnvId = null;
      document.getElementById('envName').value = '';
      document.getElementById('envEndpoint').value = '';
      renderEnvs();
      syncRunState();
      saveWebuiSettings({keepInputs: true});
    }

    async function loadMobileGymEnvironments(){
      try {
        const res = await fetch('/api/environments/mobilegym');
        const body = await res.json();
        mobilegymEnvironments = (body.environments || []).map(env => ({...env, type: 'mobilegym'}));
      } catch {
        mobilegymEnvironments = [];
      }
      renderEnvs();
      syncRunState();
    }

    async function startMobileGym(){
      const name = document.getElementById('mobilegymName').value.trim() || 'MobileGym';
      const parallelEnvs = Math.max(1, parseInt(document.getElementById('mobilegymParallelEnvs').value || '5', 10) || 5);
      const button = document.getElementById('startMobileGym');
      const previous = button.textContent;
      button.disabled = true;
      button.textContent = 'Starting';
      try {
        const res = await fetch('/api/environments/mobilegym', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({name, parallel_envs: parallelEnvs})
        });
        const body = await res.json();
        if(!res.ok){
          document.getElementById('logBox').textContent = body.error || 'failed to start MobileGym';
          return;
        }
        document.getElementById('mobilegymName').value = '';
        if(body.environment){
          mobilegymEnvironments = [body.environment, ...mobilegymEnvironments.filter(env => env.id !== body.environment.id)];
          selectedEnvironmentId = body.environment.id;
          saveWebuiSettings({keepInputs: true});
        }
        await loadMobileGymEnvironments();
      } finally {
        button.disabled = false;
        button.textContent = previous;
      }
    }

    async function stopMobileGym(id){
      if(selectedEnvironmentId === id){
        selectedEnvironmentId = '';
        saveWebuiSettings({keepInputs: true});
      }
      const res = await fetch(`/api/environments/mobilegym/${encodeURIComponent(id)}/stop`, {method: 'POST'});
      if(!res.ok) document.getElementById('logBox').textContent = await res.text();
      await loadMobileGymEnvironments();
    }

    async function removeMobileGym(id){
      if(selectedEnvironmentId === id){
        selectedEnvironmentId = '';
        saveWebuiSettings({keepInputs: true});
      }
      const res = await fetch(`/api/environments/mobilegym/${encodeURIComponent(id)}`, {method: 'DELETE'});
      if(!res.ok) document.getElementById('logBox').textContent = await res.text();
      await loadMobileGymEnvironments();
    }

    async function loadADBAndroidEnvironments(){
      try {
        const res = await fetch('/api/environments/adb-android');
        const body = await res.json();
        adbAndroidEnvironments = (body.environments || []).map(env => ({...env, type: 'adb_android'}));
      } catch {
        adbAndroidEnvironments = [];
      }
      renderEnvs();
      syncRunState();
    }

    async function startADBAndroid(){
      const serial = document.getElementById('adbAndroidSerial').value.trim() || '127.0.0.1:6555';
      const name = document.getElementById('adbAndroidName').value.trim() || `ADB Android (${serial})`;
      const bridgePortRaw = document.getElementById('adbAndroidBridgePort').value.trim();
      const button = document.getElementById('startADBAndroid');
      const previous = button.textContent;
      button.disabled = true;
      button.textContent = 'Starting';
      try {
        const payload = {name, serial};
        if(bridgePortRaw) payload.bridge_port = parseInt(bridgePortRaw, 10) || 0;
        const res = await fetch('/api/environments/adb-android', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(payload)
        });
        const body = await res.json();
        if(!res.ok){
          document.getElementById('logBox').textContent = body.error || 'failed to start ADB Android bridge';
          return;
        }
        document.getElementById('adbAndroidName').value = '';
        if(body.environment){
          adbAndroidEnvironments = [body.environment, ...adbAndroidEnvironments.filter(env => env.id !== body.environment.id)];
          selectedEnvironmentId = body.environment.id;
          saveWebuiSettings({keepInputs: true});
        }
        await loadADBAndroidEnvironments();
      } finally {
        button.disabled = false;
        button.textContent = previous;
      }
    }

    async function stopADBAndroid(id){
      if(selectedEnvironmentId === id){
        selectedEnvironmentId = '';
        saveWebuiSettings({keepInputs: true});
      }
      const res = await fetch(`/api/environments/adb-android/${encodeURIComponent(id)}/stop`, {method: 'POST'});
      if(!res.ok) document.getElementById('logBox').textContent = await res.text();
      await loadADBAndroidEnvironments();
    }

    async function removeADBAndroid(id){
      if(selectedEnvironmentId === id){
        selectedEnvironmentId = '';
        saveWebuiSettings({keepInputs: true});
      }
      const res = await fetch(`/api/environments/adb-android/${encodeURIComponent(id)}`, {method: 'DELETE'});
      if(!res.ok) document.getElementById('logBox').textContent = await res.text();
      await loadADBAndroidEnvironments();
    }

    async function loadSuites(){
      const res = await fetch('/api/suites');
      suites = (await res.json()).suites || [];
      renderSuites();
      if(jobs.length) renderJobs();
    }

    // Suite category display order
    const CATEGORY_ORDER = ['Basic Operations', 'Application Scenarios', 'End-to-End Workflow', 'Perception & Control', 'Memory & Cognition', 'MobileGym', 'Other'];

    // Track collapsed category state across re-renders
    const collapsedCategories = new Set();

    function renderSuites(){
      const filter = document.getElementById('suiteFilter').value.toLowerCase();
      const container = document.getElementById('suitesContainer');

      // Group suites by category
      const grouped = {};
      suites.forEach(s => {
        const category = s.suite_category || 'Other';
        if(!grouped[category]) grouped[category] = [];
        grouped[category].push(s);
      });

      // Sort categories
      const sortedCategories = Object.keys(grouped).sort((a, b) => {
        const aIndex = CATEGORY_ORDER.indexOf(a);
        const bIndex = CATEGORY_ORDER.indexOf(b);
        if(aIndex === -1 && bIndex === -1) return a.localeCompare(b);
        if(aIndex === -1) return 1;
        if(bIndex === -1) return -1;
        return aIndex - bIndex;
      });

      // Create single table with category rows
      const table = document.createElement('table');
      const thead = document.createElement('thead');
      thead.innerHTML = '<tr><th style="width:40px"></th><th>Suite</th><th style="width:140px">Kind</th><th style="width:80px">Tasks</th></tr>';
      table.appendChild(thead);

      const tbody = document.createElement('tbody');

      sortedCategories.forEach(category => {
        const items = grouped[category];
        const filtered = items.filter(s => !filter || (s.name + ' ' + s.key).toLowerCase().includes(filter));
        if(!filtered.length) return;

        // Category header row
        const categoryRow = document.createElement('tr');
        categoryRow.className = 'suite-category-row';
        const isCollapsed = collapsedCategories.has(category);
        categoryRow.innerHTML = `<td colspan="4" class="suite-category-header-cell">
          <span class="category-arrow">${isCollapsed ? '▶' : '▼'}</span>
          <span>${escapeHtml(category)}</span>
          <span class="muted">(${filtered.length})</span>
        </td>`;

        categoryRow.onclick = () => {
          const arrow = categoryRow.querySelector('.category-arrow');
          const collapsed = arrow.textContent === '▶';
          arrow.textContent = collapsed ? '▼' : '▶';

          // Update collapsed state
          if(collapsed) {
            collapsedCategories.delete(category);
          } else {
            collapsedCategories.add(category);
          }

          // Toggle visibility of suite rows
          let nextRow = categoryRow.nextElementSibling;
          while(nextRow && !nextRow.classList.contains('suite-category-row')) {
            nextRow.style.display = collapsed ? '' : 'none';
            nextRow = nextRow.nextElementSibling;
          }
        };

        tbody.appendChild(categoryRow);

        // Suite rows
        filtered.forEach(s => {
          const tr = document.createElement('tr');
          tr.className = 'suite-row';
          if(isCollapsed) tr.style.display = 'none';
          const mockBadge = s.mock_environment ? ' <span class="status">mock</span>' : '';
          tr.innerHTML = `<td><input type="checkbox" ${selectedSuites.has(s.key) ? 'checked' : ''}></td>
            <td title="${escapeHtml(s.key)}"><div class="cell-main"><span>${escapeHtml(s.name)}</span><small>${escapeHtml(s.key)}</small></div></td>
            <td><span class="status">${escapeHtml(s.kind)}</span>${mockBadge}</td>
            <td><a href="#" data-suite-detail="${escapeHtml(s.key)}">${s.task_count || 0}</a></td>`;
          tr.querySelector('input').onchange = e => {
            if(e.target.checked) selectedSuites.add(s.key); else selectedSuites.delete(s.key);
            syncRunState();
          };
          const detailLink = tr.querySelector('[data-suite-detail]');
          if(detailLink) detailLink.onclick = e => { e.preventDefault(); openSuiteDetail(s.key); };
          tbody.appendChild(tr);
        });
      });

      table.appendChild(tbody);
      container.innerHTML = '';
      container.appendChild(table);

      syncRunState();
    }

    function suiteDisplayName(key){
      const suite = suites.find(s => s.key === key);
      return suite ? suite.name : key;
    }

    async function openSuiteDetail(suiteKey){
      try {
        const res = await fetch(`/api/suites/${encodeURIComponent(suiteKey)}`);
        if(!res.ok) throw new Error('Failed to load suite details');
        const body = await res.json();
        const suite = body.suite;
        document.getElementById('suiteDetailTitle').textContent = suite.name || suiteKey;
        document.getElementById('suiteDetailSubtitle').textContent = suiteKey;
        renderSuiteDetail(suite);
        document.getElementById('suiteDetailDialog').hidden = false;
      } catch (err) {
        document.getElementById('logBox').textContent = err.message || String(err);
      }
    }

    function closeSuiteDetail(){
      document.getElementById('suiteDetailDialog').hidden = true;
    }

    function renderSuiteDetail(suite){
      const container = document.getElementById('suiteDetailContent');
      container.innerHTML = '';

      if(suite.prompt_prefix){
        const section = document.createElement('div');
        section.className = 'detail-section';
        section.innerHTML = `<h3>Prompt Prefix</h3><pre>${escapeHtml(suite.prompt_prefix)}</pre>`;
        container.appendChild(section);
      }

      const tasksTitle = document.createElement('div');
      tasksTitle.className = 'detail-section';
      tasksTitle.innerHTML = `<h3>Tasks (${suite.tasks.length})</h3>`;
      container.appendChild(tasksTitle);

      suite.tasks.forEach((task, index) => {
        const taskDiv = document.createElement('div');
        taskDiv.className = 'task-detail';

        const header = document.createElement('div');
        header.className = 'task-detail-header';
        header.innerHTML = `<span>${index + 1}. ${escapeHtml(task.id)}</span><span class="muted" style="margin-left:auto">${escapeHtml(task.category)}</span>`;

        const body = document.createElement('div');
        body.className = 'task-detail-body';

        // Description
        if(task.description_for_judge){
          const desc = document.createElement('div');
          desc.className = 'detail-section';
          desc.innerHTML = `<h3>Description</h3><p style="margin:0; color:var(--text); line-height:1.5">${escapeHtml(task.description_for_judge)}</p>`;
          body.appendChild(desc);
        }

        // Prompt
        if(task.prompt){
          const prompt = document.createElement('div');
          prompt.className = 'detail-section';
          prompt.innerHTML = `<h3>Prompt</h3><pre>${escapeHtml(task.prompt)}</pre>`;
          body.appendChild(prompt);
        }

        // Expected Answer
        if(task.expected_answer){
          const answer = document.createElement('div');
          answer.className = 'detail-section';
          answer.innerHTML = `<h3>Expected Answer</h3><p style="margin:0; color:var(--green); font-weight:600">${escapeHtml(task.expected_answer)}${task.answer_format ? ` (${task.answer_format})` : ''}</p>`;
          body.appendChild(answer);
        }

        // Rubric
        if(task.rubric && task.rubric.length){
          const rubric = document.createElement('div');
          rubric.className = 'detail-section';
          rubric.innerHTML = `<h3>Rubric (${task.rubric.length} items)</h3>`;
          const list = document.createElement('ul');
          list.className = 'rubric-list';
          task.rubric.forEach(item => {
            const li = document.createElement('li');
            li.className = 'rubric-item';
            li.innerHTML = `<strong>${escapeHtml(item.id)}</strong><span>${escapeHtml(item.check)}</span>`;
            list.appendChild(li);
          });
          rubric.appendChild(list);
          body.appendChild(rubric);
        }

        // Hard Assertions
        if(task.hard_assertions){
          const ha = task.hard_assertions;
          const assertions = document.createElement('div');
          assertions.className = 'detail-section';
          assertions.innerHTML = `<h3>Hard Assertions</h3>`;
          const dl = document.createElement('dl');
          dl.className = 'detail-list';
          dl.innerHTML = `
            <div class="detail-item"><dt>Min tool calls</dt><dd>${ha.min_tool_calls || 0}</dd></div>
            <div class="detail-item"><dt>Max tool calls</dt><dd>${ha.max_tool_calls || 50}</dd></div>
            <div class="detail-item"><dt>Timeout</dt><dd>${ha.must_complete_within_sec || 180}s</dd></div>
            <div class="detail-item"><dt>Response required</dt><dd>${ha.response_required ? 'Yes' : 'No'}</dd></div>
            ${ha.required_tools && ha.required_tools.length ? `<div class="detail-item"><dt>Required tools</dt><dd>${ha.required_tools.join(', ')}</dd></div>` : ''}
            ${ha.forbidden_tools && ha.forbidden_tools.length ? `<div class="detail-item"><dt>Forbidden tools</dt><dd>${ha.forbidden_tools.join(', ')}</dd></div>` : ''}
            ${ha.prohibited_actions && ha.prohibited_actions.length ? `<div class="detail-item"><dt>Prohibited actions</dt><dd>${ha.prohibited_actions.join(', ')}</dd></div>` : ''}
          `;
          assertions.appendChild(dl);
          body.appendChild(assertions);
        }

        // Setup
        if(task.setup){
          const setup = document.createElement('div');
          setup.className = 'detail-section';
          setup.innerHTML = `<h3>Setup</h3><pre>${escapeHtml(JSON.stringify(task.setup, null, 2))}</pre>`;
          body.appendChild(setup);
        }

        // Other fields
        const other = document.createElement('div');
        other.className = 'detail-section';
        other.innerHTML = `<h3>Other</h3>`;
        const otherDl = document.createElement('dl');
        otherDl.className = 'detail-list';
        otherDl.innerHTML = `
          <div class="detail-item"><dt>Repeats</dt><dd>${task.repeats || 1}</dd></div>
          ${task.input_screenshot ? `<div class="detail-item"><dt>Input screenshot</dt><dd>${escapeHtml(task.input_screenshot)}</dd></div>` : ''}
          ${task.expected_recalled_memory_ids && task.expected_recalled_memory_ids.length ? `<div class="detail-item"><dt>Expected memory IDs</dt><dd>${task.expected_recalled_memory_ids.join(', ')}</dd></div>` : ''}
        `;
        other.appendChild(otherDl);
        body.appendChild(other);

        header.onclick = () => {
          const collapsed = body.classList.toggle('collapsed');
          header.classList.toggle('collapsed', collapsed);
        };

        taskDiv.appendChild(header);
        taskDiv.appendChild(body);
        container.appendChild(taskDiv);
      });
    }

    function selectedSuiteEnvironmentMode(){
      const selected = Array.from(selectedSuites)
        .map(key => suites.find(suite => suite.key === key))
        .filter(Boolean);
      if(!selected.length) return 'none';
      const mockCount = selected.filter(suite => suite.mock_environment).length;
      if(mockCount === selected.length) return 'mock';
      if(mockCount > 0) return 'mixed';
      return 'external';
    }

    function mockEnvironment(){
      return {
        id: 'mock-aiden-app',
        name: 'Mock Aiden App environment',
        endpoint: '',
        type: 'mock',
        status: 'running'
      };
    }

    function syncRunState(){
      const env = selectedEnv();
      const judge = currentJudgeSettings();
      const mode = selectedSuiteEnvironmentMode();
      document.getElementById('selectedSuitesLabel').textContent = `${selectedSuites.size} suites`;
      const environmentLabel = mode === 'mock'
        ? 'Mock Aiden App environment'
        : mode === 'mixed'
          ? 'Mixed environments - run separately'
          : env ? env.name : 'No environment';
      document.getElementById('selectedEnvLabel').textContent = environmentLabel;
      document.getElementById('selectedJudgeLabel').textContent = judge.enabled ? `judge: ${judge.model}` : 'judge: off';
      const runButton = document.getElementById('runBtn');
      runButton.disabled = selectedSuites.size === 0 || mode === 'mixed';
      runButton.title = mode === 'mixed'
        ? 'Mock suites and device suites must run separately.'
        : mode === 'mock'
          ? 'Run with task-level mock environments; no phone or emulator required.'
          : '';
      const confirm = document.getElementById('confirmRunBtn');
      if(confirm) confirm.disabled = !envCanRun(env) || selectedSuites.size === 0;
    }

    async function openRunEnvironmentDialog(){
      if(selectedSuites.size === 0) return;
      const mode = selectedSuiteEnvironmentMode();
      if(mode === 'mixed'){
        document.getElementById('logBox').textContent = 'Mock suites and external device suites must run in separate jobs.';
        return;
      }
      if(mode === 'mock'){
        await startRun(mockEnvironment());
        return;
      }
      await loadMobileGymEnvironments();
      await loadADBAndroidEnvironments();
      renderEnvs();
      syncRunState();
      document.getElementById('runEnvDialog').hidden = false;
    }

    function closeRunEnvironmentDialog(){
      document.getElementById('runEnvDialog').hidden = true;
    }

    async function confirmRun(){
      const env = selectedEnv();
      if(!envCanRun(env) || selectedSuites.size === 0) return;
      const started = await startRun(env);
      if(started) closeRunEnvironmentDialog();
    }

    async function startRun(env){
      try {
        if(!agentConfigLoaded) await loadAgentConfig();
        if(agentConfigDirty){
          const saved = await saveAgentConfig({silent: true});
          if(!saved) return false;
        }
        const judge = currentJudgeSettings();
        if(env.type !== 'mock') selectedEnvironmentId = env.id;
        const settingsSaved = await saveWebuiSettings({keepInputs: true});
        if(!settingsSaved) return false;
        const res = await fetch('/api/jobs', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({
            endpoint: env.endpoint,
            environment: {id: env.id, name: env.name, type: env.type, public_endpoint: env.public_endpoint || '', web_url: env.web_url || '', serial: env.serial || '', parallel_envs: env.parallel_envs || 5},
            environment_type: env.type,
            suites: Array.from(selectedSuites),
            parallel_tasks: env.type === 'mobilegym' ? (env.parallel_envs || 5) : 1,
            no_judge: !judge.enabled,
            judge_model: judge.model,
            judge_base_url: judge.baseUrl,
            judge_api_key: judge.apiKey || ''
          })
        });
        const body = await res.json();
        if(!res.ok){
          document.getElementById('logBox').textContent = body.error || 'Failed to start job';
          console.error('Start run failed:', body);
          return false;
        }
        activeJobId = body.job.id;
        activeTaskLogId = null;
        await refreshJobs();
        return true;
      } catch(err) {
        document.getElementById('logBox').textContent = 'Error: ' + (err.message || String(err));
        console.error('Start run exception:', err);
        return false;
      }
    }

    async function refreshJobs(){
      const res = await fetch('/api/jobs');
      jobs = (await res.json()).jobs || [];
      const previousActiveJobId = activeJobId;
      if(!activeJobId && jobs.length) activeJobId = jobs[0].id;
      if(activeJobId && !jobs.find(job => job.id === activeJobId)) activeJobId = jobs[0] ? jobs[0].id : null;
      if(previousActiveJobId !== activeJobId) activeTaskLogId = null;
      renderJobs();
      if(activeJobId) await loadActiveJob(); else resetActiveJob();
    }

    function renderJobs(){
      const categoryFilter = document.getElementById('jobCategoryFilter').value;
      const tbody = document.getElementById('jobRows');
      tbody.innerHTML = '';

      // Populate category filter if empty
      const select = document.getElementById('jobCategoryFilter');
      if(select.options.length === 1 && suites.length){
        const categories = new Set();
        suites.forEach(s => categories.add(s.suite_category || 'Other'));
        const sortedCategories = Array.from(categories).sort((a, b) => {
          const aIndex = CATEGORY_ORDER.indexOf(a);
          const bIndex = CATEGORY_ORDER.indexOf(b);
          if(aIndex === -1 && bIndex === -1) return a.localeCompare(b);
          if(aIndex === -1) return 1;
          if(bIndex === -1) return -1;
          return aIndex - bIndex;
        });
        sortedCategories.forEach(cat => {
          const option = document.createElement('option');
          option.value = cat;
          option.textContent = cat;
          select.appendChild(option);
        });
      }

      // Filter jobs by category
      const filtered = jobs.filter(job => {
        if(!categoryFilter) return true;
        const jobSuiteKeys = (job.suites || []).map(key => String(key));
        return jobSuiteKeys.some(key => {
          const suite = suites.find(s => s.key === key);
          return suite && (suite.suite_category || 'Other') === categoryFilter;
        });
      });

      if(!filtered.length){
        tbody.innerHTML = '<tr><td class="empty-row" colspan="6">No jobs yet</td></tr>';
      }

      filtered.forEach(job => {
        const suiteKeys = (job.suites || []).map(key => String(key));
        const suiteNames = suiteKeys.map(suiteDisplayName);
        const suiteLabel = suiteNames.length ? suiteNames.join(', ') : 'No suites';
        const suiteDetail = suiteKeys.join(', ');
        const report = job.report_url
          ? `<a href="${escapeHtml(job.report_url)}" target="_blank" rel="noreferrer">report</a>`
          : '';
        const envLabel = job.environment_name || job.endpoint || 'No environment';
        const envType = job.environment_type || 'device';
        const actionHtml = jobCanStop(job)
          ? `<button class="danger" data-stop-job="${escapeHtml(job.id)}" ${job.status === 'stopping' ? 'disabled' : ''}>Stop</button>`
          : '';
        const tr = document.createElement('tr');
        tr.innerHTML = `<td><div class="cell-main"><a href="#" data-job="${job.id}">${escapeHtml(job.id)}</a><small>${escapeHtml(job.created_at || '')}</small></div></td>
          <td title="${escapeHtml(suiteDetail || suiteLabel)}"><div class="cell-main"><span>${escapeHtml(suiteLabel)}</span><small>${escapeHtml(suiteDetail)}</small></div></td>
          <td title="${escapeHtml(job.environment_name || job.endpoint || envLabel)}"><div class="cell-main"><span>${escapeHtml(envLabel)}</span><small>${escapeHtml(envType)}</small></div></td>
          <td><span class="status ${cssToken(job.status)}">${escapeHtml(job.status)}</span></td>
          <td>${report || '<span class="muted">none</span>'}</td>
          <td>${actionHtml}</td>`;
        tr.querySelector('[data-job]').onclick = e => { e.preventDefault(); activeJobId = job.id; activeTaskLogId = null; loadActiveJob(); };
        const stop = tr.querySelector('[data-stop-job]');
        if(stop) stop.onclick = () => stopJob(job.id);
        tbody.appendChild(tr);
      });
    }

    async function loadActiveJob(){
      const res = await fetch(`/api/jobs/${activeJobId}`);
      if(!res.ok) return;
      const job = (await res.json()).job;
      renderActiveJob(job);
      await loadActiveLog(job);
    }

    async function loadActiveLog(job){
      const tasks = job.task_records || [];
      const task = activeTaskLogId ? tasks.find(item => item.id === activeTaskLogId) : null;
      if(activeTaskLogId && !task) activeTaskLogId = null;
      const logUrl = task
        ? `/api/jobs/${encodeURIComponent(job.id)}/tasks/${encodeURIComponent(task.id)}/log`
        : `/api/jobs/${encodeURIComponent(job.id)}/log`;
      const logRes = await fetch(logUrl);
      document.getElementById('logBox').textContent = await logRes.text();
      document.getElementById('logScopeLabel').textContent = task
        ? `${task.suite || ''}:${task.task_id || ''} runner and daemon output`
        : 'Runner and daemon output';
    }

    function renderActiveJob(job){
      const activeLabel = document.getElementById('activeJobLabel');
      const runtimeLabel = job.environment_type === 'mock'
        ? (job.environment_name || 'Mock Aiden App environment')
        : job.agent_url;
      activeLabel.textContent = `${job.id} - ${runtimeLabel}`;
      const st = document.getElementById('activeJobStatus');
      st.className = 'status ' + job.status;
      st.textContent = job.status;
      const stop = document.getElementById('activeStopJob');
      stop.hidden = !jobCanStop(job);
      stop.disabled = job.status === 'stopping';
      const progress = job.progress || {};
      const total = progress.total || (job.totals && job.totals.tasks) || 0;
      const completed = progress.completed || 0;
      document.getElementById('progressBar').style.width = total ? Math.min(100, Math.round(completed * 100 / total)) + '%' : '0%';
      const progressTotals = progress.totals || {};
      const suiteTotals = job.totals || {};
      const totals = Object.keys(progressTotals).length ? progressTotals : suiteTotals;
      document.getElementById('mTasks').textContent = totals.tasks || total || 0;
      document.getElementById('mPassed').textContent = totals.passed || 0;
      document.getElementById('mFailed').textContent = totals.failed || 0;
      document.getElementById('mSkipped').textContent = totals.skipped || 0;
      document.getElementById('mJudge').textContent = totals.judge_error || 0;
      document.getElementById('mTimeout').textContent = totals.timeout || 0;
      document.getElementById('headerStatus').textContent = job.status;
      renderTaskRows(job);
    }

    function renderTaskRows(job){
      const tbody = document.getElementById('taskRows');
      const tasks = job.task_records || [];
      tbody.innerHTML = '';
      if(!tasks.length){
        tbody.innerHTML = '<tr><td class="empty-row" colspan="4">No task workers for this job</td></tr>';
        return;
      }
      if(activeTaskLogId && !tasks.find(task => task.id === activeTaskLogId)) activeTaskLogId = null;
      tasks.forEach(task => {
        const links = [];
        if(task.screen_url) links.push(`<a href="${escapeHtml(task.screen_url)}" target="_blank" rel="noreferrer">screen</a>`);
        if(task.report_url) links.push(`<a href="${escapeHtml(task.report_url)}" target="_blank" rel="noreferrer">report</a>`);
        links.push(`<button class="ghost-button table-button" data-task-log="${escapeHtml(task.id)}" type="button">log</button>`);
        if(taskCanStop(task)) links.push(`<button class="danger table-button" data-stop-task="${escapeHtml(task.id)}" type="button" ${task.status === 'stopping' ? 'disabled' : ''}>Stop</button>`);
        const agent = task.agent_url || task.container_name || '';
        const detail = task.run_id || task.benchmark_task_id || task.message || '';
        const tr = document.createElement('tr');
        tr.className = activeTaskLogId === task.id ? 'selected-row' : '';
        tr.innerHTML = `<td title="${escapeHtml(task.benchmark_task_id || '')}"><div class="cell-main"><span>${escapeHtml(task.task_id || task.id)}</span><small>${escapeHtml(task.suite || '')}</small></div></td>
          <td><span class="status ${cssToken(task.status)}">${escapeHtml(task.status || 'queued')}</span></td>
          <td title="${escapeHtml(agent)}"><div class="cell-main"><span>${escapeHtml(agent || 'pending')}</span><small>${escapeHtml(detail)}</small></div></td>
          <td><div class="inline-actions">${links.join(' ')}</div></td>`;
        const logButton = tr.querySelector('[data-task-log]');
        if(logButton) logButton.onclick = () => { activeTaskLogId = task.id; loadActiveJob(); };
        const stopButton = tr.querySelector('[data-stop-task]');
        if(stopButton) stopButton.onclick = () => stopTask(job.id, task.id);
        tbody.appendChild(tr);
      });
    }

    function resetActiveJob(){
      activeTaskLogId = null;
      document.getElementById('activeJobLabel').textContent = 'No active job';
      const st = document.getElementById('activeJobStatus');
      st.className = 'status';
      st.textContent = 'idle';
      const stop = document.getElementById('activeStopJob');
      stop.hidden = true;
      stop.disabled = false;
      document.getElementById('progressBar').style.width = '0%';
      ['mTasks', 'mPassed', 'mFailed', 'mSkipped', 'mJudge', 'mTimeout'].forEach(id => document.getElementById(id).textContent = '0');
      document.getElementById('headerStatus').textContent = 'Idle';
      document.getElementById('taskRows').innerHTML = '<tr><td class="empty-row" colspan="4">No active job</td></tr>';
      document.getElementById('logScopeLabel').textContent = 'Runner and daemon output';
    }

    function jobCanStop(job){
      return job && !['passed', 'failed', 'stopped', 'canceled'].includes(job.status || '');
    }

    function taskCanStop(task){
      return task && !['passed', 'failed', 'stopped', 'canceled'].includes(task.status || '');
    }

    async function stopJob(id){
      const res = await fetch(`/api/jobs/${encodeURIComponent(id)}/stop`, {method: 'POST'});
      if(!res.ok) document.getElementById('logBox').textContent = await res.text();
      await refreshJobs();
    }

    async function stopTask(jobId, taskId){
      const res = await fetch(`/api/jobs/${encodeURIComponent(jobId)}/tasks/${encodeURIComponent(taskId)}/stop`, {method: 'POST'});
      if(!res.ok) document.getElementById('logBox').textContent = await res.text();
      await loadActiveJob();
    }

    function cssToken(value){
      return String(value ?? '').toLowerCase().replace(/[^a-z0-9_-]/g, '_');
    }

    function escapeHtml(value){
      return String(value ?? '').replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
    }

    document.getElementById('deviceTab').onclick = () => setEnvMode('device');
    document.getElementById('mobilegymTab').onclick = () => setEnvMode('mobilegym');
    document.getElementById('adbAndroidTab').onclick = () => setEnvMode('adb_android');
    document.getElementById('saveEnv').onclick = saveEnvFromForm;
    document.getElementById('startMobileGym').onclick = startMobileGym;
    document.getElementById('startADBAndroid').onclick = startADBAndroid;
    document.getElementById('editAgentConfig').onclick = () => setAgentConfigEditing(true);
    document.getElementById('saveAgentConfig').onclick = () => saveAgentConfig();
    document.getElementById('resetAgentConfig').onclick = resetAgentConfig;
    document.getElementById('judgeEnabled').onchange = persistJudgeSettings;
    document.getElementById('judgeModel').oninput = persistJudgeSettings;
    document.getElementById('judgeBaseUrl').oninput = persistJudgeSettings;
    document.getElementById('judgeApiKey').oninput = syncRunState;
    document.getElementById('judgeApiKey').onchange = persistJudgeSettings;
    document.getElementById('metricsK').oninput = persistJudgeSettings;
    document.getElementById('agentConfigText').oninput = () => {
      if(!agentConfigEditing) return;
      agentConfigDirty = true;
      setAgentConfigStatus('Modified');
    };
    document.getElementById('suiteFilter').oninput = renderSuites;
    document.getElementById('jobCategoryFilter').onchange = renderJobs;
    const runEnvDialog = document.getElementById('runEnvDialog');
    let runEnvBackdropPointerDown = false;
    document.getElementById('runBtn').onclick = openRunEnvironmentDialog;
    document.getElementById('closeRunEnv').onclick = closeRunEnvironmentDialog;
    document.getElementById('cancelRunEnv').onclick = closeRunEnvironmentDialog;
    document.getElementById('confirmRunBtn').onclick = confirmRun;
    runEnvDialog.onpointerdown = e => {
      runEnvBackdropPointerDown = e.target === runEnvDialog;
    };
    runEnvDialog.onpointercancel = () => {
      runEnvBackdropPointerDown = false;
    };
    runEnvDialog.onclick = e => {
      const backdropClick = e.target === runEnvDialog && runEnvBackdropPointerDown;
      runEnvBackdropPointerDown = false;
      if(backdropClick) closeRunEnvironmentDialog();
    };
    document.getElementById('suiteDetailDialog').onclick = e => {
      if(e.target.id === 'suiteDetailDialog') closeSuiteDetail();
    };
    document.getElementById('closeSuiteDetail').onclick = closeSuiteDetail;
    document.getElementById('cancelSuiteDetail').onclick = closeSuiteDetail;
    document.getElementById('activeStopJob').onclick = () => { if(activeJobId) stopJob(activeJobId); };
    document.getElementById('showJobLog').onclick = () => { activeTaskLogId = null; if(activeJobId) loadActiveJob(); };
    setAgentConfigEditing(false);
    renderEnvs();
    loadWebuiSettings();
    loadAgentConfig();
    loadMobileGymEnvironments();
    loadADBAndroidEnvironments();
    loadSuites();
    refreshJobs();
    setInterval(refreshJobs, 2000);
    setInterval(loadMobileGymEnvironments, 5000);
    setInterval(loadADBAndroidEnvironments, 5000);
  </script>
</body>
</html>
"""


if __name__ == "__main__":
    raise SystemExit(cli())

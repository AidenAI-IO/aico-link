"""Load benchmark results for diagnosis without turning missing evidence into success."""
from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

from ci.plan import load_catalog
from runner.analysis import AnalysisConfig, redact_text
from runner.metrics import status_fields


def sanitize(value: Any) -> Any:
    if isinstance(value, dict):
        secret_fields = {"api_key", "token", "password", "secret", "authorization", "control_token"}
        return {k: "[REDACTED]" if str(k).lower() in secret_fields else sanitize(v) for k, v in value.items()}
    if isinstance(value, list):
        return [sanitize(v) for v in value]
    if isinstance(value, str):
        text = redact_text(value, AnalysisConfig())
        for name, secret in os.environ.items():
            if any(part in name.upper() for part in ("KEY", "TOKEN", "SECRET", "PASSWORD")) and len(secret) >= 8:
                text = text.replace(secret, "[REDACTED]")
        return text
    return value


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def results(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]


def catalog_tasks(root: Path) -> dict[str, dict]:
    catalog = load_catalog(catalog_path=root / "benchmark/ci/suites.json",
                           suites_dir=root / "benchmark/suites")
    tasks = {}
    for case in catalog.cases:
        suite = read_json(root / "benchmark" / case.suite)
        for task in suite["tasks"]:
            if case.task_ids and task["id"] not in case.task_ids:
                continue
            if task.get("platforms") and case.target_platform != "auto" and case.target_platform not in task["platforms"]:
                continue
            tasks[f"{case.id}:{task['id']}"] = {"task": task, "case_id": case.id,
                                                  "environment": case.environment, "suite": case.suite,
                                                  "runnable": case.runnable}
    return tasks


def source_failures(artifact: Path, source_id: str, attempt: str, tasks: dict[str, dict]) -> tuple[list[dict], list[str]]:
    prefix = f"ci-{source_id}-{attempt}-"
    failures, warnings = [], []
    runs = artifact / "benchmark/runs"
    if not runs.is_dir():
        raise ValueError("Benchmark artifact has no run directories")
    manifests = 0
    for run_dir in sorted(runs.glob(f"{prefix}*")):
        if not (run_dir / "manifest.json").is_file():
            continue
        manifests += 1
        manifest = read_json(run_dir / "manifest.json")
        case_id = run_dir.name[len(prefix):]
        suite_file = run_dir / "suite.json"
        source_tasks = {t["id"]: t for t in read_json(suite_file).get("tasks", [])} if suite_file.is_file() else {}
        try:
            rows = results(run_dir / "results.jsonl")
        except (OSError, ValueError) as exc:
            warnings.append(f"{case_id}: results unavailable: {exc}")
            continue
        for row in rows:
            task_id = row.get("task_id")
            if (not isinstance(task_id, str) or not task_id or Path(task_id).name != task_id
                    or task_id in {".", ".."} or "\\" in task_id):
                warnings.append(f"{case_id}: invalid task identity")
                continue
            try:
                repeat = int(row.get("attempt", 1))
                if repeat < 1:
                    raise ValueError()
            except (ValueError, TypeError):
                warnings.append(f"{case_id}:{task_id}: invalid attempt identity")
                continue
            key = f"{case_id}:{task_id}"
            success, failure_class, eligible = status_fields(row)
            if success is True:
                continue
            item = {"key": key, "status": row.get("status"), "failure_class": failure_class,
                    "agent_eligible": eligible, "metrics": row.get("metrics"),
                    "rubric": row.get("rubric"), "hard_assertion_failures": row.get("hard_assertion_failures"),
                    "final_response": row.get("final_response"), "task": source_tasks.get(row.get("task_id")),
                    "current_task": tasks.get(key, {}).get("task"),
                    "manifest": {k: manifest.get(k) for k in ("git_sha", "agent_model", "judge_config",
                                  "suite_sha256", "target_platform", "execution_environment")}}
            # Each evidence path is scoped to the downloaded artifact, never to the runner's host.
            local_dir = run_dir / "tasks" / row["task_id"]
            if repeat > 1 or (local_dir / "attempt_1").is_dir():
                local_dir /= f"attempt_{repeat}"
            for name in ("judge.json", "trace.json", "history.json", "environment_state.json"):
                path = local_dir / name
                if path.is_file():
                    try:
                        item[name.removesuffix(".json")] = read_json(path)
                    except ValueError:
                        warnings.append(f"{key}: invalid {name}")
            for name in ("pre.jpg", "post.jpg"):
                path = local_dir / name
                if path.is_file():
                    item[name.removesuffix(".jpg") + "_image"] = str(path)
            failures.append(sanitize(item))
    if not manifests:
        warnings.append("No completed suite manifests in source artifact")
    return failures, warnings


def source_identity(source_id: str, attempt: str) -> str:
    if not source_id.isdigit() or not attempt.isdigit():
        raise ValueError("Source run ID and attempt must be numeric")
    return f"{source_id}-{attempt}"

from __future__ import annotations

import argparse
import html
import json
import re
import sys
from pathlib import Path
from typing import Any

from runner.analysis import AnalysisResult, analyze_run, config_from_env
from runner.html_report import HTML_TEMPLATE, analysis_html_for_run_dir


BENCHMARK_ROOT = Path(__file__).resolve().parents[1]


TASK_STATUSES = ("passed", "failed", "timeout", "error", "unknown", "worker_failed")
SUMMARY_STATUSES = TASK_STATUSES + ("empty",)
_ANSI_RE = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")
_COMPOSE_TOOL_CALL_RE = re.compile(r"Tool call:\s+name=(?P<name>\S+)\s+input=(?P<input>.*?)(?:\s+description=|$)")
_COMPOSE_START_RUN_RE = re.compile(r"Starting agent run:\s+input=(?P<input>\"(?:\\.|[^\"\\])*\")")


def generate_reports(batch_dir: str | Path) -> dict[str, Any]:
    batch = Path(batch_dir)
    if not batch.exists() or not batch.is_dir():
        raise FileNotFoundError(f"batch directory not found: {batch}")
    if _is_direct_run_dir(batch):
        return _generate_direct_run_report(batch)

    suite_summaries = []
    suite_reports: list[tuple[Path, list[dict[str, Any]], dict[str, Any]]] = []
    all_rows: list[dict[str, Any]] = []
    for suite_dir in sorted(path for path in batch.iterdir() if path.is_dir()):
        shard_dirs = _suite_shard_dirs(suite_dir)
        if not shard_dirs:
            continue
        rows: list[dict[str, Any]] = []
        shard_metadatas: list[dict[str, Any]] = []
        for shard_dir in shard_dirs:
            shard_rows, metadata = _normalize_shard(shard_dir)
            rows.extend(shard_rows)
            shard_metadatas.append(metadata)
        suite_name = _summary_suite_name(suite_dir.name, shard_metadatas)
        summary = _summary_for(batch.name, suite_name, rows, shard_metadatas)
        _write_suite_report(suite_dir, rows, summary)
        suite_reports.append((suite_dir, rows, summary))
        suite_summaries.append(summary)
        all_rows.extend(rows)

    batch_summary = _batch_summary(batch.name, suite_summaries, all_rows)
    _write_batch_report(batch, suite_summaries, batch_summary)
    if _run_analysis_if_enabled(batch):
        for suite_dir, rows, summary in suite_reports:
            _write_suite_report(suite_dir, rows, summary, analysis_dir=batch)
        _write_batch_report(batch, suite_summaries, batch_summary)
    return batch_summary


def _is_direct_run_dir(run_dir: Path) -> bool:
    return (run_dir / "results.jsonl").is_file() or (run_dir / "errors.jsonl").is_file()


def _generate_direct_run_report(run_dir: Path) -> dict[str, Any]:
    meta = _read_json(run_dir / "meta.json")
    result_rows = {_row_task_id(row): row for row in _read_jsonl(run_dir / "results.jsonl") if _row_task_id(row)}
    error_rows = {_row_task_id(row): row for row in _read_jsonl(run_dir / "errors.jsonl") if _row_task_id(row)}
    actions_by_task = _read_bridge_actions(run_dir)
    task_ids = list(dict.fromkeys(_direct_meta_task_ids(meta) + sorted(set(result_rows) | set(error_rows))))
    links = {
        "results": ["results.jsonl"] if (run_dir / "results.jsonl").is_file() else [],
        "errors": ["errors.jsonl"] if (run_dir / "errors.jsonl").is_file() else [],
        "console": ["console.log"] if (run_dir / "console.log").is_file() else [],
        "trajectory": ["trajectory/"] if (run_dir / "trajectory").is_dir() else [],
    }
    rows = []
    for task_id in task_ids:
        result = result_rows.get(task_id)
        error = error_rows.get(task_id)
        status, reason = _status_for(result, error)
        rows.append(
            {
                "task_id": task_id,
                "suite": _direct_suite_for_task(meta, task_id, result, error),
                "shard": "direct",
                "status": status,
                "reason": reason,
                "result": result or {},
                "error": error or {},
                "actions": actions_by_task.get(task_id, []),
                "links": links,
            }
        )

    suite_summaries = []
    for suite in sorted({row["suite"] for row in rows} or {_direct_default_suite(meta)}):
        suite_rows = [row for row in rows if row["suite"] == suite]
        suite_summaries.append(_summary_for(run_dir.name, suite, suite_rows, [{"cleanup_failed": 0}]))
    summary = _batch_summary(run_dir.name, suite_summaries, rows)
    model = _model_from_meta(meta)
    if model:
        summary["model"] = model
    _write_direct_run_report(run_dir, rows, summary)
    if _run_analysis_if_enabled(run_dir):
        _write_direct_run_report(run_dir, rows, summary)
    return summary


def _direct_meta_task_ids(meta: dict[str, Any]) -> list[str]:
    task_max_steps = meta.get("task_max_steps")
    if isinstance(task_max_steps, dict):
        return [str(task_id) for task_id in task_max_steps]
    task_id = meta.get("task_id")
    if task_id:
        return [str(task_id)]
    task_ids = meta.get("task_ids")
    if isinstance(task_ids, list):
        return [str(task_id) for task_id in task_ids]
    return []


def _direct_suite_for_task(meta: dict[str, Any], task_id: str, result: dict[str, Any] | None, error: dict[str, Any] | None) -> str:
    for row in (result, error):
        if row and row.get("suite"):
            return str(row["suite"])
    suite = meta.get("suite")
    if isinstance(suite, list) and len(suite) == 1:
        return str(suite[0])
    if isinstance(suite, str) and suite:
        return suite
    if "." in task_id:
        return task_id.split(".", 1)[0]
    return _direct_default_suite(meta)


def _direct_default_suite(meta: dict[str, Any]) -> str:
    suite = meta.get("suite")
    if isinstance(suite, list) and suite:
        return str(suite[0])
    if isinstance(suite, str) and suite:
        return suite
    return "direct"


def _suite_shard_dirs(suite_dir: Path) -> list[Path]:
    return sorted(path.parent for path in suite_dir.glob("**/shard.json") if path.is_file())


def _summary_suite_name(default: str, shard_metadatas: list[dict[str, Any]]) -> str:
    for metadata in shard_metadatas:
        suite = metadata.get("suite")
        if suite:
            return str(suite)
    return default


def _normalize_shard(shard_dir: Path) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    metadata = _read_json(shard_dir / "shard.json")
    raw_dir = shard_dir / "raw"
    results, result_links = _read_results(raw_dir)
    errors, error_links = _read_errors(raw_dir)
    actions_by_task = _read_bridge_actions(raw_dir)
    console_links = [path.relative_to(shard_dir).as_posix() for path in sorted(raw_dir.glob("**/console.log"))]

    selected_task_ids = [str(task_id) for task_id in metadata.get("selected_task_ids") or []]
    task_rows = dict(results)
    task_rows.update(errors)
    compose_actions_by_task = _read_compose_tool_calls(shard_dir / "compose.log", selected_task_ids, task_rows)
    selected_task_count = int(metadata.get("selected_task_count") or len(selected_task_ids))
    exit_code = int(metadata.get("exit_code") or 0)
    shard_name = shard_dir.name
    suite = str(metadata.get("suite") or shard_dir.parent.name)
    task_metadata = metadata.get("task_metadata") if isinstance(metadata.get("task_metadata"), dict) else {}

    def _prefixed(path: str) -> str:
        return f"{shard_name}/{path}" if path else ""

    links = {
        "runner": _prefixed("runner.log") if (shard_dir / "runner.log").exists() else "",
        "compose": _prefixed("compose.log") if (shard_dir / "compose.log").exists() else "",
        "results": [_prefixed(path) for path in result_links],
        "errors": [_prefixed(path) for path in error_links],
        "console": [_prefixed(path) for path in console_links],
    }

    if selected_task_count == 0 and exit_code == 0:
        return [
            {
                "task_id": "",
                "suite": suite,
                "shard": shard_name,
                "status": "empty",
                "reason": "empty shard",
                "links": links,
            }
        ], metadata
    if selected_task_count == 0 and exit_code != 0:
        return [
            {
                "task_id": f"{suite}.{shard_name}",
                "suite": suite,
                "shard": shard_name,
                "status": "worker_failed",
                "reason": f"worker exited {exit_code}",
                "result": {},
                "error": {},
                "actions": [],
                "links": links,
            }
        ], metadata

    task_ids = list(dict.fromkeys(selected_task_ids + sorted(set(results) | set(errors))))
    rows = []
    for task_id in task_ids:
        result = results.get(task_id)
        error = errors.get(task_id)
        task_meta = task_metadata.get(task_id) if isinstance(task_metadata.get(task_id), dict) else {}
        result = _merge_task_metadata(result, task_meta)
        status, reason = _status_for(result, error)
        if result is None and error is None:
            status = "worker_failed" if exit_code != 0 else "unknown"
            reason = "missing result"
        rows.append(
            {
                "task_id": task_id,
                "suite": suite,
                "shard": shard_name,
                "status": status,
                "reason": reason,
                "result": result or {},
                "error": error or {},
                "actions": actions_by_task.get(task_id) or compose_actions_by_task.get(task_id, []),
                "links": links,
            }
        )
    return rows, metadata


def _merge_task_metadata(result: dict[str, Any] | None, metadata: dict[str, Any]) -> dict[str, Any] | None:
    if not metadata:
        return result
    merged = dict(result or {})
    for key in ("description_for_judge", "rubric", "rubric_spec", "hard_assertion_failures", "hard_assertions"):
        if key in metadata and key not in merged:
            merged[key] = metadata[key]
    return merged


def _read_results(raw_dir: Path) -> tuple[dict[str, dict[str, Any]], list[str]]:
    results: dict[str, dict[str, Any]] = {}
    links = []
    for path in sorted(raw_dir.glob("**/results.jsonl")):
        links.append(path.relative_to(raw_dir.parent).as_posix())
        for row in _read_jsonl(path):
            task_id = _row_task_id(row)
            if task_id:
                results[task_id] = row
    return results, links


def _read_errors(raw_dir: Path) -> tuple[dict[str, dict[str, Any]], list[str]]:
    errors: dict[str, dict[str, Any]] = {}
    links = []
    for path in sorted(raw_dir.glob("**/errors.jsonl")):
        links.append(path.relative_to(raw_dir.parent).as_posix())
        for row in _read_jsonl(path):
            task_id = _row_task_id(row)
            if task_id:
                errors[task_id] = row
    return errors, links


def _read_bridge_actions(raw_dir: Path) -> dict[str, list[dict[str, Any]]]:
    actions: dict[str, list[dict[str, Any]]] = {}
    for path in sorted(raw_dir.glob("**/aico_bridge_actions.json")):
        task_id = _task_id_from_artifact_dir(path.parent)
        if not task_id:
            continue
        payload = _read_json_payload(path)
        if not isinstance(payload, list):
            continue
        actions.setdefault(task_id, []).extend(entry for entry in payload if isinstance(entry, dict))
    return actions


def _read_compose_tool_calls(
    compose_path: Path,
    task_ids: list[str],
    task_rows: dict[str, dict[str, Any]] | None = None,
) -> dict[str, list[dict[str, Any]]]:
    if not task_ids:
        return {}
    segments = _compose_tool_call_segments(compose_path)
    if not segments:
        return {}
    result: dict[str, list[dict[str, Any]]] = {}
    used_segment_indexes: set[int] = set()
    task_rows = task_rows or {}
    for task_id in task_ids:
        prompt = _normalize_compose_text(_task_prompt_for_compose(task_rows.get(task_id)))
        if not prompt:
            continue
        for index, segment in enumerate(segments):
            if index in used_segment_indexes:
                continue
            request = _normalize_compose_text(str(segment.get("request") or ""))
            if not _compose_request_matches_task(request, prompt):
                continue
            actions = [entry for entry in segment.get("actions") or [] if isinstance(entry, dict)]
            if actions:
                result[task_id] = actions
            used_segment_indexes.add(index)
            break
    if result or len(segments) != len(task_ids):
        return result
    for task_id, segment in zip(task_ids, segments, strict=True):
        actions = [entry for entry in segment.get("actions") or [] if isinstance(entry, dict)]
        if actions:
            result[task_id] = actions
    return result


def _compose_tool_call_segments(compose_path: Path) -> list[dict[str, Any]]:
    try:
        lines = compose_path.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError:
        return []
    segments: list[dict[str, Any]] = []
    current: dict[str, Any] | None = None
    for raw_line in lines:
        line = _ANSI_RE.sub("", raw_line)
        if "Chat request (" in line:
            current = {"request": _compose_chat_request_text(line), "actions": []}
            segments.append(current)
            continue
        if current is None:
            continue
        start_match = _COMPOSE_START_RUN_RE.search(line)
        if start_match:
            current["request"] = _jsonish(start_match.group("input"))
            continue
        match = _COMPOSE_TOOL_CALL_RE.search(line)
        if not match:
            continue
        actions = current.setdefault("actions", [])
        if not isinstance(actions, list):
            actions = []
            current["actions"] = actions
        actions.append(
            {
                "tool_name": match.group("name"),
                "tool_input": _jsonish(match.group("input").strip()),
                "source": "compose.log",
            }
        )
    return segments


def _compose_chat_request_text(line: str) -> str:
    marker = "Chat request ("
    start = line.find(marker)
    if start < 0:
        return ""
    text = line[start + len(marker):]
    close = text.find("): ")
    if close >= 0:
        text = text[close + len("): "):]
    return re.sub(r"\s+attachments=\d+\s*$", "", text).strip()


def _task_prompt_for_compose(row: dict[str, Any] | None) -> str:
    if not isinstance(row, dict):
        return ""
    return _first_text(row, "task_name", "prompt", "instruction", "goal")


def _normalize_compose_text(text: str) -> str:
    return " ".join(str(text or "").split())


def _compose_request_matches_task(request: str, prompt: str) -> bool:
    if not request or not prompt:
        return False
    if request == prompt:
        return True
    if len(prompt) >= 80 and prompt in request:
        return True
    return len(request) >= 80 and request in prompt


def _task_id_from_artifact_dir(path: Path) -> str:
    meta = _read_json(path / "meta.json")
    if meta.get("task_id"):
        return str(meta["task_id"])
    name = path.name
    if "_" not in name:
        return ""
    suite, task = name.split("_", 1)
    return f"{suite}.{task}" if suite and task else ""


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows = []
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


def _read_json_payload(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def _status_for(result: dict[str, Any] | None, error: dict[str, Any] | None) -> tuple[str, str]:
    if error is not None:
        if _is_timeout(result, error):
            return "timeout", str(error.get("error") or error.get("message") or "timeout")
        return "error", str(error.get("error") or error.get("message") or "errors.jsonl")
    if result is None:
        return "unknown", "missing result"

    stop_reason = _stop_reason(result)
    if _is_timeout(result, None):
        return "timeout", stop_reason or _execution_error(result) or "timeout"
    if result.get("is_error") is True or stop_reason in {"crash", "exception"}:
        return "error", stop_reason or "is_error"
    if stop_reason == "false_complete":
        return "failed", stop_reason
    if result.get("is_success") is True:
        return "passed", "is_success"
    if result.get("is_success") is False:
        return "failed", "is_success false"

    status = result.get("status")
    if status == "passed":
        return "passed", "status"
    if status == "failed":
        return "failed", "status"
    if result.get("success") is True or result.get("passed") is True:
        return "passed", "fallback"
    if result.get("success") is False or result.get("passed") is False:
        return "failed", "fallback false"
    for key in ("assertion_passed", "assertions_passed", "evaluation_passed", "eval_passed", "rubric_passed"):
        if result.get(key) is False:
            return "failed", key
    return "unknown", "unrecognized result"


def _stop_reason(result: dict[str, Any]) -> str:
    execution = result.get("execution")
    if isinstance(execution, dict) and execution.get("stop_reason"):
        return str(execution["stop_reason"])
    if result.get("stop_reason"):
        return str(result["stop_reason"])
    return ""


def _is_timeout(result: dict[str, Any] | None, error: dict[str, Any] | None) -> bool:
    if result is not None:
        stop_reason = _stop_reason(result).lower()
        if stop_reason in {"overdue_termination", "timeout"}:
            return True
        if "aicoadaptertimeout" in _execution_error(result).lower():
            return True
    if error is None:
        return False
    return "aicoadaptertimeout" in str(error.get("error") or error.get("message") or error).lower()


def _execution_error(result: dict[str, Any]) -> str:
    execution = result.get("execution")
    if isinstance(execution, dict) and execution.get("error"):
        return str(execution["error"])
    if result.get("error"):
        return str(result["error"])
    return ""


def _row_task_id(row: dict[str, Any]) -> str:
    for key in ("id", "task_id", "name"):
        if row.get(key):
            return str(row[key])
    return ""


def _summary_for(
    batch_id: str,
    suite: str,
    rows: list[dict[str, Any]],
    shard_metadatas: list[dict[str, Any]],
) -> dict[str, Any]:
    summary: dict[str, Any] = {
        "batch_id": batch_id,
        "suite": suite,
        "shards": len(shard_metadatas),
        "tasks": sum(1 for row in rows if row["status"] != "empty"),
        "cleanup_failed": sum(1 for metadata in shard_metadatas if int(metadata.get("cleanup_failed") or 0) != 0),
    }
    for status in SUMMARY_STATUSES:
        summary[status] = sum(1 for row in rows if row["status"] == status)
    denominator = sum(int(summary[status]) for status in TASK_STATUSES)
    summary["pass_rate"] = (summary["passed"] / denominator) if denominator else 0.0
    model = _model_from_metadatas(shard_metadatas)
    if model:
        summary["model"] = model
    return summary


def _batch_summary(batch_id: str, suite_summaries: list[dict[str, Any]], rows: list[dict[str, Any]]) -> dict[str, Any]:
    summary: dict[str, Any] = {"batch_id": batch_id, "suites": suite_summaries}
    for key in ("shards", "tasks", "cleanup_failed") + SUMMARY_STATUSES:
        summary[key] = sum(int(suite.get(key) or 0) for suite in suite_summaries)
    denominator = sum(int(summary[status]) for status in TASK_STATUSES)
    summary["pass_rate"] = (summary["passed"] / denominator) if denominator else 0.0
    model = _model_from_summaries(suite_summaries)
    if model:
        summary["model"] = model
    summary["rows"] = rows
    return summary


def _model_from_meta(meta: dict[str, Any]) -> str:
    for key in ("model", "model_name", "MODEL_NAME"):
        value = meta.get(key)
        if value:
            return str(value)
    return ""


def _model_from_metadatas(metadatas: list[dict[str, Any]]) -> str:
    for meta in metadatas:
        model = _model_from_meta(meta)
        if model:
            return model
    return ""


def _model_from_summaries(summaries: list[dict[str, Any]]) -> str:
    for summary in summaries:
        value = summary.get("model")
        if value:
            return str(value)
    return ""


def _write_suite_report(
    suite_dir: Path,
    rows: list[dict[str, Any]],
    summary: dict[str, Any],
    analysis_dir: Path | None = None,
) -> None:
    (suite_dir / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True), encoding="utf-8")
    (suite_dir / "index.html").write_text(_drawer_html(summary["suite"], summary, rows, analysis_dir or suite_dir), encoding="utf-8")


def _write_batch_report(batch_dir: Path, suite_summaries: list[dict[str, Any]], summary: dict[str, Any]) -> None:
    serializable = dict(summary)
    serializable.pop("rows", None)
    (batch_dir / "summary.json").write_text(json.dumps(serializable, ensure_ascii=False, indent=2, sort_keys=True), encoding="utf-8")
    rows = summary.get("rows") or []
    (batch_dir / "index.html").write_text(_drawer_html(summary["batch_id"], serializable, rows, batch_dir), encoding="utf-8")


def _write_direct_run_report(run_dir: Path, rows: list[dict[str, Any]], summary: dict[str, Any]) -> None:
    serializable = dict(summary)
    serializable.pop("rows", None)
    (run_dir / "summary.json").write_text(json.dumps(serializable, ensure_ascii=False, indent=2, sort_keys=True), encoding="utf-8")
    (run_dir / "index.html").write_text(_drawer_html(summary["batch_id"], serializable, rows, run_dir), encoding="utf-8")


def _drawer_html(title: str, summary: dict[str, Any], rows: list[dict[str, Any]], run_dir: Path | None = None) -> str:
    total = int(summary.get("tasks") or 0)
    passed = int(summary.get("passed") or 0)
    timeout = int(summary.get("timeout") or 0)
    failed = int(summary.get("failed") or 0) + int(summary.get("error") or 0) + int(summary.get("worker_failed") or 0) + int(summary.get("unknown") or 0)
    skipped = int(summary.get("empty") or 0)
    judge_error = 0
    pass_rate = f"{passed / total * 100:.1f}%" if total else "0%"

    def pct(n: int) -> str:
        return f"{(n / total * 100) if total else 0:.4f}"

    display_rows = [row for row in rows if row.get("status") != "empty"]
    tasks = [_drawer_task(row) for row in display_rows]
    tasks_json = json.dumps(tasks, ensure_ascii=False, indent=2).replace("</", r"<\/")
    rows_html = "".join(_drawer_row_html(index, task) for index, task in enumerate(tasks))
    return HTML_TEMPLATE.format(
        run_id=str(summary.get("batch_id") or title),
        suite=title,
        passed=passed,
        total=total,
        failed=failed,
        skipped=skipped,
        pass_rate=pass_rate,
        agent_url="MobileGym",
        started="",
        finished="",
        rows_html=rows_html,
        tasks_json=tasks_json,
        completed=total,
        timeout=timeout,
        judge_error=judge_error,
        pass_pct=pct(passed),
        fail_pct=pct(failed),
        skip_pct=pct(skipped),
        capability_metrics_html="",
        analysis_html=analysis_html_for_run_dir(run_dir) if run_dir else "",
    )


def _run_analysis_if_enabled(run_dir: Path) -> AnalysisResult | None:
    cfg = config_from_env()
    if not cfg.enabled:
        return None
    result = analyze_run(run_dir, BENCHMARK_ROOT.parent, cfg)
    if not result.ok:
        print(f"warning: MobileGym LLM analysis failed for {run_dir}: {result.warning}", file=sys.stderr)
    return result


def _drawer_task(row: dict[str, Any]) -> dict[str, Any]:
    status = str(row.get("status") or "unknown")
    links = row.get("links") if isinstance(row.get("links"), dict) else {}
    links_text = _links_text(links)
    result = row.get("result") if isinstance(row.get("result"), dict) else {}
    error = row.get("error") if isinstance(row.get("error"), dict) else {}
    actions = row.get("actions") if isinstance(row.get("actions"), list) else []
    execution = result.get("execution") if isinstance(result.get("execution"), dict) else {}
    history_tool_calls = _history_tool_calls(result, execution)
    prompt = _first_text(result, "task_name", "prompt", "instruction", "goal")
    description = _first_text(result, "description_for_judge", "task_name", "prompt", "instruction", "goal") or str(row.get("reason") or "")
    response = _first_text(execution, "agent_answer", "agent_message") or _first_text(result, "aico_last_response", "response", "answer")
    wall_ms = _runtime_ms(result, execution)
    tool_calls_detail = _tool_calls_detail(history_tool_calls, actions)
    tool_calls_count = len(history_tool_calls) if history_tool_calls else len(actions)
    errors = []
    if status in {"error", "failed", "timeout", "worker_failed", "unknown"}:
        errors.append([status, str(row.get("reason") or status)])
    execution_error = execution.get("error") or result.get("error")
    if execution_error:
        errors.append(["execution_error", str(execution_error)])
    if error:
        errors.append(["error", str(error.get("error") or error.get("message") or error)])
    rubric = _rubric_rows(result, row)
    hard_assertion_failures = _hard_assertion_failures(result)
    return {
        "id": str(row.get("task_id") or row.get("suite") or "-"),
        "category": str(row.get("suite") or "MobileGym"),
        "status": status,
        "wall_ms": wall_ms,
        "tool_calls_count": tool_calls_count,
        "screenshots_taken": 0,
        "rubric_pass": _rubric_pass_count(rubric, status),
        "rubric_total": len(rubric),
        "description": description,
        "prompt": prompt,
        "response": response,
        "tool_calls_detail": tool_calls_detail,
        "artifacts_detail": links_text,
        "error_log_detail": _task_error_log_detail(status, row, result, error, execution),
        "rubric": rubric,
        "hard_assertion_failures": hard_assertion_failures,
        "errors": _dedupe_errors(errors),
        "trace_observations": [],
    }


def _task_error_log_detail(
    status: str,
    row: dict[str, Any],
    result: dict[str, Any],
    error: dict[str, Any],
    execution: dict[str, Any],
) -> str:
    if status not in {"failed", "timeout", "error", "unknown", "worker_failed"} and not error:
        return ""
    parts: list[str] = []
    if error:
        parts.append("### Error row\n" + _json_excerpt(error))
    execution_error = execution.get("error") or result.get("error")
    if execution_error or execution.get("stop_reason"):
        parts.append(
            "### Execution failure\n"
            + _json_excerpt(
                {
                    "status": status,
                    "reason": row.get("reason"),
                    "error": execution_error,
                    "stop_reason": execution.get("stop_reason"),
                    "agent_answer": execution.get("agent_answer"),
                    "agent_message": execution.get("agent_message"),
                }
            )
        )
    history = _chat_history(result, execution)
    if isinstance(history, list) and history:
        parts.append("### Aico chat history\n" + _json_excerpt(history))
    return "\n\n".join(parts)


def _json_excerpt(value: Any, max_chars: int = 8000) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True)[:max_chars]


def _rubric_rows(result: dict[str, Any], row: dict[str, Any]) -> list[list[str]]:
    rubric_items = result.get("rubric")
    rows = _rubric_items_rows(rubric_items)
    if rows:
        return rows

    rubric_spec = result.get("rubric_spec")
    rows = _rubric_spec_rows(rubric_spec)
    if rows:
        return rows

    status = str(row.get("status") or "unknown")
    if status == "empty":
        return []
    return [["mobilegym_status", str(row.get("reason") or status), "yes" if status == "passed" else "no"]]


def _rubric_items_rows(rubric_items: Any) -> list[list[str]]:
    if not isinstance(rubric_items, list):
        return []
    rows = []
    for item in rubric_items:
        if not isinstance(item, dict):
            continue
        verdict = str(item.get("verdict")) if item.get("verdict") is not None else "—"
        rows.append([
            str(item.get("id") or ""),
            _first_text(item, "reason", "check", "description"),
            verdict,
        ])
    return rows


def _rubric_spec_rows(rubric_spec: Any) -> list[list[str]]:
    if not isinstance(rubric_spec, list):
        return []
    rows = []
    for item in rubric_spec:
        if not isinstance(item, dict):
            continue
        rows.append([str(item.get("id") or ""), _first_text(item, "check", "reason", "description"), "—"])
    return rows


def _rubric_pass_count(rubric: list[list[str]], status: str) -> int:
    if not rubric:
        return 0
    verdicts = [row[2] for row in rubric if len(row) > 2]
    if any(verdict in {"yes", "no"} for verdict in verdicts):
        return sum(1 for verdict in verdicts if verdict == "yes")
    return 1 if status == "passed" else 0


def _hard_assertion_failures(result: dict[str, Any]) -> list[dict[str, str]]:
    failures = result.get("hard_assertion_failures")
    if not isinstance(failures, list):
        legacy = result.get("hard_assertions")
        if isinstance(legacy, dict):
            nested = legacy.get("hard_assertion_failures") or legacy.get("failures")
            if isinstance(nested, list):
                failures = nested
            else:
                failures = [
                    {"id": key, "actual": "failed"}
                    for key, value in legacy.items()
                    if value is False
                ]
        else:
            failures = legacy
    if not isinstance(failures, list):
        return []
    rows: list[dict[str, str]] = []
    for item in failures:
        if not isinstance(item, dict):
            continue
        rows.append(
            {
                "id": str(item.get("id") or ""),
                "label": str(item.get("label") or item.get("id") or ""),
                "requirement": str(item.get("requirement") or ""),
                "actual": str(item.get("actual") or ""),
            }
        )
    return rows


def _chat_history(result: dict[str, Any], execution: dict[str, Any]) -> list[Any] | None:
    for source in (result, execution):
        history = source.get("aico_last_chat_history")
        if isinstance(history, list):
            return history
    return None


def _history_tool_calls(result: dict[str, Any], execution: dict[str, Any]) -> list[dict[str, Any]]:
    history = _chat_history(result, execution)
    if not isinstance(history, list):
        return []
    calls = []
    for message in history:
        if not isinstance(message, dict) or message.get("type") != "tool_call":
            continue
        tool_input = message.get("tool_input", message.get("input", {}))
        calls.append(
            {
                "tool_name": str(message.get("tool_name") or message.get("name") or "tool_call"),
                "tool_input": _jsonish(tool_input),
            }
        )
    return calls


def _jsonish(value: Any) -> Any:
    if isinstance(value, str):
        if not value.strip():
            return {}
        try:
            return json.loads(value)
        except json.JSONDecodeError:
            return value
    return {} if value is None else value


def _first_text(source: dict[str, Any], *keys: str) -> str:
    for key in keys:
        value = source.get(key)
        if value is not None and str(value).strip():
            return str(value)
    return ""


def _runtime_ms(result: dict[str, Any], execution: dict[str, Any]) -> int:
    value = execution.get("runtime_s", result.get("runtime_s"))
    try:
        return int(float(value) * 1000)
    except (TypeError, ValueError):
        return 0


def _dedupe_errors(errors: list[list[str]]) -> list[list[str]]:
    seen = set()
    unique = []
    for entry in errors:
        key = entry[1] if len(entry) > 1 else str(entry)
        if key in seen:
            continue
        seen.add(key)
        unique.append(entry)
    return unique


def _actions_text(actions: list[Any]) -> str:
    chunks = []
    for action in actions:
        if not isinstance(action, dict):
            continue
        tool_name = str(action.get("tool_name") or action.get("action_id") or "action")
        payload = {
            "tool_input": action.get("tool_input"),
            "mobilegym_action": action.get("mobilegym_action"),
            "duration_ms": action.get("duration_ms"),
            "error": action.get("error"),
        }
        chunks.append(f"[{tool_name}]\n{json.dumps(payload, ensure_ascii=False, indent=2)}")
    return "\n\n".join(chunks)


def _tool_calls_detail(history_tool_calls: list[dict[str, Any]], actions: list[Any]) -> str:
    chunks = []
    for call in history_tool_calls:
        chunks.append(f"[{call['tool_name']}]\n{json.dumps(call['tool_input'], ensure_ascii=False, indent=2)}")
    actions_text = _actions_text(actions)
    if actions_text:
        chunks.append(actions_text)
    return "\n\n".join(chunks)


def _drawer_row_html(index: int, task: dict[str, Any]) -> str:
    status = str(task["status"])
    badge_cls = "pass" if status == "passed" else "fail" if status in {"failed", "timeout", "judge_error", "error", "worker_failed", "unknown"} else "skip"
    badge_label = "Pass" if status == "passed" else "Fail" if status == "failed" else status.title().replace("_", " ")
    return f"""<tr data-task=\"{index}\">
  <td><span class=\"task-id\">{_esc(task['id'])}</span></td>
  <td>{_esc(task['category'])}</td>
  <td><span class=\"badge {badge_cls}\">{_esc(badge_label)}</span></td>
  <td class=\"mono\">{task['rubric_pass']}/{task['rubric_total']}</td>
  <td class=\"mono\">{task['tool_calls_count']}</td>
  <td class=\"mono\">{task['wall_ms']}ms</td>
</tr>
"""


def _links_text(links: dict[str, Any]) -> str:
    lines = []
    for key, value in sorted(links.items()):
        values = value if isinstance(value, list) else [value]
        for item in values:
            if item:
                lines.append(f"{key}: {item}")
    return "\n".join(lines)


def _esc(value: Any) -> str:
    return html.escape(str(value)) if value is not None else ""


def _html_page(title: str, summary: dict[str, Any], rows: list[dict[str, Any]], base_dir: Path) -> str:
    del base_dir
    row_html = "".join(_row_html(row) for row in rows)
    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>MobileGym {html.escape(title)}</title>
<style>
body {{ font-family: -apple-system, BlinkMacSystemFont, sans-serif; margin: 32px; color: #17202a; }}
.cards {{ display: flex; flex-wrap: wrap; gap: 12px; margin-bottom: 24px; }}
.card {{ border: 1px solid #d8dee4; border-radius: 10px; padding: 12px 14px; min-width: 110px; }}
.label {{ color: #667085; font-size: 12px; }}
.value {{ font-size: 24px; font-weight: 700; margin-top: 4px; }}
table {{ border-collapse: collapse; width: 100%; font-size: 13px; }}
th, td {{ border-bottom: 1px solid #e5e7eb; padding: 10px; text-align: left; vertical-align: top; }}
.passed {{ color: #067647; }} .failed,.error,.worker_failed {{ color: #b42318; }} .unknown,.empty {{ color: #93370d; }}
a {{ color: #175cd3; }}
</style>
</head>
<body>
<h1>MobileGym {html.escape(title)}</h1>
<div class="cards">
{_summary_cards(summary)}
</div>
<table>
<thead><tr><th>Task</th><th>Suite</th><th>Shard</th><th>Status</th><th>Reason</th><th>Artifacts</th></tr></thead>
<tbody>{row_html}</tbody>
</table>
</body>
</html>
"""


def _summary_cards(summary: dict[str, Any]) -> str:
    keys = ["tasks", "passed", "failed", "error", "unknown", "worker_failed", "empty", "pass_rate"]
    cards = []
    for key in keys:
        value = summary.get(key, 0)
        if key == "pass_rate":
            value = f"{float(value) * 100:.1f}%"
        cards.append(f'<div class="card"><div class="label">{html.escape(key)}</div><div class="value">{html.escape(str(value))}</div></div>')
    return "\n".join(cards)


def _row_html(row: dict[str, Any]) -> str:
    status = str(row.get("status", "unknown"))
    return "<tr>" + "".join(
        [
            f"<td>{html.escape(str(row.get('task_id') or '-'))}</td>",
            f"<td>{html.escape(str(row.get('suite') or ''))}</td>",
            f"<td>{html.escape(str(row.get('shard') or ''))}</td>",
            f'<td class="{html.escape(status)}">{html.escape(status)}</td>',
            f"<td>{html.escape(str(row.get('reason') or ''))}</td>",
            f"<td>{_links_html(row.get('links') or {})}</td>",
        ]
    ) + "</tr>\n"


def _links_html(links: dict[str, Any]) -> str:
    parts = []
    for key, value in sorted(links.items()):
        values = value if isinstance(value, list) else [value]
        for item in values:
            if item:
                escaped = html.escape(str(item))
                parts.append(f'<a href="{escaped}">{html.escape(str(key))}: {escaped}</a>')
    return "<br>".join(parts)


def _read_json(path: Path) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return {}
    return data if isinstance(data, dict) else {}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate MobileGym batch reports.")
    parser.add_argument("batch_dir", type=Path)
    args = parser.parse_args(argv)
    try:
        generate_reports(args.batch_dir)
    except FileNotFoundError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Read benchmark history from Langfuse v4 experiments and paginated scores."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
import math
from pathlib import Path
import re
from typing import Any

from runner.metrics import FAILURE_CLASSES, FAILURE_STAGES
from ci.dashboard_metrics import DISTRIBUTION_FIELDS, DISTRIBUTION_SCORE_NAMES, distribution_score_name


REPORT_SCORE_NAMES = tuple(dict.fromkeys((
    "capability.pass_at_1", "capability.attempt_success_rate",
    *(f"capability.{metric}{suffix}" for metric in ("pass_at_k", "pass_pow_k")
      for suffix in ("", ".successes", ".eligible_tasks", ".total_tasks")),
    "capability.pass_at_1.total_tasks", "capability.pass_at_1.eligible_tasks",
    "capability.pass_at_1.successes", "coverage.attempts", "coverage.metrics_k",
    "efficiency.task_wall_ms.p50", "efficiency.total_tokens.p50",
    "efficiency.cost_usd.p50", "efficiency.cost_usd.sum", "efficiency.total_tokens.sum",
    *DISTRIBUTION_SCORE_NAMES,
    "coverage.agent_eligible_attempts",
    "diagnostics.failure_stage_coverage", "diagnostics.failure_stage_coverage.classified",
    "diagnostics.failure_stage_coverage.eligible_failures",
    *(f"failures.stage.{name}" for name in sorted(FAILURE_STAGES)),
    *(f"failures.class.{name}" for name in FAILURE_CLASSES),
)))


COMPARISON_FIELDS = (
    "git_branch", "suite_sha256", "workload_sha256", "metrics_schema_version",
    "metrics_k", "agent_model", "judge_provider", "judge_model",
    "judge_prompt_version", "target_platform", "active_skills",
)


@dataclass
class HistoryResult:
    records: list[dict[str, Any]] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)


def comparison_key(record: dict[str, Any]) -> tuple[Any, ...] | None:
    """Require a known workload/configuration before claiming a regression."""
    required = ("git_branch", "suite_sha256", "workload_sha256", "metrics_schema_version",
                "metrics_k", "agent_model", "target_platform")
    if record.get("git_dirty") or any(record.get(key) in (None, "") for key in required):
        return None
    return tuple(str(record.get(key) or "") for key in COMPARISON_FIELDS)


def _new_client() -> Any:
    from langfuse import Langfuse
    from runner.langfuse_reporter import _langfuse_connection

    public_key, secret_key, base_url = _langfuse_connection()
    return Langfuse(public_key=public_key, secret_key=secret_key, base_url=base_url,
                    timeout=20, tracing_enabled=False)


def _pages(call: Any, **kwargs: Any):
    cursor = None
    seen = set()
    while True:
        response = call(limit=100, cursor=cursor, request_options={"max_retries": 2}, **kwargs)
        yield from response.data
        cursor = response.meta.cursor
        if not cursor:
            break
        if cursor in seen:
            raise RuntimeError("Langfuse returned a repeated pagination cursor")
        seen.add(cursor)


def _error(exc: Exception) -> str:
    # Do not include exception bodies, request headers, or proxy credentials.
    status = getattr(exc, "status_code", None)
    return f"{type(exc).__name__}" + (f" (HTTP {status})" if status else "")


def fetch_benchmark_history(
    *,
    lookback_days: int = 30,
    branch_filter: str | None = None,
    local_runs_dir: Path | None = None,
    local_run_id_prefix: str | None = None,
    client: Any = None,
    now: datetime | None = None,
    include_failure_details: bool = False,
) -> HistoryResult:
    """Fetch every experiment page, then every score page; overlay local runs.

    Missing history and scores are reported explicitly. Embedded experiment
    scores are never used: that response truncates each experiment to 50 scores.
    Five experiment IDs share one score query to reduce request overhead.
    Optional task evidence is fetched only for each dataset's latest run in
    the dashboard's seven-day window, after applying the local overlay.
    """
    if lookback_days <= 0:
        raise ValueError("lookback_days must be positive")
    if local_runs_dir is not None and not local_run_id_prefix:
        raise ValueError("local_run_id_prefix is required with local_runs_dir")
    now = now or datetime.now(timezone.utc)
    cutoff = now - timedelta(days=lookback_days)
    result = HistoryResult()
    experiments: dict[str, Any] = {}
    try:
        client = client or _new_client()
        for experiment in _pages(client.api.experiments.list,
                                 from_start_time=cutoff, to_start_time=now,
                                 fields="core,metadata"):
            metadata = experiment.metadata or {}
            if str(metadata.get("aiden_benchmark", "")).lower() != "true":
                continue
            if branch_filter and metadata.get("git_branch") != branch_filter:
                if not metadata.get("git_branch"):
                    result.warnings.append(f"{experiment.name}: excluded because branch metadata is missing.")
                continue
            timestamp = parse_timestamp(metadata.get("benchmark_started_at")) or experiment.start_time
            if not cutoff <= timestamp < now:
                continue
            experiments.setdefault(experiment.id, experiment)
    except Exception as exc:
        result.warnings.append(f"Langfuse experiment history is incomplete: {_error(exc)}.")

    values = list(experiments.values())
    for offset in range(0, len(values), 5):
        batch = values[offset:offset + 5]
        scores: dict[str, dict[str, float]] = {e.id: {} for e in batch}
        try:
            for score in _pages(client.api.scores_v3.get_many_v3,
                                experiment_id=",".join(scores), fields="subject",
                                name=",".join(REPORT_SCORE_NAMES), source="API", data_type="NUMERIC"):
                subject = score.subject
                if subject is None or subject.kind != "experiment" or subject.id not in scores:
                    raise RuntimeError("Langfuse returned a score without the requested experiment subject")
                number = _number(score.value)
                if number is not None:
                    scores[subject.id][score.name] = number
        except Exception as exc:
            # Discard partial pages; they must not masquerade as complete metrics.
            scores = {e.id: {} for e in batch}
            result.warnings.append(f"Scores unavailable for {len(batch)} experiment(s): {_error(exc)}.")
        for experiment in batch:
            metadata = experiment.metadata or {}
            record = _record(
                metadata=metadata, run_id=metadata.get("benchmark_run_id") or experiment.name,
                timestamp=parse_timestamp(metadata.get("benchmark_started_at")) or experiment.start_time,
                scores=scores[experiment.id], experiment_id=experiment.id, source="langfuse",
            )
            result.records.append(record)

    # The current run may not yet be visible in Langfuse, or publication may
    # have failed. Its completed local metrics replace the same suite/run once.
    local = _read_local_runs(local_runs_dir, local_run_id_prefix, branch_filter, result.warnings)
    by_identity: dict[tuple[str, str], dict[str, Any]] = {}
    for record in sorted(result.records, key=lambda r: r["timestamp"]):
        by_identity[(record["run_id"], record["suite"])] = record
    for record in local:
        if cutoff <= parse_timestamp(record["timestamp"]) < now:
            identity = (record["run_id"], record["suite"])
            if identity in by_identity:
                record["experiment_id"] = by_identity[identity]["experiment_id"]
            by_identity[identity] = record
    result.records = sorted(by_identity.values(), key=lambda r: r["timestamp"])
    if include_failure_details:
        from ci.failure_details import fetch_failure_details

        latest = {}
        for record in result.records:
            if now - timedelta(days=7) <= parse_timestamp(record["timestamp"]) < now:
                latest[record["suite"]] = record
        for record in latest.values():
            if record["source"] == "local":
                continue
            record.update(fetch_failure_details(client, experiments[record["experiment_id"]]))
            if record["failure_details_status"] != "available":
                result.warnings.append(f"{record['run_id']}: failure details {record['failure_details_status']}: "
                                       f"{record['failure_details_inspected']} / "
                                       f"{record['failure_details_expected']} attempt outputs inspected.")
    for record in result.records:
        if record["missing_metrics"]:
            result.warnings.append(f"{record['run_id']}: missing or invalid metrics: "
                                   + ", ".join(record["missing_metrics"]) + ".")
    return result


def _number(value: Any) -> float | None:
    if isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value):
        return float(value)
    return None


def parse_timestamp(value: Any) -> datetime | None:
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    if not isinstance(value, str) or not value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)
    except ValueError:
        return None


def _record(*, metadata: dict[str, Any], run_id: str, timestamp: datetime,
            scores: dict[str, float], experiment_id: str | None, source: str) -> dict[str, Any]:
    match = re.fullmatch(r"ci-(\d+)-(\d+)-.+", run_id)
    record = {
        **{key: metadata.get(key) for key in COMPARISON_FIELDS},
        "run_id": run_id,
        "experiment_id": experiment_id,
        "workflow_run_id": metadata.get("workflow_run_id") or (match.group(1) if match else None),
        "workflow_run_attempt": metadata.get("workflow_run_attempt") or (match.group(2) if match else None),
        "workflow_run_number": metadata.get("workflow_run_number"),
        "timestamp": timestamp.isoformat(),
        "commit_sha": metadata.get("git_sha") or "unknown",
        "git_dirty": str(metadata.get("git_dirty", "false")).lower() == "true",
        "suite": metadata.get("benchmark_suite") or "unknown",
        "source": source,
        "tasks": scores.get("capability.pass_at_1.total_tasks"),
        "eligible_tasks": scores.get("capability.pass_at_1.eligible_tasks"),
        "passed": scores.get("capability.pass_at_1.successes"),
        "pass_at_1": scores.get("capability.pass_at_1"),
        "metrics_k": metadata.get("metrics_k") or scores.get("coverage.metrics_k"),
        **{f"{metric}{suffix}": scores.get(f"capability.{metric}{suffix.replace('_', '.', 1)}")
           for metric in ("pass_at_k", "pass_pow_k")
           for suffix in ("", "_successes", "_eligible_tasks", "_total_tasks")},
        "attempt_success_rate": scores.get("capability.attempt_success_rate"),
        "total_attempts": scores.get("coverage.attempts"),
        "eligible_attempts": scores.get("coverage.agent_eligible_attempts"),
        "distributions": {metric: {statistic: scores.get(distribution_score_name(metric, statistic))
                                   for statistic in fields}
                          for metric, fields in DISTRIBUTION_FIELDS.items()},
        "failure_stages": {key.removeprefix("failures.stage."): value for key, value in scores.items()
                           if key.startswith("failures.stage.")},
        "failure_stage_coverage": scores.get("diagnostics.failure_stage_coverage"),
        "classified_failures": scores.get("diagnostics.failure_stage_coverage.classified"),
        "eligible_failures": scores.get("diagnostics.failure_stage_coverage.eligible_failures"),
        "wall_ms_p50": scores.get("efficiency.task_wall_ms.p50"),
        "total_tokens_p50": scores.get("efficiency.total_tokens.p50"),
        "cost_usd_p50": scores.get("efficiency.cost_usd.p50"),
        "cost_usd_sum": scores.get("efficiency.cost_usd.sum"),
        "total_tokens_sum": scores.get("efficiency.total_tokens.sum"),
        "failure_classes": {k.removeprefix("failures.class."): v for k, v in scores.items()
                            if k.startswith("failures.class.")},
    }
    missing = [key for key in ("tasks", "eligible_tasks", "passed") if record[key] is None]
    if not missing:
        total, eligible, passed = (record[k] for k in ("tasks", "eligible_tasks", "passed"))
        valid = all(v >= 0 and float(v).is_integer() for v in (total, eligible, passed)) and passed <= eligible <= total
        rate = record["pass_at_1"]
        if not valid or (eligible and (rate is None or not math.isclose(rate, passed / eligible))):
            missing.append("pass_at_1")
    record["missing_metrics"] = missing
    if missing or not record["eligible_tasks"]:
        record["pass_at_1"] = None
    for metric in ("pass_at_k", "pass_pow_k"):
        total, eligible, successes = (record[f"{metric}_{key}"]
                                      for key in ("total_tasks", "eligible_tasks", "successes"))
        rate = record[metric]
        valid = all(v is not None and v >= 0 and float(v).is_integer()
                    for v in (total, eligible, successes))
        if not valid or not (0 <= successes <= eligible <= total) or not eligible or rate is None or not math.isclose(rate, successes / eligible):
            record[metric] = None
    return record


def _read_local_runs(runs_dir: Path | None, prefix: str | None, branch_filter: str | None,
                     warnings: list[str]) -> list[dict[str, Any]]:
    if runs_dir is None:
        return []
    if not runs_dir.is_dir():
        warnings.append("Current local runs directory is unavailable; only Langfuse history is included.")
        return []
    from runner.langfuse_reporter import _load_run_artifacts, _experiment_metadata, _run_scores

    records = []
    for run_dir in sorted(runs_dir.glob(f"{prefix}-*")):
        if not run_dir.is_dir():
            continue
        if not (run_dir / "manifest.json").exists() and not (run_dir / "metrics.json").exists():
            # Image-preparation logs share the prefix but are not suite runs.
            continue
        try:
            artifacts = _load_run_artifacts(run_dir)
            metadata = _experiment_metadata(artifacts)
            if branch_filter and metadata.get("git_branch") != branch_filter:
                continue
            timestamp = parse_timestamp(artifacts.manifest.get("started_at"))
            if timestamp is None:
                raise ValueError("manifest started_at is missing")
            scores = {score["name"]: float(score["value"])
                      for score in _run_scores(artifacts.metrics["aggregate"])
                      if _number(score["value"]) is not None}
            record = _record(metadata=metadata, run_id=metadata["benchmark_run_id"],
                                   timestamp=timestamp, scores=scores,
                                   experiment_id=None, source="local")
            from ci.failure_details import normalize_failure
            record.update(failure_details=[detail for row in artifacts.results
                          if (detail := normalize_failure(row, row)) is not None],
                          failure_details_status="available",
                          failure_details_inspected=len(artifacts.results),
                          failure_details_expected=len(artifacts.results))
            records.append(record)
        except Exception as exc:
            warnings.append(f"{run_dir.name}: local metrics unavailable: {_error(exc)}.")
    return records

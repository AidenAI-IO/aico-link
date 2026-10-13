from __future__ import annotations

import dataclasses as dc
import hashlib
import json
import os
import uuid
from collections.abc import Callable
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Mapping
from urllib.parse import urlencode

from runner.suite import Suite, TaskSpec, load_suite


REPO_ROOT = Path(__file__).resolve().parents[2]

ATTEMPT_NUMERIC_METRICS = (
    "task_wall_ms",
    "setup_ms",
    "llm_time_ms",
    "vision_llm_time_ms",
    "time_to_first_token_ms",
    "device_execution_ms",
    "screenshot_capture_ms",
    "tool_calls",
    "device_actions",
    "llm_calls",
    "vision_llm_calls",
    "tool_errors",
    "replan_count",
    "retry_count",
    "input_tokens",
    "output_tokens",
    "total_tokens",
    "cached_input_tokens",
    "reasoning_tokens",
    "cost_usd",
    "screenshots_taken",
)

ATTEMPT_BOOLEAN_METRICS = (
    "recovery_attempted",
    "recovery_succeeded",
    "expected_answer_match",
    "expected_recalled_memory_match",
    "pre_screenshot_file",
    "post_screenshot_file",
)

EFFICIENCY_DISTRIBUTION_METRICS = tuple(
    metric
    for metric in ATTEMPT_NUMERIC_METRICS
    if metric not in {
        "tool_errors",
        "replan_count",
        "retry_count",
        "screenshots_taken",
    }
)
RELIABILITY_DISTRIBUTION_METRICS = (
    "tool_errors",
    "replan_count",
    "retry_count",
)
RUN_DISTRIBUTION_STATISTICS = ("count", "sum", "mean", "p50", "p90", "p95")


class LangfusePublishError(RuntimeError):
    pass


@dc.dataclass(frozen=True)
class PublishResult:
    dataset_name: str
    run_name: str
    dataset_run_id: str | None
    dataset_run_url: str | None
    item_count: int
    already_exists: bool = False


@dc.dataclass(frozen=True)
class _RunArtifacts:
    run_dir: Path
    manifest: dict[str, Any]
    metrics: dict[str, Any]
    results: list[dict[str, Any]]
    suite: Suite


@dc.dataclass(frozen=True)
class _DatasetItem:
    id: str
    dataset_id: str
    dataset_name: str
    input: Any
    expected_output: Any
    metadata: Any


def publish_run(
    run_dir: Path,
    *,
    client: Any | None = None,
    dataset_prefix: str = "aiden-benchmark",
    progress: Callable[[str], None] | None = None,
) -> PublishResult:
    """Publish one completed benchmark run as a Langfuse dataset experiment."""
    owned_client = client is None
    phase = "load-artifacts"
    dataset_name: str | None = None
    run_name: str | None = None
    try:
        artifacts = _load_run_artifacts(Path(run_dir))
        phase = "client-init"
        if client is None:
            client = _new_client(artifacts.manifest)

        phase = "auth-check"
        auth_check = getattr(client, "auth_check", None)
        if callable(auth_check) and not auth_check():
            raise LangfusePublishError("Langfuse authentication failed")

        dataset_name = _dataset_name(artifacts, dataset_prefix)
        run_name = str(artifacts.manifest["run_id"])
        _emit_progress(
            progress,
            f"target: dataset={dataset_name} run={run_name} "
            f"items={len(artifacts.results)}",
        )
        phase = "ensure-dataset"
        _ensure_dataset(client, dataset_name, artifacts)
        _emit_progress(progress, "dataset ready")

        tasks = {task.id: task for task in artifacts.suite.tasks}
        results_by_key = {_result_key(row): row for row in artifacts.results}
        item_ids = {
            key: str(uuid.uuid5(uuid.NAMESPACE_URL, f"{dataset_name}:{key}"))
            for key in results_by_key
        }
        aggregate = artifacts.metrics["aggregate"]
        experiment_metadata = _experiment_metadata(artifacts)
        phase = "lookup-existing-run"
        existing = _get_existing_run(client, dataset_name, run_name)
        existing_run_id: str | None = None
        existing_item_traces: dict[str, str] = {}
        if existing is not None:
            _validate_existing_run_metadata(existing, experiment_metadata, run_name)
            existing_run_id = _string_attr(existing, "id")
            if not existing_run_id:
                raise LangfusePublishError(
                    f"Langfuse run {run_name} is missing its dataset run ID"
                )
            existing_item_traces = _dataset_run_item_traces(existing)
            existing_item_ids = set(existing_item_traces)
            _emit_progress(
                progress,
                f"existing run found: dataset_run_id={existing_run_id} "
                f"linked_items={len(existing_item_ids)}/{len(item_ids)}",
            )
            unexpected_item_ids = existing_item_ids - set(item_ids.values())
            if unexpected_item_ids:
                raise LangfusePublishError(
                    f"Langfuse run {run_name} contains unexpected dataset items; "
                    "use a unique benchmark run_id"
                )
            missing_item_ids = set(item_ids.values()) - existing_item_ids
            if not missing_item_ids:
                phase = "publish-scores"
                _emit_progress(progress, "publishing benchmark scores")
                _publish_scores(
                    client,
                    dataset_name=dataset_name,
                    run_name=run_name,
                    artifacts=artifacts,
                    item_ids=item_ids,
                    item_traces=existing_item_traces,
                    dataset_run_id=existing_run_id,
                    aggregate=aggregate,
                )
                _emit_progress(progress, "benchmark scores published")
                return PublishResult(
                    dataset_name=dataset_name,
                    run_name=run_name,
                    dataset_run_id=existing_run_id,
                    dataset_run_url=None,
                    item_count=len(artifacts.results),
                    already_exists=True,
                )
        else:
            _emit_progress(progress, "no existing run found")
            missing_item_ids = set(item_ids.values())

        phase = "create-dataset-items"
        dataset_items = []
        for key in sorted(results_by_key):
            item_id = item_ids[key]
            if item_id not in missing_item_ids:
                continue
            row = results_by_key[key]
            task = tasks.get(str(row["task_id"]))
            if task is None:
                raise LangfusePublishError(
                    f"result references task missing from suite: {row['task_id']}"
                )
            dataset_items.append(_create_dataset_item(
                client,
                id=item_id,
                dataset_name=dataset_name,
                input=_dataset_input(task, row),
                expected_output=_expected_output(task),
                metadata=_dataset_item_metadata(artifacts, row, key),
            ))
        _emit_progress(
            progress,
            f"dataset items ready: created={len(dataset_items)} "
            f"reused={len(item_ids) - len(dataset_items)}",
        )

        def replay_saved_result(*, item: Any, **_: Any) -> dict[str, Any]:
            item_input = _item_input(item)
            key = _item_key(item_input["task_id"], item_input["attempt"])
            row = results_by_key[key]
            output = _experiment_output(artifacts.run_dir, row)
            trace_url = getattr(client, "get_trace_url", None)
            if output["aiden_trace_id"] and callable(trace_url):
                try:
                    output["aiden_trace_url"] = trace_url(trace_id=output["aiden_trace_id"])
                except Exception:
                    # A missing project lookup must not discard benchmark scores.
                    pass
            update_span = getattr(client, "update_current_span", None)
            if callable(update_span) and _.get("record_replay_span", True):
                episode_id = _episode_id(row)
                span_metadata: dict[str, Any] = {
                    "benchmark_task_id": row["task_id"],
                    "benchmark_attempt": row["attempt"],
                }
                if episode_id:
                    span_metadata.update({
                        "aiden_episode_id": episode_id,
                        "aiden_trace_id": _agent_trace_id(episode_id),
                        "aiden_trace_url": output.get("aiden_trace_url"),
                    })
                update_span(
                    name=f"benchmark/{row['task_id']}#attempt-{row['attempt']}",
                    metadata=span_metadata,
                )
            return output

        phase = "run-experiment"
        _emit_progress(
            progress,
            f"experiment publication start: items={len(dataset_items)} concurrency=1",
        )
        experiment_args = dict(
            name=f"Aiden benchmark: {artifacts.suite.name}",
            run_name=run_name,
            description="Published from completed Aiden benchmark artifacts.",
            data=dataset_items,
            task=replay_saved_result,
            evaluators=[],
            run_evaluators=[],
            # The dataset-run-items endpoint creates or finds a run by name for
            # every item. Self-hosted Langfuse can create duplicate same-name
            # runs when those first requests race, so replay these local results
            # serially. The task itself performs no model or device work.
            max_concurrency=1,
            metadata=experiment_metadata,
        )
        if callable(getattr(client, "start_observation", None)):
            experiment = _publish_execution_items(client, **experiment_args)
        else:
            experiment = client.run_experiment(**experiment_args)
        _validate_experiment_result(experiment, len(dataset_items))
        flush = getattr(client, "flush", None)
        if callable(flush):
            flush()
        experiment_run_id = _string_attr(experiment, "dataset_run_id")
        _emit_progress(
            progress,
            f"experiment publication complete: dataset_run_id={experiment_run_id or 'missing'} "
            f"linked_items={len(getattr(experiment, 'item_results', []) or [])}",
        )
        if existing_run_id and experiment_run_id != existing_run_id:
            raise LangfusePublishError(
                "Langfuse repaired items were linked to a different dataset run"
            )
        phase = "collect-item-traces"
        # The write response already carries the item/trace mapping that scores
        # need, so there is nothing to read back and no eventual consistency to
        # wait out. A repaired run merges the traces it already had with the ones
        # this experiment just created.
        item_traces = {
            **existing_item_traces,
            **_experiment_item_traces(experiment),
        }
        phase = "publish-scores"
        _emit_progress(progress, "publishing benchmark scores")
        _publish_scores(
            client,
            dataset_name=dataset_name,
            run_name=run_name,
            artifacts=artifacts,
            item_ids=item_ids,
            item_traces=item_traces,
            dataset_run_id=experiment_run_id,
            aggregate=aggregate,
        )
        _emit_progress(progress, "benchmark scores published")
        return PublishResult(
            dataset_name=dataset_name,
            run_name=run_name,
            dataset_run_id=_string_attr(experiment, "dataset_run_id"),
            dataset_run_url=_string_attr(experiment, "dataset_run_url"),
            item_count=len(artifacts.results),
        )
    except Exception as exc:
        message = (
            str(exc)
            if isinstance(exc, LangfusePublishError)
            else f"Langfuse publish failed: {exc}"
        )
        context = f"phase={phase}"
        if dataset_name is not None:
            context += f" dataset={dataset_name}"
        if run_name is not None:
            context += f" run={run_name}"
        raise LangfusePublishError(f"{message} [{context}]") from exc
    finally:
        if owned_client and client is not None:
            shutdown = getattr(client, "shutdown", None)
            if callable(shutdown):
                shutdown()


def _new_client(manifest: Mapping[str, Any]) -> Any:
    public_key, secret_key, base_url = _langfuse_connection()
    try:
        from langfuse import Langfuse
    except ImportError as exc:
        raise LangfusePublishError(
            "Langfuse SDK is not installed; run `uv sync` in benchmark/"
        ) from exc
    return Langfuse(
        public_key=public_key,
        secret_key=secret_key,
        base_url=base_url,
        environment="benchmark",
        release=str(manifest.get("git_sha") or "") or None,
    )


def _publish_execution_items(client: Any, **experiment_args: Any) -> Any:
    """Attach experiment result observations to the original execution traces."""
    results = []
    fallback_items = []
    dataset_run_url = None
    for item in experiment_args["data"]:
        output = experiment_args["task"](item=item, record_replay_span=False)
        trace_id = output.get("aiden_trace_id")
        if not trace_id:
            fallback_items.append(item)
            continue
        roots = client.api.observations.get_many(
            trace_id=trace_id, name="agent-run", fields="core,basic", limit=2,
        ).data
        if len(roots) > 1:
            raise LangfusePublishError(f"ambiguous Agent roots in trace {trace_id}")
        if roots and _string_attr(roots[0], "environment") != "benchmark":
            raise LangfusePublishError(
                f"refusing to attach benchmark results to non-benchmark trace {trace_id}"
            )
        parent_id = _string_attr(roots[0], "id") if roots else None
        if not parent_id:
            # Never write into an execution trace whose environment we cannot
            # verify. Preserve the result in a separate replay if export is late
            # or disabled; its output still links to the expected execution ID.
            fallback_items.append(item)
            continue
        output["execution_trace_status"] = "available"
        observation = client.start_observation(
            name=f"benchmark/{item.input['task_id']}#attempt-{item.input['attempt']}",
            trace_context={"trace_id": trace_id, "parent_span_id": parent_id},
            input=item.input,
            output=output,
            metadata={
                **experiment_args["metadata"], "benchmark_execution_trace": True,
                "aiden_episode_id": output["aiden_episode_id"],
                "aiden_trace_url": output["aiden_trace_url"],
            },
        )
        try:
            linked = client.api.dataset_run_items.create(
                run_name=experiment_args["run_name"],
                run_description=experiment_args["description"],
                metadata=experiment_args["metadata"],
                dataset_item_id=item.id,
                trace_id=trace_id,
                observation_id=observation.id,
            )
            # Native v4 experiment association uses OTLP attributes, in addition
            # to the dataset-run link retained for API compatibility.
            observation._otel_span.set_attributes({
                # Distributed tracing defaults to an application root in the
                # pinned SDK. Keep this evaluation under the existing Agent root.
                "langfuse.internal.as_root": False,
                "langfuse.experiment.id": linked.dataset_run_id,
                "langfuse.experiment.name": experiment_args["run_name"],
                "langfuse.experiment.description": experiment_args["description"],
                **{
                    f"langfuse.experiment.metadata.{key}": value
                    for key, value in experiment_args["metadata"].items()
                },
                "langfuse.experiment.dataset.id": item.dataset_id,
                "langfuse.experiment.item.id": item.id,
                "langfuse.experiment.item.root_observation_id": observation.id,
                "langfuse.experiment.item.expected_output": json.dumps(item.expected_output),
                **{
                    f"langfuse.experiment.item.metadata.{key}": (
                        value if isinstance(value, str) else json.dumps(value)
                    )
                    for key, value in item.metadata.items()
                },
            })
            results.append(SimpleNamespace(
                item=item, output=output, trace_id=trace_id,
                dataset_run_id=linked.dataset_run_id,
            ))
        finally:
            observation.end()
        if dataset_run_url is None and output.get("aiden_trace_url"):
            project_url, separator, _ = output["aiden_trace_url"].rpartition("/traces/")
            if separator:
                dataset_run_url = (
                    f"{project_url}/experiments/results?"
                    + urlencode({"baseline": linked.dataset_run_id})
                )
    if fallback_items:
        def replay_without_execution(*, item: Any, **kwargs: Any) -> dict[str, Any]:
            output = experiment_args["task"](item=item, **kwargs)
            output["execution_trace_status"] = (
                "pending_or_missing" if output.get("aiden_trace_id") else "no_episode"
            )
            return output

        fallback = client.run_experiment(**{
            **experiment_args, "data": fallback_items, "task": replay_without_execution,
        })
        _validate_experiment_result(fallback, len(fallback_items))
        results.extend(fallback.item_results)
        if dataset_run_url is None:
            dataset_run_url = _string_attr(fallback, "dataset_run_url")
    return SimpleNamespace(
        item_results=results,
        dataset_run_id=results[0].dataset_run_id if results else None,
        dataset_run_url=dataset_run_url,
    )


def _validate_experiment_result(experiment: Any, expected_item_count: int) -> None:
    item_results = getattr(experiment, "item_results", None)
    if not isinstance(item_results, list) or len(item_results) != expected_item_count:
        raise LangfusePublishError(
            "Langfuse experiment did not link all dataset items "
            f"({len(item_results) if isinstance(item_results, list) else 0}/"
            f"{expected_item_count})"
        )
    linked_run_ids = [
        _string_attr(item_result, "dataset_run_id")
        for item_result in item_results
    ]
    experiment_run_id = _string_attr(experiment, "dataset_run_id")
    if (
        any(run_id is None for run_id in linked_run_ids)
        or len(set(linked_run_ids)) != 1
        or experiment_run_id not in linked_run_ids
    ):
        raise LangfusePublishError(
            "Langfuse experiment did not link all dataset items to one dataset run"
        )


def _validate_existing_run_metadata(
    dataset_run: Any,
    expected: Mapping[str, str],
    run_name: str,
) -> None:
    actual = (
        dataset_run.get("metadata")
        if isinstance(dataset_run, Mapping)
        else getattr(dataset_run, "metadata", None)
    )
    if not isinstance(actual, Mapping) or any(
        str(actual.get(key) or "") != value
        for key, value in expected.items()
    ):
        raise LangfusePublishError(
            f"Langfuse run {run_name} conflicts with benchmark artifacts; "
            "use a unique benchmark run_id"
        )


def _experiment_item_traces(experiment: Any) -> dict[str, str]:
    """Map dataset item IDs to trace IDs using the experiment write response."""
    traces: dict[str, str] = {}
    for item_result in getattr(experiment, "item_results", None) or []:
        item_id = _string_attr(getattr(item_result, "item", None), "id")
        trace_id = _string_attr(item_result, "trace_id")
        if not item_id or not trace_id:
            raise LangfusePublishError(
                "Langfuse experiment item result is missing its dataset item or trace ID"
            )
        traces[item_id] = trace_id
    return traces


def _emit_progress(
    progress: Callable[[str], None] | None,
    message: str,
) -> None:
    if progress is not None:
        progress(message)


def _publish_scores(
    client: Any,
    *,
    dataset_name: str,
    run_name: str,
    artifacts: _RunArtifacts,
    item_ids: Mapping[str, str],
    item_traces: Mapping[str, str],
    dataset_run_id: str | None,
    aggregate: Mapping[str, Any],
) -> None:
    api = getattr(client, "api", None)
    scores_api = getattr(api, "scores", None)
    create_score = getattr(scores_api, "create", None)
    if not callable(create_score):
        raise LangfusePublishError("Langfuse client does not provide the synchronous scores API")

    expected_item_ids = set(item_ids.values())
    if set(item_traces) != expected_item_ids:
        raise LangfusePublishError("Langfuse run does not contain all expected dataset items")
    results_by_key = {_result_key(row): row for row in artifacts.results}
    for key, item_id in sorted(item_ids.items()):
        trace_id = item_traces[item_id]
        output = _experiment_output(artifacts.run_dir, results_by_key[key])
        for score in _attempt_scores(output=output):
            _create_score(
                create_score,
                score=score,
                score_id=_score_id(
                    dataset_name,
                    run_name,
                    f"item:{item_id}",
                    score["name"],
                ),
                trace_id=trace_id,
            )

    if not dataset_run_id:
        raise LangfusePublishError("Langfuse run is missing its dataset run ID")
    for score in _run_scores(aggregate):
        _create_score(
            create_score,
            score=score,
            score_id=_score_id(dataset_name, run_name, "run", score["name"]),
            dataset_run_id=dataset_run_id,
        )


def _create_score(
    create_score: Any,
    *,
    score: Mapping[str, Any],
    score_id: str,
    trace_id: str | None = None,
    dataset_run_id: str | None = None,
) -> None:
    value = score["value"]
    if score.get("data_type") == "BOOLEAN":
        value = 1 if value else 0
    create_score(
        id=score_id,
        name=score["name"],
        value=value,
        data_type=score["data_type"],
        trace_id=trace_id,
        dataset_run_id=dataset_run_id,
    )


def _score_id(dataset_name: str, run_name: str, scope: str, score_name: str) -> str:
    return str(
        uuid.uuid5(
            uuid.NAMESPACE_URL,
            f"{dataset_name}:{run_name}:{scope}:{score_name}",
        )
    )


def _load_run_artifacts(run_dir: Path) -> _RunArtifacts:
    run_dir = run_dir.resolve()
    if not run_dir.is_dir():
        raise LangfusePublishError(f"benchmark run directory does not exist: {run_dir}")
    manifest = _read_json_object(run_dir / "manifest.json")
    metrics = _read_json_object(run_dir / "metrics.json")
    results = _read_jsonl(run_dir / "results.jsonl")
    if not results:
        raise LangfusePublishError("results.jsonl contains no benchmark attempts")

    run_id = str(manifest.get("run_id") or "").strip()
    if not run_id:
        raise LangfusePublishError("manifest.json is missing run_id")
    if str(metrics.get("run_id") or "") != run_id:
        raise LangfusePublishError("manifest.json and metrics.json run_id values differ")
    aggregate = metrics.get("aggregate")
    if not isinstance(aggregate, dict):
        raise LangfusePublishError("metrics.json is missing aggregate metrics")

    suite_path = _resolve_suite_path(run_dir, manifest)
    suite = load_suite(suite_path)
    expected_sha = str(manifest.get("suite_sha256") or "")
    if expected_sha and suite.sha256 != expected_sha:
        raise LangfusePublishError(
            "suite file no longer matches the suite_sha256 recorded by the run"
        )
    if str(metrics.get("suite") or "") != suite.name:
        raise LangfusePublishError("metrics.json suite does not match the suite file")

    seen: set[str] = set()
    for row in results:
        if str(row.get("run_id") or "") != run_id:
            raise LangfusePublishError("results.jsonl contains a different run_id")
        key = _result_key(row)
        if key in seen:
            raise LangfusePublishError(f"duplicate benchmark attempt: {key}")
        seen.add(key)
    return _RunArtifacts(run_dir, manifest, metrics, results, suite)


def _read_json_object(path: Path) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise LangfusePublishError(f"missing benchmark artifact: {path.name}") from exc
    except json.JSONDecodeError as exc:
        raise LangfusePublishError(f"invalid JSON in {path.name}: {exc}") from exc
    if not isinstance(payload, dict):
        raise LangfusePublishError(f"{path.name} must contain a JSON object")
    return payload


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError as exc:
        raise LangfusePublishError(f"missing benchmark artifact: {path.name}") from exc
    rows = []
    for line_number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise LangfusePublishError(
                f"invalid JSON in results.jsonl line {line_number}: {exc}"
            ) from exc
        if not isinstance(row, dict):
            raise LangfusePublishError(
                f"results.jsonl line {line_number} must contain an object"
            )
        rows.append(row)
    return rows


def _resolve_suite_path(run_dir: Path, manifest: Mapping[str, Any]) -> Path:
    snapshot_path = str(manifest.get("suite_snapshot_path") or "").strip()
    if snapshot_path:
        relative_path = Path(snapshot_path)
        if relative_path.is_absolute():
            raise LangfusePublishError("suite_snapshot_path must be relative to the run directory")
        candidate = (run_dir / relative_path).resolve()
        if not candidate.is_relative_to(run_dir):
            raise LangfusePublishError("suite_snapshot_path escapes the run directory")
        if not candidate.is_file():
            raise LangfusePublishError(
                f"suite snapshot referenced by manifest was not found: {snapshot_path}"
            )
        return candidate

    raw_path = str(manifest.get("suite_path") or "")
    if not raw_path:
        raise LangfusePublishError("manifest.json is missing suite_path")
    path = Path(raw_path)
    candidates = [path] if path.is_absolute() else [
        Path.cwd() / path,
        REPO_ROOT / path,
        REPO_ROOT / "benchmark" / path,
    ]
    for candidate in candidates:
        if candidate.is_file():
            return candidate.resolve()
    raise LangfusePublishError(f"suite file referenced by manifest was not found: {raw_path}")


def _dataset_name(artifacts: _RunArtifacts, prefix: str) -> str:
    prefix = prefix.strip().strip(":")
    if not prefix:
        raise LangfusePublishError("dataset prefix must not be empty")
    return f"{prefix}:{artifacts.suite.name}"


def _workload_sha256(results: list[Mapping[str, Any]]) -> str:
    keys = sorted(_result_key(row) for row in results)
    return hashlib.sha256(
        json.dumps(keys, separators=(",", ":")).encode("utf-8")
    ).hexdigest()


def _artifact_sha256(artifacts: _RunArtifacts) -> str:
    payload = {
        "metrics": artifacts.metrics,
        "results": artifacts.results,
    }
    return hashlib.sha256(
        json.dumps(
            payload,
            ensure_ascii=True,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
    ).hexdigest()


def _ensure_dataset(client: Any, name: str, artifacts: _RunArtifacts) -> None:
    datasets_api = getattr(getattr(client, "api", None), "datasets", None)
    get_dataset = getattr(datasets_api, "get", None)
    if not callable(get_dataset):
        raise LangfusePublishError(
            "Langfuse client does not provide the synchronous datasets API"
        )
    try:
        get_dataset(name)
        return
    except Exception as exc:
        if getattr(exc, "status_code", None) != 404:
            raise
    try:
        client.create_dataset(
            name=name,
            description=(
                f"Aiden benchmark suite {artifacts.suite.name}; suite changes are tracked "
                "through versioned dataset items and experiment metadata."
            ),
            metadata={
                "benchmark_suite": artifacts.suite.name,
            },
        )
    except Exception as create_error:
        try:
            get_dataset(name)
            return
        except Exception:
            raise create_error


def _create_dataset_item(client: Any, **fields: Any) -> Any:
    try:
        return client.create_dataset_item(**fields)
    except Exception as exc:
        # Langfuse 3.178 omits this newly-added SDK v4 response field. The write
        # has already succeeded, so hydrate the item from the public endpoint.
        if "media_references" not in str(exc):
            raise
        return _fetch_dataset_item_via_public_api(str(fields["id"]))


def _fetch_dataset_item_via_public_api(item_id: str) -> Any:
    payload = _public_api_get(f"/api/public/dataset-items/{item_id}")
    return _DatasetItem(
        id=payload["id"],
        input=payload.get("input"),
        expected_output=payload.get("expectedOutput"),
        metadata=payload.get("metadata"),
        dataset_id=payload["datasetId"],
        dataset_name=payload["datasetName"],
    )


def _public_api_get(path: str) -> dict[str, Any]:
    import requests

    public_key, secret_key, base_url = _langfuse_connection()
    response = requests.get(
        f"{base_url}{path}",
        auth=(public_key, secret_key),
        timeout=30,
    )
    response.raise_for_status()
    payload = response.json()
    if not isinstance(payload, dict):
        raise LangfusePublishError(f"unexpected Langfuse response from {path}")
    return payload


def _langfuse_connection() -> tuple[str, str, str]:
    public_key = os.environ.get("LANGFUSE_PUBLIC_KEY", "").strip()
    secret_key = os.environ.get("LANGFUSE_SECRET_KEY", "").strip()
    base_url = (
        os.environ.get("LANGFUSE_BASE_URL")
        or os.environ.get("LANGFUSE_HOST")
        or "https://cloud.langfuse.com"
    ).rstrip("/")
    if not public_key or not secret_key:
        raise LangfusePublishError(
            "LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are required"
        )
    return public_key, secret_key, base_url


def _get_existing_run(client: Any, dataset_name: str, run_name: str) -> Any | None:
    try:
        return client.get_dataset_run(dataset_name=dataset_name, run_name=run_name)
    except Exception as exc:
        if getattr(exc, "status_code", None) == 404:
            return None
        raise


def _dataset_run_item_traces(dataset_run: Any) -> dict[str, str]:
    items = (
        dataset_run.get("dataset_run_items")
        if isinstance(dataset_run, Mapping)
        else getattr(dataset_run, "dataset_run_items", None)
    )
    if not isinstance(items, list):
        return {}
    traces: dict[str, str] = {}
    for item in items:
        item_id = _string_attr(item, "dataset_item_id")
        trace_id = _string_attr(item, "trace_id")
        if not item_id or not trace_id:
            raise LangfusePublishError(
                "Langfuse dataset run item is missing its dataset item or trace ID"
            )
        traces[item_id] = trace_id
    return traces


def _dataset_input(task: TaskSpec, row: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "task_id": task.id,
        "attempt": int(row.get("attempt") or 1),
        "category": task.category,
        "prompt": task.prompt,
    }


def _expected_output(task: TaskSpec) -> dict[str, Any]:
    return {
        "description_for_judge": task.description_for_judge,
        "rubric": [dc.asdict(item) for item in task.rubric],
        "hard_assertions": dc.asdict(task.hard_assertions),
        "expected_answer": task.expected_answer,
        "expected_recalled_memory_ids": task.expected_recalled_memory_ids,
    }


def _dataset_item_metadata(
    artifacts: _RunArtifacts,
    row: Mapping[str, Any],
    key: str,
) -> dict[str, Any]:
    return {
        "item_key": key,
        "task_id": row["task_id"],
        "attempt": int(row.get("attempt") or 1),
        "category": row.get("category"),
        "suite": artifacts.suite.name,
        "suite_sha256": artifacts.suite.sha256,
    }


def _saved_trace_output(run_dir: Path, row: Mapping[str, Any]) -> dict[str, Any]:
    """Read optional, attempt-specific evidence from this run (including moved runs)."""
    run_dir = run_dir.resolve()
    raw = Path(str(row.get("artifact_dir") or ""))
    attempt = int(row.get("attempt") or 1)
    task_dir = run_dir / "tasks" / str(row["task_id"])
    candidates = [task_dir / f"attempt_{attempt}"]
    if str(row.get("artifact_dir") or "") and (
        raw.name == f"attempt_{attempt}"
        or (attempt == 1 and not raw.name.startswith("attempt_"))
    ):
        candidates.append(raw if raw.is_absolute() else run_dir / raw)
    # Never borrow the single-attempt trace for a missing repeated attempt.
    if attempt == 1 and not raw.name.startswith("attempt_"):
        candidates.append(task_dir)

    output: dict[str, Any] = {
        "final_response": None,
        "tool_calls": None,
        "trace_artifact": {"status": "missing", "path": None},
    }
    for directory in candidates:
        try:
            path = (directory / "trace.json").resolve()
        except (OSError, RuntimeError):
            output["trace_artifact"]["status"] = "unreadable"
            continue
        if not path.is_relative_to(run_dir):
            continue
        try:
            payload = path.read_text(encoding="utf-8")
        except FileNotFoundError:
            continue
        except (OSError, UnicodeError):
            output["trace_artifact"] = {
                "status": "unreadable", "path": str(path.relative_to(run_dir)),
            }
            return output
        output["trace_artifact"] = {
            "status": "invalid", "path": str(path.relative_to(run_dir)),
        }
        try:
            trace = json.loads(payload)
        except json.JSONDecodeError:
            return output
        if not isinstance(trace, dict):
            return output
        response = trace.get("final_response")
        calls = trace.get("tool_calls")
        if not isinstance(response, str) or not isinstance(calls, list) or not all(
            isinstance(call, dict) for call in calls
        ):
            return output
        output.update(final_response=response, tool_calls=calls)
        output["trace_artifact"]["status"] = "available"
        return output
    return output


def _experiment_output(run_dir: Path, row: Mapping[str, Any]) -> dict[str, Any]:
    artifact_dir = str(row.get("artifact_dir") or "")
    artifact_path = Path(artifact_dir)
    if artifact_path.is_absolute():
        try:
            artifact_dir = str(artifact_path.relative_to(run_dir))
        except ValueError:
            pass
    metrics = row.get("metrics") if isinstance(row.get("metrics"), dict) else {}
    return {
        **_saved_trace_output(run_dir, row),
        "status": row.get("status"),
        "rubric": row.get("rubric") or [],
        "rubric_pass_count": row.get("rubric_pass_count", 0),
        "rubric_total": row.get("rubric_total", 0),
        "hard_assertions": row.get("hard_assertions"),
        "hard_assertion_failures": row.get("hard_assertion_failures") or [],
        "metrics": metrics,
        "artifact_dir": artifact_dir,
        "started_at": row.get("started_at"),
        "finished_at": row.get("finished_at"),
        "aiden_episode_id": _episode_id(row),
        "aiden_trace_id": _agent_trace_id(_episode_id(row)) if _episode_id(row) else None,
        "aiden_trace_url": None,
    }


def _attempt_scores(*, output: Any, **_: Any) -> list[dict[str, Any]]:
    if not isinstance(output, Mapping):
        return []
    metrics = output.get("metrics") if isinstance(output.get("metrics"), Mapping) else {}
    evaluations: list[dict[str, Any]] = []
    _add_score(evaluations, "benchmark.status", output.get("status"), "CATEGORICAL")
    _add_score(evaluations, "benchmark.success", metrics.get("success"), "BOOLEAN")
    _add_score(
        evaluations,
        "benchmark.agent_eligible",
        metrics.get("agent_eligible"),
        "BOOLEAN",
    )
    _add_score(evaluations, "benchmark.quality_score", metrics.get("quality_score"))
    rubric_total = output.get("rubric_total")
    rubric_pass = output.get("rubric_pass_count")
    if _number(rubric_total) and _number(rubric_pass) is not None:
        _add_score(
            evaluations,
            "benchmark.rubric_pass_rate",
            float(rubric_pass) / float(rubric_total),
        )
    _add_score(
        evaluations,
        "benchmark.failure_class",
        metrics.get("failure_class"),
        "CATEGORICAL",
    )
    _add_score(
        evaluations,
        "benchmark.first_failure_stage",
        metrics.get("first_failure_stage"),
        "CATEGORICAL",
    )
    for metric_name in ATTEMPT_NUMERIC_METRICS:
        _add_score(evaluations, f"benchmark.{metric_name}", metrics.get(metric_name))
    for metric_name in ATTEMPT_BOOLEAN_METRICS:
        _add_score(
            evaluations,
            f"benchmark.{metric_name}",
            metrics.get(metric_name),
            "BOOLEAN",
        )
    for observation in metrics.get("trace_observations") or []:
        if not isinstance(observation, Mapping):
            continue
        observation_id = str(observation.get("id") or "").strip()
        if observation_id:
            _add_score(
                evaluations,
                f"benchmark.trace_observation.{observation_id}",
                observation.get("passed"),
                "BOOLEAN",
            )
    return evaluations


def _run_scores(aggregate: Mapping[str, Any]) -> list[dict[str, Any]]:
    evaluations: list[dict[str, Any]] = []
    for metric_name in (
        "pass_at_1",
        "pass_at_k",
        "pass_pow_k",
        "attempt_success_rate",
        "oracle_best_score_at_k",
    ):
        _add_score(
            evaluations,
            f"capability.{metric_name}",
            _nested(aggregate, metric_name, "value"),
        )
    for metric_name in ("pass_at_1", "pass_at_k", "pass_pow_k"):
        for field in ("successes", "eligible_tasks", "total_tasks"):
            _add_score(
                evaluations,
                f"capability.{metric_name}.{field}",
                _nested(aggregate, metric_name, field),
            )
        for bound in ("lower", "upper"):
            _add_score(
                evaluations,
                f"capability.{metric_name}.ci95_{bound}",
                _nested(aggregate, metric_name, "ci95", bound),
            )
    for field in ("successes", "eligible_attempts", "total_attempts", "coverage"):
        _add_score(
            evaluations,
            f"capability.attempt_success_rate.{field}",
            _nested(aggregate, "attempt_success_rate", field),
        )
    for bound in ("lower", "upper"):
        _add_score(
            evaluations,
            f"capability.attempt_success_rate.ci95_{bound}",
            _nested(aggregate, "attempt_success_rate", "ci95", bound),
        )
    for field in ("count", "eligible_tasks"):
        _add_score(
            evaluations,
            f"capability.oracle_best_score_at_k.{field}",
            _nested(aggregate, "oracle_best_score_at_k", field),
        )
    for metric_name in ("pass_at_1", "pass_at_k", "pass_pow_k"):
        _add_score(
            evaluations,
            f"coverage.{metric_name}",
            _nested(aggregate, metric_name, "coverage"),
        )
    for metric_name in (
        "unique_tasks",
        "attempts",
        "metrics_k",
        "agent_eligible_attempts",
        "invalid_attempts",
    ):
        _add_score(evaluations, f"coverage.{metric_name}", aggregate.get(metric_name))
    for metric_name in EFFICIENCY_DISTRIBUTION_METRICS:
        for statistic in RUN_DISTRIBUTION_STATISTICS:
            _add_score(
                evaluations,
                f"efficiency.{metric_name}.{statistic}",
                _nested(aggregate, metric_name, statistic),
            )
    for metric_name in RELIABILITY_DISTRIBUTION_METRICS:
        for statistic in RUN_DISTRIBUTION_STATISTICS:
            _add_score(
                evaluations,
                f"reliability.{metric_name}.{statistic}",
                _nested(aggregate, metric_name, statistic),
            )
    for statistic in ("count", "mean", "p50"):
        _add_score(
            evaluations,
            f"capability.first_success_attempt.{statistic}",
            _nested(aggregate, "first_success_attempt", statistic),
        )
    for metric_name in ("task_wall_ms", "total_tokens", "cost_usd"):
        for statistic in ("count", "mean", "p50"):
            _add_score(
                evaluations,
                f"cost_to_first_success.{metric_name}.{statistic}",
                _nested(
                    aggregate,
                    "cost_to_first_success",
                    metric_name,
                    statistic,
                ),
            )
    _add_score(
        evaluations,
        "diagnostics.failure_stage_coverage",
        _nested(aggregate, "failure_stage_coverage", "value"),
    )
    for field in ("classified", "eligible_failures"):
        _add_score(
            evaluations,
            f"diagnostics.failure_stage_coverage.{field}",
            _nested(aggregate, "failure_stage_coverage", field),
        )
    for failure_class, count in sorted((aggregate.get("failure_classes") or {}).items()):
        _add_score(evaluations, f"failures.class.{failure_class}", count)
    for stage, count in sorted((aggregate.get("failure_stages") or {}).items()):
        _add_score(evaluations, f"failures.stage.{stage}", count)
    for status, count in sorted((aggregate.get("by_status") or {}).items()):
        _add_score(evaluations, f"status.{status}", count)
    for observation_id, values in sorted(
        (aggregate.get("trace_observations") or {}).items()
    ):
        if not isinstance(values, Mapping):
            continue
        passed = _number(values.get("tasks_with_observation"))
        observed = _number(values.get("tasks_observed"))
        _add_score(
            evaluations,
            f"observations.{observation_id}.passed_tasks",
            passed,
        )
        _add_score(
            evaluations,
            f"observations.{observation_id}.observed_tasks",
            observed,
        )
        if passed is not None and observed:
            _add_score(
                evaluations,
                f"observations.{observation_id}.pass_rate",
                passed / observed,
            )
    return evaluations


def _add_score(
    scores: list[dict[str, Any]],
    name: str,
    value: Any,
    data_type: str = "NUMERIC",
) -> None:
    if data_type == "NUMERIC":
        value = _number(value)
        if value is None:
            return
    elif data_type == "BOOLEAN":
        if not isinstance(value, bool):
            return
    elif data_type == "CATEGORICAL":
        if not isinstance(value, str) or not value:
            return
    scores.append({"name": name, "value": value, "data_type": data_type})


def _number(value: Any) -> int | float | None:
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def _nested(value: Mapping[str, Any], *path: str) -> Any:
    current: Any = value
    for key in path:
        if not isinstance(current, Mapping):
            return None
        current = current.get(key)
    return current


def _experiment_metadata(artifacts: _RunArtifacts) -> dict[str, str]:
    manifest = artifacts.manifest
    judge = manifest.get("judge_config") if isinstance(manifest.get("judge_config"), dict) else {}
    values = {
        "benchmark_run_id": manifest.get("run_id"),
        "benchmark_suite": artifacts.suite.name,
        "suite_sha256": artifacts.suite.sha256,
        "workload_sha256": _workload_sha256(artifacts.results),
        "artifact_sha256": _artifact_sha256(artifacts),
        "metrics_schema_version": manifest.get("metrics_schema_version"),
        "metrics_k": manifest.get("metrics_k"),
        "git_sha": manifest.get("git_sha"),
        "git_dirty": manifest.get("git_dirty"),
        "agent_model": manifest.get("agent_model"),
        "judge_provider": judge.get("provider"),
        "judge_model": judge.get("model"),
        "judge_prompt_version": manifest.get("judge_prompt_version"),
        "target_platform": manifest.get("target_platform"),
        "selected_task_ids": manifest.get("selected_task_ids"),
        "active_skills": manifest.get("active_skills"),
    }
    metadata = {"aiden_benchmark": "true"}
    for key, value in values.items():
        if value is None or value == "":
            continue
        if isinstance(value, (dict, list)):
            metadata[key] = json.dumps(value, ensure_ascii=True, separators=(",", ":"))
        elif isinstance(value, bool):
            metadata[key] = str(value).lower()
        else:
            metadata[key] = str(value)
    return metadata


def _result_key(row: Mapping[str, Any]) -> str:
    task_id = str(row.get("task_id") or "").strip()
    if not task_id:
        raise LangfusePublishError("results.jsonl contains an attempt without task_id")
    try:
        attempt = int(row.get("attempt") or 1)
    except (TypeError, ValueError) as exc:
        raise LangfusePublishError(f"invalid attempt number for task {task_id}") from exc
    if attempt < 1:
        raise LangfusePublishError(f"invalid attempt number for task {task_id}")
    return _item_key(task_id, attempt)


def _item_key(task_id: str, attempt: int) -> str:
    return f"{task_id}#attempt-{attempt}"


def _item_input(item: Any) -> Mapping[str, Any]:
    value = item.get("input") if isinstance(item, Mapping) else getattr(item, "input", None)
    if not isinstance(value, Mapping):
        raise LangfusePublishError("Langfuse dataset item is missing its benchmark input")
    return value


def _episode_id(row: Mapping[str, Any]) -> str | None:
    metrics = row.get("metrics") if isinstance(row.get("metrics"), Mapping) else {}
    value = metrics.get("episode_id")
    return value.strip() if isinstance(value, str) and value.strip() else None


def _agent_trace_id(episode_id: str | None) -> str | None:
    if not episode_id:
        return None
    try:
        return uuid.UUID(episode_id).hex
    except ValueError:
        return uuid.uuid5(uuid.NAMESPACE_URL, episode_id).hex


def _string_attr(value: Any, name: str) -> str | None:
    item = value.get(name) if isinstance(value, Mapping) else getattr(value, name, None)
    return str(item) if item else None

"""Exercise the real pinned SDK's transport, models, and cursor pagination."""

from datetime import datetime, timezone
import json
from pathlib import Path
from types import SimpleNamespace

import httpx
import pytest
from langfuse.api.client import LangfuseAPI

from ci import fetch_langfuse_history as history

NOW = datetime(2026, 10, 13, tzinfo=timezone.utc)
STAMP = "2026-10-12T08:04:20Z"


def _experiment(id="exp-1", **metadata):
    return {"id": id, "name": "ci-123-1-memory", "startTime": STAMP,
            "endTime": STAMP, "itemCount": 16, "datasetId": "dataset-1",
            "metadata": {"aiden_benchmark": "true", "benchmark_suite": "memory_v1",
                         "benchmark_run_id": "ci-123-1-memory", "git_branch": "main",
                         "git_sha": "abc", **metadata}}


def _score(name, value, experiment_id="exp-1"):
    return {"id": name, "projectId": "project-1", "name": name, "value": value,
            "dataType": "NUMERIC", "source": "API", "timestamp": STAMP,
            "environment": "default", "createdAt": STAMP, "updatedAt": STAMP,
            "subject": {"kind": "experiment", "id": experiment_id}}


def _metrics(experiment_id="exp-1"):
    return [_score(name, value, experiment_id) for name, value in {
        "capability.pass_at_1": 0.9375, "capability.pass_at_1.successes": 15,
        "capability.pass_at_1.eligible_tasks": 16, "capability.pass_at_1.total_tasks": 18,
        "capability.pass_at_k": 1, "capability.pass_at_k.successes": 12,
        "capability.pass_at_k.eligible_tasks": 12, "capability.pass_at_k.total_tasks": 18,
        "capability.pass_pow_k": 0.75, "capability.pass_pow_k.successes": 9,
        "capability.pass_pow_k.eligible_tasks": 12, "capability.pass_pow_k.total_tasks": 18,
        "coverage.metrics_k": 2,
        "coverage.attempts": 36, "efficiency.total_tokens.p50": 23118,
        "coverage.agent_eligible_attempts": 32,
        "efficiency.task_wall_ms.p50": 14200, "efficiency.task_wall_ms.p90": 31500,
        "efficiency.task_wall_ms.count": 32,
        "efficiency.tool_calls.mean": 4.5, "efficiency.tool_calls.count": 30,
        "efficiency.device_execution_ms.p50": 0, "efficiency.device_execution_ms.count": 32,
        "reliability.tool_errors.mean": 0, "reliability.tool_errors.count": 32,
        "diagnostics.failure_stage_coverage": 0.5,
        "diagnostics.failure_stage_coverage.classified": 1,
        "diagnostics.failure_stage_coverage.eligible_failures": 2,
        "failures.stage.device_execution": 1, "failures.stage.unknown": 1,
        "efficiency.cost_usd.sum": 0.1, "failures.class.agent": 1,
    }.items()]


def _client(handler):
    sdk = LangfuseAPI(base_url="https://langfuse.test", username="pk-test", password="sk-test",
                         httpx_client=httpx.Client(transport=httpx.MockTransport(handler)))
    return SimpleNamespace(api=sdk)


def _response(data, cursor=None):
    return httpx.Response(200, json={"data": data, "meta": {"cursor": cursor, "limit": 100}})


def test_real_sdk_paginates_experiments_and_scores_beyond_fifty():
    requests = []
    padding = [_score(f"efficiency.extra.{i}", i) for i in range(100)]

    def handler(request):
        requests.append(request)
        assert request.method == "GET"
        if request.url.path == "/api/public/experiments":
            assert request.url.params["fields"] == "core,metadata"
            return _response([_experiment()], "experiments-next") if not request.url.params.get("cursor") else _response([])
        assert request.url.path == "/api/public/v3/scores"
        assert request.url.params["experimentId"] == "exp-1"
        assert {"capability.pass_at_k.eligible_tasks", "capability.pass_pow_k",
                "capability.pass_pow_k.successes", "coverage.metrics_k",
                "efficiency.tool_calls.mean", "efficiency.tool_calls.count",
                "efficiency.task_wall_ms.p90", "reliability.tool_errors.mean",
                "failures.stage.unknown", "diagnostics.failure_stage_coverage"} <= set(request.url.params["name"].split(","))
        return _response(padding, "scores-next") if not request.url.params.get("cursor") else _response(_metrics())

    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert not result.warnings
    assert len(requests) == 4
    record = result.records[0]
    assert record["pass_at_1"] == 0.9375
    assert record["passed"] == 15
    assert record["eligible_tasks"] == 16
    assert record["tasks"] == 18
    assert record["pass_at_k"] == 1
    assert record["pass_pow_k"] == 0.75
    assert record["pass_at_k_eligible_tasks"] == 12
    assert record["pass_pow_k_successes"] == 9
    assert record["metrics_k"] == 2
    assert record["failure_classes"] == {"agent": 1}
    assert record["total_tokens_p50"] == 23118
    assert record["workflow_run_id"] == "123"
    assert record["workflow_run_attempt"] == "1"
    assert record["distributions"]["task_wall_ms"] == {"p50": 14200, "p90": 31500, "count": 32}
    assert record["distributions"]["tool_calls"] == {"mean": 4.5, "count": 30}
    assert record["distributions"]["device_execution_ms"]["p50"] == 0
    assert record["distributions"]["tool_errors"]["mean"] == 0
    assert record["distributions"]["retry_count"] == {"mean": None, "count": None}
    assert record["eligible_attempts"] == 32
    assert record["failure_stages"] == {"device_execution": 1, "unknown": 1}
    assert record["failure_stage_coverage"] == 0.5
    assert record["eligible_failures"] == 2


def test_batched_scores_are_assigned_to_their_experiment():
    def handler(request):
        if request.url.path.endswith("/experiments"):
            return _response([_experiment(), {**_experiment("exp-2"), "name": "other",
                                              "metadata": {**_experiment()["metadata"], "benchmark_run_id": "other"}}])
        assert request.url.params["experimentId"] == "exp-1,exp-2"
        return _response(_metrics() + [_score("capability.pass_at_1.successes", 0, "exp-2"),
            _score("capability.pass_at_1.eligible_tasks", 16, "exp-2"),
            _score("capability.pass_at_1.total_tasks", 16, "exp-2"),
            _score("capability.pass_at_1", 0, "exp-2")])
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert {r["run_id"]: r["pass_at_1"] for r in result.records} == {"ci-123-1-memory": 0.9375, "other": 0}


@pytest.mark.parametrize("metadata", [{"aiden_benchmark": "false"}, {"git_branch": "feature"}, {"git_branch": None}])
def test_filter_does_not_include_nonbenchmark_or_unknown_branches(metadata):
    def handler(request):
        assert request.url.path.endswith("/experiments")
        return _response([_experiment(**metadata)])
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW, branch_filter="main")
    assert result.records == []
    assert bool(result.warnings) == (metadata.get("git_branch", "main") is None)


def test_score_error_preserves_unknown_metrics_and_continues_other_batches():
    experiments = [{**_experiment(f"exp-{i}"), "metadata": {**_experiment()["metadata"],
                   "benchmark_run_id": f"run-{i}"}} for i in range(6)]
    def handler(request):
        if request.url.path.endswith("/experiments"):
            return _response(experiments)
        if request.url.params["experimentId"] != "exp-5":
            return httpx.Response(403, json={"message": "private-header-value"})
        return _response(_metrics("exp-5"))
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert len(result.records) == 6
    assert sum(r["pass_at_1"] is not None for r in result.records) == 1
    assert "HTTP 403" in result.warnings[0]
    assert "private-header-value" not in str(result.warnings)


def test_failed_second_score_page_discards_partial_counts():
    def handler(request):
        if request.url.path.endswith("/experiments"):
            return _response([_experiment()])
        return _response(_metrics(), "next") if not request.url.params.get("cursor") else httpx.Response(403, json={})
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert result.records[0]["pass_at_1"] is None
    assert result.records[0]["tasks"] is None
    assert result.warnings


def test_repeated_cursor_reports_incomplete_history_without_hanging():
    def handler(request):
        if request.url.path.endswith("/experiments"):
            return _response([_experiment()], "repeat")
        return _response(_metrics())
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert len(result.records) == 1
    assert "incomplete" in result.warnings[0]


def test_experiment_failure_is_visible_and_local_data_is_retained(monkeypatch):
    def handler(request):
        return httpx.Response(401, json={"message": "Invalid credentials"})
    local = history._record(metadata=_experiment()["metadata"], run_id="local",
        timestamp=datetime.fromisoformat(STAMP.replace("Z", "+00:00")),
        scores={s["name"]: s["value"] for s in _metrics()}, experiment_id=None, source="local")
    monkeypatch.setattr(history, "_read_local_runs", lambda *args: [local])
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert result.records == [local]
    assert "HTTP 401" in result.warnings[0]


def test_local_copy_overlays_the_remote_record_without_double_counting(monkeypatch):
    def handler(request):
        return _response([_experiment()]) if request.url.path.endswith("/experiments") else _response(_metrics())
    local = history._record(metadata=_experiment()["metadata"], run_id="ci-123-1-memory",
        timestamp=datetime.fromisoformat(STAMP.replace("Z", "+00:00")),
        scores={s["name"]: float(s["value"]) for s in _metrics()}, experiment_id=None, source="local")
    monkeypatch.setattr(history, "_read_local_runs", lambda *args: [local])
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW)
    assert len(result.records) == 1
    assert result.records[0]["source"] == "local"
    assert result.records[0]["experiment_id"] == "exp-1"


def test_local_metrics_use_the_publishers_score_mapping_and_configuration(tmp_path, monkeypatch):
    from runner import langfuse_reporter
    (tmp_path / "ci-123-1-memory").mkdir()
    (tmp_path / "ci-123-1-memory" / "manifest.json").touch()
    artifacts = SimpleNamespace(
        run_dir=tmp_path, manifest={"run_id": "ci-123-1-memory", "started_at": STAMP,
        "git_sha": "abc", "git_branch": "main", "metrics_k": 1, "agent_model": "test"},
        suite=SimpleNamespace(name="memory_v1", sha256="suitehash"),
        results=[{"task_id": "task", "attempt": 1}],
        metrics={"aggregate": {"pass_at_1": {"value": 0.5, "successes": 1,
                   "eligible_tasks": 2, "total_tasks": 3}}},
    )
    monkeypatch.setattr(langfuse_reporter, "_load_run_artifacts", lambda path: artifacts)
    warnings = []
    records = history._read_local_runs(tmp_path, "ci-123-1", "main", warnings)
    assert not warnings
    assert records[0]["eligible_tasks"] == 2
    assert records[0]["tasks"] == 3
    assert records[0]["suite_sha256"] == "suitehash"
    assert records[0]["workflow_run_attempt"] == "1"


def test_inconsistent_pass_counts_are_unknown():
    scores = {s["name"]: float(s["value"]) for s in _metrics()}
    scores["capability.pass_at_1.successes"] = 17
    record = history._record(metadata={}, run_id="run", timestamp=NOW,
                             scores=scores, experiment_id=None, source="langfuse")
    assert record["pass_at_1"] is None
    assert "pass_at_1" in record["missing_metrics"]


def _run_item(index, observation_id=None):
    return {"id": f"item-{index}", "datasetRunId": "exp-1", "datasetRunName": "ci-123-1-memory",
            "datasetItemId": f"task-{index}", "traceId": f"trace-{index}",
            "observationId": observation_id, "createdAt": STAMP, "updatedAt": STAMP}


def _trace(index, output, observations=None):
    return {"id": f"trace-{index}", "timestamp": STAMP,
            "input": {"task_id": f"task-{index}", "attempt": index}, "output": output,
            "tags": [], "public": False, "environment": "default", "htmlPath": "/trace",
            "observations": observations or [], "scores": []}


def _observation(id, output):
    return {"id": id, "type": "SPAN", "startTime": STAMP, "modelParameters": {},
            "input": json.dumps({"task_id": "native-task", "attempt": 2}),
            "output": json.dumps(output), "metadata": {},
            "usage": {"input": 0, "output": 0, "total": 0}, "level": "DEFAULT",
            "usageDetails": {}, "costDetails": {}, "environment": "default"}


def _experiment_item(index, output, input=None, cursor=None):
    return {"id": f"item-{index}", "traceId": f"trace-{index}", "startTime": STAMP,
            "endTime": STAMP, "level": "DEFAULT", "environment": "default",
            "experimentId": "exp-1", "experimentName": "ci-123-1-memory",
            "experimentItemId": f"task-{index}", "experimentDatasetId": "dataset-1",
            "input": input or {"task_id": f"task-{index}", "attempt": index},
            "output": output, "experimentItemMetadata": {}}


def test_failure_details_use_paginated_items_and_exact_child_observation():
    requests = []
    failed = {"status": "failed", "metrics": {"agent_eligible": True, "success": False,
              "failure_class": "agent", "first_failure_stage": "unknown"},
              "rubric": [{"id": "answer", "verdict": "no", "reason": "Returned 12, expected 42"}],
              "aiden_trace_url": "https://langfuse.test/project/p/traces/trace-1"}
    def handler(request):
        requests.append(request)
        if request.url.path.endswith("/experiments"):
            return _response([{**_experiment(), "itemCount": 3}])
        if request.url.path.endswith("/v3/scores"):
            return _response(_metrics())
        assert request.url.path.endswith("/experiment-items")
        assert request.url.params["experimentId"] == "exp-1"
        assert request.url.params["fields"] == "core,dataset,io,itemMetadata"
        cursor = request.url.params.get("cursor")
        data = [_experiment_item(1, failed, {"task_id": "native-task", "attempt": 2})] if not cursor else [
            _experiment_item(2, {"status": "passed", "metrics": {}}),
            _experiment_item(3, {"status": "judge_error", "metrics": {"error": "Judge response was invalid"}})]
        return httpx.Response(200, json={"data": data, "meta": {"cursor": "next" if not cursor else None}})
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW, include_failure_details=True)
    assert not result.warnings
    record = result.records[0]
    assert record["failure_details_status"] == "available"
    assert record["failure_details_inspected"] == 3
    failed, excluded = record["failure_details"]
    assert failed["task_id"] == "native-task" and failed["attempt"] == 2
    assert failed["evidence"][0]["reason"] == "Returned 12, expected 42"
    assert failed["trace_url"].startswith("https://")
    assert excluded["failure_class"] == "evaluation" and not excluded["agent_eligible"]
    assert len(requests) == 4


@pytest.mark.parametrize("missing_child", [False, True])
def test_inaccessible_detail_never_means_no_failures(missing_child):
    def handler(request):
        if request.url.path.endswith("/experiments"):
            return _response([{**_experiment(), "itemCount": 1}])
        if request.url.path.endswith("/v3/scores"):
            return _response(_metrics())
        if request.url.path.endswith("/experiment-items"):
            return httpx.Response(200, json={"data": [] if missing_child else [_experiment_item(1, {"status": "failed", "metrics": {}})],
                "meta": {"cursor": None}}) if missing_child else httpx.Response(403, json={"message": "private-value"})
        return httpx.Response(500, json={})
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW, include_failure_details=True)
    assert result.records[0]["failure_details_status"] == "unavailable"
    assert result.records[0]["failure_details_inspected"] == 0
    assert "0 / 1 attempt outputs" in result.warnings[0]
    assert "private-value" not in str(result.warnings)


def test_failure_details_only_fetch_latest_dataset_in_seven_day_window(monkeypatch):
    from ci import failure_details
    fetched = []
    def read(client, experiment):
        fetched.append(experiment.id)
        return {"failure_details": [], "failure_details_status": "available"}
    monkeypatch.setattr(failure_details, "fetch_failure_details", read)
    older = {**_experiment("old", benchmark_run_id="older", benchmark_started_at="2026-10-10T00:00:00Z")}
    outside = _experiment("outside", benchmark_run_id="outside", benchmark_suite="old-dataset",
                          benchmark_started_at="2026-10-01T00:00:00Z")
    def handler(request):
        return _response([_experiment(), older, outside]) if request.url.path.endswith("/experiments") else _response(_metrics())
    history.fetch_benchmark_history(client=_client(handler), now=NOW, include_failure_details=True)
    assert fetched == ["exp-1"]


def test_normalized_hard_assertions_and_missing_reasons_preserve_evidence():
    from ci.failure_details import normalize_failure
    detail = normalize_failure({"task_id": "task", "attempt": 1}, {"status": "failed", "metrics": {},
             "hard_assertion_failures": [{"id": "exit", "label": "Exit code", "requirement": 0, "actual": 1}]})
    assert detail["evidence"] == [{"kind": "Hard assertion", "label": "Exit code", "reason": "", "expected": "0", "actual": "1"}]
    assert normalize_failure({}, {"status": "timeout", "metrics": {}})["evidence"] == []


def test_partial_item_listing_keeps_retrieved_failure_evidence():
    def handler(request):
        if request.url.path.endswith("/experiments"):
            return _response([{**_experiment(), "itemCount": 2}])
        if request.url.path.endswith("/v3/scores"):
            return _response(_metrics())
        if request.url.path.endswith("/experiment-items"):
            if request.url.params.get("cursor") == "next":
                return httpx.Response(403, json={"message": "private-value"})
            return httpx.Response(200, json={"data": [_experiment_item(1, {"status": "timeout", "metrics": {"error": "Task exceeded time limit"}})],
                "meta": {"cursor": "next"}})
        return httpx.Response(500, json={})
    result = history.fetch_benchmark_history(client=_client(handler), now=NOW, include_failure_details=True)
    record = result.records[0]
    assert record["failure_details_status"] == "partial"
    assert record["failure_details_inspected"] == 1
    assert record["failure_details"][0]["evidence"][0]["reason"] == "Task exceeded time limit"
    assert any("HTTP 403" in reason for reason in record["failure_details_errors"])
    assert "private-value" not in str(record["failure_details_errors"])


def test_zero_eligible_tasks_have_no_rate_and_no_missing_counts():
    record = history._record(metadata={}, run_id="empty", timestamp=NOW,
        scores={"capability.pass_at_1.total_tasks": 4.0,
                "capability.pass_at_1.eligible_tasks": 0.0,
                "capability.pass_at_1.successes": 0.0}, experiment_id=None, source="langfuse")
    assert record["pass_at_1"] is None
    assert not record["missing_metrics"]


def test_local_preparation_logs_are_not_treated_as_failed_suite_runs(tmp_path):
    (tmp_path / "ci-123-1-prepare-images").mkdir()
    warnings = []
    assert history._read_local_runs(tmp_path, "ci-123-1", None, warnings) == []
    assert not warnings

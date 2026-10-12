from __future__ import annotations

import hashlib
import json
import shutil
import uuid
from pathlib import Path
from types import SimpleNamespace

import pytest

from runner.langfuse_reporter import (
    LangfusePublishError,
    _agent_trace_id,
    _fetch_dataset_item_via_public_api,
    publish_run,
)


class FakeAPIError(RuntimeError):
    def __init__(self, status_code: int, message: str):
        super().__init__(message)
        self.status_code = status_code


class FakeScores:
    def __init__(self, owner):
        self.owner = owner

    def create(self, **kwargs):
        if self.owner.score_error is not None:
            raise self.owner.score_error
        self.owner.score_calls.append(kwargs)
        return SimpleNamespace(id=kwargs["id"])


class FakeDatasets:
    def __init__(self, owner):
        self.owner = owner

    def get(self, name):
        if name not in self.owner.datasets:
            raise FakeAPIError(404, "not found")
        return self.owner.datasets[name]


class FakeTrace:
    def __init__(self, owner):
        self.owner = owner

    def get(self, trace_id, **kwargs):
        self.owner.trace_calls.append(trace_id)
        if self.owner.trace_error is not None:
            raise self.owner.trace_error
        return SimpleNamespace(id=trace_id)


class FakeLangfuse:
    def __init__(self, existing_run=None, score_error=None, trace_error=None):
        self.datasets = {}
        self.items = []
        self.spans = []
        self.experiment = None
        self.flushed = False
        self.existing_run = existing_run
        self.score_error = score_error
        self.score_calls = []
        self.trace_error = trace_error
        self.trace_calls = []
        self.api = SimpleNamespace(
            datasets=FakeDatasets(self),
            scores=FakeScores(self),
            trace=FakeTrace(self),
        )

    def auth_check(self):
        return True

    def create_dataset(self, **kwargs):
        dataset = SimpleNamespace(**kwargs)
        self.datasets[kwargs["name"]] = dataset
        return dataset

    def get_dataset_run(self, **kwargs):
        if self.existing_run is not None:
            return self.existing_run
        raise FakeAPIError(404, "not found")

    def create_dataset_item(self, **kwargs):
        item = SimpleNamespace(**kwargs)
        self.items.append(item)
        return item

    def update_current_span(self, **kwargs):
        self.spans.append(kwargs)

    def run_experiment(self, **kwargs):
        item_results = []
        for item in kwargs["data"]:
            output = kwargs["task"](item=item)
            evaluations = []
            for evaluator in kwargs["evaluators"]:
                evaluations.extend(
                    evaluator(
                        input=item.input,
                        output=output,
                        expected_output=item.expected_output,
                        metadata=item.metadata,
                    )
                )
            item_results.append(SimpleNamespace(
                item=item,
                output=output,
                evaluations=evaluations,
                trace_id=f"trace-{item.id}",
            ))
        run_evaluations = []
        for evaluator in kwargs["run_evaluators"]:
            run_evaluations.extend(evaluator(item_results=item_results))
        self.experiment = {**kwargs, "item_results": item_results, "run_evaluations": run_evaluations}
        existing_items = (
            list(self.existing_run.dataset_run_items)
            if self.existing_run is not None
            else []
        )
        existing_items.extend(
            SimpleNamespace(dataset_item_id=result.item.id, trace_id=result.trace_id)
            for result in item_results
        )
        self.existing_run = SimpleNamespace(
            id="dataset-run-1",
            metadata=kwargs["metadata"],
            dataset_run_items=existing_items,
        )
        return SimpleNamespace(
            dataset_run_id="dataset-run-1",
            dataset_run_url="http://langfuse.local/dataset-run-1",
            item_results=[
                SimpleNamespace(
                    **vars(item_result),
                    dataset_run_id="dataset-run-1",
                )
                for item_result in item_results
            ],
            run_evaluations=run_evaluations,
        )

    def flush(self):
        self.flushed = True

    def get_trace_url(self, *, trace_id):
        return f"http://langfuse.local/project/project-1/traces/{trace_id}"


class ExecutionLangfuse(FakeLangfuse):
    def __init__(self, *, environment="benchmark", roots_available=True):
        super().__init__()
        self.environment = environment
        self.roots_available = roots_available
        self.execution_observations = []
        self.link_calls = []
        self.api.observations = SimpleNamespace(get_many=self.get_roots)
        self.api.dataset_run_items = SimpleNamespace(create=self.link_item)

    def create_dataset_item(self, **kwargs):
        return super().create_dataset_item(dataset_id="dataset-1", **kwargs)

    def get_roots(self, **kwargs):
        self.trace_calls.append(kwargs["trace_id"])
        roots = [SimpleNamespace(id="agent-root", environment=self.environment)]
        return SimpleNamespace(data=roots if self.roots_available else [])

    def start_observation(self, **kwargs):
        record = {**kwargs, "attributes": {}, "ended": False}
        self.execution_observations.append(record)
        observation_id = f"observation-{len(self.execution_observations)}"
        return SimpleNamespace(
            id=observation_id,
            _otel_span=SimpleNamespace(set_attributes=record["attributes"].update),
            end=lambda: record.update(ended=True),
        )

    def link_item(self, **kwargs):
        self.link_calls.append(kwargs)
        existing_items = list(self.existing_run.dataset_run_items) if self.existing_run else []
        existing_items.append(SimpleNamespace(
            dataset_item_id=kwargs["dataset_item_id"], trace_id=kwargs["trace_id"],
        ))
        self.existing_run = SimpleNamespace(
            id="dataset-run-1", metadata=kwargs["metadata"], dataset_run_items=existing_items,
        )
        return SimpleNamespace(dataset_run_id="dataset-run-1")


def test_publish_run_binds_item_and_scores_to_execution_trace(tmp_path: Path):
    client = ExecutionLangfuse()
    run_dir = _write_run(tmp_path)
    published = publish_run(run_dir, client=client)
    trace_id = _agent_trace_id("episode-a")
    assert published.dataset_run_id == "dataset-run-1"
    assert published.dataset_run_url == (
        "http://langfuse.local/project/project-1/experiments/results?baseline=dataset-run-1"
    )
    assert client.experiment is None
    assert client.spans == []
    record = client.execution_observations[0]
    assert record["trace_context"] == {"trace_id": trace_id, "parent_span_id": "agent-root"}
    assert record["output"]["execution_trace_status"] == "available"
    assert record["attributes"]["langfuse.experiment.item.root_observation_id"] == "observation-1"
    assert record["attributes"]["langfuse.experiment.id"] == "dataset-run-1"
    assert record["attributes"]["langfuse.internal.as_root"] is False
    assert record["attributes"]["langfuse.experiment.metadata.benchmark_run_id"] == "run-a"
    assert client.link_calls[0]["trace_id"] == trace_id
    assert client.link_calls[0]["observation_id"] == "observation-1"
    assert record["ended"] is True
    assert all(s["trace_id"] == trace_id for s in client.score_calls if s.get("trace_id"))
    publish_run(run_dir, client=client)
    assert len(client.execution_observations) == 1


@pytest.mark.parametrize("environment", ["production", "default", "", None])
def test_publish_run_refuses_to_modify_normal_agent_trace(tmp_path: Path, environment):
    client = ExecutionLangfuse(environment=environment)
    with pytest.raises(LangfusePublishError, match="non-benchmark trace"):
        publish_run(_write_run(tmp_path), client=client)
    assert client.execution_observations == []
    assert client.link_calls == []
    assert client.score_calls == []


def test_publish_run_does_not_attach_to_unverified_execution_trace(tmp_path: Path):
    client = ExecutionLangfuse(roots_available=False)
    published = publish_run(_write_run(tmp_path), client=client)
    assert published.dataset_run_url == "http://langfuse.local/dataset-run-1"
    assert client.execution_observations == []
    assert client.link_calls == []
    output = client.experiment["item_results"][0].output
    assert output["aiden_trace_id"] == _agent_trace_id("episode-a")
    assert output["execution_trace_status"] == "pending_or_missing"


def test_publish_run_without_episode_keeps_failure_replay(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    path = run_dir / "results.jsonl"
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    for row in rows:
        row["metrics"].pop("episode_id", None)
    path.write_text("\n".join(json.dumps(row) for row in rows) + "\n")
    client = ExecutionLangfuse()
    published = publish_run(run_dir, client=client)
    assert published.dataset_run_url == "http://langfuse.local/dataset-run-1"
    assert client.execution_observations == []
    assert client.experiment is not None
    assert client.experiment["item_results"][0].output["aiden_trace_id"] is None
    assert client.experiment["item_results"][0].output["execution_trace_status"] == "no_episode"


def test_publish_run_preserves_mixed_execution_and_replay_items(tmp_path: Path):
    run_dir = _write_run(tmp_path, attempt_count=2)
    path = run_dir / "results.jsonl"
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    rows[1]["metrics"].pop("episode_id")
    path.write_text("".join(json.dumps(row) + "\n" for row in rows))
    client = ExecutionLangfuse()

    published = publish_run(run_dir, client=client)

    assert published.item_count == 2
    assert published.dataset_run_url == (
        "http://langfuse.local/project/project-1/experiments/results?baseline=dataset-run-1"
    )
    assert len(client.execution_observations) == 1
    assert len(client.experiment["data"]) == 1
    assert len(client.existing_run.dataset_run_items) == 2
    assert {score["trace_id"] for score in client.score_calls if score.get("trace_id")} == {
        _agent_trace_id("episode-a-1"),
        f"trace-{client.experiment['data'][0].id}",
    }
    publish_run(run_dir, client=client)
    assert len(client.execution_observations) == 1


def _write_run(
    tmp_path: Path,
    *,
    metrics_run_id: str = "run-a",
    suite_prompt: str = "Do task A.",
    attempt_count: int = 1,
) -> Path:
    tmp_path.mkdir(parents=True, exist_ok=True)
    suite_path = tmp_path / "suite.json"
    suite_payload = {
        "name": "smoke_suite",
        "tasks": [
            {
                "id": "task-a",
                "category": "diagnostic",
                "description_for_judge": "Complete task A.",
                "prompt": suite_prompt,
                "rubric": [{"id": "done", "check": "Task is done."}],
                "hard_assertions": {"min_tool_calls": 0, "max_tool_calls": 3},
            }
        ],
    }
    suite_bytes = json.dumps(suite_payload).encode()
    suite_path.write_bytes(suite_bytes)
    suite_sha = hashlib.sha256(suite_bytes).hexdigest()

    run_dir = tmp_path / "run-a"
    run_dir.mkdir()
    manifest = {
        "run_id": "run-a",
        "suite_path": str(suite_path),
        "suite_snapshot_path": "suite.json",
        "suite_sha256": suite_sha,
        "git_sha": "abc123",
        "git_dirty": False,
        "agent_model": "test-model",
        "metrics_schema_version": "p0-v1",
        "metrics_k": attempt_count,
        "selected_task_ids": [],
        "active_skills": [],
    }
    aggregate = {
        "tasks": 1,
        "passed": 1,
        "by_status": {"passed": 1},
        "by_category": {
            "diagnostic": {
                "passed": 1,
                "total": 1,
                "rubric_pass": 1,
                "rubric_total": 1,
            }
        },
        "unique_tasks": 1,
        "attempts": attempt_count,
        "metrics_k": attempt_count,
        "agent_eligible_attempts": attempt_count,
        "invalid_attempts": 0,
        "pass_at_1": {
            "value": 1.0, "coverage": 1.0, "successes": 1,
            "eligible_tasks": 1, "total_tasks": 1,
            "ci95": {"lower": 0.2, "upper": 1.0},
        },
        "pass_at_k": {
            "value": 1.0, "coverage": 1.0, "successes": 1,
            "eligible_tasks": 1, "total_tasks": 1,
            "ci95": {"lower": 0.2, "upper": 1.0},
        },
        "pass_pow_k": {
            "value": 1.0, "coverage": 1.0, "successes": 1,
            "eligible_tasks": 1, "total_tasks": 1,
            "ci95": {"lower": 0.2, "upper": 1.0},
        },
        "attempt_success_rate": {
            "value": 1.0, "coverage": 1.0, "successes": 1,
            "eligible_attempts": 1, "total_attempts": 1,
            "ci95": {"lower": 0.2, "upper": 1.0},
        },
        "oracle_best_score_at_k": {
            "value": 1.0, "count": 1, "eligible_tasks": 1,
        },
        "task_wall_ms": {
            "count": 1, "sum": 123.0, "mean": 123.0,
            "p50": 123.0, "p90": 123.0, "p95": 123.0,
        },
        "llm_time_ms": {
            "count": 1, "sum": 50.0, "mean": 50.0,
            "p50": 50.0, "p90": 50.0, "p95": 50.0,
        },
        "vision_llm_time_ms": {
            "count": 1, "sum": 40.0, "mean": 40.0,
            "p50": 40.0, "p90": 40.0, "p95": 40.0,
        },
        "time_to_first_token_ms": {
            "count": 1, "sum": 10.0, "mean": 10.0,
            "p50": 10.0, "p90": 10.0, "p95": 10.0,
        },
        "device_execution_ms": {
            "count": 1, "sum": 20.0, "mean": 20.0,
            "p50": 20.0, "p90": 20.0, "p95": 20.0,
        },
        "screenshot_capture_ms": {
            "count": 1, "sum": 5.0, "mean": 5.0,
            "p50": 5.0, "p90": 5.0, "p95": 5.0,
        },
        "retry_count": {
            "count": 1, "sum": 1.0, "mean": 1.0,
            "p50": 1.0, "p90": 1.0, "p95": 1.0,
        },
        "tool_calls": {
            "count": 1, "sum": 2.0, "mean": 2.0,
            "p50": 2.0, "p90": 2.0, "p95": 2.0,
        },
        "first_success_attempt": {"count": 1, "mean": 1.0, "p50": 1.0},
        "failure_classes": {},
        "failure_stages": {},
        "failure_stage_coverage": {
            "classified": 0, "eligible_failures": 0, "value": None,
        },
        "trace_observations": {
            "used_memory": {
                "tasks_with_observation": 1,
                "tasks_observed": 1,
            }
        },
    }
    result = {
        "suite": "smoke_suite",
        "run_id": "run-a",
        "task_id": "task-a",
        "category": "diagnostic",
        "attempt": 1,
        "status": "passed",
        "rubric": [{"id": "done", "verdict": "yes", "reason": "done"}],
        "rubric_pass_count": 1,
        "rubric_total": 1,
        "hard_assertions": {"min_tool_calls": True},
        "hard_assertion_failures": [],
        "metrics": {
            "success": True,
            "agent_eligible": True,
            "quality_score": 1.0,
            "task_wall_ms": 123,
            "llm_time_ms": 50,
            "vision_llm_time_ms": 40,
            "time_to_first_token_ms": 10,
            "device_execution_ms": 20,
            "screenshot_capture_ms": 5,
            "retry_count": 1,
            "tool_calls": 2,
            "screenshots_taken": 1,
            "expected_answer_match": True,
            "expected_recalled_memory_match": False,
            "trace_observations": [
                {"id": "used_memory", "passed": True, "reason": "found"}
            ],
            "episode_id": "episode-a",
        },
        "artifact_dir": str(run_dir / "tasks" / "task-a"),
        "started_at": "2026-09-20T00:00:00+00:00",
        "finished_at": "2026-09-20T00:00:01+00:00",
    }
    (run_dir / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")
    (run_dir / "suite.json").write_bytes(suite_bytes)
    (run_dir / "metrics.json").write_text(
        json.dumps({
            "schema_version": "p0-v1",
            "suite": "smoke_suite",
            "run_id": metrics_run_id,
            "suite_sha256": suite_sha,
            "metrics_k": attempt_count,
            "aggregate": aggregate,
        }),
        encoding="utf-8",
    )
    results = [
        {
            **result,
            "attempt": attempt,
            "metrics": {
                **result["metrics"],
                "episode_id": (
                    "episode-a"
                    if attempt_count == 1
                    else f"episode-a-{attempt}"
                ),
            },
        }
        for attempt in range(1, attempt_count + 1)
    ]
    (run_dir / "results.jsonl").write_text(
        "".join(json.dumps(row) + "\n" for row in results),
        encoding="utf-8",
    )
    return run_dir


def test_publish_run_maps_attempts_and_aggregate_metrics(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    client = FakeLangfuse()

    published = publish_run(run_dir, client=client)

    assert published.run_name == "run-a"
    assert published.item_count == 1
    assert published.dataset_run_url == "http://langfuse.local/dataset-run-1"
    assert published.dataset_name == "aiden-benchmark:smoke_suite"
    assert client.items[0].input == {
        "task_id": "task-a",
        "attempt": 1,
        "category": "diagnostic",
        "prompt": "Do task A.",
    }
    item_scores = {
        score["name"]: score["value"]
        for score in client.score_calls
        if score.get("trace_id")
    }
    assert item_scores["benchmark.success"] == 1
    assert item_scores["benchmark.task_wall_ms"] == 123
    assert item_scores["benchmark.time_to_first_token_ms"] == 10
    assert item_scores["benchmark.device_execution_ms"] == 20
    assert item_scores["benchmark.retry_count"] == 1
    assert item_scores["benchmark.screenshots_taken"] == 1
    assert item_scores["benchmark.expected_answer_match"] == 1
    assert item_scores["benchmark.expected_recalled_memory_match"] == 0
    assert item_scores["benchmark.trace_observation.used_memory"] == 1
    run_scores = {
        score["name"]: score["value"]
        for score in client.score_calls
        if score.get("dataset_run_id")
    }
    assert run_scores["capability.pass_at_1"] == 1.0
    assert run_scores["capability.pass_at_1.successes"] == 1
    assert run_scores["capability.pass_at_1.ci95_lower"] == 0.2
    assert run_scores["capability.attempt_success_rate.total_attempts"] == 1
    assert run_scores["efficiency.task_wall_ms.count"] == 1
    assert run_scores["efficiency.task_wall_ms.mean"] == 123.0
    assert run_scores["efficiency.task_wall_ms.p50"] == 123.0
    assert run_scores["efficiency.task_wall_ms.p95"] == 123.0
    assert run_scores["efficiency.llm_time_ms.p50"] == 50.0
    assert run_scores["efficiency.time_to_first_token_ms.p90"] == 10.0
    assert run_scores["reliability.retry_count.sum"] == 1.0
    assert run_scores["capability.first_success_attempt.p50"] == 1.0
    assert run_scores["status.passed"] == 1
    assert not any(name.startswith("category.") for name in run_scores)
    assert run_scores["observations.used_memory.pass_rate"] == 1.0
    assert client.experiment["metadata"]["git_sha"] == "abc123"
    assert len(client.experiment["metadata"]["workload_sha256"]) == 64
    assert client.spans[0]["metadata"]["aiden_episode_id"] == "episode-a"
    trace_id = uuid.uuid5(uuid.NAMESPACE_URL, "episode-a").hex
    expected_url = f"http://langfuse.local/project/project-1/traces/{trace_id}"
    output = client.experiment["item_results"][0].output
    assert output["aiden_trace_id"] == trace_id
    assert output["aiden_trace_url"] == expected_url
    assert client.spans[0]["metadata"]["aiden_trace_url"] == expected_url
    assert client.flushed is True


@pytest.mark.parametrize("client_type", [FakeLangfuse, ExecutionLangfuse])
def test_publish_run_retains_category_data_without_category_scores(tmp_path: Path, client_type):
    run_dir = _write_run(tmp_path)
    metrics_path = run_dir / "metrics.json"
    metrics = json.loads(metrics_path.read_text())
    metrics["aggregate"]["by_category"]["memory"] = {
        "passed": 2, "total": 3, "rubric_pass": 4, "rubric_total": 6,
    }
    metrics_path.write_text(json.dumps(metrics), encoding="utf-8")
    client = client_type()

    publish_run(run_dir, client=client)

    assert not any(score["name"].startswith("category.") for score in client.score_calls)
    assert any(score["name"] == "capability.pass_at_1" for score in client.score_calls)
    assert client.items[0].input["category"] == "diagnostic"
    assert client.items[0].metadata["category"] == "diagnostic"
    assert json.loads(metrics_path.read_text()) == metrics


@pytest.mark.parametrize("client_type", [FakeLangfuse, ExecutionLangfuse])
def test_publish_run_keeps_scores_when_trace_url_lookup_fails(tmp_path: Path, client_type):
    client = client_type()

    def unavailable(**kwargs):
        raise RuntimeError("project lookup unavailable")

    client.get_trace_url = unavailable
    published = publish_run(_write_run(tmp_path), client=client)
    if isinstance(client, ExecutionLangfuse):
        assert client.execution_observations[0]["output"]["aiden_trace_url"] is None
        assert published.dataset_run_url is None
    else:
        assert client.experiment["item_results"][0].output["aiden_trace_url"] is None
    assert client.score_calls


def test_agent_trace_id_matches_otlp_uuid_format():
    assert _agent_trace_id("8f1c0f4e-6c2f-4a2e-9f1e-2f6b0f0d3a11") == "8f1c0f4e6c2f4a2e9f1e2f6b0f0d3a11"
    assert _agent_trace_id(None) is None


def test_publish_run_uses_stable_dataset_and_item_ids(tmp_path: Path):
    first_run = _write_run(tmp_path / "first")
    second_run = _write_run(
        tmp_path / "second",
        suite_prompt="Do the revised task A.",
    )
    first = FakeLangfuse()
    second = FakeLangfuse()

    first_result = publish_run(first_run, client=first)
    second_result = publish_run(second_run, client=second)

    assert first_result.dataset_name == second_result.dataset_name
    assert first.items[0].id == second.items[0].id
    assert first.items[0].input != second.items[0].input
    assert (
        first.experiment["metadata"]["suite_sha256"]
        != second.experiment["metadata"]["suite_sha256"]
    )


def test_publish_run_includes_answer_and_tools_from_moved_repeated_run(tmp_path: Path):
    original = _write_run(tmp_path / "original", attempt_count=2)
    rows = [json.loads(line) for line in (original / "results.jsonl").read_text().splitlines()]
    for row in rows:
        directory = original / "tasks" / "task-a" / f"attempt_{row['attempt']}"
        directory.mkdir(parents=True)
        row["artifact_dir"] = str(directory)
        (directory / "trace.json").write_text(json.dumps({
            "final_response": f"已更新为浅色模式（第 {row['attempt']} 次）",
            "tool_calls": [{"step": 1, "tool": "save_memory", "input": {"content": "light mode"}}],
        }), encoding="utf-8")
    (original / "results.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows))
    moved = tmp_path / "downloaded" / "run-a"
    shutil.copytree(original, moved)
    # The original still exists: publication must use the downloaded evidence.
    (original / "tasks/task-a/attempt_1/trace.json").write_text('{}')
    client = FakeLangfuse()

    publish_run(moved, client=client)

    outputs = [result.output for result in client.experiment["item_results"]]
    assert [output["final_response"] for output in outputs] == [
        "已更新为浅色模式（第 1 次）", "已更新为浅色模式（第 2 次）",
    ]
    assert outputs[0]["tool_calls"][0]["input"] == {"content": "light mode"}
    assert outputs[1]["trace_artifact"] == {
        "status": "available", "path": "tasks/task-a/attempt_2/trace.json",
    }


@pytest.mark.parametrize("payload,expected_status,answer", [
    (None, "missing", None),
    ('{"final_response": "", "tool_calls": []}', "available", ""),
    ('{"final_response": "answer", "tool_calls": []}', "available", "answer"),
    ('{broken', "invalid", None),
    ('[]', "invalid", None),
    ('{"final_response": 5, "tool_calls": []}', "invalid", None),
])
def test_publish_run_distinguishes_missing_invalid_and_empty_answers(
    tmp_path: Path, payload, expected_status, answer,
):
    run_dir = _write_run(tmp_path)
    directory = run_dir / "tasks/task-a"
    directory.mkdir(parents=True)
    if payload is not None:
        (directory / "trace.json").write_text(payload)
    client = FakeLangfuse()

    publish_run(run_dir, client=client)

    output = client.experiment["item_results"][0].output
    assert output["final_response"] == answer
    assert output["trace_artifact"]["status"] == expected_status
    assert output["status"] == "passed"


def test_publish_run_does_not_borrow_trace_from_another_attempt_or_outside_run(tmp_path: Path):
    run_dir = _write_run(tmp_path, attempt_count=2)
    directory = run_dir / "tasks/task-a"
    directory.mkdir(parents=True)
    (directory / "trace.json").write_text(json.dumps({"final_response": "wrong attempt", "tool_calls": []}))
    outside = tmp_path / "private"
    outside.mkdir()
    (outside / "trace.json").write_text(json.dumps({"final_response": "outside", "tool_calls": []}))
    (directory / "attempt_1").symlink_to(outside, target_is_directory=True)
    rows = [json.loads(line) for line in (run_dir / "results.jsonl").read_text().splitlines()]
    for row in rows:
        row["artifact_dir"] = f"tasks/task-a/attempt_{row['attempt']}"
    (run_dir / "results.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows))
    client = FakeLangfuse()

    publish_run(run_dir, client=client)

    assert all(result.output["final_response"] is None for result in client.experiment["item_results"])
    assert all(result.output["trace_artifact"]["status"] == "missing" for result in client.experiment["item_results"])


def test_publish_run_serializes_dataset_run_item_creation(tmp_path: Path):
    run_dir = _write_run(tmp_path, attempt_count=3)
    client = FakeLangfuse()

    published = publish_run(run_dir, client=client)

    assert published.item_count == 3
    assert client.experiment["max_concurrency"] == 1


def test_publish_run_reports_publication_progress(tmp_path: Path):
    progress = []

    publish_run(_write_run(tmp_path), client=FakeLangfuse(), progress=progress.append)

    assert any(
        "dataset=aiden-benchmark:smoke_suite run=run-a items=1" in message
        for message in progress
    )
    assert any("experiment publication start" in message for message in progress)
    assert any("benchmark scores published" in message for message in progress)


def test_publish_run_uses_suite_snapshot_when_source_changes(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    Path(manifest["suite_path"]).write_text("{}", encoding="utf-8")
    client = FakeLangfuse()

    publish_run(run_dir, client=client)

    assert client.items[0].input["prompt"] == "Do task A."


def test_publish_run_rejects_mismatched_artifacts_before_network(tmp_path: Path):
    run_dir = _write_run(tmp_path, metrics_run_id="different")

    with pytest.raises(LangfusePublishError, match="run_id values differ"):
        publish_run(run_dir, client=FakeLangfuse())


def test_publish_run_wraps_invalid_suite_as_publish_error(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    (run_dir / "suite.json").write_text("{}", encoding="utf-8")
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    manifest["suite_sha256"] = hashlib.sha256(b"{}").hexdigest()
    (run_dir / "manifest.json").write_text(json.dumps(manifest), encoding="utf-8")

    with pytest.raises(LangfusePublishError, match="Langfuse publish failed"):
        publish_run(run_dir, client=FakeLangfuse())


def test_publish_run_rejects_incomplete_experiment_result(tmp_path: Path):
    class IncompleteExperimentLangfuse(FakeLangfuse):
        def run_experiment(self, **kwargs):
            return SimpleNamespace(
                dataset_run_id=None,
                dataset_run_url=None,
                item_results=[],
                run_evaluations=[],
            )

    with pytest.raises(LangfusePublishError, match="did not link all dataset items"):
        publish_run(_write_run(tmp_path), client=IncompleteExperimentLangfuse())


def test_publish_run_fails_when_existing_run_lookup_fails(tmp_path: Path):
    class UnavailableLangfuse(FakeLangfuse):
        def get_dataset_run(self, **kwargs):
            raise FakeAPIError(503, "temporarily unavailable")

    with pytest.raises(LangfusePublishError, match="temporarily unavailable"):
        publish_run(_write_run(tmp_path), client=UnavailableLangfuse())


def test_publish_run_repairs_incomplete_existing_run(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    seed = FakeLangfuse()
    publish_run(run_dir, client=seed)
    existing_run = SimpleNamespace(
        id="dataset-run-1",
        metadata=seed.experiment["metadata"],
        dataset_run_items=[],
    )
    client = FakeLangfuse(existing_run=existing_run)

    published = publish_run(run_dir, client=client)

    assert published.already_exists is False
    assert len(client.items) == 1
    assert client.experiment is not None


def test_publish_run_repairs_scores_for_complete_existing_run(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    seed = FakeLangfuse()
    publish_run(run_dir, client=seed)
    item_result = seed.experiment["item_results"][0]
    existing_run = SimpleNamespace(
        id="dataset-run-1",
        metadata=seed.experiment["metadata"],
        dataset_run_items=[SimpleNamespace(
            dataset_item_id=item_result.item.id,
            trace_id="existing-trace-1",
        )],
    )
    client = FakeLangfuse(existing_run=existing_run)

    published = publish_run(run_dir, client=client)

    assert published.already_exists is True
    assert client.experiment is None
    assert any(score.get("trace_id") == "existing-trace-1" for score in client.score_calls)
    assert any(score.get("dataset_run_id") == "dataset-run-1" for score in client.score_calls)


def test_publish_run_rejects_reused_run_id_with_different_results(tmp_path: Path):
    run_dir = _write_run(tmp_path)
    first = FakeLangfuse()
    publish_run(run_dir, client=first)
    item_result = first.experiment["item_results"][0]
    existing_run = SimpleNamespace(
        id="dataset-run-1",
        metadata=first.experiment["metadata"],
        dataset_run_items=[SimpleNamespace(
            dataset_item_id=item_result.item.id,
            trace_id=item_result.trace_id,
        )],
    )
    row = json.loads((run_dir / "results.jsonl").read_text(encoding="utf-8"))
    row["metrics"]["task_wall_ms"] = 999
    (run_dir / "results.jsonl").write_text(json.dumps(row) + "\n", encoding="utf-8")

    with pytest.raises(LangfusePublishError, match="conflicts with benchmark artifacts"):
        publish_run(run_dir, client=FakeLangfuse(existing_run=existing_run))


def test_publish_run_fails_when_score_write_fails(tmp_path: Path):
    client = FakeLangfuse(score_error=FakeAPIError(503, "score storage unavailable"))

    with pytest.raises(LangfusePublishError, match="score storage unavailable"):
        publish_run(_write_run(tmp_path), client=client)


def test_publish_run_publishes_scores_without_reading_the_run_back(tmp_path: Path):
    """Scores must not depend on a read-back that Langfuse may not serve yet.

    Self-hosted Langfuse acknowledges the experiment write before the dataset run
    is readable, so a publisher that re-reads the run to collect trace IDs never
    reaches the score phase. This client serves the write and then refuses every
    subsequent run lookup.
    """

    class WriteOnlyLangfuse(FakeLangfuse):
        def __init__(self):
            super().__init__()
            self.published = False

        def get_dataset_run(self, **kwargs):
            if self.published:
                raise FakeAPIError(404, "run is not readable yet")
            return super().get_dataset_run(**kwargs)

        def run_experiment(self, **kwargs):
            result = super().run_experiment(**kwargs)
            self.published = True
            return result

    client = WriteOnlyLangfuse()

    published = publish_run(_write_run(tmp_path), client=client)

    assert published.dataset_run_id == "dataset-run-1"
    score_names = {call["name"] for call in client.score_calls}
    assert "capability.pass_at_1" in score_names
    assert "benchmark.success" in score_names
    attempt_scores = [call for call in client.score_calls if call["trace_id"]]
    assert attempt_scores
    assert all(
        call["trace_id"].startswith("trace-") for call in attempt_scores
    )


def test_publish_run_does_not_create_dataset_after_lookup_failure(tmp_path: Path):
    class UnavailableDatasets(FakeDatasets):
        def get(self, name):
            raise FakeAPIError(503, "dataset storage unavailable")

    client = FakeLangfuse()
    client.api.datasets = UnavailableDatasets(client)

    with pytest.raises(LangfusePublishError, match="dataset storage unavailable"):
        publish_run(_write_run(tmp_path), client=client)

    assert client.datasets == {}


def test_langfuse_v3_dataset_item_compatibility(monkeypatch):
    monkeypatch.setattr(
        "runner.langfuse_reporter._public_api_get",
        lambda path: {
            "id": "item-1",
            "status": "ACTIVE",
            "input": {"task_id": "task-a", "attempt": 1},
            "expectedOutput": {"ok": True},
            "metadata": {},
            "sourceTraceId": None,
            "sourceObservationId": None,
            "datasetId": "dataset-1",
            "datasetName": "benchmark",
            "createdAt": "2026-09-20T00:00:00Z",
            "updatedAt": "2026-09-20T00:00:00Z",
        },
    )

    item = _fetch_dataset_item_via_public_api("item-1")

    assert item.id == "item-1"
    assert item.dataset_id == "dataset-1"
    assert item.input == {"task_id": "task-a", "attempt": 1}

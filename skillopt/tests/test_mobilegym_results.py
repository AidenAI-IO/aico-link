import json
from pathlib import Path

import pytest

from runner.judge import JudgeConfig, JudgeOutput
from runner.models import RubricVerdict
import runner.runtask as runtask_mod
from runner.suite import HardAssertions, RubricItem, Suite, TaskSpec, TraceObservationSpec


def _task(*, expected_answer: str | None = None, timeout_sec: int = 180) -> TaskSpec:
    return TaskSpec(
        id="case_one",
        category="single_step",
        description_for_judge="Judge case one.",
        prompt="Do case one.",
        rubric=[RubricItem(id="ok", check="Task succeeds.")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=2, must_complete_within_sec=timeout_sec, response_required=True),
        expected_answer=expected_answer,
        answer_format="option_letter" if expected_answer else None,
    )


def _suite(tmp_path: Path, *, expected_answer: str | None = None, timeout_sec: int = 180) -> Suite:
    task = _task(expected_answer=expected_answer, timeout_sec=timeout_sec)
    return Suite(
        name="device_operator_train",
        global_reset={},
        tasks=[task],
        sha256="sha",
        source_path=tmp_path / "device_operator_train.json",
    )


def _suite_with_trace_requirement(tmp_path: Path) -> Suite:
    task = TaskSpec(
        id="case_one",
        category="single_step",
        description_for_judge="Judge case one.",
        prompt="Do case one.",
        rubric=[RubricItem(id="ok", check="Task succeeds.")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=5, must_complete_within_sec=180, response_required=True),
    )
    return Suite(
        name="device_operator_train",
        global_reset={},
        tasks=[task],
        sha256="sha",
        source_path=tmp_path / "device_operator_train.json",
        trace_observations=[TraceObservationSpec(id="skill_read", description="read skill", skill_name="device-operator")],
    )


def _write_jsonl(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8")


def _write_result(batch: Path, suite: Suite, row: dict) -> Path:
    _write_jsonl(batch / suite.name / "shard-0" / "raw" / "run" / "results.jsonl", [row])
    return batch


def _write_error(batch: Path, suite: Suite, row: dict) -> Path:
    _write_jsonl(batch / suite.name / "shard-0" / "raw" / "run" / "errors.jsonl", [row])
    return batch


def _write_evidence(batch: Path, suite: Suite, task: TaskSpec, evidence: dict) -> None:
    meta = {"task_id": f"{suite.name}.{task.id}", **evidence}
    path = batch / suite.name / "shard-0" / "raw" / "run" / "trajectory" / f"{suite.name}_{task.id}" / "meta.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(meta, ensure_ascii=False, indent=2), encoding="utf-8")


def _load_results(batch: Path, suite: Suite, tmp_path: Path, *, judge_cfg=None):
    from skillopt import mobilegym_results

    return mobilegym_results.load_aiden_suite_task_results(
        batch_dir=batch,
        suite=suite,
        tasks=suite.tasks,
        run_id="run-1",
        phase_artifact_dir=tmp_path / "phase",
        judge_cfg=judge_cfg,
        judge_cache_dir=None,
    )


def test_mobilegym_passed_row_without_aiden_history_is_hard_failure(tmp_path: Path):
    suite = _suite(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {"id": f"{suite.name}.case_one", "is_success": True},
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "failed"
    assert "missing aiden_last_chat_history" in results[0].metrics["error"]


def test_mobilegym_passed_row_uses_execution_agent_message_as_aiden_response(tmp_path: Path):
    suite = _suite(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {
            "id": f"{suite.name}.case_one",
            "is_success": True,
            "execution": {
                "stop_reason": "COMPLETE",
                "agent_message": "done from mobilegym execution",
            },
        },
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "skipped"
    assert results[0].metrics["success"] is None
    assert results[0].metrics["quality_score"] is None
    assert results[0].metrics["agent_eligible"] is False
    assert results[0].metrics["mobilegym_status"] == "passed"
    assert results[0].metrics["aiden_history_source"] == "mobilegym_execution_agent_message"


def test_mobilegym_passed_row_with_mobilegym_judge_still_uses_aiden_checks(tmp_path: Path):
    suite = _suite_with_trace_requirement(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {
            "id": f"{suite.name}.case_one",
            "is_success": True,
            "execution": {
                "stop_reason": "COMPLETE",
                "agent_message": "done from mobilegym execution",
                "runtime_s": 10,
            },
            "judge": {"passed": True, "success": True, "issues": []},
        },
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "failed"
    assert results[0].rubric_pass_count == 0
    assert "missing aiden_last_chat_history" in results[0].metrics["error"]


@pytest.mark.parametrize("verdict,expected_status", [(None, "skipped"), ("yes", "passed"), ("no", "failed")])
def test_mobilegym_passed_row_uses_meta_evidence_and_aiden_checks(
    tmp_path: Path, monkeypatch, verdict, expected_status
):
    suite = _suite(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {"id": f"{suite.name}.case_one", "is_success": True},
    )
    _write_evidence(
        batch,
        suite,
        suite.tasks[0],
        {
            "aiden_last_response": "done",
            "aiden_last_chat_history": [{"type": "assistant", "content": "done"}],
        },
    )

    judged = []

    def judge(**kwargs):
        judged.append(kwargs)
        return JudgeOutput([RubricVerdict("ok", verdict, "Evidence checked")], "", "key", "")

    monkeypatch.setattr(runtask_mod, "judge_task", judge)
    results = _load_results(batch, suite, tmp_path, judge_cfg=JudgeConfig() if verdict else None)

    assert results[0].status == expected_status
    assert results[0].metrics["mobilegym_status"] == "passed"
    assert results[0].metrics["success"] is (verdict == "yes" if verdict else None)
    assert results[0].metrics["agent_eligible"] is (verdict is not None)
    if verdict:
        assert len(judged) == 1
        assert judged[0]["final_response"] == "done"
        assert judged[0]["rubric"] == suite.tasks[0].rubric
    else:
        assert judged == []
        assert results[0].metrics["quality_score"] is None


def test_mobilegym_passed_row_over_task_timeout_fails_aiden_timeout_check(tmp_path: Path):
    suite = _suite(tmp_path, timeout_sec=1)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {
            "id": f"{suite.name}.case_one",
            "is_success": True,
            "execution": {"runtime_s": 2.5},
        },
    )
    _write_evidence(
        batch,
        suite,
        suite.tasks[0],
        {
            "aiden_last_response": "done",
            "aiden_last_chat_history": [{"type": "assistant", "content": "done"}],
        },
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "timeout"
    assert results[0].hard_assertions is not None
    assert results[0].hard_assertions.timeout is False


def test_mobilegym_passed_row_still_fails_expected_answer_mismatch(tmp_path: Path):
    suite = _suite(tmp_path, expected_answer="(c)")
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {
            "id": f"{suite.name}.case_one",
            "is_success": True,
            "aiden_last_response": "<final_answer>(b)</final_answer>",
            "aiden_last_chat_history": [{"type": "assistant", "content": "<final_answer>(b)</final_answer>"}],
        },
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "failed"
    assert results[0].metrics["expected_answer_match"] is False


def test_mobilegym_raw_failure_gates_before_aiden_checks(tmp_path: Path):
    suite = _suite(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {
            "id": f"{suite.name}.case_one",
            "is_success": False,
            "aiden_last_response": "done",
            "aiden_last_chat_history": [{"type": "assistant", "content": "done"}],
        },
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "failed"
    assert results[0].metrics["mobilegym_status"] == "failed"


def test_mobilegym_errors_jsonl_wins_over_success_row(tmp_path: Path):
    suite = _suite(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {
            "id": f"{suite.name}.case_one",
            "is_success": True,
            "aiden_last_response": "done",
            "aiden_last_chat_history": [{"type": "assistant", "content": "done"}],
        },
    )
    _write_error(
        batch,
        suite,
        {"id": f"{suite.name}.case_one", "error": "AidenAdapterTimeout: deadline"},
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "timeout"
    assert results[0].metrics["mobilegym_status"] == "timeout"


def test_mobilegym_unknown_status_becomes_failed_task_result(tmp_path: Path):
    suite = _suite(tmp_path)
    batch = _write_result(
        tmp_path / "batch",
        suite,
        {"id": f"{suite.name}.case_one", "status": "mystery"},
    )

    results = _load_results(batch, suite, tmp_path)

    assert results[0].status == "failed"
    assert results[0].metrics["mobilegym_status"] == "unknown"


def test_mobilegym_worker_failure_without_rows_fails_loudly(tmp_path: Path):
    from skillopt import mobilegym_results

    suite = _suite(tmp_path)
    batch = tmp_path / "batch"
    shard = batch / suite.name / "shard-0"
    shard.mkdir(parents=True)
    (shard / "shard.json").write_text(json.dumps({"exit_code": 7}), encoding="utf-8")

    with pytest.raises(mobilegym_results.MobileGymResultError, match="worker failed"):
        _load_results(batch, suite, tmp_path)

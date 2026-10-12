from __future__ import annotations
import base64
import dataclasses as dc
import json
import shutil
import time
from pathlib import Path
from typing import Any

from PIL import Image

from runner.agent_client import AgentClient, AgentRequestError, AgentTimeoutError
from runner.assertions import (
    evaluate_environment_state_assertions,
    evaluate_expected_answer,
    evaluate_expected_recalled_memory_ids,
    evaluate_hard_assertions,
    evaluate_trace_observations,
)
from runner.capture import take_environment_screenshot
from runner.environment_state import read_environment_state
from runner.judge import judge_task, JudgeConfig
from runner.metrics import derive_episode_metrics, derive_history_metrics
from runner.models import HardAssertionFailure, HardAssertionResults, RubricVerdict, TaskResult
from runner.recovery import prepare_task_isolation, recover_agent_after_timeout
from runner.reset import ResetError, SetupAssertionError, assert_memory_results, read_memory_results
from runner.suite import Suite, TaskSpec, effective_mock_environment
from runner.trace import extract_trace
from runner.report import now_iso


def _attempt_metric_defaults() -> dict[str, Any]:
    return {
        "success": None,
        "agent_eligible": False,
        "failure_class": None,
        "quality_score": None,
        "task_wall_ms": None,
        "setup_ms": None,
        "tool_calls": None,
        "device_actions": None,
        "llm_calls": None,
        "llm_time_ms": None,
        "vision_llm_time_ms": None,
        "time_to_first_token_ms": None,
        "device_execution_ms": None,
        "screenshot_capture_ms": None,
        "screenshot_capture_source": None,
        "input_tokens": None,
        "output_tokens": None,
        "total_tokens": None,
        "cached_input_tokens": None,
        "reasoning_tokens": None,
        "cost_usd": None,
        "cost_source": None,
        "tool_errors": None,
        "replan_count": None,
        "retry_count": None,
        "recovery_attempted": False,
        "recovery_succeeded": None,
        "first_failure_stage": None,
        "failure_event_ref": None,
    }


def skipped_task_result(
    suite: Suite,
    task: TaskSpec,
    attempt: int,
    artifact_dir: Path,
    run_id: str,
    error: str,
) -> TaskResult:
    artifact_dir.mkdir(parents=True, exist_ok=True)
    started = now_iso()
    return TaskResult(
        suite=suite.name,
        run_id=run_id,
        task_id=task.id,
        category=task.category,
        attempt=attempt,
        status="skipped",
        rubric=[],
        rubric_pass_count=0,
        rubric_total=len(task.rubric),
        artifact_dir=str(artifact_dir),
        started_at=started,
        finished_at=started,
        description_for_judge=task.description_for_judge,
        rubric_spec=[dc.asdict(r) for r in task.rubric],
        metrics={
            **_attempt_metric_defaults(),
            "error": error,
            "success": None,
            "agent_eligible": False,
            "failure_class": "skipped",
            "first_failure_stage": "setup",
        },
    )


def _set_outcome_metrics(
    result: TaskResult,
    *,
    success: bool | None,
    eligible: bool,
    failure_class: str | None = None,
    stage: str | None = None,
    evidence_ref: str | None = None,
) -> None:
    result.metrics["success"] = success
    result.metrics["agent_eligible"] = eligible
    result.metrics["failure_class"] = failure_class
    if success is True:
        result.metrics["first_failure_stage"] = None
        result.metrics["failure_event_ref"] = None
        return
    if stage is not None:
        result.metrics["first_failure_stage"] = stage
    if evidence_ref is not None:
        result.metrics["failure_event_ref"] = evidence_ref


def _failure_stage(result: TaskResult) -> str:
    recorded_stage = str(result.metrics.get("first_failure_stage") or "")
    if recorded_stage and recorded_stage != "unknown":
        return recorded_stage
    return "unknown"


def evaluate_task_history(
    *,
    suite: Suite,
    task: TaskSpec,
    history: list[dict[str, Any]],
    attempt: int,
    artifact_dir: Path,
    judge_cfg: JudgeConfig | None,
    judge_cache_dir: Path | None,
    run_id: str,
    timed_out: bool,
    metrics: dict[str, Any] | None = None,
    pre_screenshot: Path | None = None,
    post_screenshot: Path | None = None,
    started_at: str | None = None,
    started_mono: float | None = None,
    active_skills: list[str] | None = None,
    episode: dict[str, Any] | None = None,
    memory_state: list[dict[str, Any]] | None = None,
    memory_state_error: str = "",
) -> TaskResult:
    artifact_dir.mkdir(parents=True, exist_ok=True)
    started = started_at or now_iso()
    base = TaskResult(
        suite=suite.name,
        run_id=run_id,
        task_id=task.id,
        category=task.category,
        attempt=attempt,
        status="failed",
        rubric=[],
        rubric_pass_count=0,
        rubric_total=len(task.rubric),
        artifact_dir=str(artifact_dir),
        started_at=started,
        description_for_judge=task.description_for_judge,
        rubric_spec=[dc.asdict(r) for r in task.rubric],
        metrics={**_attempt_metric_defaults(), **(metrics or {})},
    )
    (artifact_dir / "history.json").write_text(
        json.dumps(history, ensure_ascii=False, indent=2), encoding="utf-8")
    if episode is not None:
        (artifact_dir / "episode.json").write_text(
            json.dumps(episode, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
    trace = extract_trace(history)
    recorded_task_wall = base.metrics.get("task_wall_ms")
    if isinstance(recorded_task_wall, (int, float)) and not isinstance(recorded_task_wall, bool):
        wall_ms = int(recorded_task_wall)
    elif started_mono is None:
        wall_ms = None
    else:
        wall_ms = int((time.monotonic() - started_mono) * 1000)
    active_skills = _normalise_active_skills(active_skills)
    if active_skills:
        base.metrics["active_skills"] = active_skills
    if suite.trace_observations:
        observation_results = evaluate_trace_observations(
            trace,
            suite.trace_observations,
            active_skills=active_skills,
        )
        base.metrics["trace_observations"] = [
            {
                "id": item.id,
                "description": item.description,
                "passed": item.passed,
                "reason": item.reason,
            }
            for item in observation_results
        ]
    trace_dict = {
        "tool_calls": [dc.asdict(tc) for tc in trace.tool_calls],
        "tool_results": [
            msg for msg in history
            if msg.get("type") == "tool_result" and msg.get("tool_name") in {
                "save_memory", "recall_memory", "forget_memory", "recall_session_chunks",
            }
        ],
        "final_response": trace.final_response,
        "total_tool_calls": trace.total_tool_calls,
    }
    if task.memory_assertions is not None:
        # Harness observations are evidence, not Agent tool calls.
        trace_dict["memory_state"] = memory_state
        trace_dict["memory_assertions"] = task.memory_assertions
    last_shot_path = post_screenshot if post_screenshot is not None and post_screenshot.exists() else None
    base.metrics.update(derive_history_metrics(history))
    base.metrics.update(derive_episode_metrics(episode))
    base.metrics.setdefault("cost_usd", None)
    base.metrics.setdefault("cost_source", None)
    base.metrics.setdefault("time_to_first_token_ms", None)
    base.metrics.setdefault("screenshot_capture_ms", None)
    base.metrics.setdefault("recovery_attempted", False)
    base.metrics.setdefault("recovery_succeeded", None)
    base.metrics.update({"wall_ms": wall_ms, "task_wall_ms": wall_ms, "tool_calls": trace.total_tool_calls,
                         "screenshots_taken": sum(1 for tc in trace.tool_calls if tc.has_screenshot),
                         "pre_screenshot_file": bool(pre_screenshot and pre_screenshot.exists()),
                         "post_screenshot_file": bool(post_screenshot and post_screenshot.exists())})
    recall_outcome = None
    if task.expected_recalled_memory_ids:
        recall_outcome = evaluate_expected_recalled_memory_ids(
            history,
            task.expected_recalled_memory_ids,
            episode=episode,
            recall_tool=task.expected_recalled_memory_tool,
            require_inline_recall=task.expected_recall_from_consolidation,
        )
        base.metrics.update({
            "expected_recalled_memory_ids": recall_outcome.expected_memory_ids,
            "recalled_memory_ids": recall_outcome.recalled_memory_ids,
            "memory_recall_evidence_source": recall_outcome.evidence_source,
            "expected_recalled_memory_match": recall_outcome.passed,
        })
        trace_dict.update({
            "recalled_memory_ids": recall_outcome.recalled_memory_ids,
            "memory_recall_evidence_source": recall_outcome.evidence_source,
            "expected_recalled_memory_match": recall_outcome.passed,
        })
    (artifact_dir / "trace.json").write_text(
        json.dumps(trace_dict, ensure_ascii=False, indent=2), encoding="utf-8")
    outcome = evaluate_hard_assertions(trace, task.hard_assertions, timed_out=timed_out)
    base.hard_assertions = outcome.results
    base.hard_assertion_failures = list(outcome.failures)
    if task.memory_assertions is not None and not timed_out and not base.metrics.get("agent_error"):
        if memory_state_error or memory_state is None:
            base.status = "judge_error"
            base.metrics["judge_error"] = memory_state_error or "Memory state unavailable"
            _set_outcome_metrics(base, success=None, eligible=False, failure_class="evaluation", stage="evaluation")
            base.finished_at = now_iso()
            return base
        try:
            assert_memory_results(memory_state, task.memory_assertions)
            base.hard_assertions.memory_state = True
        except SetupAssertionError as e:
            base.hard_assertions.memory_state = False
            outcome.all_passed = False
            base.hard_assertion_failures.append(HardAssertionFailure(
                id="memory_state", label="Memory State",
                requirement=json.dumps(task.memory_assertions, ensure_ascii=False), actual=str(e),
            ))
    if recall_outcome is not None:
        base.hard_assertions.expected_recalled_memory = recall_outcome.passed
    execution_error = bool(base.metrics.get("agent_error"))
    if execution_error or not outcome.all_passed:
        base.status = "timeout" if timed_out else "failed"
        execution_started = (
            trace.total_tool_calls > 0
            or bool(base.metrics.get("llm_calls"))
            or bool(trace.final_response)
        )
        if (execution_error or timed_out) and not execution_started:
            base.metrics["quality_score"] = None
            _set_outcome_metrics(
                base,
                success=None,
                eligible=False,
                failure_class="unknown",
                stage="unknown",
            )
        else:
            base.metrics["quality_score"] = 0.0
            _set_outcome_metrics(
                base,
                success=False,
                eligible=True,
                failure_class="unknown" if execution_error else "agent",
                stage=_failure_stage(base),
            )
        base.finished_at = now_iso()
        return base
    if task.expected_answer is not None:
        answer_outcome = evaluate_expected_answer(
            trace.final_response, task.expected_answer, task.answer_format or "option_letter"
        )
        base.metrics.update({
            "expected_answer": answer_outcome.expected_answer,
            "predicted_answer": answer_outcome.predicted_answer,
            "expected_answer_match": answer_outcome.passed,
        })
        base.hard_assertions.expected_answer = answer_outcome.passed
        if not answer_outcome.passed:
            base.hard_assertion_failures.append(
                HardAssertionFailure(
                    id="expected_answer",
                    label="Expected Answer",
                    requirement=f"Final answer must be {answer_outcome.expected_answer or 'unparseable expected answer'}.",
                    actual=f"Predicted answer was {answer_outcome.predicted_answer or 'none'}.",
                )
            )
            base.status = "failed"
            base.metrics["quality_score"] = 0.0
            _set_outcome_metrics(base, success=False, eligible=True, failure_class="agent", stage="unknown")
            base.finished_at = now_iso()
            return base
    if recall_outcome is not None:
        if recall_outcome.passed is None:
            base.status = "judge_error"
            base.metrics["judge_error"] = (
                "Memory recall evidence is unavailable: episode could not be used "
                "and inline recall_memory results were incomplete or unparsable."
            )
            base.metrics["quality_score"] = None
            _set_outcome_metrics(base, success=None, eligible=False, failure_class="evaluation", stage="evaluation")
            base.finished_at = now_iso()
            return base
        if recall_outcome.passed is False:
            missing_memory_ids = [
                memory_id
                for memory_id in recall_outcome.expected_memory_ids
                if memory_id not in recall_outcome.recalled_memory_ids
            ]
            if recall_outcome.recall_memory_called:
                actual = (
                    f"Missing: {_format_csv(missing_memory_ids)}. "
                    f"Recalled: {_format_csv(recall_outcome.recalled_memory_ids)}."
                )
            else:
                actual = (
                    "No recall_memory call was found. "
                    f"Missing: {_format_csv(missing_memory_ids)}. Recalled: none."
                )
            base.hard_assertion_failures.append(
                HardAssertionFailure(
                    id="expected_recalled_memory",
                    label="Expected Recalled Memory",
                    requirement=f"Must recall memory id(s): {_format_csv(recall_outcome.expected_memory_ids)}.",
                    actual=actual,
                )
            )
            base.status = "failed"
            base.metrics["quality_score"] = 0.0
            _set_outcome_metrics(base, success=False, eligible=True, failure_class="agent", stage="unknown")
            base.finished_at = now_iso()
            return base
    if judge_cfg is None:
        base.status = "passed"
        base.metrics["quality_score"] = 1.0
        _set_outcome_metrics(base, success=True, eligible=True)
        base.finished_at = now_iso()
        return base
    try:
        verdict = judge_task(
            description=task.description_for_judge,
            rubric=task.rubric,
            pre_screenshot=pre_screenshot if pre_screenshot and pre_screenshot.exists() else None,
            post_screenshot=last_shot_path,
            trace=trace_dict,
            final_response=trace.final_response,
            cfg=judge_cfg,
            cache_dir=judge_cache_dir,
        )
    except Exception as e:
        base.status = "judge_error"
        base.metrics["judge_error"] = str(e)
        base.metrics["quality_score"] = None
        _set_outcome_metrics(base, success=None, eligible=False, failure_class="evaluation", stage="evaluation")
        base.finished_at = now_iso()
        return base
    base.rubric = verdict.verdicts
    base.rubric_pass_count = sum(1 for v in verdict.verdicts if v.verdict == "yes")
    base.metrics["judge_image_count"] = verdict.image_count
    base.metrics["judge_image_labels"] = verdict.image_labels
    (artifact_dir / "judge.json").write_text(json.dumps({
        "verdicts": [dc.asdict(v) for v in verdict.verdicts],
        "overall_notes": verdict.overall_notes,
        "cache_key": verdict.cache_key,
        "image_count": verdict.image_count,
        "image_labels": verdict.image_labels,
    }, ensure_ascii=False, indent=2), encoding="utf-8")
    base.status = "passed" if base.rubric_pass_count == base.rubric_total else "failed"
    base.metrics["quality_score"] = (
        base.rubric_pass_count / base.rubric_total if base.rubric_total else (1.0 if base.status == "passed" else 0.0)
    )
    if base.status == "passed":
        _set_outcome_metrics(base, success=True, eligible=True)
    else:
        _set_outcome_metrics(base, success=False, eligible=True, failure_class="agent", stage="unknown")
    base.finished_at = now_iso()
    return base


def run_one_task(
    client: AgentClient,
    suite: Suite,
    task: TaskSpec,
    attempt: int,
    artifact_dir: Path,
    judge_cfg: JudgeConfig | None,
    judge_cache_dir: Path | None,
    run_id: str,
    environment_url: str | None = None,
    benchmark_task_id: str | None = None,
    active_skills: list[str] | None = None,
) -> TaskResult:
    artifact_dir.mkdir(parents=True, exist_ok=True)
    started = now_iso()
    started_mono = time.monotonic()
    base = TaskResult(
        suite=suite.name, run_id=run_id, task_id=task.id, category=task.category,
        attempt=attempt, status="failed",
        rubric=[], rubric_pass_count=0, rubric_total=len(task.rubric),
        artifact_dir=str(artifact_dir), started_at=started,
        description_for_judge=task.description_for_judge,
        rubric_spec=[dc.asdict(r) for r in task.rubric],
        metrics=_attempt_metric_defaults(),
    )
    active_skills = _normalise_active_skills(active_skills)
    setup_result: dict[str, Any] | None = None
    try:
        setup_started_mono = time.monotonic()
        setup_result = prepare_task_isolation(
            client,
            suite,
            task,
            environment_url=environment_url,
            benchmark_task_id=benchmark_task_id,
        )
        base.metrics["setup_ms"] = int((time.monotonic() - setup_started_mono) * 1000)
    except SetupAssertionError as e:
        base.status = "failed"
        base.metrics = {
            **_attempt_metric_defaults(),
            "error": f"setup assertion: {e}",
            "success": False,
            "agent_eligible": False,
            "failure_class": "environment",
            "first_failure_stage": "setup",
            "setup_ms": int((time.monotonic() - started_mono) * 1000),
        }
        base.finished_at = now_iso()
        return base
    except (ResetError, AgentTimeoutError, AgentRequestError) as e:
        failed_consolidation = getattr(e, "consolidation", None)
        if isinstance(failed_consolidation, dict):
            (artifact_dir / "consolidation.json").write_text(
                json.dumps(failed_consolidation, ensure_ascii=False, indent=2),
                encoding="utf-8",
            )
        base.status = "failed" if isinstance(failed_consolidation, dict) else "skipped"
        base.metrics = {
            **_attempt_metric_defaults(),
            "error": f"setup: {e}",
            "success": False if isinstance(failed_consolidation, dict) else None,
            "agent_eligible": False,
            "failure_class": "environment" if isinstance(failed_consolidation, dict) else "skipped",
            "first_failure_stage": "setup",
            "setup_ms": int((time.monotonic() - started_mono) * 1000),
        }
        if isinstance(failed_consolidation, dict):
            base.metrics["consolidation_goal_result"] = (
                failed_consolidation.get("assessment", {}).get("goal_result")
                if isinstance(failed_consolidation.get("assessment"), dict)
                else None
            )
            base.metrics["consolidation_memory_count"] = len(
                failed_consolidation.get("memory_ids", [])
            )
        base.finished_at = now_iso()
        return base
    if setup_result is not None:
        if setup_result.get("consolidation") is not None:
            (artifact_dir / "consolidation.json").write_text(
                json.dumps(setup_result["consolidation"], ensure_ascii=False, indent=2),
                encoding="utf-8",
            )
            consolidation = setup_result["consolidation"]
            base.metrics["consolidation_goal_result"] = (
                consolidation.get("assessment", {}).get("goal_result")
                if isinstance(consolidation.get("assessment"), dict)
                else None
            )
            base.metrics["consolidation_memory_count"] = len(
                consolidation.get("memory_ids", [])
            )
        else:
            (artifact_dir / "setup.json").write_text(
                json.dumps(setup_result, ensure_ascii=False, indent=2),
                encoding="utf-8",
            )
    effective_task = task
    if task.expected_recall_from_consolidation:
        consolidation = setup_result.get("consolidation") if isinstance(setup_result, dict) else None
        memory_ids = consolidation.get("memory_ids") if isinstance(consolidation, dict) else None
        if not isinstance(memory_ids, list) or not memory_ids:
            base.status = "failed"
            base.metrics["error"] = "expected_recall_from_consolidation requires non-empty consolidation memory_ids"
            _set_outcome_metrics(base, success=False, eligible=False, failure_class="environment", stage="setup")
            base.finished_at = now_iso()
            return base
        effective_task = dc.replace(task, expected_recalled_memory_ids=[str(item) for item in memory_ids])
    pre_path = artifact_dir / "pre.jpg"
    attachments = None
    input_screenshot_path = (
        suite.source_path.parent / task.input_screenshot
        if task.input_screenshot
        else None
    )
    if input_screenshot_path is not None and not input_screenshot_path.exists():
        base.status = "skipped"
        base.metrics = {
            **_attempt_metric_defaults(),
            "error": f"input_screenshot not found: {input_screenshot_path}",
            "success": None,
            "agent_eligible": False,
            "failure_class": "skipped",
            "first_failure_stage": "setup",
        }
        base.finished_at = now_iso()
        return base
    mock_environment = effective_mock_environment(suite, task)
    uses_single_frame_mock = bool(
        mock_environment is not None and mock_environment.single_frame
    )
    if uses_single_frame_mock:
        if environment_url:
            try:
                capture_started = time.monotonic()
                take_environment_screenshot(
                    environment_url,
                    pre_path,
                    benchmark_task_id=benchmark_task_id,
                )
                base.metrics["screenshot_capture_ms"] = (base.metrics.get("screenshot_capture_ms") or 0) + int((time.monotonic() - capture_started) * 1000)
                base.metrics["screenshot_capture_source"] = "environment_bridge"
            except Exception as e:
                base.metrics["pre_screenshot_error"] = str(e)[:300]
        else:
            base.metrics["pre_screenshot_error"] = (
                "environment_url is required for mock screenshot capture"
            )
        if not pre_path.exists() and input_screenshot_path is not None:
            shutil.copy(input_screenshot_path, pre_path)
    elif input_screenshot_path is not None:
        shutil.copy(input_screenshot_path, pre_path)
        img_b64 = base64.b64encode(input_screenshot_path.read_bytes()).decode("ascii")
        with Image.open(input_screenshot_path) as image:
            width, height = image.size
        attachments = [
            {
                "kind": "image",
                "mime_type": "image/jpeg",
                "width": width,
                "height": height,
                "data": img_b64,
            }
        ]
    else:
        if environment_url:
            try:
                capture_started = time.monotonic()
                take_environment_screenshot(
                    environment_url,
                    pre_path,
                    benchmark_task_id=benchmark_task_id,
                )
                base.metrics["screenshot_capture_ms"] = (base.metrics.get("screenshot_capture_ms") or 0) + int((time.monotonic() - capture_started) * 1000)
                base.metrics["screenshot_capture_source"] = "environment_bridge"
            except Exception as e:
                base.metrics["pre_screenshot_error"] = str(e)[:300]
    timed_out = False
    episode = None
    episode_id = None
    base.metrics.setdefault("recovery_attempted", False)
    base.metrics.setdefault("recovery_succeeded", None)
    task_started_mono = time.monotonic()
    try:
        prompt = effective_task.prompt
        if suite.prompt_prefix:
            prompt = f"{suite.prompt_prefix.rstrip()}\n\n{effective_task.prompt}"
        chat_kwargs: dict[str, Any] = {
            "timeout_sec": task.hard_assertions.must_complete_within_sec,
            "attachments": attachments,
        }
        if active_skills:
            chat_kwargs["skills"] = active_skills
        chat = client.chat(prompt, **chat_kwargs)
        base.metrics["task_wall_ms"] = int((time.monotonic() - task_started_mono) * 1000)
        history = chat.history
        # The client resolves request-scoped evidence. Context history may refer
        # to an earlier task, so never replace a missing request ID with it.
        episode_id = chat.episode_id
    except AgentTimeoutError as exc:
        base.metrics["task_wall_ms"] = int((time.monotonic() - task_started_mono) * 1000)
        timed_out = True
        history = client_history_or_empty(client)
        episode_id = _request_episode_id(client, exc)
        base.metrics["recovery_attempted"] = True
        recovery_succeeded = recover_agent_after_timeout(client)
        base.metrics["recovery_succeeded"] = recovery_succeeded
        if not recovery_succeeded:
            base.metrics["recovery_failed"] = True
    except Exception as e:
        base.metrics["task_wall_ms"] = int((time.monotonic() - task_started_mono) * 1000)
        history = client_history_or_empty(client)
        episode_id = _request_episode_id(client, e)
        base.metrics["agent_error"] = str(e)[:300]
    if episode_id is not None:
        base.metrics["episode_id"] = episode_id
        try:
            episode = client.get_episode(episode_id)
        except Exception as e:
            base.metrics["episode_error"] = str(e)[:300]
    # Capture the final device state directly from the environment screen API.
    # The agent history no longer embeds base64 image data, so the post-screenshot
    # must be grabbed live rather than extracted from history.
    post_path = artifact_dir / "post.jpg"
    if uses_single_frame_mock:
        post_path = None
    elif environment_url:
        try:
            capture_started = time.monotonic()
            take_environment_screenshot(
                environment_url,
                post_path,
                benchmark_task_id=benchmark_task_id,
            )
            base.metrics["screenshot_capture_ms"] = (base.metrics.get("screenshot_capture_ms") or 0) + int((time.monotonic() - capture_started) * 1000)
            base.metrics["screenshot_capture_source"] = "environment_bridge"
        except Exception as e:
            base.metrics["post_screenshot_error"] = str(e)[:300]
            post_path = None
    else:
        post_path = None
    environment_state: dict[str, Any] | None = None
    environment_state_error = ""
    if effective_task.environment_assertions:
        if not environment_url:
            environment_state_error = "environment_url is required for environment_assertions"
        else:
            try:
                environment_state = read_environment_state(
                    environment_url,
                    benchmark_task_id=benchmark_task_id,
                )
                (artifact_dir / "environment_state.json").write_text(
                    json.dumps(environment_state, ensure_ascii=False, indent=2),
                    encoding="utf-8",
                )
            except Exception as exc:
                environment_state_error = str(exc)[:300]

    memory_state = None
    memory_state_error = ""
    if effective_task.memory_assertions is not None and not timed_out and not base.metrics.get("agent_error"):
        try:
            memory_state = read_memory_results(client, effective_task.memory_assertions)
        except (ResetError, AgentTimeoutError, AgentRequestError) as e:
            memory_state_error = str(e)

    result = evaluate_task_history(
        suite=suite,
        task=effective_task,
        history=history,
        attempt=attempt,
        artifact_dir=artifact_dir,
        judge_cfg=judge_cfg,
        judge_cache_dir=judge_cache_dir,
        run_id=run_id,
        timed_out=timed_out,
        metrics=base.metrics,
        pre_screenshot=pre_path if pre_path.exists() else None,
        post_screenshot=post_path if post_path and post_path.exists() else None,
        started_at=started,
        started_mono=None,
        active_skills=active_skills,
        episode=episode,
        memory_state=memory_state,
        memory_state_error=memory_state_error,
    )
    return _apply_environment_assertions(
        result,
        effective_task,
        environment_state,
        environment_state_error,
    )


def _apply_environment_assertions(
    result: TaskResult,
    task: TaskSpec,
    state: dict[str, Any] | None,
    error: str,
) -> TaskResult:
    if not task.environment_assertions:
        return result
    if result.hard_assertions is None:
        result.hard_assertions = HardAssertionResults()
    if error or state is None:
        result.hard_assertions.environment_state = None
        result.metrics["environment_state_error"] = error or "environment state unavailable"
        result.status = "judge_error"
        result.metrics["quality_score"] = None
        _set_outcome_metrics(
            result,
            success=None,
            eligible=False,
            failure_class="evaluation",
            stage="evaluation",
        )
        result.finished_at = now_iso()
        return result

    checks = evaluate_environment_state_assertions(state, task.environment_assertions)
    result.metrics["environment_state_assertions"] = [dc.asdict(check) for check in checks]
    all_passed = all(check.passed for check in checks)
    result.hard_assertions.environment_state = all_passed
    if not all_passed:
        for check in checks:
            if check.passed:
                continue
            result.hard_assertion_failures.append(
                HardAssertionFailure(
                    id=f"environment_state:{check.path}",
                    label="Environment State",
                    requirement=f"{check.path} must equal {check.expected!r}.",
                    actual=f"Observed {check.actual!r}.",
                )
            )
        if result.status == "passed":
            result.status = "failed"
            result.metrics["quality_score"] = 0.0
            _set_outcome_metrics(
                result,
                success=False,
                eligible=True,
                failure_class="agent",
                stage=_failure_stage(result),
            )
    result.finished_at = now_iso()
    return result


def client_history_or_empty(client: AgentClient) -> list[dict]:
    try:
        return client.get_history()
    except Exception:
        pass
    return []


def _format_csv(items: list[str]) -> str:
    return ", ".join(items) if items else "none"


def _request_episode_id(client: AgentClient, error: Exception) -> str | None:
    episode_id = getattr(error, "episode_id", None)
    if episode_id:
        return episode_id
    request_id = getattr(error, "request_id", None)
    if request_id:
        try:
            return client.chat_episode_id(request_id)
        except Exception:
            # Correlation must not replace the original execution failure.
            pass
    return None


def _normalise_active_skills(skills: list[str] | None) -> list[str]:
    seen = set()
    out = []
    for item in skills or []:
        name = str(item).strip()
        if not name or name in seen:
            continue
        seen.add(name)
        out.append(name)
    return out

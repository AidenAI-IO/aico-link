"""Execute one text-driven foreground/backend collaboration scenario."""
from __future__ import annotations

import json
import time
from pathlib import Path
from typing import Any

from runner.agent_client import AgentClient
from runner.collaboration import CollaborationEventCollector, CollaborationEvidenceError
from runner.collaboration_claims import adjudicate_claims
from runner.judge import JudgeConfig
from runner.matching import dict_contains
from runner.models import TaskResult
from runner.recovery import prepare_task_isolation
from runner.report import now_iso
from runner.suite import Suite, TaskSpec


def _pointer(data: Any, pointer: str) -> Any:
    for token in pointer.split("/")[1:]:
        token = token.replace("~1", "/").replace("~0", "~")
        data = data[int(token)] if isinstance(data, list) else data[token]
    return data


def _match(event: dict[str, Any], spec: dict[str, Any], tasks: dict[str, str],
           turns: dict[str, str], revisions: dict[str, int]) -> bool:
    meta = event.get("collaboration") or {}
    payload = event.get("payload") or {}
    for key, value in spec.items():
        if key == "task": value = tasks[value]
        elif key == "turn": value = turns[value]
        elif key in {"goal_revision", "applied_revision"} and isinstance(value, str):
            value = revisions[value.removeprefix("$revision.")]
        actual = {"kind": event.get("kind"), "actor": meta.get("actor"),
                  "phase": payload.get("phase"), "status": payload.get("status"),
                  "task": event.get("task_id"), "turn": event.get("turn_id"),
                  "goal_revision": meta.get("goal_revision"),
                  "applied_revision": meta.get("applied_revision"), "gate": payload.get("gate")}[key]
        if actual != value:
            return False
    return True


def _latest_task_state(events: list[dict[str, Any]], task_id: str) -> dict[str, Any] | None:
    return next((event for event in reversed(events) if event.get("kind") == "task.updated"
                 and event.get("task_id") == task_id), None)


def _check(spec: dict[str, Any], daemon_events: list[dict[str, Any]], fixture: dict[str, Any],
           tasks: dict[str, str], turns: dict[str, str]) -> tuple[bool, list[str]]:
    kind = spec["kind"]
    events = fixture.get("events", [])
    input_contains = spec.get("input_contains", {})
    effects = [event for event in events if event["kind"] == "fixture.effect"
               and event["payload"].get("operation") == spec.get("operation")
               and dict_contains(event["payload"].get("input", {}), input_contains)]
    if kind in {"no_effect", "effect_count"}:
        count = spec.get("count", 0)
        return len(effects) == count, [event["event_id"] for event in effects]
    if kind == "fixture_call_count":
        calls = [event for event in events if event["kind"] == "fixture.call"
                 and event["payload"].get("phase") == "accepted"
                 and event["payload"].get("operation") == spec["operation"]
                 and dict_contains(event["payload"].get("input", {}), input_contains)]
        return len(calls) == spec["count"], [event["event_id"] for event in calls]
    if kind == "fixture_failure":
        failed = [event for event in events if event["kind"] == "fixture.call"
                  and event["payload"].get("phase") == "failed"
                  and event["payload"].get("operation") == spec["operation"]
                  and dict_contains(event["payload"].get("input", {}), input_contains)]
        return bool(failed), [event["event_id"] for event in failed]
    if kind == "tasks_created":
        created = [event for event in daemon_events if event.get("kind") == "task.updated"
                   and (event.get("payload") or {}).get("phase") == "created"
                   and event.get("turn_id") == turns[spec["turn"]]]
        return len(created) == spec["count"], [event["event_id"] for event in created]
    if kind == "task_state":
        task_id = tasks[spec["task"]]
        state = _latest_task_state(daemon_events, task_id)
        starts = [event for event in daemon_events if event.get("kind") == "run.started"
                  and event.get("task_id") == task_id]
        ok = state is not None and state["payload"]["status"] == spec["status"]
        if "started" in spec: ok = ok and bool(starts) == spec["started"]
        return ok, [state["event_id"]] if state else []
    if kind == "state_equals":
        value = spec["value"]
        if isinstance(value, dict): value = _pointer(fixture["initial_state"], value["fixture_value"])
        try: return _pointer(fixture["state"], spec["path"]) == value, []
        except (KeyError, IndexError): return False, []
    raise ValueError(f"unsupported scenario assertion: {kind}")


def _response_text(event: dict[str, Any]) -> str:
    payload = event.get("payload") or {}
    return str(payload.get("text") or payload.get("transcript") or "")


def _milestones(events: list[dict[str, Any]], task_id: str, revision: int | None) -> dict[str, Any]:
    terminal = next((e for e in reversed(events) if e.get("kind") == "task.updated"
                     and e.get("task_id") == task_id and e.get("payload", {}).get("phase") == "terminal"
                     and (revision is None or e.get("collaboration", {}).get("goal_revision") == revision)), None)
    found = {"backend_terminal": terminal, "result_injected": None, "foreground_feedback": None}
    if terminal is None:
        return found
    for injection in events:
        if injection.get("kind") != "task.result_injected" or injection["sequence"] <= terminal["sequence"]:
            continue
        ids = {u["update_id"] for u in injection.get("payload", {}).get("updates", [])
               if u.get("task_id") == task_id and u.get("status") == terminal["payload"]["status"]
               and u.get("goal_revision") == terminal["collaboration"]["goal_revision"]}
        if not ids:
            continue
        found["result_injected"] = injection
        feedback = next((e for e in events if e.get("kind") == "response.feedback_terminal"
                         and e["sequence"] > injection["sequence"]
                         and ids.intersection(e.get("collaboration", {}).get("update_ids", []))
                         and e.get("payload", {}).get("status") == "completed"
                         and _response_text(e).strip()), None)
        if feedback:
            found["foreground_feedback"] = feedback
            break
    return found


def evaluate_scenario(evidence: dict[str, Any], task: TaskSpec, judge_cfg: JudgeConfig | None,
                      artifact_dir: Path, *, evaluation_name: str = "collaboration-evaluation.json") -> dict[str, Any]:
    """Offline evaluation: never issue tools or change the captured execution."""
    from runner.judge import judge_collaboration_claims, judge_task
    from runner.suite import RubricItem
    scenario = task.scenario
    assert scenario is not None
    checks = list(evidence["assertions"])
    claims: list[dict[str, Any]] = []
    rubric_verdicts = []
    events = evidence["events"]
    # Recompute delivery gates from raw evidence during both live and offline
    # scoring. A semantic judge cannot replace missing causal milestones.
    for target in scenario.completion.get("tasks", []):
        alias = target["task"]
        task_id = evidence.get("task_aliases", {}).get(alias)
        revision = target.get("goal_revision")
        if isinstance(revision, str):
            revision = evidence.get("revisions", {}).get(revision.removeprefix("$revision."))
        milestones = _milestones(events, task_id, revision) if task_id else {}
        terminal = milestones.get("backend_terminal")
        if terminal is None:
            verdict = "unscored" if not evidence["complete"] else "fail"
        elif terminal.get("payload", {}).get("status") != target["status"]:
            verdict = "fail"
        else:
            verdict = "pass" if milestones.get("result_injected") and milestones.get("foreground_feedback") else (
                "unscored" if not evidence["complete"] else "fail")
        checks.append({"id": f"delivery.{alias}", "verdict": verdict,
                       "evidence_refs": [e["event_id"] for e in milestones.values() if e]})
    if not evidence["complete"]:
        checks.append({"id": "evidence_complete", "verdict": "unscored", "reason": "capture unavailable or incomplete"})
    else:
        if "supported_completion_claims" in scenario.policies:
            try:
                if judge_cfg is None:
                    raise ValueError("claim judge disabled")
                extracted = judge_collaboration_claims(events, judge_cfg)
                claims = adjudicate_claims(extracted, events, evidence["fixture"], True)
                verdict = ("fail" if any(c["verdict"] == "unsupported" for c in claims) else
                           "unscored" if any(c["verdict"] == "indeterminate" for c in claims) else "pass")
                checks.append({"id": "supported_completion_claims", "verdict": verdict,
                               "evidence_refs": [c["event_id"] for c in claims if c["verdict"] != "supported"]})
            except Exception as exc:
                checks.append({"id": "supported_completion_claims", "verdict": "unscored", "reason": str(exc)})
        rubric = list(task.rubric)
        for target in scenario.completion.get("tasks", []):
            if target.get("feedback") == "required":
                alias = target["task"]
                rubric.append(RubricItem(id=f"feedback.{alias}", check=
                    f"The foreground response linked by milestones[{alias}] accurately communicates the actual outcome of "
                    "that task and current goal, including failures. An unrelated reply or generic acknowledgement is not delivery. "
                    "Use task_aliases, task events, linked response and fixture evidence; do not credit backend text as a foreground reply."))
        if rubric:
            try:
                if judge_cfg is None:
                    raise ValueError("feedback/rubric judge disabled")
                linked_feedback = {
                    target["task"]: _milestones(
                        events, evidence.get("task_aliases", {}).get(target["task"]),
                        evidence.get("revisions", {}).get(target.get("goal_revision", "").removeprefix("$revision."))
                        if isinstance(target.get("goal_revision"), str) else target.get("goal_revision"),
                    ).get("foreground_feedback")
                    for target in scenario.completion.get("tasks", []) if target.get("feedback") == "required"
                }
                scoped_evidence = {**evidence, "required_feedback": {
                    alias: event for alias, event in linked_feedback.items() if event is not None
                }}
                replies = [_response_text(event) for event in linked_feedback.values() if event is not None]
                verdict = judge_task(description=task.description_for_judge, rubric=rubric,
                    pre_screenshot=None, post_screenshot=None, trace=scoped_evidence,
                    final_response="\n\n".join(replies), cfg=judge_cfg, task_prompt=task.prompt)
                rubric_verdicts = [dict(id=v.id, verdict=v.verdict, reason=v.reason) for v in verdict.verdicts]
                checks.extend({"id": v.id, "verdict": "pass" if v.verdict == "yes" else "fail", "reason": v.reason}
                              for v in verdict.verdicts)
            except Exception as exc:
                checks.append({"id": "feedback_rubric", "verdict": "unscored", "reason": str(exc)})
    failed = any(c["verdict"] == "fail" for c in checks)
    unresolved = any(c["verdict"] == "unscored" for c in checks)
    outcome = "fail" if failed else "unscored" if unresolved else "pass"
    evaluation = {"version": 1, "outcome": outcome, "checks": checks, "claims": claims,
                  "rubric": rubric_verdicts, "judge_model": judge_cfg.model if judge_cfg else None}
    (artifact_dir / evaluation_name).write_text(json.dumps(evaluation, ensure_ascii=False, indent=2), encoding="utf-8")
    return evaluation


def execute_scenario(client: AgentClient, suite: Suite, task: TaskSpec, attempt: int,
                     artifact_dir: Path, run_id: str, environment_url: str, judge_cfg: JudgeConfig | None,
                     benchmark_task_id: str | None = None, fixture_token: str = "") -> TaskResult:
    from runner.models import HardAssertionFailure, RubricVerdict

    scenario = task.scenario
    assert scenario is not None
    artifact_dir.mkdir(parents=True, exist_ok=True)
    result = TaskResult(suite=suite.name, run_id=run_id, task_id=task.id, category=task.category,
        attempt=attempt, status="judge_error", rubric=[], artifact_dir=str(artifact_dir), started_at=now_iso(),
        task_prompt=f"{suite.prompt_prefix.rstrip()}\n\n{task.prompt}".strip(), description_for_judge=task.description_for_judge,
        rubric_spec=[{"id": r.id, "check": r.check} for r in task.rubric],
        metrics={"execution_mode": "realtime-text", "success": None, "agent_eligible": False,
                 "quality_score": None, "failure_class": "evaluation"})
    collector = CollaborationEventCollector(client, artifact_dir / "collaboration-events.jsonl")
    turns: dict[str, str] = {}
    tasks: dict[str, str] = {}
    revisions: dict[str, int] = {}
    checks: list[dict[str, Any]] = []
    steps: list[dict[str, Any]] = []
    fixture: dict[str, Any] = {}
    milestones = {}
    stage = "setup"
    started = time.monotonic()
    deadline = started + scenario.timeout_sec
    collection_started = False
    timeout_failure = False
    execution_finished = False
    error = ""

    def remaining(limit: float = 60) -> float:
        value = min(limit, deadline - time.monotonic())
        if value <= 0:
            raise TimeoutError("scenario deadline exceeded")
        return value

    def wait(match: dict[str, Any], limit: float = 60) -> dict[str, Any]:
        def predicate(e):
            if e.get("kind") == "turn.settled" and e.get("payload", {}).get("status") == "failed":
                raise CollaborationEvidenceError("foreground request failed: " + e["payload"].get("error", "unknown"))
            if e.get("kind") == "stream.error":
                raise CollaborationEvidenceError("realtime provider reported a stream error")
            return _match(e, match, tasks, turns, revisions)
        return collector.wait_for(predicate, remaining(limit))

    def append_check(check, check_id):
        snapshot = collector.barrier()
        state = client.fixture_state(environment_url, fixture_token)
        ok, refs = _check(check, collector.snapshot(), state, tasks, turns)
        checks.append({"id": check_id, "verdict": "pass" if ok else "fail", "evidence_refs": refs,
                       "daemon_watermark": snapshot["cursor"], "fixture_sequence": len(state["events"])})

    try:
        mock_environment = task.mock_environment or suite.mock_environment
        permission_gates = {gate["id"] for gate in mock_environment.collaboration_fixture.get("gates", [])
                            if gate.get("at") == "permission"}
        if any(step.get("type") == "release" and step.get("gate") in permission_gates
               for step in scenario.steps):
            raise CollaborationEvidenceError("permission gates cannot be released by scenario steps")
        if not environment_url or not fixture_token:
            raise CollaborationEvidenceError("realtime scenario requires a controlled fixture")
        prepare_task_isolation(client, suite, task, environment_url=environment_url, benchmark_task_id=benchmark_task_id)
        capability = client.collaboration_status()
        if capability.get("execution_mode") != "realtime-text" or capability.get("use_backend_agent") is not True or not capability.get("handler_ready"):
            raise CollaborationEvidenceError("realtime foreground/backend handler is not ready")
        result.metrics["collaboration_models"] = {"foreground": [capability.get("foreground_provider"), capability.get("foreground_model")],
                                                 "backend": [capability.get("backend_provider"), capability.get("backend_model")]}
        collector.start()
        collection_started = True
        collector.barrier()
        deadline = time.monotonic() + scenario.timeout_sec
        stage = "initial_turn"
        submitted = client.submit_chat(result.task_prompt, timeout_sec=10)
        if not isinstance(submitted, str):
            raise CollaborationEvidenceError("realtime chat did not return a request ID")
        turns["initial"] = submitted
        steps.append({"id": "initial", "request_id": submitted, "text": result.task_prompt})
        for step in scenario.steps:
            stage = step["id"]
            remaining()
            if step["type"] == "send":
                submitted = client.submit_chat(step["text"], timeout_sec=remaining(10))
                if not isinstance(submitted, str):
                    raise CollaborationEvidenceError("realtime chat is not asynchronous")
                turns[step["id"]] = submitted
                steps.append({"id": stage, "request_id": submitted, "text": step["text"]})
            elif step["type"] == "release":
                checkpoint = collector.barrier()
                client.fixture_release(environment_url, fixture_token, step["gate"], checkpoint["cursor"])
                steps.append({"id": stage, "gate": step["gate"], "daemon_watermark": checkpoint["cursor"]})
            elif step["type"] == "await":
                match = step["match"]
                if match["kind"].startswith("fixture."):
                    wait_end = time.monotonic() + remaining(step.get("timeout_sec", 60))
                    while True:
                        collector.snapshot()  # propagate collector failures, including during fixture waits
                        fixture = client.fixture_state(environment_url, fixture_token)
                        found = next((e for e in fixture["events"] if _match(
                            {**e, "task_id": e["payload"].get("task_id")}, match, tasks, turns, revisions)), None)
                        if found is not None:
                            break
                        if time.monotonic() >= wait_end:
                            raise TimeoutError("fixture event wait timed out")
                        time.sleep(.05)
                else:
                    found = wait(match, step.get("timeout_sec", 60))
                if step.get("bind_task"):
                    task_id = found["task_id"]
                    if task_id in tasks.values():
                        checks.append({"id": stage + ".distinct_task", "verdict": "fail", "evidence_refs": [found["event_id"]]})
                    tasks[step["bind_task"]] = task_id
                if step.get("bind_revision"):
                    revisions[step["bind_revision"]] = found["collaboration"]["goal_revision"]
                steps.append({"id": stage, "event_id": found["event_id"]})
            elif step["type"] == "assert":
                append_check(step["check"], stage)
        stage = "completion"
        # Every explicitly submitted turn must settle, not just the initial response.
        for alias in turns:
            wait({"kind": "turn.settled", "turn": alias, "status": "completed"}, scenario.timeout_sec)
        for target in scenario.completion.get("tasks", []):
            alias, task_id = target["task"], tasks[target["task"]]
            revision = target.get("goal_revision")
            if isinstance(revision, str):
                revision = revisions[revision.removeprefix("$revision.")]
            terminal = wait({"kind": "task.updated", "task": alias, "phase": "terminal"}, scenario.timeout_sec)
            status_ok = terminal["payload"].get("status") == target["status"]
            revision_ok = revision is None or terminal["collaboration"].get("goal_revision") == revision
            checks.append({"id": f"{alias}.terminal", "verdict": "pass" if status_ok and revision_ok else "fail",
                           "evidence_refs": [terminal["event_id"]]})
            if target.get("feedback") == "required":
                collector.wait_for(lambda _: bool(_milestones(collector.events, task_id, revision)["foreground_feedback"]), remaining(30))
            milestones[alias] = {k: v["event_id"] if v else None for k, v in _milestones(collector.snapshot(), task_id, revision).items()}
        if any(target["status"] == "cancelled" for target in scenario.completion.get("tasks", [])):
            stage = "post_cancellation_observation"
            observation_started = time.monotonic()
            if remaining(5) < 5:
                raise TimeoutError("insufficient deadline for cancellation observation")
            while time.monotonic() - observation_started < 5:
                collector.snapshot()
                time.sleep(.05)
            steps.append({"id": stage, "duration_sec": time.monotonic() - observation_started})
        if scenario.completion.get("delegation") == "none":
            append_check({"kind": "tasks_created", "turn": "initial", "count": 0}, "no_delegation")
        for check in scenario.assertions:
            append_check(check, check["id"])
        # Bindings declared as singular must remain singular through scenario end.
        for step in scenario.steps:
            if step.get("bind_task"):
                append_check({"kind": "tasks_created", "turn": step["match"]["turn"], "count": 1}, step["id"] + ".unique")
        execution_finished = True
    except Exception as exc:
        error = f"{stage}: {exc}"
        timeout_failure = isinstance(exc, TimeoutError)
    finally:
        # Freeze effects before scoring/cleanup. The worker owner stops the daemon
        # before releasing the environment; no cleanup action counts as task success.
        complete = collection_started
        events = []
        try:
            if collection_started:
                collector.barrier()
                events = collector.snapshot()
            fixture = client.fixture_state(environment_url, fixture_token, freeze=True) if fixture_token else {}
        except Exception as exc:
            complete = False
            error = error or str(exc)
        try:
            collector.close()
        except Exception as exc:
            complete = False
            error = error or str(exc)
        if collection_started and not events:
            events = list(collector.events)
        # Score only the captured interval, not events produced by cleanup.
        provider_error = any(e.get("kind") == "stream.error" or (e.get("kind") == "turn.settled"
            and e.get("payload", {}).get("status") == "failed") for e in events)
        ready = any(e.get("kind") == "session.ready" for e in events)
        fixture_expired = any(e.get("payload", {}).get("phase") == "expired" for e in fixture.get("events", []))
        if not execution_finished:
            checks.append({"id": "scenario_completed", "verdict": "fail" if timeout_failure and complete and ready and not provider_error and not fixture_expired else "unscored",
                           "reason": error})
        evidence = {"schema_version": 1, "scenario_id": task.id, "task_aliases": tasks, "turns": turns,
                    "revisions": revisions, "milestones": milestones, "assertions": checks, "complete": complete,
                    "fixture": fixture, "events": events, "steps": steps, "stage": stage, "error": error}
        (artifact_dir / "collaboration.json").write_text(json.dumps(evidence, ensure_ascii=False, indent=2), encoding="utf-8")
        (artifact_dir / "fixture-events.jsonl").write_text("".join(json.dumps(e, ensure_ascii=False) + "\n" for e in fixture.get("events", [])), encoding="utf-8")
        (artifact_dir / "scenario-events.jsonl").write_text("".join(json.dumps(e, ensure_ascii=False) + "\n" for e in steps), encoding="utf-8")
    evaluation = evaluate_scenario(evidence, task, judge_cfg, artifact_dir)
    failed = evaluation["outcome"] == "fail"
    passed = evaluation["outcome"] == "pass"
    result.status = "passed" if passed else "failed" if failed else "judge_error"
    result.metrics.update(success=passed if passed or failed else None, agent_eligible=passed or failed,
        quality_score=1.0 if passed else 0.0 if failed else None, failure_class="agent" if failed else None if passed else "evaluation",
        first_failure_stage=None if passed else stage, failure_event_ref=None if passed else "collaboration-evaluation.json",
        task_wall_ms=int((time.monotonic()-started)*1000), wall_ms=int((time.monotonic()-started)*1000),
        evidence_complete=complete)
    result.rubric = [RubricVerdict(**v) for v in evaluation["rubric"]]
    result.rubric_total = len(result.rubric) or len(task.rubric)
    result.rubric_pass_count = sum(v.verdict == "yes" for v in result.rubric)
    result.hard_assertion_failures = [HardAssertionFailure(id=c["id"], label=c["id"], requirement="Collaboration contract",
        actual=c.get("reason") or json.dumps(c.get("evidence_refs", []))) for c in evaluation["checks"] if c["verdict"] == "fail"]
    if error:
        result.metrics["error"] = error
    from runner.collaboration_report import write_collaboration_report
    report_summary = write_collaboration_report(evidence, evaluation, artifact_dir)
    result.metrics["collaboration"] = report_summary
    result.finished_at = now_iso()
    return result

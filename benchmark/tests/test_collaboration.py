"""Behavioral evidence tests: no provider credentials or physical devices required."""
import json
import threading
from concurrent.futures import ThreadPoolExecutor

import pytest

from runner.collaboration import CollaborationEventCollector, CollaborationEvidenceError
from runner.collaboration_claims import adjudicate_claims, foreground_outputs, validate_claims
from runner.config import configure_realtime_text
from runner.phone_data_fixture import PhoneDataFixture
from runner.scenario import _milestones, evaluate_scenario
from runner.suite import HardAssertions, ScenarioSpec, TaskSpec


def event(sequence, kind, task="A", revision=1, **fields):
    return {"event_id": f"e{sequence}", "session_epoch": 7, "sequence": sequence,
            "kind": kind, "task_id": task, "turn_id": "user-1", "response_id": "response-1",
            "payload": {}, "collaboration": {"actor": "backend", "clock_id": "clock",
                "observed_elapsed_ns": sequence * 100, "goal_revision": revision, "applied_revision": revision},
            **fields}


def claim(at=5, task="A", revision=1):
    return {"event_id": f"e{at}", "sequence": at, "clock_id": "clock", "observed_elapsed_ns": at * 100,
            "turn_id": "user-1", "task_id": task, "goal_revision": revision, "scope": "current_goal",
            "operation": "bridge_notification", "input_contains": {"title": "Ready"},
            "result_contains": {}, "quote": "Posted.", "response_id": "response-1"}


def effect_fixture(task="A", call="call-1"):
    return {"events": [{"event_id": "f1", "kind": "fixture.call", "payload": {
        "phase": "completed", "task_id": task, "call_id": call,
        "operation": "bridge_notification", "input": {"title": "Ready", "body": "Package"},
        "result": {"ok": True}}}]}


def execution():
    return [event(1, "task.updated", payload={"phase": "created"}), event(2, "run.started"),
            event(3, "tool.call_ready", call_id="call-1"), event(4, "tool.completed", call_id="call-1")]


@pytest.mark.parametrize("at,expected", [(1, "completion_before_execution"), (5, None)])
def test_later_success_cannot_repair_early_claim(at, expected):
    verdict = adjudicate_claims([claim(at)], execution(), effect_fixture(), True)[0]
    assert verdict["category"] == expected
    assert verdict["verdict"] == ("supported" if expected is None else "unsupported")


def test_claim_without_delegation_is_not_repaired_by_later_task():
    verdict = adjudicate_claims([claim(1, task=None)], [event(2, "task.updated", payload={"phase": "created"})], {}, True)[0]
    assert verdict["category"] == "completion_without_delegation"


@pytest.mark.parametrize("case", ["other_task", "missing_call", "incomplete", "unknown_revision", "old_revision", "in_flight"])
def test_uncertain_or_wrong_effect_never_passes(case):
    events, fixture, c, complete = execution(), effect_fixture(), claim(), True
    if case == "other_task": fixture = effect_fixture(task="B")
    if case == "missing_call": fixture = effect_fixture(call="other")
    if case == "incomplete": complete = False
    if case == "unknown_revision": c["goal_revision"] = None
    if case == "old_revision":
        events.append(event(5, "task.updated", revision=2, payload={"phase": "goal_updated"}))
        c = claim(at=6)
    if case == "in_flight": events[-1] = event(6, "tool.completed", call_id="call-1")
    verdict = adjudicate_claims([c], events, fixture, complete)[0]
    assert verdict["verdict"] != "supported"
    if case == "old_revision": assert verdict["category"] == "obsolete_goal_completion"


def test_claim_selectors_are_grounded_in_visible_text():
    def output(text):
        item = event(5, "text.final", payload={"Text": text})
        item["collaboration"]["actor"] = "foreground"
        return foreground_outputs([item])

    def extract(text, **fields):
        parsed = {"outputs": [{"event_id": "e5", "claims": [{"quote": text, "task_id": "A", "goal_revision": 1,
                                                          "scope": "current_goal", **fields}]}]}
        return validate_claims(parsed, output(text), {"A"}, context="Post a notification titled Ready.")

    clipboard = {"events": [{"event_id": "f1", "kind": "fixture.call", "payload": {
        "phase": "completed", "task_id": "A", "call_id": "call-1", "operation": "bridge_clipboard",
        "input": {"action": "read"}, "result": {"ok": True, "text": "398b8f"}}}]}
    # A spoken rendering of the recorded value proves the read result.
    [spoken] = extract("It is 3 9 8 b 8 f.", operation="bridge_clipboard", result_contains={"text": "3 9 8 b 8 f"})
    assert spoken["input_contains"] == {"action": "read"}
    assert adjudicate_claims([spoken], execution(), clipboard, True)[0]["verdict"] == "supported"
    # A placeholder is dropped rather than treated as a wrong body; the title comes from the user turn.
    [placeholder] = extract("Posted it.", operation="bridge_notification",
                            input_contains={"title": "Ready", "body": "[clipboard content]"})
    assert placeholder["input_contains"] == {"title": "Ready"}
    assert adjudicate_claims([placeholder], execution(), effect_fixture(), True)[0]["verdict"] == "supported"
    # Without a concrete operation a claim is never supported, nor declared false.
    [vague] = extract("All done.", operation=None)
    assert adjudicate_claims([vague], execution(), effect_fixture(), True)[0]["category"] == "claim_operation_unspecified"


def test_gate_watermark_proves_premature_effect_without_cross_service_clocks():
    fixture = effect_fixture()
    fixture["events"] += [
        {"event_id": "entered", "kind": "fixture.gate", "payload": {
            "phase": "entered", "task_id": "A", "call_id": "call-1", "gate": "post",
            "operation": "bridge_notification", "input": {"title": "Ready"}}},
        {"event_id": "released", "kind": "fixture.gate", "payload": {
            "phase": "released", "gate": "post", "daemon_watermark": {"session_epoch": 7, "sequence": 5}}},
    ]
    verdict = adjudicate_claims([claim()], execution()[:-1], fixture, True)[0]
    assert verdict["category"] == "completion_before_effect"


def test_claim_extraction_times_claims_by_first_visible_prefix_and_keeps_correction():
    def foreground(sequence, kind, text):
        item = event(sequence, kind, payload={"Text": text})
        item["collaboration"]["actor"] = "foreground"
        return item

    # Streaming deltas and a matching final are one output; a rewriting final is a new one.
    outputs = foreground_outputs([foreground(1, "text.delta", "Done. "), foreground(2, "text.delta", "Posted."),
                                  foreground(3, "text.final", "Done. Posted."),
                                  foreground(4, "text.final", "Not posted yet.")])
    assert [(o["event_id"], o["text"]) for o in outputs] == [("e2", "Done. Posted."), ("e4", "Not posted yet.")]
    parsed = {"outputs": [{"event_id": "e2", "claims": [{"quote": "Posted.", "start": 0,
        "task_id": "A", "goal_revision": 1, "scope": "current_goal", "operation": "bridge_notification",
        "input_contains": None}]},
        {"event_id": "e4", "claims": []}]}
    # A wrong judge offset is resolved from the exact quote; timing comes from the delta that completed it.
    # A null selector means "nothing to match", not a malformed extraction.
    [extracted] = validate_claims(parsed, outputs, {"A"})
    assert (extracted["start"], extracted["end"], extracted["sequence"]) == (6, 13, 2)
    assert extracted["input_contains"] == {}
    parsed["outputs"][0]["claims"][0]["quote"] = "Delivered."
    with pytest.raises(ValueError, match="quote not found"):
        validate_claims(parsed, outputs, {"A"})
    parsed["outputs"].pop(0)
    with pytest.raises(ValueError, match="every foreground output"):
        validate_claims(parsed, outputs, {"A"})


def test_collector_replays_only_after_durable_cursor_and_rejects_gaps(tmp_path):
    collector = CollaborationEventCollector(None, tmp_path / "events.jsonl")
    with collector.path.open("w") as output:
        collector._accept({"type": "connected", "session_epoch": 7, "sequence": 0}, output)
        collector._accept(event(1, "run.started"), output)
        # Reconnect advertises server's newest watermark, not our persisted position.
        collector._accept({"type": "connected", "session_epoch": 7, "sequence": 2}, output)
        collector._accept(event(1, "run.started"), output)
        collector._accept(event(2, "tool.completed"), output)
        assert collector.cursor == (7, 2)
        assert len(collector.path.read_text().splitlines()) == 2
        with pytest.raises(CollaborationEvidenceError, match="sequence gap"):
            collector._accept(event(4, "tool.completed"), output)
        with pytest.raises(CollaborationEvidenceError, match="epoch changed"):
            collector._accept({**event(3, "run.started"), "session_epoch": 8}, output)
        with pytest.raises(CollaborationEvidenceError, match="conflicting duplicate"):
            collector._accept(event(2, "tool.failed"), output)


def test_persistence_failure_never_advances_cursor(tmp_path, monkeypatch):
    collector = CollaborationEventCollector(None, tmp_path / "events.jsonl")
    with collector.path.open("w") as output:
        collector._accept({"type": "connected", "session_epoch": 7, "sequence": 0}, output)
        monkeypatch.setattr("runner.collaboration.os.fsync", lambda _: (_ for _ in ()).throw(OSError("disk full")))
        with pytest.raises(OSError, match="disk full"):
            collector._accept(event(1, "run.started"), output)
        assert collector.cursor == (7, 0)
        assert collector.events == []


@pytest.mark.parametrize("finish", ["release", "cancel", "freeze", "fail"])
def test_fixture_gate_controls_real_effect_and_cancellation(finish):
    spec = {"state": {"clipboard": {"generate": "token"}}, "gates": [{"id": "post", "tool": "bridge_notification"}]}
    if finish == "fail": spec["failures"] = {"bridge_notification": "permission denied"}
    fixture = PhoneDataFixture(spec)
    cancelled = threading.Event()
    with ThreadPoolExecutor(max_workers=1) as pool:
        future = pool.submit(fixture.invoke, "bridge_notification", {"title": "Ready", "body": "Package"},
                             task_id="A", call_id="call-1", canceled=cancelled.is_set)
        with fixture._condition:
            assert fixture._condition.wait_for(lambda: any(e["kind"] == "fixture.gate" for e in fixture.events), timeout=2)
        assert fixture.snapshot()["state"]["notifications"] == []
        if finish == "cancel": cancelled.set()
        elif finish == "freeze": fixture.close()
        else: fixture.release("post", {"session_epoch": 7, "sequence": 9})
        result = future.result(timeout=2)
    assert result["ok"] is (finish == "release")
    assert len(fixture.snapshot()["state"]["notifications"]) == (1 if finish == "release" else 0)
    assert fixture.initial_state["clipboard"] != {"generate": "token"}
    fixture.close()


@pytest.mark.parametrize("action", [{"action": "query"}, {"schedule_at": "2099-01-01T00:00:00Z"}])
def test_fixture_does_not_turn_queries_or_scheduled_requests_into_immediate_effects(action):
    fixture = PhoneDataFixture({})
    result = fixture.invoke("bridge_notification", {"title": "Delivery", "body": "Parcel", **action},
                            task_id="A", call_id="call", canceled=lambda: False)
    assert result["ok"] is False
    assert fixture.snapshot()["state"]["notifications"] == []
    fixture.close()


def test_permission_gate_denies_until_operator_release():
    fixture = PhoneDataFixture({"gates": [{"id": "unlock", "tool": "bridge_notification",
        "at": "permission", "error": "Phone locked"}]})
    args = {"title": "Delivery", "body": "Parcel"}
    invoke = lambda: fixture.invoke("bridge_notification", args, task_id="A", call_id="call", canceled=lambda: False)
    assert invoke() == {"ok": False, "error": "Phone locked"}
    assert fixture.snapshot()["state"]["notifications"] == []
    fixture.release("unlock")
    assert invoke()["ok"] is True
    assert len(fixture.snapshot()["state"]["notifications"]) == 1
    fixture.close()


@pytest.mark.parametrize("feedback", ["absent", "wrong_update", "failed_response", "empty", "valid", "accurate_failure"])
def test_three_milestones_require_matching_outcome_feedback(feedback):
    outcome = "failed" if feedback == "accurate_failure" else "completed"
    terminal = event(1, "task.updated", payload={"phase": "terminal", "status": outcome})
    injection = event(2, "task.result_injected", payload={"updates": [
        {"task_id": "A", "update_id": "u1", "status": outcome, "goal_revision": 1}]})
    events = [terminal, injection]
    if feedback != "absent":
        reply = event(3, "response.feedback_terminal", payload={"text": "Permission denied", "status": "cancelled" if feedback == "failed_response" else "completed"})
        reply["collaboration"]["update_ids"] = ["u1" if feedback != "wrong_update" else "u2"]

        if feedback == "empty": reply["payload"]["text"] = ""
        events.append(reply)
    milestones = _milestones(events, "A", 1)
    assert milestones["backend_terminal"] == terminal
    assert milestones["result_injected"] == injection
    assert bool(milestones["foreground_feedback"]) is (feedback in {"valid", "accurate_failure"})


def test_controlled_tool_failure_is_distinct_from_task_manager_failure(tmp_path):
    from runner.scenario import _check
    fixture = {"events": [{"event_id": "f1", "kind": "fixture.call", "payload": {
        "phase": "failed", "operation": "bridge_notification", "input": {"title": "Delivery"}}}]}
    assert _check({"kind": "fixture_failure", "operation": "bridge_notification", "input_contains": {"title": "Delivery"}},
                  [], fixture, {}, {}) == (True, ["f1"])


def test_provider_failure_is_unscored_not_missing_delegation(tmp_path):
    task = TaskSpec(id="scenario", category="multi_step", prompt="Post", description_for_judge="Post",
                    rubric=[], hard_assertions=HardAssertions(), scenario=ScenarioSpec(
                        version=1, steps=[], completion={"tasks": [{"task": "posting", "status": "completed", "feedback": "required"}]},
                        assertions=[], policies=["supported_completion_claims"], timeout_sec=30))
    evidence = {"complete": True, "assertions": [{"id": "scenario_completed", "verdict": "unscored"}],
                "events": [event(1, "turn.settled", payload={"status": "failed", "error": "provider unavailable"})],
                "fixture": {}, "task_aliases": {}, "milestones": {}}
    assert evaluate_scenario(evidence, task, None, tmp_path)["outcome"] == "unscored"


def test_disabled_judge_or_incomplete_evidence_never_passes(tmp_path):
    task = TaskSpec(id="scenario", category="multi_step", prompt="Post", description_for_judge="Post",
                    rubric=[], hard_assertions=HardAssertions(), scenario=ScenarioSpec(version=1, steps=[], completion={"turn": "initial", "delegation": "none"},
                                                    assertions=[], policies=["supported_completion_claims"], timeout_sec=30))
    for complete in (True, False):
        evidence = {"complete": complete, "assertions": [], "events": [], "fixture": {}, "milestones": {}}
        assert evaluate_scenario(evidence, task, None, tmp_path)["outcome"] == "unscored"


def test_evaluation_retains_early_false_claim_even_with_final_success(tmp_path, monkeypatch):
    from runner.judge import JudgeConfig
    task = TaskSpec(id="scenario", category="multi_step", prompt="Post", description_for_judge="Post",
                    rubric=[], hard_assertions=HardAssertions(), scenario=ScenarioSpec(
                        version=1, steps=[], completion={"turn": "initial", "delegation": "none"},
                        assertions=[], policies=["supported_completion_claims"], timeout_sec=30))
    monkeypatch.setattr("runner.judge.judge_collaboration_claims", lambda *_args, **_kw: [claim(1), claim(5)])
    evidence = {"complete": True, "assertions": [], "events": execution(), "fixture": effect_fixture()}
    evaluation = evaluate_scenario(evidence, task, JudgeConfig(model="test"), tmp_path)
    assert evaluation["outcome"] == "fail"
    assert [c["verdict"] for c in evaluation["claims"]] == ["unsupported", "supported"]


def test_report_links_are_attempt_scoped_and_escape_content(tmp_path):
    from runner.collaboration_report import write_collaboration_report
    from runner.html_report import _collaboration_html
    attempt = tmp_path / "tasks" / "<task>" / "attempt_2"
    attempt.mkdir(parents=True)
    summary = write_collaboration_report({"events": execution(), "complete": True},
        {"outcome": "fail", "claims": [{"verdict": "unsupported"}], "checks": []}, attempt)
    html = _collaboration_html(tmp_path, [{"task_id": "<task>", "artifact_dir": str(attempt),
        "metrics": {"collaboration": summary}}])
    assert "1 unsupported claims" in html
    assert "&lt;task&gt;" in html
    assert "attempt_2/collaboration-timeline.json" in html
    assert json.loads((attempt / "collaboration-timeline.json").read_text())[0]["event_id"] == "e1"


@pytest.mark.parametrize("invalid", [
    {"gates": [{"id": "g", "tool": "unknown"}]},
    {"gates": [{"id": "g", "tool": "bridge_clipboard", "timeout_sec": True}]},
    {"gates": [{"id": "g", "tool": "bridge_clipboard", "at": "after_effect"}]},
    {"state": {"clipboard": 12}},
    {"failures": {"unknown": "failed"}},
])
def test_fixture_validation_rejects_unexecutable_controls(tmp_path, invalid):
    from runner.suite import load_suite, SuiteValidationError
    fixture = {"name": "phone_data_v1", **invalid}
    raw = {"name": "synthetic", "global_reset": {}, "tasks": [{
        "id": "task", "category": "multi_step", "prompt": "Hello", "description_for_judge": "Hello",
        "rubric": [{"id": "answer", "check": "Respond"}],
        "mock_environment": {"platform": "android", "tools": {}, "collaboration_fixture": fixture},
        "scenario": {"version": 1, "steps": [], "completion": {"turn": "initial", "delegation": "none"}},
    }]}
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(raw))
    with pytest.raises(SuiteValidationError, match="collaboration_fixture"):
        load_suite(path)


def test_rejudge_uses_collaboration_evidence_not_final_answer(tmp_path, monkeypatch):
    from runner import rejudge
    from runner.suite import Suite
    task = TaskSpec(id="scenario", category="multi_step", prompt="Post", description_for_judge="Post",
                    rubric=[], hard_assertions=HardAssertions(), scenario=ScenarioSpec(
                        version=1, steps=[], completion={"turn": "initial", "delegation": "none"},
                        assertions=[], policies=["supported_completion_claims"], timeout_sec=30))
    suite = Suite(name="synthetic", tasks=[task], global_reset={}, sha256="test", source_path=tmp_path / "suite.json")
    (tmp_path / "suite.json").write_text("{}")
    monkeypatch.setattr(rejudge, "load_suite", lambda _: suite)
    attempt = tmp_path / "tasks" / "scenario"
    attempt.mkdir(parents=True)
    evidence = {"complete": True, "assertions": [], "events": execution(), "fixture": effect_fixture()}
    (attempt / "collaboration.json").write_text(json.dumps(evidence))
    (tmp_path / "results.jsonl").write_text(json.dumps({"task_id": "scenario", "attempt": 1,
        "status": "passed", "metrics": {"execution_mode": "realtime-text"}}) + "\n")
    monkeypatch.setattr("runner.judge.judge_collaboration_claims", lambda *_args, **_kw: [claim(1), claim(5)])
    monkeypatch.setattr(rejudge, "judge_task", lambda **_: pytest.fail("backend-only judge used"))
    rejudge.rejudge_run(tmp_path, "test-model", "https://judge.invalid/v1")
    row = json.loads((tmp_path / "results.rejudged.jsonl").read_text())
    assert row["status"] == "failed"
    assert row["metrics"]["success"] is False
    assert (attempt / "collaboration-evaluation.rejudged.json").exists()
    assert not (attempt / "collaboration-evaluation.json").exists()


@pytest.mark.parametrize("provider,options", [
    ("openai", {"model": "gpt-realtime-2", "endpoint": "wss://gateway.example/v1/realtime", "realtime_protocol": "legacy"}),
    ("speko", {"model": "gemini-live", "upstream_provider": "google"}),
    ("gemini", {"auth_mode": "api_key"}),
    ("qwen", {}), ("xai", {}),
])
def test_ci_foreground_options_reach_toml_without_changing_backend(monkeypatch, provider, options):
    import tomllib
    from ci.run_case import _runtime_environment
    suffixes = {"endpoint": "ENDPOINT", "realtime_protocol": "PROTOCOL",
                "upstream_provider": "UPSTREAM_PROVIDER", "auth_mode": "AUTH_MODE", "model": "MODEL"}
    env = _runtime_environment({"BENCHMARK_REALTIME_" + suffixes[key]: value for key, value in options.items()})
    forwarded = {key: env["AICO_BENCHMARK_REALTIME_" + suffixes[key]] for key in options}
    monkeypatch.setenv("TEST_REALTIME_KEY", "test-key")
    base = '[model_settings.model]\nprovider="existing"\nmodel="fixed-backend"\n'
    rendered = configure_realtime_text(base, provider=provider, api_key_env="TEST_REALTIME_KEY",
                                      **{"model": "", **forwarded})
    config = tomllib.loads(rendered)
    record = config["voice_settings"]["realtime"]["providers"]["benchmark-foreground"]
    assert config["model_settings"] == tomllib.loads(base)["model_settings"]
    assert record["type"] == provider
    for key, value in options.items():
        assert record[key] == value
    assert ("model" in record) is bool(options.get("model"))
    assert record["api_key"] == "$TEST_REALTIME_KEY"
    # Explicit TOML remains authoritative, including provider-specific routing.
    assert configure_realtime_text(rendered, provider="", model="", api_key_env="TEST_REALTIME_KEY") == rendered
    with pytest.raises(ValueError, match="not both"):
        configure_realtime_text(rendered, provider="", model="", api_key_env="TEST_REALTIME_KEY", endpoint="wss://other.example")


def test_foreground_config_preserves_main_model_and_credential_reference(monkeypatch):
    import tomllib
    monkeypatch.setenv("TEST_REALTIME_KEY", "test-only-not-a-secret")
    base = '[model]\nprovider="existing"\nmodel="fixed-backend"\n'
    rendered = configure_realtime_text(base, provider="openai", model="test-realtime", api_key_env="TEST_REALTIME_KEY")
    parsed = tomllib.loads(rendered)
    assert parsed["model"] == tomllib.loads(base)["model"]
    assert parsed["voice_settings"]["realtime"]["providers"]["benchmark-foreground"]["api_key"] == "$TEST_REALTIME_KEY"
    assert "test-only-not-a-secret" not in rendered

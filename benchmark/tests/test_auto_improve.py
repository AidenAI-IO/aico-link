"""Behavioral contracts for unattended improvements, using synthetic evidence only."""
from __future__ import annotations

from datetime import datetime, timedelta, timezone
import io
import json
from pathlib import Path
import stat
import subprocess
import zipfile

import pytest

from ci.auto_improve import control, driver, emulator, evidence, execution, github, policy, review


def row(repeat, success=True, **metrics):
    return {"repeat": repeat, "status": "passed" if success else "failed", "metrics": {
        "agent_eligible": True, "success": success, "tool_calls": 3, "total_tokens": 100, **metrics}}


def pair():
    plan = {"targets": ["phone:tap"], "neighbors": ["phone:type"], "held_out": ["memory:recall"]}
    baseline = {k: [row(i, k != "phone:tap") for i in range(1, 4)] for keys in plan.values() for k in keys}
    candidate = {k: [row(i) for i in range(1, 4)] for k in baseline}
    return plan, baseline, candidate


def test_gate_requires_complete_paired_evidence():
    plan, baseline, candidate = pair()
    assert policy.compare(plan, baseline, candidate)["passed"]
    candidate["phone:tap"] = candidate["phone:tap"][:2]
    result = policy.compare(plan, baseline, candidate)
    assert not result["passed"]
    assert any("incomplete" in reason for reason in result["reasons"])


@pytest.mark.parametrize("mutation", ["duplicate", "judge", "infra", "regression", "missing_cost", "cost", "no_improvement"])
def test_gate_rejects_unmeasured_or_regressing_changes(mutation):
    plan, baseline, candidate = pair()
    if mutation == "duplicate":
        candidate["phone:tap"][1]["repeat"] = 1
    elif mutation == "judge":
        candidate["phone:tap"][0] = {"repeat": 1, "status": "judge_error"}
    elif mutation == "infra":
        candidate["phone:tap"][0]["metrics"]["agent_eligible"] = False
    elif mutation == "regression":
        candidate["phone:type"][0] = row(1, False)
    elif mutation == "missing_cost":
        candidate["phone:tap"][0]["metrics"]["total_tokens"] = None
    elif mutation == "cost":
        for rows in candidate.values():
            for r in rows:
                r["metrics"]["total_tokens"] = 500
    else:
        candidate["phone:tap"] = baseline["phone:tap"]
    assert not policy.compare(plan, baseline, candidate)["passed"]


@pytest.mark.parametrize("path", ["src/agent/internal/agent/navigation.go", "src/agent/cmd/daemon/main.go"])
def test_code_patches_are_allowed_and_atomic(tmp_path, path):
    file = tmp_path / path
    file.parent.mkdir(parents=True)
    before = 'package agent\nfunc retries() int { return 0 }\n'
    after = before.replace('return 0', 'return 1')
    file.write_text(before)
    edits = [{"path": path, "old": "return 0", "new": "return 1"}]
    assert policy.apply_edits(tmp_path, edits) == [path]
    policy.validate_change(path, before, after)
    assert file.read_text() == after
    with pytest.raises(ValueError):
        policy.apply_edits(tmp_path, [{"path": path, "old": "return 1", "new": "return 2"},
                                      {"path": "benchmark/suites/fake.json", "old": "x", "new": "y"}])
    assert file.read_text() == after


@pytest.mark.parametrize("path", ["../SKILL.md", "/src/agent/config/skills/x/SKILL.md",
    "src/agent/internal/agent/tools_demo_test.go", "src/agent/go.mod", "src/agent/go.sum",
    "src/agent/internal/../go.mod", "src/agent/internal/vendor/dep/code.go",
    "src/agent/internal/testdata/example.go", "src/agent/internal//agent/code.go",
    "src/agent/internal/agent/.cache/code.go", "benchmark/runner/judge.py",
    ".github/workflows/ci.yml", "scripts/run_tests_in_docker.sh"])
def test_edit_scope_preserves_evaluators_tests_dependencies_and_ci(path):
    assert not policy.edit_target(path)


def test_selection_defers_instead_of_dropping_neighbors():
    tasks = {f"phone:{i}": {"case_id": "phone", "environment": "mobilegym", "task": {"category": "touch"}} for i in range(5)}
    tasks["other:keep"] = {"case_id": "other", "environment": "mobilegym", "task": {"category": "input"}}
    cluster = {"targets": ["phone:0"], "neighbors": []}
    with pytest.raises(ValueError, match="budget"):
        policy.select_tasks(cluster, tasks, max_tasks=3)
    plan = policy.select_tasks(cluster, tasks, max_tasks=8)
    assert len(plan["neighbors"]) == 4
    assert plan["held_out"] == ["other:keep"]


def test_source_selection_delays_and_deduplicates():
    now = datetime(2026, 1, 20, tzinfo=timezone.utc)
    def run(number, hours, **extra):
        return {"id": number, "run_attempt": 1, "status": "completed", "event": "schedule", "head_branch": "main",
                "conclusion": "success", "created_at": (now - timedelta(hours=hours + 1)).isoformat(),
                "updated_at": (now - timedelta(hours=hours)).isoformat(), **extra}
    runs = [run(1, 4), run(2, 2), run(3, 6, event="pull_request"), run(4, 6, head_branch="fork"), run(5, 24 * 15), run(6, 5)]
    selected = github.choose_source(runs, {"6-1"}, branch="main", now=now)
    assert selected["id"] == 1
    assert github.choose_source(runs, {"1-1", "6-1"}, branch="main", now=now) is None
    assert github.choose_source(runs, set(), branch="main", now=now, requested="4") is None
    assert github.choose_source(runs, {"1-1"}, branch="main", now=now, requested="1")["id"] == 1
    assert github.choose_source(runs, set(), branch="main", now=now, prefer_latest=True)["id"] == 1


@pytest.mark.parametrize("branch", ["release", "develop", "refs/heads/main", ""])
def test_non_main_discovery_and_publication_are_rejected_before_side_effects(tmp_path, branch):
    now = datetime.now(timezone.utc)
    with pytest.raises(ValueError, match="only supports main"):
        github.choose_source([], set(), branch=branch, now=now)
    with pytest.raises(ValueError, match="only supports main"):
        control.discover(None, tmp_path / "source", "42", branch)
    with pytest.raises(ValueError, match="only supports main"):
        control.publish(None, tmp_path, tmp_path, branch)
    assert not (tmp_path / "source").exists()


@pytest.mark.parametrize("event,ref,mode,requested,expected", [
    ("push", "refs/heads/auto-improve-test/diagnose/first", "improve", True, {"mode": "diagnose", "publish": False}),
    ("push", "refs/heads/auto-improve-test/improve/first", "diagnose", True, {"mode": "improve", "publish": False}),
    ("workflow_dispatch", "refs/heads/auto-improve-test/diagnose/first", "improve", True, {"mode": "improve", "publish": False}),
    ("workflow_dispatch", "refs/heads/main", "improve", False, {"mode": "improve", "publish": False}),
    ("workflow_dispatch", "refs/heads/main", "improve", True, {"mode": "improve", "publish": True}),
    ("schedule", "refs/heads/main", "improve", False, {"mode": "improve", "publish": True}),
    ("schedule", "refs/heads/main", "diagnose", True, {"mode": "diagnose", "publish": False}),
])
def test_branch_execution_is_separate_from_publication(event, ref, mode, requested, expected):
    assert policy.execution_policy(event, ref, mode, requested) == expected


@pytest.mark.parametrize("event,ref", [
    ("pull_request", "refs/heads/main"), ("pull_request_target", "refs/heads/main"),
    ("push", "refs/heads/main"), ("push", "refs/heads/auto-improve-test/other"),
    ("schedule", "refs/heads/auto-improve-test/diagnose/first"),
    ("workflow_dispatch", "refs/heads/feature"), ("workflow_dispatch", "refs/tags/main"),
])
def test_execution_rejects_unintended_triggers(event, ref):
    with pytest.raises(ValueError):
        policy.execution_policy(event, ref, "diagnose")


def test_workflow_policy_command_needs_no_github_credentials(tmp_path, monkeypatch):
    monkeypatch.delenv("GH_TOKEN", raising=False)
    monkeypatch.delenv("GITHUB_REPOSITORY", raising=False)
    output = tmp_path / "output"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setattr("sys.argv", ["control", "policy", "--event", "push", "--ref",
                                    "refs/heads/auto-improve-test/diagnose/first", "--mode", "improve"])
    assert control.cli() == 0
    values = dict(line.split("=", 1) for line in output.read_text().splitlines())
    assert values == {"value": "diagnose", "publish": "false", "artifact_prefix": "auto-improve-test-source-"}


def test_checkout_uses_logged_deadline_bounded_commands(tmp_path, monkeypatch):
    import time
    commands = []
    monkeypatch.setattr(driver.execution, "command", lambda args, **kwargs: commands.append((args, kwargs)))
    driver.checkout(tmp_path, tmp_path / "baseline", "base", deadline=time.monotonic() + 1200)
    assert len(commands) == 3
    assert all(120 < kwargs["timeout"] <= 900 for _, kwargs in commands)
    assert [kwargs["log"].name for _, kwargs in commands] == [
        "baseline-clone.log", "baseline-checkout.log", "baseline-submodule.log"]
    commands.clear()
    with pytest.raises(TimeoutError):
        driver.checkout(tmp_path, tmp_path / "candidate", "base", deadline=time.monotonic() - 1)
    assert not commands


@pytest.mark.parametrize("branch_test", [False, True])
def test_driver_reports_checkout_timeout_as_blocked(tmp_path, monkeypatch, branch_test):
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    monkeypatch.setenv("GITHUB_REF", "refs/heads/auto-improve-test/improve/initial" if branch_test else "refs/heads/main")
    repo, source, out = tmp_path / "repo", tmp_path / "source", tmp_path / "out"
    repo.mkdir(); source.mkdir()
    target = "src/agent/config/skills/test/SKILL.md"
    file = repo / target
    file.parent.mkdir(parents=True)
    file.write_text("Existing source path\n")
    (source / "source.json").write_text(json.dumps({"run": {"id": 42, "run_attempt": 1, "head_sha": "old", "head_branch": "main"}}))
    tasks = {k: {"case_id": k.split(':')[0], "environment": "isolated", "runnable": True,
                 "task": {"category": "task"}} for k in ("a:target", "a:neighbor", "b:held")}
    cluster = {"title": "issue", "category": "task", "cause": "cause", "judge_review": "supported",
               "fix_target": "skill", "targets": ["a:target"], "neighbors": [],
               "source_paths": [target], "evidence": ["failure-001"]}
    monkeypatch.setattr(driver, "git", lambda *args: "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures", lambda *args: ([{"key": "a:target", "agent_eligible": True}], []))
    monkeypatch.setattr(driver, "ask", lambda _out, name, *args, **kwargs:
                        {"summary": "one failure", "clusters": [cluster]} if name == "diagnosis" else {})
    monkeypatch.setattr(driver, "validated_source_paths", lambda *args: [target])
    monkeypatch.setattr(driver, "checkout", lambda *args, **kwargs: (_ for _ in ()).throw(
                        subprocess.TimeoutExpired("git clone", 900)))
    state = driver.run(repo, source, out, mode="improve")
    assert state["status"] == "blocked"
    assert state["attempts"][0]["status"] == "inconclusive"
    assert "timed out" in state["attempts"][0]["error"].lower()


def test_driver_rejects_non_main_source_even_when_given_local_artifacts(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    (source / "source.json").write_text(json.dumps({"run": {"head_branch": "feature"}}))
    with pytest.raises(ValueError, match="only supports main"):
        driver.run(tmp_path, source, tmp_path / "out")
    assert not (tmp_path / "out").exists()


@pytest.mark.parametrize("name,mode", [("../escape", 0), ("/escape", 0), ("link", stat.S_IFLNK | 0o777), ("..\\escape", 0)])
def test_artifact_extraction_rejects_unsafe_entries(tmp_path, name, mode):
    data = io.BytesIO()
    with zipfile.ZipFile(data, "w") as archive:
        info = zipfile.ZipInfo(name)
        info.external_attr = mode << 16
        archive.writestr(info, "bad")
    with pytest.raises(ValueError):
        github.extract_archive(data.getvalue(), tmp_path / "out")
    assert not (tmp_path / "escape").exists()


def test_avd_clone_rewrites_writable_disks_and_leaves_template_unchanged(tmp_path):
    template = tmp_path / "template"
    template.mkdir()
    (template / "userdata-qemu.img").write_bytes(b"clean disk")
    (template / "config.ini").write_text(f"AvdId=original\ndisk.dataPartition.path={template}/userdata-qemu.img\n")
    before = emulator.template_digest(template)
    for name in ("baseline", "candidate"):
        clone = tmp_path / name
        emulator.clone_template(template, clone, name)
        assert f"disk.dataPartition.path={clone}/userdata-qemu.img" in (clone / "config.ini").read_text()
    (tmp_path / "baseline/userdata-qemu.img").write_bytes(b"mutated")
    assert (tmp_path / "candidate/userdata-qemu.img").read_bytes() == b"clean disk"
    assert emulator.template_digest(template) == before
    (template / "hardware-qemu.ini.lock").touch()
    with pytest.raises(ValueError, match="stopped"):
        emulator.template_digest(template)


def test_emulator_failure_terminates_only_owned_process(tmp_path, monkeypatch):
    template = tmp_path / "template"
    template.mkdir()
    (template / "config.ini").write_text("AvdId=fixture\n")
    (template / "userdata-qemu.img").write_bytes(b"clean")
    class Process:
        pid = 123
        def poll(self):
            return 1
    proc = Process()
    terminated = []
    commands = []
    monkeypatch.setattr(emulator.shutil, "which", lambda value: value)
    monkeypatch.setattr(emulator, "free_port", lambda: 5580)
    monkeypatch.setattr(emulator, "process_start", lambda _: "test-start")
    monkeypatch.setattr(emulator.subprocess, "check_output", lambda *a, **kw: "List of devices attached\nemulator-5554\tdevice\n")
    monkeypatch.setattr(emulator.subprocess, "Popen", lambda cmd, **kw: commands.append(cmd) or proc)
    monkeypatch.setattr(emulator, "terminate_process_tree", lambda p: terminated.append(p))
    with pytest.raises(RuntimeError, match="exited"):
        with emulator.isolated_emulator(tmp_path / "owned", template=template,
                                       expected_sha256=emulator.template_digest(template)):
            pytest.fail("Exited emulator must not run tasks")
    assert terminated == [proc]
    assert commands[0][commands[0].index("-port") + 1] == "5580"
    assert not (tmp_path / "owned/avds").exists()
    assert template.is_dir()


def test_review_has_no_mutating_tools_or_publication_credentials(tmp_path, monkeypatch):
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_MODEL", "review-model")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_API_KEY", "review-key")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_BUDGET_USD", "0.01")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_BASE_URL", "https://gateway.example/anthropic")
    monkeypatch.setenv("GH_TOKEN", "never-forward")
    monkeypatch.setenv("BENCHMARK_AGENT_API_KEY", "never-forward-either")
    monkeypatch.setattr(review.shutil, "which", lambda _: "/bin/claude")
    captured = {}
    class Process:
        returncode = 0
        def communicate(self, prompt, timeout):
            return json.dumps({"total_cost_usd": 12.5, "num_turns": 10,
                               "structured_output": {"supported": True, "reason": "ok", "evidence": []}}), None
    def popen(command, **kwargs):
        captured.update(command=command, **kwargs)
        return Process()
    monkeypatch.setattr(review.subprocess, "Popen", popen)
    assert review.ask(tmp_path, "audit", review.AUDIT)["supported"]
    assert "GH_TOKEN" not in captured["env"] and "BENCHMARK_AGENT_API_KEY" not in captured["env"]
    assert "--max-budget-usd" not in captured["command"]
    assert json.loads((tmp_path / "session-usage.json").read_text())["total_cost_usd"] == 12.5
    assert captured["command"][captured["command"].index("--tools") + 1] == "Read,Grep,Glob"
    assert "--restricted" in captured["command"]
    assert captured["env"]["ANTHROPIC_BASE_URL"] == "https://gateway.example/anthropic"
    assert captured["env"]["ANTHROPIC_API_KEY"] == "review-key"


@pytest.mark.parametrize("returncode,payload", [
    (1, {"type": "result", "is_error": True, "errors": ["Gateway rejected synthetic-key"], "session_id": "private-session"}),
    (0, {"is_error": True, "result": "Gateway rejected synthetic-key"}),
])
def test_review_preserves_stdout_errors_without_leaking_secrets(tmp_path, monkeypatch, returncode, payload):
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_MODEL", "review-model")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_API_KEY", "synthetic-key")
    monkeypatch.setattr(review.shutil, "which", lambda _: "/bin/claude")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_BUDGET_USD", "0.01")
    class Process:
        def communicate(self, *args, **kwargs):
            return json.dumps(payload), None
    proc = Process()
    proc.returncode = returncode
    monkeypatch.setattr(review.subprocess, "Popen", lambda *args, **kwargs: proc)
    with pytest.raises((RuntimeError, ValueError)):
        review.ask(tmp_path, "audit", review.AUDIT)
    saved = (tmp_path / "session-error.log").read_text()
    assert "Gateway rejected" in saved
    assert "synthetic-key" not in saved
    assert "private-session" not in saved


def test_failure_evidence_uses_source_rubric_and_keeps_unknown_tasks(tmp_path):
    run = tmp_path / "benchmark/runs/ci-42-1-phone"
    run.mkdir(parents=True)
    (run / "manifest.json").write_text(json.dumps({"metrics_k": 1, "agent_model": "old-model"}))
    (run / "suite.json").write_text(json.dumps({"tasks": [{"id": "tap", "rubric": ["original rule"]}]}))
    (run / "results.jsonl").write_text(json.dumps({"task_id": "tap", "attempt": 1, "status": "failed"}) + "\n")
    failures, warnings = evidence.source_failures(tmp_path, "42", "1", {})
    assert not warnings
    assert failures[0]["task"]["rubric"] == ["original rule"]
    assert failures[0]["current_task"] is None


def test_benchmark_environment_excludes_review_and_github_secrets(monkeypatch):
    monkeypatch.setenv("GH_TOKEN", "secret")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_API_KEY", "secret")
    monkeypatch.setenv("BENCHMARK_AGENT_API_KEY", "benchmark-only")
    env = execution.benchmark_environment()
    assert "GH_TOKEN" not in env and "AUTO_IMPROVE_REVIEW_API_KEY" not in env
    assert env["BENCHMARK_AGENT_API_KEY"] == "benchmark-only"


@pytest.mark.parametrize("paths,go_check", [
    (["src/agent/config/skills/test/SKILL.md"], False),
    (["src/agent/internal/agent/navigation.go"], True),
    (["src/agent/cmd/daemon/main.go", "src/agent/config/skills/test/SKILL.md"], True),
])
def test_implementation_changes_require_go_unit_suite(paths, go_check):
    checks = policy.verification_commands(paths)
    assert checks[0] == ["make", "check"]
    assert (["bash", "scripts/run_tests_in_docker.sh", "--suite", "go-unit"] in checks) is go_check


def test_review_workspace_does_not_copy_paths_from_untrusted_payload(tmp_path):
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "secret").write_text("private")
    out = tmp_path / "output"
    out.mkdir()
    stage = driver.workspace(out, "review", tmp_path,
                             {"evidence": str(outside), "pre_image": str(outside / "secret")}, include_code=False)
    assert not (stage / "images").exists()
    assert not (stage / "attempts").exists()


@pytest.mark.parametrize("costs,total,measured,complete", [
    ([1.25, 2.5], 3.75, 2, True), ([None, 2.5], 2.5, 1, False),
    ([None], None, 0, False), ([False, -1, float('nan')], None, 0, False),
])
def test_cost_reporting_distinguishes_missing_prices_from_zero(costs, total, measured, complete):
    result = policy.review_cost_summary([{"total_cost_usd": cost} for cost in costs])
    assert result == {"estimated_cost_usd": total, "measured_sessions": measured,
                      "total_sessions": len(costs), "complete": complete}


def test_reproduction_filters_targets_independently_without_discarding_regression_coverage():
    plan = {"targets": ["a:broken", "a:recovered", "a:environment"], "neighbors": ["a:neighbor"], "held_out": ["b:held"]}
    baseline = {key: [row(i, key == "a:recovered") for i in range(1, 4)] for key in plan["targets"]}
    audit = {"tasks": [{"task": key, "disposition": disposition, "reason": "evidence", "evidence": ["trace"]}
                       for key, disposition in [("a:broken", "agent_failure"), ("a:recovered", "agent_failure"),
                                                ("a:environment", "environment_or_evaluation")]]}
    filtered, removed = policy.prune_reproduction(plan, audit, baseline)
    assert filtered["targets"] == ["a:broken"]
    assert set(filtered["neighbors"]) == {"a:neighbor", "a:recovered", "a:environment"}
    assert removed["targets"] == ["a:recovered", "a:environment"]
    assert plan["targets"] == ["a:broken", "a:recovered", "a:environment"]
    audit["tasks"].pop()
    with pytest.raises(ValueError, match="task IDs"):
        policy.prune_reproduction(plan, audit, baseline)


def test_diagnosis_rejects_evidence_paths_as_edit_targets(tmp_path):
    cluster = {"targets": ["memory:task"], "category": "memory", "cause": "unknown",
               "judge_review": "supported", "fix_target": "go_code",
               "source_paths": ["details/failure-001.json"], "evidence": ["details/failure-001.json"]}
    assert not driver.validated_source_paths(tmp_path, cluster)
    code = tmp_path / "src/agent/internal/agent/memory.go"
    code.parent.mkdir(parents=True)
    code.write_text("package agent\n")
    cluster["source_paths"] = ["src/agent/internal/agent/memory.go"]
    assert driver.validated_source_paths(tmp_path, cluster) == ["src/agent/internal/agent/memory.go"]


def test_diagnosis_index_keeps_large_evidence_in_addressable_files(tmp_path):
    payload = {"failures": [{"key": "a:one", "status": "failed", "failure_class": "agent",
                            "agent_eligible": True, "rubric": [{"id": "saved", "verdict": "no", "reason": "missing"}],
                            "history": {"messages": ["detail " * 10000]},
                            "task": {"prompt": "Save the note"}}],
               "history": {"runs": [{"id": i, "details": "history " * 1000} for i in range(100)]},
               "feedback": [], "configuration": {"model": "test"}}
    stage = driver.workspace(tmp_path, "diagnosis", tmp_path, payload, include_code=False)
    index = json.loads((stage / "input.json").read_text())
    assert (stage / "input.json").stat().st_size < 10000
    entry = index["failures"][0]
    assert entry["key"] == "a:one" and entry["rubric"] == payload["failures"][0]["rubric"]
    assert json.loads((stage / entry["detail_file"]).read_text()) == payload["failures"][0]
    assert json.loads((stage / index["history"]["detail_file"]).read_text()) == payload["history"]


@pytest.mark.parametrize("go_code", [False, True])
@pytest.mark.parametrize("ref", ["refs/heads/main", "refs/heads/auto-improve-test/improve/first"])
def test_driver_reproduces_before_edit_and_verifies_before_ready(tmp_path, monkeypatch, go_code, ref):
    monkeypatch.setenv("GITHUB_REF", ref)
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    repo, source, out = tmp_path / "repo", tmp_path / "source", tmp_path / "out"
    repo.mkdir(); source.mkdir()
    path = "src/agent/internal/agent/navigation.go" if go_code else "src/agent/config/skills/test/SKILL.md"
    before = "package agent\nfunc retries() int { return 0 }\n" if go_code else "Observe before acting.\n"
    after = before.replace("return 0", "return 1") if go_code else "Observe and verify before acting.\n"
    original = repo / path
    original.parent.mkdir(parents=True)
    original.write_text(before)
    (source / "source.json").write_text(json.dumps({"run": {"id": 42, "run_attempt": 1, "head_sha": "old", "head_branch": "main"}}))
    tasks = {k: {"case_id": k.split(':')[0], "environment": "isolated", "runnable": True,
                 "task": {"category": "task"}} for k in ("a:target", "a:neighbor", "b:held")}
    cluster = {"title": "Verify the screen", "category": "verification", "cause": "missing observation",
               "judge_review": "supported", "fix_target": "go_code" if go_code else "skill", "targets": ["a:target"],
               "neighbors": [], "source_paths": [path], "evidence": ["trace step 2"]}
    monkeypatch.setattr(driver, "git", lambda *a: "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures", lambda *a: ([{"key": "a:target", "agent_eligible": True}], []))
    events = []
    def fake_ask(_out, name, *args, **kwargs):
        events.append(name)
        if name == "diagnosis":
            return {"summary": "One observed failure", "clusters": [cluster]}
        if name == "reproduction-review":
            return {"summary": "Reproduced", "tasks": [{"task": "a:target", "disposition": "agent_failure",
                                                          "reason": "Failed deterministic assertion", "evidence": ["fresh trace"]}]}
        return {"summary": "Verify first", "edits": [{"path": path, "old": before, "new": after}]}
    def fake_checkout(_, dest, base, *, deadline):
        (dest / path).parent.mkdir(parents=True)
        (dest / path).write_text(original.read_text())
    def fake_run(root, key, repeat, *args, **kwargs):
        events.append(f"run:{root.name}:{key}")
        return row(repeat, root.name == "candidate" or key != "a:target")
    monkeypatch.setattr(driver, "ask", fake_ask)
    monkeypatch.setattr(driver, "checkout", fake_checkout)
    monkeypatch.setattr(driver, "audit", lambda *a: {"supported": True, "reason": "checked", "evidence": ["trace"]})
    monkeypatch.setattr(driver.execution, "prepare", lambda *a: None)
    monkeypatch.setattr(driver.execution, "run_task", fake_run)
    checks = []
    def fake_check(command, **kwargs):
        assert (kwargs["cwd"] / path).read_text() == after
        checks.append(command)
        events.append("check")
    monkeypatch.setattr(driver.execution, "command", fake_check)
    monkeypatch.setattr(driver.subprocess, "check_output", lambda *a, **kw: "diff\n")
    result = driver.run(repo, source, out, mode="improve")
    assert result["attempts"][0]["status"] == "ready"
    assert checks == policy.verification_commands([path])
    assert result["attempts"][0]["checks"] == checks
    assert result["execution_ref"] == ref
    assert events.index("run:baseline:a:target") < events.index("fix") < events.index("check")
    assert events.index("check") < events.index("run:candidate:a:target")
    assert not (out / "cluster-1/candidate").exists()
    assert (out / "cluster-1/candidate.patch").read_text() == "diff\n"


def test_raw_artifact_paths_cannot_escape_run(tmp_path):
    run = tmp_path / "benchmark/runs/ci-42-1-a"
    run.mkdir(parents=True)
    (run / "manifest.json").write_text('{}')
    (run / "results.jsonl").write_text(json.dumps({"task_id": "../../other", "status": "failed", "attempt": 1}))
    failures, warnings = evidence.source_failures(tmp_path, "42", "1", {})
    assert not failures
    assert warnings


def test_publisher_recomputes_gate_before_touching_remote(tmp_path, monkeypatch):
    plan, baseline, candidate = pair()
    cluster = {"targets": plan["targets"], "source_paths": ["src/agent/config/skills/test/SKILL.md"], "category": "verification"}
    patch = tmp_path / "candidate.patch"
    patch.write_text("not a real patch")
    manifest = {"base_sha": "base", "base_branch": "main", "execution_ref": "refs/heads/main", "source": "42-1", "attempts": [{
        "status": "ready", "signature": policy.signature(cluster), "cluster": cluster,
        "patch": "candidate.patch", "patch_sha256": policy.digest(patch.read_text()),
        "check_passed": True, "audit": {"supported": True}, "plan": plan,
        "comparison": policy.compare(plan, baseline, candidate)}]}
    candidate["phone:tap"].pop()
    (tmp_path / "manifest.json").write_text(json.dumps(manifest))
    (tmp_path / "baseline.json").write_text(json.dumps(baseline))
    (tmp_path / "candidate.json").write_text(json.dumps(candidate))
    monkeypatch.setattr(control, "git", lambda *args: "base")
    class API:
        def request(self, path, payload=None):
            assert payload is None, "No publication is allowed before revalidation"
            return {"object": {"sha": "base"}}
        def issues(self):
            return []
    with pytest.raises(ValueError, match="Stored acceptance"):
        control.publish(API(), tmp_path, tmp_path, "main")


def test_orphan_cleanup_refuses_reused_pid(tmp_path, monkeypatch):
    owned = tmp_path / "job/result/cluster-1/baseline-runs/task/1/emulator"
    owned.mkdir(parents=True)
    owner = owned / "owner.json"
    owner.write_text(json.dumps({"pid": 123, "name": "auto-improve-test", "started": "old",
                                "avd": str(owned / "avds/auto-improve-test.avd")}))
    monkeypatch.setattr(emulator, "process_start", lambda _: "new")
    killed = []
    monkeypatch.setattr(emulator.os, "kill", lambda *args: killed.append(args))
    with pytest.raises(RuntimeError, match="reused"):
        emulator.cleanup_orphans(tmp_path)
    assert not killed
    assert owner.exists()


def test_evidence_redaction_keeps_json_valid(tmp_path, monkeypatch):
    run, out = tmp_path / "run", tmp_path / "out"
    run.mkdir()
    secret = 'secret-with-"quotes"'
    monkeypatch.setenv("BENCHMARK_AGENT_API_KEY", secret)
    value = {"task_id": "test", "metrics": {"error": f"Provider rejected {secret}"}, "token": secret}
    (run / "results.jsonl").write_text(json.dumps(value) + "\n")
    execution.retain_evidence(run, out)
    result = json.loads((out / "results.jsonl").read_text())
    assert result["token"] == "[REDACTED]"
    assert secret not in result["metrics"]["error"]


@pytest.mark.parametrize("value", [{"supported": "false", "reason": "no", "evidence": []},
                                   {"supported": True, "reason": "ok", "evidence": [], "extra": True}])
def test_review_contract_rejects_truthy_non_boolean_approval(value):
    with pytest.raises(ValueError):
        review.validate_output(value, review.AUDIT)


@pytest.mark.parametrize("nested", [False, True])
@pytest.mark.parametrize("branch_test", [False, True])
def test_discovery_normalizes_artifact_root(tmp_path, monkeypatch, nested, branch_test):
    monkeypatch.setenv("AUTO_IMPROVE_TEST_RUN", str(branch_test).lower())
    class API:
        def discover(self, *args, **kwargs):
            assert kwargs["include_processed"] is branch_test
            return {"artifact": {"id": 1}, "identity": "42-1", "run": {"id": 42}}
        def download(self, artifact, destination):
            root = destination / "benchmark" if nested else destination
            (root / "runs").mkdir(parents=True)
            (root / "dashboard-data.json").write_text('{"history": []}')
        def issues(self):
            return []
        def pages(self, *args, **kwargs):
            return []
    out = tmp_path / "source"
    result = control.discover(API(), out, "", "main")
    assert result["identity"] == "42-1"
    assert (out / "benchmark/runs").is_dir()
    assert json.loads((out / "history.json").read_text()) == {"history": []}


@pytest.mark.parametrize("go_code", [False, True])
def test_publisher_creates_only_verified_draft_from_exact_patch(tmp_path, monkeypatch, go_code):
    repo = tmp_path / "repo"
    repo.mkdir()
    import os
    env = {**os.environ, "GIT_EDITOR": "true", "GIT_CONFIG_NOSYSTEM": "1",
           "GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
           "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.invalid"}
    def local_git(*args):
        return subprocess.check_output(["git", *args], cwd=repo, env=env, text=True).strip()
    local_git("init", "-q")
    path = "src/agent/internal/agent/navigation.go" if go_code else "src/agent/config/skills/test/SKILL.md"
    before = "package agent\nfunc retries() int { return 0 }\n" if go_code else "Observe first.\n"
    after = before.replace("return 0", "return 1") if go_code else "Observe and verify first.\n"
    target = repo / path
    target.parent.mkdir(parents=True)
    target.write_text(before)
    local_git("add", path)
    local_git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
    base = local_git("rev-parse", "HEAD")
    target.write_text(after)
    patch = subprocess.check_output(["git", "diff", "--", path], cwd=repo, text=True)
    target.write_text(before)
    plan, baseline, candidate = pair()
    cluster = {"targets": plan["targets"], "source_paths": [path], "category": "verification", "cause": "Verify actions", "evidence": ["trace step 2"]}
    out = tmp_path / "artifact"
    out.mkdir()
    (out / "candidate.patch").write_text(patch)
    comparison = policy.compare(plan, baseline, candidate)
    manifest = {"base_sha": base, "base_branch": "main", "execution_ref": "refs/heads/main", "source_sha": base, "source": "42-1", "configuration": {}, "avd_fixture": "",
                "attempts": [{"status": "ready", "title": "Verify actions", "cluster": cluster, "signature": policy.signature(cluster),
                              "patch": "candidate.patch", "patch_sha256": policy.digest(patch), "paths": [path],
                              "check_passed": True, "checks": policy.verification_commands([path]),
                              "audit": {"supported": True}, "plan": plan, "comparison": comparison}]}
    for name, value in (("manifest", manifest), ("baseline", baseline), ("candidate", candidate), ("verification-review", {"supported": True})):
        (out / f"{name}.json").write_text(json.dumps(value))
    calls = []
    class API:
        repository = "org/repo"
        def issues(self): return []
        def pages(self, *args): return []
        def request(self, endpoint, payload=None):
            calls.append((endpoint, payload))
            if endpoint == "git/ref/heads/main": return {"object": {"sha": base}}
            if endpoint.startswith("git/commits/"): return {"tree": {"sha": "old-tree"}}
            if endpoint == "git/blobs":
                assert payload["content"] == after
                return {"sha": "blob"}
            if endpoint == "git/trees": return {"sha": "tree"}
            if endpoint == "git/commits": return {"sha": "commit"}
            if endpoint == "git/refs":
                assert payload["ref"].startswith("refs/heads/auto-improve/")
                return {}
            if endpoint == "pulls":
                assert payload["draft"] is True and payload["base"] == "main"
                return {"number": 1, "html_url": "https://github.com/org/repo/pull/1"}
            if endpoint == "issues/1/labels": return {}
            pytest.fail(endpoint)
    assert control.publish(API(), repo, out, "main") == ["https://github.com/org/repo/pull/1"]
    assert target.read_text() == before
    if go_code:
        manifest["attempts"][0]["checks"] = [["make", "check"]]
        (out / "manifest.json").write_text(json.dumps(manifest))
        calls.clear()
        with pytest.raises(ValueError, match="checks required"):
            control.publish(API(), repo, out, "main")
        assert all(payload is None for _, payload in calls)
    manifest["execution_ref"] = "refs/heads/auto-improve-test/improve/first"
    (out / "manifest.json").write_text(json.dumps(manifest))
    calls.clear()
    with pytest.raises(ValueError, match="Branch-test"):
        control.publish(API(), repo, out, "main")
    assert not calls

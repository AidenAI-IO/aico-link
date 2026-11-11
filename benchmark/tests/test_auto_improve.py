"""Behavioral contracts for unattended improvements, using synthetic evidence only."""
from __future__ import annotations

import base64
from datetime import datetime, timedelta, timezone
import io
import json
from pathlib import Path
import stat
import subprocess
import time
import zipfile

import pytest

from ci.auto_improve import control, driver, emulator, evidence, execution, github, policy, review


AUDIT_OUTPUT = {"verdict": "confirmed", "reason": "Fresh trace shows the same failure", "title": "Saves one-off plans",
                "summary": "The agent stores a one-off appointment as long-term memory.",
                "impact": "Memory fills with transient facts.", "reproduction": "Run the task once.",
                "expected": "No memory is saved.", "actual": "A fact memory is saved.",
                "root_cause": "The admission rule accepts dated plans.", "code_locations": ["src/agent/memory.go:12"],
                "evidence": ["attempts/x/tasks/t/trace.json"], "acceptance_criteria": ["The task passes 2/3 attempts"],
                "fix_direction": ""}


def row(repeat, success=True, **metrics):
    return {"repeat": repeat, "status": "passed" if success else "failed", "metrics": {
        "agent_eligible": True, "success": success, "tool_calls": 3, "total_tokens": 100, **metrics}}


def test_window_keeps_each_days_outcome_and_exact_task_definition(tmp_path):
    for number, status in ((41, "failed"), (42, "passed")):
        run = tmp_path / f"benchmark/runs/ci-{number}-1-case"
        run.mkdir(parents=True)
        (run / "manifest.json").write_text(json.dumps({"git_sha": str(number)}))
        (run / "suite.json").write_text(json.dumps({"tasks": [{"id": "t", "rubric": [str(number)]}]}))
        (run / "results.jsonl").write_text(json.dumps({"task_id": "t", "status": status}) + "\n")
    failures, warnings = evidence.source_failures_window(tmp_path, [("41", "1"), ("42", "1")], {})
    assert not warnings and len(failures) == 1
    f = failures[0]
    assert (f["failure_count"], f["run_count"]) == (1, 2)
    assert [(o["source"], o["status"], o["task"]["rubric"]) for o in f["observations"]] == [
        ("41-1", "failed", ["41"]), ("42-1", "passed", ["42"])]


def test_tool_error_symptom_matches_only_failed_tool_results():
    symptom = {"kind": "tool_error", "pattern": "socket"}
    ok = {"content": json.dumps({"ok": True, "text": "socket ready"})}
    failed = {"content": json.dumps({"ok": False, "error": "socket missing"})}
    assert policy.symptom_matches(symptom, {}, [{"tool_results": [ok]}]) == []
    assert policy.symptom_matches(symptom, {}, [{"tool_results": [ok, failed]}]) == [failed["content"]]


def test_source_selection_delays_and_deduplicates():
    now = datetime(2026, 1, 20, tzinfo=timezone.utc)
    def run(number, hours, **extra):
        return {"id": number, "run_attempt": 1, "status": "completed", "event": "schedule", "head_branch": "main",
                "conclusion": "success", "created_at": (now - timedelta(hours=hours + 1)).isoformat(),
                "updated_at": (now - timedelta(hours=hours)).isoformat(), **extra}
    runs = [run(1, 4), run(2, 2), run(3, 6, event="pull_request"), run(4, 6, head_branch="fork"), run(5, 24 * 15), run(6, 5)]
    selected, skipped = github.choose_window(runs, set(), branch="main", now=now)
    assert [r["id"] for r in selected] == [6, 1] and not skipped
    assert github.choose_window(runs, {"1-1", "6-1"}, branch="main", now=now) == ([], [])
    assert github.choose_window(runs, set(), branch="main", now=now, requested=("4",)) == ([], [])
    assert github.choose_window(runs, {"1-1"}, branch="main", now=now, requested=("1",))[0][0]["id"] == 1
    # Older late-finishing runs must not be lost behind a newer processed run.
    assert github.choose_window(runs, {"1-1"}, branch="main", now=now)[0][0]["id"] == 6
    with pytest.raises(ValueError, match="budget"):
        github.choose_window(runs, set(), branch="main", now=now, limit=1)


@pytest.mark.parametrize("branch", ["release", "develop", "refs/heads/main", ""])
def test_non_main_discovery_and_publication_are_rejected_before_side_effects(tmp_path, branch):
    now = datetime.now(timezone.utc)
    with pytest.raises(ValueError, match="only supports main"):
        github.choose_window([], set(), branch=branch, now=now)
    with pytest.raises(ValueError, match="only supports main"):
        control.discover(None, tmp_path / "source", "42", branch)
    with pytest.raises(ValueError, match="only supports main"):
        control.publish(None, tmp_path, branch)
    assert not (tmp_path / "source").exists()


@pytest.mark.parametrize("ref,status,expected", [
    ("refs/heads/main", "completed", "auto-improve-source-"),
    ("refs/heads/main", "diagnosed", "auto-improve-retry-"),
    ("refs/heads/main", "blocked", "auto-improve-retry-"),
    ("refs/heads/main", "error", "auto-improve-retry-"),
    ("refs/heads/main", "", "auto-improve-retry-"),
    ("refs/heads/auto-improve-test/improve/x", "completed", "auto-improve-test-source-"),
])
def test_only_settled_main_runs_mark_sources_processed(ref, status, expected):
    assert policy.artifact_name(ref, "41-1__42-1", status) == expected + "41-1__42-1"


@pytest.mark.parametrize("manifest,expected", [({"status": "completed"}, "auto-improve-source-"),
                                               ({"status": "blocked"}, "auto-improve-retry-"),
                                               (None, "auto-improve-retry-")])
def test_artifact_command_names_by_settled_status(tmp_path, monkeypatch, manifest, expected):
    if manifest is not None:
        (tmp_path / "manifest.json").write_text(json.dumps(manifest))
    output = tmp_path / "output"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setattr("sys.argv", ["control", "artifact", "--ref", "refs/heads/main",
                                    "--identity", "41-1__42-1", "--out", str(tmp_path)])
    assert control.cli() == 0
    assert output.read_text() == f"name={expected}41-1__42-1\n"


def test_workflow_policy_command_needs_no_github_credentials(tmp_path, monkeypatch):
    monkeypatch.delenv("GH_TOKEN", raising=False)
    monkeypatch.delenv("GITHUB_REPOSITORY", raising=False)
    output = tmp_path / "output"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setattr("sys.argv", ["control", "policy", "--event", "push", "--ref",
                                    "refs/heads/auto-improve-test/diagnose/first"])
    assert control.cli() == 0
    values = dict(line.split("=", 1) for line in output.read_text().splitlines())
    assert values == {"publish": "false"}


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
                               "structured_output": AUDIT_OUTPUT}), None
    def popen(command, **kwargs):
        captured.update(command=command, **kwargs)
        return Process()
    monkeypatch.setattr(review.subprocess, "Popen", popen)
    assert review.ask(tmp_path, "audit", review.ISSUE_AUDIT)["verdict"] == "confirmed"
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
        review.ask(tmp_path, "audit", review.ISSUE_AUDIT)
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
    assert failures[0]["observations"][0]["task"]["rubric"] == ["original rule"]
    assert failures[0]["observations"][0]["current_task"] is None


def test_benchmark_environment_excludes_review_and_github_secrets(monkeypatch):
    monkeypatch.setenv("GH_TOKEN", "secret")
    monkeypatch.setenv("AUTO_IMPROVE_REVIEW_API_KEY", "secret")
    monkeypatch.setenv("BENCHMARK_AGENT_API_KEY", "benchmark-only")
    env = execution.benchmark_environment()
    assert "GH_TOKEN" not in env and "AUTO_IMPROVE_REVIEW_API_KEY" not in env
    assert env["BENCHMARK_AGENT_API_KEY"] == "benchmark-only"


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


def test_raw_artifact_paths_cannot_escape_run(tmp_path):
    run = tmp_path / "benchmark/runs/ci-42-1-a"
    run.mkdir(parents=True)
    (run / "manifest.json").write_text('{}')
    (run / "results.jsonl").write_text(json.dumps({"task_id": "../../other", "status": "failed", "attempt": 1}))
    failures, warnings = evidence.source_failures(tmp_path, "42", "1", {})
    assert not failures
    assert warnings


def test_orphan_cleanup_refuses_reused_pid(tmp_path, monkeypatch):
    owned = tmp_path / "job/result/finding-1/reproduction-runs/task/1/emulator"
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


def test_judge_errors_are_rejudged_without_rerunning_the_agent(tmp_path, monkeypatch):
    import time
    import runner.rejudge as rejudge
    run = tmp_path / "run"
    run.mkdir()
    executed = {"task_id": "t", "attempt": 1, "status": "judge_error", "final": "agent output",
                "metrics": {"agent_eligible": False, "judge_error": "connection reset"}}
    (run / "results.jsonl").write_text(json.dumps(executed) + "\n")
    outcomes = iter(["judge_error", "passed"])
    calls = []
    def fake_rejudge(path, model, base_url, *, judge_api_key_env, only_judge_errors):
        calls.append((model, judge_api_key_env, only_judge_errors))
        row = json.loads((path / "results.jsonl").read_text())
        row["status"] = next(outcomes)
        (path / "results.rejudged.jsonl").write_text(json.dumps(row) + "\n")
    monkeypatch.setenv("BENCHMARK_JUDGE_MODEL", "same-judge")
    monkeypatch.setattr(rejudge, "rejudge_run", fake_rejudge)
    monkeypatch.setattr(execution.time, "sleep", lambda _: None)
    execution.rejudge_transient_errors(run, deadline=time.monotonic() + 3600, delays=(0, 0, 0))
    assert calls == [("same-judge", "BENCHMARK_JUDGE_API_KEY", True)] * 2
    result = json.loads((run / "results.jsonl").read_text())
    assert result["status"] == "passed" and result["final"] == "agent output"
    assert json.loads((run / "results.original.jsonl").read_text()) == executed
    # Too little time left keeps the judge_error row, which the comparison gate rejects.
    (run / "results.jsonl").write_text(json.dumps(executed) + "\n")
    calls.clear()
    execution.rejudge_transient_errors(run, deadline=time.monotonic() + 10, delays=(0,))
    assert not calls and json.loads((run / "results.jsonl").read_text())["status"] == "judge_error"


@pytest.mark.parametrize("failure", [None, "timeout", "cancel_during_rejudge"])
def test_run_task_rejudges_only_completed_runs_and_propagates_cancellation(tmp_path, monkeypatch, failure):
    from ci.plan import Catalog, SuiteCase
    root = tmp_path / "root"
    run_dir = root / "benchmark/runs"
    monkeypatch.setattr(execution, "load_catalog", lambda **_: Catalog((SuiteCase("case", "suite.json", "isolated"),)))
    def fake_command(*args, **kwargs):
        run = next(iter(run_dir.glob("*")), None) or run_dir / args[0][4]
        run.mkdir(parents=True, exist_ok=True)
        (run / "results.jsonl").write_text(json.dumps({"task_id": "t", "status": "judge_error"}) + "\n")
        if failure == "timeout":
            raise TimeoutError("runner timed out")
    rejudged = []
    def fake_rejudge(run, **kwargs):
        rejudged.append(run)
        if failure == "cancel_during_rejudge":
            raise InterruptedError("Workflow interrupted by signal 15")
    monkeypatch.setattr(execution, "command", fake_command)
    monkeypatch.setattr(execution, "rejudge_transient_errors", fake_rejudge)
    out = tmp_path / "out"
    expected = {None: None, "timeout": TimeoutError, "cancel_during_rejudge": InterruptedError}[failure]
    if expected:
        with pytest.raises(expected):
            execution.run_task(root, "case:t", 1, "image", out, deadline=time.monotonic() + 3600)
    else:
        assert execution.run_task(root, "case:t", 1, "image", out, deadline=time.monotonic() + 3600)["task_id"] == "t"
    # A timed-out run is retained as evidence but never waits for a rejudge.
    assert len(rejudged) == (0 if failure == "timeout" else 1)
    assert (out / "evidence/results.jsonl").is_file() and not (out / "rejudge-error.log").exists()


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


@pytest.mark.parametrize("change", [{"verdict": "likely"}, {"code_locations": "src/a.go"}, {"extra": True}])
def test_review_contract_rejects_values_outside_the_schema(change):
    review.validate_output(AUDIT_OUTPUT, review.ISSUE_AUDIT)
    with pytest.raises(ValueError):
        review.validate_output({**AUDIT_OUTPUT, **change}, review.ISSUE_AUDIT)


@pytest.mark.parametrize("nested", [False, True])
@pytest.mark.parametrize("branch_test", [False, True])
def test_discovery_normalizes_artifact_root(tmp_path, monkeypatch, nested, branch_test):
    monkeypatch.setenv("AUTO_IMPROVE_TEST_RUN", str(branch_test).lower())
    class API:
        def discover(self, *args, **kwargs):
            assert kwargs["include_processed"] is branch_test
            return {"identity": "40-1__41-1__42-1", "sources": [
                {"artifact": {"id": i}, "identity": f"{i}-1", "run": {"id": i}} for i in (40, 41, 42)]}
        def download(self, artifact, destination):
            root = destination / "benchmark" if nested else destination
            # Run 40 failed before any task ran, so its artifact holds only the dashboard.
            if artifact["id"] != 40:
                (root / "runs" / f"ci-{artifact['id']}-1-a").mkdir(parents=True)
            root.mkdir(parents=True, exist_ok=True)
            (root / "dashboard-data.json").write_text(json.dumps({"history": [artifact["id"]]}))
        def issues(self):
            return []
        def pages(self, *args, **kwargs):
            return []
        def auto_improve_artifacts(self, cutoff):
            return []
    out = tmp_path / "source"
    result = control.discover(API(), out, "", "main")
    assert result["identity"] == "40-1__41-1__42-1" and result["empty_artifacts"] == ["40-1"]
    assert sorted(p.name for p in (out / "benchmark/runs").iterdir()) == ["ci-41-1-a", "ci-42-1-a"]
    assert json.loads((out / "history.json").read_text()) == {"history": [42]}


def cluster(**overrides):
    base = {"title": "Saves one-off plans", "category": "memory", "cause": "Admission accepts dated plans",
            "judge_review": "supported", "fix_target": "go_code", "symptom": {"kind": "none", "pattern": ""},
            "targets": ["a:one"], "neighbors": [], "source_paths": ["src/agent/memory.go"], "evidence": ["trace step 2"]}
    return {**base, **overrides}


def observation(source, success=False, eligible=True):
    return {"source": source, "status": "passed" if success else "failed", "agent_eligible": eligible,
            "metrics": {"success": success, "agent_eligible": eligible}}


def analyze(tmp_path, monkeypatch, clusters, *, outcome="failed", audit=None, feedback=(), evaluation_issues=(),
            failing_runs=2, eligible=True, run_error=None, max_issues=None):
    """Run the driver on a synthetic two-day window with mocked model sessions and task runs."""
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    monkeypatch.setenv("GITHUB_REF", "refs/heads/main")
    if max_issues is not None:
        monkeypatch.setenv("AUTO_IMPROVE_MAX_ISSUES", str(max_issues))
    repo, source, out = tmp_path / "repo", tmp_path / "source", tmp_path / "out"
    repo.mkdir(); source.mkdir()
    (source / "source.json").write_text(json.dumps({"sources": [
        {"run": {"id": i, "run_attempt": 1, "head_sha": "s", "head_branch": "main"}} for i in (41, 42)]}))
    (source / "feedback.json").write_text(json.dumps(list(feedback)))
    keys = sorted({"a:one", *(t for c in clusters for t in c["targets"] if not t.startswith("z:"))})
    tasks = {k: {"case_id": k.split(":")[0], "environment": "isolated", "runnable": True,
                 "suite": "suites/memory_v1.json", "task": {"id": k.split(":")[1]}} for k in keys}
    failures = [{"key": k, "observations": [observation(s, eligible=eligible) for s in ("41-1", "42-1")[:failing_runs]]
                 + [observation(s, success=True) for s in ("41-1", "42-1")[failing_runs:]]} for k in keys]
    runs = []
    analyze.rejudge = []
    def run_task(root, key, repeat, image, directory, **kwargs):
        runs.append(key)
        analyze.rejudge.append(kwargs["rejudge"])
        if run_error:
            raise run_error
        trace = directory / "evidence/tasks" / key.split(":")[1] / "trace.json"
        trace.parent.mkdir(parents=True)
        trace.write_text(json.dumps({"tool_results": []}))
        status = {"failed": row(1, False), "passed": row(1), "judge_error": {"status": "judge_error", "metrics": {}}}[outcome]
        return {**status, "task_id": key.split(":")[1], "evidence": str(directory / "evidence")}
    def ask(_out, name, _repo, payload, *args, **kwargs):
        if name == "diagnosis":
            return {"summary": "Window summary", "clusters": clusters, "evaluation_issues": [
                {"task": t, "kind": "evaluator", "reason": "r", "evidence": []} for t in evaluation_issues]}
        return audit(payload) if callable(audit) else {**AUDIT_OUTPUT, **(audit or {})}
    monkeypatch.setattr(driver, "git", lambda *a: "0123456789abcdef")
    monkeypatch.setattr(driver, "tracked_files", lambda _repo: {"src/agent/memory.go", "benchmark/runner/judge.py"})
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda _repo: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures_window", lambda *a: (failures, []))
    monkeypatch.setattr(driver, "ask", ask)
    monkeypatch.setattr(driver.execution, "prepare", lambda *a, **k: None)
    monkeypatch.setattr(driver.execution, "remove_images", lambda images: None)
    monkeypatch.setattr(driver.execution, "run_task", run_task)
    state = driver.run(repo, source, out)
    drafts = json.loads((out / "issues.json").read_text()) if (out / "issues.json").is_file() else []
    return state, drafts, runs, out


def test_reproduced_and_audited_finding_becomes_one_issue_for_a_coding_agent(tmp_path, monkeypatch):
    state, drafts, runs, out = analyze(tmp_path, monkeypatch, [cluster()])
    assert state["status"] == "completed" and runs == ["a:one"] and analyze.rejudge == [True]
    assert [f["status"] for f in state["findings"]] == ["issue_ready"]
    [draft] = drafts
    assert draft["action"] == "create" and draft["labels"] == ["auto-improve", "domain:agent"]
    assert draft["title"] == "[auto-improve][agent] Saves one-off plans"
    body = draft["body"]
    meta = policy.issue_metadata({"user": {"login": "bot"}, "body": body}, {"bot"})
    assert {k: meta[k] for k in ("domain", "targets")} == {"domain": "agent", "targets": ["a:one"]}
    # The issue carries what a developer needs: command, observed impact, code and acceptance.
    assert "--suite suites/memory_v1.json --task-id one" in body
    assert "| `a:one` | isolated | 2/2 | yes |" in body
    assert "- `src/agent/memory.go:12`" in body and "The task passes 2/3 attempts" in body
    assert (out / "issues/01-create.md").is_file()


@pytest.mark.parametrize("change,status", [
    ({"cluster": {"targets": ["z:unobserved"]}}, "invalid"),
    ({"cluster": {"source_paths": ["src/agent/missing.go", "details/failure-001.json"]}}, "invalid"),
    ({"cluster": {"evidence": []}}, "invalid"),
    ({"cluster": {"judge_review": "disputed"}}, "report_only"),
    ({"cluster": {"fix_target": "unknown"}}, "report_only"),
    ({"cluster": {"fix_target": "benchmark_code"}}, "invalid"),
    ({"evaluation_issues": ["a:one"]}, "invalid"),
    ({"eligible": False}, "invalid"),
    ({"outcome": "passed"}, "not_reproduced"),
    ({"outcome": "judge_error"}, "not_reproduced"),
    ({"audit": {"verdict": "refuted"}}, "refuted"),
    ({"audit": {"verdict": "insufficient_evidence"}}, "insufficient_evidence"),
    ({"audit": {"code_locations": ["src/agent/missing.go:3", "../etc/passwd"]}}, "insufficient_evidence"),
    ({"audit": {"acceptance_criteria": []}}, "insufficient_evidence"),
])
def test_unsupported_findings_are_never_filed(tmp_path, monkeypatch, change, status):
    options = {k: v for k, v in change.items() if k != "cluster"}
    state, drafts, _, _ = analyze(tmp_path, monkeypatch, [cluster(**change.get("cluster", {}))], **options)
    assert [f["status"] for f in state["findings"]] == [status] and drafts == []
    assert state["findings"][0].get("reason")


def test_benchmark_findings_reproduce_their_symptom_without_rejudging(tmp_path, monkeypatch):
    symptom = {"kind": "judge_error", "pattern": ""}
    real = policy.reproduction
    assert real("benchmark", {"status": "judge_error", "metrics": {}}, symptom, [])["reproduced"]
    assert not real("benchmark", row(1, False), symptom, [])["reproduced"]
    seen = []
    monkeypatch.setattr(policy, "reproduction", lambda domain, row, s, traces: seen.append((domain, s)) or
                        {"reproduced": True, "evidence": ["judge_error"], "reason": "symptom observed"})
    state, drafts, _, _ = analyze(tmp_path, monkeypatch, [cluster(fix_target="benchmark_code", symptom=symptom,
                                                                  source_paths=["benchmark/runner/judge.py"])])
    assert seen == [("benchmark", symptom)] and drafts[0]["labels"] == ["auto-improve", "domain:benchmark"]
    # Rejudging would hide the judge failure the finding is about.
    assert analyze.rejudge == [False]


@pytest.mark.parametrize("failing_runs,status", [(2, "issue_ready"), (1, "not_persistent")])
def test_findings_the_runner_cannot_execute_need_repeated_daily_failures(tmp_path, monkeypatch, failing_runs, status):
    state, drafts, runs, _ = analyze(tmp_path, monkeypatch, [cluster(fix_target="external_code")],
                                     failing_runs=failing_runs)
    assert runs == [] and [f["status"] for f in state["findings"]] == [status]
    assert len(drafts) == (status == "issue_ready")


def test_new_issues_are_ranked_capped_and_deduplicated_within_a_run(tmp_path, monkeypatch):
    clusters = [cluster(title=f"Finding {i}", targets=[f"a:t{i}"]) for i in range(4)]
    clusters.append(cluster(title="Same tasks again", targets=["a:t0", "a:t1"]))
    state, drafts, _, _ = analyze(tmp_path, monkeypatch, clusters, max_issues=2)
    statuses = {f["title"]: f["status"] for f in state["findings"]}
    # The broadest reproduced finding ranks first; overlapping later findings are not duplicates.
    assert statuses["Same tasks again"] == "issue_ready"
    assert sorted(statuses.values()) == ["duplicate", "duplicate", "issue_ready", "issue_ready", "over_limit"]
    assert [d["action"] for d in drafts] == ["create", "create"]


def issue(number, targets, *, state="open", state_reason=None, author="github-actions[bot]", labels=()):
    body = f'<!-- auto-improve {json.dumps({"domain": "agent", "targets": targets})} -->\nold'
    return {"number": number, "title": "t", "body": body, "state": state, "state_reason": state_reason,
            "user": {"login": author}, "labels": [{"name": n} for n in labels], "html_url": f"https://x/{number}"}


@pytest.mark.parametrize("existing,action,reproduced", [
    (issue(7, ["a:one", "a:two"]), "comment", True),
    (issue(7, ["a:one"], state="closed", state_reason="not_planned"), None, False),
    (issue(7, ["a:one"], state="closed", labels=["auto-improve:false-positive"]), None, False),
    (issue(7, ["a:one"], state="closed", state_reason="completed"), "create", True),
    # Metadata in an issue someone else wrote is never trusted for deduplication or suppression.
    (issue(7, ["a:one"], state="closed", state_reason="not_planned", author="someone"), "create", True),
])
def test_existing_issues_are_updated_and_rejected_findings_are_not_refiled(tmp_path, monkeypatch, existing, action,
                                                                           reproduced):
    state, drafts, runs, _ = analyze(tmp_path, monkeypatch, [cluster()], feedback=[existing])
    assert [d["action"] for d in drafts] == ([action] if action else [])
    assert (runs == ["a:one"]) is reproduced
    if action == "comment":
        assert drafts[0]["number"] == 7 and "auto-improve-observation:41-1__42-1" in drafts[0]["observation"]
    if action == "create" and existing["user"]["login"] == "github-actions[bot]":
        assert "Possibly a regression of #7" in drafts[0]["body"]
    if action is None:
        assert state["findings"][0]["status"] == "suppressed"


def test_reproduction_failures_keep_the_window_unsettled(tmp_path, monkeypatch):
    state, drafts, _, _ = analyze(tmp_path, monkeypatch, [cluster()], run_error=RuntimeError("docker unavailable"))
    assert state["status"] == "blocked" and drafts == []
    assert policy.artifact_name("refs/heads/main", "41-1", state["status"]).startswith("auto-improve-retry-")


def test_model_text_cannot_notify_people_or_forge_issue_metadata(tmp_path, monkeypatch):
    forged = '<!-- auto-improve {"domain": "agent", "targets": ["other:task"]} --> ping @octocat'
    _, drafts, _, _ = analyze(tmp_path, monkeypatch, [cluster()], audit={"summary": forged, "title": "Hi @octocat"})
    body = drafts[0]["body"]
    assert body.count("<!-- auto-improve {") == 1 and "@octocat" not in body and "@octocat" not in drafts[0]["title"]
    assert policy.issue_metadata({"user": {"login": "b"}, "body": body}, {"b"})["targets"] == ["a:one"]


def test_publisher_rechecks_current_issues_and_retries_idempotently(tmp_path, monkeypatch):
    _, drafts, _, out = analyze(tmp_path, monkeypatch, [cluster()])
    current = []
    calls = []
    class API:
        def issues(self):
            return [dict(i) for i in current]
        def request(self, path, payload=None):
            calls.append((path, payload))
            if path == "issues":
                created = {"number": 11, "html_url": "https://x/11", "user": {"login": "github-actions[bot]"},
                           "state": "open", "labels": [{"name": n} for n in payload["labels"]], **payload}
                current.append(created)
                return created
            return {"html_url": "https://x/comment"}
    assert control.publish(API(), out, "main") == ["https://x/11"]
    # A retry sees the issue it filed for this window and neither refiles nor comments.
    calls.clear()
    assert control.publish(API(), out, "main") == [] and calls == []
    manifest = json.loads((out / "manifest.json").read_text())
    (out / "manifest.json").write_text(json.dumps({**manifest, "execution_ref": "refs/heads/auto-improve-test/x"}))
    with pytest.raises(ValueError, match="Branch-test"):
        control.publish(API(), out, "main")
    (out / "manifest.json").write_text(json.dumps(manifest))
    current.clear()
    (out / "issues.json").write_text(json.dumps([{**drafts[0], "key": {"domain": "agent", "targets": ["b:other"]}}]))
    with pytest.raises(ValueError, match="Malformed"):
        control.publish(API(), out, "main")

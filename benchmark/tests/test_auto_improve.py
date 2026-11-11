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


def test_remove_checkout_handles_read_only_module_cache_without_following_symlinks(tmp_path):
    checkout = tmp_path / "candidate"
    module = checkout / ".cache" / "go-mod" / "example.org" / "module@v1"
    module.mkdir(parents=True)
    (module / "go.mod").write_text("module example.org/module\n")
    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "keep").write_text("retained")
    (checkout / "external").symlink_to(outside, target_is_directory=True)
    module.chmod(0o555)
    driver.remove_checkout(checkout)
    assert not checkout.exists()
    assert (outside / "keep").read_text() == "retained"


def row(repeat, success=True, **metrics):
    return {"repeat": repeat, "status": "passed" if success else "failed", "metrics": {
        "agent_eligible": True, "success": success, "tool_calls": 3, "total_tokens": 100, **metrics}}


def pair():
    """Targets have three attempts per side; stable regression tasks were screened once."""
    plan = {"targets": ["phone:tap"], "neighbors": ["phone:type"], "held_out": ["memory:recall"]}
    baseline = {k: [row(i, k != "phone:tap") for i in range(1, 4 if k == "phone:tap" else 2)]
                for keys in plan.values() for k in keys}
    candidate = {k: [row(i) for i in range(1, len(rows) + 1)] for k, rows in baseline.items()}
    return plan, baseline, candidate


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


@pytest.mark.parametrize("failed_eligible", [False, True])
def test_passing_days_do_not_make_a_failure_agent_eligible(tmp_path, monkeypatch, failed_eligible):
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    source = tmp_path / "source"
    source.mkdir()
    (source / "source.json").write_text(json.dumps({"sources": [
        {"run": {"id": i, "run_attempt": 1, "head_sha": "s", "head_branch": "main"}} for i in (41, 42)]}))
    failed = {"status": "failed" if failed_eligible else "judge_error",
              "metrics": {"agent_eligible": failed_eligible}}
    observations = [{**failed, "source": "41-1"}, {"status": "passed", "metrics": {}, "source": "42-1"}]
    monkeypatch.setattr(driver, "git", lambda *a: "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: {})
    monkeypatch.setattr(driver.evidence, "source_failures_window", lambda *a: (
        [{"key": "a:t", "observations": [dict(o, agent_eligible=driver.status_fields(o)[2]) for o in observations]}], []))
    seen = {}
    monkeypatch.setattr(driver, "ask", lambda _o, _n, _r, payload, *a, **k: seen.update(payload) or {
        "summary": "s", "clusters": []})
    driver.run(tmp_path, source, tmp_path / "out", mode="diagnose")
    assert seen["failures"][0]["agent_eligible"] is failed_eligible


@pytest.mark.parametrize("path,domain,allowed", [
    ("benchmark/runner/judge.py", "benchmark", True),
    ("benchmark/suites/example.json", "benchmark", True),
    ("src/agent/cmd/benchmark-mcu/main.go", "benchmark", True),
    ("src/agent/internal/agent/main.go", "benchmark", False),
    ("benchmark/ci/auto_improve/policy.py", "benchmark", False),
    ("benchmark/tests/test_example.py", "benchmark", False),
    ("benchmark/mobilegym/vendor/main.py", "benchmark", False),
    ("benchmark/vphone/local.env", "benchmark", False),
    (".github/workflows/benchmark.yml", "benchmark", False),
])
def test_repair_domains_cannot_change_each_other_or_their_acceptance_gate(path, domain, allowed):
    assert policy.edit_target(path, domain) is allowed


@pytest.mark.parametrize("mutation", [None, "not_reproduced", "remaining", "missing", "new_error", "scores_drop",
                                      "reworded"])
def test_benchmark_acceptance_measures_symptom_not_pass_rate(mutation):
    plan, baseline, candidate = pair()
    symptom = {"kind": "tool_error", "pattern": "socket missing"}
    for side in (baseline, candidate):
        for key, rows in side.items():
            for r in rows:
                failing = side is baseline and key == "phone:tap"
                r["symptom_evidence"] = ["socket missing"] if failing else []
                r["tool_failures"] = int(failing)
    if mutation == "not_reproduced":
        baseline["phone:tap"][0]["symptom_evidence"] = []
        baseline["phone:tap"][1]["symptom_evidence"] = []
    elif mutation == "remaining":
        candidate["phone:tap"][0]["symptom_evidence"] = ["socket missing"]
    elif mutation == "missing":
        candidate["phone:tap"].pop()
    elif mutation == "new_error":
        candidate["phone:type"][0].update(status="judge_error", metrics={"agent_eligible": False})
    elif mutation == "scores_drop":
        for rows in candidate.values():
            for r in rows:
                r.update(status="failed", metrics={"success": False, "agent_eligible": True})
    elif mutation == "reworded":
        # Changing the error text hides the pattern but leaves the tool failing.
        for r in candidate["phone:tap"]:
            r["tool_failures"] = 1
    verdict = policy.compare_symptom(plan, symptom, baseline, candidate, [])
    assert verdict["passed"] is (mutation in {None, "scores_drop"})


def test_contract_changes_flag_task_removal_and_scoring_changes():
    before = {"tasks": [{"id": "removed", "rubric": []}, {"id": "kept", "expected_answer": "old"}]}
    after = {"tasks": [{"id": "kept", "expected_answer": "new"}]}
    changes = policy.contract_changes("benchmark/suites/example.json", json.dumps(before), json.dumps(after))
    assert any("removed task removed" in c for c in changes)
    assert any("changed expected_answer" in c for c in changes)
    # Code that records the measured fields could hide a symptom without fixing it.
    assert policy.contract_changes("benchmark/runner/main.py", "a = 1\n", "a = 2\n")
    assert not policy.contract_changes("benchmark/runner/environment.py", "a = 1\n", "a = 2\n")


def test_tool_error_symptom_matches_only_failed_tool_results():
    symptom = {"kind": "tool_error", "pattern": "socket"}
    ok = {"content": json.dumps({"ok": True, "text": "socket ready"})}
    failed = {"content": json.dumps({"ok": False, "error": "socket missing"})}
    assert policy.symptom_matches(symptom, {}, [{"tool_results": [ok]}]) == []
    assert policy.symptom_matches(symptom, {}, [{"tool_results": [ok, failed]}]) == [failed["content"]]


def test_gate_requires_complete_paired_evidence():
    plan, baseline, candidate = pair()
    assert policy.compare(plan, baseline, candidate)["passed"]
    candidate["phone:tap"] = candidate["phone:tap"][:2]
    result = policy.compare(plan, baseline, candidate)
    assert not result["passed"]
    assert any("incomplete" in reason for reason in result["reasons"])


def test_regression_tasks_are_screened_once_and_rerun_only_when_worse():
    plan, baseline, candidate = pair()
    plan["held_out"] = [f"memory:{i}" for i in range(20)]
    for key in plan["held_out"]:
        baseline[key], candidate[key] = [row(1)], [row(1)]
    assert policy.compare(plan, baseline, candidate)["passed"]
    # A screening drop must be rerun; an unrerun drop is never accepted.
    candidate["memory:0"] = [row(1, False)]
    assert policy.needs_rerun(baseline["memory:0"], candidate["memory:0"])
    assert any("not rerun" in r for r in policy.compare(plan, baseline, candidate)["reasons"])
    # Rerun to three: one lost pass is ordinary variance, two is a regression.
    baseline["memory:0"] = [row(i) for i in range(1, 4)]
    candidate["memory:0"] = [row(1, False), row(2), row(3)]
    assert policy.compare(plan, baseline, candidate)["passed"]
    candidate["memory:0"][1] = row(2, False)
    assert any("observed regression" in r for r in policy.compare(plan, baseline, candidate)["reasons"])
    # Many single-pass losses together are a regression even though no task lost two.
    candidate["memory:0"][1] = row(2)
    for key in plan["held_out"][1:3]:
        baseline[key] = [row(i) for i in range(1, 4)]
        candidate[key] = [row(1, False), row(2), row(3)]
    assert any("regression set" in r for r in policy.compare(plan, baseline, candidate)["reasons"])


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
        baseline["phone:type"] = [row(i) for i in range(1, 4)]
        candidate["phone:type"] = [row(1, False), row(2, False), row(3)]
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
    "src/agent/cmd/benchmark-mcu/main.go", "src/agent/internal/agent/benchmark_mock_tools.go",
    ".github/workflows/ci.yml", "scripts/run_tests_in_docker.sh"])
def test_edit_scope_preserves_evaluators_tests_dependencies_and_ci(path):
    assert not policy.edit_target(path)


def test_agent_changes_cannot_special_case_the_benchmark():
    path = "src/agent/internal/agent/memory.go"
    before = "package agent\nfunc save() bool { return false }\n"
    with pytest.raises(policy.ProposalError, match="benchmark-specific"):
        policy.validate_change(path, before, before.replace("return false", "return cfg.Benchmark.Token != \"\""))
    policy.validate_change(path, before, before.replace("false", "true"))


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


@pytest.mark.parametrize("ref,status,expected", [
    ("refs/heads/main", "completed", "auto-improve-source-"),
    ("refs/heads/main", "diagnosed", "auto-improve-source-"),
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
                                    "refs/heads/auto-improve-test/diagnose/first", "--mode", "improve"])
    assert control.cli() == 0
    values = dict(line.split("=", 1) for line in output.read_text().splitlines())
    assert values == {"value": "diagnose", "publish": "false"}


def git_fixture(*args, cwd):
    subprocess.run(["git", "-c", "protocol.file.allow=always", *args], cwd=cwd, check=True,
                   capture_output=True, text=True)


def test_checkout_reuses_local_submodule_without_its_remote(tmp_path):
    import time
    module = tmp_path / "module"
    module.mkdir()
    git_fixture("init", cwd=module)
    (module / "marker.txt").write_text("pinned\n")
    git_fixture("add", "marker.txt", cwd=module)
    git_fixture("-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "module", cwd=module)
    repo = tmp_path / "repo"
    repo.mkdir()
    git_fixture("init", cwd=repo)
    git_fixture("submodule", "add", str(module), driver.MOBILEGYM_SUBMODULE_PATH, cwd=repo)
    # The isolated checkout must not contact this unreachable remote.
    git_fixture("config", "-f", ".gitmodules", f"submodule.{driver.MOBILEGYM_SUBMODULE_PATH}.url",
                "https://example.invalid/mobilegym.git", cwd=repo)
    git_fixture("add", ".gitmodules", cwd=repo)
    git_fixture("-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "base", cwd=repo)
    base = driver.git(repo, "rev-parse", "HEAD")
    destination = tmp_path / "work/candidate"
    destination.parent.mkdir()
    driver.checkout(repo, destination, base, deadline=time.monotonic() + 120)
    assert (destination / driver.MOBILEGYM_SUBMODULE_PATH / "marker.txt").read_text() == "pinned\n"
    assert driver.git(destination / driver.MOBILEGYM_SUBMODULE_PATH, "rev-parse", "HEAD") == driver.git(
        repo, "rev-parse", f"HEAD:{driver.MOBILEGYM_SUBMODULE_PATH}")
    assert (tmp_path / "work/candidate-submodule.log").is_file()
    with pytest.raises(TimeoutError):
        driver.checkout(repo, tmp_path / "work/late", base, deadline=time.monotonic() - 1)
    assert not (tmp_path / "work/late").exists()


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


def test_unrunnable_clusters_do_not_consume_repair_slots(tmp_path, monkeypatch):
    """Unrunnable or invalid clusters are skipped before checkout and never block the run."""
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    monkeypatch.delenv("AUTO_IMPROVE_AVD_TEMPLATE", raising=False)
    monkeypatch.setenv("AUTO_IMPROVE_MAX_TASKS", "5")
    repo, source, out = tmp_path / "repo", tmp_path / "source", tmp_path / "out"
    repo.mkdir(); source.mkdir()
    (source / "source.json").write_text(json.dumps({"run": {"id": 42, "run_attempt": 1, "head_sha": "old", "head_branch": "main"}}))
    tasks = {f"big:{i}": {"case_id": "big", "environment": "isolated", "runnable": True,
                          "task": {"category": "memory"}} for i in range(8)}
    tasks.update({"phone:type": {"case_id": "phone", "environment": "adb", "runnable": True, "task": {}},
                  "other:held": {"case_id": "other", "environment": "isolated", "runnable": True, "task": {}}})
    clusters = [{"title": title, "category": "task", "cause": "cause", "judge_review": "supported",
                 "fix_target": "go_code", "targets": targets, "neighbors": [], "source_paths": ["x.go"],
                 "evidence": ["trace"]} for title, targets in (("phone", ["phone:type"]), ("big", ["big:0"]),
                                                               ("unobserved", ["other:held"]))]
    clusters.append({**clusters[1], "title": "no symptom", "fix_target": "benchmark_code",
                     "symptom": {"kind": "none", "pattern": ""}})
    seen = {}
    monkeypatch.setattr(driver, "git", lambda *a: "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures", lambda *a: (
        [{"key": k, "agent_eligible": True} for k in ("phone:type", "big:0")], []))
    def fake_ask(_out, name, _repo, payload, *args, **kwargs):
        seen[name] = payload
        return {"summary": "two clusters", "clusters": clusters}
    monkeypatch.setattr(driver, "ask", fake_ask)
    monkeypatch.setattr(driver, "checkout", lambda *a, **k: pytest.fail("must not prepare a checkout"))
    state = driver.run(repo, source, out, mode="improve")
    assert seen["diagnosis"]["repairable_environments"] == ["isolated", "mobilegym"]
    assert [a["status"] for a in state["attempts"]] == ["environment_unavailable", "deferred",
                                                        "invalid_proposal", "invalid_proposal"]
    assert "adb" in state["attempts"][0]["reason"] and "budget" in state["attempts"][1]["reason"]
    assert state["status"] == "completed" and not list(out.glob("cluster-*"))


def test_evaluator_attributed_failures_are_regression_checks_not_targets(tmp_path, monkeypatch):
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    repo, source, out = tmp_path / "repo", tmp_path / "source", tmp_path / "out"
    repo.mkdir(); source.mkdir()
    (source / "source.json").write_text(json.dumps({"run": {"id": 42, "run_attempt": 1, "head_sha": "old", "head_branch": "main"}}))
    tasks = {k: {"case_id": k.split(":")[0], "environment": "isolated", "runnable": True, "task": {"category": k}}
             for k in ("a:agent", "a:neighbor", "b:graded", "b:sibling", "c:held")}
    tasks["a:neighbor"]["task"]["category"] = "a:agent"
    tasks["b:sibling"]["task"]["category"] = "b:graded"
    base = {"category": "task", "cause": "cause", "judge_review": "supported", "fix_target": "go_code",
            "neighbors": [], "source_paths": ["x.go"], "evidence": ["trace"]}
    diagnosis = {"summary": "s", "clusters": [{**base, "title": "mixed", "targets": ["a:agent", "b:graded"]},
                                             {**base, "title": "graded only", "targets": ["b:graded"]}],
                 "evaluation_issues": [{"task": "b:graded", "kind": "evaluator", "reason": "scores last turn only",
                                        "evidence": ["trace"]}]}
    monkeypatch.setattr(driver, "git", lambda *a: "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures", lambda *a: (
        [{"key": k, "agent_eligible": True} for k in ("a:agent", "b:graded")], []))
    monkeypatch.setattr(driver, "ask", lambda *a, **k: diagnosis)
    monkeypatch.setattr(driver, "validated_source_paths", lambda *a: ["x.go"])
    monkeypatch.setattr(driver, "checkout", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("stop after planning")))
    state = driver.run(repo, source, out, mode="improve")
    mixed, graded = state["attempts"]
    assert mixed["quarantined_targets"] == ["b:graded"]
    plan = json.loads((out / "cluster-1/plan.json").read_text())
    # The other suite contributes only the quarantined task itself, not its suite neighbors.
    assert plan["targets"] == ["a:agent"] and plan["neighbors"] == ["a:neighbor", "b:graded"]
    assert graded["status"] == "evaluation_issue" and not (out / "cluster-2").exists()


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
    assert failures[0]["observations"][0]["task"]["rubric"] == ["original rule"]
    assert failures[0]["observations"][0]["current_task"] is None


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
                 "task": {"category": "task"}} for k in ("a:target", "a:neighbor", "b:held", "b:graded")}
    cluster = {"title": "Verify the screen", "category": "verification", "cause": "missing observation",
               "judge_review": "supported", "fix_target": "go_code" if go_code else "skill", "targets": ["a:target"],
               "neighbors": [], "source_paths": [path], "evidence": ["trace step 2"]}
    monkeypatch.setattr(driver, "git", lambda _root, *a: "" if a[0] == "status" else "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures", lambda *a: ([{"key": "a:target", "agent_eligible": True}], []))
    events = []
    def fake_ask(_out, name, *args, **kwargs):
        events.append(name)
        if name == "diagnosis":
            # The second cluster also names a task the diagnosis attributes to the environment.
            return {"summary": "One observed failure",
                    "clusters": [cluster, {**cluster, "title": "Second issue", "targets": ["a:target", "b:graded"]}],
                    "evaluation_issues": [{"task": "b:graded", "kind": "environment", "reason": "socket missing",
                                           "evidence": ["trace"]}]}
        if name == "reproduction-review":
            return {"summary": "Reproduced", "tasks": [{"task": "a:target", "disposition": "agent_failure",
                                                          "reason": "Failed deterministic assertion", "evidence": ["fresh trace"]}]}
        return {"summary": "Verify first", "edits": [{"path": path, "old": before, "new": after}]}
    checkouts = []
    def fake_checkout(_, dest, base, *, deadline):
        checkouts.append(dest.relative_to(out).as_posix())
        (dest / path).parent.mkdir(parents=True)
        (dest / path).write_text(original.read_text())
    def fake_run(root, key, repeat, *args, **kwargs):
        events.append(f"run:{root.name}:{key}")
        return row(repeat, root.name == "candidate" or key != "a:target")
    monkeypatch.setattr(driver, "ask", fake_ask)
    monkeypatch.setattr(driver, "checkout", fake_checkout)
    audits = []
    def fake_audit(_out, _name, _repo, payload, _deadline):
        audits.append(payload)
        return {"supported": True, "reason": "checked", "evidence": ["trace"]}
    monkeypatch.setattr(driver, "audit", fake_audit)
    monkeypatch.setattr(driver.execution, "prepare", lambda *a: None)
    removed_images = []
    monkeypatch.setattr(driver.execution, "remove_images", lambda images: removed_images.append(images))
    monkeypatch.setattr(driver.execution, "run_task", fake_run)
    checks = []
    def fake_check(command, **kwargs):
        assert (kwargs["cwd"] / path).read_text() == after
        checks.append(command)
        events.append("check")
    monkeypatch.setattr(driver.execution, "command", fake_check)
    monkeypatch.setattr(driver.execution, "resource_snapshot", lambda *a: None)
    monkeypatch.setattr(driver.subprocess, "check_output", lambda *a, **kw: "diff\n")
    result = driver.run(repo, source, out, mode="improve")
    assert [attempt["status"] for attempt in result["attempts"]] == ["ready", "ready"]
    assert checkouts == ["baseline", "cluster-1/candidate", "cluster-2/candidate"]
    assert checks == policy.verification_commands([path]) * 2
    assert result["attempts"][0]["checks"] == policy.verification_commands([path])
    assert result["execution_ref"] == ref
    # The target is checked first; repository checks gate the regression set.
    assert events.index("run:baseline:a:target") < events.index("fix") < events.index("run:candidate:a:target")
    assert events.index("run:candidate:a:target") < events.index("check") < events.index("run:candidate:a:neighbor")
    # Stable regression tasks are screened once per side; targets use three attempts.
    assert events.count("run:candidate:a:neighbor") == events.count("run:baseline:a:neighbor") == 2
    assert events.count("run:candidate:a:target") == 6
    assert not (out / "cluster-1/candidate").exists()
    assert not (out / "baseline").exists()
    assert (out / "cluster-1/candidate.patch").read_text() == "diff\n"
    # The verification reviewer audits the exact change under test.
    assert audits and all(payload["candidate_diff"] == "diff\n" for payload in audits)
    # Quarantine rewrites the stored cluster, so the publisher's recomputed identity matches.
    quarantined = result["attempts"][1]
    assert quarantined["cluster"]["targets"] == ["a:target"]
    assert quarantined["diagnosed_targets"] == ["a:target", "b:graded"]
    assert all(policy.signature(a["cluster"]) == a["signature"] for a in result["attempts"])
    # Known evaluation issues follow the task into any plan that contains it.
    assert all([i["task"] for i in a["known_evaluation_issues"]] == ["b:graded"] for a in audits)
    assert removed_images == [[f"aico-auto-improve:{policy.digest('42-1')[:20]}-{n}-base", f"aico-auto-improve:{policy.digest('42-1')[:20]}-{n}-candidate"]
                              for n in (1, 2)]


FAILED_TOOL = {"is_error": True, "content": "socket missing"}


def test_driver_stops_before_checks_when_the_patch_does_not_fix_a_target(tmp_path, monkeypatch):
    for kind in ("AGENT", "JUDGE"):
        for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY"):
            monkeypatch.setenv(f"BENCHMARK_{kind}_{field}", "synthetic")
    repo, source, out = tmp_path / "repo", tmp_path / "source", tmp_path / "out"
    repo.mkdir(); source.mkdir()
    path = "src/agent/config/skills/test/SKILL.md"
    (repo / path).parent.mkdir(parents=True)
    (repo / path).write_text("Observe.\n")
    (source / "source.json").write_text(json.dumps({"run": {"id": 42, "run_attempt": 1, "head_sha": "s", "head_branch": "main"}}))
    tasks = {k: {"case_id": k.split(":")[0], "environment": "isolated", "runnable": True, "task": {"category": "c"}}
             for k in ("a:target", "a:neighbor", "b:held")}
    cluster = {"title": "t", "category": "c", "cause": "c", "judge_review": "supported", "fix_target": "skill",
               "targets": ["a:target"], "neighbors": [], "source_paths": [path], "evidence": ["e"]}
    def fake_ask(_out, name, *args, **kwargs):
        if name == "diagnosis":
            return {"summary": "s", "clusters": [cluster]}
        if name == "reproduction-review":
            return {"summary": "s", "tasks": [{"task": "a:target", "disposition": "agent_failure",
                                               "reason": "r", "evidence": ["e"]}]}
        return {"summary": "s", "edits": [{"path": path, "old": "Observe.", "new": "Observe twice."}]}
    def fake_checkout(_, dest, base, *, deadline):
        (dest / path).parent.mkdir(parents=True)
        (dest / path).write_text("Observe.\n")
    runs = []
    monkeypatch.setattr(driver, "git", lambda _root, *a: "" if a[0] == "status" else "base")
    monkeypatch.setattr(driver.evidence, "catalog_tasks", lambda *_: tasks)
    monkeypatch.setattr(driver.evidence, "source_failures", lambda *a: ([{"key": "a:target", "agent_eligible": True}], []))
    monkeypatch.setattr(driver, "ask", fake_ask)
    monkeypatch.setattr(driver, "checkout", fake_checkout)
    monkeypatch.setattr(driver.execution, "prepare", lambda *a: None)
    monkeypatch.setattr(driver.execution, "remove_images", lambda images: None)
    monkeypatch.setattr(driver.execution, "run_task", lambda root, key, repeat, *a, **k: runs.append((root.name, key)) or row(repeat, False))
    monkeypatch.setattr(driver.execution, "command", lambda *a, **k: pytest.fail("checks must not run"))
    state = driver.run(repo, source, out, mode="improve")
    assert state["attempts"][0]["status"] == "rejected" and state["status"] == "completed"
    assert {key for _, key in runs} == {"a:target"} and runs.count(("candidate", "a:target")) == 3


@pytest.mark.parametrize("change", ["contained", "remove_task", "host"])
def test_benchmark_repair_keeps_agent_fixed_and_verifies_before_ready(tmp_path, monkeypatch, change):
    from ci.auto_improve.benchmark_repair import repair
    remove_task = change == "remove_task"
    repo, out = tmp_path / "repo", tmp_path / "result/cluster-1"
    repo.mkdir(); out.mkdir(parents=True)
    path = {"remove_task": "benchmark/suites/example.json", "host": "benchmark/runner/environment.py",
            "contained": "benchmark/docker/agent-daemon-entrypoint.sh"}[change]
    before = json.dumps({"tasks": [{"id": "t", "rubric": []}]}) if remove_task else "message='socket missing'\n"
    after = json.dumps({"tasks": []}) if remove_task else "message='ready'\n"
    file = repo / path
    file.parent.mkdir(parents=True); file.write_text(before)
    plan = {"targets": ["a:t"], "neighbors": ["a:n"], "held_out": ["b:h"]}
    tasks = {k: {"environment": "isolated", "task": {}} for keys in plan.values() for k in keys}
    cluster = {"title": "Fix setup", "category": "setup", "judge_review": "supported", "fix_target": "benchmark_code",
               "source_paths": [path], "targets": plan["targets"], "symptom": {"kind": "tool_error", "pattern": "socket missing"}}
    attempt = {"cluster": cluster, "plan": plan}
    events = []
    def checkout(_repo, dest, _base, **kwargs):
        (dest / path).parent.mkdir(parents=True)
        (dest / path).write_text(before)
    def ask(_out, stage, _repo, payload, *args, **kwargs):
        events.append(stage)
        if stage == "fix":
            return {"summary": "repair scenario", "edits": [{"path": path, "old": before, "new": after}]}
        return {"supported": True, "reason": "evidence", "evidence": []}
    def run(root, key, repeat, image, dest, **kwargs):
        assert kwargs["rejudge"] is False
        events.append(f"run:{root.name}:{key}")
        evidence_dir = dest / "evidence/tasks/t"
        evidence_dir.mkdir(parents=True)
        result = FAILED_TOOL if root.name == "baseline" and key == "a:t" else {"content": "ready"}
        (evidence_dir / "trace.json").write_text(json.dumps({"tool_results": [result]}))
        return {**row(repeat), "evidence": str(dest / "evidence")}
    monkeypatch.setattr(driver, "checkout", checkout)
    monkeypatch.setattr(driver, "ask", ask)
    monkeypatch.setattr(execution, "prepare", lambda *a, **kw: None)
    monkeypatch.setattr(execution, "remove_images", lambda *a: None)
    monkeypatch.setattr(execution, "run_task", run)
    monkeypatch.setattr(execution, "command", lambda *a, **kw: events.append("check"))
    monkeypatch.setattr(evidence, "catalog_tasks", lambda _: {k: v for k, v in tasks.items() if not (remove_task and k == "a:t")})
    monkeypatch.setattr(subprocess, "check_output", lambda cmd, **kw: "" if "--name-only" in cmd else "diff\n")
    repair(repo, "base", out, attempt, tasks, [path], time.monotonic() + 1000)
    assert attempt["check_passed"] and attempt["reproduction"] == {"a:t": 3}
    assert not (out / "baseline").exists() and not (out / "candidate").exists()
    if change == "host":
        # Model-authored host code is proposed for human verification, never executed.
        assert attempt["status"] == "proposal_ready" and attempt["host_paths"] == [path]
        assert not any(e.startswith("run:candidate") for e in events) and "verification-review" not in events
        return
    assert attempt["status"] == "ready" and attempt["comparison"]["passed"]
    assert events.index("run:baseline:a:t") < events.index("fix") < events.index("check") < events.index("verification-review")
    assert bool(attempt["contract_changes"]) is remove_task
    assert attempt["removed_tasks"] == (["a:t"] if remove_task else [])


def test_raw_artifact_paths_cannot_escape_run(tmp_path):
    run = tmp_path / "benchmark/runs/ci-42-1-a"
    run.mkdir(parents=True)
    (run / "manifest.json").write_text('{}')
    (run / "results.jsonl").write_text(json.dumps({"task_id": "../../other", "status": "failed", "attempt": 1}))
    failures, warnings = evidence.source_failures(tmp_path, "42", "1", {})
    assert not failures
    assert warnings


def test_issue_publication_is_idempotent_and_requires_supported_diagnosis():
    cluster = {"targets": ["a:t"], "source_paths": ["src/device.cpp"], "category": "device",
               "fix_target": "external_code", "judge_review": "supported", "cause": "Broken device transport",
               "evidence": ["trace step 2"]}
    attempt = {"cluster": cluster, "signature": policy.signature(cluster), "title": "Fix transport"}
    feedback, calls = [], []
    class API:
        def request(self, path, payload):
            calls.append((path, payload))
            return {"html_url": "https://example.invalid/issue/1", "body": payload["body"]}
    manifest = {"source": "41-1__42-1"}
    assert control.publish_issue(API(), manifest, attempt, feedback) == "https://example.invalid/issue/1"
    control.publish_issue(API(), manifest, attempt, feedback)
    assert len(calls) == 1
    cluster["judge_review"] = "insufficient_evidence"
    with pytest.raises(ValueError, match="supported diagnosis"):
        control.publish_issue(API(), manifest, attempt, feedback)


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


@pytest.mark.parametrize("go_code,benchmark,proposal", [(False, False, False), (True, False, False),
                                                       (False, True, False), (False, True, True)])
def test_publisher_creates_only_verified_draft_from_exact_patch(tmp_path, monkeypatch, go_code, benchmark, proposal):
    repo = tmp_path / "repo"
    repo.mkdir()
    import os
    env = {**os.environ, "GIT_EDITOR": "true", "GIT_CONFIG_NOSYSTEM": "1",
           "GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
           "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.invalid"}
    def local_git(*args):
        return subprocess.check_output(["git", *args], cwd=repo, env=env, text=True).strip()
    local_git("init", "-q")
    path = ("benchmark/runner/environment.py" if proposal else "benchmark/docker/agent-daemon-entrypoint.sh" if benchmark else
            "src/agent/internal/agent/navigation.go" if go_code else "src/agent/config/skills/test/SKILL.md")
    before = "package agent\nfunc retries() int { return 0 }\n" if go_code else "Observe first.\n\nKeep it short.\n"
    after = before.replace("return 0", "return 1") if go_code else before.replace("Observe", "Observe and verify")
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
    if benchmark:
        cluster.update(fix_target="benchmark_code", judge_review="supported",
                       symptom={"kind": "tool_error", "pattern": "socket missing"})
        for side, data in (("baseline", baseline), ("candidate", candidate)):
            for key, rows in data.items():
                for r in rows:
                    directory = out / "cluster-1" / f"{side}-runs" / policy.digest(key)[:12] / str(r["repeat"]) / "evidence"
                    task = directory / "tasks/t"
                    task.mkdir(parents=True)
                    failing = side == "baseline" and key in plan["targets"]
                    result = FAILED_TOOL if failing else {"content": "ready"}
                    (task / "trace.json").write_text(json.dumps({"tool_results": [result]}))
                    (directory / "results.jsonl").write_text(json.dumps(r) + "\n")
                    r.update(evidence=str(directory), symptom_evidence=[result["content"]] if failing else [],
                             tool_failures=int(failing))
        comparison = policy.compare_symptom(plan, cluster["symptom"], baseline, candidate, [])
        monkeypatch.setattr(evidence, "catalog_tasks", lambda _: {k: {} for keys in plan.values() for k in keys})
    else:
        comparison = policy.compare(plan, baseline, candidate)
    manifest = {"base_sha": base, "base_branch": "main", "execution_ref": "refs/heads/main", "source_sha": base, "source": "42-1", "configuration": {}, "avd_fixture": "",
                "attempts": [{"status": "ready", "title": "Verify actions", "cluster": cluster, "signature": policy.signature(cluster),
                              "patch": "candidate.patch", "patch_sha256": policy.digest(patch), "paths": [path],
                              "check_passed": True, "checks": policy.verification_commands([path]),
                              "audit": {"supported": True}, "plan": plan, "comparison": comparison}]}
    if proposal:
        # Only targets are reproduced; a proposal never runs the candidate or regression tasks.
        baseline = {key: baseline[key] for key in plan["targets"]}
        manifest["attempts"][0].update(status="proposal_ready", host_paths=[path], reproduction={"phone:tap": 3},
                                       audit={}, comparison={})
    for name, value in (("manifest", manifest), ("baseline", baseline), ("candidate", candidate),
                        ("verification-review", {"supported": True}), ("reproduction-review", {"supported": True})):
        (out / f"{name}.json").write_text(json.dumps(value))
    calls = []
    main = {"sha": base, "content": before}
    class API:
        repository = "org/repo"
        def issues(self): return []
        def pages(self, *args): return []
        def request(self, endpoint, payload=None):
            calls.append((endpoint, payload))
            if endpoint == "git/ref/heads/main": return {"object": {"sha": main["sha"]}}
            if endpoint.startswith("contents/"):
                assert endpoint.endswith(f"?ref={main['sha']}")
                return {"type": "file", "encoding": "base64",
                        "content": base64.b64encode(main["content"].encode()).decode()}
            if endpoint.startswith("git/commits/"): return {"tree": {"sha": "old-tree"}}
            if endpoint == "git/blobs":
                assert payload["content"] == main.get("expected", after)
                return {"sha": "blob"}
            if endpoint == "git/trees": return {"sha": "tree"}
            if endpoint == "git/commits":
                assert payload["parents"] == [main["sha"]]
                return {"sha": "commit"}
            if endpoint == "git/refs":
                assert payload["ref"].startswith("refs/heads/auto-improve/")
                return {}
            if endpoint == "pulls":
                assert payload["draft"] is True and payload["base"] == "main"
                return {"number": 1, "html_url": "https://github.com/org/repo/pull/1"}
            if endpoint == "issues/1/labels": return {}
            if endpoint == "issues":
                return {"html_url": "https://github.com/org/repo/issues/2", "body": payload["body"]}
            pytest.fail(endpoint)
    assert control.publish(API(), repo, out, "main") == ["https://github.com/org/repo/pull/1"]
    assert target.read_text() == before
    body = next(payload for endpoint, payload in calls if endpoint == "pulls")["body"]
    assert ("Not executed: human verification required" in body) is proposal
    if not (go_code or benchmark):
        # main advanced during the run: the same patch lands on the new main when it applies.
        main.update(sha="newer", content=before + "Footer added on main.\n",
                    expected=after + "Footer added on main.\n")
        calls.clear()
        assert control.publish(API(), repo, out, "main") == ["https://github.com/org/repo/pull/1"]
        assert "`main` advanced to `newer`" in next(p for e, p in calls if e == "pulls")["body"]
        main.update(content="Observe nothing.\n\nKeep it short.\n")
        with pytest.raises(ValueError, match="rerun validation"):
            control.publish(API(), repo, out, "main")
        main.update(sha=base, content=before)
        main.pop("expected")
    if proposal:
        # A host-path change claiming to have been executed is refused.
        manifest["attempts"][0]["status"] = "ready"
        (out / "manifest.json").write_text(json.dumps(manifest))
        with pytest.raises(ValueError, match="only be published as a proposal"):
            control.publish(API(), repo, out, "main")
        return
    if benchmark:
        assert any(endpoint == "issues" for endpoint, _ in calls)
        # Tampering with an embedded status must fail against retained raw execution evidence.
        baseline[plan["targets"][0]][0]["status"] = "passed"
        (out / "baseline.json").write_text(json.dumps(baseline))
        with pytest.raises(ValueError, match="retained execution"):
            control.publish(API(), repo, out, "main")
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

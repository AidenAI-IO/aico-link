"""Pure selection, patch, and acceptance rules shared by the driver and publisher."""
from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path, PurePosixPath
import re
import statistics
from typing import Any

from runner.metrics import status_fields
from runner.trace import tool_result_failed

# Targets need three attempts per side to tell a reproducible failure or fix from noise.
REPEATS = 3
# Regression tasks are screened once per side; only one that looks worse is rerun to REPEATS.
SCREEN_REPEATS = 1
BASE_BRANCH = "main"
# Repair domains. agent and benchmark changes are verified differently and published as
# separate draft PRs; external code cannot be verified by the benchmark and only gets an issue.
REPAIR_DOMAINS = ("agent", "benchmark")
BENCHMARK_EDIT_ROOTS = (
    "benchmark/runner/", "benchmark/ci/", "benchmark/docker/", "benchmark/suites/",
    "benchmark/adbandroid/", "benchmark/desktop/", "benchmark/vphone/",
    "benchmark/mobilegym/bridge/", "benchmark/mobilegym/adapter/", "benchmark/mobilegym/docker/",
    "benchmark/mobilegym/scripts/", "src/agent/cmd/benchmark-mcu/")
BENCHMARK_SUFFIXES = {".py", ".sh", ".json", ".go", ".yml", ".yaml"}
# Benchmark files that are task data or run only inside disposable containers. Every
# other benchmark file runs on the self-hosted runner, which holds model credentials
# and the Docker socket; model-authored changes there are proposed, never executed.
BENCHMARK_CONTAINED = (
    "benchmark/suites/", "benchmark/ci/suites.json", "benchmark/docker/Dockerfile",
    "benchmark/docker/agent-daemon-entrypoint.sh", "benchmark/mobilegym/bridge/",
    "benchmark/mobilegym/adapter/", "benchmark/mobilegym/docker/",
    "benchmark/mobilegym/scripts/start_simulator.py", "src/agent/cmd/benchmark-mcu/")
# Final run states that settle a source window. Anything else is retried by a later run.
SETTLED_STATUSES = {"completed", "diagnosed", "no_failures"}
# Task fields that define how an outcome is scored. Changing them needs human judgment.
SCORING_FIELDS = ("rubric", "hard_assertions", "environment_assertions", "memory_assertions",
                  "description_for_judge", "expected_answer", "answer_format",
                  "expected_recalled_memory_ids", "expected_recalled_memory_tool",
                  "expected_recall_from_consolidation")
GRADING_CODE = ("benchmark/runner/judge.py", "benchmark/runner/assertions.py", "benchmark/runner/matching.py",
                "benchmark/runner/metrics.py", "benchmark/runner/rejudge.py")
# Code that writes the status, error, stage and trace fields a symptom is measured from.
# A repair here could hide the symptom by changing what is recorded, so it needs human review.
RESULT_RECORDING_CODE = ("benchmark/runner/main.py", "benchmark/runner/runtask.py", "benchmark/runner/scenario.py",
                         "benchmark/runner/trace.py", "benchmark/runner/harness.py")
SYMPTOM_KINDS = ("none", "setup_failure", "judge_error", "environment_failure", "tool_error",
                 "hard_assertion", "error_text")
# Regression tolerance for three repeats per side: one lost pass per task is ordinary
# model variance, so a task regresses only when it loses at least two of its passes.
TASK_REGRESSION_DROP = 2


class ProposalError(ValueError):
    """A model proposal violates policy. The attempt is rejected; the run is not blocked."""


def require_main_branch(branch: str) -> None:
    if branch != BASE_BRANCH:
        raise ValueError("Auto-improve only supports main")


def execution_policy(event: str, ref: str, mode: str, publish_requested: bool = False) -> dict:
    """Separate the code under test from the main-only source/publication policy."""
    if not ref.startswith("refs/heads/") or mode not in {"diagnose", "improve"}:
        raise ValueError("Expected a repository branch and a supported mode")
    if event == "schedule":
        if ref != "refs/heads/main":
            raise ValueError("Scheduled auto-improve runs only on main")
    elif event == "push":
        if ref.startswith("refs/heads/auto-improve-test/diagnose/"):
            mode = "diagnose"
        elif ref.startswith("refs/heads/auto-improve-test/improve/"):
            mode = "improve"
        else:
            raise ValueError("Push tests require an explicit auto-improve-test mode prefix")
    elif event == "workflow_dispatch":
        if ref != "refs/heads/main" and not ref.startswith("refs/heads/auto-improve-test/"):
            raise ValueError("Manual auto-improve runs require main or a test branch")
    else:
        raise ValueError("Unsupported auto-improve trigger")
    publish = (ref == "refs/heads/main" and mode == "improve" and
               (event == "schedule" or (event == "workflow_dispatch" and publish_requested)))
    return {"mode": mode, "publish": publish}


def digest(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False).encode()).hexdigest()


def edit_target(path: str, domain: str = "agent") -> bool:
    """Each domain edits only its own code; neither may touch tests, CI, dependencies or this pipeline."""
    if not isinstance(path, str) or not path or "\\" in path:
        return False
    p = PurePosixPath(path)
    if (p.is_absolute() or p.as_posix() != path or
            any(part.startswith(".") or part in {"vendor", "testdata", "tests", "__pycache__"} for part in p.parts)
            or p.name.endswith("_test.go") or p.name.startswith("test_")):
        return False
    if domain == "agent":
        # Benchmark mode and the MCU emulator are part of the evaluation environment.
        if any(part.startswith("benchmark") for part in p.parts[2:]):
            return False
        return ((path.startswith(("src/agent/internal/", "src/agent/cmd/")) and p.suffix == ".go") or
                (path.startswith("src/agent/config/skills/") and p.name == "SKILL.md"))
    if domain == "benchmark":
        # The pipeline that decides acceptance must never rewrite itself.
        if path.startswith("benchmark/ci/auto_improve/"):
            return False
        return (path.startswith(BENCHMARK_EDIT_ROOTS) and
                (p.suffix in BENCHMARK_SUFFIXES or p.name.startswith("Dockerfile")))
    return False


def runs_on_host(path: str) -> bool:
    """Whether a benchmark file executes on the runner host rather than in a container."""
    return not path.startswith(BENCHMARK_CONTAINED)


def artifact_name(ref: str, identity: str, status: str) -> str:
    """Only a settled main run marks its sources processed; failed runs stay eligible."""
    if ref != f"refs/heads/{BASE_BRANCH}":
        return f"auto-improve-test-source-{identity}"
    prefix = "auto-improve-source-" if status in SETTLED_STATUSES else "auto-improve-retry-"
    return prefix + identity


def verification_commands(paths: list[str]) -> list[list[str]]:
    """Quick checks alone exercise neither the agent's Go tests nor the full benchmark suite."""
    commands = [["make", "check"]]
    if any(path.endswith(".go") for path in paths):
        commands.append(["bash", "scripts/run_tests_in_docker.sh", "--suite", "go-unit"])
    if any(path.startswith("benchmark/") for path in paths):
        commands.append(["bash", "scripts/run_tests_in_docker.sh", "--suite", "benchmark-python"])
    return commands


def validate_change(path: str, before: str, after: str, domain: str = "agent") -> None:
    if not edit_target(path, domain) or before == after:
        raise ProposalError(f"Not an allowed {domain} change: {path}")
    if len(after.encode()) > 128 * 1024:
        raise ProposalError("Candidate file exceeds text budget")
    # An agent fix must improve real behavior, not special-case the evaluation.
    if domain == "agent" and len(BENCHMARK_REFERENCE.findall(after)) > len(BENCHMARK_REFERENCE.findall(before)):
        raise ProposalError(f"Agent change adds benchmark-specific behavior: {path}")
    if path.endswith(".json"):
        try:
            json.loads(after)
        except ValueError as exc:
            raise ProposalError(f"Candidate JSON is invalid: {path}") from exc


BENCHMARK_REFERENCE = re.compile(r"benchmark", re.IGNORECASE)


def contract_changes(path: str, before: str, after: str) -> list[str]:
    """Name every change to what a task asks or how it is scored, for explicit human review."""
    if path in GRADING_CODE:
        return [f"{path}: grading code changed"]
    if path in RESULT_RECORDING_CODE:
        return [f"{path}: result recording code changed"]
    if path == "benchmark/ci/suites.json":
        old, new = json.loads(before), json.loads(after)
        old_cases = {c["id"]: c for c in old.get("cases", [])}
        new_cases = {c["id"]: c for c in new.get("cases", [])}
        return ([f"{path}: removed case {k}" for k in sorted(set(old_cases) - set(new_cases))] +
                [f"{path}: case {k} task selection changed" for k in sorted(set(old_cases) & set(new_cases))
                 if old_cases[k] != new_cases[k]])
    if not (path.startswith("benchmark/suites/") and path.endswith(".json")):
        return []
    old, new = json.loads(before), json.loads(after)
    old_tasks = {t["id"]: t for t in old.get("tasks", [])}
    new_tasks = {t["id"]: t for t in new.get("tasks", [])}
    changes = [f"{path}: removed task {k}" for k in sorted(set(old_tasks) - set(new_tasks))]
    changes += [f"{path}: added task {k}" for k in sorted(set(new_tasks) - set(old_tasks))]
    for key in sorted(set(old_tasks) & set(new_tasks)):
        changes += [f"{path}: task {key} changed {field}" for field in sorted(set(old_tasks[key]) | set(new_tasks[key]))
                    if old_tasks[key].get(field) != new_tasks[key].get(field)]
    changes += [f"{path}: suite field {k} changed" for k in sorted(set(old) | set(new))
                if k != "tasks" and old.get(k) != new.get(k)]
    return changes


def validate_symptom(symptom: dict) -> None:
    """A benchmark repair needs a mechanically observable failure signature."""
    if not isinstance(symptom, dict) or symptom.get("kind") not in SYMPTOM_KINDS or symptom["kind"] == "none":
        raise ProposalError("Benchmark repair needs an observable symptom")
    pattern = symptom.get("pattern", "")
    if not isinstance(pattern, str) or len(pattern) > 200:
        raise ProposalError("Symptom pattern must be a short string")
    if symptom["kind"] in {"tool_error", "hard_assertion", "error_text"} and not pattern:
        raise ProposalError(f"Symptom {symptom['kind']} requires a pattern")
    try:
        re.compile(pattern)
    except re.error as exc:
        raise ProposalError("Symptom pattern is not a valid regular expression") from exc


def symptom_matches(symptom: dict, row: dict, traces: list[dict]) -> list[str]:
    """Return short evidence excerpts when one attempt shows the symptom."""
    pattern = re.compile(symptom.get("pattern") or ".*", re.IGNORECASE | re.DOTALL)
    metrics = row.get("metrics") if isinstance(row.get("metrics"), dict) else {}
    errors = [str(metrics.get(k)) for k in ("error", "judge_error") if metrics.get(k)]
    kind = symptom["kind"]
    if kind == "setup_failure":
        texts = errors if metrics.get("first_failure_stage") == "setup" else []
    elif kind == "judge_error":
        texts = (errors or ["judge_error"]) if row.get("status") == "judge_error" else []
    elif kind == "environment_failure":
        texts = (errors or ["environment"]) if status_fields(row)[1] == "environment" else []
    elif kind == "hard_assertion":
        texts = [f"{f.get('id')}: {f.get('actual')}" for f in row.get("hard_assertion_failures") or []
                 if isinstance(f, dict)]
    elif kind == "tool_error":
        # Only results the runner classifies as failed, not every tool output that happens to match.
        texts = [str(result.get("content")) for trace in traces for result in trace.get("tool_results") or []
                 if isinstance(result, dict) and result.get("content") and tool_result_failed(result)]
    else:
        texts = errors
    return [text[:300] for text in texts if pattern.search(text)][:3]


def apply_edits(root: Path, edits: list[dict], domain: str = "agent") -> list[str]:
    """Apply exact replacements after validating the entire patch, without partial writes."""
    if not isinstance(edits, list) or not 1 <= len(edits) <= 8:
        raise ProposalError("Expected 1-8 text edits")
    original, modified = {}, {}
    for edit in edits:
        path = edit["path"]
        if not edit_target(path, domain):
            raise ProposalError(f"Forbidden edit: {path}")
        target = root / path
        if target.is_symlink() or not target.resolve().is_relative_to(root.resolve()):
            raise ProposalError("Candidate path escapes checkout")
        if path not in original:
            original[path] = modified[path] = target.read_text()
        old, new = edit["old"], edit["new"]
        if not isinstance(old, str) or not old or not isinstance(new, str):
            raise ProposalError("Edits require nonempty old text and string new text")
        if modified[path].count(old) != 1:
            raise ProposalError(f"Replacement is not unique: {path}")
        modified[path] = modified[path].replace(old, new, 1)
    for path, text in modified.items():
        validate_change(path, original[path], text, domain)
    for path, text in modified.items():
        (root / path).write_text(text)
    return sorted(modified)


def review_cost_summary(sessions: list[dict]) -> dict:
    """Unknown provider pricing or interrupted calls are not zero-cost sessions."""
    costs = [s.get("total_cost_usd") for s in sessions]
    known = [cost for cost in costs if isinstance(cost, (int, float)) and not isinstance(cost, bool)
             and math.isfinite(cost) and cost >= 0]
    return {"estimated_cost_usd": sum(known) if known else None,
            "measured_sessions": len(known), "total_sessions": len(sessions),
            "complete": bool(sessions) and len(known) == len(sessions)}


def signature(cluster: dict) -> str:
    # Ignore model-authored titles. Include the actual affected identity, location and domain.
    identity = {"targets": sorted(cluster["targets"]),
                "paths": sorted(cluster["source_paths"]), "category": cluster["category"]}
    if domain_of(cluster) != "agent":
        identity["domain"] = domain_of(cluster)
    return digest(identity)[:24]


AGENT_TARGETS = ("skill", "prompt", "tool_description", "go_code")
BENCHMARK_TARGETS = ("benchmark_code", "task_definition")


def domain_of(cluster: dict) -> str:
    """agent and benchmark clusters are repaired; external code gets an issue; unknown is report-only."""
    target = cluster.get("fix_target", "go_code")
    if target in AGENT_TARGETS:
        return "agent"
    if target in BENCHMARK_TARGETS:
        return "benchmark"
    return "external" if target == "external_code" else "unknown"


def select_tasks(cluster: dict, tasks: dict[str, dict], *, max_tasks: int = 32,
                 exposed_tasks: set[str] | None = None) -> dict[str, list[str]]:
    targets = list(dict.fromkeys(cluster["targets"]))
    neighbors = list(dict.fromkeys(cluster.get("neighbors", [])))
    if not targets or any(k not in tasks for k in targets + neighbors):
        raise ValueError("Diagnosis references unknown tasks")
    if len({tasks[k]["environment"] for k in targets}) != 1:
        raise ValueError("One cluster must use one execution environment")
    selected = set(targets)
    for key in targets:
        task = tasks[key]
        tools = set(task["task"].get("hard_assertions", {}).get("required_tools", []))
        for other, entry in sorted(tasks.items()):
            candidate = entry["task"]
            if entry["case_id"] == task["case_id"] and other not in selected:
                if (candidate.get("category") == task["task"].get("category") or
                        tools.intersection(candidate.get("hard_assertions", {}).get("required_tools", []))):
                    neighbors.append(other)
    neighbors = sorted(set(neighbors) - selected)
    # Select held-out tasks deterministically after diagnosis; never show these to the fixer.
    environment = tasks[targets[0]]["environment"]
    remaining = sorted((k for k in set(tasks) - selected - set(neighbors) - (exposed_tasks or set())
                        if tasks[k]["environment"] == environment), key=lambda k: digest(k))
    held_out = remaining[:min(10, len(remaining))]
    if not neighbors or not held_out:
        raise ValueError("Need both neighboring and held-out regression tasks; defer if unavailable")
    plan = {"targets": sorted(targets), "neighbors": neighbors, "held_out": held_out}
    if sum(map(len, plan.values())) > max_tasks:
        raise ValueError("Required regression coverage exceeds task budget; defer rather than truncate")
    return plan


def prune_reproduction(plan: dict[str, list[str]], audit: dict,
                       baseline: dict[str, list[dict]]) -> tuple[dict[str, list[str]], dict[str, list[str]]]:
    """Keep only individually supported, reproducible targets; quarantine uncertain targets as regression cases."""
    tasks = audit.get("tasks")
    if not isinstance(tasks, list):
        raise ValueError("Reproduction audit must give a per-task disposition")
    indexed = {item.get("task"): item for item in tasks if isinstance(item, dict)}
    expected = set(plan["targets"])
    if len(indexed) != len(tasks) or set(indexed) != expected:
        raise ValueError("Reproduction audit omitted, duplicated, or invented task IDs")
    kept_targets, uncertain_targets = [], []
    for key in plan["targets"]:
        result = indexed[key]
        rows = baseline.get(key, [])
        reproduced = (len(rows) == REPEATS and
                      {r.get("repeat") for r in rows} == set(range(1, REPEATS + 1)) and
                      all(status_fields(r)[2] and status_fields(r)[0] is not None for r in rows) and
                      sum(status_fields(r)[0] is False for r in rows) >= 2)
        if result["disposition"] == "agent_failure" and reproduced:
            kept_targets.append(key)
        else:
            uncertain_targets.append(key)
    if not kept_targets:
        raise ValueError("No target task has individually supported, reproducible agent failure evidence")
    pruned = dict(plan)
    pruned["targets"] = kept_targets
    pruned["neighbors"] = sorted(set(plan["neighbors"] + uncertain_targets))
    if not pruned["neighbors"] or not plan["held_out"]:
        raise ValueError("Pruned verification lacks neighbor or held-out task coverage")
    return pruned, {"targets": uncertain_targets}


def stopped_at_setup(row: dict) -> bool:
    """A failed attempt whose first failure was its setup step, before any agent turn ran."""
    metrics = row.get("metrics") if isinstance(row.get("metrics"), dict) else {}
    return status_fields(row)[0] is False and metrics.get("first_failure_stage") == "setup"


def complete(rows: list[dict], repeats: int) -> bool:
    return len(rows) == repeats and {r.get("repeat") for r in rows} == set(range(1, repeats + 1))


def needs_rerun(baseline: list[dict], candidate: list[dict], *, symptom: bool = False) -> bool:
    """Whether a screened regression task looks worse on the candidate and needs REPEATS attempts.

    Agent repairs gate on passes; benchmark repairs gate on the symptom and evaluability.
    """
    if symptom:
        return (any(r.get("symptom_evidence") for r in candidate) and not any(r.get("symptom_evidence") for r in baseline)
                or sum(map(unevaluable, candidate)) > sum(map(unevaluable, baseline)))
    return (sum(status_fields(r)[0] is True for r in candidate) < sum(status_fields(r)[0] is True for r in baseline))


def task_repeats(group: str, candidate: list[dict] | None) -> int:
    """Targets always use REPEATS; a regression task uses REPEATS only once it was rerun."""
    return REPEATS if group == "targets" or (candidate is not None and len(candidate) > SCREEN_REPEATS) else SCREEN_REPEATS


def fixed_targets(plan: dict[str, list[str]], baseline: dict[str, list[dict]],
                  candidate: dict[str, list[dict]]) -> list[str]:
    """Targets that failed at least 2/3 on baseline and passed at least 2/3 on the candidate."""
    return [key for key in plan["targets"]
            if sum(status_fields(r)[0] is True for r in baseline.get(key, [])) <= 1
            and sum(status_fields(r)[0] is True for r in candidate.get(key, [])) >= 2]


def compare(plan: dict[str, list[str]], baseline: dict[str, list[dict]],
            candidate: dict[str, list[dict]]) -> dict:
    """Fail closed on missing, duplicate, ineligible, or unmeasured outcomes.

    Targets run REPEATS times per side. Regression tasks are screened once per side
    and rerun to REPEATS only when the candidate looks worse; a single-attempt drop that
    was not rerun is rejected. Three repeats cannot separate a one-pass drop from model
    variance, so a rerun task regresses when it loses TASK_REGRESSION_DROP passes, and
    the regression set regresses when more than max(1, 5%) of its tasks lose any pass.
    """
    reasons, table, worse = [], [], []
    regression_tasks = 0
    for group, keys in plan.items():
        for key in keys:
            rows = {"baseline": baseline.get(key, []), "candidate": candidate.get(key, [])}
            repeats = task_repeats(group, rows["candidate"])
            pair = []
            for side, side_rows in rows.items():
                if not complete(side_rows, repeats):
                    reasons.append(f"{key}: incomplete {side} repeats")
                if any(status_fields(r)[0] is None or not status_fields(r)[2] for r in side_rows):
                    reasons.append(f"{key}: unevaluable {side} outcome")
                pair.append(sum(status_fields(r)[0] is True for r in side_rows))
            before, after = pair
            table.append({"task": key, "group": group, "baseline": before, "candidate": after, "repeats": repeats})
            if before - after >= TASK_REGRESSION_DROP:
                reasons.append(f"{key}: observed regression {before}/{repeats} -> {after}/{repeats}")
            if group == "targets":
                continue
            regression_tasks += 1
            if repeats == SCREEN_REPEATS and needs_rerun(rows["baseline"], rows["candidate"]):
                reasons.append(f"{key}: screening drop was not rerun")
            elif after < before:
                worse.append(key)
    tolerance = max(1, math.ceil(0.05 * regression_tasks))
    if len(worse) > tolerance:
        reasons.append(f"regression set: {len(worse)} tasks lost passes (tolerance {tolerance})")
    fixed = fixed_targets(plan, baseline, candidate)
    if not fixed:
        reasons.append("No reproduced target improved to at least 2/3 passes")
    efficiency = {}
    for metric in ("tool_calls", "total_tokens"):
        medians = []
        for side, data in (("baseline", baseline), ("candidate", candidate)):
            # An attempt that failed its setup assertion never reached the agent turn, so it
            # has no agent cost to measure; every other attempt must be measured.
            rows = [r for keys in plan.values() for key in keys for r in data.get(key, [])
                    if not stopped_at_setup(r)]
            values = [r.get("metrics", {}).get(metric) for r in rows]
            if not values or any(isinstance(v, bool) or not isinstance(v, (int, float))
                                 or not math.isfinite(v) or v < 0 for v in values):
                reasons.append(f"{side}: missing {metric} measurements")
                medians.append(None)
            else:
                medians.append(statistics.median(values))
        before, after = medians
        efficiency[metric] = {"baseline": before, "candidate": after}
        if before is not None and after is not None and after > before * 1.2:
            reasons.append(f"{metric}: median increased more than 20%")
    return {"passed": not reasons, "reasons": reasons, "tasks": table,
            "fixed": fixed, "efficiency": efficiency, "repeats": REPEATS}


def unevaluable(row: dict) -> bool:
    success, _, eligible = status_fields(row)
    return success is None or not eligible


def compare_symptom(plan: dict[str, list[str]], symptom: dict, baseline: dict[str, list[dict]],
                    candidate: dict[str, list[dict]], removed: list[str]) -> dict:
    """Benchmark repairs: the same agent shows the symptom before and never after; pass rates are reported only.

    Targets run REPEATS times per side; regression tasks are screened as in compare().

    Every row carries symptom_evidence, recorded by the driver from that attempt's own trace.
    A target the candidate removed from the catalog is accepted only as a flagged contract change.
    A tool_error target must also lose failed tool results, not merely change their wording.
    """
    validate_symptom(symptom)
    reasons, table, fixed = [], [], []
    for group, keys in plan.items():
        for key in keys:
            sides = {"baseline": baseline.get(key, []), "candidate": candidate.get(key, [])}
            if key in removed:
                if group != "targets" or sides["candidate"]:
                    reasons.append(f"{key}: only a target can be removed, and it must not run")
                sides["candidate"] = None
            repeats = task_repeats(group, sides["candidate"])
            entry = {"task": key, "group": group, "removed": key in removed, "repeats": repeats}
            for side, rows in sides.items():
                if rows is None:
                    continue
                if not complete(rows, repeats):
                    reasons.append(f"{key}: incomplete {side} repeats")
                if any(not isinstance(r.get("symptom_evidence"), list) or isinstance(r.get("tool_failures"), bool)
                       or not isinstance(r.get("tool_failures"), int) for r in rows):
                    reasons.append(f"{key}: {side} symptom not measured")
                entry[side] = {"passed": sum(status_fields(r)[0] is True for r in rows),
                               "symptom": sum(bool(r.get("symptom_evidence")) for r in rows),
                               "unevaluable": sum(unevaluable(r) for r in rows),
                               "tool_failures": sum(r.get("tool_failures") if isinstance(r.get("tool_failures"), int)
                                                    else 0 for r in rows)}
            table.append(entry)
            before, after = entry.get("baseline"), entry.get("candidate")
            if before is None:
                continue
            if group == "targets":
                if before["symptom"] < 2:
                    reasons.append(f"{key}: symptom reproduced only {before['symptom']}/3 on baseline")
                elif (symptom["kind"] == "tool_error" and after is not None
                      and after["tool_failures"] >= before["tool_failures"]):
                    reasons.append(f"{key}: failed tool results did not decrease "
                                   f"({before['tool_failures']} -> {after['tool_failures']})")
                elif after is None or (after["symptom"] == 0 and after["unevaluable"] == 0):
                    fixed.append(key)
                else:
                    reasons.append(f"{key}: candidate still shows the symptom {after['symptom']}/3 "
                                   f"or has {after['unevaluable']}/3 unevaluable attempts")
            elif after is not None and repeats == SCREEN_REPEATS:
                if needs_rerun(sides["baseline"], sides["candidate"], symptom=True):
                    reasons.append(f"{key}: screening symptom or unevaluable attempt was not rerun")
            elif after is not None:
                # Other tasks must not gain the symptom or become unevaluable; pass rates are not gated.
                if after["symptom"] > before["symptom"]:
                    reasons.append(f"{key}: symptom spread {before['symptom']}/3 -> {after['symptom']}/3")
                if after["unevaluable"] > before["unevaluable"]:
                    reasons.append(f"{key}: unevaluable attempts {before['unevaluable']}/3 -> {after['unevaluable']}/3")
    if not plan.get("targets") or set(fixed) != set(plan["targets"]):
        reasons.append("Every reproduced target must lose the symptom")
    return {"passed": not reasons, "reasons": reasons, "tasks": table, "fixed": sorted(fixed),
            "symptom": symptom, "removed": sorted(removed), "repeats": REPEATS}

"""Pure selection, patch, and acceptance rules shared by the driver and publisher."""
from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path, PurePosixPath
import statistics
from typing import Any

from runner.metrics import status_fields

REPEATS = 3
BASE_BRANCH = "main"


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


def edit_target(path: str) -> bool:
    """Allow agent implementation and skill changes, preserving the evaluation contract."""
    if not isinstance(path, str) or not path or "\\" in path:
        return False
    p = PurePosixPath(path)
    if (p.is_absolute() or p.as_posix() != path or
            any(part.startswith(".") or part in {"vendor", "testdata"} for part in p.parts)):
        return False
    return ((path.startswith(("src/agent/internal/", "src/agent/cmd/")) and
             p.suffix == ".go" and not p.name.endswith("_test.go")) or
            (path.startswith("src/agent/config/skills/") and p.name == "SKILL.md"))


def verification_commands(paths: list[str]) -> list[list[str]]:
    """Quick checks alone do not exercise the agent's Go implementation tests."""
    commands = [["make", "check"]]
    if any(path.endswith(".go") for path in paths):
        commands.append(["bash", "scripts/run_tests_in_docker.sh", "--suite", "go-unit"])
    return commands


def validate_change(path: str, before: str, after: str) -> None:
    if not edit_target(path) or before == after:
        raise ValueError(f"Not an allowed agent change: {path}")
    if len(after.encode()) > 128 * 1024:
        raise ValueError("Candidate file exceeds text budget")


def apply_edits(root: Path, edits: list[dict]) -> list[str]:
    """Apply exact replacements after validating the entire patch, without partial writes."""
    if not isinstance(edits, list) or not 1 <= len(edits) <= 8:
        raise ValueError("Expected 1-8 text edits")
    original, modified = {}, {}
    for edit in edits:
        path = edit["path"]
        if not edit_target(path):
            raise ValueError(f"Forbidden edit: {path}")
        target = root / path
        if target.is_symlink() or not target.resolve().is_relative_to(root.resolve()):
            raise ValueError("Candidate path escapes checkout")
        if path not in original:
            original[path] = modified[path] = target.read_text()
        old, new = edit["old"], edit["new"]
        if not isinstance(old, str) or not old or not isinstance(new, str):
            raise ValueError("Edits require nonempty old text and string new text")
        if modified[path].count(old) != 1:
            raise ValueError(f"Replacement is not unique: {path}")
        modified[path] = modified[path].replace(old, new, 1)
    for path, text in modified.items():
        validate_change(path, original[path], text)
    for path, text in modified.items():
        (root / path).write_text(text)
    return sorted(modified)


def signature(cluster: dict) -> str:
    # Ignore model-authored titles. Include the actual affected identity and location.
    return digest({"targets": sorted(cluster["targets"]),
                   "paths": sorted(cluster["paths"]), "category": cluster["category"]})[:24]


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


def compare(plan: dict[str, list[str]], baseline: dict[str, list[dict]],
            candidate: dict[str, list[dict]]) -> dict:
    """Fail closed on missing, duplicate, ineligible, or unmeasured outcomes."""
    reasons, table, fixed = [], [], []
    for group, keys in plan.items():
        for key in keys:
            pair = []
            for side, rows in (("baseline", baseline.get(key, [])), ("candidate", candidate.get(key, []))):
                if len(rows) != REPEATS or {r.get("repeat") for r in rows} != set(range(1, REPEATS + 1)):
                    reasons.append(f"{key}: incomplete {side} repeats")
                if any(status_fields(r)[0] is None or not status_fields(r)[2] for r in rows):
                    reasons.append(f"{key}: unevaluable {side} outcome")
                pair.append(sum(status_fields(r)[0] is True for r in rows))
            before, after = pair
            table.append({"task": key, "group": group, "baseline": before, "candidate": after})
            if after < before:
                reasons.append(f"{key}: observed regression {before}/3 -> {after}/3")
            if group == "targets" and before <= 1 and after >= 2:
                fixed.append(key)
    if not fixed:
        reasons.append("No reproduced target improved to at least 2/3 passes")
    efficiency = {}
    for metric in ("tool_calls", "total_tokens"):
        medians = []
        for side, data in (("baseline", baseline), ("candidate", candidate)):
            values = [r.get("metrics", {}).get(metric)
                      for keys in plan.values() for key in keys for r in data.get(key, [])]
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

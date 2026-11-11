"""Pure trigger, evidence and issue-quality rules shared by the driver and publisher."""
from __future__ import annotations

import hashlib
import json
import math
from pathlib import PurePosixPath
import re
from typing import Any

from runner.metrics import status_fields
from runner.trace import tool_result_failed

BASE_BRANCH = "main"
TEST_BRANCH_PREFIX = "refs/heads/auto-improve-test/"
# Findings that the runner can reproduce on current main. External code is not executed here.
REPRODUCIBLE_DOMAINS = ("agent", "benchmark")
DOMAINS = (*REPRODUCIBLE_DOMAINS, "external")
# Distinct failing daily runs required when a finding cannot be reproduced on main.
PERSISTENT_RUNS = 2
# New issues filed per run; updates to existing issues are not counted.
DEFAULT_MAX_NEW_ISSUES = 3
# Final run states that settle a source window. Anything else is retried by a later run.
SETTLED_STATUSES = {"completed", "no_failures"}
SYMPTOM_KINDS = ("none", "setup_failure", "judge_error", "environment_failure", "tool_error",
                 "hard_assertion", "error_text")
# A closed issue with one of these labels is a rejected finding; the same tasks are not refiled.
REJECTED_LABELS = {"invalid", "wontfix", "duplicate", "auto-improve:false-positive"}
AGENT_TARGETS = ("skill", "prompt", "tool_description", "go_code")
BENCHMARK_TARGETS = ("benchmark_code", "task_definition")


class InvalidFinding(ValueError):
    """A diagnosed finding fails a mechanical evidence check; it is dropped, never filed."""


def require_main_branch(branch: str) -> None:
    if branch != BASE_BRANCH:
        raise ValueError("Auto-improve only supports main")


def execution_policy(event: str, ref: str, publish_requested: bool = False) -> dict:
    """Branch tests render issues into the artifact; only main may file them."""
    if not ref.startswith("refs/heads/"):
        raise ValueError("Expected a repository branch")
    if event == "schedule":
        if ref != f"refs/heads/{BASE_BRANCH}":
            raise ValueError("Scheduled auto-improve runs only on main")
    elif event == "push":
        if not ref.startswith(TEST_BRANCH_PREFIX):
            raise ValueError("Push tests require an auto-improve-test/ branch")
    elif event == "workflow_dispatch":
        if ref != f"refs/heads/{BASE_BRANCH}" and not ref.startswith(TEST_BRANCH_PREFIX):
            raise ValueError("Manual auto-improve runs require main or a test branch")
    else:
        raise ValueError("Unsupported auto-improve trigger")
    publish = (ref == f"refs/heads/{BASE_BRANCH}" and
               (event == "schedule" or (event == "workflow_dispatch" and publish_requested)))
    return {"publish": publish}


def artifact_name(ref: str, identity: str, status: str) -> str:
    """Only a settled main run marks its sources processed; failed runs stay eligible."""
    if ref != f"refs/heads/{BASE_BRANCH}":
        return f"auto-improve-test-source-{identity}"
    prefix = "auto-improve-source-" if status in SETTLED_STATUSES else "auto-improve-retry-"
    return prefix + identity


def digest(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False).encode()).hexdigest()


def domain_of(cluster: dict) -> str:
    """agent and benchmark findings are reproduced; external code is only evidenced; unknown is dropped."""
    target = cluster.get("fix_target", "go_code")
    if target in AGENT_TARGETS:
        return "agent"
    if target in BENCHMARK_TARGETS:
        return "benchmark"
    return "external" if target == "external_code" else "unknown"


def source_path(path: str) -> bool:
    """A cited code location must be a plain repository-relative path."""
    if not isinstance(path, str) or not path or "\\" in path or len(path) > 300:
        return False
    p = PurePosixPath(path)
    return (not p.is_absolute() and p.as_posix() == path and ".." not in p.parts
            and not any(part.startswith(".") for part in p.parts))


def code_location(value: str) -> tuple[str, int | None] | None:
    """Parse `path` or `path:line`; anything else is not a code citation."""
    match = re.fullmatch(r"([^\s:]+)(?::(\d{1,6}))?", value.strip()) if isinstance(value, str) else None
    if not match or not source_path(match.group(1)):
        return None
    return match.group(1), int(match.group(2)) if match.group(2) else None


def validate_symptom(symptom: dict) -> None:
    """A benchmark finding needs a mechanically observable failure signature to reproduce."""
    if not isinstance(symptom, dict) or symptom.get("kind") not in SYMPTOM_KINDS or symptom["kind"] == "none":
        raise InvalidFinding("Benchmark finding needs an observable symptom")
    pattern = symptom.get("pattern", "")
    if not isinstance(pattern, str) or len(pattern) > 200:
        raise InvalidFinding("Symptom pattern must be a short string")
    if symptom["kind"] in {"tool_error", "hard_assertion", "error_text"} and not pattern:
        raise InvalidFinding(f"Symptom {symptom['kind']} requires a pattern")
    try:
        re.compile(pattern)
    except re.error as exc:
        raise InvalidFinding("Symptom pattern is not a valid regular expression") from exc


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


def reproduction(domain: str, row: dict, symptom: dict | None, traces: list[dict]) -> dict:
    """Classify one fresh attempt on main. Only an evaluable failure (or the symptom) reproduces."""
    success, failure_class, eligible = status_fields(row)
    if domain == "benchmark":
        matches = symptom_matches(symptom or {}, row, traces)
        return {"reproduced": bool(matches), "symptom_excerpts": matches,
                "reason": "symptom observed" if matches else "symptom not observed on main"}
    if success is True:
        return {"reproduced": False, "symptom_excerpts": [], "reason": "passed on main"}
    if success is None or not eligible:
        return {"reproduced": False, "symptom_excerpts": [],
                "reason": f"unevaluable on main ({row.get('status')}, {failure_class})"}
    return {"reproduced": True, "symptom_excerpts": [], "reason": "failed on main"}


def failing_runs(failures: list[dict], targets: list[str]) -> int:
    """Distinct daily runs in the window in which any target failed."""
    by_key = {f["key"]: f for f in failures}
    return len({o["source"] for key in targets for o in by_key.get(key, {}).get("observations", [])
                if status_fields(o)[0] is not True and "source" in o})


def finding_key(domain: str, targets: list[str]) -> dict:
    """Stable identity of a finding for deduplication: what failed, not the model's wording."""
    return {"domain": domain, "targets": sorted(set(targets))}


def issue_metadata(issue: dict, trusted_authors: set[str]) -> dict | None:
    """Read the hidden finding metadata, trusting only issues filed by the pipeline itself."""
    if (issue.get("user") or {}).get("login") not in trusted_authors or issue.get("pull_request"):
        return None
    match = re.search(r"<!-- auto-improve (\{.*?\}) -->", issue.get("body") or "")
    if not match:
        return None
    try:
        meta = json.loads(match.group(1))
    except ValueError:
        return None
    if (not isinstance(meta, dict) or meta.get("domain") not in DOMAINS or not isinstance(meta.get("targets"), list)
            or not all(isinstance(t, str) for t in meta["targets"])):
        return None
    return meta


def rejected(issue: dict) -> bool:
    labels = {label.get("name") if isinstance(label, dict) else label for label in issue.get("labels") or []}
    return issue.get("state") == "closed" and (issue.get("state_reason") == "not_planned" or bool(labels & REJECTED_LABELS))


def plan_issue(key: dict, issues: list[dict], trusted_authors: set[str]) -> dict:
    """Decide whether a confirmed finding opens, updates, or is suppressed by an existing issue.

    An open issue for the same domain that shares a target is the same problem: it gets a
    comment instead of a duplicate. A rejected closed issue covering every target suppresses
    the finding. A completed closed issue is linked as a possible regression.
    """
    targets = set(key["targets"])
    related = []
    for issue in issues:
        meta = issue_metadata(issue, trusted_authors)
        if meta and meta["domain"] == key["domain"] and targets & set(meta["targets"]):
            related.append((issue, set(meta["targets"])))
    for issue, _ in sorted(related, key=lambda item: item[0].get("number", 0)):
        if issue.get("state") == "open":
            return {"action": "comment", "number": issue["number"]}
    for issue, covered in related:
        if rejected(issue) and targets <= covered:
            return {"action": "suppress", "number": issue["number"]}
    regressions = sorted(issue["number"] for issue, _ in related if issue.get("state") == "closed" and not rejected(issue))
    return {"action": "create", "regression_of": regressions}


def rank(finding: dict) -> tuple:
    """Order new issues by evidence strength and impact: reproduced, persistent, broad first."""
    return (-len(finding.get("reproduced_targets", [])), -finding.get("failing_runs", 0),
            -len(finding["key"]["targets"]), finding["title"])


def review_cost_summary(sessions: list[dict]) -> dict:
    """Unknown provider pricing or interrupted calls are not zero-cost sessions."""
    costs = [s.get("total_cost_usd") for s in sessions]
    known = [cost for cost in costs if isinstance(cost, (int, float)) and not isinstance(cost, bool)
             and math.isfinite(cost) and cost >= 0]
    return {"estimated_cost_usd": sum(known) if known else None,
            "measured_sessions": len(known), "total_sessions": len(sessions),
            "complete": bool(sessions) and len(known) == len(sessions)}

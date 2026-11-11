"""Daily results -> diagnosis -> evidence checks -> reproduction on main -> independent audit -> issue drafts.

Nothing here edits code or publishes. The separate publisher files the drafts in
issues.json after rechecking them against the repository's current issues.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time

from ci.auto_improve import evidence, execution, issues, policy, review
from runner.metrics import status_fields

# Finding states that produce an issue action in issues.json.
PUBLISHABLE = {"issue_ready", "update_ready"}


def progress(stage: str) -> None:
    print(f"[auto-improve] {stage}", flush=True)


def write(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(evidence.sanitize(value), indent=2, ensure_ascii=False) + "\n")
    temp.replace(path)


def git(root: Path, *args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=root, text=True,
                                   env={**os.environ, "GIT_EDITOR": "true", "GIT_TERMINAL_PROMPT": "0"}, timeout=120).strip()


def tracked_files(repo: Path) -> set[str]:
    return set(subprocess.check_output(["git", "ls-files", "-z"], cwd=repo, timeout=120).decode().split("\0")) - {""}


def trusted_authors() -> set[str]:
    """Accounts whose issue metadata the pipeline trusts: the workflow token's bot and an optional bot user."""
    extra = os.environ.get("AUTO_IMPROVE_ISSUE_AUTHOR", "")
    return {"github-actions[bot]", *(name.strip() for name in extra.split(",") if name.strip())}


def workspace(out: Path, name: str, repo: Path, payload: dict, *, include_code: bool = True,
              image_root: Path | None = None) -> Path:
    path = out / name
    path.mkdir(parents=True, exist_ok=False)
    if include_code:
        review.stage_code(repo, path, scopes=("agent", "benchmark", "external"))
    # Evidence images must be accessible within restricted file-tool roots.
    def materialize(value):
        if isinstance(value, dict):
            result = {}
            for key, item in value.items():
                if (key in {"pre_image", "post_image"} and isinstance(item, str) and image_root
                        and Path(item).resolve().is_relative_to(image_root.resolve()) and Path(item).is_file()):
                    source = Path(item)
                    target = path / "images" / (policy.digest(item)[:20] + source.suffix)
                    target.parent.mkdir(exist_ok=True)
                    shutil.copyfile(source, target)
                    result[key] = str(target.relative_to(path))
                elif (key == "evidence" and isinstance(item, str) and Path(item).is_dir()
                      and Path(item).resolve().is_relative_to(out.resolve())
                      and Path(item).name == "evidence" and Path(item).parent.name == "1"
                      and Path(item).parents[2].name == "reproduction-runs"):
                    target = path / "attempts" / policy.digest(item)[:20]
                    shutil.copytree(item, target, dirs_exist_ok=True)
                    result[key] = str(target.relative_to(path))
                else:
                    result[key] = materialize(item)
            return result
        if isinstance(value, list):
            return [materialize(item) for item in value]
        return value
    staged = materialize(payload)
    attempts = path / "attempts"
    if attempts.is_dir():
        # Reviewers otherwise see only top-level files and miss per-task traces.
        write(path / "evidence-index.json", {
            str(attempt.relative_to(path)): sorted(str(f.relative_to(path)) for f in attempt.rglob("*") if f.is_file())
            for attempt in sorted(attempts.iterdir()) if attempt.is_dir()})
    if "failures" in staged:
        # Full observations are large; index them and keep each in an addressable file.
        details = path / "details"
        details.mkdir()
        indexed = []
        for number, failure in enumerate(staged["failures"], 1):
            detail_file = f"details/failure-{number:03d}.json"
            write(path / detail_file, failure)
            summary = {key: failure.get(key) for key in
                       ("key", "failure_count", "run_count", "status", "failure_class", "agent_eligible", "rubric",
                        "hard_assertion_failures", "final_response") if key in failure}
            summary["detail_file"] = detail_file
            indexed.append(summary)
        staged["failures"] = indexed
        history = staged.get("history")
        if history is not None:
            history_file = "details/history.json"
            write(path / history_file, history)
            staged["history"] = {"detail_file": history_file}
    write(path / "input.json", staged)
    return path


def ask(out: Path, name: str, repo: Path, payload: dict, prompt: str, schema: dict, deadline: float,
        *, image_root: Path | None = None) -> dict:
    remaining = deadline - time.monotonic()
    if remaining < 30:
        raise TimeoutError("Budget exhausted before review")
    progress(f"review start: {name}")
    stage = workspace(out, name, repo, payload, image_root=image_root)
    try:
        result = review.ask(stage, "Read input.json and source-files.json (exact available code paths), then relevant source/evidence files. " + prompt, schema,
                            timeout=min(900, remaining))
        write(out / f"{name}.json", result)
        progress(f"review completed: {name}")
        return result
    finally:
        error_log = stage / "session-error.log"
        if error_log.is_file():
            (out / f"{name}-error.log").write_text(evidence.sanitize(error_log.read_text(errors="replace")))
        usage = stage / "session-usage.json"
        if usage.is_file():
            write(out / f"{name}-usage.json", evidence.read_json(usage))
        else:
            write(out / f"{name}-usage.json", {"total_cost_usd": None, "subtype": "usage_unavailable"})
        shutil.rmtree(stage)


def compact_issues(feedback: list[dict], trusted: set[str]) -> list[dict]:
    """Earlier findings and their outcome, so diagnosis does not re-propose rejected ones."""
    rows = []
    for issue in feedback:
        meta = policy.issue_metadata(issue, trusted)
        if meta is None:
            continue
        rows.append({"number": issue.get("number"), "title": issue.get("title"), "state": issue.get("state"),
                     "state_reason": issue.get("state_reason"), "domain": meta["domain"], "targets": meta["targets"],
                     "labels": [label.get("name") if isinstance(label, dict) else label for label in issue.get("labels") or []]})
    return rows


DIAGNOSIS_PROMPT = (
    "Read input.json first. It indexes ALL failures; detail_file points to the full task, trace, judge and "
    "image references. Inspect relevant detail files and screenshots when the index alone cannot support a "
    "conclusion. Historical data is optional and on demand. Review every failure and its judge evidence, "
    "cluster supported common root causes, and cite exact case_id:task_id keys as targets. Each cluster "
    "becomes at most one issue for a developer, so only group failures that share one root cause. "
    "Inspect source code before claiming a root cause; source_paths must contain exact existing repository "
    "source files, never details/trace/history files. Use source_paths=[] and fix_target=unknown if the cause "
    "is not established. Inspect supplied images for visual claims; absent images or unresolved "
    "coordinate/fixture alternatives mean insufficient_evidence. Attribute grading or setup problems to the "
    "benchmark (benchmark_code/task_definition) and list those tasks in evaluation_issues; they are never agent "
    "defects. For a benchmark finding, name an observable symptom: setup_failure, judge_error, "
    "environment_failure, tool_error (matching failed tool result text), hard_assertion (matching id/actual), "
    "or error_text (matching metrics error), with a short literal or simple regex. For agent or external_code "
    "use symptom kind=none, pattern=''. Hardware, firmware, app or service code that this benchmark cannot "
    "execute is external_code. existing_issues lists earlier findings and how people resolved them: do not "
    "propose a finding a person closed as not planned or false positive unless new evidence contradicts that "
    "outcome. Each failure detail contains observations from all selected daily runs; prefer persistent causes "
    "and mark a cluster supported only when the evidence establishes its cause. Do not propose code edits.")

AUDIT_PROMPT = (
    "You are the independent auditor deciding whether this finding becomes a GitHub issue. A wrong issue "
    "costs more than a missed one, so try to refute it first. reproduction holds ONE fresh attempt per target "
    "on current main with its evidence directory (see evidence-index.json); failures holds the historical "
    "observations from the daily window. Check, citing evidence: does the failure reproduce for the stated "
    "reason; is it an agent, benchmark or external defect as claimed, or actually evaluator scoping, setup, "
    "environment or model flakiness; do the cited code locations really implement the failing behavior? "
    "Return verdict=confirmed only if the evidence establishes both the failure and its cause; refuted if the "
    "evidence contradicts the finding; otherwise insufficient_evidence. For a confirmed finding, write the issue "
    "for a developer who will fix it locally with a coding agent: a specific title (no prefix), a summary, "
    "the user-visible impact, reproduction notes beyond the standard command (setup, environment, what to "
    "look for), expected and actual behavior, the root cause, code_locations as repository paths with optional "
    ":line, evidence as short citations of trace/judge/screenshot files, and acceptance_criteria that a fix must "
    "meet (for example the target tasks passing at least 2 of 3 attempts). fix_direction is an optional, "
    "non-binding hint; leave it empty when unsure. Do not quote secrets.")


def cite_artifacts(verdict: dict, finding: dict, out: Path) -> dict:
    """Rewrite paths from the audit's temporary workspace into ones a developer can find.

    Fresh attempts are staged as attempts/<digest>; in the uploaded artifact they live
    under finding-N/reproduction-runs/. Historical detail files are not uploaded, so they
    are named by the task whose daily observations they hold.
    """
    replacements = {}
    for record in finding.get("reproduction", {}).values():
        directory = (record.get("row") or {}).get("evidence")
        if directory:
            replacements["attempts/" + policy.digest(directory)[:20]] = Path(directory).relative_to(out).as_posix()
    for index, target in enumerate(finding["key"]["targets"], 1):
        replacements[f"details/failure-{index:03d}.json"] = f"daily-run observations of {target}"
    def rewrite(value):
        if isinstance(value, str):
            for staged, cited in replacements.items():
                value = value.replace(staged, cited)
            return value
        return [rewrite(item) for item in value] if isinstance(value, list) else value
    return {key: rewrite(value) for key, value in verdict.items()}


def screen(cluster: dict, failures: list[dict], evaluation_issues: dict, tracked: set[str]) -> dict:
    """Mechanical evidence checks; raise InvalidFinding rather than file an unsupported issue."""
    domain = policy.domain_of(cluster)
    observed = {f["key"]: f for f in failures}
    targets = list(dict.fromkeys(cluster["targets"]))
    if not targets or any(t not in observed for t in targets):
        raise policy.InvalidFinding("Targets must be failures observed in the daily window")
    if domain == "agent":
        # The diagnosis's own evaluator/environment attributions are binding for agent findings.
        targets = [t for t in targets if t not in evaluation_issues]
        if not targets:
            raise policy.InvalidFinding("Every target was attributed to the evaluator or environment")
        if not all(observed[t]["agent_eligible"] for t in targets):
            raise policy.InvalidFinding("Agent findings need agent-eligible failures")
    if domain == "benchmark":
        policy.validate_symptom(cluster.get("symptom"))
    paths = [p for p in dict.fromkeys(cluster["source_paths"]) if policy.source_path(p) and p in tracked]
    if not paths:
        raise policy.InvalidFinding("Finding cites no existing repository source file")
    if not cluster["evidence"]:
        raise policy.InvalidFinding("Finding cites no evidence")
    return {"domain": domain, "targets": targets, "source_paths": paths}


def run(repo: Path, source_dir: Path, out: Path) -> dict:
    source = evidence.read_json(source_dir / "source.json")
    sources = source.get("sources", [source])
    for entry in sources:
        policy.require_main_branch(entry["run"].get("head_branch", ""))
    identities = [evidence.source_identity(str(s["run"]["id"]), str(s["run"].get("run_attempt", 1))) for s in sources]
    identity = "__".join(identities)
    base = git(repo, "rev-parse", "HEAD")
    # Local/branch tests retain their identity and can never masquerade as a main run.
    execution_ref = os.environ.get("GITHUB_REF", "local")
    if execution_ref != "local" and not execution_ref.startswith("refs/heads/"):
        raise ValueError("Auto-improve execution requires a repository branch")
    required = ["BENCHMARK_AGENT_MODEL", "BENCHMARK_AGENT_PROVIDER", "BENCHMARK_AGENT_BASE_URL",
                "BENCHMARK_AGENT_API_KEY", "BENCHMARK_JUDGE_MODEL", "BENCHMARK_JUDGE_BASE_URL", "BENCHMARK_JUDGE_API_KEY"]
    missing = [name for name in required if not os.environ.get(name)]
    if missing:
        raise ValueError("Missing benchmark configuration: " + ", ".join(missing))
    deadline = time.monotonic() + int(os.environ.get("AUTO_IMPROVE_TIMEOUT_SECONDS", "7200"))
    tasks = evidence.catalog_tasks(repo)
    failures, warnings = evidence.source_failures_window(
        source_dir, [tuple(i.split("-")) for i in identities], tasks)
    # Agent eligibility comes from failed observations only: a passing day says nothing
    # about whether the failing ones were the agent's fault.
    for failure in failures:
        failure["agent_eligible"] = any(o["agent_eligible"] for o in failure["observations"]
                                        if status_fields(o)[0] is not True)
    warnings += [f"Missing artifact for source {s}" for s in source.get("missing_artifacts", [])]
    # Missing artifacts are not part of the identity; dashboard-only runs executed no task.
    window_runs = len(identities) - len(source.get("empty_artifacts", []))
    history = evidence.read_json(source_dir / "history.json") if (source_dir / "history.json").is_file() else {}
    feedback = evidence.read_json(source_dir / "feedback.json") if (source_dir / "feedback.json").is_file() else []
    trusted = trusted_authors()
    max_new = int(os.environ.get("AUTO_IMPROVE_MAX_ISSUES", str(policy.DEFAULT_MAX_NEW_ISSUES)))
    state = {"schema": 2, "source": identity, "source_sha": sources[-1]["run"]["head_sha"],
             "sources": [{"identity": i, "sha": s["run"]["head_sha"], "date": str(s["run"].get("created_at", ""))[:10]}
                         for i, s in zip(identities, sources)],
             "base_sha": base, "base_branch": policy.BASE_BRANCH, "execution_ref": execution_ref,
             "warnings": warnings, "findings": [], "status": "running", "max_new_issues": max_new,
             "empty_sources": source.get("empty_artifacts", []),
             "configuration": {k: v for k, v in execution.benchmark_environment().items()
                               if k.endswith(("_MODEL", "_BASE_URL", "_PROVIDER"))}}
    out.mkdir(parents=True, exist_ok=False)
    write(out / "manifest.json", state)
    write(out / "source-window.json", source)
    images: list[str] = []
    try:
        if not failures:
            state["status"] = "no_failures" if not warnings else "insufficient_evidence"
            return state
        executable = {"isolated", "mobilegym"}
        if os.environ.get("AUTO_IMPROVE_AVD_TEMPLATE") and os.environ.get("AUTO_IMPROVE_AVD_SHA256"):
            executable.add("adb")
        diagnosis = ask(out, "diagnosis", repo, {"failures": failures, "history": history,
                        "existing_issues": compact_issues(feedback, trusted), "configuration": state["configuration"]},
                        DIAGNOSIS_PROMPT, review.DIAGNOSIS, deadline, image_root=source_dir / "benchmark/runs")
        write(out / "diagnosis.json", diagnosis)
        evaluation_issues = {item["task"]: item for item in diagnosis.get("evaluation_issues", [])}
        tracked = tracked_files(repo)
        observed = {f["key"]: f for f in failures}
        findings = state["findings"]
        for number, cluster in enumerate(diagnosis["clusters"], 1):
            finding = {"number": number, "title": cluster["title"], "cluster": cluster, "status": "report_only"}
            findings.append(finding)
            if policy.domain_of(cluster) == "unknown" or cluster["judge_review"] != "supported":
                finding["reason"] = "Diagnosis did not establish a supported cause"
                continue
            try:
                checked = screen(cluster, failures, evaluation_issues, tracked)
            except policy.InvalidFinding as exc:
                finding.update({"status": "invalid", "reason": str(exc)})
                continue
            key = policy.finding_key(checked["domain"], checked["targets"])
            finding.update({"key": key, "source_paths": checked["source_paths"], "signature": policy.digest(key)[:24],
                            "failing_runs": policy.failing_runs(failures, key["targets"]), "window_runs": window_runs,
                            "failures_by_target": {t: policy.failing_runs(failures, [t]) for t in key["targets"]},
                            "tasks": {t: {k: tasks[t][k] for k in ("suite", "environment", "case_id")}
                                      for t in key["targets"] if t in tasks}})
            plan = policy.plan_issue(key, feedback, trusted)
            finding["plan"] = plan
            if plan["action"] == "suppress":
                finding.update({"status": "suppressed", "reason": f"Rejected earlier in #{plan['number']}"})
                continue
            runnable = (checked["domain"] in policy.REPRODUCIBLE_DOMAINS and
                        all(t in tasks and tasks[t]["runnable"] and tasks[t]["environment"] in executable
                            for t in key["targets"]))
            finding["status"] = "reproduce" if runnable else "persistence"
            if not runnable and finding["failing_runs"] < policy.PERSISTENT_RUNS:
                finding.update({"status": "not_persistent",
                                "reason": f"Cannot run on main and failed in only {finding['failing_runs']} daily run(s)"})
        # One fresh attempt per target on current main, sharing one image build.
        pending = [f for f in findings if f["status"] == "reproduce"]
        attempts: dict[tuple[str, bool], dict] = {}
        execution_errors = []
        if pending:
            environments = {f["tasks"][t]["environment"] for f in pending for t in f["key"]["targets"]}
            image = f"aico-auto-improve:{policy.digest(identity)[:20]}"
            images = [image]
            progress("build daemon image for reproduction")
            execution.prepare(repo, image, environments, out / "build", deadline - time.monotonic())
            for finding in pending:
                symptom = finding["cluster"].get("symptom")
                benchmark = finding["key"]["domain"] == "benchmark"
                finding["reproduction"] = {}
                for target in finding["key"]["targets"]:
                    cache = (target, benchmark)
                    if cache not in attempts:
                        progress(f"finding {finding['number']}: reproduce {target}")
                        directory = out / f"finding-{finding['number']}" / "reproduction-runs" / policy.digest(target)[:12] / "1"
                        try:
                            # Benchmark findings must observe judge failures, never hide them by rejudging.
                            row = execution.run_task(repo, target, 1, image, directory, deadline=deadline,
                                                     rejudge=not benchmark)
                        except (ValueError, RuntimeError, OSError, subprocess.SubprocessError, TimeoutError) as exc:
                            execution_errors.append(f"{target}: {exc}")
                            attempts[cache] = {"reproduced": False, "evidence": [], "reason": f"execution failed: {exc}"}
                        else:
                            traces = [evidence.read_json(p) for p in sorted(Path(row["evidence"]).glob("tasks/**/trace.json"))]
                            attempts[cache] = {**policy.reproduction(finding["key"]["domain"], row, symptom, traces),
                                               "row": row}
                    finding["reproduction"][target] = attempts[cache]
                finding["reproduced_targets"] = [t for t, r in finding["reproduction"].items() if r["reproduced"]]
                if finding["reproduced_targets"]:
                    finding["status"] = "persistence"
                else:
                    finding.update({"status": "not_reproduced", "reason": "No target failed for the stated reason on main"})
        for finding in [f for f in findings if f["status"] == "persistence"]:
            payload = {"finding": finding["cluster"], "domain": finding["key"]["domain"],
                       "targets": finding["key"]["targets"], "tasks": {t: tasks.get(t) for t in finding["key"]["targets"]},
                       "failures": [observed[t] for t in finding["key"]["targets"]],
                       "reproduction": finding.get("reproduction", {}),
                       "existing_issues": [row for row in compact_issues(feedback, trusted)
                                           if set(row["targets"]) & set(finding["key"]["targets"])]}
            verdict = ask(out, f"audit-{finding['number']}", repo, payload, AUDIT_PROMPT, review.ISSUE_AUDIT, deadline,
                          image_root=source_dir / "benchmark/runs")
            verdict = cite_artifacts(verdict, finding, out)
            finding["audit"] = {"verdict": verdict["verdict"], "reason": verdict["reason"]}
            locations = []
            for value in verdict["code_locations"]:
                parsed = policy.code_location(value)
                if parsed and parsed[0] in tracked:
                    locations.append(f"{parsed[0]}:{parsed[1]}" if parsed[1] else parsed[0])
            required_text = ("title", "summary", "impact", "expected", "actual", "root_cause")
            if verdict["verdict"] != "confirmed":
                finding.update({"status": verdict["verdict"], "reason": verdict["reason"]})
            elif not locations or not verdict["acceptance_criteria"] or not all(verdict[k].strip() for k in required_text):
                finding.update({"status": "insufficient_evidence",
                                "reason": "Audit confirmed without existing code locations or complete issue content"})
            else:
                finding.update({"status": "confirmed", "issue": verdict, "code_locations": list(dict.fromkeys(locations))})
        drafts = decide(findings, max_new, state, out)
        write(out / "issues.json", drafts)
        if execution_errors:
            # Reproduction infrastructure failed for some targets; do not settle the window on it.
            state.setdefault("warnings", []).extend(execution_errors)
            state["status"] = "blocked"
        else:
            state["status"] = "completed"
        return state
    except BaseException as exc:
        state.update({"status": "interrupted" if isinstance(exc, (InterruptedError, KeyboardInterrupt)) else "error",
                      "error": str(exc)})
        raise
    finally:
        if images:
            warning = execution.remove_images(images)
            if warning:
                state.setdefault("warnings", []).append(warning)
        for finding in state["findings"]:
            for record in finding.get("reproduction", {}).values():
                record.pop("row", None)
        usage = []
        for path in sorted(out.glob("**/*-usage.json")):
            record = evidence.read_json(path)
            usage.append({"stage": path.relative_to(out).as_posix(), **record})
        state["review_usage"] = usage
        state["review_cost"] = policy.review_cost_summary(usage)
        write(out / "manifest.json", state)
        summarize(state, out)


def decide(findings: list[dict], max_new: int, state: dict, out: Path) -> list[dict]:
    """Turn confirmed findings into create/comment drafts, deduplicated and capped."""
    run_url = os.environ.get("AUTO_IMPROVE_RUN_URL", "")
    drafts, claimed = [], []
    confirmed = [f for f in findings if f["status"] == "confirmed"]
    for finding in sorted(confirmed, key=policy.rank):
        key, plan = finding["key"], finding["plan"]
        overlap = next((c for c in claimed if c["key"]["domain"] == key["domain"]
                        and set(c["key"]["targets"]) & set(key["targets"])), None)
        if overlap:
            finding.update({"status": "duplicate", "reason": f"Same tasks as finding {overlap['number']} in this run"})
            continue
        if plan["action"] == "create" and sum(d["action"] == "create" for d in drafts) >= max_new:
            finding.update({"status": "over_limit", "reason": f"More than {max_new} new issues in one run"})
            continue
        finding.update({"status": "update_ready" if plan["action"] == "comment" else "issue_ready",
                        "regression_of": plan.get("regression_of", [])})
        # Both texts are rendered: the publisher rechecks current issues and may switch actions.
        drafts.append({"action": plan["action"], "number": plan.get("number"), "finding": finding["number"],
                       "key": key, "title": issues.title(finding), "labels": issues.labels(finding),
                       "body": issues.body(finding, state, run_url),
                       "observation": issues.observation(finding, state, run_url)})
        claimed.append(finding)
    rendered = out / "issues"
    rendered.mkdir(exist_ok=True)
    for index, draft in enumerate(drafts, 1):
        text = (f"# {draft['title']}\n\n{draft['body']}" if draft["action"] == "create"
                else f"# Comment on #{draft['number']}\n\n{draft['observation']}")
        (rendered / f"{index:02d}-{draft['action']}.md").write_text(text + "\n")
    return drafts


def summarize(state: dict, out: Path) -> None:
    lines = ["# Benchmark auto-improve", "", f"Source: `{state['source']}`; base: `{state['base_sha']}`.", "",
             f"Execution status: **{state['status']}**. New issues: "
             f"{sum(f['status'] == 'issue_ready' for f in state['findings'])}; updates: "
             f"{sum(f['status'] == 'update_ready' for f in state['findings'])}.", ""]
    cost = state.get("review_cost") or {}
    if cost.get("total_sessions"):
        estimate = f"${cost['estimated_cost_usd']:.3f}" if cost["estimated_cost_usd"] is not None else "unavailable"
        lines += [f"Review model estimated cost: {estimate}; reported for {cost['measured_sessions']}/"
                  f"{cost['total_sessions']} sessions. CLI estimate, not a provider invoice.", ""]
    if (out / "diagnosis.json").is_file():
        lines += [evidence.read_json(out / "diagnosis.json")["summary"], ""]
    for finding in state["findings"]:
        lines.append(f"- {finding['title']}: **{finding['status']}**")
        if finding.get("reason"):
            lines.append(f"  {finding['reason']}")
    if state.get("error"):
        lines += ["", f"Error: {state['error']}"]
    (out / "summary.md").write_text("\n".join(lines) + "\n")


def cli() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, required=True)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise InterruptedError(f"Workflow interrupted by signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    state = run(args.repo.resolve(), args.source.resolve(), args.out.resolve())
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
            stream.write(f"ready={sum(f['status'] in PUBLISHABLE for f in state['findings'])}\n")
    return 1 if state["status"] in {"blocked", "error", "interrupted", "insufficient_evidence"} else 0


if __name__ == "__main__":
    raise SystemExit(cli())

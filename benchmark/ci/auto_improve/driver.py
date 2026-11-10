"""Daily-result -> evidence audit -> reproduction -> minimal patch -> verification."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import time

from ci.auto_improve import evidence, execution, policy, review
from runner.environment import MOBILEGYM_SUBMODULE_PATH
from runner.metrics import status_fields


def progress(stage: str) -> None:
    print(f"[auto-improve] {stage}", flush=True)


def write(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(evidence.sanitize(value), indent=2, ensure_ascii=False) + "\n")
    temp.replace(path)


def remove_checkout(path: Path) -> None:
    """Go module caches contain owner-readable but non-writable directories."""
    for root, dirs, _ in os.walk(path, followlinks=False):
        directory = Path(root)
        if not directory.is_symlink():
            directory.chmod(stat.S_IMODE(directory.stat().st_mode) | stat.S_IWUSR | stat.S_IXUSR)
    shutil.rmtree(path)


def git(root: Path, *args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=root, text=True,
                                   env={**os.environ, "GIT_EDITOR": "true", "GIT_TERMINAL_PROMPT": "0"}, timeout=120).strip()


def workspace(out: Path, name: str, repo: Path, payload: dict, *, include_code: bool = True,
              image_root: Path | None = None) -> Path:
    path = out / name
    path.mkdir(parents=True, exist_ok=False)
    if include_code:
        scopes = (("agent", "benchmark", "external") if name == "diagnosis" else
                  ("agent", "benchmark") if payload.get("domain") == "benchmark" else ("agent",))
        extra = tuple(payload.get("allowed_paths", [])) if payload.get("domain") == "benchmark" else ()
        review.stage_code(repo, path, scopes=scopes, extra=extra)
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
                      and Path(item).name == "evidence" and Path(item).parent.name in {"1", "2", "3"}
                      and Path(item).parents[2].name in {"baseline-runs", "candidate-runs"}):
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
    if name == "diagnosis":
        details = path / "details"
        details.mkdir()
        indexed = []
        for number, failure in enumerate(staged.get("failures", []), 1):
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
        previous = staged.get("previous_attempts")
        if previous is not None:
            previous_file = "details/previous-attempts.json"
            write(path / previous_file, previous)
            staged["previous_attempts"] = {"detail_file": previous_file}
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



def checkout(repo: Path, destination: Path, base: str, *, deadline: float) -> None:
    """Materialize the pinned repository without downloading its submodule again.

    The workflow checkout has already fetched the MobileGym commit pinned by
    `base`. Each isolated checkout clones that local copy, then Git verifies the
    gitlink commit is present before checking it out.
    """
    module_source = repo / MOBILEGYM_SUBMODULE_PATH
    if not (module_source / ".git").exists():
        raise RuntimeError(f"Initialize {MOBILEGYM_SUBMODULE_PATH} in the workflow checkout first")
    steps = [
        (["git", "clone", "--shared", "--no-checkout", str(repo), str(destination)], repo, "clone"),
        (["git", "checkout", "--detach", base], destination, "checkout"),
        (["git", "submodule", "init", MOBILEGYM_SUBMODULE_PATH], destination, "submodule-init"),
        (["git", "config", f"submodule.{MOBILEGYM_SUBMODULE_PATH}.url", str(module_source.resolve())],
         destination, "submodule-source"),
        (["git", "-c", "protocol.file.allow=always", "submodule", "update", "--checkout",
          MOBILEGYM_SUBMODULE_PATH], destination, "submodule"),
    ]
    for args, cwd, label in steps:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("Execution budget exhausted while preparing checkout")
        progress(f"prepare {destination.name}: {label}")
        execution.command(args, cwd=cwd, timeout=min(900, remaining),
                          log=destination.parent / f"{destination.name}-{label}.log",
                          env=execution.benchmark_environment())


def require_unmodified(checkout_path: Path) -> None:
    """The shared baseline may hold caches and run output, but never source edits."""
    status = git(checkout_path, "status", "--porcelain", "--untracked-files=no", "--ignore-submodules=none")
    if status:
        raise RuntimeError("Shared baseline checkout has tracked changes")


def validated_source_paths(repo: Path, cluster: dict) -> list[str]:
    """Accept existing agent files, not evidence paths, directories, or escaped paths."""
    paths = cluster.get("source_paths")
    if not isinstance(paths, list) or not paths or len(paths) > 12:
        return []
    checked = []
    for path in paths:
        if not policy.edit_target(path, policy.domain_of(cluster)):
            return []
        target = repo / path
        if target.is_symlink() or not target.is_file() or not target.resolve().is_relative_to(repo.resolve()):
            return []
        checked.append(path)
    return sorted(set(checked))


def audit(out: Path, name: str, repo: Path, payload: dict, deadline: float) -> dict:
    return ask(out, name, repo, payload,
               "Audit ALL listed task outcomes, including successes, against rubric, judge reasons and actual "
               "trace/screenshots/state. evidence-index.json lists every retained file; each attempt directory "
               "holds tasks/<task_id>/trace.json (tool calls, results, memory_state, final_response), judge.json "
               "and history.json. candidate_diff, when present, is the exact change under test. The claims under "
               "audit are comparison.fixed and the absence of regressions; report gaps in other tasks, and treat "
               "them as blocking when they could hide a regression or explain a claimed fix. known_evaluation_issues, "
               "when present, lists tasks the diagnosis already attributed to the evaluator or environment; they are "
               "regression checks, not claimed fixes, and that known issue alone is not an unresolved gap. Still block "
               "when their trace shows candidate behavior worse than baseline. supported=true only "
               "if evidence supports those claims with no unresolved grading or setup gaps. Cite exact evidence. "
               "Do not substitute your verdict for the stored score.",
               review.AUDIT, deadline)


def run(repo: Path, source_dir: Path, out: Path, *, mode: str = "diagnose") -> dict:
    source = evidence.read_json(source_dir / "source.json")
    sources = source.get("sources", [source])
    for entry in sources:
        policy.require_main_branch(entry["run"].get("head_branch", ""))
    identity = "__".join(evidence.source_identity(str(s["run"]["id"]), str(s["run"].get("run_attempt", 1))) for s in sources)
    base = git(repo, "rev-parse", "HEAD")
    # Local/branch tests retain their identity and can never masquerade as a main run.
    execution_ref = os.environ.get("GITHUB_REF", "local")
    if execution_ref != "local" and not execution_ref.startswith("refs/heads/"):
        raise ValueError("Auto-improve execution requires a repository branch")
    if mode == "improve":
        required = ["BENCHMARK_AGENT_MODEL", "BENCHMARK_AGENT_PROVIDER", "BENCHMARK_AGENT_BASE_URL",
                    "BENCHMARK_AGENT_API_KEY", "BENCHMARK_JUDGE_MODEL", "BENCHMARK_JUDGE_BASE_URL", "BENCHMARK_JUDGE_API_KEY"]
        missing = [name for name in required if not os.environ.get(name)]
        if missing:
            raise ValueError("Missing benchmark configuration: " + ", ".join(missing))
    deadline = time.monotonic() + int(os.environ.get("AUTO_IMPROVE_TIMEOUT_SECONDS", "18000"))
    tasks = evidence.catalog_tasks(repo)
    if "sources" in source:
        failures, warnings = evidence.source_failures_window(source_dir,
            [(str(s["run"]["id"]), str(s["run"].get("run_attempt", 1))) for s in sources], tasks)
    else:
        failures, warnings = evidence.source_failures(source_dir, str(source["run"]["id"]),
                                                    str(source["run"].get("run_attempt", 1)), tasks)
    # Source rows remain individually addressable; repeated failures across days are not independent
    # baseline reproductions. Agent eligibility is true only if at least one observation is eligible.
    for failure in failures:
        if "observations" in failure:
            failure["agent_eligible"] = any(o["agent_eligible"] for o in failure["observations"])
    warnings += [f"Missing artifact for source {s}" for s in source.get("missing_artifacts", [])]
    history = evidence.read_json(source_dir / "history.json") if (source_dir / "history.json").is_file() else {}
    feedback = evidence.read_json(source_dir / "feedback.json") if (source_dir / "feedback.json").is_file() else []
    previous = evidence.read_json(source_dir / "previous.json") if (source_dir / "previous.json").is_file() else []
    state = {"schema": 1, "source": identity, "source_sha": sources[-1]["run"]["head_sha"],
             "sources": [{"identity": evidence.source_identity(str(s["run"]["id"]), str(s["run"].get("run_attempt", 1))),
                          "sha": s["run"]["head_sha"]} for s in sources], "base_sha": base,
             "mode": mode, "warnings": warnings, "attempts": [], "status": "running", "base_branch": policy.BASE_BRANCH,
             "execution_ref": execution_ref,
             "configuration": {k: v for k, v in execution.benchmark_environment().items()
                               if k.endswith(("_MODEL", "_BASE_URL", "_PROVIDER"))},
             "avd_fixture": os.environ.get("AUTO_IMPROVE_AVD_SHA256", "")}
    out.mkdir(parents=True, exist_ok=False)
    write(out / "manifest.json", state)
    write(out / "source-window.json", source)
    # Every cluster compares against the same pinned base, so one unmodified
    # checkout serves them all; each candidate keeps its own isolated tree.
    baseline_repo = out / "baseline"
    baseline_ready = False
    try:
        if not failures:
            state["status"] = "no_failures" if not warnings else "insufficient_evidence"
            return state
        executable_environments = {"isolated", "mobilegym"}
        if os.environ.get("AUTO_IMPROVE_AVD_TEMPLATE") and os.environ.get("AUTO_IMPROVE_AVD_SHA256"):
            executable_environments.add("adb")
        diagnosis = ask(out, "diagnosis", repo, {"failures": failures, "history": history, "feedback": feedback,
                        "previous_attempts": previous,
                        "configuration": state["configuration"],
                        "repairable_environments": sorted(executable_environments)},
                        "Read input.json first. It indexes ALL failures; detail_file points to the full task, "
                        "trace, judge and image references. Inspect relevant detail files and screenshots when "
                        "the index alone cannot support a conclusion. Historical data is optional and on demand. "
                        "Review every failure and its judge evidence, cluster supported common root causes, "
                        "and cite exact case_id:task_id keys. List neighbors only when known; the driver expands "
                        "coverage. Inspect source code before claiming a repairable root cause. source_paths must "
                        "contain exact existing repository source files, never details/trace/history files. "
                        "Cite code locations and trace/screenshot locations separately in evidence. Use source_paths=[] "
                        "and fix_target=unknown if the source cause is not established. Inspect supplied images for visual "
                        "claims; absent images or unresolved coordinate/fixture alternatives mean insufficient_evidence. "
                        "Report evaluator/environment issues and missing evidence. Do not propose edits. "
                        "List every failure you attribute to grading/evaluator scoping or setup/environment in "
                        "evaluation_issues. These tasks may be benchmark_code/task_definition targets, never agent targets. "
                        "For benchmark repairs, identify an observable symptom using setup_failure, judge_error, "
                        "environment_failure, tool_error (matching tool result text), hard_assertion (matching id/actual), "
                        "or error_text (matching metrics error). Use a short literal or simple regex pattern. For agent "
                        "or external_code use symptom kind=none, pattern=''. External hardware/app/service code that "
                        "this benchmark cannot verify is external_code: report evidence for an issue, do not repair. "
                        "Review task scenario value; unjustified tasks may be proposed for removal via task_definition. "
                        "Only targets in repairable_environments can be reproduced in this run; keep each cluster "
                        "within one suite when possible and report other causes without making them repair targets. "
                        "Return at most two clusters per repair domain (agent, benchmark), plus supported external_code findings. "
                        "Each failure detail contains observations from all selected daily runs; inspect every observation, "
                        "prioritize persistent causes without dropping one-off failures. ", review.DIAGNOSIS, deadline,
                        image_root=source_dir / "benchmark/runs")
        write(out / "diagnosis.json", diagnosis)
        if mode == "diagnose":
            state["status"] = "diagnosed"
            return state
        executable_tasks = {k: t for k, t in tasks.items()
                            if t["runnable"] and t["environment"] in executable_environments}
        allowed_targets = {f["key"] for f in failures if f["agent_eligible"] and f["key"] in executable_tasks}
        # The diagnosis's own evaluator/environment attributions are binding: such a
        # task can never be a repair target, only an explicit regression check.
        evaluation_issues = {item["task"]: item for item in diagnosis.get("evaluation_issues", [])}
        count = 0
        domain_counts = {d: 0 for d in policy.REPAIR_DOMAINS}
        benchmark_targets = {key for c in diagnosis["clusters"] if policy.domain_of(c) == "benchmark"
                             and c["judge_review"] == "supported" for key in c["targets"]}
        for cluster in diagnosis["clusters"]:
            attempt = {"title": cluster["title"], "status": "report_only", "cluster": cluster}
            state["attempts"].append(attempt)
            plan, images = None, []
            domain = policy.domain_of(cluster)
            attempt["domain"] = domain
            if cluster["judge_review"] != "supported":
                continue
            if domain == "external":
                if (not cluster["targets"] or not set(cluster["targets"]).issubset({f["key"] for f in failures})
                        or not cluster["evidence"] or not cluster["source_paths"]):
                    attempt["reason"] = "External finding lacks observed targets, source paths or evidence"
                    continue
                attempt.update({"status": "issue_ready", "signature": policy.signature(cluster)})
                continue
            if domain not in policy.REPAIR_DOMAINS:
                continue
            if domain == "agent" and set(cluster["targets"]) & benchmark_targets:
                attempt.update({"status": "deferred", "reason": "Benchmark defect affects target; repair evaluation before agent"})
                continue
            quarantined = sorted(set(cluster["targets"]) & set(evaluation_issues))
            if quarantined and domain == "agent":
                attempt["quarantined_targets"] = quarantined
                cluster = {**cluster, "targets": [k for k in cluster["targets"] if k not in evaluation_issues],
                           "neighbors": sorted(set(cluster["neighbors"]) | set(quarantined))}
                # The publisher recomputes the signature from attempt["cluster"], so it must
                # hold the cluster actually repaired; the diagnosed targets stay auditable.
                attempt.update({"cluster": cluster, "diagnosed_targets": sorted(set(cluster["targets"]) | set(quarantined))})
                if not cluster["targets"]:
                    attempt.update({"status": "evaluation_issue",
                                    "reason": "Every target was attributed to the evaluator or environment"})
                    continue
            # Known capability limits are decided before a repair slot, checkout, or model call is spent.
            unavailable = sorted({tasks[k]["environment"] for k in cluster["targets"]
                                  if k in tasks and tasks[k]["environment"] not in executable_environments})
            if unavailable:
                attempt.update({"status": "environment_unavailable",
                                "reason": "Runner cannot reproduce environments: " + ", ".join(unavailable)})
                continue
            domain_targets = allowed_targets if domain == "agent" else {f["key"] for f in failures if f["key"] in executable_tasks}
            if cluster["targets"] and set(cluster["targets"]).issubset(domain_targets):
                try:
                    plan = policy.select_tasks(cluster, executable_tasks,
                                               max_tasks=int(os.environ.get("AUTO_IMPROVE_MAX_TASKS", "32")),
                                               exposed_tasks={f["key"] for f in failures})
                except ValueError as exc:
                    attempt.update({"status": "deferred", "reason": str(exc)})
                    continue
            if domain_counts[domain] >= 2:
                attempt["status"] = "deferred"
                continue
            count += 1
            domain_counts[domain] += 1
            cluster_dir = out / f"cluster-{count}"
            cluster_dir.mkdir()
            try:
                if plan is None:
                    raise ValueError("Repair targets must be observed eligible failures")
                source_paths = validated_source_paths(repo, cluster)
                if domain == "benchmark" and source_paths:
                    suite_paths = ["benchmark/" + tasks[k]["suite"] for k in cluster["targets"]
                                   if k in tasks and tasks[k].get("suite")]
                    source_paths = sorted(set(source_paths + suite_paths))
                    if any(not (repo / path).is_file() or (repo / path).is_symlink() or
                           not (repo / path).resolve().is_relative_to(repo.resolve()) for path in source_paths):
                        source_paths = []
                if not source_paths:
                    attempt["status"] = "no_verified_source_path"
                    continue
                sig = policy.signature(cluster)
                attempt["signature"] = sig
                if any(f"auto-improve:{sig}" in (issue.get("body") or "") for issue in feedback):
                    attempt["status"] = "previously_reviewed"
                    continue
                write(cluster_dir / "plan.json", plan)
                attempt["plan"] = plan
                if domain == "benchmark":
                    from ci.auto_improve.benchmark_repair import repair
                    repair(repo, base, cluster_dir, attempt, tasks, source_paths, deadline)
                    continue
                candidate_repo = cluster_dir / "candidate"
                if not baseline_ready:
                    if baseline_repo.exists():
                        remove_checkout(baseline_repo)
                    checkout(repo, baseline_repo, base, deadline=deadline)
                    baseline_ready = True
                require_unmodified(baseline_repo)
                checkout(repo, candidate_repo, base, deadline=deadline)
                environments = {tasks[k]["environment"] for keys in plan.values() for k in keys}
                image_prefix = f"aico-auto-improve:{policy.digest(identity)[:20]}-{count}"
                images = [image_prefix + "-base", image_prefix + "-candidate"]
                progress(f"cluster {count}: build baseline")
                execution.prepare(baseline_repo, image_prefix + "-base", environments, cluster_dir / "base-build", deadline - time.monotonic())
                # Evidence age starts with the first target run, not with the image build.
                reproduction_started = time.monotonic()
                baseline, candidate = {}, {}
                for key in plan["targets"]:
                    progress(f"cluster {count}: reproduce {key} (3 attempts)")
                    baseline[key] = [execution.run_task(baseline_repo, key, repeat, image_prefix + "-base",
                                     cluster_dir / "baseline-runs" / policy.digest(key)[:12] / str(repeat), deadline=deadline)
                                     for repeat in range(1, 4)]
                write(cluster_dir / "baseline.json", baseline)
                if not any(sum(status_fields(r)[0] is False and status_fields(r)[2] for r in rows) >= 2 for rows in baseline.values()):
                    attempt["status"] = "not_reproduced"
                    continue
                if any(not status_fields(r)[2] or status_fields(r)[0] is None for rows in baseline.values() for r in rows):
                    raise ValueError("Reproduction has unevaluable attempts")
                reproduction_audit = ask(cluster_dir, "reproduction-review", repo,
                    {"historical_hypothesis": cluster, "tasks": {k: tasks[k] for k in plan["targets"]},
                     "baseline": baseline},
                    "Assess EACH target independently using the NEW baseline evidence and current task assertions. "
                    "The historical hypothesis describes a different run; changed outcomes do not invalidate other tasks. "
                    "Classify each task as agent_failure, not_reproduced, environment_or_evaluation, or insufficient_evidence. "
                    "A deterministic assertion failure does not require an LLM rubric or screenshot unless the assertion itself "
                    "needs visual evidence. Old details/failure references are historical and not expected in this workspace. "
                    "Inspect fresh evidence directories and source. Resolve confounding setup/tool failures per task. "
                    "Return exactly one disposition for each target, with its fresh evidence and corrected causal explanation.",
                    review.REPRODUCTION_AUDIT, deadline)
                write(cluster_dir / "reproduction-review.json", reproduction_audit)
                original_targets = list(plan["targets"])
                try:
                    plan, removed = policy.prune_reproduction(plan, reproduction_audit, baseline)
                except ValueError as exc:
                    attempt.update({"status": "grading_disputed", "reason": str(exc)})
                    continue
                attempt["reproduction_dispositions"] = reproduction_audit
                attempt["removed_targets"] = removed["targets"]
                attempt["plan"] = plan
                write(cluster_dir / "plan.json", plan)
                repair_cluster = {**cluster, "targets": plan["targets"],
                                  "cause": reproduction_audit["summary"],
                                  "evidence": [item for task in reproduction_audit["tasks"]
                                               if task["task"] in plan["targets"] for item in task["evidence"]]}
                patch = ask(cluster_dir, "fix", repo, {"cluster": repair_cluster,
                            "baseline": {key: baseline[key] for key in plan["targets"]},
                            "audit": reproduction_audit, "allowed_paths": source_paths},
                            "Propose exact old/new replacements for the smallest fix. Only edit allowed_paths. "
                            "Agent Go implementation changes are allowed under src/agent/internal and src/agent/cmd, "
                            "along with skills. Preserve public interfaces unless the root cause requires a change. "
                            "Do not modify tests, fixtures, benchmark code, build/workflow files or dependency metadata. "
                            "Existing tests and grading must remain intact. Return an empty edits list if no justified fix is possible.", review.PATCH, deadline)
                if not patch["edits"]:
                    attempt["status"] = "no_fix"
                    continue
                if not all(e["path"] in source_paths for e in patch["edits"]):
                    raise ValueError("Patch escapes diagnosed paths")
                changed = policy.apply_edits(candidate_repo, patch["edits"])
                checks = policy.verification_commands(changed)
                for index, check in enumerate(checks):
                    progress(f"cluster {count}: check {' '.join(check)}")
                    resources = cluster_dir / "check-resources.log"
                    execution.resource_snapshot(resources, "before candidate " + " ".join(check))
                    try:
                        execution.command(check, cwd=candidate_repo,
                                          log=cluster_dir / ("check.log" if index == 0 else "go-unit-check.log"),
                                          timeout=deadline - time.monotonic(), env=execution.check_environment())
                    except RuntimeError:
                        execution.resource_snapshot(resources, "after failed candidate " + " ".join(check))
                        # Keep the candidate failure as a gate, and collect the same
                        # check on unmodified code to distinguish existing failures.
                        progress(f"cluster {count}: diagnose failed check on baseline")
                        baseline_log = cluster_dir / ("baseline-check.log" if index == 0 else "baseline-go-unit-check.log")
                        attempt["failed_check"] = check
                        try:
                            execution.command(check, cwd=baseline_repo, log=baseline_log,
                                              timeout=deadline - time.monotonic(), env=execution.check_environment())
                            attempt["baseline_check_passed"] = True
                        except (RuntimeError, OSError, subprocess.SubprocessError) as baseline_error:
                            attempt["baseline_check_passed"] = False
                            attempt["baseline_check_error"] = str(baseline_error)
                        raise
                progress(f"cluster {count}: build candidate")
                execution.prepare(candidate_repo, image_prefix + "-candidate", environments,
                                  cluster_dir / "candidate-build", deadline - time.monotonic())
                if time.monotonic() - reproduction_started > 3600:
                    raise TimeoutError("Target baseline is more than one hour old; rerun instead of comparing stale evidence")
                for group, keys in plan.items():
                    for key in keys:
                        progress(f"cluster {count}: verify {group} task {key} (3 attempts per side)")
                        baseline.setdefault(key, [])
                        candidate[key] = []
                        for repeat in range(1, 4):
                            if key not in original_targets:
                                baseline[key].append(execution.run_task(baseline_repo, key, repeat, image_prefix + "-base",
                                    cluster_dir / "baseline-runs" / policy.digest(key)[:12] / str(repeat), deadline=deadline))
                            candidate[key].append(execution.run_task(candidate_repo, key, repeat, image_prefix + "-candidate",
                                cluster_dir / "candidate-runs" / policy.digest(key)[:12] / str(repeat), deadline=deadline))
                comparison = policy.compare(plan, baseline, candidate)
                write(cluster_dir / "baseline.json", baseline)
                write(cluster_dir / "candidate.json", candidate)
                write(cluster_dir / "comparison.json", comparison)
                attempt["comparison"] = comparison
                if not comparison["passed"]:
                    attempt["status"] = "rejected"
                    continue
                patch_path = cluster_dir / "candidate.patch"
                patch_path.write_text(subprocess.check_output(
                    ["git", "diff", "--binary", "--", *changed], cwd=candidate_repo, text=True))
                verdict = audit(cluster_dir, "verification-review", repo, {"plan": plan, "comparison": comparison,
                    "candidate_diff": patch_path.read_text(),
                    "known_evaluation_issues": [evaluation_issues[k] for keys in plan.values()
                                                for k in keys if k in evaluation_issues],
                    "tasks": {k: tasks[k] for keys in plan.values() for k in keys}, "baseline": baseline, "candidate": candidate}, deadline)
                write(cluster_dir / "verification-review.json", verdict)
                if not verdict["supported"]:
                    attempt["status"] = "grading_disputed"
                    continue
                attempt.update({"status": "ready", "patch": str(patch_path.relative_to(out)),
                                "patch_sha256": policy.digest(patch_path.read_text()), "paths": changed,
                                "audit": verdict, "check_passed": True, "checks": checks,
                                "source_configuration": [o["manifest"] for f in failures if f["key"] in plan["targets"]
                                                         for o in f.get("observations", [f]) if "manifest" in o]})
            except InterruptedError:
                attempt["status"] = "interrupted"
                raise
            except (ValueError, RuntimeError, OSError, subprocess.SubprocessError) as exc:
                attempt.update({"status": "inconclusive", "error": str(exc)})
            finally:
                progress(f"cluster {count}: {attempt['status']}")
                write(out / "manifest.json", state)
                if images:
                    # Per-cluster daemon images are never reused; keep the persistent runner's disk bounded.
                    warning = execution.remove_images(images)
                    if warning:
                        state.setdefault("warnings", []).append(warning)
                    images = []
                owned = cluster_dir / "candidate"
                if owned.exists():
                    try:
                        remove_checkout(owned)
                    except OSError as cleanup_error:
                        state.setdefault("warnings", []).append(f"Cleanup failed for {owned.name}: {cleanup_error}")
                        attempt.update({"status": "inconclusive", "cleanup_error": str(cleanup_error)})
                        progress(f"cluster {count}: cleanup failed for {owned.name}")
        if baseline_repo.exists():
            try:
                remove_checkout(baseline_repo)
            except OSError as cleanup_error:
                state.setdefault("warnings", []).append(f"Cleanup failed for baseline: {cleanup_error}")
                state["status"] = "blocked"
                return state
        state["status"] = ("blocked" if any(a["status"] == "inconclusive" for a in state["attempts"])
                           else "completed")
        return state
    except BaseException as exc:
        state.update({"status": "interrupted" if isinstance(exc, (InterruptedError, KeyboardInterrupt)) else "error",
                      "error": str(exc)})
        raise
    finally:
        usage = []
        for path in sorted(out.glob("**/*-usage.json")):
            record = evidence.read_json(path)
            usage.append({"stage": path.relative_to(out).as_posix(), **record})
        state["review_usage"] = usage
        state["review_cost"] = policy.review_cost_summary(usage)
        write(out / "manifest.json", state)
        lines = ["# Benchmark auto-improve", "", f"Source: `{identity}`; base: `{base}`; mode: `{mode}`.",
                 "", f"Execution status: **{state['status']}**. "
                 f"Validated candidates: {sum(a['status'] == 'ready' for a in state['attempts'])}.", ""]
        if usage:
            cost = state["review_cost"]
            estimate = (f"${cost['estimated_cost_usd']:.3f}" if cost["estimated_cost_usd"] is not None
                        else "unavailable")
            lines.append(f"Review model estimated cost: {estimate}; "
                         f"reported for {cost['measured_sessions']}/{cost['total_sessions']} sessions. "
                         "CLI estimate, not a provider invoice; no spending cap is applied.")
            lines.append("")
        if (out / "diagnosis.json").is_file():
            lines.extend([evidence.read_json(out / "diagnosis.json")["summary"], ""])
        for attempt in state["attempts"]:
            lines.append(f"- {attempt['title']}: **{attempt['status']}**")
            if attempt.get("error"):
                lines.append(f"  {attempt['error']}")
        (out / "summary.md").write_text("\n".join(lines) + "\n")


def cli() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, required=True)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--mode", choices=["diagnose", "improve"], default="diagnose")
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise InterruptedError(f"Workflow interrupted by signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    state = run(args.repo.resolve(), args.source.resolve(), args.out.resolve(), mode=args.mode)
    # A blocked sibling cluster still fails this step, but must not hide a validated candidate:
    # the workflow publishes whenever ready > 0, and the publisher revalidates every one.
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
            stream.write(f"ready={sum(a['status'] in {'ready', 'issue_ready'} for a in state['attempts'])}\n")
    return 1 if state["status"] in {"blocked", "error", "interrupted", "insufficient_evidence"} else 0


if __name__ == "__main__":
    raise SystemExit(cli())

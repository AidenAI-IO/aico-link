"""Daily-result -> evidence audit -> reproduction -> minimal patch -> verification."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time

from ci.auto_improve import evidence, execution, policy, review
from runner.metrics import status_fields


def write(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(".tmp")
    temp.write_text(json.dumps(evidence.sanitize(value), indent=2, ensure_ascii=False) + "\n")
    temp.replace(path)


def git(root: Path, *args: str) -> str:
    return subprocess.check_output(["git", *args], cwd=root, text=True,
                                   env={**os.environ, "GIT_EDITOR": "true", "GIT_TERMINAL_PROMPT": "0"}, timeout=120).strip()


def workspace(out: Path, name: str, repo: Path, payload: dict, *, include_code: bool = True,
              image_root: Path | None = None) -> Path:
    path = out / name
    path.mkdir(parents=True, exist_ok=False)
    if include_code:
        review.stage_code(repo, path)
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
    if name == "diagnosis":
        details = path / "details"
        details.mkdir()
        indexed = []
        for number, failure in enumerate(staged.get("failures", []), 1):
            detail_file = f"details/failure-{number:03d}.json"
            write(path / detail_file, failure)
            summary = {key: failure.get(key) for key in
                       ("key", "status", "failure_class", "agent_eligible", "rubric",
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
    stage = workspace(out, name, repo, payload, image_root=image_root)
    try:
        result = review.ask(stage, "Read input.json and relevant source/evidence files. " + prompt, schema,
                            timeout=min(900, remaining))
        write(out / f"{name}.json", result)
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
    """Materialize the pinned large repository within the overall attempt budget."""
    steps = [
        (["git", "clone", "--shared", "--no-checkout", str(repo), str(destination)], repo, "clone"),
        (["git", "checkout", "--detach", base], destination, "checkout"),
        (["git", "submodule", "update", "--init", "--depth", "1",
          "benchmark/mobilegym/vendor/mobilegym"], destination, "submodule"),
    ]
    for args, cwd, label in steps:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("Execution budget exhausted while preparing checkout")
        execution.command(args, cwd=cwd, timeout=min(900, remaining),
                          log=destination.parent / f"{destination.name}-{label}.log",
                          env=execution.benchmark_environment())


def validated_source_paths(repo: Path, cluster: dict) -> list[str]:
    """Accept existing agent files, not evidence paths, directories, or escaped paths."""
    paths = cluster.get("source_paths")
    if not isinstance(paths, list) or not paths or len(paths) > 12:
        return []
    checked = []
    for path in paths:
        if not policy.edit_target(path):
            return []
        target = repo / path
        if target.is_symlink() or not target.is_file() or not target.resolve().is_relative_to(repo.resolve()):
            return []
        checked.append(path)
    return sorted(set(checked))


def audit(out: Path, name: str, repo: Path, payload: dict, deadline: float) -> dict:
    return ask(out, name, repo, payload,
               "Audit ALL listed task outcomes, including successes, against rubric, judge reasons and actual "
               "trace/screenshots/state. supported=true only if evidence supports the conclusions with no "
               "unresolved grading or setup gaps. Cite exact evidence. Do not substitute your verdict for the stored score.",
               review.AUDIT, deadline)


def run(repo: Path, source_dir: Path, out: Path, *, mode: str = "diagnose") -> dict:
    source = evidence.read_json(source_dir / "source.json")
    policy.require_main_branch(source["run"].get("head_branch", ""))
    identity = evidence.source_identity(str(source["run"]["id"]), str(source["run"].get("run_attempt", 1)))
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
    failures, warnings = evidence.source_failures(source_dir, str(source["run"]["id"]),
                                                str(source["run"].get("run_attempt", 1)), tasks)
    history = evidence.read_json(source_dir / "history.json") if (source_dir / "history.json").is_file() else {}
    feedback = evidence.read_json(source_dir / "feedback.json") if (source_dir / "feedback.json").is_file() else []
    previous = evidence.read_json(source_dir / "previous.json") if (source_dir / "previous.json").is_file() else []
    state = {"schema": 1, "source": identity, "source_sha": source["run"]["head_sha"], "base_sha": base,
             "mode": mode, "warnings": warnings, "attempts": [], "status": "running", "base_branch": policy.BASE_BRANCH,
             "execution_ref": execution_ref,
             "configuration": {k: v for k, v in execution.benchmark_environment().items()
                               if k.endswith(("_MODEL", "_BASE_URL", "_PROVIDER"))},
             "avd_fixture": os.environ.get("AUTO_IMPROVE_AVD_SHA256", "")}
    out.mkdir(parents=True, exist_ok=False)
    write(out / "manifest.json", state)
    try:
        if not failures:
            state["status"] = "no_failures" if not warnings else "insufficient_evidence"
            return state
        diagnosis = ask(out, "diagnosis", repo, {"failures": failures, "history": history, "feedback": feedback,
                        "previous_attempts": previous,
                        "configuration": state["configuration"]},
                        "Read input.json first. It indexes ALL failures; detail_file points to the full task, "
                        "trace, judge and image references. Inspect relevant detail files and screenshots when "
                        "the index alone cannot support a conclusion. Historical data is optional and on demand. "
                        "Review every failure and its judge evidence, cluster supported common root causes, "
                        "and cite exact case_id:task_id keys. List neighbors only when known; the driver expands "
                        "coverage. Inspect source code before claiming a repairable root cause. source_paths must "
                        "contain exact existing agent Go files or SKILL.md paths, never details/trace/history files. "
                        "Cite code locations and trace/screenshot locations separately in evidence. Use source_paths=[] "
                        "and fix_target=unknown if the source cause is not established. Inspect supplied images for visual "
                        "claims; absent images or unresolved coordinate/fixture alternatives mean insufficient_evidence. "
                        "Report evaluator/environment issues and missing evidence. Do not propose edits. "
                        "Return no more than two repairable clusters.", review.DIAGNOSIS, deadline,
                        image_root=source_dir / "benchmark/runs")
        write(out / "diagnosis.json", diagnosis)
        if mode == "diagnose":
            state["status"] = "diagnosed"
            return state
        executable_tasks = {k: t for k, t in tasks.items() if t["runnable"] and t["environment"] in {"isolated", "mobilegym", "adb"}}
        allowed_targets = {f["key"] for f in failures if f["agent_eligible"] and f["key"] in executable_tasks}
        count = 0
        for cluster in diagnosis["clusters"]:
            attempt = {"title": cluster["title"], "status": "report_only", "cluster": cluster}
            state["attempts"].append(attempt)
            if (cluster["judge_review"] != "supported" or cluster["fix_target"] not in {"skill", "prompt", "tool_description", "go_code"}):
                continue
            if count >= 2:
                attempt["status"] = "deferred"
                continue
            count += 1
            cluster_dir = out / f"cluster-{count}"
            cluster_dir.mkdir()
            try:
                if not cluster["targets"] or not set(cluster["targets"]).issubset(allowed_targets):
                    raise ValueError("Repair targets must be observed eligible failures")
                source_paths = validated_source_paths(repo, cluster)
                if not source_paths:
                    attempt["status"] = "no_verified_source_path"
                    continue
                sig = policy.signature(cluster)
                attempt["signature"] = sig
                if any(f"auto-improve:{sig}" in (issue.get("body") or "") for issue in feedback):
                    attempt["status"] = "previously_reviewed"
                    continue
                plan = policy.select_tasks(cluster, executable_tasks,
                                           max_tasks=int(os.environ.get("AUTO_IMPROVE_MAX_TASKS", "32")),
                                           exposed_tasks={f["key"] for f in failures})
                write(cluster_dir / "plan.json", plan)
                attempt["plan"] = plan
                baseline_repo = cluster_dir / "baseline"
                candidate_repo = cluster_dir / "candidate"
                checkout(repo, baseline_repo, base, deadline=deadline)
                checkout(repo, candidate_repo, base, deadline=deadline)
                environments = {tasks[k]["environment"] for keys in plan.values() for k in keys}
                image_prefix = f"aico-auto-improve:{identity}-{count}"
                reproduction_started = time.monotonic()
                execution.prepare(baseline_repo, image_prefix + "-base", environments, cluster_dir / "base-build", deadline - time.monotonic())
                baseline, candidate = {}, {}
                for key in plan["targets"]:
                    baseline[key] = [execution.run_task(baseline_repo, key, repeat, image_prefix + "-base",
                                     cluster_dir / "baseline-runs" / policy.digest(key)[:12] / str(repeat), deadline=deadline)
                                     for repeat in range(1, 4)]
                write(cluster_dir / "baseline.json", baseline)
                if not any(sum(status_fields(r)[0] is False and status_fields(r)[2] for r in rows) >= 2 for rows in baseline.values()):
                    attempt["status"] = "not_reproduced"
                    continue
                if any(not status_fields(r)[2] or status_fields(r)[0] is None for rows in baseline.values() for r in rows):
                    raise ValueError("Reproduction has unevaluable attempts")
                reproduction_audit = audit(cluster_dir, "reproduction-review", repo, {"cluster": cluster,
                                             "tasks": {k: tasks[k] for k in plan["targets"]}, "baseline": baseline}, deadline)
                write(cluster_dir / "reproduction-review.json", reproduction_audit)
                if not reproduction_audit["supported"]:
                    attempt["status"] = "grading_disputed"
                    continue
                patch = ask(cluster_dir, "fix", repo, {"cluster": cluster, "baseline": baseline,
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
                    execution.command(check, cwd=candidate_repo,
                                      log=cluster_dir / ("check.log" if index == 0 else "go-unit-check.log"),
                                      timeout=deadline - time.monotonic(), env=execution.benchmark_environment())
                execution.prepare(candidate_repo, image_prefix + "-candidate", environments,
                                  cluster_dir / "candidate-build", deadline - time.monotonic())
                if time.monotonic() - reproduction_started > 3600:
                    raise TimeoutError("Target baseline is more than one hour old; rerun instead of comparing stale evidence")
                for group, keys in plan.items():
                    for key in keys:
                        baseline.setdefault(key, [])
                        candidate[key] = []
                        for repeat in range(1, 4):
                            if group != "targets":
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
                verdict = audit(cluster_dir, "verification-review", repo, {"plan": plan, "comparison": comparison,
                    "tasks": {k: tasks[k] for keys in plan.values() for k in keys}, "baseline": baseline, "candidate": candidate}, deadline)
                write(cluster_dir / "verification-review.json", verdict)
                if not verdict["supported"]:
                    attempt["status"] = "grading_disputed"
                    continue
                patch_path = cluster_dir / "candidate.patch"
                patch_path.write_text(subprocess.check_output(
                    ["git", "diff", "--binary", "--", *changed], cwd=candidate_repo, text=True))
                attempt.update({"status": "ready", "patch": str(patch_path.relative_to(out)),
                                "patch_sha256": policy.digest(patch_path.read_text()), "paths": changed,
                                "audit": verdict, "check_passed": True, "checks": checks,
                                "source_configuration": [f["manifest"] for f in failures
                                                         if f["key"] in plan["targets"] and "manifest" in f]})
            except InterruptedError:
                attempt["status"] = "interrupted"
                raise
            except (ValueError, RuntimeError, OSError, subprocess.SubprocessError) as exc:
                attempt.update({"status": "inconclusive", "error": str(exc)})
            finally:
                write(out / "manifest.json", state)
                for name in ("baseline", "candidate"):
                    owned = cluster_dir / name
                    if owned.exists():
                        shutil.rmtree(owned)
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
    return 1 if state["status"] in {"blocked", "error", "interrupted", "insufficient_evidence"} else 0


if __name__ == "__main__":
    raise SystemExit(cli())

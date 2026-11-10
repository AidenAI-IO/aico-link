"""Benchmark repairs keep agent source fixed and measure a diagnosed failure symptom, not scores."""
from __future__ import annotations

from pathlib import Path
import subprocess
import time

from ci.auto_improve import evidence, execution, policy, review


def measure(row: dict, symptom: dict) -> dict:
    root = Path(row["evidence"])
    traces = [evidence.read_json(p) for p in sorted(root.glob("tasks/**/trace.json"))]
    return {**row, "symptom_evidence": policy.symptom_matches(symptom, row, traces)}


def repair(repo: Path, base: str, out: Path, attempt: dict, tasks: dict,
           paths: list[str], deadline: float) -> None:
    # Import lazily: driver dispatches here; both share trusted review/staging helpers.
    from ci.auto_improve.driver import ask, checkout, remove_checkout, write
    cluster, plan = attempt["cluster"], attempt["plan"]
    symptom = cluster.get("symptom", {})
    policy.validate_symptom(symptom)
    baseline_repo, candidate_repo = out / "baseline", out / "candidate"
    prefix = "aico-auto-improve:bench-" + policy.digest(str(out))[:20]
    images = [prefix + "-base", prefix + "-candidate"]
    mobile_images = [prefix + "-mobile-base", prefix + "-mobile-candidate"] if "mobilegym" in {
        tasks[k]["environment"] for keys in plan.values() for k in keys} else []
    baseline, candidate = {}, {}
    environments = {tasks[k]["environment"] for keys in plan.values() for k in keys}
    try:
        checkout(repo, baseline_repo, base, deadline=deadline)
        checkout(repo, candidate_repo, base, deadline=deadline)
        execution.prepare(baseline_repo, images[0], environments, out / "base-build", deadline - time.monotonic(),
                          mobilegym_image=mobile_images[0] if mobile_images else None)
        def run(root, image, side, key, repeat):
            # Benchmark repairs must observe judge failures, never hide them by rejudging.
            row = execution.run_task(root, key, repeat, image,
                out / f"{side}-runs" / policy.digest(key)[:12] / str(repeat), deadline=deadline, rejudge=False,
                mobilegym_image=mobile_images[0 if side == "baseline" else 1] if mobile_images else None)
            return measure(row, symptom)
        for key in plan["targets"]:
            baseline[key] = [run(baseline_repo, images[0], "baseline", key, r) for r in range(1, 4)]
        write(out / "baseline.json", baseline)
        if any(sum(bool(r["symptom_evidence"]) for r in rows) < 2 for rows in baseline.values()):
            attempt["status"] = "not_reproduced"
            return
        payload = {"domain": "benchmark", "cluster": cluster, "baseline": baseline,
                   "tasks": {k: tasks[k] for k in plan["targets"]}, "allowed_paths": paths}
        verdict = ask(out, "reproduction-review", repo, payload,
            "Verify the diagnosed benchmark defect against NEW baseline traces and source. The symptom must "
            "reflect an actual evaluation/setup/scenario defect, not just a legitimate agent failure. For task "
            "removal, verify why it has no real-world value. Cite evidence; fail closed if causality is uncertain.",
            review.AUDIT, deadline)
        write(out / "reproduction-review.json", verdict)
        if not verdict["supported"]:
            attempt["status"] = "grading_disputed"
            return
        patch = ask(out, "fix", repo, payload,
            "Propose the smallest benchmark fix using exact old/new replacements only in allowed_paths. "
            "Keep agent code/model/settings fixed. Do not edit tests, this auto-improve pipeline, dependencies "
            "or workflow files. Preserve rubric/assertions/expected by default. If changing them or removing "
            "a task is necessary to represent the real scenario, explain why in summary for human review. "
            "Never merely lower the bar to raise scores. Return no edits if unsupported.", review.PATCH, deadline)
        attempt["repair_reason"] = patch["summary"]
        if not patch["edits"]:
            attempt["status"] = "no_fix"
            return
        if any(e["path"] not in paths for e in patch["edits"]):
            raise ValueError("Patch escapes diagnosed paths")
        # The diagnosed files alone are editable; extra target suites are review context.
        if any(e["path"] not in cluster["source_paths"] for e in patch["edits"]):
            raise ValueError("Benchmark patch escapes diagnosed source files")
        changed = policy.apply_edits(candidate_repo, patch["edits"], "benchmark")
        contracts = [change for p in changed for change in policy.contract_changes(
            p, (baseline_repo / p).read_text(), (candidate_repo / p).read_text())]
        candidate_tasks = evidence.catalog_tasks(candidate_repo)
        removed = sorted(k for keys in plan.values() for k in keys if k not in candidate_tasks)
        if set(removed) - set(plan["targets"]):
            raise ValueError("Benchmark patch removes regression coverage")
        if removed and not contracts:
            raise ValueError("Task removal lacks a recorded contract change")
        attempt.update({"contract_changes": contracts, "removed_tasks": removed})
        agent_diff = subprocess.check_output(["git", "diff", "--name-only", "--", "src/agent"], cwd=candidate_repo, text=True).splitlines()
        if any(not p.startswith("src/agent/cmd/benchmark-mcu/") for p in agent_diff):
            raise ValueError("Benchmark repair changed the agent")
        checks = policy.verification_commands(changed)
        for index, check in enumerate(checks):
            execution.command(check, cwd=candidate_repo, log=out / f"check-{index}.log",
                              timeout=deadline - time.monotonic(), env=execution.check_environment())
        execution.prepare(candidate_repo, images[1], environments, out / "candidate-build", deadline - time.monotonic(),
                          mobilegym_image=mobile_images[1] if mobile_images else None)
        for group, keys in plan.items():
            for key in keys:
                if group != "targets":
                    baseline[key] = [run(baseline_repo, images[0], "baseline", key, r) for r in range(1, 4)]
                candidate[key] = [] if key in removed else [run(candidate_repo, images[1], "candidate", key, r) for r in range(1, 4)]
        comparison = policy.compare_symptom(plan, symptom, baseline, candidate, removed)
        for name, value in (("baseline", baseline), ("candidate", candidate), ("comparison", comparison)):
            write(out / f"{name}.json", value)
        attempt["comparison"] = comparison
        if not comparison["passed"]:
            attempt["status"] = "rejected"
            return
        patch_path = out / "candidate.patch"
        patch_path.write_text(subprocess.check_output(["git", "diff", "--binary", "--", *changed], cwd=candidate_repo, text=True))
        verdict = ask(out, "verification-review", repo,
            {"domain": "benchmark", "allowed_paths": paths, "cluster": cluster, "plan": plan,
             "candidate_diff": patch_path.read_text(), "comparison": comparison, "baseline": baseline,
             "candidate": candidate, "contract_changes": contracts, "repair_reason": patch["summary"]},
            "Audit the benchmark repair. Agent source and settings are unchanged. Confirm the baseline defect "
            "and that the same agent no longer encounters it, not that passing scores increased. Inspect ALL "
            "task traces for new setup/judge/tool errors, including different errors replacing the symptom. "
            "For removed tasks or scoring changes, explicitly assess scenario value and whether this is "
            "just weakening evaluation. Contract changes require human review even when supported=true. "
            "evidence-index.json indexes retained traces. Fail closed on unresolved causal gaps.", review.AUDIT, deadline)
        write(out / "verification-review.json", verdict)
        if not verdict["supported"]:
            attempt["status"] = "grading_disputed"
            return
        attempt.update({"status": "ready", "patch": str(patch_path.relative_to(out.parent)),
                        "patch_sha256": policy.digest(patch_path.read_text()), "paths": changed,
                        "audit": verdict, "check_passed": True, "checks": checks})
    finally:
        warning = execution.remove_images(images + mobile_images)
        if warning:
            attempt["cleanup_warning"] = warning
        for root in (candidate_repo, baseline_repo):
            if root.exists():
                remove_checkout(root)

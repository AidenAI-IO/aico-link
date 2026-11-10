"""Credential-bearing discovery/publication steps, never executed by the model."""
from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.parse

from ci.auto_improve import policy
from ci.auto_improve.driver import git, write
from ci.auto_improve.github import GitHub
from ci.auto_improve.policy import (BASE_BRANCH, compare, digest, execution_policy,
                                    require_main_branch, signature, validate_change,
                                    verification_commands)


def discover(client: GitHub, out: Path, requested: str, branch: str) -> dict | None:
    require_main_branch(branch)
    ids = tuple(part.strip() for part in requested.split(",") if part.strip())
    if any(not part.isdigit() for part in ids):
        raise ValueError("source_run must be one or more comma-separated run IDs")
    window = client.discover(branch, ids, float(os.environ.get("AUTO_IMPROVE_DELAY_HOURS", "3")),
                             include_processed=os.environ.get("AUTO_IMPROVE_TEST_RUN") == "true",
                             limit=int(os.environ.get("AUTO_IMPROVE_WINDOW_LIMIT", "13")))
    if window is None:
        return None
    out.mkdir(parents=True, exist_ok=False)
    runs = out / "benchmark/runs"
    runs.mkdir(parents=True)
    dashboard = None
    for source in window["sources"]:
        download = out / "download" / source["identity"]
        client.download(source["artifact"], download)
        # upload-artifact strips the least common ancestor (benchmark/) from paths.
        root = download / "benchmark" if (download / "benchmark/runs").is_dir() else download
        if not (root / "runs").is_dir():
            raise ValueError(f"Source artifact {source['identity']} contains no benchmark runs")
        # Run directories are prefixed ci-<run>-<attempt>-, so artifacts never collide.
        for run_dir in (root / "runs").iterdir():
            if not run_dir.name.startswith(f"ci-{source['identity']}-") or run_dir.is_symlink():
                continue
            run_dir.rename(runs / run_dir.name)
        if (root / "dashboard-data.json").is_file():
            dashboard = root / "dashboard-data.json"
    if dashboard is not None:
        # Sources are oldest first, so this is the newest run's history.
        dashboard.rename(out / "benchmark/dashboard-data.json")
    shutil.rmtree(out / "download", ignore_errors=True)
    write(out / "source.json", window)
    write(out / "feedback.json", client.issues())
    recent = client.pages("actions/artifacts", "artifacts", cutoff=datetime.now(timezone.utc) - timedelta(days=7))
    previous = []
    for item in recent:
        if item.get("expired") or not item["name"].startswith("auto-improve-source-") or len(previous) >= 7:
            continue
        # Only the manifest/diagnosis are retained as feedback; traces remain run-scoped.
        with tempfile.TemporaryDirectory(prefix="auto-improve-history-") as temp:
            target = Path(temp)
            try:
                client.download(item, target)
                previous.append({"artifact": item["name"],
                                 "manifest": json.loads((target / "manifest.json").read_text()),
                                 "diagnosis": json.loads((target / "diagnosis.json").read_text())
                                              if (target / "diagnosis.json").is_file() else None})
            except (OSError, ValueError) as exc:
                previous.append({"artifact": item["name"], "unavailable": type(exc).__name__})
    write(out / "previous.json", previous)
    dashboard = out / "benchmark/dashboard-data.json"
    write(out / "history.json", json.loads(dashboard.read_text()) if dashboard.is_file() else
          {"status": "unavailable", "reason": "Source artifact has no historical dashboard data"})
    return window


def publish_issue(client: GitHub, manifest: dict, attempt: dict, feedback: list[dict]) -> str:
    """Idempotent issue publication; credentials stay in the publish job."""
    sig = signature(attempt["cluster"])
    if sig != attempt.get("signature") or attempt["cluster"].get("judge_review") != "supported":
        raise ValueError("Issue lacks a supported diagnosis with matching identity")
    marker = f"auto-improve-issue:{sig}"
    for item in feedback:
        if marker in (item.get("body") or ""):
            return item["html_url"]
    cluster = attempt["cluster"]
    body = [f"<!-- {marker} -->", cluster["cause"], "", "## Evidence", *[f"- {e}" for e in cluster["evidence"]],
            "", "## Source window", json.dumps(manifest.get("sources", [manifest["source"]]), indent=2),
            "", f"Artifacts: {os.environ.get('AUTO_IMPROVE_RUN_URL', '')} (14-day retention)",
            "", "## Verification", json.dumps(attempt.get("comparison", {}), indent=2)]
    issue = client.request("issues", {"title": ("[auto-improve] " + attempt["title"])[:240], "body": "\n".join(body)})
    feedback.append(issue)
    return issue["html_url"]


def publish(client: GitHub, repo: Path, artifacts: Path, branch: str) -> list[str]:
    """Revalidate patch scope in a fresh checkout, then create commits through GitHub API."""
    require_main_branch(branch)
    manifest = json.loads((artifacts / "manifest.json").read_text())
    require_main_branch(manifest.get("base_branch", ""))
    if manifest.get("execution_ref") != "refs/heads/main":
        raise ValueError("Branch-test candidates cannot be published to main")
    if not any(a.get("status") in {"ready", "issue_ready"} for a in manifest.get("attempts", [])):
        return []
    base = manifest["base_sha"]
    if git(repo, "rev-parse", "HEAD") != base:
        raise ValueError("Publisher checkout does not match validated base")
    # Do not silently publish against a changed main. A rerun can refresh it.
    if client.request(f"git/ref/heads/{urllib.parse.quote(branch, safe='')}")["object"]["sha"] != base:
        raise ValueError("main advanced; refresh validation before publishing")
    feedback = client.issues()
    urls = []
    published = {d: 0 for d in policy.REPAIR_DOMAINS}
    for attempt in manifest["attempts"]:
        domain = policy.domain_of(attempt["cluster"])
        if attempt.get("status") == "issue_ready" and domain == "external":
            publish_issue(client, manifest, attempt, feedback)
            continue
        if domain not in published or published[domain] >= 2:
            continue
        if attempt.get("status") != "ready":
            continue
        sig = signature(attempt["cluster"])
        if sig != attempt["signature"]:
            raise ValueError("Diagnosis identity changed after verification")
        marker = f"<!-- auto-improve:{sig} source:{manifest['source']} -->"
        if any(item.get("pull_request") and f"auto-improve:{sig}" in (item.get("body") or "") for item in feedback):
            continue
        if not attempt.get("check_passed") or not attempt.get("audit", {}).get("supported") or not attempt.get("comparison", {}).get("passed"):
            raise ValueError("Candidate lacks required validation")
        patch = (artifacts / attempt["patch"]).resolve()
        if not patch.is_relative_to(artifacts.resolve()) or digest(patch.read_text()) != attempt["patch_sha256"]:
            raise ValueError("Candidate patch identity mismatch")
        baseline = json.loads((patch.parent / "baseline.json").read_text())
        candidate = json.loads((patch.parent / "candidate.json").read_text())
        if domain == "benchmark":
            from ci.auto_improve.benchmark_repair import measure
            # Recompute symptom measurements from retained traces, not model-authored flags.
            for data in (baseline, candidate):
                for rows in data.values():
                    for row in rows:
                        recorded = Path(row["evidence"])
                        parts = recorded.parts
                        index = next((i for i, part in enumerate(parts) if part.startswith("cluster-")), None)
                        if index is None:
                            raise ValueError("Invalid benchmark evidence path")
                        local = artifacts.joinpath(*parts[index:]).resolve()
                        if not local.is_relative_to(artifacts.resolve()) or not local.is_dir():
                            raise ValueError("Missing retained benchmark evidence")
                        from ci.auto_improve.evidence import results
                        saved = results(local / "results.jsonl")
                        if len(saved) != 1:
                            raise ValueError("Missing unique retained task result")
                        for field in ("status", "metrics", "hard_assertion_failures"):
                            if saved[0].get(field) != row.get(field):
                                raise ValueError("Benchmark result differs from retained execution")
                        measured = measure({**row, "evidence": str(local)}, attempt["cluster"]["symptom"])
                        row["symptom_evidence"] = measured["symptom_evidence"]
            recomputed = policy.compare_symptom(attempt["plan"], attempt["cluster"]["symptom"], baseline, candidate,
                                                attempt.get("removed_tasks", []))
        else:
            recomputed = compare(attempt["plan"], baseline, candidate)
        if not recomputed["passed"] or recomputed != attempt["comparison"]:
            raise ValueError("Stored acceptance verdict does not match measured task results")
        if not json.loads((patch.parent / "verification-review.json").read_text()).get("supported"):
            raise ValueError("Missing final evidence audit")
        with tempfile.TemporaryDirectory(prefix="auto-improve-publish-") as temp:
            checkout = Path(temp) / "repo"
            subprocess.run(["git", "clone", "--shared", "--no-checkout", str(repo), str(checkout)], check=True, timeout=120)
            git(checkout, "checkout", "--detach", base)
            git(checkout, "apply", "--check", str(patch))
            git(checkout, "apply", str(patch))
            paths = git(checkout, "diff", "--name-only").splitlines()
            if sorted(paths) != sorted(attempt["paths"]):
                raise ValueError("Patch file set does not match validated paths")
            checks = verification_commands(paths)
            if attempt.get("checks") != checks:
                raise ValueError("Candidate lacks the checks required for its changed files")
            tree = []
            contracts = []
            for path in paths:
                mode = git(repo, "ls-tree", base, "--", path).split()[0]
                target = checkout / path
                if mode not in {"100644", "100755"} or target.is_symlink() or not target.is_file():
                    raise ValueError("Only existing regular text files can be published")
                before = subprocess.check_output(["git", "show", f"{base}:{path}"], cwd=repo).decode()
                after = target.read_text()
                validate_change(path, before, after, domain)
                contracts.extend(policy.contract_changes(path, before, after))
                blob = client.request("git/blobs", {"content": after, "encoding": "utf-8"})
                tree.append({"path": path, "mode": mode, "type": "blob", "sha": blob["sha"]})
            if domain == "benchmark":
                if sorted(contracts) != sorted(attempt.get("contract_changes", [])):
                    raise ValueError("Unreported scoring or task-removal changes")
                from ci.auto_improve.evidence import catalog_tasks
                available = catalog_tasks(checkout)
                removed = sorted(k for keys in attempt["plan"].values() for k in keys if k not in available)
                if removed != sorted(attempt.get("removed_tasks", [])):
                    raise ValueError("Reported task removals differ from patch")
            base_tree = client.request(f"git/commits/{base}")["tree"]["sha"]
            new_tree = client.request("git/trees", {"base_tree": base_tree, "tree": tree})
            title = f"fix({domain}): " + attempt["title"][:180].replace("\n", " ")
            commit = client.request("git/commits", {"message": title, "tree": new_tree["sha"], "parents": [base]})
            # A window may include thirteen source IDs; keep a branch component below filesystem limits.
            window_ref = manifest["source"] if len(manifest["source"]) < 100 else digest(manifest["source"])[:24]
            head = f"auto-improve/{window_ref}-{sig}"
            # Never overwrite an existing branch, including a partially published retry.
            try:
                client.request("git/refs", {"ref": f"refs/heads/{head}", "sha": commit["sha"]})
            except Exception:
                existing = client.request(f"git/ref/heads/{head}")
                existing_tree = client.request(f"git/commits/{existing['object']['sha']}")["tree"]["sha"]
                if existing_tree != new_tree["sha"]:
                    raise ValueError("Existing auto-improve branch differs; manual reconciliation required")
                commit["sha"] = existing["object"]["sha"]
            # A previous attempt may have created the PR before losing its response.
            existing_prs = client.pages(f"pulls?state=all&head={urllib.parse.quote(client.repository.split('/')[0] + ':' + head)}")
            if existing_prs:
                continue
            comparison = attempt["comparison"]
            lines = [marker, "## Diagnosis", attempt["cluster"]["cause"], "", "## Evidence",
                     *[f"- {e}" for e in attempt["cluster"]["evidence"]],
                     "", f"Source benchmark window: `{manifest['source']}`; latest revision `{manifest['source_sha']}`.",
                     "```json", json.dumps(manifest.get("sources", []), indent=2), "```",
                     f"Validated base: `{base}`. Candidate: `{commit['sha']}`.",
                     "", "## Paired verification", "| Task | Set | Baseline | Candidate |",
                     "| --- | --- | --- | --- |"]
            if domain == "benchmark":
                issue_url = publish_issue(client, manifest, attempt, feedback)
                lines.extend([f"Related issue: {issue_url}", "", "Same agent; pass-rate changes are informational only.",
                              "Baseline/candidate columns below report symptom counts, not passes."])
                lines.extend(f"| `{r['task']}` | {r['group']} | {r['baseline']['symptom']}/3 | "
                             + ("removed" if r["removed"] else f"{r['candidate']['symptom']}/3") + " |"
                             for r in comparison["tasks"])
                lines.extend(["", "## Human review required: evaluation contract", attempt.get("repair_reason", ""),
                              *[f"- {c}" for c in contracts], "", "```json", json.dumps(comparison, indent=2), "```"])
            else:
                lines.extend(f"| `{r['task']}` | {r['group']} | {r['baseline']}/3 | {r['candidate']}/3 |" for r in comparison["tasks"])
            cost = manifest.get("review_cost", {})
            estimate = (f"${cost['estimated_cost_usd']:.3f}" if cost.get("estimated_cost_usd") is not None
                        else "unavailable")
            lines.extend(["", "Checks passed: " + ", ".join("`" + " ".join(check) + "`" for check in checks) + ".",
                          f"Review model estimated cost: {estimate} "
                          f"({cost.get('measured_sessions', 0)}/{cost.get('total_sessions', 0)} sessions with pricing). "
                          "CLI estimate, not a provider invoice; no model-cost limit imposed.",
                          "Judge evidence audited before and after the change.",
                          "Three repeats are screening evidence, not statistical proof of improvement.",
                          "", "## Configuration and efficiency", "```json",
                          json.dumps({"configuration": manifest["configuration"], "source_configuration": attempt.get("source_configuration"),
                                      "avd_fixture": manifest["avd_fixture"],
                                      "efficiency": comparison.get("efficiency")}, indent=2), "```",
                          "", "Configuration may differ from the source daily run; both validation sides used the recorded settings.",
                          f"Detailed artifacts: {os.environ.get('AUTO_IMPROVE_RUN_URL', '')} (14-day retention).",
                          "No automatic merge. Refresh affected checks if the candidate or base changes."])
            pr = client.request("pulls", {"title": title, "head": head, "base": branch,
                                          "body": "\n".join(lines), "draft": True})
            # The body marker also supports recovery if labeling fails.
            client.request(f"issues/{pr['number']}/labels", {"labels": ["auto-improve"]})
            urls.append(pr["html_url"])
            published[domain] += 1
    return urls


def cli() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["discover", "publish", "policy"])
    parser.add_argument("--out", type=Path)
    parser.add_argument("--repo", type=Path)
    parser.add_argument("--source-run", default="")
    parser.add_argument("--branch", choices=[BASE_BRANCH], default=BASE_BRANCH)
    parser.add_argument("--mode", choices=["diagnose", "improve"], default="diagnose")
    parser.add_argument("--event", default="")
    parser.add_argument("--ref", default="")
    parser.add_argument("--publish-requested", default="false")
    args = parser.parse_args()
    if args.operation == "policy":
        policy = execution_policy(args.event, args.ref, args.mode,
                                  args.publish_requested.lower() == "true")
        policy["artifact_prefix"] = ("auto-improve-source-" if args.ref == "refs/heads/main"
                                     else "auto-improve-test-source-")
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                for key, value in policy.items():
                    stream.write(f"{key if key != 'mode' else 'value'}={str(value).lower() if isinstance(value, bool) else value}\n")
        return 0
    if args.out is None or (args.operation == "publish" and args.repo is None):
        parser.error("discover/publish require --out; publish also requires --repo")
    client = GitHub(os.environ["GITHUB_REPOSITORY"], os.environ["GH_TOKEN"])
    if args.operation == "discover":
        source = discover(client, args.out.resolve(), args.source_run, args.branch)
        outputs = {"found": str(source is not None).lower()}
        if source:
            outputs["identity"] = source["identity"]
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                for key, value in outputs.items():
                    stream.write(f"{key}={value}\n")
    else:
        urls = publish(client, args.repo.resolve(), args.out.resolve(), args.branch)
        print("\n".join(urls) if urls else "No new validated candidate to publish")
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())

"""Credential-bearing discovery/publication steps, never executed by the model."""
from __future__ import annotations

import argparse
import base64
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.parse

from ci.auto_improve import policy
from ci.auto_improve.driver import PUBLISHABLE, git, write
from ci.auto_improve.github import GitHub
from ci.auto_improve.policy import (BASE_BRANCH, compare, digest, execution_policy,
                                    require_main_branch, signature, validate_change,
                                    verification_commands)


# Previous runs are feedback only; never transfer a large evidence artifact for two summaries.
HISTORY_ARTIFACT_BYTES = 64 * 1024 * 1024


def evidence_identity(identity: str) -> str:
    """Artifact names embed only validated numeric source identities."""
    parts = identity.split("__")
    if not identity or any(len(p.split("-")) != 2 or not all(x.isdigit() for x in p.split("-")) for p in parts):
        raise ValueError("Invalid source window identity")
    return identity


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
        # A daily run that failed before any task ran uploads only its dashboard. It stays in
        # the window identity, so it is settled with the rest instead of retried, and the
        # driver reports it as a source without completed suite manifests.
        run_dirs = list((root / "runs").iterdir()) if (root / "runs").is_dir() else []
        if not run_dirs:
            window.setdefault("empty_artifacts", []).append(source["identity"])
        # Run directories are prefixed ci-<run>-<attempt>-, so artifacts never collide.
        for run_dir in run_dirs:
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
    recent = client.auto_improve_artifacts(datetime.now(timezone.utc) - timedelta(days=7))
    previous = []
    for item in recent:
        if (item.get("expired") or not item["name"].startswith(("auto-improve-source-", "auto-improve-retry-"))
                or len(previous) >= 7):
            continue
        if item.get("size_in_bytes", 0) > HISTORY_ARTIFACT_BYTES:
            previous.append({"artifact": item["name"], "unavailable": "too_large"})
            continue
        # Only the manifest/diagnosis are retained as feedback; traces remain run-scoped.
        with tempfile.TemporaryDirectory(prefix="auto-improve-history-") as temp:
            target = Path(temp)
            try:
                client.download(item, target, members={"manifest.json", "diagnosis.json"})
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


def rebased_contents(client: GitHub, head: str, validated: dict[str, tuple[str, str]]) -> dict[str, tuple[str, str]]:
    """Three-way merge each validated (base, candidate) file onto main's current file.

    A conflict means main changed the same lines, so the validated change needs a fresh run.
    """
    merged = {}
    with tempfile.TemporaryDirectory(prefix="auto-improve-rebase-") as temp:
        root = Path(temp)
        for path, (before, after) in validated.items():
            data = client.request(f"contents/{urllib.parse.quote(path)}?ref={head}")
            if data.get("type") != "file" or data.get("encoding") != "base64":
                raise ValueError(f"main no longer has {path} as a regular file; rerun validation")
            current = base64.b64decode(data["content"]).decode()
            for name, text in (("current", current), ("base", before), ("candidate", after)):
                (root / name).write_text(text)
            result = subprocess.run(["git", "merge-file", "-p", "current", "base", "candidate"], cwd=root,
                                    capture_output=True, text=True, timeout=60)
            if result.returncode:
                raise ValueError(f"main advanced and changed {path} where the validated patch applies; rerun validation")
            merged[path] = (current, result.stdout)
    return merged


def retained_rows(artifacts: Path, data: dict, symptom: dict) -> None:
    """Recompute symptom measurements from retained traces, not model-authored flags."""
    from ci.auto_improve.benchmark_repair import measure
    from ci.auto_improve.evidence import results
    for rows in data.values():
        for row in rows:
            parts = Path(row["evidence"]).parts
            index = next((i for i, part in enumerate(parts) if part.startswith("cluster-")), None)
            if index is None:
                raise ValueError("Invalid benchmark evidence path")
            local = artifacts.joinpath(*parts[index:]).resolve()
            if not local.is_relative_to(artifacts.resolve()) or not local.is_dir():
                raise ValueError("Missing retained benchmark evidence")
            saved = results(local / "results.jsonl")
            if len(saved) != 1:
                raise ValueError("Missing unique retained task result")
            for field in ("status", "metrics", "hard_assertion_failures"):
                if saved[0].get(field) != row.get(field):
                    raise ValueError("Benchmark result differs from retained execution")
            measured = measure({**row, "evidence": str(local)}, symptom)
            row.update(symptom_evidence=measured["symptom_evidence"], tool_failures=measured["tool_failures"])


def verified_attempt(artifacts: Path, attempt: dict, domain: str) -> tuple[Path, dict | None]:
    """Recompute every acceptance fact the stored attempt claims; return its patch and comparison."""
    proposal = attempt["status"] == "proposal_ready"
    if proposal and domain != "benchmark":
        raise ValueError("Only benchmark repairs can be published as unexecuted proposals")
    if not proposal and domain == "benchmark" and any(policy.runs_on_host(p) for p in attempt["paths"]):
        raise ValueError("A host-executed benchmark change can only be published as a proposal")
    if not attempt.get("check_passed") or (not proposal and (
            not attempt.get("audit", {}).get("supported") or not attempt.get("comparison", {}).get("passed"))):
        raise ValueError("Candidate lacks required validation")
    patch = (artifacts / attempt["patch"]).resolve()
    if not patch.is_relative_to(artifacts.resolve()) or digest(patch.read_text()) != attempt["patch_sha256"]:
        raise ValueError("Candidate patch identity mismatch")
    baseline = json.loads((patch.parent / "baseline.json").read_text())
    if proposal:
        retained_rows(artifacts, baseline, attempt["cluster"]["symptom"])
        reproduced = {key: sum(bool(r["symptom_evidence"]) for r in rows) for key, rows in baseline.items()}
        if (reproduced != attempt.get("reproduction") or set(reproduced) != set(attempt["plan"]["targets"])
                or any(count < 2 for count in reproduced.values())):
            raise ValueError("Proposal lacks a reproduced symptom on every target")
        if not json.loads((patch.parent / "reproduction-review.json").read_text()).get("supported"):
            raise ValueError("Missing reproduction audit")
        if sorted(p for p in attempt["paths"] if policy.runs_on_host(p)) != sorted(attempt.get("host_paths", [])):
            raise ValueError("Proposal host paths differ from its patch")
        if not attempt.get("host_paths"):
            raise ValueError("A container-only benchmark repair must be executed, not proposed")
        return patch, None
    candidate = json.loads((patch.parent / "candidate.json").read_text())
    if domain == "benchmark":
        for data in (baseline, candidate):
            retained_rows(artifacts, data, attempt["cluster"]["symptom"])
        recomputed = policy.compare_symptom(attempt["plan"], attempt["cluster"]["symptom"], baseline, candidate,
                                            attempt.get("removed_tasks", []))
    else:
        recomputed = compare(attempt["plan"], baseline, candidate)
    if not recomputed["passed"] or recomputed != attempt["comparison"]:
        raise ValueError("Stored acceptance verdict does not match measured task results")
    if not json.loads((patch.parent / "verification-review.json").read_text()).get("supported"):
        raise ValueError("Missing final evidence audit")
    return patch, recomputed


def publish(client: GitHub, repo: Path, artifacts: Path, branch: str) -> list[str]:
    """Revalidate patch scope in a fresh checkout, then create commits through GitHub API.

    If main advanced during the run, a change that still merges cleanly is published
    on top of the current main; the PR states which base its checks and tasks ran on.
    """
    require_main_branch(branch)
    manifest = json.loads((artifacts / "manifest.json").read_text())
    require_main_branch(manifest.get("base_branch", ""))
    if manifest.get("execution_ref") != "refs/heads/main":
        raise ValueError("Branch-test candidates cannot be published to main")
    if not any(a.get("status") in PUBLISHABLE for a in manifest.get("attempts", [])):
        return []
    base = manifest["base_sha"]
    if git(repo, "rev-parse", "HEAD") != base:
        raise ValueError("Publisher checkout does not match validated base")
    head = client.request(f"git/ref/heads/{urllib.parse.quote(branch, safe='')}")["object"]["sha"]
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
        if attempt.get("status") not in {"ready", "proposal_ready"}:
            continue
        proposal = attempt["status"] == "proposal_ready"
        sig = signature(attempt["cluster"])
        if sig != attempt["signature"]:
            raise ValueError("Diagnosis identity changed after verification")
        marker = f"<!-- auto-improve:{sig} source:{manifest['source']} -->"
        if any(item.get("pull_request") and f"auto-improve:{sig}" in (item.get("body") or "") for item in feedback):
            continue
        patch, comparison = verified_attempt(artifacts, attempt, domain)
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
            contracts = []
            validated = {}
            for path in paths:
                mode = git(repo, "ls-tree", base, "--", path).split()[0]
                target = checkout / path
                if mode not in {"100644", "100755"} or target.is_symlink() or not target.is_file():
                    raise ValueError("Only existing regular text files can be published")
                before = subprocess.check_output(["git", "show", f"{base}:{path}"], cwd=repo).decode()
                after = target.read_text()
                validate_change(path, before, after, domain)
                contracts.extend(policy.contract_changes(path, before, after))
                validated[path] = (mode, before, after)
            if domain == "benchmark":
                if sorted(contracts) != sorted(attempt.get("contract_changes", [])):
                    raise ValueError("Unreported scoring or task-removal changes")
                from ci.auto_improve.evidence import catalog_tasks
                available = catalog_tasks(checkout)
                removed = sorted(k for keys in attempt["plan"].values() for k in keys if k not in available)
                if removed != sorted(attempt.get("removed_tasks", [])):
                    raise ValueError("Reported task removals differ from patch")
        if head != base:
            # Same validated change on the current main; the content check reruns on main's files.
            merged = rebased_contents(client, head, {path: (before, after) for path, (_, before, after) in validated.items()})
            for path, (current, after) in merged.items():
                validate_change(path, current, after, domain)
                validated[path] = (validated[path][0], current, after)
        tree = []
        for path, (mode, _, after) in validated.items():
            blob = client.request("git/blobs", {"content": after, "encoding": "utf-8"})
            tree.append({"path": path, "mode": mode, "type": "blob", "sha": blob["sha"]})
        base_tree = client.request(f"git/commits/{head}")["tree"]["sha"]
        new_tree = client.request("git/trees", {"base_tree": base_tree, "tree": tree})
        title = ("proposal" if proposal else "fix") + f"({domain}): " + attempt["title"][:180].replace("\n", " ")
        commit = client.request("git/commits", {"message": title, "tree": new_tree["sha"], "parents": [head]})
        # A window may include thirteen source IDs; keep a branch component below filesystem limits.
        window_ref = manifest["source"] if len(manifest["source"]) < 100 else digest(manifest["source"])[:24]
        head_ref = f"auto-improve/{window_ref}-{sig}"
        # Never overwrite an existing branch, including a partially published retry.
        try:
            client.request("git/refs", {"ref": f"refs/heads/{head_ref}", "sha": commit["sha"]})
        except Exception:
            existing = client.request(f"git/ref/heads/{head_ref}")
            existing_tree = client.request(f"git/commits/{existing['object']['sha']}")["tree"]["sha"]
            if existing_tree != new_tree["sha"]:
                raise ValueError("Existing auto-improve branch differs; manual reconciliation required")
            commit["sha"] = existing["object"]["sha"]
        # A previous attempt may have created the PR before losing its response.
        existing_prs = client.pages(f"pulls?state=all&head={urllib.parse.quote(client.repository.split('/')[0] + ':' + head_ref)}")
        if existing_prs:
            continue
        base_note = (f"Validated base: `{base}`. Candidate: `{commit['sha']}`." if head == base else
                     f"Validated base: `{base}`. `main` advanced to `{head}` during the run; the same patch "
                     f"applied cleanly and is committed on top of it as `{commit['sha']}`. Checks and paired tasks "
                     "ran on the validated base; let CI check the new base before merging.")
        lines = [marker, "## Diagnosis", attempt["cluster"]["cause"], "", "## Evidence",
                 *[f"- {e}" for e in attempt["cluster"]["evidence"]],
                 "", f"Source benchmark window: `{manifest['source']}`; latest revision `{manifest['source_sha']}`.",
                 "```json", json.dumps(manifest.get("sources", []), indent=2), "```", base_note, ""]
        if domain == "benchmark":
            issue_url = publish_issue(client, manifest, attempt, feedback)
            lines.extend([f"Related issue: {issue_url}", ""])
        if proposal:
            lines.extend(["## Not executed: human verification required",
                          "This repair changes benchmark code that runs on the self-hosted runner, which holds model "
                          "credentials and the Docker socket. Model-authored host code is never executed "
                          "automatically, so the paired symptom comparison was not run.",
                          "", "Host-executed files:", *[f"- `{p}`" for p in attempt["host_paths"]],
                          "", "Baseline symptom reproduction (retained traces, recomputed by the publisher):",
                          "| Task | Baseline symptom |", "| --- | --- |",
                          *[f"| `{k}` | {v}/3 |" for k, v in sorted(attempt["reproduction"].items())],
                          "", "Before merging, review the diff for anything beyond the stated repair, then run the "
                          "target tasks and confirm the symptom is gone.", ""])
        else:
            lines.extend(["## Paired verification", "| Task | Set | Baseline | Candidate |", "| --- | --- | --- | --- |"])
            if domain == "benchmark":
                lines[-3:-3] = ["Same agent; pass-rate changes are informational only.",
                                "Baseline/candidate columns below report symptom counts, not passes."]
                lines.extend(f"| `{r['task']}` | {r['group']} | {r['baseline']['symptom']}/3 | "
                             + ("removed" if r["removed"] else f"{r['candidate']['symptom']}/3") + " |"
                             for r in comparison["tasks"])
            else:
                lines.extend(f"| `{r['task']}` | {r['group']} | {r['baseline']}/3 | {r['candidate']}/3 |"
                             for r in comparison["tasks"])
        if domain == "benchmark":
            lines.extend(["", "## Human review required: evaluation contract", attempt.get("repair_reason", ""),
                          *[f"- {c}" for c in contracts]])
            if comparison is not None:
                lines.extend(["", "```json", json.dumps(comparison, indent=2), "```"])
        cost = manifest.get("review_cost", {})
        estimate = (f"${cost['estimated_cost_usd']:.3f}" if cost.get("estimated_cost_usd") is not None
                    else "unavailable")
        lines.extend(["", "Checks passed: " + ", ".join("`" + " ".join(check) + "`" for check in checks) + ".",
                      f"Review model estimated cost: {estimate} "
                      f"({cost.get('measured_sessions', 0)}/{cost.get('total_sessions', 0)} sessions with pricing). "
                      "CLI estimate, not a provider invoice; no model-cost limit imposed."])
        if not proposal:
            lines.extend(["Judge evidence audited before and after the change.",
                          "Three repeats are screening evidence, not statistical proof of improvement."])
        lines.extend(["", "## Configuration and efficiency", "```json",
                      json.dumps({"configuration": manifest["configuration"], "source_configuration": attempt.get("source_configuration"),
                                  "avd_fixture": manifest["avd_fixture"],
                                  "efficiency": (comparison or {}).get("efficiency")}, indent=2), "```",
                      "", "Configuration may differ from the source daily run; both validation sides used the recorded settings.",
                      f"Detailed artifacts: {os.environ.get('AUTO_IMPROVE_RUN_URL', '')} (14-day retention).",
                      "No automatic merge. Refresh affected checks if the candidate or base changes."])
        pr = client.request("pulls", {"title": title, "head": head_ref, "base": branch,
                                      "body": "\n".join(lines), "draft": True})
        # The body marker also supports recovery if labeling fails.
        client.request(f"issues/{pr['number']}/labels", {"labels": ["auto-improve"]})
        urls.append(pr["html_url"])
        published[domain] += 1
    return urls


def cli() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["discover", "publish", "policy", "artifact"])
    parser.add_argument("--out", type=Path)
    parser.add_argument("--repo", type=Path)
    parser.add_argument("--source-run", default="")
    parser.add_argument("--branch", choices=[BASE_BRANCH], default=BASE_BRANCH)
    parser.add_argument("--mode", choices=["diagnose", "improve"], default="diagnose")
    parser.add_argument("--event", default="")
    parser.add_argument("--ref", default="")
    parser.add_argument("--publish-requested", default="false")
    parser.add_argument("--identity", default="")
    args = parser.parse_args()
    if args.operation == "artifact":
        # A missing or unreadable manifest means the run did not settle its sources.
        manifest = args.out / "manifest.json" if args.out else None
        try:
            status = json.loads(manifest.read_text()).get("status", "") if manifest else ""
        except (OSError, ValueError):
            status = ""
        name = policy.artifact_name(args.ref, evidence_identity(args.identity), status)
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                stream.write(f"name={name}\n")
        print(name)
        return 0
    if args.operation == "policy":
        decision = execution_policy(args.event, args.ref, args.mode,
                                    args.publish_requested.lower() == "true")
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                for key, value in decision.items():
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

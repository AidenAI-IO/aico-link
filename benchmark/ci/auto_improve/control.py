"""Credential-bearing discovery/publication steps, never executed by the model."""
from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import tempfile
import urllib.parse

from ci.auto_improve.driver import git, write
from ci.auto_improve.github import GitHub
from ci.auto_improve.policy import (BASE_BRANCH, compare, digest, execution_policy,
                                    require_main_branch, signature, validate_change,
                                    verification_commands)


def discover(client: GitHub, out: Path, requested: str, branch: str) -> dict | None:
    require_main_branch(branch)
    source = client.discover(branch, requested, float(os.environ.get("AUTO_IMPROVE_DELAY_HOURS", "3")),
                             include_processed=os.environ.get("AUTO_IMPROVE_TEST_RUN") == "true")
    if source is None:
        return None
    out.mkdir(parents=True, exist_ok=False)
    download = out / "download"
    client.download(source["artifact"], download)
    # upload-artifact strips the least common ancestor (benchmark/) from paths.
    root = download / "benchmark" if (download / "benchmark/runs").is_dir() else download
    if not (root / "runs").is_dir():
        raise ValueError("Source artifact contains no benchmark runs")
    root.rename(out / "benchmark")
    if download.exists() and not any(download.iterdir()):
        download.rmdir()
    write(out / "source.json", source)
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
    return source


def publish(client: GitHub, repo: Path, artifacts: Path, branch: str) -> list[str]:
    """Revalidate patch scope in a fresh checkout, then create commits through GitHub API."""
    require_main_branch(branch)
    manifest = json.loads((artifacts / "manifest.json").read_text())
    require_main_branch(manifest.get("base_branch", ""))
    if manifest.get("execution_ref") != "refs/heads/main":
        raise ValueError("Branch-test candidates cannot be published to main")
    if not any(a.get("status") == "ready" for a in manifest.get("attempts", [])):
        return []
    base = manifest["base_sha"]
    if git(repo, "rev-parse", "HEAD") != base:
        raise ValueError("Publisher checkout does not match validated base")
    # Do not silently publish against a changed main. A rerun can refresh it.
    if client.request(f"git/ref/heads/{urllib.parse.quote(branch, safe='')}")["object"]["sha"] != base:
        raise ValueError("main advanced; refresh validation before publishing")
    feedback = client.issues()
    today = datetime.now(timezone.utc).date().isoformat()
    published_today = sum(bool(item.get("pull_request")) and str(item.get("created_at", "")).startswith(today)
                          for item in feedback)
    urls = []
    for attempt in manifest["attempts"]:
        if published_today + len(urls) >= 2:
            break
        if attempt.get("status") != "ready":
            continue
        sig = signature(attempt["cluster"])
        if sig != attempt["signature"]:
            raise ValueError("Diagnosis identity changed after verification")
        marker = f"<!-- auto-improve:{sig} source:{manifest['source']} -->"
        if any(f"auto-improve:{sig}" in (item.get("body") or "") for item in feedback):
            continue
        if not attempt.get("check_passed") or not attempt.get("audit", {}).get("supported") or not attempt.get("comparison", {}).get("passed"):
            raise ValueError("Candidate lacks required validation")
        patch = (artifacts / attempt["patch"]).resolve()
        if not patch.is_relative_to(artifacts.resolve()) or digest(patch.read_text()) != attempt["patch_sha256"]:
            raise ValueError("Candidate patch identity mismatch")
        recomputed = compare(attempt["plan"], json.loads((patch.parent / "baseline.json").read_text()),
                             json.loads((patch.parent / "candidate.json").read_text()))
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
            for path in paths:
                mode = git(repo, "ls-tree", base, "--", path).split()[0]
                target = checkout / path
                if mode != "100644" or target.is_symlink() or not target.is_file():
                    raise ValueError("Only existing regular text files can be published")
                before = subprocess.check_output(["git", "show", f"{base}:{path}"], cwd=repo).decode()
                after = target.read_text()
                validate_change(path, before, after)
                blob = client.request("git/blobs", {"content": after, "encoding": "utf-8"})
                tree.append({"path": path, "mode": "100644", "type": "blob", "sha": blob["sha"]})
            base_tree = client.request(f"git/commits/{base}")["tree"]["sha"]
            new_tree = client.request("git/trees", {"base_tree": base_tree, "tree": tree})
            title = "fix(agent): " + attempt["title"][:180].replace("\n", " ")
            commit = client.request("git/commits", {"message": title, "tree": new_tree["sha"], "parents": [base]})
            head = f"auto-improve/{manifest['source']}-{sig}"
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
                     "", f"Source benchmark: `{manifest['source']}` at `{manifest['source_sha']}`.",
                     f"Validated base: `{base}`. Candidate: `{commit['sha']}`.",
                     "", "## Paired verification", "| Task | Set | Baseline | Candidate |",
                     "| --- | --- | --- | --- |"]
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
                                      "efficiency": comparison["efficiency"]}, indent=2), "```",
                          "", "Configuration may differ from the source daily run; both validation sides used the recorded settings.",
                          f"Detailed artifacts: {os.environ.get('AUTO_IMPROVE_RUN_URL', '')} (14-day retention).",
                          "No automatic merge. Refresh affected checks if the candidate or base changes."])
            pr = client.request("pulls", {"title": title, "head": head, "base": branch,
                                          "body": "\n".join(lines), "draft": True})
            # The body marker also supports recovery if labeling fails.
            client.request(f"issues/{pr['number']}/labels", {"labels": ["auto-improve"]})
            urls.append(pr["html_url"])
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

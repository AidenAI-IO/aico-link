"""Credential-bearing discovery and issue publication, never executed by the model."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil

from ci.auto_improve import policy
from ci.auto_improve.driver import trusted_authors, write
from ci.auto_improve.github import GitHub
from ci.auto_improve.policy import BASE_BRANCH, execution_policy, require_main_branch


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
    # Earlier findings and their resolution feed diagnosis and deduplication.
    write(out / "feedback.json", client.issues())
    dashboard = out / "benchmark/dashboard-data.json"
    write(out / "history.json", json.loads(dashboard.read_text()) if dashboard.is_file() else
          {"status": "unavailable", "reason": "Source artifact has no historical dashboard data"})
    return window


def valid_draft(draft: dict) -> bool:
    """Drafts come from the trusted driver; still refuse anything malformed before writing."""
    key = draft.get("key") or {}
    if key.get("domain") not in policy.DOMAINS or not key.get("targets"):
        return False
    meta = policy.issue_metadata({"user": {"login": "pipeline"}, "body": draft.get("body") or ""}, {"pipeline"})
    return (meta is not None and policy.finding_key(meta["domain"], meta["targets"]) == key
            and isinstance(draft.get("title"), str) and draft["title"].startswith("[auto-improve]")
            and isinstance(draft.get("observation"), str)
            and draft.get("labels") == ["auto-improve"])


def publish(client: GitHub, artifacts: Path, branch: str) -> list[str]:
    """File or update issues from issues.json, rechecking each against current issues first.

    The recheck makes retries idempotent and covers issues filed or closed since discovery.
    """
    require_main_branch(branch)
    manifest = json.loads((artifacts / "manifest.json").read_text())
    require_main_branch(manifest.get("base_branch", ""))
    if manifest.get("execution_ref") != f"refs/heads/{BASE_BRANCH}":
        raise ValueError("Only findings from main can be published")
    drafts_file = artifacts / "issues.json"
    drafts = json.loads(drafts_file.read_text()) if drafts_file.is_file() else []
    if not drafts:
        return []
    current = client.issues()
    trusted = trusted_authors()
    max_new = int(manifest.get("max_new_issues", policy.DEFAULT_MAX_NEW_ISSUES))
    marker = f"auto-improve-observation:{manifest['source']}"
    urls, created = [], 0
    for draft in drafts:
        if not valid_draft(draft):
            raise ValueError("Malformed issue draft")
        plan = policy.plan_issue(draft["key"], current, trusted)
        if plan["action"] == "suppress":
            continue
        if plan["action"] == "comment":
            issue = next(i for i in current if i.get("number") == plan["number"])
            if marker in (issue.get("body") or "") or any(marker in (c.get("body") or "") for c in issue.get("discussion", [])):
                continue
            comment = client.request(f"issues/{plan['number']}/comments", {"body": draft["observation"]})
            issue.setdefault("discussion", []).append(comment)
            urls.append(comment.get("html_url", issue.get("html_url", "")))
            continue
        if created >= max_new:
            continue
        issue = client.request("issues", {"title": draft["title"], "body": draft["body"], "labels": draft["labels"]})
        # Later drafts in this run deduplicate against the issue just filed.
        current.append({**issue, "user": issue.get("user") or {}, "state": "open"})
        created += 1
        urls.append(issue["html_url"])
    return urls


def cli() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=["discover", "publish", "policy", "artifact"])
    parser.add_argument("--out", type=Path)
    parser.add_argument("--source-run", default="")
    parser.add_argument("--branch", choices=[BASE_BRANCH], default=BASE_BRANCH)
    parser.add_argument("--event", default="")
    parser.add_argument("--ref", default="")
    parser.add_argument("--publish-requested", default="false")
    parser.add_argument("--identity", default="")
    parser.add_argument("--publish", default="true")
    args = parser.parse_args()
    if args.operation == "artifact":
        # A missing or unreadable manifest means the run did not settle its sources.
        manifest = args.out / "manifest.json" if args.out else None
        try:
            status = json.loads(manifest.read_text()).get("status", "") if manifest else ""
        except (OSError, ValueError):
            status = ""
        marker = policy.processed_marker(args.ref, evidence_identity(args.identity), status,
                                         args.publish.lower() == "true")
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                stream.write(f"marker={marker}\n")
        print(marker or "window stays eligible")
        return 0
    if args.operation == "policy":
        decision = execution_policy(args.event, args.ref, args.publish_requested.lower() == "true")
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                stream.write(f"publish={str(decision['publish']).lower()}\n")
        return 0
    if args.out is None:
        parser.error("discover/publish require --out")
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
        urls = publish(client, args.out.resolve(), args.branch)
        print("\n".join(urls) if urls else "No new or updated issue to publish")
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())

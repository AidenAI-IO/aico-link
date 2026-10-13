#!/usr/bin/env python3
"""Release decisions use immutable, published channel records as their baseline."""

from datetime import datetime, timedelta, timezone
import fnmatch
import hashlib
import json
from pathlib import Path
import re
import subprocess

CHANNELS = ("dev", "staging", "prod")
VERSION = r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
TAG = re.compile(r"(dev|staging|prod)-v(" + VERSION + r")\Z")
POLICY = Path(__file__).with_name("policy.json")
CONFIG_BOUNDARY = json.loads((Path(__file__).resolve().parents[1] / "debian-system/config-package.json").read_text())


def command(*args, cwd=None):
    return subprocess.check_output(args, cwd=cwd, text=True, timeout=300).strip()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def digest(value):
    return hashlib.sha256(canonical(value)).hexdigest()


def version_tuple(value):
    if not re.fullmatch(VERSION, value):
        raise ValueError(f"Invalid release version: {value}")
    return tuple(map(int, value.split(".")))


def validate_record(record):
    if record["format"] != 1 or record["channel"] not in CHANNELS:
        raise ValueError("Unknown release record format/channel")
    if record.get("runtime_config", 0) not in (0, 1):
        raise ValueError("Unknown runtime configuration ownership version")
    version_tuple(record["version"])
    if record["tag"] != f"{record['channel']}-v{record['version']}":
        raise ValueError("Release tag/version mismatch")
    if record["kind"] not in ("business", "ota", "none"):
        raise ValueError("Unknown release kind")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*", record["repo"]):
        raise ValueError("Invalid repository")
    for name in ("source_commit", "source_tree"):
        if not re.fullmatch(r"[0-9a-f]{40}", record[name]):
            raise ValueError(f"Invalid {name}")
    if set(record["fingerprints"]) != {"business", "system"}:
        raise ValueError("Missing business/system fingerprints")
    for fingerprint in record["fingerprints"].values():
        if not re.fullmatch(r"[0-9a-f]{64}", fingerprint):
            raise ValueError("Invalid source fingerprint")
    platform = record["platform"]
    if not re.fullmatch(r"[0-9a-f]{40}", platform["source_commit"]):
        raise ValueError("Invalid platform source commit")
    if type(platform["contract"]) is not int or platform["contract"] < 1:
        raise ValueError("Platform contract must be a positive integer")
    base = TAG.fullmatch(platform["base_release"])
    if not base or base[1] != record["channel"]:
        raise ValueError("Platform base belongs to another channel")
    if record["kind"] == "ota" and platform != {
        "contract": platform["contract"], "base_release": record["tag"],
        "source_commit": record["source_commit"],
    }:
        raise ValueError("OTA must establish its own platform base")
    datetime.strptime(record["build_time"], "%Y-%m-%dT%H:%M:%SZ")


def validate_history(records, repo):
    tags, versions, contracts = set(), set(), set()
    by_tag = {record["tag"]: record for record in records}
    for record in records:
        validate_record(record)
        if record["repo"] != repo or record["kind"] == "none":
            raise ValueError("Invalid published release record")
        if record["tag"] in tags or record["version"] in versions:
            raise ValueError("Duplicate release tag/version")
        tags.add(record["tag"])
        versions.add(record["version"])
        if record["kind"] == "ota":
            if record["platform"]["contract"] in contracts:
                raise ValueError("Platform contract was allocated more than once")
            contracts.add(record["platform"]["contract"])
        else:
            base = by_tag.get(record["platform"]["base_release"])
            if not base or base["kind"] != "ota" or base["platform"] != record["platform"]:
                raise ValueError("Business release has no valid OTA base")
            if base["fingerprints"]["system"] != record["fingerprints"]["system"]:
                raise ValueError("Business release changed the system fingerprint")
            if base.get("runtime_config", 0) != record.get("runtime_config", 0):
                raise ValueError("Package ownership changes require a new OTA base")
    ordered = sorted(records, key=lambda item: version_tuple(item["version"]))
    previous = {}
    for record in ordered:
        baseline = previous.get(record["channel"])
        if record["previous_tag"] != (baseline["tag"] if baseline else None):
            raise ValueError("Published release history is incomplete or forked")
        if record["previous_commit"] != (baseline["source_commit"] if baseline else None):
            raise ValueError("Published previous commit does not match its release")
        if record["kind"] == "business" and (not baseline or record["platform"] != baseline["platform"]):
            raise ValueError("Business release does not use the channel's latest OTA base")
        previous[record["channel"]] = record
    return ordered


def github_releases(repo):
    pages = json.loads(command("gh", "api", "--paginate", "--slurp",
                               f"repos/{repo}/releases?per_page=100"))
    return [release for page in pages for release in page]


def github_history(repo):
    records = []
    for release in github_releases(repo):
        if release["draft"] or not TAG.fullmatch(release["tag_name"]):
            continue
        assets = [a for a in release["assets"] if a["name"] == "release.json"]
        if len(assets) != 1 or assets[0]["size"] > 2 * 1024 * 1024:
            raise ValueError(f"Missing/invalid release.json: {release['tag_name']}")
        record = json.loads(command("gh", "api", "-H", "Accept: application/octet-stream",
                                    f"repos/{repo}/releases/assets/{assets[0]['id']}"))
        sha = command("gh", "api", f"repos/{repo}/commits/{release['tag_name']}", "--jq", ".sha")
        if record["tag"] != release["tag_name"] or record["source_commit"] != sha:
            raise ValueError("Published release record does not match its Git tag")
        records.append(record)
    return validate_history(records, repo)


def history_digest(records):
    return digest(sorted(records, key=lambda item: item["tag"]))


def classify(path, policy):
    if path.startswith("overlay-debian/"):
        relative = path.removeprefix("overlay-debian/")
        if (any(fnmatch.fnmatchcase(relative, p) for p in CONFIG_BOUNDARY["include"])
                and not any(fnmatch.fnmatchcase(relative, p) for p in CONFIG_BOUNDARY["exclude"])):
            return "config"
    shipped_content = ("overlay-debian/", "src/agent/config/skills/",
                       "src/agent/internal/agent/web_ui/", "src/config_web/web/",
                       "assets/business/")
    if ((path.endswith(".md") or path == ".gitignore" or path.endswith("/.gitignore"))
            and not path.startswith(shipped_content)):
        return "ignore"
    for kind in ("ignore", "system", "business"):
        if any(fnmatch.fnmatchcase(path, pattern) for pattern in policy[kind]):
            return kind
    return policy["default"]


def source_fingerprints(root, commit, policy):
    raw = subprocess.check_output(["git", "ls-tree", "-rz", "--full-tree", commit], cwd=root)
    groups = {"business": [], "system": []}
    for entry in raw.split(b"\0"):
        if not entry:
            continue
        metadata, path = entry.decode().split("\t", 1)
        kind = classify(path, policy)
        if kind == "config":
            kind = "business"
        if kind != "ignore":
            groups[kind].append([path, metadata])
    return {kind: digest(entries) for kind, entries in groups.items()}


def ota_requirement(root, source_commit, log_range, system_paths, previous, policy):
    """Find the first system change and whether more follow, beyond the log limit."""
    first = None
    if not system_paths:
        return {"commit": source_commit, "more": False}
    for commit in command("git", "rev-list", "--reverse", "--topo-order", log_range, cwd=root).splitlines():
        raw = subprocess.check_output([
            "git", "diff-tree", "--root", "-m", "--first-parent", "--no-commit-id",
            "--name-only", "--no-renames", "-r", "-z", commit, "--",
        ], cwd=root)
        if not system_paths.intersection(p.decode() for p in raw.split(b"\0") if p):
            continue
        # A squash/rebase may repeat the already published system tree.
        if previous and source_fingerprints(root, commit, policy)["system"] == previous["fingerprints"]["system"]:
            continue
        if first:
            return {"commit": first, "more": True}
        first = commit
    return {"commit": first or source_commit, "more": False}


def make_plan(root, repo, channel, records, ref="HEAD", version=None, force_ota=False):
    if channel not in CHANNELS:
        raise ValueError("Choose dev, staging or prod")
    records = validate_history(records, repo)
    policy = json.loads(POLICY.read_text())
    commit = command("git", "rev-parse", "--verify", "--end-of-options", f"{ref}^{{commit}}", cwd=root)
    fingerprints = source_fingerprints(root, commit, policy)
    previous = next((r for r in reversed(records) if r["channel"] == channel), None)
    if previous:
        # A squash merge or rebase can preserve released content without
        # preserving its commit as an ancestor. Compare the two release trees,
        # using ancestry only to reject older commits and unrelated histories.
        if command("git", "rev-parse", "--is-shallow-repository", cwd=root) == "true":
            command("git", "fetch", "--unshallow", "origin", previous["source_commit"], commit, cwd=root)
        common = subprocess.run(["git", "merge-base", previous["source_commit"], commit],
                                cwd=root, capture_output=True, text=True, timeout=300)
        if common.returncode == 1:
            raise ValueError(f"Release source is unrelated to {previous['tag']}; choose a source with shared Git history")
        common.check_returncode()
        if common.stdout.strip() == commit and commit != previous["source_commit"]:
            raise ValueError(f"Release source is older than {previous['tag']}; choose the current source")
        raw = subprocess.check_output(["git", "diff", "--name-only", "--no-renames", "-z",
                                       previous["source_commit"], commit, "--"], cwd=root)
        paths = [p.decode() for p in raw.split(b"\0") if p]
        previous_current_policy = source_fingerprints(root, previous["source_commit"], policy)
        kind = "none"
        if fingerprints["business"] != previous_current_policy["business"]:
            kind = "business"
        if fingerprints["system"] != previous_current_policy["system"]:
            kind = "ota"
        if previous.get("runtime_config", 0) != 1:
            kind = "ota"
    else:
        raw = subprocess.check_output(["git", "ls-tree", "-rz", "--name-only", commit], cwd=root)
        paths = [p.decode() for p in raw.split(b"\0") if p]
        kind = "ota"
    if force_ota:
        kind = "ota"
    if previous and kind != "ota":
        fingerprints["system"] = previous["fingerprints"]["system"]
    latest = max([version_tuple(r["version"]) for r in records] + [(0, 0, 1)])
    next_version = version or f"{latest[0]}.{latest[1]}.{latest[2] + 1}"
    if version_tuple(next_version) <= latest:
        raise ValueError("Version must exceed every published channel version (and 0.0.1)")
    tag = f"{channel}-v{next_version}"
    if kind == "ota":
        contract = max([r["platform"]["contract"] for r in records] + [0]) + 1
        platform = {"contract": contract, "base_release": tag, "source_commit": commit}
    else:
        platform = dict(previous["platform"])
    log_range = f"{previous['source_commit']}..{commit}" if previous else commit
    commits = command("git", "log", "--format=%h %s", "-100", log_range, cwd=root).splitlines()
    build_time = datetime.now(timezone.utc)
    if records:
        last_time = max(datetime.strptime(r["build_time"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
                        for r in records)
        build_time = max(build_time, last_time + timedelta(seconds=1))
    plan = {
        "format": 1, "runtime_config": 1, "repo": repo, "channel": channel, "version": next_version, "tag": tag,
        "kind": kind, "source_commit": commit,
        "source_tree": command("git", "rev-parse", f"{commit}^{{tree}}", cwd=root),
        "build_time": build_time.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "previous_tag": previous["tag"] if previous else None,
        "previous_commit": previous["source_commit"] if previous else None,
        "history_digest": history_digest(records), "fingerprints": fingerprints,
        "platform": platform, "force_ota": force_ota,
        "changes": {k: [p for p in paths if classify(p, policy) == k]
                    for k in ("business", "config", "system", "ignore")},
        "commits": commits,
    }
    if kind == "ota":
        plan["ota_requirement"] = ota_requirement(
            root, commit, log_range, set(plan["changes"]["system"]),
            {**previous, "fingerprints": previous_current_policy} if previous else None, policy)
    validate_record(plan)
    return plan


def environment(plan):
    validate_record(plan)
    return {
        "AIDEN_BUSINESS_VERSION": plan["version"], "AIDEN_BUSINESS_REVISION": "1",
        "AIDEN_PLATFORM_CONTRACT": str(plan["platform"]["contract"]),
        "AIDEN_PLATFORM_BASE": plan["platform"]["base_release"],
        "AIDEN_RELEASE_CHANNEL": plan["channel"],
        "AIDEN_SYSTEM_FINGERPRINT": plan["fingerprints"]["system"],
        "OTA_REPO": plan["repo"], "OTA_CHANNEL": plan["channel"],
        "OTA_BUILD_VERSION": plan["tag"], "OTA_BUILD_TIME": plan["build_time"],
        "OTA_BASE_URL": f"https://github.com/{plan['repo']}/releases/download/{plan['tag']}",
    }

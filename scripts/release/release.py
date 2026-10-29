#!/usr/bin/env python3
"""Plan, build, verify and publish dev/staging/prod releases."""

import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

from plan import (CHANNELS, canonical, command, environment, github_history, github_releases,
                  history_digest, make_plan, validate_history, validate_record, version_tuple)

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("business_release", ROOT / "scripts/debian-package/release.py")
business = importlib.util.module_from_spec(spec)
spec.loader.exec_module(business)
IMAGE_ASSETS = {"boot_a.img.tar.gz", "boot_b.img.tar.gz", "rootfs.img.tar.gz",
                "update.img.tar.gz", "update.img.sha256", "manifest.json", "ota-public-key.pem"}


def read_json(path):
    return json.loads(path.read_text())


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def file_hash(path):
    with path.open("rb") as stream:
        hasher = hashlib.sha256()
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def notes(plan):
    text = (f"Channel: `{plan['channel']}`; kind: `{plan['kind']}`.\n\n"
            f"Source: `{plan['source_commit']}`. Previous release: `{plan['previous_tag'] or 'none'}`.\n\n"
            f"Platform contract: `{plan['platform']['contract']}`; base OTA: `{plan['platform']['base_release']}`.\n\n")
    if plan["previous_tag"]:
        text += f"[Full comparison](https://github.com/{plan['repo']}/compare/{plan['previous_tag']}..{plan['tag']})\n\n"
    if plan["kind"] == "ota":
        requirement = plan.get("ota_requirement", {})
        commit = requirement.get("commit", plan["source_commit"])
        more = " (and more)" if requirement.get("more") else ""
        text += (f"Commit [`{commit[:8]}`](https://github.com/{plan['repo']}/commit/{commit})"
                 f"{more} requires OTA update.\n\n")
        text += "Install the signed boot + rootfs OTA together, or flash update.img. This establishes a new base contract.\n\n"
    elif plan["kind"] == "business":
        text += "Install the .deb with apt on the matching channel/base. Previously running services are stopped and restored by dpkg.\n\n"
    else:
        text += "No runtime changes; no build, version allocation or publication.\n\n"
    text += "## Commits (up to 100)\n\n" + "\n".join(f"- {c}" for c in plan["commits"]) + "\n"
    return text


def check_checkout(plan, allow_dirty_sdk=False):
    if command("git", "rev-parse", "HEAD", cwd=ROOT) != plan["source_commit"]:
        raise ValueError("Checkout differs from the release plan")
    args = ["git", "status", "--porcelain"]
    if allow_dirty_sdk:
        args.extend(["--", ".", ":(exclude)pico-sdk"])
    if command(*args, cwd=ROOT):
        raise ValueError("Release builds require committed, clean source and SDK")


def verify_ota(directory, plan):
    manifest = read_json(directory / "manifest.json")
    if (manifest["schema_version"], manifest["version"], manifest["channel"], manifest["build_time"]) != (
            2, plan["tag"], plan["channel"], plan["build_time"]):
        raise ValueError("OTA manifest does not match the release plan")
    if sorted(p["name"] for p in manifest["parts"]) != ["boot", "rootfs"]:
        raise ValueError("OTA must contain both boot and rootfs")
    found = set()
    for part in manifest["parts"]:
        expected_keys = {"asset_a", "asset_b"} if part["name"] == "boot" else {"asset"}
        if {key for key in part if key.startswith("asset")} != expected_keys:
            raise ValueError("Invalid OTA partition assets")
        names = {"asset_a": "boot_a.img.tar.gz", "asset_b": "boot_b.img.tar.gz", "asset": "rootfs.img.tar.gz"}
        for key in expected_keys:
            asset = part[key]
            filename = asset["name"]
            if filename != names[key]:
                raise ValueError("Unexpected OTA asset name")
            found.add(filename)
            path = directory / filename
            if asset["size"] != path.stat().st_size or asset["sha256"] != file_hash(path):
                raise ValueError("OTA asset checksum/size mismatch")
            if asset["url"] != environment(plan)["OTA_BASE_URL"] + "/" + filename:
                raise ValueError("OTA asset URL is not pinned to this immutable release")
    if len(found) != 3:
        raise ValueError("OTA assets are incomplete")
    signature = manifest["signature"].pop("value")
    if manifest["signature"]["algorithm"] != "ed25519":
        raise ValueError("Expected an Ed25519 OTA signature")
    try:
        signature_bytes = bytes.fromhex(signature)
    except ValueError:
        signature_bytes = b""
    if len(signature_bytes) != 64:
        signature_bytes = base64.b64decode(signature, validate=True)
    if len(signature_bytes) != 64:
        raise ValueError("Invalid Ed25519 signature size")
    with tempfile.TemporaryDirectory() as temporary:
        temporary = Path(temporary)
        (temporary / "message").write_bytes(canonical(manifest))
        (temporary / "signature").write_bytes(signature_bytes)
        subprocess.run(["openssl", "pkeyutl", "-verify", "-pubin", "-rawin", "-inkey",
                        str(directory / "ota-public-key.pem"), "-in", str(temporary / "message"),
                        "-sigfile", str(temporary / "signature")], check=True, capture_output=True)


def verify(directory):
    record = read_json(directory / "release.json")
    validate_record(record)
    if record["kind"] == "none":
        raise ValueError("A no-change plan cannot be published")
    package = f"aico-business_{record['version']}-1_armhf.deb"
    expected = {package, "release-manifest.json", "platform-contract.json", "RELEASE-NOTES.md"}
    if record["kind"] == "ota":
        expected |= IMAGE_ASSETS
    if set(record["assets"]) != expected:
        raise ValueError("Release asset inventory is incomplete or unexpected")
    if {p.name for p in directory.iterdir()} != expected | {"release.json", "SHA256SUMS"}:
        raise ValueError("Release directory contains missing or unexpected assets")
    for name in expected | {"release.json", "SHA256SUMS"}:
        path = directory / name
        if path.is_symlink() or not path.is_file():
            raise ValueError("Release assets must be regular files")
        if name in expected and record["assets"][name] != {"size": path.stat().st_size, "sha256": file_hash(path)}:
            raise ValueError(f"Release asset was modified: {name}")
    expected_sums = "".join(f"{file_hash(directory / n)}  {n}\n" for n in sorted(expected | {"release.json"}))
    if (directory / "SHA256SUMS").read_text() != expected_sums:
        raise ValueError("SHA256SUMS mismatch")
    package_path = directory / package
    for field, value in (("Package", "aico-business"), ("Architecture", "armhf"), ("Version", record["version"] + "-1")):
        if command("dpkg-deb", "--field", str(package_path), field) != value:
            raise ValueError(f"Package {field} mismatch")
    embedded = business.package_manifest(package_path)
    if embedded != (directory / "release-manifest.json").read_bytes():
        raise ValueError("Package manifest differs from release asset")
    manifest = json.loads(embedded)
    if manifest.get("runtime_config", 0) != record.get("runtime_config", 0):
        raise ValueError("Runtime configuration ownership differs from release plan")
    business.verify_runtime_config(package_path, record.get("runtime_config", 0))
    platform = read_json(directory / "platform-contract.json")
    if platform.get("runtime_config", 0) != record.get("runtime_config", 0):
        raise ValueError("Platform runtime configuration ownership differs from release plan")
    contract = record["platform"]["contract"]
    required = manifest["required_platform_contract"]
    if (not isinstance(required, dict)
            or any(type(required.get(k)) is not int for k in ("min", "max_exclusive"))
            or required != {"min": contract, "max_exclusive": contract + 1}):
        raise ValueError("Package contract range differs from the plan")
    if (manifest["business_release"], manifest["package_revision"]) != (record["version"], "1"):
        raise ValueError("Package manifest version mismatch")
    if (type(platform.get("platform_contract")) is not int
            or type(manifest.get("platform", {}).get("platform_contract")) is not int
            or manifest.get("platform") != platform or any(platform.get(k) != v for k, v in {
        "platform_contract": record["platform"]["contract"], "base_release": record["platform"]["base_release"],
        "channel": record["channel"], "system_fingerprint": record["fingerprints"]["system"],
    }.items())):
        raise ValueError("Package/platform binding differs from the release plan")
    if record["kind"] == "ota":
        verify_ota(directory, record)
    return record


def stage(plan, destination):
    validate_record(plan)
    # BSP build overlays tracked SDK configs, but the application tree must stay clean.
    check_checkout(plan, allow_dirty_sdk=True)
    metadata = ROOT / "output/debian-apps/apps/metadata"
    if (metadata / "hardware-demo-commit.txt").read_text().strip() != plan["source_commit"]:
        raise ValueError("Applications were built from a different commit")
    for name in ("hardware-demo-status.txt", "pico-sdk-status.txt"):
        if (metadata / name).read_text().strip():
            raise ValueError("Applications were built from dirty source")
    # Vendored SDK provenance is the firmware commit, not the SDK subtree ID.
    if (metadata / "pico-sdk-commit.txt").read_text().strip() != plan["source_commit"]:
        raise ValueError("Applications were built with a different SDK commit")
    if "status=pass" not in (ROOT / "output/debian-apps/apps-audit/summary.txt").read_text().splitlines():
        raise ValueError("Application audit has not passed")
    destination.mkdir(parents=True, exist_ok=True)
    if any(destination.iterdir()):
        raise ValueError("Use an empty release staging directory")
    package = f"aico-business_{plan['version']}-1_armhf.deb"
    source = ROOT / ("output/debian-system" if plan["kind"] == "ota" else "output/debian-package")
    shutil.copyfile(source / package, destination / package)
    manifest = business.package_manifest(destination / package)
    (destination / "release-manifest.json").write_bytes(manifest)
    write_json(destination / "platform-contract.json", json.loads(manifest)["platform"])
    if plan["kind"] == "ota":
        if "Audit passed" not in (source / "audit-report.txt").read_text().splitlines():
            raise ValueError("System image audit has not passed")
        if read_json(source / "expected-contract.json") != json.loads(manifest)["platform"]:
            raise ValueError("Rootfs contract differs from the business package")
        for name in IMAGE_ASSETS - {"ota-public-key.pem"}:
            shutil.copyfile(ROOT / "output/debian/image" / name, destination / name)
        shutil.copyfile(os.environ["OTA_PUBLIC_KEY_PATH"], destination / "ota-public-key.pem")
    (destination / "RELEASE-NOTES.md").write_text(notes(plan))
    record = dict(plan)
    record["assets"] = {p.name: {"sha256": file_hash(p), "size": p.stat().st_size}
                        for p in sorted(destination.iterdir())}
    write_json(destination / "release.json", record)
    (destination / "SHA256SUMS").write_text("".join(
        f"{file_hash(p)}  {p.name}\n" for p in sorted(destination.iterdir())))
    verify(destination)


def assert_current(record, history):
    if history_digest(history) != record["history_digest"]:
        raise ValueError("Published history changed during the build; create a new plan")
    validate_history([*history, record], record["repo"])
    latest = max([version_tuple(r["version"]) for r in history] + [(0, 0, 1)])
    if version_tuple(record["version"]) <= latest:
        raise ValueError("Release version is no longer newer than published versions")
    if record["kind"] == "ota":
        contract = max([r["platform"]["contract"] for r in history] + [0]) + 1
        if record["platform"]["contract"] != contract:
            raise ValueError("OTA contract is not the next global contract")


def gh_retry(*args):
    for attempt in range(3):
        result = subprocess.run(["gh", *args], timeout=1800)
        if result.returncode == 0:
            return
        if attempt < 2:
            time.sleep(5)
    raise ValueError(f"GitHub operation failed: {args[0]}")


def publish(directory):
    record = verify(directory)
    repo, tag, commit = record["repo"], record["tag"], record["source_commit"]
    assert_current(record, github_history(repo))
    existing = next((r for r in github_releases(repo) if r["tag_name"] == tag), None)
    if existing and not existing["draft"]:
        raise ValueError("Published releases are immutable")
    refs = json.loads(command("gh", "api", f"repos/{repo}/git/matching-refs/tags/{tag}"))
    if any(ref["ref"] == "refs/tags/" + tag for ref in refs):
        if command("gh", "api", f"repos/{repo}/commits/{tag}", "--jq", ".sha") != commit:
            raise ValueError("Existing release tag points to a different commit")
    else:
        command("gh", "api", "--method", "POST", f"repos/{repo}/git/refs",
                "-f", "ref=refs/tags/" + tag, "-f", "sha=" + commit)
    if existing:
        assets = [a for a in existing["assets"] if a["name"] == "release.json"]
        if assets:
            draft = json.loads(command("gh", "api", "-H", "Accept: application/octet-stream",
                                       f"repos/{repo}/releases/assets/{assets[0]['id']}"))
            if draft != record:
                raise ValueError("Draft has different build assets; resume with the original artifacts")
    else:
        command("gh", "release", "create", tag, "--repo", repo, "--target", commit,
                "--title", "Aico Link " + tag, "--notes-file", str(directory / "RELEASE-NOTES.md"),
                "--draft", "--latest=false")
    gh_retry("release", "upload", tag, str(directory / "release.json"), "--repo", repo, "--clobber")
    for asset in sorted(directory.iterdir()):
        if asset.name != "release.json":
            gh_retry("release", "upload", tag, str(asset), "--repo", repo, "--clobber")
    with tempfile.TemporaryDirectory() as temporary:
        gh_retry("release", "download", tag, "--repo", repo, "--dir", temporary)
        if verify(Path(temporary)) != record:
            raise ValueError("Uploaded release differs from the local build")
    assert_current(record, github_history(repo))
    # Only prod firmware can move /releases/latest, used by legacy OTA clients.
    command("gh", "release", "edit", tag, "--repo", repo, "--draft=false",
            "--prerelease=" + str(record["channel"] != "prod").lower(),
            "--latest=" + str(record["channel"] == "prod" and record["kind"] == "ota").lower())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    planner = sub.add_parser("plan")
    planner.add_argument("--channel", required=True, choices=CHANNELS)
    planner.add_argument("--repo", required=True)
    planner.add_argument("--ref", default="HEAD")
    planner.add_argument("--version")
    planner.add_argument("--force-ota", action="store_true")
    planner.add_argument("--history", type=Path, help="Offline JSON history for preview/testing")
    planner.add_argument("--output", type=Path, default=ROOT / "output/release/plan.json")
    for action in ("build", "stage"):
        child = sub.add_parser(action)
        child.add_argument("--plan", type=Path, required=True)
        child.add_argument("--output", type=Path, default=ROOT / "output/release/assets")
    for action in ("verify", "publish"):
        sub.add_parser(action).add_argument("directory", type=Path)
    args = parser.parse_args()
    if args.action == "plan":
        history = read_json(args.history) if args.history else github_history(args.repo)
        plan = make_plan(ROOT, args.repo, args.channel, history, args.ref, args.version, args.force_ota)
        write_json(args.output, plan)
        args.output.with_suffix(".md").write_text(notes(plan))
        print(f"{plan['tag']}: {plan['kind']}; contract {plan['platform']['contract']}; base {plan['platform']['base_release']}")
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as stream:
                for key in ("tag", "kind", "source_commit", "channel"):
                    stream.write(f"{key}={plan[key]}\n")
    elif args.action in ("build", "stage"):
        plan = read_json(args.plan)
        if plan["kind"] == "none":
            raise ValueError("No runtime changes to build")
        os.environ.update(environment(plan))
        if args.action == "build":
            check_checkout(plan)
            script = "debian_build.sh" if plan["kind"] == "ota" else "scripts/debian-package/release.sh"
            subprocess.run(["bash", str(ROOT / script)], cwd=ROOT, check=True)
        stage(plan, args.output)
    elif args.action == "verify":
        verify(args.directory)
        print("Channel release verification passed")
    elif args.action == "publish":
        publish(args.directory)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        sys.exit(f"Channel release: {error}")

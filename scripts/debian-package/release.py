#!/usr/bin/env python3
"""Stage and verify standalone business releases before GitHub publication."""

import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def check_source(root, apps):
    commit = command("git", "-C", str(root), "rev-parse", "HEAD")
    if command("git", "-C", str(root), "status", "--porcelain"):
        raise ValueError("Commit source and SDK changes before building a release")
    metadata = apps / "apps/metadata"
    if (metadata / "hardware-demo-commit.txt").read_text().strip() != commit:
        raise ValueError("Application binaries are from another commit; run release.sh build")
    for name in ("hardware-demo-status.txt", "pico-sdk-status.txt"):
        if (metadata / name).read_text().strip():
            raise ValueError("Application binaries were built from a dirty source tree")
    # The vendored SDK shares the firmware source commit.
    sdk_commit = commit
    if (metadata / "pico-sdk-commit.txt").read_text().strip() != sdk_commit:
        raise ValueError("Application SDK commit does not match the checkout")
    if "status=pass" not in (apps / "apps-audit/summary.txt").read_text().splitlines():
        raise ValueError("Application audit has not passed")
    return commit, sdk_commit


def package_manifest(package):
    data = subprocess.check_output(["dpkg-deb", "--fsys-tarfile", str(package)])
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        return archive.extractfile("./usr/share/doc/aiden-business/release-manifest.json").read()


def verify_runtime_config(package, enabled):
    if enabled not in (0, 1):
        raise ValueError("Unknown runtime configuration ownership")
    data = subprocess.check_output(["dpkg-deb", "--fsys-tarfile", str(package)])
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        name = "./usr/lib/aiden/runtime-config.json"
        if not enabled:
            if name in archive.getnames():
                raise ValueError("Undeclared runtime configuration payload")
            return
        document = json.load(archive.extractfile(name))
        if document["format"] != 1 or not document["files"]:
            raise ValueError("Missing runtime configuration inventory")
        expected_conffiles = set()
        for path, record in document["files"].items():
            entry = Path(path)
            hdmi_profile = entry.parent.as_posix() == "usr/share/aiden/hdmi" and entry.suffix == ".hex"
            if entry.is_absolute() or ".." in entry.parts or not (path.startswith(("etc/", "usr/lib/aiden/")) or hdmi_profile):
                raise ValueError("Unsafe runtime configuration path")
            member = archive.getmember("./" + path)
            if (not member.isfile() or (member.uid, member.gid, member.mode) != (0, 0, int(record["mode"], 8))
                    or hashlib.sha256(archive.extractfile(member).read()).hexdigest() != record["sha256"]
                    or record["activation"] not in ("live", "reboot")):
                raise ValueError("Runtime configuration payload differs from inventory")
            if path.startswith("etc/"):
                expected_conffiles.add("/" + path)
    control = subprocess.check_output(["dpkg-deb", "--ctrl-tarfile", str(package)])
    with tarfile.open(fileobj=io.BytesIO(control)) as archive:
        actual = set(archive.extractfile("./conffiles").read().decode().splitlines())
        if actual != expected_conffiles:
            raise ValueError("Runtime configuration conffiles are incomplete")


def stage(root, apps, output):
    commit, sdk_commit = check_source(root, apps)
    version = os.environ["AIDEN_BUSINESS_VERSION"] + "-" + os.environ["AIDEN_BUSINESS_REVISION"]
    package_name = f"aiden-business_{version}_armhf.deb"
    release = output / "release"
    if release.exists():
        shutil.rmtree(release)
    release.mkdir(parents=True)
    package = release / package_name
    shutil.copyfile(output / package_name, package)
    (release / "release-manifest.json").write_bytes(package_manifest(package))
    compatibility = json.loads(package_manifest(package))["required_platform_contract"]
    metadata = {
        "format": 1, "package": "aiden-business", "version": version, "architecture": "armhf",
        "tag": f"business-v{version}", "source_commit": commit, "sdk_commit": sdk_commit,
        "package_file": package_name, "package_sha256": sha256(package),
    }
    (release / "build-metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    (release / "RELEASE-NOTES.md").write_text(
        f"Aiden business package {version} for armhf.\n\n"
        f"Source: `{commit}`\n\n"
        f"Requires Debian platform contract >={compatibility['min']} and <{compatibility['max_exclusive']}. "
        "Contains applications, resources and managed runtime configuration; no partition images.\n\n"
        "Verify SHA256SUMS before installing with apt. Package maintainer scripts "
        "stop and restore previously running Aiden services; then run the OTA self-check. "
        "See docs/08-ota/debian-package.md for the installation procedure.\n\n"
        "This GitHub prerelease distributes .deb files; it is not an APT repository "
        "or a signed firmware OTA release.\n"
    )
    assets = sorted(release.iterdir())
    (release / "SHA256SUMS").write_text("".join(f"{sha256(p)}  {p.name}\n" for p in assets))
    verify(release)
    print(f"Release assets: {release}")


def verify(release):
    metadata = json.loads((release / "build-metadata.json").read_text())
    version = metadata["version"]
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-[1-9][0-9]*", version):
        raise ValueError("Invalid package version")
    for field in ("source_commit", "sdk_commit"):
        if not re.fullmatch(r"[0-9a-f]{40}", metadata[field]):
            raise ValueError(f"Invalid {field}")
    name = f"aiden-business_{version}_armhf.deb"
    if metadata["package_file"] != name or metadata["tag"] != f"business-v{version}":
        raise ValueError("Package filename or release tag does not match its version")
    if metadata["package"] != "aiden-business" or metadata["architecture"] != "armhf":
        raise ValueError("Wrong package or architecture")
    expected_assets = {name, "release-manifest.json", "build-metadata.json", "RELEASE-NOTES.md", "SHA256SUMS"}
    if {p.name for p in release.iterdir()} != expected_assets:
        raise ValueError("Release directory has missing or unexpected files")
    for name in expected_assets:
        if (release / name).is_symlink() or not (release / name).is_file():
            raise ValueError("Release assets must be regular files")
    expected_sums = "".join(f"{sha256(release / n)}  {n}\n" for n in sorted(expected_assets - {"SHA256SUMS"}))
    if (release / "SHA256SUMS").read_text() != expected_sums:
        raise ValueError("Release checksum verification failed")
    package = release / metadata["package_file"]
    if sha256(package) != metadata["package_sha256"]:
        raise ValueError("Package checksum does not match build metadata")
    for field, expected in (("Package", "aiden-business"), ("Version", version), ("Architecture", "armhf")):
        if command("dpkg-deb", "--field", str(package), field) != expected:
            raise ValueError(f"Package {field} mismatch")
    manifest_bytes = (release / "release-manifest.json").read_bytes()
    if package_manifest(package) != manifest_bytes:
        raise ValueError("Published manifest does not match the package")
    manifest = json.loads(manifest_bytes)
    verify_runtime_config(package, manifest.get("runtime_config", 0))
    if f"{manifest['business_release']}-{manifest['package_revision']}" != version:
        raise ValueError("Business manifest version mismatch")
    return metadata


def main():
    action, *args = sys.argv[1:]
    if action == "check-source" and len(args) == 2:
        check_source(*map(Path, args))
    elif action == "stage" and len(args) == 3:
        stage(*map(Path, args))
    elif action == "verify" and len(args) in (1, 2):
        metadata = verify(Path(args[0]))
        print(metadata[args[1]] if len(args) == 2 else "Release checks passed")
    else:
        raise ValueError("Usage: release.py check-source ROOT APPS | stage ROOT APPS OUTPUT | verify RELEASE [FIELD]")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"Business release: {error}")

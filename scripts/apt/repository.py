#!/usr/bin/env python3
"""Build a signed GitHub Pages APT repository from verified, published releases."""

import argparse
from datetime import datetime, timezone
from email.parser import Parser
from email.utils import format_datetime
import gzip
import hashlib
import html
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts/release"))
from plan import command, github_history, validate_history, version_tuple
from release import business

PUBLIC_KEY = ROOT / "overlay-debian/usr/share/keyrings/aico-archive-keyring.asc"
PAGES_LIMIT = 900 * 1024 * 1024


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def suite_for(record):
    return f"{record['channel']}-c{record['platform']['contract']}"


def select_records(records, repo, keep=3):
    if keep < 2:
        raise ValueError("Retain at least two versions per contract for cached APT indexes")
    suites = {}
    for record in validate_history(records, repo):
        suites.setdefault(suite_for(record), []).append(record)
    return {suite: items[-keep:] for suite, items in sorted(suites.items())}


def fetch_package(record, cache):
    name = f"aico-business_{record['version']}-1_armhf.deb"
    expected = record["assets"][name]
    directory = cache / record["tag"]
    directory.mkdir(parents=True, exist_ok=True)
    package = directory / name
    if not package.exists():
        # Download into an isolated directory: a failed download never poisons the cache.
        with tempfile.TemporaryDirectory(dir=cache) as temporary:
            subprocess.run(["gh", "release", "download", record["tag"], "--repo", record["repo"],
                            "--pattern", name, "--dir", temporary], check=True, timeout=300)
            downloaded = Path(temporary) / name
            verify_asset(downloaded, expected)
            os.replace(downloaded, package)
    verify_asset(package, expected)
    return package


def verify_asset(path, expected):
    if path.is_symlink() or path.stat().st_size != expected["size"] or sha256(path) != expected["sha256"]:
        raise ValueError(f"Published package checksum/size mismatch: {path.name}")


def package_control(package, record):
    control = command("dpkg-deb", "--field", str(package))
    fields = Parser().parsestr(control)
    for field, expected in (("Package", "aico-business"), ("Version", record["version"] + "-1"),
                            ("Architecture", "armhf")):
        if fields.get_all(field) != [expected]:
            raise ValueError(f"Published package {field} differs from release record")
    if any(field in fields for field in ("Filename", "Size", "SHA256", "SHA512")):
        raise ValueError("Package control must not supply repository checksums/paths")
    manifest = json.loads(business.package_manifest(package))
    if manifest.get("runtime_config", 0) != record.get("runtime_config", 0):
        raise ValueError("Runtime configuration ownership differs from release record")
    business.verify_runtime_config(package, record.get("runtime_config", 0))
    platform = manifest.get("platform", {})
    expected_platform = {
        "format": 1, "product": "aico", "platform_id": "luckfox-rv1106", "architecture": "armhf",
        "os_release": "debian-13", "libc": "glibc", "channel": record["channel"],
        "platform_contract": record["platform"]["contract"],
        "base_release": record["platform"]["base_release"],
        "system_fingerprint": record["fingerprints"]["system"],
    }
    if record.get("runtime_config", 0):
        expected_platform["runtime_config"] = record["runtime_config"]
    contract = record["platform"]["contract"]
    required = manifest.get("required_platform_contract")
    if (type(platform.get("platform_contract")) is not int
            or platform != expected_platform or manifest.get("business_release") != record["version"]
            or manifest.get("package_revision") != "1"
            or not isinstance(required, dict)
            or any(type(required.get(k)) is not int for k in ("min", "max_exclusive"))
            or required != {"min": contract, "max_exclusive": contract + 1}):
        raise ValueError("Published package manifest differs from channel/contract/version")
    return control + "\n"


def fingerprint(public_key):
    result = command("gpg", "--batch", "--with-colons", "--import-options", "show-only",
                     "--import", str(public_key))
    fingerprints = [line.split(":")[9] for line in result.splitlines() if line.startswith("fpr:")]
    if len(fingerprints) != 1:
        raise ValueError("Expected one signing-only repository key")
    return fingerprints[0]


def sign_release(directory, key, keyring):
    for flag, filename in (("--clearsign", "InRelease"), ("--detach-sign", "Release.gpg")):
        subprocess.run(["gpg", "--batch", "--yes", "--pinentry-mode", "loopback", "--armor",
                        "--local-user", key + "!", "--digest-algo", "SHA256",
                        "--output", str(directory / filename), flag, str(directory / "Release")], check=True)
    subprocess.run(["gpgv", "--keyring", str(keyring), str(directory / "InRelease")],
                   check=True, stdout=subprocess.DEVNULL)


def write_suite(directory, packages, records, now, key, keyring):
    binary = directory / "main/binary-armhf"
    binary.mkdir(parents=True)
    payload = "\n".join(packages).encode()
    (binary / "Packages").write_bytes(payload)
    (binary / "Packages.gz").write_bytes(gzip.compress(payload, mtime=0))
    metadata = directory / "platform.json"
    metadata.write_text(json.dumps({"channel": records[-1]["channel"], "platform": records[-1]["platform"],
                                    "system_fingerprint": records[-1]["fingerprints"]["system"]}, indent=2) + "\n")
    files = [metadata, binary / "Packages", binary / "Packages.gz"]
    text = (f"Origin: Aico\nLabel: Aico business packages\nSuite: {directory.name}\nCodename: {directory.name}\n"
            f"Date: {format_datetime(now, usegmt=True)}\n"
            "Architectures: armhf\nComponents: main\nAcquire-By-Hash: yes\nSHA256:\n")
    for path in files:
        digest = sha256(path)
        text += f" {digest} {path.stat().st_size} {path.relative_to(directory).as_posix()}\n"
        if path.parent == binary:
            hashed = binary / "by-hash/SHA256" / digest
            hashed.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, hashed)
    (directory / "Release").write_text(text)
    sign_release(directory, key, keyring)


def build_site(records, repo, output, cache, public_key=PUBLIC_KEY, keep=3, now=None):
    suites = select_records(records, repo, keep)
    if not suites:
        raise ValueError("No published managed releases to index")
    if output.exists():
        raise ValueError(f"Output already exists; choose a fresh staging directory: {output}")
    key = fingerprint(public_key)
    now = (now or datetime.now(timezone.utc)).replace(microsecond=0)
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=output.parent) as temporary:
        temporary = Path(temporary)
        site = temporary / "site"
        apt = site / "apt"
        apt.mkdir(parents=True)
        keyring = temporary / "trusted.gpg"
        subprocess.run(["gpg", "--batch", "--yes", "--dearmor", "--output", str(keyring), str(public_key)], check=True)
        published = {}
        for suite, items in suites.items():
            packages = []
            for record in sorted(items, key=lambda r: version_tuple(r["version"]), reverse=True):
                package = fetch_package(record, cache)
                control = package_control(package, record)
                relative = Path("pool") / suite / record["tag"] / package.name
                destination = apt / relative
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(package, destination)
                packages.append(control + f"Filename: {relative.as_posix()}\nSize: {package.stat().st_size}\nSHA256: {sha256(package)}\n")
            write_suite(apt / "dists" / suite, packages, items, now, key, keyring)
            published[suite] = [r["tag"] for r in items]
        shutil.copyfile(public_key, apt / "aico-archive-keyring.asc")
        (site / ".nojekyll").touch()
        (apt / "repository.json").write_text(json.dumps({"repo": repo, "signing_key": key, "suites": published}, indent=2) + "\n")
        links = "".join(f'<li><a href="apt/dists/{suite}/Release">{suite}</a>: {html.escape(", ".join(tags))}</li>' for suite, tags in published.items())
        (site / "index.html").write_text('<!doctype html><html lang="en"><meta charset="utf-8"><title>Aico Link APT</title>'
                                       '<h1>Aico Link business packages</h1><p>Signed updates for Aico Link firmware and the Aico AI Agent.</p><p><a href="https://aico.link">aico.link</a></p>'
                                       f'<ul>{links}</ul><p>Signing key: <code>{key}</code></p>'
                                       '<a href="apt/aico-archive-keyring.asc">Repository public key</a></html>\n')
        if sum(p.stat().st_size for p in site.rglob("*") if p.is_file()) > PAGES_LIMIT:
            raise ValueError("APT site exceeds the 900 MiB budget; archive obsolete platform contracts before deploying")
        os.replace(site, output)
    return published


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--output", type=Path, default=ROOT / "output/apt/site")
    parser.add_argument("--cache", type=Path, default=ROOT / "output/apt/packages")
    parser.add_argument("--public-key", type=Path, default=PUBLIC_KEY)
    parser.add_argument("--history", type=Path, help="Offline published records, for validation")
    args = parser.parse_args()
    records = json.loads(args.history.read_text()) if args.history else github_history(args.repo)
    print(json.dumps(build_site(records, args.repo, args.output, args.cache, args.public_key), indent=2))


if __name__ == "__main__":
    main()

"""Embedded in maintainer hooks; compare actual configs, preserving conffile edits."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(os.environ.get("DPKG_ROOT") or "/")
NEW_FILES = {}  # Replaced by the package builder, before payload unpacking.
DOCUMENT = ROOT / "usr/lib/aico/runtime-config.json"


def installed_files():
    if not DOCUMENT.exists():
        return {}
    document = json.loads(DOCUMENT.read_text())
    if document["format"] != 1:
        raise ValueError("Unsupported runtime configuration inventory")
    return document["files"]


def digest(path):
    target = ROOT / path
    if target.is_symlink():
        return "symlink:" + os.readlink(target)
    if not target.exists():
        return None
    return {"sha256": hashlib.sha256(target.read_bytes()).hexdigest(),
            "mode": target.stat().st_mode & 0o7777}


def snapshot(state):
    target = state / "config-before.json"
    before = json.loads(target.read_text()) if target.exists() else {}
    files = {**installed_files(), **NEW_FILES}
    # Old prerm runs before new preinst; extend its snapshot for newly owned paths.
    for path, record in files.items():
        before.setdefault(path, {"digest": digest(path), "activation": record["activation"]})
    state.mkdir(parents=True, exist_ok=True)
    temporary = state / "config-before.tmp"
    temporary.write_text(json.dumps(before))
    temporary.replace(target)


def configure(state):
    files = installed_files()
    if files and any(p.startswith("etc/sudoers.d/") for p in files):
        # Check the actual conffile, including any administrator edits dpkg kept.
        subprocess.run(["visudo", "-c"], check=True)
    if not (state / "config-before.json").exists():
        return  # Reconfigure after a successful transaction is not another upgrade.
    before = json.loads((state / "config-before.json").read_text())
    changed = [p for p in sorted(set(before) | set(files))
               if (files.get(p, before.get(p))["activation"] == "reboot"
                   and before.get(p, {}).get("digest") != digest(p))]
    if changed:
        run = ROOT / "run"
        run.mkdir(parents=True, exist_ok=True)
        (run / "reboot-required").touch()
        packages = run / "reboot-required.pkgs"
        existing = packages.read_text().splitlines() if packages.exists() else []
        if "aico-business" not in existing:
            with packages.open("a") as stream:
                stream.write("aico-business\n")
        detail = run / "aico-business-reboot-required.json"
        paths = set(json.loads(detail.read_text())) if detail.exists() else set()
        detail.write_text(json.dumps(sorted(paths | set(changed)), indent=2) + "\n")
        print("aico-business: configuration installed; reboot when convenient to apply network/boot settings")


action, directory = sys.argv[1:]
if action == "snapshot":
    snapshot(Path(directory))
elif action == "configure":
    configure(Path(directory))
else:
    raise ValueError("Unknown configuration transition action")

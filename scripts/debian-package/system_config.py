#!/usr/bin/env python3
"""Business-owned overlay files within the OTA platform's namespace boundary."""
import argparse
import fnmatch
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[2]
CATALOG = ROOT / "scripts/debian-system/config-package.json"


def managed(path, catalog=None):
    catalog = catalog or json.loads(CATALOG.read_text())
    return (any(fnmatch.fnmatchcase(path, p) for p in catalog["include"])
            and not any(fnmatch.fnmatchcase(path, p) for p in catalog["exclude"]))


def inventory():
    catalog = json.loads(CATALOG.read_text())
    if catalog["format"] != 1:
        raise ValueError("Unknown runtime configuration boundary")
    result = {}
    for source in sorted((ROOT / "overlay-debian").rglob("*")):
        path = source.relative_to(ROOT / "overlay-debian").as_posix()
        if not managed(path, catalog):
            continue
        if source.is_dir() and not source.is_symlink():
            continue
        if source.is_symlink() or not source.is_file():
            raise ValueError(f"Runtime configuration must be a regular file: {path}")
        mode = "0440" if path.startswith("etc/sudoers.d/") else "0755" if source.stat().st_mode & 0o111 else "0644"
        result[path] = {"mode": mode, "sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
                        "activation": "live" if any(fnmatch.fnmatchcase(path, p) for p in catalog["live"]) else "reboot"}
    return result


def stage(root):
    files = inventory()
    for path, record in files.items():
        destination = root / path
        if destination.exists():
            raise ValueError(f"Configuration collides with another package payload: {path}")
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / "overlay-debian" / path, destination)
        destination.chmod(int(record["mode"], 8))
    (root / "DEBIAN/conffiles").write_text("".join("/" + p + "\n" for p in files if p.startswith("etc/")))
    document = root / "usr/lib/aico/runtime-config.json"
    document.parent.mkdir(parents=True, exist_ok=True)
    document.write_text(json.dumps({"format": 1, "files": files}, indent=2) + "\n")


def audit(root):
    for path, record in inventory().items():
        installed = root / path
        stat = installed.stat()
        if installed.is_symlink() or (stat.st_uid, stat.st_gid, stat.st_mode & 0o777) != (0, 0, int(record["mode"], 8)):
            raise ValueError(f"Configuration ownership/mode invalid: {path}")
        owner = subprocess.check_output(["dpkg-query", "--root=" + str(root), "-S", "/" + path], text=True)
        if owner.strip() != "aico-business: /" + path:
            raise ValueError(f"Wrong dpkg owner: {path}")
        if installed.read_bytes() != (ROOT / "overlay-debian" / path).read_bytes():
            raise ValueError(f"Configuration differs from source: {path}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("stage", "exclude", "audit", "inventory"))
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    if args.action == "stage":
        stage(args.destination)
    elif args.action == "exclude":
        args.destination.write_text("".join("/" + p + "\n" for p in inventory()))
    elif args.action == "inventory":
        args.destination.write_text(json.dumps({"format": 1, "files": inventory()}, indent=2) + "\n")
    else:
        audit(args.destination)


if __name__ == "__main__":
    main()

"""Disposable Android SDK AVDs. Never reset or terminate a pre-existing device."""
from __future__ import annotations

from contextlib import contextmanager
import hashlib
import json
import os
import subprocess
from pathlib import Path
import shutil
import socket
import time
from typing import Iterator

from runner.process import terminate_process_tree


def template_digest(template: Path) -> str:
    """The fixture is an offline export, including userdata and its pinned config."""
    if not template.is_dir() or template.is_symlink():
        raise ValueError("AVD template must be an existing regular directory")
    h = hashlib.sha256()
    for path in sorted(template.rglob("*")):
        if path.is_symlink() or ".lock" in path.name or path.name.endswith(".pid"):
            raise ValueError("AVD template must be a stopped export without symlinks or locks")
        if path.is_file():
            h.update(path.relative_to(template).as_posix().encode() + b"\0")
            with path.open("rb") as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b""):
                    h.update(block)
    return h.hexdigest()


def clone_template(template: Path, destination: Path, name: str) -> None:
    if not (template / "config.ini").is_file() or not (template / "userdata-qemu.img").is_file():
        raise ValueError("Template needs config.ini and a provisioned userdata-qemu.img")
    shutil.copytree(template, destination, ignore=shutil.ignore_patterns("snapshots", "*.lock", "*.pid"))
    lines = []
    for line in (destination / "config.ini").read_text().splitlines():
        key, sep, value = line.partition("=")
        key, value = key.strip(), value.strip()
        if key in {"AvdId", "avd.ini.displayname"}:
            value = name
        elif key in {"disk.dataPartition.path", "disk.encryptionKeyPartition.path", "sdcard.path", "disk.cachePartition.path"}:
            # Writable disks must resolve to copied files, not back into the source AVD.
            filename = Path(value).name
            if not filename or not (destination / filename).is_file():
                raise ValueError(f"AVD has an uncopied writable disk: {key}")
            value = str(destination / filename)
        elif key == "image.sysdir.1" and not Path(value).is_absolute():
            sdk = os.environ.get("ANDROID_SDK_ROOT") or os.environ.get("ANDROID_HOME")
            if not sdk or not (Path(sdk) / value).is_dir():
                raise ValueError("Set ANDROID_SDK_ROOT to the installed system image")
            value = str(Path(sdk) / value)
        lines.append(f"{key}={value}" if sep else line)
    (destination / "config.ini").write_text("\n".join(lines) + "\n")
    # Emulator overlays can retain absolute backing paths into the original AVD.
    # Flatten each copied overlay instead of trusting copytree to isolate it.
    overlays = sorted(destination.glob("*.qcow2"))
    if overlays:
        qemu_img = shutil.which(os.environ.get("AUTO_IMPROVE_QEMU_IMG", "qemu-img"))
        if not qemu_img:
            raise ValueError("qemu-img is required to isolate AVD disk overlays")
        for overlay in overlays:
            # Validate the original chain before opening it for conversion. A copied
            # qcow2 may still refer to a live nightly disk through an absolute path.
            original = template / overlay.name
            chain = json.loads(subprocess.check_output(
                [qemu_img, "info", "--backing-chain", "--output=json", str(original)], timeout=30))
            for image in chain:
                filename = Path(image["filename"])
                if not filename.is_absolute():
                    filename = original.parent / filename
                if not filename.resolve().is_relative_to(template.resolve()):
                    raise ValueError("Export/flatten AVD backing disks into the offline template first")
            flattened = overlay.with_suffix(".flattened")
            subprocess.run([qemu_img, "convert", "-O", "qcow2", str(original), str(flattened)],
                           check=True, timeout=300, capture_output=True)
            flattened.replace(overlay)
    # Reconstructed by the emulator; may contain stale host paths from the export.
    (destination / "hardware-qemu.ini").unlink(missing_ok=True)


def free_port() -> int:
    # The workflow serializes owned emulator creation. The emulator still checks the
    # pair atomically at bind time; a race is a startup failure, never a device reuse.
    for port in range(5580, 5682, 2):
        sockets = []
        try:
            for number in (port, port + 1):
                sock = socket.socket()
                sockets.append(sock)
                sock.bind(("127.0.0.1", number))
            return port
        except OSError:
            continue
        finally:
            for sock in sockets:
                sock.close()
    raise RuntimeError("No private emulator port pair available")


@contextmanager
def isolated_emulator(work: Path, *, template: Path, expected_sha256: str,
                      timeout: float = 240) -> Iterator[dict[str, str]]:
    executable = shutil.which(os.environ.get("AUTO_IMPROVE_EMULATOR", "emulator"))
    adb = shutil.which("adb")
    if not executable or not adb:
        raise RuntimeError("Android SDK emulator and adb must be installed")
    if not expected_sha256 or template_digest(template) != expected_sha256:
        raise ValueError("AVD template checksum differs from AUTO_IMPROVE_AVD_SHA256")
    work.mkdir(parents=True, exist_ok=False)
    name = "auto-improve-" + hashlib.sha256(str(work.resolve()).encode()).hexdigest()[:16]
    avds = work / "avds"
    avds.mkdir()
    avd = avds / f"{name}.avd"
    proc = None
    try:
        clone_template(template, avd, name)
        (avds / f"{name}.ini").write_text(f"avd.ini.encoding=UTF-8\npath={avd.resolve()}\n")
        port = free_port()
        serial = f"emulator-{port}"
        existing = subprocess.check_output([adb, "devices"], text=True, timeout=10)
        if any(line.split() and line.split()[0] == serial for line in existing.splitlines()[1:]):
            raise RuntimeError("Allocated serial is already registered with adb")
        env = {**os.environ, "ANDROID_AVD_HOME": str(avds.resolve())}
        command = [executable, "-avd", name, "-port", str(port), "-no-window",
                   "-no-audio", "-no-boot-anim", "-no-snapshot-load", "-no-snapshot-save"]
        with (work / "emulator.log").open("w") as log:
            proc = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                    start_new_session=True)
            (work / "owner.json").write_text(json.dumps({"pid": proc.pid, "serial": serial,
                "avd": str(avd), "name": name, "fixture": expected_sha256,
                "started": process_start(proc.pid)}))
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if proc.poll() is not None:
                    raise RuntimeError("Private emulator exited during startup; inspect emulator.log")
                result = subprocess.run([adb, "-s", serial, "shell", "getprop", "sys.boot_completed"],
                                        capture_output=True, text=True, timeout=10)
                if result.returncode == 0 and result.stdout.strip() == "1":
                    identity = subprocess.check_output([adb, "-s", serial, "emu", "avd", "name"],
                                                       text=True, timeout=10).splitlines()
                    if not identity or identity[0].strip() != name or proc.poll() is not None:
                        raise RuntimeError("ADB serial does not identify the owned AVD")
                    yield {"ANDROID_SERIAL": serial}
                    return
                time.sleep(1)
            raise TimeoutError("Private emulator did not boot within the readiness budget")
    finally:
        # Only the process handle created above is terminated. Never adb kill-server.
        terminate_process_tree(proc)
        if proc is None or proc.poll() is not None:
            shutil.rmtree(avds, ignore_errors=True)
            (work / "owner.json").unlink(missing_ok=True)
        else:
            # Preserve ownership metadata so a runner operator can recover an orphan.
            raise RuntimeError(f"Private emulator process {proc.pid} did not stop; see {work / 'owner.json'}")


def process_start(pid: int) -> str:
    result = subprocess.run(["ps", "-p", str(pid), "-o", "lstart="], capture_output=True, text=True, timeout=10)
    return result.stdout.strip() if result.returncode == 0 else ""


def cleanup_orphans(root: Path) -> None:
    """Recover SIGKILL leftovers only when PID, start time, and unique AVD name match."""
    import signal
    for owner in root.glob("*/result/cluster-*/*-runs/*/*/emulator/owner.json"):
        record = json.loads(owner.read_text())
        pid, name = int(record["pid"]), record["name"]
        avds = owner.parent / "avds"
        if not name.startswith("auto-improve-") or Path(record["avd"]).parent.resolve() != avds.resolve():
            raise ValueError("Invalid emulator ownership record")
        started = process_start(pid)
        if started:
            if not record.get("started") or started != record["started"]:
                raise RuntimeError("PID was reused; refusing to terminate it")
            command = subprocess.check_output(["ps", "-p", str(pid), "-o", "args="], text=True, timeout=10)
            if name not in command or "-avd" not in command:
                raise RuntimeError("Emulator identity no longer matches; refusing to terminate it")
            os.kill(pid, signal.SIGTERM)
            for _ in range(30):
                if not process_start(pid):
                    break
                time.sleep(0.1)
            else:
                if process_start(pid) == started:
                    os.kill(pid, signal.SIGKILL)
                time.sleep(0.1)
            if process_start(pid):
                raise RuntimeError("Emulator still alive; retaining disk and owner record")
        shutil.rmtree(avds, ignore_errors=True)
        owner.unlink()


def cleanup_completed_work(root: Path, run_name: str) -> None:
    """Delete an uploaded run's files only after all owned AVDs are stopped."""
    import re
    if not re.fullmatch(r"auto-improve-[0-9]+-[0-9]+", run_name):
        raise ValueError("Invalid owned workflow directory")
    work = root / run_name
    if work.is_symlink() or not work.resolve().is_relative_to(root.resolve()):
        raise ValueError("Workflow directory escapes work root")
    if any(work.glob("result/cluster-*/*-runs/*/*/emulator/owner.json")):
        raise RuntimeError("Retain workflow directory until its emulator ownership records are resolved")
    shutil.rmtree(work, ignore_errors=True)


def prepare_companion(serial: str) -> None:
    """Enable only the pinned, preinstalled companion on the owned emulator."""
    def adb(*args: str) -> str:
        return subprocess.check_output(["adb", "-s", serial, *args], text=True, timeout=30).strip()
    package = "link.aico.app"
    if not adb("shell", "pm", "path", package).startswith("package:"):
        raise RuntimeError("The pinned template must contain the required AidenBridge APK")
    adb("root")
    subprocess.run(["adb", "-s", serial, "wait-for-device"], check=True, timeout=60)
    if adb("shell", "id", "-u") != "0":
        raise RuntimeError("Companion-app cases require a rootable Android SDK image")
    for permission in ("READ_CALENDAR", "WRITE_CALENDAR", "READ_CONTACTS", "WRITE_CONTACTS"):
        adb("shell", "pm", "grant", package, f"android.permission.{permission}")
    if int(adb("shell", "getprop", "ro.build.version.sdk")) >= 33:
        adb("shell", "pm", "grant", package, "android.permission.POST_NOTIFICATIONS")


def cli() -> int:
    import argparse
    import signal
    parser = argparse.ArgumentParser(description="Fingerprint or smoke-test an offline Android AVD fixture")
    parser.add_argument("operation", choices=["fingerprint", "smoke"])
    parser.add_argument("--template", type=Path, required=True)
    parser.add_argument("--sha256", default="")
    parser.add_argument("--out", type=Path)
    parser.add_argument("--companion", action="store_true")
    args = parser.parse_args()
    if args.operation == "fingerprint":
        print(template_digest(args.template.resolve()))
        return 0
    if not args.out or not args.sha256:
        parser.error("smoke requires --out and --sha256")
    def interrupted(signum, frame):
        raise InterruptedError(f"Interrupted by signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    def connected() -> set[str]:
        return {line.split()[0] for line in subprocess.check_output(["adb", "devices"], text=True, timeout=10).splitlines()[1:]
                if len(line.split()) == 2 and line.split()[1] == "device"}
    before = connected()
    serials = []
    marker = "/data/local/tmp/aico-auto-improve-isolation-probe"
    for attempt in range(2):
        with isolated_emulator(out / str(attempt), template=args.template.resolve(), expected_sha256=args.sha256) as env:
            serial = env["ANDROID_SERIAL"]
            serials.append(serial)
            if serial in before:
                raise RuntimeError("Smoke test reused an existing device")
            if args.companion:
                prepare_companion(serial)
            result = subprocess.run(["adb", "-s", serial, "shell", "test", "-e", marker], timeout=10)
            if result.returncode == 0:
                raise RuntimeError("Fresh AVD unexpectedly contains state from a prior attempt")
            subprocess.run(["adb", "-s", serial, "shell", "touch", marker], check=True, timeout=10)
            subprocess.run(["adb", "-s", serial, "shell", "test", "-e", marker], check=True, timeout=10)
    if not before.issubset(connected()):
        raise RuntimeError("An unrelated device disappeared during the smoke test")
    (out / "smoke.json").write_text(json.dumps({"passed": True, "fixture": args.sha256,
        "serials": serials, "preexisting_devices": sorted(before)}, indent=2) + "\n")
    print("Private AVD reset smoke passed; no model calls were made.")
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())

"""Run one task per fresh daemon/environment, retaining only reviewable evidence."""
from __future__ import annotations

from contextlib import nullcontext
import dataclasses
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

from ci.auto_improve.emulator import isolated_emulator, prepare_companion
from ci.auto_improve.evidence import sanitize
from ci.auto_improve.policy import digest
from ci.plan import load_catalog
from runner.process import terminate_process_tree


def command(args: list[str], *, cwd: Path, log: Path, timeout: float, env: dict | None = None) -> None:
    log.parent.mkdir(parents=True, exist_ok=True)
    try:
        with log.open("w") as stream:
            proc = subprocess.Popen(args, cwd=cwd, env=env, stdout=stream, stderr=subprocess.STDOUT,
                                    start_new_session=True)
            try:
                code = proc.wait(timeout=max(0.1, timeout))
            except BaseException:
                terminate_process_tree(proc)
                raise
    finally:
        # Also redact when a command times out or the workflow is cancelled.
        if log.is_file():
            log.write_text(sanitize(log.read_text(errors="replace")))
    if code:
        raise RuntimeError(f"Command failed ({code}); see {log.name}")


def resource_snapshot(log: Path, label: str) -> None:
    """Append host load evidence without process arguments, which may contain secrets."""
    probes = [
        ["nproc"], ["cat", "/proc/loadavg"], ["free", "-m"],
        ["ps", "-eo", "pcpu,pmem,etimes,comm", "--sort=-pcpu"],
        ["docker", "ps", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}"],
    ]
    log.parent.mkdir(parents=True, exist_ok=True)
    with log.open("a") as stream:
        stream.write(f"## {label} at {time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}\n")
        for args in probes:
            stream.write("$ " + " ".join(args) + "\n")
            try:
                result = subprocess.run(args, capture_output=True, text=True, timeout=20)
                lines = (result.stdout + result.stderr).splitlines()
                stream.write("\n".join(lines[:40]) + "\n")
            except (OSError, subprocess.SubprocessError) as exc:
                stream.write(f"unavailable: {exc}\n")
        stream.write("\n")


def benchmark_environment() -> dict[str, str]:
    names = {"PATH", "HOME", "LANG", "DOCKER_HOST", "DOCKER_CONFIG", "ANDROID_HOME", "ANDROID_SDK_ROOT",
             "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy",
             "DAEMON_DOCKERFILE", "MOBILEGYM_IMAGE", "MOBILEGYM_NODE_IMAGE", "MOBILEGYM_DEBIAN_MIRROR"}
    names.update({f"BENCHMARK_{kind}_{field}" for kind in ("AGENT", "JUDGE")
                  for field in ("MODEL", "PROVIDER", "BASE_URL", "API_KEY")})
    env = {k: v for k, v in os.environ.items() if k in names}
    # Do not create extra Langfuse records, and never pass publication/review secrets.
    env.update({"BENCHMARK_REPEATS": "1", "BENCHMARK_TELEMETRY": "0",
                "BENCHMARK_CI_IMAGES_PREPARED": "1", "GIT_TERMINAL_PROMPT": "0"})
    return env


def prepare(root: Path, image: str, environments: set[str], out: Path, timeout: float) -> None:
    command([sys.executable, "-m", "ci.prepare_images", "--matrix-json",
             json.dumps({"include": [{"environment": e} for e in sorted(environments)]}),
             "--daemon-image", image, "--daemon-dockerfile",
             os.environ.get("DAEMON_DOCKERFILE", "benchmark/docker/Dockerfile.agent-daemon.cn"),
             "--mobilegym-image", os.environ.get("MOBILEGYM_IMAGE", "aico-mobilegym-simulator:py311"),
             "--mobilegym-log", str(out / "mobilegym-build.log")], cwd=root / "benchmark",
            log=out / "build.log", timeout=timeout, env=benchmark_environment())


def retain_evidence(run: Path, destination: Path) -> None:
    destination.mkdir(parents=True, exist_ok=True)
    for name in ("manifest.json", "metrics.json", "results.jsonl", "summary.md"):
        if (run / name).is_file():
            if name.endswith('.json'):
                value = sanitize(json.loads((run / name).read_text()))
                (destination / name).write_text(json.dumps(value, indent=2) + "\n")
            elif name.endswith('.jsonl'):
                values = [sanitize(json.loads(line)) for line in (run / name).read_text().splitlines() if line.strip()]
                (destination / name).write_text("".join(json.dumps(value) + "\n" for value in values))
            else:
                (destination / name).write_text(sanitize((run / name).read_text()))
    for path in (run / "tasks").rglob("*"):
        if path.is_file() and path.name in {"trace.json", "history.json", "judge.json", "environment_state.json",
                                           "pre.jpg", "post.jpg", "setup_failure.json"}:
            target = destination / path.relative_to(run)
            target.parent.mkdir(parents=True, exist_ok=True)
            if path.suffix == '.json':
                target.write_text(json.dumps(sanitize(json.loads(path.read_text())), indent=2) + "\n")
            else:
                shutil.copyfile(path, target)


def run_task(root: Path, key: str, repeat: int, image: str, out: Path, *, deadline: float) -> dict:
    case_id, task_id = key.split(":", 1)
    catalog = load_catalog(catalog_path=root / "benchmark/ci/suites.json", suites_dir=root / "benchmark/suites")
    case = next(case for case in catalog.cases if case.id == case_id)
    case = dataclasses.replace(case, task_ids=[task_id], max_concurrency=1)
    remaining = deadline - time.monotonic()
    if remaining < 60:
        raise TimeoutError("Attempt budget exhausted before next task")
    run_id = f"ai-{digest(str(out))[:16]}-{repeat}"
    env = benchmark_environment()
    env["DAEMON_IMAGE"] = image
    out.mkdir(parents=True, exist_ok=True)
    case_file = out / "case.json"
    case_file.write_text(json.dumps(case.matrix_entry()))
    emulator = nullcontext({})
    if case.environment == "adb":
        template = os.environ.get("AUTO_IMPROVE_AVD_TEMPLATE", "")
        checksum = os.environ.get("AUTO_IMPROVE_AVD_SHA256", "")
        if not template or not checksum:
            raise ValueError("ADB validation requires AUTO_IMPROVE_AVD_TEMPLATE/SHA256")
        emulator = isolated_emulator(out / "emulator", template=Path(template), expected_sha256=checksum,
                                     timeout=min(240, remaining))
    try:
        with emulator as device_env:
            env.update(device_env)
            if case.android_phone_bridge:
                prepare_companion(device_env["ANDROID_SERIAL"])
            # Run trusted CI code in the selected checkout so config/skills and images
            # both come from that revision. Each invocation starts a fresh daemon.
            code = ("import dataclasses,json,os,signal; from ci.plan import SuiteCase; "
                    "from ci.run_case import run_case; import sys; "
                    "signal.signal(signal.SIGTERM,lambda *_: sys.exit(143)); "
                    "case=SuiteCase(**json.load(open(sys.argv[1]))); "
                    "raise SystemExit(run_case(case,run_id=sys.argv[2],environment=os.environ,"
                    "timeout_seconds=float(sys.argv[3])))")
            command([sys.executable, "-c", code, str(case_file), run_id,
                     str(max(1, deadline - time.monotonic() - 30))], cwd=root / "benchmark",
                    log=out / "runner.log", timeout=max(1, deadline - time.monotonic()), env=env)
    finally:
        # Keep evidence even on timeout; no generated agent configuration is copied.
        run = root / "benchmark/runs" / run_id
        if run.is_dir():
            retain_evidence(run, out / "evidence")
    rows = [json.loads(line) for line in (run / "results.jsonl").read_text().splitlines() if line.strip()]
    if len(rows) != 1 or rows[0].get("task_id") != task_id:
        raise ValueError(f"Incomplete result for {key}")
    return sanitize({**rows[0], "repeat": repeat, "evidence": str(out / "evidence")})

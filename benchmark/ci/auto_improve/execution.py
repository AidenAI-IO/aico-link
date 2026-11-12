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
from ci.run_case import _incomplete_run_reasons
from runner.process import terminate_process_tree


class CommandFailed(RuntimeError):
    def __init__(self, returncode: int, log: Path):
        super().__init__(f"Command failed ({returncode}); see {log.name}")
        self.returncode = returncode


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
        raise CommandFailed(code, log)


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


def remove_images(images: list[str]) -> str | None:
    """Untag this cluster's daemon images; shared layers and other images are untouched."""
    try:
        result = subprocess.run(["docker", "image", "rm", *images], capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.SubprocessError) as exc:
        return f"Image cleanup failed: {exc}"
    errors = [line for line in result.stderr.splitlines() if line.strip() and "No such image" not in line]
    return f"Image cleanup failed: {sanitize(' '.join(errors))[:500]}" if result.returncode and errors else None


def retain_evidence(run: Path, destination: Path) -> None:
    destination.mkdir(parents=True, exist_ok=True)
    for name in ("manifest.json", "metrics.json", "results.jsonl", "results.original.jsonl", "summary.md"):
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


def rejudge_transient_errors(run: Path, *, deadline: float, delays: tuple[float, ...] = (30, 90)) -> None:
    """Retry only a failed judge call; the recorded agent execution is never rerun or changed.

    Uses the repository's rejudge path with the same judge model, rubric, trace and
    screenshots. Rows still failing stay judge_error, so the comparison gate rejects them.
    The original rows are kept as results.original.jsonl for audit.
    """
    from runner.rejudge import rejudge_run
    results = run / "results.jsonl"
    for delay in delays:
        rows = [json.loads(line) for line in results.read_text().splitlines() if line.strip()]
        if not any(row.get("status") == "judge_error" for row in rows) or deadline - time.monotonic() < delay + 300:
            return
        original = run / "results.original.jsonl"
        if not original.exists():
            shutil.copyfile(results, original)
        time.sleep(delay)
        rejudge_run(run, os.environ.get("BENCHMARK_JUDGE_MODEL", ""), os.environ.get("BENCHMARK_JUDGE_BASE_URL", ""),
                    judge_api_key_env="BENCHMARK_JUDGE_API_KEY", only_judge_errors=True)
        (run / "results.rejudged.jsonl").replace(results)


def judge_error_only(run: Path) -> bool:
    """A one-task run that exited 1 and passes run_case's completeness checks except its judge call.

    Uses the same exit-code convention as run_cases: with the effective exit code 1, a complete
    run whose only defect is one judge error yields exactly one reason.
    """
    return _incomplete_run_reasons(1, run / "manifest.json") == ["judge_errors=1"]


def run_task(root: Path, key: str, repeat: int, image: str, out: Path, *, deadline: float, rejudge: bool = True) -> dict:
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
            try:
                command([sys.executable, "-c", code, str(case_file), run_id,
                         str(max(1, deadline - time.monotonic() - 30))], cwd=root / "benchmark",
                        log=out / "runner.log", timeout=max(1, deadline - time.monotonic()), env=env)
            except CommandFailed as exc:
                # run_case exits 1 for a judge error; a complete judge-error result is data.
                # Any other exit, such as 143 on cancellation, or an incomplete run still fails.
                if exc.returncode != 1 or not judge_error_only(root / "benchmark/runs" / run_id):
                    raise
    finally:
        # Keep evidence even on timeout; no generated agent configuration is copied.
        run = root / "benchmark/runs" / run_id
        if run.is_dir():
            # Rejudging waits 30/90 s, so only rejudge a completed run, including one whose only
            # defect is a judge error; never wait while a failure or cancellation propagates.
            try:
                if rejudge and sys.exc_info()[0] is None and (run / "results.jsonl").is_file():
                    try:
                        rejudge_transient_errors(run, deadline=deadline)
                    except InterruptedError:
                        # Cancellation (SIGTERM) must stop the driver, not be logged as a judge failure.
                        raise
                    except Exception as exc:  # keep the original judge_error rows; never mask the run outcome
                        (out / "rejudge-error.log").write_text(sanitize(f"{type(exc).__name__}: {exc}\n"))
            finally:
                retain_evidence(run, out / "evidence")
    rows = [json.loads(line) for line in (run / "results.jsonl").read_text().splitlines() if line.strip()]
    if len(rows) != 1 or rows[0].get("task_id") != task_id:
        raise ValueError(f"Incomplete result for {key}")
    return sanitize({**rows[0], "repeat": repeat, "evidence": str(out / "evidence")})

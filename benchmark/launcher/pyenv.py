"""Launcher-managed Python environments for host bridges.

Some bridges need packages that are not in the benchmark lock (pyautogui,
pymobiledevice3). Each gets its own virtual environment under
``<state_dir>/pyenv/<name>``, created with uv. An environment whose installed
packages match the requirements is reused; otherwise the launcher installs them.
"""

from __future__ import annotations

import os
import re
import shutil
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping, Sequence

from .system import System

UV_MISSING = "Install uv (https://docs.astral.sh/uv/) so the launcher can prepare bridge dependencies."
PYTHON_VERSION = "3.12"
_REQUIREMENT_RE = re.compile(r"^\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*(?:\[[^\]]*\]\s*)?([^;#]*)")
# Only plain release versions: the probe's key cannot order pre-release numbers.
_SPECIFIER_RE = re.compile(r"^\s*(==|>=)\s*(\d+(?:\.\d+)*)\s*$")

# Runs inside the environment, so it may only use the standard library.
# Versions compare by their numeric release; a pre-release (a, b, rc, dev)
# sorts below its release and any other suffix (post, local) above it. That is
# exact only because constraint versions are plain releases (see _SPECIFIER_RE).
_PROBE = """\
import re
from importlib.metadata import version


def key(text):
    match = re.match(r"\\d+(?:\\.\\d+)*", text)
    release = [int(part) for part in match.group(0).split(".")] if match else []
    while release and release[-1] == 0:
        release.pop()
    rest = text[match.end():] if match else text
    rank = 1 if not rest else 0 if re.match(r"[.-]?(a|b|c|rc|alpha|beta|pre|preview|dev)", rest) else 2
    return release, rank


for name, constraints in CONSTRAINTS:
    installed = version(name)
    for op, wanted in constraints:
        if op == "==":
            ok = installed == wanted or key(installed) == key(wanted)
        else:
            ok = key(installed) >= key(wanted)
        assert ok, (name, installed, op, wanted)
"""

# One install at a time per environment: a start and an action may race.
_locks: dict[str, threading.Lock] = {}
_locks_guard = threading.Lock()

# run(args) -> (exit code, captured output); the output may be '' when logged elsewhere.
Runner = Callable[[Sequence[str]], "tuple[int, str]"]


class PyEnvError(RuntimeError):
    """The environment could not be prepared; the message is shown to the user."""


class PyEnvCancelled(PyEnvError):
    """should_stop() fired while waiting for another install of the same environment."""

    def __init__(self) -> None:
        super().__init__("Preparing the bridge dependencies was cancelled.")


@dataclass(frozen=True)
class PythonEnv:
    """A named environment and the requirement specifiers it must satisfy."""

    name: str
    requirements: tuple[str, ...]

    def __post_init__(self) -> None:
        self.probe_code()  # reject unsupported specifiers when the env is declared

    def directory(self, state_dir: Path) -> Path:
        return Path(state_dir) / "pyenv" / self.name

    def python(self, state_dir: Path) -> Path:
        directory = self.directory(state_dir)
        if os.name == "nt":
            return directory / "Scripts" / "python.exe"
        return directory / "bin" / "python"

    def probe_code(self) -> str:
        """Python that exits non-zero unless every requirement is installed.

        Presence, exact pins (==), and minimum versions (>=) are checked; that
        is what decides whether an existing environment can be reused. Other
        specifiers are not evaluated, so only these two may be used, each with a
        plain release version (no pre-release, post or local suffix).
        """
        constraints: list[tuple[str, list[tuple[str, str]]]] = []
        for spec in self.requirements:
            match = _REQUIREMENT_RE.match(spec)
            if not match:
                continue
            checks = []
            for part in match.group(2).split(","):
                if not part.strip():
                    continue
                specifier = _SPECIFIER_RE.match(part)
                if not specifier:
                    raise ValueError(
                        f"unsupported requirement specifier in {spec!r}; use == or >= with a release version"
                    )
                checks.append((specifier.group(1), specifier.group(2)))
            constraints.append((match.group(1), checks))
        return f"CONSTRAINTS = {constraints!r}\n" + _PROBE


def requirements_file(path: Path) -> tuple[str, ...]:
    """Requirement lines of a requirements.txt, without comments."""
    lines = []
    for line in Path(path).read_text(encoding="utf-8").splitlines():
        line = line.split("#", 1)[0].strip()
        if line:
            lines.append(line)
    return tuple(lines)


def uv_environ(environ: Mapping[str, str]) -> dict[str, str]:
    """Environment for uv commands: plain text output for logs and the UI."""
    result = dict(environ)
    for name in ("FORCE_COLOR", "CLICOLOR_FORCE", "UV_COLOR"):
        result.pop(name, None)
    result["NO_COLOR"] = "1"
    return result


def _lock(directory: Path) -> threading.Lock:
    with _locks_guard:
        return _locks.setdefault(str(directory), threading.Lock())


def is_ready(system: System, env: PythonEnv, state_dir: Path) -> bool:
    python = env.python(state_dir)
    if not python.exists():
        return False
    return system.run([str(python), "-c", env.probe_code()], timeout=60).returncode == 0


def ensure(
    system: System,
    env: PythonEnv,
    state_dir: Path,
    run: Runner,
    *,
    should_stop: Callable[[], bool] | None = None,
) -> tuple[str, bool]:
    """Return (python path, installed) after making sure env satisfies its requirements.

    ``run`` executes the uv commands, so the caller decides where their output
    goes (a bridge log while starting, the action result otherwise). While
    another install of the same environment runs, this waits for it, raising
    :class:`PyEnvCancelled` as soon as ``should_stop()`` returns true.
    """
    directory = env.directory(state_dir)
    lock = _lock(directory)
    while not lock.acquire(timeout=0.5):
        if should_stop is not None and should_stop():
            raise PyEnvCancelled()
    try:
        return _ensure_locked(system, env, state_dir, run)
    finally:
        lock.release()


def _ensure_locked(system: System, env: PythonEnv, state_dir: Path, run: Runner) -> tuple[str, bool]:
    directory = env.directory(state_dir)
    python = env.python(state_dir)
    if is_ready(system, env, state_dir):
        return str(python), False
    uv = system.which("uv")
    if not uv:
        raise PyEnvError(UV_MISSING)
    if not python.exists():
        # A directory without an interpreter is a broken earlier attempt.
        shutil.rmtree(directory, ignore_errors=True)
        directory.parent.mkdir(parents=True, exist_ok=True)
        code, output = run([uv, "venv", "--python", PYTHON_VERSION, str(directory)])
        if code != 0:
            raise PyEnvError(_failure(f"uv venv for {env.name}", code, output))
    code, output = run([uv, "pip", "install", "--python", str(python), *env.requirements])
    if code != 0:
        raise PyEnvError(_failure(f"Installing {env.name} bridge dependencies", code, output))
    if not is_ready(system, env, state_dir):
        raise PyEnvError(
            f"The {env.name} bridge dependencies are still incomplete after installing them "
            f"({', '.join(env.requirements)})."
        )
    return str(python), True


def _failure(what: str, code: int, output: str) -> str:
    detail = output.strip().splitlines()[-1] if output.strip() else ""
    if code == -1:
        return f"{what} was cancelled."
    return f"{what} failed (exit code {code})" + (f": {detail}" if detail else "; see the log.")

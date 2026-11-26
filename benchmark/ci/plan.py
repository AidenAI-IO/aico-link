from __future__ import annotations

import argparse
import dataclasses as dc
import json
from pathlib import Path
from typing import Any


BENCHMARK_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_CATALOG_PATH = Path(__file__).with_name("suites.json")
DEFAULT_SUITES_DIR = BENCHMARK_ROOT / "suites"
# Tasks that must pass before baseline code is accepted; see load_gate.
DEFAULT_GATE_PATH = Path(__file__).with_name("baseline_gate_v1.json")
ENVIRONMENTS = {"isolated", "mobilegym", "adb", "external"}
# ADB cases use the CI emulator. They are runnable automatically while still
# belonging to the hardware profile and sharing its exclusive device lock.
RUNNABLE_ENVIRONMENTS = {"isolated", "mobilegym", "adb"}
HARDWARE_ENVIRONMENTS = {"adb", "external"}
PROFILE_ENVIRONMENTS = {
    "runnable": RUNNABLE_ENVIRONMENTS,
    "hardware": HARDWARE_ENVIRONMENTS,
    "adb": {"adb"},
    "all": ENVIRONMENTS,
}
PROFILES = set(PROFILE_ENVIRONMENTS)
TARGET_PLATFORMS = {"auto", "ios", "android", "mac", "windows", "linux"}

if RUNNABLE_ENVIRONMENTS | HARDWARE_ENVIRONMENTS != ENVIRONMENTS:
    raise RuntimeError("runnable and hardware environments must cover all environments")


class CatalogError(ValueError):
    pass


@dc.dataclass(frozen=True)
class SuiteCase:
    id: str
    suite: str
    environment: str
    environment_variable: str = ""
    target_platform: str = "auto"
    # Parallel tasks. An ADB case runs one task per device, so this is also the
    # most devices it leases from the pool; fewer are used when fewer exist.
    max_concurrency: int = 1
    timeout_minutes: int = 180
    # False keeps the case out of the automatic runnable profile; it stays
    # selectable by explicit --suite (workflow_dispatch).
    runnable: bool = True
    android_phone_bridge: bool = False
    execution_mode: str = "backend-text"
    # Reuse a suite's tasks without copying or changing its scoring contract.
    task_ids: list[str] = dc.field(default_factory=list)

    def matrix_entry(self) -> dict[str, Any]:
        return dc.asdict(self)


@dc.dataclass(frozen=True)
class Catalog:
    cases: tuple[SuiteCase, ...]


def _suite_paths(suites_dir: Path) -> set[str]:
    return {
        f"suites/{path.relative_to(suites_dir).as_posix()}"
        for path in suites_dir.rglob("*.json")
        if not path.name.startswith("._")
    }


def _case_from_raw(raw: Any, index: int) -> SuiteCase:
    if not isinstance(raw, dict):
        raise CatalogError(f"case {index} must be an object")
    try:
        case = SuiteCase(**raw)
    except TypeError as exc:
        raise CatalogError(f"invalid case {index}: {exc}") from exc
    if not case.id or not case.id.replace("-", "").replace("_", "").isalnum():
        raise CatalogError(f"invalid case id: {case.id!r}")
    if not case.suite.startswith("suites/") or not case.suite.endswith(".json"):
        raise CatalogError(f"invalid suite path for {case.id}: {case.suite!r}")
    if case.execution_mode not in {"backend-text", "realtime-text"}:
        raise CatalogError(f"unsupported execution mode for {case.id}: {case.execution_mode}")
    if case.execution_mode == "realtime-text" and case.environment != "isolated":
        raise CatalogError(f"realtime-text case {case.id} requires an isolated environment")
    if case.environment not in ENVIRONMENTS:
        raise CatalogError(f"unsupported environment for {case.id}: {case.environment}")
    if case.target_platform not in TARGET_PLATFORMS:
        raise CatalogError(
            f"unsupported target platform for {case.id}: {case.target_platform}"
        )
    if case.environment == "external" and not case.environment_variable:
        raise CatalogError(f"external case {case.id} requires environment_variable")
    if case.android_phone_bridge and (
        case.environment != "adb"
        or case.target_platform != "android"
        or case.max_concurrency != 1
    ):
        raise CatalogError(
            f"{case.id} requires a single Android ADB worker for phone bridge"
        )
    if case.max_concurrency < 1:
        raise CatalogError(f"max_concurrency for {case.id} must be positive")
    if case.timeout_minutes < 1:
        raise CatalogError(f"timeout_minutes for {case.id} must be positive")
    if not isinstance(case.task_ids, list) or not all(
        isinstance(task_id, str) and task_id.strip() == task_id and task_id
        for task_id in case.task_ids
    ):
        raise CatalogError(f"task_ids for {case.id} must be an array of non-empty IDs")
    if len(set(case.task_ids)) != len(case.task_ids):
        raise CatalogError(f"duplicate task_ids for {case.id}")
    return case


def load_catalog(
    *,
    catalog_path: Path = DEFAULT_CATALOG_PATH,
    suites_dir: Path = DEFAULT_SUITES_DIR,
) -> Catalog:
    try:
        raw = json.loads(catalog_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CatalogError(f"cannot read catalog {catalog_path}: {exc}") from exc
    if not isinstance(raw, dict) or raw.get("version") != 1:
        raise CatalogError("catalog version must be 1")
    raw_cases = raw.get("cases")
    if not isinstance(raw_cases, list):
        raise CatalogError("catalog cases must be an array")

    cases = tuple(_case_from_raw(item, index) for index, item in enumerate(raw_cases))
    ids = [case.id for case in cases]
    duplicate_ids = sorted({case_id for case_id in ids if ids.count(case_id) > 1})
    if duplicate_ids:
        raise CatalogError(f"duplicate case ids: {', '.join(duplicate_ids)}")

    discovered = _suite_paths(suites_dir)
    classified = {case.suite for case in cases}
    missing = sorted(discovered - classified)
    unknown = sorted(classified - discovered)
    if missing:
        raise CatalogError(f"unclassified suites: {', '.join(missing)}")
    if unknown:
        raise CatalogError(f"catalog references missing suites: {', '.join(unknown)}")
    for case in cases:
        if not case.task_ids:
            continue
        by_id = _suite_tasks(suites_dir, case)
        for task_id in case.task_ids:
            if task_id not in by_id:
                raise CatalogError(f"unknown task {task_id!r} for {case.id}")
            platforms = by_id[task_id].get("platforms", [])
            if platforms and case.target_platform != "auto" and case.target_platform not in platforms:
                raise CatalogError(f"task {task_id!r} does not support {case.target_platform} for {case.id}")
    return Catalog(cases=cases)


def _suite_tasks(suites_dir: Path, case: SuiteCase) -> dict[str, Any]:
    path = suites_dir / case.suite.removeprefix("suites/")
    try:
        tasks = json.loads(path.read_text(encoding="utf-8"))["tasks"]
        return {task["id"]: task for task in tasks}
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise CatalogError(f"cannot read tasks for {case.id}: {exc}") from exc


def load_gate(
    catalog: Catalog,
    *,
    gate_path: Path = DEFAULT_GATE_PATH,
    suites_dir: Path = DEFAULT_SUITES_DIR,
) -> tuple[SuiteCase, ...]:
    """Return catalog cases narrowed to the gate's must-pass tasks.

    A gate entry names a catalog case and a subset of its tasks, so gated tasks
    keep the case's environment, concurrency and scoring contract unchanged.
    """
    try:
        raw = json.loads(gate_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CatalogError(f"cannot read gate {gate_path}: {exc}") from exc
    if not isinstance(raw, dict) or raw.get("version") != 1:
        raise CatalogError("gate version must be 1")
    entries = raw.get("cases")
    if not isinstance(entries, list) or not entries:
        raise CatalogError("gate cases must be a non-empty array")
    by_case = {case.id: case for case in catalog.cases}
    selected: list[SuiteCase] = []
    for index, entry in enumerate(entries):
        if not isinstance(entry, dict) or set(entry) != {"case", "task_ids"}:
            raise CatalogError(f"gate entry {index} must have exactly case and task_ids")
        case = by_case.get(entry["case"])
        if case is None:
            raise CatalogError(f"gate references unknown case: {entry['case']!r}")
        if any(item.id == case.id for item in selected):
            raise CatalogError(f"duplicate gate case: {case.id}")
        if case.environment not in RUNNABLE_ENVIRONMENTS:
            # The gate runs unattended on the benchmark runner.
            raise CatalogError(f"gate case {case.id} needs external hardware")
        task_ids = entry["task_ids"]
        if (
            not isinstance(task_ids, list)
            or not task_ids
            or not all(isinstance(task_id, str) and task_id for task_id in task_ids)
            or len(set(task_ids)) != len(task_ids)
        ):
            raise CatalogError(f"gate task_ids for {case.id} must be unique non-empty IDs")
        available = set(case.task_ids) or set(_suite_tasks(suites_dir, case))
        unknown = [task_id for task_id in task_ids if task_id not in available]
        if unknown:
            raise CatalogError(f"gate tasks not in case {case.id}: {', '.join(unknown)}")
        selected.append(dc.replace(case, task_ids=list(task_ids)))
    return tuple(selected)


def select_cases(
    catalog: Catalog,
    *,
    profile: str,
    suite: str = "",
) -> tuple[SuiteCase, ...]:
    if profile not in PROFILES:
        raise CatalogError(f"unsupported profile: {profile}")
    suites = list(dict.fromkeys(item.strip() for item in suite.split(",") if item.strip()))
    if suite.strip() and not suites:
        # A typo such as "," must not silently widen to the whole profile.
        raise CatalogError(f"suite input contains no suite paths: {suite!r}")
    if len(suites) > 1:
        # A comma-separated list selects each suite under the same rules as a
        # single suite, so one dispatch can verify several related suites.
        return tuple(
            case
            for item in suites
            for case in select_cases(catalog, profile=profile, suite=item)
        )
    suite = suites[0] if suites else ""
    if suite:
        selected = tuple(case for case in catalog.cases if case.suite == suite)
        if not selected:
            raise CatalogError(f"suite is not in the CI catalog: {suite}")
        if profile != "all":
            allowed_environments = PROFILE_ENVIRONMENTS[profile]
            selected = tuple(
                case for case in selected if case.environment in allowed_environments
            )
            if not selected:
                raise CatalogError(
                    f"suite {suite} is outside the {profile} profile; "
                    "select the matching profile or all"
                )
        return selected
    if profile == "runnable":
        return tuple(
            case
            for case in catalog.cases
            if case.environment in RUNNABLE_ENVIRONMENTS and case.runnable
        )
    if profile == "hardware":
        return tuple(
            case for case in catalog.cases if case.environment in HARDWARE_ENVIRONMENTS
        )
    if profile == "adb":
        return tuple(case for case in catalog.cases if case.environment == "adb")
    return catalog.cases


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Validate and select benchmark CI suites")
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("validate")
    matrix = subparsers.add_parser("matrix")
    matrix.add_argument("--profile", choices=sorted(PROFILES), required=True)
    matrix.add_argument("--suite", default="")
    gate_matrix = subparsers.add_parser("gate-matrix")
    gate_matrix.add_argument("--gate", type=Path, default=DEFAULT_GATE_PATH)
    args = parser.parse_args(argv)

    try:
        catalog = load_catalog()
        if args.command == "validate":
            gated = load_gate(catalog)
            print(f"benchmark CI catalog covers {len(_suite_paths(DEFAULT_SUITES_DIR))} suites")
            print(f"baseline gate covers {sum(len(case.task_ids) for case in gated)} tasks")
            return 0
        if args.command == "gate-matrix":
            cases = load_gate(catalog, gate_path=args.gate)
        else:
            cases = select_cases(catalog, profile=args.profile, suite=args.suite)
    except CatalogError as exc:
        parser.error(str(exc))
    print(json.dumps({"include": [case.matrix_entry() for case in cases]}, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())

from __future__ import annotations
import dataclasses as dc
import hashlib
import json
import math
import re
from pathlib import Path
from typing import Any

from runner.platform import (
    TargetPlatform,
    normalize_target_platform,
    resolve_mock_platform,
)

VALID_CATEGORIES = {"diagnostic", "single_step", "multi_step", "memory", "perception", "device_operation"}
SETUP_KEYS = {
    "agent_prompt": {"type", "prompt", "timeout_sec", "clear_history_after", "expected_response", "device_prepare"},
    "environment_tools": {"type", "tools", "expected_state"},
    "seed_memory": {"type", "memories", "timeout_sec", "clear_history_after"},
    "seed_episode": {"type", "episode", "consolidate", "timeout_sec", "consolidation_expectation", "evaluate"},
    "seed_session_chunk": {"type", "session_id", "summary", "messages", "timeout_sec", "clear_history_after"},
    "seed_notification": {
        "type", "events", "consolidate", "timeout_sec",
        "expected_memory_count", "expected_memory_scope", "expected_memory_query", "evaluate",
    },
    "assert_memory": {
        "type", "query", "expected", "absent_ids", "expected_count", "timeout_sec", "evaluate",
    },
}

ASSERT_MEMORY_EXPECTED_KEYS = {
    "id", "memory_scope", "type", "revision", "content_contains",
    "title_contains", "tags_contains", "entities_contains",
    "source_refs_contain", "evidence_refs_contain", "sensitive",
}
ASSERT_MEMORY_REFERENCE_KEYS = {"type", "id", "event_ids_contains"}

class SuiteValidationError(ValueError):
    pass

@dc.dataclass
class RubricItem:
    id: str
    check: str

@dc.dataclass
class TraceObservationSpec:
    id: str
    description: str
    skill_name: str = ""
    tool_name: str = ""
    input_contains: dict[str, Any] = dc.field(default_factory=dict)

@dc.dataclass
class RequiredToolCallSpec:
    tool: str
    input_contains: dict[str, Any] = dc.field(default_factory=dict)
    # Only the first remaining call to this tool may satisfy the requirement.
    first_attempt: bool = False

@dc.dataclass
class HardAssertions:
    min_tool_calls: int = 0
    max_tool_calls: int = 50
    must_complete_within_sec: int = 180
    response_required: bool = True
    required_tools: list[str] = dc.field(default_factory=list)
    forbidden_tools: list[str] = dc.field(default_factory=list)
    prohibited_actions: list[str] = dc.field(default_factory=list)
    required_tool_calls: list[RequiredToolCallSpec] = dc.field(default_factory=list)

@dc.dataclass
class ConsolidationExpectation:
    """Assertions over the response returned by process_episode_memory."""

    goal_result: str | None = None
    min_memory_ids: int = 0
    max_memory_ids: int | None = None
    allow_empty_memory: bool = False
    required_assessment_evidence: bool = False
    expected_status: str | None = None
    forbidden_memory_substrings: list[str] = dc.field(default_factory=list)
    required_memory_substrings: list[str] = dc.field(default_factory=list)
    required_memory_types: list[str] = dc.field(default_factory=list)
    required_procedure_steps: list[str] = dc.field(default_factory=list)
    required_memory_scope: dict[str, str] = dc.field(default_factory=dict)

@dc.dataclass
class MockToolResponseSpec:
    input_contains: dict[str, Any] = dc.field(default_factory=dict)
    screen_contains: str = ""
    output: Any = dc.field(default_factory=dict)
    is_error: bool = False
    error: str = ""
    screen_text: str = ""

@dc.dataclass
class MockEnvironmentSpec:
    platform: TargetPlatform
    phone_bridge: dict[str, Any]
    tools: dict[str, list[MockToolResponseSpec]]
    single_frame: bool = False
    screen: str | None = None
    screen_text: str = ""
    default_tool_response: MockToolResponseSpec | None = None
    collaboration_fixture: dict[str, Any] | None = None

@dc.dataclass
class ScenarioSpec:
    version: int
    steps: list[dict[str, Any]]
    completion: dict[str, Any]
    assertions: list[dict[str, Any]]
    policies: list[str]
    timeout_sec: int = 300

@dc.dataclass
class TaskSpec:
    id: str
    category: str
    description_for_judge: str
    prompt: str
    rubric: list[RubricItem]
    hard_assertions: HardAssertions
    setup: dict[str, Any] | list[dict[str, Any]] | None = None
    mock_environment: MockEnvironmentSpec | None = None
    repeats: int = 1
    input_screenshot: str | None = None
    expected_answer: str | None = None
    answer_format: str | None = None
    platforms: list[str] = dc.field(default_factory=list)
    environment_bridge_types: list[str] = dc.field(default_factory=list)
    expected_recalled_memory_ids: list[str] = dc.field(default_factory=list)
    expected_recalled_memory_tool: str = "recall_memory"
    expected_recall_from_consolidation: bool = False
    app_ids: list[str] = dc.field(default_factory=list)
    foreground_app_id: str | None = None
    environment_assertions: dict[str, Any] = dc.field(default_factory=dict)
    android_bridge_input_text: str | None = None
    android_bridge_fixture: str | None = None
    memory_assertions: dict[str, Any] | None = None
    consolidation_expectation: ConsolidationExpectation | None = None
    scenario: ScenarioSpec | None = None

@dc.dataclass
class Suite:
    name: str
    global_reset: dict[str, Any]
    tasks: list[TaskSpec]
    sha256: str
    source_path: Path
    source_bytes: bytes = b""
    prompt_prefix: str = ""
    trace_observations: list[TraceObservationSpec] = dc.field(default_factory=list)
    mock_environment: MockEnvironmentSpec | None = None
    status: str = "active"


def effective_mock_environment(
    suite: Suite,
    task: TaskSpec,
) -> MockEnvironmentSpec | None:
    return task.mock_environment or suite.mock_environment


def resolve_mock_task_platform(
    suite: Suite,
    task: TaskSpec,
    *,
    constraint: str | TargetPlatform | None = None,
) -> TargetPlatform:
    spec = effective_mock_environment(suite, task)
    if spec is None:
        raise ValueError(
            f"mock environment task {task.id!r} has no mock environment"
        )
    return resolve_mock_platform(spec.platform, constraint=constraint)

def load_suite(path: Path) -> Suite:
    raw_bytes = Path(path).read_bytes()
    sha = hashlib.sha256(raw_bytes).hexdigest()
    try:
        data = json.loads(raw_bytes.decode("utf-8"))
    except json.JSONDecodeError as e:
        raise SuiteValidationError(f"invalid JSON: {e}") from e
    if not isinstance(data.get("tasks"), list):
        raise SuiteValidationError("suite must contain a 'tasks' array")
    seen = set()
    tasks: list[TaskSpec] = []
    for raw in data["tasks"]:
        tid = raw.get("id")
        if not tid or tid in seen:
            raise SuiteValidationError(f"missing or duplicate task id: {tid!r}")
        seen.add(tid)
        cat = raw.get("category")
        if cat not in VALID_CATEGORIES:
            raise SuiteValidationError(f"task {tid}: invalid category {cat!r}")
        scenario = _parse_scenario(raw.get("scenario"), tid)
        rubric_raw = raw.get("rubric") or []
        rubric: list[RubricItem] = []
        for r in rubric_raw:
            if not isinstance(r, dict):
                raise SuiteValidationError(f"task {tid}: rubric items must be objects")
            check = r.get("check", r.get("description"))
            if not r.get("id") or not check:
                raise SuiteValidationError(f"task {tid}: rubric items require id and check")
            rubric.append(RubricItem(id=r["id"], check=check))
        ha = raw.get("hard_assertions") or {}
        # A task may omit the judge only when deterministic call checks decide it.
        if not rubric_raw and not ha.get("required_tool_calls") and scenario is None:
            raise SuiteValidationError(f"task {tid}: empty rubric requires hard_assertions.required_tool_calls")
        # Validate hard_assertions types
        rr = ha.get("response_required", True)
        if isinstance(rr, str):
            rr = rr.lower() == "true"
        elif not isinstance(rr, bool):
            raise SuiteValidationError(f"task {tid}: response_required must be bool")
        try:
            min_tc = int(ha.get("min_tool_calls", 0))
            max_tc = int(ha.get("max_tool_calls", 50))
            timeout_sec = int(ha.get("must_complete_within_sec", 180))
        except (ValueError, TypeError) as e:
            raise SuiteValidationError(f"task {tid}: invalid hard_assertions numeric value: {e}") from e
        if min_tc < 0 or max_tc < 0 or timeout_sec <= 0:
            raise SuiteValidationError(f"task {tid}: hard_assertions values must be non-negative")
        required_tools = _string_list_assertion(ha.get("required_tools", []), tid, "required_tools")
        forbidden_tools = _string_list_assertion(ha.get("forbidden_tools", []), tid, "forbidden_tools")
        prohibited_actions = _string_list_assertion(ha.get("prohibited_actions", []), tid, "prohibited_actions")
        required_tool_calls = _required_tool_call_assertions(
            ha.get("required_tool_calls", []), tid
        )
        overlap = sorted(set(required_tools) & set(forbidden_tools))
        if overlap:
            raise SuiteValidationError(
                f"task {tid}: hard_assertions has overlapping required/forbidden tools: {overlap}"
            )
        hard = HardAssertions(
            min_tool_calls=min_tc,
            max_tool_calls=max_tc,
            must_complete_within_sec=timeout_sec,
            response_required=rr,
            required_tools=required_tools,
            forbidden_tools=forbidden_tools,
            prohibited_actions=prohibited_actions,
            required_tool_calls=required_tool_calls,
        )
        # Validate and bound repeats
        try:
            repeats = int(raw.get("repeats", 1))
        except (ValueError, TypeError) as e:
            raise SuiteValidationError(f"task {tid}: invalid repeats: {e}") from e
        if repeats < 1:
            repeats = 1
        elif repeats > 100:
            raise SuiteValidationError(f"task {tid}: repeats {repeats} exceeds max 100")
        expected_answer = raw.get("expected_answer")
        answer_format = raw.get("answer_format")
        if expected_answer is None and answer_format is not None:
            raise SuiteValidationError(f"task {tid}: answer_format requires expected_answer")
        if expected_answer is not None:
            if not isinstance(expected_answer, str):
                raise SuiteValidationError(f"task {tid}: expected_answer must be string")
            answer_format = answer_format or "option_letter"
            if answer_format == "option_letter":
                matches = re.findall(r"\(([a-dA-D])\)|\b([a-dA-D])\b", expected_answer)
                letters = [a or b for a, b in matches]
                if len(letters) != 1:
                    raise SuiteValidationError(
                        f"task {tid}: expected_answer must contain exactly one option A-D, got {expected_answer!r}"
                    )
        if answer_format is not None and answer_format != "option_letter":
            raise SuiteValidationError(f"task {tid}: unsupported answer_format {answer_format!r}")
        expected_recalled_memory_ids = raw.get("expected_recalled_memory_ids", [])
        if not isinstance(expected_recalled_memory_ids, list) or not all(
            isinstance(item, str) and item.strip() for item in expected_recalled_memory_ids
        ):
            raise SuiteValidationError(f"task {tid}: expected_recalled_memory_ids must be a list of non-empty strings")
        expected_recalled_memory_tool = raw.get("expected_recalled_memory_tool", "recall_memory")
        if expected_recalled_memory_tool not in {"recall_memory", "recall_device_memory"}:
            raise SuiteValidationError(
                f"task {tid}: expected_recalled_memory_tool must be recall_memory or recall_device_memory"
            )
        expected_recall_from_consolidation = raw.get("expected_recall_from_consolidation", False)
        if not isinstance(expected_recall_from_consolidation, bool):
            raise SuiteValidationError(
                f"task {tid}: expected_recall_from_consolidation must be boolean"
            )
        raw_app_ids = raw.get("app_ids", [])
        if not isinstance(raw_app_ids, list) or not all(
            isinstance(item, str) and item.strip() for item in raw_app_ids
        ):
            raise SuiteValidationError(f"task {tid}: app_ids must be a list of non-empty strings")
        app_ids = list(dict.fromkeys(item.strip() for item in raw_app_ids))
        foreground_app_id = raw.get("foreground_app_id")
        if foreground_app_id is not None and (
            not isinstance(foreground_app_id, str)
            or not foreground_app_id.strip()
            or foreground_app_id != foreground_app_id.strip()
        ):
            raise SuiteValidationError(f"task {tid}: foreground_app_id must be a non-empty app ID")
        environment_assertions = _parse_environment_assertions(
            raw.get("environment_assertions"), tid
        )
        android_bridge_input_text = raw.get("android_bridge_input_text")
        if android_bridge_input_text is not None and (
            not isinstance(android_bridge_input_text, str) or not android_bridge_input_text
        ):
            raise SuiteValidationError(f"task {tid}: android_bridge_input_text must be non-empty text")
        android_bridge_fixture = raw.get("android_bridge_fixture")
        if android_bridge_fixture not in (None, "calendar", "contacts"):
            raise SuiteValidationError(f"task {tid}: android_bridge_fixture must be calendar or contacts")
        platforms = _platform_list(raw.get("platforms", []), tid)
        environment_bridge_types = raw.get("environment_bridge_types", [])
        if not isinstance(environment_bridge_types, list) or any(
            not isinstance(kind, str) or not re.fullmatch(r"[a-z][a-z0-9_]*", kind)
            for kind in environment_bridge_types
        ):
            raise SuiteValidationError(f"task {tid}: environment_bridge_types must be a list of bridge type names")
        task_mock_environment = _parse_mock_environment(
            raw.get("mock_environment"),
            path=f"task {tid}.mock_environment",
        )
        setup = raw.get("setup")
        if setup is not None:
            setup_items = setup if isinstance(setup, list) else [setup]
            if not setup_items:
                raise SuiteValidationError(f"task {tid}: setup sequence must not be empty")
            for setup_index, setup_item in enumerate(setup_items):
                if not isinstance(setup_item, dict):
                    raise SuiteValidationError(
                        f"task {tid}: setup[{setup_index}] must be an object"
                    )
                setup_type = setup_item.get("type")
                if not isinstance(setup_type, str):
                    raise SuiteValidationError(
                        f"task {tid}: setup type must be a string (setup[{setup_index}])"
                    )
                allowed_setup_keys = SETUP_KEYS.get(setup_type)
                if allowed_setup_keys is None:
                    raise SuiteValidationError(
                        f"task {tid}: unsupported setup type {setup_type!r}"
                    )
                unknown_setup_keys = sorted(set(setup_item) - allowed_setup_keys)
                if unknown_setup_keys:
                    raise SuiteValidationError(
                        f"task {tid}: unsupported {setup_type} setup keys: {', '.join(unknown_setup_keys)}"
                    )
                validate_setup_evaluation(setup_item)
                validate_setup_device_prepare(setup_item)
                if setup_type == "environment_tools":
                    tools = setup_item.get("tools")
                    if not isinstance(tools, list) or not tools:
                        raise SuiteValidationError(f"task {tid}: environment_tools requires a non-empty tools list")
                    for tool in tools:
                        if (not isinstance(tool, dict) or set(tool) != {"name", "input"}
                                or not isinstance(tool["name"], str) or not tool["name"].strip()
                                or not isinstance(tool["input"], dict)):
                            raise SuiteValidationError(f"task {tid}: invalid environment_tools entry")
                    if "expected_state" in setup_item:
                        _parse_environment_assertions(setup_item["expected_state"], tid)
                if setup_type == "assert_memory":
                    validate_assert_memory_setup(
                        setup_item,
                        path=f"task {tid}: setup[{setup_index}] assert_memory",
                    )
                if setup_type == "seed_notification":
                    expected_query = setup_item.get("expected_memory_query")
                    if expected_query is not None and not isinstance(expected_query, dict):
                        raise SuiteValidationError(
                            f"task {tid}: setup[{setup_index}] seed_notification "
                            "expected_memory_query must be an object"
                        )
            consolidation_expectation = None
            seed_episode_expectations = [
                setup_item
                for setup_item in setup_items
                if setup_item.get("type") == "seed_episode"
                and setup_item.get("consolidation_expectation") is not None
            ]
            if len(seed_episode_expectations) > 1:
                raise SuiteValidationError(
                    f"task {tid}: only one seed_episode may declare consolidation_expectation"
                )
            if seed_episode_expectations:
                expectation_setup_item = seed_episode_expectations[0]
                consolidation_expectation = _parse_consolidation_expectation(
                    expectation_setup_item.get("consolidation_expectation"), tid
                )
                if expectation_setup_item.get("consolidate", False) is not True:
                    raise SuiteValidationError(
                        f"task {tid}: consolidation_expectation requires consolidate=true"
                    )
        else:
            consolidation_expectation = None
        memory_assertions = raw.get("memory_assertions")
        if memory_assertions is not None:
            allowed = SETUP_KEYS["assert_memory"] - {"type", "evaluate"}
            if not isinstance(memory_assertions, dict) or not memory_assertions or set(memory_assertions) - allowed:
                raise SuiteValidationError(f"task {tid}: invalid memory_assertions")
            if not (
                memory_assertions.get("expected_count") is not None
                or memory_assertions.get("expected")
                or memory_assertions.get("absent_ids")
            ):
                raise SuiteValidationError(f"task {tid}: memory_assertions requires an expectation")
            validate_assert_memory_setup(memory_assertions, path=f"task {tid}: memory_assertions")
        tasks.append(TaskSpec(
            id=tid, category=cat,
            description_for_judge=raw["description_for_judge"],
            prompt=raw["prompt"],
            rubric=rubric, hard_assertions=hard,
            setup=setup,
            mock_environment=task_mock_environment,
            repeats=repeats,
            input_screenshot=raw.get("input_screenshot"),
            expected_answer=expected_answer,
            answer_format=answer_format,
            platforms=platforms,
            environment_bridge_types=environment_bridge_types,
            expected_recalled_memory_ids=expected_recalled_memory_ids,
            expected_recalled_memory_tool=expected_recalled_memory_tool,
            expected_recall_from_consolidation=expected_recall_from_consolidation,
            app_ids=app_ids,
            foreground_app_id=foreground_app_id,
            environment_assertions=environment_assertions,
            android_bridge_input_text=android_bridge_input_text,
            android_bridge_fixture=android_bridge_fixture,
            memory_assertions=memory_assertions,
            consolidation_expectation=consolidation_expectation,
            scenario=scenario,
        ))
    prompt_prefix = data.get("prompt_prefix", "")
    if not isinstance(prompt_prefix, str):
        raise SuiteValidationError("suite prompt_prefix must be a string")
    status = data.get("status", "active")
    if not isinstance(status, str) or not status.strip():
        raise SuiteValidationError("suite status must be a non-empty string")

    trace_observations: list[TraceObservationSpec] = []
    seen_obs_ids: set[str] = set()
    for raw_obs in data.get("trace_observations") or []:
        if not isinstance(raw_obs, dict):
            raise SuiteValidationError("trace_observations entries must be objects")
        obs_id = raw_obs.get("id")
        description = raw_obs.get("description")
        skill_name = str(raw_obs.get("skill_name") or "").strip()
        tool_name = str(raw_obs.get("tool_name") or "").strip()
        input_contains = raw_obs.get("input_contains") or {}
        if not isinstance(input_contains, dict):
            raise SuiteValidationError("trace_observations input_contains must be an object")
        if not obs_id or not description or not (skill_name or tool_name):
            raise SuiteValidationError(
                "trace_observations entries require id, description, and skill_name or tool_name"
            )
        if skill_name and tool_name:
            raise SuiteValidationError(
                f"trace_observations {obs_id!r}: specify only one of skill_name or tool_name"
            )
        if obs_id in seen_obs_ids:
            raise SuiteValidationError(f"duplicate trace_observations id: {obs_id!r}")
        seen_obs_ids.add(obs_id)
        trace_observations.append(
            TraceObservationSpec(
                id=obs_id,
                description=description,
                skill_name=skill_name,
                tool_name=tool_name,
                input_contains=input_contains,
            )
        )

    mock_environment = _parse_mock_environment(data.get("mock_environment"))
    for task in tasks:
        if task.scenario is None:
            continue
        fixture = (task.mock_environment or mock_environment)
        if fixture is None or fixture.collaboration_fixture is None:
            raise SuiteValidationError(f"task {task.id}: scenario requires collaboration_fixture")
        if task.setup or task.input_screenshot or task.memory_assertions or task.expected_answer or task.environment_assertions:
            raise SuiteValidationError(f"task {task.id}: unsupported legacy assertions/setup on a scenario")
        gates = {gate["id"] for gate in fixture.collaboration_fixture.get("gates", [])}
        for step in task.scenario.steps:
            gate = step.get("gate") or step.get("match", {}).get("gate")
            if gate is not None and gate not in gates:
                raise SuiteValidationError(f"task {task.id}: unknown fixture gate {gate}")
    return Suite(
        name=data.get("name", Path(path).stem),
        global_reset=data.get("global_reset") or {},
        tasks=tasks,
        sha256=sha,
        source_path=Path(path),
        source_bytes=raw_bytes,
        prompt_prefix=prompt_prefix,
        trace_observations=trace_observations,
        mock_environment=mock_environment,
        status=status.strip().lower(),
    )


def _parse_scenario(raw: Any, task_id: str) -> ScenarioSpec | None:
    if raw is None:
        return None
    if not isinstance(raw, dict) or type(raw.get("version")) is not int or raw.get("version") != 1 or set(raw) - {
        "version", "steps", "completion", "assertions", "policies", "timeout_sec",
    }:
        raise SuiteValidationError(f"task {task_id}: invalid scenario version or keys")
    steps = raw.get("steps", [])
    if not isinstance(steps, list):
        raise SuiteValidationError(f"task {task_id}: scenario.steps must be a list")
    known_steps = set()
    turns = {"initial"}
    tasks = set()
    revisions = set()
    for step in steps:
        if not isinstance(step, dict) or not isinstance(step.get("id"), str) or not step["id"]:
            raise SuiteValidationError(f"task {task_id}: scenario step requires id")
        if step["id"] in known_steps or step["id"] == "initial":
            raise SuiteValidationError(f"task {task_id}: duplicate scenario step id")
        known_steps.add(step["id"])
        kind = step.get("type")
        allowed = {
            "send": {"id", "type", "text"},
            "await": {"id", "type", "match", "bind_task", "allow_merge", "bind_revision", "timeout_sec", "requires_distinct"},
            "release": {"id", "type", "gate"},
            "assert": {"id", "type", "check", "requires_distinct"},
        }.get(kind)
        if allowed is None or set(step) - allowed:
            raise SuiteValidationError(f"task {task_id}: unsupported scenario step {kind!r}")
        if "requires_distinct" in step:
            _validate_requires_distinct(step["requires_distinct"], tasks, task_id)
        if kind == "send":
            if not isinstance(step.get("text"), str) or not step["text"].strip():
                raise SuiteValidationError(f"task {task_id}: send requires text")
            turns.add(step["id"])
        if kind == "await":
            match = step.get("match")
            if not isinstance(match, dict) or not isinstance(match.get("kind"), str) or not match["kind"] or set(match) - {
                "kind", "actor", "phase", "status", "turn", "task", "goal_revision", "applied_revision", "gate",
            }:
                raise SuiteValidationError(f"task {task_id}: invalid await match")
            if match["kind"].startswith("fixture.") and set(match) - {"kind", "phase", "gate", "task"}:
                raise SuiteValidationError(f"task {task_id}: invalid fixture match fields")
            for key in ("turn", "task", "gate", "actor", "phase", "status"):
                if key in match and (not isinstance(match[key], str) or not match[key]):
                    raise SuiteValidationError(f"task {task_id}: match {key} must be nonempty text")
            if match.get("turn") and match["turn"] not in turns:
                raise SuiteValidationError(f"task {task_id}: unknown turn alias {match['turn']}")
            if match.get("task") and match["task"] not in tasks:
                raise SuiteValidationError(f"task {task_id}: unknown task alias {match['task']}")
            for field in ("goal_revision", "applied_revision"):
                ref = match.get(field)
                if ref is not None and not ((type(ref) is int and ref > 0) or
                    (isinstance(ref, str) and ref.startswith("$revision.") and ref[10:] in revisions)):
                    raise SuiteValidationError(f"task {task_id}: invalid revision reference {ref}")
            if "bind_task" in step:
                if not isinstance(step["bind_task"], str) or not step["bind_task"] or step["bind_task"] in tasks:
                    raise SuiteValidationError(f"task {task_id}: invalid or duplicate task alias")
                if match.get("kind") != "task.updated" or match.get("phase") != "created" or not match.get("turn"):
                    raise SuiteValidationError(f"task {task_id}: bind_task requires a task-created event from a turn")
                tasks.add(step["bind_task"])
            if "allow_merge" in step and (step["allow_merge"] is not True or "bind_task" not in step or not tasks - {step["bind_task"]}):
                raise SuiteValidationError(f"task {task_id}: allow_merge requires bind_task and an earlier task to merge into")
            if "bind_revision" in step:
                if not isinstance(step["bind_revision"], str) or not step["bind_revision"] or step["bind_revision"] in revisions:
                    raise SuiteValidationError(f"task {task_id}: invalid or duplicate revision alias")
                if match.get("phase") != "goal_updated" or not match.get("task"):
                    raise SuiteValidationError(f"task {task_id}: bind_revision requires goal_updated on a task")
                revisions.add(step["bind_revision"])
            if "timeout_sec" in step and (type(step["timeout_sec"]) is not int or step["timeout_sec"] <= 0):
                raise SuiteValidationError(f"task {task_id}: await timeout_sec must be positive")
        if kind == "release" and not isinstance(step.get("gate"), str):
            raise SuiteValidationError(f"task {task_id}: release requires a gate id")
        if kind == "assert":
            _validate_scenario_check(step.get("check"), tasks, turns, task_id, final=False)
    completion = raw.get("completion")
    if not isinstance(completion, dict) or not completion:
        raise SuiteValidationError(f"task {task_id}: scenario completion is required")
    if "tasks" in completion:
        if set(completion) != {"tasks"}:
            raise SuiteValidationError(f"task {task_id}: conflicting completion forms")
        if not isinstance(completion["tasks"], list) or not completion["tasks"]:
            raise SuiteValidationError(f"task {task_id}: completion.tasks must be nonempty")
        for target in completion["tasks"]:
            if not isinstance(target, dict) or set(target) - {"task", "status", "goal_revision", "feedback"}:
                raise SuiteValidationError(f"task {task_id}: invalid completion task")
            if not isinstance(target.get("task"), str) or target["task"] not in tasks or not _valid_terminal_status(target.get("status")):
                raise SuiteValidationError(f"task {task_id}: invalid completion task")
            if target.get("feedback", "required") != "required":
                raise SuiteValidationError(f"task {task_id}: delegated completion requires feedback")
            target["feedback"] = "required"
            revision = target.get("goal_revision")
            if revision is not None and not ((type(revision) is int and revision > 0) or
                (isinstance(revision, str) and revision.startswith("$revision.") and revision[10:] in revisions)):
                raise SuiteValidationError(f"task {task_id}: invalid completion revision")
    elif completion != {"turn": "initial", "delegation": "none"}:
        raise SuiteValidationError(f"task {task_id}: unsupported direct completion")
    timeout = raw.get("timeout_sec", 300)
    if type(timeout) is not int or not 0 < timeout <= 1800:
        raise SuiteValidationError(f"task {task_id}: scenario timeout_sec must be positive")
    policies = raw.get("policies", ["supported_completion_claims"])
    if policies != ["supported_completion_claims"]:
        raise SuiteValidationError(f"task {task_id}: unsupported scenario policy")
    assertions = raw.get("assertions", [])
    if not isinstance(assertions, list) or any(not isinstance(check, dict) for check in assertions):
        raise SuiteValidationError(f"task {task_id}: scenario assertions must be objects")
    ids = set()
    for check in assertions:
        if "requires_distinct" in check:
            _validate_requires_distinct(check["requires_distinct"], tasks, task_id)
        _validate_scenario_check({k: v for k, v in check.items() if k != "requires_distinct"},
                                 tasks, turns, task_id, final=True)
        if check["id"] in ids:
            raise SuiteValidationError(f"task {task_id}: duplicate assertion id")
        ids.add(check["id"])
    return ScenarioSpec(version=1, steps=steps, completion=completion, assertions=assertions,
                        policies=policies, timeout_sec=timeout)


def _valid_terminal_status(status: Any) -> bool:
    """A delegated completion status, or a nonempty list of acceptable ones."""
    allowed = {"completed", "failed", "cancelled"}
    if isinstance(status, list):
        return bool(status) and len(set(status)) == len(status) and all(item in allowed for item in status)
    return status in allowed


def _validate_requires_distinct(aliases: Any, tasks: set[str], task_id: str) -> None:
    """A step or assertion that only applies when two bound aliases are separate tasks."""
    if (not isinstance(aliases, list) or len(aliases) != 2 or len(set(aliases)) != 2
            or any(not isinstance(alias, str) or alias not in tasks for alias in aliases)):
        raise SuiteValidationError(f"task {task_id}: requires_distinct needs two known task aliases")


def _validate_scenario_check(check: Any, tasks: set[str], turns: set[str], task_id: str, *, final: bool) -> None:
    fields = {
        "task_state": ({"kind", "task", "status", "started"}, {"task", "status"}),
        "tasks_created": ({"kind", "turn", "count"}, {"turn", "count"}),
        "no_effect": ({"kind", "operation", "input_contains"}, {"operation"}),
        "effect_count": ({"kind", "operation", "input_contains", "count"}, {"operation", "count"}),
        "fixture_call_count": ({"kind", "operation", "input_contains", "count"}, {"operation", "count"}),
        "fixture_failure": ({"kind", "operation", "input_contains"}, {"operation"}),
        "state_equals": ({"kind", "path", "value"}, {"path", "value"}),
    }
    if not isinstance(check, dict) or not isinstance(check.get("kind"), str) or check["kind"] not in fields:
        raise SuiteValidationError(f"task {task_id}: unsupported scenario assertion")
    allowed, required = fields[check["kind"]]
    if set(check) - (allowed | {"id"}) or not required <= set(check):
        raise SuiteValidationError(f"task {task_id}: invalid scenario assertion fields")
    if final and (not isinstance(check.get("id"), str) or not check["id"]):
        raise SuiteValidationError(f"task {task_id}: assertion requires id")
    for field, aliases in (("task", tasks), ("turn", turns)):
        if field in check and (not isinstance(check[field], str) or check[field] not in aliases):
            raise SuiteValidationError(f"task {task_id}: unknown assertion {field} alias")
    if "count" in check and (type(check["count"]) is not int or check["count"] < 0):
        raise SuiteValidationError(f"task {task_id}: assertion count must be non-negative")
    if "started" in check and type(check["started"]) is not bool:
        raise SuiteValidationError(f"task {task_id}: started must be boolean")
    if "status" in check and check["status"] not in {"queued", "running", "cancelling", "cancelled", "failed", "completed"}:
        raise SuiteValidationError(f"task {task_id}: invalid task status")
    if "operation" in check and check["operation"] not in {"bridge_clipboard", "bridge_notification"}:
        raise SuiteValidationError(f"task {task_id}: unsupported fixture operation")
    if "input_contains" in check and not isinstance(check["input_contains"], dict):
        raise SuiteValidationError(f"task {task_id}: invalid input selector")
    if "path" in check and (not isinstance(check["path"], str) or not check["path"].startswith("/")):
        raise SuiteValidationError(f"task {task_id}: state path must be a JSON Pointer")
    value = check.get("value")
    if isinstance(value, dict) and not (
            (set(value) == {"fixture_value"} and isinstance(value["fixture_value"], str) and value["fixture_value"].startswith("/"))
            or (set(value) == {"$contains"} and isinstance(value["$contains"], str) and value["$contains"])):
        raise SuiteValidationError(f"task {task_id}: invalid state value matcher")


def validate_setup_device_prepare(setup: dict[str, Any]) -> None:
    """Reject device_prepare settings that the recipe path cannot honor.

    A confirmed recipe replaces the agent prompt, so every key that describes the
    prompt run itself would silently stop taking effect: nothing reads the
    response, and no conversation is left behind for the task.
    """
    if "device_prepare" not in setup:
        return
    spec = setup["device_prepare"]
    if not isinstance(spec, dict) or set(spec) - {"recipe", "params"}:
        raise SuiteValidationError(
            "device_prepare must be an object with only recipe and params"
        )
    if not isinstance(spec.get("recipe"), str) or not spec["recipe"].strip():
        raise SuiteValidationError("device_prepare requires a non-empty recipe")
    params = spec.get("params", {})
    if params is not None and not isinstance(params, dict):
        raise SuiteValidationError("device_prepare params must be an object")
    if "expected_response" in setup:
        raise SuiteValidationError(
            "device_prepare cannot be combined with expected_response: a confirmed "
            "recipe replaces the agent prompt, so the expected response is never produced"
        )
    if setup.get("clear_history_after") is False:
        raise SuiteValidationError(
            "device_prepare cannot be combined with clear_history_after=false: a "
            "confirmed recipe leaves no setup conversation for the task, so the "
            "retained context would depend on whether preparation succeeded"
        )


def validate_setup_evaluation(setup: dict[str, Any]) -> None:
    if "evaluate" not in setup:
        return
    if not isinstance(setup["evaluate"], bool):
        raise SuiteValidationError("setup evaluate must be boolean")
    if setup["evaluate"] and setup.get("type") in {"seed_episode", "seed_notification"}:
        if setup.get("consolidate") is not True:
            raise SuiteValidationError("setup evaluate requires consolidate=true")


def validate_assert_memory_setup(
    setup: dict[str, Any],
    *,
    path: str = "assert_memory",
) -> None:
    expected_count = setup.get("expected_count")
    if expected_count is not None and (
        isinstance(expected_count, bool) or not isinstance(expected_count, int) or expected_count < 0
    ):
        raise SuiteValidationError(f"{path} expected_count must be a non-negative integer")
    query = setup.get("query")
    if query is not None and not isinstance(query, dict):
        raise SuiteValidationError(f"{path} query must be an object")
    absent_ids = setup.get("absent_ids")
    if absent_ids is not None and (
        not isinstance(absent_ids, list)
        or not all(isinstance(item, str) and item.strip() for item in absent_ids)
    ):
        raise SuiteValidationError(
            f"{path} absent_ids must be a list of non-empty strings"
        )
    expected = setup.get("expected")
    if expected is None:
        return
    if not isinstance(expected, list) or not all(
        isinstance(item, dict) for item in expected
    ):
        raise SuiteValidationError(f"{path} expected must be a list of objects")
    for index, spec in enumerate(expected):
        spec_path = f"{path} expected[{index}]"
        if not spec:
            raise SuiteValidationError(f"{spec_path} must not be empty")
        unknown = sorted(set(spec) - ASSERT_MEMORY_EXPECTED_KEYS)
        if unknown:
            raise SuiteValidationError(
                f"{spec_path} contains unsupported keys: {', '.join(unknown)}"
            )
        for field in (
            "id", "memory_scope", "type", "content_contains", "title_contains",
        ):
            value = spec.get(field)
            if value is not None and (
                not isinstance(value, str) or not value.strip()
            ):
                raise SuiteValidationError(
                    f"{spec_path} {field} must be a non-empty string"
                )
        if "sensitive" in spec and not isinstance(spec["sensitive"], bool):
            raise SuiteValidationError(f"{spec_path} sensitive must be a boolean")
        revision = spec.get("revision")
        if revision is not None and (
            isinstance(revision, bool) or not isinstance(revision, int) or revision <= 0
        ):
            raise SuiteValidationError(
                f"{spec_path} revision must be a positive integer"
            )
        for field in ("tags_contains", "entities_contains"):
            wanted = spec.get(field)
            if wanted is not None and (
                not isinstance(wanted, list)
                or not all(isinstance(item, str) for item in wanted)
            ):
                raise SuiteValidationError(
                    f"{spec_path} {field} must be a list of strings"
                )
        for field in ("source_refs_contain", "evidence_refs_contain"):
            refs = spec.get(field)
            if refs is None:
                continue
            if not isinstance(refs, list) or not all(
                isinstance(item, dict) for item in refs
            ):
                raise SuiteValidationError(
                    f"{spec_path} {field} must be a list of objects"
                )
            for ref_index, ref in enumerate(refs):
                ref_path = f"{spec_path} {field}[{ref_index}]"
                unknown_ref_keys = sorted(set(ref) - ASSERT_MEMORY_REFERENCE_KEYS)
                if unknown_ref_keys:
                    raise SuiteValidationError(
                        f"{ref_path} contains unsupported keys: "
                        + ", ".join(unknown_ref_keys)
                    )
                for ref_field in ("type", "id"):
                    ref_value = ref.get(ref_field)
                    if ref_value is not None and (
                        not isinstance(ref_value, str) or not ref_value.strip()
                    ):
                        raise SuiteValidationError(
                            f"{ref_path} {ref_field} must be a non-empty string"
                        )
                event_ids = ref.get("event_ids_contains")
                if event_ids is not None and (
                    not isinstance(event_ids, list)
                    or not all(isinstance(item, str) and item.strip() for item in event_ids)
                ):
                    raise SuiteValidationError(
                        f"{ref_path} event_ids_contains must be a list of non-empty strings"
                    )


def _string_list_assertion(raw: Any, task_id: str, field: str) -> list[str]:
    if raw is None:
        return []
    if not isinstance(raw, list) or not all(isinstance(item, str) and item.strip() for item in raw):
        raise SuiteValidationError(f"task {task_id}: hard_assertions.{field} must be a list of non-empty strings")
    seen = set()
    out = []
    for item in raw:
        name = item.strip()
        if name in seen:
            continue
        seen.add(name)
        out.append(name)
    return out


RANGE_ASSERTION_KEYS = frozenset({"min", "max"})


def is_range_assertion(value: Any) -> bool:
    """True when ``value`` is a numeric-range spec.

    A range spec is a non-empty object whose keys are a subset of ``{"min", "max"}``,
    evaluated as an inclusive numeric band (``min <= actual <= max``). Any other
    value keeps the exact-equality semantics.
    """
    return (
        isinstance(value, dict)
        and bool(value)
        and set(value.keys()) <= RANGE_ASSERTION_KEYS
    )


def _validate_range_assertion(task_id: str, path: str, spec: dict[str, Any]) -> None:
    for key in RANGE_ASSERTION_KEYS:
        if key not in spec:
            continue
        value = spec[key]
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise SuiteValidationError(
                f"task {task_id}: environment_assertions[{path!r}].{key} must be a number"
            )
    lo = spec.get("min")
    hi = spec.get("max")
    if lo is not None and hi is not None and lo > hi:
        raise SuiteValidationError(
            f"task {task_id}: environment_assertions[{path!r}] min must be <= max"
        )


def _parse_environment_assertions(raw: Any, task_id: str) -> dict[str, Any]:
    if raw is None:
        return {}
    if not isinstance(raw, dict) or not raw:
        raise SuiteValidationError(
            f"task {task_id}: environment_assertions must be a non-empty object"
        )
    assertions: dict[str, Any] = {}
    for path, expected in raw.items():
        if not isinstance(path, str) or not path.strip():
            raise SuiteValidationError(
                f"task {task_id}: environment_assertions paths must be non-empty strings"
            )
        normalized = path.strip()
        if normalized in assertions:
            raise SuiteValidationError(
                f"task {task_id}: duplicate environment_assertions path {normalized!r}"
            )
        if any(not part for part in normalized.split(".")):
            raise SuiteValidationError(
                f"task {task_id}: invalid environment_assertions path {path!r}"
            )
        try:
            json.dumps(expected, ensure_ascii=False)
        except (TypeError, ValueError) as exc:
            raise SuiteValidationError(
                f"task {task_id}: environment_assertions[{normalized!r}] must be JSON-serializable"
            ) from exc
        if is_range_assertion(expected):
            _validate_range_assertion(task_id, normalized, expected)
        assertions[normalized] = expected
    return assertions


def _parse_consolidation_expectation(
    raw: Any, task_id: str
) -> ConsolidationExpectation | None:
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation must be an object"
        )
    allowed = {
        "goal_result", "min_memory_ids", "max_memory_ids", "allow_empty_memory",
        "required_assessment_evidence", "expected_status", "forbidden_memory_substrings",
        "required_memory_substrings", "required_memory_types", "required_memory_scope", "required_procedure_steps",
    }
    unknown = sorted(set(raw) - allowed)
    if unknown:
        raise SuiteValidationError(
            f"task {task_id}: unsupported consolidation_expectation keys: {', '.join(unknown)}"
        )
    goal_result = raw.get("goal_result")
    if goal_result is not None:
        if not isinstance(goal_result, str) or goal_result.strip().lower() not in {"achieved", "not_achieved", "unknown"}:
            raise SuiteValidationError(
                f"task {task_id}: consolidation_expectation.goal_result must be one of ['achieved', 'not_achieved', 'unknown']"
            )
        goal_result = goal_result.strip().lower()

    def non_negative_int(name: str, default: int | None) -> int | None:
        value = raw.get(name, default)
        if value is None:
            return None
        if isinstance(value, bool):
            raise SuiteValidationError(
                f"task {task_id}: consolidation_expectation.{name} must be an integer"
            )
        try:
            value = int(value)
        except (TypeError, ValueError) as exc:
            raise SuiteValidationError(
                f"task {task_id}: consolidation_expectation.{name} must be an integer"
            ) from exc
        if value < 0:
            raise SuiteValidationError(
                f"task {task_id}: consolidation_expectation.{name} must be non-negative"
            )
        return value

    min_memory_ids = non_negative_int("min_memory_ids", 0)
    max_memory_ids = non_negative_int("max_memory_ids", None)
    if max_memory_ids is not None and max_memory_ids < (min_memory_ids or 0):
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation.max_memory_ids must be >= min_memory_ids"
        )
    allow_empty_memory = raw.get("allow_empty_memory", False)
    required_evidence = raw.get("required_assessment_evidence", False)
    expected_status = raw.get("expected_status")
    if expected_status is not None:
        if not isinstance(expected_status, str) or expected_status.strip().lower() not in {"done", "ignored"}:
            raise SuiteValidationError(
                f"task {task_id}: consolidation_expectation.expected_status must be done or ignored"
            )
        expected_status = expected_status.strip().lower()
    if not isinstance(allow_empty_memory, bool) or not isinstance(required_evidence, bool):
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation boolean fields must be bool"
        )
    lists: dict[str, Any] = {
        "forbidden_memory_substrings": raw.get("forbidden_memory_substrings", []),
        "required_memory_substrings": raw.get("required_memory_substrings", []),
        "required_memory_types": raw.get("required_memory_types", []),
        "required_procedure_steps": raw.get("required_procedure_steps", []),
    }
    for field, values in lists.items():
        if not isinstance(values, list) or not all(isinstance(item, str) and item.strip() for item in values):
            raise SuiteValidationError(
                f"task {task_id}: consolidation_expectation.{field} must be a list of non-empty strings"
            )
    required_scope = raw.get("required_memory_scope", {})
    if not isinstance(required_scope, dict) or not all(
        isinstance(key, str) and key.strip() and isinstance(value, str) and value.strip()
        for key, value in required_scope.items()
    ):
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation.required_memory_scope must be an object of non-empty strings"
        )
    positive_contract = bool(lists["required_memory_substrings"] or lists["required_memory_types"] or lists["required_procedure_steps"] or required_scope)
    if positive_contract and allow_empty_memory:
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation.allow_empty_memory cannot be true when required memory content, type, or scope is declared"
        )
    if positive_contract and max_memory_ids == 0:
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation.max_memory_ids cannot be 0 when required memory content, type, or scope is declared"
        )
    if allow_empty_memory and (min_memory_ids or 0) > 0:
        raise SuiteValidationError(
            f"task {task_id}: consolidation_expectation.allow_empty_memory cannot be true when min_memory_ids is positive"
        )
    return ConsolidationExpectation(
        goal_result=goal_result,
        min_memory_ids=min_memory_ids or 0,
        max_memory_ids=max_memory_ids,
        allow_empty_memory=allow_empty_memory,
        required_assessment_evidence=required_evidence,
        expected_status=expected_status,
        forbidden_memory_substrings=list(lists["forbidden_memory_substrings"]),
        required_memory_substrings=list(lists["required_memory_substrings"]),
        required_memory_types=list(lists["required_memory_types"]),
        required_procedure_steps=list(lists["required_procedure_steps"]),
        required_memory_scope=dict(required_scope),
    )

def _platform_list(raw: Any, task_id: str) -> list[str]:
    if raw is None:
        return []
    if not isinstance(raw, list):
        raise SuiteValidationError(
            f"task {task_id}: platforms must be a list of ios, android, mac, windows, or linux"
        )
    out: list[str] = []
    for item in raw:
        try:
            platform = normalize_target_platform(item).value
        except ValueError as exc:
            raise SuiteValidationError(
                f"task {task_id}: invalid platform {item!r}"
            ) from exc

        if platform not in out:
            out.append(platform)
    return out


def _validate_matcher_bounds(value: Any, path: str) -> None:
    """Reject {"$gte"/"$lte"} bounds that are not numbers or that cross."""
    if isinstance(value, list):
        for index, item in enumerate(value):
            _validate_matcher_bounds(item, f"{path}[{index}]")
        return
    if not isinstance(value, dict):
        return
    if value and set(value) <= {"$gte", "$lte"}:
        for key, bound in value.items():
            if isinstance(bound, bool) or not isinstance(bound, (int, float)) or not math.isfinite(bound):
                raise SuiteValidationError(f"{path}.{key} must be a finite number")
        if "$gte" in value and "$lte" in value and value["$gte"] > value["$lte"]:
            raise SuiteValidationError(f"{path}: $gte must not exceed $lte")
        return
    for key, item in value.items():
        _validate_matcher_bounds(item, f"{path}.{key}")


def _required_tool_call_assertions(raw: Any, task_id: str) -> list[RequiredToolCallSpec]:
    if raw is None:
        return []
    if not isinstance(raw, list):
        raise SuiteValidationError(
            f"task {task_id}: hard_assertions.required_tool_calls must be an array"
        )
    out: list[RequiredToolCallSpec] = []
    for index, item in enumerate(raw):
        if not isinstance(item, dict):
            raise SuiteValidationError(
                f"task {task_id}: required_tool_calls[{index}] must be an object"
            )
        tool = str(item.get("tool") or "").strip()
        input_contains = item.get("input_contains") or {}
        if not tool:
            raise SuiteValidationError(
                f"task {task_id}: required_tool_calls[{index}] requires tool"
            )
        if not isinstance(input_contains, dict):
            raise SuiteValidationError(
                f"task {task_id}: required_tool_calls[{index}].input_contains must be an object"
            )
        _validate_matcher_bounds(input_contains, f"task {task_id}: required_tool_calls[{index}].input_contains")
        first_attempt = item.get("first_attempt", False)
        if not isinstance(first_attempt, bool):
            raise SuiteValidationError(
                f"task {task_id}: required_tool_calls[{index}].first_attempt must be boolean"
            )
        out.append(RequiredToolCallSpec(tool=tool, input_contains=input_contains, first_attempt=first_attempt))
    return out


def _parse_mock_environment(
    raw: Any,
    *,
    path: str = "mock_environment",
) -> MockEnvironmentSpec | None:
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise SuiteValidationError(f"{path} must be an object")
    single_frame = raw.get("single_frame", False)
    if not isinstance(single_frame, bool):
        raise SuiteValidationError(f"{path}.single_frame must be boolean")
    phone_bridge = raw.get("phone_bridge") or {}
    if not isinstance(phone_bridge, dict):
        raise SuiteValidationError(f"{path}.phone_bridge must be an object")
    top_level_platform = raw.get("platform")
    legacy_platform = phone_bridge.get("platform")
    if top_level_platform is None and legacy_platform is None:
        raise SuiteValidationError(
            f"{path} must declare a target platform (ios, android, mac, windows, or linux)"
        )
    try:
        platform = normalize_target_platform(
            top_level_platform if top_level_platform is not None else legacy_platform,
            field=f"{path}.platform",
        )
    except ValueError as exc:
        raise SuiteValidationError(
            f"{path}.platform must be ios, android, mac, windows, or linux"
        ) from exc
    if top_level_platform is not None and legacy_platform is not None:
        try:
            normalized_legacy_platform = normalize_target_platform(
                legacy_platform,
                field=f"{path}.phone_bridge.platform",
            )
        except ValueError as exc:
            raise SuiteValidationError(
                f"{path}.phone_bridge.platform must be ios, android, mac, windows, or linux"
            ) from exc
        if normalized_legacy_platform is not platform:
            raise SuiteValidationError(
                f"{path}.phone_bridge.platform conflicts with {path}.platform"
            )
    phone_bridge = dict(phone_bridge)
    phone_bridge.pop("platform", None)
    app_state = str(phone_bridge.get("app_state") or "").strip().lower()
    if app_state and app_state not in {"active", "background", "inactive"}:
        raise SuiteValidationError(
            f"{path}.phone_bridge.app_state must be active, background, or inactive"
        )
    for field in (
        "connected",
        "return_entry_available",
        "pip_bridge_enabled",
        "fgs_bridge_enabled",
    ):
        if field in phone_bridge and not isinstance(phone_bridge[field], bool):
            raise SuiteValidationError(
                f"{path}.phone_bridge.{field} must be boolean"
            )

    tools_raw = raw.get("tools") or {}
    if not isinstance(tools_raw, dict):
        raise SuiteValidationError(f"{path}.tools must be an object")
    tools: dict[str, list[MockToolResponseSpec]] = {}
    for raw_name, raw_responses in tools_raw.items():
        name = str(raw_name or "").strip()
        if not name:
            raise SuiteValidationError(f"{path}.tools contains an empty tool name")
        items = raw_responses if isinstance(raw_responses, list) else [raw_responses]
        if not items:
            raise SuiteValidationError(
                f"{path}.tools.{name} must contain at least one response"
            )
        tools[name] = [
            _parse_mock_tool_response(item, f"{path}.tools.{name}")
            for item in items
        ]

    default_raw = raw.get("default_tool_response")
    default_response = None
    if default_raw is not None:
        default_response = _parse_mock_tool_response(
            default_raw, f"{path}.default_tool_response"
        )
    screen = raw.get("screen")
    if screen is not None and (not isinstance(screen, str) or not screen.strip()):
        raise SuiteValidationError(f"{path}.screen must be a non-empty string")
    screen_text = raw.get("screen_text", "")
    if not isinstance(screen_text, str):
        raise SuiteValidationError(f"{path}.screen_text must be a string")
    collaboration_fixture = raw.get("collaboration_fixture")
    if collaboration_fixture is not None:
        if not isinstance(collaboration_fixture, dict) or collaboration_fixture.get("name") != "phone_data_v1":
            raise SuiteValidationError(f"{path}.collaboration_fixture requires name=phone_data_v1")
        if set(collaboration_fixture) - {"name", "state", "gates", "failures"}:
            raise SuiteValidationError(f"{path}.collaboration_fixture contains unsupported fields")
        state = collaboration_fixture.get("state", {})
        if not isinstance(state, dict) or set(state) - {"clipboard", "notifications"}:
            raise SuiteValidationError(f"{path}.collaboration_fixture.state must contain clipboard/notifications only")
        clipboard = state.get("clipboard", "")
        if not (isinstance(clipboard, str) or clipboard == {"generate": "token"}):
            raise SuiteValidationError(f"{path}.collaboration_fixture.state.clipboard must be text or generate=token")
        notifications = state.get("notifications", [])
        if not isinstance(notifications, list) or any(not isinstance(item, dict) for item in notifications):
            raise SuiteValidationError(f"{path}.collaboration_fixture.state.notifications must be a list of objects")
        gates = collaboration_fixture.get("gates", [])
        if not isinstance(gates, list):
            raise SuiteValidationError(f"{path}.collaboration_fixture.gates must be a list")
        from runner.phone_data_fixture import PhoneDataFixture
        gate_ids = set()
        for gate in gates:
            if not isinstance(gate, dict) or set(gate) - {"id", "tool", "input_contains", "at", "timeout_sec", "error"}:
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates contain an invalid gate")
            if not isinstance(gate.get("id"), str) or not gate["id"] or gate["id"] in gate_ids:
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates require unique IDs")
            if gate.get("tool") not in PhoneDataFixture.tools:
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates name unsupported tools")
            selector = gate.get("input_contains", {})
            if not isinstance(selector, dict):
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates.input_contains must be an object")
            at = gate.get("at", "before_result")
            if at not in {"before_result", "permission"}:
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates.at must be before_result or permission")
            if at == "permission" and (not isinstance(gate.get("error"), str) or not gate["error"].strip()):
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates.error is required for permission gates")
            if at == "before_result" and "error" in gate:
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates.error is only valid for permission gates")
            if "timeout_sec" in gate and (type(gate["timeout_sec"]) is not int or gate["timeout_sec"] <= 0):
                raise SuiteValidationError(f"{path}.collaboration_fixture.gates.timeout_sec must be positive")
            gate_ids.add(gate["id"])
        failures = collaboration_fixture.get("failures", {})
        if not isinstance(failures, dict) or set(failures) - PhoneDataFixture.tools or any(
            not isinstance(message, str) or not message.strip() for message in failures.values()
        ):
            raise SuiteValidationError(f"{path}.collaboration_fixture.failures must map fixture tool names to messages")
    return MockEnvironmentSpec(
        platform=platform,
        phone_bridge=phone_bridge,
        tools=tools,
        collaboration_fixture=collaboration_fixture,
        single_frame=single_frame,
        screen=screen.strip() if isinstance(screen, str) else None,
        screen_text=screen_text,
        default_tool_response=default_response,
    )


def _parse_mock_tool_response(raw: Any, path: str) -> MockToolResponseSpec:
    if not isinstance(raw, dict):
        raise SuiteValidationError(f"{path} response must be an object")
    input_contains = raw.get("input_contains") or {}
    if not isinstance(input_contains, dict):
        raise SuiteValidationError(f"{path}.input_contains must be an object")
    _validate_matcher_bounds(input_contains, f"{path}.input_contains")
    screen_contains = raw.get("screen_contains", "")
    if not isinstance(screen_contains, str):
        raise SuiteValidationError(f"{path}.screen_contains must be a string")
    is_error = raw.get("is_error", False)
    if not isinstance(is_error, bool):
        raise SuiteValidationError(f"{path}.is_error must be boolean")
    error = raw.get("error", "")
    if not isinstance(error, str):
        raise SuiteValidationError(f"{path}.error must be a string")
    screen_text = raw.get("screen_text", "")
    if not isinstance(screen_text, str):
        raise SuiteValidationError(f"{path}.screen_text must be a string")
    return MockToolResponseSpec(
        input_contains=input_contains,
        screen_contains=screen_contains,
        output=raw.get("output", {}),
        is_error=is_error,
        error=error,
        screen_text=screen_text,
    )

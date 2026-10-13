from __future__ import annotations
import dataclasses as dc
from typing import Any

@dc.dataclass
class ToolCall:
    step: int
    tool: str
    input: dict[str, Any]
    has_screenshot: bool = False

@dc.dataclass
class Trace:
    tool_calls: list[ToolCall]
    final_response: str
    total_tool_calls: int
    total_duration_ms: int

@dc.dataclass
class RubricVerdict:
    id: str
    verdict: str  # "yes" | "no"
    reason: str

@dc.dataclass
class HardAssertionResults:
    min_tool_calls: bool | None = None
    max_tool_calls: bool | None = None
    required_tools: bool | None = None
    forbidden_tools: bool | None = None
    prohibited_actions: bool | None = None
    required_tool_calls: bool | None = None
    timeout: bool = True
    response_exists: bool = False
    expected_answer: bool | None = None
    expected_recalled_memory: bool | None = None
    environment_state: bool | None = None
    memory_state: bool | None = None

@dc.dataclass
class HardAssertionFailure:
    id: str
    label: str
    requirement: str
    actual: str

@dc.dataclass
class TaskResult:
    suite: str
    run_id: str
    task_id: str
    category: str
    attempt: int
    status: str  # passed|failed|skipped|judge_error|timeout
    rubric: list[RubricVerdict]
    rubric_pass_count: int = 0
    rubric_total: int = 0
    hard_assertions: HardAssertionResults | None = None
    hard_assertion_failures: list[HardAssertionFailure] = dc.field(default_factory=list)
    metrics: dict[str, Any] = dc.field(default_factory=dict)
    artifact_dir: str = ""
    started_at: str = ""
    finished_at: str = ""
    description_for_judge: str = ""
    rubric_spec: list[dict] = dc.field(default_factory=list)

---
sidebar_position: 4
---

# Benchmark Metrics Reference

This reference defines benchmark result fields and exported scores, including
measurement scope, denominators, units, and missing-value semantics.

Read the field tables for definitions, then use
[Calculation rules](#3-calculation-rules) to follow a value from its source
through attempt-level calculation and run-level aggregation. Examples use
illustrative data, not measured benchmark results.

## 1. What the pipeline measures

```text
Benchmark runner
  → isolated task setup → Agent execution → assertions and optional LLM judge
  → results.jsonl → metrics.json, summary.md, report.html
  → Langfuse Dataset Experiment and benchmark scores
```

A **task** is one benchmark definition. An **attempt** is one planned execution
opportunity with its own number; setup failure or skipping can produce a result
without Agent execution. A **run** contains the selected tasks and their attempts.
Repeating a task is different from the Agent retrying a tool inside one attempt.

Use these artifacts together:

| Artifact | Question it answers |
| --- | --- |
| `manifest.json`, `suite.json` | Which code, suite, model, judge, platform, active skills, and repeat plan were evaluated? |
| `results.jsonl` | What happened in each task attempt, including eligibility, assertions, and scores? |
| `metrics.json` | What are the aggregate and per-task results? |
| `tasks/<task>/history.json`, `trace.json`, optional `episode.json` | What did the Agent observe, decide, and execute? Use the row's `artifact_dir` for repeated attempts. |
| `judge.json`, `hard_assertion_failures`, optional `environment_state.json` | What evidence made an attempt pass or fail? |
| `summary.md`, `report.html` | Where should a reviewer start investigating? |

Langfuse stores one Dataset per stable suite name, a Dataset Item per task/attempt,
and an Experiment Run per benchmark run. Agent episode IDs, when available, link
the experiment to the underlying Agent telemetry.

### Experiment output and execution evidence

| Field | Meaning and boundary |
| --- | --- |
| `final_response` | Final response from the attempt's saved `trace.json`. Null means evidence is unavailable; an empty string means the recorded response is empty. |
| `tool_calls` in Experiment Output | Ordered tool names and arguments from the saved trace, not tool results. This list differs from the numeric `benchmark.tool_calls` score. |
| `trace_artifact.status` | `available`: valid saved trace; `missing`: absent; `invalid`: malformed or unexpected structure; `unreadable`: file could not be read. |
| `trace_artifact.path` | Evidence path relative to the run directory; may be null when unavailable. |
| `aiden_episode_id` | Unambiguous Agent episode ID, or null if unavailable. |
| `aiden_trace_id` | Execution trace's 32-character hexadecimal OTLP ID. A known ID does not prove successful ingestion. |
| `aiden_trace_url` | Optional link to the execution trace; null when the episode ID or project lookup is unavailable. |
| `execution_trace_status` | `available`: a benchmark execution root was found and linked; `pending_or_missing`: the episode is known but its execution root was not found; `no_episode`: no unambiguous episode ID. Older items may omit this field. |

When `execution_trace_status=available`, the Experiment drawer uses the execution
trace. `agent-run` contains the goal and final answer, `agent-response` contains
model inputs and outputs, and tool observations contain arguments and results.
The `benchmark/<task>#attempt-<n>` node contains the evaluation output. Finding a
root does not guarantee all child observations were ingested. Unavailable
execution evidence uses a separate result replay; existing items retain their
original binding on publication retries. Replay parent and child observations
do not represent separate Agent attempts.

The Agent's `success` score describes its own outcome; `benchmark.success`
records the benchmark outcome. The evaluation node's duration measures
publication, so native Experiment Latency/Cost must not be substituted for the
benchmark execution metrics below. Publication success is not task success.

## 2. Metric reference

### Outcomes and eligibility

Attempt fields below are exported as `benchmark.<field>` scores unless noted.

| Field | Current meaning | How to use it |
| --- | --- | --- |
| `status` | `passed`, `failed`, `timeout`, `judge_error`, or `skipped` | Start the investigation here, then inspect eligibility and failure class. |
| `success` | `true`, `false`, or unknown | A known task outcome; unknown must not become zero. |
| `agent_eligible` | Whether this attempt participates in Agent capability metrics | Always inspect it alongside success rates. |
| `failure_class` | `agent`, `environment`, `evaluation`, `skipped`, or `unknown` | Separate capability failures from invalid measurements. |
| `quality_score` | Usually the fraction of judge rubric checks passed; hard-gate failures receive zero; unavailable evaluation receives unknown | Indicates partial completion, not necessarily task success. With `--no-judge`, a passing attempt receives 1 without semantic rubric evaluation. |
| `rubric_pass_rate` | Passed rubric checks divided by rubric count | Inspect individual verdicts: hard-gate failure may prevent the judge from running. |
| `expected_answer_match`, `expected_recalled_memory_match` | Deterministic checks when the suite configures them | Not automatically present in every memory task. |

The runner explicitly sets eligibility. Do not rebuild the denominator from
`status` alone: a `failed` setup assertion can still be ineligible, and a timeout
without evidence that execution started can have an unknown outcome. Inspect
the setup error instead of silently treating exclusion as product success.

### Status, failure class, stage, and eligibility are different dimensions

`status` records the runner's terminal result. `failure_class` is a coarse
classification, not a proven root cause or assignment of team responsibility.
`first_failure_stage` locates available failure evidence. `agent_eligible` controls
inclusion in capability denominators; `success` records the outcome value.

| Status | Meaning and boundary |
| --- | --- |
| `passed` | Enabled evaluations passed; disabled checks were not verified. |
| `failed` | A failure was recorded, including some setup failures; not necessarily an eligible Agent failure. |
| `timeout` | Task execution was recorded as timed out; setup timeouts can instead be skipped. |
| `judge_error` | Evaluation could not complete, including missing required memory or environment-state evidence; not limited to the LLM judge service. |
| `skipped` | Recorded as skipped or unable to complete normal execution; outer exception handlers can use it after partial execution. |

| Failure class | Current interpretation |
| --- | --- |
| `agent` | Eligible assertion/rubric/answer failure or ordinary timeout with execution evidence; audit the evaluator before concluding the model is wrong. |
| `environment` | Primarily setup assertion failures, initialization failures with consolidation evidence, or missing setup memory IDs; does not capture every environment incident. |
| `evaluation` | Judge failure or unavailable evidence needed to evaluate the attempt. |
| `skipped` | Coarse fallback for skips, preparation failures, and outer exceptions; inspect `metrics.error` and logs for the actual cause. |
| `unknown` | Insufficient classification evidence, for example chat request errors; can be eligible or ineligible. |
| null/absent | Usually no failure class on success, or missing legacy data; not the explicit `unknown` class. |

| Example | Status | Class | Raw success | Eligible |
| --- | --- | --- | --- | --- |
| Enabled checks passed | `passed` | null | true | true |
| Ordinary assertion/rubric failure | `failed` | `agent` | false | true |
| Ordinary timeout with execution evidence | `timeout` | `agent` | false | true |
| Timeout without execution evidence | `timeout` | `unknown` | null | false |
| Chat error with execution evidence | `failed` | `unknown` | false | true |
| Chat error without execution evidence | `failed` | `unknown` | null | false |
| Agent not ready, input screenshot missing, ordinary setup error | `skipped` | `skipped` | null | false |
| Setup assertion or specific consolidation initialization failure | `failed` | `environment` | false | false |
| Judge or required evidence unavailable | `judge_error` | `evaluation` | null | false |

Thus `status=skipped` and `failure_class=skipped` describe the same attempt on
two axes. Do not add `status.skipped` to `failures.class.skipped`. A skipped
classification alone does not identify the cause or prove no work took place.
Class counts include all classified result rows; stage counts include only
eligible failures. They cannot be directly compared as the same population.

Null means unavailable, unknown, or inapplicable depending on the field; numeric
zero means recorded zero under the instrumentation's scope. Boolean false is an
explicit negative. `recovery_attempted=false` means the runner did not enter its
timeout-recovery branch, and `recovery_succeeded=null` is not a failed recovery.

For legacy compatibility, the aggregator derives success from status whenever
the raw value is not Boolean, including null: passed becomes true, failed/timeout
become false. Explicit `agent_eligible=false` remains authoritative, excluding
such attempts from capability scores. Inspect raw results for event semantics;
this fallback supplies no new evidence. Missing eligibility is also inferred for
legacy rows, so old and new classifications are not necessarily comparable.

Hard-assertion Boolean values describe whether a check passed. In particular,
`hard_assertions.timeout=true` means the timeout check passed, not that a timeout
occurred. Required-tool checks mean required behavior appeared; forbidden-tool
and prohibited-action checks mean prohibited behavior did not appear. Null can
mean unconfigured, unevaluated, or missing evidence. Consult the specific error
and `hard_assertion_failures` (ID, requirement, actual value).

`rubric_spec` is the definition; `rubric` holds evaluated verdicts and reasons.
Nonzero `rubric_total` with zero `rubric_pass_count` can occur when the judge never
ran after a hard-gate failure. Check verdicts and `judge.json` before interpreting
that ratio as an actual semantic evaluation.

### Capability and consistency

Run scores use the `capability.` prefix. Let `k` be `manifest.metrics_k`, currently
the minimum planned repeat count across the selected tasks. Use `--repeats N`
to give every selected task the same repeat count. There is no separate
`--metrics-k` CLI option.

| Run score | Definition in this repository |
| --- | --- |
| `pass_at_1` | Fraction of tasks whose actual attempt 1 succeeds, among tasks with eligible attempt 1. |
| `pass_at_k` | Fraction of tasks with at least one success in attempts 1 through k; requires all k attempts to exist and be eligible. |
| `pass_pow_k` | Fraction of tasks whose first k attempts all succeed, using the same complete-task denominator. |
| `attempt_success_rate` | Successful eligible attempts / all eligible attempts, including attempts beyond k. |
| `oracle_best_score_at_k` | Mean of each complete task's best available quality score in the first k attempts. It describes best observed performance, not typical experience. |
| `first_success_attempt.mean`, `.p50`, `.count` | First successful attempt within each task's continuous eligible prefix starting at attempt 1. Can include attempts beyond k. Tasks with no success are absent. |

These are observed fixed-k outcomes, not an estimate from arbitrary combinations
of attempts. At `k=1`, `pass_at_1`, `pass_at_k`, and `pass_pow_k` coincide on the
same eligible task set; a single attempt cannot demonstrate repeatability.

The three pass metrics include `successes`, `eligible_tasks`, `total_tasks`, and
Wilson 95% confidence bounds. Attempt success also has confidence bounds, but
repeats of the same task are correlated: its simple interval does not account for
that dependence. For release decisions, compare paired tasks and use repeated
runs or uncertainty analysis grouped by task. Do not infer significance merely
from a small percentage increase or from overlap of separate confidence bounds.

### Coverage and diagnostics

| Score or artifact field | Meaning and limitation |
| --- | --- |
| `coverage.pass_at_1`, `.pass_at_k`, `.pass_pow_k` | Eligible tasks / observed unique tasks for that metric. Compare the manifest's planned task/attempt list as well: entirely missing results can escape this denominator. |
| `coverage.unique_tasks`, `.attempts`, `.metrics_k` | Scope of the observed workload. Legacy aggregate `tasks` counts attempts, not unique tasks. |
| `coverage.agent_eligible_attempts`, `.invalid_attempts` | How many observations can or cannot measure Agent capability. |
| `capability.attempt_success_rate.coverage` | Eligible attempts / observed attempts. |
| `failures.class.<class>` | Counts by failure class. These are counts, not normalized rates. |
| `failures.stage.<stage>` | Counts for eligible failed attempts, including `unknown`. |
| `diagnostics.failure_stage_coverage` | Fraction of eligible failures assigned a known stage. Unknown when there are no eligible failures. |
| `benchmark.first_failure_stage` | Evidence-based label where available; not a complete root-cause diagnosis. |
| `failure_event_ref` in an attempt's metrics | Pointer into history or episode evidence; not a separate Langfuse score. |

Stage vocabulary includes setup, perception, planning, action selection, device
execution, state verification, recovery, final response, evaluation, and unknown.
Having these labels available does not mean the runtime identifies every stage.
Many semantic failures currently remain `unknown`. Find the first meaningful
deviation in the trace; the first tool error can be a downstream symptom.

Category summaries remain in `metrics.json` under `aggregate.by_category` and
in the local summary report. The publisher no longer exports `category.*`
Langfuse scores. Historical experiments can still contain them: their pass
rate uses all category attempts, including invalid attempts, unlike eligible-only
capability scores. Task category remains available in Dataset item input and
metadata. Observation scores (`observations.<id>.*`) count evaluated attempt rows
despite their historical `passed_tasks`/`observed_tasks` names. Trace observations
describe behavior, such as tool usage, and do not themselves make a task fail.

### Time, work, and cost

Per-attempt numeric fields appear as `benchmark.<field>`. Most run distributions
appear as `efficiency.<field>.{count,sum,mean,p50,p90,p95}`. Tool errors, replans,
and retries instead use the `reliability.` prefix.

| Field | Measurement boundary and interpretation |
| --- | --- |
| `task_wall_ms` | Elapsed time around the task's Agent chat request. Excludes setup, judge time, post-run artifact collection, and timeout recovery. Legacy `wall_ms` mirrors it in current results. |
| `setup_ms` | Task isolation/setup work. Report separately from task execution. |
| `llm_time_ms` | Sum of recorded durations on usage-bearing assistant/tool-call messages; unavailable when those durations are incomplete. |
| `vision_llm_calls`, `vision_llm_time_ms` | Calls inferred from image/screenshot observations preceding model messages. This is an instrumentation heuristic, not an exact provider billing category. |
| `time_to_first_token_ms` | Episode's reported first-token timing, when available. |
| `device_execution_ms` | Sum of episode durations for recognized device-action results; unavailable if required durations are missing. Tool classification is an explicit allowlist. |
| `screenshot_capture_ms` | Runner-side timed screenshot captures; not all screenshots taken internally by the Agent. Inspect `screenshot_capture_source`. |
| `tool_calls`, `device_actions` | All traced tool calls versus recognized device-changing calls. Read-only bridge queries are excluded from device actions. |
| `llm_calls` | Count derived from messages carrying usage; unavailable when no usage is present. Validate telemetry completeness before treating it as every provider call. |
| `input_tokens`, `output_tokens`, `total_tokens` | Reported Agent usage; episode totals can override history-derived totals. Does not measure the entire run cost or judge/setup consumption. |
| `cached_input_tokens`, `reasoning_tokens` | Optional usage details; unknown when not reported. Do not add them to totals as independent extra usage. |
| `tool_errors` | Recorded tool-result errors, including errors the Agent later recovered from. |
| `replan_count` | Explicit episode `needs_replan` evidence; not inferred from tool names. |
| `retry_count`, `cost_usd` | Schema/export support exists, but the current task runner does not populate a retry counter or calculate monetary cost. Expect unknown unless an explicit source supplies them. |
| `recovery_attempted`, `recovery_succeeded` | Runner recovery after an Agent timeout; not success of the user's task or a count of Agent strategy retries. |

Efficiency distributions use eligible attempts with numeric values; `setup_ms`
uses all observed attempts with values. Always read `count`: a lower mean based
on fewer measurements is not necessarily an improvement. Missing scores are
omitted from Langfuse, not published as zero. Percentiles use interpolation and
are weak evidence with only a few samples. Timing fields overlap or have different
boundaries; do not add them together as a complete wall-time decomposition.

`cost_to_first_success.{task_wall_ms,total_tokens,cost_usd}.{count,mean,p50}` sums
each field from attempt 1 through the first success, stopping at missing or
ineligible attempts. A field must be complete through success to contribute.
Tasks that never succeed contribute no value. Pair these metrics with success,
coverage, and total resource consumption; they cannot alone show the cost of
serving every user request.

### Numeric suffixes and remaining interpretation details

| Field or suffix | Meaning |
| --- | --- |
| `value` | Primary JSON metric value; proportions are 0–1. Langfuse usually omits `.value` from the primary score name. |
| `count` | Number of numeric samples, typically attempts for efficiency and tasks for first-success aggregates. |
| `sum`, `mean` | Sum and arithmetic mean of available values; partial coverage does not produce a complete bill. |
| `p50`, `p90`, `p95` | Interpolated percentiles in the metric's unit, not confidence levels. |
| `ci95.lower/upper` | JSON Wilson interval bounds; Langfuse suffixes are `ci95_lower/ci95_upper`. |
| `successes` | Successful tasks for pass metrics, successful attempts for attempt success. |
| `eligible_tasks/total_tasks` | Eligible versus observed unique tasks for the metric. |
| `eligible_attempts/total_attempts` | Eligible versus observed attempts. |
| `_ms`, `_tokens`, `_usd` | Milliseconds, token counts, and dollars respectively; divide milliseconds by 1000 to display seconds. |
| `screenshots_taken` | Tool calls marked `has_screenshot`, not every Agent/runner screenshot. |
| `pre_screenshot_file/post_screenshot_file` | Whether local screenshot files exist, not whether their content is correct. |
| `screenshot_capture_source/cost_source` | Source metadata in results, not independent numeric Langfuse scores. |
| `error/agent_error/judge_error/environment_state_error` | Details needed to interpret coarse failure labels. |
| `episode_error/pre_screenshot_error/post_screenshot_error` | Artifact acquisition errors; may reduce evidence/metrics without necessarily failing every task. |
| `episode_id/active_skills` | Telemetry correlation and activated skill names; names alone do not establish content identity. |
| `started_at/finished_at` | Result timestamps, whose difference is not interchangeable with task-only wall time. |
| `environment_state_assertions/trace_observations` | Individual configured state checks / behavioral observations. |

`aggregate.per_task` reports total attempts including invalid attempts, but uses
eligible attempts for passed/failed/pass rate and known numeric values for best
and average quality and average wall time. Its current `first_attempt_passed`
means the first **eligible** attempt after sorting, unlike `pass_at_1`, which
requires actual attempt 1. Do not substitute one for the other.

`oracle_best_score_at_k.count` can be smaller than its `eligible_tasks` when
quality scores are absent. Read `manifest.metrics_k` for each run; separate runs
are not automatically combined into k attempts.

## 3. Calculation rules

### 3.1 Sources and precedence

Attempt metrics are assembled before run-level aggregation:

1. The runner records setup, chat-request, and screenshot timings.
2. Saved history supplies model usage, tool calls, and initial error evidence.
3. The request's episode supplies device-result durations, explicit replans,
   first-token timing, and available token totals. Fields emitted by this step
   overwrite the same history-derived fields; the two sources are not added.
4. Assertions and the judge determine outcome, eligibility, quality, and final
   failure labels. A passing result clears failure stage and event reference,
   even if recovered tool errors remain in the counters.
5. The aggregator reads attempt rows and calculates task-level capability and
   attempt-level resource distributions. The Langfuse publisher exports these
   values; it does not reconstruct them from trace spans.

| Source of truth | Responsibilities |
| --- | --- |
| [Task runner](../../benchmark/runner/runtask.py) | Measurement boundaries, evaluation order, outcome fields, source precedence |
| [Deterministic assertions](../../benchmark/runner/assertions.py) | Expected-answer parsing and memory-recall evidence checks |
| [Metric derivation and aggregation](../../benchmark/runner/metrics.py) | History/episode calculations, device classification, fixed-k metrics, distributions |
| [Trace extraction](../../benchmark/runner/trace.py) | Tool-call list and final response from history |
| [Tool execution](../../src/agent/internal/agent/tool_execution.go) and [episode recording](../../src/agent/internal/agent/task_episode.go) | Agent-side device-call timers and recorded event durations |
| [Runtime callbacks](../../src/agent/internal/agent/runtime.go) | First-token timing and Agent usage totals |
| [Run planning](../../benchmark/runner/main.py) | Planned repeats and manifest `metrics_k` |
| [Langfuse publisher](../../benchmark/runner/langfuse_reporter.py) | Score names, attempt rubric ratios, and omission of unavailable scores |

### 3.2 Task, setup, and screenshot timers

The runner uses a monotonic clock and truncates elapsed milliseconds to integers:

```text
elapsed_ms = int((end_monotonic - start_monotonic) * 1000)
```

| Field | Start and end | Missing or partial measurement |
| --- | --- | --- |
| `task_wall_ms` / `wall_ms` | Immediately before prompt preparation and `client.chat`, through return or exception. Includes request/response overhead. | Unavailable if execution never reaches this timer. Timeout duration is recorded before recovery. Offline evaluation uses a supplied task timing; its legacy fallback can use a supplied start clock, so check provenance. |
| `setup_ms` | Around `prepare_task_isolation`, including configured setup work. | Setup failures record elapsed time from the attempt's initial clock. Later readiness checks and runner screenshots are outside the normal setup timer. |
| `screenshot_capture_ms` | Sum of successful runner pre/post `take_environment_screenshot` calls, including retrieval and local saving. | A failed capture contributes no duration. A successful pre-capture plus failed post-capture leaves a partial sum; inspect screenshot errors. No successful timed capture leaves null. Copying an input image adds no capture duration. |
| `time_to_first_token_ms` | Copied from `episode.extra.first_token_time_ms`; the runtime measures from its run start to its first streaming callback. | Exported by the runtime only when positive. Without that episode value, unavailable. This is not per-model-request TTFT or time until the final answer becomes visible. |

Runner screenshots generally happen outside `task_wall_ms`. Screenshots or waits
inside an Agent tool can instead be part of that tool's duration. These timers
therefore do not form mutually exclusive pieces of a task-duration total.

### 3.3 Model calls, model time, and visual inference

History derivation visits messages in order. A message counts as a model call
only when its type is `assistant` or `tool_call` and its `usage` is a mapping.
There is no additional deduplication by provider request ID.

```text
llm_calls   = number of qualifying usage-bearing messages
llm_time_ms = sum(duration_ms on those messages)
```

The call count is null if no qualifying usage is present. The duration sum is
null if any qualifying message lacks a numeric, nonnegative duration; it is not
the sum of only the measured subset. Calls absent from history or without usage
cannot be recovered by this calculation. Recorded model durations measure call
latency, not a separately measured GPU compute time.

Visual inference uses a pending-image flag:

1. Set the flag on an image attachment or a screenshot-like tool result: tool
   name `screenshot`, text containing `screenshot observation`, or JSON with a
   string `data` field and a width or height. Screenshot-result detection
   requires string content; image attachments are checked independently.
2. Count the next qualifying usage-bearing message as a visual model call and
   include its duration in the visual sum.
3. Clear the flag after that qualifying message.

`vision_llm_calls` is zero when usage exists but no calls match this heuristic;
it is null when there is no qualifying usage. `vision_llm_time_ms` requires at
least one matching call and complete valid durations for those calls; otherwise
it is null. Multiple screenshots before one model call count as one visual call.
An image retained in later model context is not automatically counted again.

For example, model calls of 900, 1,400, and 700 ms give `llm_calls=3` and
`llm_time_ms=3000`. If only the second follows a screenshot, then
`vision_llm_calls=1` and `vision_llm_time_ms=1400`. The visual duration is already
inside the model duration; adding them would double-count it.

### 3.4 Device actions and device execution time

The current device-action allowlist is:

```text
touch_gesture, enter_text, keyboard_text, keyboard_tap, mouse_move,
mouse_scroll, quick_action, open_app, launch_app, open_url,
search_launch_app, bridge_open_app, bridge_clipboard, bridge_contacts,
bridge_calendar, bridge_notification, bridge_media,
swipe, tap, long_press, drag, press_key
```

Names must match the allowlist. `quick_action` with Boolean `list=true` is
excluded. Any listed `bridge_*` tool with a case-insensitive `action` of `query`,
`read`, `get`, `inspect`, `list`, or `search` is excluded. An absent or unparseable
input does not establish either exclusion. New tools require an explicit
classification update; the metric does not infer device effects from output.

```text
device_actions = count of qualifying tool_call messages in history
device_execution_ms = sum(max(0, duration_ms))
                      over qualifying tool_result events in the episode
```

For episode classification, the result's `tool_input` takes precedence. If it is
missing/null, the last preceding input for the same tool name is used. This
fallback is based on tool name, not a unique call ID.

The runtime starts the timer when handling a tool call, before input
normalization and validation, and measures elapsed time through the tool's
return and error handling. It records the duration before after-call hooks and
result emission. Positive durations are converted to integer milliseconds in
the episode, so a sub-millisecond call can be recorded as zero.

The duration can include communication, internal waits, nested work, and device
execution. It is not pure hardware time, the gesture's requested `duration_ms`,
or a measurement that independently proves the page finished loading. Failed
and rejected device calls also contribute when they have recorded durations.
Standalone screenshot, search, memory, and model calls are outside this
allowlist; similar work performed inside a listed tool remains inside its timer.

The sum is available only when the episode has events and every qualifying
result has a numeric duration. Negative recorded values are clamped to zero.
With events but no qualifying results, the value is zero. Without episode
events, or with a missing duration on a qualifying result, it is unavailable.
This completeness check covers recorded results only: a call whose result event
is entirely missing can still be omitted from the sum. Consequently,
`device_actions` and timed-result counts can differ.

For example, a 200 ms tap, 800 ms text entry, and 400 ms failed swipe contribute
1,400 ms. A separate 300 ms screenshot and an 11 ms `bridge_contacts` query
contribute nothing to this device sum.

### 3.5 Tokens, work counters, and failure evidence

Token calculations use the same qualifying messages as `llm_calls`. Each field
is summed independently; a missing field on any qualifying message makes that
history-derived total null.

| Metric | History source and calculation | Episode override when numeric |
| --- | --- | --- |
| `input_tokens` | Sum `usage.input_tokens`, falling back to `prompt_tokens` per message | `extra.prompt_tokens` |
| `output_tokens` | Sum `usage.output_tokens`, falling back to `completion_tokens` | `extra.completion_tokens` |
| `total_tokens` | Sum `usage.total_tokens`; if absent, use input + output only when both are known | `extra.total_tokens` |
| `cached_input_tokens` | Sum `usage.cached_input_tokens`, falling back to `cached_tokens` | `extra.cached_prompt_tokens` |
| `reasoning_tokens` | Sum `usage.reasoning_tokens` | `extra.reasoning_tokens` |

An episode value replaces its entire field's history sum, including a numeric
zero; it is not added to it. Explicit totals are preserved rather than forced
to equal input plus output. Optional cached/reasoning fields are not assumed to
be zero when absent. These values cover the recorded task Agent, not judge or
separate setup model consumption. `cost_usd` has no automatic token-price
calculation in the current runner.

| Field | Derivation and edge cases |
| --- | --- |
| `tool_calls` | Number of tool calls extracted from history, including calls without a following result. Counts an exposed composite tool once, not each internal operation. |
| `screenshots_taken` | Count extracted calls whose result has truthy JSON `data` or contains `screenshot observation`. It is a result-shape heuristic, not a count of every screenshot. |
| `tool_errors` | History counts result-level `is_error`, JSON content `is_error`, or JSON `ok=false`. A nonempty episode event list replaces this count with the number of `tool_result` events marked `is_error`; it does not parse their content again. Empty history yields zero, which alone does not prove complete telemetry. |
| `replan_count` | Count truthy `needs_replan` values across episode events. At least one event must contain the key; otherwise null. Explicit all-false evidence yields zero. |
| `retry_count` | Remains null without an explicit source. Repeated tool names or benchmark attempts are not counted as Agent retries. |
| `first_failure_stage`, `failure_event_ref` | History initially points to its first recognized tool error; an episode error can replace this reference. An allowlisted device error maps to `device_execution`, other tool errors to `unknown`. Outcome handling can then overwrite the stage with setup/evaluation/unknown or clear both fields on success. |
| `recovery_attempted`, `recovery_succeeded` | Set when the runner catches an Agent timeout and invokes recovery. Recovery's Boolean return is recorded separately from the failed task outcome. This pair does not count tool retries or all initialization recovery work. |

### 3.6 Quality score and rubric pass rate

The evaluation order explains why these two values can differ:

1. Execution errors and hard assertions are checked first. An eligible failure
   receives `quality_score=0`; an execution error/timeout without execution
   evidence receives null and is ineligible.
2. Configured expected-answer and memory-recall checks run next. A mismatch
   receives zero; unavailable required recall evidence produces an evaluation
   error with unknown quality.
3. If the judge is disabled, passing the preceding gates yields quality 1.
   Otherwise, quality is `rubric_pass_count / rubric_total`, where a pass is a
   `yes` verdict. Full task success requires all rubric checks to pass. Judge
   errors leave quality unknown. With zero rubric items and zero passed checks,
   the result passes and quality is 1.
4. Configured environment-state checks are applied afterward. Missing state
   makes quality unknown and the attempt ineligible; a failed state check turns
   an otherwise passing result into an eligible failure with quality zero.

`expected_answer_match` currently supports the `option_letter` format: the
assertion parser normalizes the expected option, extracts the predicted option
from the final response, and compares them. An unparseable expected or predicted
answer fails; this is not a semantic similarity score.

`expected_recalled_memory_match` requires a call to the configured recall tool
and checks whether all expected memory IDs occur in its evidence; extra IDs do
not fail the check. Complete inline tool results take precedence. If they are
incomplete, episode `retrieved_memory_refs` may supply fallback evidence unless
the task requires inline evidence. The fallback checks attribution against
other recall tools: missing expected IDs fail, but ambiguous attribution or
unavailable required evidence yields null. No call to the configured tool
yields false. Read `memory_recall_evidence_source` alongside the Boolean.

Langfuse's `benchmark.rubric_pass_rate` independently divides the stored pass
count by the stored rubric total whenever the total is nonzero. It is omitted
when that total is zero. For example, three `yes` verdicts out of four yield
quality 0.75 and rubric pass rate 0.75, but the task fails. With `--no-judge`,
the same four configured checks may remain unevaluated: quality can be 1 and
the exported rubric ratio 0/4. Inspect verdicts before comparing these fields.

### 3.7 Fixed-k capability and first-success calculations

Group result rows by `task_id` and actual attempt number. Let `T` be the number
of observed unique tasks, `E1` the tasks with eligible attempt 1, and `Ek` the
tasks with every attempt from 1 through k present and eligible.

```text
pass_at_1 = tasks in E1 whose attempt 1 succeeds / size(E1)
pass_at_k = tasks in Ek with any success in attempts 1..k / size(Ek)
pass_pow_k = tasks in Ek with all successes in attempts 1..k / size(Ek)
coverage.pass_at_1 = size(E1) / T
coverage.pass_at_k = coverage.pass_pow_k = size(Ek) / T
attempt_success_rate = successful eligible rows / eligible rows
```

Pass values are null with no eligible denominator; coverage is zero when no
tasks are observed. `attempt_success_rate` includes eligible attempts beyond k.
Rows wholly absent from results do not enter observed coverage; check planned
attempts in the manifest separately. Duplicate task/attempt rows overwrite one
another in task grouping, while attempt distributions still use all rows; input
results must contain one row per planned task/attempt.

For k=2, consider:

| Task | Attempt 1 | Attempt 2 | In E1? | In Ek? |
| --- | --- | --- | --- | --- |
| A | Pass | Fail | Yes | Yes |
| B | Fail | Pass | Yes | Yes |
| C | Pass | Pass | Yes | Yes |
| D | Pass | Ineligible | Yes | No |

Here `pass_at_1=3/4`, `pass_at_k=3/3`, `pass_pow_k=1/3`, fixed-k coverage is
`3/4`, and attempt success is `5/7`. The higher pass-at-k describes observed
success with more opportunities, not improved first-attempt reliability.

For `oracle_best_score_at_k`, take the maximum available quality in each task
in Ek, then average those maxima. A task with no numeric quality is omitted
from `count`; a task with only some known qualities still contributes its best
known value.

For first-success metrics, start at actual attempt 1 and stop at the first
missing or ineligible attempt. Within that continuous prefix, find the first
success; the search can extend beyond k. Its attempt number contributes to
`first_success_attempt`. Sum each resource field separately up to and including
that success for `cost_to_first_success`; every value through success must be
numeric for that field to contribute. A task that never succeeds is omitted.

For example, eligible Fail/Pass attempts taking 2,000 and 3,000 ms contribute
first-success attempt 2 and cumulative task time 5,000 ms. If the second
attempt's tokens are missing, this task still contributes timing but contributes
no cumulative-token value. Fail/Ineligible/Pass contributes to neither
first-success metric because the eligible prefix ends before success.

### 3.8 Distributions, confidence bounds, and diagnostic ratios

For each efficiency/reliability field, collect numeric, non-Boolean values from
eligible attempt rows, including eligible failures. `setup_ms` instead uses all
observed rows. Every field therefore has its own sample count.

```text
count = number of included values
sum = sum of included values
mean = sum / count
```

With no values, count is zero and sum/mean/percentiles are null. For sorted
values `x[0]` through `x[n-1]`, a percentile P uses linear interpolation:

```text
h = (n - 1) * P / 100
f = floor(h)
c = min(f + 1, n - 1)
percentile(P) = x[f] + (x[c] - x[f]) * (h - f)
```

For 100, 200, and 900 ms, p50 is 200 ms and p90 is 760 ms. The latter need not
be an observed duration. A single sample makes all percentiles equal to it;
that does not establish stable tail latency.

Pass metrics and attempt success use Wilson 95% intervals. With `s` successes,
`n` eligible trials, `p=s/n`, and `z=1.959963984540054`:

```text
d = 1 + z*z/n
center = (p + z*z/(2*n)) / d
margin = z * sqrt(p*(1-p)/n + z*z/(4*n*n)) / d
lower = max(0, center - margin)
upper = min(1, center + margin)
```

Bounds are null when n=0. Trials are tasks for pass metrics and attempts for
attempt success; the latter interval does not model within-task correlation.

Remaining diagnostic ratios use distinct populations:

| Field | Calculation |
| --- | --- |
| `coverage.invalid_attempts` | Observed attempt rows minus eligible attempt rows |
| `diagnostics.failure_stage_coverage` | Eligible failed attempts with a recognized non-unknown stage / eligible failed attempts; null with no eligible failures |
| `aggregate.by_category.<category>.passed/total` | Category pass ratio in local artifacts: rows with `status=passed` / all rows in that category, including ineligible rows; no longer exported as `category.<category>.pass_rate` |
| `aggregate.by_category.<category>.rubric_pass/rubric_total` | Category rubric ratio in local reports: sum of stored rubric pass counts / sum of rubric totals when positive; rubric-weighted, not a mean of task ratios; no longer exported as `category.<category>.rubric_pass_rate` |
| `observations.<id>.pass_rate` | Rows with at least one passed observation for that ID / rows containing that ID; repeated checks of the same ID within a row count once |

Failure class counts use all classified rows; stage counts use eligible failures
only. Neither a class count nor a stage count is a percentage without an
explicit denominator.

---
sidebar_position: 5
---

# Realtime Collaboration Benchmark

The `realtime-text` execution mode evaluates how the realtime foreground model
works with the backend Agent. Text drives a real realtime provider, which
delegates work to the backend through the production task manager. The
benchmark scores delegation, task control, result delivery, and whether every
foreground statement that work is done was true when it was made.

Speech recognition, speech synthesis, VAD, audio playback, and voice
interruption are outside its scope.

## Execution modes

The runner's `--execution-mode` option, or a CI catalog case's
`execution_mode`, selects the Agent path under test:

| Mode | Path | Use |
| --- | --- | --- |
| `backend-text` (default) | The runner sends each task prompt to the backend Agent through `/api/chat`. No realtime foreground is involved. | All ordinary suites. Baseline for backend execution quality. |
| `realtime-text` | The runner sends text to the realtime foreground, which creates, updates, queries, and cancels backend tasks. | Collaboration scenario suites. |

Scenario suites require `realtime-text`, and `realtime-text` accepts only
scenario suites. Results and Langfuse experiment metadata record the mode, and
run-to-run comparisons in the dashboard and weekly report treat a mode change
as a configuration change. The weekly report's overall pass rate covers every
non-diagnostic suite, including the realtime collaboration suite; use its
per-suite row to follow this benchmark.

## Runtime

```text
┌─────────────┐   text turns    ┌──────────────────────────────────────┐
│   Runner    │ ──/api/chat───▶ │ Agent daemon (one per attempt)       │
│             │                 │  realtime foreground ──▶ task manager│
│             │ ◀──SSE events── │  backend Agent ◀───────────┘         │
└──────┬──────┘                 └──────────────┬───────────────────────┘
       │ gate release                          │ bridge_clipboard,
       ▼                                       ▼ bridge_notification
┌──────────────────────────────────────────────────────────┐
│ Mock environment with the phone_data_v1 fixture          │
└──────────────────────────────────────────────────────────┘
```

Each scenario attempt runs in one isolated daemon started by
`run --auto-agent-setup`. The foreground session, task manager, and backend
Agent share that daemon, which stays alive until every task has finished and
its result has been reported. Repetitions start from fresh state.

The runner starts the daemon with `-benchmark-realtime-text`. In this mode the
daemon:

- uses the configured realtime provider, production task tools, task manager,
  and backend main model;
- requests text-only output from OpenAI, Qwen, and xAI providers. Gemini Live
  native-audio models only produce audio, so they are scored from the output
  transcript;
- replaces audio devices with a silent sink and skips MCU wakeup, spoken
  failure announcements, and voice notifications;
- records foreground, task-manager, and backend events in one ordered stream.

Text-only output can change model behavior relative to spoken sessions.
Compare foreground models within this mode, not against audio-mode results.

### Collaboration events

The daemon exposes the event stream only in this mode and only to requests
carrying `X-Aico-Benchmark-Token`:

| Request | Purpose |
| --- | --- |
| `GET /api/benchmark/collaboration` | Recorder status, effective foreground and backend models, and evidence completeness. `?barrier=1` waits until previously observed events are published. |
| `GET /api/benchmark/collaboration/events?since=latest` | SSE stream from the current position. |
| Same endpoint with `Last-Event-ID: <epoch>:<sequence>` | Replay after a saved cursor, then continue live. |

Events use the version 2 semantic event envelope with an added
`collaboration` object: `actor` (`foreground`, `task_manager`, or `backend`),
a per-actor `source_sequence`, a daemon monotonic `observed_elapsed_ns`, goal
and applied revisions, cause links, and result `update_ids`. Recording never
blocks the Agent. If its queue overflows or an event is rejected, the recorder
reports itself incomplete instead of dropping events silently.

The runner writes each event to `collaboration-events.jsonl` before advancing
its cursor and reconnects from the last saved cursor. A sequence gap, an epoch
change, or a conflicting duplicate marks the evidence incomplete.

## Running

Configure only the foreground. The backend keeps the main model from the
benchmark configuration.

```bash
cd benchmark
export AICO_BENCHMARK_REALTIME_API_KEY=...
uv run --locked python -m runner run \
  --suite suites/realtime_collaboration_v1.json \
  --execution-mode realtime-text \
  --auto-agent-setup \
  --realtime-provider openai \
  --realtime-model <realtime-model>
```

| Option | Environment default | Agent TOML field |
| --- | --- | --- |
| `--realtime-provider` | `AICO_BENCHMARK_REALTIME_PROVIDER` | provider `type` |
| `--realtime-model` | `AICO_BENCHMARK_REALTIME_MODEL` | `model` |
| `--realtime-endpoint` | `AICO_BENCHMARK_REALTIME_ENDPOINT` | `endpoint` |
| `--realtime-protocol` | `AICO_BENCHMARK_REALTIME_PROTOCOL` | `realtime_protocol` |
| `--realtime-upstream-provider` | `AICO_BENCHMARK_REALTIME_UPSTREAM_PROVIDER` | `upstream_provider` |
| `--realtime-auth-mode` | `AICO_BENCHMARK_REALTIME_AUTH_MODE` | `auth_mode` |
| `--realtime-api-key-env` | `AICO_BENCHMARK_REALTIME_API_KEY` | `api_key` reference |

The runner adds a `benchmark-foreground` provider to the generated
`agent.toml` with `input_mode = "realtime"` and `use_backend_agent = true`. The
key is first written as a reference to the environment variable. As for the
backend model key, auto-started workers then receive a read-only worker config
with the reference resolved to the key's value, so treat worker config
directories as secret-bearing.
Empty options are omitted, so provider defaults apply; providers that require
an explicit model still reject an incomplete configuration. For an
OpenAI-compatible gateway, set the endpoint (for example
`wss://gateway.example/v1/realtime`) and the protocol the gateway implements
(`ga` or `legacy`).

To use an existing configuration instead, pass `--agent-config` with a TOML
that already sets `input_mode = "realtime"` and `use_backend_agent = true`, and
leave the foreground options unset. Supplying both is rejected.

The judge is required for completion-claim and feedback checks. With
`--no-judge`, evidence is still recorded but those checks are unscored.

The WebUI rejects scenario suites; run them from the CLI or CI.

### CI

The catalog case `realtime-collaboration` has `runnable: false`, so the
scheduled run and an unfiltered `runnable` dispatch skip it. It runs when its
suite is selected in a manual dispatch, or with a manual `all` dispatch, which
selects every catalog case:

```bash
gh workflow run benchmark.yml --ref <branch> \
  -f profile=runnable -f suite=suites/realtime_collaboration_v1.json -f repeats=1
```

The workflow maps these repository settings to the variables above:

- Variables: `BENCHMARK_REALTIME_PROVIDER`, `BENCHMARK_REALTIME_MODEL`,
  `BENCHMARK_REALTIME_ENDPOINT`, `BENCHMARK_REALTIME_PROTOCOL`,
  `BENCHMARK_REALTIME_UPSTREAM_PROVIDER`, and `BENCHMARK_REALTIME_AUTH_MODE`.
- Secret: `BENCHMARK_REALTIME_API_KEY`.

Clear provider-specific variables when switching providers; they are explicit
overrides and are not inferred from the endpoint. For example,
`BENCHMARK_REALTIME_PROTOCOL` is accepted only with `openai`. Endpoint
variables must not contain credentials.

To investigate a provider protocol problem, set `level = "debug"` under
`[advanced_settings.log]` in the Agent configuration. For a manual CI dispatch,
edit `benchmark/config/agent.toml.template` on the dispatched branch, or
`benchmark/config/agent.toml` if present (it takes precedence). CI does not pass
`--agent-config`, so editing another Agent configuration will not affect workers.
Worker logs are uploaded as `daemon.log`. At debug level, the Gemini adapter logs
incoming raw frames, which may contain transcripts, tool arguments, and audio;
frames longer than 4,000 bytes are truncated to their first 4,000 bytes. The OpenAI
adapter and shared WebSocket transport do not log raw frames at debug level.
The default level is `info`; restore it after the investigation.

When the runner has an HTTPS proxy, realtime-text workers forward it to the
daemon for the foreground provider. The backend model endpoint is added to
`NO_PROXY` and keeps the direct route used in every other mode.

## Scenario format

A scenario task is an ordinary suite task with a `scenario` object. Its
`prompt` is sent once as turn `initial`, after the fixture is prepared and the
event stream is connected.

```json
{
  "id": "goal_update_during_read",
  "category": "multi_step",
  "description_for_judge": "Update the same running task; keep the body and silent setting, and post only the revised title.",
  "prompt": "Read my phone clipboard and post a silent local notification titled Draft using exactly its content as the body.",
  "mock_environment": {
    "platform": "android",
    "phone_bridge": {"connected": true, "phone_id": "realtime-test", "app_state": "active"},
    "tools": {},
    "collaboration_fixture": {
      "name": "phone_data_v1",
      "state": {"clipboard": {"generate": "token"}, "notifications": []},
      "gates": [{"id": "clipboard_read", "tool": "bridge_clipboard", "input_contains": {"action": "read"}, "at": "before_result"}]
    }
  },
  "scenario": {
    "version": 1,
    "steps": [
      {"id": "created", "type": "await", "match": {"kind": "task.updated", "phase": "created", "turn": "initial"}, "bind_task": "posting"},
      {"id": "held", "type": "await", "match": {"kind": "fixture.gate", "phase": "entered", "gate": "clipboard_read", "task": "posting"}},
      {"id": "initial_reply", "type": "await", "match": {"kind": "turn.settled", "turn": "initial", "status": "completed"}},
      {"id": "change", "type": "send", "text": "Change the title to Final. Keep the clipboard body and keep it silent."},
      {"id": "accepted", "type": "await", "match": {"kind": "task.updated", "phase": "goal_updated", "task": "posting", "turn": "change"}, "bind_revision": "changed"},
      {"id": "no_early_notification", "type": "assert", "check": {"kind": "no_effect", "operation": "bridge_notification"}},
      {"id": "release_read", "type": "release", "gate": "clipboard_read"},
      {"id": "applied", "type": "await", "match": {"kind": "task.updated", "phase": "goal_applied", "task": "posting", "applied_revision": "$revision.changed"}}
    ],
    "completion": {"tasks": [{"task": "posting", "status": "completed", "goal_revision": "$revision.changed"}]},
    "assertions": [
      {"id": "no_replacement_task", "kind": "tasks_created", "turn": "change", "count": 0},
      {"id": "one_notification", "kind": "effect_count", "operation": "bridge_notification", "count": 1},
      {"id": "title", "kind": "state_equals", "path": "/notifications/0/title", "value": "Final"},
      {"id": "body", "kind": "state_equals", "path": "/notifications/0/body", "value": {"fixture_value": "/clipboard"}}
    ]
  },
  "rubric": [{"id": "accurate_feedback", "check": "Report only the verified Final notification as complete."}]
}
```

The runner never creates, updates, or cancels a task itself. If the model
never delegates, the dependent steps time out and the attempt is scored from
what was captured.

### Steps

Steps run in order while the event stream is collected continuously.

| Type | Fields | Behavior |
| --- | --- | --- |
| `send` | `id`, `text` | Submit a user turn. The step ID becomes the turn alias. If the foreground is still replying, the runner waits for it to finish and resubmits, up to three times. |
| `await` | `id`, `match`, optional `bind_task`, `bind_revision`, `timeout_sec` | Wait for a recorded event matching every `match` field. Events received before the step also match. |
| `release` | `id`, `gate` | Open a fixture gate. It never applies an effect by itself. |
| `assert` | `id`, `check` | Evaluate a deterministic check at this point. |

`match` accepts `kind`, `actor`, `phase`, `status`, `turn`, `task`,
`goal_revision`, `applied_revision`, and `gate`. `turn` and `task` name aliases,
and a revision can be written as `$revision.<alias>`. Common matches:

| `kind` | `phase` values |
| --- | --- |
| `task.updated` | `created`, `queued`, `running`, `goal_updated`, `goal_applied`, `user_action_required`, `resumed`, `cancelling`, `terminal` |
| `fixture.gate` | `entered`, `denied`, `released` |
| `turn.settled` | none; use `status` (`completed` or `failed`) |
| `task.result_injected` | none |

- `bind_task` names the task created by a turn. The match must be
  `task.updated` with phase `created` and a `turn`. A turn bound this way must
  create exactly one new task by the end of the scenario. A new request that
  the foreground folds into existing work with `update_agent_task`, or that
  reuses another bound task, fails the attempt immediately: folding an
  unrelated request into running work interrupts that work.
- `bind_revision` captures the goal revision from a `goal_updated` event for
  later matches and completion targets.

### Completion

`completion.tasks` lists the outcome required for each delegated task:

- `task`: alias;
- `status`: `completed`, `failed`, or `cancelled`, or a list of acceptable
  statuses (for example, removing a pending operation with an update can end as
  `completed` where cancelling ends as `cancelled`);
- optional `goal_revision`: literal or `$revision.<alias>`.

Feedback is always required for delegated tasks. A scenario that expects no
delegation uses `"completion": {"turn": "initial", "delegation": "none"}`.

After the steps, the runner waits for every turn to settle and every task to
reach its terminal state and receive feedback. When any target allows
`cancelled`, it observes for five more seconds to catch late effects.

The default scenario deadline is 300 seconds (`timeout_sec`, at most 1800).
Each `await` defaults to 60 seconds, and final feedback to 30 seconds after a
task ends. All waits are bounded by the scenario deadline.

### Assertions

`scenario.assertions` run at the end; `assert` steps run the same checks at a
checkpoint. Final assertions require an `id`.

| `kind` | Fields | Passes when |
| --- | --- | --- |
| `task_state` | `task`, `status`, optional `started` | The task's latest status matches, and backend execution has (not) started. |
| `tasks_created` | `turn`, `count` | That turn created exactly `count` tasks. |
| `no_effect` | `operation`, optional `input_contains` | No matching fixture effect occurred. |
| `effect_count` | `operation`, `count`, optional `input_contains` | Exactly `count` matching effects occurred. |
| `fixture_call_count` | `operation`, `count`, optional `input_contains` | Exactly `count` matching calls reached the fixture. |
| `fixture_failure` | `operation`, optional `input_contains` | At least one matching call failed. |
| `state_equals` | `path`, `value` | The fixture state at the JSON Pointer `path` equals `value`. |

`value` can be a literal, `{"fixture_value": "/clipboard"}` for a seeded
value, or `{"$contains": "text"}` for text the model may punctuate
differently.

### Phone data fixture

`collaboration_fixture` with `name: "phone_data_v1"` handles
`bridge_clipboard` and `bridge_notification` without adding model-visible
tools. Only Agent tool calls change its state.

- `state.clipboard`: text, or `{"generate": "token"}` for a random value
  generated per attempt that the model can only learn by reading it.
- `state.notifications`: initial notifications.
- `gates`: each has `id`, `tool`, optional `input_contains`, and `at`.
  `before_result` holds matching calls until the gate is released or the
  task is cancelled (`timeout_sec` defaults to 120). `permission` fails
  matching calls with `error` until released.
- `failures`: maps a tool name to an error returned by every call.

The backend passes its task ID and tool call ID to the fixture in request
headers, outside the model-visible arguments, so every call and effect is
attributed to a task.

## Scoring

### Delivery milestones

For each completion target, the runner requires three milestones, linked by
ID rather than by text:

1. The backend task reaches its terminal state with the expected status and
   revision.
2. A `task.result_injected` event delivers that result to the foreground.
3. A completed, non-empty foreground response carries that result's update ID.

A cancelled task's result lists the tool calls that had already succeeded, so
feedback about a cancellation can report effects that already happened.

### Completion claims

Every foreground response is checked, not only the last one. The judge
extracts statements that a phone operation has already taken effect. The
runner then validates each quote against the recorded text, and drops
negated statements, progress or intent, and confirmations that a goal was
changed. Finally it adjudicates each claim using only events recorded before
the claim became visible.

A claim is `supported` only when the fixture recorded the claimed operation
for that task, and the daemon had already observed the corresponding tool
completion. Unsupported claims are classified as:

| Category | Meaning |
| --- | --- |
| `completion_without_delegation` | No task had been created. |
| `completion_before_execution` | The task had not started backend execution. |
| `completion_before_effect` | The operation was held at a gate or had not been issued. |
| `effect_not_performed` | The task never attempted the operation. |
| `obsolete_goal_completion` | The proof belongs to a superseded goal revision. |

Claims that cannot be attributed or timed are `indeterminate`. A later
correction or later success does not remove an earlier unsupported claim.

### Outcome

The judge also checks each target's feedback and the task rubric. The
attempt:

- fails when any check fails;
- is otherwise unscored when a check is unscored, for example because the
  provider failed, the evidence is incomplete, or the judge is unavailable;
- passes only when every check passes.

| Outcome | `status` | `failure_class` |
| --- | --- | --- |
| Pass | `passed` | none |
| Fail | `failed` | `agent` |
| Unscored | `judge_error` | `evaluation` |

Missing evidence or a provider failure never produces a pass. `runner rejudge`
re-evaluates saved evidence without running tools and writes
`collaboration-evaluation.rejudged.json`.

## Artifacts and reports

Each attempt directory contains:

| Artifact | Contents |
| --- | --- |
| `collaboration-events.jsonl` | Daemon events as received |
| `fixture-events.jsonl` | Fixture calls, gates, and effects |
| `scenario-events.jsonl` | Executed steps |
| `collaboration.json` | Bindings, milestones, checks, fixture state, and the captured events |
| `collaboration-evaluation.json` | Outcome, checks, claim verdicts, and rubric verdicts |
| `collaboration-claim-judge-raw.json` | Raw claim-judge response |
| `collaboration-timeline.json`, `collaboration-summary.json` | Readable timeline and summary used by `report.html` |
| `episode.json`, `episodes.json` | First backend episode, and all backend episodes in start order (`null` for one that could not be read) |

Result metrics include:

- `episode_id` (first) and `episode_ids` (all) for the backend runs;
- backend tokens, cost, LLM calls and time, tool calls, and device actions,
  summed over every backend episode, and unknown if any episode cannot be read;
- `foreground_responses`, `foreground_input_tokens`, and
  `foreground_output_tokens` for the realtime foreground;
- `collaboration_models` with the effective foreground and backend models.

When published to Langfuse, each backend run links to its `agent-run` trace as
in `backend-text` runs. Publishing also rebuilds the foreground session from
`collaboration-events.jsonl` as a separate `realtime-foreground` trace with
the original timestamps. It contains:

- user turns;
- foreground responses with token usage;
- task tool calls;
- backend tasks and runs, with links to their `agent-run` traces;
- result injections;
- adjudicated completion claims, with unsupported claims marked as warnings.

The experiment item records `foreground_trace_id`, `foreground_trace_url`, and
`foreground_trace_status` (`exported`, `export_failed`, `not_exported`,
`no_capture`, or `unreadable`). A failed export is recorded with
`foreground_trace_error` and does not block the benchmark scores. Experiment
metadata adds `execution_mode` and `foreground_model`. The dashboard and weekly
report treat both as comparison fields.

## Limitations

- Text injection replaces speech input, and most providers return text instead
  of audio. Results do not measure spoken interaction quality.
- Claim extraction relies on an LLM judge plus validation heuristics. The raw
  judge output is saved for review.
- The fixture covers clipboard and notification operations only.
- Assistance with several waiting tasks, and suppression of stale results after
  a cancellation or update, are not covered by the current suite.
- Scenario outcomes depend on both models. For example, the user-assistance
  scenario requires the backend model to request user help.

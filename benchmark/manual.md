# Benchmark User Manual

This document describes the design and usage of the Aiden benchmark under the
current `benchmark/` directory, covering the recommended entry points:

- WebUI: best for day-to-day runs, multiple suites, MobileGym concurrency, and
  viewing reports interactively.
- CLI: best for scripted runs, single-suite debugging, rejudge, compare, and unit
  tool tests.

The commands below assume you have entered `benchmark/` from the repo root:

```bash
cd benchmark
uv sync
```

## 1. Design overview

### 1.1 Core goal

The benchmark evaluates the Aiden agent on phone UI, memory, planning, perception,
and similar tasks. It separates "running the task" from "scoring":

1. The Python runner reads `benchmark/suites/*.json`.
2. The runner calls the Aiden Go agent daemon over HTTP.
3. The agent calls tools to operate a device or simulator based on the prompt.
4. The runner collects artifacts: history, trace, pre/post screenshots, logs.
5. Hard assertions run first as deterministic checks.
6. An optional judge model then scores offline against the rubric, trace, and
   screenshots.

The benefit of this design: task execution depends on a real agent and
environment, but scoring can be re-run offline; on failure you can inspect the
full trace and screenshots without re-operating the device.

### 1.2 Main components

| Component | Location | Role |
| --- | --- | --- |
| Suite | `benchmark/suites/*.json` | Defines tasks, prompts, rubric, hard assertions, setup |
| Runner | `benchmark/runner/main.py` | CLI entry point; runs suites and generates reports |
| WebUI | `benchmark/runner/webui.py` | Web console; manages suites, jobs, environments, logs, reports |
| Agent client | `benchmark/runner/agent_client.py` | Calls the Go agent's `/api/chat`, `/api/tools/*`, `/api/history` |
| Judge | `benchmark/runner/judge.py` | Calls an OpenAI-compatible endpoint; scores using pre/post screenshots and the trace |
| MobileGym bridge | `benchmark/mobilegym/bridge/` | Wraps a MobileGym env as an environment bridge API |
| ADB Android bridge | `benchmark/adbandroid/` | Wraps an Android emulator/physical device as an environment bridge API via adb (see its README) |
| Desktop bridge | `benchmark/desktop/` | Wraps the host macOS/Linux/Windows desktop through pyautogui as an environment bridge API |
| Docker daemon worker | `benchmark/docker/Dockerfile.agent-daemon` | The isolated agent daemon the WebUI starts when running a job |

### 1.3 Execution flow

The main flow for a single task lives in `runner/runtask.py`:

1. Check that the agent daemon is reachable.
2. Clear the agent conversation history.
3. If `environment_url` is set, call the environment's `/api/setup`.
4. Run the suite/task's optional setup.
5. If `environment_url` is set, fetch `pre.jpg` via `/api/providers/screenshot`.
6. Call the agent's `/api/chat` with the actual prompt.
7. If `environment_url` is set, fetch `post.jpg` via `/api/providers/screenshot` again.
8. Extract the tool trace from the agent history.
9. Run the hard assertions.
10. If the judge is enabled, submit the rubric, trace, final response, and
    pre/post screenshots to the judge model.
11. Write `manifest.json`, `results.jsonl`, `summary.md`, `report.html`, and each
    task's artifacts.
12. If `environment_url` is set, call `/api/release` to free the task route.

The judge currently uses only two images: `pre.jpg` before the task and `post.jpg`
after. It does not consume every intermediate screenshot.

### 1.4 What is an environment bridge

The environment bridge is the unified HTTP protocol the benchmark uses to connect
to a real device, a simulator, or another environment. Both the Go agent and the
MobileGym bridge implement this interface set; environment bridge mode wires the
agent's screen and MNK providers to the selected bridge.

Typical scenario:

- The WebUI starts an isolated Docker agent daemon per job/task worker.
- That daemon uses `--environment-bridge-mode` to wire its screen and MNK
  providers to the selected environment bridge.
- From the agent's perspective it still calls ordinary tools such as
  `screenshot`, `touch_gesture`, and `keyboard_tap`; those tools use the injected
  providers.
- The bridge receives `POST /api/providers/screenshot` and
  `POST /api/providers/mnk` requests.

Notes:

- The environment bridge is the single channel through which the agent performs
  actions and the runner initializes the environment and captures pre/post
  screenshots.
- The runner does not obtain pre/post screenshots through agent tool calls; it
  calls the environment bridge's `/api/providers/screenshot` directly.
- `run --agent-url ...` on the CLI calls the specified agent directly and does not
  start an environment bridge automatically; if you need an environment bridge you
  must start the daemon yourself and pass the relevant daemon parameters.

#### Mock Aiden App environments

Phone Bridge strategy tests usually do not need a physical phone or emulator. A
suite can declare a default `mock_environment`, and each task can override it, to
start an in-process scripted environment bridge that provides:

- A fixed Phone Bridge runtime state, including iOS/Android, foreground/background,
  iOS PiP Bridge, and Android FGS Bridge.
- Deterministic responses for contacts, clipboard, calendar, notifications, app
  search, and text-entry tools.
- A generated phone screen artifact whose text can change after scripted tool
  calls; prompt-conditioned policy suites do not require the Agent to inspect it.

Keep these suites in a separate directory such as `suites/aiden_app/`. Prefer one
policy-matrix suite with task-level mocks when cases only differ by runtime state
or scripted tool results. A suite-level mock remains useful as a default; an
individual task may override it. If there is no suite-level default, every task
must declare its own mock.

The Notes entry suite declares three UI states directly in task prompts, so the
benchmark tests policy selection without mixing in visual perception:

| Prompt-defined UI state | Expected app-entry policy | Suite |
| --- | --- | --- |
| Blank Notes editor already visible | Do not reopen or search; enter text directly | `notes_entry_policy_v1.json` |
| Home screen with Notes icon visible | Click the visible icon; do not search | `notes_entry_policy_v1.json` |
| Notes page/icon not visible | Use `search_launch_app`; do not use `bridge_open_app` | `notes_entry_policy_v1.json` |

All three return the same fixed Biden contact, hide the unavailable
`bridge_open_app`, require `enter_text`, and forbid a separate
`bridge_clipboard` staging call.

`phone_bridge_data_policy_v1.json` covers contacts, calendar query/create,
clipboard read/write, and notifications across these runtime policies:

| Runtime state | Expected Phone Bridge policy |
| --- | --- |
| iOS background, PiP disabled, Dynamic Island return entry reachable | Call the `bridge_*` data tool directly; the tool restores Aiden internally before sending the command. Do not click Dynamic Island manually. |
| iOS background with PiP enabled | Send background-safe data commands directly through the PiP queue. |
| Android background with FGS enabled | Send background-safe data commands directly through the FGS queue. |

`bridge_open_app` is excluded in all of these background-policy cases. PiP and
FGS keep data commands available; they do not turn app launch into a
background-safe operation.

Run it with:

```bash
uv run python -m runner run \
  --suite suites/aiden_app/phone_bridge_data_policy_v1.json \
  --auto-agent-setup \
  --no-judge \
  --verbose
```

Do not pass `--environment-url`: the suite starts and owns its mock environment.
On the WebUI, selecting only mock suites automatically shows `Mock Aiden App
environment`; clicking Run skips the device picker and uses the same isolated
task-worker path. Do not mix mock suites and real-device/MobileGym suites in one
job.

On the CLI, mock suites require `--auto-agent-setup` so every task gets an isolated
daemon and benchmark token. The runner uses that token to call the benchmark-only
`/api/benchmark/phone_bridge_state` endpoint before the task. The endpoint is not
registered on a normal daemon without a benchmark token.

#### Real Aiden App connection capabilities

`suites/aiden_app/connection_capabilities_v1.json` validates the real end-to-end
route introduced by Phone Bridge proxy mode:

```text
Aiden App -> environment bridge -> per-task Agent daemon -> Agent tool -> Aiden App
```

Unlike the mock policy suites above, this suite requires a physical iOS or Android
phone. It covers:

- forwarding the App's environment snapshot into task runtime context;
- semantic app and HTTPS URL opening;
- a clipboard write/read/restore round trip;
- a calendar create/query/delete round trip with stale-fixture cleanup;
- a contact query/update/restore round trip; and
- local notification delivery with a structured result.

Before running it:

1. Connect the Aiden App to the environment bridge and grant Contacts, Calendar,
   and Notification permissions.
2. Create exactly one contact named `Aiden Benchmark Fixture` with phone
   `+1 202-555-0199`, email `benchmark-contact@example.com`, organization
   `Aiden Baseline`, and note `managed benchmark fixture`.
3. Keep the phone unlocked. Each task's setup returns to the Aiden App so a
   foreground connection is available before the scored prompt.

The contact case restores every field after verification. It intentionally does
not benchmark contact creation because the App capability has no contact-delete
operation, so a repeated suite could not clean up safely. The clipboard and
calendar cases also restore their initial state; a notification is the only
intentional transient side effect.

Run one platform at a time:

```bash
uv run python -m runner run \
  --suite suites/aiden_app/connection_capabilities_v1.json \
  --auto-agent-setup \
  --environment-url http://<environment-bridge-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --verbose
```

Use `--target-platform android` for Android. Do not use this suite with the mock
environment: its purpose is to verify that the bridge forwards the real Aiden App
connection, status, commands, and structured responses.

Example schema:

```json
{
  "tasks": [
    {
      "id": "ios_dynamic_island_contacts_query",
      "prompt": "iOS，Aiden 在后台，PiP 未开启，灵动岛入口可达。查询 Biden。",
      "mock_environment": {
        "phone_bridge": {
          "connected": false,
          "platform": "ios",
          "app_state": "background",
          "return_entry": "dynamic_island",
          "return_entry_available": true,
          "pip_bridge_enabled": false,
          "fgs_bridge_enabled": false
        },
        "screen_text": "Aiden Dynamic Island return entry is reachable.",
        "tools": {
          "bridge_contacts": {
            "input_contains": {"action": "query", "query": "Biden"},
            "output": {
              "ok": true,
              "restored_from_return_entry": true,
              "contacts": [{"name": "Biden", "phone_numbers": ["+1 202-555-0147"]}]
            }
          }
        }
      }
    }
  ]
}
```

`screen_contains` is an optional scripted-state precondition. The mock response
matches only when the current generated screen contains that text. This prevents
a task from entering text before the preceding open/click action has actually
transitioned the fixture into Notes. It is an internal fixture state guard, not a
requirement for the Agent to call `screenshot`.

`input_contains` normally compares leaf values exactly. For a string field that
may include harmless surrounding context, use `{"$contains": "substring"}` as
the expected value. The same matcher works in mock responses and
`hard_assertions.required_tool_calls`; for example, `{"text": {"$contains":
"+1 202-555-0147"}}` accepts both the bare number and `Biden: +1 202-555-0147`.

`enter_text` has its own internal decision chain in the real Go tool:

1. Prepare or reuse the clipboard through the connected/background Phone Bridge
   route.
2. Focus the visible field.
3. Try `quick_action` paste (`Meta+V` on iOS, `Ctrl+V` on Android).
4. If that action reports an error, call `keyboard_tap` with the paste shortcut.
5. Observe and verify the field.
6. If the shortcut had no visible effect and the field is still empty, long-press
   the field and tap the visible Paste/粘贴 menu action.

The clipboard/paste sub-path does not fall back to typing the target text itself.
The top-level `enter_text` tool owns the HID/IME typing fallback. Because mock suites route
`enter_text` through the scripted environment, they validate the Agent's
tool selection but not this internal fallback implementation. The Go unit tests
cover those branches; a real-phone smoke test is still needed for platform paste
behavior.

Use a real phone for a separate end-to-end smoke suite when the result depends on
actual Contacts permissions/data, iOS PiP polling, Android FGS lifecycle, USB ECM,
background queue delivery, app launching, or HID paste reliability. The mock suite
validates agent policy and tool selection; it does not validate those OS and
hardware integrations.

### 1.5 What the bridge server does

The bridge server is the HTTP adapter layer between a concrete environment and the
Aiden benchmark. The MobileGym bridge exposes a MobileGym env as an environment
bridge; the Go agent exposes the same interface set when connected to a real
device.

When integrating a new environment, see
[`benchmark/environment_bridge.md`](environment_bridge.md).

Standard environment bridge interface:

| Endpoint | Purpose |
| --- | --- |
| `GET /health` | Health check |
| `GET /api/tools` | Legacy tool catalog for bridge clients and debugging |
| `POST /api/tools/<tool>` | Legacy bridge tool invocation |
| `POST /api/providers/mnk` | MNK provider operation used by the Go agent |
| `POST /api/setup` | Reset/claim the env for a benchmark task |
| `POST /api/release` | Release the env held by a benchmark task |
| `GET /api/concurrent` | Return how many concurrent tasks this bridge supports |
| `POST /api/providers/screenshot` | Return a JSON screenshot; used by the runner and the WebUI task screen page |

Concurrent MobileGym is routed by `benchmark-task-id`:

- The WebUI generates a `benchmark-task-id` of the form `<suite-key>:<task-id>` per
  task worker.
- The runner sends this header when calling `/api/setup`, `/api/release`,
  `/api/providers/screenshot`.
- The agent daemon's provider requests carry the same benchmark task id.
- The bridge routes requests to the same env based on this id.

If a MobileGym environment's env pool capacity is `N` and the suite has more than
`N` tasks:

- The WebUI reads the bridge capacity via `/api/concurrent` and starts at most `N`
  task workers at once.
- The excess tasks wait in the runner's worker queue.
- After a finished task calls `/api/release`, its env can be reused by later tasks.

When the WebUI currently creates a MobileGym environment, `Envs` defaults to `5`.

### 1.6 Suite structure

Minimal structure of a regular suite:

```json
{
  "name": "phone_control_v1",
  "description": "Agent-driven phone control benchmark.",
  "prompt_prefix": "Common constraints prepended to every task prompt.",
  "global_reset": {},
  "tasks": [
    {
      "id": "open_settings",
      "category": "single_step",
      "description_for_judge": "Agent must open system Settings.",
      "prompt": "Please open system Settings.",
      "rubric": [
        {
          "id": "in_settings",
          "check": "Post-screenshot shows the Settings app main page."
        }
      ],
      "hard_assertions": {
        "min_tool_calls": 1,
        "max_tool_calls": 8,
        "must_complete_within_sec": 90
      }
    }
  ]
}
```

Common fields:

| Field | Description |
| --- | --- |
| `prompt_prefix` | Prefix for every task prompt; constrains device type, tool usage, etc. |
| `global_reset` | Suite-level reset configuration |
| `setup` | One task-level pre-step or an ordered array of pre-steps; supports `agent_prompt`, benchmark-token-protected `seed_memory`, `seed_episode`, `seed_notification`, and generic `assert_memory` checks |
| `app_ids` | Optional MobileGym app IDs to preload during environment setup; omitted tasks skip eager app data loading |
| `foreground_app_id` | Optional exact MobileGym app ID to open after reset; independent of the `app_ids` preload list. Setup fails if it cannot be opened |
| `environment_assertions` | Optional dotted-path checks against the environment's final state and route (for example `apps.scroll_lab.selectedItemId` or `route.path`). A scalar value must match exactly; an object whose keys are a subset of `min`/`max` is an inclusive numeric band (for example `{"min": 4, "max": 10}`) |
| `memory_assertions` | Optional post-task `recall_memory` observation with `query`, `expected_count`, `expected`, and/or `absent_ids`; uses the same checks as `assert_memory` setup |
| `rubric` | The judge model's scoring items |
| `hard_assertions` | Deterministic checks, e.g. tool-call counts, timeout, required/forbidden tools |
| `hard_assertions.required_tool_calls` | Requires a tool call whose input contains a specified nested subset |
| `repeats` | Number of times a single task is repeated |
| `input_screenshot` | Static image input, suitable for perception tasks |
| `expected_answer` | Direct answer for multiple-choice/fixed-answer tasks |
| `trace_observations` | Checks on specific behaviors in the trace, e.g. whether a given skill was read |
| `mock_environment` | Suite-level default or task-level scripted Phone Bridge state, tool responses, and mock screen |

For `agent_prompt`, set `expected_response` when setup success has a precise
completion marker. The runner compares the trimmed response exactly and fails
the task setup on any other output.

A unit suite is a different format with `kind` set to `unit`; it tests a tool's
input/output directly without going through agent chat.

#### Memory benchmark

Run `suites/memory_v1.json` with `--auto-agent-setup` so each task and repeat
starts with an isolated memory store. Reusing a live user's memory store is
unsupported: setup verifies exact fixture counts but does not erase unrelated
memories. Recall/application tasks seed and verify their own data instead of
depending on another task's writes. The multi-turn update task intentionally
uses real Agent turns to test repeated updates and the final stored value; its
last answer may use the conversation context without an unnecessary recall.

`memory_assertions` checks the persisted state after the Agent finishes. These
harness reads are excluded from Agent tool counts and recall evidence. The
runner records their results in `trace.json` as `memory_state`; semantic rubrics
can evaluate the actual stored meaning. A state mismatch fails the task, while
unavailable evidence is an evaluation error and is excluded from Agent success
metrics. Use a query limit covering the known isolated fixture set; absence and
count checks describe the returned result set. The judge also receives the
Agent's memory-tool results, so failed writes cannot pass on intent alone.

Session-chunk setup verifies that `recall_session_chunks` can return the seeded
session and its summary without topic filters, after any requested history
clear. Missing or unreadable evidence is a setup error, excluded from Agent
success metrics. This check does not supply the answer to the Agent or choose
its retrieval query.

The corrected fixtures and rubrics change the suite hash. Establish a new
baseline before evaluating Agent changes; scores from the previous suite are
not directly comparable. City names and equivalent natural-language facts are
judged semantically, while identifiers such as employee IDs remain exact.

#### Notification Memory benchmark

Use `suites/notification_memory_v1.json` to validate the notification memory
path end to end. Its `seed_notification` setup writes deterministic raw events
to the date-sharded JSONL log, runs the real notification processor through the
benchmark-only control endpoint, and then checks the Agent's `recall_memory`
behavior. The setup endpoint is benchmark-token protected and does not
exist on a daemon without a benchmark token.

The suite separates ingestion/recall claims from explicit Memory action claims:

- `delivery_notification_recall` checks that a useful notification becomes one
  temporary memory and is recalled with the original delivery fact.
- `notification_noise_is_filtered` checks that OTP and marketing events remain
  in the raw log but do not become memory.
- `notification_user_fact_beats_public_info` repeats the real-model
  classification five times: public automotive news must be ignored while the
  user's repayment amount and due date must be retained.
- `notification_batch_cursor_drain` checks that a backlog larger than one batch
  is fully committed without producing memory for verification-code noise.
- `notification_explicit_update` checks revision-guarded replacement of an
  existing conclusion.
- `notification_explicit_reinforce` checks that repeated evidence preserves the
  conclusion while advancing its revision without creating a duplicate.
- `notification_explicit_remove` checks that a removed notification deletes only
  the temporary conclusion tied to that stable notification identity.
- `notification_explicit_promote` checks temporary-to-long-term promotion and
  removal of the temporary source.

The useful-notification case depends on the configured model's consolidation
decision; the raw-log and obvious-noise cases have deterministic setup gates.
The Go notification processor tests cover cursor recovery, generation reset,
batch-call count, proposal validation, update/remove, and storage behavior.
Run the suite against isolated benchmark daemons because notification cursors
and memory are durable. Explicit action tasks compose generic setup primitives:
`seed_memory`, `seed_notification`, then `assert_memory`.
`assert_memory.expected` supports scalar/content/tag/entity checks plus generic
`source_refs_contain` and `evidence_refs_contain` entries. Reference entries may
match `type`, `id`, and `event_ids_contains`, which lets suites verify evidence
preservation without adding scenario-specific runner logic.

```bash
cd benchmark
uv run python -m runner run \
  --suite suites/notification_memory_v1.json \
  --auto-agent-setup \
  --agent-config /path/to/agent.toml \
  --run-id notification-memory-v1
```

This command enables the configured LLM judge for effect checks: whether the
useful notification answer preserves the delivery fact and whether it states
that OTP/marketing noise is not retained. Use `--no-judge` for deterministic
functional gates only; those gates still validate fixture persistence through
the benchmark endpoint, cursor advancement, memory count/scope, and tool-call
policy. Raw JSONL preservation is covered by the Go integration tests rather
than an Agent shell task. A judge API key is required for the effect run.

For a lower-level performance snapshot, run the deterministic Go benchmarks:

```bash
cd src/agent
go test ./internal/agent -run '^$' -bench 'BenchmarkNotification' -benchmem
```

## 2. WebUI guide

### 2.1 Startup

Recommended command:

```bash
cd benchmark
uv run python -m runner webui
```

Default address:

```text
http://127.0.0.1:8765
```

Common parameters:

| Parameter | Default | Description |
| --- | --- | --- |
| `--host` | `127.0.0.1` | WebUI listen address |
| `--port` | `8765` | WebUI listen port |
| `--suites-dir` | `benchmark/suites` | Suite scan directory |
| `--runs-dir` | `benchmark/runs/webui` | Directory for WebUI jobs, raw runs, logs |
| `--base-config-dir` | `benchmark/config` | Agent config template directory |
| `--agent-config` | empty | Path where the WebUI's `agent.toml` is saved |
| `--daemon-image` | `aiden-agent-daemon:local` | Job worker daemon image |
| `--mobilegym-image` | `aiden-mobilegym-simulator:py311` | MobileGym environment image |
| `--no-build-daemon-image` | false | Do not build the daemon image automatically |
| `--no-build-mobilegym-image` | false | Do not build the MobileGym image automatically |

### 2.2 Page areas

Main areas on the WebUI home screen:

- Suites: the suite list on the left; filter and select one or more suites.
- Run configuration: judge toggle, judge model, API key.
- Run selected suites: opens the environment selection/configuration dialog.
- Progress: overall progress of the current active job.
- Task workers: task-level status, screen, log, and stop for MobileGym concurrent
  tasks.
- Jobs: historical jobs, showing job id, suite name, environment, status, and the
  combined report.
- Agent config: view and edit the `agent.toml` used by this WebUI session.
- Log: view the runner/daemon logs of a job or task worker.

### 2.3 Configuring the judge

The WebUI enables the judge by default. The judge calls an OpenAI-compatible
endpoint and uses the `AIDEN_BENCHMARK_JUDGE_API_KEY` credential.

Usage:

1. Keep `Enable judge` checked.
2. Fill in `Judge model`; the default is `anthropic/claude-sonnet-4-6`.
3. Fill in the API key. After saving, the WebUI persists the setting but does not
   echo the plaintext key back on the page.

If you only want to collect the trace and screenshots without calling the judge,
uncheck `Enable judge`.

### 2.4 Configuring the environment

Clicking `Run selected suites` opens the environment dialog.

#### Device environment

A device environment is for an existing device/tool endpoint. Fill in:

- Name: a custom name.
- Endpoint: the HTTP endpoint.

When the WebUI starts a job it will:

1. Start an isolated Docker agent daemon for the job.
2. Connect that daemon's screen and MNK providers to this endpoint via the
   environment bridge.
3. Run the benchmark using the isolated daemon's `/api/chat`.

This is suitable for a real device or external tool environment that needs
isolated agent config, logs, and memory.

#### MobileGym environment

A MobileGym environment is created and managed by the WebUI directly:

1. Switch to the `MobileGym` tab.
2. Fill in a name.
3. Set `Envs`, default `5`.
4. Click `Start MobileGym`.
5. Wait for the status to become running, then select that environment.
6. Click `Run selected suites` at the bottom of the dialog.

The WebUI starts the MobileGym container and bridge server and records:

- Bridge endpoint: used by the Docker daemon's environment bridge.
- Public endpoint: used by the WebUI and the runner to call `/api/concurrent`,
  `/api/setup`, `/api/providers/screenshot`, `/api/release`.
- Task screen link: the screen link for each task worker is provided by the WebUI;
  the WebUI backend pulls the screenshot via the bridge's `/api/providers/screenshot`.

MobileGym containers recovered after a WebUI restart are marked `stale`. They
remain visible so they can be stopped or deleted, but cannot be selected for a
new benchmark. Start a fresh MobileGym environment for every new WebUI session.

### 2.5 Running a job

Flow:

1. Select one or more suites.
2. Configure the judge.
3. Click `Run selected suites`.
4. Select or create an environment in the dialog.
5. Confirm the run at the bottom of the dialog.

After running:

- The Jobs table shows the suite name and job status.
- Progress shows overall progress.
- In MobileGym concurrent mode, Task workers shows each task's status, screen, and
  log.
- You can click Stop for the whole job, or Stop for an individual task worker.

### 2.6 Viewing results

The `report` in the Jobs table is a combined report, not a single-task report.

Report contents include:

- The task list and pass rate.
- Each task's prompt, final response, hard assertions, rubric.
- Pre/post screenshots.
- `View full trace`, for inspecting the execution manually and validating whether
  the judge was reasonable.

WebUI run data is saved by default under:

```text
benchmark/runs/webui/<job-id>/
├── job.json
├── state.json
├── runner.log
├── daemon.log
├── config/
├── raw/<run-id>/
└── workers/
```

The WebUI persists job records. After a WebUI restart, historical jobs are restored
from `job.json`; if a job was still running before the restart, it is marked as
stopped on recovery.

### 2.7 Common issues

#### The MobileGym screen does not show

Check first:

- Whether the MobileGym environment is running.
- Whether the task worker has already started running.
- Whether the `screen` link carries a `benchmark-task-id`.
- Whether the task has already been released; after release the screen may have no
  active env.

#### The judge says there are no screenshots

Check the task metrics in the report:

- `pre_screenshot_file`
- `post_screenshot_file`
- `judge_image_count`
- `judge_image_labels`

In MobileGym mode the runner should produce `pre.jpg` and `post.jpg` via the
bridge's `/api/providers/screenshot`. If they are missing, it is usually because the environment
endpoint is unreachable, the `benchmark-task-id` routing is missing, or the task
was released/failed before the screenshot.

#### The suite has more tasks than Envs

This is normal. The WebUI runs concurrently up to the env pool capacity and queues
the remaining tasks.

## 3. CLI guide

### 3.1 Entry points

Recommended entry point:

```bash
cd benchmark
uv run python -m runner <command> [options]
```

Compatibility entry point:

```bash
python3 scripts/aiden_benchmark.py <command> [options]
```

If `<command>` is omitted, the legacy script treats it as `run`.

### 3.2 run: run a regular suite

Basic command:

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --agent-url http://127.0.0.1:8080
```

Common parameters:

| Parameter | Description |
| --- | --- |
| `--suite PATH` | Required, path to the suite JSON |
| `--agent-url URL` | Agent daemon address; default `http://localhost:8080` or `AIDEN_AGENT_URL` |
| `--environment-url URL` | Optional, environment bridge address; used for `/api/setup`, `/api/providers/screenshot`, `/api/release` |
| `--target-platform PLATFORM` | Target device platform (`auto`, `ios`, or `android`); validated against the environment or agent |
| `--benchmark-token-file PATH` | File containing the benchmark control token used for protected agent endpoints |
| `--auto-agent-setup` | Ignore `--agent-url`; auto-start isolated agent daemons concurrently per `/api/concurrent` |
| `--daemon-image IMAGE` | Agent daemon image used by `--auto-agent-setup` |
| `--base-config-dir DIR` | Agent config template directory used by `--auto-agent-setup` |
| `--agent-config PATH` | agent.toml used by `--auto-agent-setup` |
| `--no-build-daemon-image` | Do not build the agent daemon image automatically |
| `--judge-model MODEL` | Judge model; default `claude-sonnet-4-6` |
| `--no-judge` | Skip the LLM judge; only run hard assertions |
| `--repeats N` | Override the task's `repeats` |
| `--out DIR` | Output directory; default `benchmark/runs` |
| `--state-file PATH` | Write the run-state JSON; used by the WebUI to show progress |
| `--task-id ID` | Run only one task; can be repeated |
| `--task-ids A,B` | Run only the comma-separated tasks |
| `--run-id ID` | Set the run directory name |
| `--benchmark-task-id ID` | Environment routing id, used by MobileGym workers |
| `--skip-clock-wait` | Do not wait for the agent board clock to sync |
| `--clock-timeout-sec N` | Max time to wait for clock sync |
| `--agent-ready-timeout-sec N` | Time to wait for the agent to be ready before each task |
| `--agent-recovery-timeout-sec N` | Recovery wait after timeout/skipped/failed |
| `--inter-task-cooldown-sec N` | Cooldown between tasks |
| `--verbose` / `-v` | Print detailed rubric results |

Run only a few tasks:

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --task-id open_settings \
  --task-id scroll_page_down
```

Without the judge:

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --no-judge
```

CLI run with the MobileGym bridge:

```bash
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --agent-url http://127.0.0.1:8080 \
  --environment-url http://127.0.0.1:8888
```

Notes:

- `--agent-url` is the agent daemon.
- `--environment-url` is the environment bridge endpoint that implements
  `/api/setup`, `/api/providers/screenshot`, `/api/providers/mnk`, and `/api/release`.
- Without `--environment-url`, the runner can still run agent chat but will not
  save live pre/post screenshots; judge results that rely on visual screenshots
  will be weaker.

Auto-start the agent daemon and run concurrently per bridge capacity:

```bash
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --environment-url http://127.0.0.1:19090 \
  --auto-agent-setup
```

This mode ignores `--agent-url` and reads the environment bridge's
`/api/concurrent`; if that read fails, it runs with concurrency `1`.

Agent configuration notes:

- `runner run --agent-url ...` only calls an already-started agent daemon and does
  not read or modify `agent.toml` itself. In this mode, configure the agent when
  starting the daemon, e.g. `start-agent-daemon --agent-config path/to/agent.toml`,
  or mount the config the way the daemon expects.
- `runner run --auto-agent-setup` prepares a separate agent config directory and
  starts a daemon automatically for each concurrent task worker. Here you can use
  `--agent-config path/to/agent.toml` to specify the agent config the benchmark
  uses.
- `--base-config-dir` defaults to `benchmark/config`; the runner copies static
  configuration assets into the worker config directory. Runtime state such as
  memory, logs, caches, sessions, and skill state is always created fresh. The
  static `memory/extraction.yaml` policy is preserved when present.
- If `--agent-config` is specified, its content is written as the worker's
  `agent.toml`; if not, the runner prefers rendering `agent.toml` from
  `--base-config-dir/agent.toml.template`. The built-in template reads only the
  `AIDEN_BENCHMARK_AGENT_*` variables listed below.
- Credential references such as `api_key = "$ANTHROPIC_AUTH_TOKEN"` are resolved
  on the host while preparing the worker config. The resolved config is mounted
  read-only into Docker. A missing credential fails before the daemon starts.
- Before `--auto-agent-setup` uses a MobileGym endpoint, the runner performs a
  short `setup` / `release` preflight. A bridge that is healthy but cannot reset
  is rejected with instructions to start a fresh environment.

Example:

```bash
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --environment-url http://127.0.0.1:19090 \
  --auto-agent-setup \
  --agent-config ./local.agent.toml
```

### 3.3 unit: run a tool unit-test suite

A unit suite calls a single tool directly and checks the output structure and
error state.

Run a single unit suite:

```bash
uv run python -m runner unit \
  --suite suites/unit/tools/quick_action_android_v1.json \
  --agent-url http://127.0.0.1:8080
```

Run all unit suites under a directory:

```bash
uv run python -m runner unit \
  --suite-dir suites/unit/tools \
  --agent-url http://127.0.0.1:8080
```

`unit` does not call the LLM judge.

### 3.4 rejudge: re-score

When you only modify the rubric or want to switch the judge model, you do not need
to re-operate the device:

```bash
uv run python -m runner rejudge \
  --run-dir runs/<run-id> \
  --judge-model anthropic/claude-sonnet-4-6
```

`rejudge` reads the existing artifacts and `rubric_spec` and rewrites the judge
verdict/status in `results.rejudged.jsonl`. Recorded execution failures and hard
assertion failures remain failed. Text-only tasks do not require screenshots;
screenshots recorded as present in the original run must still be available.
Post-task memory evidence must also be present when memory assertions were used.

### 3.5 compare: compare two runs

```bash
uv run python -m runner compare \
  --runs runs/<run-a> runs/<run-b>
```

Use it to see task status changes and performance changes; good for regression
checks.

#### Episode Memory consolidation diagnostic

Each of the three suites contains both a natural recall task and a physical-iPhone
execution task. The conditions are:

1. `before`: the Episode exists but has not produced Memory;
2. `after`: the Episode Memory Worker processes the same Episode into Device Memory;
3. `legacy`: a fixed legacy direct-extraction Memory fixture is present.

This is a fast retrieval-and-decision diagnostic, not evidence of improved UI
execution. Use it to determine whether failures come from consolidation,
on-demand recall, or use of recalled Memory. The primary comparison is `before`
versus `after`; the secondary comparison is `after` versus `legacy`. The legacy
fixture does not execute or benchmark the removed extractor itself.

The natural recall task uses the same real-environment suite type as the
physical-iPhone task, but its prompt and hard assertions prohibit device
operation. Use `--auto-agent-setup` with a real environment bridge so every
attempt gets a fresh agent daemon and isolated data directory, preventing
Memory, Episode, session, and context-cache leakage between conditions.

```bash
uv run python -m runner run \
  --suite suites/episode_memory_before_v1.json \
  --task-id qa_notes_title_save_procedure \
  --auto-agent-setup \
  --environment-url http://<real-environment-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --repeats 5 \
  --no-judge \
  --run-id episode-memory-before

uv run python -m runner run \
  --suite suites/episode_memory_after_v1.json \
  --task-id qa_notes_title_save_procedure \
  --auto-agent-setup \
  --environment-url http://<real-environment-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --repeats 5 \
  --no-judge \
  --run-id episode-memory-after

uv run python -m runner run \
  --suite suites/episode_memory_legacy_v1.json \
  --task-id qa_notes_title_save_procedure \
  --auto-agent-setup \
  --environment-url http://<real-environment-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --repeats 5 \
  --no-judge \
  --run-id episode-memory-legacy

uv run python -m runner compare \
  --runs runs/episode-memory-before runs/episode-memory-after

uv run python -m runner compare \
  --runs runs/episode-memory-legacy runs/episode-memory-after
```

The comparison reports pass-count, median tool-call, and median wall-time
deltas. The prompt does not name or force a particular memory tool. After the
five-repeat comparison is stable, rerun the same commands with `--repeats 10`
and distinct run IDs.

#### Episode Memory physical-iPhone execution comparison

The physical-iPhone benchmark has the same three conditions as the diagnostic:

1. `before`: the completed Settings Episode exists but is not consolidated;
2. `after`: the Episode Memory Worker processes that Episode into Device Memory;
3. `legacy`: a fixed Device Memory fixture represents the removed synchronous
   Episode-to-Memory procedure output.

Every condition asks the agent to reach the active Ethernet interface's IPv4
details. All suites require a final screenshot, and the judge checks the visible
Ethernet page rather than trusting the final response. Task pass rate is the
primary metric. Tool calls and wall time are secondary efficiency metrics, and
the recall observation is diagnostic only.

Run the physical-iPhone task from each of the same three suites with isolated
agent state:

```bash
uv run python -m runner run \
  --suite suites/episode_memory_before_v1.json \
  --task-id settings_ethernet_ipv4_from_episode \
  --auto-agent-setup \
  --environment-url http://<physical-device-environment-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --repeats 5 \
  --run-id episode-memory-iphone-before

uv run python -m runner run \
  --suite suites/episode_memory_after_v1.json \
  --task-id settings_ethernet_ipv4_from_episode \
  --auto-agent-setup \
  --environment-url http://<physical-device-environment-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --repeats 5 \
  --run-id episode-memory-iphone-after

uv run python -m runner run \
  --suite suites/episode_memory_legacy_v1.json \
  --task-id settings_ethernet_ipv4_from_episode \
  --auto-agent-setup \
  --environment-url http://<physical-device-environment-host>:<port> \
  --target-platform ios \
  --benchmark-token-file /path/to/control_token \
  --repeats 5 \
  --run-id episode-memory-iphone-legacy

uv run python -m runner compare \
  --runs runs/episode-memory-iphone-before runs/episode-memory-iphone-after

uv run python -m runner compare \
  --runs runs/episode-memory-iphone-after runs/episode-memory-iphone-legacy
```

Omit `--task-id` from these commands to run each complete suite. Both tasks then
share the same physical-device environment type; the natural recall task still
does not operate the phone.

Use the same iPhone, model configuration, prompt, and environment reset for all
three runs. Use an isolated agent data directory for every attempt so Episodes,
Memory, sessions, and context caches cannot leak across conditions. After the
five-repeat comparison is stable, rerun the same commands with `--repeats 10`
and distinct run IDs.
MobileGym can validate runner and setup plumbing, but the USB Ethernet execution
result must be validated on the physical iPhone. The primary conclusion comes
from `before` versus `after`; `after` versus `legacy` is a compatibility
comparison and does not benchmark the removed legacy extractor itself.

### 3.6 webui: start the WebUI from the CLI

```bash
uv run python -m runner webui \
  --host 127.0.0.1 \
  --port 8765
```

This command uses the same entry point as the WebUI in Section 2.

### 3.7 start-mobilegym-env: start a MobileGym environment

If you are not using the WebUI, you can also start a detached MobileGym
simulator + bridge container from the CLI:

```bash
uv run python -m runner start-mobilegym-env
```

The command prints:

- `environment_url`: the `--environment-url` the runner uses.
- `docker_environment_url`: the endpoint the agent daemon container uses to reach
  the bridge.
- `web_url`: the MobileGym simulator page.
- `stop_command`: the command to stop this environment.

Common parameters:

| Parameter | Default | Description |
| --- | --- | --- |
| `--envs` / `--parallel-envs` | `1` | Number of MobileGym envs behind the bridge |
| `--web-port` | auto | MobileGym web page port |
| `--bridge-port` | auto | Bridge API port |
| `--mobilegym-image` | `aiden-mobilegym-simulator:py311` | MobileGym container image |
| `--no-build-mobilegym-image` | false | Use only a locally available image |
| `--json` | false | Print machine-readable JSON |

`start-mobilegym-env` does not bind a `benchmark-task-id`. The bridge routes based
on the `benchmark-task-id` carried by each subsequent setup, screen, or tool
request.

### 3.8 start-agent-daemon: start an agent daemon

Start a detached benchmark agent daemon container:

```bash
uv run python -m runner start-agent-daemon \
  --environment-bridge-endpoint http://127.0.0.1:19090 \
  --benchmark-task-id cli-task
```

The command prints:

- `agent_url`: the `--agent-url` the runner uses.
- `environment_bridge_endpoint`: the environment endpoint from the host's
  perspective.
- `docker_environment_bridge_endpoint`: the environment endpoint from inside the
  container.
- `benchmark_task_id`: the route id the daemon's environment bridge requests carry.
- `device_type`: the effective daemon device type read after startup.
- `target_platform`: the benchmark platform derived from the effective device type.
- `stop_command`: the command to stop this daemon.

Common parameters:

| Parameter | Default | Description |
| --- | --- | --- |
| `--port` | auto | Agent daemon API port |
| `--environment-bridge-endpoint` | empty | Device or MobileGym bridge endpoint; empty disables the environment bridge |
| `--device-type` | empty | Optional process-local `basic_settings.device.device_type` override; without a bridge, the daemon otherwise keeps the value from `agent.toml`; with a bridge, the value constrains its reported platform |
| `--benchmark-task-id` | `cli-task` | Route id used by environment bridge requests |
| `--agent-config` | empty | Specify agent.toml |
| `--base-config-dir` | `benchmark/config` | Agent config template directory |
| `--daemon-image` | `aiden-agent-daemon:local` | Agent daemon image |
| `--no-build-daemon-image` | false | Use only a locally available image |
| `--json` | false | Print machine-readable JSON |

The agent config rules used by `start-agent-daemon` are the same as
`run --auto-agent-setup`. Platform resolution never rewrites the generated
config. When an environment bridge is provided,
the command resolves its platform once and passes it to the daemon through
process-local `--device-type`. Without a bridge, an explicit `--device-type` is
an optional override; if it is omitted, the daemon keeps
`[basic_settings.device].device_type` from `agent.toml`. The daemon applies a process-local
override after loading `agent.toml`, reports the effective value as
`status.device_type`, and the command validates that value after startup. The
deprecated `--target-platform` spelling remains accepted as a compatibility
alias. After starting the daemon manually, pass the printed `agent_url` to
`runner run --agent-url`; the runner
derives and validates its platform against the environment or CLI constraint.

Recommended CLI MobileGym debug flow:

```bash
uv run python -m runner start-mobilegym-env --bridge-port 19090

uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --environment-url http://127.0.0.1:19090 \
  --auto-agent-setup
```

If you want to manually pin one route to debug a single task, you can also start
the agent daemon explicitly:

```bash
uv run python -m runner start-agent-daemon \
  --port 18081 \
  --environment-bridge-endpoint http://127.0.0.1:19090 \
  --benchmark-task-id cli-task

uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --agent-url http://127.0.0.1:18081 \
  --environment-url http://127.0.0.1:19090 \
  --benchmark-task-id cli-task
```

Note: if there are multiple envs behind the MobileGym bridge, `/api/setup`,
`/api/providers/screenshot`, `/api/providers/mnk`, and `/api/release` must all use the same
`benchmark-task-id`. When manually starting a long-lived agent daemon from the CLI,
use a fixed route id such as `cli-task`; when you need to run multiple tasks
concurrently, prefer the WebUI so each task worker gets its own daemon and its own
route id.

## 4. Output and reports

Default output of a regular CLI run:

```text
benchmark/runs/<run-id>/
├── manifest.json
├── results.jsonl
├── summary.md
├── report.html
├── _judge_cache/
└── tasks/<task-id>/
    ├── pre.jpg
    ├── post.jpg
    ├── history.json
    ├── trace.json
    └── judge.json
```

Key files:

| File | Description |
| --- | --- |
| `manifest.json` | Run metadata, suite sha, git sha, totals |
| `results.jsonl` | One result line per task, for scripting |
| `summary.md` | Text summary |
| `report.html` | Main report for manual viewing |
| `history.json` | Raw agent history |
| `trace.json` | Tool calls and final response extracted from history |
| `pre.jpg` / `post.jpg` | Before/after screenshots used by the judge |
| `judge.json` | Judge verdict, reason, image_count, cache_key |

At the end of a run, the runner tries to upload the report to the agent board's
`/benchmark` page. If the upload fails, the local `report.html` is still available.

## 5. Environment variables

Common environment variables:

| Variable | Description |
| --- | --- |
| `AIDEN_AGENT_URL` | Default for CLI `--agent-url` |
| `AIDEN_ENVIRONMENT_URL` | Default for CLI `--environment-url` |
| `AIDEN_BENCHMARK_AGENT_PROVIDER` | Agent provider type rendered into the default `agent.toml` |
| `AIDEN_BENCHMARK_AGENT_MODEL` | Agent model rendered into `agent.toml` and recorded in the manifest |
| `AIDEN_BENCHMARK_AGENT_BASE_URL` | Agent model endpoint rendered into the default `agent.toml` |
| `AIDEN_BENCHMARK_AGENT_API_KEY` | Agent credential rendered into the default read-only `agent.toml` |
| `AIDEN_BENCHMARK_JUDGE_MODEL` | Default judge model for `run` and `rejudge` |
| `AIDEN_BENCHMARK_JUDGE_BASE_URL` | Default OpenAI-compatible judge endpoint |
| `AIDEN_BENCHMARK_JUDGE_API_KEY` | Judge API key |
| `AIDEN_BENCHMARK_ANALYSIS_API_KEY` | Optional post-run analysis credential; defaults to the Judge key |
| `BENCHMARK_STATE_FILE` | Default for CLI `--state-file` |
| `MOBILEGYM_PARALLEL_ENVS` | Default parallel env count for `start_simulator.py` |
| `AIDEN_BRIDGE_BIND_HOST` | MobileGym bridge bind host |
| `AIDEN_BRIDGE_PORT` | MobileGym bridge port |
| `AIDEN_BRIDGE_PUBLIC_HOST` | Bridge external hostname in Docker scenarios |

## 6. Recommended workflows

### Debug a single task

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --task-id open_settings \
  --no-judge \
  --verbose
```

Confirm the agent can complete the operation first, then enable the judge.

### Run a full regression

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --judge-model anthropic/claude-sonnet-4-6 \
  --verbose
```

### MobileGym concurrent regression

The WebUI is recommended:

```bash
uv run python -m runner webui
```

On the page:

1. Start MobileGym and set `Envs`.
2. Select the suite.
3. Click `Run selected suites`.
4. Select the MobileGym environment.
5. Run and inspect Task workers, screen, log, and report.

### Re-score after changing the rubric

```bash
uv run python -m runner rejudge \
  --run-dir runs/<run-id> \
  --judge-model anthropic/claude-sonnet-4-6
```

## 7. Troubleshooting

### The agent is unreachable

Symptom:

```text
agent at http://... is not reachable
```

Check:

```bash
curl http://127.0.0.1:8080/api/tools
```

If it is a WebUI job, look at the job's daemon log.

### The judge reports missing env var AIDEN_BENCHMARK_JUDGE_API_KEY

CLI:

```bash
export AIDEN_BENCHMARK_JUDGE_API_KEY=...
```

WebUI:

Fill in the API key in Run configuration, or turn off `Enable judge`.

### The report has no pre/post images

Check whether `--environment-url` was passed and whether that endpoint supports:

```bash
curl -X POST http://127.0.0.1:8888/api/providers/screenshot \
  -H "Content-Type: application/json" \
  -d '{"format": "jpeg", "quality": 80}'
```

MobileGym concurrency requires a task id:

```bash
curl -X POST http://127.0.0.1:8888/api/providers/screenshot \
  -H "Content-Type: application/json" \
  -H 'benchmark-task-id: suite.json:task_id' \
  -d '{"format": "jpeg", "quality": 80}'
```

### A MobileGym task stays pending or reports no env available

Check:

- Whether the MobileGym environment is running.
- Whether `Envs` is greater than 0.
- Whether task requests carry a `benchmark-task-id`.
- Whether a task exited abnormally without releasing; stopping the job and
  restarting the environment usually clears the state.

### After a WebUI restart, a running job becomes stopped

This is by design. The WebUI persists history but does not recover interrupted
processes; on restart, non-terminal jobs are marked as stopped to avoid falsely
showing them as still running.

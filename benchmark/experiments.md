# Agent operation experiments

The benchmark supports one experiment containing separate Aiden, pi, OpenClaw,
Claude Code, and Codex runs.
Use **Agent experiments** in the WebUI header, or open `/experiments`. Existing
device benchmark jobs and Aiden regression suites keep their existing workflow.

## Existing suite comparison

Experiments reuse the existing suite files without copying or weakening tasks.
Start with `mobilegym_basic.json`, `mobilegym_scroll_regression.json`, or
`mobilegym_scroll_sweep.json`. There is no separate calculation or harness suite.
Each experiment freezes the original suite and runs one independent run per
agent, interleaving task/repetition blocks against the same MobileGym
environment, one attempt at a time unless `concurrency` is raised. The
experiment repeat count overrides task repeats for every agent. Prompt prefixes, task prompts, rubric, tool assertions, state
assertions, app selection, and deadlines retain their original meaning. For
example, the original scroll regression still rejects overshooting item 24/83.

Rubric tasks require the existing Judge. WebUI freezes its enabled/model/base
URL/credential settings at experiment creation; CLI uses `--judge-model`,
`--judge-base-url`, and `AIDEN_BENCHMARK_JUDGE_API_KEY`. Missing or disabled Judge
configuration prevents launch. Judge errors are ineligible attempts, never
successes. Both agents use the same judge, original rubric, before/after
screenshots, tool execution evidence, and final response. Deterministic state
assertions remain additional requirements.

### Device context

Suite prompts were written for Aiden, which knows it operates a device. Coding
harnesses can read a prompt such as "open Settings" as a request about their
own host, so every agent, Aiden included, receives the same short device context
before the suite prompt prefix and task prompt. The manifest records it as
`device_context`. Shared `agent_prompt` preparation runs with at least 120
seconds, because suite timeouts are sized for a warm device rather than a cold
preparer daemon.

Aiden attempts export the per-step screenshots of its episode from the daemon
before the daemon is removed, so its run report shows each step like the
external agents' gateway screenshots.

### Tool layers

The comparison measures device UI operation. External harnesses choose one of
two tool layers with the agent's `tool_layer` field:

- `aiden` (default): the Aiden tool layer described below. This compares
  harnesses, meaning agent loop, prompt, context, planning, and visual grounding,
  with the same tool implementation.
- `raw`: neutral primitives on the environment bridge, without the Aiden tool
  layer. Comparing native Aiden with an external harness on raw tools compares
  complete stacks; comparing the same external harness on both layers measures
  what the Aiden tool layer contributes.

Aiden always uses its own tool layer. In the WebUI, **Device UI tools** selects
the layer for external agents; **Both** runs each selected external agent once
per layer, for example `pi` and `pi-raw`.

The raw layer exposes `screenshot`, `tap`, `long_press`, `swipe`, `type_text`,
`press_key` (`back`, `home`, `enter`, or `backspace`), and `wait`. Coordinates
are pixels in the latest screenshot; the gateway converts them to the bridge's
normalized coordinates. Tool descriptions name the connected mobile device,
because task prompts may not mention it. Each action calls the bridge's shared input primitive,
waits a fixed 500 ms, and returns a new screenshot, so tool-call budgets keep
their meaning. The raw layer does not wait for a stable screen, detect screen
changes, map semantic shortcuts such as `quick_action`, or recover app launches.
External raw attempts do not start an Aiden daemon.

Raw tool names differ from Aiden tool names, so a task's `required_tools` and
`required_tool_calls` do not apply to raw attempts. Those attempts record
`tool_name_assertions: not_applicable`; call budgets, state assertions, and
rubric scoring still apply. The manifest freezes one catalog per tool layer
under `business_tool_catalogs`, and the report labels each agent's layer.

### Aiden tool layer

The shared business tool catalog of the `aiden` layer
is the UI operation surface that Aiden's model sees on the environment
platform: every daemon tool whose `/api/tools` descriptor has
`ui_operation: true`, `agent_exposed: true`, and an `agent_platforms` list that
is empty or includes the platform. On MobileGym Android this covers
`screenshot`, `wait_for_stable_screen`, `touch_gesture`, `keyboard_tap`,
`enter_text`, `mouse_move`, `mouse_scroll`, and `quick_action`, plus `open_app`
and `open_url` when the daemon registers them. The daemon tool metadata is the
single source of truth; a new UI tool joins the comparison when it is marked
there. Descriptions and schemas are shared unchanged.

Each catalog must contain `screenshot`, `touch_gesture`, `keyboard_tap`,
`enter_text`, `mouse_move`, `mouse_scroll`, and `wait_for_stable_screen`, and
required-tool assertions may name only these. Each attempt records the catalog
and its hash in `business-tools.json`; the experiment manifest freezes the
first catalog of each layer, and a later mismatch makes the attempt ineligible. A daemon image
without UI tool metadata fails with a request to rebuild it.

### Tool exposure and the tool gateway

External harnesses run unsandboxed on the benchmark host. Their native tools,
such as shell, file editing, and desktop or browser control, would act on that
host or bypass the device interface, so external harnesses receive only the
business tools of their tool layer. The comparison therefore measures how each
harness plans, grounds, and operates the UI with the same tools. Aiden keeps its
full toolset because it runs inside its own daemon container.

| Harness | Where it runs | Device access | Other tools |
| --- | --- | --- | --- |
| Aiden | Its per-attempt daemon container | Its daemon tools directly | Full toolset and skills, including memory and shell |
| pi | Benchmark host subprocess | Gateway through the operation tools extension | None: `--tools` enables only the catalog |
| OpenClaw | Benchmark host subprocess | Gateway through the stdio MCP bridge | None: its tool policy allows only `benchmark__*` |
| Claude Code | Benchmark host subprocess | Gateway through the stdio MCP bridge | None: `--bare`, `--tools ""`, and `--strict-mcp-config` |
| Codex | Benchmark host subprocess | Gateway through the stdio MCP bridge | MCP resource helpers and `request_user_input` only; shell, web, image, agent, browser, and computer tools are disabled |

Each harness kind is registered in `runner/harnesses.py` with its
configuration rules, installed-runtime check, and adapter; adding a harness
does not change experiment orchestration.

External attempts on the `aiden` layer also start a fresh Aiden daemon
container, used only as the tool server; its chat API and agent loop are never
called.

The gateway is a per-attempt loopback endpoint in the runner. It serves only
catalog tools from one backend: the daemon tool API for the `aiden` layer, or
the environment bridge for the `raw` layer. External harnesses never receive
the daemon or bridge URL, or the daemon's non-UI tools. It folds explicit business failures
(`ok:false`, including screenshot-wrapped `action_output`) into `is_error`,
returns images unchanged, and writes `tool-audit.jsonl` with `step_<n>`
screenshots for every external harness. The pi extension and the MCP bridge
only translate this contract to each harness's native tool and image format;
neither delegates the task to Aiden's chat API. Native planning, context,
retry, and memory behavior stay agent-owned. Each measured attempt starts cold.

External harnesses run on the benchmark host with isolated configuration and a
scratch workspace. OpenClaw's normal workspace, personal sessions, and ambient
configuration are not loaded.

Each attempt records `business_tool_calls` and `native_tool_calls`. For
external harnesses, any native call means the tool policy did not hold; OpenClaw
native calls are taken from its `toolSummary` because they never reach the
gateway audit. `native_loopback_calls` counts native calls whose input
references a local endpoint. `api_retries` counts failed model requests or
endpoint reconnects that pi, Claude Code, or Codex reported; the report lists
attempts with retries, whose timeouts may reflect the network rather than the
harness. The comparison report lists native calls and
flagged attempts per agent.

Existing `phone_control_v1` and `app_workflow_v1` definitions are also accepted
when their requirements are supported. Choose tasks whose apps and capabilities
exist in the connected environment; accepting a suite does not install missing
apps. The current comparison environment is MobileGym Android. Tasks explicitly
requiring other platforms are recorded as skipped for both agents, with reasons
and reduced coverage. Informational trace observations remain diagnostics.
Unsupported suites remain visible in the UI with an explanation, including
Aiden memory seeding/assertions, mocks, initial attachments, and required tools
outside the shared business catalog. Their ordinary Aiden regression entry
points remain available.

For unevaluated `agent_prompt` setup with `clear_history_after=true`, a shared
Aiden preparer runs the original global reset and task setup before either
agent. Every preparation step and measured attempt uses fresh agent state while
preserving the device route. Expected setup responses are checked; failures
exclude the attempt. Preparation traces are retained separately and excluded
from measured time/tokens/cost. Setup that needs retained conversation, seeded
memory, or evaluated preparation is rejected instead of silently changing it.
The preparer is an environment fixture, and is not part of pi's harness.

All agents require Docker for the shared tool runtime. Pi runs on the WebUI
host with isolated configuration and workspace and only the business tools.
OpenClaw runs on the WebUI host with a fresh one-shot state directory and
workspace. Install OpenClaw 2026.10.8 or a compatible version and Node 24.22.1+
(or 26.12.1+) there. Set `AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE` and
`AIDEN_BENCHMARK_OPENCLAW_NODE` if they are not on the WebUI's `PATH`.
Personal pi configuration and sessions are not loaded. Model/tool-runtime costs
and preparation costs are not an all-in billing comparison. Use the same model
and comparable budgets when measuring harness differences; otherwise report
agent-plus-model operation performance.

## Setup and WebUI

1. Configure Aiden in the existing WebUI **Agent Config**. It is captured when
   the experiment starts; edits during execution do not change that experiment.
2. Install pi on the same host that runs the WebUI. Use
   `@earendil-works/pi-coding-agent` 0.85.0 or a version with the same
   print/JSON protocol, and pin the package version for repeatable comparisons. `pi --help` must expose
   `--mode json`, `--print`, `--no-session`, `--no-context-files`, `--tools`,
   `--no-tools`, and the resource discovery switches used in `runner/harness.py`.
3. Set the chosen provider's API key in the WebUI process environment, for
   example `OPENROUTER_API_KEY`. Custom API endpoints can instead use the explicit
   settings below. Personal pi OAuth files and `models.json` are deliberately
   not loaded. If pi is not on PATH, set
   `AIDEN_BENCHMARK_PI_EXECUTABLE` to the executable path before starting WebUI.
4. Start the normal WebUI from `benchmark/`:

   ```bash
   uv run python -m runner webui
   ```

5. Open **Agent experiments**, select the comparison suite and agents, enter the
   provider and exact model ID, and choose repetitions. External agents share
   the model settings in this UI. Aiden uses its model
   from Agent Config; the shared model field does not override Aiden. Select a
   running MobileGym environment from the selector, or click **Start MobileGym**
   to create one on this page. A ready environment is selected automatically when
   none is selected. Configure Judge in the main page Settings.
   Use **External URL (advanced)** only for a separately managed simulator.
6. Click **Save pi settings** to persist the model, endpoint, and credential
   variable name on the server. New tabs and page reloads restore these settings;
   starting an experiment also saves the current values. API keys remain in the
   server environment.
7. Start the experiment. The status panel above the form shows preparation,
   the current agent/task/repeat, elapsed time, completed attempts, and failures.
   **Setup log** remains available even when image preparation fails. Open its comparison report and follow an agent name
   to the existing detailed run report. Stop preserves completed attempts and
   cancels the active execution. Unstarted attempts remain `not_run`.

One experiment executes at a time per WebUI process. Agent order is shuffled
inside each task/repetition block using the scheduling seed. Blocks start in
schedule order; with `concurrency` set above 1 (at most 8), that many attempts
run at once. Each attempt owns a fresh daemon, tool gateway, and environment
route, and the MobileGym environment must have at least as many parallel
environments; launch fails otherwise. Concurrent attempts share the model
endpoint and host, so latency can differ from a sequential run. Results are
written in task order regardless of completion order. This seed controls
scheduling only; it is not a model sampling seed. Each attempt has a wall-clock
deadline from the suite. The tool-call limit is a scoring assertion, not a live
execution budget. Startup time is separate from Aiden execution time; pi and
OpenClaw timing includes CLI startup, and OpenClaw timing also includes MCP
bridge startup. The runner deadline is authoritative for every harness:
OpenClaw's own turn timeout is set slightly above it, so overruns are recorded
as timeouts rather than execution errors. Compare latency with this distinction
in mind.

## Custom pi API endpoints

In the pi panel, expand **Custom API endpoint (optional)** and set the base URL,
API protocol, and the **name** of an API key environment variable. Export the
actual key in the shell that starts the WebUI or CLI, for example
`AIDEN_BENCHMARK_PI_API_KEY`. Restart an existing WebUI after changing its
environment. Credentials are not submitted as part of the experiment payload.
For a keyless local server, set that variable to a non-secret placeholder if the
server accepts one; pi still requires configured authentication.

Supported protocol values are `openai-completions` (Chat Completions),
`openai-responses`, `anthropic-messages`, and `google-generative-ai`. Use the
backend's base URL, such as `https://gateway.example.com/v1`, rather than the
full `/chat/completions` request URL. URLs containing embedded credentials,
query strings, or fragments are rejected. The URL is reached by pi on the
benchmark host; `127.0.0.1` refers to that host, not the Aiden Docker container.
The Aiden model URL is configured independently in **Agent Config**.

`provider` may be a custom alias such as `benchmark-custom`. `model` must be the
exact identifier accepted by the backend. The runner generates a temporary
`models.json` in pi's isolated configuration directory and registers that
provider/model with the selected protocol. Operation tasks declare image input;
the actual model must support it. The file references the selected credential
through an environment variable instead of embedding the secret. Personal
`models.json`, OAuth sessions, custom headers, and arbitrary provider extensions
are not imported.

Set Thinking to `off` for backends without reasoning support. Other levels mark
the custom model as reasoning-capable and use pi's native protocol behavior.
Set `context_window` and `max_tokens` to match the backend. The defaults, 128000
and 16384 respectively, are pi configuration defaults, not detected capabilities
of your model. Both must be positive integers and max output cannot exceed the
context window. Custom endpoint pricing is unknown and appears as `—`, even if
pi emits its default zero-cost estimate; token usage remains available.

See `experiments.custom.example.json` for a complete operation comparison. The
new fields belong to the pi entry inside `agents`:

```json
{
  "id": "pi",
  "kind": "pi",
  "provider": "benchmark-custom",
  "model": "replace-with-exact-backend-model-id",
  "thinking": "off",
  "base_url": "https://gateway.example.com/v1",
  "api": "openai-completions",
  "api_key_env": "AIDEN_BENCHMARK_PI_API_KEY",
  "context_window": 128000,
  "max_tokens": 16384
}
```

Leaving the base URL unset preserves the built-in provider behavior. Custom
endpoint fields require a base URL; a raw `api_key` field is not accepted.

## CLI

Copy `experiments.example.json` and replace the pi model and environment URL placeholders, then run
from `benchmark/`:

```bash
uv run python -m runner experiment \
  --config experiments.example.json \
  --agent-config /absolute/path/to/agent.toml
```

Use `--no-build-daemon-image` to reuse an existing daemon image. Pi-only
experiments also require the shared tool runtime. Configuration files
contain model identifiers, not API keys. Keep credentials in the existing Aiden
configuration and provider environment variables.

CLI exit code 0 means all planned attempts were recorded; individual tasks may
have failed. Read `comparison.json` for quality results and coverage. Setup or
orchestration errors return nonzero; interruption returns 130.

## Artifacts and interpretation

Artifacts live below `<runs-dir>/experiments/<experiment-id>/`:

```text
manifest.json                  experiment identity, status, configuration, schedule
suite.json                     frozen suite, including expected answers
comparison.json                summaries and paired task/repetition cells
report.html                    comparison report
runs/<agent-id>/manifest.json   independent run metadata
runs/<agent-id>/results.jsonl   recorded attempts
runs/<agent-id>/report.html     detailed existing benchmark report
runs/<agent-id>/tasks/.../      normalized history, trace, and native evidence
```

Raw pi events and stderr are retained. OpenClaw retains its one-shot output
and native transcript when available. External harness attempts also retain
the gateway's `tool-audit.jsonl` and per-step screenshots. Pi tool
results are matched by call ID
before conversion to the existing scorer's adjacent call/result representation;
the raw stream preserves original ordering. Native tool names are retained.
Adapters never add task-solving hints or retry a failed task. Native agent
retries remain part of the measured execution.

Success is reported over eligible attempts together with coverage over planned
attempts. Adapter/infrastructure failures are recorded separately and cannot
inflate coverage. Missing attempts and canceled attempts are not successes.
The primary comparison uses matched task/repetition pairs where both agents
are eligible, with paired coverage. Latency is compared on pairs where both
passed. Individual run rates remain diagnostics; their denominators can differ.
The report separately counts timeouts, execution errors (which may overlap
timeouts), and exclusions. `comparison_policy=matched-eligible-v2` identifies
this reporting policy in new manifests; older artifacts are not rewritten.

Connection failures before any tool call, assistant text, or generated tokens
are excluded consistently. Empty, zero-usage assistant error/retry records do
not establish execution merely by increasing the LLM call count. Errors after
execution starts remain failures and are exposed separately; they do not prove
the harness caused the failure. Runner deadlines retain partial Aiden history
before daemon cleanup, just as pi retains its native event stream.

External harnesses receive explicit business failures as tool errors while
retaining the returned image and text. This translates the tool contract;
native image processing, context management, retry policy, and task deadlines
are unchanged.

Agent-driven preparation still runs independently for each attempt; it does
not restore a common prepared snapshot. The report identifies affected tasks.
`environment_tools` setup instead replays fixed bridge actions and checks its
`expected_state` before the attempt starts, so it is not counted as
agent-driven preparation.
Inspect starting screenshots before attributing a difference to a harness.
The existing `mobilegym_basic` (3 tasks), `mobilegym_scroll_regression` (2), and
`mobilegym_scroll_sweep` (100) avoid agent-driven preparation. They are useful
for baseline control and scroll reliability, but the sweep is one capability
family, not broad harness coverage. `phone_control_v1` covers more controls;
`app_workflow_v1` adds editing, persistence, publishing, navigation, and cross-app
workflows, but its apps and task fixtures must be validated in MobileGym first.
Do not treat suite acceptance as proof that these prerequisites exist.

Reported pi costs use the model's pricing estimates, not a billing receipt;
missing cost is unknown, and incomplete experiments do not show a misleading
complete cost total. Raw artifacts may contain task and model output; they are
local generated data and must not be committed.

Experiments use unique directories and never overwrite earlier results. The
server lists interrupted experiments after restart, preserving their artifacts.
Interrupted experiments are not resumed, and independently launched
experiments are not merged; start a new experiment to rerun, retaining the
original evidence.

Transient Judge failures are retried once after all agent attempts finish,
using their saved screenshots and tool traces without repeating device actions.
If the Judge is still unavailable, the experiment keeps `judge_error` attempts
out of paired coverage. Attempts whose final environment state could not be
read are never rescored, because their state assertions were not verified. Once the Judge recovers, use **Retry Judge** on the
experiment list to score only those attempts and refresh the run and comparison
reports. The first scoring results remain in each run's
`results.original.jsonl`; the main `results.jsonl` and reports show the latest
score. Retrying uses the frozen Judge model and endpoint from the experiment,
with the current WebUI Judge credential.

## OpenClaw model setup

Select OpenClaw in the experiment form and set an explicit custom API endpoint,
provider, model, API protocol, key environment variable name, and model token
limits. The configured key must be present in the WebUI process environment.
For a like-for-like comparison with pi, both consume these same model settings;
the OpenClaw run uses its own native model request and image handling. The
adapter writes a one-shot OpenClaw config with the credential referenced through
an environment variable; neither the key nor personal OpenClaw auth profiles
are copied into experiment artifacts. OpenClaw cost remains unknown for a custom
endpoint. No Gateway or channel delivery is used.

For CLI experiments, include an `openclaw` agent alongside `aiden` or `pi`:

```json
{
  "id": "openclaw", "kind": "openclaw", "provider": "benchmark-custom",
  "model": "replace-with-exact-backend-model-id", "thinking": "medium",
  "base_url": "https://gateway.example.com/v1", "api": "openai-completions",
  "api_key_env": "AIDEN_BENCHMARK_PI_API_KEY",
  "context_window": 128000, "max_tokens": 16384
}
```

Before task execution, the runner checks that the OpenClaw CLI exists and that
Node is a supported version. The CLI version is recorded in the manifest but
not enforced. Its
MCP bridge runs on the WebUI host and calls the same tool gateway used by pi.
OpenClaw may name the exposed tools with its MCP server prefix; scoring uses
the gateway audit, which records the original business tool names. Reports mark
adapter/setup errors as ineligible. Use a small smoke experiment before a full
suite to confirm the selected OpenClaw version exposes and calls the MCP tools.

## Claude Code and Codex setup

Install the CLIs on the WebUI host. The integration was verified with Claude
Code 2.1.308 and codex-cli 0.165.0. Set `AIDEN_BENCHMARK_CLAUDE_CODE_EXECUTABLE`
or `AIDEN_BENCHMARK_CODEX_EXECUTABLE` when `claude` or `codex` is not on `PATH`.
Both require an explicit custom endpoint and share the experiment's provider,
model, base URL, key variable, and thinking settings with the other external
agents. Their protocols are fixed: Claude Code uses `anthropic-messages` and
Codex uses `openai-responses`, so the endpoint must serve that protocol for the
selected model. Claude Code receives the base URL without a trailing `/v1`.

Each attempt runs with a scratch home and configuration directory. Personal
settings, credentials, sessions, `CLAUDE.md`, hooks, plugins, skills, and Codex
rules are not loaded; the key is passed only through the environment. Thinking
maps to Claude Code `--effort` (`off` and `minimal` become `low`) and to Codex
`model_reasoning_effort` (`off` becomes `none`). Codex runs with a read-only
sandbox and pre-approves only the benchmark MCP tools. Timing includes CLI and
MCP bridge startup, and cost is unknown for custom endpoints. Native calls that
either CLI reports are counted in `native_tool_calls`.

## CI policy

Deterministic adapter, scheduling, validation, cancellation, and aggregation
tests run in `make check` and the full CI benchmark suite. An opt-in test in
`tests/test_openclaw.py` runs an installed OpenClaw against a local fake model
and checks that it offers only the business tools; set
`AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE` and `AIDEN_BENCHMARK_OPENCLAW_NODE` to
enable it. Similar opt-in tests in `tests/test_harnesses.py` run installed
Claude Code or Codex against a local fake model that calls a business tool
through the gateway; set `AIDEN_BENCHMARK_CLAUDE_CODE_EXECUTABLE` or
`AIDEN_BENCHMARK_CODEX_EXECUTABLE`. Operation lifecycle,
state-based scoring and the real JavaScript tool extension are tested without
paid model calls. They use fake agents
and subprocesses: no installed pi, paid model, live phone, or provider key is
required. HTTP/UI integration tests run in the full benchmark suite.

Real-model comparisons run manually through WebUI, CLI, or the **Agent
Experiment** workflow (`.github/workflows/agent-experiment.yml`). They are not a
per-PR merge gate: provider availability, model variability, latency, and cost
are external factors.

The workflow is dispatched with a suite, a comma-separated agent list such as
`aiden,pi,openclaw,codex,pi-raw` (append `-raw` to an external agent for raw
tools), a repeat count, and a concurrency that starts that many MobileGym
environments (default 3). It runs on the benchmark runner, builds the daemon
and MobileGym images as the benchmark workflow does, and installs pinned
external CLI versions into the job workspace. Every agent uses the repository's
benchmark model (`BENCHMARK_AGENT_PROVIDER`, `BENCHMARK_AGENT_MODEL`,
`BENCHMARK_AGENT_BASE_URL`, and `BENCHMARK_AGENT_API_KEY`), and the Judge uses
the benchmark Judge settings. Claude Code and Codex need an endpoint and model
that serve their protocols. The job summary shows per-agent results and matched
pairs; reports and results are uploaded as an artifact.

# Agent operation experiments

The benchmark supports one experiment containing separate Aiden, pi, and OpenClaw runs.
Use **Agent experiments** in the WebUI header, or open `/experiments`. Existing
device benchmark jobs and Aiden regression suites keep their existing workflow.

## Existing suite comparison

Experiments reuse the existing suite files without copying or weakening tasks.
Start with `mobilegym_basic.json`, `mobilegym_scroll_regression.json`, or
`mobilegym_scroll_sweep.json`. There is no separate calculation or harness suite.
Each experiment freezes the original suite and runs one independent run per
agent, interleaving task/repetition blocks sequentially against the same
MobileGym environment. The experiment repeat count overrides task repeats for
both agents. Prompt prefixes, task prompts, rubric, tool assertions, state
assertions, app selection, and deadlines retain their original meaning. For
example, the original scroll regression still rejects overshooting item 24/83.

Rubric tasks require the existing Judge. WebUI freezes its enabled/model/base
URL/credential settings at experiment creation; CLI uses `--judge-model`,
`--judge-base-url`, and `AIDEN_BENCHMARK_JUDGE_API_KEY`. Missing or disabled Judge
configuration prevents launch. Judge errors are ineligible attempts, never
successes. Both agents use the same judge, original rubric, before/after
screenshots, tool execution evidence, and final response. Deterministic state
assertions remain additional requirements.

The shared business tools are `screenshot`, `touch_gesture`, `keyboard_tap`,
`enter_text`, `mouse_move`, `mouse_scroll`, and `wait_for_stable_screen`. Their
schemas and execution runtime are identical. Each attempt records its catalog
hash; mismatches are ineligible. Pi's explicit extension calls the daemon tool
API, forwards images as native image content, and retains actual execution
inputs and errors for the existing scorer. It never delegates the task to
Aiden's chat API. Native planning, context, retry, and memory behavior stay
agent-owned. Each measured attempt starts cold.
OpenClaw receives the same frozen tool catalog through a per-attempt stdio MCP
server. Its `agent exec` owns the agent loop and native image/context behavior;
the bridge records each actual business-tool request and result for scoring and
saves step screenshots. OpenClaw's normal workspace, personal sessions, and
ambient configuration are not loaded. The comparison tool policy exposes only
the named MCP business tools, not filesystem, shell, or browser tools.

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
host with isolated configuration and workspace, which is not a shell sandbox.
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
2. Install pi on the same host that runs the WebUI. Integration was developed
   against `@earendil-works/pi-coding-agent` 0.85.0 and its print/JSON protocol.
   Pin the package version for repeatable comparisons. `pi --help` must expose
   `--mode json`, `--print`, `--no-session`, `--no-context-files`, and the resource
   discovery switches used in `runner/harness.py`.
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
   provider and exact model ID, and choose repetitions. pi and OpenClaw share
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
inside each task/repetition block using the scheduling seed. This seed controls
scheduling only; it is not a model sampling seed. Each attempt has a wall-clock
deadline from the suite. The tool-call limit is a scoring assertion, not a live
execution budget. Startup time is separate from Aiden execution time; pi timing
currently includes CLI startup. Compare latency with this distinction in mind.

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

Raw pi events and stderr are retained. OpenClaw retains its one-shot output,
native transcript when available, MCP audit, and per-step screenshots. Tool
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

Pi receives explicit business failures (`ok:false`, including screenshot-wrapped
`action_output`) as tool errors while retaining the returned image and text.
This translates the tool contract; native image processing, context management,
retry policy, and task deadlines are unchanged.

Agent-driven preparation still runs independently for each attempt; it does
not restore a common prepared snapshot. The report identifies affected tasks.
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
Automatic resume and merging independently launched experiments are not yet
implemented; start a new experiment to rerun, retaining the original evidence.

Transient Judge failures are retried once after all agent attempts finish,
using their saved screenshots and tool traces without repeating device actions.
If the Judge is still unavailable, the experiment keeps `judge_error` attempts
out of paired coverage. Once the Judge recovers, use **Retry Judge** on the
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
  "id": "openclaw", "kind": "openclaw", "provider": "apibest",
  "model": "gpt-5.6-sol", "thinking": "medium",
  "base_url": "https://apibest.ai/v1", "api": "openai-completions",
  "api_key_env": "AIDEN_BENCHMARK_PI_API_KEY",
  "context_window": 1050000, "max_tokens": 16384
}
```

The OpenClaw CLI and a compatible Node are checked before task execution. Its
MCP bridge runs on the WebUI host and calls the same benchmark daemon tool API
used by pi. OpenClaw may name the exposed tools with its MCP server prefix; the
scorer maps observed calls to the original business tool names. Reports mark
adapter/setup errors as ineligible. Use a small smoke experiment before a full
suite to confirm the selected OpenClaw version exposes and calls the MCP tools.

## CI policy

Deterministic adapter, scheduling, validation, cancellation, and aggregation
tests run in `make check` and the full CI benchmark suite. Operation lifecycle,
state-based scoring and the real JavaScript tool extension are tested without
paid model calls. They use fake agents
and subprocesses: no installed pi, paid model, live phone, or provider key is
required. HTTP/UI integration tests run in the full benchmark suite.

Real-model comparisons are manual through WebUI or CLI initially. Do not make
them a per-PR merge gate: provider availability, model variability, latency, and
cost are external factors. A later opt-in nightly or manually dispatched job can
run a pinned configuration, retain artifacts, and publish trends using the
existing operation suites. Such a job should distinguish infrastructure
failures from agent regressions before considering any release gate.

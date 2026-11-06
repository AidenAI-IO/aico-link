---
sidebar_position: 2
---

# Benchmark Architecture

Benchmark uses an HTTP API-driven execution model plus offline scoring. Task
execution happens through the real agent and an optional environment bridge;
scoring can be re-run from saved artifacts.

## Core Components

```text
┌─────────────┐
│   Runner    │  CLI or WebUI worker
└──────┬──────┘
       │ /api/chat, /api/history
       ▼
┌─────────────┐
│ Go Agent    │  Existing daemon or isolated Docker worker
└──────┬──────┘
       │ /api/tools/<tool> when environment bridge mode is enabled
       ▼
┌────────────────────┐
│ Environment Bridge │  Go device bridge, MobileGym/ADB bridge, desktop bridge, or custom bridge
└────────────────────┘
```

The environment bridge contract is defined in
[`benchmark/environment_bridge.md`](../../benchmark/environment_bridge.md).

## Task Flow

For each task, `runner/runtask.py` performs:

1. Prepare task isolation and clear agent conversation history.
2. If `environment_url` is set, call bridge `/api/setup` with `benchmark-task-id`.
3. Run optional suite/task setup.
4. Capture `pre.jpg` directly from bridge `POST /api/providers/screenshot`.
5. Send the task prompt to agent `/api/chat`.
6. Capture `post.jpg` directly from bridge `POST /api/providers/screenshot`.
7. Extract structured tool trace from agent history.
8. Evaluate any `scope: "precondition"` rubric items against the pre-screenshot alone. An invalid initial state excludes the attempt from Agent scores; unavailable evidence is an evaluation error. These checks happen during scoring, after execution, and do not prevent the task from running.
9. Evaluate hard assertions, then submit outcome rubric, trace, final response, and pre/post screenshots to the judge when enabled.
10. Persist task artifacts and release the bridge route through `/api/release`.

The runner does not call agent screenshot tools to create judge images. Judge
only receives `pre.jpg` and `post.jpg`; intermediate screenshots may exist in
agent history, but they are not the judge image input.

A task's `setup` may use `environment_tools` to seed the same UI state before
each evaluated harness without an agent. The setup lists bridge tools with
`name` and `input` and may include `expected_state` paths. With
`expected_state`, the runner confirms that the environment reports state before
sending input, then checks tool errors and the expected state before capturing
`pre.jpg`; failed preparation does not become an agent score. State paths can
address list elements with numeric segments, such as
`apps.notes.notes.0.content`. The phone-control `select_all_and_delete` task
uses MobileGym coordinates and declares `environment_bridge_types: ["mobilegym"]`.
Other bridges, including ADB Android, skip it before setup.

## Evaluation contract audit

The suite audit covers the task definitions under `benchmark/suites`, including
manually selected hardware suites. Reading a definition does not certify its
real-device behavior. Apply these rules when adding or changing tasks:

- Setup calls are not part of the final request's tool trace. Check evaluated
  setup behavior at that step, or supply its captured state as explicit evidence.
  Never ask a final-request tool-count assertion to count earlier setup calls.
- Initial-screen requirements belong to `scope: "precondition"`, not the
  Agent outcome score. Mixed checks about the starting screen and the Agent's
  subsequent action remain outcome checks until separated explicitly.
- Check persisted memory when testing persistence. A correct answer or a save
  attempt alone does not prove that storage changed. Keep structural checks
  deterministic and judge generated content semantically. Notification update
  and promotion checks use post-task memory evidence; reinforcement retains its
  original-content check because it must preserve the seeded conclusion.
- Exact copied identifiers, requested literal text, coordinate bounds, and
  procedure steps reconstructed from source events retain exact checks. Do not
  relax those contracts to increase the pass rate.
- A successful copy requires evidence of the copied value, not shortcut delivery.
  `quick_action_copy_and_verify` copies selected source text and pastes into a
  different initially empty field. Its precondition checks the source selection
  and empty destination; its outcome checks the copy/paste trace and visible
  destination text. This works without a clipboard-read tool on macOS. The new
  task ID separates it from the old copy-only workload. Unchanged source text
  proves nothing. A clear-all operation need not include multiple swipes.

Known evidence gaps remain in manually selected suites. The forwarded-environment
context tasks in `aico_app/connection_capabilities_v1.json` ask the judge to
validate runtime context that is not independently attached to its input. Several
hardware setups still rely on Agent preparation without a separate initial-state
check. Those tasks need captured context or device-state contracts before their
scores can support a readiness claim. The device-information preparation recipe
also still falls back to Agent setup on the tested emulator.

Benchmark CI archives `pre.jpg` and `post.jpg` alongside task traces so starting
state and outcome verdicts can be audited. Precondition verdicts remain separate
from outcome rubric counts in metrics and reports; excluded attempts must still
be reviewed through coverage, not silently treated as successes.

## Environment Bridge Routing

Concurrent environments route requests by the `benchmark-task-id` HTTP header.
The same id must be used for:

- `/api/setup`
- `/api/tools/<tool>`
- `/api/providers/screenshot`
- `/api/release`

WebUI and `run --auto-agent-setup` create one isolated agent daemon per active
task worker. The scheduler reads `/api/concurrent` from the bridge and runs at
most that many workers at once; extra tasks wait in the queue until a worker
releases its env.

## Scoring

### Offline metrics

Each attempt records `success`, `agent_eligible`, `failure_class`, and
`quality_score` in the machine-readable run artifacts. Fixture preparation failures,
unavailable providers, judge failures, and deliberate skips remain in raw
results and coverage counts but do not enter Agent success or efficiency
denominators. Missing telemetry is represented as `null`, not zero.

When setup invokes the capability being tested, mark that individual
`seed_episode`, `seed_notification`, or `assert_memory` step with `"evaluate": true`.
Consolidating seed steps must also set `"consolidate": true`. A behavior assertion
or explicit semantic processing error in that step becomes an eligible Agent
failure, with stage `setup`. Ordinary seed, connection, timeout, and evidence-read
errors remain excluded. The default is false, so assertions that only validate
fixtures do not penalize the Agent. Place the marker on each evaluated step; it
does not carry forward to subsequent steps in a sequence.

`setup_failure.json` records the failing step and available observed memory or
consolidation evidence. Sequence wrapping preserves this evidence. Successful
setup still proceeds to the normal task and its assertions; a step passing on its
own does not make the task pass. Historical results retain their recorded
eligibility and must be rerun to adopt the new evaluation boundary.

The notification reinforcement suite verifies deduplication with post-task
`memory_assertions`, including record count, identity, revision, and source
references. Its language rubric checks the reported meeting time. Repeated TTS
and display text does not imply duplicated storage. Static perception fixtures
accept well-formed normalized taps anywhere on the image; target correctness
remains a separate first-attempt assertion, not a mock execution result.


For a fixed planned repeat count `k` (the common planned prefix, normally the
smallest repeat count requested by the tasks), the runner considers only tasks
whose first `k` planned attempts are all present and eligible. It reports:

- `pass@1`: the fraction of eligible tasks whose first attempt succeeds;
- `pass@k`: the fraction with at least one success among those first `k`
  planned attempts;
- `pass^k`: the fraction whose first `k` planned attempts are all successful;
- `oracle_best_score@k`: the mean, across tasks with a recorded score in that
  cohort, of each task's maximum offline quality score within the first `k`
  attempts; and
- first-success attempt and cumulative cost-to-first-success, using tasks with
  a contiguous eligible attempt prefix.

`pass@1` uses tasks with an eligible first attempt. `pass@k`, `pass^k`, and
`oracle_best_score@k` use only the cohort whose first `k` planned attempts are
all present and eligible. First-success metrics use their own contiguous
eligible-prefix cohort rather than silently skipping invalid attempts.

Success rates include 95% Wilson confidence intervals, and eligible-task
coverage is reported separately. The metrics are recomputable from the raw
attempt results and are written to `metrics.json` alongside `summary.md`.

### Hard Assertions

Hard assertions run before LLM judge and are deterministic. They cover checks
such as:

- Required final response.
- Minimum/maximum tool calls.
- Required or forbidden tools.
- Timeout.
- Expected answer for deterministic QA tasks.
- Trace observation checks.

Hard assertion failures include the requirement and actual observed value in
the HTML report.

### LLM Judge

The judge uses OpenAI-compatible chat completions. Its input is:

- Task description.
- Rubric.
- Pre/post screenshots, when available.
- Structured tool trace.
- Agent final response.

The output is one yes/no verdict plus reason per rubric item. Results are
cached by screenshots, trace, rubric, final response, and model.

Agent, Judge, and Analysis runtime configuration use separate role-specific
namespaces: `AICO_BENCHMARK_AGENT_*`, `AICO_BENCHMARK_JUDGE_*`, and
`AICO_BENCHMARK_ANALYSIS_*`. Provider-specific environment variable names are
not part of the benchmark runtime contract.

## Artifacts

CLI run output:

```text
benchmark/runs/<run_id>/
├── manifest.json
├── results.jsonl
├── summary.md
├── report.html
├── _judge_cache/
└── tasks/<task_id>/
    ├── pre.jpg
    ├── post.jpg
    ├── history.json
    ├── trace.json
    └── judge.json
```

WebUI job output:

```text
benchmark/runs/webui/<job_id>/
├── job.json
├── state.json
├── runner.log
├── daemon.log
├── raw/<run_id>/
└── workers/
```

WebUI persists `job.json`, so historical job records survive WebUI restarts.
If a job was running during restart, it is recovered as stopped.

## Design Decisions

### Why HTTP APIs?

- Agent daemon already exposes HTTP endpoints.
- Runner can execute against local or remote agents.
- Environment bridge lets real devices, MobileGym, and future environments use
  the same scheduling and artifact pipeline.

### Why Separate Execution And Scoring?

- Rubric and judge model changes can be rejudged offline.
- Judge failures do not require re-running device actions.
- Cached judge results reduce API cost.

### Why Isolated Daemon Workers?

For concurrent environment runs, each active task worker gets its own agent
daemon and config directory. This avoids conversation, memory, and log
cross-talk while still sharing the same bridge pool.

## Related Documentation

- [Quick Start](./README.md)
- [Detailed Guide](./quickstart.md)
- [Full Manual](../../benchmark/manual.md)

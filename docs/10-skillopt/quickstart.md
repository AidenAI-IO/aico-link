---
sidebar_position: 1
---

# SkillOpt Quickstart

This page covers the shortest path to run SkillOpt. For internals, see
[Architecture](./architecture.md). For task execution details, see the
[Benchmark docs](../09-benchmark/README.md).

## Prerequisites

1. Install Python dependencies from the repo root:

   ```bash
   uv sync --project skillopt
   uv sync --project benchmark
   ```

2. Configure an optimizer and judge API key:

   ```bash
   export OPENROUTER_API_KEY=...
   ```

3. Make sure the skill exists:

   ```text
   src/agent/config/skills/device-operator/SKILL.md
   ```

4. For MobileGym or bridge-backed runs, start or select an environment bridge.

## WebUI

Start the SkillOpt WebUI from the repo root:

```bash
uv run --project skillopt python -m skillopt webui --host 127.0.0.1 --port 8766
```

Open `http://127.0.0.1:8766`.

Recommended WebUI flow:

1. Select a target from the `Targets` table.
2. Save an agent config with a non-empty model API key.
3. Start or select a MobileGym environment.
4. Set judge model, max iterations, max edits per iteration, and `min_delta`.
5. Run the job and open the `Report` link when it appears.

The WebUI stores jobs under `skillopt/runs/webui/<job_id>/`. While a job runs,
the progress area follows SkillOpt's own phase records, including the active
phase, task counts, failed task IDs, best score, and whether the top-level
report is available. Historical and failed jobs only show a report link when
`report.html` exists.

## MobileGym CLI Run

Start a MobileGym bridge from the benchmark project:

```bash
cd benchmark
uv run python -m runner start-mobilegym-env --envs 5 --bridge-port 19090
```

In another shell from the repo root, run SkillOpt through the benchmark runner
backend:

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend mobilegym \
  --environment-url http://127.0.0.1:19090 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --edit-budget 2 \
  --min-delta 0.03 \
  --no-build-daemon-image \
  --output /tmp/device-operator-best.md
```

MobileGym runs use isolated benchmark daemon workers and route each task through
the environment bridge with a stable `benchmark-task-id`. The CLI logs
`Max iterations`, `Max edits / iteration`, and `min_delta` before launching the
optimization loop.

## Existing Device Daemon Run

If an Aiden agent daemon is already running, SkillOpt can use it directly:

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend device \
  --agent-url http://127.0.0.1:8080 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --output /tmp/device-operator-best.md
```

This mode temporarily overrides the target skill during rollout and reloads the
agent skill registry. Use bridge-backed runs when you need pre/post screenshots,
isolated daemon workers, or MobileGym concurrency.

## Bridge-Backed Physical Device Run

For physical devices exposed through an environment bridge, keep `--backend
device` but provide `--environment-url`:

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend device \
  --environment-url http://127.0.0.1:19090 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --output /tmp/device-operator-best.md
```

When `--environment-url` is set, SkillOpt uses the benchmark runner backend even
for `--backend device`.

## Dry Run And Review

Print the proposed diff without writing the output file or web artifacts:

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend device \
  --agent-url http://127.0.0.1:8080 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 1 \
  --dry-run
```

After a normal run, review before applying:

```bash
diff src/agent/config/skills/device-operator/SKILL.md /tmp/device-operator-best.md
```

To inspect artifacts:

```bash
open skillopt/runs/<run_id>/report.html
```

The top-level SkillOpt report is separate from child benchmark reports. Use the
SkillOpt report to see the optimization timeline, accepted/rejected candidates,
best score, failure reason, artifacts, and diff. Use each phase's `report`
drilldown for task-level benchmark evidence.

## Common Options

| Option | Meaning |
| --- | --- |
| `--skill` | Skill directory name under `src/agent/config/skills`. |
| `--suite` | One suite to split 70/30 into train and selection tasks. |
| `--train-suite` | Explicit train suite label. |
| `--validation-suite` | Explicit held-out validation suite label. |
| `--budget` | Maximum optimization iterations. |
| `--edit-budget` | Maximum skill edits proposed per optimization iteration. |
| `--min-delta` | Required hard-score improvement to accept a candidate. |
| `--optimizer-model` | OpenRouter model for reflection and patch generation. |
| `--judge-model` | OpenRouter model for benchmark rubric judge. |
| `--no-judge` | Skip LLM judge and use hard assertions only. |
| `--sample-classifier` | `rules` (default) or `typesafe`: let a confident TypeSafe Jev answer override the rule-based sample quality. Needs `TYPESAFE_API_KEY`; the rule result is kept as `sample_quality_rule`. |
| `--environment-url` | Environment bridge endpoint for benchmark-backed rollouts. |
| `--agent-config` | Agent config passed to benchmark daemon workers. |
| `--artifact-root` | Root directory for SkillOpt run artifacts. |
| `--run-id` | Stable run id, useful for reproducible scripts. |

## Troubleshooting

`mobilegym backend requires environment_url`

Start a MobileGym bridge or pass the endpoint from the WebUI environment table.

`missing env var OPENROUTER_API_KEY`

Set `OPENROUTER_API_KEY` or provide an agent config whose model API key can be
resolved by the shared runner config helpers.

`skill not found`

SkillOpt looks under `AIDEN_SKILLS_DIR`, then
`src/agent/config/skills/<skill>/SKILL.md`, then `skills/<skill>/SKILL.md`.

Judge model returns HTTP 403 or region errors

Change `--judge-model` to a model available in the current OpenRouter account
and region. A judge error does not mean the task executed incorrectly.

Validation looks too good with `--no-judge`

`--no-judge` only runs hard assertions. It is useful for trace collection, but
it can produce false positives for rubric-heavy UI tasks.

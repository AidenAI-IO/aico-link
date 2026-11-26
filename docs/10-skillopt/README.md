---
sidebar_label: Overview
sidebar_position: 0
---

# SkillOpt

SkillOpt is a developer workflow for improving Agent `SKILL.md`
files through repeated rollout, reflection, patching, and held-out validation.
It optimizes skill text, not model weights or benchmark suite definitions.

The dependency direction is intentionally one-way:

```text
SkillOpt -> benchmark runner APIs -> environment bridge -> device or MobileGym
```

Benchmark stays generic: it runs suites and produces task reports. SkillOpt uses
those runner capabilities as rollout backends, then decides whether a candidate
skill should be accepted.

## Start Here

- [Quickstart](./quickstart.md) - WebUI, CLI, MobileGym, and common commands.
- [Architecture](./architecture.md) - Optimization loop, backends, scoring, and artifacts.
- [Benchmark docs](../09-benchmark/README.md) - Runner, bridge, judge, and report details.

## Recommended Entry Points

### WebUI

Use the standalone SkillOpt WebUI for day-to-day optimization runs:

```bash
uv run --project skillopt python -m skillopt webui --host 127.0.0.1 --port 8766
```

Open `http://127.0.0.1:8766`.

The SkillOpt WebUI can discover targets from `skillopt/suites`, manage
MobileGym environments, run optimization jobs, show the current phase and task
progress, surface the best score, and open the generated SkillOpt report.

### CLI

Use the CLI for scripted runs and debugging:

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend mobilegym \
  --environment-url http://127.0.0.1:19090 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --edit-budget 2 \
  --output /tmp/device-operator-best.md
```

Review before applying:

```bash
diff src/agent/config/skills/device-operator/SKILL.md /tmp/device-operator-best.md
```

## What SkillOpt Produces

Each run writes a self-contained artifact directory under `skillopt/runs/` or
`skillopt/runs/webui/`:

```text
skillopt/runs/<run_id>/
|-- manifest.json
|-- result.json
|-- report.html
|-- best_skill.md
|-- diff.patch
|-- phases/
|   |-- baseline_selection.json
|   |-- step_01_train.json
|   `-- step_01_selection.json
|-- step_01/
|   |-- candidate.md
|   |-- patch.json
|   |-- patch_reports.json
|   `-- decision.json
|-- logs/
`-- benchmark/<run_id>-<phase>/
```

`report.html` is the top-level SkillOpt report and is written for both
successful and failed CLI/WebUI runs when artifacts are enabled. It answers
which optimizer phase ran, why the run stopped, whether the skill improved, and
which candidate was accepted. `best_skill.md` is the accepted best skill text
when available, and `diff.patch` shows how it differs from the original skill.

`phases/*.json` are SkillOpt-owned phase records used by the WebUI and report
to show queued, running, passed, failed, skipped, judge-error, and timeout task
counts. Child benchmark reports remain available through `report` drilldown
links for task-level evidence.

## Boundary With Benchmark

- Benchmark WebUI lists benchmark suites only.
- SkillOpt WebUI is a separate process and defaults to port `8766`.
- SkillOpt-owned suites live under `skillopt/suites`.
- Bridge-backed SkillOpt runs use benchmark runner child runs.
- Benchmark reports are evidence drilldowns; SkillOpt reports decide which skill
  text won and where the optimizer stopped.
- `no_judge` runs can be useful for traces, but they are not reliable validation.

## Current Targets

The current repository includes one SkillOpt target family:

```text
skillopt/suites/device-operator/
|-- device_operator_train.json
|-- device_operator_verification.json
|-- shopping_scenario_train.json
`-- shopping_scenario_verification.json
```

Suite labels passed to the CLI omit `suites/` and `.json`, for example
`skillopt/device-operator/device_operator_train`.

## Related Documentation

- [Quickstart](./quickstart.md)
- [Architecture](./architecture.md)
- [Benchmark Quick Start](../09-benchmark/README.md)
- [Benchmark Architecture](../09-benchmark/architecture.md)
- [Environment Bridge Protocol](../../benchmark/environment_bridge.md)

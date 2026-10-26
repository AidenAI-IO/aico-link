# Custom Test Suites

Aico benchmark suites use `benchmark/suites/<name>.json`. When MobileGym runs as an
environment bridge, the benchmark WebUI lists these Aico JSON suites and uses
`benchmark-task-id` to route concurrent task workers to different environments within
the same MobileGym instance. The benchmark runner does not currently load YAML files
from this directory.

## Currently supported workflows

### Option 1: Concurrency through the Benchmark WebUI

```bash
cd benchmark
uv run python -m runner webui
```

When creating a MobileGym environment, set `Envs` to a value greater than 1, then select
that environment to run an Aico JSON suite. The WebUI starts a separate daemon for each
concurrent task worker and uses `benchmark-task-id` to route workers to different
environments within the same MobileGym instance.

### Option 2: Benchmark CLI services

```bash
cd benchmark
uv run python -m runner start-mobilegym-env --envs 5
uv run python -m runner start-agent-daemon --environment-bridge-endpoint http://127.0.0.1:<bridge-port>
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --agent-url http://127.0.0.1:<agent-port> \
  --environment-url http://127.0.0.1:<bridge-port>
```

## YAML format (retained for a future loader)

```yaml
name: suite_name
description: Suite description
tasks:
  - task.id.1
  - task.id.2
```

## Built-in suites

See the MobileGym registry: `account, alipay, bilibili, calendar, clock, crossapp_*, ebay, file_manager, launcher, map, notes, payment, railway12306, redbook, reddit, sms, spotify, tencent_meeting, weather, wechat, wechat_reading, x`

## Example files

- `aico_smoke.yaml` — a historical example; **it cannot currently be run directly with `--suite aico_smoke`** and is provided only as a task-list reference.

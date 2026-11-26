---
sidebar_label: 概览
sidebar_position: 0
---

# Agent 基准测试

Agent 基准测试用于评估 Aico Go Agent 在手机 UI、记忆、规划、感知和环境控制任务上的表现。
当前推荐的入口有：

- WebUI：用于日常运行、MobileGym 并发、任务级日志/屏幕以及报告。
- CLI：用于脚本化运行、单套件调试、重新评判和比较。
- Agent 实验：在相同的操作套件上比较 Aiden、pi、OpenClaw、Claude Code 和 Codex；
  见 [`benchmark/experiments.md`](../../benchmark/experiments.md)。

完整手册见 [`benchmark/manual.md`](../../benchmark/manual.md)。
字段定义、单位、分母以及缺失值语义，见[指标参考](./metrics-reference.md)。

## 快速开始

### 专用 Agent

先启动一个基准测试守护进程。`start-agent-daemon` 会在 `runs/cli-services` 下创建专用的配置目录，
只从 `config` 复制静态配置，并以全新的记忆和其他运行时状态启动守护进程。复用普通的 Agent
守护进程会复用它的持久化状态。

```bash
cd benchmark
uv sync

uv run python -m runner start-agent-daemon \
  --name memory-quickstart \
  --port 18081 \
  --base-config-dir config

uv run python -m runner run \
  --suite suites/memory_v1.json \
  --agent-url http://127.0.0.1:18081
```

运行完成后，使用 `start-agent-daemon` 打印出的 `stop_command` 停止守护进程。

对于通知记忆，请带着基准测试 token 针对一个全新的守护进程运行专用套件。它会注入确定性的原始通知 fixture，
通过真实的通知处理器处理它们，并检查临时记忆的召回、OTP/营销过滤以及多批次游标排空：

```bash
uv run python -m runner run \
  --suite suites/notification_memory_v1.json \
  --auto-agent-setup \
  --agent-config /path/to/agent.toml
```

该运行启用了 Judge，以便对有用通知的回答和噪声过滤行为进行效果评分。如果只想检查确定性关卡
（通过基准测试端点持久化 fixture、游标推进、记忆数量/范围以及必需/禁止的工具调用），请加上 `--no-judge`。
原始 JSONL 的保留由 Go 集成测试覆盖，而不是由 Agent shell 任务覆盖。对应的 Go 性能基准测试见完整手册。

通知套件还会预加载已有的临时记忆，并验证外部可观察的 `update`、`reinforce`、`remove` 和 `promote` 路径。
它使用有序的通用 setup 原语和 `assert_memory`；runner 中不包含特定于某种操作的通知逻辑。
来源/证据引用检查使用通用的 `source_refs_contain`/`evidence_refs_contain` 断言字段。

### WebUI

```bash
cd benchmark
uv run python -m runner webui
```

打开：

```text
http://127.0.0.1:8765
```

WebUI 可以启动 MobileGym 环境、从 `/api/concurrent` 读取桥接并发数、启动隔离的 Agent 守护进程 worker，
并显示任务级的屏幕/日志记录。

### 通过 CLI 运行 MobileGym

```bash
cd benchmark
uv run python -m runner start-mobilegym-env --envs 5 --bridge-port 19090

uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --environment-url http://127.0.0.1:19090 \
  --auto-agent-setup
```

请传入 `start-mobilegym-env` 输出的 `environment_url`。在 macOS 上它是
`http://127.0.0.1:<port>`；在原生 Linux 上，桥接绑定的是 Docker 网桥网关，而不是 `127.0.0.1`。

`--auto-agent-setup` 会忽略 `--agent-url`，从 `/api/concurrent` 读取桥接容量，
并为每个活跃的任务 worker 启动一个隔离的 Agent 守护进程。

### 无需手机的配套 App 策略测试

使用套件级默认值或任务级 `mock_environment` fixture，进行确定性的 Phone Bridge 策略测试。
runner 会模拟平台/App 状态和工具结果，因此通讯录及类似的 App 侧数据无需物理手机或模拟器。
任务级 fixture 让一个套件就能容纳一个运行时策略矩阵，而不必为每个用例创建一个 JSON 文件：

在 WebUI 中，选择一个或多个 mock 套件并点击 `Run selected suites`。运行配置会切换为
`Mock Aico companion app environment`，跳过设备选择器，并自动启动任务级 fixture。
mock 套件和外部设备套件必须作为不同的作业分开运行。

```bash
cd benchmark
uv run python -m runner run \
  --suite suites/aico_app/phone_bridge_data_policy_v1.json \
  --auto-agent-setup \
  --no-judge \
  --verbose
```

Aico 配套 App 的用例被合并为两个套件：

- `notes_entry_policy_v1.json`：备忘录已打开、其图标可见，或两者都不可见。Agent 分别应直接输入文本、
  点击可见的图标，或使用 `open_app`。
- `phone_bridge_data_policy_v1.json`：通讯录、日历查询/创建、剪贴板读/写，以及针对 iOS
  灵动岛恢复、iOS PiP 和 Android FGS 的通知用例。

脚本化套件以 `--benchmark-mock-tools` 运行其隔离的 worker，这需要基准测试 token 和环境桥接。
工具描述、schema、可用性过滤以及操作后的截图验证都保持不变；只有设备工具的执行会被发送到 fixture 的
`/api/tools/<name>` 端点。mock 会提供原生的 `/api/phone-bridge/status` 对象，因此 PiP、FGS 和灵动岛能力
反映的是当前激活的 fixture。启用了 FGS 的 fixture 会报告一个新鲜的心跳，除非它显式设置了
`fgs_bridge_updated_at`。mock 策略运行不验证 App 内部的恢复、粘贴或命令传输机制。

这些 mock fixture 不模拟 `ble_service`，也不模拟在线的 iOS Wake 订阅者，因此不覆盖实时的
BLE Wake 能力发现或投递。平台和 Phone Bridge 的可用性过滤仍使用脚本化的状态。

没有环境桥接的无头隔离容器会在守护进程之前启动 `benchmark-mcu`，并将 `AICO_MCU_SERVICE_SOCKET` 指向它。
它只模拟真实 `mcu_service` 代码背后的 MCU 固件对端，因此与在设备上一样，`schedule` 工具会设置和取消真实的倒计时，
到期的例行任务会收到 RTC 唤醒。电量读取返回固定值。不支持 LED，挂起和关机请求会被拒绝。
桥接、MobileGym 和 ADB 环境不会启动它。如果模拟器退出，入口脚本会重启它；如果它无法启动，
守护进程会在没有 MCU 服务的情况下运行，而不是让每个任务都失败。

由于 socket 存在，无头守护进程会走 MCU 唤醒路径，而不是报告“socket unavailable”；`schedule`
会设置真实的倒计时，而不是返回 socket 错误。与定时任务相关的任务结果无法与此变更之前的每日运行结果进行比较。

对于没有 PiP 的 iOS 后台场景，可达的灵动岛返回入口会让数据工具保持可见。Agent 直接调用所请求的
`bridge_*` 工具；该工具会在执行前在内部恢复 Aico 配套 App，因此 Agent 不得点按灵动岛或调用
`bridge_open_app`。启用 iOS PiP 或 Android FGS 时，可在后台安全执行的数据工具会直接通过后台队列执行。
`bridge_open_app` 仍被排除，因为 PiP/FGS 不提供在后台启动 App 的能力。

备忘录用例使用固定的 `bridge_contacts` 结果，并要求使用 `enter_text`，而不单独调用
`bridge_clipboard`。生成的屏幕保留用于 runner 的操作前/后产物和 fixture 状态转换；
策略测试不要求 Agent 检查它。脚本化的 `screen_contains` 前置条件可防止在 fixture
实际进入备忘录编辑器之前就输入文本。

mock 检查的是 Agent 是否选择了 `enter_text`；它不会运行该工具内部的粘贴回退。
在真实的 Go 工具中，顺序是：Phone Bridge 剪贴板、`quick_action` 粘贴、操作出错时直接用键盘粘贴、
视觉验证，然后如果快捷方式没有可见效果，再长按“粘贴”。普通的逐字输入回退属于 `enter_text`。

以下内容请另行用真实设备验证：iOS BLE 配对/Wake、更窄的 BLE Wake 白名单、iOS PiP/Android FGS 生命周期、
USB ECM、原生权限、真实的后台队列投递、App 启动行为以及 HID 粘贴。mock 套件测试的是它们所建模的策略和工具选择，
而不是操作系统集成。

### 与时间无关的记忆 fixture

`seed_memory` 条目可以将 `expires_in_sec` 设置为不超过一年（31536000 秒）的正整数。
runner 会在每次尝试前立即将其解析为绝对的 `expires_at`，而不改变套件快照。不要同时指定这两个字段。
通知变更 fixture 使用一天的有效期，这样它们不会随着日历推进而过期。只有在有意模拟过期的测试中才保留绝对时间戳。

`results: null` 的内联召回结果是一个已完成的空结果，与 Go 中 nil 切片的 JSON 编码一致。
该结果中缺失的预期 ID 属于计入的召回失败，而不是 Judge 错误。缺失或被截断的证据仍不计入。

## 报告

运行会在 `benchmark/runs/<run-id>/` 下写入一份自包含的报告；对于 WebUI，则写入
`benchmark/runs/webui/<job-id>/`。

报告包括：

- 套件/任务状态和通过率。
- 提示词、最终回复、硬断言失败、评分细则判定。
- 从 Agent 历史中提取的工具 trace。
- 配置了环境桥接时的 `pre.jpg` 和 `post.jpg` 截图。
- 一个用于人工检查的 `View full trace` 操作。

Judge 只使用操作前/后截图以及 trace/最终回复，不会使用每一张中间截图。

CI 要求每个计划中的任务都有一行结果，并且 manifest 中的总数与这些行一致。任务级的 `failed`、
`skipped`、`timeout` 和 `judge_error` 会在 `summary.md` 和 `report.html` 中保持可见；
只要所有结果产物完整，它们就不会自动让工作流失败。工作流成功意味着基准测试产出了完整的产物，
而不是每个任务都通过了、或每次尝试都被评了分。Judge 错误仍是未计分的评估错误；在解读分数之前，
请先检查计入资格和覆盖率。已记录的尝试可以在不重复设备操作的情况下被检查和重新评判。

视觉前置条件失败（`preconditions_passed=false`）仍会让工作流失败，因为起始状态无效。
manifest 缺失或无效、结果行或报告缺失、总数不一致，或 runner 意外退出，也会让工作流失败。

产物上传成功时，Actions 作业摘要会链接到报告包。要直接从 CI 提供同一份自包含、可交互的
`report.html`，请配置下面两个仓库变量。发布步骤会原样复制生成的 HTML，因此其布局和抽屉交互与 WebUI
报告完全一致。

- `BENCHMARK_REPORT_PUBLISH_DIR`：由自托管 runner 上的静态 HTTP 服务器提供服务的持久化目录。
- `BENCHMARK_REPORT_BASE_URL`：映射到该目录的公网或内网 URL。

直接发布报告是可选的，不影响基准测试结果。仓库的 GitHub Pages 站点保留给 APT 仓库使用，
除非经过协调进行合并部署，否则不得用作此目录。

## 目录结构

```text
benchmark/
├── runner/              # Python package
│   ├── main.py          # CLI entry point
│   ├── suite.py         # Suite loading
│   ├── runtask.py       # Task execution
│   ├── judge.py         # OpenAI-compatible LLM judge
│   ├── experiment.py    # Agent experiment orchestration and comparison reports
│   ├── harness.py       # Aiden and external agent execution adapters
│   ├── harnesses.py     # Registry of comparable agent harness kinds
│   ├── operations.py    # Shared business tools and environment preparation
│   ├── tool_gateway.py  # Allowlisted business tool endpoint for external agents
│   ├── raw_tools.py     # Neutral raw UI tools on the environment bridge
│   └── html_report.py   # HTML report generation
├── suites/              # Benchmark suites
├── environment_bridge.md
├── experiments.md
├── manual.md
└── runs/<run_id>/       # CLI run results
    ├── manifest.json
    ├── results.jsonl
    ├── summary.md
    ├── report.html
    └── tasks/<task_id>/
        ├── pre.jpg
        ├── post.jpg
        ├── history.json
        ├── trace.json
        └── judge.json
```

## CLI 命令

### `run`

```bash
uv run python -m runner run --suite <suite.json> [options]
```

常用选项：

- `--suite PATH` - 基准测试套件 JSON 路径。
- `--agent-url URL` - 已有 Agent 守护进程的 URL。
- `--environment-url URL` - 用于 setup、操作前/后屏幕采集和 release 的环境桥接 URL。
- `--additional-environment-url URL` - 另一个相同类型和平台的桥接，可重复指定。配合 `--auto-agent-setup` 时，每个 worker 从任务 setup 到 release 期间占用一个桥接槽位，因此一次运行可以把任务分散到多台 ADB 设备上。
- `--auto-agent-setup` - 启动隔离的 Agent 守护进程 worker，并按桥接并发数调度。
- `--agent-config PATH` - 自动启动的守护进程 worker 使用的 Agent 配置。
- `--no-judge` - 只运行硬断言。评分细则未被评估的任务为 `skipped`，在重新评判之前不计入成功率；断言失败仍算失败。
- `--task-id ID` / `--task-ids A,B` - 运行指定任务。
- `--repeats N` - 覆盖任务重复次数。
- `--execution-mode MODE` - `backend-text`（默认）或 `realtime-text`。见[实时协作基准测试](./realtime-text-collaboration.md)。
- `--state-file PATH` - 为 WebUI 或脚本写入进度 JSON。
- `--verbose` - 打印详细的评分细则结果。

带有套件级或任务级 `mock_environment` 的套件需要 `--auto-agent-setup`，且不得同时传入
`--environment-url`；runner 会自行启动脚本化桥接，并在每个任务前激活匹配的 fixture。

自动启动的 Docker worker 会收到一份在主机上解析好的只读 `agent.toml`。
`api_key = "$AICO_BENCHMARK_AGENT_API_KEY"` 之类的凭据引用必须在守护进程启动前就能在主机上解析。
MobileGym 端点在创建 worker 之前，还需通过一次功能性的 `setup` / `release` 预检。

在 Linux 上，MobileGym 的桥接端口必须能通过 Docker 主机网关访问。固定的和自动分配的桥接端口都绑定在
Docker 网桥网关接口上；模拟器的 Web UI 仍绑定在回环地址。该桥接没有身份验证。绑定在网关上并不能将其与同一主机上的
其他容器隔离；请使用一台可信的基准测试主机，不要让不可信的工作负载共享 Docker 守护进程。
对于自定义 Linux 网桥、守护进程的 `host-gateway-ip`，或 Linux 上的 Docker Desktop（`127.0.0.1`），
请覆盖 `AICO_MOBILEGYM_BRIDGE_BIND_HOST`。仅凭主机侧 `/health` 成功并不能证明 worker 可以连通。

自动启动的 worker 会收到 `config/skills` 下捆绑的 Skill 文件。Go Agent 向模型暴露它们的目录摘要和
`skill_read` 工具；只有调用该工具时，Skill 正文才会进入对话。runner 没有预加载 Skill 的选项：
是否读取某个 Skill 由 Agent 自行决定，而 `skill_discovery_v1` 衡量的正是这个决定。
请通过记录下来的 `skill_read` 调用来核实 Skill 的实际使用情况。

由于 Skill 文本会改变 Agent 的行为，每次运行都会在 `manifest.json` 中记录一个
`skills_fingerprint`：

```json
"skills_fingerprint": {
  "sha256": "...",
  "skills": {"device-operator": "..."},
  "source": "worker-config"
}
```

该摘要在运行结束时根据每个 worker 复制的 Skill 目录树计算，因此记录的是 `--agent-config` 或替换了
`SKILL.md` 的 SkillOpt 候选，而不是仓库检出的版本。所有 worker 一致时 `source` 为 `worker-config`；
worker 之间不一致时为 `mixed`（此时运行级摘要为 null，但保留所见的每个 Skill 的摘要，从而指出出现分歧的 Skill）；
`--agent-url` 运行则为 `external-daemon`，因为其 Skill 目录树从未由本进程配置，所以无法观察。
`external-daemon` 运行不是同等条件下的能力比较。

phone-control 套件通过每个任务已有的 `hard_assertions.forbidden_tools` 禁止
`request_user_action`：这些任务要求自主完成。转交并不是其他套件的全局失败策略。

### `rejudge`

```bash
uv run python -m runner rejudge --run-dir runs/<run_id>
```

在不重新执行任务的情况下，对已有产物重新评判。

### `compare`

```bash
uv run python -m runner compare --runs runs/<run_a> runs/<run_b>
```

比较两次运行之间的任务状态翻转、延迟和通过率变化。

### `experiment`

```bash
uv run python -m runner experiment --config experiments.example.json \
  --agent-config /absolute/path/to/agent.toml
```

运行一次 Agent 实验，在 MobileGym 环境中针对已有的操作套件比较 Aiden 与外部 Agent。结果写入
`<out>/experiments/<experiment-id>/` 下。选项包括 `--suites-dir`、
`--out`、`--daemon-image`、`--no-build-daemon-image`、`--judge-model` 和
`--judge-base-url`。配置、公平性边界和报告语义见
[`benchmark/experiments.md`](../../benchmark/experiments.md)。手动分派的
**Agent Experiment** 工作流会在基准测试 runner 上运行同样的命令。

### 将已完成的运行发布到 Langfuse

发布是一个独立的运行后步骤，因此上传失败时可以重试，而无需重新运行 Agent 或设备环境。每个稳定的套件名称对应一个
Langfuse Dataset，每次任务尝试对应一个稳定的 Dataset Item，每次基准测试执行会创建一个 Experiment Run，
其中包含条目级和汇总的基准测试分数。更新套件会 upsert 新的 Dataset Item 版本，而不是创建新的 Dataset。
每次新运行还会在 manifest 旁边保存 `suite.json`，因此即使源套件发生变化，它仍可被发布。

发布器会在能力分数之前写入显式的 `failures.class.environment` 和
`failures.class.evaluation` 计数（包括 0）。为了兼容省略零值类别的旧版发布器，历史数据会把缺失的环境和评估失败计数默认为 0。
历史运行无需重新发布产物即可保留其能力比率。显式无效的计数仍会使该测量不可用，缺失的任务计数或能力分数仍为未知。
非零的环境或评估计数会将覆盖率标记为不完整：工作流级别的 pass@1、pass@k 和 pass^k 将不予给出，
周度匹配比较也会排除窗口总数不完整的 dataset。

```bash
cd benchmark
export LANGFUSE_PUBLIC_KEY="pk-lf-..."
export LANGFUSE_SECRET_KEY="sk-lf-..."
export LANGFUSE_BASE_URL="http://127.0.0.1:3010"

uv run python -m runner publish-langfuse --run-dir runs/<run-id>
```

套件名称相同的运行会出现在同一个 Dataset 中。实验元数据会记录 Git SHA、套件哈希、工作负载哈希、模型、Judge、平台、
指标 schema 以及固定的 `k`。工作负载哈希只涵盖任务和尝试的键。要进行同等条件的回归比较，需要套件哈希、工作负载哈希、
`k`、Agent 模型、Judge 提供商/模型/提示词版本、目标平台以及 Skill 指纹全部一致。
如果有意比较模型或 Judge，请将被改变的字段视为实验轴，并保持其他影响分数的元数据不变。来自不同套件版本的运行仍可在
Langfuse 中查看，但除非使用未变更条目的交集，或针对新的套件定义重新运行旧的产品版本，否则不应将它们的汇总分数视为同等条件的回归比较。
当存在 Agent episode ID 时，实验 trace 还会记录确定性的 Agent trace ID，以便与 Aico 遥测数据关联。

发布按运行 ID 保证幂等。重试时会核对 Dataset Run Item 集合、补充缺失的条目，并以稳定的 ID 重写分数。
运行 ID 冲突时会直接失败，而不是默默混合结果。Langfuse 原生的 Latency 和 Cost 列描述的是用于构建 Experiment Run
的产物重放过程；真实的基准测试测量请使用 `benchmark.*`、`efficiency.*`、`reliability.*` 和
`cost_to_first_success.*` 分数。

产物重放被刻意串行化。Langfuse 在链接每个 Dataset Item 时会创建或查找 Dataset Run，而在自托管部署上，
并发的首个条目请求可能会创建重名的重复运行。发布 CLI 会记录 dataset/运行标识、条目准备、重放、回读重试、分数发布，
以及最终的 CI 发布总数，以免把部分摄取误认为发布成功。

独立的发布命令是 CI 的集成点。GitHub Actions 作业可以用唯一的运行 ID 运行套件，再在第二个步骤中发布已完成的产物：

```yaml
- name: Run benchmark
  working-directory: benchmark
  run: |
    RUN_ID="ci-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-${GITHUB_SHA::8}"
    echo "RUN_ID=$RUN_ID" >> "$GITHUB_ENV"
    uv run python -m runner run \
      --suite suites/<suite>.json \
      --run-id "$RUN_ID" \
      <environment-options>

- name: Publish benchmark to Langfuse
  working-directory: benchmark
  env:
    LANGFUSE_PUBLIC_KEY: ${{ secrets.LANGFUSE_PUBLIC_KEY }}
    LANGFUSE_SECRET_KEY: ${{ secrets.LANGFUSE_SECRET_KEY }}
    LANGFUSE_BASE_URL: ${{ vars.LANGFUSE_BASE_URL }}
  run: uv run python -m runner publish-langfuse --run-dir "runs/$RUN_ID"
```

## 历史仪表盘与周报

最新结果区域会始终展示分数、变化量、测试领域过滤器和失败证据。指标定义和数据覆盖说明默认折叠。
选择一个 dataset，即可在 **Weekly trends → Run details** 下查看其运行比较：模型、Judge 提示词、每个任务的尝试次数、
任务数和尝试数、Skill 以及环境。分支和提交用于标识每次运行；仅分支变化不会被呈现为评估设置的变化。
当两个提交都已知时，变化的套件文件会被标记并附带源码比较链接。这并不意味着语义发生了变化：仅格式调整也可能改变文件指纹。
原始指纹、Judge 提供商和指标 schema 保留在报告数据中以便排查，不会以单独的表格显示。
任务选择元数据可以区分重复次数的变化与覆盖范围的变化；没有该元数据的旧记录会保留一个通用的覆盖范围变化提示。

每日基准测试工作流会生成 `dashboard.html`，其中包含最近 7 天的 pass@1 趋势和效率指标。
另一个 `benchmark-weekly-report.yml` 工作流每周四运行，发送一张飞书（Lark）卡片，汇总最近 7 天的情况以及每个 dataset
相对其自身前 7 天的变化，并附带仪表盘链接。历史数据来自 Langfuse v4 的实验和分数；两者都不会下载更早的 Actions 产物。
两个工作流默认都使用工作流所在分支的历史数据。手动生成周报时，可将 `branch_filter` 设为其他分支，或设为 `*` 表示所有分支。

凭据、报告命令、时间窗口和比较规则见
[跨运行仪表盘与周报](../../benchmark/manual.md#cross-run-dashboard-and-weekly-report)。

## GitHub Actions CI

基准测试工作流为 `.github/workflows/benchmark.yml`。它刻意使用 CLI runner 而不是 WebUI，
这样每个用例都有稳定的退出码、自包含的产物，以及幂等的 Langfuse 发布步骤。

套件选择的唯一事实来源是 `benchmark/ci/suites.json`。当新的 `benchmark/suites/**/*.json`
文件未被分类时，CI 目录校验作业会失败，从而防止新套件被悄悄遗漏。当一个套件的任务需要不同平台时，
它可以由多个用例表示；`connection_capabilities_v1.json` 目前被拆分为 iOS 和 Android 两个用例。

执行策略只取决于 CI 能否准备好环境：

| 触发方式 | Profile | 用途 |
| --- | --- | --- |
| 每日定时 | `runnable` | 自动运行的 isolated、MobileGym 和 ADB 模拟器用例（18 个） |
| 手动分派 | `runnable` | 重新运行同样的自动用例，或显式选择一个或多个套件 |
| 手动分派 | `hardware` | ADB、VPhone、桌面和真实手机桥接用例（16 个） |
| 手动分派 | `adb` | 在配置的模拟器上进行原生 Android 对比、内置 App 工作流和 Bridge 能力测试 |
| 手动分派 | `all` | 所有目录用例；需要所有环境均已配置 |

定时全量运行每天 Asia/Shanghai 时间 02:07 执行。它会选择环境为 `isolated`、`mobilegym` 或 `adb`
且目录中 `runnable` 标志为 true 的所有用例。这包括两组 ADB 对比（四个用例），以及在 runner 所配置的 Android
模拟器上运行的内置 App 用例。ADB 用例仍可通过手动 `hardware` profile 运行，并且在每个 profile 中都从同一个
Android 设备池租用设备。`all` profile 会将每个目录用例各运行一次，不会重复。
工作流在 Docker 中启动 Android MobileGym 环境；isolated 和 mock 套件使用隔离的 Agent worker 运行，不需要外部设备。
VPhone 目前被排除在外，因为它需要一个单独托管的 macOS Apple Silicon 桥接；一旦
`BENCHMARK_VPHONE_ENVIRONMENT_URL` 指向一个可达的桥接，仍可通过手动 `hardware`/`all` 分派运行。

包含 100 个任务的 MobileGym 校准套件不在自动全量运行之内；仍可在手动校准运行中显式选择它。手动运行单个套件时必须使用匹配的
`runnable`、`hardware` 或 `adb` profile；如需有意越过该边界，请使用 `all`。`suite` 输入也接受以逗号分隔的路径，例如
`suites/quick_capture_v1.json,suites/notification_memory_v1.json`；每个路径都按同样的 profile 规则、按给定顺序选择。

要在 MobileGym 中建立 phone-control 基线，请以 profile `runnable`、套件 `suites/phone_control_v1.json`、
重复次数 `1` 手动分派。CI 会自动准备 Android 风格的 MobileGym 模拟器；不需要外部 iOS 桥接。该用例只能通过手动分派运行。
仅适用于 iOS 的蓝牙搜索任务会被过滤掉，剩下 17 个适用任务。模拟器支持问题和 setup 失败必须与 Agent 能力失败分开审阅。
同一套件以 profile `hardware` 运行时会选择外部 iOS 用例；profile `all` 会同时选择两种环境。

对于原生 Android，请以 profile `adb` 分派：一般操作用
`suites/adb_android_basic.json`，文本编辑用
`suites/adb_android_text_editing.json`，Chrome、地图、相机、相册、文件、信息、时钟和通讯录工作流用
`suites/adb_android_builtin_apps.json`。
将 suite 输入留空，则会运行两组对比、内置 App 套件以及专门的 Bridge 能力套件（六个用例）。覆盖范围和设备前提条件见
[Android 套件](../../benchmark/adbandroid/README.md#test-suite)。

用例可以声明 `task_ids` 来复用已有套件的一个子集。ID 必须存在、唯一，并支持该用例的目标平台。空列表表示选择整个套件
（仍受平台过滤约束）。ADB 用例要求并发数为 1。

当一个套件在多个环境 profile 中都有用例时，显式选择套件只会运行属于所请求 profile 的用例。没有匹配用例的选择仍会报错。

pull request 会在常规 `CI` 工作流中运行目录和规划器测试，但不会获得 Agent/Judge/Langfuse 的 secret，
因此不会操作设备。真实的基准测试执行来自默认分支上的每日定时运行，或任意仓库分支上的手动 `workflow_dispatch`。
pull request 和 fork 的 ref 仍然无法手动分派。

手动分派工作流时，使用 `model` 下拉菜单为该次运行选择 Agent 模型。手动分派默认使用
`google/gemini-3.5-flash-lite`，这也是定时运行所用的模型。

工作流需要以下 GitHub 配置值。面向 GitHub 的名称省略了 `AICO_` 前缀；工作流会将它们映射到下文所述的 runner 变量。

- Variables：`BENCHMARK_REPEATS`、`BENCHMARK_AGENT_PROVIDER`、
  `BENCHMARK_AGENT_BASE_URL`、`BENCHMARK_JUDGE_MODEL`、
  `BENCHMARK_JUDGE_BASE_URL`、`DAEMON_IMAGE`、`BENCHMARK_ANDROID_SERIAL`、
  `BENCHMARK_ANDROID_SDK_DIR`、
  `LANGFUSE_BASE_URL`、`BENCHMARK_PHONE_ENVIRONMENT_URL`、
  `BENCHMARK_IOS_ENVIRONMENT_URL`、`BENCHMARK_MAC_ENVIRONMENT_URL`、
  `BENCHMARK_VPHONE_ENVIRONMENT_URL`、
  `BENCHMARK_AICO_APP_IOS_ENVIRONMENT_URL` 和
  `BENCHMARK_AICO_APP_ANDROID_ENVIRONMENT_URL`。
- Secrets：`BENCHMARK_AGENT_API_KEY`、`BENCHMARK_JUDGE_API_KEY`、
  `LANGFUSE_PUBLIC_KEY` 和 `LANGFUSE_SECRET_KEY`。

实时协作套件还会使用
[实时协作基准测试](./realtime-text-collaboration.md#ci)中所述的 `BENCHMARK_REALTIME_*` 设置。

将仓库变量 `BENCHMARK_REPEATS` 设为正整数（例如 `3`），即可在定时和手动运行中重复每个任务。
非空的手动 `repeats` 输入会在单次运行中覆盖该变量；留空则使用仓库设置。两者都未设置时，工作流使用 `1`。
报告会根据计划的任务重复次数推导出 `metrics_k`；不需要单独的指标变量。

ADB 用例需要一台已启动、并且可从 runner 主机访问的 Android 设备或模拟器。将 `BENCHMARK_ANDROID_SERIAL`
设为其 adb 序列号，例如 `emulator-5554`；或设为逗号分隔的列表，例如
`emulator-5554,emulator-5556,emulator-5558,emulator-5560,emulator-5562`，为 ADB 用例提供一个设备池；
工作流会将其作为 `ANDROID_SERIAL` 传给 runner。池中的每台设备都必须是独立的模拟器，且具有与单台设备相同的系统镜像，
以及相同的 root、Aico App 和 SystemUI 就绪状态。当 `adb` 不在 runner 的 `PATH` 中时，工作流会使用
`<sdk>/platform-tools/adb`，其中 `<sdk>` 是可选的 `BENCHMARK_ANDROID_SDK_DIR` 变量，默认为 `~/android-sdk`。
在任何 ADB 用例开始之前，工作流会等待设备启动完成，然后检查 `com.android.systemui` 正在运行、并且
`uiautomator dump` 能返回界面层级。仅有 `sys.boot_completed` 是不够的：GPU 挂起后，SystemUI 可能一直处于死亡状态，
而设备仍报告已完成启动。在自托管 runner 上，如果 UI 检查失败或模拟器服务的 cgroup 超过 12 GiB，
会重启该服务一次并再次执行同样的检查。`emulator-<port>` 对应的服务是用户单元
`android-emulator@<port>.service`；第一个序列号也可能使用原来的 `android-emulator.service`。
两种单元都没有的序列号无法被重启。客户机内存上限为 4 GiB；由于 SwiftShader 叠加在其上，一个刚启动的服务约占 7 GiB。
模拟器永远不会释放已死亡 Android 进程在主机上的 RenderThread，因此每次进程死亡会泄漏约 120 MiB；
每个 ADB 任务都会强制停止其 App，一个作业会让每个模拟器增加 2-3 GiB。因此，在任何运行过 ADB 用例的作业结束后，
工作流都会重启所有已配置的模拟器；它们会在作业发布结果期间启动，而下一个作业（无论是定时还是分派）会在其设备检查中等待启动完成。
12 GiB 检查只用于捕获那些因作业被取消或丢失而未能重启的模拟器。仍然没有可用 UI 的设备会带着警告被移出设备池，
由其余设备运行 ADB 用例。只有当没有任何可用设备时，作业才会在用例开始前失败。

当任一被选中的用例启用了 Aico App 时，CI 会从不带版本号的分发 URL 读取当前发布版本的文件名，并将其
`versionCode` 与模拟器上已安装的 `link.aico.app` 软件包进行比较。只有版本不同时才会下载并安装 APK。
池中的每台设备都会安装该 App 并完成路由设置，因为任何一台都可能接到 Aico App 用例。作业摘要会记录发布文件名、
已安装的 `versionCode` 以及是否复用了已有安装；下载了 APK 时，还会记录其 SHA-256。以相同 `versionCode`
重新发布的 APK 被视为同一个发布版本。安装在每个作业中只执行一次。如果安装或模拟器路由失败，只有 Aico App 用例会以
`aidenbridge_setup_failed` 失败；其他所有用例仍会运行并发布，但作业整体仍会失败。

文本编辑套件以配对方式运行，使用相同的 JSON 任务、setup、提示词、评分细则、重复次数和一个 worker：

| 套件 | 不带 Aico App | 带 Aico App |
| --- | --- | --- |
| `adb_android_text_editing.json`（4 个任务） | `adb_android_text_editing` | `adb_android_text_editing-aidenbridge` |

`adb_android_basic.json`（16 个任务）只在不带 Aico App 的情况下运行。它的导航、返回、最近任务和滚动任务在两种变体中
都使用 ADB 截图和输入，并且只要配套 App 处于后台，它的 `open_app` 调用就会回退为可见搜索，因此连接变体测得的是同一条执行路径。
文本输入才是路由取决于 Phone Bridge 的通用 Android 工作负载：使用可在后台安全执行的剪贴板写入再粘贴，而不是 ADB 按键输入。

这些任务定义同步自 main 提交
`16ab57b50c68a91a05915a4112f8bc2a896170af`。基础任务覆盖通用的 UI 导航和状态恢复；文本编辑任务覆盖 ASCII 空格、
标点、替换和清空。`phone_control_v1.json` 套件仍为 MobileGym 和 iOS 注册，没有额外的 ADB 用例。

文本编辑配对在每日 `runnable` 全量运行中执行。以 `runnable`、`hardware`、`adb` 或 `all` 手动选择该套件时，
同样会运行两种变体。基线变体会在开始前停止任何残留的配套 App，并在没有 Phone Bridge 连接的情况下使用 ADB 环境。
连接变体会在每个任务的 Agent 上建立 Phone Bridge，同时仍由 ADB 提供截图和输入。两种变体都会保留任务准备好的屏幕；
连接变体不会在任务 setup 之后将配套 App 切到前台。
每个 ADB 用例在整个运行期间从设备池租用设备，因此不会有两个用例共享同一个模拟器。用例在目录中的 `max_concurrency`
是它最多租用的设备数，每台设备一个任务；配置的设备较少时它会使用更少的设备。Aico App 用例始终使用一台设备。
用例按先到先得的方式获取设备，需要设备最多的用例优先申请。

配套 App 只有在绑定到一个可路由到开发板子网的 USB 或以太网 Android Network 之后，才能访问开发板，而模拟器两者都没有。
在前台，没有这种绑定时 App 会拒绝打开 WebSocket；在后台，其前台服务（FGS）只会通过该网络轮询 HTTP 命令队列。
因此，对于包含可运行 Bridge 任务的运行，runner 会为已 root 的模拟器提供一条类似开发板的链路；被跳过的任务不会触发此设置：

1. runner 分别检查 `android.hardware.ethernet` 特性及其静态 `eth1` 配置（`192.168.42.2/24`）。
   如果缺少该特性，它会 bind-mount 一份添加了该特性的 `/vendor/etc/permissions` 副本。
   如果已存储的配置不同，它会安装基准测试配置。任一变更都会触发一次 Android 框架重启，以便 EthernetService
   加载配置。bind mount 在重启后会丢失，因此两项检查在每次 Bridge 运行时都会执行。
2. 它添加一对 veth：`eth1` 及其对端 `aicobd`（`192.168.42.1/24`），Android 会将其配置为一个以太网网络。
   App 将其开发板客户端绑定到该网络。发往 `192.168.42.1:8080` 的请求仍会通过已有的 iptables DNAT 规则和
   `adb reverse` 到达任务 Agent。
3. 后台任务会等待 Agent 报告 `fgs_bridge_enabled=true`，否则 setup 失败，而不是悄悄回退到 ADB 文本输入。
   运行结束时 runner 会删除这对 veth。CI 在终止超时的 runner 后，也会在父用例的清理步骤中删除它，
   赶在另一个用例使用共享模拟器之前。工作流中始终运行的清理步骤为作业取消提供了进一步的兜底。

`eth1` 存在期间，Android 会优先将其作为默认网络，因此模拟器无法访问互联网。“设置”文本编辑和 Aico App
能力任务不需要互联网，而不含 Bridge 任务的运行永远不会创建该链路。

运行 ID 用于区分变体，`manifest.json`/Langfuse 元数据会将 `execution_environment` 记录为 `adb` 或
`adb_aidenbridge`。仪表盘在其配置比较和运行历史中显示 **ADB device** 和 **ADB + Aico App**。
请使用通过率、工具调用、任务耗时以及 token/成本指标，比较来自同一工作流尝试/模型/重复次数的匹配任务。
套件哈希不同的历史运行不构成受控的 Bridge 对比。

`aico_app/android_bridge_capabilities_v1.json` 是连接状态下的 Aico App 套件。它测试的是 Bridge 专有的 API，
而不是作为无 Bridge 的对比工作负载。它明确地在配套 App 处于前台时开始其任务。它在已 root 的 Android 模拟器上用一个
Agent worker 运行，逐个任务检查 Phone Bridge 连接，覆盖剪贴板往返、按语义打开 App 和 URL、日历创建/查询/删除、
通讯录创建/查询/更新、本地通知，以及单行和多行 Unicode 文本输入。写入类任务使用保留的 fixture
（`AidenBridgeBenchmark` 本地日历、`AidenBridgeBenchmarkContact` 联系人，以及发给 `+12025550147` 的信息草稿）。
runner 在每个写入任务前后（包括失败时）只删除这些 fixture。每个任务的 ADB setup 只会让模拟器回到主屏幕，而“信息”会在下次打开时
恢复已保存的草稿，因此短信草稿 fixture 会用一个哨兵正文打开该收件人并将其清空。CI 模拟器上已授予日历/通讯录的写入权限。
文本输入任务既要求 App 产生一条新的剪贴板写入诊断事件，也要求非 Bridge 的 Android 输入框中出现完全一致的文本；
其证据包含在 `results.jsonl` 以及本地 `android_bridge_input.json` 任务产物中。如果无法收集输入证据，
输入断言为未知；原本通过的任务会变为评估错误。清理失败时会保留已完成的任务结果，并记录在
`android_bridge_cleanup_error` 和 worker 日志中。使用 `hardware` profile 选择该套件，可只运行连接状态下的 App 用例。
这个专门的能力套件也会在每日 `runnable` 全量运行中执行。

该套件取代了以前单独的 `android_bridge_data_v1.json` 套件。它并不声称覆盖仅限 iOS 的传输方式、BLE 通知转发，
或每一种权限/后台失败模式。定时通知投递和可选的日历提醒还需要单独的设备侧断言。

基准测试作业默认使用 `aiden-hosted-02` runner（可通过 `AICO_RUNNER_PRIMARY` 覆盖），因为 MobileGym 和
agent-daemon 路径都需要 Docker。一个作业只执行一次检出、依赖和 Docker 设置，准备可复用的镜像，然后将选中的用例分发出去。
`BENCHMARK_CI_MAX_PARALLEL` 限制同时运行的基于容器（isolated 和 MobileGym）的用例数量，其中 MobileGym 用例优先调度。
基于设备的用例不占用这些槽位，会立即开始，因为它们通常处于关键路径上：ADB 用例只等待 Android 池中的空闲设备，
外部硬件用例共享一把互斥锁以避免设备/桥接争用。需要设备最多的设备用例优先申请；数量相同时按目录顺序决定，
因此目录把耗时最长的单设备用例排在最前。在有五个模拟器时，`adb_android_builtin_apps` 使用三个，其余每个 ADB 用例各使用一个，
因此 ADB 用例大约 20 分钟即可完成，而不是在一个模拟器上依次运行所需的 67 分钟。
每次运行在其用例结束后会立即发布到 Langfuse（此时后续用例仍在运行），并被标记为已发布。用例结束后的独立发布步骤只发布没有该标记的运行，
例如发布失败的运行，或用例步骤被中断后的所有运行。
每个已完成的运行都会上传一个保留 14 天的诊断包，其中包含 manifest、指标、结果、摘要、HTML 报告、套件快照、
setup/用例日志和 worker 日志、在需要构建镜像时的镜像构建日志，以及每个任务的证据：`trace.json`、
`pre.jpg`/`post.jpg` 截图、历史记录、Judge 结果、setup、episode、环境状态，以及协作/fixture/场景事件文件。
临时失败后会以相同的文件列表重试上传一次。该诊断包足以检查状态并重试 `runner publish-langfuse`；生成的 worker
配置被排除在外，因为其中包含已实体化的 Agent 凭据。

## Runner 与本地 CLI 环境变量

- `AICO_BENCHMARK_AGENT_PROVIDER` - 默认配置模板的 Agent 提供商类型。
- `AICO_BENCHMARK_AGENT_MODEL` - 默认配置模板和运行 manifest 中的 Agent 模型。
- `AICO_BENCHMARK_AGENT_BASE_URL` - 默认配置模板的 Agent 模型端点。
- `AICO_BENCHMARK_AGENT_API_KEY` - 嵌入到生成的只读配置中的 Agent 凭据。
- `AICO_BENCHMARK_JUDGE_MODEL` - 默认 Judge 模型。
- `AICO_BENCHMARK_JUDGE_BASE_URL` - 默认的兼容 OpenAI 的 Judge 端点。
- `AICO_BENCHMARK_JUDGE_API_KEY` - 启用 Judge 时必需。
- `AICO_BENCHMARK_ANALYSIS_API_KEY` - 可选的运行后分析凭据；默认使用 Judge 的 key。
- `AICO_AGENT_URL` - 默认的 `--agent-url`。
- `AICO_ENVIRONMENT_URL` - 默认的 `--environment-url`。
- `AICO_DAEMON_IMAGE` - 自动 Agent 设置所用的默认守护进程 worker 镜像。
- `AIDEN_BENCHMARK_PI_EXECUTABLE` - Agent 实验使用的 pi 可执行文件。
- `AIDEN_BENCHMARK_PI_API_KEY` - 外部 Agent 自定义端点的默认凭据变量。
- `AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE` - Agent 实验使用的 OpenClaw CLI。
- `AIDEN_BENCHMARK_OPENCLAW_NODE` - 用于运行 OpenClaw 的 Node 运行时。
- `AIDEN_BENCHMARK_CLAUDE_CODE_EXECUTABLE` - Agent 实验使用的 Claude Code CLI。
- `AIDEN_BENCHMARK_CODEX_EXECUTABLE` - Agent 实验使用的 Codex CLI。
- `LANGFUSE_PUBLISH_VERIFY_TIMEOUT_SECONDS` - 发布后回读校验的最长等待时间；默认 660 秒，以覆盖 Langfuse 异步摄取的延迟。

## 执行模式

- 已有 Agent：CLI 与一个已在运行的 Go Agent 守护进程通信。
- 自动 Agent 设置：CLI 启动隔离的守护进程 worker，并通过环境桥接获取工具/屏幕。
- WebUI：管理套件、作业、环境、worker、任务屏幕、日志以及持久化的作业记录。

基准测试套件格式在物理设备和 MobileGym 之间通用。与这些守护进程设置无关，`--execution-mode`
用于选择被测的 Agent 路径：`backend-text`（默认）只测后台 Agent，`realtime-text` 则测实时前台与后台的协作。
SkillOpt 是独立的，位于 `skillopt/` 下；它可以调用 benchmark runner 的 API，但 benchmark 不会暴露 SkillOpt
的运行、套件或报告。

## 相关文档

- [架构设计](./architecture.md)
- [实时协作基准测试](./realtime-text-collaboration.md)
- [详细指南](./quickstart.md)
- [自动改进：基准测试 issue 报告](./auto-improve.md)
- [环境桥接协议](../../benchmark/environment_bridge.md)
- [完整手册](../../benchmark/manual.md)

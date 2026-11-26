---
sidebar_position: 16
---

# Episode 遥测与 Langfuse 集成

每个任务完成后，Aico 会异步地把完整的任务 episode（元数据、事件链、截图）上报到 [Langfuse](https://langfuse.com/)，每个 episode 对应一条 trace。

Trace 通过 Langfuse 的 OpenTelemetry（OTLP）端点（`POST /api/public/otel/v1/traces`）发送，并遵循 `langfuse.*` 属性约定。旧版 `/api/public/ingestion` 事件 API 已被弃用，并将于 2026-11-16 停止接受 trace 和 observation 事件，因此不再使用。

## 功能开关

在 `agent.toml` 中配置 `[advanced_settings.runtime.telemetry]` 一节。默认关闭；启用后也不会影响任务执行（尽力而为的异步上报）。

```toml
[advanced_settings.runtime.telemetry]
enabled = true
provider = "langfuse"
base_url = "http://langfuse.example.com:3000"
public_key = "pk-lf-..."
secret_key = "sk-lf-..."
upload_screenshots = true
upload_timeout_sec = 30
max_retry = 2
environment = "prod"
tags = ["aico-hardware"]
```

| 字段 | 说明 |
| --- | --- |
| `enabled` | 总开关；为 `false` 时零开销 |
| `base_url` | Langfuse Web 地址（不含路径） |
| `public_key` / `secret_key` | Langfuse API 密钥 |
| `upload_screenshots` | 是否上传 `artifacts/step_*.jpeg` 截图 |
| `upload_timeout_sec` | 单次上报的超时时间 |
| `max_retry` | 失败时的重试次数 |
| `environment` | Langfuse trace 环境标签 |
| `tags` | 附加到每条 trace 的标签 |

凭据直接写在 `agent.toml` 的 `[advanced_settings.runtime.telemetry]` 一节中。

Langfuse 部署必须支持 OTLP 接入：自托管 Langfuse `>= 3.22.0`，或 Langfuse Cloud。Agent 会发送 `x-langfuse-ingestion-version: 4` 头，使 span 实时落入以 observation 为先的数据模型。

## 数据流

```text
Runtime.Run()
  → Agent execution loop (model calls captured with role, timing, usage, cost)
  → EpisodeRecorder records events
  → CommitEpisode persists to disk (episode.yaml + events.jsonl + artifacts/)
  → exportEpisodeBestEffort async maps episode + events + prompt calls to OTLP spans
  → POST /api/public/otel/v1/traces (spans), POST /api/public/scores (outcome)
```

Span 只完整导出一次：Langfuse 把已接入的 span 视为不可变，因此不会先创建 observation 再更新它。只有未被接受的 chunk 会被重试；部分成功的响应会被报告而不是重新发送，因此重试不会产生重复的 observation。

### Langfuse 映射

| Aico Episode | Langfuse |
| --- | --- |
| `TaskEpisode` | Trace（`aico-episode`），每个 episode 一条 trace |
| 运行 | 根 observation `agent-run`（`agent` 类型），携带用户目标和最终回答 |
| `loop_phase` | `span` `phase/{phase}` |
| 迭代 | `span` `agent-iteration`，metadata 中带有 `iteration` |
| `tool_call` + `tool_result` | 一个以工具命名的 `tool` observation，调用参数作为输入，结果作为输出 |
| 模型调用 | `generation` observation：`agent-response`、`summarize-context`（上下文压缩）、`llm-response` 兜底 |
| 记忆检索 | `retriever` `memory/retrieve` |
| STT / 语音 | `span` `stt/*`、`voice/*`，带有模型、提供商和延迟元数据 |
| `Outcome.Success` | 通过 scores API 写入的布尔 Score `success=1/0` |
| `artifacts/*.jpeg` | 媒体上传，在工具 observation 的输出中引用 |
| `Extra` 指标 | Trace metadata + generation 的模型/成本/用量字段 |

典型的 trace 结构：

```text
aico-episode (trace)
└── agent-run (agent)
    ├── session/begin
    ├── memory/retrieve (retriever)
    └── phase/default
        └── agent-iteration
            ├── agent-response (generation)
            ├── screenshot (tool)
            └── verifier
```

Observation 名称是稳定的操作名，绝不是每次调用的具体值：一次运行中的每次模型调用都会产生自己的 generation（带有各自的模型、token 和成本），重复的步骤共用一个名称，运行相关的值放在 metadata 中。

### Trace 元数据和标签

Trace 级别的上下文——`langfuse.trace.name`、`langfuse.user.id`、`langfuse.session.id`、`langfuse.trace.tags`、`langfuse.release`、`langfuse.version`、`langfuse.environment` 以及 `langfuse.trace.metadata.*`——会附加到**每一个** observation 上，而不仅是根节点。Langfuse v4 按 observation 进行过滤和聚合，因此只放在根节点上的上下文在其子节点上不可用。

除了 `episode.Extra` 中的 token、时长和模型信息之外，导出器还会从事件链中推导执行指标，并写入 trace 的 `metadata`：

| 字段 | 说明 |
| --- | --- |
| `tool_call_count` | episode 中工具调用的总次数 |
| `iteration_count` | Agent 迭代次数（来自 episode recorder 计数器或 episode.Extra） |

额外的 trace `tags`：

| 标签 | 条件 |
| --- | --- |
| `success` | 任务成功完成 |
| `failure` | 任务失败 |

在 Langfuse UI 中，你可以按成功/失败以及工具使用模式筛选任务。

本地 episode 仍会写入 `/userdata/agent/memory/episodes/`；Langfuse 作为额外的副本，用于集中分析和数据集管理。

## 自托管 Langfuse

项目提供了一份 Docker Compose 配置：[`deploy/langfuse/docker-compose.yml`](../../deploy/langfuse/docker-compose.yml)

```bash
cd deploy/langfuse
cp .env.example .env
# Edit .env, set ENCRYPTION_KEY=$(openssl rand -hex 32)
docker compose up -d
```

启动后，访问 `http://localhost:3000`，创建 Organization / Project，并把 Public Key 和 Secret Key 写入设备 `agent.toml` 中 `[advanced_settings.runtime.telemetry]` 一节的 `public_key` 与 `secret_key`；Agent 不会从环境变量读取 Langfuse 密钥。如果想跳过手动创建项目，可在首次启动前在 `.env` 中设置 `LANGFUSE_INIT_ORG_ID`、`LANGFUSE_INIT_PROJECT_ID` 以及项目密钥对；这样整套服务会在启动时自动创建该项目，但仍需把这对密钥复制到 `agent.toml` 中。

组件：Langfuse Web + Worker、Postgres、ClickHouse、Redis、MinIO（截图和事件 blob 存储）。

## 验证

1. 启动 Langfuse（本地或远程）
2. 在设备的 `agent.toml` 中设置 `telemetry.enabled = true`
3. 执行一个任务（Web UI 或基准测试）
4. 在 Langfuse UI → Traces 中确认：
   - 存在 `aico-episode` trace，根 observation 为 `agent-run`
   - 工具调用以 `tool` observation 出现，输入和输出位于同一个 observation 上
   - 模型调用以 `generation` observation 出现，带有模型、token 用量和成本
   - 可以在工具 observation 中预览截图
   - Metadata 包含 `tool_call_count`、`iteration_count` 和 token 统计
   - Tags 包含 `success` 或 `failure`
   - Trace 包含 `userId`（设备 ID）和 `sessionId`（运行时会话 ID）

如需针对运行中的 Langfuse 验证导出器本身，请运行可选的端到端测试。它会通过生产环境的导出器导出一个合成 episode，并通过 Langfuse API 读回生成的 trace：

```bash
cd src/agent
AICO_LANGFUSE_LIVE=1 \
LANGFUSE_BASE_URL=http://localhost:3000 \
LANGFUSE_PUBLIC_KEY=pk-lf-... LANGFUSE_SECRET_KEY=sk-lf-... \
go test ./internal/agent/ -run TestLangfuseLiveEpisodeExport -v -count=1
```

## Trace → 数据集 → 基准测试工作流

Langfuse 用于从生产 episode 中筛选高质量样本，并将其转换为项目的基准测试套件。

### 1. 在 Langfuse 中筛选 Trace

- 打开 **Traces**，按标签（`success` / `failure`）或 metadata 筛选
- 查看迭代 span 和截图，确认任务质量
- 为合格的 trace 添加标签或评分

### 2. 创建数据集

1. Langfuse UI → **Datasets** → New Dataset（例如 `phone_control_candidates_v2`）
2. 在 trace 详情页 → **Add to dataset**
3. 填写输入（用户目标 `user_goal`）和预期输出（`final_answer` 或评分标准描述）
4. 可选：把截图媒体作为数据集条目的 metadata 引用

### 3. 导出并转换为基准测试任务

Langfuse 支持导出数据集条目（通过 UI 或 [Public API](https://langfuse.com/docs/api-and-data-platform/features/public-api)）。

手动或通过脚本把选中的条目转换为 [`benchmark/suites/`](../../benchmark/suites/) 格式：

```json
{
  "id": "open_settings_from_prod_001",
  "category": "single_step",
  "description_for_judge": "Agent must open Settings from home screen.",
  "prompt": "Please open system settings.",
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
```

字段映射：

**Episode / trace metadata 附加字段：**

| 字段 | 说明 |
| --- | --- |
| `model` / `model_name` / `model_provider` | 本次运行使用的 LLM（来自 `agent.toml` 中的 `[model_settings.model]`） |
| `agent_commit` | 构建 Agent 可执行文件时的 Git commit（由应用构建任务通过 ldflags 注入） |
| `agent_build` | Agent 构建版本号（`YYYYMMDD-HHMMSS-<commit>`） |
| `firmware_version` | 设备 OTA 状态文件 `/userdata/ota/state.json` 中的 `current_version` |
| `session_boundary_decision` / `session_boundary_reason` | 本次运行的会话边界分类器输出。 |
| `session_rotated` | 本次运行在处理用户轮次之前是否归档了之前的活动会话。 |
| `pending_chunks_recalled` | 显式结果 `source` 为 `pending` 的 `recall_session_chunks` 结果数量；忽略 `chunk_id` 前缀。 |

**Langfuse trace 字段映射：**

| Langfuse 字段 | 来源 |
| --- | --- |
| `version` | Agent 构建版本 `agent_build`，缺失时为 `firmware_version` |
| `release` | Git commit `agent_commit`，缺失时为 `firmware_version` |
| `metadata.model` | LLM 模型（例如 `openrouter/google/gemini-3.5-flash`） |
| `metadata` | 上述所有字段 + episode 指标 |
| `tags` | 配置的标签 + `model:{provider/model}` |
| `userId` | `device_scope.device_id`，或 `extra.user_id` |
| `sessionId` | 运行时会话 ID，或 `extra.session_id` |
| generation `modelParameters` | 调用参数，例如 `temperature`、`max_tokens`、工具数量等 |
| generation `usageDetails` / `costDetails` | Token 用量以及提供商/本地估算的成本 |
| generation `completionStartTime` | 开始时间加上提供商上报的首个内容到达时间指标（如有） |
| score `success` | 每个任务都会写入，成功为 `1`，失败为 `0` |

**Langfuse 数据集 → 基准测试字段映射：**

| Langfuse 数据集条目 | 基准测试 TaskSpec |
| --- | --- |
| `input` | `prompt` |
| trace metadata `user_goal` | 相同，或作为 `description_for_judge` 的参考 |
| 预期输出 / 人工标注 | `rubric` 检查项 |
| trace 工具调用次数 | 作为 `hard_assertions.min/max_tool_calls` 的参考 |
| 截图 artifact | `input_screenshot`（用于静态感知任务） |

### 4. 回归验证

```bash
cd benchmark
uv run python -m runner run --suite suites/phone_control_v1.json --agent-url http://device:8080
```

新任务加入套件后，使用基准测试运行器进行自动化回归；Langfuse 则持续收集新的生产 trace，形成闭环。

## 故障排查

| 现象 | 可能原因 |
| --- | --- |
| 日志 `[telemetry] export episode failed` | `base_url` 不可达、凭据错误、超时，或 Langfuse 版本不支持 OTLP 接入（`< 3.22.0`） |
| 日志 `langfuse rejected N span(s)` | Langfuse 接受了请求但丢弃了部分 span；由于其余部分已被接入，该批次不会重试 |
| Trace 中没有截图 / 显示 media not yet uploaded | Agent 没有 PATCH 上传状态（已修复）；或者 MinIO 预签名 URL 使用了 `localhost:9090`，设备无法访问；检查 Agent 日志 `[telemetry] screenshot upload failed` |

截图上传的完整流程：

1. Agent `POST {base_url}/api/public/media` 获取 `mediaId` 和预签名的 `uploadUrl`
2. Agent `PUT uploadUrl` 直接上传到 MinIO
3. Agent `PATCH {base_url}/api/public/media/{mediaId}` 写入 `uploadHttpStatus=200`（**缺少这一步会显示 media not yet uploaded**）
4. Agent `POST {base_url}/api/public/otel/v1/traces` 发送包含 `@@@langfuseMedia:...@@@` 的工具 observation

当 Agent 运行在 Luckfox 等设备上时，Langfuse 的 `.env` 必须使用该设备可以访问的 MinIO 地址：

```bash
LANGFUSE_S3_MEDIA_UPLOAD_ENDPOINT=http://192.168.50.246:9090
```

修改后，使用 `docker compose up -d` 重启 langfuse-web / langfuse-worker。
| 没有 trace | `telemetry.enabled=false`，或 episode 未提交（`user_goal` 为空） |

上报失败不会影响任务执行或本地记忆平面的写入。

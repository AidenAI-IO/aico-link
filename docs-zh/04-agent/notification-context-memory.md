---
sidebar_position: 12
---

# 通知持久化与自动记忆

本文介绍通知的消费、持久化与自动记忆提取流水线。Agent 消费来自 BLE 服务的通知事件，将其持久化到磁盘，并在空闲窗口内整合为临时记忆（Temporary）或长期记忆（Long-Term）。

## 架构

通知记忆系统使用一个共享的后台 Worker，串行处理 Episode 和 Notification 两个场景：

```text
MemoryWorker (idle-scheduled, serial execution)
├── EpisodeProcessor
└── NotificationProcessor
```

Worker 负责前台抢占、默认 5 分钟的空闲延迟、按顺序串行调用已注册的 Processor、待处理（pending）状态以及生命周期管理。Worker 不理解 Episode、Notification、记忆类型或 Store 语义，也不建立跨场景的优先级队列。

每个 Processor 自行负责读取策略、批大小、提取规则、提议（proposal）校验和持久化。两个 Processor 都会在 Worker 启动前完成注册，确保第一个空闲批次也遵循固定的 Episode → Notification 顺序。

`MemoryMergeEngine` 只抽取两个场景都需要的机械步骤：

```text
Scenario input + related Memory → build messages → call LLM → return raw proposal
```

Episode 和 Notification 的提议 schema、证据阈值和冲突解决仍保留在各自的 Processor 中。校验通过后，两个 Processor 都会向各自的 Store 适配器提交显式的 `MemoryIntent` 操作。Intent 的执行共享创建/更新/强化/删除语义，同时保留现有的 Device Memory 和 Temporary/Long-Term 记录 schema。

## 组件职责

| 模块                        | 职责                                                                                            |
| ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `ble_service`                 | 接收 iOS/Android 通知，进行规范化、分配事件 ID，维护短期事件环（event ring）        |
| `NotificationContext`         | 消费事件、去重、持久化为 JSONL，管理 source/memory 游标，执行清理                  |
| `MemoryWorker`                | 统一的空闲窗口、前台取消、串行执行 Processor、重试和停止                 |
| `episodeMemoryProcessor`      | 读取 Episode，按 Episode 提取规则，写入 Device Memory                                       |
| `NotificationMemoryProcessor` | 读取通知、过滤、调用提取、校验提议，写入 Temporary/Long-Term 记忆 |
| `MemoryMergeEngine`           | 召回相关记忆、构建消息、调用 LLM，返回原始提议                                  |

## 生命周期

通知首先进入 `ble_service` 事件环。当 Agent 进入空闲状态时，NotificationProcessor 消费事件环，将原始记录持久化到 NotificationContext，然后继续执行提取：

```text
Notification source → ble_service event ring
Agent foreground task ends → wait 5 minutes idle
                           → EpisodeProcessor (up to 5 Episodes)
                           → NotificationProcessor
                                → Consume / NotificationContext.append
                                → Extract up to 10 notifications
                           → Continue next batch in same idle window if still pending
```

通知发布时会唤醒共享 Worker 的空闲计时器。Worker 被调度后，Processor 先对 BLE 事件环调用 `Consume`，再读取自己的待处理数据。没有待处理工作时，返回空结果。

## Notification Context

`NotificationContext` 为运行时提供：

```text
Consume(limit)               Pull from events_since and persist reliably
ReadPending(limit)           Read records after memory cursor
CommitProcessed(batch)       Advance memory cursor after Memory write succeeds
CleanupProcessedBefore(...)  Only clean processed date shards
```

持久化记录保留 BLE 原始字段，并额外增加一个 Agent 本地单调递增的 `context_id`。memory 游标使用 `context_id`，不依赖 BLE generation。BLE 的 `source_cursor` 仅在当前 generation 内有效，而 `stored_cursor` 和 `memory_cursor` 始终是单调递增的 Agent 游标；BLE generation 重置后，出现 `source_cursor=0` 而 stored 游标更大的情况属于预期行为。重置诊断信息会同时保留新的 generation 和 `previous_generation`。

持久化使用按 UTC 日期分片的 JSONL。分片日期优先使用手机提供的通知 `date`（包括 ANCS 的 `YYYYMMDDTHHMMSS`），只有在手机日期缺失或无效时才回退到开发板的 `received_at`。清理和可回收空间估算在删除一个按旧日期命名的分片前，会逐条检查其中的记录。只要有一条近期或无法解析的记录，就会保护整个分片，这同样能保护那些因开发板时钟错误而写入错误文件名下的历史记录。source 游标只在写入成功后才前进。断电恢复时，完整写入的记录会被保留，最后一行不完整的 JSON 会被修复。

`CommitProcessed` 要求传入从当前游标开始的连续批次。清理器（Cleaner）必须调用 `NotificationContext` 的清理方法，不能绕过 Context 直接删除 JSONL，以免与 append/commit 产生竞争。

Agent 的 `shell` 工具可以只读访问 `/userdata/agent/memory/notifications/events/`。文件按 UTC 日期保存为 `YYYY-MM-DD.jsonl`，每行一条原始通知记录。按日期、应用、正文和数量的过滤可使用系统现有的只读 shell 工具完成。Agent 不得通过 shell 修改这些文件。

## Notification Processor

Processor 负责通知场景的完整业务流程：

1. 对当前 BLE 事件环执行 `Consume`，然后为本批次执行 `ReadPending`；
2. 合并同一条通知的 added/modified/removed 变更；
3. 从结构上过滤已移除和空的通知；
4. 为批次中的每条通知检索相关记忆，并合并为一次 `MemoryMergeEngine.Extract` 调用；
5. 将每条通知分类为用户特定事实、公共信息、营销、机密或一次性噪声，然后按 `context_id` 拆分并逐一校验提议；每条通知最多产生一个动作；
6. 在 Temporary 或 Long-Term Store 中执行动作（新增、更新、删除、强化或提升）；
7. 只有在所有相关记忆写入成功后才执行 `CommitProcessed`；如果仍有积压，则在同一空闲窗口内继续处理下一批，无需再等 5 分钟。

只有用户特定的状态、承诺、截止时间、预订、快递、账单、偏好和规则才有资格成为记忆。一般新闻、产品事实和公开价格即使属实也会被忽略。银行、健康、法律、消息等来源领域本身不构成拒绝理由。没有可用模型时，来源摄取和原始存储会继续进行，但 `memory_cursor` 保持不动，以便之后配置好模型的运行能对待处理的上下文进行分类。Processor 不会把通知业务规则放进 Worker，也不要求 Episode 和 Notification 使用相同的动作集合或状态机。

通知提议的动作包括 `ignore`、`add`、`update`、`reinforce`、`remove` 和 `promote`。更新已有记录需要准确的 `memory_id` 和 `memory_revision`。缺失、重复或未知的 `context_id` 等批次协议违规仍属于致命错误，不会推进 memory 游标。针对某条已知通知的格式错误提议会被记录日志并视为空操作，同批其他有效提议仍会被执行，已完成的来源批次也会推进。多个冗余的 `ignore` 动作会被规范化为一个 `ignore`。

Store 不会重新解释 Processor 的显式动作。`add` 直接使用 Processor 分配的 ID 创建记录，不做相似度搜索；`update`、`reinforce` 和 `remove` 只作用于指定 ID，并进行 revision 校验。独立的 `save_memory` 候选路径仍使用 Store 自己的重复检测，因为它不携带预先校验过的目标动作。

## Episode Processor

EpisodeProcessor 保留现有的 Episode 语义：

- 只接受已完成且带有设备证据的 Episode；
- 每批最多处理 5 个 Episode，并将整批 Episode 与相关 Device Memory 合并为一次 LLM 调用；
- LLM 返回带有 `episode_id` 的结果，Processor 随后按 Episode 逐条校验并持久化；
- 自行校验 `goal_result`、候选类型、证据引用和 revision；
- 自行执行 Device Memory 的新增、更新和冲突解决；
- 使用 Episode 处理台账保存 `processing → proposed → done/retry/ignored`，以支持崩溃恢复。

对于两个 Processor，优先级都由代码决定，以便跨场景召回拥有稳定的权重。置信度由模型针对每条结论给出，校验范围为 `(0, 1]`，仅在省略时默认取 `0.7`；显式的 0 是无效值。`update` 会用当前结论的元数据替换这两个值，而 `reinforce` 和 Notification 的 promote 会保留累积的更强元数据。

## 记忆与召回

```text
memory/
├── notifications/   # Raw notifications and cursors
├── temporary/       # Short-term conclusions with expires_at
├── long_term/       # Stable preferences, rules, and facts
├── device/          # Device knowledge produced by Episodes
└── episodes/        # Immutable Episode traces
```

Temporary 和 Long-Term 使用相同的可检索记录格式。`recall_memory` 会同时搜索两个 Store，并过滤已过期的 Temporary 条目；Device Memory 由 EpisodeProcessor 单独维护。

默认值：单条通知正文最多 4 KiB；单次 BLE 消费最多 100 条；Notification 单批最多 10 条；通知重放去重窗口最多保留 4096 个指纹；Temporary 默认 7 天；自动生成的 Long-Term 默认 90 天。Episode 空闲延迟由 `episode_memory_idle_delay_seconds` 配置，默认 300 秒；该值同时也是共享 Worker 的空闲窗口。

## 存储等级与隐私

`StorageMonitor` 统一管理存储等级和清理器。Normal/Warning/Critical 等级下分别启用 14/7/1 天的 Notification Context 清理；Emergency 会清理所有已处理的分片；任何等级都不会删除未处理的记录。Critical 和 Emergency 会禁用 `notification_context` 写入能力，此时 Processor 不再从 BLE 消费新通知，并在等级恢复后从原来的 source 游标继续。Temporary Memory 清理器从 Normal 等级起即可清理过期或已删除的记录。

通知正文会作为提取输入发送给当前配置的 LLM 提供商；只有配置了本地模型时，这些内容才会完全保留在设备上。日志只记录事件 ID、App ID、处理结果和缺口（gap），不记录正文内容。

## 故障与恢复

- BLE 消费失败：不推进 source 游标，保留待处理状态，在下次空闲时重试；
- LLM 批次协议失败或记忆写入失败：不推进 memory 游标；单条记录级别的提议校验失败会被隔离为空操作；
- 单个 Processor 失败：记录该场景的错误并保留其待处理状态，按空闲延迟退避，避免持续的 BLE/LLM/磁盘错误造成热循环；其他已注册场景在同一轮中仍可串行执行，且不共享业务事务；
- 前台任务开始：共享 Worker 取消进行中的模型调用；Processor 保留自身的持久化状态，并在下次空闲时继续；
- Worker 重启：Episode 从处理台账恢复，Notification 从 source/memory 游标恢复；
- Notification 的 add/update 逻辑必须保持幂等；如果进程在记忆写入成功之后、游标提交之前崩溃，下次运行会重新提取该记录；
- 同一 Notification 批次内的 Store 动作按顺序执行；I/O 中途失败时不推进游标，依靠固定 ID、revision 检查和重试实现收敛；
- 任何缺口都必须写入 `NotificationContextState.Gaps`，不得被静默吞掉。

## 验收标准

- Episode 和 Notification 由同一个 `MemoryWorker` 按注册顺序串行调度；每个 Processor 每批调用一次 LLM，然后在本地逐条校验并持久化结果；
- Agent 前台任务不会被后台记忆提取阻塞，并且可以取消进行中的模型调用；
- 两个 Processor 都可以独立决定批大小、提议结构和 Apply 逻辑；
- `MemoryMergeEngine` 不拥有跨场景的 Apply 语义；Store 适配器执行共享的显式 `MemoryIntent` 契约；
- 游标只在对应的记忆写入成功后才推进。

## 验证

在 `src/agent` 下运行核心回归测试套件：

```bash
go test ./internal/agent -count=1
go vet ./...
```

仍需要在真实设备上验证，以衡量真实模型提供商和 BLE 事件流下的通知提取质量、游标正确性以及前台 p50/p95 延迟。

## 相关文档

- [记忆平面](memory-plane.md)
- [Agent 上下文生命周期](context-lifecycle.md)
- [存储管理器](storage-manager.md)
- [BLE 服务](../03-services/ble-service.md)

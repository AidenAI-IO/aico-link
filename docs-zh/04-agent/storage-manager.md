---
sidebar_position: 10
---

# 存储子系统：StorageManager 和 StorageMonitor

## 概述

设备有两个相互独立的存储组件。它们共用 `[storage_settings.storage]` 配置段，但运行在不同的服务所有者中，解决的问题也不同。

| 组件 | 职责 | 主要 HTTP API |
| --- | --- | --- |
| `StorageManager`（Config Web） | 检测并挂载 SD 卡，推导出仅 eMMC 或双存储模式，路由受管数据，迁移较旧的数据，安全弹出卡，以及格式化卡 | Config Web `/api/storage/status`、`/api/storage/eject`、`/api/storage/format` |
| `StorageMonitor` | 采样持久存储容量，划分压力等级，执行清理，并在空间不足时降级非必要的写入 | `/api/storage/monitor/status`、`/api/storage/cleanup` |

`StorageManager` 并不取代 `StorageMonitor`。插入 SD 卡会为受管数据提供一个迁移层，而 `StorageMonitor` 继续保护配置的持久化根目录（默认为 `/userdata`）。

相关文档：

- [路径、产物与配置速查表](../02-architecture/paths-and-artifacts.md)
- [Agent 配置参考](configuration.md)
- [持久化 Python 软件包环境设计](python-packages.md)

## StorageManager：SD 卡与双存储路由

生产环境的 Config Web 进程会无条件地创建并启动 `StorageManager`。卡缺失、被拔出、被拒绝或不可用时，都会回退到仅 eMMC 运行；没有任何用户偏好可以强制指定存储模式。Agent 只会创建状态镜像的只读视图，从不接触 SD 卡硬件。

### 卡的生命周期与有效模式

管理器轮询配置的 SD 块设备，并对插入和拔出做防抖处理。当检测到卡时，它会：

1. 优先使用第一个分区；没有分区时回退到整个设备。
2. 识别文件系统，并运行 `fsck.<type> -p`，截止时间为 60 秒。未知文件系统或缺少检查工具时，回退到挂载/探测关卡。已纠正错误和建议重启的状态（ext4/exFAT 的 0–3）会继续进入该关卡。FAT 只接受状态 0/1；状态 2（用法错误）及更高的状态会拒绝该卡。I/O 失败、未纠正的错误和其他检查工具失败都会拒绝该卡。
3. 以读写方式把卡挂载到配置的挂载点。
4. 通过一个唯一的探测文件、`fsync` 和回读来验证挂载。已有的挂载必须属于所请求的设备。只读挂载视为不健康。
5. 尝试通过卸载来拒绝剩余空间低于配置最小值的卡。如果文件系统忙而无法卸载，管理器会保持该卡挂载，并将其报告为可用，而不是记录一个错误的未挂载状态。

默认挂载点为 `/mnt/sdcard`，默认设备设置为 `auto`，默认最小剩余空间为 64 MiB。设备发现会选择唯一一个值为 `SD` 的 `/sys/block/mmcblk*/device/type`；它会排除 eMMC（`MMC`）和 Wi-Fi（`SDIO`）。它不依赖内核的 `removable` 标志。Aico Link SCH v1 使用 `mmcblk1`；较旧的 Pico Zero 配置可能使用 `mmcblk2`。显式的设备覆盖设置会被保留；发现结果不唯一时必须提供覆盖设置。卡通常只有在通过挂载和写入检查后才会变为可用；上面所说的卸载失败（文件系统忙）情形是最小剩余空间拒绝规则的例外。

| 有效模式 | JSON 值 | 条件 |
| --- | --- | --- |
| 仅 eMMC | `1` | 没有挂载可用的卡 |
| 双存储 | `2` | 已挂载可用的卡 |

被明确识别为空白的卡，每次插入时会自动格式化为 FAT32 一次。已有文件系统但不受支持、已损坏或因其他原因无法挂载的卡，永远不会被自动擦除。空白检测还要求底层签名检查成功且起始扇区全为零；读取失败和 I/O 失败永远不会触发自动格式化。

管理器会把卡、格式化、迁移和有效模式的状态镜像到 `/run/aico/storage.state`，供 Agent 和其他设备服务使用。这个状态文件与位于 `/run/agent/storage_level` 的 StorageMonitor 等级文件相互独立。

### 数据路由与迁移

新的受管数据始终写入 eMMC。当前的生产集成对音频归档使用如下路由：

| 操作 | 未挂载 SD 卡 | 已挂载 SD 卡 |
| --- | --- | --- |
| 新的音频归档 | `/userdata/audio` | `/userdata/audio` |
| 读取音频归档 | `/userdata/audio` | 先 eMMC，再 `/mnt/sdcard/aico/audio` |
| 保留期清理根目录 | `/userdata/audio` | `/mnt/sdcard/aico/audio` |

把新写入保留在 eMMC 上，可以避免让正在进行的录音依赖可移动介质。当 SD 层可用且 eMMC 剩余空间低于启动水位时，管理器会把较旧的音频归档迁移到 SD 卡，从最旧的开始。当 eMMC 达到停止水位、没有符合条件的文件、卡变得不可用、下一次复制会越过配置的 SD 剩余空间预留，或者弹出或格式化操作取消了本次运行时，迁移就会停止。容量错误会停止本次运行，并把源文件保留在 eMMC 上。

默认的迁移水位为：

- eMMC 剩余空间低于 10% 时开始。
- eMMC 剩余空间达到 50% 时停止。

只有存在至少 60 秒的普通文件才符合条件。每个文件会被复制到一个 `.aico-partial` 路径，执行同步、校验大小、重命名到最终位置，然后才从 eMMC 删除。在两份副本可能同时存在的短暂窗口内，读取方优先使用 eMMC 上的副本。失败或已耗尽的运行使用十分钟的重试冷却时间。

### 安全弹出与格式化

安全弹出会取消正在进行的迁移，同步并卸载卡，并把卡锁定为已弹出状态。在被物理拔出并重新插入之前，它不会再次被挂载。如果同步报告回写错误，该失败会被记录，并且仍会尝试卸载，以便能够分离出故障卡或手动重新格式化。在分离成功但刷写出错的情况下，安全弹出会报告待写入的数据可能未保存，并保持弹出锁定。一旦分离成功，就可以继续进行格式化和重新配置。分离成功后，弹出端点返回 HTTP 200；如果这次弹出遇到了刷写失败，会附加 `warning_code: "storage_flush_failed"`。页面会显示本地化的移除/数据丢失警告。真正的弹出失败返回 HTTP 409。卸载失败会保留已挂载状态，并阻止格式化。存储状态中的 `last_warning` 会在管理器的整个生命周期内保留最近一次刷写警告，包括重新配置和重新挂载之后。状态镜像以 `LAST_WARNING` 的形式向只读使用方提供相同的警告。只要存在这条历史警告，页面也会显示一条本地化提醒；它不会把之后一次成功的弹出标记为刷写失败。

手动格式化具有破坏性：它会用全新的 MBR、一个分区和一个标签为 `AICO` 的新文件系统重写整张卡。支持的文件系统为 FAT32、ext4 和 exFAT。格式化以异步方式运行。`FormatDisk` 成功后，管理器会尝试挂载新的文件系统；由于格式化后的挂载仍可能失败，请检查 `card.mounted` 和 `effective_mode` 来确认双存储模式已恢复。

格式化程序会在修改 MBR 之前先解析所需的 `mkfs` 可执行文件。FAT32 使用 `mkfs.vfat -F 32`。Debian 包含 `dosfstools`、`e2fsprogs` 和 `exfatprogs`；镜像审计会检查它们的格式化和检查可执行文件。挂载/卸载/同步命令的截止时间为 15 秒，签名探测每次调用的截止时间为 3 秒，格式化的截止时间为 5 分钟。处于不可中断睡眠中的内核 I/O 仍可能超出用户态的截止时间。存储事件和拒绝原因会记录到 Config Web 日志中。已挂载的卡还会每 30 秒接受一次带同步的写入/回读探测。健康检查 I/O 在状态锁之外运行；I/O 或只读失败会分离 SD 层，并在重新插入之前阻止重复的挂载尝试。卡已满或配额耗尽时会保持挂载，报告容量问题并暂停迁移，以便现有文件仍可读取，并可以删除它们来回收空间。之后的一次成功探测会清除容量原因，无需重新插入。

### StorageManager HTTP API

这些端点用于报告和控制 SD/eMMC 管理器。它们与后文介绍的 StorageMonitor 端点不同。

方法错误时，三个端点都返回 HTTP 405；运行时中没有 StorageManager 时返回 HTTP 503。正常的生产运行时总会创建该管理器，即使没有插卡也是如此。

#### 获取管理器状态

~~~http
GET /api/storage/status
~~~

响应示例：

~~~json
{
  "effective_mode": 2,
  "card": {
    "present": true,
    "mounted": true,
    "device": "/dev/mmcblk1p1",
    "total_bytes": 68719476736,
    "free_bytes": 64424509440
  },
  "mount_point": "/mnt/sdcard",
  "format_job": {
    "status": "idle"
  },
  "migration": {
    "status": "idle"
  }
}
~~~

`format_job.status` 和 `migration.status` 的取值为 `idle`、`running`、`success` 或 `failed`。进行中和已完成的作业还会报告相应的文件系统、时间戳、详情、错误、已移动文件数和已移动字节数。

#### 安全弹出卡

~~~http
POST /api/storage/eject
~~~

成功时的响应是更新后的管理器状态。当卡无法弹出时，端点返回 HTTP 409，包括以下情况：没有挂载任何卡、正在进行挂载尝试或格式化、迁移无法及时停止，或者文件系统忙而无法卸载。

#### 格式化卡

~~~http
POST /api/storage/format
Content-Type: application/json
~~~

~~~json
{
  "fs": "fat32",
  "confirm": "format-sd-card"
}
~~~

`fs` 可以是 `fat32`、`ext4` 或 `exfat`；为空时默认为 `fat32`。由于格式化会擦除整张卡，必须提供完全一致的确认令牌。HTTP 202 表示异步作业已被接受，而不是格式化已经完成。请轮询 `GET /api/storage/status` 并查看 `format_job` 获取结果。无效请求返回 HTTP 400。

### StorageManager 配置

~~~toml
[storage_settings.storage]
mount_point = "/mnt/sdcard"
device = "auto" # Or an explicit mmcblk1/mmcblk2 override
min_card_free_mb = 64
migrate_start_free_pct = 10
migrate_stop_free_pct = 50
~~~

## StorageMonitor：容量保护

`StorageMonitor` 防止 Agent 因持久存储耗尽而出问题。它负责：

- 采样配置的存储根目录。
- 划分最终的存储等级。
- 按顺序执行清理阶段。
- 每个清理器执行后重新采样。
- 应用恢复滞回。
- 在压力下禁用非必要的写入。
- 在出现 ENOSPC 或 EROFS 错误后触发补救。
- 提供状态和手动清理 HTTP 端点。
- 把最终等级发布给部署侧的守卫。

通知的使用方、配套 App 的行为、LED 显示以及其他面向用户的交付策略不在 StorageMonitor 的范围之内。

### 能力

| 能力 | 状态 | 说明 |
| --- | --- | --- |
| 启动检查 | 可用 | Agent 运行时启动时运行一次 |
| 周期性监控 | 可用 | 默认间隔为 300 秒 |
| 分级划分 | 可用 | Normal、Warning、Critical、Emergency |
| 恢复滞回 | 可用 | 防止在阈值附近等级来回跳变 |
| 自动清理 | 可用 | LLM HTTP 日志、音频归档、会话归档 |
| 清理后重新采样 | 可用 | 最终状态始终来自 statfs |
| 降级写入控制 | 可用 | 暂停非必要的持久化 |
| 写入错误补救 | 可用 | 处理 ENOSPC 和 EROFS |
| 状态端点 | 可用 | `GET /api/storage/monitor/status` |
| 清理端点 | 可用 | `POST /api/storage/cleanup` |
| 部署状态文件 | 可用 | /run/agent/storage_level |
| Agent 日志集成 | 可用 | 默认路径为 /userdata/agent/log/agent.log |
| 通知事件输出 | 可用 | 通过全局 VoiceNotificationManager 发布 |
| LED 和 App 显示 | 不在范围内 | 使用方可以轮询状态端点 |

### 存储范围

#### 管理的根目录

StorageMonitor 管理一个配置的持久存储根目录。默认值为：

~~~text
/userdata
~~~

默认的 Agent 配置目录为 /userdata/agent。重要的持久化路径包括：

| 数据 | 默认路径 |
| --- | --- |
| Agent 主日志 | /userdata/agent/log/agent.log |
| LLM HTTP 日志 | /userdata/agent/log/llm-http-*.log |
| 会话数据 | /userdata/agent/memory |
| 会话归档 | /userdata/agent/memory/session_archive |
| 长期记忆 | /userdata/agent/memory/long_term |
| 音频归档 | /userdata/audio |
| Agent 配置 | /userdata/agent/agent.toml |

Agent 主日志已从 /var/log/agent/agent.log 移到 `<CONFIG_DIR>/log/agent.log`。使用默认配置目录时，解析后的路径为 /userdata/agent/log/agent.log。旧文件不会被迁移，也不会被用作回退。

其他服务可能仍在 /var/log 下写日志，包括 audio_service、frame_service 和 adb。这些日志不由 Agent StorageMonitor 的清理器管理。

#### 受保护的数据

StorageMonitor 不会删除：

- 当前活跃的会话。
- 长期记忆。
- Agent 或设备配置。
- 正在使用的 OTA 文件。
- 配置的清理器目录之外的任意文件。

它也不管理 RAM、CPU、温度、电池压力或易失性的 /tmp 使用量。

#### 与 OTA 分区的交互

OTA 使用一个挂载在 `/userdata/ota` 的专用 300 MiB ext4 分区。StorageMonitor 仍然采样 `/userdata` 文件系统，因此 OTA 下载不会减少 50/10/5 MiB 阈值所使用的可用字节数。

StorageMonitor 的清理器不针对 OTA 路径。SD 卡 StorageManager 不会路由或迁移 OTA 下载，也没有 OTA 锁或容量耦合。对于存放在 `/userdata` 上的非 OTA 数据，现有的清理阶段和面向用户的存储通知仍然是必要的。参见 [OTA 专用存储分区](../08-ota/no-space-plan.md)。

### 运行流程

~~~text
Startup check ───────┐
Periodic check ──────┤
ENOSPC / EROFS ──────┼→ StorageMonitor.CheckAndRemediate
Manual cleanup ──────┘              │
                                    ├→ statfs(root_path)
                                    ├→ classify initial level
                                    ├→ run eligible cleaners by priority
                                    ├→ statfs after every cleaner
                                    ├→ apply recovery hysteresis
                                    └→ publish final StorageMonitorStatus
                                           ├→ update write capabilities
                                           ├→ update storage_level file
                                           ├→ serve HTTP status
                                           └→ optionally publish an event
~~~

所有入口共用同一条补救路径。一个监控器级别的互斥锁会串行化各次检查，因此启动、周期性、写入失败和手动清理请求不会并发删除相同的文件。

### 检查触发条件

#### 启动

守护进程在创建 Agent 运行时之后调用 StartStorageMonitor。

StartStorageMonitor 会：

1. 以 startup 原因运行 CheckAndRemediate。
2. 即使初次检查报告了错误，也启动周期性监控循环。
3. 运行时关闭时，停止循环并删除部署状态文件。

#### 周期性监控

默认间隔为 300 秒。即使存储等级为 Normal，周期性检查也可以运行维护性清理器。例如，Normal 级别的 LLM HTTP 日志阶段会删除超出默认保留期的文件。

#### 写入错误

写入方会把与存储相关的错误交给 StorageMonitor。只有以下错误会安排一次立即的异步检查：

- ENOSPC：设备上没有剩余空间。
- EROFS：只读文件系统。

重复的失败会被合并。同一时间最多只有一个写入失败补救请求处于待处理状态。

#### 手动清理

POST /api/storage/cleanup 以 manual 原因使用同样的 CheckAndRemediate 流程。

- 当 force=false 时，即使当前等级为 Normal，Normal、Warning 和 Critical 清理阶段也都符合条件。仅限 Emergency 的阶段仍受限制，除非有效等级为 Emergency。当某个清理器执行后重新采样得到 Normal 时，流程仍会停止。
- 当 force=true 时，所选清理器会忽略普通的保留阈值，执行其强制清理行为。
- 强制清理仍然遵守清理器目录边界和受保护数据规则。

### 存储等级

等级基于可用字节数，而不是已用百分比。

| 等级 | 默认可用空间范围 | 行为 |
| --- | --- | --- |
| Normal | 大于 50 MB | 正常运行和低风险维护 |
| Warning | 大于 10 MB 且不超过 50 MB | Warning 阶段清理 |
| Critical | 大于 5 MB 且不超过 10 MB | 激进清理并暂停非必要写入 |
| Emergency | 不超过 5 MB | 在有效等级保持为 Emergency 期间，启用配置的紧急清理阶段 |

阈值比较使用精确字节数。恰好 10 MB 属于 Critical，恰好 5 MB 属于 Emergency。

#### 恢复滞回

等级升高立即生效。等级降低则要求可用空间超过上一等级的阈值加上 recovery_hysteresis_mb。

使用默认的 5 MB 滞回时：

| 之前的等级 | 降级所需的空间 |
| --- | --- |
| Warning | 大于 55 MB |
| Critical | 大于 15 MB |
| Emergency | 大于 10 MB |

这可以防止在可用空间在阈值附近波动时反复切换等级。

#### 最终状态规则

初次采样只决定是否开始补救。StorageMonitor 会在每个清理器执行后重新采样文件系统。

最终等级、不可用的能力、部署状态文件和 HTTP 响应都基于应用滞回后的最后一次 statfs 采样。清理器报告的已释放字节数会保留在审计历史中，但绝不会替代重新采样。

### 清理流水线

#### 清理器接口

~~~go
type StorageCleaner interface {
    Name() string
    Priority() int
    EstimateReclaimable(ctx context.Context) (uint64, error)
    Clean(ctx context.Context) (freed uint64, err error)
}
~~~

清理器按 Priority 升序运行。非强制检查通常会先调用 EstimateReclaimable，估算为零时跳过该清理器。清理器还可以额外实现 EmergencyClean，用于只有在有效等级为 Emergency 时才安全的行为。

#### 默认阶段

运行时根据配置的保留期数组构建清理阶段。

| 数据 | 默认阶段 | 最低等级 | 保护措施 |
| --- | --- | --- | --- |
| LLM HTTP 日志 | 7 天 | Normal | 保护当前会话的日志 |
| LLM HTTP 日志 | 3 天 | Warning | 保护当前会话的日志 |
| LLM HTTP 日志 | 1 天 | Critical | 保护当前会话的日志 |
| LLM HTTP 日志 | 0 天 | Emergency | 删除匹配的日志，但保留当前会话的日志 |
| 音频归档 | 30 天，最多保留 10 个 | Warning | 只涉及音频归档目录 |
| 音频归档 | 7 天，最多保留 3 个 | Critical | 只涉及音频归档目录 |
| 音频归档 | 0 天，不保留 | Emergency | 只涉及音频归档目录 |
| 会话归档 | 30 天 | Warning | 不涉及当前会话或长期记忆 |

保留期值为 0 会创建一个紧急清理阶段。session_archive_retention_days 的默认值为 [30]，因此除非显式加入 0，否则不会启用清理所有会话归档的紧急阶段。

#### 通知记忆阶段

当 Agent 配置目录可用时，运行时会注册临时记忆（Temporary Memory）和通知上下文（Notification Context）清理器。

| 数据 | 阶段 | 最低等级 | 保护措施 |
| --- | --- | --- | --- |
| 临时记忆 | 按记录的 `expires_at` | Normal | 只删除已过期的记录并重建其索引 |
| 通知上下文 | 14 天 | Normal | 只删除已被记忆处理过的日期分片 |
| 通知上下文 | 7 天 | Warning | 只删除已被记忆处理过的事件 |
| 通知上下文 | 1 天 | Critical | 只删除已被记忆处理过的事件 |
| 通知上下文 | 0 天 | Emergency | 删除所有已完全处理的分片；未处理的记录仍受保护 |

通知清理会感知游标。StorageMonitor 调用已注册的清理器，而文件选择由 `NotificationContext.CleanupProcessedBefore(...)` 负责；StorageMonitor 从不直接删除通知 JSONL。在 Critical 和 Emergency 等级下，监控器还会把 `notification_context` 标记为不可用，因此 Agent 会暂停新的 BLE 消费，直到存储等级恢复。已有的未处理 JSONL 仍受保护。

这并不是复用现有的 Episode 清理器。当前的 Episode 流水线没有针对 `memory/episodes/` 的保留期清理器；其有界的处理台账只会删除旧的终态状态。设备记忆的 TTL 同样只是召回过滤器，而不是物理存储回收。

#### Python 软件包清理

持久化 Python 环境注册了一个 `python_userbase` 清理器。

| 清理器 | Normal 和手动强制行为 | Emergency 行为 | 保护措施 |
| --- | --- | --- | --- |
| `python_userbase` | 删除 `/userdata/tmp` 下超过 24 小时的直接子项；保留整个 Python 用户目录 | 清空 `/userdata/agent/python`，重新创建为空目录，并保留近期的临时条目 | 从不跟随符号链接目标；低于 Emergency 时从不清空用户目录 |

手动清理接受 `python_userbase` 作为目标。低于 Emergency 时，`force=true` 仍会保留已安装的软件包。环境和清理的详细信息参见 [持久化 Python 软件包](python-packages.md)。

#### 阶段资格与停止条件

每个清理器执行后，StorageMonitor 都会重新采样并重新计算有效等级。

- 非强制补救在恢复到 Normal 后停止。
- 自动的启动、周期性和写入错误补救，会跳过最低等级高于重新计算后有效等级的清理器。
- 非强制手动清理会绕过 Normal、Warning 和 Critical 的最低等级限制，但保留 Emergency 限制。
- 如果某次清理让等级从 Emergency 回升到 Critical，后续仅限 Emergency 的阶段就不会再运行。
- 强制清理会继续执行所有选定的清理器。
- 清理器失败会被记录，但不会阻止后续清理器运行。

#### 清理重试间隔

自动清理默认的最小重试间隔为 60 秒，以避免在持续压力下反复扫描目录。

以下情况会绕过重试间隔：

- force=true。
- 手动清理。
- ENOSPC 或 EROFS 补救。

### 降级写入

Warning 会触发清理，但不会禁用能力。Critical 和 Emergency 会根据降级模式配置计算出不可用的能力。

| 能力 | 默认行为 |
| --- | --- |
| llm_http_log | 停止新的原始 LLM HTTP 日志 |
| audio_archive | 停止新的音频归档 |
| session_archive | 停止新的会话归档 |
| session_persistence | 保留内存中的会话，并把持久化标记为待处理 |

能力会在每次成功检查后重新计算，并在存储回到安全等级时自动恢复。

#### Agent 主日志

Agent 主日志不会通过 AllowWrite 被完全禁用。`aico-agent-log-retention.timer` 每分钟检查一次它。正常运行时，大于 10 MiB 的日志会被裁剪为最新的 5 MiB。在 Critical 或 Emergency 等级下，该定时器会改为把它裁剪到 `storage_settings.storage.degraded_mode.max_agent_log_mb`（默认 1 MiB）。裁剪会以原子方式折叠文件开头一个对齐的范围，保留 inode 和最新内容，因此 systemd 可以继续追加，而无需重启 Agent。由于文件系统对齐，保留的大小最多可能比目标少一个块。对于按字节设置的部署覆盖值，一个文件系统块是最小的有效上限；产品配置以 MiB 表示，因此不受影响。

### 状态模型

StorageMonitorStatus 是一次成功检查产生的不可变快照。

| 字段 | 含义 |
| --- | --- |
| Path | 管理的根目录 |
| TotalBytes, UsedBytes, AvailableBytes | 最终的 statfs 值 |
| PercentUsed | 已用空间百分比 |
| Level | 最终存储等级 |
| UnavailableCapabilities | 当前被暂停的写入 |
| Revision | 每次成功检查后递增 |
| LastCleanupAt | 最近一次清理器执行的时间 |
| LastCleanupFreedBytes | 最近一次清理流程报告的字节数 |
| CleanupHistory | 最近的清理器结果，最多 50 条 |
| CheckedAt | 最终采样时间 |

Status 返回切片字段的副本，因此调用方无法修改监控器的内部状态。

#### 部署状态文件

每次成功检查都会以原子方式更新：

~~~text
/run/agent/storage_level
~~~

该文件内容为 normal、warning、critical 或 emergency。部署侧的守卫用它来应用 Agent 主日志上限。

禁用或停止 StorageMonitor 时，会同时删除状态文件及其临时文件，以免部署脚本读取到过期状态。

### StorageMonitor HTTP API

#### 获取状态

~~~http
GET /api/storage/monitor/status
~~~

响应示例：

~~~json
{
  "path": "/userdata",
  "total_mb": 3072,
  "used_mb": 3027,
  "available_mb": 45,
  "percent_used": 98.5,
  "alert_level": "warning",
  "degraded_mode": false,
  "unavailable_capabilities": [],
  "status_revision": 12,
  "last_cleanup": "2026-07-22T08:30:00Z",
  "last_cleanup_freed_mb": 32,
  "cleanup_history": [
    {
      "timestamp": "2026-07-22T08:30:00Z",
      "cleaner": "llm_http_log_7d",
      "freed_mb": 32,
      "status": "success"
    }
  ]
}
~~~

该端点提供适合轮询的一致快照。实时推送和用户界面显示不在本模块范围内。

#### 执行清理

~~~http
POST /api/storage/cleanup
Content-Type: application/json
~~~

请求示例：

~~~json
{
  "force": false,
  "targets": ["llm_http_log", "audio_archive"]
}
~~~

targets 可以为空，也可以包含以下清理类别：

- llm_http_log
- audio_archive
- session_archive

也接受完整的清理器名称。未知类别、缩写、未知 JSON 字段、尾随的 JSON 文档以及大于 64 KB 的请求体都会返回 HTTP 400。

响应示例：

~~~json
{
  "success": true,
  "freed_mb": 45,
  "final_alert_level": "normal",
  "available_mb": 90,
  "cleaners_run": [
    {
      "name": "llm_http_log_7d",
      "freed_mb": 45,
      "status": "success"
    }
  ]
}
~~~

freed_mb 只报告当前这次 POST 请求所执行的清理器。它不会重复上一次清理释放的空间值。

### StorageMonitor 配置

默认配置：

~~~toml
[storage_settings.storage]
monitor_enabled = true
root_path = "/userdata"
check_interval_seconds = 300
warning_threshold_mb = 50
critical_threshold_mb = 10
emergency_threshold_mb = 5
recovery_hysteresis_mb = 5

[storage_settings.storage.degraded_mode]
disable_llm_http_log = true
disable_audio_archive = true
disable_session_archive = true
max_agent_log_mb = 1

[storage_settings.storage.cleanup]
enabled = true
llm_http_log_retention_days = [7, 3, 1, 0]
audio_archive_retention_days = [30, 7, 0]
session_archive_retention_days = [30]
notification_context_retention_days = [14, 7, 1, 0]
cleanup_retry_interval_seconds = 60
~~~

校验规则：

- 当 storage.monitor_enabled 为 true 时，root_path 为必填项。
- check_interval_seconds 必须大于零。
- 阈值必须满足 emergency < critical < warning。
- max_agent_log_mb 必须大于零。
- cleanup_retry_interval_seconds 不能为负数。
- 保留期值不能为负数。通知上下文清理只删除已完全处理的日期分片；即使是强制清理，比 `memory_cursor` 更新的记录也仍受保护。

### 可选的事件输出

StorageMonitor 提供一个 VoiceNotificationSink 接口，用于输出最终的存储状态事件。Agent 运行时注入全局的 VoiceNotificationManager 作为生产环境的使用方。

- 非 Normal 状态发布 active。
- 严重程度变化会更新稳定的 storage:device 状况。
- 恢复到 Normal 时发布 resolved。
- 周期性检查可以刷新一个等价的 active 事件。
- 发布失败不会回滚清理或 StorageMonitorStatus。

排队、显示、播放、去重和交付策略仍由 VoiceNotificationManager 负责。

### 测试

当前测试套件覆盖：

- 精确字节的阈值边界。
- 恢复滞回。
- 清理后重新采样以及最终状态发布。
- 清理重试间隔。
- 手动清理和强制清理行为。
- 运行时清理器顺序和最低等级。
- 能力降级与恢复。
- ENOSPC 和 EROFS 补救。
- 部署状态文件的生命周期。
- 事件激活、严重程度更新、解除和重试。
- GET 状态的一致性和清理历史。
- POST 清理的目标和 JSON 校验。
- Critical 和 Emergency 下的 Agent 主日志上限。

常用检查：

~~~bash
cd src/agent
go test ./internal/agent
~~~

### 当前限制

- 只管理一个 root_path。
- 不清理 /var/log 下其他服务的日志。
- 状态 API 支持轮询，但不支持实时推送。
- LED、配套 App 和完整的 Web UI 显示不在本模块范围内。

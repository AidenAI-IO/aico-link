---
sidebar_position: 13
---

# 实时活动 / 灵动岛

实时活动（Live Activity）是 Aico 的 iOS 任务状态面板，也是返回配套 App 的系统入口。任务状态只保留在开发板和手机本地。Aico 不会注册 ActivityKit 推送 token，不会把任务状态发布到 Aico 托管的后端，也不会使用 APNs 更新实时活动。

## 本地更新流程

前台和后台更新读取的是同一份 Agent 快照：

```text
Agent task state changes
        |
        | coalesced BLE Wake (reason=live_activity, no task text)
        v
iOS CoreBluetooth wakes the companion app briefly
        |
        | USB ECM HTTP GET /api/live-activity/current?phone_id=<stable>
        v
Swift native layer parses the latest snapshot
        |
        v
Activity.request / Activity.update
```

BLE 值仍是 [BLE 服务](../04-services/ble-service.md)中记录的 12 字节 Wake 封装。它只包含协议版本、原因和一个单调递增的序列号。任务标题、步骤、目标、错误和结果永远不会出现在 GATT 通知中。手机通过 USB ECM 链路从 `192.168.42.1` 读取这些字段。

Agent 会合并状态变化引起的 Wake 通知，最多大约每 750 ms 发送一次。这样既能及时反映步骤更新，又不会为每个流式 token 或字符都发送一条 BLE 通知。iOS 端总是获取最新的快照，因此被跳过的中间 Wake 通知不会导致最终状态丢失。

## 行为

- `ready` / `connected`：Aico 可用。App 在进入后台之前或进入后台期间创建这个待机实时活动，使灵动岛始终是返回 Aico 配套 App 的入口。
- `running`：显示任务标题、当前步骤、阶段、进度、停止状态，以及（如有）详细的 `tool_status` 生命周期。
- `needs_app`：当某个操作需要配套 App 位于前台时，请用户返回 Aico 配套 App。
- `waiting_user` 且 `current_action=request_user_input`：在 `request_user_action` 之后保持转交说明可见，以便用户在手机上接手操作。
- `completed` / `failed` / `canceled`：在现有实时活动中保留终态文本，而不是立即替换为待机状态。

App 只在高价值的状态转换时请求一次性系统提醒：首次建立 USB ECM 连接、人工转交、恢复配套 App 到前台、完成和失败。普通的工具调用、进度变化和重复快照会静默更新。提醒按请求和状态转换去重；一个明确的新 USB 连接会话会获得新的连接事件 ID。

在 App、Agent Web UI 或其他本地 Agent 入口启动的任务使用相同的快照和本地更新路径。

## 隐私与可用性边界

- 不会请求或注册 ActivityKit 推送 token 或 push-to-start token。
- Agent 中没有任何中继、注册或直接的 APNs 发布路径。
- BLE 不携带任务内容。携带内容的请求只在 USB ECM 子网内进行，并且仅限连接到开发板的手机。
- 实时活动并不会授予 iOS 持续的后台执行权限。BLE Wake 只会打开一个由 CoreBluetooth 触发的短暂执行机会。
- 可靠的后台刷新需要一个活跃的 BLE Wake 订阅，以及一条通往 `192.168.42.1:8080` 的可用 USB ECM 路径。
- App 应在被挂起之前创建待机实时活动。如果用户强制退出 App、移除蓝牙权限、断开 USB，或 iOS 拒绝后台执行，更新会一直等到 App 再次打开。
- 有意不提供 APNs 回退，也没有远程 push-to-start 路径。

## 状态模型

当异步聊天请求提供了 `request_id` 时，Agent 会为该请求维护一份 `live_activity` 快照：

```json
{
  "request_id": "chat_...",
  "status": "running",
  "phase": "observing",
  "task_title": "Open Settings",
  "current_step": "Checking the current screen",
  "current_action": "observe_screen",
  "current_target": "",
  "last_tool_name": "screenshot",
  "tool_status": "running",
  "tool_started_at": "2026-06-12T07:00:01Z",
  "progress": 0.21,
  "shows_progress": true,
  "can_stop": true,
  "requires_app": false,
  "started_at": "2026-06-12T07:00:00Z",
  "updated_at": "2026-06-12T07:00:02Z"
}
```

状态值：

- `running`：任务正在执行。
- `needs_app`：用户必须返回配套 App。
- `completed`：任务已完成。
- `failed`：任务失败；`last_error` 可能包含一段可安全显示的摘要。
- `canceled`：请求已被取消或中断。

结构化字段包括：

- `phase`：`planning`、`observing`、`acting`、`phone_bridge`、`waiting_app`、`waiting_user`、`verifying` 或 `answering`。
- `current_action`：机器可读的操作，例如 `open_app`、`observe_screen`、`control_phone`、`clipboard` 或 `calendar`。
- `current_target`：简短的显示目标。不应把敏感输入放入此字段。
- `current_app`：已知时为当前目标 App。
- `requires_app`：用户是否必须把配套 App 恢复到前台。
- `tool_status`：详细的生命周期状态。当前取值为 `processing`、`thinking`、`running`、`preparing`、`verifying`、`succeeded`、`failed`、`retrying`、`waiting_app` 和 `waiting_user`。终态仍以任务级的 `status` 为准。
- `tool_started_at`：当前正在计时的工具的可选开始时间。iOS 客户端用它在本地渲染已用时间，并在非计时状态和终态下清除计时器。

普通模型输出使用 `tool_status=processing`。只有非空的流式推理增量才会把它改为 `thinking`；实时活动使用安全的固定文案，不显示原始推理流。成功的 App 启动请求会一直使用 `verifying`，直到之后的屏幕观察能确认目标 App 已可见。

Device CLI 会在 shell 内执行每项设备操作时更新当前任务。一次批处理可以在一个 BLE 更新间隔内执行多项操作。卡片显示最新动作，并在 Bridge 操作需要时提示用户返回 Aico。CLI 进度使用本地化标签，省略原始命令、工具输出和私密参数。

`phone_app_state` 事件还会报告 `return_entry` 和 `return_entry_available`。当 iOS App 处于后台或非活动状态时，它可以报告 `return_entry="dynamic_island"`，让开发板端的恢复逻辑知道有一个可见的配套 App 入口可以把用户带回 App。

## API

### 查询单个请求

```http
GET /api/live-activity/status?request_id=<id>
```

响应：

```json
{
  "status": "ok",
  "live_activity": {
    "request_id": "chat_...",
    "status": "running",
    "task_title": "Open Settings",
    "current_step": "Checking the current screen",
    "progress": 0.21,
    "shows_progress": true,
    "can_stop": true
  }
}
```

### 查询当前任务

原生 BLE Wake 处理程序使用：

```http
GET /api/live-activity/current?phone_id=<stable-phone-id>
```

当不存在匹配的活动任务或保留任务时：

```json
{"status":"not_found"}
```

`GET /api/chat/result?request_id=<id>&offset=<n>` 也包含 `live_activity` 字段，因此前台聊天轮询可以通过同一个本地原生模块更新 ActivityKit。

## 配置

Agent 唯一需要的设置是保持实时活动快照处于启用状态：

```toml
[advanced_settings.runtime.live_activity]
enabled = true
```

`enabled` 就是实时活动的全部配置项。Agent 没有后端 URL、APNs 凭据、注册 token 或开发板 ID 之类的设置。

iOS App 可以选择设置 `LIVE_ACTIVITY_PHONE_ID`。为空时，原生层会把 `identifierForVendor` 持久化为稳定的手机 ID，用于过滤开发板快照。

## Apple 开发者前提条件

- 为 App 启用 Live Activities，并包含 `AidenLiveActivityExtension` Widget Extension。
- 这一纯本地流程不需要 Push Notifications 能力和 APNs 凭据。
- 启用现有 BLE Wake 服务所需的蓝牙后台模式，并在真实 iPhone 上测试；模拟器无法验证 USB ECM 或 CoreBluetooth 后台投递。

## 验证

1. 将 iPhone 与开发板配对，并通过 `/api/bluetooth/status` 确认 `wake_subscriber=true`。
2. 通过 USB 连接手机，确认它可以访问 `http://192.168.42.1:8080/api/live-activity/current`。
3. 将 App 切到后台，确认已存在一个 `Aiden Ready` 实时活动。
4. 从 Agent Web UI 启动一个任务，确认卡片在不重新打开 App 的情况下变为 `running`。
5. 触发 `request_user_action`，确认卡片只提醒一次、显示建议操作，并在 Agent 轮次停止后保持在转交状态。
6. 确认完成和失败的文本保持可见并只提醒一次；普通的进度和工具更新不得触发提醒。
7. 检查 App 和 Agent 日志中的 `live_activity` Wake 投递和本地开发板同步记录。
8. 从回环地址和 USB ECM 子网之外查询 `/api/live-activity/current`，确认请求被以 HTTP `403` 拒绝。

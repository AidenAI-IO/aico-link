---
sidebar_position: 12
---

# 手机桥接协议约定

**版本**：1.2
**日期**：2026-08-13

本文档定义硬件开发板（aico-link-firmware）与手机 App（`aiden-app` 检出）之间的 WebSocket 命令协议。

## 连接

- **URL**：`ws://192.168.42.1:8080/api/phone-bridge?platform=ios`（或 `platform=android`）
- **方向**：App 作为 WebSocket 客户端，主动连接开发板（WebSocket 服务端）
- **网络**：经由 USB ECM 建立的 `192.168.42.0/24` 子网，开发板固定 IP 为 `192.168.42.1`

WebSocket 是前台快速路径。开发板还提供 `/api/phone-bridge/commands` 和
`/api/phone-bridge/results` 两个 HTTP 队列端点。不能把 iOS 后台中的 React Native
定时器和 WebSocket 当作永久的执行路径，因此 Agent 会根据实时能力，在前台 WebSocket、
灵动岛恢复、PiP Bridge、Android FGS Bridge 和 iOS BLE Wake 之间进行选择。
锁屏实时活动入口需要视觉确认，而不是盲点。

iOS BLE Wake 是一条按需路由，用于 `calendar_create`、`calendar_query`、
`calendar_delete`、`contacts_query`、`contacts_create` 和
`notification_send`。开发板先将命令持久化入队，然后发送一个非敏感的 12 字节
GATT Wake 提示。App 打开一个短暂的后台窗口，用其稳定的 `phone_id` 轮询 HTTP 队列，
执行原生命令，并通过 HTTP 提交结果。BLE 从不承载命令或结果载荷，因此使用 BLE Wake
时手机必须保持连接在开发板的 USB ECM 网络上。该路由不支持剪贴板读写和联系人更新。

PiP Bridge 是一种范围很窄的后台队列模式。当 App 在后台上报 `pip_bridge_enabled=true` 时，PiP 会话静默运行，灵动岛返回入口保持可见。在该状态下，公开的 `open_app` 工具会选择可见的系统搜索，而不是其内部的 Phone Bridge 路由；`open_url` 等仅限前台的命令会通过已上报的灵动岛入口恢复配套 App；只有由命令类型（`clipboard_*`、`calendar_*`、`contacts_*`、`notification_send`）支撑的后台安全数据工具（`bridge_clipboard`、`bridge_calendar`、`bridge_contacts`、`bridge_notification`）可以使用 HTTP 队列。

Android FGS Bridge 也是一种 HTTP 队列模式。前台服务轮询 `/api/phone-bridge/commands?platform=android&phone_id=<stable>&app_state=background&fgs_bridge_enabled=true&limit=10`；开发板将其视为仅用于后台安全数据命令的后台队列消费者。它不把 WebSocket 当作可靠的 Android 后台传输，也不通过 FGS 队列暴露 `open_app`。

## 心跳

当前 App 每 5 秒发送一次心跳（id 为 `"heartbeat"` 或 `"ping"` 的 JSON），
开发板原样回送。App 以此检测连接是否存活。

示例：
```json
{"id": "heartbeat"}
```

开发板记录 `last_heartbeat_at` 时间戳；超过 60 秒没有心跳即视为连接不健康。

## 消息格式

### BridgeCommand（开发板 → App）

```typescript
{
  id: string;              // Unique command ID
  type: string;            // Command type (see below)
  timeout_ms?: number;     // Timeout milliseconds (optional, default 5000)

  // Following fields used based on type
  app?: string;                  // open_app semantic app name or alias
  url?: string;                  // open_app URL using http, https, sms, mailto, or tel, sent by public open_url
  payload?: object;              // JSON payload for other command types
}
```

### BridgeCommandResponse（App → 开发板）

```typescript
{
  id: string;          // Matches BridgeCommand.id
  method?: string;     // Execution method (optional)
  error?: {            // Omitted on success
    category: string;
    code: string;
    message: string;
    details?: object;
  };
  data?: object;       // Return data (optional, used by read commands)
}
```

成功以不存在 `error` 字段表示；当前协议不发送 `ok` 布尔值。

### AppEvent（App → 开发板）

App 也可以主动发送事件消息。事件复用 `BridgeCommandResponse` 的外层字段，但 `id`/`method` 不对应任何开发板下发的命令；开发板不会把它当作待处理命令的确认。

当前事件：

- `phone_environment`：App 在 WebSocket 连接成功以及从后台回到前台时上报手机环境快照。
- `phone_app_state`：App 的可见生命周期状态在 `active`、`background` 和 `inactive` 之间变化时，App 上报最新状态，同时上报是否有可返回 Aico 配套 App 的实时活动/灵动岛入口，以及是否启用了 PiP Bridge 模式。该状态用于诊断和策略决策；它并不意味着 App 可以在 iOS 后台永久执行。
- `phone_orientation`：iOS App 上报手机的实际握持方向，该方向读取自 CoreMotion 重力向量；每当稳定在一个新值时上报，WebSocket 连接时也会再上报一次。处于后台的 App 通常改用 HTTP 轮询，因此 `GET /api/phone-bridge/commands` 也接受以 `device_orientation` 传递的同一个值；在没有 WebSocket 时，通过 HTTP 携带的值在 10 秒内保持有效。取值为 `portrait`、`portrait_upside_down`、`landscape_left`、`landscape_right`（与 `UIDeviceOrientation` 对应）以及 `flat`。它描述的是设备本身，而不是前台 App 把界面转向了哪个方向。开发板记录该值用于诊断；iOS 触控板则根据触摸在屏幕上可见的响应位置来确认其横屏方向（参见 [USB HID](../04-services/usb-hid.md#横屏下的-ios-触控板)）。

示例：

```json
{
  "id": "phone_environment",
  "method": "phone_environment",
  "data": {
    "captured_at": "2026-06-10T03:20:00Z",
    "source": "aico-app",
    "platform": "ios",
    "system_name": "iOS",
    "system_version": "18.5",
    "is_tablet": false,
    "locale": "zh-Hans-CN",
    "language": "zh",
    "region": "CN",
    "time_zone": "Asia/Shanghai",
    "utc_offset_minutes": 480,
    "utc_offset": "+08:00",
    "uses_24_hour_clock": true,
    "manufacturer": "Apple",
    "brand": "Apple",
    "model": "iPhone16,2",
    "screen": {
      "width": 393,
      "height": 852,
      "width_pixels": 1179,
      "height_pixels": 2556,
      "scale": 3
    },
    "battery": {
      "level": 0.87,
      "charging": true,
      "state": "charging"
    },
    "system_apps": [
      {"name": "Camera", "available": true, "category": "system", "availability_source": "builtin"},
      {"name": "Contacts", "available": true, "category": "system", "availability_source": "builtin"}
    ],
    "third_party_apps": [
      {"name": "WeChat", "available": true, "category": "third_party", "availability_source": "can_open_url", "ios_url": "weixin://"},
      {"name": "Douyin", "available": false, "category": "third_party", "availability_source": "can_open_url", "ios_url": "snssdk1128://"}
    ],
    "available_apps": [
      {"name": "WeChat", "available": true, "category": "third_party", "ios_url": "weixin://"},
      {"name": "Douyin", "available": false, "category": "third_party", "ios_url": "snssdk1128://"}
    ]
  }
}
```

开发板将最新的完整环境写入 `GET /api/phone-bridge/status` 的 `environment` 字段，
并保留 `app_state`、`return_entry`、`return_entry_available`、`pip_bridge_enabled`
和 `fgs_bridge_enabled`，供 Agent 运行时上下文和内部路由使用。状态中还报告
`hid_connection_id`，这是开发板为当前物理 USB HID 会话生成的标识符。它在 WebSocket
和 HTTP 传输切换时保持不变，并在 UDC 报告 USB 主机断开时被清除。运行时上下文携带
紧凑的状态事实，例如连接状态、App 前台/后台状态、返回入口可见性、PiP/灵动岛可见状态、
Android FGS Bridge 状态、系统类型/版本、语言/地区/时区、屏幕尺寸，以及已确认可打开的
第三方候选 App。每次对话运行前，Agent 会根据实时的 Phone Bridge 和 BLE 能力，单独
过滤仅限 App 的工具和操作。`open_app` 始终可用，因为它可以回退到可见的系统搜索；
iOS BLE Wake 不包含剪贴板读写和联系人更新。WebSocket 断开时，非屏幕的环境数据会被
清除。屏幕尺寸可能会针对同一个 `hid_connection_id` 保留，并在该物理 USB 会话结束时
失效；最新的 App 前台/后台状态也可能被保留，用于灵动岛恢复。

`phone_app_state` 示例：

```json
{
  "id": "phone_app_state",
  "method": "phone_app_state",
  "data": {
    "app_state": "background",
    "return_entry": "dynamic_island",
    "return_entry_available": true,
    "pip_bridge_enabled": true,
    "reported_at": "2026-06-10T03:20:05Z"
  }
}
```

`phone_orientation` 示例：

```json
{
  "id": "phone_orientation",
  "method": "phone_orientation",
  "data": {
    "orientation": "landscape_left",
    "reported_at": "2026-06-10T03:20:07Z"
  }
}
```

`system_apps` 表示系统内置 App/能力；在 iOS 上不依赖 `canOpenURL` 判断是否存在。`third_party_apps` 表示第三方候选 App，在 iOS 上通过 `canOpenURL` 探测，在 Android 上通过包是否可启动探测。`available_apps` 是为兼容旧开发板保留的遗留第三方候选摘要；新实现应优先读取拆分后的字段。

## 命令类型

### 1. `open_app`

打开指定的 App 或 URL。

**请求**：
```json
{
  "id": "open_001",
  "type": "open_app",
  "app": "WeChat",
  "timeout_ms": 10000
}
```

**iOS 实现**：在配套 App 内解析语义化的 App 或 URL 请求，然后打开匹配的 iOS URL scheme 或系统 URL。
**Android 实现**：在配套 App 内解析语义化的 App 或 URL 请求，然后启动匹配的包、intent URI 或系统 URL。

开发板发送语义化的启动目标，由配套 App 解析平台细节。内部的 BridgeOpenApp 路由发送 `app`（例如 `"WeChat"` 或 `"weixin"`），而 `open_url` 发送 `http`、`https`、`sms`、`mailto` 或 `tel` URL。每条命令只设置一个目标字段。

**响应**：
```json
{
  "id": "open_001",
  "method": "ios_url_scheme"
}
```

`method` 表示 App 端使用的底层机制，常见取值包括 `ios_url_scheme`、`ios_shortcut`、`android_intent`、`android_deeplink`、`launch_package` 和 `open_url`。这里的 `open_url` 表示一个显式的受支持 URL。
Agent 暴露的 `open_app` 和 `open_url` 工具会将配套 App 的底层机制归一化为面向任务的 `method` 值，并把底层值放在 `mechanism` 字段中。

失败时：
```json
{
  "id": "open_001",
  "error": {
    "category": "precondition_failed",
    "code": "app_not_installed",
    "message": "App not installed"
  }
}
```

---

### 2. `clipboard_read`

读取系统剪贴板内容。

**请求**：
```json
{
  "id": "clip_read_001",
  "type": "clipboard_read",
  "timeout_ms": 5000
}
```

**iOS 实现**：`UIPasteboard.general.string`
**Android 实现**：`ClipboardManager.getPrimaryClip()`

**响应**：
```json
{
  "id": "clip_read_001",
  "data": {
    "text": "clipboard content"
  }
}
```

剪贴板为空时返回 `"text": ""`。

**权限**：
- iOS 16+ 会显示粘贴横幅，频繁读取会打扰用户
- Android 10+ 只有前台 App 可以读取剪贴板，后台需要前台服务

---

### 3. `clipboard_write`

写入系统剪贴板。

**请求**：
```json
{
  "id": "clip_write_001",
  "type": "clipboard_write",
  "payload": {
    "text": "content to copy"
  },
  "timeout_ms": 5000
}
```

**iOS 实现**：`UIPasteboard.general.string = payload.text`
**Android 实现**：`ClipboardManager.setPrimaryClip(ClipData.newPlainText("label", text))`

**响应**：
```json
{
  "id": "clip_write_001",
  "method": "clipboard_write"
}
```

---

### 4. `calendar_create`

创建日历事件。

**请求**：
```json
{
  "id": "cal_create_001",
  "type": "calendar_create",
  "payload": {
    "title": "Dentist appointment",
    "start_at": "2026-06-02T15:00:00+08:00",
    "end_at": "2026-06-02T16:00:00+08:00",
    "all_day": false,
    "location": "Clinic",
    "notes": "Bring insurance card"
  },
  "timeout_ms": 8000
}
```

**字段说明**：
- `title`（必填）：事件标题
- `start_at`（必填）：开始时间（带时区的 RFC3339 格式）
- `end_at`（必填）：结束时间（带时区的 RFC3339 格式）
- `all_day`（可选）：是否为全天事件，默认 false
- `location`（可选）：地点
- `notes`（可选）：备注

**iOS 实现**：使用 `EventKit` 框架，需要 `NSCalendarsUsageDescription` 或 `NSCalendarsWriteOnlyAccessUsageDescription` 权限。
**Android 实现**：使用 `CalendarContract` API，需要 `WRITE_CALENDAR` 权限。

**响应**：
```json
{
  "id": "cal_create_001",
  "data": {
    "event_id": "ios_calendar_id_123"
  }
}
```

`event_id` 是平台返回的事件唯一标识，用于后续删除。

---

### 5. `calendar_query`

查询指定时间范围内的日历事件。

**请求**：
```json
{
  "id": "cal_query_001",
  "type": "calendar_query",
  "payload": {
    "from": "2026-06-02T00:00:00+08:00",
    "to": "2026-06-03T00:00:00+08:00"
  },
  "timeout_ms": 8000
}
```

**iOS 实现**：`EKEventStore.events(matching:)` 查询 `from` 到
`to` 的范围。
**Android 实现**：查询 `CalendarContract.Instances` 表，需要 `READ_CALENDAR` 权限。

**响应**：
```json
{
  "id": "cal_query_001",
  "data": {
    "events": [
      {
        "event_id": "ios_calendar_id_123",
        "title": "Dentist appointment",
        "start_at": "2026-06-02T15:00:00+08:00",
        "end_at": "2026-06-02T16:00:00+08:00",
        "location": "Clinic"
      }
    ]
  }
}
```

没有事件时返回空数组 `"events": []`。

---

### 6. `calendar_delete`

删除指定的日历事件。

**请求**：
```json
{
  "id": "cal_delete_001",
  "type": "calendar_delete",
  "payload": {
    "event_id": "ios_calendar_id_123"
  },
  "timeout_ms": 8000
}
```

**iOS 实现**：`EKEventStore.remove(event:, span:, commit:)`
**Android 实现**：`ContentResolver.delete(CalendarContract.Events.CONTENT_URI, ...)`

**响应**：
```json
{
  "id": "cal_delete_001",
  "method": "calendar_delete"
}
```

如果事件不存在，App 会返回结构化的 `invalid_arguments` 错误，并在
`details.native_code` 中携带原生的未找到错误码。

---

### 7. `contacts_query`

查询联系人。

**请求**：
```json
{
  "id": "contacts_query_001",
  "type": "contacts_query",
  "payload": {
    "query": "Zhang San",
    "limit": 20
  },
  "timeout_ms": 8000
}
```

**字段说明**：
- `query`（可选）：搜索关键词，匹配姓名或电话号码
- `limit`（可选）：最大返回数量，默认 20

**iOS 实现**：使用 `CNContactStore` 查询，需要 `NSContactsUsageDescription` 权限。
**Android 实现**：查询 `ContactsContract` API，需要 `READ_CONTACTS` 权限。

**响应**：
```json
{
  "id": "contacts_query_001",
  "data": {
    "contacts": [
      {
        "contact_id": "contact_123",
        "name": "Zhang San",
        "phone_numbers": ["+86 138 1234 5678"],
        "emails": ["zhangsan@example.com"]
      }
    ]
  }
}
```

没有匹配的联系人时返回空数组 `"contacts": []`。

---

### 8. `contacts_create`

添加新联系人。

**请求**：
```json
{
  "id": "contacts_create_001",
  "type": "contacts_create",
  "payload": {
    "name": "Li Si",
    "phone_numbers": ["+86 139 8765 4321"],
    "emails": ["lisi@example.com"],
    "organization": "Company name",
    "notes": "Notes"
  },
  "timeout_ms": 8000
}
```

**字段说明**：
- `name`（必填）：联系人姓名
- `phone_numbers`（可选）：电话号码数组
- `emails`（可选）：电子邮件地址数组
- `organization`（可选）：公司/组织名称
- `notes`（可选）：Android 上的备注。iOS 会忽略此字段，因为读取或更新
  `CNContactNoteKey` 需要一项配套 App 未申请的 entitlement。

**iOS 实现**：使用 `CNContactStore.add(CNSaveRequest)` 创建，需要 `NSContactsUsageDescription` 权限。
**Android 实现**：使用 `ContentResolver.insert(ContactsContract.RawContacts.CONTENT_URI)`，需要 `WRITE_CONTACTS` 权限。

**响应**：
```json
{
  "id": "contacts_create_001",
  "data": {
    "contact_id": "new_contact_id_123"
  }
}
```

`contact_id` 是平台返回的联系人唯一标识，用于后续更新。

---

### 9. `contacts_update`

更新已有联系人。

**请求**：
```json
{
  "id": "contacts_update_001",
  "type": "contacts_update",
  "payload": {
    "contact_id": "contact_123",
    "name": "Li Si (updated)",
    "phone_numbers": ["+86 139 8765 4321", "+86 010 1234 5678"],
    "emails": ["lisi_new@example.com"]
  },
  "timeout_ms": 8000
}
```

**字段说明**：
- `contact_id`（必填）：要更新的联系人 ID
- 其他字段与 `contacts_create` 相同，提供的字段会覆盖原值

**iOS 实现**：使用 `CNContactStore.execute(CNSaveRequest)` 更新联系人。
**Android 实现**：使用 `ContentResolver.update()` 更新 `ContactsContract.Data` 表。

**响应**：
```json
{
  "id": "contacts_update_001",
  "method": "contacts_update"
}
```

如果联系人不存在，App 会返回结构化的 `invalid_arguments` 错误，并在
`details.native_code` 中携带原生的未找到错误码。

---

### 10. `notification_send`

发送本地通知。

**请求**：
```json
{
  "id": "notification_001",
  "type": "notification_send",
  "payload": {
    "title": "Reminder",
    "body": "Time to take medicine",
    "schedule_at": "2026-06-04T18:00:00+08:00",
    "sound": true,
    "badge": 1
  },
  "timeout_ms": 5000
}
```

**字段说明**：
- `title`（必填）：通知标题
- `body`（可选）：通知正文
- `schedule_at`（可选）：计划发送时间（RFC3339），不填则立即发送
- `sound`（可选）：是否播放声音，默认 true
- `badge`（可选）：App 角标数字（iOS）

**iOS 实现**：使用 `UNUserNotificationCenter` 发送本地通知，需要用户授权。
**Android 实现**：使用 `NotificationManager` 和 `AlarmManager`（定时），Android 13+ 需要 `POST_NOTIFICATIONS` 权限。

**响应**：
```json
{
  "id": "notification_001",
  "data": {
    "notification_id": "notification_123"
  }
}
```

`notification_id` 在 App 结果中标识该本地通知。当前的 Phone Bridge 命令集不提供
取消通知的能力。

---

## 错误处理

当 App 无法执行某条命令时，会返回结构化的 `error` 字段：

```json
{
  "id": "...",
  "error": {
    "category": "user_action_required",
    "code": "permission_denied",
    "message": "Calendar access not granted"
  }
}
```

常见错误场景：
- 未授予权限：`permission_denied`
- App 未安装：`app_not_installed`
- 参数无效：`invalid_arguments`
- 原生系统 API 失败：`native_module_failed`
- 不支持的后台路由：`app_backgrounded`

## 超时与重连

- **超时**：开发板端的每条命令都有 `timeout_ms`；超时后停止等待响应。App 应尽可能在超时之前响应。
- **重连**：WebSocket 断开后，App 应自动重连，以 3-5 秒的间隔重试。开发板端没有主动重连机制。
- **幂等**：WebSocket/HTTP 去重会缓存命令 ID，HTTP 队列会重试超时的执行中命令。
  只有对同一条逻辑命令才复用同一个唯一 ID。

## 时间格式

所有时间字段都必须是**带时区偏移的 RFC3339 格式**，例如：
- `2026-06-02T15:00:00+08:00`（GMT+8 下午 3 点）
- `2026-06-02T07:00:00Z`（UTC 上午 7 点）

请使用标准库进行解析（iOS `ISO8601DateFormatter`，Android `Instant.parse`）。

## 权限管理

### iOS

- **剪贴板读取**：iOS 14+ 会显示横幅，无需声明权限
- **日历**：必须在 `Info.plist` 中添加：
  ```xml
  <key>NSCalendarsUsageDescription</key>
  <string>Used to quickly create and manage calendar events</string>
  ```
  iOS 17+ 细分为 `NSCalendarsFullAccessUsageDescription`（读写）和 `NSCalendarsWriteOnlyAccessUsageDescription`（仅写入）。
- **联系人**：必须在 `Info.plist` 中添加：
  ```xml
  <key>NSContactsUsageDescription</key>
  <string>Used to query and manage contacts</string>
  ```
- **通知**：在安排通知之前，通过 `UNUserNotificationCenter.current().requestAuthorization(...)` 请求用户授权。

### Android

- **剪贴板**：Android 10+ 在后台无法读取剪贴板，需要前台服务或确保 App 处于前台。
- **日历**：需要运行时权限：
  ```xml
  <uses-permission android:name="android.permission.READ_CALENDAR" />
  <uses-permission android:name="android.permission.WRITE_CALENDAR" />
  ```
  首次使用时弹出授权；拒绝时返回结构化的
  `permission_denied` 错误。
- **联系人**：需要运行时权限：
  ```xml
  <uses-permission android:name="android.permission.READ_CONTACTS" />
  <uses-permission android:name="android.permission.WRITE_CONTACTS" />
  ```
- **通知**：Android 13+ 需要运行时权限：
  ```xml
  <uses-permission android:name="android.permission.POST_NOTIFICATIONS" />
  ```

## 测试建议

1. **聚焦的单元测试**：在两个仓库中覆盖命令路由、HTTP 队列过滤、去重以及结构化错误处理。
2. **超时场景**：测试权限弹窗期间的超时，并确认授权后 App 能正确处理后续命令。
3. **真机路由**：在手机连接开发板 USB ECM 链路的情况下，验证前台 WebSocket、
   PiP/FGS 轮询、灵动岛恢复以及 iOS BLE Wake。
4. **边界情况**：空剪贴板、没有日历事件、无效的 `event_id`、全天事件、跨时区查询、
   空的联系人查询、重复联系人、无效的 `contact_id`，以及定时通知。

## 版本兼容性

当前协议版本为 1.2。今后扩展新命令时：
- 新字段向后兼容（旧 App 会忽略未知字段）
- 新命令类型需要开发板与 App 协同发布，并进行兼容性测试
- 修改现有字段的语义需要升级版本号

---

## 附录：完整示例

### 心跳
**App → 开发板**：
```json
{"id": "heartbeat"}
```

**开发板 → App（回送）**：
```json
{"id": "heartbeat"}
```

### 打开微信
**开发板 → App**：
```json
{
  "id": "open_1717667890123_1",
  "type": "open_app",
  "app": "WeChat",
  "timeout_ms": 10000
}
```

**App → 开发板**：
```json
{
  "id": "open_1717667890123_1",
  "method": "ios_url_scheme"
}
```

### 读取剪贴板
**开发板 → App**：
```json
{
  "id": "clip_read_1717667890234_2",
  "type": "clipboard_read",
  "timeout_ms": 5000
}
```

**App → 开发板**：
```json
{
  "id": "clip_read_1717667890234_2",
  "data": {
    "text": "https://example.com"
  }
}
```

### 创建日历事件
**开发板 → App**：
```json
{
  "id": "cal_create_1717667890345_3",
  "type": "calendar_create",
  "payload": {
    "title": "Team meeting",
    "start_at": "2026-06-05T10:00:00+08:00",
    "end_at": "2026-06-05T11:00:00+08:00",
    "location": "Meeting room A",
    "notes": "Discuss Q2 planning"
  },
  "timeout_ms": 8000
}
```

**App → 开发板**：
```json
{
  "id": "cal_create_1717667890345_3",
  "data": {
    "event_id": "12345678-ABCD-1234-5678-1234567890AB"
  }
}
```

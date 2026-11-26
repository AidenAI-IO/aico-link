---
sidebar_position: 11
---

# 手机桥接（Phone Bridge）

## 目的

# 目的

Aico 配套 App 通过 USB ECM 连接硬件开发板，并执行可通过 iOS 和 Android 公开 API 完成的手机端操作。

开发板仍负责屏幕观察、任务规划和 HID 回退。Phone Bridge 是一条软件快速路径，用于启动 App、访问剪贴板，以及处理日历、联系人和通知等操作。

## 能力

| 能力 | iOS | Android | 解决的问题 |
| --- | --- | --- | --- |
| 打开 App 或 URL | URL scheme / Universal Link / `openURL` | 包名 / Intent / deep link | 当系统提供直接启动路径时，无需去寻找图标。 |
| 剪贴板读写 | `UIPasteboard` | Clipboard API | 不经过 UI 操作的复制粘贴即可交换文本。 |
| 日历 | EventKit | Calendar provider | 查询、创建和删除日历事件。 |
| 联系人 | Contacts framework | Contacts provider | 查询、创建和更新联系人。 |
| 通知 | 本地通知和 ANCS 查询 | 本地通知和通知使用权查询 | 发送提醒，并查询共享的通知事件环形缓冲区。 |
| 与开发板通信 | 前台 WebSocket，后台恢复能力有限 | 前台 WebSocket 和 FGS HTTP 轮询 | 接收命令并返回结构化结果。 |

## 通信方式

开发板拥有固定的 USB 网络地址 `192.168.42.1`。配套 App 作为客户端连接到：

```text
Phone relay app -> ws://192.168.42.1:8080/api/phone-bridge
```

开发板是 WebSocket 服务端，因此它无需发现手机的 DHCP 地址，App 也无需暴露本地服务。Agent 的 HTTP API 使用同一个端口 `8080`。

开发板还提供 `/api/phone-bridge/commands` 和 `/api/phone-bridge/results` 两个 HTTP 队列端点，但不能把 iOS 后台中的 React Native JS、WebSocket 和轮询定时器当作通用的工具执行路径。在 iOS 上，Phone Bridge 通常是一条前台快速路径：如果 Aico 配套 App 处于后台，且 App 已上报 `return_entry=dynamic_island`，Agent 会通过灵动岛恢复 Aico 配套 App，等待前台 WebSocket 桥接重连，然后执行所请求的工具命令。锁屏实时活动入口需要视觉确认，而不是按固定坐标点按。

PiP Bridge 是一个范围很窄的例外。当 App 在后台上报 `pip_bridge_enabled=true` 时，PiP 会话静默运行，灵动岛返回入口保持可见，因此 `open_url` 等仅限前台的命令仍会通过灵动岛恢复配套 App。对于后台安全的数据命令，`pip_bridge_enabled` 是权威的 PiP 传输信号；普通的前台 App 状态时间戳不会额外构成 PiP 过期条件。HTTP 目录保持完整以便直接诊断，而对话 Agent 的目录会在每次运行前根据实时运行时能力进行过滤。`open_app` 仍然暴露，因为它可以回退到 SearchLaunchApp；只有可执行的后台安全数据工具（`bridge_clipboard`、`bridge_calendar`、`bridge_contacts`、`bridge_notification`）会通过 HTTP 队列暴露，不可用的 App 操作会被省略。

BLE Wake 为一组更小的命令提供按需的 iOS 后台路由：日历创建/查询/删除、联系人查询/创建，以及本地通知发送。剪贴板读写在 iOS 后台不可靠，联系人更新也不通过 BLE Wake 暴露；除非前台 Phone Bridge、PiP 或灵动岛恢复提供了其他可执行路由，这些调用会返回明确的 `app_backgrounded` 错误。每次 Agent 运行前，运行时能力过滤会检查 `ble_service` 是否报告有已连接的 Wake 订阅者，并只暴露可执行的工具和操作。随后命令被放入现有的 HTTP 队列，开发板尽力发起一次 `wake` 调用。BLE 只携带唤醒提示：App 使用其 `phone_id` 轮询，执行原生模块，并通过 HTTP 提交结构化结果。因此手机必须保持连接在开发板的 USB ECM 网络上；BLE 不是命令传输的替代品。如果 BLE 不可用，除非前台 Phone Bridge、PiP/FGS 轮询或灵动岛恢复等其他路由可用，否则这些仅限 App 的工具会被省略；Wake 通知失败永远不会移除已入队的命令。

Android FGS Bridge 遵循相同的 HTTP 队列约定，不使用 WebSocket 作为后台传输。当 Android 前台服务以 `app_state=background` 和 `fgs_bridge_enabled=true` 轮询 `/api/phone-bridge/commands` 时，Agent 会保持 `open_app` 不可用，只将后台安全的数据工具路由到 HTTP 队列。在 PiP 和 FGS 两种模式下，`open_app` 的系统搜索都会通过该队列把查询词写入剪贴板，并粘贴到已获得焦点的搜索框中，而不是通过本地输入法键入；如果写入或粘贴失败，它会清空搜索框并回退到本地键盘输入。如果粘贴已发送但没有出现 App 结果，Agent 会检查搜索框是否仍为空；若为空，它会通过长按菜单再粘贴一次，如果搜索框仍为空，则在本地键入查询词，然后再尝试备用搜索词。

Android 系统通知采集是一条独立的单向路径。用户授予通知使用权后，
App 的原生监听器会通过 USB ECM 向 `/api/phone-notifications/events` 提交
有大小上限、可安全重试的事件批次。Agent 会将它们转发到 iOS ANCS 使用的同一个
`ble_service` 事件环形缓冲区；该路径在 Android 上无需 BLE 配对。

## 队列与剪贴板约定

没有符合条件的命令时，`GET /api/phone-bridge/commands` 返回 `commands: []`，包括平台、手机身份或后台能力过滤排除了所有已入队命令的情况。客户端可以据此区分空队列和无效响应，而不必把 HTTP 200 当作 schema 错误。

对于 `bridge_clipboard`，`action: "write"` 需要一个 `text` 字符串。缺失、null 或非字符串的 text 会在发送手机命令之前被拒绝。显式的空字符串仍然有效，会清空剪贴板；`action: "read"` 不需要 text。

## 使用 ADB Reverse 的桌面 Agent

在开发电脑而不是 Luckfox 开发板上运行 Agent 时，手机无法访问 `192.168.42.1`，因为 USB ECM 开发板网络并不存在。当前的 Android 配套 App 使用固定的开发板地址，且没有回环地址回退。因此，单纯的 ADB reverse 映射对这个 App 构建是不够的。桌面开发需要一个可配置回环目标的 App 构建，或者一个将固定开发板地址重定向到 ADB reverse socket 的已 root 模拟器。

```text
Phone relay app -> ws://192.168.42.1:8080/api/phone-bridge
Emulator NAT    -> 127.0.0.1:8080
ADB reverse     -> host computer Agent port
```

当 `[advanced_settings.hardware.hid].input_backend = "adb"` 生效时，桌面 Agent 会在启动时尝试配置一个固定端口的 reverse 映射：

```bash
adb reverse tcp:8080 tcp:8080
```

ADB Android 环境桥接提供截图和输入，但没有 Phone Bridge 端点。请使用
`--local-phone-bridge` 将配套 App 的连接、状态和 HTTP 命令队列保留在 Agent 上。
该参数不需要基准测试 token。Aico Launcher、原生 Android launcher 以及基准测试的
ADB 启动路径会自动选择本地 Phone Bridge。对于手动启动的 PC Docker 容器，请设置
`AICO_LOCAL_PHONE_BRIDGE=1`。

本地 Phone Bridge 仍然需要一条从配套 App 到 Agent 的网络路径。基准测试中的
`aidenbridge-capabilities-android` 用例会配置模拟器 NAT 规则，并将 ADB reverse
映射到每个任务 Agent 发布的端口。它需要已 root 的模拟器，并会在运行剪贴板和 URL
任务之前检查 `/api/phone-bridge/status`。仅选择本地 Phone Bridge 并不会重定向
App 的固定开发板地址。

在这种桌面/PC Agent 模式下，如需主机麦克风录音和 TTS 回复，请保持 `audio.backend = "auto"` 或将其设为 `"local"`。Agent 会通过已安装的主机录音命令采集 PCM，并通过主机操作系统的播放器播放临时 WAV 文件，而不是使用 `audio_service`。

启用 Phone Bridge 代理模式时，只提供屏幕和输入操作的环境桥接可能没有
`/api/phone-bridge/status` 端点。HTTP 404、405 或 501 会被视为 Phone Bridge 不可用：
Agent 报告其为未连接，以 debug 级别记录这次探测，并等待五分钟后再次探测。
其他失败（包括超时和无效响应）会报告未连接，并在 30 秒后重试。
并发的状态读取共享失败退避。成功的状态读取保持实时，之后某次探测成功时恢复正常轮询。

## 打开 App 的流程

```text
Board HDMI sees screen / user issues task
        │
        ▼
AI identifies target app, e.g., "WeChat"
        │
        ▼
Look up mapping table
iOS: weixin://
Android: com.tencent.mm
        │
        ▼
Public open_app reads the live relay-app state:
If foreground Phone Bridge is ready:
    Send semantic open_app command
Otherwise:
    Use SearchLaunchApp through the visible system UI
        │
        ▼
Relay app executes:
iOS: openURL("weixin://")
Android: Intent launch package name
        │
        ├─ HDMI verification success -> continue next step
        └─ Failure / timeout -> fallback to hardware simulation: find icon coordinates + HID tap
```

## 关键边界

当 `open_app` 回退到系统搜索且 iOS PiP 已启用时，查询词输入会先通过后台剪贴板命令
写入新文本，并粘贴到已获得焦点的搜索框中。之后现有的 App 结果查找、点按和 App 打开
确认照常进行。如果 PiP 不可用或其剪贴板/粘贴命令失败，查询词输入会使用现有的本地
HID/输入法路径；剪贴板尝试失败时会先清空搜索框再键入。

确认通过系统搜索启动 App 后，Agent 会检查已打开的 App 是否有弹窗。粘贴权限提示
（"Allow Paste"）或广告的关闭按钮会被自动点按；其他弹窗（系统权限、登录、隐私同意、
更新提示）会保持打开，并在 `open_app` 结果中以 `popup` 报告其类型和简短摘要，
由 Agent 根据任务决定如何处理。粘贴后搜索框仍为空时也会运行同样的检查。

在 iOS 上，中继 App 不是常驻后台的系统代理。它更像一个前台快速路径执行器；灵动岛可以作为自动返回 Aico 配套 App 的入口，而锁屏实时活动卡片需要视觉确认：

```text
Aico companion app foreground
-> Receives board command
-> openURL opens WeChat
-> Aico companion app enters background
-> Subsequent operations continue via hardware HDMI observation + HID operation
```

因此 iOS 无法保证长期在后台接收命令。BLE Wake 为其允许列表增加了一个短暂的按需
HTTP 队列窗口，但并不会让 App 永久常驻。Android 可以通过前台服务获得更稳定的表现，
后台轮询能力也更强。

## iOS 要点

- 不要用 `canOpenURL` 做大规模预检查。
- 直接调用 `openURL(url)` 尝试打开。
- `LSApplicationQueriesSchemes` 主要限制的是 `canOpenURL` 查询，而不是动态的 `openURL` 尝试。
- 配套 App 维护当前的 URL scheme/包名/intent 映射；
  开发板发送的是语义目标。
- `openURL` 返回成功并不代表目标页面已完全可用；最终要通过 HDMI 视觉验证。

## Android 要点

- 优先按包名启动：`getLaunchIntentForPackage("com.tencent.mm")`。
- Android 11+ 有包可见性限制，需要配置 `<queries>`，或针对特定场景评估 `QUERY_ALL_PACKAGES`。
- 后台启动 Activity 有系统限制，但在使用前台服务、通知和用户授权后，可用性比 iOS 更强。

## 配套 App 实现

配套 App 是一个 bare React Native 应用。React Native 负责 UI、WebSocket 客户端、前台命令分发以及共享业务逻辑。原生 Swift 和 Kotlin/Java 模块提供未直接暴露给 JavaScript 的平台能力。

重要的原生部分包括：

- iOS 本地网络权限、URL 启动、日历、联系人、通知、实时活动、PiP Bridge 以及 BLE Wake 集成。
- Android 包名与 intent 启动、包可见性、通知、通知使用权、开发板网络绑定以及前台服务轮询。

配套 App 的源码和平台权限声明请参见相关的 `aiden-app` 仓库。其 `package.json` 是当前 React Native 和工具链版本的唯一可信来源。

## 为什么在 USB 网络之上使用 WebSocket

USB ECM（`192.168.42.1`）和 WebSocket 是两个层次：

| 层次 | 作用 |
| --- | --- |
| USB ECM | 网络链路，使手机和开发板之间能够进行 IP 包通信 |
| WebSocket | 应用协议，在这条链路上传输命令和确认 |

USB ECM 相当于“路已经修好”，WebSocket 相当于“在路上跑的车”。

现有的 `192.168.42.1:80` 配置页是 HTTP 请求-响应模式，适合人工的网页操作。但开发板需要**主动向手机 App 推送命令**（例如“现在打开微信”），这是 HTTP 做不到的：HTTP 由客户端发起，服务端无法主动与客户端通信。

WebSocket 的核心价值：

- **双向实时**：开发板可以随时向 App 推送命令，App 也可以随时回复
- **长连接**：无需每次重建连接，延迟低
- **状态感知**：连接即在线，断开立即可知，并触发 HID 回退

## 已实现的运行时流程

1. 中继 App 启动后连接 `ws://192.168.42.1:8080/api/phone-bridge`，
   并定期发送心跳。
2. App 在连接后和回到前台时上报 `phone_environment`，并在其可见生命周期变化时
   上报 `phone_app_state`。Android FGS Bridge 通过 HTTP 队列轮询上报
   `fgs_bridge_enabled`。
3. 开发板维护 `bridge_connected`、`platform`、`last_heartbeat_at`、
   `app_state`、返回入口和后台桥接相关字段，以及最新的环境快照。`hid_connection_id`
   根据开发板 UDC 状态标识当前的物理 USB HID 会话。它在 WebSocket 重连以及
   HTTP/FGS 回退期间保持不变，只有在 USB 主机断开并重新连接后才会改变。
4. 每次对话运行前，Agent 会根据前台 Phone Bridge、灵动岛恢复、PiP/FGS 轮询、
   BLE Wake 以及 BLE 通知查询能力，过滤仅限 App 的工具和操作。

`bridge_connected` 只表示 WebSocket 当前处于活跃状态，并不等同于 USB 线缆已连接。iOS App 进入后台后，WebSocket 可能断开，而 USB ECM 仍然可达；灵动岛更新使用本地 BLE Wake，随后通过 USB ECM 读取 `/api/live-activity/current`，而不是使用手机桥接 WebSocket 或远程中继。

Agent 将缓存的手机屏幕尺寸与 `hid_connection_id` 关联。因此，只要同一个 USB HID
会话仍然连接，心跳超时就不会丢弃这些尺寸。物理 USB 断开会在新的主机会话复用缓存
之前使其失效。

当不存在实时上报时（例如 App 关闭期间 Agent 重启后），截图还会回退到 Agent 配置
目录中的 `phone_screen.json`。它保存最后一次上报的尺寸及其裁剪时对应的采集帧尺寸；
如果从未有过上报，则保存从黑边裁剪中学到的宽高比，前提是该裁剪的切边显示的是屏幕
内容而不是接近黑色的像素。该条目只适用于相同的设备类型和帧尺寸，因此接近全黑的页面
仍会按手机的宽高比裁剪，而不会裁进页面内部。每使用该条目 20 次采集，就会重新检测一次，
因此另一台裁剪结果经过验证、宽高比不同的手机会替换它。

当 `app_state=background|inactive`、`return_entry=dynamic_island` 且
`return_entry_available=true` 时，无论是否处于 PiP Bridge 模式，`open_url`
和桥接数据工具都可以点击 Aico 配套 App 的灵动岛入口，等待 Phone Bridge 恢复，
然后发送命令。而 `open_app` 只要前台 Bridge 无法启动 App，就会选择
SearchLaunchApp。对于 `http` 和 `https` URL，当 Phone Bridge 既不能执行命令
也不能恢复配套 App，或 Bridge 命令失败时，`open_url` 有一条可见 UI 回退：
它通过 SearchLaunchApp 打开浏览器（iOS 上为 Safari；Android 上先尝试 Chrome，
再尝试厂商自带浏览器），点按地址栏，在本地键入 URL 并按下 Enter。`sms`、`mailto`
和 `tel` 链接仍然需要 Phone Bridge。
锁屏实时活动入口不会被盲点，因为它们在屏幕上的位置不稳定；请改用截图/HID 回退或
视觉确认。当 iOS 在后台时 `pip_bridge_enabled=true`，或 Android 在后台时
`fgs_bridge_enabled=true`，通用的后台安全数据命令会使用 HTTP 队列。在存在已认证的
iOS Wake 订阅者时，范围更窄的 BLE Wake 允许列表会在一个非敏感的 GATT 提示之后
使用同一个队列。

### 命令协议

开发板通过 WebSocket 向 App 发送 `BridgeCommand`，App 执行后回复 `BridgeCommandResponse`。

#### 通用字段

**BridgeCommand（开发板 → App）**：
```json
{
  "id": "cmd_001",
  "type": "open_app | clipboard_read | clipboard_write | calendar_* | contacts_* | notification_send",
  "timeout_ms": 5000,
  "payload": { }  // Optional, command-related JSON (clipboard text, calendar event, etc.)
}
```

**BridgeCommandResponse（App → 开发板）**：
```json
{
  "id": "cmd_001",
  "method": "calendar_create",
  "data": {"event_id": "event_123"}
}
```

失败示例：

```json
{
  "id": "cmd_001",
  "error": {
    "category": "user_action_required",
    "code": "permission_denied",
    "message": "Calendar access not granted"
  }
}
```

成功时省略 `error`。当前的线路 schema 不包含 `ok` 布尔值；`method` 和 `data`
是可选的成功字段。

**App 活跃事件（App → 开发板）**：
```json
{
  "id": "phone_environment",
  "method": "phone_environment",
  "data": {
    "platform": "ios",
    "system_name": "iOS",
    "system_version": "18.5",
    "locale": "zh-Hans-CN",
    "time_zone": "Asia/Shanghai",
    "utc_offset": "+08:00",
    "manufacturer": "Apple",
    "model": "iPhone16,2",
    "screen": {"width_pixels": 1179, "height_pixels": 2556, "scale": 3},
    "battery": {"level": 0.87, "charging": true},
    "system_apps": [{"name": "Camera", "available": true, "category": "system", "availability_source": "builtin"}],
    "third_party_apps": [{"name": "WeChat", "available": true, "category": "third_party", "availability_source": "can_open_url"}],
    "available_apps": [{"name": "WeChat", "available": true, "category": "third_party"}]
  }
}
```

`phone_environment` 不对应任何开发板命令 ID；开发板只更新桥接状态，不会把它当作工具调用的确认。
`system_apps` 是系统内置 App/能力列表；`third_party_apps` 是安装/可打开性探测的结果。`available_apps` 仅作为兼容旧开发板的遗留字段保留。

#### 命令类型

##### 1. `open_app`：打开 App

```json
{
  "id": "open_001",
  "type": "open_app",
  "app": "WeChat",
  "timeout_ms": 10000
}
```

回复：
```json
{
  "id": "open_001",
  "method": "ios_url_scheme"
}
```

App 端的 `method` 表示底层机制（例如 `ios_url_scheme`、`ios_shortcut`、`android_intent`、`android_deeplink`、`launch_package` 或 `open_url`）。公开的 `open_app` 工具将语义化的 App 启动请求路由到内部的 Phone Bridge 启动器或可见的系统搜索。公开的 `open_url` 工具通过 Phone Bridge 发送 `http`、`https`、`sms`、`mailto` 和 `tel` URL。它们的结果会将任务语义归一化为 `method:"open_app"` 或 `method:"open_url"`，而底层的 App 端方法以 `mechanism` 返回。

App 启动和 URL 的语义是分开的：要启动浏览器本身，请用 `{"app":"browser"}` 调用 `open_app`；要打开 URL，请用受支持的 URL 调用 `open_url`，例如 `{"url":"https://example.com"}` 或 `{"url":"tel:+15551234567"}`。配套 App 负责平台相关的 URL/包名/intent 映射。

##### 2. `clipboard_read`：读取剪贴板

```json
{
  "id": "clip_read_001",
  "type": "clipboard_read",
  "timeout_ms": 5000
}
```

回复：
```json
{
  "id": "clip_read_001",
  "data": {
    "text": "clipboard content"
  }
}
```

##### 3. `clipboard_write`：写入剪贴板

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

回复：
```json
{
  "id": "clip_write_001",
  "method": "clipboard_write"
}
```

##### 4. `calendar_create`：创建日历事件

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

回复：
```json
{
  "id": "cal_create_001",
  "data": {
    "event_id": "ios_calendar_id_123"
  }
}
```

##### 5. `calendar_query`：查询日历事件

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

回复：
```json
{
  "id": "cal_query_001",
  "data": {
    "events": [
      {
        "event_id": "...",
        "title": "Dentist appointment",
        "start_at": "2026-06-02T15:00:00+08:00",
        "end_at": "2026-06-02T16:00:00+08:00",
        "location": "Clinic"
      }
    ]
  }
}
```

##### 6. `calendar_delete`：删除日历事件

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

回复：
```json
{
  "id": "cal_delete_001",
  "method": "calendar_delete"
}
```

##### 7. `contacts_query`：查询联系人

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

回复：
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

##### 8. `contacts_create`：添加联系人

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

回复：
```json
{
  "id": "contacts_create_001",
  "data": {
    "contact_id": "new_contact_id_123"
  }
}
```

在 iOS 上，创建/更新联系人时会忽略 `notes`，因为 `CNContactNoteKey` 需要一项配套 App
未申请的 entitlement。Android 支持该字段。

##### 9. `contacts_update`：更新联系人

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

回复：
```json
{
  "id": "contacts_update_001",
  "method": "contacts_update"
}
```

##### 10. `notification_send`：发送通知

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

公开的 `bridge_notification` 工具既支持通过配套 App 发送本地通知，也支持在开发板端
查询共享的系统通知：

```json
{"action":"send","title":"Reminder","body":"Time to take medicine","sound":true}
```

```json
{"action":"query","limit":20}
```

`action=send` 仍通过配套 App 使用 `notification_send`。
`action=query` 直接读取 `ble_service`，返回通知变化、当前 generation 以及游标字段。
省略 `since` 时返回最近保留的事件。传入 `since=0` 可从最早保留的事件开始向后翻页；
增量查询时，将上一次的 `last_id` 作为 `since`，并同时传入上一次的 `generation`。
查询不要求配套 App 处于前台，也不要求 Phone Bridge 已连接。在 iOS 上，
事件来自 ANCS；如果安装了手机通知采集功能，同一个环形缓冲区中也可能包含通过该
路径转发来的 Android 事件。

回复：
```json
{
  "id": "notification_001",
  "data": {
    "notification_id": "notification_123"
  }
}
```

### 时间格式

所有时间字段都必须是**带时区偏移的 RFC3339 格式**，例如：
- `2026-06-02T15:00:00+08:00`（GMT+8 下午 3 点）
- `2026-06-02T07:00:00Z`（UTC 上午 7 点）

在可用时使用手机环境中的时区。Agent 可以用 `shell` 获取控制端时间作为基准，但不能假定控制端时区与手机一致。

### 权限与隐私

- **剪贴板读取**：iOS 16+ 会显示一次性授权横幅，频繁读取会影响体验。Android 10+ 要求处于前台 App 或前台服务中。
- **日历读写**：iOS 和 Android 都需要运行时权限。权限被拒绝时，
  响应会携带结构化的 `permission_denied` 错误；超时由开发板端的 `timeout_ms` 控制。
- **联系人读写**：iOS 需要 `NSContactsUsageDescription`；Android 需要
  `READ_CONTACTS` 和 `WRITE_CONTACTS`。拒绝时使用相同的结构化错误。
- **通知权限**：iOS 通过 `UNUserNotificationCenter` 请求授权；
  Android 13+ 需要 `POST_NOTIFICATIONS`。
- **通知读取**：Android 系统通知采集还需要用户在“设置 > 通知使用权”中
  单独启用配套 App（目前显示为 `Aiden`）。
  仅有 `POST_NOTIFICATIONS` 并不能访问其他 App 的通知。

### 运行时路由

1. `open_app` 读取配套 App 的实时状态。前台 Phone Bridge 就绪时使用
   BridgeOpenApp；否则通过可见的系统 UI 使用 SearchLaunchApp。
2. `open_url` 和桥接数据工具可以在发送命令之前，通过已确认的灵动岛入口恢复处于后台的
   iOS Aico 配套 App。
3. PiP/FGS 后台路由允许通过 HTTP 队列执行剪贴板、日历、联系人和本地通知命令。
   BLE Wake 只用于日历创建/查询/删除、联系人查询/创建以及通知发送。
4. 每次对话运行前，不可用的 App 工具和操作会从 Agent 目录中过滤掉。HTTP 目录保持
   完整以便直接诊断。
5. 当不存在可执行的前台、恢复、后台队列或 BLE 路由时，桥接数据工具会返回明确的
   桥接不可用错误。

## 控制边界

Phone Bridge 不替代硬件控制。它增加的是一条软件快速路径：

```text
What can be completed quickly via software, go through relay app;
What software cannot do or is unstable, continue via HDMI + HID.
```

iOS 使用前台快速路径，配合有限的后台恢复和硬件回退。Android 还支持通过前台服务轮询执行后台安全的命令。在这两种情况下，当任务完成与否取决于手机的可见状态时，HDMI 观察仍是最终的验证路径。

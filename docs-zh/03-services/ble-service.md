---
sidebar_position: 6
---

# BLE Service：配对、手机通知与后台唤醒

`ble_service` 负责 Aico Link 的低功耗蓝牙（BLE）集成。它在 `hci0` 上通过 BlueZ
同时承担两种角色：

- BLE 外设（Peripheral）：广播一个 Aico Link Wake 服务，供 iOS 配套 App 订阅。
- ANCS 消费者（Consumer）：订阅已配对 iPhone 提供的 Apple 通知中心服务（Notification
  Center Service），并对系统通知事件进行归一化处理。
- Android 通知接收端：接收配套 App 通过仅限 USB 的 Agent HTTP API 转发的、已归一化的通知变更。

BLE 的职责被刻意限定得很窄。它不承载手机桥接（Phone Bridge）的工具命令、结果、实时活动文本或任务元数据，
手机通知事件也不会写入 Agent 记忆。手机桥接命令仍然使用 WebSocket 或现有的 HTTP 队列。
实时活动唤醒会提示 iOS 原生层通过 USB ECM 读取最新的任务快照。

## 启动与持久化

Pico Zero 内核必须合并以下两个配置片段：

```text
aico-zram.config rv1106-bt.config
```

运行时的启动顺序为：

```text
aico-wifi-driver.service -> aico-bluetooth-attach.service -> bluetooth.service -> aico-ble.service
```

`aico-bluetooth-attach.service` 先加载 LE SMP 所需的 AES/CMAC 加密模块，然后以 1.5 Mbaud
在 `/dev/ttyS0` 上挂接 AIC8800D80 控制器，而不通过旧的 `HCIDEVUP` ioctl 为其上电。
Debian 的 `bluetooth.service` 会 bind-mount BlueZ 状态目录，启动守护进程，并由 BlueZ
通过管理 API 为 `hci0` 上电。只有按这个顺序，内核才会注册加密 GATT 读取所用的 LE SMP 固定信道。
如果控制器消失，看门狗会在重启 BlueZ 之前重复同样的初始化流程：

```text
/userdata/ble_service/bluetooth -> /var/lib/bluetooth
```

这样，iPhone 的绑定关系在重启和固件 rootfs 替换后都能保留。

在 `ble_service` 启动之前，`aico-bluetooth-clear-adv` 会最多等待 10 秒让 `hci0` 就绪，
然后禁用并清除控制器中所有的扩展广播集。只要控制器固件仍在运行（例如蓝牙协议栈在未重启的情况下重启），
控制器就会在 HCI Reset 之后保留其广播集；只有 Wi-Fi 驱动启动时的 WL_EN 复位才会清除它们。
如果不做清理，控制器会以 `Command Disallowed (0x0c)` 拒绝
`LE Set Extended Advertising Parameters`，`bluetoothd` 每两秒记录一次
`Failed to add advertisement: Busy (0x0a)`，`ble_service.log` 则反复出现 `register BLE advertisement: Failed to register
advertisement`。此时 App 将无法发现开发板。清理失败会记录到 `ble_service.log`，但不会阻塞启动。
由于 `ble_service` 会在进程内重试其后端，且不会因该错误退出，它还会在每次注册广播失败后的重试之前再次运行该辅助程序，
因此即使服务单元启动时跳过了清理（例如 `hci0` 未在 10 秒内就绪），也仍然能够恢复。

Wake 服务注册在固定的 GATT 句柄上：服务 `0x0013`，Wake 值 `0x0015`，这正是全新启动的
`bluetoothd` 会分配的位置。iOS 按绑定缓存 GATT 句柄，而 BlueZ 从不复用之前
`RegisterApplication` 释放的句柄，因此如果自动分配，Wake 特征值会在每次后端重试或重启时改变位置。
这时已绑定的 iPhone 会一直读取旧句柄，`btmon` 显示 `Read Request` 得到
`Invalid Handle (0x01)` 应答，App 则报告配对被取消或未完成。如果 BlueZ 拒绝固定句柄，
`ble_service.log` 会记录 `BLE GATT pinned handles rejected`，注册回退为自动分配。

## 配对与绑定

广播名称由配置的基础名称加上适配器地址的最后四位十六进制数字组成，例如 `Aico Link-12AB`。
该名称仅用于显示。完整的适配器标识通过 USB 状态 API 以 `board_identity` 的形式提供，
并在 BLE 广播中以六字节的厂商自定义数据（manufacturer-specific data）形式携带。
App 要求服务 UUID、显示名称和完整标识全部匹配才会配对，因此附近名称后缀相同的开发板不会被误选。

Wake 特征值具有加密的读取操作。配套 App 在连接后执行该读取，促使 iOS 启动 SMP 并弹出系统配对对话框。
不注册任何 HOGP 服务；USB 仍然是唯一的 HID 控制通路。

即使不存在绑定，`ble_service` 启动时也处于不可配对、不可发现状态。iOS App 会通过 USB ECM
显式调用 Agent 配对 API；只有这时服务才会打开一个五分钟的配对窗口。App 会先读取开发板稳定的
`device_name` 和抗冲突的 `board_identity`，并且只连接同时携带这两个值的 Wake 服务广播者。
只有当 Wake 订阅和一次成功的加密 Wake 读取都在当前窗口内发生（顺序不限）时，窗口才会关闭。
仅有通知订阅并不会关闭窗口：Wake 的 CCCD 没有加密要求，因此 iOS 可能在一条它还无法解密的链路上订阅，
或恢复缓存的 CCCD。当 iOS 已忘记开发板而 BlueZ 仍保留旧密钥时就会出现这种情况。
如果在那时关闭窗口，内核会以 "Pairing not supported" 应答手机的下一次 SMP Pairing Request。
已有绑定不会阻止显式的 Connect 操作重新打开窗口。在窗口之外，只有被选中的受信任手机才会获得授权。
`/etc/aico_ble_service.conf` 中的 `PAIRING_WINDOW_SECONDS` 控制用户发起的窗口的最长时间。

五分钟是一个上限，用于容忍蓝牙权限授予和 iOS 确认带来的延迟；App 会立即开始扫描，配对成功后窗口会提前关闭。
服务重启本身绝不会打开窗口。在用户发起的窗口有效期间，服务还会核对 BlueZ 实际的 `Pairable` 和
`Discoverable` 属性，因为删除绑定和 BlueZ 的其他状态变化可能会独立于服务状态重置这些属性。
Wake 广播在每个 `ble_service`/BlueZ 生命周期内只注册一次，并保持稳定的服务 UUID 和开发板标识。
打开、刷新或关闭配对窗口都不会注销或重新创建广播；变化的只有适配器的配对属性和配对代理的授权。

App 只报告实时连接状态。绑定只是重连缓存，不能证明 App Wake 会话已连接。每次显式的 Connect
操作都会重新打开开发板窗口；CoreBluetooth 会尽量复用已保存的外设，否则执行系统配对流程。

在加密 Wake 读取成功之前，ACL/CoreBluetooth 连接不会被报告为已认证的蓝牙连接。
如果 iOS 已忘记该设备而 BlueZ 仍保留旧密钥，该读取会返回加密不足。App 会在第一次出现此类失败后停止重连，
并将状态标记为需要重置开发板绑定，而不是反复弹出系统配对对话框。

## Wake GATT 约定

| 项目 | UUID |
| --- | --- |
| Wake 服务 | `a1de0001-7c4b-4f52-8d9a-6b4f6e6f7469` |
| Wake 特征值 | `a1de0002-7c4b-4f52-8d9a-6b4f6e6f7469` |

该特征值支持加密的 `read` 和标准的 `notify`。开发板上的 BlueZ 5.65 即使在绑定之后也会拒绝
`encrypt-notify` 的 CCCD 写入，因此由读取操作作为认证和配对的触发器，而通知只携带一个不敏感的轮询信号。
通知是一个 12 字节的小端序值：

```text
byte 0      protocol version (1)
byte 1      reason (0 unknown, 1 Phone Bridge queue, 2 manual, 3 system,
            4 Live Activity state refresh, 5 planned USB re-enumeration)
bytes 2-3   reserved
bytes 4-11  uint64 wake sequence
```

iOS App 仅将其视为原生唤醒钩子。原因 1 会打开一个短暂的后台手机桥接命令窗口；原因 4 会读取
`/api/live-activity/current` 并在本地更新 ActivityKit；原因 5 会启动一个宽限期，
使计划内的 USB gadget 重新枚举不会被当作线缆断开。不会从 BLE 中解码任何命令或任务负载。

## 手机通知事件结构

该服务订阅 ANCS 的 Notification Source 和 Data Source，通过 Control Point 请求通知属性，
并将事件存放在一个有容量上限的内存环形缓冲区中。事件包含：

- 单调递增的字符串 `id`，用作 UDS 游标；
- `source`、来源通知/事件 ID，以及当事件来自 Android 时配套 App 的 `device_id`；
- ANCS 的 `notification_uid`、事件类型、标志、类别和类别计数；
- 属性获取成功时的应用标识、标题、副标题、消息和日期；
- 当断开连接或协议错误导致属性响应不完整时的 `metadata_complete` 和 `metadata_error`。

环形缓冲区默认容纳 512 个事件。重启会清除事件，但不会清除蓝牙绑定。

ANCS 订阅独立于配套 App 的 Wake 订阅。Wake 是否可用只影响按需执行的后台手机桥接；
它不能延迟 Notification Source 或 Data Source 的订阅，因为那会改变 iOS/BlueZ 的 GATT
初始化顺序，可能导致属性读取得不到 Data Source 响应。

在 Android 上，用户授予通知访问权限后，配套 App 会使用 `NotificationListenerService`。
它会过滤掉 Aico 配套 App 自身的通知和分组摘要，在本地将新增/修改/移除事件排队，并通过 USB ECM
批量提交到 `/api/phone-notifications/events`。每个来源事件都有一个稳定的 `source_event_id`；
只要该事件仍在有界环形缓冲区中，`ble_service` 就会对重试进行去重。Android 通知转发不需要蓝牙配对。

## UDS API

默认 socket：

```text
/run/ble_service/ble_service.sock
```

帧格式为通用的 12 字节 UDS 信封，详见 [Unix 域 Socket 协议](../06-protocols/uds-protocol.md)。

### `status`

```json
{"op":"status"}
```

返回适配器、GATT 注册、配对窗口、受信任绑定、广播、Wake 订阅者、已连接 iPhone、ANCS 订阅、游标、
最近一次 Wake 投递结果以及最近错误的状态。已断开的受信任设备始终报告 `wake_subscriber=false` 和
`ancs_subscribed=false`。

### `wake`

```json
{"op":"wake","reason":"phone_bridge"}
```

返回字符串 `wake_id` 和 `delivered`。`delivered=true` 只确认 Wake 通知已投递给订阅的 iOS 中心设备，
并不确认 HTTP 队列已被轮询、原生工具已执行或结果已提交；客户端必须继续等待手机桥接命令的结果。
`delivered=false` 表示没有 iOS 中心设备订阅，HTTP 队列保持不变。

### `events_since`

```json
{"op":"events_since","since":"42","generation":"<service-generation>","limit":50}
```

返回当前的 `generation` 以及游标之后的事件。从 `since=0` 开始，保存返回的 generation，
并在增量读取时带上它。`ble_service` 重启后，过期的 generation 会返回 `reset_required=true`
且不含事件；此时请以 `since=0` 重试。在同一个 generation 内，`truncated=true`
表示请求的游标早于有界环形缓冲区保留的历史。

### `notification_publish`

```json
{
  "op": "notification_publish",
  "phone_id": "android-...",
  "events": [
    {
      "source_id": "0|com.example.mail|42|null|1000",
      "source_event_id": "<sha256>",
      "event": "added",
      "app_identifier": "com.example.mail",
      "title": "New message",
      "message": "Hello"
    }
  ]
}
```

接受 1-8 个 Android 通知事件，校验有长度限制的元数据字段，设置 `source=android` 和请求中的
`device_id`，并返回已接受和重复的数量。该操作供 Agent HTTP 桥接使用，而不是供任意本地发布者调用。

### `pairing_start`

```json
{"op":"pairing_start"}
```

打开或刷新配置的连接窗口。已有绑定会保留，不会造成冲突；当 App 完成加密读取并订阅 Wake 通知，
或截止时间到期时，窗口关闭。

### `disconnect`

```json
{"op":"disconnect"}
```

对当前手机连接调用 BlueZ `Device1.Disconnect`，关闭配对窗口，禁用广播，清除实时 Wake/ANCS 状态，
并保留已配对/受信任设备及其绑定密钥。重新连接需要再执行一次显式的 Connect 操作（它会调用
`pairing_start`）；之后可以复用现有绑定，无需重新进行系统配对。

### `pairing_forget`

```json
{"op":"pairing_forget"}
```

对开发板端的绑定调用 BlueZ `Adapter1.RemoveDevice`，返回删除数量以及最新的蓝牙状态。
它仍是一个本地维护操作；Agent 只通过仅限 USB 的 `/api/bluetooth/pairing/reset` 恢复端点暴露它。
正常断开连接不会使用它，系统绑定的双方都会保留。

## Agent HTTP API

配套 App 通过 USB ECM 上的 Agent 访问配对操作：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/api/bluetooth/status` | 读取 BLE 运行时状态和实时连接状态 |
| `POST` | `/api/bluetooth/pairing/start` | 打开或刷新用户发起的连接窗口 |
| `POST` | `/api/bluetooth/pairing/reset` | 在一次全新配对尝试之前，删除已确认过期的开发板绑定 |
| `POST` | `/api/bluetooth/disconnect` | 断开物理 BLE/ANCS 链路，但不删除绑定 |
| `POST` | `/api/phone-notifications/events` | 接收一批 Android 通知变更 |

蓝牙控制和手机通知写入只接受来自开发板 USB ECM 地址（`192.168.42.1/24`）或回环地址的请求；
通过 Wi-Fi 或其他监听器到达的请求会收到 `403`。Android 通知内容通过这条本地 USB 通路传输。
正常的 iOS App 流程会先停用其 CoreBluetooth 重连循环，再请求 BlueZ 断开共享的物理链路。
BLE 密钥、iOS ANCS 内容和手机桥接命令负载都不会经过这些端点。

## 运维

```bash
systemctl status aico-bluetooth-attach.service
systemctl status bluetooth.service
systemctl status aico-ble.service
```

日志：

```text
/var/log/aico-hciattach.log
/var/log/ble_service/ble_service.log
/userdata/agent/log/agent.log
```

`bluetoothd` 写入 systemd journal（`journalctl -u bluetooth`）。在默认日志级别下它不会记录
SMP 配对结果，因此请使用 `btmon` 在空口上抓取配对失败。

`ble_service.log` 记录设备连接变化、配对窗口的状态转换及其原因（`app_connected`、`deadline`、用户断开）、
因 Wake 订阅未认证而保持打开的窗口、加密 Wake 读取、配对代理的决策以及开发板绑定的删除。
`agent.log` 记录每次由 App 发起的 `pairing/start`、`pairing/reset` 和 `disconnect` 调用及其产生的开发板状态。

常用检查：

```bash
hciconfig -a
bluetoothctl show
ls -l /userdata/ble_service/bluetooth
```

在 App 请求配对之前，`bluetoothctl show` 应显示 `Pairable: no` 和 `Discoverable: no`。
执行 `pairing_start` 后，两者都会变为 `yes`，直到 App 完成已认证的 Wake 连接或配置的截止时间到期。

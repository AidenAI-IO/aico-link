---
sidebar_position: 3
---

# Config Web 管理 API

Config Web 是 Go Agent 二进制文件的 `config-web` 子命令。Config Web 和 Agent 运行时分别监听 80 和 8080 端口，
各自拥有独立的 PID 和服务生命周期。配置保存后，Config Web 会调用 Agent 仅限回环访问的重新加载端点；
Agent 则读取 Config Web 的存储状态镜像，用于查找归档。Config Web 不代理 Agent 的聊天、会话或手机桥接流量。

## 资源端点

所有公开端点都使用 `/api` 根路径，不带额外的版本前缀：

| 资源 | 方法与路径 | 说明 |
| --- | --- | --- |
| 配置 | `GET /api/config` | 读取解析后的 `agent.toml` 配置 |
| 配置 | `PATCH /api/config` | 持久化一个合并补丁并排队应用 |
| 配置 | `POST /api/config/plan` | 校验合并补丁但不保存，并报告其应用级别 |
| 配置 | `GET /api/config/application` | 读取待应用、已应用、失败或需要重启的状态 |
| 配置 | `GET /api/config/schema` | 读取字段类型、默认值、可选项、敏感字段标记和重启提示 |
| 配置 | `PUT /api/config/locale` | 更新页面语言 |
| 配置 | `POST /api/config/test` | 校验配置和设备环境，但不保存 |
| 配置 | `GET /api/config/backup` | 下载已持久化的分组 `agent.toml` 备份 |
| 配置 | `PUT /api/config/backup` | 校验、原子恢复并应用一个分组 TOML 备份 |
| 记忆 | `POST /api/memory/reset` | 清除对话历史和持久化记忆，然后重启 Agent |
| 对话 | `POST /api/conversation/reset` | 只清除当前对话历史；保留记忆，Agent 继续运行 |
| 模型 | `GET /api/models?provider=...&locale=...` | 返回本地化的模型目录；`provider=openrouter&output_modalities=decisions` 会从 OpenRouter 的公开目录获取原生 Decisions 模型 |
| STT 测试 | `POST /api/config-test/stt/start` | 使用提交的未保存设置开始一次麦克风录音测试 |
| STT 测试 | `POST /api/config-test/stt/stop` | 停止录音并返回转写结果 |
| 存储 | `GET /api/storage/status` | 读取 SD/eMMC 状态和格式化任务 |
| 存储 | `POST /api/storage/format` | 异步格式化 SD 卡 |
| 存储 | `POST /api/storage/eject` | 同步并安全弹出 SD 卡 |
| 设备 | `GET /api/device/snapshot` | 读取聚合的设备模型：配置、Wi-Fi、设备、固件/组件版本和存储摘要 |
| 设备 | `GET /api/device/status` | 读取设备、固件/组件版本、Agent 进程、USB/HID 和能力摘要 |
| 设备 | `GET /api/device/firmware` | 读取固件/组件和业务软件包版本，不探测 Agent 是否就绪 |
| 设备 | `POST /api/device/reboot` | 重启设备 |
| Agent | `POST /api/agent/restart` | 重启 Agent 进程；Config Web 和 USB 链路保持在线 |
| 安全 | `GET /api/security/lan-access` | 读取 LAN 访问开关以及是否已设置设备密码 |
| 安全 | `PUT /api/security/lan-access` | 开启或关闭 LAN 访问，并设置设备密码 |
| 安全 | `DELETE /api/security/password` | 清除设备密码；保留 LAN 访问设置 |
| 安全 | `GET /login` | 局域网浏览器的登录页面；USB 浏览器会被静默配对 |
| 安全 | `POST /api/auth/login` | 用设备密码换取会话 token |
| 安全 | `POST /api/auth/pair` | 无需密码，为 USB 客户端签发会话 token |
| 安全 | `POST /api/auth/logout` | 清除会话 cookie |
| 安全 | `GET /api/auth/session` | 报告该请求是否已登录 |
| 设备 | `POST /api/device/usb/reenumerate` | 重新枚举 USB HID/ECM |
| 网络 | `POST /api/network/wifi/scan` | 扫描附近的 Wi-Fi 网络 |
| 网络 | `GET /api/network/wifi/status` | 读取实时无线状态和已保存的配置，不执行扫描或完整设备快照 |
| 网络 | `PUT /api/network/wifi/connection` | 启动一个有时限的异步连接并保存任务 |
| 网络 | `GET /api/network/wifi/connection?task_id=...` | 轮询连接、持久化和回滚状态 |
| 网络 | `DELETE /api/network/wifi/connection?ssid=...` | 忽略一个网络，无需依赖 DELETE 请求体 |
| 系统 | `GET/PUT /api/system/environment` | 读取/保存环境变量，并报告是否需要重启 Agent |
| 系统 | `POST /api/system/environment/apply` | 显式应用已保存的环境变量并重启 Agent |
| OTA | `GET /api/ota/status` | 读取状态、进度和日志摘要；`?logs=0` 在后台轮询时省略日志 |
| OTA | `POST /api/ota/updates` | 创建 OTA 任务并返回其 `task_id` |
| 日志 | `GET /api/logs/agent` | 读取 Agent 日志摘要 |
| 日志 | `GET /api/logs/llm` | 列出可用的 LLM HTTP 日志文件 |
| 日志 | `GET/PUT /api/logs/llm/{name}` | 导出原始 LLM HTTP 日志，或导入同名日志 |
| 日志 | `GET /api/logs/support` | 导出诊断用的支持日志归档 |

以上所有端点都由 Config Web 在 80 端口提供。页面只使用同源的管理请求。因此 Agent 的端口映射不会影响配置门户，
Agent 也不会在 8080 端口上重复提供模型、STT 测试或存储管理处理程序。

通过普通 POST 即可重启或重置某些内容的端点（`POST
/api/agent/restart`、`/api/device/reboot`、`/api/memory/reset`、
`/api/conversation/reset`）以及 `POST /api/config/plan` 会以 `403` 拒绝跨站请求：
即 `Sec-Fetch-Site` 为 `cross-site` 的浏览器请求，或 `Origin` 指向其他主机的请求。
不带 `Origin` 的请求（curl、配套 App 的原生代码）会被接受。

设置分区下的路径（`/wifi`、`/basic`、`/conversation`、`/model`、`/voice`、`/memory`、`/storage`、
`/advanced`、`/firmware` 及其下的任意路径）会返回设置页面，以便页面可以被重新加载或直接链接；
其他任何未知路径都返回 `404`。

`POST /api/network/wifi/scan` 保留旧的 `networks` 字符串数组，同时还返回 `network_details`。
每条详情包含 `ssid`、`secured`，以及驱动上报时的 `signal_dbm` 和 `signal_percent`。
同样，`wifi_status` 中的当前连接在 `iw link` 上报时也会包含 `signal_dbm`。
信号值是扫描/链路操作时的观测结果，在不提供这些信息的驱动上可能缺失。

`GET /api/config` 和 `GET /api/device/snapshot` 包含 `config_valid` 以及由 `{field, message}`
对象组成的 `config_errors` 数组。语义校验错误不会使这些端点不可用；它们用于标识恢复界面需要高亮的字段。
页面不渲染的字段，或校验器没有归属到任何字段的消息，会报告空的 `field`，并改为在详情中显示。
无法读取或解码的 `agent.toml` 会返回不可用响应，因为无法构造出可编辑的配置。
文件完全不存在则不算校验失败：运行时会将其视为内置默认值，页面渲染这些默认值，保存时会创建该文件。

`GET /api/storage/status` 和 `GET /api/device/snapshot` 的 `storage` 字段使用以下相同的响应结构：

```json
{
  "effective_mode": 1,
  "card": {
    "present": false,
    "mounted": false,
    "device": "",
    "total_bytes": 0,
    "free_bytes": 0,
    "reason": ""
  },
  "mount_point": "/mnt/sdcard",
  "format_job": {"status": "idle"},
  "migration": {"status": "idle"}
}
```

`format_job` 是异步的，其状态可以是 `idle`、`running`、`success` 或 `failed`。
`migration` 表示从 eMMC 到 SD 的后台迁移，使用相同的状态值。卡可能存在但未挂载，
因此客户端在启用格式化和弹出操作时，必须分别使用 `card.present` 和 `card.mounted` 判断。

## 配置保存响应

`PATCH /api/config` 只接受 `{"config":{...}}`。已废弃的请求字段（`apply_wifi`、`agent.default_platform`、
`agent.instruction`、扁平的提供商凭据、`model.base_url`、`audio.playback_backend`、提供商记录中的
`provider` 别名）会被拒绝。提供商记录使用 `type`；凭据应放在对应的提供商记录中。
响应使用 `reboot_required`，不再包含以前的重启别名。

成功的 `PATCH /api/config` 会持久化文件并排队运行时应用：

```json
{
  "ok": true,
  "persisted": true,
  "applied": false,
  "pending": true,
  "state": "pending",
  "revision": 123,
  "changed_paths": ["model.model"],
  "reboot_required": false,
  "reboot_reasons": [],
  "apply": "live",
  "apply_reasons": [],
  "agent_restart_scheduled": false
}
```

轮询 `GET /api/config/application` 获取 `state`、`pending`、`applied`、`reboot_required`、
`revision`、`applied_revision`、`persisted_revision` 以及可选的 `error`。
`persisted_revision` 标识当前磁盘上的文件，与运行时最近一次请求的 `revision` 相互独立。
在 Agent 状态不可用时，HTTP 503 响应中也会包含它，因此客户端仍能观察到外部编辑。
出现此类错误时，客户端必须保留不可用的运行时状态。文件版本变化会使缓存的配置和校验结果失效；
仅凭运行时状态匹配并不能确定当前文件已被应用。
`persisted` 确认原子文件写入已完成；`applied` 确认该版本中的所有设置都已生效。版本号是不透明的 64 位值；
浏览器客户端应使用返回的状态，而不是对版本号做数值运算。

| 状态 | 含义 |
| --- | --- |
| `pending` | 等待当前任务、工具操作或受影响的语音会话结束 |
| `applied` | 已保存的版本已生效 |
| `reboot_required` | 在线设置已生效；USB 标识/布局变更等待显式的设备重启（`applied=false`） |
| `failed` | 应用失败；检查 `error`，修正配置后重试 |

守护进程启动时，状态会根据已保存的配置以及当前设备启动所使用的 USB 设置进行初始化。
应用失败后，UI 会保留已持久化的值，并提供 **Retry
apply**（使用 `{"config":{}}` 调用 `PATCH /api/config`）。同步的持久化、服务或重新加载请求失败会返回错误的 HTTP 状态；
异步的组件失败由状态端点报告。两者都不会安排 Agent 重启。帧/存储的应用可能在之后某个组件失败之前就已完成。
失败的帧/存储工作会在下一次保存时重试。状态中可能包含一个 `runtime_id`，用于标识作出响应的 Agent 进程。
重新加载请求的错误与所请求的版本绑定，一旦 Agent 报告该版本已应用或正在等待 USB 重启，错误就会清除。
帧/存储失败需要一次成功的服务重试才能清除。

`reboot_required` 表示 USB 描述符变更（Android 与非 Android 设备类型之间的切换）以及 `hid.keyboard_layout` 变更。
运行时会保留当前的 USB 设置，直到重启。一个以 boot ID 为作用域的缓存 `<config-dir>/cache/usb-boot.json`
以及已绑定的 USB 描述符，会在后续保存和仅重启 Agent 时保留这一例外状态；将这些设置改回原值会清除它。
重启是用户的显式操作，保存永远不会触发重启；页面如何请求重启参见“应用级别”。CLI 的 `--device-type`
仍会为当前进程覆盖文件中的值。

## 应用级别

每个已保存的变更都有一个应用级别，定义在 Agent 中（`agent.ApplyLevel`，
`src/agent/internal/agent/apply_level.go`），这样 Config Web、设置页面和配套 App 都不需要维护各自的列表：

| `apply` | 含义 | 执行方式 |
| --- | --- | --- |
| `live` | 由运行中的 Agent 热重载 | 无需其他操作 |
| `agent_restart` | 只在 Agent 启动时读取（系统环境变量文件） | `POST /api/agent/restart`；页面和 USB 链路保持在线 |
| `reboot` | 改变了手机所枚举的 USB gadget（`apply_reasons`：`pointer_mode`、`keyboard_layout`） | `POST /api/device/reboot`；与设备的所有连接都会断开 |

重启设备包含了重启 Agent。级别取决于变更后的结果值，而不是编辑了哪个字段：在 iOS、macOS、Windows
和 Linux 之间切换设备类型是 `live`，而切换到 Android 或从 Android 切换出来则是 `reboot`。

`POST /api/config/plan` 接受与 `PATCH /api/config` 相同的 `{"config":{...}}` 请求体，
运行 `agent config-update --dry-run`，且不写入任何内容：

```json
{"ok": true, "apply": "reboot", "apply_reasons": ["keyboard_layout"], "changed_paths": ["basic_settings.device.hid.keyboard_layout"], "reboot_required": true}
```

设置页面会在用户编辑时调用它。`live` 变更一经提交就会保存，页面不显示保存按钮；任一重启级别的变更都会被暂存，
页面为所有暂存的变更提供 "Save & Restart"（`apply.save_restart`）。点击后会弹出一个注明将要执行何种重启的确认框；
只有在用户确认后，页面才会保存并调用重启端点，因此重启始终是用户的显式操作，`PATCH /api/config`
本身永远不会重启任何东西。保存响应中的 `apply` 是权威结果。`GET/PUT /api/system/environment` 同样会报告 `apply`。

在配套 App 内，页面通过 `aico_config_save_state` `{title, dirty, apply, action_label}`
将级别报告给原生导航栏（`title` 和 `action_label` 已翻译为设备语言，因为 App 不为它们维护文案目录），
并通过 `aico_config_restart` `{apply, phase}`（`started`、`finished`、`failed`）通告重启。
在 `reboot` 期间，Android 上的 App 会重新绑定 USB 网络，直到页面报告重启完成，因为否则进程会一直绑定在已消失的网络上。

## Agent 重启生命周期

`PUT /api/system/environment` 持久化文件并返回 `agent_restart_required`；它不会重启 Agent。
`GET` 通过比较已保存的文件与 Agent 已加载的环境版本来报告同一标志。Agent 状态不可用时，`GET` 对该标志返回 `null`；
UI 会保留上一次确认的需求。没有 Agent 确认的保存需要执行应用。该标志在 Config Web 和 Agent 进程重启后依然保留，
只有在已保存的环境被加载后才会清除。`POST /api/system/environment/apply` 就是显式的
**Apply and restart Agent** 操作：它会刷新 Wi-Fi 代理，然后安排 Agent 重启。代理/重启失败会返回错误，且可以重试。
配置变更使用上文所述的在线应用生命周期。

## 存储所有权

Config Web 是 `StorageManager` 的唯一所有者：它负责检测和挂载 SD 卡、执行格式化/弹出操作、迁移受管数据，
并写入 `/run/aico/storage.state`。Agent 运行时从不执行这些硬件操作。它使用一个只读的状态视图，
在提供之前已迁移的音频时把 SD 归档根目录包含进来。

## Wi-Fi 任务生命周期

`GET /api/network/wifi/status` 返回 `wifi`（已保存的配置，代理 URL 已脱敏）和 `wifi_status`
（当前的 `connected`、`ssid`、`state`、`ip_address`、可选的 `signal_dbm`，以及 `observed_at_unix_us`，
即主探测完成时设备的 Unix 时间戳，单位为微秒）。聚合快照携带相同的时间戳，因此客户端可以不依赖响应到达顺序来比较无线观测结果。
该实时端点不做缓存，探测截止时间为三秒，并且在连接和回滚任务期间仍可读取。`wpa_cli` 读取卡住时，
会为 `iw` 回退留出时间。IPv4 和信号信息的补充使用各自独立的短时预算；补充信息缺失不会使已确认的无线关联失效。
如果两个无线探测都没有得到可用的观测结果，则返回 HTTP 503 和 `Wi-Fi status unavailable`。
如果探测成功并确认已断开，且配置加载也成功，则返回 HTTP 200。配置加载失败时返回 HTTP 503 和
`Wi-Fi profiles unavailable` 或 `Wi-Fi proxy profiles unavailable`，包括代理配置文件无法读取的情况。
无线关联和任务结果是两回事：`connected` 反映关联状态，而任务成功还要求获取到地址并完成持久化。
新关联的 SSID 可能要等其任务成功后才会被保存。

`PUT /api/network/wifi/connection` 会立即返回 HTTP 202 和一个 `task_id`。该任务的总截止时间为 120 秒，
涵盖候选配置的应用、验证、持久化和回滚。客户端轮询 GET 形式，直到状态为 `succeeded` 或 `failed`；
失败响应会区分候选配置的应用结果和回滚结果。

失败的连接任务响应包含一个稳定的 `failure_reason` 字符串。只有当 `wpa_supplicant` 针对所请求的 SSID
上报 `WRONG_KEY` 控制事件时，其值才为 `wrong_password`。其他取值包括 `association_failed`、
`ip_unavailable`、`apply_failed`、`persistence_failed`、`timeout`、`connection_unconfirmed` 或
`configuration_error`。客户端必须使用该字段来呈现面向用户的错误状态，而不是把每个 `ok: false` 都当作密码错误。

PUT 请求体可以包含 `proxy_mode`，取值为 `system`、`direct` 或 `proxy`。`proxy` 还需要一个使用 HTTP、
HTTPS 或 SOCKS5 的 `proxy_url`。成功后，策略会按 SSID 保存到 `/userdata/system/wifi-proxies.json`；
连接失败会同时回滚 Wi-Fi 和代理的变更。快照和任务响应返回的是脱敏后的代理 URL，DELETE
会删除该 SSID 的代理映射及其 Wi-Fi 凭据。

`no_proxy` 只在 `proxy_mode=proxy` 时被接受，用于定义该 SSID 自定义代理的绕过规则。
`proxy_mode=system` 使用 `/userdata/system/env` 中的代理和 `NO_PROXY` 值，不做按 Wi-Fi 的覆盖。

## LAN 访问

两个相互独立的设置保护着开发板上的服务：

- **LAN 访问**决定谁可以访问这些服务。默认关闭。
- **设备密码**决定谁必须登录。默认未设置。

LAN 访问关闭时：

- Config Web（80 端口）和 Agent（8080 端口，包括 `/api/tools/*`、手机桥接和 `/webtty/`）会拒绝所有既不是通过其绑定到
  `usb0` 的 socket 到达、也不是来自开发板自身的请求。`/mcp` 不受该开关影响；参见
  [MCP 访问 token](#mcp-访问-token)。API 客户端会收到 `403` 和 `"error": "lan_access_disabled"`；
  浏览器会收到一个简短页面，说明如何开启该功能。
- ttyd 只监听回环地址。USB 客户端使用 `http://192.168.42.1:3000/webtty/`，由 Config Web 在绑定到
  `usb0` 的 socket 上提供；局域网客户端使用 Agent 的 `/webtty/`，它遵循该开关。

LAN 访问开启时，局域网客户端可以访问与 USB 客户端相同的服务。

未设置设备密码时，所有能够访问这些服务的客户端都无需登录即可进入。一旦设置了密码，除开发板自身外的所有客户端都需要会话 token，
无论是通过局域网还是 USB 到达（参见[登录与配对](#登录与配对)）：局域网客户端用密码登录，
USB 客户端无需密码即可配对，因为只有设备所有者才握有那根线缆。配套 App 会通过 USB 静默配对，
之后在 USB 和 Wi-Fi 上都使用其 token。`/mcp` 遵循自己的规则：所有客户端（包括 USB 和板上的客户端）都要发送
[MCP 访问 token](#mcp-访问-token)。

| LAN 访问 | 设备密码 | USB 客户端 | 局域网客户端 |
| --- | --- | --- | --- |
| 关闭 | 未设置 | 允许 | `403 lan_access_disabled` |
| 开启 | 未设置 | 允许 | 允许 |
| 关闭 | 已设置 | 需要会话（配对） | `403 lan_access_disabled` |
| 开启 | 已设置 | 需要会话（配对） | 需要会话（登录） |

SSH 和 Web 终端使用设备密码登录 `aico` 账户。未设置密码时，该账户的密码处于锁定状态，因此任何人都无法通过
USB 或局域网登录 shell，即使使用镜像以前的出厂密码也不行；请先设置设备密码（如果你愿意，可以设为 `luckfox`）。
`root` 仍然只允许密钥登录。只有在 LAN 访问开启**并且**已应用设备密码时，sshd 才接受来自
`192.168.42.0/24`、`127.0.0.0/8` 和 `::1` 以外的来源；否则它会对所有账户（包括 `root`）拒绝这些来源。

### 跨站请求

USB 客户端无需密码即可配对，未设置密码时所有可达的客户端都会被允许，但线缆另一端的手机或电脑上也运行着浏览器，
它加载的页面可以向 `192.168.42.1` 发送请求，这些请求会像 App 自己的请求一样到达 USB socket。
因此，Config Web、Agent 和 3000 端口上的 USB 终端会在所有连接上、先于其他任何检查，以 `403`
和以下错误之一拒绝代表其他站点发起的浏览器请求：

| 错误 | 请求 |
| --- | --- |
| `cross_site_request` | `Sec-Fetch-Site: cross-site`，但顶层 `GET` 导航（`Sec-Fetch-Mode: navigate`、`Sec-Fetch-Dest: document`）除外，例如打开一个共享文件链接 |
| `host_not_allowed` | `Host` 是 `localhost` 以外的名称，而不是 IP 地址（DNS 重绑定） |
| `origin_not_allowed` | `Origin` 为 `null`，或不是主机为 IP 地址或 `localhost` 的 `http`/`https` URL，或与 `Host` 不匹配 |

原生客户端（配套 App、MCP 主机、`curl`）既不发送 `Origin` 也不发送 `Sec-Fetch-Site`，并通过 IP 访问开发板，
因此不受影响。请通过 IP 地址打开开发板；mDNS `.local` 名称之类的主机名会被拒绝。

`GET /api/security/lan-access` 永远不会返回密码或其哈希：

```json
{"ok": true, "enabled": false, "password_set": false, "via_usb": true, "min_length": 6}
```

已设置设备密码时，`PUT /api/security/lan-access` 接受来自任何已获准客户端的请求；未设置密码时，只接受通过
USB 的请求（否则返回 `403`），因此局域网客户端无法通过设置第一个密码来占有开发板。它也只接受同源请求。
请求体包含 `enabled`、`password` 或两者；两者相互独立：

```jsonc
{"enabled": true}                                         // turn on, with or without a password
{"enabled": false}                                        // turn off; the password and sessions are kept
{"password": "6 characters to 72 bytes"}                  // set the first password
{"password": "new password", "current_password": "old"}  // replace it
```

替换已有密码时，所有客户端（包括 App）都需要提供 `current_password`（否则返回 `403 current_password_invalid`），
这样被盗用的会话也无法把所有者锁在门外。错误的当前密码与登录共用同一个按地址计数的锁定机制
（返回 `429 too_many_attempts` 并带 `Retry-After`）。设置或替换密码会让所有客户端（包括 App）退出登录；App 会通过 USB 重新配对。

`DELETE /api/security/password` 会清除设备密码，效果如同从未设置过：它会撤销所有会话，并重新锁定 `aico` 登录。
LAN 访问设置和 MCP 访问 token 会保留，因此在 LAN 访问开启时，局域网随后无需登录即可访问 Config Web 和 Agent（但无法获得 shell）。
它要求所有客户端都提供当前密码（`{"current_password": "old"}`；否则返回 `403
current_password_invalid`）。

忘记的密码需要在开发板本身上移除，这需要 root 权限：删除 `/userdata/system/device-password` 并运行
`/usr/lib/aico/aico-lan-access-sync`（例如，让 Agent 来执行，或使用 root SSH 密钥）。Agent 和 Config Web
每次请求都会读取该文件，因此改动会立即生效；辅助程序会重新锁定 `aico` 登录。之前签发的会话仍可验证，但已不再需要，
下次设置密码时会撤销它们。两个端点都会返回状态以及 `system_synced`；当 SSH 或登录账户未能更新时，该值为 `false`。
无论哪种情况，HTTP 服务都会立即遵循已存储的状态。对于 SSH：

- 如果设备密码未能应用，规则文件会让 SSH 只在 USB 链路上开放，即使开关已开启。
- 如果 sshd 没有确认重新加载，正在运行的 sshd 可能仍在使用之前的规则，例如在开关关闭后仍对局域网开放。
  之后的每次同步都会再次重新加载 sshd，无论规则文件是否变化；sshd 每次启动时也会读取规则文件。

当 `system_synced` 为 `false` 时，请检查 `systemctl status aico-lan-access ssh` 和
`/run/aico/sshd/lan.conf`，然后重试该更改并检查新的 `system_synced`。每次重试都会再次应用密码并重新加载 sshd。
重启 sshd 只在重新加载失败时有帮助：它会重新读取规则文件，但不会应用密码，因为 `aico-lan-access.service`
每次启动只运行一次。该单元在每次启动时都会重试，但持续性的故障（例如密码文件损坏）仍会再次失败。

设备密码至少为 6 个字符，最多为 UTF-8 编码下的 72 字节（bcrypt 的限制），因此由多字节字符组成的密码可能在达到
72 个字符之前就被以 `400` 拒绝。

状态保存在 userdata 上，因此在 OTA 后依然保留：

| 文件 | 内容 |
| --- | --- |
| `/userdata/system/lan-access-enabled` | LAN 访问开启时存在 |
| `/userdata/system/device-password` | 设备密码的 bcrypt 哈希（权限 0600） |
| `/userdata/system/lan-session-secret` | 用于签名会话 token 的密钥（权限 0600） |
| `/userdata/system/mcp-tokens.json` | MCP 访问 token 的名称、创建时间和 SHA-256 哈希（权限 0600） |

`/usr/lib/aico/aico-lan-access-sync` 负责应用这些状态：它写入 `/run/aico/sshd/lan.conf`
（由 `/etc/ssh/sshd_config.d/20-aico.conf` 最后包含），将 `aico` 的密码设置为设备密码，并在每次运行时重新加载 sshd。
Config Web 会等待重新加载完成；启动单元则不会等待，因为 `ssh.service` 排在它之后。没有设备密码或密码已损坏时，
它会锁定该账户（`usermod -p '*'`）；镜像出厂时该账户也是锁定的。
`aico-lan-access.service` 在每次启动时运行它，因为 `/run` 是易失的，而 OTA 会重置 `/etc/shadow`；
`ssh.service` 依赖该单元，因此 sshd 永远不会在没有规则的情况下启动。Config Web 在每次更改后都会运行该辅助程序。

在 Docker 和主机上运行的 Agent 与 Config Web 不传递 `-lan-guard` / `--lan-guard`，因此不受影响。

### 登录与配对

设置了设备密码时，Config Web（80 端口）、Agent（8080 端口）和 USB 终端（3000 端口）各自提供登录端点，
并拒绝所有不带有效会话 token 的其他请求，来自开发板自身的请求除外：

- 浏览器（带 `Accept: text/html` 的 `GET`）会以 `303` 重定向到 `/login?next=<original path>`。
  在局域网上，该页面会要求输入密码；在 USB 链路上，它会为浏览器配对并直接跳转到 `next`，不要求输入。
- 其他客户端会收到 `401`，以及 `"error": "login_required"` 和 `WWW-Authenticate: Bearer`。

LAN 访问关闭时，发往登录端点的局域网请求会像其他局域网请求一样被拒绝。未设置密码时，`/login` 会直接重定向到 `next`。

`POST /api/auth/login` 接受设备密码：

```jsonc
// Content-Type: application/json
{"password": "device password"}
// 200
{"ok": true, "token": "v1.<expiry>.<issued>.<signature>", "expires_at": "2026-12-09T08:00:00Z"}
```

`POST /api/auth/pair` 不带请求体，只在 USB 链路上（否则返回 `403 usb_required`）且同源时被接受。
它返回同类 token，外加 `password_set`：

```json
{"ok": true, "token": "v1.<expiry>.<issued>.<signature>", "expires_at": "2026-12-09T08:00:00Z", "password_set": true}
```

配套 App 首次通过 USB 连接时会静默配对，保存 token，此后无论通过 USB 还是 Wi-Fi 都以 bearer 方式发送它。
当请求返回 `401 login_required` 时，如果它在 USB 上就会重新配对（密码已更改或清除，或 token 已过期）；
如果只有 Wi-Fi，它会等待下一次 USB 连接。配对得到的 token 与登录得到的 token 能访问的内容完全相同。

两个响应还会设置 `aico_session` cookie（`HttpOnly`、`SameSite=Strict`、`Path=/`）。`/login`
页面会向登录端点提交一个 HTML 表单，并被重定向到其 `next` 路径，该路径必须是同一主机上的路径。
错误包括 `401 invalid_password`（未设置密码时也会返回）、`403 cross_origin_rejected`（`Origin` 头指向其他主机）
以及带 `Retry-After` 的 `429 too_many_attempts`：同一地址失败五次后，之后每次失败都会使锁定时间加倍，
锁定时间从 30 秒开始，最长 15 分钟。一次成功登录会清零计数。

请求通过 `Authorization: Bearer <token>` 或 `aico_session` cookie 进行认证。Cookie 的作用域是主机而不是端口，
因此在 80 端口登录的浏览器在 8080 端口上同样处于登录状态，包括 `/webtty/` 和手机桥接 WebSocket。
脚本则发送 bearer token。会话 token 无法访问 `/mcp`；MCP 客户端使用 [MCP 访问 token](#mcp-访问-token)。

token 的有效期为 30 天。浏览器在 token 签发一天以后使用其 cookie 时，会收到一个新的 30 天 cookie，
因此经常使用的浏览器会一直保持登录。token 不会被存储：它们由 Agent 和 Config Web 共享的
`/userdata/system/lan-session-secret` 签名。设置、替换或清除设备密码会替换该密钥，并让所有客户端（包括已配对的 App）退出登录。
开启或关闭 LAN 访问则不会。`POST /api/auth/logout` 只清除 cookie；bearer token 在过期或密钥被替换之前仍然有效。

`GET /api/auth/session` 返回 `{"ok": true, "via_usb": false,
"password_set": true, "authenticated": true}`。

Config Web 的 HTTP 端点是明文的，因此任何能够在局域网上抓包的人都可以截获密码或会话 token。
对于 Wi-Fi 客户端，请使用 Config Web 的 HTTPS MCP 端点，并信任开发板本地的 TLS CA。
直接访问 Agent 的 HTTP MCP 只接受通过 USB 或回环地址的请求。只在受信任的网络上开启 LAN 访问，并撤销不再使用的 MCP token。

### MCP 访问 token

Agent 上的 `/mcp` 要求所有客户端（包括直接插在 Aico Link 上的电脑和开发板上的进程）都提供 MCP 访问 token，
并且不接受其他任何凭据：会话 token 不行，仅凭 USB 链路也不行。它不依赖 LAN 访问，因此在 LAN 访问保持关闭时，
同一 Wi-Fi 下的电脑也可以使用 MCP。创建 token 需要一个已获准的客户端（未设置设备密码时需为 USB 客户端），
而创建 token 正是开放 MCP 的方式。MCP 访问 token：

- 只能访问 MCP `POST /mcp`，可以通过 Config Web 的 HTTPS 反向代理访问，也可以通过 USB 直接访问 Agent 的 8080 端口。
  在其他任何地方都会被拒绝，包括 `/api/tools/*`（它会绕过 MCP 工具白名单和 `mcp_shell_enabled` 开关）、
  Config Web、备份、终端和 SSH；
- 永不过期，并且在密码被修改或清除、LAN 访问被关闭后依然有效。每个 token 在创建时命名，并可单独撤销。
  关闭 MCP（`mcp_enabled`）可以一次性阻止所有客户端。

在 Settings > Advanced > MCP 中，或通过以下端点创建和撤销 token。设置了设备密码时，它们接受来自任何已获准客户端
（已登录或已配对）的请求；未设置密码时只接受通过 USB 的请求，因为 token 的寿命会长于之后设置的密码；token 本身永远不能管理 token。
写操作必须同源。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/security/mcp-tokens` | 列出 token（从不返回其密钥或哈希） |
| `POST` | `/api/security/mcp-tokens` | 创建 token：`{"name": "MacBook Claude Code"}` |
| `DELETE` | `/api/security/mcp-tokens/{id}` | 撤销一个 token |

```jsonc
// POST /api/security/mcp-tokens → 201
{"ok": true,
 "token": {"id": "3f9a1c0b7e2d", "name": "MacBook Claude Code", "created_at": "2026-11-10T08:00:00Z"},
 "secret": "aico_mcp_3f9a1c0b7e2d_<43 characters>"}
// GET /api/security/mcp-tokens → 200
{"ok": true, "tokens": [{"id": "3f9a1c0b7e2d", "name": "MacBook Claude Code", "created_at": "2026-11-10T08:00:00Z"}]}
```

密钥只返回一次且不会被存储：开发板只保留其 SHA-256 哈希。名称为单行的 1 到 64 个字符（否则返回 `400`）；
token 数量不受限制。撤销未知 ID 会返回 `404`。token 文件缺失或损坏时不接受任何 token。客户端以
`Authorization: Bearer <secret>` 发送密钥；Wi-Fi 下请使用 Config Web 的 HTTPS URL，客户端配置参见
[MCP](../05-agent/mcp.md#认证)。

## API 边界

已废弃的路由，例如 `/api/wifi/*`、`/api/system/env`、`/api/ota/update`、`/api/reboot`、除 `POST /api/agent/restart` 之外的 `/api/agent/*`
和 `/api/llm-logs/*`，已被移除并返回 `404 Not Found`；不提供任何兼容适配。客户端必须使用上表中的规范资源。
`GET /api/config` 只具有配置语义，在进程内使用规范的编辑器加载器和不含凭据的 DTO。它包含已持久化的版本、
校验状态和配置路径。前端分别独立读取配置、固件、Wi-Fi 和存储，因此一个缓慢的状态探测不会拖慢无关的摘要。
`/api/device/snapshot` 仍可用于聚合这些资源。

## Agent 内部重新加载

`POST /api/internal/config/reload` 只接受回环请求，并要求请求体为 `{"revision":<nonzero revision>}`。
Agent 会校验已保存文件的版本，排队应用，并返回 HTTP 202 和 `pending=true`。对同一仅限回环的路由执行 `GET`
会返回应用状态。过期的版本返回 HTTP 409。待处理的保存会被合并；当前应用完成后，最新排队的快照生效。
无法解码或包含未知字段的请求体会返回 HTTP 400 和解码器给出的原因，因此确实包含了版本号的请求永远不会被报告为缺少版本号。
这些端点不属于公开管理 API。

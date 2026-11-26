---
sidebar_position: 7
---

# USB HID 与设备控制

本项目通过 Linux USB gadget 框架配置 Luckfox Pico Zero 的 USB-C 端口。
`aico-usb-gadget.service` 会创建一个复合 gadget，包含键盘 HID、指针/触控 HID、Consumer Control
以及 CDC ECM 网络；这些 HID 节点用于模拟键盘、鼠标和触控输入。

## 启动方式

Debian 固件通过 systemd 负责 gadget 的启动：

```bash
systemctl status aico-usb-gadget.service --no-pager
systemctl restart aico-usb-gadget.service
```

该单元直接执行 Debian rootfs 中的辅助脚本：

```bash
/usr/lib/aico/aico-usb-gadget start
```

它会：

1. 挂载 configfs：`/sys/kernel/config`；
2. 加载 `dwc2` 模块；
3. 加载 `libcomposite` 模块；
4. 创建并绑定复合 gadget。

`aico-usb-gadget.service` 是生产环境中权威的启动路径。它还会在 `192.168.42.1` 上启用 `usb0`，
供配置页面和本地 USB 网络使用。组装完成的复合设备只会绑定到 UDC 一次。对于 iOS，它由
Consumer Control + 触控板 + ECM 组成，不含键盘（参见
[iOS 触控板与按需键盘](#ios-触控板与按需键盘)）；其他设备类型还会链接键盘。
不要在启动时为同一身份添加解绑/重新绑定：在线缆保持连接时，iOS 可能保留物理 USB 会话，
却以不一致的状态重建 HID 接口。物理拔插才会彻底拆除该主机会话。

重启该单元确实会删除并重建 configfs 组。厂商内核对这一路径处理有误：`CONFIG_USB_CONFIGFS_UEVENT`
保留了一个全局的 `android_device`，拆除时从未清除，因此下一次控制传输会解引用一个已释放的 `struct device`。
生产内核在 `pico-sdk/sysdrv/source/kernel/drivers/usb/gadget/configfs.c` 中包含了修复。
它从 gadget 本身推导 gadget 状态，在销毁设备之前取消待处理的工作，并在拆除时清除全局设备指针。
历史上的 [SDK 提交](https://github.com/AidenAI-IO/aiden-sdk/commit/abe2bc1fa59691c81418c3cba64eaf703f55870c)
以 `usb: gadget: configfs: fix use-after-free on
gadget teardown` 为标题记录了这三个缺陷。在修改拆除流程之前，请先阅读该说明。

## example_usb_hid 用法

```text
example_usb_hid [global options] setup <keyboard|touch|composite>
example_usb_hid [global options] cleanup
example_usb_hid [global options] keyboard press <KEY> [KEY ...]
example_usb_hid [global options] keyboard release [KEY ...]
example_usb_hid [global options] keyboard tap <KEY> [KEY ...]
example_usb_hid [global options] keyboard text <TEXT>
example_usb_hid [global options] touch move <X> <Y>
example_usb_hid [global options] touch click <X> <Y> [left|right|middle]
example_usb_hid [global options] touch click [left|right|middle]
example_usb_hid [global options] touch down [left|right|middle]
example_usb_hid [global options] touch up
example_usb_hid [global options] touch scroll <amount>
example_usb_hid [global options] server [PORT]
```

### Server 命令

`server` 命令会启动一个简单的 HTTP 服务器，通过 POST 请求接收 HID 命令。这对远程控制或集成测试很有用。

```bash
sudo example_usb_hid server 8090
```

服务器监听指定端口（未指定时默认为 8080），并接收包含命令描述的 JSON 负载。

常用示例：

```bash
sudo example_usb_hid keyboard tap ENTER
sudo example_usb_hid keyboard tap CTRL ALT DELETE
sudo example_usb_hid keyboard text "hello from pico"
sudo example_usb_hid touch click 16000 16000
sudo example_usb_hid cleanup
```

## 全局参数

| 参数                           | 说明                 |
| ----------------------------------- | --------------------------- |
| `--gadget-root PATH`                | configfs gadget 根路径   |
| `--gadget-name NAME`                | gadget 名称                 |
| `--keyboard-dev PATH`               | 键盘 HID 设备         |
| `--touch-dev PATH`                  | 触控/鼠标 HID 设备      |
| `--state-dir PATH`                  | 状态文件目录        |
| `--manufacturer TEXT`               | USB 制造商字符串     |
| `--product-name TEXT`               | USB 产品字符串          |
| `--serial TEXT`                     | USB 序列号字符串           |
| `--vendor INT` / `--product-id INT` | USB VID/PID                 |
| `--udc NAME`                        | 指定 UDC                 |
| `--width INT` / `--height INT`      | 坐标空间尺寸 |
| `--duration-ms INT`                 | 输入动作持续时间       |
| `--force`                           | 强制执行某些操作    |

## Agent HID 配置

```toml
[basic_settings.device]
device_type = "iOS"

[advanced_settings.hardware.hid]
keyboard_device = "/dev/hidg0"
keyboard_layout = "qwerty"
mouse_device = "/dev/hidg1"
android_keyboard_device = "/dev/hidg2"
frame_socket = "/run/frame_service/frame_service.sock"
```

`[basic_settings.device].device_type` 决定 `hid.usb1` 的指针类型：

| `device_type` | `pointer_mode` | `hid.usb1` |
| --- | --- | --- |
| `iOS` | `touchpad` | Digitizer 触控板，iOS 将其作为直接触控使用 |
| `Android` | `touchscreen` | Digitizer 触摸屏 |
| `macOS`、`windows`、`linux` | `absolute` | 绝对坐标鼠标 |

固件还会将 `hid.usb2` 绑定为 `/dev/hidg2`。这第二个类键盘接口声明 Consumer Control 用途。
在 `pointer_mode =
"touchscreen"` 下，它用于 Android 扩展按键，例如返回、主屏幕、应用切换、搜索、电源和音量。
`touchpad` 和 `absolute` 模式提供一个更小的位图式媒体键接口，只声明音量静音/加/减、媒体播放控制、截屏以及亮度加/减。

内置的 Agent 工具：

- `keyboard_tap`
- `keyboard_text`
- `mouse_move`（仅限绝对坐标鼠标）
- `mouse_scroll`（仅限绝对坐标鼠标；直接触控通过 `touch_gesture` 滑动来滚动）
- `touch_gesture`

`touch_gesture` 在常规交互中使用标准的 `type` 手势。它还支持一种低频使用的原子 `actions` 程序，
用于标准手势无法表达的、对触点敏感的自定义输入。该程序使用归一化的 `0..1000` 坐标点以及
`touch_down`、`move_to`、`wait` 和 `touch_up` 动作；它在一个 HID 指针会话中执行，并且必须在返回之前释放所有触点。示例：

```json
{"actions":[{"action":"touch_down","point":{"x":500,"y":700}},{"action":"wait","ms":80},{"action":"move_to","point":{"x":500,"y":300},"speed":2500},{"action":"touch_up"}]}
```

`move_to.speed` 是可选的，单位为每秒归一化坐标单位。同时提供 `speed` 和 `duration_ms` 时，
以 `duration_ms` 为准；两者都省略时保留现有的立即移动行为。在保持触点按下期间，带时长的原子移动使用与
HID 滑动主运动相同的五次加速和制动曲线。对于已知位于内部的触点，紧接着释放的带时长移动还会保留最后两个归一化单位
（最多为该段的一半）用于 100ms 的真实低速移动，主运动最短为 180ms，与标准 HID 滑动一致。
这会在请求的主运动时长之外增加释放时间；它并不是在终点静止停留。从边缘起始的手势、显式的结束等待、
立即移动以及指定了不同终点的释放会保留各自的时序。主曲线同样覆盖预定义快捷操作中的带时长移动。
该曲线配置在 `mnk/motion_profile.go` 中共享；新的触控工具应调用 `SwipeWithOptions` 或
`TouchActions`，而不是自行实现曲线。

如果在模型据以选择坐标的那张截图之后，屏幕在竖屏和横屏之间发生了旋转，则不会发送
`touch_gesture` 或 `mouse_move`；工具会返回当前截图，以便模型重新选择目标。

Agent 的 ADB 输入后端会使用 `sendevent` 将标准滑动作为一个连续的 DOWN/MOVE/UP 程序执行，
并以 `input motionevent` 作为回退。两条路径对带时长的移动使用相同的曲线和符合条件的释放尾段。
对于标准的多点滑动，180ms 的最短时长对整个符合条件的主路径只应用一次，随后是一次 100ms 的释放尾段。
中间段保持线性；只有最后一个主运动段使用制动曲线，与 HID 一致。`steps` 控制插值，可选的停留发生在触点按下期间。
ADB 命令和注入的开销可能会延长实际耗时。这些变更不影响鼠标滚轮事件、ADB 的 `mouse_scroll` 近似实现、
专用的拖拽接口，也不影响独立的基准测试桥接后端。原子程序最多包含 128 个动作，每个动作声明的时长不超过 30 秒，
所有声明时长之和不超过 60 秒。HID 和 ADB 在发送任何输入之前，都会同时检查提交的程序和展开后的运动曲线（包括其释放尾段）。

在常规的快捷操作和脚本中，默认使用受支持的单对象 `type` 形式；不支持 `type:"drag"`。

移动可拖拽目标时绝不使用原子 `actions`。它使用两次 `touch_gesture` 调用，以便 Agent
在选择最终目的地之前观察拖拽状态：

```json
{"type":"drag_start","point":{"x":400,"y":500}}
{"type":"drag_release","point":{"x":750,"y":500}}
```

`drag_start` 会在当前目标上按住 500ms，然后沿可用屏幕空间最大的轴，以每秒 500 个归一化单位的速度
自动移动恰好 200 个归一化单位（一次 400ms 的插值移动），并在其内部的屏幕稳定等待期间保持触点按下。
等待成功时，触点会一直保持按下直到最终截图完成。只有当结果报告 `screen_stable=true` 时，
Agent 才会根据返回的截图确认目的地，然后调用 `drag_release`。当结果报告 `screen_stable=false` 时，
运行时会移回最初的 `drag_start` 点并释放触点；Agent 会检查返回的截图并重试 `drag_start`，
而不是调用 `drag_release`。在正常拖拽流程中，它不会单独调用 `wait_for_stable_screen`，
也不会根据 `screen_stable=false` 的结果选择目的地。释放调用会直接移动到已确认的点，停留 200ms 后释放。
以前的单次调用 `type:"drag"` 手势已不再支持。

建议使用归一化坐标（`0..1000`，中心为 `500,500`），以避免显示分辨率变化导致点击位置偏移。
对于小按钮、列表项和输入框等密集目标，应优先估算目标中心的归一化坐标。输入工具执行成功后会返回一张操作后的截图；
在继续之前应确认屏幕已变化，以避免重复点击。
`keyboard_layout` 必须与手机解释外接 USB HID 键盘的方式一致。支持的值为 `qwerty`（默认）、`azerty` 和 `qwertz`。可见的软键盘布局并不可靠：手机可能显示 AZERTY 软键盘，却仍然按 QWERTY 解释 Aico 的 USB HID 报告。`keyboard_text` 和 `keyboard_tap` 中的标准文本类按键都使用这一映射。映射本身由 Agent 加载，不会改变 USB 描述符，但 Config Web 要求保存后重启开发板，以便主机开始一个干净的 USB 会话。

在 Config Web 的 `[advanced_settings.hardware.hid]` 下选择匹配的布局（`qwerty`、`azerty` 或 `qwertz`）。大多数用户保持默认的 `qwerty` 即可；只有在输入的字符出现错位时（例如 "shape" 变成 "shqpe"）才需要修改。

运行 iOS 的手机会在 USB 键盘被枚举的那一刻，根据当时处于活动状态的软键盘锁定其硬件键盘布局。之后切换屏幕键盘不会改变已锁定的解释方式。要对齐非 QWERTY 布局：

1. 在手机上将输入语言切换为对应的语言（例如 `azerty` 对应法语，`qwertz` 对应德语）。
2. 在 Config Web 中将 `keyboard_layout` 设置为目标布局并保存。
3. 接受 Config Web 的重启提示，等待开发板和 USB 复合设备重新连接。在 iOS 上，复合设备重新连接时不带键盘；第一次键盘操作才会挂载键盘（参见[按需键盘](#按需键盘)），手机在挂载时锁定硬件布局。在键盘挂载之前，请保持第 1 步的语言处于活动状态。

顺序很重要：要在保存配置*之前*切换语言，因为布局是在枚举时锁定的。Config Web 的保存流程不会热重载，也不会对同一 USB 身份进行软重新枚举：iOS 在这种切换中可能保留不一致的键盘和指针会话。如果取消了立即重启，请在验证新布局之前手动重启开发板。

`keyboard_text` 只能输入可键入的 ASCII 字符。对于中文或其他非 ASCII 文本，请使用 `enter_text`（它利用手机的屏幕键盘和输入法候选词），而不是转写为拼音或罗马化的近似形式。已配置的布局覆盖常见的 ASCII 按键，但各国特有的标点变体可能仍需在设备上验证。

## iOS 触控板与按需键盘

### iPhone 设置

```text
Settings > Accessibility > Zoom                      On
Settings > Accessibility > Touch > AssistiveTouch    Off
```

开启缩放（Zoom）后，iOS 会将开发板的 digitizer 触控板（`05 0d 09 05`，report id 5，两个触点，
1920x1080 逻辑范围）作为直接触控接受：触点落在发送的位置，不绘制指针，也没有悬停。
关闭缩放或开启辅助触控（AssistiveTouch）时，iOS 会忽略所有触控，而键盘输入仍然正常。
digitizer 触摸屏描述符和绝对坐标鼠标不会以这种方式被接受。

触控板的逻辑范围覆盖的是手机屏幕本身，而不是 HDMI 画面，因此归一化的 `0..1000` 坐标可以直接映射到它上面，
镜像画面周围的黑边不受影响。每个报告为 12 字节：
`[0x05, scan_lo, scan_hi, buttons, finger1(4), finger2(4)]`，其中每个手指为
`tip | confidence<<1 | contact_id<<2`，后跟以小端序打包的 X（12 位）和 Y（12 位）。只使用手指 1。

### 按需键盘

连接了硬件键盘时，iOS 会隐藏其软键盘，因此 iOS 复合设备在空闲时不包含键盘：

| 配置 | 接口 | PID |
| --- | --- | --- |
| 未连接键盘（启动时） | Consumer Control（`hid.usb2`）-> 触控板（`hid.usb1`）-> ECM | `0x0180` |
| 已连接键盘 | 键盘（`hid.usb0`）-> Consumer Control -> 触控板 -> ECM | `0x0181` |

所有 HID 都必须位于 ECM 之前：位于 ECM IAD 之后的 HID 会导致 iOS 重置总线。
每种配置都有自己的产品 ID 和序列号，因此 iOS 永远不会复用为另一种配置缓存的描述符。

一次对话式 Agent 运行拥有键盘作用域。其第一个键盘动作（`keyboard_tap`、`keyboard_text`、`enter_text`、
`quick_action` 快捷键、`open_app` 搜索查询）会连接键盘；该运行中后续的键盘、触控和媒体键动作会复用该配置，
无需再次枚举。运行结束时会断开键盘：成功、失败、取消、被抢占以及 panic 清理都会通过一个与上下文无关的最终断开操作离开该作用域。
从不输入文字的运行永远不会重新枚举。连接失败会立即断开，断开失败会在下一次 HID 输入之前重试（冷却 5 秒后）。
直接的 HTTP Tool API 调用在单次调用内遵循相同规则。每次切换大约需要两秒，并记录为
`keyboard attach|detach ok=... ms=...`。

键盘处于连接状态时，软键盘会被隐藏；这是预期行为，Agent 提示词中也有说明。修饰键快捷键
（`Cmd+A`、`Cmd+V`、`Cmd+Space`、`Shift`）可以与触控板同时工作，因此不需要隔离修饰键。
编辑文本的快捷键需要文本已上屏：使用中文输入法时，字母可能仍处于拼音组字状态，此时 `Cmd+A` 什么也选不中。

当 `search_launch_app` 省略 `platform` 时，处于活动状态的 iOS 键盘控制器也会被视为 iOS 平台信号。
这样，即使手机桥接不可用或处于后台，文档中的 `{"app":"WeChat"}` 输入仍会走 `Cmd+Space` Spotlight 路径。

Luckfox 开发板只有一个 UDC，因此连接和断开键盘时会短暂断开整个复合设备，包括 ECM。
动态控制器和 ECM 看门狗共用 `/run/aico_dynamic_keyboard.lock`，以防止 UDC 重置相互重叠。
控制器还会发布一个切换后的短暂宽限截止时间，使看门狗不会把计划内的 ECM 中断计入其停滞阈值。
HID 节点打开恢复也遵循这一截止时间：暂时性的 `/dev/hidg*` `ENXIO` 会等待并重试，而不是强制再次刷新复合设备。
每次切换之前，开发板会发送一条 `usb_reenumeration` BLE 通知，以便配套 App 忽略 ECM 的中断。

手动检查或操作这些配置：

```bash
/usr/lib/aico/aico-dynamic-keyboard status
/usr/lib/aico/aico-dynamic-keyboard attach
/usr/lib/aico/aico-dynamic-keyboard detach
```

任何测试结束后，`status` 都应报告 `mode=detached`，并列出 `hid.usb1`、`hid.usb2` 和 `ecm.usb0`。
`/dev/hidgN` 的编号跟随已链接的功能，因此请在 configfs（`functions/hid.usbN/dev`）中查找，
而不要假定 `/dev/hidg1` 就是触控板。请使用 HDMI 画面反馈来验证效果；写入成功只能证明 gadget 接受了该报告。

### 横屏下的 iOS 触控板

触控板以手机面板自身的坐标报告位置，就像手指放在玻璃上一样，而 HDMI 画面始终是正立的。
因此在横屏下，根据截图选择的点在落下时会旋转四分之一圈，旋转方向取决于手机朝哪个方向转动：

| 手机 | 发送的 `(x, y)` 落在 |
| --- | --- |
| 竖屏 | `(x, y)` |
| 横屏，`turned_left` | `(1000-y, x)` |
| 横屏，`turned_right` | `(y, 1000-x)` |

与辅助触控指针不同，触控板不会在竖屏宽度处被截断，重新枚举也不会改变映射。提供方会将每个点按当前旋转的逆变换发送，
因此点按、滑动、拖拽和 `tap_sequence` 都会落在所选位置。

截图无法区分两种横屏方向，因此横屏画面从 `turned_right` 开始（这是在正立 iPhone 上用强制横屏的 App 测得的方向），
并且在模型看到触控在何处产生反应之前，方向一直处于未确认状态：

- 方向未确认期间，`touch_gesture` 只发送携带 `"mirror_ok": true` 的 `tap`、`double_tap`、
  `long_press` 和 `tap_sequence`，表示每个触点的镜像点 `(1000-x, 1000-y)` 同样是无害的。
  其他手势会以镜像方式执行，因此会被拒绝。屏幕中心的镜像就是它自己。
- 在横屏画面上的 `mirror_ok` 触控（无论方向是否已确认）都是一次方向检查。它的截图在触控后约 300 ms 拍摄，
  不使用操作前基线、屏幕稳定等待或 UI grounding，因为视频控件之类的反应可能在几秒内就会淡出。
  它会用红圈标出目标点（`tap_sequence` 的最后一次点按），用蓝圈标出其镜像点。
- 作为检查的 `tap_sequence` 会跳过其布局保护，并且除非设置了 `interval_ms`，否则在两次点按之间等待 1000 ms。
  这样一次调用就可以唤出会再次隐藏的控件（点按中心）并按下其中一个：使用 400 ms 时，
  第二次点按落在控件出现动画期间而丢失了。
- 随后，如果触控在红圈处产生反应，模型调用 `{"type":"orientation_set","landed":"expected"}`；
  如果在蓝圈处产生反应，则调用 `"landed":"mirror"`，这会翻转方向；然后重新执行该触控。
  由于检查在方向已确认时同样有效，这也覆盖了手机直接从一种横屏方向转到另一种横屏方向的情况。
- 坐标调试（Coordinate Debug）页面以 `mirror_ok` 方式发送点按，因此在横屏下它会使用当前方向
  （确认之前为默认方向），而不会被拒绝；如果点按在镜像位置产生反应，就能看出应如何调整。

当截图重新变为竖屏时，方向信息会被丢弃。因采集失效而被清除的截图会保留正在使用的方向。
`{"type":"orientation_probe","point":...}` 仍作为可选检查保留：它会按下一个在按住时会高亮的控件，
并在保持触点按下时比较该点及其镜像点处的画面。

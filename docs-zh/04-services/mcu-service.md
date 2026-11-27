---
sidebar_position: 7
---

# MCU Service 与烧录后验证

`mcu_service` 在现有的 `aico-mcu-event.service` 单元下取代了原先用于诊断的 Python 日志程序。
它以 115200 8N1 独占 `/dev/ttyS5`，响应 MCU 的状态探测，转发硬件事件，并协调由 MCU、
仅限 root 的本地 CLI 或标准 Linux 关机命令所请求的 Linux 关机。本地应用使用
`/run/mcu_service/mcu_service.sock`（`0660 root:aico`）；Agent 通过 USB ECM 或回环 HTTP
提供相同的操作。

MCU 的实现维护在 `aiden-mcu` 中。要使用全部功能，需要同时安装新的 RV 业务软件包和包含 LED 扩展的 MCU 镜像。
旧版 MCU 镜像仍支持电池/定时器/唤醒处理；GET_LED 会报告 `UNSUPPORTED`。本页描述的是源码实现和离线验证，
并不代表某块开发板已经烧录或已经测量过其关机时序。

## 应用操作

UDS 使用通用的 [12 字节信封](../07-protocols/uds-protocol.md)：JSON 长度 LE32、二进制长度
LE64（必须为零），然后是 UTF-8 JSON。请求限制为 16 KiB，响应限制为 256 KiB。每个连接只发一个请求。
Go 客户端位于 `src/agent/internal/mcu/uds.go`。

| UDS `op` | HTTP | 结果字段 / 请求字段 |
| --- | --- | --- |
| `get_state` | `GET /api/mcu/state` | `state`：RV 生命周期/版本、generation、UART 打开状态、最近接收时间、LED 支持情况、插拔状态 |
| `get_battery` | `GET /api/mcu/battery` | `battery`：百分比/毫伏（未知时为 null）、充电状态、原始哨兵值、接收时间 |
| `get_wakeup_timer` | `GET /api/mcu/wakeup-timer` | `timer`：timer_id、active、remaining_s |
| `set_wakeup_timer` | `PUT /api/mcu/wakeup-timer` | `delay_s`：1–604800；返回 `timer` |
| `cancel_wakeup_timer` | `DELETE /api/mcu/wakeup-timer` | 返回已确认为非激活的 `timer` |
| `events_since` | `GET /api/mcu/events` | `since`、`generation`、`limit`、`wait_ms`；事件分页 |
| `get_led` | `GET /api/mcu/leds/red` 或 `/white` | `led`：请求的/实际生效的模式、时序、剩余租期、输出以及优先级原因 |
| `set_led` | `PUT /api/mcu/leds/red` 或 `/white` | `mode`、`on_ms`、`off_ms`、`lease_s`；返回 `led` |
| `poweroff` | 仅限 root 的本地 UDS/CLI | `generation`、`request_id`；返回已接受的 `shutting_down` 状态 |
| `suspend_prepare` | 仅限 root 的本地 UDS | 在 Linux 挂起之前请求 MCU 许可；返回 `sleeping` 状态 |
| `suspend_resume` | 仅限 root 的本地 UDS | 在唤醒或挂起失败后清除 MCU 授权；返回 `ready` 状态 |

对于 UDS LED 请求，还需要提供 `led: "red"` 或 `"white"`。HTTP 从路径中推导 LED 和操作。
所有成功的响应都带有 `status: "OK"`。其他状态在可用时会保留原始的数字 `mcu_status`。
HTTP 将无效参数映射为 400，拒绝访问映射为 403，变更冲突映射为 409，不支持的可选命令映射为 501，
不可用/忙/未就绪映射为 503，超时映射为 504，对端数据格式错误映射为 502，内部错误映射为 500。

`get_state` 是一个**本地 RV 查询**。UART `GET_STATE` 是 MCU 发往 RV 的，无法读取 MCU 固件或监控器状态。
UART 处于打开状态并不能证明对端健康；请检查 `last_rx_at` 以及一次成功的电池/定时器查询。
电池接收时间不是硬件采样时间。未知的百分比/电压保持为 null；MCU 的六字节电池响应使用 `FF`/`FFFF` 作为哨兵值。

应用的变更操作需要 `get_state` 返回的 `generation` 和一个唯一的 `request_id`。两个仅限 root 的挂起操作保留给
`aico-power` 使用，并由服务串行化处理；它们不使用变更 ID。在 `suspend_prepare` 之前，工作进程会持久化恢复意图。
超时或 PREPARE 应答丢失/无效时，MCU 授权的结果未知，因此工作进程必须中止挂起并发送 `suspend_resume`。
清理意图会一直保留，直到 RESUME 成功。工作进程和 systemd 的 post-stop 清理各进行一次有时限的尝试；
`aico-suspend-recover.timer` 会在上一次激活之后每十秒重试待处理的操作。它在启动时也会运行。
在每次 RV 启动后的第一次 PREPARE 之前，即使没有 `/run` 恢复文件，工作进程也会确认 RESUME，
因为 MCU/UART 的供电和授权可能在 RV 重启后依然存在。链路不可用时，恢复会保持待处理状态，并阻止新的挂起。
当挂起工作进程持有授权时，一把共享锁会排除恢复操作。
这一义务同样适用于这些仅限 root 操作的任何直接调用方。重复的 RV_READY 事件不会撤销 MCU 授权，
而且授权没有可能中断一次正常长时间休眠的定时器到期机制。
例如，一个定时器 PUT 请求体为：

```json
{"delay_s":120,"generation":"<current-generation>","request_id":"<unique-id>"}
```

守护进程最多保留 512 个变更结果，保留十分钟。相同的重试会返回原始结果；参数冲突或 generation 过旧的请求会被拒绝。
DELETE 同样接受包含这些字段的 JSON 请求体。在传输超时或 UDS 变更响应丢失/无法读取之后，
`unknown_outcome=true` 表示命令可能已经执行。在发送请求之前发生的连接失败不会设置该标志。
在有意发送新操作之前，请先读取当前状态。不要自动重试已过期的 ID。MCU 只有一个共享定时器：新的 SET 会替换之前的定时器。
它是相对时间，在 MCU 保持供电时可以在 RV 断电后保留，但 MCU 复位或断电后会丢失。

## 唤醒长轮询

```text
GET /api/mcu/events?since=0&limit=50&wait_ms=25000
```

首次请求时从零开始且不带 generation，之后保存返回的 generation，并前进到 **`next_id`**，即最后一个返回的事件。
`last_id` 是整个环形缓冲区的最高水位，可能超前于受 limit 限制的分页。即使你的应用只处理唤醒事件，也要读取分页中的所有条目。
`reset_required=true` 表示需要不带 generation 从零重新开始；`truncated=true` 表示历史已被淘汰。
正常超时后返回的空分页属于成功，且会保留游标。

环形缓冲区保留 512 个事件，各消费者的游标相互独立。`limit` 为 1–100（默认 50）；`wait_ms` 为 0–25000
（UDS 默认 0，HTTP 默认 25000）。客户端应为传输额外预留五秒，并通过关闭其专用的请求连接来取消等待。
同时进行的等待最多 32 个。等待不会占用 UART 请求槽位。守护进程关闭时会取消所有等待。

唤醒事件包含 `type: "wakeup"`、十进制字符串 `id`、`received_at`、`uart_seq`、`source_code` 和 `source`。
来源 1–7 分别为 `external_power`、`power_switch`、`touch_key`、`rtc_timer`、`other`、`asr`、`external_input`。
来源 7 是 SCH V2.19 扩展接口的 EXT_WAKE_IN 引脚，目前没有应用对其作出响应。
未知代码保留其数值，并带有 `source: "unknown"`；应用不得为其选择任何已知操作。无效的负载长度/保留字节会被拒绝。
单独的 `rk628_plugin_det_changed` 事件包含插入状态/原始电平数据。

正常的自锁开关启动不会合成唤醒事件。UART 事件没有 ACK 或重试，MCU 可能合并尚未发送的触发，守护进程重启会清空环形缓冲区。
请使用 `(generation,id)` 而不是 UART SEQ 对应用操作去重。该 API 在保留的历史范围内可重放，但不是持久化/恰好一次的。

### Agent 语音激活

在 STT（单轮和追问）模式以及实时模式下，Agent 直接通过本地 UDS 消费这个事件流。`source: "asr"` 且
`source_code: 6`，或 `source: "touch_key"` 且 `source_code: 3` 的 `wakeup` 事件取代了之前的
GPIO32/GPIO33 语音触发。它们进入现有的录音、输出打断和对话逻辑，沿用现有的 500 ms 防抖和待处理事件合并。
`tap_wakeup_enabled` 默认为 `true`；禁用它只会屏蔽触摸触发的语音，仍保留 MCU 的休眠唤醒。定时器、插拔和未知事件只推进游标，
不会激活语音。快速捕获所配置的 GPIO3 输入与此无关。文字模式不会启动该消费者。

该消费者使用 `AICO_MCU_SERVICE_SOCKET`，或默认的服务 socket。它以 25 秒的等待时间进行长轮询，
服务失败时每秒重试一次，并处理分页、generation 重置和历史截断。停止或重新加载语音循环会取消待处理的 UDS 请求并等待监视器退出。
各应用保留各自独立的事件游标。

Agent 在每次执行操作之前，会将 `(generation,id)` 原子地写入检查点 `/run/aico/agent-mcu-wakeup.json`
（权限 `0600`），从而在同一次启动内跨配置重新加载和 Agent 进程重启保留其游标。全新启动或新的 MCU 服务 generation
会从保留的历史开始，因此触摸或 ASR 唤醒可以在 Agent 初始化之后触发语音。当没有已保存的游标时，
在第一次成功订阅之前收到的事件（包括连接重试期间）会在排空保留的启动历史时获得两分钟的新鲜度窗口。
追上进度之后、带着已保存游标重启时，或 MCU generation 重置时，适用常规的 30 秒限制。
无效时间戳和超过适用限制的事件会被跳过，以避免因旧历史启动一次延迟的对话。这项时效检查使用 RV 的墙上时钟；
时钟向前校正也可能导致事件被跳过。在检查点和回调之间发生崩溃可能会丢失一次操作。该消费者不提供持久化投递，也不会恢复已被淘汰的历史。

在设备上，以 STT 或实时模式验证完整的语音通路：

```bash
tail -F /userdata/agent/log/agent.log
```

按下触摸开关或说出 ASR 唤醒词，将对应的 `MCU touch_key wakeup event` 或 `MCU asr wakeup event`
日志与服务事件以及随后的语音录制/会话对应起来。在自锁开关处于 ON 且安装了[所需的挂起固件](../01-getting-started/firmware.md#系统挂起)
的情况下，执行 `aico-power sleep` 后重复测试，确认按一次即可恢复 Linux 并启动语音。在正在回答时重复测试，以验证所配置的打断行为。
定时器事件必须能通过 `/api/mcu/events` 看到，且不激活语音。重启 Agent 并重新加载其语音配置，验证已处理过的事件不会重复触发。
烧录后验证脚本的 `--expect-source touch_key` 和 `--expect-source asr` 只检查事件转发；
它们不能证明录音、云端识别或语音输出成功。RV 断电时，ASR 模块的提示音不作为验收标准。

## LED 扩展

MCU 命令 `0x0B SET_LED` 接受八个字节：ID、mode、on_ms LE16、off_ms LE16、lease_s LE16。
`0x0C GET_LED` 接受一个 ID 字节。两者都返回 12 字节：STATUS、ID、请求的/实际生效的模式、on_ms LE16、
off_ms LE16、remaining_s LE16、output_level、override_reason。ID 中红灯为 0、白灯为 1。
模式中 auto 为 0、off 为 1、on 为 2、blink 为 3。

Auto 在所有时序/租期值为零时释放控制权。Off/on 要求时长为零，租期为 1–3600 秒。Blink 要求每个时长在
20–60000 ms 之间，租期为 1–3600 秒。启动指示以及红灯的低电量/充电策略优先。租期在被屏蔽期间仍会继续计时，
到期后恢复为 auto，并在 MCU 复位或 RV 关机/断电时清除。重复的 UART SET 永远不会续期租约。

## 关机与运维

MCU 自锁开关处于 OFF 状态时会全局禁止所有唤醒来源，包括触摸、ASR、定时器以及将来新增的来源。
它会取消定时器、已排队的重启以及尚未发送的 WAKEUP 通知；被禁止的输入在切换到 ON 时不会重放。
MCU 复位后，唤醒会一直处于禁止状态，直到 ON 状态满足条件。该策略由 MCU 强制执行，早于 RV 服务/事件转发运行。

从 OFF 状态正向唤醒的测试必须让开关保持 ON，并使用协调的软件关机。修正后的 MCU 在观察到 RV SHUTTING_DOWN
并收到安全的 SHUTDOWN_READY 之后进入 OFF 等待，直到收到新的唤醒。使用本地 CLI：

```sh
sudo /usr/lib/aico/mcu_service poweroff
```

CLI 会查询当前服务的 generation，并提交一个幂等的 UDS `poweroff` 操作。守护进程检查 Linux `SO_PEERCRED`，
只接受来自 UID 0 的该操作；普通的 `aico` 组客户端会收到 `DENIED`。凭据查询失败同样会拒绝访问。
Poweroff 没有 HTTP 端点。守护进程会持久化本次启动的意图，进入 SHUTTING_DOWN，关闭事件等待并停止应用的 UART 请求。
它会等待 MCU 的下一次 GET_STATE 探测（通常在五秒内），在写出 SHUTTING_DOWN 响应后安排 Linux 关机。
最终关机钩子保留其存储安全检查。意图持久化失败时，正在运行的服务保持不变。接受响应表示关机已安排，
并不表示 MCU 已经切断电源。

如果状态响应无法发送，一个十秒的回退机制仍会安全地停止 Linux。与守护进程重启后恢复已持久化意图的情况一样，
此时 MCU 是否断电未经确认，必须在唤醒测试之前检查。常规的 `sudo shutdown now`、`sudo poweroff` 和
`sudo systemctl poweroff` 也会通过最终关机钩子协调 MCU 断电。它们不需要守护进程意图：存储卸载完成后，
钩子会先以 SHUTTING_DOWN 应答 MCU 下一次有效的 GET_STATE，然后发送 SHUTDOWN_READY。
在此阶段到达的有效 MCU SHUTDOWN_REQUEST 也会在 READY 之前得到确认。因此，对于这条路径，
MCU 的断电计时器只会在存储安全之后才开始。强制关机并绕过 systemd 最终钩子的命令不支持 MCU 断电。
现有开发板报告中关于 OFF 开关下触摸/定时器唤醒的描述反映的是旧固件行为，不能作为修正后策略的验收依据。

守护进程在 ACK 之前将本次启动的意图持久化到 tmpfs，然后在写出响应后安排 systemd 关机（若 UART 无法送达，则有一个有时限的回退）。
`/usr/lib/systemd/system-shutdown/aico-mcu-shutdown` 在最终关机阶段调用静态链接的服务二进制文件。
它以 root 身份运行，只在 `poweroff`、swap 未激活且没有可写的持久化文件系统时才发送 SHUTDOWN_READY。
一个有效的、属于 root 的本次启动意图允许立即发送 READY；没有意图时，钩子会建立上文所述的握手。
格式错误、不安全或过期的意图文件会以拒绝方式失败（fail closed）。辅助程序会持续应答探测，并每 100 ms 重试一次 READY，
直到 MCU 断电，最长十秒。这可以处理缓冲的过期探测和丢失的完成事件。如果到截止时间时它仍在运行，
它会报告未确认断电，并让 Linux 完成停机。服务重启、软件包升级、halt 和 Linux 重启永远不会授权此握手。

如果 MCU 请求的关机在意图持久化时失败，RV 会回复 INTERNAL_ERROR，但仍会安排 Linux 清理：MCU 的强制断电已经开始，
不会因该错误而取消。在存储安全之后，如果既没有有效意图，也没有新的最终阶段握手，最终钩子会扣留 SHUTDOWN_READY。
本地软件关机请求事先不存在 MCU 断电计时，因此持久化失败时该请求根本不会开始。

MCU 的 30 秒截止时间从其关机状态转换时开始计算，即使 RV 拒绝或错过了该请求也是如此。收到 SHUTDOWN_READY 后，
允许在 75 ms 后断电。Agent 和 swap 的停止时限已经缩短，但仍必须在写入/swap/OTA 负载下测量，确保**整个**
操作系统关机路径低于 25 秒。最终辅助程序在文件系统状态不安全时会扣留 READY；它无法取消 MCU 的强制断电。
请审查其他最终关机钩子，并测量最后一次写入和电源轨断电的时间。不要仅凭主机测试通过或网络重新连接就声称满足这一物理时序。

Aico 生产设备树移除了从 EVB 继承的 `restart-poweroff` 节点。除了更新后的 `mcu_service`，
还需要安装重新构建的 BSP 启动镜像；如果只更新用户态，MCU 断电失败时内核的重启回退仍然有效。
`poweroff-dtb` 测试套件检查编译后的自定义设备树，BSP 审计则检查从两个启动镜像中提取的 DTB。

```sh
systemctl status aico-mcu-event.service
journalctl -u aico-mcu-event.service -f
```

在使用 MCU 仓库中的直连串口对端之前，请先停止该服务。`RV_HOLD` 仍未使用；SCH V2.19 已不再连接该信号（见
[SCH V2.19 适配](../03-hardware/sch-v2-19-adaptation.md)）。保留原服务名称，
可以让现有的软件包生命周期处理逻辑启停替换后的服务，而不会出现与之竞争的日志程序。

## 烧录后：可执行的检查

验证脚本 [scripts/test_mcu_after_flash.py](../../scripts/test_mcu_after_flash.py)
不会烧录设备，也不会发出原始电源命令。它默认是只读的。可以在 RV 上的代码检出目录中运行，
或在通过 USB 连接的主机上使用 `--url`。主机端测试需要 Agent HTTP 路由可用。
在运行需要显式开启的变更测试之前，请停止其他定时器和 LED 写入方：这些资源是共享的。

```sh
# On RV: ready/link state, battery, timer and required new LED support.
python3 scripts/test_mcu_after_flash.py --require-led --report /tmp/mcu-query.json

# On RV: short white blink, lease expiry, two-second timer and its WAKEUP.
python3 scripts/test_mcu_after_flash.py --require-led --exercise-led \
  --exercise-timer --report /tmp/mcu-functional.json

# Keep the maintained switch ON; press/release after the runner starts waiting.
python3 scripts/test_mcu_after_flash.py --expect-source touch_key \
  --timeout 60 --report /tmp/mcu-touch.json

# Keep the maintained switch ON; speak after the runner starts waiting.
python3 scripts/test_mcu_after_flash.py --expect-source asr \
  --timeout 60 --report /tmp/mcu-asr.json

# Select this scenario on an external host. Keep the switch ON; after timer
# arming, run sudo /usr/lib/aico/mcu_service poweroff on the board.
python3 scripts/test_mcu_after_flash.py --url http://192.168.42.1:8080 \
  --power-cycle --allow-power-cycle --wake-delay 120 \
  --report /tmp/mcu-power-cycle.json
```

该脚本会拒绝替换已经激活的定时器或白灯覆盖设置，会清理自己所做的更改，并将失败/清理错误记录在 JSON 报告中。
电源循环场景要求观察到断开连接、新的守护进程 generation 以及来源为定时器的 WAKEUP。要确认物理关机安全，
仍需要对电源轨和文件系统时序进行仪器测量。`--image <MCU.bin>` 记录预期产物的 SHA256；它不能证明该镜像已被烧录。

离线检查在 `make check` 中运行，包括 Go 竞态测试、模拟链路、UDS 和 HTTP 测试、Linux/ARM 交叉编译，
以及无需访问设备的验证脚本测试。MCU 仓库另外运行构建、主机/固件工具测试和依赖校验。
请将开发板报告保存在 Git 之外，并在物理验收结果中记录确切的镜像标识。


### 挂起所需的持久化部署

已安装的 `aico-mcu-event.service` 必须独占 UART5，并运行与之匹配的 `/usr/lib/aico/mcu_service`
二进制文件。`/run/systemd/system` 下的 drop-in 只是临时的诊断覆盖，重启后就会消失。
在适配较旧的开发用 rootfs 时，请先保存原始单元和二进制文件，将经过审查的覆盖安装到
`/etc/systemd/system/<existing-mcu-unit>.d/` 下，重新加载 systemd 并重启该现有单元。
不要启动第二个 UART 占用方。在尝试挂起之前，请在实际重启之后验证生效的 ExecStart、socket 权限和 MCU READY 状态。
新镜像已经安装了生产单元，不需要这种兼容覆盖。
